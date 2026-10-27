package http

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
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
	tenant := seedTestFixtures(t, repo, vault, cfg.MPWebhookSecret)

	extractor, _ := ai.NewGeminiReceiptExtractor(context.Background(), "", "", demo)
	mpClient := mp.NewMPClient(demo)
	waClient := whatsapp.NewClient("", "")
	verifyUC := application.NewVerifyReceiptUseCase(repo, extractor, mpClient)
	h := NewWebhooksHandler(cfg, repo, vault, extractor, mpClient, waClient, queue.NewRedisQueueStore("", "", 0), verifyUC, tenant.ID)
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
	tenant, _ := env.handler.repo.GetTenantByID(context.Background(), env.handler.tenantID)
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
	if rec := env.do(http.MethodGet, "/login", "", false); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<app-root>") {
		t.Fatalf("GET /login: esperaba 200 con la app del panel, obtuvo %d", rec.Code)
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

	// Cuando la transferencia es correcta e incluye datos del cliente, banco y hora de llegada, se muestran:
	payTime := time.Date(2026, 10, 26, 20, 45, 0, 0, time.Local)
	verified := &domain.VerificationOutcome{
		Status: domain.StatusVerifiedExactID,
		ExtractedData: &domain.ExtractedReceiptData{
			Amount:             18500,
			SenderName:         "Carlos Alberto Gomez",
			SourceBankOrWallet: "Banco Galicia",
		},
		MatchedPayment: &domain.MPPayment{
			Amount:      18500,
			PaymentDate: payTime,
		},
	}
	gotVerified := replyForOutcome(verified)
	for _, expectedPart := range []string{
		"✅ Transferencia *correcta* — $ 18.500,00",
		"👤 Cliente: Carlos Alberto Gomez",
		"🏦 Banco: Banco Galicia",
		"🕒 Hora: 20:45 hs",
	} {
		if !strings.Contains(gotVerified, expectedPart) {
			t.Errorf("respuesta verificada %q no contiene %q", gotVerified, expectedPart)
		}
	}
}

func TestAlreadySentReply(t *testing.T) {
	at := time.Date(2026, 10, 26, 20, 56, 0, 0, time.Local)
	data := &domain.ExtractedReceiptData{Amount: 500}
	got := replyForOutcome(&domain.VerificationOutcome{Status: domain.StatusFraudDuplicateFile, ExtractedData: data, AlreadySentAt: &at})
	for _, part := range []string{"🔁 Comprobante *ya enviado* — $ 500,00", "ya se envió y se validó el 26/10 a las 20:56 hs"} {
		if !strings.Contains(got, part) {
			t.Errorf("respuesta %q no contiene %q", got, part)
		}
	}
	if strings.Contains(got, "reenvialo") {
		t.Error("sin coincidencia solo por emisor no hace falta la aclaración")
	}
	got = replyForOutcome(&domain.VerificationOutcome{Status: domain.StatusFraudDuplicateFile, ExtractedData: data, AlreadySentAt: &at, AlreadySentBySender: true})
	if !strings.Contains(got, "reenvialo cuando se acredite") {
		t.Errorf("falta la aclaración para otra transferencia: %q", got)
	}
}

// El panel de Angular: los archivos estáticos son públicos, las páginas piden sesión.
func TestPanelServing(t *testing.T) {
	env := newTestEnv(t, false, "una-clave-larga-123")
	entries, err := fs.ReadDir(panelFS, ".")
	if err != nil {
		t.Fatal(err)
	}
	var js string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".js") {
			js = e.Name()
			break
		}
	}
	if js == "" {
		t.Fatal("el panel no está compilado (falta correr npm run build en web/)")
	}
	if rec := env.do(http.MethodGet, "/"+js, "", false); rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("GET /%s sin sesión: esperaba 200 cacheable, obtuvo %d", js, rec.Code)
	}
	for _, page := range []string{"/", "/cobrar", "/configuracion"} {
		if rec := env.do(http.MethodGet, page, "", false); rec.Code != http.StatusSeeOther {
			t.Errorf("GET %s sin sesión: esperaba redirección al login, obtuvo %d", page, rec.Code)
		}
		if rec := env.do(http.MethodGet, page, "", true); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<app-root>") {
			t.Errorf("GET %s con sesión: esperaba la app, obtuvo %d", page, rec.Code)
		}
	}
	if rec := env.do(http.MethodGet, "/no-existe.js", "", true); rec.Code != http.StatusNotFound {
		t.Errorf("un archivo inexistente debía dar 404, obtuvo %d", rec.Code)
	}
}

// Historial paginado con filtros y borrado de un comprobante (libera el pago asignado).
func TestValidationsHistoryAndDelete(t *testing.T) {
	env := newTestEnv(t, true, "")
	ctx := context.Background()
	tenantID := env.handler.tenantID

	pay := &domain.MPPayment{TenantID: tenantID, MPPaymentID: 777000111, Amount: 500, Currency: "ARS", MPStatus: "approved",
		ReconciliationStatus: "CONSUMED", ConsumedByReceiptID: "val-ok", PaymentDate: time.Now()}
	if err := env.handler.repo.UpsertMPPayment(ctx, pay); err != nil {
		t.Fatal(err)
	}
	yesterday := time.Now().AddDate(0, 0, -1)
	for i, v := range []*domain.ReceiptValidation{
		{ID: "val-old", ValidationResult: domain.StatusRejectedWrongDest, ExtractedAmount: 100, CreatedAt: yesterday},
		{ID: "val-fraud", ValidationResult: domain.StatusFraudTampered, ExtractedAmount: 353, CreatedAt: time.Now().Add(-time.Minute)},
		{ID: "val-ok", ValidationResult: domain.StatusVerifiedHeuristic, ExtractedAmount: 500, CreatedAt: time.Now()},
	} {
		v.TenantID, v.SenderPhone, v.WAMessageID, v.FileSHA256 = tenantID, "549110", fmt.Sprintf("wamid.h%d", i), fmt.Sprintf("sha%d", i)
		if err := env.handler.repo.SaveReceiptValidation(ctx, v); err != nil {
			t.Fatal(err)
		}
	}

	type page struct {
		Items []struct {
			ID       string `json:"id"`
			Category string `json:"category"`
		} `json:"items"`
		Total int `json:"total"`
	}
	get := func(q string) page {
		t.Helper()
		rec := env.do(http.MethodGet, "/api/v1/validations?"+q, "", true)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET ?%s: %d %s", q, rec.Code, rec.Body.String())
		}
		var p page
		_ = json.Unmarshal(rec.Body.Bytes(), &p)
		return p
	}

	if p := get("page=1&page_size=2"); p.Total < 3 || len(p.Items) != 2 {
		t.Fatalf("paginación: total=%d items=%d", p.Total, len(p.Items))
	}
	if p := get("category=fraud&date=" + time.Now().Format("2006-01-02")); p.Total != 1 || p.Items[0].ID != "val-fraud" || p.Items[0].Category != "fraud" {
		t.Fatalf("filtro fraude de hoy: %+v", p)
	}
	if p := get("date=" + yesterday.Format("2006-01-02")); p.Total != 1 || p.Items[0].Category != "rejected" {
		t.Fatalf("filtro de ayer: %+v", p)
	}
	if rec := env.do(http.MethodGet, "/api/v1/validations?date=26-10-2026", "", true); rec.Code != http.StatusBadRequest {
		t.Fatalf("una fecha inválida debía dar 400, obtuvo %d", rec.Code)
	}

	rec := env.do(http.MethodDelete, "/api/v1/validations?id=val-ok", "", true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"payment_released":true`) {
		t.Fatalf("DELETE: %d %s", rec.Code, rec.Body.String())
	}
	payments, _ := env.handler.repo.ListMPPayments(ctx, tenantID, 100)
	for _, p := range payments {
		if p.MPPaymentID == 777000111 && p.ReconciliationStatus != "AVAILABLE" {
			t.Fatalf("el pago debía quedar libre, está %s", p.ReconciliationStatus)
		}
	}
	if rec := env.do(http.MethodDelete, "/api/v1/validations?id=val-ok", "", true); rec.Code != http.StatusNotFound {
		t.Fatalf("borrar dos veces debía dar 404, obtuvo %d", rec.Code)
	}
	if rec := env.do(http.MethodGet, "/api/v1/export/validations.csv?category=fraud", "", true); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "val-fraud") || strings.Contains(rec.Body.String(), "val-old") {
		t.Fatalf("el CSV debía respetar el filtro: %d", rec.Code)
	}
}

// seedTestFixtures arma un comercio de prueba con Mercado Pago vinculado, tres empleados y dos
// pagos acreditados. Estos datos existen solo dentro de los tests.
func seedTestFixtures(t *testing.T, repo *persistence.MySQLRepository, vault *crypto.TokenVault, webhookSecret string) *domain.Tenant {
	t.Helper()
	ctx := context.Background()
	tenant, err := repo.EnsureTenant(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.UpdateTenantProfile(ctx, tenant.ID, "Comercio de prueba", "", ""); err != nil {
		t.Fatal(err)
	}
	spec := domain.GetPlanSpecs()[domain.PlanPro]
	if err := repo.UpdateTenantPlan(ctx, tenant.ID, spec.Plan, spec.MonthlyQuota, 120); err != nil {
		t.Fatal(err)
	}
	encAcc, _ := vault.Encrypt("DEMO_TOKEN")
	encRef, _ := vault.Encrypt("DEMO_REFRESH")
	now := time.Now()
	if err := repo.UpsertMPAccount(ctx, &domain.MPAccount{
		TenantID: tenant.ID, MPUserID: 1492837465, ExpectedAlias: "comercio.prueba.mp", ExpectedCVU: "0000003100084920194829",
		AccessTokenEnc: encAcc, RefreshTokenEnc: encRef, TokenExpiresAt: now.Add(6 * time.Hour), WebhookSecret: webhookSecret,
		Status: "CONNECTED", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	for _, s := range []*domain.AuthorizedSender{
		{PhoneNumber: "5491155001111", EmployeeName: "Dueño de prueba", Role: domain.RoleOwner},
		{PhoneNumber: "5491155002222", EmployeeName: "Cajera de prueba", Role: domain.RoleCashier},
		{PhoneNumber: "5491155003333", EmployeeName: "Repartidor de prueba", Role: domain.RoleDelivery},
	} {
		s.TenantID, s.IsActive, s.CreatedAt = tenant.ID, true, now
		if err := repo.CreateAuthorizedSender(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []*domain.MPPayment{
		{MPPaymentID: 88492019482, Amount: 15450.50, OperationType: "money_transfer", PayerCUIT: "20348912349", PayerName: "Juan Ignacio Perez", CoelsaID: "2410200000149281746391", PaymentDate: now.Add(-5 * time.Minute)},
		{MPPaymentID: 88492019999, Amount: 24800, OperationType: "bank_transfer", PayerCUIT: "27319482714", PayerName: "Maria Florencia Sosa", CoelsaID: "2410200000998877665544", PaymentDate: now.Add(-8 * time.Minute)},
	} {
		p.TenantID, p.Currency, p.MPStatus = tenant.ID, "ARS", "approved"
		if err := repo.UpsertMPPayment(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	return tenant
}

func TestCustomersAndBusinessAPI(t *testing.T) {
	env := newTestEnv(t, true, "")
	ctx := context.Background()

	// Los pagos de prueba crearon a sus clientes (por el CUIT del pagador)
	rec := env.do(http.MethodGet, "/api/v1/customers?q=juan", "", true)
	var page struct {
		Items []struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			CUIT     string `json:"cuit"`
			Payments int    `json:"payments"`
		} `json:"items"`
		Total int `json:"total"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &page)
	if rec.Code != http.StatusOK || page.Total != 1 || page.Items[0].CUIT != "20348912349" || page.Items[0].Payments != 1 {
		t.Fatalf("GET /customers?q=juan: %d %s", rec.Code, rec.Body.String())
	}
	if rec := env.do(http.MethodGet, "/api/v1/customers?id="+page.Items[0].ID, "", true); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Juan Ignacio Perez") {
		t.Fatalf("GET /customers?id: %d %s", rec.Code, rec.Body.String())
	}
	if rec := env.do(http.MethodGet, "/api/v1/customers?id=no-existe", "", true); rec.Code != http.StatusNotFound {
		t.Fatalf("un cliente inexistente debía dar 404, obtuvo %d", rec.Code)
	}
	if rec := env.do(http.MethodGet, "/api/v1/customers?risk=fraud", "", true); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"total":0`) {
		t.Fatalf("sin fraudes la lista debía estar vacía: %s", rec.Body.String())
	}

	// Datos del comercio
	if rec := env.do(http.MethodPost, "/api/v1/business", `{"business_name":"Mi Negocio","cuit":"30-71649281-9","email":"hola@minegocio.com"}`, true); rec.Code != http.StatusOK {
		t.Fatalf("POST /business: %d %s", rec.Code, rec.Body.String())
	}
	tenant, _ := env.handler.repo.GetTenantByID(ctx, env.handler.tenantID)
	if tenant.BusinessName != "Mi Negocio" || tenant.CUIT != "30716492819" || tenant.Email != "hola@minegocio.com" {
		t.Fatalf("datos guardados: %+v", tenant)
	}
	for _, body := range []string{`{"business_name":""}`, `{"business_name":"X","cuit":"123"}`, `{"business_name":"X","email":"no-es-email"}`} {
		if rec := env.do(http.MethodPost, "/api/v1/business", body, true); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: esperaba 400, obtuvo %d", body, rec.Code)
		}
	}
}
