-- 0005: clientes normalizados para cruzar datos entre comprobantes y pagos. Idempotente.
--
--  * customers: un registro por cliente y comercio. Se identifica por CUIT; si el comprobante no
--    trae CUIT, por el nombre normalizado. identity_key = 'CUIT:<11 dígitos>' o 'NAME:<nombre>'.
--  * receipt_validations.customer_id y mp_payments.customer_id apuntan al cliente. Los datos tal
--    como figuran en el comprobante / en Mercado Pago se conservan aparte como evidencia.
--  * tenants: el CUIT, el email y la contraseña dejan de ser obligatorios (antes se completaban
--    con valores inventados). Se cargan desde el panel.

-- 1. Clientes
CREATE TABLE IF NOT EXISTS customers (
    id CHAR(36) PRIMARY KEY,
    tenant_id CHAR(36) NOT NULL,
    identity_key VARCHAR(170) NOT NULL,
    cuit CHAR(11) NULL,
    name VARCHAR(150) NOT NULL DEFAULT '',
    name_key VARCHAR(150) NOT NULL DEFAULT '',
    email VARCHAR(150) NULL,
    first_seen_at TIMESTAMP NOT NULL,
    last_seen_at TIMESTAMP NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    UNIQUE KEY uk_customer_identity (tenant_id, identity_key),
    KEY idx_customer_cuit (tenant_id, cuit),
    KEY idx_customer_name (tenant_id, name_key),
    KEY idx_customer_last_seen (tenant_id, last_seen_at),
    CONSTRAINT fk_customer_tenant FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE CASCADE
) ENGINE=InnoDB;

-- 2. Datos del comercio opcionales (sin valores inventados)
ALTER TABLE tenants
    MODIFY COLUMN cuit VARCHAR(13) NULL,
    MODIFY COLUMN email VARCHAR(150) NULL,
    MODIFY COLUMN password_hash VARCHAR(255) NULL;

-- 3. receipt_validations.customer_id + índices para filtrar por cliente y por resultado
SET @has := (SELECT COUNT(*) FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'receipt_validations' AND COLUMN_NAME = 'customer_id');
SET @ddl := IF(@has = 0, 'ALTER TABLE receipt_validations ADD COLUMN customer_id CHAR(36) NULL AFTER tenant_id', 'DO 0');
PREPARE stmt FROM @ddl; EXECUTE stmt; DEALLOCATE PREPARE stmt;

SET @has := (SELECT COUNT(*) FROM information_schema.STATISTICS
    WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'receipt_validations' AND INDEX_NAME = 'idx_val_customer');
SET @ddl := IF(@has = 0, 'ALTER TABLE receipt_validations ADD INDEX idx_val_customer (tenant_id, customer_id, created_at)', 'DO 0');
PREPARE stmt FROM @ddl; EXECUTE stmt; DEALLOCATE PREPARE stmt;

SET @has := (SELECT COUNT(*) FROM information_schema.STATISTICS
    WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'receipt_validations' AND INDEX_NAME = 'idx_val_result');
SET @ddl := IF(@has = 0, 'ALTER TABLE receipt_validations ADD INDEX idx_val_result (tenant_id, validation_result, created_at)', 'DO 0');
PREPARE stmt FROM @ddl; EXECUTE stmt; DEALLOCATE PREPARE stmt;

SET @has := (SELECT COUNT(*) FROM information_schema.TABLE_CONSTRAINTS
    WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'receipt_validations' AND CONSTRAINT_NAME = 'fk_val_customer');
SET @ddl := IF(@has = 0, 'ALTER TABLE receipt_validations ADD CONSTRAINT fk_val_customer FOREIGN KEY (customer_id) REFERENCES customers(id) ON DELETE SET NULL', 'DO 0');
PREPARE stmt FROM @ddl; EXECUTE stmt; DEALLOCATE PREPARE stmt;

-- 4. mp_payments: cliente, email del pagador y relación con el comercio
SET @has := (SELECT COUNT(*) FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'mp_payments' AND COLUMN_NAME = 'customer_id');
SET @ddl := IF(@has = 0, 'ALTER TABLE mp_payments ADD COLUMN customer_id CHAR(36) NULL AFTER tenant_id', 'DO 0');
PREPARE stmt FROM @ddl; EXECUTE stmt; DEALLOCATE PREPARE stmt;

SET @has := (SELECT COUNT(*) FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'mp_payments' AND COLUMN_NAME = 'payer_email');
SET @ddl := IF(@has = 0, 'ALTER TABLE mp_payments ADD COLUMN payer_email VARCHAR(150) NULL AFTER payer_name', 'DO 0');
PREPARE stmt FROM @ddl; EXECUTE stmt; DEALLOCATE PREPARE stmt;

SET @has := (SELECT COUNT(*) FROM information_schema.STATISTICS
    WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'mp_payments' AND INDEX_NAME = 'idx_pay_customer');
SET @ddl := IF(@has = 0, 'ALTER TABLE mp_payments ADD INDEX idx_pay_customer (tenant_id, customer_id, payment_date)', 'DO 0');
PREPARE stmt FROM @ddl; EXECUTE stmt; DEALLOCATE PREPARE stmt;

SET @has := (SELECT COUNT(*) FROM information_schema.STATISTICS
    WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'mp_payments' AND INDEX_NAME = 'idx_pay_date');
SET @ddl := IF(@has = 0, 'ALTER TABLE mp_payments ADD INDEX idx_pay_date (tenant_id, payment_date)', 'DO 0');
PREPARE stmt FROM @ddl; EXECUTE stmt; DEALLOCATE PREPARE stmt;

SET @has := (SELECT COUNT(*) FROM information_schema.TABLE_CONSTRAINTS
    WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'mp_payments' AND CONSTRAINT_NAME = 'fk_pay_customer');
SET @ddl := IF(@has = 0, 'ALTER TABLE mp_payments ADD CONSTRAINT fk_pay_customer FOREIGN KEY (customer_id) REFERENCES customers(id) ON DELETE SET NULL', 'DO 0');
PREPARE stmt FROM @ddl; EXECUTE stmt; DEALLOCATE PREPARE stmt;

SET @has := (SELECT COUNT(*) FROM information_schema.TABLE_CONSTRAINTS
    WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'mp_payments' AND CONSTRAINT_NAME = 'fk_pay_tenant');
SET @ddl := IF(@has = 0, 'ALTER TABLE mp_payments ADD CONSTRAINT fk_pay_tenant FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE CASCADE', 'DO 0');
PREPARE stmt FROM @ddl; EXECUTE stmt; DEALLOCATE PREPARE stmt;
