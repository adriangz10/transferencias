package http

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
	"veripago-saas/internal/domain"
)

// ---------------------------------------------------------------------------
// Login Multi-Tenant y Super-Admin con sesión (cookie HttpOnly)
// ---------------------------------------------------------------------------

const (
	sessionCookieName = "veripago_session"
	sessionTTL        = 12 * time.Hour
	loginMaxFailures  = 5                // intentos fallidos permitidos por IP...
	loginLockWindow   = 15 * time.Minute // ...dentro de esta ventana
)

// sessionData almacena la identidad de la sesión activa (comercio o Super-Admin).
type sessionData struct {
	TenantID      string
	IsSuperAdmin  bool
	Impersonating bool
	ExpiresAt     time.Time
}

// authEnabled indica si el panel pide login. Solo en modo demo sin contraseña queda abierto por defecto.
func (h *WebhooksHandler) authEnabled() bool {
	return h.cfg.AdminPassword != "" || !h.cfg.DemoMode
}

// sessionStore guarda las sesiones activas en memoria.
type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]sessionData // token -> datos de sesión
	failures map[string][]time.Time
}

func newSessionStore() *sessionStore {
	return &sessionStore{
		sessions: make(map[string]sessionData),
		failures: make(map[string][]time.Time),
	}
}

func (s *sessionStore) create(tenantID string, isSuperAdmin bool) string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	token := base64.RawURLEncoding.EncodeToString(b)
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for t, sess := range s.sessions {
		if now.After(sess.ExpiresAt) {
			delete(s.sessions, t)
		}
	}
	s.sessions[token] = sessionData{
		TenantID:     tenantID,
		IsSuperAdmin: isSuperAdmin,
		ExpiresAt:    now.Add(sessionTTL),
	}
	return token
}

func (s *sessionStore) get(token string) (sessionData, bool) {
	if token == "" {
		return sessionData{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[token]
	if !ok {
		return sessionData{}, false
	}
	if time.Now().After(sess.ExpiresAt) {
		delete(s.sessions, token)
		return sessionData{}, false
	}
	return sess, true
}

func (s *sessionStore) valid(token string) bool {
	_, ok := s.get(token)
	return ok
}

func (s *sessionStore) setTenant(token, tenantID string, impersonating bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[token]
	if !ok || time.Now().After(sess.ExpiresAt) {
		return false
	}
	sess.TenantID = tenantID
	sess.Impersonating = impersonating
	s.sessions[token] = sess
	return true
}

func (s *sessionStore) destroy(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, token)
}

// recentFailures devuelve los intentos fallidos recientes de una IP (y limpia los viejos).
func (s *sessionStore) recentFailures(ip string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().Add(-loginLockWindow)
	kept := s.failures[ip][:0]
	for _, t := range s.failures[ip] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(s.failures, ip)
		return 0
	}
	s.failures[ip] = kept
	return len(kept)
}

func (s *sessionStore) addFailure(ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures[ip] = append(s.failures[ip], time.Now())
}

func (s *sessionStore) clearFailures(ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.failures, ip)
}

func clientIP(r *http.Request) string {
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	return strings.Trim(host, "[]")
}

func (h *WebhooksHandler) currentSession(r *http.Request) (sessionData, bool) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return sessionData{}, false
	}
	return h.sessions.get(c.Value)
}

func (h *WebhooksHandler) isLoggedIn(r *http.Request) bool {
	_, ok := h.currentSession(r)
	return ok
}

// activeTenantID devuelve el comercio asociado a la sesión HTTP actual (o el comercio por defecto).
func (h *WebhooksHandler) activeTenantID(r *http.Request) string {
	if sess, ok := h.currentSession(r); ok && sess.TenantID != "" {
		return sess.TenantID
	}
	return h.tenantID
}

// isSuperAdmin indica si la sesión actual pertenece al Super-Admin de la plataforma.
func (h *WebhooksHandler) isSuperAdmin(r *http.Request) bool {
	if sess, ok := h.currentSession(r); ok {
		return sess.IsSuperAdmin
	}
	if !h.authEnabled() {
		return true
	}
	return false
}

// isImpersonating indica si el Super-Admin está inspeccionando el panel de un comercio puntual.
func (h *WebhooksHandler) isImpersonating(r *http.Request) bool {
	if sess, ok := h.currentSession(r); ok {
		return sess.Impersonating
	}
	return false
}

func (h *WebhooksHandler) setSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   h.secureCookies(r),
		SameSite: http.SameSiteLaxMode,
	})
}

// requireAdmin protege el panel y la API con la sesión iniciada en /login o /registro.
// Las páginas redirigen al login; la API responde 401 en JSON.
func (h *WebhooksHandler) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.authEnabled() && !h.isLoggedIn(r) {
			next(w, r)
			return
		}
		if !h.isLoggedIn(r) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Tu sesión venció. Volvé a iniciar sesión."})
				return
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		// Defensa CSRF: los cambios solo pueden venir del propio panel
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			http.Error(w, "Origen no permitido", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// requireSuperAdmin restringe el acceso exclusivamente al Super-Admin de la plataforma.
func (h *WebhooksHandler) requireSuperAdmin(next http.HandlerFunc) http.HandlerFunc {
	return h.requireAdmin(func(w http.ResponseWriter, r *http.Request) {
		if !h.isSuperAdmin(r) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "Acceso exclusivo para el Super-Admin de la plataforma"})
			return
		}
		next(w, r)
	})
}

// handleLogin muestra el formulario (GET) y valida email de comercio o usuario Super-Admin (POST).
func (h *WebhooksHandler) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if (!h.authEnabled() && r.URL.Query().Get("switch") == "") || h.isLoggedIn(r) {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		servePanelIndex(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "Método no permitido", http.StatusMethodNotAllowed)
		return
	}

	ip := clientIP(r)
	if h.sessions.recentFailures(ip) >= loginMaxFailures {
		http.Redirect(w, r, "/login?error=locked", http.StatusSeeOther)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var user, pass string
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		var body struct {
			Username string `json:"username"`
			Email    string `json:"email"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "JSON inválido"})
			return
		}
		user = strings.TrimSpace(body.Username)
		if user == "" {
			user = strings.TrimSpace(body.Email)
		}
		pass = body.Password
	} else {
		if err := r.ParseForm(); err != nil {
			http.Redirect(w, r, "/login?error=1", http.StatusSeeOther)
			return
		}
		user = strings.TrimSpace(r.PostFormValue("username"))
		if user == "" {
			user = strings.TrimSpace(r.PostFormValue("email"))
		}
		pass = r.PostFormValue("password")
	}

	// 1. ¿Es el Super-Admin de la plataforma (ADMIN_USER / ADMIN_PASSWORD)?
	adminPass := h.cfg.AdminPassword
	if adminPass == "" && h.cfg.DemoMode {
		adminPass = "admin"
	}
	if adminPass != "" {
		userOK := subtle.ConstantTimeCompare([]byte(user), []byte(h.cfg.AdminUser)) == 1
		passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(adminPass)) == 1
		if userOK && passOK {
			h.sessions.clearFailures(ip)
			h.setSessionCookie(w, r, h.sessions.create(h.tenantID, true))
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
	}

	// 2. ¿Es un comercio registrado ingresando con su email y contraseña?
	if tenant, err := h.repo.GetTenantByEmail(r.Context(), user); err == nil && tenant != nil && tenant.PasswordHash != "" {
		if bcrypt.CompareHashAndPassword([]byte(tenant.PasswordHash), []byte(pass)) == nil {
			if !tenant.IsActive {
				http.Redirect(w, r, "/login?error=suspended", http.StatusSeeOther)
				return
			}
			h.sessions.clearFailures(ip)
			h.setSessionCookie(w, r, h.sessions.create(tenant.ID, false))
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
	}

	h.sessions.addFailure(ip)
	time.Sleep(300 * time.Millisecond) // frena los intentos automáticos
	http.Redirect(w, r, "/login?error=1", http.StatusSeeOther)
}

// handleRegister sirve la pantalla de registro (GET /registro) y da de alta un nuevo comercio (POST /api/v1/register).
func (h *WebhooksHandler) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if h.isLoggedIn(r) {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		servePanelIndex(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "Método no permitido", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		BusinessName string `json:"business_name"`
		Email        string `json:"email"`
		Password     string `json:"password"`
		CUIT         string `json:"cuit"`
		OwnerPhone   string `json:"owner_phone"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "JSON inválido"})
		return
	}

	name := strings.TrimSpace(req.BusinessName)
	if name == "" || utf8.RuneCountInString(name) > 150 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Ingresá el nombre del comercio (hasta 150 caracteres)"})
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if a, err := mail.ParseAddress(email); err != nil || a.Address != email || len(email) > 150 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Ingresá un email válido"})
		return
	}
	if len(req.Password) < 8 || len(req.Password) > 72 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "La contraseña debe tener entre 8 y 72 caracteres"})
		return
	}
	cuit := strings.TrimSpace(req.CUIT)
	if cuit != "" && domain.NormalizeCUIT(cuit) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "El CUIT debe tener 11 números"})
		return
	}
	phone := normalizePhone(req.OwnerPhone)
	if strings.TrimSpace(req.OwnerPhone) != "" && !phoneRegex.MatchString(phone) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "El WhatsApp debe incluir código de país con solo números (ej: 5491155004444)"})
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "No se pudo procesar la contraseña"})
		return
	}

	spec := domain.GetPlanSpecs()[domain.PlanFreeTrial]
	exp := time.Now().AddDate(0, 0, 14)
	tenant := &domain.Tenant{
		BusinessName:       name,
		CUIT:               domain.NormalizeCUIT(cuit),
		Email:              email,
		PasswordHash:       string(hash),
		PlanType:           spec.Plan,
		MonthlyQuota:       spec.MonthlyQuota,
		TimeWindowMinutes:  domain.DefaultTimeWindowMinutes,
		SubscriptionStatus: "TRIAL",
		PlanExpiresAt:      &exp,
		IsActive:           true,
	}
	if err := h.repo.CreateTenant(r.Context(), tenant); err != nil {
		status := http.StatusInternalServerError
		msg := "No se pudo crear la cuenta del comercio"
		if errors.Is(err, domain.ErrTenantEmailTaken) || errors.Is(err, domain.ErrTenantCUITTaken) {
			status = http.StatusConflict
			msg = err.Error()
		}
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}

	if phone != "" {
		_ = h.repo.CreateAuthorizedSender(r.Context(), &domain.AuthorizedSender{
			TenantID:     tenant.ID,
			PhoneNumber:  phone,
			EmployeeName: name + " (Titular)",
			Role:         domain.RoleOwner,
			IsActive:     true,
		})
	}

	h.setSessionCookie(w, r, h.sessions.create(tenant.ID, false))
	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"status":    "registered",
		"tenant_id": tenant.ID,
		"tenant":    tenant,
	})
}

// handleChangePassword permite al comercio cambiar su contraseña desde Configuración.
func (h *WebhooksHandler) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "Método no permitido", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "JSON inválido"})
		return
	}
	if len(req.NewPassword) < 8 || len(req.NewPassword) > 72 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "La nueva contraseña debe tener entre 8 y 72 caracteres"})
		return
	}

	tenantID := h.activeTenantID(r)
	tenant, err := h.repo.GetTenantByID(r.Context(), tenantID)
	if err != nil || tenant == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Comercio no encontrado"})
		return
	}
	if tenant.PasswordHash != "" && !h.isSuperAdmin(r) {
		if bcrypt.CompareHashAndPassword([]byte(tenant.PasswordHash), []byte(req.CurrentPassword)) != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "La contraseña actual es incorrecta"})
			return
		}
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "No se pudo guardar la contraseña"})
		return
	}
	if err := h.repo.UpdateTenantPassword(r.Context(), tenantID, string(hash)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "No se pudo actualizar la contraseña"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// handleLogout cierra la sesión.
func (h *WebhooksHandler) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "Método no permitido", http.StatusMethodNotAllowed)
		return
	}
	if c, err := r.Cookie(sessionCookieName); err == nil {
		h.sessions.destroy(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   h.secureCookies(r),
		SameSite: http.SameSiteLaxMode,
	})
	if !h.authEnabled() {
		http.Redirect(w, r, "/login?switch=1", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (h *WebhooksHandler) secureCookies(r *http.Request) bool {
	return r.TLS != nil || strings.HasPrefix(strings.ToLower(h.cfg.PublicBaseURL), "https://")
}

// demoOnly expone una ruta solo en modo demo (en producción responde 404).
func (h *WebhooksHandler) demoOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.cfg.DemoMode {
			http.NotFound(w, r)
			return
		}
		next(w, r)
	}
}

// ---------------------------------------------------------------------------
// OAuth state firmado: evita que alguien vincule su propia cuenta de MP a otro comercio
// ---------------------------------------------------------------------------

const oauthStateTTL = 10 * time.Minute

var errInvalidOAuthState = errors.New("state OAuth inválido o vencido")

type oauthStateSigner struct {
	key []byte

	mu   sync.Mutex
	used map[string]time.Time // nonces ya usados (un state sirve una sola vez)
}

func newOAuthStateSigner(masterKey string) *oauthStateSigner {
	mac := hmac.New(sha256.New, []byte(masterKey))
	mac.Write([]byte("veripago-oauth-state-v1"))
	return &oauthStateSigner{key: mac.Sum(nil), used: make(map[string]time.Time)}
}

func (s *oauthStateSigner) sign(payload string) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

// Issue genera un state para el tenant con vencimiento y nonce de un solo uso.
func (s *oauthStateSigner) Issue(tenantID string) string {
	nonce := make([]byte, 12)
	_, _ = rand.Read(nonce)
	payload := tenantID + "|" + strconv.FormatInt(time.Now().Add(oauthStateTTL).Unix(), 10) + "|" + hex.EncodeToString(nonce)
	enc := base64.RawURLEncoding.EncodeToString([]byte(payload))
	return enc + "." + s.sign(payload)
}

// Verify valida firma, vencimiento y uso único; devuelve el tenant.
func (s *oauthStateSigner) Verify(state string) (string, error) {
	enc, sig, ok := strings.Cut(state, ".")
	if !ok {
		return "", errInvalidOAuthState
	}
	raw, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return "", errInvalidOAuthState
	}
	payload := string(raw)
	if !hmac.Equal([]byte(s.sign(payload)), []byte(sig)) {
		return "", errInvalidOAuthState
	}
	parts := strings.Split(payload, "|")
	if len(parts) != 3 || parts[0] == "" {
		return "", errInvalidOAuthState
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return "", errInvalidOAuthState
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for n, t := range s.used {
		if now.Sub(t) > oauthStateTTL {
			delete(s.used, n)
		}
	}
	if _, dup := s.used[parts[2]]; dup {
		return "", errInvalidOAuthState
	}
	s.used[parts[2]] = now
	return parts[0], nil
}
