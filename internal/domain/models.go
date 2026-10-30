package domain

import (
	"errors"
	"math"
	"strings"
	"time"
)

var (
	ErrPaymentAlreadyConsumed = errors.New("el pago ya fue consumido previamente por otro comprobante")
	ErrPaymentNotFound        = errors.New("no se encontró un pago acreditado disponible")
	ErrPaymentAmountMismatch  = errors.New("el pago existe pero el monto no coincide con el comprobante")
	ErrValidationNotFound     = errors.New("el comprobante no existe")
	ErrTenantEmailTaken       = errors.New("ya existe un comercio registrado con ese email")
	ErrTenantCUITTaken        = errors.New("ya existe un comercio registrado con ese CUIT")
	ErrPhoneAlreadyInUse      = errors.New("ese número de WhatsApp ya está registrado en otro comercio")
	ErrSubscriptionNotFound   = errors.New("la orden de suscripción no existe")
)

// ChargeOrderIDPrefix identifica en consumed_by_receipt_id los pagos tomados por una orden /cobrar.
const ChargeOrderIDPrefix = "cob-"

// ToCents convierte un monto a centavos enteros para comparar sin errores de coma flotante.
func ToCents(amount float64) int64 {
	return int64(math.Round(amount * 100))
}

// SameAmount compara dos montos al centavo.
func SameAmount(a, b float64) bool {
	return ToCents(a) == ToCents(b)
}

type ValidationStatus string

const (
	StatusVerifiedExactID     ValidationStatus = "VERIFIED_EXACT_ID"
	StatusVerifiedHeuristic   ValidationStatus = "VERIFIED_HEURISTIC"
	StatusRejectedNotFound    ValidationStatus = "REJECTED_NOT_FOUND"
	StatusRejectedWrongDest   ValidationStatus = "REJECTED_WRONG_DESTINATION"
	StatusFraudAlreadyUsed    ValidationStatus = "FRAUD_ALREADY_USED"
	StatusFraudDuplicateFile  ValidationStatus = "FRAUD_DUPLICATE_FILE"
	StatusFraudIncomplete     ValidationStatus = "FRAUD_INCOMPLETE_TRANSFER"
	StatusInvalidReceiptImage ValidationStatus = "INVALID_RECEIPT_IMAGE"
	StatusRejectedAmountDiff  ValidationStatus = "REJECTED_AMOUNT_MISMATCH"
	// StatusFraudTampered: el comprobante fue editado (ej. el CVU de destino es casi igual al
	// del comercio, con uno o dos dígitos cambiados). No es una transferencia a otra cuenta.
	StatusFraudTampered ValidationStatus = "FRAUD_TAMPERED_RECEIPT"

	// StatusQuotaExceeded no se persiste: solo se informa al empleado.
	StatusQuotaExceeded ValidationStatus = "QUOTA_EXCEEDED"
	// StatusMPNotConnected no se persiste: el comercio no tiene Mercado Pago vinculado.
	StatusMPNotConnected ValidationStatus = "MP_NOT_CONNECTED"
)

// Modos de conciliación pendiente para la Cola de Espera Reactiva.
const (
	MatchModeCoelsa = "COELSA"
	MatchModeMPID   = "MP_ID"
	MatchModeAmount = "AMOUNT"
)

// ExtractedReceiptData representa la salida estructurada que devuelve Gemini 2.5 Flash
type ExtractedReceiptData struct {
	IsValidReceipt          bool     `json:"is_valid_receipt"`
	IsCompletedTransfer     bool     `json:"is_completed_transfer"` // False si dice "Programada", "En proceso" o "¿Confirmás el envío?"
	Amount                  float64  `json:"amount"`
	Currency                string   `json:"currency"`
	OperationNumber         string   `json:"operation_number"`      // ID de Mercado Pago o Nro de Comprobante bancario
	CoelsaID                string   `json:"coelsa_id"`             // Código COELSA de 22 caracteres si figura
	SourceBankOrWallet      string   `json:"source_bank_or_wallet"` // Ej: "Mercado Pago", "Brubank", "Banco Galicia", "MODO"
	SenderName              string   `json:"sender_name"`
	SenderCUIT              string   `json:"sender_cuit"` // Solo dígitos
	ReceiverName            string   `json:"receiver_name"`
	ReceiverAliasOrCVU      string   `json:"receiver_alias_or_cvu"`
	ReceiptTimestampISO     string   `json:"receipt_timestamp_iso"`
	VisualManipulationNotes []string `json:"visual_manipulation_notes"` // Alertas si hay fuentes desalineadas o artefactos
}

// MPPayment representa un ingreso real acreditado en Mercado Pago
type MPPayment struct {
	ID                   string     `json:"id"`
	TenantID             string     `json:"tenant_id"`
	MPPaymentID          int64      `json:"mp_payment_id"`
	Amount               float64    `json:"amount"`
	Currency             string     `json:"currency"`
	MPStatus             string     `json:"mp_status"`
	OperationType        string     `json:"operation_type"`
	PayerCUIT            string     `json:"payer_cuit"`
	PayerName            string     `json:"payer_name"`
	PayerEmail           string     `json:"payer_email,omitempty"`
	CustomerID           string     `json:"customer_id,omitempty"` // cliente al que pertenece el pago (si se pudo identificar)
	CoelsaID             string     `json:"coelsa_id"`
	PaymentDate          time.Time  `json:"payment_date"`
	ReconciliationStatus string     `json:"reconciliation_status"` // "AVAILABLE" | "CONSUMED" | "DISPUTED"
	ConsumedAt           *time.Time `json:"consumed_at,omitempty"`
	ConsumedByReceiptID  string     `json:"consumed_by_receipt_id,omitempty"`
	RawJSON              []byte     `json:"raw_json,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
}

// VerificationOutcome es la respuesta final que se formatea hacia WhatsApp
type VerificationOutcome struct {
	ValidationID   string                `json:"validation_id"`
	Status         ValidationStatus      `json:"status"`
	MessageForUser string                `json:"message_for_user"`
	MatchedPayment *MPPayment            `json:"matched_payment,omitempty"`
	ExtractedData  *ExtractedReceiptData `json:"extracted_data,omitempty"`
	ProcessingTime time.Duration         `json:"processing_time_ns"`
	ProcessingMs   int64                 `json:"processing_time_ms"`
	QueuedForRetry bool                  `json:"queued_for_retry"`

	// Datos para la Cola de Espera Reactiva cuando el pago todavía no impactó
	FileSHA256       string `json:"file_sha256,omitempty"`
	MatchCoelsaID    string `json:"match_coelsa_id,omitempty"`
	MatchMPPaymentID int64  `json:"match_mp_payment_id,omitempty"`
	MatchByAmount    bool   `json:"match_by_amount,omitempty"`

	// true si el pago ya había sido conciliado por una orden /cobrar (no es fraude en sí mismo)
	ConsumedByChargeOrder bool `json:"consumed_by_charge_order,omitempty"`

	// Cruce con el historial del cliente (se calcula antes de guardar este comprobante)
	CustomerRisk *CustomerRisk `json:"customer_risk,omitempty"`

	// Cuándo se validó antes este mismo comprobante (reenvío del PDF como captura o al revés).
	AlreadySentAt *time.Time `json:"already_sent_at,omitempty"`
	// true si se reconoció por emisor + monto sin poder confirmar la hora (puede ser otra transferencia)
	AlreadySentBySender bool `json:"already_sent_by_sender,omitempty"`
}

// PlanType define los planes comerciales B2B de VeriPago SaaS (Sección 1.3)
type PlanType string

const (
	PlanFreeTrial   PlanType = "FREE_TRIAL"
	PlanEmprendedor PlanType = "EMPRENDEDOR"
	PlanPro         PlanType = "PRO"
	PlanFull        PlanType = "FULL"
)

// PlanSpec describe límites y precios de cada plan B2B
type PlanSpec struct {
	Plan               PlanType `json:"plan"`
	Name               string   `json:"name"`
	PriceUSD           int      `json:"price_usd"`
	MonthlyQuota       int      `json:"monthly_quota"`
	MaxMPAccounts      int      `json:"max_mp_accounts"`
	MaxAuthorizedPhone int      `json:"max_authorized_phones"`
	OwnerFraudAlerts   bool     `json:"owner_fraud_alerts"`
	CSVExport          bool     `json:"csv_export"`
	PosnetVirtual      bool     `json:"posnet_virtual"`
}

// GetPlanSpecs devuelve el catálogo de planes definido en la Sección 1.3 del PRD
func GetPlanSpecs() map[PlanType]PlanSpec {
	return map[PlanType]PlanSpec {
		PlanFreeTrial: {
			Plan:               PlanFreeTrial,
			Name:               "Free Trial",
			PriceUSD:           0,
			MonthlyQuota:       50,
			MaxMPAccounts:      1,
			MaxAuthorizedPhone: 2,
			OwnerFraudAlerts:   true,
			CSVExport:          true,
			PosnetVirtual:      true,
		},
		PlanEmprendedor: {
			Plan:               PlanEmprendedor,
			Name:               "Emprendedor",
			PriceUSD:           12,
			MonthlyQuota:       300,
			MaxMPAccounts:      1,
			MaxAuthorizedPhone: 2,
			OwnerFraudAlerts:   false,
			CSVExport:          false,
			PosnetVirtual:      false,
		},
		PlanPro: {
			Plan:               PlanPro,
			Name:               "Comercio Pro",
			PriceUSD:           28,
			MonthlyQuota:       1500,
			MaxMPAccounts:      2,
			MaxAuthorizedPhone: 10,
			OwnerFraudAlerts:   true,
			CSVExport:          true,
			PosnetVirtual:      true,
		},
		PlanFull: {
			Plan:               PlanFull,
			Name:               "Full / Franquicia",
			PriceUSD:           65,
			MonthlyQuota:       5000,
			MaxMPAccounts:      99,
			MaxAuthorizedPhone: 999,
			OwnerFraudAlerts:   true,
			CSVExport:          true,
			PosnetVirtual:      true,
		},
	}
}

// Tenant representa un comercio suscripto (Tabla: tenants)
type Tenant struct {
	ID                 string     `json:"id"`
	BusinessName       string     `json:"business_name"`
	CUIT               string     `json:"cuit"`
	Email              string     `json:"email"`
	PasswordHash       string     `json:"-"`
	PlanType           PlanType   `json:"plan_type"`
	MonthlyQuota       int        `json:"monthly_quota"`
	UsedQuota          int        `json:"used_quota"`
	TimeWindowMinutes  int        `json:"time_window_minutes"`
	SubscriptionStatus string     `json:"subscription_status"` // TRIAL | ACTIVE | PAST_DUE | CANCELLED
	PlanExpiresAt      *time.Time `json:"plan_expires_at,omitempty"`
	IsActive           bool       `json:"is_active"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

// SubscriptionOrder representa una orden de pago/suscripción de un comercio hacia la plataforma SaaS
type SubscriptionOrder struct {
	ID             string     `json:"id"`
	TenantID       string     `json:"tenant_id"`
	BusinessName   string     `json:"business_name,omitempty"`
	TenantEmail    string     `json:"tenant_email,omitempty"`
	PlanType       PlanType   `json:"plan_type"`
	PriceUSD       int        `json:"price_usd"`
	AmountARS      float64    `json:"amount_ars"`
	Status         string     `json:"status"`       // PENDING | APPROVED | REJECTED | CANCELLED
	PaymentMode    string     `json:"payment_mode"` // MP_CHECKOUT | DEMO | MANUAL
	MPPreferenceID string     `json:"mp_preference_id,omitempty"`
	MPPaymentID    int64      `json:"mp_payment_id,omitempty"`
	CheckoutURL    string     `json:"checkout_url,omitempty"`
	PaidAt         *time.Time `json:"paid_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// TenantAdminSummary resume el estado de un comercio para el panel de Super-Admin
type TenantAdminSummary struct {
	Tenant
	HasPassword    bool   `json:"has_password"`
	MPConnected    bool   `json:"mp_connected"`
	MPUserID       int64  `json:"mp_user_id,omitempty"`
	ExpectedAlias  string `json:"expected_alias,omitempty"`
	SendersCount   int    `json:"senders_count"`
	TotalValidated int    `json:"total_validated"`
}

// PlatformStats resume las métricas globales del SaaS para el Super-Admin
type PlatformStats struct {
	TotalTenants       int     `json:"total_tenants"`
	ActiveTenants      int     `json:"active_tenants"`
	PaidSubscriptions  int     `json:"paid_subscriptions"`
	MonthlyValidations int     `json:"monthly_validations"`
	ApprovedRevenueARS float64 `json:"approved_revenue_ars"`
	PendingOrdersCount int     `json:"pending_orders_count"`
}

// MPAccount representa una cuenta vinculada de Mercado Pago (Tabla: mp_accounts)
type MPAccount struct {
	ID                 string    `json:"id"`
	TenantID           string    `json:"tenant_id"`
	MPUserID           int64     `json:"mp_user_id"`
	ExpectedAlias      string    `json:"expected_alias"`
	ExpectedCVU        string    `json:"expected_cvu"`
	ExpectedHolderName string    `json:"expected_holder_name"`
	AccessTokenEnc     string    `json:"-"`
	RefreshTokenEnc    string    `json:"-"`
	TokenExpiresAt     time.Time `json:"token_expires_at"`
	WebhookSecret      string    `json:"-"`
	Status             string    `json:"status"` // CONNECTED | EXPIRED | REVOKED
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

// SenderRole define el rol del empleado en el comercio
type SenderRole string

const (
	RoleOwner    SenderRole = "OWNER"
	RoleCashier  SenderRole = "CASHIER"
	RoleDelivery SenderRole = "DELIVERY"
)

// AuthorizedSender representa un número de WhatsApp autorizado (Tabla: authorized_senders)
type AuthorizedSender struct {
	ID           string     `json:"id"`
	TenantID     string     `json:"tenant_id"`
	PhoneNumber  string     `json:"phone_number"`
	EmployeeName string     `json:"employee_name"`
	Role         SenderRole `json:"role"`
	IsActive     bool       `json:"is_active"`
	CreatedAt    time.Time  `json:"created_at"`
}

// ReceiptValidation representa un registro de auditoría en receipt_validations
type ReceiptValidation struct {
	ID                          string           `json:"id"`
	TenantID                    string           `json:"tenant_id"`
	SenderPhone                 string           `json:"sender_phone"`
	WAMessageID                 string           `json:"wa_message_id"`
	FileSHA256                  string           `json:"file_sha256"`
	ExtractedAmount             float64          `json:"extracted_amount"`
	ExtractedOpNumber           string           `json:"extracted_op_number"`
	ExtractedCoelsaID           string           `json:"extracted_coelsa_id"`
	ExtractedDate               *time.Time       `json:"extracted_date,omitempty"`
	ExtractedSenderName         string           `json:"extracted_sender_name"`
	ExtractedSenderCUIT         string           `json:"extracted_sender_cuit"`
	ExtractedReceiverAliasOrCBU string           `json:"extracted_receiver_alias_or_cbu"`
	ExtractedSourceBank         string           `json:"extracted_source_bank"`
	AIFraudWarnings             []string         `json:"ai_fraud_warnings"`
	ValidationResult            ValidationStatus `json:"validation_result"`
	MatchedMPPaymentID          string           `json:"matched_mp_payment_id,omitempty"`
	CustomerID                  string           `json:"customer_id,omitempty"`
	ProcessingTimeMs            int              `json:"processing_time_ms"`
	CreatedAt                   time.Time        `json:"created_at"`
}

// ValidationFilter filtra y pagina el historial de comprobantes del panel.
type ValidationFilter struct {
	From, To   *time.Time         // rango de fechas [From, To)
	Statuses   []ValidationStatus // vacío = todos
	CustomerID string             // vacío = todos los clientes
	Limit      int
	Offset     int
}

// VerifiedReceiptRef resume una validación ya verificada, para detectar el mismo comprobante
// reenviado en otro formato (PDF y captura) aunque no se haya podido leer su número de operación.
type VerifiedReceiptRef struct {
	ValidatedAt time.Time
	SenderCUIT  string
	SenderName  string
	ReceiptTime *time.Time // fecha/hora leída del comprobante, si se pudo
	PaymentDate *time.Time // hora en que acreditó el pago de MP asignado
	PayerCUIT   string     // CUIT del pagador según Mercado Pago
}

// argentinaTZ es la hora de Argentina (UTC-3, sin horario de verano).
var argentinaTZ = time.FixedZone("ART", -3*60*60)

// ParseReceiptTimestamp interpreta la fecha/hora leída del comprobante. Si no trae zona
// horaria se asume hora de Argentina, que es la que muestran los comprobantes.
func ParseReceiptTimestamp(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02 15:04"} {
		if t, err := time.ParseInLocation(layout, s, argentinaTZ); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// PendingReceiptQueueItem representa un comprobante en la Cola de Espera Reactiva (Redis 3 min TTL)
// Innovación 1.4.3: "Te aviso apenas entre"
type PendingReceiptQueueItem struct {
	ValidationID       string    `json:"validation_id"`
	TenantID           string    `json:"tenant_id"`
	SenderPhone        string    `json:"sender_phone"`
	EmployeeName       string    `json:"employee_name"`
	ExpectedAmount     float64   `json:"expected_amount"`
	OperationNumber    string    `json:"operation_number"`
	CoelsaID           string    `json:"coelsa_id"`
	SourceBankOrWallet string    `json:"source_bank_or_wallet"`
	SenderName         string    `json:"sender_name"`
	SenderCUIT         string    `json:"sender_cuit,omitempty"`
	FileSHA256         string    `json:"file_sha256,omitempty"`
	MPPaymentID        int64     `json:"mp_payment_id,omitempty"` // >0 si el comprobante trae un ID de MP
	MatchByAmount      bool      `json:"match_by_amount"`         // true solo si el comprobante no trae ningún ID
	CreatedAt          time.Time `json:"created_at"`
	ExpiresAt          time.Time `json:"expires_at"`
}

// UniqueChargeOrder representa una orden generada por el comando /cobrar o Posnet Virtual
// Innovación 1.4.4: Centavos/Pesos Críticos como identificador único para transferencias CVU
type UniqueChargeOrder struct {
	ID             string     `json:"id"`
	TenantID       string     `json:"tenant_id"`
	RequestedBy    string     `json:"requested_by"`
	RequestedPhone string     `json:"requested_phone,omitempty"`
	Reference      string     `json:"reference"`
	BaseAmount     float64    `json:"base_amount"`
	UniqueAmount   float64    `json:"unique_amount"`
	AliasOrCVU     string     `json:"alias_or_cvu"`
	Status         string     `json:"status"` // PENDING | PAID | EXPIRED
	MatchedMPID    int64      `json:"matched_mp_id,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	ExpiresAt      time.Time  `json:"expires_at"`
	PaidAt         *time.Time `json:"paid_at,omitempty"`
}

// ---------------------------------------------------------------------------
// Clientes (quienes transfieren al comercio)
// ---------------------------------------------------------------------------

// Customer es una persona que le transfirió (o intentó transferir) al comercio. Se identifica por
// CUIT; si no se conoce, por el nombre normalizado.
type Customer struct {
	ID          string    `json:"id"`
	TenantID    string    `json:"tenant_id"`
	IdentityKey string    `json:"-"`
	CUIT        string    `json:"cuit,omitempty"`
	Name        string    `json:"name"`
	NameKey     string    `json:"-"`
	Email       string    `json:"email,omitempty"`
	FirstSeenAt time.Time `json:"first_seen_at"`
	LastSeenAt  time.Time `json:"last_seen_at"`
}

// CustomerStats son los totales del historial de un cliente (se calculan con consultas, no se guardan).
type CustomerStats struct {
	Receipts       int        `json:"receipts"`
	Verified       int        `json:"verified"`
	Fraud          int        `json:"fraud"`
	Rejected       int        `json:"rejected"`
	NotFound       int        `json:"not_found"`
	VerifiedAmount float64    `json:"verified_amount"`
	Payments       int        `json:"payments"`
	LastFraudAt    *time.Time `json:"last_fraud_at,omitempty"`
}

// CustomerSummary es un cliente con sus totales, para el panel.
type CustomerSummary struct {
	Customer
	CustomerStats
}

// CustomerFilter busca y pagina clientes.
type CustomerFilter struct {
	Query     string // nombre o CUIT
	OnlyFraud bool   // solo clientes con algún intento de fraude
	Limit     int
	Offset    int
}

// CustomerRisk es el resultado de cruzar un comprobante con el historial de su cliente.
type CustomerRisk struct {
	CustomerID     string     `json:"customer_id"`
	PreviousFraud  int        `json:"previous_fraud"`
	LastFraudAt    *time.Time `json:"last_fraud_at,omitempty"`
	RecentFailures int        `json:"recent_failures"` // rechazos y fraudes en las últimas 24 h
	// Alertas en texto: CUIT con otro nombre, CUIT distinto al del pagador en MP, etc.
	Notes []string `json:"notes,omitempty"`
}

// HasWarnings indica si hay algo que avisar al comercio.
func (r *CustomerRisk) HasWarnings() bool {
	return r != nil && (r.PreviousFraud > 0 || r.RecentFailures >= RecentFailuresAlert || len(r.Notes) > 0)
}

// DefaultTimeWindowMinutes es la ventana inicial para buscar pagos sin número de operación.
const DefaultTimeWindowMinutes = 120

// RecentFailuresAlert: a partir de cuántos rechazos en 24 h se avisa.
const RecentFailuresAlert = 2

// FraudStatuses son los resultados que cuentan como intento de fraude en el historial.
var FraudStatuses = []ValidationStatus{
	StatusFraudTampered, StatusFraudDuplicateFile, StatusFraudAlreadyUsed, StatusFraudIncomplete, StatusRejectedAmountDiff,
}

// IsFraudStatus indica si el resultado es un intento de fraude.
func IsFraudStatus(s ValidationStatus) bool {
	for _, f := range FraudStatuses {
		if f == s {
			return true
		}
	}
	return false
}

// NormalizeCUIT deja solo los dígitos; devuelve "" si no son 11.
func NormalizeCUIT(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	if b.Len() != 11 {
		return ""
	}
	return b.String()
}

// NormalizePersonName pasa a minúsculas, quita acentos y espacios de más. Sirve para comparar
// "Adrián  Gómez" con "ADRIAN GOMEZ".
func NormalizePersonName(s string) string {
	r := strings.NewReplacer("á", "a", "é", "e", "í", "i", "ó", "o", "ú", "u", "ü", "u", "ñ", "n",
		"Á", "a", "É", "e", "Í", "i", "Ó", "o", "Ú", "u", "Ü", "u", "Ñ", "n")
	return strings.Join(strings.Fields(strings.ToLower(r.Replace(s))), " ")
}

// CustomerIdentityKey identifica a un cliente dentro del comercio: por CUIT si lo hay, si no por
// nombre. Devuelve "" si no hay datos suficientes.
func CustomerIdentityKey(cuit, name string) string {
	if c := NormalizeCUIT(cuit); c != "" {
		return "CUIT:" + c
	}
	if n := NormalizePersonName(name); len(n) >= 3 {
		return "NAME:" + n
	}
	return ""
}

// SamePersonName indica si dos nombres son compatibles (iguales, o uno contiene al otro, como
// "Adrian Gomez" y "Adrian Alberto Gomez").
func SamePersonName(a, b string) bool {
	a, b = NormalizePersonName(a), NormalizePersonName(b)
	if a == "" || b == "" || a == b {
		return true
	}
	wa, wb := strings.Fields(a), strings.Fields(b)
	if len(wa) > len(wb) {
		wa, wb = wb, wa
	}
	set := map[string]bool{}
	for _, w := range wb {
		set[w] = true
	}
	for _, w := range wa {
		if !set[w] {
			return false
		}
	}
	return true
}
