package persistence

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"veripago-saas/internal/config"
	"veripago-saas/internal/domain"
)

// Test de integración contra el MySQL configurado en el .env de la raíz del proyecto.
// Se saltea si no hay MySQL disponible. Usa un tenant temporal y borra lo que crea.
func TestMySQLMigrationsAndQueries(t *testing.T) {
	t.Chdir("../../..") // para leer .env y migrations/ desde la raíz
	cfg := config.Load()
	if cfg.DBUser == "" {
		t.Skip("DB_USER no configurado: se saltea el test de integración con MySQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	migDB, err := sql.Open("mysql", cfg.DBMigrateDSN)
	if err != nil {
		t.Fatal(err)
	}
	migDB.SetMaxOpenConns(1)
	if err := migDB.PingContext(ctx); err != nil {
		_ = migDB.Close()
		t.Skipf("MySQL no disponible: %v", err)
	}
	if _, err := RunMigrations(ctx, migDB, "migrations"); err != nil {
		t.Fatalf("migraciones: %v", err)
	}
	_ = migDB.Close()

	db, err := sql.Open("mysql", cfg.DBDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var cols int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'tenants' AND COLUMN_NAME = 'quota_period'`).Scan(&cols); err != nil || cols != 1 {
		t.Fatalf("falta la columna tenants.quota_period (%v)", err)
	}

	repo := NewMySQLRepository(db)
	tenantID := "test-" + uuid.NewString()[:31]
	if _, err := db.ExecContext(ctx, `INSERT INTO tenants (id, business_name, plan_type, monthly_quota, time_window_minutes)
		VALUES (?, 'Comercio temporal de test', 'PRO', 1500, 120)`, tenantID); err != nil {
		t.Fatalf("creando comercio temporal: %v", err)
	}
	defer func() {
		bg := context.Background()
		_, _ = db.ExecContext(bg, `DELETE FROM receipt_validations WHERE tenant_id = ?`, tenantID)
		_, _ = db.ExecContext(bg, `DELETE FROM tenants WHERE id = ?`, tenantID) // borra en cascada pagos y clientes
	}()

	// Las consultas nuevas deben ser SQL válido
	if _, _, err := repo.CheckFileDuplicate(ctx, tenantID, "hash-inexistente"); err != nil {
		t.Fatalf("CheckFileDuplicate: %v", err)
	}
	if found, _, err := repo.FindVerifiedByReceiptID(ctx, tenantID, "95261521", 500); err != nil || found {
		t.Fatalf("FindVerifiedByReceiptID: found=%v err=%v", found, err)
	}
	if _, err := repo.ExistsValidationByWAMessageID(ctx, "wamid.inexistente"); err != nil {
		t.Fatalf("ExistsValidationByWAMessageID: %v", err)
	}
	if err := repo.IncrementTenantQuota(ctx, tenantID); err != nil {
		t.Fatalf("IncrementTenantQuota: %v", err)
	}
	if _, err := repo.GetTenantByID(ctx, tenantID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetTenantByID: %v", err)
	}

	// Conciliación por monto con CUIT
	p := &domain.MPPayment{TenantID: tenantID, MPPaymentID: 900000000001, Amount: 1234.56, MPStatus: "approved", PayerCUIT: "20111111112", CoelsaID: "TESTCOELSA0000000000001", PaymentDate: time.Now()}
	if err := repo.UpsertMPPayment(ctx, p); err != nil {
		t.Fatalf("UpsertMPPayment: %v", err)
	}
	if _, err := repo.ClaimPaymentByHeuristic(ctx, tenantID, 1234.56, 60, "20999999999", "v-1"); !errors.Is(err, ErrPaymentNotFound) {
		t.Fatalf("con CUIT distinto esperaba ErrPaymentNotFound, obtuvo %v", err)
	}
	if _, err := repo.ClaimPaymentByCoelsaID(ctx, tenantID, "TESTCOELSA0000000000001", 999, "v-2"); !errors.Is(err, ErrPaymentAmountMismatch) {
		t.Fatalf("con monto distinto esperaba ErrPaymentAmountMismatch, obtuvo %v", err)
	}
	got, err := repo.ClaimPaymentByHeuristic(ctx, tenantID, 1234.56, 60, "20-11111111-2", "v-3")
	if err != nil || got == nil {
		t.Fatalf("con el mismo CUIT debía conciliar: %v", err)
	}
	if _, err := repo.ClaimPaymentByExactID(ctx, tenantID, 900000000001, 1234.56, "v-4"); !errors.Is(err, ErrPaymentAlreadyConsumed) {
		t.Fatalf("esperaba ErrPaymentAlreadyConsumed, obtuvo %v", err)
	}

	// Clientes: el pago queda asociado a su cliente por CUIT
	cust, err := repo.UpsertCustomer(ctx, tenantID, "20-11111111-2", "Adrián  Gómez", "", time.Now())
	if err != nil || cust == nil || cust.CUIT != "20111111112" {
		t.Fatalf("UpsertCustomer: %+v %v", cust, err)
	}
	var payCustomer sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT customer_id FROM mp_payments WHERE tenant_id = ? AND mp_payment_id = 900000000001`, tenantID).Scan(&payCustomer); err != nil || payCustomer.String != cust.ID {
		t.Fatalf("el pago debía quedar asociado al cliente %s, tiene %q (%v)", cust.ID, payCustomer.String, err)
	}
	// Primero conocido solo por nombre; al aparecer su CUIT se completa el mismo registro
	byName, err := repo.UpsertCustomer(ctx, tenantID, "", "Laura Pérez", "", time.Now())
	if err != nil || byName == nil {
		t.Fatalf("UpsertCustomer por nombre: %v", err)
	}
	withCUIT, err := repo.UpsertCustomer(ctx, tenantID, "27222222223", "LAURA PEREZ", "laura@example.com", time.Now())
	if err != nil || withCUIT.ID != byName.ID || withCUIT.CUIT != "27222222223" || withCUIT.Email != "laura@example.com" {
		t.Fatalf("debía completar el mismo cliente con el CUIT: %+v %v", withCUIT, err)
	}

	// Comprobantes del cliente: historial, riesgo, búsqueda y borrado
	for i, st := range []domain.ValidationStatus{domain.StatusVerifiedHeuristic, domain.StatusFraudTampered, domain.StatusRejectedWrongDest} {
		v := &domain.ReceiptValidation{TenantID: tenantID, CustomerID: cust.ID, SenderPhone: "549110", FileSHA256: "sha-test",
			ExtractedAmount: 1234.56, ExtractedSenderCUIT: "20111111112", ExtractedSenderName: "Adrián Gómez", ValidationResult: st,
			WAMessageID: "wamid.test." + uuid.NewString(), CreatedAt: time.Now().Add(time.Duration(i) * time.Second)}
		if err := repo.SaveReceiptValidation(ctx, v); err != nil {
			t.Fatalf("SaveReceiptValidation: %v", err)
		}
	}
	fraud, last, recent, err := repo.CustomerRiskStats(ctx, tenantID, cust.ID, time.Now().Add(-time.Hour))
	if err != nil || fraud != 1 || last == nil || recent != 2 {
		t.Fatalf("CustomerRiskStats: fraud=%d last=%v recent=%d err=%v", fraud, last, recent, err)
	}
	list, total, err := repo.SearchCustomers(ctx, tenantID, domain.CustomerFilter{Query: "gomez", Limit: 10})
	if err != nil || total != 1 || list[0].Receipts != 3 || list[0].Fraud != 1 || list[0].Verified != 1 || list[0].Payments != 1 {
		t.Fatalf("SearchCustomers por nombre: total=%d %+v %v", total, list, err)
	}
	if _, total, err := repo.SearchCustomers(ctx, tenantID, domain.CustomerFilter{Query: "2011", Limit: 10}); err != nil || total != 1 {
		t.Fatalf("SearchCustomers por CUIT: total=%d %v", total, err)
	}
	if _, total, err := repo.SearchCustomers(ctx, tenantID, domain.CustomerFilter{OnlyFraud: true, Limit: 10}); err != nil || total != 1 {
		t.Fatalf("SearchCustomers solo fraude: total=%d %v", total, err)
	}
	page, total, err := repo.SearchReceiptValidations(ctx, tenantID, domain.ValidationFilter{CustomerID: cust.ID, Statuses: domain.FraudStatuses, Limit: 10})
	if err != nil || total != 1 || page[0].CustomerID != cust.ID {
		t.Fatalf("SearchReceiptValidations por cliente: total=%d %v", total, err)
	}
	if refs, err := repo.ListRecentVerifiedByAmount(ctx, tenantID, 1234.56, time.Now().Add(-time.Hour)); err != nil || len(refs) != 1 {
		t.Fatalf("ListRecentVerifiedByAmount: %d %v", len(refs), err)
	}
	if _, err := repo.DeleteReceiptValidation(ctx, tenantID, page[0].ID); err != nil {
		t.Fatalf("DeleteReceiptValidation: %v", err)
	}

	// Multi-Tenant SaaS (migración 0006): alta de comercio, login por email y suscripciones
	mtEmail := "test-" + uuid.NewString()[:8] + "@comercio.ar"
	mtTenant := &domain.Tenant{
		BusinessName:       "Comercio SaaS Test",
		Email:              mtEmail,
		PasswordHash:       "$2a$10$testhashtesthashtesthashtesthashtesthashtesthashtest",
		PlanType:           domain.PlanFreeTrial,
		SubscriptionStatus: "TRIAL",
		MonthlyQuota:       50,
		TimeWindowMinutes:  120,
		IsActive:           true,
	}
	if err := repo.CreateTenant(ctx, mtTenant); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	defer func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM tenants WHERE id = ?`, mtTenant.ID)
	}()

	byEmail, err := repo.GetTenantByEmail(ctx, mtEmail)
	if err != nil || byEmail == nil || byEmail.ID != mtTenant.ID {
		t.Fatalf("GetTenantByEmail: %+v %v", byEmail, err)
	}

	subOrd := &domain.SubscriptionOrder{
		ID:        uuid.NewString(),
		TenantID:  mtTenant.ID,
		PlanType:  domain.PlanPro,
		AmountARS: 33600,
		PriceUSD:  28,
		Status:    "PENDING",
	}
	if err := repo.CreateSubscriptionOrder(ctx, subOrd); err != nil {
		t.Fatalf("CreateSubscriptionOrder: %v", err)
	}
	approvedOrd, err := repo.ApproveSubscriptionOrder(ctx, subOrd.ID, 123456789)
	if err != nil || approvedOrd.Status != "APPROVED" {
		t.Fatalf("ApproveSubscriptionOrder: %+v %v", approvedOrd, err)
	}
	afterSub, err := repo.GetTenantByID(ctx, mtTenant.ID)
	if err != nil || afterSub.PlanType != domain.PlanPro || afterSub.SubscriptionStatus != "ACTIVE" || afterSub.MonthlyQuota != 1500 {
		t.Fatalf("el comercio debía pasar a PRO ACTIVE: %+v %v", afterSub, err)
	}
	if _, stats, err := repo.ListTenantsAdmin(ctx, "Comercio SaaS Test"); err != nil || stats.TotalTenants < 1 {
		t.Fatalf("ListTenantsAdmin: stats=%+v err=%v", stats, err)
	}
}
