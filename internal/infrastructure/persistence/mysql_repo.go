package persistence

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"veripago-saas/internal/domain"
)

// Alias de los errores del dominio (se mantienen para compatibilidad).
var (
	ErrPaymentAlreadyConsumed = domain.ErrPaymentAlreadyConsumed
	ErrPaymentNotFound        = domain.ErrPaymentNotFound
	ErrPaymentAmountMismatch  = domain.ErrPaymentAmountMismatch
)

const (
	DefaultDemoTenantID    = "11111111-2222-3333-4444-555555555555"
	DefaultDemoMPAccountID = "22222222-3333-4444-5555-666666666666"
)

var _ domain.Repository = (*MySQLRepository)(nil)

// verifiedStatusesSQL son los resultados que "usan" un archivo: solo esos cuentan como archivo repetido.
const verifiedStatusesSQL = "'VERIFIED_EXACT_ID','VERIFIED_HEURISTIC'"

func isVerifiedStatus(s domain.ValidationStatus) bool {
	return s == domain.StatusVerifiedExactID || s == domain.StatusVerifiedHeuristic
}

// currentQuotaPeriod devuelve el período mensual de cuota (ej: "2026-10").
func currentQuotaPeriod() string {
	return time.Now().Format("2006-01")
}

func digitsOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// checkClaimable aplica las reglas comunes a todos los métodos de conciliación.
func checkClaimable(p *domain.MPPayment, expectedAmount float64) error {
	if p.ReconciliationStatus == "CONSUMED" {
		return ErrPaymentAlreadyConsumed
	}
	if p.MPStatus != "approved" {
		return ErrPaymentNotFound
	}
	if !domain.SameAmount(p.Amount, expectedAmount) {
		return ErrPaymentAmountMismatch
	}
	return nil
}

type MySQLRepository struct {
	db  *sql.DB
	mem *inMemoryStore // Respaldo transaccional en memoria cuando MySQL no está conectado
}

type inMemoryStore struct {
	mu           sync.Mutex
	quotaPeriods map[string]string // tenantID -> período de la cuota usada
	tenants      map[string]*domain.Tenant
	mpAccounts   map[string]*domain.MPAccount
	senders      map[string]*domain.AuthorizedSender
	payments     map[string]*domain.MPPayment // key: tenantID + ":" + mpPaymentID
	validations  []*domain.ReceiptValidation
}

func newInMemoryStore() *inMemoryStore {
	return &inMemoryStore{
		quotaPeriods: make(map[string]string),
		tenants:      make(map[string]*domain.Tenant),
		mpAccounts:   make(map[string]*domain.MPAccount),
		senders:      make(map[string]*domain.AuthorizedSender),
		payments:     make(map[string]*domain.MPPayment),
		validations:  make([]*domain.ReceiptValidation, 0),
	}
}

func NewMySQLRepository(db *sql.DB) *MySQLRepository {
	return &MySQLRepository{
		db:  db,
		mem: newInMemoryStore(),
	}
}

// IsUsingMySQL devuelve true si el repositorio está conectado a una instancia MySQL 8.0 activa.
func (r *MySQLRepository) IsUsingMySQL() bool {
	return r.db != nil
}

// CheckFileDuplicate verifica en <1ms si el mismo archivo exacto ya fue enviado antes
func (r *MySQLRepository) CheckFileDuplicate(ctx context.Context, tenantID string, fileSHA256 string) (bool, time.Time, error) {
	if r.db == nil {
		r.mem.mu.Lock()
		defer r.mem.mu.Unlock()
		for _, v := range r.mem.validations {
			if v.TenantID == tenantID && v.FileSHA256 == fileSHA256 && isVerifiedStatus(v.ValidationResult) {
				return true, v.CreatedAt, nil
			}
		}
		return false, time.Time{}, nil
	}

	var createdAt time.Time
	query := `SELECT created_at FROM receipt_validations
		WHERE tenant_id = ? AND file_sha256 = ? AND validation_result IN (` + verifiedStatusesSQL + `)
		ORDER BY created_at ASC LIMIT 1`
	err := r.db.QueryRowContext(ctx, query, tenantID, fileSHA256).Scan(&createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, time.Time{}, nil
	}
	if err != nil {
		return false, time.Time{}, err
	}
	return true, createdAt, nil
}

// NormalizeReceiptID deja solo letras y números en mayúsculas, para comparar números de
// operación aunque el comprobante los muestre con guiones o espacios.
func NormalizeReceiptID(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// receiptIDSQL normaliza una columna en SQL igual que NormalizeReceiptID (guiones, espacios y puntos).
func receiptIDSQL(col string) string {
	return "UPPER(REPLACE(REPLACE(REPLACE(COALESCE(" + col + ",''),'-',''),' ',''),'.',''))"
}

// FindVerifiedByReceiptID busca una validación verificada con el mismo número de operación
// (o ID COELSA) y el mismo monto.
func (r *MySQLRepository) FindVerifiedByReceiptID(ctx context.Context, tenantID string, receiptID string, amount float64) (bool, time.Time, error) {
	id := NormalizeReceiptID(receiptID)
	if id == "" {
		return false, time.Time{}, nil
	}
	if r.db == nil {
		r.mem.mu.Lock()
		defer r.mem.mu.Unlock()
		for _, v := range r.mem.validations {
			if v.TenantID == tenantID && isVerifiedStatus(v.ValidationResult) && domain.SameAmount(v.ExtractedAmount, amount) &&
				(NormalizeReceiptID(v.ExtractedOpNumber) == id || NormalizeReceiptID(v.ExtractedCoelsaID) == id) {
				return true, v.CreatedAt, nil
			}
		}
		return false, time.Time{}, nil
	}

	var createdAt time.Time
	query := `SELECT created_at FROM receipt_validations
		WHERE tenant_id = ? AND extracted_amount = CAST(? AS DECIMAL(15,2))
		  AND validation_result IN (` + verifiedStatusesSQL + `)
		  AND (` + receiptIDSQL("extracted_op_number") + ` = ? OR ` + receiptIDSQL("extracted_coelsa_id") + ` = ?)
		ORDER BY created_at ASC LIMIT 1`
	err := r.db.QueryRowContext(ctx, query, tenantID, strconv.FormatFloat(amount, 'f', 2, 64), id, id).Scan(&createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, time.Time{}, nil
	}
	if err != nil {
		return false, time.Time{}, err
	}
	return true, createdAt, nil
}

// ExistsValidationByWAMessageID evita procesar dos veces el mismo mensaje cuando Meta reenvía el webhook.
func (r *MySQLRepository) ExistsValidationByWAMessageID(ctx context.Context, waMessageID string) (bool, error) {
	if waMessageID == "" {
		return false, nil
	}
	if r.db == nil {
		r.mem.mu.Lock()
		defer r.mem.mu.Unlock()
		for _, v := range r.mem.validations {
			if v.WAMessageID == waMessageID {
				return true, nil
			}
		}
		return false, nil
	}
	var one int
	err := r.db.QueryRowContext(ctx, `SELECT 1 FROM receipt_validations WHERE wa_message_id = ? LIMIT 1`, waMessageID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// UpsertMPPayment inserta un pago proveniente de Webhook o de la API de MP sin pisar su estado si ya fue consumido
func (r *MySQLRepository) UpsertMPPayment(ctx context.Context, p *domain.MPPayment) error {
	if p.ID == "" {
		p.ID = uuid.NewString()
	}
	if p.Currency == "" {
		p.Currency = "ARS"
	}
	if p.PaymentDate.IsZero() {
		p.PaymentDate = time.Now()
	}
	if len(p.RawJSON) == 0 {
		p.RawJSON, _ = json.Marshal(p)
	}

	if r.db == nil {
		r.mem.mu.Lock()
		defer r.mem.mu.Unlock()
		key := memPaymentKey(p.TenantID, p.MPPaymentID)
		if existing, ok := r.mem.payments[key]; ok {
			existing.MPStatus = p.MPStatus
			if p.PayerCUIT != "" {
				existing.PayerCUIT = p.PayerCUIT
			}
			if p.PayerName != "" {
				existing.PayerName = p.PayerName
			}
			if p.CoelsaID != "" {
				existing.CoelsaID = p.CoelsaID
			}
			return nil
		}
		copyP := *p
		if copyP.ReconciliationStatus == "" {
			copyP.ReconciliationStatus = "AVAILABLE"
		}
		if copyP.CreatedAt.IsZero() {
			copyP.CreatedAt = time.Now()
		}
		r.mem.payments[key] = &copyP
		return nil
	}

	query := `
		INSERT INTO mp_payments (
			id, tenant_id, mp_payment_id, amount, currency, mp_status,
			operation_type, payer_cuit, payer_name, coelsa_id, payment_date,
			reconciliation_status, raw_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'AVAILABLE', ?)
		ON DUPLICATE KEY UPDATE
			mp_status = VALUES(mp_status),
			payer_cuit = COALESCE(NULLIF(VALUES(payer_cuit), ''), payer_cuit),
			coelsa_id = COALESCE(NULLIF(VALUES(coelsa_id), ''), coelsa_id)
	`
	_, err := r.db.ExecContext(ctx, query,
		p.ID, p.TenantID, p.MPPaymentID, p.Amount, p.Currency, p.MPStatus,
		p.OperationType, p.PayerCUIT, p.PayerName, p.CoelsaID, p.PaymentDate,
		string(p.RawJSON),
	)
	return err
}

// ClaimPaymentByExactID bloquea la fila con FOR UPDATE, verifica que no haya sido usada y que coincida el monto, y la marca como CONSUMED
func (r *MySQLRepository) ClaimPaymentByExactID(ctx context.Context, tenantID string, mpPaymentID int64, expectedAmount float64, receiptValidationID string) (*domain.MPPayment, error) {
	if r.db == nil {
		r.mem.mu.Lock()
		defer r.mem.mu.Unlock()

		key := memPaymentKey(tenantID, mpPaymentID)
		p, ok := r.mem.payments[key]
		if !ok {
			return nil, ErrPaymentNotFound
		}
		if err := checkClaimable(p, expectedAmount); err != nil {
			copyP := *p
			return &copyP, err
		}
		now := time.Now()
		p.ReconciliationStatus = "CONSUMED"
		p.ConsumedAt = &now
		p.ConsumedByReceiptID = receiptValidationID
		copyP := *p
		return &copyP, nil
	}

	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var p domain.MPPayment
	querySelect := `
		SELECT id, mp_payment_id, amount, mp_status, payment_date, reconciliation_status,
		       COALESCE(consumed_by_receipt_id, '')
		FROM mp_payments
		WHERE tenant_id = ? AND mp_payment_id = ?
		FOR UPDATE
	`
	err = tx.QueryRowContext(ctx, querySelect, tenantID, mpPaymentID).Scan(
		&p.ID, &p.MPPaymentID, &p.Amount, &p.MPStatus, &p.PaymentDate, &p.ReconciliationStatus, &p.ConsumedByReceiptID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPaymentNotFound
	}
	if err != nil {
		return nil, err
	}

	// Verificamos que no esté consumido, que esté aprobado y que el monto coincida al centavo
	if err := checkClaimable(&p, expectedAmount); err != nil {
		return &p, err
	}

	queryUpdate := `
		UPDATE mp_payments
		SET reconciliation_status = 'CONSUMED', consumed_at = NOW(), consumed_by_receipt_id = ?
		WHERE id = ?
	`
	if _, err := tx.ExecContext(ctx, queryUpdate, receiptValidationID, p.ID); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	p.ReconciliationStatus = "CONSUMED"
	return &p, nil
}

// ClaimPaymentByCoelsaID (Para transferencias desde otros bancos por CVU)
// Busca el pago por el ID COELSA del comprobante, que Mercado Pago informa como e2e_id
func (r *MySQLRepository) ClaimPaymentByCoelsaID(ctx context.Context, tenantID string, coelsaID string, expectedAmount float64, receiptValidationID string) (*domain.MPPayment, error) {
	if coelsaID == "" {
		return nil, ErrPaymentNotFound
	}

	if r.db == nil {
		r.mem.mu.Lock()
		defer r.mem.mu.Unlock()

		var p *domain.MPPayment
		for _, candidate := range r.mem.payments {
			if candidate.TenantID == tenantID && strings.EqualFold(candidate.CoelsaID, coelsaID) {
				p = candidate
				break
			}
		}
		if p == nil {
			return nil, ErrPaymentNotFound
		}
		if err := checkClaimable(p, expectedAmount); err != nil {
			copyP := *p
			return &copyP, err
		}
		now := time.Now()
		p.ReconciliationStatus = "CONSUMED"
		p.ConsumedAt = &now
		p.ConsumedByReceiptID = receiptValidationID
		copyP := *p
		return &copyP, nil
	}

	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var p domain.MPPayment
	querySelect := `
		SELECT id, mp_payment_id, amount, mp_status, payment_date, reconciliation_status, coelsa_id,
		       COALESCE(consumed_by_receipt_id, '')
		FROM mp_payments
		WHERE tenant_id = ? AND coelsa_id = ?
		LIMIT 1
		FOR UPDATE
	`
	err = tx.QueryRowContext(ctx, querySelect, tenantID, coelsaID).Scan(
		&p.ID, &p.MPPaymentID, &p.Amount, &p.MPStatus, &p.PaymentDate, &p.ReconciliationStatus, &p.CoelsaID, &p.ConsumedByReceiptID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPaymentNotFound
	}
	if err != nil {
		return nil, err
	}

	if err := checkClaimable(&p, expectedAmount); err != nil {
		return &p, err
	}

	queryUpdate := `
		UPDATE mp_payments
		SET reconciliation_status = 'CONSUMED', consumed_at = NOW(), consumed_by_receipt_id = ?
		WHERE id = ?
	`
	if _, err := tx.ExecContext(ctx, queryUpdate, receiptValidationID, p.ID); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	p.ReconciliationStatus = "CONSUMED"
	return &p, nil
}

// ClaimPaymentByHeuristic (Para comprobantes SIN ningún ID verificable)
// Busca un pago AVAILABLE con el monto exacto dentro de la ventana de tiempo permitida por el comercio.
// Si el comprobante trae CUIT del emisor y MP informa el CUIT del pagador, ambos deben coincidir.
func (r *MySQLRepository) ClaimPaymentByHeuristic(ctx context.Context, tenantID string, expectedAmount float64, timeWindowMinutes int, senderCUIT string, receiptValidationID string) (*domain.MPPayment, error) {
	cuit := digitsOnly(senderCUIT)
	if r.db == nil {
		r.mem.mu.Lock()
		defer r.mem.mu.Unlock()

		cutoff := time.Now().Add(-time.Duration(timeWindowMinutes) * time.Minute)
		var candidates []*domain.MPPayment
		for _, p := range r.mem.payments {
			if p.TenantID == tenantID &&
				p.ReconciliationStatus == "AVAILABLE" &&
				p.MPStatus == "approved" &&
				domain.SameAmount(p.Amount, expectedAmount) &&
				(cuit == "" || digitsOnly(p.PayerCUIT) == "" || digitsOnly(p.PayerCUIT) == cuit) &&
				!p.PaymentDate.Before(cutoff) {
				candidates = append(candidates, p)
			}
		}
		if len(candidates) == 0 {
			return nil, ErrPaymentNotFound
		}
		sort.Slice(candidates, func(i, j int) bool {
			return candidates[i].PaymentDate.After(candidates[j].PaymentDate)
		})
		chosen := candidates[0]
		now := time.Now()
		chosen.ReconciliationStatus = "CONSUMED"
		chosen.ConsumedAt = &now
		chosen.ConsumedByReceiptID = receiptValidationID
		copyP := *chosen
		return &copyP, nil
	}

	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var p domain.MPPayment
	querySelect := `
		SELECT id, mp_payment_id, amount, mp_status, payment_date, reconciliation_status
		FROM mp_payments
		WHERE tenant_id = ?
		  AND reconciliation_status = 'AVAILABLE'
		  AND mp_status = 'approved'
		  AND amount = CAST(? AS DECIMAL(15,2))
		  AND payment_date >= DATE_SUB(NOW(), INTERVAL ? MINUTE)
		  AND (? = '' OR COALESCE(payer_cuit, '') = '' OR payer_cuit = ?)
		ORDER BY payment_date DESC
		LIMIT 1
		FOR UPDATE
	`
	amountStr := strconv.FormatFloat(float64(domain.ToCents(expectedAmount))/100, 'f', 2, 64)
	err = tx.QueryRowContext(ctx, querySelect, tenantID, amountStr, timeWindowMinutes, cuit, cuit).Scan(
		&p.ID, &p.MPPaymentID, &p.Amount, &p.MPStatus, &p.PaymentDate, &p.ReconciliationStatus,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPaymentNotFound
	}
	if err != nil {
		return nil, err
	}

	queryUpdate := `
		UPDATE mp_payments
		SET reconciliation_status = 'CONSUMED', consumed_at = NOW(), consumed_by_receipt_id = ?
		WHERE id = ?
	`
	if _, err := tx.ExecContext(ctx, queryUpdate, receiptValidationID, p.ID); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	p.ReconciliationStatus = "CONSUMED"
	return &p, nil
}

func memPaymentKey(tenantID string, mpID int64) string {
	return tenantID + ":" + strconv.FormatInt(mpID, 10)
}

// SaveReceiptValidation registra la auditoría completa del comprobante procesado en receipt_validations.
func (r *MySQLRepository) SaveReceiptValidation(ctx context.Context, v *domain.ReceiptValidation) error {
	if v.ID == "" {
		v.ID = uuid.NewString()
	}
	if v.WAMessageID == "" {
		v.WAMessageID = "wa_" + uuid.NewString()
	}
	if v.CreatedAt.IsZero() {
		v.CreatedAt = time.Now()
	}

	if r.db == nil {
		r.mem.mu.Lock()
		defer r.mem.mu.Unlock()
		copyV := *v
		r.mem.validations = append([]*domain.ReceiptValidation{&copyV}, r.mem.validations...)
		return nil
	}

	warningsJSON, _ := json.Marshal(v.AIFraudWarnings)
	var matchedID interface{}
	if v.MatchedMPPaymentID != "" {
		matchedID = v.MatchedMPPaymentID
	}

	query := `
		INSERT INTO receipt_validations (
			id, tenant_id, sender_phone, wa_message_id, file_sha256,
			extracted_amount, extracted_op_number, extracted_coelsa_id,
			extracted_date, extracted_sender_name, extracted_sender_cuit,
			extracted_receiver_alias_or_cbu, extracted_source_bank,
			ai_fraud_warnings, validation_result, matched_mp_payment_id,
			processing_time_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	_, err := r.db.ExecContext(ctx, query,
		v.ID, v.TenantID, v.SenderPhone, v.WAMessageID, v.FileSHA256,
		v.ExtractedAmount, v.ExtractedOpNumber, v.ExtractedCoelsaID,
		v.ExtractedDate, v.ExtractedSenderName, v.ExtractedSenderCUIT,
		v.ExtractedReceiverAliasOrCBU, v.ExtractedSourceBank,
		string(warningsJSON), string(v.ValidationResult), matchedID,
		v.ProcessingTimeMs,
	)
	return err
}

// UpdateReceiptValidationStatus actualiza el estado de una validación cuando impacta un pago en la Cola de Espera Reactiva.
func (r *MySQLRepository) UpdateReceiptValidationStatus(ctx context.Context, validationID string, status domain.ValidationStatus, matchedPaymentUUID string) error {
	if r.db == nil {
		r.mem.mu.Lock()
		defer r.mem.mu.Unlock()
		for _, v := range r.mem.validations {
			if v.ID == validationID {
				v.ValidationResult = status
				v.MatchedMPPaymentID = matchedPaymentUUID
				return nil
			}
		}
		return nil
	}

	query := `UPDATE receipt_validations SET validation_result = ?, matched_mp_payment_id = ? WHERE id = ?`
	_, err := r.db.ExecContext(ctx, query, string(status), matchedPaymentUUID, validationID)
	return err
}

func (r *MySQLRepository) GetTenantByID(ctx context.Context, tenantID string) (*domain.Tenant, error) {
	if r.db == nil {
		r.mem.mu.Lock()
		defer r.mem.mu.Unlock()
		if t, ok := r.mem.tenants[tenantID]; ok {
			copyT := *t
			if p, ok := r.mem.quotaPeriods[tenantID]; ok && p != currentQuotaPeriod() {
				copyT.UsedQuota = 0 // cambió el mes: la cuota se reinicia
			}
			return &copyT, nil
		}
		return nil, sql.ErrNoRows
	}

	var t domain.Tenant
	var planStr string
	var period sql.NullString
	query := `
		SELECT id, business_name, cuit, email, password_hash, plan_type,
		       monthly_quota, used_quota, quota_period, time_window_minutes, is_active,
		       created_at, updated_at
		FROM tenants WHERE id = ?
	`
	err := r.db.QueryRowContext(ctx, query, tenantID).Scan(
		&t.ID, &t.BusinessName, &t.CUIT, &t.Email, &t.PasswordHash, &planStr,
		&t.MonthlyQuota, &t.UsedQuota, &period, &t.TimeWindowMinutes, &t.IsActive,
		&t.CreatedAt, &t.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	if period.Valid && period.String != currentQuotaPeriod() {
		t.UsedQuota = 0 // cambió el mes: la cuota se reinicia
	}
	t.PlanType = domain.PlanType(planStr)
	return &t, nil
}

func (r *MySQLRepository) UpdateTenantPlan(ctx context.Context, tenantID string, plan domain.PlanType, quota int, timeWindowMinutes int) error {
	if r.db == nil {
		r.mem.mu.Lock()
		defer r.mem.mu.Unlock()
		if t, ok := r.mem.tenants[tenantID]; ok {
			t.PlanType = plan
			t.MonthlyQuota = quota
			if timeWindowMinutes > 0 {
				t.TimeWindowMinutes = timeWindowMinutes
			}
			t.UpdatedAt = time.Now()
		}
		return nil
	}

	query := `UPDATE tenants SET plan_type = ?, monthly_quota = ?,
		time_window_minutes = IF(? > 0, ?, time_window_minutes) WHERE id = ?`
	_, err := r.db.ExecContext(ctx, query, string(plan), quota, timeWindowMinutes, timeWindowMinutes, tenantID)
	return err
}

func (r *MySQLRepository) IncrementTenantQuota(ctx context.Context, tenantID string) error {
	if r.db == nil {
		r.mem.mu.Lock()
		defer r.mem.mu.Unlock()
		if t, ok := r.mem.tenants[tenantID]; ok {
			period := currentQuotaPeriod()
			if p, ok := r.mem.quotaPeriods[tenantID]; ok && p != period {
				t.UsedQuota = 0
			}
			r.mem.quotaPeriods[tenantID] = period
			t.UsedQuota++
			t.UpdatedAt = time.Now()
		}
		return nil
	}

	// Si cambió el mes, la cuenta arranca de nuevo en 1
	period := currentQuotaPeriod()
	query := `UPDATE tenants
		SET used_quota = IF(quota_period <=> ?, used_quota + 1, 1), quota_period = ?
		WHERE id = ?`
	_, err := r.db.ExecContext(ctx, query, period, period, tenantID)
	return err
}

func (r *MySQLRepository) GetTenantBySenderPhone(ctx context.Context, phone string) (*domain.Tenant, *domain.AuthorizedSender, error) {
	cleanPhone := normalizePhone(phone)
	if r.db == nil {
		r.mem.mu.Lock()
		defer r.mem.mu.Unlock()
		for _, s := range r.mem.senders {
			if s.IsActive && normalizePhone(s.PhoneNumber) == cleanPhone {
				if t, ok := r.mem.tenants[s.TenantID]; ok && t.IsActive {
					copyT := *t
					copyS := *s
					return &copyT, &copyS, nil
				}
			}
		}
		return nil, nil, sql.ErrNoRows
	}

	var s domain.AuthorizedSender
	var roleStr string
	querySender := `
		SELECT id, tenant_id, phone_number, employee_name, role, is_active, created_at
		FROM authorized_senders
		WHERE phone_number = ? AND is_active = TRUE
		LIMIT 1
	`
	err := r.db.QueryRowContext(ctx, querySender, cleanPhone).Scan(
		&s.ID, &s.TenantID, &s.PhoneNumber, &s.EmployeeName, &roleStr, &s.IsActive, &s.CreatedAt,
	)
	if err != nil {
		return nil, nil, err
	}
	s.Role = domain.SenderRole(roleStr)

	t, err := r.GetTenantByID(ctx, s.TenantID)
	if err != nil {
		return nil, nil, err
	}
	return t, &s, nil
}

func (r *MySQLRepository) GetActiveMPAccount(ctx context.Context, tenantID string) (*domain.MPAccount, error) {
	if r.db == nil {
		r.mem.mu.Lock()
		defer r.mem.mu.Unlock()
		for _, acc := range r.mem.mpAccounts {
			if acc.TenantID == tenantID && acc.Status == "CONNECTED" {
				copyAcc := *acc
				return &copyAcc, nil
			}
		}
		return nil, sql.ErrNoRows
	}

	var acc domain.MPAccount
	var alias, cvu, holder, secret sql.NullString
	query := `
		SELECT id, tenant_id, mp_user_id, expected_alias, expected_cvu, expected_holder_name,
		       access_token_enc, refresh_token_enc, token_expires_at, webhook_secret,
		       status, created_at, updated_at
		FROM mp_accounts
		WHERE tenant_id = ? AND status = 'CONNECTED'
		ORDER BY updated_at DESC
		LIMIT 1
	`
	err := r.db.QueryRowContext(ctx, query, tenantID).Scan(
		&acc.ID, &acc.TenantID, &acc.MPUserID, &alias, &cvu, &holder,
		&acc.AccessTokenEnc, &acc.RefreshTokenEnc, &acc.TokenExpiresAt, &secret,
		&acc.Status, &acc.CreatedAt, &acc.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	acc.ExpectedAlias = alias.String
	acc.ExpectedCVU = cvu.String
	acc.ExpectedHolderName = holder.String
	acc.WebhookSecret = secret.String
	return &acc, nil
}

func (r *MySQLRepository) GetMPAccountByMPUserID(ctx context.Context, mpUserID int64) (*domain.MPAccount, error) {
	if r.db == nil {
		r.mem.mu.Lock()
		defer r.mem.mu.Unlock()
		for _, acc := range r.mem.mpAccounts {
			if acc.MPUserID == mpUserID {
				copyAcc := *acc
				return &copyAcc, nil
			}
		}
		return nil, sql.ErrNoRows
	}

	var acc domain.MPAccount
	var alias, cvu, holder, secret sql.NullString
	query := `
		SELECT id, tenant_id, mp_user_id, expected_alias, expected_cvu, expected_holder_name,
		       access_token_enc, refresh_token_enc, token_expires_at, webhook_secret,
		       status, created_at, updated_at
		FROM mp_accounts
		WHERE mp_user_id = ?
		LIMIT 1
	`
	err := r.db.QueryRowContext(ctx, query, mpUserID).Scan(
		&acc.ID, &acc.TenantID, &acc.MPUserID, &alias, &cvu, &holder,
		&acc.AccessTokenEnc, &acc.RefreshTokenEnc, &acc.TokenExpiresAt, &secret,
		&acc.Status, &acc.CreatedAt, &acc.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	acc.ExpectedAlias = alias.String
	acc.ExpectedCVU = cvu.String
	acc.ExpectedHolderName = holder.String
	acc.WebhookSecret = secret.String
	return &acc, nil
}

func (r *MySQLRepository) UpsertMPAccount(ctx context.Context, acc *domain.MPAccount) error {
	if acc.ID == "" {
		acc.ID = uuid.NewString()
	}
	if acc.Status == "" {
		acc.Status = "CONNECTED"
	}
	now := time.Now()
	if acc.CreatedAt.IsZero() {
		acc.CreatedAt = now
	}
	acc.UpdatedAt = now

	if r.db == nil {
		r.mem.mu.Lock()
		defer r.mem.mu.Unlock()
		copyAcc := *acc
		r.mem.mpAccounts[acc.TenantID] = &copyAcc
		return nil
	}

	query := `
		INSERT INTO mp_accounts (
			id, tenant_id, mp_user_id, expected_alias, expected_cvu, expected_holder_name,
			access_token_enc, refresh_token_enc, token_expires_at, webhook_secret, status
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			expected_alias = VALUES(expected_alias),
			expected_cvu = VALUES(expected_cvu),
			expected_holder_name = VALUES(expected_holder_name),
			access_token_enc = VALUES(access_token_enc),
			refresh_token_enc = VALUES(refresh_token_enc),
			token_expires_at = VALUES(token_expires_at),
			webhook_secret = VALUES(webhook_secret),
			status = VALUES(status)
	`
	_, err := r.db.ExecContext(ctx, query,
		acc.ID, acc.TenantID, acc.MPUserID, acc.ExpectedAlias, acc.ExpectedCVU, acc.ExpectedHolderName,
		acc.AccessTokenEnc, acc.RefreshTokenEnc, acc.TokenExpiresAt, acc.WebhookSecret, acc.Status,
	)
	return err
}

func (r *MySQLRepository) ListAuthorizedSenders(ctx context.Context, tenantID string) ([]*domain.AuthorizedSender, error) {
	if r.db == nil {
		r.mem.mu.Lock()
		defer r.mem.mu.Unlock()
		var list []*domain.AuthorizedSender
		for _, s := range r.mem.senders {
			if s.TenantID == tenantID {
				copyS := *s
				list = append(list, &copyS)
			}
		}
		sort.Slice(list, func(i, j int) bool {
			return list[i].CreatedAt.Before(list[j].CreatedAt)
		})
		return list, nil
	}

	query := `
		SELECT id, tenant_id, phone_number, employee_name, role, is_active, created_at
		FROM authorized_senders
		WHERE tenant_id = ?
		ORDER BY created_at ASC
	`
	rows, err := r.db.QueryContext(ctx, query, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []*domain.AuthorizedSender
	for rows.Next() {
		var s domain.AuthorizedSender
		var roleStr string
		if err := rows.Scan(&s.ID, &s.TenantID, &s.PhoneNumber, &s.EmployeeName, &roleStr, &s.IsActive, &s.CreatedAt); err != nil {
			return nil, err
		}
		s.Role = domain.SenderRole(roleStr)
		list = append(list, &s)
	}
	return list, rows.Err()
}

func (r *MySQLRepository) CreateAuthorizedSender(ctx context.Context, s *domain.AuthorizedSender) error {
	if s.ID == "" {
		s.ID = uuid.NewString()
	}
	s.PhoneNumber = normalizePhone(s.PhoneNumber)
	if s.Role == "" {
		s.Role = domain.RoleCashier
	}
	s.IsActive = true
	if s.CreatedAt.IsZero() {
		s.CreatedAt = time.Now()
	}

	if r.db == nil {
		r.mem.mu.Lock()
		defer r.mem.mu.Unlock()
		copyS := *s
		r.mem.senders[s.ID] = &copyS
		return nil
	}

	query := `
		INSERT INTO authorized_senders (id, tenant_id, phone_number, employee_name, role, is_active)
		VALUES (?, ?, ?, ?, ?, TRUE)
		ON DUPLICATE KEY UPDATE
			employee_name = VALUES(employee_name),
			role = VALUES(role),
			is_active = TRUE
	`
	_, err := r.db.ExecContext(ctx, query, s.ID, s.TenantID, s.PhoneNumber, s.EmployeeName, string(s.Role))
	return err
}

func (r *MySQLRepository) DeleteAuthorizedSender(ctx context.Context, tenantID, senderID string) error {
	if r.db == nil {
		r.mem.mu.Lock()
		defer r.mem.mu.Unlock()
		if s, ok := r.mem.senders[senderID]; ok && s.TenantID == tenantID {
			delete(r.mem.senders, senderID)
		}
		return nil
	}

	query := `DELETE FROM authorized_senders WHERE id = ? AND tenant_id = ?`
	_, err := r.db.ExecContext(ctx, query, senderID, tenantID)
	return err
}

func (r *MySQLRepository) ListReceiptValidations(ctx context.Context, tenantID string, limit int) ([]*domain.ReceiptValidation, error) {
	if limit <= 0 {
		limit = 50
	}
	if r.db == nil {
		r.mem.mu.Lock()
		defer r.mem.mu.Unlock()
		var list []*domain.ReceiptValidation
		for _, v := range r.mem.validations {
			if v.TenantID == tenantID {
				copyV := *v
				list = append(list, &copyV)
				if len(list) >= limit {
					break
				}
			}
		}
		return list, nil
	}

	query := `
		SELECT id, tenant_id, sender_phone, wa_message_id, file_sha256,
		       COALESCE(extracted_amount, 0), COALESCE(extracted_op_number, ''),
		       COALESCE(extracted_coelsa_id, ''), extracted_date,
		       COALESCE(extracted_sender_name, ''), COALESCE(extracted_sender_cuit, ''),
		       COALESCE(extracted_receiver_alias_or_cbu, ''), COALESCE(extracted_source_bank, ''),
		       ai_fraud_warnings, validation_result, COALESCE(matched_mp_payment_id, ''),
		       COALESCE(processing_time_ms, 0), created_at
		FROM receipt_validations
		WHERE tenant_id = ?
		ORDER BY created_at DESC
		LIMIT ?
	`
	rows, err := r.db.QueryContext(ctx, query, tenantID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []*domain.ReceiptValidation
	for rows.Next() {
		var v domain.ReceiptValidation
		var extDate sql.NullTime
		var warningsRaw []byte
		var statusStr string
		if err := rows.Scan(
			&v.ID, &v.TenantID, &v.SenderPhone, &v.WAMessageID, &v.FileSHA256,
			&v.ExtractedAmount, &v.ExtractedOpNumber, &v.ExtractedCoelsaID, &extDate,
			&v.ExtractedSenderName, &v.ExtractedSenderCUIT,
			&v.ExtractedReceiverAliasOrCBU, &v.ExtractedSourceBank,
			&warningsRaw, &statusStr, &v.MatchedMPPaymentID,
			&v.ProcessingTimeMs, &v.CreatedAt,
		); err != nil {
			return nil, err
		}
		if extDate.Valid {
			t := extDate.Time
			v.ExtractedDate = &t
		}
		if len(warningsRaw) > 0 {
			_ = json.Unmarshal(warningsRaw, &v.AIFraudWarnings)
		}
		v.ValidationResult = domain.ValidationStatus(statusStr)
		list = append(list, &v)
	}
	return list, rows.Err()
}

func (r *MySQLRepository) ListMPPayments(ctx context.Context, tenantID string, limit int) ([]*domain.MPPayment, error) {
	if limit <= 0 {
		limit = 50
	}
	if r.db == nil {
		r.mem.mu.Lock()
		defer r.mem.mu.Unlock()
		var list []*domain.MPPayment
		for _, p := range r.mem.payments {
			if p.TenantID == tenantID {
				copyP := *p
				list = append(list, &copyP)
			}
		}
		sort.Slice(list, func(i, j int) bool {
			return list[i].PaymentDate.After(list[j].PaymentDate)
		})
		if len(list) > limit {
			list = list[:limit]
		}
		return list, nil
	}

	query := `
		SELECT id, tenant_id, mp_payment_id, amount, currency, mp_status,
		       COALESCE(operation_type, ''), COALESCE(payer_cuit, ''), COALESCE(payer_name, ''),
		       COALESCE(coelsa_id, ''), payment_date, reconciliation_status,
		       consumed_at, COALESCE(consumed_by_receipt_id, ''), created_at
		FROM mp_payments
		WHERE tenant_id = ?
		ORDER BY payment_date DESC
		LIMIT ?
	`
	rows, err := r.db.QueryContext(ctx, query, tenantID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []*domain.MPPayment
	for rows.Next() {
		var p domain.MPPayment
		var consumedAt sql.NullTime
		if err := rows.Scan(
			&p.ID, &p.TenantID, &p.MPPaymentID, &p.Amount, &p.Currency, &p.MPStatus,
			&p.OperationType, &p.PayerCUIT, &p.PayerName, &p.CoelsaID,
			&p.PaymentDate, &p.ReconciliationStatus, &consumedAt, &p.ConsumedByReceiptID, &p.CreatedAt,
		); err != nil {
			return nil, err
		}
		if consumedAt.Valid {
			t := consumedAt.Time
			p.ConsumedAt = &t
		}
		list = append(list, &p)
	}
	return list, rows.Err()
}

// SeedDefaultDemoData inicializa un comercio de demostración, cuenta MP vinculada (con tokens encriptados AES-256-GCM),
// números de WhatsApp de empleados y pagos acreditados reales listos para probar conciliación en 1 clic.
func (r *MySQLRepository) SeedDefaultDemoData(ctx context.Context, encryptedAccessToken, encryptedRefreshToken, webhookSecret string) error {
	now := time.Now()
	demoTenant := &domain.Tenant{
		ID:                DefaultDemoTenantID,
		BusinessName:      "Pizzería & Empanadas Don Mateo SRL",
		CUIT:              "30716492819",
		Email:             "admin@donmateo.com.ar",
		PasswordHash:      "$2a$10$veripagodemo",
		PlanType:          domain.PlanPro,
		MonthlyQuota:      1500,
		UsedQuota:         14,
		TimeWindowMinutes: 120,
		IsActive:          true,
		CreatedAt:         now.Add(-72 * time.Hour),
		UpdatedAt:         now,
	}

	demoAccount := &domain.MPAccount{
		ID:                 DefaultDemoMPAccountID,
		TenantID:           DefaultDemoTenantID,
		MPUserID:           1492837465,
		ExpectedAlias:      "don.mateo.mp",
		ExpectedCVU:        "0000003100084920194821",
		ExpectedHolderName: "Pizzeria Don Mateo SRL",
		AccessTokenEnc:     encryptedAccessToken,
		RefreshTokenEnc:    encryptedRefreshToken,
		TokenExpiresAt:     now.Add(6 * time.Hour),
		WebhookSecret:      webhookSecret,
		Status:             "CONNECTED",
		CreatedAt:          now.Add(-72 * time.Hour),
		UpdatedAt:          now,
	}

	demoSenders := []*domain.AuthorizedSender{
		{
			ID:           "sender-owner-01",
			TenantID:     DefaultDemoTenantID,
			PhoneNumber:  "5491155001111",
			EmployeeName: "Mateo Rossi (Dueño)",
			Role:         domain.RoleOwner,
			IsActive:     true,
			CreatedAt:    now.Add(-48 * time.Hour),
		},
		{
			ID:           "sender-cashier-01",
			TenantID:     DefaultDemoTenantID,
			PhoneNumber:  "5491155002222",
			EmployeeName: "Lucía Gómez (Cajera Mostrador)",
			Role:         domain.RoleCashier,
			IsActive:     true,
			CreatedAt:    now.Add(-36 * time.Hour),
		},
		{
			ID:           "sender-delivery-01",
			TenantID:     DefaultDemoTenantID,
			PhoneNumber:  "5491155003333",
			EmployeeName: "Nahuel Pereyra (Repartidor Moto 1)",
			Role:         domain.RoleDelivery,
			IsActive:     true,
			CreatedAt:    now.Add(-24 * time.Hour),
		},
	}

	demoPayments := []*domain.MPPayment{
		{
			ID:                   "pay-uuid-88492019482",
			TenantID:             DefaultDemoTenantID,
			MPPaymentID:          88492019482,
			Amount:               15450.50,
			Currency:             "ARS",
			MPStatus:             "approved",
			OperationType:        "money_transfer",
			PayerCUIT:            "20348912349",
			PayerName:            "Juan Ignacio Perez",
			CoelsaID:             "2410200000149281746391",
			PaymentDate:          now.Add(-5 * time.Minute),
			ReconciliationStatus: "AVAILABLE",
			CreatedAt:            now.Add(-5 * time.Minute),
		},
		{
			ID:                   "pay-uuid-88492019999",
			TenantID:             DefaultDemoTenantID,
			MPPaymentID:          88492019999,
			Amount:               24800.00,
			Currency:             "ARS",
			MPStatus:             "approved",
			OperationType:        "bank_transfer",
			PayerCUIT:            "27319482714",
			PayerName:            "Maria Florencia Sosa",
			CoelsaID:             "2410200000998877665544",
			PaymentDate:          now.Add(-8 * time.Minute),
			ReconciliationStatus: "AVAILABLE",
			CreatedAt:            now.Add(-8 * time.Minute),
		},
	}

	if r.db == nil {
		r.mem.mu.Lock()
		r.mem.tenants[demoTenant.ID] = demoTenant
		r.mem.mpAccounts[demoTenant.ID] = demoAccount
		for _, s := range demoSenders {
			r.mem.senders[s.ID] = s
		}
		r.mem.mu.Unlock()
		for _, p := range demoPayments {
			_ = r.UpsertMPPayment(ctx, p)
		}
		return nil
	}

	// Si MySQL está activo, insertamos con INSERT IGNORE para no pisar datos previos
	_, _ = r.db.ExecContext(ctx, `
		INSERT IGNORE INTO tenants (id, business_name, cuit, email, password_hash, plan_type, monthly_quota, used_quota, time_window_minutes, is_active)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, TRUE)
	`, demoTenant.ID, demoTenant.BusinessName, demoTenant.CUIT, demoTenant.Email, demoTenant.PasswordHash, string(demoTenant.PlanType), demoTenant.MonthlyQuota, demoTenant.UsedQuota, demoTenant.TimeWindowMinutes)

	_ = r.UpsertMPAccount(ctx, demoAccount)
	for _, s := range demoSenders {
		_ = r.CreateAuthorizedSender(ctx, s)
	}
	for _, p := range demoPayments {
		_ = r.UpsertMPPayment(ctx, p)
	}
	return nil
}

// RevokeDemoMPAccount desactiva la cuenta MP de demostración sembrada en arranques anteriores,
// para que nunca tape a la cuenta real vinculada por OAuth.
func (r *MySQLRepository) RevokeDemoMPAccount(ctx context.Context) error {
	if r.db == nil {
		r.mem.mu.Lock()
		defer r.mem.mu.Unlock()
		for k, acc := range r.mem.mpAccounts {
			if acc.ID == DefaultDemoMPAccountID {
				delete(r.mem.mpAccounts, k)
			}
		}
		return nil
	}
	_, err := r.db.ExecContext(ctx, `UPDATE mp_accounts SET status = 'REVOKED' WHERE id = ? AND status <> 'REVOKED'`, DefaultDemoMPAccountID)
	return err
}

func normalizePhone(p string) string {
	p = strings.TrimSpace(p)
	p = strings.ReplaceAll(p, "+", "")
	p = strings.ReplaceAll(p, " ", "")
	p = strings.ReplaceAll(p, "-", "")
	return p
}
