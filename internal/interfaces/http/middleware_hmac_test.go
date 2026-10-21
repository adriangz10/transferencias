package http

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"veripago-saas/internal/application"
	"veripago-saas/internal/config"
	"veripago-saas/internal/domain"
	"veripago-saas/internal/infrastructure/ai"
	"veripago-saas/internal/infrastructure/crypto"
	"veripago-saas/internal/infrastructure/mp"
	"veripago-saas/internal/infrastructure/persistence"
	"veripago-saas/internal/infrastructure/queue"
	"veripago-saas/internal/infrastructure/whatsapp"
)

func TestValidateMPWebhookSignature(t *testing.T) {
	secret := "my_mp_webhook_secret_key_2026"
	dataID := "88492019482"
	reqID := "req-uuid-12345"
	ts := "1729460000"

	sigHeader := ComputeMPWebhookSignature(secret, dataID, reqID, ts)

	req, _ := http.NewRequest(http.MethodPost, "https://veripago.ar/webhooks/mercadopago?data.id="+dataID, nil)
	req.Header.Set("x-signature", sigHeader)
	req.Header.Set("x-request-id", reqID)
	if !ValidateMPWebhookSignature(secret, req) {
		t.Fatalf("debía aceptar una firma HMAC válida")
	}

	reqTampered, _ := http.NewRequest(http.MethodPost, "https://veripago.ar/webhooks/mercadopago?data.id=99999999999", nil)
	reqTampered.Header.Set("x-signature", sigHeader)
	reqTampered.Header.Set("x-request-id", reqID)
	if ValidateMPWebhookSignature(secret, reqTampered) {
		t.Fatalf("debía rechazar un data.id adulterado")
	}

	if ValidateMPWebhookSignature("", req) {
		t.Fatalf("sin secreto configurado no debía aceptar nada")
	}
}

func TestValidateWhatsAppSignature(t *testing.T) {
	body := []byte(`{"entry":[]}`)
	sig := ComputeWhatsAppSignature("app-secret", body)
	if !ValidateWhatsAppSignature("app-secret", sig, body) {
		t.Fatal("debía aceptar una firma válida")
	}
	if ValidateWhatsAppSignature("app-secret", "", body) {
		t.Fatal("debía rechazar un mensaje sin firma")
	}
	if ValidateWhatsAppSignature("", sig, body) {
		t.Fatal("sin app secret no debía aceptar nada")
	}
}

type testEnv struct {
	cfg     *config.Config
	handler *WebhooksHandler
	mux     *http.ServeMux
	waLog   *whatsapp.Client
	cookie  *http.Cookie
}

// login envía el formulario de /login y devuelve la respuesta.
func (e *testEnv) login(user, pass, remoteAddr string) *httptest.ResponseRecorder {
	form := url.Values{"username": {user}, "password": {pass}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

// sessionCookie inicia sesión una vez y reutiliza la cookie.
func (e *testEnv) sessionCookie() *http.Cookie {
	if e.cookie != nil {
		return e.cookie
	}
	rec := e.login(e.cfg.AdminUser, e.cfg.AdminPassword, "")
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			e.cookie = c
			return c
		}
	}
	return &http.Cookie{Name: sessionCookieName, Value: "sin-sesion"}
}

func newTestEnv(t *testing.T, demo bool, adminPassword string) *testEnv {
	t.Helper()
	cfg := &config.Config{
		DemoMode:        demo,
		AdminUser:       "admin",
		AdminPassword:   adminPassword,
		MasterKey:       "0123456789abcdef0123456789abcdef",
		MPWebhookSecret: "mp-secret",
		WAAppSecret:     "wa-secret",
		WAVerifyToken:   "verify",
		MPClientID:      "MP_DEMO_CLIENT_ID",
	}
	vault, err := crypto.NewTokenVault(cfg.MasterKey)
	if err != nil {
		t.Fatal(err)
	}
	repo := persistence.NewMySQLRepository(nil)
	encAcc, _ := vault.Encrypt("DEMO_TOKEN")
	encRef, _ := vault.Encrypt("DEMO_REFRESH")
	_ = repo.SeedDefaultDemoData(context.Background(), encAcc, encRef, cfg.MPWebhookSecret)

	extractor, _ := ai.NewGeminiReceiptExtractor(context.Background(), "", "", demo)
	mpClient := mp.NewMPClient(demo)
	waClient := whatsapp.NewClient("", "")
	verifyUC := application.NewVerifyReceiptUseCase(repo, extractor, mpClient)
	h := NewWebhooksHandler(cfg, repo, vault, extractor, mpClient, waClient, queue.NewRedisQueueStore("", "", 0), verifyUC)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	return &testEnv{cfg: cfg, handler: h, mux: mux, waLog: waClient}
}

func (e *testEnv) do(method, path, body string, auth bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if auth {
		req.AddCookie(e.sessionCookie())
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

func TestAdminAuthRequired(t *testing.T) {
	env := newTestEnv(t, false, "una-clave-larga-123")
	for _, path := range []string{"/api/v1/dashboard", "/api/v1/export/validations.csv"} {
		if rec := env.do(http.MethodGet, path, "", false); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s sin sesión: esperaba 401, obtuvo %d", path, rec.Code)
		}
	}
	// El panel redirige al login
	if rec := env.do(http.MethodGet, "/", "", false); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Errorf("/ sin sesión: esperaba 303 a /login, obtuvo %d %q", rec.Code, rec.Header().Get("Location"))
	}
	// La autenticación básica ya no sirve
	req := httptest.NewRequest(http.MethodGet, "/api/v1/dashboard", nil)
	req.SetBasicAuth(env.cfg.AdminUser, env.cfg.AdminPassword)
	rec := httptest.NewRecorder()
	env.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("Basic Auth: esperaba 401, obtuvo %d", rec.Code)
	}
	if rec := env.do(http.MethodGet, "/api/v1/dashboard", "", true); rec.Code != http.StatusOK {
		t.Errorf("con credenciales: esperaba 200, obtuvo %d", rec.Code)
	}
	// Webhooks y health no usan contraseña
	if rec := env.do(http.MethodGet, "/health", "", false); rec.Code != http.StatusOK {
		t.Errorf("/health: esperaba 200, obtuvo %d", rec.Code)
	}
}

func TestSimulatorDisabledOutsideDemo(t *testing.T) {
	env := newTestEnv(t, false, "una-clave-larga-123")
	for _, path := range []string{"/api/v1/simulate/receipt", "/api/v1/simulate/mp-webhook", "/api/v1/simulate/whatsapp-command"} {
		if rec := env.do(http.MethodPost, path, `{"amount":27650}`, true); rec.Code != http.StatusNotFound {
			t.Errorf("%s fuera del modo demo: esperaba 404, obtuvo %d", path, rec.Code)
		}
	}
}

func TestSimulatedReceiptBytesRejectedOutsideDemo(t *testing.T) {
	extractor, _ := ai.NewGeminiReceiptExtractor(context.Background(), "", "", false)
	out, err := extractor.ExtractFromMedia(context.Background(), []byte(`SIMULATED_RECEIPT_JSON:{"is_valid_receipt":true,"is_completed_transfer":true,"amount":1}`), "application/pdf")
	if err == nil && out != nil && out.IsValidReceipt {
		t.Fatal("fuera del modo demo un archivo simulado nunca debe aceptarse como comprobante válido")
	}
	// Un archivo cualquiera sin GEMINI_API_KEY tampoco: no hay extractor de mentira en producción
	if _, err := extractor.ExtractFromMedia(context.Background(), []byte("cualquier-archivo-de-mas-de-16-bytes"), "image/jpeg"); err == nil {
		t.Fatal("sin GEMINI_API_KEY y fuera del modo demo debía devolver error")
	}
}

func TestWhatsAppWebhookRequiresSignature(t *testing.T) {
	env := newTestEnv(t, false, "una-clave-larga-123")
	body := `{"entry":[{"changes":[{"value":{"messages":[{"id":"wamid.1","from":"5491155002222","type":"text","text":{"body":"hola"}}]}}]}]}`

	if rec := env.do(http.MethodPost, "/webhooks/whatsapp", body, false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("sin firma: esperaba 401, obtuvo %d", rec.Code)
	}

	req := httptest.NewRequest(http.MethodPost, "/webhooks/whatsapp", strings.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", ComputeWhatsAppSignature("wa-secret", []byte(body)))
	rec := httptest.NewRecorder()
	env.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("con firma válida: esperaba 200, obtuvo %d", rec.Code)
	}
	_ = env.handler.Shutdown(context.Background())
}

func TestWhatsAppMessageDeduplicated(t *testing.T) {
	env := newTestEnv(t, true, "")
	ctx := context.Background()
	env.handler.processIncomingWhatsAppMessage(ctx, "5491155002222", "wamid.dup", "text", "hola", "", "", "", "")
	before := len(env.waLog.ListMessages())
	env.handler.processIncomingWhatsAppMessage(ctx, "5491155002222", "wamid.dup", "text", "hola", "", "", "", "")
	if after := len(env.waLog.ListMessages()); after != before {
		t.Fatalf("el mensaje repetido no debía procesarse de nuevo (%d -> %d mensajes)", before, after)
	}
}

func TestOAuthState(t *testing.T) {
	s := newOAuthStateSigner("0123456789abcdef0123456789abcdef")
	state := s.Issue("tenant-1")
	if _, err := s.Verify(state + "x"); err == nil {
		t.Fatal("debía rechazar un state adulterado")
	}
	if _, err := s.Verify("tenant-victima"); err == nil {
		t.Fatal("debía rechazar un tenant_id sin firmar")
	}
	tenant, err := s.Verify(state)
	if err != nil || tenant != "tenant-1" {
		t.Fatalf("state válido rechazado: %v", err)
	}
	if _, err := s.Verify(state); err == nil {
		t.Fatal("un state no debía poder usarse dos veces")
	}

	env := newTestEnv(t, false, "una-clave-larga-123")
	if rec := env.do(http.MethodGet, "/oauth/mp/callback?code=X&state=11111111-2222-3333-4444-555555555555", "", false); rec.Code != http.StatusBadRequest {
		t.Fatalf("callback con state sin firmar: esperaba 400, obtuvo %d", rec.Code)
	}
}

func TestEndToEndHTTPFlow(t *testing.T) {
	env := newTestEnv(t, true, "")

	// 1. Comprobante con COELSA que todavía no impactó -> entra en la Cola Reactiva
	rec := env.do(http.MethodPost, "/api/v1/simulate/receipt", `{"scenario":"DELAYED_COELSA_QUEUE","amount":27650}`, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("esperaba 200 OK en simulate/receipt, obtuvo %d: %s", rec.Code, rec.Body.String())
	}
	var outDelay domain.VerificationOutcome
	_ = json.NewDecoder(rec.Body).Decode(&outDelay)
	if outDelay.Status != domain.StatusRejectedNotFound || !outDelay.QueuedForRetry || outDelay.MatchCoelsaID == "" {
		t.Fatalf("esperaba REJECTED_NOT_FOUND encolado por COELSA, obtuvo %+v", outDelay)
	}

	// 2. Llega el pago con ese COELSA -> se resuelve la cola y se avisa por WhatsApp
	recWH := env.do(http.MethodPost, "/api/v1/simulate/mp-webhook", `{}`, false)
	if recWH.Code != http.StatusOK {
		t.Fatalf("esperaba 200 OK en simulate/mp-webhook, obtuvo %d", recWH.Code)
	}
	var whResp struct {
		HMACVerified     bool     `json:"hmac_verified"`
		ProactiveActions []string `json:"proactive_actions"`
	}
	_ = json.NewDecoder(recWH.Body).Decode(&whResp)
	if !whResp.HMACVerified || len(whResp.ProactiveActions) == 0 {
		t.Fatalf("esperaba HMAC verificado y acción proactiva, obtuvo %+v", whResp)
	}

	// 3. Orden /cobrar: el pago exacto la marca pagada y avisa al empleado que la pidió
	tenant, _ := env.handler.repo.GetTenantByID(context.Background(), persistence.DefaultDemoTenantID)
	senders, _ := env.handler.repo.ListAuthorizedSenders(context.Background(), tenant.ID)
	order := env.handler.handleCobrarCommand(context.Background(), tenant, senders[1], "/cobrar 10000 Mesa 4")
	if order == nil {
		t.Fatal("no se generó la orden /cobrar")
	}
	recPay := env.do(http.MethodPost, "/api/v1/simulate/mp-webhook", `{"amount":`+jsonNum(order.UniqueAmount)+`}`, false)
	if recPay.Code != http.StatusOK {
		t.Fatalf("pago de orden: esperaba 200, obtuvo %d", recPay.Code)
	}
	found := false
	for _, m := range env.waLog.ListMessages() {
		if m.ToPhone == senders[1].PhoneNumber && strings.Contains(m.Body, "Transferencia *correcta*") && strings.Contains(m.Body, "Mesa 4") {
			found = true
		}
	}
	if !found {
		for _, m := range env.waLog.ListMessages() {
			t.Logf("%s -> %s: %.70q", m.Direction, m.ToPhone, m.Body)
		}
		t.Fatalf("el empleado que pidió /cobrar debía recibir el aviso de pago (orden %+v, respuesta %s)", order, recPay.Body.String())
	}
}

func jsonNum(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

func TestLoginFlow(t *testing.T) {
	env := newTestEnv(t, false, "una-clave-larga-123")

	// La página de login es pública
	if rec := env.do(http.MethodGet, "/login", "", false); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Ingresar") {
		t.Fatalf("GET /login: esperaba 200 con el formulario, obtuvo %d", rec.Code)
	}

	// Contraseña incorrecta: vuelve al login con error y sin cookie
	rec := env.login("admin", "incorrecta", "10.0.0.1:1234")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login?error=1" {
		t.Fatalf("login inválido: esperaba 303 a /login?error=1, obtuvo %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Fatal("login inválido no debe crear sesión")
	}

	// Login correcto: cookie HttpOnly y acceso a la API
	rec = env.login("admin", "una-clave-larga-123", "10.0.0.1:1234")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("login válido: esperaba 303 a /, obtuvo %d %q", rec.Code, rec.Header().Get("Location"))
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie de sesión inválida: %+v", cookie)
	}
	env.cookie = cookie
	if rec := env.do(http.MethodGet, "/api/v1/dashboard", "", true); rec.Code != http.StatusOK {
		t.Fatalf("con sesión: esperaba 200, obtuvo %d", rec.Code)
	}
	if rec := env.do(http.MethodGet, "/", "", true); rec.Code != http.StatusOK {
		t.Fatalf("panel con sesión: esperaba 200, obtuvo %d", rec.Code)
	}

	// Un POST de otro sitio se rechaza aunque tenga la cookie
	req := httptest.NewRequest(http.MethodPost, "/api/v1/plan", strings.NewReader(`{}`))
	req.AddCookie(cookie)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("POST cross-site: esperaba 403, obtuvo %d", rr.Code)
	}

	// Logout invalida la sesión
	if rec := env.do(http.MethodPost, "/logout", "", true); rec.Code != http.StatusSeeOther {
		t.Fatalf("logout: esperaba 303, obtuvo %d", rec.Code)
	}
	if rec := env.do(http.MethodGet, "/api/v1/dashboard", "", true); rec.Code != http.StatusUnauthorized {
		t.Fatalf("después del logout: esperaba 401, obtuvo %d", rec.Code)
	}
}

func TestLoginLockout(t *testing.T) {
	env := newTestEnv(t, false, "una-clave-larga-123")
	for i := 0; i < loginMaxFailures; i++ {
		env.login("admin", "mala", "10.0.0.9:5555")
	}
	// Bloqueada aunque ahora la contraseña sea correcta
	rec := env.login("admin", "una-clave-larga-123", "10.0.0.9:5555")
	if rec.Header().Get("Location") != "/login?error=locked" {
		t.Fatalf("esperaba bloqueo tras %d fallos, obtuvo %q", loginMaxFailures, rec.Header().Get("Location"))
	}
	// Otra IP no queda bloqueada
	rec = env.login("admin", "una-clave-larga-123", "10.0.0.10:5555")
	if rec.Header().Get("Location") != "/" {
		t.Fatalf("otra IP no debería estar bloqueada, obtuvo %q", rec.Header().Get("Location"))
	}
}

func TestDemoWithoutPasswordIsOpen(t *testing.T) {
	env := newTestEnv(t, true, "")
	if rec := env.do(http.MethodGet, "/api/v1/dashboard", "", false); rec.Code != http.StatusOK {
		t.Fatalf("demo sin contraseña: esperaba 200, obtuvo %d", rec.Code)
	}
	if rec := env.do(http.MethodGet, "/login", "", false); rec.Code != http.StatusSeeOther {
		t.Fatalf("demo sin contraseña: /login debería redirigir al panel, obtuvo %d", rec.Code)
	}
}

func TestReceiptUploadIsAsync(t *testing.T) {
	env := newTestEnv(t, true, "una-clave-larga-123")

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("receipt_file", "comprobante.png")
	_, _ = fw.Write([]byte("imagen-de-prueba"))
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/receipts/upload", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.AddCookie(env.sessionCookie())
	rec := httptest.NewRecorder()
	env.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("subida: esperaba 202, obtuvo %d: %s", rec.Code, rec.Body.String())
	}
	var started struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil || started.JobID == "" {
		t.Fatalf("respuesta sin job_id: %s", rec.Body.String())
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		rec := env.do(http.MethodGet, "/api/v1/receipts/upload?job="+started.JobID, "", true)
		var job struct {
			Status  string                      `json:"status"`
			Outcome *domain.VerificationOutcome `json:"outcome"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &job)
		if job.Status == uploadDone {
			if job.Outcome == nil || job.Outcome.Status == "" {
				t.Fatalf("análisis terminado sin resultado: %s", rec.Body.String())
			}
			break
		}
		if job.Status != uploadProcessing {
			t.Fatalf("estado inesperado: %s", rec.Body.String())
		}
		if time.Now().After(deadline) {
			t.Fatal("el análisis no terminó a tiempo")
		}
		time.Sleep(50 * time.Millisecond)
	}

	if rec := env.do(http.MethodGet, "/api/v1/receipts/upload?job=no-existe", "", true); rec.Code != http.StatusNotFound {
		t.Fatalf("job inexistente: esperaba 404, obtuvo %d", rec.Code)
	}
}

func TestNormalizePhone(t *testing.T) {
	cases := map[string]string{
		"+54 9 345 405-4424": "5493454054424",
		"543454054424":       "5493454054424", // celular argentino cargado sin el 9
		"5493454054424":      "5493454054424",
		"+1 (555) 010-9999":  "15550109999",
	}
	for in, want := range cases {
		if got := normalizePhone(in); got != want {
			t.Errorf("normalizePhone(%q) = %q, esperaba %q", in, got, want)
		}
	}
	got := phoneCandidates("5493454054424")
	if len(got) != 2 || got[0] != "5493454054424" || got[1] != "543454054424" {
		t.Errorf("phoneCandidates: %v", got)
	}
}

func TestSenderSavedWithoutNineIsNormalized(t *testing.T) {
	env := newTestEnv(t, false, "una-clave-larga-123")
	rec := env.do(http.MethodPost, "/api/v1/senders", `{"employee_name":"Prueba","phone_number":"+54 345 405 4424","role":"CASHIER"}`, true)
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("alta de empleado: %d %s", rec.Code, rec.Body.String())
	}
	_, sender, err := env.handler.repo.GetTenantBySenderPhone(context.Background(), "5493454054424")
	if err != nil || sender == nil {
		t.Fatalf("el número debería guardarse como 5493454054424: %v", err)
	}
}

func TestWhatsAppRepliesAreOneOfFour(t *testing.T) {
	data := &domain.ExtractedReceiptData{Amount: 12500.5}
	cases := []struct {
		outcome domain.VerificationOutcome
		want    string
	}{
		{domain.VerificationOutcome{Status: domain.StatusVerifiedExactID, ExtractedData: data}, replyCorrect},
		{domain.VerificationOutcome{Status: domain.StatusVerifiedHeuristic, ExtractedData: data}, replyCorrect},
		{domain.VerificationOutcome{Status: domain.StatusRejectedNotFound, ExtractedData: data, QueuedForRetry: true}, replyPending},
		{domain.VerificationOutcome{Status: domain.StatusRejectedNotFound, ExtractedData: data}, replyNotArrived},
		{domain.VerificationOutcome{Status: domain.StatusInvalidReceiptImage}, replyNotArrived},
		{domain.VerificationOutcome{Status: domain.StatusFraudDuplicateFile, ExtractedData: data}, replyFake},
		{domain.VerificationOutcome{Status: domain.StatusFraudAlreadyUsed, ExtractedData: data}, replyFake},
		{domain.VerificationOutcome{Status: domain.StatusFraudIncomplete, ExtractedData: data}, replyFake},
		{domain.VerificationOutcome{Status: domain.StatusRejectedWrongDest, ExtractedData: data}, replyFake},
		{domain.VerificationOutcome{Status: domain.StatusRejectedAmountDiff, ExtractedData: data}, replyFake},
		{domain.VerificationOutcome{Status: domain.StatusQuotaExceeded}, replyCouldNotCheck},
		{domain.VerificationOutcome{Status: domain.StatusMPNotConnected}, replyCouldNotCheck},
	}
	for _, c := range cases {
		got := replyForOutcome(&c.outcome)
		if !strings.HasPrefix(got, c.want) {
			t.Errorf("%s: respuesta %q, esperaba que empiece con %q", c.outcome.Status, got, c.want)
		}
	}
	if got := formatARS(12500.5); got != "$ 12.500,50" {
		t.Errorf("formatARS: %q", got)
	}
	if got := formatARS(999); got != "$ 999,00" {
		t.Errorf("formatARS: %q", got)
	}
}
