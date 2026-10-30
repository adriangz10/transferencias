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

// CreateTenant registra un nuevo comercio en el SaaS.
func (r *MySQLRepository) CreateTenant(ctx context.Context, t *domain.Tenant) error {
	now := time.Now()
	if t.ID == "" {
		t.ID = uuid.NewString()
	}
	t.BusinessName = strings.TrimSpace(t.BusinessName)
	t.CUIT = onlyDigits(t.CUIT)
	t.Email = strings.ToLower(strings.TrimSpace(t.Email))
	if t.PlanType == "" {
		t.PlanType = domain.PlanFreeTrial
	}
	if t.MonthlyQuota <= 0 {
		if spec, ok := domain.GetPlanSpecs()[t.PlanType]; ok {
			t.MonthlyQuota = spec.MonthlyQuota
		} else {
			t.MonthlyQuota = 50
		}
	}
	if t.TimeWindowMinutes <= 0 {
		t.TimeWindowMinutes = domain.DefaultTimeWindowMinutes
	}
	if t.SubscriptionStatus == "" {
		if t.PlanType == domain.PlanFreeTrial {
			t.SubscriptionStatus = "TRIAL"
		} else {
			t.SubscriptionStatus = "ACTIVE"
		}
	}
	if t.PlanExpiresAt == nil {
		var exp time.Time
		if t.SubscriptionStatus == "TRIAL" {
			exp = now.AddDate(0, 0, 14) // 14 días de prueba
		} else {
			exp = now.AddDate(0, 1, 0)
		}
		t.PlanExpiresAt = &exp
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	t.UpdatedAt = now

	if r.db == nil {
		r.mem.mu.Lock()
		defer r.mem.mu.Unlock()
		for _, other := range r.mem.tenants {
			if t.Email != "" && strings.EqualFold(other.Email, t.Email) {
				return domain.ErrTenantEmailTaken
			}
			if t.CUIT != "" && other.CUIT == t.CUIT {
				return domain.ErrTenantCUITTaken
			}
		}
		copyT := *t
		r.mem.tenants[t.ID] = &copyT
		return nil
	}

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO tenants (
			id, business_name, cuit, email, password_hash, plan_type,
			monthly_quota, used_quota, time_window_minutes,
			subscription_status, plan_expires_at, is_active
		) VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?)`,
		t.ID, t.BusinessName, nullIfEmpty(t.CUIT), nullIfEmpty(t.Email), nullIfEmpty(t.PasswordHash),
		string(t.PlanType), t.MonthlyQuota, t.TimeWindowMinutes,
		t.SubscriptionStatus, t.PlanExpiresAt, t.IsActive,
	)
	if err != nil && strings.Contains(err.Error(), "Duplicate") {
		if strings.Contains(strings.ToLower(err.Error()), "cuit") {
			return domain.ErrTenantCUITTaken
		}
		return domain.ErrTenantEmailTaken
	}
	return err
}

// GetTenantByEmail busca un comercio por su email de acceso.
func (r *MySQLRepository) GetTenantByEmail(ctx context.Context, email string) (*domain.Tenant, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return nil, sql.ErrNoRows
	}
	if r.db == nil {
		r.mem.mu.Lock()
		var foundID string
		for _, t := range r.mem.tenants {
			if strings.EqualFold(t.Email, email) {
				foundID = t.ID
				break
			}
		}
		r.mem.mu.Unlock()
		if foundID == "" {
			return nil, sql.ErrNoRows
		}
		return r.GetTenantByID(ctx, foundID)
	}

	var id string
	err := r.db.QueryRowContext(ctx, `SELECT id FROM tenants WHERE LOWER(email) = ? LIMIT 1`, email).Scan(&id)
	if err != nil {
		return nil, err
	}
	return r.GetTenantByID(ctx, id)
}

// UpdateTenantPassword actualiza el hash de contraseña del comercio.
func (r *MySQLRepository) UpdateTenantPassword(ctx context.Context, tenantID, passwordHash string) error {
	if r.db == nil {
		r.mem.mu.Lock()
		defer r.mem.mu.Unlock()
		if t, ok := r.mem.tenants[tenantID]; ok {
			t.PasswordHash = passwordHash
			t.UpdatedAt = time.Now()
		}
		return nil
	}
	_, err := r.db.ExecContext(ctx, `UPDATE tenants SET password_hash = ? WHERE id = ?`, nullIfEmpty(passwordHash), tenantID)
	return err
}

// UpdateTenantAdmin permite al Super-Admin actualizar plan, cuota, estado activo, suscripción y reinicio de cuota.
func (r *MySQLRepository) UpdateTenantAdmin(
	ctx context.Context,
	tenantID string,
	plan domain.PlanType,
	quota int,
	isActive bool,
	subStatus string,
	expiresAt *time.Time,
	resetQuota bool,
) error {
	if subStatus == "" {
		subStatus = "ACTIVE"
	}
	if r.db == nil {
		r.mem.mu.Lock()
		defer r.mem.mu.Unlock()
		t, ok := r.mem.tenants[tenantID]
		if !ok {
			return sql.ErrNoRows
		}
		t.PlanType = plan
		if quota > 0 {
			t.MonthlyQuota = quota
		}
		t.IsActive = isActive
		t.SubscriptionStatus = subStatus
		if expiresAt != nil {
			exp := *expiresAt
			t.PlanExpiresAt = &exp
		}
		if resetQuota {
			t.UsedQuota = 0
		}
		t.UpdatedAt = time.Now()
		return nil
	}

	if resetQuota {
		_, err := r.db.ExecContext(ctx, `
			UPDATE tenants
			SET plan_type = ?, monthly_quota = ?, is_active = ?, subscription_status = ?,
			    plan_expires_at = COALESCE(?, plan_expires_at), used_quota = 0
			WHERE id = ?`,
			string(plan), quota, isActive, subStatus, expiresAt, tenantID)
		return err
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE tenants
		SET plan_type = ?, monthly_quota = ?, is_active = ?, subscription_status = ?,
		    plan_expires_at = COALESCE(?, plan_expires_at)
		WHERE id = ?`,
		string(plan), quota, isActive, subStatus, expiresAt, tenantID)
	return err
}

// ListTenantsAdmin devuelve todos los comercios (filtrados por query) y las métricas globales de la plataforma.
func (r *MySQLRepository) ListTenantsAdmin(ctx context.Context, query string) ([]domain.TenantAdminSummary, domain.PlatformStats, error) {
	q := strings.ToLower(strings.TrimSpace(query))
	period := currentQuotaPeriod()

	if r.db == nil {
		r.mem.mu.Lock()
		defer r.mem.mu.Unlock()

		var stats domain.PlatformStats
		var list []domain.TenantAdminSummary

		for _, t := range r.mem.tenants {
			copyT := *t
			if copyT.SubscriptionStatus == "" {
				copyT.SubscriptionStatus = "TRIAL"
			}
			if p, ok := r.mem.quotaPeriods[t.ID]; ok && p != period {
				copyT.UsedQuota = 0
			}

			stats.TotalTenants++
			if copyT.IsActive {
				stats.ActiveTenants++
			}
			if copyT.PlanType != domain.PlanFreeTrial && copyT.SubscriptionStatus == "ACTIVE" {
				stats.PaidSubscriptions++
			}
			stats.MonthlyValidations += copyT.UsedQuota

			if q != "" {
				match := strings.Contains(strings.ToLower(copyT.BusinessName), q) ||
					strings.Contains(strings.ToLower(copyT.Email), q) ||
					strings.Contains(copyT.CUIT, q)
				if !match {
					continue
				}
			}

			sum := domain.TenantAdminSummary{
				Tenant:      copyT,
				HasPassword: copyT.PasswordHash != "",
			}
			for _, acc := range r.mem.mpAccounts {
				if acc.TenantID == t.ID && acc.Status == "CONNECTED" {
					sum.MPConnected = true
					sum.MPUserID = acc.MPUserID
					sum.ExpectedAlias = acc.ExpectedAlias
					break
				}
			}
			for _, s := range r.mem.senders {
				if s.TenantID == t.ID && s.IsActive {
					sum.SendersCount++
				}
			}
			for _, v := range r.mem.validations {
				if v.TenantID == t.ID {
					sum.TotalValidated++
				}
			}
			list = append(list, sum)
		}

		for _, o := range r.mem.subOrders {
			if o.Status == "APPROVED" {
				stats.ApprovedRevenueARS += o.AmountARS
			} else if o.Status == "PENDING" {
				stats.PendingOrdersCount++
			}
		}

		sort.Slice(list, func(i, j int) bool {
			return list[i].CreatedAt.After(list[j].CreatedAt)
		})
		return list, stats, nil
	}

	var stats domain.PlatformStats
	_ = r.db.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN is_active = TRUE THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN plan_type <> 'FREE_TRIAL' AND subscription_status = 'ACTIVE' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN quota_period <=> ? THEN used_quota ELSE 0 END), 0)
		FROM tenants`, period).Scan(&stats.TotalTenants, &stats.ActiveTenants, &stats.PaidSubscriptions, &stats.MonthlyValidations)

	_ = r.db.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN status = 'APPROVED' THEN amount_ars ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = 'PENDING' THEN 1 ELSE 0 END), 0)
		FROM subscription_orders`).Scan(&stats.ApprovedRevenueARS, &stats.PendingOrdersCount)

	var where string
	var args []interface{}
	if q != "" {
		like := "%" + escapeLike(q) + "%"
		where = `WHERE LOWER(t.business_name) LIKE ? OR LOWER(COALESCE(t.email, '')) LIKE ? OR COALESCE(t.cuit, '') LIKE ?`
		args = append(args, like, like, like)
	}

	sqlQuery := `
		SELECT
			t.id, t.business_name, COALESCE(t.cuit, ''), COALESCE(t.email, ''), COALESCE(t.password_hash, ''),
			t.plan_type, t.monthly_quota, t.used_quota, t.quota_period, t.time_window_minutes,
			COALESCE(t.subscription_status, 'TRIAL'), t.plan_expires_at, t.is_active, t.created_at, t.updated_at,
			(SELECT COUNT(*) FROM authorized_senders s WHERE s.tenant_id = t.id AND s.is_active = TRUE) AS senders_count,
			(SELECT COUNT(*) FROM receipt_validations v WHERE v.tenant_id = t.id) AS total_validated
		FROM tenants t
		` + where + `
		ORDER BY t.created_at DESC
		LIMIT 200
	`
	rows, err := r.db.QueryContext(ctx, sqlQuery, args...)
	if err != nil {
		return nil, stats, err
	}
	defer rows.Close()

	var list []domain.TenantAdminSummary
	for rows.Next() {
		var sum domain.TenantAdminSummary
		var planStr string
		var periodCol sql.NullString
		var expiresAt sql.NullTime
		if err := rows.Scan(
			&sum.ID, &sum.BusinessName, &sum.CUIT, &sum.Email, &sum.PasswordHash,
			&planStr, &sum.MonthlyQuota, &sum.UsedQuota, &periodCol, &sum.TimeWindowMinutes,
			&sum.SubscriptionStatus, &expiresAt, &sum.IsActive, &sum.CreatedAt, &sum.UpdatedAt,
			&sum.SendersCount, &sum.TotalValidated,
		); err != nil {
			return nil, stats, err
		}
		if periodCol.Valid && periodCol.String != period {
			sum.UsedQuota = 0
		}
		if expiresAt.Valid {
			exp := expiresAt.Time
			sum.PlanExpiresAt = &exp
		}
		sum.PlanType = domain.PlanType(planStr)
		sum.HasPassword = sum.PasswordHash != ""
		if acc, err := r.GetActiveMPAccount(ctx, sum.ID); err == nil && acc != nil {
			sum.MPConnected = true
			sum.MPUserID = acc.MPUserID
			sum.ExpectedAlias = acc.ExpectedAlias
		}
		list = append(list, sum)
	}
	return list, stats, rows.Err()
}

// CreateSubscriptionOrder registra una orden de suscripción/cambio de plan.
func (r *MySQLRepository) CreateSubscriptionOrder(ctx context.Context, o *domain.SubscriptionOrder) error {
	now := time.Now()
	if o.ID == "" {
		o.ID = uuid.NewString()
	}
	if o.Status == "" {
		o.Status = "PENDING"
	}
	if o.PaymentMode == "" {
		o.PaymentMode = "MANUAL"
	}
	if o.CreatedAt.IsZero() {
		o.CreatedAt = now
	}
	o.UpdatedAt = now

	if r.db == nil {
		r.mem.mu.Lock()
		defer r.mem.mu.Unlock()
		copyO := *o
		r.mem.subOrders[o.ID] = &copyO
		return nil
	}

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO subscription_orders (
			id, tenant_id, plan_type, price_usd, amount_ars, status,
			payment_mode, mp_preference_id, mp_payment_id, checkout_url, paid_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		o.ID, o.TenantID, string(o.PlanType), o.PriceUSD, o.AmountARS, o.Status,
		o.PaymentMode, nullIfEmpty(o.MPPreferenceID), nullInt64(o.MPPaymentID), nullIfEmpty(o.CheckoutURL), o.PaidAt,
	)
	return err
}

func nullInt64(n int64) interface{} {
	if n == 0 {
		return nil
	}
	return n
}

// GetSubscriptionOrder obtiene una orden de suscripción por ID.
func (r *MySQLRepository) GetSubscriptionOrder(ctx context.Context, orderID string) (*domain.SubscriptionOrder, error) {
	if r.db == nil {
		r.mem.mu.Lock()
		defer r.mem.mu.Unlock()
		o, ok := r.mem.subOrders[orderID]
		if !ok {
			return nil, domain.ErrSubscriptionNotFound
		}
		copyO := *o
		if t, ok := r.mem.tenants[copyO.TenantID]; ok {
			copyO.BusinessName = t.BusinessName
			copyO.TenantEmail = t.Email
		}
		return &copyO, nil
	}

	var o domain.SubscriptionOrder
	var planStr string
	var prefID, urlCol sql.NullString
	var mpPayID sql.NullInt64
	var paidAt sql.NullTime
	err := r.db.QueryRowContext(ctx, `
		SELECT o.id, o.tenant_id, COALESCE(t.business_name, ''), COALESCE(t.email, ''),
		       o.plan_type, o.price_usd, o.amount_ars, o.status, o.payment_mode,
		       o.mp_preference_id, o.mp_payment_id, o.checkout_url, o.paid_at, o.created_at, o.updated_at
		FROM subscription_orders o
		LEFT JOIN tenants t ON t.id = o.tenant_id
		WHERE o.id = ?`, orderID).Scan(
		&o.ID, &o.TenantID, &o.BusinessName, &o.TenantEmail,
		&planStr, &o.PriceUSD, &o.AmountARS, &o.Status, &o.PaymentMode,
		&prefID, &mpPayID, &urlCol, &paidAt, &o.CreatedAt, &o.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrSubscriptionNotFound
	}
	if err != nil {
		return nil, err
	}
	o.PlanType = domain.PlanType(planStr)
	o.MPPreferenceID = prefID.String
	o.MPPaymentID = mpPayID.Int64
	o.CheckoutURL = urlCol.String
	if paidAt.Valid {
		pt := paidAt.Time
		o.PaidAt = &pt
	}
	return &o, nil
}

// ApproveSubscriptionOrder aprueba una orden de suscripción y activa el nuevo plan del comercio por 30 días.
func (r *MySQLRepository) ApproveSubscriptionOrder(ctx context.Context, orderID string, mpPaymentID int64) (*domain.SubscriptionOrder, error) {
	now := time.Now()
	exp := now.AddDate(0, 1, 0)

	if r.db == nil {
		r.mem.mu.Lock()
		o, ok := r.mem.subOrders[orderID]
		if !ok {
			r.mem.mu.Unlock()
			return nil, domain.ErrSubscriptionNotFound
		}
		if o.Status != "APPROVED" {
			o.Status = "APPROVED"
			o.PaidAt = &now
			o.UpdatedAt = now
			if mpPaymentID > 0 {
				o.MPPaymentID = mpPaymentID
			}
			if t, ok := r.mem.tenants[o.TenantID]; ok {
				spec := domain.GetPlanSpecs()[o.PlanType]
				t.PlanType = o.PlanType
				if spec.MonthlyQuota > 0 {
					t.MonthlyQuota = spec.MonthlyQuota
				}
				t.SubscriptionStatus = "ACTIVE"
				t.PlanExpiresAt = &exp
				t.UpdatedAt = now
			}
		}
		r.mem.mu.Unlock()
		return r.GetSubscriptionOrder(ctx, orderID)
	}

	o, err := r.GetSubscriptionOrder(ctx, orderID)
	if err != nil {
		return nil, err
	}
	if o.Status == "APPROVED" {
		return o, nil
	}

	spec := domain.GetPlanSpecs()[o.PlanType]
	quota := spec.MonthlyQuota
	if quota <= 0 {
		quota = 300
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
		UPDATE subscription_orders
		SET status = 'APPROVED', paid_at = ?, mp_payment_id = COALESCE(?, mp_payment_id)
		WHERE id = ?`, now, nullInt64(mpPaymentID), orderID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE tenants
		SET plan_type = ?, monthly_quota = ?, subscription_status = 'ACTIVE', plan_expires_at = ?, is_active = TRUE
		WHERE id = ?`, string(o.PlanType), quota, exp, o.TenantID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return r.GetSubscriptionOrder(ctx, orderID)
}

// ListSubscriptionOrders lista órdenes de suscripción de un comercio (o de todos si tenantID == "").
func (r *MySQLRepository) ListSubscriptionOrders(ctx context.Context, tenantID string, limit int) ([]*domain.SubscriptionOrder, error) {
	if limit <= 0 {
		limit = 50
	}
	if r.db == nil {
		r.mem.mu.Lock()
		defer r.mem.mu.Unlock()
		var list []*domain.SubscriptionOrder
		for _, o := range r.mem.subOrders {
			if tenantID != "" && o.TenantID != tenantID {
				continue
			}
			copyO := *o
			if t, ok := r.mem.tenants[copyO.TenantID]; ok {
				copyO.BusinessName = t.BusinessName
				copyO.TenantEmail = t.Email
			}
			list = append(list, &copyO)
		}
		sort.Slice(list, func(i, j int) bool {
			return list[i].CreatedAt.After(list[j].CreatedAt)
		})
		if len(list) > limit {
			list = list[:limit]
		}
		return list, nil
	}

	var where string
	var args []interface{}
	if tenantID != "" {
		where = `WHERE o.tenant_id = ?`
		args = append(args, tenantID)
	}
	args = append(args, limit)

	rows, err := r.db.QueryContext(ctx, `
		SELECT o.id, o.tenant_id, COALESCE(t.business_name, ''), COALESCE(t.email, ''),
		       o.plan_type, o.price_usd, o.amount_ars, o.status, o.payment_mode,
		       o.mp_preference_id, o.mp_payment_id, o.checkout_url, o.paid_at, o.created_at, o.updated_at
		FROM subscription_orders o
		LEFT JOIN tenants t ON t.id = o.tenant_id
		`+where+`
		ORDER BY o.created_at DESC
		LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []*domain.SubscriptionOrder
	for rows.Next() {
		var o domain.SubscriptionOrder
		var planStr string
		var prefID, urlCol sql.NullString
		var mpPayID sql.NullInt64
		var paidAt sql.NullTime
		if err := rows.Scan(
			&o.ID, &o.TenantID, &o.BusinessName, &o.TenantEmail,
			&planStr, &o.PriceUSD, &o.AmountARS, &o.Status, &o.PaymentMode,
			&prefID, &mpPayID, &urlCol, &paidAt, &o.CreatedAt, &o.UpdatedAt,
		); err != nil {
			return nil, err
		}
		o.PlanType = domain.PlanType(planStr)
		o.MPPreferenceID = prefID.String
		o.MPPaymentID = mpPayID.Int64
		o.CheckoutURL = urlCol.String
		if paidAt.Valid {
			pt := paidAt.Time
			o.PaidAt = &pt
		}
		list = append(list, &o)
	}
	return list, rows.Err()
}
