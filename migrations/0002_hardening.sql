-- 0002: cuota mensual con período, índice para conciliar por ID COELSA y nuevo resultado
-- REJECTED_AMOUNT_MISMATCH. Escrita de forma idempotente (se puede correr más de una vez).

-- 1. Período de la cuota usada (ej: '2026-10'). Al cambiar el mes, la cuota se reinicia.
SET @has_col := (SELECT COUNT(*) FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'tenants' AND COLUMN_NAME = 'quota_period');
SET @ddl := IF(@has_col = 0,
    'ALTER TABLE tenants ADD COLUMN quota_period CHAR(7) NULL AFTER used_quota',
    'DO 0');
PREPARE stmt FROM @ddl;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

-- 2. Índice para buscar pagos por ID COELSA (e2e_id de Mercado Pago)
SET @has_idx := (SELECT COUNT(*) FROM information_schema.STATISTICS
    WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'mp_payments' AND INDEX_NAME = 'idx_tenant_coelsa');
SET @ddl := IF(@has_idx = 0,
    'ALTER TABLE mp_payments ADD INDEX idx_tenant_coelsa (tenant_id, coelsa_id)',
    'DO 0');
PREPARE stmt FROM @ddl;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

-- 3. Nuevo resultado: el comprobante trae un ID real pero el monto no coincide
ALTER TABLE receipt_validations MODIFY COLUMN validation_result ENUM(
    'VERIFIED_EXACT_ID',
    'VERIFIED_HEURISTIC',
    'REJECTED_NOT_FOUND',
    'REJECTED_WRONG_DESTINATION',
    'REJECTED_AMOUNT_MISMATCH',
    'FRAUD_ALREADY_USED',
    'FRAUD_DUPLICATE_FILE',
    'FRAUD_INCOMPLETE_TRANSFER',
    'INVALID_RECEIPT_IMAGE'
) NOT NULL;
