package persistence

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"veripago-saas/internal/domain"
)

// Historial de comprobantes del panel: búsqueda paginada, conteos y borrado.

const validationColumns = `id, tenant_id, sender_phone, wa_message_id, file_sha256,
	COALESCE(extracted_amount, 0), COALESCE(extracted_op_number, ''),
	COALESCE(extracted_coelsa_id, ''), extracted_date,
	COALESCE(extracted_sender_name, ''), COALESCE(extracted_sender_cuit, ''),
	COALESCE(extracted_receiver_alias_or_cbu, ''), COALESCE(extracted_source_bank, ''),
	ai_fraud_warnings, validation_result, COALESCE(matched_mp_payment_id, ''),
	COALESCE(processing_time_ms, 0), created_at, COALESCE(customer_id, '')`

func scanValidation(rows *sql.Rows) (*domain.ReceiptValidation, error) {
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
		&v.ProcessingTimeMs, &v.CreatedAt, &v.CustomerID,
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
	return &v, nil
}

// validationWhere arma el WHERE del filtro (sin paginación).
func validationWhere(tenantID string, f domain.ValidationFilter) (string, []interface{}) {
	where := []string{"tenant_id = ?"}
	args := []interface{}{tenantID}
	if f.From != nil {
		where = append(where, "created_at >= ?")
		args = append(args, *f.From)
	}
	if f.To != nil {
		where = append(where, "created_at < ?")
		args = append(args, *f.To)
	}
	if f.CustomerID != "" {
		where = append(where, "customer_id = ?")
		args = append(args, f.CustomerID)
	}
	if len(f.Statuses) > 0 {
		where = append(where, "validation_result IN (?"+strings.Repeat(",?", len(f.Statuses)-1)+")")
		for _, s := range f.Statuses {
			args = append(args, string(s))
		}
	}
	return strings.Join(where, " AND "), args
}

func matchesFilter(v *domain.ReceiptValidation, tenantID string, f domain.ValidationFilter) bool {
	if v.TenantID != tenantID {
		return false
	}
	if f.From != nil && v.CreatedAt.Before(*f.From) {
		return false
	}
	if f.To != nil && !v.CreatedAt.Before(*f.To) {
		return false
	}
	if f.CustomerID != "" && v.CustomerID != f.CustomerID {
		return false
	}
	if len(f.Statuses) == 0 {
		return true
	}
	for _, s := range f.Statuses {
		if v.ValidationResult == s {
			return true
		}
	}
	return false
}

// SearchReceiptValidations devuelve una página del historial (más nuevos primero) y el total.
func (r *MySQLRepository) SearchReceiptValidations(ctx context.Context, tenantID string, f domain.ValidationFilter) ([]*domain.ReceiptValidation, int, error) {
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 20
	}
	if f.Offset < 0 {
		f.Offset = 0
	}

	if r.db == nil {
		r.mem.mu.Lock()
		defer r.mem.mu.Unlock()
		var page []*domain.ReceiptValidation
		total := 0
		for _, v := range r.mem.validations { // ya están de más nuevo a más viejo
			if !matchesFilter(v, tenantID, f) {
				continue
			}
			if total >= f.Offset && len(page) < f.Limit {
				copyV := *v
				page = append(page, &copyV)
			}
			total++
		}
		return page, total, nil
	}

	where, args := validationWhere(tenantID, f)
	var total int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM receipt_validations WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if total == 0 || f.Offset >= total {
		return []*domain.ReceiptValidation{}, total, nil
	}

	query := `SELECT ` + validationColumns + ` FROM receipt_validations WHERE ` + where +
		` ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`
	rows, err := r.db.QueryContext(ctx, query, append(args, f.Limit, f.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	page := make([]*domain.ReceiptValidation, 0, f.Limit)
	for rows.Next() {
		v, err := scanValidation(rows)
		if err != nil {
			return nil, 0, err
		}
		page = append(page, v)
	}
	return page, total, rows.Err()
}

// CountValidationsByStatus cuenta los comprobantes por resultado en el rango [from, to).
func (r *MySQLRepository) CountValidationsByStatus(ctx context.Context, tenantID string, from, to time.Time) (map[domain.ValidationStatus]int, error) {
	out := map[domain.ValidationStatus]int{}
	if r.db == nil {
		r.mem.mu.Lock()
		defer r.mem.mu.Unlock()
		f := domain.ValidationFilter{From: &from, To: &to}
		for _, v := range r.mem.validations {
			if matchesFilter(v, tenantID, f) {
				out[v.ValidationResult]++
			}
		}
		return out, nil
	}

	rows, err := r.db.QueryContext(ctx, `SELECT validation_result, COUNT(*) FROM receipt_validations
		WHERE tenant_id = ? AND created_at >= ? AND created_at < ? GROUP BY validation_result`, tenantID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			return nil, err
		}
		out[domain.ValidationStatus(s)] = n
	}
	return out, rows.Err()
}

// DeleteReceiptValidation borra el comprobante y, si había tomado un pago de Mercado Pago,
// lo deja disponible otra vez para que lo pueda usar el comprobante correcto.
func (r *MySQLRepository) DeleteReceiptValidation(ctx context.Context, tenantID, validationID string) (bool, error) {
	if validationID == "" {
		return false, domain.ErrValidationNotFound
	}
	if r.db == nil {
		r.mem.mu.Lock()
		defer r.mem.mu.Unlock()
		idx := -1
		for i, v := range r.mem.validations {
			if v.TenantID == tenantID && v.ID == validationID {
				idx = i
				break
			}
		}
		if idx < 0 {
			return false, domain.ErrValidationNotFound
		}
		r.mem.validations = append(r.mem.validations[:idx:idx], r.mem.validations[idx+1:]...)
		released := false
		for _, p := range r.mem.payments {
			if p.TenantID == tenantID && p.ConsumedByReceiptID == validationID {
				p.ReconciliationStatus = "AVAILABLE"
				p.ConsumedAt = nil
				p.ConsumedByReceiptID = ""
				released = true
			}
		}
		return released, nil
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `UPDATE mp_payments
		SET reconciliation_status = 'AVAILABLE', consumed_at = NULL, consumed_by_receipt_id = NULL
		WHERE tenant_id = ? AND consumed_by_receipt_id = ?`, tenantID, validationID)
	if err != nil {
		return false, err
	}
	releasedRows, _ := res.RowsAffected()

	res, err = tx.ExecContext(ctx, `DELETE FROM receipt_validations WHERE tenant_id = ? AND id = ?`, tenantID, validationID)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, domain.ErrValidationNotFound
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return releasedRows > 0, nil
}
