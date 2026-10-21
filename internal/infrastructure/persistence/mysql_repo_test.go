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
	defer func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM mp_payments WHERE tenant_id = ?`, tenantID)
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
}
