package domain

import (
	"context"
	"time"
)

// PaymentLedger es el puerto que necesita el caso de uso de verificación:
// libro mayor de pagos de MP con bloqueo transaccional y auditoría.
type PaymentLedger interface {
	// CheckFileDuplicate indica si el mismo archivo ya fue usado en una validación VERIFICADA.
	CheckFileDuplicate(ctx context.Context, tenantID string, fileSHA256 string) (bool, time.Time, error)
	// FindVerifiedByReceiptID indica si ya hay una validación VERIFICADA con ese número de operación
	// (o ID COELSA) y ese monto: detecta el mismo comprobante aunque la imagen sea otra.
	FindVerifiedByReceiptID(ctx context.Context, tenantID string, receiptID string, amount float64) (bool, time.Time, error)
	// ListRecentVerifiedByAmount devuelve las validaciones VERIFICADAS por ese monto desde "since",
	// para reconocer el mismo comprobante enviado como PDF y como captura.
	ListRecentVerifiedByAmount(ctx context.Context, tenantID string, amount float64, since time.Time) ([]VerifiedReceiptRef, error)
	UpsertMPPayment(ctx context.Context, p *MPPayment) error
	ClaimPaymentByExactID(ctx context.Context, tenantID string, mpPaymentID int64, expectedAmount float64, consumerID string) (*MPPayment, error)
	ClaimPaymentByCoelsaID(ctx context.Context, tenantID string, coelsaID string, expectedAmount float64, consumerID string) (*MPPayment, error)
	ClaimPaymentByHeuristic(ctx context.Context, tenantID string, expectedAmount float64, timeWindowMinutes int, senderCUIT string, consumerID string) (*MPPayment, error)
	SaveReceiptValidation(ctx context.Context, v *ReceiptValidation) error
	// UpsertCustomer crea o actualiza al cliente (por CUIT, o por nombre si no hay CUIT).
	// Devuelve nil si no hay datos para identificarlo.
	UpsertCustomer(ctx context.Context, tenantID, cuit, name, email string, seenAt time.Time) (*Customer, error)
	// LinkPaymentCustomer asocia un pago de MP a un cliente si todavía no tenía uno.
	LinkPaymentCustomer(ctx context.Context, tenantID, paymentID, customerID string) error
	// CustomerRiskStats: intentos de fraude del cliente (y el último) y rechazos desde "since".
	CustomerRiskStats(ctx context.Context, tenantID, customerID string, since time.Time) (fraud int, lastFraud *time.Time, recentFailures int, err error)
	IncrementTenantQuota(ctx context.Context, tenantID string) error
}

// Repository define el puerto de persistencia transaccional completo (Arquitectura Hexagonal)
type Repository interface {
	PaymentLedger

	ExistsValidationByWAMessageID(ctx context.Context, waMessageID string) (bool, error)
	UpdateReceiptValidationStatus(ctx context.Context, validationID string, status ValidationStatus, matchedPaymentUUID string) error
	GetTenantByID(ctx context.Context, tenantID string) (*Tenant, error)
	UpdateTenantPlan(ctx context.Context, tenantID string, plan PlanType, quota int, timeWindowMinutes int) error
	GetTenantBySenderPhone(ctx context.Context, phone string) (*Tenant, *AuthorizedSender, error)
	GetActiveMPAccount(ctx context.Context, tenantID string) (*MPAccount, error)
	GetMPAccountByMPUserID(ctx context.Context, mpUserID int64) (*MPAccount, error)
	UpsertMPAccount(ctx context.Context, acc *MPAccount) error
	ListAuthorizedSenders(ctx context.Context, tenantID string) ([]*AuthorizedSender, error)
	CreateAuthorizedSender(ctx context.Context, s *AuthorizedSender) error
	DeleteAuthorizedSender(ctx context.Context, tenantID, senderID string) error
	ListReceiptValidations(ctx context.Context, tenantID string, limit int) ([]*ReceiptValidation, error)
	// SearchReceiptValidations devuelve una página del historial y el total que cumple el filtro.
	SearchReceiptValidations(ctx context.Context, tenantID string, f ValidationFilter) ([]*ReceiptValidation, int, error)
	// CountValidationsByStatus cuenta los comprobantes por resultado en el rango [from, to).
	CountValidationsByStatus(ctx context.Context, tenantID string, from, to time.Time) (map[ValidationStatus]int, error)
	// DeleteReceiptValidation borra un comprobante y libera el pago de MP que tenía asignado.
	// Devuelve si se liberó un pago. ErrValidationNotFound si no existe.
	DeleteReceiptValidation(ctx context.Context, tenantID, validationID string) (bool, error)
	ListMPPayments(ctx context.Context, tenantID string, limit int) ([]*MPPayment, error)
	SearchCustomers(ctx context.Context, tenantID string, f CustomerFilter) ([]CustomerSummary, int, error)
	GetCustomerSummary(ctx context.Context, tenantID, customerID string) (*CustomerSummary, error)
	CustomerSummariesByIDs(ctx context.Context, tenantID string, ids []string) (map[string]CustomerSummary, error)
	// EnsureTenant devuelve el comercio de la instalación (lo crea vacío si no hay ninguno).
	EnsureTenant(ctx context.Context) (*Tenant, error)
	UpdateTenantProfile(ctx context.Context, tenantID, businessName, cuit, email string) error
}

// ReceiptExtractor define el puerto para extracción forense multimodal (Gemini 2.5 Flash)
type ReceiptExtractor interface {
	ExtractFromMedia(ctx context.Context, fileBytes []byte, mimeType string) (*ExtractedReceiptData, error)
}

// MercadoPagoGateway define el puerto para consultar pagos en Mercado Pago
type MercadoPagoGateway interface {
	GetPaymentByID(ctx context.Context, accessToken string, tenantID string, paymentID string) (*MPPayment, error)
	FetchRecentApprovedPayments(ctx context.Context, accessToken string, tenantID string) ([]*MPPayment, error)
}

// WhatsAppGateway define el puerto para enviar mensajes y descargar media de WhatsApp Cloud API
type WhatsAppGateway interface {
	SendTextMessage(ctx context.Context, toPhone string, body string) error
	DownloadMedia(ctx context.Context, mediaID string) ([]byte, string, error)
}

// ReactiveQueue define el puerto para la Cola de Espera Reactiva y las órdenes /cobrar
type ReactiveQueue interface {
	EnqueuePendingReceipt(ctx context.Context, item *PendingReceiptQueueItem) error
	ListPendingReceipts(ctx context.Context, tenantID string) ([]*PendingReceiptQueueItem, error)
	RemovePendingReceipt(ctx context.Context, tenantID, validationID string) error
	RemovePendingByFileHash(ctx context.Context, tenantID, fileSHA256 string) error
	CreateUniqueChargeOrder(ctx context.Context, tenantID, requestedBy, requestedPhone, reference, aliasOrCVU string, baseAmount float64) (*UniqueChargeOrder, error)
	MatchChargeOrder(ctx context.Context, tenantID string, amount float64, mpPaymentID int64) (*UniqueChargeOrder, error)
	ListChargeOrders(ctx context.Context, tenantID string) ([]*UniqueChargeOrder, error)
}
