package persistence

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"veripago-saas/internal/domain"
)

// Clientes: alta/actualización a partir de comprobantes y pagos, totales y búsqueda.
// Los totales no se guardan: se calculan con consultas indexadas por cliente, así nunca quedan
// desactualizados.

func nullIfEmpty(s string) interface{} {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

// SQL con la lista de resultados que cuentan como fraude, verificado y rechazado.
var (
	fraudStatusesSQL    = statusListSQL(domain.FraudStatuses)
	rejectedStatusesSQL = statusListSQL([]domain.ValidationStatus{domain.StatusRejectedWrongDest, domain.StatusInvalidReceiptImage})
)

func statusListSQL(list []domain.ValidationStatus) string {
	parts := make([]string, len(list))
	for i, s := range list {
		parts[i] = "'" + string(s) + "'"
	}
	return strings.Join(parts, ",")
}

// UpsertCustomer crea o actualiza al cliente identificado por CUIT (o por nombre si no hay CUIT).
// Si antes se lo conocía solo por el nombre y ahora aparece su CUIT, se completa ese registro.
// Devuelve nil si no hay datos suficientes para identificarlo.
func (r *MySQLRepository) UpsertCustomer(ctx context.Context, tenantID, cuit, name, email string, seenAt time.Time) (*domain.Customer, error) {
	key := domain.CustomerIdentityKey(cuit, name)
	if key == "" || tenantID == "" {
		return nil, nil
	}
	if seenAt.IsZero() {
		seenAt = time.Now()
	}
	cuitN := domain.NormalizeCUIT(cuit)
	name = strings.TrimSpace(name)
	nameKey := domain.NormalizePersonName(name)
	email = strings.ToLower(strings.TrimSpace(email))

	if r.db == nil {
		r.mem.mu.Lock()
		defer r.mem.mu.Unlock()
		mk := tenantID + "|" + key
		c, ok := r.mem.customers[mk]
		if !ok && cuitN != "" && nameKey != "" {
			// Se lo conocía solo por el nombre: se le asigna el CUIT
			if old, found := r.mem.customers[tenantID+"|NAME:"+nameKey]; found && old.CUIT == "" {
				delete(r.mem.customers, tenantID+"|NAME:"+nameKey)
				old.CUIT, old.IdentityKey = cuitN, key
				r.mem.customers[mk] = old
				c, ok = old, true
			}
		}
		if !ok {
			c = &domain.Customer{ID: uuid.NewString(), TenantID: tenantID, IdentityKey: key, CUIT: cuitN, FirstSeenAt: seenAt, LastSeenAt: seenAt}
			r.mem.customers[mk] = c
		}
		if c.Name == "" && name != "" {
			c.Name, c.NameKey = name, nameKey
		}
		if c.Email == "" && email != "" {
			c.Email = email
		}
		if seenAt.Before(c.FirstSeenAt) {
			c.FirstSeenAt = seenAt
		}
		if seenAt.After(c.LastSeenAt) {
			c.LastSeenAt = seenAt
		}
		copyC := *c
		return &copyC, nil
	}

	if cuitN != "" && nameKey != "" {
		var exists int
		err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM customers WHERE tenant_id = ? AND identity_key = ?`, tenantID, key).Scan(&exists)
		if err != nil {
			return nil, err
		}
		if exists == 0 {
			// Si otro proceso ya lo creó con el CUIT, el índice único hace fallar esto: se ignora
			_, _ = r.db.ExecContext(ctx, `UPDATE customers SET cuit = ?, identity_key = ?
				WHERE tenant_id = ? AND identity_key = ? AND cuit IS NULL`, cuitN, key, tenantID, "NAME:"+nameKey)
		}
	}

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO customers (id, tenant_id, identity_key, cuit, name, name_key, email, first_seen_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) AS new
		ON DUPLICATE KEY UPDATE
			name = IF(customers.name = '', new.name, customers.name),
			name_key = IF(customers.name_key = '', new.name_key, customers.name_key),
			email = COALESCE(customers.email, new.email),
			first_seen_at = LEAST(customers.first_seen_at, new.first_seen_at),
			last_seen_at = GREATEST(customers.last_seen_at, new.last_seen_at)`,
		uuid.NewString(), tenantID, key, nullIfEmpty(cuitN), name, nameKey, nullIfEmpty(email), seenAt, seenAt)
	if err != nil {
		return nil, err
	}

	var c domain.Customer
	var cuitCol, emailCol sql.NullString
	err = r.db.QueryRowContext(ctx, `SELECT id, tenant_id, identity_key, cuit, name, name_key, email, first_seen_at, last_seen_at
		FROM customers WHERE tenant_id = ? AND identity_key = ?`, tenantID, key).
		Scan(&c.ID, &c.TenantID, &c.IdentityKey, &cuitCol, &c.Name, &c.NameKey, &emailCol, &c.FirstSeenAt, &c.LastSeenAt)
	if err != nil {
		return nil, err
	}
	c.CUIT, c.Email = cuitCol.String, emailCol.String
	return &c, nil
}

// LinkPaymentCustomer asocia un pago a un cliente si todavía no tenía uno.
func (r *MySQLRepository) LinkPaymentCustomer(ctx context.Context, tenantID, paymentID, customerID string) error {
	if paymentID == "" || customerID == "" {
		return nil
	}
	if r.db == nil {
		r.mem.mu.Lock()
		defer r.mem.mu.Unlock()
		for _, p := range r.mem.payments {
			if p.TenantID == tenantID && p.ID == paymentID && p.CustomerID == "" {
				p.CustomerID = customerID
			}
		}
		return nil
	}
	_, err := r.db.ExecContext(ctx, `UPDATE mp_payments SET customer_id = ?
		WHERE tenant_id = ? AND id = ? AND customer_id IS NULL`, customerID, tenantID, paymentID)
	return err
}

// CustomerRiskStats cuenta en el historial del cliente los intentos de fraude (y el último) y los
// rechazos o fraudes desde "since".
func (r *MySQLRepository) CustomerRiskStats(ctx context.Context, tenantID, customerID string, since time.Time) (int, *time.Time, int, error) {
	if customerID == "" {
		return 0, nil, 0, nil
	}
	if r.db == nil {
		r.mem.mu.Lock()
		defer r.mem.mu.Unlock()
		fraud, recent := 0, 0
		var last *time.Time
		for _, v := range r.mem.validations {
			if v.TenantID != tenantID || v.CustomerID != customerID {
				continue
			}
			isFraud := domain.IsFraudStatus(v.ValidationResult)
			if isFraud {
				fraud++
				if last == nil || v.CreatedAt.After(*last) {
					t := v.CreatedAt
					last = &t
				}
			}
			if !v.CreatedAt.Before(since) && (isFraud || v.ValidationResult == domain.StatusRejectedWrongDest || v.ValidationResult == domain.StatusInvalidReceiptImage) {
				recent++
			}
		}
		return fraud, last, recent, nil
	}

	var fraud, recent sql.NullInt64
	var last sql.NullTime
	err := r.db.QueryRowContext(ctx, `SELECT
			SUM(validation_result IN (`+fraudStatusesSQL+`)),
			MAX(CASE WHEN validation_result IN (`+fraudStatusesSQL+`) THEN created_at END),
			SUM(created_at >= ? AND validation_result IN (`+fraudStatusesSQL+`,`+rejectedStatusesSQL+`))
		FROM receipt_validations WHERE tenant_id = ? AND customer_id = ?`, since, tenantID, customerID).
		Scan(&fraud, &last, &recent)
	if err != nil {
		return 0, nil, 0, err
	}
	var lastPtr *time.Time
	if last.Valid {
		t := last.Time
		lastPtr = &t
	}
	return int(fraud.Int64), lastPtr, int(recent.Int64), nil
}

// SearchCustomers devuelve una página de clientes (los vistos más recientemente primero) con sus totales.
func (r *MySQLRepository) SearchCustomers(ctx context.Context, tenantID string, f domain.CustomerFilter) ([]domain.CustomerSummary, int, error) {
	if f.Limit <= 0 || f.Limit > 100 {
		f.Limit = 20
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	q := strings.TrimSpace(f.Query)
	digits := onlyDigits(q)
	nameQ := domain.NormalizePersonName(q)

	if r.db == nil {
		r.mem.mu.Lock()
		var list []*domain.Customer
		for _, c := range r.mem.customers {
			if c.TenantID != tenantID {
				continue
			}
			if q != "" && !(digits != "" && strings.HasPrefix(c.CUIT, digits)) && !(nameQ != "" && strings.Contains(c.NameKey, nameQ)) {
				continue
			}
			copyC := *c
			list = append(list, &copyC)
		}
		r.mem.mu.Unlock()
		sort.Slice(list, func(i, j int) bool { return list[i].LastSeenAt.After(list[j].LastSeenAt) })
		var all []domain.CustomerSummary
		for _, c := range list {
			st := r.memCustomerStats(tenantID, c.ID)
			if f.OnlyFraud && st.Fraud == 0 {
				continue
			}
			all = append(all, domain.CustomerSummary{Customer: *c, CustomerStats: st})
		}
		total := len(all)
		if f.Offset >= total {
			return []domain.CustomerSummary{}, total, nil
		}
		end := min(f.Offset+f.Limit, total)
		return all[f.Offset:end], total, nil
	}

	// 1) Página de IDs (con índices por comercio + última vez visto / CUIT / nombre)
	from := ` FROM customers c`
	where := []string{"c.tenant_id = ?"}
	args := []interface{}{tenantID}
	if f.OnlyFraud {
		from += ` JOIN (SELECT DISTINCT customer_id FROM receipt_validations
			WHERE tenant_id = ? AND customer_id IS NOT NULL AND validation_result IN (` + fraudStatusesSQL + `)) f
			ON f.customer_id = c.id`
		args = append([]interface{}{tenantID}, args...)
	}
	if q != "" {
		cond := []string{}
		if digits != "" {
			cond = append(cond, "c.cuit LIKE ?")
			args = append(args, digits+"%")
		}
		if nameQ != "" {
			cond = append(cond, "c.name_key LIKE ?")
			args = append(args, "%"+escapeLike(nameQ)+"%")
		}
		where = append(where, "("+strings.Join(cond, " OR ")+")")
	}
	whereSQL := " WHERE " + strings.Join(where, " AND ")

	var total int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*)`+from+whereSQL, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if total == 0 || f.Offset >= total {
		return []domain.CustomerSummary{}, total, nil
	}
	rows, err := r.db.QueryContext(ctx, `SELECT c.id`+from+whereSQL+` ORDER BY c.last_seen_at DESC, c.id LIMIT ? OFFSET ?`,
		append(args, f.Limit, f.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()

	// 2) Datos y totales solo de esa página
	byID, err := r.CustomerSummariesByIDs(ctx, tenantID, ids)
	if err != nil {
		return nil, 0, err
	}
	out := make([]domain.CustomerSummary, 0, len(ids))
	for _, id := range ids {
		if s, ok := byID[id]; ok {
			out = append(out, s)
		}
	}
	return out, total, nil
}

// GetCustomerSummary devuelve un cliente con sus totales.
func (r *MySQLRepository) GetCustomerSummary(ctx context.Context, tenantID, customerID string) (*domain.CustomerSummary, error) {
	m, err := r.CustomerSummariesByIDs(ctx, tenantID, []string{customerID})
	if err != nil {
		return nil, err
	}
	s, ok := m[customerID]
	if !ok {
		return nil, ErrCustomerNotFound
	}
	return &s, nil
}

// ErrCustomerNotFound se devuelve si el cliente no existe en ese comercio.
var ErrCustomerNotFound = errors.New("el cliente no existe")

// CustomerSummariesByIDs devuelve los clientes pedidos con sus totales (2 consultas indexadas).
func (r *MySQLRepository) CustomerSummariesByIDs(ctx context.Context, tenantID string, ids []string) (map[string]domain.CustomerSummary, error) {
	out := make(map[string]domain.CustomerSummary, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	if r.db == nil {
		want := map[string]bool{}
		for _, id := range ids {
			want[id] = true
		}
		r.mem.mu.Lock()
		var found []domain.Customer
		for _, c := range r.mem.customers {
			if c.TenantID == tenantID && want[c.ID] {
				found = append(found, *c)
			}
		}
		r.mem.mu.Unlock()
		for _, c := range found {
			out[c.ID] = domain.CustomerSummary{Customer: c, CustomerStats: r.memCustomerStats(tenantID, c.ID)}
		}
		return out, nil
	}

	in := "?" + strings.Repeat(",?", len(ids)-1)
	args := []interface{}{tenantID}
	for _, id := range ids {
		args = append(args, id)
	}

	rows, err := r.db.QueryContext(ctx, `SELECT id, tenant_id, COALESCE(cuit, ''), name, name_key, COALESCE(email, ''), first_seen_at, last_seen_at
		FROM customers WHERE tenant_id = ? AND id IN (`+in+`)`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c domain.Customer
		if err := rows.Scan(&c.ID, &c.TenantID, &c.CUIT, &c.Name, &c.NameKey, &c.Email, &c.FirstSeenAt, &c.LastSeenAt); err != nil {
			rows.Close()
			return nil, err
		}
		out[c.ID] = domain.CustomerSummary{Customer: c}
	}
	rows.Close()
	if len(out) == 0 {
		return out, nil
	}

	rows, err = r.db.QueryContext(ctx, `SELECT customer_id, COUNT(*),
			SUM(validation_result IN (`+verifiedStatusesSQL+`)),
			SUM(validation_result IN (`+fraudStatusesSQL+`)),
			SUM(validation_result IN (`+rejectedStatusesSQL+`)),
			SUM(validation_result = 'REJECTED_NOT_FOUND'),
			COALESCE(SUM(CASE WHEN validation_result IN (`+verifiedStatusesSQL+`) THEN extracted_amount END), 0),
			MAX(CASE WHEN validation_result IN (`+fraudStatusesSQL+`) THEN created_at END)
		FROM receipt_validations WHERE tenant_id = ? AND customer_id IN (`+in+`)
		GROUP BY customer_id`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		var st domain.CustomerStats
		var last sql.NullTime
		if err := rows.Scan(&id, &st.Receipts, &st.Verified, &st.Fraud, &st.Rejected, &st.NotFound, &st.VerifiedAmount, &last); err != nil {
			rows.Close()
			return nil, err
		}
		if last.Valid {
			t := last.Time
			st.LastFraudAt = &t
		}
		s := out[id]
		payments := s.Payments
		s.CustomerStats = st
		s.Payments = payments
		out[id] = s
	}
	rows.Close()

	rows, err = r.db.QueryContext(ctx, `SELECT customer_id, COUNT(*) FROM mp_payments
		WHERE tenant_id = ? AND customer_id IN (`+in+`) GROUP BY customer_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		s := out[id]
		s.Payments = n
		out[id] = s
	}
	return out, rows.Err()
}

// memCustomerStats calcula los totales en el modo en memoria.
func (r *MySQLRepository) memCustomerStats(tenantID, customerID string) domain.CustomerStats {
	r.mem.mu.Lock()
	defer r.mem.mu.Unlock()
	var st domain.CustomerStats
	for _, v := range r.mem.validations {
		if v.TenantID != tenantID || v.CustomerID != customerID {
			continue
		}
		st.Receipts++
		switch {
		case isVerifiedStatus(v.ValidationResult):
			st.Verified++
			st.VerifiedAmount += v.ExtractedAmount
		case domain.IsFraudStatus(v.ValidationResult):
			st.Fraud++
			if st.LastFraudAt == nil || v.CreatedAt.After(*st.LastFraudAt) {
				t := v.CreatedAt
				st.LastFraudAt = &t
			}
		case v.ValidationResult == domain.StatusRejectedNotFound:
			st.NotFound++
		default:
			st.Rejected++
		}
	}
	for _, p := range r.mem.payments {
		if p.TenantID == tenantID && p.CustomerID == customerID {
			st.Payments++
		}
	}
	return st
}

func onlyDigits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// ---------------------------------------------------------------------------
// Comercio
// ---------------------------------------------------------------------------

// EnsureTenant devuelve el comercio de esta instalación. Si la base está vacía lo crea sin datos
// inventados: el nombre, el CUIT y el email se completan desde el panel.
func (r *MySQLRepository) EnsureTenant(ctx context.Context) (*domain.Tenant, error) {
	spec := domain.GetPlanSpecs()[domain.PlanFreeTrial]
	if r.db == nil {
		r.mem.mu.Lock()
		var oldest *domain.Tenant
		for _, t := range r.mem.tenants {
			if oldest == nil || t.CreatedAt.Before(oldest.CreatedAt) {
				oldest = t
			}
		}
		if oldest == nil {
			now := time.Now()
			oldest = &domain.Tenant{
				ID: uuid.NewString(), PlanType: spec.Plan, MonthlyQuota: spec.MonthlyQuota,
				TimeWindowMinutes: domain.DefaultTimeWindowMinutes, IsActive: true, CreatedAt: now, UpdatedAt: now,
			}
			r.mem.tenants[oldest.ID] = oldest
		}
		id := oldest.ID
		r.mem.mu.Unlock()
		return r.GetTenantByID(ctx, id)
	}

	var id string
	err := r.db.QueryRowContext(ctx, `SELECT id FROM tenants ORDER BY created_at, id LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		id = uuid.NewString()
		_, err = r.db.ExecContext(ctx, `INSERT INTO tenants (id, business_name, plan_type, monthly_quota, used_quota, time_window_minutes, is_active)
			VALUES (?, '', ?, ?, 0, ?, TRUE)`, id, string(spec.Plan), spec.MonthlyQuota, domain.DefaultTimeWindowMinutes)
	}
	if err != nil {
		return nil, err
	}
	return r.GetTenantByID(ctx, id)
}

// UpdateTenantProfile guarda los datos del comercio cargados en el panel.
func (r *MySQLRepository) UpdateTenantProfile(ctx context.Context, tenantID, businessName, cuit, email string) error {
	businessName = strings.TrimSpace(businessName)
	cuit = onlyDigits(cuit)
	email = strings.ToLower(strings.TrimSpace(email))
	if r.db == nil {
		r.mem.mu.Lock()
		defer r.mem.mu.Unlock()
		if t, ok := r.mem.tenants[tenantID]; ok {
			t.BusinessName, t.CUIT, t.Email, t.UpdatedAt = businessName, cuit, email, time.Now()
		}
		return nil
	}
	_, err := r.db.ExecContext(ctx, `UPDATE tenants SET business_name = ?, cuit = ?, email = ? WHERE id = ?`,
		businessName, nullIfEmpty(cuit), nullIfEmpty(email), tenantID)
	return err
}
