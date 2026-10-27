package http

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Login con sesión (cookie HttpOnly) para el panel y la API
// ---------------------------------------------------------------------------

const (
	sessionCookieName = "veripago_session"
	sessionTTL        = 12 * time.Hour
	loginMaxFailures  = 5                // intentos fallidos permitidos por IP...
	loginLockWindow   = 15 * time.Minute // ...dentro de esta ventana
)

// authEnabled indica si el panel pide login. Solo en modo demo sin contraseña queda abierto.
func (h *WebhooksHandler) authEnabled() bool {
	return h.cfg.AdminPassword != "" || !h.cfg.DemoMode
}

// sessionStore guarda las sesiones activas en memoria (al reiniciar el servidor hay que volver a entrar).
type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]time.Time // token -> vencimiento
	failures map[string][]time.Time
}

func newSessionStore() *sessionStore {
	return &sessionStore{sessions: make(map[string]time.Time), failures: make(map[string][]time.Time)}
}

func (s *sessionStore) create() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	token := base64.RawURLEncoding.EncodeToString(b)
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for t, exp := range s.sessions {
		if now.After(exp) {
			delete(s.sessions, t)
		}
	}
	s.sessions[token] = now.Add(sessionTTL)
	return token
}

func (s *sessionStore) valid(token string) bool {
	if token == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.sessions[token]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(s.sessions, token)
		return false
	}
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

func (h *WebhooksHandler) isLoggedIn(r *http.Request) bool {
	c, err := r.Cookie(sessionCookieName)
	return err == nil && h.sessions.valid(c.Value)
}

// requireAdmin protege el panel y la API con la sesión iniciada en /login.
// Las páginas redirigen al login; la API responde 401 en JSON.
func (h *WebhooksHandler) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.authEnabled() {
			next(w, r)
			return
		}
		if h.cfg.AdminPassword == "" {
			http.Error(w, "Panel deshabilitado: configurá ADMIN_PASSWORD", http.StatusServiceUnavailable)
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

// handleLogin muestra el formulario (GET) y valida usuario/contraseña (POST).
func (h *WebhooksHandler) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !h.authEnabled() || h.isLoggedIn(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	switch r.Method {
	case http.MethodGet:
		// La pantalla de login es parte de la app de Angular
		servePanelIndex(w, r)
	case http.MethodPost:
		if h.cfg.AdminPassword == "" {
			http.Error(w, "Panel deshabilitado: configurá ADMIN_PASSWORD", http.StatusServiceUnavailable)
			return
		}
		ip := clientIP(r)
		if h.sessions.recentFailures(ip) >= loginMaxFailures {
			http.Redirect(w, r, "/login?error=locked", http.StatusSeeOther)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
		if err := r.ParseForm(); err != nil {
			http.Redirect(w, r, "/login?error=1", http.StatusSeeOther)
			return
		}
		user := strings.TrimSpace(r.PostFormValue("username"))
		pass := r.PostFormValue("password")
		userOK := subtle.ConstantTimeCompare([]byte(user), []byte(h.cfg.AdminUser)) == 1
		passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(h.cfg.AdminPassword)) == 1
		if !userOK || !passOK {
			h.sessions.addFailure(ip)
			time.Sleep(300 * time.Millisecond) // frena los intentos automáticos
			http.Redirect(w, r, "/login?error=1", http.StatusSeeOther)
			return
		}
		h.sessions.clearFailures(ip)
		http.SetCookie(w, &http.Cookie{
			Name:     sessionCookieName,
			Value:    h.sessions.create(),
			Path:     "/",
			MaxAge:   int(sessionTTL.Seconds()),
			HttpOnly: true,
			Secure:   h.secureCookies(r),
			SameSite: http.SameSiteLaxMode,
		})
		http.Redirect(w, r, "/", http.StatusSeeOther)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "Método no permitido", http.StatusMethodNotAllowed)
	}
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
