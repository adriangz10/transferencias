package http

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
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

const (
	maxBackgroundWorkers = 16               // procesamientos de webhooks en paralelo
	workerAcquireTimeout = 3 * time.Second  // si no hay lugar, respondemos 503 y el emisor reintenta
	waBodyLimit          = 2 << 20          // 2 MB
	mpBodyLimit          = 64 << 10         // 64 KB
	uploadLimit          = 15 << 20         // 15 MB
	seenMessagesTTL      = 24 * time.Hour   // ventana de deduplicación de mensajes de WhatsApp
	waProcessTimeout     = 60 * time.Second // descarga + Gemini + MP
	mpProcessTimeout     = 20 * time.Second
)

var (
	phoneRegex = regexp.MustCompile(`^\d{8,15}$`)
	aliasRegex = regexp.MustCompile(`^[a-z0-9.\-]{6,20}$`)
	cvuRegex   = regexp.MustCompile(`^\d{22}$`)
)

type WebhooksHandler struct {
	// tenantID es el comercio de esta instalación (se crea o se lee de la base al arrancar)
	tenantID   string
	cfg        *config.Config
	repo       *persistence.MySQLRepository
	vault      *crypto.TokenVault
	extractor  *ai.GeminiReceiptExtractor
	mpClient   *mp.MPClient
	waClient   *whatsapp.Client
	queue      *queue.RedisQueueStore
	verifyUC   *application.VerifyReceiptUseCase
	oauthState *oauthStateSigner
	sessions   *sessionStore
	uploads    *uploadJobs

	// Procesamiento en segundo plano acotado y con apagado ordenado
	workers    chan struct{}
	wg         sync.WaitGroup
	baseCtx    context.Context
	cancelBase context.CancelFunc

	// Deduplicación de mensajes de WhatsApp (Meta puede entregar el mismo webhook dos veces)
	seenMu   sync.Mutex
	seenMsgs map[string]time.Time
}

func NewWebhooksHandler(
	cfg *config.Config,
	repo *persistence.MySQLRepository,
	vault *crypto.TokenVault,
	extractor *ai.GeminiReceiptExtractor,
	mpClient *mp.MPClient,
	waClient *whatsapp.Client,
	queueStore *queue.RedisQueueStore,
	verifyUC *application.VerifyReceiptUseCase,
	tenantID string,
) *WebhooksHandler {
	baseCtx, cancel := context.WithCancel(context.Background())
	return &WebhooksHandler{
		tenantID:   tenantID,
		cfg:        cfg,
		repo:       repo,
		vault:      vault,
		extractor:  extractor,
		mpClient:   mpClient,
		waClient:   waClient,
		queue:      queueStore,
		verifyUC:   verifyUC,
		oauthState: newOAuthStateSigner(cfg.MasterKey),
		sessions:   newSessionStore(),
		uploads:    newUploadJobs(),
		workers:    make(chan struct{}, maxBackgroundWorkers),
		baseCtx:    baseCtx,
		cancelBase: cancel,
		seenMsgs:   make(map[string]time.Time),
	}
}

func (h *WebhooksHandler) RegisterRoutes(mux *http.ServeMux) {
	admin := h.requireAdmin

	// Webhooks oficiales (autenticados por firma HMAC, no por contraseña)
	mux.HandleFunc("/webhooks/whatsapp", h.handleWhatsAppWebhook)
	mux.HandleFunc("/webhooks/mercadopago", h.handleMPWebhook)

	// OAuth 2.0 con Mercado Pago (el callback se valida con el state firmado)
	mux.HandleFunc("/oauth/mp/connect", admin(h.handleMPOAuthConnect))
	mux.HandleFunc("/oauth/mp/callback", h.handleMPOAuthCallback)

	// API del panel (requiere ADMIN_USER / ADMIN_PASSWORD)
	mux.HandleFunc("/api/v1/dashboard", admin(h.handleDashboardData))
	mux.HandleFunc("/api/v1/senders", admin(h.handleSenders))
	mux.HandleFunc("/api/v1/account", admin(h.handleAccount))
	mux.HandleFunc("/api/v1/plan", admin(h.handleUpdatePlan))
	mux.HandleFunc("/api/v1/posnet/charge", admin(h.handleCreatePosnetCharge))
	mux.HandleFunc("/api/v1/receipts/upload", admin(h.handleReceiptUpload))
	mux.HandleFunc("/api/v1/export/validations.csv", admin(h.handleExportValidationsCSV))
	mux.HandleFunc("/api/v1/validations", admin(h.handleValidations))
	mux.HandleFunc("/api/v1/customers", admin(h.handleCustomers))
	mux.HandleFunc("/api/v1/business", admin(h.handleBusiness))

	// Simulador: solo con DEMO_MODE=true
	mux.HandleFunc("/api/v1/simulate/receipt", admin(h.demoOnly(h.handleSimulateReceipt)))
	mux.HandleFunc("/api/v1/simulate/mp-webhook", admin(h.demoOnly(h.handleSimulateMPWebhook)))
	mux.HandleFunc("/api/v1/simulate/whatsapp-command", admin(h.demoOnly(h.handleSimulateWACommand)))

	mux.HandleFunc("/health", h.handleHealth)

	// Login del panel
	mux.HandleFunc("/login", h.handleLogin)
	mux.HandleFunc("/api/v1/login", h.handleLogin) // el formulario de Angular envía acá (también en desarrollo)
	mux.HandleFunc("/logout", h.handleLogout)

	// Panel Web (Angular). Los archivos estáticos son públicos; las páginas piden sesión.
	mux.HandleFunc("/", h.handleWebUI)
}

// Shutdown espera a que terminen los procesamientos en curso (o cancela al vencer ctx).
func (h *WebhooksHandler) Shutdown(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		h.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		h.cancelBase()
		return nil
	case <-ctx.Done():
		h.cancelBase()
		return ctx.Err()
	}
}

// pendingSweepInterval es cada cuánto se revisan los comprobantes cuya espera venció.
const pendingSweepInterval = 15 * time.Second

// StartBackgroundJobs arranca las tareas periódicas. Terminan al llamar a Shutdown.
func (h *WebhooksHandler) StartBackgroundJobs() {
	go func() {
		ticker := time.NewTicker(pendingSweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-h.baseCtx.Done():
				return
			case <-ticker.C:
				h.notifyExpiredPending(h.baseCtx)
			}
		}
	}()
}

// notifyExpiredPending avisa "no ingresó" por cada comprobante que esperó 3 minutos sin que
// llegara el pago a Mercado Pago.
func (h *WebhooksHandler) notifyExpiredPending(ctx context.Context) {
	items, err := h.queue.TakeExpiredPendingReceipts(ctx)
	if err != nil {
		log.Printf("⚠️ No se pudo revisar la cola de espera: %v", err)
		return
	}
	for _, item := range items {
		_ = h.sendWA(ctx, item.SenderPhone, transferReply(replyNotArrived, item.ExpectedAmount)+pendingFrom(item))
	}
}

// acquireWorker reserva un lugar para procesar en segundo plano.
func (h *WebhooksHandler) acquireWorker() bool {
	select {
	case h.workers <- struct{}{}:
		return true
	case <-time.After(workerAcquireTimeout):
		return false
	}
}

// runInBackground ejecuta fn con un lugar ya reservado por acquireWorker.
func (h *WebhooksHandler) runInBackground(timeout time.Duration, fn func(ctx context.Context)) {
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		defer func() { <-h.workers }()
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("🔥 panic procesando webhook: %v", rec)
			}
		}()
		ctx, cancel := context.WithTimeout(h.baseCtx, timeout)
		defer cancel()
		fn(ctx)
	}()
}

// markMessageSeen devuelve true si el mensaje ya se estaba procesando o se procesó.
func (h *WebhooksHandler) markMessageSeen(waMsgID string) bool {
	if waMsgID == "" {
		return false
	}
	h.seenMu.Lock()
	defer h.seenMu.Unlock()
	now := time.Now()
	for id, t := range h.seenMsgs {
		if now.Sub(t) > seenMessagesTTL {
			delete(h.seenMsgs, id)
		}
	}
	if _, ok := h.seenMsgs[waMsgID]; ok {
		return true
	}
	h.seenMsgs[waMsgID] = now
	return false
}

func (h *WebhooksHandler) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":       "ok",
		"service":      "veripago-saas",
		"demo_mode":    h.cfg.DemoMode,
		"mysql_active": h.repo.IsUsingMySQL(),
		"redis_active": h.queue.IsUsingRedis(),
		"gemini_live":  h.extractor.HasLiveAPIKey(),
	})
}

// ---------------------------------------------------------------------------
// WhatsApp
// ---------------------------------------------------------------------------

type waMessage struct {
	ID   string `json:"id"`
	From string `json:"from"`
	Type string `json:"type"`
	Text struct {
		Body string `json:"body"`
	} `json:"text"`
	Image struct {
		ID       string `json:"id"`
		MimeType string `json:"mime_type"`
	} `json:"image"`
	Document struct {
		ID       string `json:"id"`
		MimeType string `json:"mime_type"`
	} `json:"document"`
}

// handleWhatsAppWebhook procesa el challenge GET de Meta y los eventos POST de mensajes/comprobantes.
func (h *WebhooksHandler) handleWhatsAppWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		q := r.URL.Query()
		token := q.Get("hub.verify_token")
		if q.Get("hub.mode") == "subscribe" && h.cfg.WAVerifyToken != "" &&
			subtle.ConstantTimeCompare([]byte(token), []byte(h.cfg.WAVerifyToken)) == 1 {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(q.Get("hub.challenge")))
			return
		}
		http.Error(w, "Forbidden: invalid verify_token", http.StatusForbidden)
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	rawBody, err := io.ReadAll(http.MaxBytesReader(w, r.Body, waBodyLimit))
	if err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	// La firma X-Hub-Signature-256 es OBLIGATORIA (solo se omite en modo demo sin app secret)
	if !(h.cfg.DemoMode && h.cfg.WAAppSecret == "") {
		if !ValidateWhatsAppSignature(h.cfg.WAAppSecret, r.Header.Get("X-Hub-Signature-256"), rawBody) {
			http.Error(w, "Invalid X-Hub-Signature-256", http.StatusUnauthorized)
			return
		}
	}

	var payload struct {
		Entry []struct {
			Changes []struct {
				Value struct {
					Messages []waMessage `json:"messages"`
				} `json:"value"`
			} `json:"changes"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(rawBody, &payload); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	var msgs []waMessage
	for _, entry := range payload.Entry {
		for _, change := range entry.Changes {
			msgs = append(msgs, change.Value.Messages...)
		}
	}
	if len(msgs) == 0 { // notificaciones de estado (entregado/leído)
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}

	if !h.acquireWorker() {
		http.Error(w, "Busy, retry later", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "received"})

	h.runInBackground(waProcessTimeout, func(ctx context.Context) {
		for _, msg := range msgs {
			h.processIncomingWhatsAppMessage(ctx, msg.From, msg.ID, msg.Type, msg.Text.Body, msg.Image.ID, msg.Image.MimeType, msg.Document.ID, msg.Document.MimeType)
		}
	})
}

func (h *WebhooksHandler) processIncomingWhatsAppMessage(
	ctx context.Context,
	fromPhone, waMsgID, msgType, textBody, imageID, imageMime, docID, docMime string,
) {
	if h.markMessageSeen(waMsgID) {
		return // entrega duplicada de Meta
	}

	var (
		tenant *domain.Tenant
		sender *domain.AuthorizedSender
		err    error
	)
	for _, candidate := range phoneCandidates(fromPhone) {
		if tenant, sender, err = h.repo.GetTenantBySenderPhone(ctx, candidate); err == nil && tenant != nil && sender != nil {
			break
		}
	}
	if err != nil || tenant == nil || sender == nil {
		log.Printf("⛔ Mensaje de un número no registrado: %s", maskPhone(normalizePhone(fromPhone)))
		_ = h.sendWA(ctx, fromPhone, "⛔ *Número no autorizado*\nPara usar el bot, el dueño tiene que cargar este número en el panel (Configuración → Personas autorizadas):\n*"+normalizePhone(fromPhone)+"*")
		return
	}
	// Respondemos siempre al número tal como lo informa WhatsApp (aunque se haya cargado sin el 9)
	senderCopy := *sender
	senderCopy.PhoneNumber = normalizePhone(fromPhone)
	sender = &senderCopy

	if msgType == "text" {
		h.waClient.RecordInboundMessage(fromPhone, sender.EmployeeName, textBody)
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(textBody)), "/cobrar") {
			h.handleCobrarCommand(ctx, tenant, sender, textBody)
			return
		}
		_ = h.sendWA(ctx, fromPhone, "👋 Hola *"+sender.EmployeeName+"*.\n• Reenviame la *foto o PDF del comprobante* para validarlo en 2 segundos.\n• O enviá `/cobrar 10000` para generar un monto con pesos únicos.")
		return
	}

	var mediaID, mimeType string
	switch msgType {
	case "image":
		mediaID, mimeType = imageID, imageMime
	case "document":
		mediaID, mimeType = docID, docMime
	default:
		return
	}

	// Si ya hay auditoría para este mensaje (por ejemplo tras un reinicio), no se reprocesa
	if exists, err := h.repo.ExistsValidationByWAMessageID(ctx, waMsgID); err == nil && exists {
		return
	}

	fileBytes, downloadedMime, err := h.waClient.DownloadMedia(ctx, mediaID)
	if err != nil {
		log.Printf("⚠️ Error descargando media %s: %v", mediaID, err)
		_ = h.sendWA(ctx, sender.PhoneNumber, replyCouldNotCheck)
		return
	}
	if downloadedMime != "" {
		mimeType = downloadedMime
	}

	if _, err := h.processReceiptForTenant(ctx, tenant, sender, waMsgID, fileBytes, mimeType); err != nil {
		log.Printf("⚠️ Error procesando comprobante %s: %v", waMsgID, err)
	}
}

// chargeDestination devuelve el alias (o CVU) que se le indica al cliente para transferir.
func (h *WebhooksHandler) chargeDestination(acc *domain.MPAccount) string {
	if acc != nil && acc.ExpectedAlias != "" {
		return acc.ExpectedAlias
	}
	if acc != nil && acc.ExpectedCVU != "" {
		return acc.ExpectedCVU
	}
	return ""
}

func (h *WebhooksHandler) handleCobrarCommand(ctx context.Context, tenant *domain.Tenant, sender *domain.AuthorizedSender, textBody string) *domain.UniqueChargeOrder {
	parts := strings.Fields(strings.TrimSpace(textBody))
	if len(parts) < 2 {
		_ = h.sendWA(ctx, sender.PhoneNumber, "💡 *Uso del comando /cobrar:*\nEscribí `/cobrar <monto>` (ej: `/cobrar 10000`) para generar un importe con pesos únicos inconfundibles.")
		return nil
	}

	baseAmt, ok := parseARSAmount(parts[1])
	if !ok {
		_ = h.sendWA(ctx, sender.PhoneNumber, "⚠️ Monto inválido. Ejemplo válido: `/cobrar 10000`")
		return nil
	}

	acc, _ := h.repo.GetActiveMPAccount(ctx, tenant.ID)
	dest := h.chargeDestination(acc)
	if dest == "" {
		_ = h.sendWA(ctx, sender.PhoneNumber, "⚠️ El comercio todavía no configuró el alias o CVU de cobro. Pedile al dueño que lo cargue en el panel.")
		return nil
	}

	ref := "Mostrador / WhatsApp"
	if len(parts) > 2 {
		ref = strings.Join(parts[2:], " ")
	}

	order, err := h.queue.CreateUniqueChargeOrder(ctx, tenant.ID, sender.EmployeeName, sender.PhoneNumber, ref, dest, baseAmt)
	if err != nil {
		msg := "⚠️ No se pudo generar la orden de cobro. Probá de nuevo en unos segundos."
		if errors.Is(err, queue.ErrNoUniqueAmountAvailable) {
			msg = "⚠️ Hay demasiadas órdenes abiertas por ese mismo importe. Esperá a que se paguen o venzan (15 min)."
		}
		_ = h.sendWA(ctx, sender.PhoneNumber, msg)
		return nil
	}

	msg := fmt.Sprintf(
		"🏷️ *ORDEN DE COBRO IDENTIFICADA (/cobrar)*\n\nPedile al cliente que transfiera exactamente:\n👉 *$%.2f*\n\n• *Destino:* `%s`\n• *Referencia:* %s\n• *Vigencia:* 15 minutos\n\n⚡ _Te aviso por acá apenas impacte el pago._",
		order.UniqueAmount, dest, ref,
	)
	_ = h.sendWA(ctx, sender.PhoneNumber, msg)
	return order
}

// parseARSAmount interpreta montos como "10000", "$10.000" o "10.000,50".
func parseARSAmount(raw string) (float64, bool) {
	s := strings.ReplaceAll(strings.TrimSpace(raw), "$", "")
	s = strings.ReplaceAll(s, ".", "")
	s = strings.ReplaceAll(s, ",", ".")
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v <= 0 || v > 1e12 {
		return 0, false
	}
	return v, true
}

// accessTokenFor desencripta el token de MP de la cuenta y lo renueva si venció.
func (h *WebhooksHandler) accessTokenFor(ctx context.Context, acc *domain.MPAccount) (string, error) {
	if acc == nil {
		return "", nil
	}
	accessToken, err := h.vault.Decrypt(acc.AccessTokenEnc)
	if err != nil {
		return "", fmt.Errorf("no se pudo desencriptar el token de MP (¿cambió MASTER_KEY?): %w", err)
	}
	if time.Now().Before(acc.TokenExpiresAt) || acc.RefreshTokenEnc == "" {
		return accessToken, nil
	}

	refreshDec, err := h.vault.Decrypt(acc.RefreshTokenEnc)
	if err != nil {
		return accessToken, nil
	}
	refreshed, err := h.mpClient.RefreshAccessToken(ctx, h.cfg.MPClientID, h.cfg.MPClientSecret, refreshDec)
	if err != nil {
		log.Printf("⚠️ No se pudo renovar el token de MP de la cuenta %s: %v", acc.ID, err)
		return accessToken, nil
	}
	if enc, err := h.vault.Encrypt(refreshed.AccessToken); err == nil {
		acc.AccessTokenEnc = enc
	}
	if refreshed.RefreshToken != "" {
		if enc, err := h.vault.Encrypt(refreshed.RefreshToken); err == nil {
			acc.RefreshTokenEnc = enc
		}
	}
	acc.TokenExpiresAt = time.Now().Add(time.Duration(refreshed.ExpiresIn) * time.Second)
	if err := h.repo.UpsertMPAccount(ctx, acc); err != nil {
		log.Printf("⚠️ No se pudo guardar el token renovado: %v", err)
	}
	return refreshed.AccessToken, nil
}

func (h *WebhooksHandler) processReceiptForTenant(
	ctx context.Context,
	tenant *domain.Tenant,
	sender *domain.AuthorizedSender,
	waMsgID string,
	fileBytes []byte,
	mimeType string,
) (*domain.VerificationOutcome, error) {
	// Por WhatsApp solo se responde uno de los 4 resultados (ver replies.go);
	// el detalle queda en MessageForUser para el panel.
	reply := func(o *domain.VerificationOutcome) (*domain.VerificationOutcome, error) {
		_ = h.sendWA(ctx, sender.PhoneNumber, replyForOutcome(o))
		return o, nil
	}

	// Límite mensual del plan
	if tenant.UsedQuota >= tenant.MonthlyQuota {
		return reply(&domain.VerificationOutcome{
			Status: domain.StatusQuotaExceeded,
			MessageForUser: fmt.Sprintf("⚠️ *Límite mensual alcanzado (%d/%d comprobantes)*.\nActualizá tu plan en el panel de VeriPago para seguir validando.",
				tenant.UsedQuota, tenant.MonthlyQuota),
		})
	}

	acc, _ := h.repo.GetActiveMPAccount(ctx, tenant.ID)
	if acc == nil && !h.cfg.DemoMode {
		return reply(&domain.VerificationOutcome{
			Status:         domain.StatusMPNotConnected,
			MessageForUser: "⚠️ *Mercado Pago no está vinculado*\nEl dueño del comercio tiene que conectar la cuenta desde el panel de VeriPago antes de validar comprobantes.",
		})
	}

	accessToken, err := h.accessTokenFor(ctx, acc)
	if err != nil {
		log.Printf("⚠️ %v", err)
	}
	var dest application.Destination
	if acc != nil {
		dest = application.Destination{Alias: acc.ExpectedAlias, CVU: acc.ExpectedCVU}
	}

	outcome, err := h.verifyUC.ExecuteAndAudit(ctx, tenant.ID, sender.PhoneNumber, waMsgID, accessToken, dest, tenant.TimeWindowMinutes, fileBytes, mimeType)
	if err != nil {
		_ = h.sendWA(ctx, sender.PhoneNumber, replyCouldNotCheck)
		return nil, err
	}

	switch outcome.Status {
	case domain.StatusRejectedNotFound:
		// Cola de Espera Reactiva ("Te aviso apenas entre") durante 3 minutos
		if outcome.ExtractedData != nil && outcome.ExtractedData.Amount > 0 {
			qItem := &domain.PendingReceiptQueueItem{
				ValidationID:       outcome.ValidationID,
				TenantID:           tenant.ID,
				SenderPhone:        sender.PhoneNumber,
				EmployeeName:       sender.EmployeeName,
				ExpectedAmount:     outcome.ExtractedData.Amount,
				OperationNumber:    outcome.ExtractedData.OperationNumber,
				CoelsaID:           outcome.MatchCoelsaID,
				MPPaymentID:        outcome.MatchMPPaymentID,
				MatchByAmount:      outcome.MatchByAmount,
				SourceBankOrWallet: outcome.ExtractedData.SourceBankOrWallet,
				SenderName:         outcome.ExtractedData.SenderName,
				SenderCUIT:         outcome.ExtractedData.SenderCUIT,
				FileSHA256:         outcome.FileSHA256,
			}
			if err := h.queue.EnqueuePendingReceipt(ctx, qItem); err != nil {
				log.Printf("⚠️ No se pudo encolar el comprobante %s: %v", outcome.ValidationID, err)
			} else {
				outcome.QueuedForRetry = true
			}
		}

	case domain.StatusVerifiedExactID, domain.StatusVerifiedHeuristic:
		// Si la misma foto estaba esperando en la cola (reenvío), ya no hace falta
		_ = h.queue.RemovePendingByFileHash(ctx, tenant.ID, outcome.FileSHA256)
		if outcome.MatchedPayment != nil {
			_, _ = h.queue.MatchChargeOrder(ctx, tenant.ID, outcome.MatchedPayment.Amount, outcome.MatchedPayment.MPPaymentID)
		}
	}

	_ = h.sendWA(ctx, sender.PhoneNumber, replyForOutcome(outcome))

	// Alerta al dueño si es fraude y el plan lo incluye (no si el pago ya era de una orden /cobrar,
	// ni si lo subió el propio dueño desde el panel)
	if isFraudStatus(outcome.Status) && !outcome.ConsumedByChargeOrder && sender.PhoneNumber != panelSenderPhone {
		if spec, ok := domain.GetPlanSpecs()[tenant.PlanType]; ok && spec.OwnerFraudAlerts {
			h.notifyOwnersOfFraud(ctx, tenant.ID, sender, outcome)
		}
	}

	return outcome, nil
}

// panelSenderPhone identifica los comprobantes subidos desde el panel web: el resultado se ve
// en pantalla, así que no se manda por WhatsApp.
const panelSenderPhone = "panel"

var panelSender = &domain.AuthorizedSender{PhoneNumber: panelSenderPhone, EmployeeName: "Panel web", Role: domain.RoleOwner, IsActive: true}

// sendWA manda un mensaje de WhatsApp, salvo a los comprobantes subidos desde el panel.
func (h *WebhooksHandler) sendWA(ctx context.Context, phone, text string) error {
	if phone == panelSenderPhone || phone == "" {
		return nil
	}
	err := h.waClient.SendTextMessage(ctx, phone, text)
	if err != nil {
		log.Printf("⚠️ No se pudo enviar WhatsApp a %s: %v", maskPhone(phone), err)
	}
	return err
}

// maskPhone deja ver solo los últimos 4 dígitos en los logs.
func maskPhone(p string) string {
	if len(p) <= 4 {
		return p
	}
	return strings.Repeat("*", len(p)-4) + p[len(p)-4:]
}

// normalizePhone deja solo dígitos y, para celulares de Argentina cargados sin el 9
// (54 + 10 dígitos), lo agrega: WhatsApp siempre informa el remitente como 549...
func normalizePhone(raw string) string {
	var b strings.Builder
	for _, r := range raw {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	p := b.String()
	if len(p) == 12 && strings.HasPrefix(p, "54") && !strings.HasPrefix(p, "549") {
		p = "549" + p[2:]
	}
	return p
}

// phoneCandidates devuelve las formas en que un número pudo haberse guardado
// (con y sin el 9 de Argentina) para encontrar al empleado.
func phoneCandidates(from string) []string {
	p := normalizePhone(from)
	out := []string{p}
	if len(p) == 13 && strings.HasPrefix(p, "549") {
		out = append(out, "54"+p[3:])
	}
	return out
}

func isFraudStatus(s domain.ValidationStatus) bool {
	return s == domain.StatusFraudDuplicateFile ||
		s == domain.StatusFraudAlreadyUsed ||
		s == domain.StatusFraudIncomplete ||
		s == domain.StatusFraudTampered
}

func (h *WebhooksHandler) notifyOwnersOfFraud(ctx context.Context, tenantID string, triggeredBy *domain.AuthorizedSender, outcome *domain.VerificationOutcome) {
	senders, err := h.repo.ListAuthorizedSenders(ctx, tenantID)
	if err != nil {
		return
	}
	for _, s := range senders {
		if s.Role == domain.RoleOwner && s.IsActive && s.PhoneNumber != triggeredBy.PhoneNumber {
			alertMsg := replyForOutcome(outcome) + "\nLa recibió " + triggeredBy.EmployeeName + "."
			_ = h.sendWA(ctx, s.PhoneNumber, alertMsg)
		}
	}
}

// ---------------------------------------------------------------------------
// Mercado Pago
// ---------------------------------------------------------------------------

// handleMPWebhook procesa notificaciones de Mercado Pago tras validar la firma x-signature.
func (h *WebhooksHandler) handleMPWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var body struct {
		Action string `json:"action"`
		Type   string `json:"type"`
		UserID int64  `json:"user_id"`
		Data   struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, mpBodyLimit)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	// El ID para consultar el pago; la firma usa SIEMPRE el data.id de la URL
	dataID := r.URL.Query().Get("data.id")
	if dataID == "" {
		dataID = body.Data.ID
	}

	// Secreto específico de la cuenta (si lo tiene) o el global
	webhookSecret := h.cfg.MPWebhookSecret
	acc, _ := h.repo.GetMPAccountByMPUserID(r.Context(), body.UserID)
	if acc != nil && acc.WebhookSecret != "" {
		webhookSecret = acc.WebhookSecret
	}
	if !ValidateMPWebhookSignature(webhookSecret, r) {
		http.Error(w, "Invalid x-signature HMAC", http.StatusUnauthorized)
		return
	}

	topic := body.Type
	if topic == "" {
		topic = r.URL.Query().Get("type")
	}
	if topic == "" {
		topic = r.URL.Query().Get("topic")
	}
	if (topic != "" && topic != "payment") || dataID == "" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}

	if acc == nil {
		if h.cfg.DemoMode {
			acc, _ = h.repo.GetActiveMPAccount(r.Context(), h.tenantID)
		}
		if acc == nil {
			log.Printf("ℹ️ Webhook MP para user_id %d sin cuenta vinculada: ignorado", body.UserID)
			writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
			return
		}
	}

	if !h.acquireWorker() {
		http.Error(w, "Busy, retry later", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})

	h.runInBackground(mpProcessTimeout, func(ctx context.Context) {
		accessToken, err := h.accessTokenFor(ctx, acc)
		if err != nil {
			log.Printf("⚠️ %v", err)
			return
		}
		payment, err := h.mpClient.GetPaymentByID(ctx, accessToken, acc.TenantID, dataID)
		if err != nil {
			log.Printf("⚠️ Webhook MP: no se pudo consultar el pago %s: %v", dataID, err)
			return
		}
		if payment == nil || payment.MPStatus != "approved" {
			return // pago saliente, inexistente o todavía no aprobado
		}
		if _, err := h.ingestMPPaymentAndResolveQueue(ctx, payment); err != nil {
			log.Printf("⚠️ Webhook MP: error ingresando el pago %s: %v", dataID, err)
		}
	})
}

// ingestMPPaymentAndResolveQueue guarda el pago en el Libro Mayor y resuelve las órdenes /cobrar
// y los comprobantes que estaban esperando ese pago en la Cola de Espera Reactiva.
func (h *WebhooksHandler) ingestMPPaymentAndResolveQueue(ctx context.Context, payment *domain.MPPayment) ([]string, error) {
	if err := h.repo.UpsertMPPayment(ctx, payment); err != nil {
		return nil, err
	}

	var notifications []string

	// 1. ¿Hay una orden /cobrar esperando exactamente este monto único?
	if order, err := h.queue.MatchChargeOrder(ctx, payment.TenantID, payment.Amount, payment.MPPaymentID); err == nil && order != nil {
		_, claimErr := h.repo.ClaimPaymentByExactID(ctx, payment.TenantID, payment.MPPaymentID, payment.Amount, order.ID)
		if claimErr == nil {
			notifications = append(notifications, fmt.Sprintf("⚡ Orden /cobrar pagada: $%.2f (MP #%d - %s)", payment.Amount, payment.MPPaymentID, payment.PayerName))
			if order.RequestedPhone != "" {
				msg := verifiedReply(payment.Amount, payment.PayerName, "", payment.PaymentDate)
				if strings.TrimSpace(order.Reference) != "" {
					msg += " (" + order.Reference + ")"
				}
				_ = h.sendWA(ctx, order.RequestedPhone, msg)
			}
		} else if !errors.Is(claimErr, domain.ErrPaymentAlreadyConsumed) {
			log.Printf("⚠️ No se pudo asignar el pago %d a la orden %s: %v", payment.MPPaymentID, order.ID, claimErr)
		}
	}

	// 2. ¿Hay comprobantes en la Cola de Espera Reactiva esperando este pago?
	waitingItems, err := h.queue.ListPendingReceipts(ctx, payment.TenantID)
	if err != nil || len(waitingItems) == 0 {
		return notifications, nil
	}

	windowMin := 120
	if tenant, _ := h.repo.GetTenantByID(ctx, payment.TenantID); tenant != nil && tenant.TimeWindowMinutes > 0 {
		windowMin = tenant.TimeWindowMinutes
	}
	paymentCoelsa := application.NormalizeCoelsa(payment.CoelsaID)

	for _, item := range waitingItems {
		var claimed *domain.MPPayment
		var claimErr error

		switch {
		case item.CoelsaID != "" && item.CoelsaID == paymentCoelsa:
			claimed, claimErr = h.repo.ClaimPaymentByCoelsaID(ctx, payment.TenantID, item.CoelsaID, item.ExpectedAmount, item.ValidationID)
		case item.MPPaymentID != 0 && item.MPPaymentID == payment.MPPaymentID:
			claimed, claimErr = h.repo.ClaimPaymentByExactID(ctx, payment.TenantID, item.MPPaymentID, item.ExpectedAmount, item.ValidationID)
		case item.MatchByAmount && domain.SameAmount(item.ExpectedAmount, payment.Amount):
			claimed, claimErr = h.repo.ClaimPaymentByHeuristic(ctx, payment.TenantID, item.ExpectedAmount, windowMin, item.SenderCUIT, item.ValidationID)
		default:
			continue // este pago no es el que espera el comprobante
		}

		switch {
		case claimErr == nil && claimed != nil:
			newStatus := domain.StatusVerifiedExactID
			if item.MatchByAmount {
				newStatus = domain.StatusVerifiedHeuristic
			}
			_ = h.repo.UpdateReceiptValidationStatus(ctx, item.ValidationID, newStatus, claimed.ID)
			_ = h.queue.RemovePendingReceipt(ctx, payment.TenantID, item.ValidationID)

			clientName := item.SenderName
			if strings.TrimSpace(clientName) == "" {
				clientName = claimed.PayerName
			}
			arrivedAt := claimed.PaymentDate
			if arrivedAt.IsZero() {
				arrivedAt = payment.PaymentDate
			}
			_ = h.sendWA(ctx, item.SenderPhone, verifiedReply(claimed.Amount, clientName, item.SourceBankOrWallet, arrivedAt))
			notifications = append(notifications, fmt.Sprintf("Notificado proactivamente a %s (%s)", item.EmployeeName, item.SenderPhone))

		case errors.Is(claimErr, domain.ErrPaymentAlreadyConsumed) && !item.MatchByAmount:
			// El ID del comprobante ya fue usado por otra venta
			_ = h.repo.UpdateReceiptValidationStatus(ctx, item.ValidationID, domain.StatusFraudAlreadyUsed, "")
			_ = h.queue.RemovePendingReceipt(ctx, payment.TenantID, item.ValidationID)
			_ = h.sendWA(ctx, item.SenderPhone, transferReply(replyFake, item.ExpectedAmount)+pendingFrom(item))
			notifications = append(notifications, fmt.Sprintf("Doble uso informado a %s", item.EmployeeName))

		case errors.Is(claimErr, domain.ErrPaymentAmountMismatch):
			_ = h.repo.UpdateReceiptValidationStatus(ctx, item.ValidationID, domain.StatusRejectedAmountDiff, "")
			_ = h.queue.RemovePendingReceipt(ctx, payment.TenantID, item.ValidationID)
			_ = h.sendWA(ctx, item.SenderPhone, transferReply(replyFake, item.ExpectedAmount)+pendingFrom(item))
			notifications = append(notifications, fmt.Sprintf("Monto distinto informado a %s", item.EmployeeName))

		default:
			// Sigue esperando (puede llegar otro pago con ese monto dentro de los 3 minutos)
		}
	}

	return notifications, nil
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

// handleMPOAuthConnect redirige a la autorización OAuth 2.0 de Mercado Pago con un state firmado.
func (h *WebhooksHandler) handleMPOAuthConnect(w http.ResponseWriter, r *http.Request) {
	state := h.oauthState.Issue(h.tenantID)

	// En modo demo la vinculación siempre es simulada: nunca se toca una cuenta real
	if h.cfg.DemoMode {
		http.Redirect(w, r, fmt.Sprintf("/oauth/mp/callback?code=DEMO_CODE_%d&state=%s", time.Now().Unix(), state), http.StatusFound)
		return
	}
	if h.cfg.MPClientID == "" || h.cfg.MPClientID == "MP_DEMO_CLIENT_ID" {
		http.Error(w, "MP_CLIENT_ID / MP_CLIENT_SECRET no están configurados", http.StatusServiceUnavailable)
		return
	}
	http.Redirect(w, r, h.mpClient.GetOAuthAuthorizationURL(h.cfg.MPClientID, h.cfg.MPRedirectURI, state), http.StatusFound)
}

// handleMPOAuthCallback recibe el código OAuth 2.0, encripta los tokens con AES-256-GCM y guarda la cuenta.
func (h *WebhooksHandler) handleMPOAuthCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		http.Error(w, "Mercado Pago rechazó la vinculación: "+e, http.StatusBadRequest)
		return
	}
	tenantID, err := h.oauthState.Verify(q.Get("state"))
	if err != nil {
		http.Error(w, "Vinculación inválida o vencida. Volvé a iniciarla desde el panel.", http.StatusBadRequest)
		return
	}
	code := q.Get("code")
	if code == "" {
		http.Error(w, "Missing OAuth authorization code", http.StatusBadRequest)
		return
	}

	tokenResp, err := h.mpClient.ExchangeOAuthCode(r.Context(), h.cfg.MPClientID, h.cfg.MPClientSecret, code, h.cfg.MPRedirectURI)
	if err != nil {
		log.Printf("⚠️ OAuth MP: %v", err)
		http.Error(w, "Error en OAuth con Mercado Pago", http.StatusBadGateway)
		return
	}

	encAccess, err := h.vault.Encrypt(tokenResp.AccessToken)
	if err != nil {
		http.Error(w, "Error encriptando access_token", http.StatusInternalServerError)
		return
	}
	encRefresh, err := h.vault.Encrypt(tokenResp.RefreshToken)
	if err != nil {
		http.Error(w, "Error encriptando refresh_token", http.StatusInternalServerError)
		return
	}

	acc := &domain.MPAccount{
		TenantID:        tenantID,
		MPUserID:        tokenResp.UserID,
		AccessTokenEnc:  encAccess,
		RefreshTokenEnc: encRefresh,
		TokenExpiresAt:  time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second),
		WebhookSecret:   h.cfg.MPWebhookSecret,
		Status:          "CONNECTED",
	}
	// Conservamos alias/CVU ya configurados por el comercio
	if existing, _ := h.repo.GetActiveMPAccount(r.Context(), tenantID); existing != nil {
		acc.ExpectedAlias = existing.ExpectedAlias
		acc.ExpectedCVU = existing.ExpectedCVU
		acc.ExpectedHolderName = existing.ExpectedHolderName
		if existing.MPUserID == tokenResp.UserID {
			acc.ID = existing.ID
		}
	}

	if err := h.repo.UpsertMPAccount(r.Context(), acc); err != nil {
		log.Printf("⚠️ OAuth MP: no se pudo guardar la cuenta: %v", err)
		http.Error(w, "No se pudo guardar la cuenta vinculada", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/?oauth=connected", http.StatusFound)
}

// ---------------------------------------------------------------------------
// API del panel
// ---------------------------------------------------------------------------

// handleDashboardData devuelve el estado completo del comercio para el Dashboard Web.
func (h *WebhooksHandler) handleDashboardData(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID := h.tenantID

	tenant, _ := h.repo.GetTenantByID(ctx, tenantID)
	mpAcc, _ := h.repo.GetActiveMPAccount(ctx, tenantID)
	senders, _ := h.repo.ListAuthorizedSenders(ctx, tenantID)
	payments, _ := h.repo.ListMPPayments(ctx, tenantID, 50)
	pendingQueue, _ := h.queue.ListPendingReceipts(ctx, tenantID)
	chargeOrders, _ := h.queue.ListChargeOrders(ctx, tenantID)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"tenant":        tenant,
		"plans":         domain.GetPlanSpecs(),
		"mp_account":    mpAcc,
		"senders":       senders,
		"today":         h.todayStats(r, tenantID),
		"payments":      payments,
		"pending_queue": pendingQueue,
		"charge_orders": chargeOrders,
		"wa_messages":   h.waClient.ListMessages(),
		"demo_mode":     h.cfg.DemoMode,
		"auth_enabled":  h.authEnabled(),
		"admin_user":    h.cfg.AdminUser,
		"system": map[string]interface{}{
			"mysql_connected": h.repo.IsUsingMySQL(),
			"redis_connected": h.queue.IsUsingRedis(),
			"gemini_live_key": h.extractor.HasLiveAPIKey(),
		},
	})
}

func (h *WebhooksHandler) handleSenders(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID := h.tenantID

	switch r.Method {
	case http.MethodPost:
		var req struct {
			PhoneNumber   string `json:"phone_number"`
			EmployeeName  string `json:"employee_name"`
			Role          string `json:"role"`
			ExpectedAlias string `json:"expected_alias"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "JSON inválido"})
			return
		}
		if req.ExpectedAlias != "" {
			if status, msg := h.updateAccountDestination(ctx, tenantID, &req.ExpectedAlias, nil); status != http.StatusOK {
				writeJSON(w, status, map[string]string{"error": msg})
				return
			}
		}
		name := strings.TrimSpace(req.EmployeeName)
		phone := normalizePhone(req.PhoneNumber)
		if phone == "" && name == "" {
			writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
			return
		}
		if !phoneRegex.MatchString(phone) || name == "" || len(name) > 100 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Nombre o teléfono inválido (usá solo dígitos con código de país, ej: 5491155004444)"})
			return
		}
		role := domain.SenderRole(strings.ToUpper(strings.TrimSpace(req.Role)))
		if role != domain.RoleOwner && role != domain.RoleCashier && role != domain.RoleDelivery {
			role = domain.RoleCashier
		}
		sender := &domain.AuthorizedSender{TenantID: tenantID, PhoneNumber: phone, EmployeeName: name, Role: role}
		if err := h.repo.CreateAuthorizedSender(ctx, sender); err != nil {
			log.Printf("⚠️ Error creando empleado: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "No se pudo guardar el empleado"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})

	case http.MethodDelete:
		if err := h.repo.DeleteAuthorizedSender(ctx, tenantID, r.URL.Query().Get("id")); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "No se pudo eliminar"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleAccount configura el alias y el CVU de cobro con los que se compara el Escudo 3.
func (h *WebhooksHandler) handleAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ExpectedAlias *string `json:"expected_alias"`
		ExpectedCVU   *string `json:"expected_cvu"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "JSON inválido"})
		return
	}
	status, msg := h.updateAccountDestination(r.Context(), h.tenantID, req.ExpectedAlias, req.ExpectedCVU)
	if status != http.StatusOK {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (h *WebhooksHandler) updateAccountDestination(ctx context.Context, tenantID string, alias, cvu *string) (int, string) {
	acc, err := h.repo.GetActiveMPAccount(ctx, tenantID)
	if err != nil || acc == nil {
		return http.StatusConflict, "Primero vinculá una cuenta de Mercado Pago"
	}
	if alias != nil {
		a := strings.ToLower(strings.TrimSpace(*alias))
		if a != "" && !aliasRegex.MatchString(a) {
			return http.StatusBadRequest, "Alias inválido (6 a 20 caracteres: letras, números, puntos o guiones)"
		}
		acc.ExpectedAlias = a
	}
	if cvu != nil {
		c := strings.NewReplacer(" ", "", "-", "").Replace(strings.TrimSpace(*cvu))
		if c != "" && !cvuRegex.MatchString(c) {
			return http.StatusBadRequest, "El CBU/CVU debe tener 22 dígitos"
		}
		acc.ExpectedCVU = c
	}
	if err := h.repo.UpsertMPAccount(ctx, acc); err != nil {
		log.Printf("⚠️ Error guardando alias/CVU: %v", err)
		return http.StatusInternalServerError, "No se pudo guardar"
	}
	return http.StatusOK, ""
}

func (h *WebhooksHandler) handleUpdatePlan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Plan              string `json:"plan"`
		TimeWindowMinutes int    `json:"time_window_minutes"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "JSON inválido"})
		return
	}
	spec, ok := domain.GetPlanSpecs()[domain.PlanType(req.Plan)]
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Plan desconocido"})
		return
	}
	if req.TimeWindowMinutes != 0 && (req.TimeWindowMinutes < 5 || req.TimeWindowMinutes > 1440) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "La ventana debe estar entre 5 y 1440 minutos"})
		return
	}
	if err := h.repo.UpdateTenantPlan(r.Context(), h.tenantID, spec.Plan, spec.MonthlyQuota, req.TimeWindowMinutes); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "No se pudo actualizar el plan"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (h *WebhooksHandler) handleCreatePosnetCharge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		BaseAmount  float64 `json:"base_amount"`
		Reference   string  `json:"reference"`
		RequestedBy string  `json:"requested_by"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil || req.BaseAmount <= 0 || req.BaseAmount > 1e12 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Monto inválido"})
		return
	}
	if req.RequestedBy == "" {
		req.RequestedBy = "Caja Posnet Virtual"
	}
	if req.Reference == "" {
		req.Reference = "Venta Mostrador"
	}
	acc, _ := h.repo.GetActiveMPAccount(r.Context(), h.tenantID)
	dest := h.chargeDestination(acc)
	if dest == "" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "Configurá el alias o CVU de cobro primero"})
		return
	}

	order, err := h.queue.CreateUniqueChargeOrder(r.Context(), h.tenantID, req.RequestedBy, "", req.Reference, dest, req.BaseAmount)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, queue.ErrNoUniqueAmountAvailable) {
			status = http.StatusConflict
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, order)
}

// handleReceiptUpload analiza un comprobante real subido desde el panel (imagen o PDF).
// handleReceiptUpload recibe el archivo y lo analiza en segundo plano: el análisis puede tardar
// más de lo que aguanta un proxy o túnel (504), así que responde enseguida con un job_id y el
// panel consulta el resultado con GET ?job=<id>.
func (h *WebhooksHandler) handleReceiptUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		job, ok := h.uploads.get(r.URL.Query().Get("job"))
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "No encontramos ese análisis. Subí el comprobante de nuevo."})
			return
		}
		writeJSON(w, http.StatusOK, job)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, uploadLimit+(1<<20))
	if err := r.ParseMultipartForm(uploadLimit); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "No se pudo leer el archivo (máximo 15 MB)"})
		return
	}
	file, header, err := r.FormFile("receipt_file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Archivo requerido"})
		return
	}
	defer file.Close()
	fileBytes, err := io.ReadAll(file)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "No se pudo leer el archivo"})
		return
	}
	mimeType := header.Header.Get("Content-Type")
	if mimeType == "" {
		mimeType = http.DetectContentType(fileBytes)
	}

	tenant, err := h.repo.GetTenantByID(r.Context(), h.tenantID)
	if err != nil || tenant == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Comercio no encontrado"})
		return
	}

	if !h.acquireWorker() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Hay muchos comprobantes analizándose. Probá de nuevo en unos segundos."})
		return
	}
	jobID := h.uploads.start()
	h.runInBackground(waProcessTimeout, func(ctx context.Context) {
		outcome, err := h.processReceiptForTenant(ctx, tenant, panelSender, "panel_"+uuid.NewString(), fileBytes, mimeType)
		if err != nil {
			log.Printf("⚠️ Error analizando comprobante subido: %v", err)
			h.uploads.finish(jobID, nil, "No pudimos analizar el comprobante. Probá de nuevo en unos minutos.")
			return
		}
		h.uploads.finish(jobID, outcome, "")
	})
	writeJSON(w, http.StatusAccepted, map[string]string{"job_id": jobID, "status": uploadProcessing})
}

// ---------------------------------------------------------------------------
// Análisis de comprobantes subidos desde el panel (en segundo plano)
// ---------------------------------------------------------------------------

const (
	uploadProcessing = "processing"
	uploadDone       = "done"
	uploadFailed     = "error"
	uploadJobTTL     = 15 * time.Minute
)

type uploadJob struct {
	Status    string                      `json:"status"`
	Outcome   *domain.VerificationOutcome `json:"outcome,omitempty"`
	Error     string                      `json:"error,omitempty"`
	createdAt time.Time
}

type uploadJobs struct {
	mu   sync.Mutex
	jobs map[string]*uploadJob
}

func newUploadJobs() *uploadJobs { return &uploadJobs{jobs: make(map[string]*uploadJob)} }

func (u *uploadJobs) start() string {
	id := uuid.NewString()
	now := time.Now()
	u.mu.Lock()
	defer u.mu.Unlock()
	for k, j := range u.jobs {
		if now.Sub(j.createdAt) > uploadJobTTL {
			delete(u.jobs, k)
		}
	}
	u.jobs[id] = &uploadJob{Status: uploadProcessing, createdAt: now}
	return id
}

func (u *uploadJobs) finish(id string, outcome *domain.VerificationOutcome, errMsg string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	j, ok := u.jobs[id]
	if !ok {
		return
	}
	if errMsg != "" {
		j.Status, j.Error = uploadFailed, errMsg
		return
	}
	j.Status, j.Outcome = uploadDone, outcome
}

func (u *uploadJobs) get(id string) (uploadJob, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	j, ok := u.jobs[id]
	if !ok {
		return uploadJob{}, false
	}
	return *j, true
}

// pickPanelSender elige quién recibe el resultado: el teléfono pedido, o el dueño, o el primer empleado.
func (h *WebhooksHandler) pickPanelSender(ctx context.Context, tenantID, phone string) *domain.AuthorizedSender {
	senders, _ := h.repo.ListAuthorizedSenders(ctx, tenantID)
	var owner, first *domain.AuthorizedSender
	for _, s := range senders {
		if !s.IsActive {
			continue
		}
		if phone != "" && s.PhoneNumber == phone {
			return s
		}
		if owner == nil && s.Role == domain.RoleOwner {
			owner = s
		}
		if first == nil {
			first = s
		}
	}
	if owner != nil {
		return owner
	}
	return first
}

// csvSafe evita que un texto leído de un comprobante se ejecute como fórmula al abrir el CSV en Excel.
func csvSafe(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

// ---------------------------------------------------------------------------
// Simulador (solo DEMO_MODE)
// ---------------------------------------------------------------------------

// handleSimulateReceipt prueba los escudos antifraude con escenarios armados.
func (h *WebhooksHandler) handleSimulateReceipt(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		h.handleReceiptUpload(w, r)
		return
	}

	ctx := r.Context()
	tenant, err := h.repo.GetTenantByID(ctx, h.tenantID)
	if err != nil || tenant == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Comercio demo no encontrado"})
		return
	}

	var req struct {
		Scenario           string  `json:"scenario"`
		SenderPhone        string  `json:"sender_phone"`
		Amount             float64 `json:"amount"`
		OperationNumber    string  `json:"operation_number"`
		SourceBankOrWallet string  `json:"source_bank_or_wallet"`
		SenderName         string  `json:"sender_name"`
		SenderCUIT         string  `json:"sender_cuit"`
		ReceiverAliasOrCVU string  `json:"receiver_alias_or_cvu"`
		IsCompleted        *bool   `json:"is_completed"`
		IsValid            *bool   `json:"is_valid"`
		FixedFileKey       string  `json:"fixed_file_key"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "JSON inválido"})
		return
	}

	activeSender := h.pickPanelSender(ctx, tenant.ID, req.SenderPhone)
	if activeSender == nil {
		activeSender = &domain.AuthorizedSender{
			PhoneNumber:  "5491155002222",
			EmployeeName: "Lucía Gómez (Cajera)",
			Role:         domain.RoleCashier,
		}
	}

	acc, _ := h.repo.GetActiveMPAccount(ctx, tenant.ID)
	expectedAlias := h.chargeDestination(acc)

	extracted, fileBytes, label := h.buildScenarioPayload(ctx, tenant.ID, expectedAlias, req.Scenario, req.Amount, req.OperationNumber, req.SourceBankOrWallet, req.SenderName, req.SenderCUIT, req.ReceiverAliasOrCVU, req.IsCompleted, req.IsValid, req.FixedFileKey)

	h.waClient.RecordInboundMessage(activeSender.PhoneNumber, activeSender.EmployeeName, fmt.Sprintf("📷 [Comprobante Reenviado: %s - $%.2f]", label, extracted.Amount))

	outcome, err := h.processReceiptForTenant(ctx, tenant, activeSender, "wa_sim_"+uuid.NewString(), fileBytes, "image/jpeg")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, outcome)
}

// randomDemoMPID genera un ID de pago de demo de 11 dígitos que empieza con prefix (1..9).
// Se usa azar criptográfico: en Windows dos llamadas seguidas a time.Now() pueden devolver lo mismo.
func randomDemoMPID(prefix int64) int64 {
	n, _ := rand.Int(rand.Reader, big.NewInt(10000000000))
	return prefix*10000000000 + n.Int64()
}

// randomCoelsa genera un ID COELSA de 22 dígitos para los escenarios de demo.
func randomCoelsa() string {
	var b strings.Builder
	b.WriteString("2410")
	for b.Len() < 22 {
		n, _ := rand.Int(rand.Reader, big.NewInt(10))
		b.WriteString(n.String())
	}
	return b.String()
}

func (h *WebhooksHandler) buildScenarioPayload(
	ctx context.Context,
	tenantID string,
	expectedAlias string,
	scenario string,
	customAmt float64,
	customOp string,
	customBank string,
	customSender string,
	customCUIT string,
	customDest string,
	customCompleted *bool,
	customValid *bool,
	fixedKey string,
) (*domain.ExtractedReceiptData, []byte, string) {
	now := time.Now()
	var data domain.ExtractedReceiptData
	var nonce string
	var label string

	switch scenario {
	case "EXACT_MP_VALID":
		// Creamos un pago fresco disponible en MP y enviamos su comprobante exacto
		mpID := randomDemoMPID(1)
		amt := customAmt
		if amt <= 0 {
			amt = 18500.00
		}
		newPay := &domain.MPPayment{
			ID:                   uuid.NewString(),
			TenantID:             tenantID,
			MPPaymentID:          mpID,
			Amount:               amt,
			Currency:             "ARS",
			MPStatus:             "approved",
			OperationType:        "money_transfer",
			PayerCUIT:            "20351122334",
			PayerName:            "Carlos Alberto Gomez",
			PaymentDate:          now,
			ReconciliationStatus: "AVAILABLE",
		}
		_ = h.repo.UpsertMPPayment(ctx, newPay)
		h.mpClient.RegisterMockPayment(newPay)

		data = domain.ExtractedReceiptData{
			IsValidReceipt:      true,
			IsCompletedTransfer: true,
			Amount:              amt,
			Currency:            "ARS",
			OperationNumber:     fmt.Sprintf("%d", mpID),
			SourceBankOrWallet:  "Mercado Pago",
			SenderName:          "Carlos Alberto Gomez",
			SenderCUIT:          "20351122334",
			ReceiverAliasOrCVU:  expectedAlias,
			ReceiptTimestampISO: now.Format(time.RFC3339),
		}
		nonce = uuid.NewString()
		label = "Mercado Pago ID Exacto #" + data.OperationNumber

	case "HEURISTIC_CVU_VALID":
		// Comprobante bancario SIN ningún ID verificable: se concilia por monto + ventana + CUIT
		mpID := randomDemoMPID(2)
		amt := customAmt
		if amt <= 0 {
			amt = 32400.00
		}
		newPay := &domain.MPPayment{
			ID:                   uuid.NewString(),
			TenantID:             tenantID,
			MPPaymentID:          mpID,
			Amount:               amt,
			Currency:             "ARS",
			MPStatus:             "approved",
			OperationType:        "bank_transfer",
			PayerCUIT:            "27389911223",
			PayerName:            "Valeria Martinez",
			PaymentDate:          now,
			ReconciliationStatus: "AVAILABLE",
		}
		_ = h.repo.UpsertMPPayment(ctx, newPay)
		h.mpClient.RegisterMockPayment(newPay)

		data = domain.ExtractedReceiptData{
			IsValidReceipt:      true,
			IsCompletedTransfer: true,
			Amount:              amt,
			Currency:            "ARS",
			OperationNumber:     "GAL-9948271-CVU", // Nro interno bancario (no es ID MP)
			SourceBankOrWallet:  "Banco Galicia",
			SenderName:          "Valeria Martinez",
			SenderCUIT:          "27389911223",
			ReceiverAliasOrCVU:  expectedAlias,
			ReceiptTimestampISO: now.Format(time.RFC3339),
		}
		nonce = uuid.NewString()
		label = "Banco Galicia CVU (Heurística)"

	case "FRAUD_DUPLICATE_SHA256":
		data = domain.ExtractedReceiptData{
			IsValidReceipt:      true,
			IsCompletedTransfer: true,
			Amount:              15450.50,
			Currency:            "ARS",
			OperationNumber:     "88492019482",
			SourceBankOrWallet:  "Mercado Pago",
			SenderName:          "Juan Ignacio Perez",
			SenderCUIT:          "20348912349",
			ReceiverAliasOrCVU:  expectedAlias,
		}
		// Mismo contenido exacto de bytes: la 2da vez dispara el Escudo 1 (archivo ya verificado)
		nonce = "STATIC_REUSED_RECEIPT_BYTES_2026"
		label = "Archivo Repetido (Hash SHA-256)"

	case "FRAUD_DOUBLE_SPEND_ID":
		// Pago real en MP pero YA CONSUMIDO por otro pedido
		usedMPID := int64(77700011122)
		amt := 19900.00
		newPay := &domain.MPPayment{
			ID:                   "pay-consumed-77700011122",
			TenantID:             tenantID,
			MPPaymentID:          usedMPID,
			Amount:               amt,
			Currency:             "ARS",
			MPStatus:             "approved",
			OperationType:        "money_transfer",
			PayerCUIT:            "20312233445",
			PayerName:            "Esteban Quito",
			PaymentDate:          now.Add(-15 * time.Minute),
			ReconciliationStatus: "AVAILABLE",
		}
		_ = h.repo.UpsertMPPayment(ctx, newPay)
		_, _ = h.repo.ClaimPaymentByExactID(ctx, tenantID, usedMPID, amt, "previous-receipt-validation-id")

		data = domain.ExtractedReceiptData{
			IsValidReceipt:      true,
			IsCompletedTransfer: true,
			Amount:              amt,
			Currency:            "ARS",
			OperationNumber:     fmt.Sprintf("%d", usedMPID),
			SourceBankOrWallet:  "Mercado Pago",
			SenderName:          "Esteban Quito (Nueva Foto Distinto Hash)",
			SenderCUIT:          "20312233445",
			ReceiverAliasOrCVU:  expectedAlias,
			ReceiptTimestampISO: now.Format(time.RFC3339),
		}
		nonce = uuid.NewString() // Distinto hash SHA-256 (ej: recortaron la foto), pero mismo mp_payment_id
		label = "Doble Gasto Operación #" + data.OperationNumber

	case "FRAUD_INCOMPLETE_TRANSFER":
		data = domain.ExtractedReceiptData{
			IsValidReceipt:          true,
			IsCompletedTransfer:     false,
			Amount:                  45000.00,
			Currency:                "ARS",
			OperationNumber:         "PENDIENTE",
			SourceBankOrWallet:      "Brubank",
			SenderName:              "Cliente Estafador",
			ReceiverAliasOrCVU:      expectedAlias,
			VisualManipulationNotes: []string{"Pantalla previa: '¿Confirmás el envío?' / 'Transferencia en proceso'"},
		}
		nonce = uuid.NewString()
		label = "Captura 'En Proceso / Sin Confirmar'"

	case "REJECTED_WRONG_DESTINATION":
		data = domain.ExtractedReceiptData{
			IsValidReceipt:      true,
			IsCompletedTransfer: true,
			Amount:              22000.00,
			Currency:            "ARS",
			OperationNumber:     "99401928374",
			SourceBankOrWallet:  "Ualá",
			SenderName:          "Martín Palermo",
			ReceiverAliasOrCVU:  "otro.comercio.falso.mp",
			ReceiptTimestampISO: now.Format(time.RFC3339),
		}
		nonce = uuid.NewString()
		label = "Destinatario Incorrecto (otro.comercio.falso.mp)"

	case "DELAYED_COELSA_QUEUE":
		amt := customAmt
		if amt <= 0 {
			amt = 27650.00
		}
		data = domain.ExtractedReceiptData{
			IsValidReceipt:      true,
			IsCompletedTransfer: true,
			Amount:              amt,
			Currency:            "ARS",
			OperationNumber:     "BNA-COELSA-88412",
			CoelsaID:            randomCoelsa(),
			SourceBankOrWallet:  "Banco Nación (Cuenta DNI)",
			SenderName:          "Roberto Ayala",
			SenderCUIT:          "20245566778",
			ReceiverAliasOrCVU:  expectedAlias,
			ReceiptTimestampISO: now.Format(time.RFC3339),
		}
		nonce = uuid.NewString()
		label = "Demora COELSA (Entra en Cola Reactiva 3 min)"

	default:
		isVal := true
		if customValid != nil {
			isVal = *customValid
		}
		isComp := true
		if customCompleted != nil {
			isComp = *customCompleted
		}
		if customAmt <= 0 {
			customAmt = 15450.50
		}
		if customOp == "" {
			customOp = "88492019482"
		}
		if customBank == "" {
			customBank = "Mercado Pago"
		}
		if customSender == "" {
			customSender = "Juan Ignacio Perez"
		}
		if customDest == "" {
			customDest = expectedAlias
		}
		data = domain.ExtractedReceiptData{
			IsValidReceipt:      isVal,
			IsCompletedTransfer: isComp,
			Amount:              customAmt,
			Currency:            "ARS",
			OperationNumber:     customOp,
			SourceBankOrWallet:  customBank,
			SenderName:          customSender,
			SenderCUIT:          customCUIT,
			ReceiverAliasOrCVU:  customDest,
			ReceiptTimestampISO: now.Format(time.RFC3339),
		}
		if fixedKey != "" {
			nonce = fixedKey
		} else {
			nonce = uuid.NewString()
		}
		label = "Comprobante Personalizado"
	}

	rawJSON, _ := json.Marshal(data)
	payloadBytes := []byte("SIMULATED_RECEIPT_JSON:" + string(rawJSON) + "|NONCE:" + nonce)
	return &data, payloadBytes, label
}

// handleSimulateMPWebhook simula la llegada de un pago de Mercado Pago (firmado con HMAC).
func (h *WebhooksHandler) handleSimulateMPWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Amount      float64 `json:"amount"`
		MPPaymentID int64   `json:"mp_payment_id"`
		PayerName   string  `json:"payer_name"`
		PayerCUIT   string  `json:"payer_cuit"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req)

	ctx := r.Context()
	tenantID := h.tenantID
	coelsa := randomCoelsa()

	// Sin monto: tomamos el primer comprobante en espera (con su COELSA) o la primera orden /cobrar
	if req.Amount <= 0 {
		if pending, _ := h.queue.ListPendingReceipts(ctx, tenantID); len(pending) > 0 {
			req.Amount = pending[0].ExpectedAmount
			if pending[0].CoelsaID != "" {
				coelsa = pending[0].CoelsaID
			}
			if pending[0].MPPaymentID != 0 && req.MPPaymentID <= 0 {
				req.MPPaymentID = pending[0].MPPaymentID
			}
			if req.PayerName == "" {
				req.PayerName = pending[0].SenderName
			}
			if req.PayerCUIT == "" {
				req.PayerCUIT = pending[0].SenderCUIT
			}
		} else if orders, _ := h.queue.ListChargeOrders(ctx, tenantID); len(orders) > 0 {
			for _, o := range orders {
				if o.Status == "PENDING" {
					req.Amount = o.UniqueAmount
					break
				}
			}
		}
	}
	if req.Amount <= 0 {
		req.Amount = 27650.00
	}
	if req.MPPaymentID <= 0 {
		req.MPPaymentID = randomDemoMPID(5)
	}
	if req.PayerName == "" {
		req.PayerName = "Roberto Ayala"
	}
	if req.PayerCUIT == "" {
		req.PayerCUIT = "20245566778"
	}

	dataID := strconv.FormatInt(req.MPPaymentID, 10)
	requestID := "req-mp-" + uuid.NewString()
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	signature := ComputeMPWebhookSignature(h.cfg.MPWebhookSecret, dataID, requestID, ts)

	// Verificamos la firma con el mismo validador de producción
	simReq, _ := http.NewRequest(http.MethodPost, "/webhooks/mercadopago?data.id="+dataID, nil)
	simReq.Header.Set("x-signature", signature)
	simReq.Header.Set("x-request-id", requestID)
	hmacValid := ValidateMPWebhookSignature(h.cfg.MPWebhookSecret, simReq)

	payment := &domain.MPPayment{
		ID:                   uuid.NewString(),
		TenantID:             tenantID,
		MPPaymentID:          req.MPPaymentID,
		Amount:               req.Amount,
		Currency:             "ARS",
		MPStatus:             "approved",
		OperationType:        "bank_transfer",
		PayerCUIT:            req.PayerCUIT,
		PayerName:            req.PayerName,
		CoelsaID:             coelsa,
		PaymentDate:          time.Now(),
		ReconciliationStatus: "AVAILABLE",
	}
	h.mpClient.RegisterMockPayment(payment)

	notifications, err := h.ingestMPPaymentAndResolveQueue(ctx, payment)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":                "webhook_processed",
		"hmac_signature_header": signature,
		"hmac_verified":         hmacValid,
		"ingested_payment":      payment,
		"proactive_actions":     notifications,
	})
}

// handleSimulateWACommand envía un comando de texto (ej: /cobrar 10000) desde el chat simulado.
func (h *WebhooksHandler) handleSimulateWACommand(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Text        string `json:"text"`
		SenderPhone string `json:"sender_phone"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "JSON inválido"})
		return
	}
	if req.SenderPhone == "" {
		req.SenderPhone = "5491155002222"
	}
	h.processIncomingWhatsAppMessage(r.Context(), req.SenderPhone, "wa_cmd_"+uuid.NewString(), "text", req.Text, "", "", "", "")
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}
