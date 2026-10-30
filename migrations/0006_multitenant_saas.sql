-- 0006: SaaS Multi-Tenant (suscripciones, estados de facturación e historial de órdenes de planes).
-- Escrita de forma idempotente (se puede ejecutar más de una vez).

-- 1. Estado de suscripción y vencimiento del plan en tenants
SET @has := (SELECT COUNT(*) FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'tenants' AND COLUMN_NAME = 'subscription_status');
SET @ddl := IF(@has = 0,
    'ALTER TABLE tenants ADD COLUMN subscription_status ENUM(''TRIAL'', ''ACTIVE'', ''PAST_DUE'', ''CANCELLED'') NOT NULL DEFAULT ''TRIAL'' AFTER time_window_minutes',
    'DO 0');
PREPARE stmt FROM @ddl; EXECUTE stmt; DEALLOCATE PREPARE stmt;

SET @has := (SELECT COUNT(*) FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'tenants' AND COLUMN_NAME = 'plan_expires_at');
SET @ddl := IF(@has = 0,
    'ALTER TABLE tenants ADD COLUMN plan_expires_at TIMESTAMP NULL AFTER subscription_status',
    'DO 0');
PREPARE stmt FROM @ddl; EXECUTE stmt; DEALLOCATE PREPARE stmt;

-- 2. Órdenes de suscripción / checkout de planes comerciales
CREATE TABLE IF NOT EXISTS subscription_orders (
    id CHAR(36) PRIMARY KEY,
    tenant_id CHAR(36) NOT NULL,
    plan_type ENUM('FREE_TRIAL', 'EMPRENDEDOR', 'PRO', 'FULL') NOT NULL,
    price_usd INT NOT NULL DEFAULT 0,
    amount_ars DECIMAL(15, 2) NOT NULL DEFAULT 0.00,
    status ENUM('PENDING', 'APPROVED', 'REJECTED', 'CANCELLED') NOT NULL DEFAULT 'PENDING',
    payment_mode VARCHAR(30) NOT NULL DEFAULT 'MANUAL',
    mp_preference_id VARCHAR(120) NULL,
    mp_payment_id BIGINT NULL,
    checkout_url TEXT NULL,
    paid_at TIMESTAMP NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    INDEX idx_sub_tenant (tenant_id, created_at),
    INDEX idx_sub_status (status, created_at),
    CONSTRAINT fk_sub_tenant FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE CASCADE
) ENGINE=InnoDB;
