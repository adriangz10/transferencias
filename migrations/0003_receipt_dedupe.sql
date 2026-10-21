-- 0003: índice para detectar rápido un comprobante ya verificado con el mismo número de
-- operación y monto (aunque WhatsApp haya recomprimido la imagen). Idempotente.
SET @has_idx := (SELECT COUNT(*) FROM information_schema.STATISTICS
    WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'receipt_validations' AND INDEX_NAME = 'idx_tenant_amount');
SET @ddl := IF(@has_idx = 0,
    'ALTER TABLE receipt_validations ADD INDEX idx_tenant_amount (tenant_id, extracted_amount)',
    'DO 0');
PREPARE stmt FROM @ddl;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;
