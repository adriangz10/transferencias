CREATE DATABASE IF NOT EXISTS veripago_db CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
USE veripago_db;

-- 1. COMERCIOS SUSCRIPTOS (TENANTS)
CREATE TABLE IF NOT EXISTS tenants (
    id CHAR(36) PRIMARY KEY,
    business_name VARCHAR(150) NOT NULL,
    cuit VARCHAR(13) UNIQUE NOT NULL,
    email VARCHAR(150) UNIQUE NOT NULL,
    password_hash VARCHAR(255) NOT NULL,
    plan_type ENUM('FREE_TRIAL', 'EMPRENDEDOR', 'PRO', 'FULL') DEFAULT 'FREE_TRIAL',
    monthly_quota INT NOT NULL DEFAULT 50,
    used_quota INT NOT NULL DEFAULT 0,
    time_window_minutes INT NOT NULL DEFAULT 120,
    is_active BOOLEAN DEFAULT TRUE,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
) ENGINE=InnoDB;

-- 2. CUENTAS VINCULADAS DE MERCADO PAGO (TOKENS ENCRIPTADOS CON AES-256-GCM)
CREATE TABLE IF NOT EXISTS mp_accounts (
    id CHAR(36) PRIMARY KEY,
    tenant_id CHAR(36) NOT NULL,
    mp_user_id BIGINT NOT NULL,
    expected_alias VARCHAR(100) NULL,
    expected_cvu VARCHAR(22) NULL,
    expected_holder_name VARCHAR(150) NULL,
    access_token_enc TEXT NOT NULL,
    refresh_token_enc TEXT NOT NULL,
    token_expires_at TIMESTAMP NOT NULL,
    webhook_secret VARCHAR(255) NULL,
    status ENUM('CONNECTED', 'EXPIRED', 'REVOKED') DEFAULT 'CONNECTED',
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    UNIQUE KEY uk_mp_user (mp_user_id),
    INDEX idx_tenant_mp (tenant_id),
    CONSTRAINT fk_mp_tenant FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE CASCADE
) ENGINE=InnoDB;

-- 3. NÚMEROS DE WHATSAPP DE EMPLEADOS / VENDEDORES AUTORIZADOS
CREATE TABLE IF NOT EXISTS authorized_senders (
    id CHAR(36) PRIMARY KEY,
    tenant_id CHAR(36) NOT NULL,
    phone_number VARCHAR(25) NOT NULL,
    employee_name VARCHAR(100) NOT NULL,
    role ENUM('OWNER', 'CASHIER', 'DELIVERY') DEFAULT 'CASHIER',
    is_active BOOLEAN DEFAULT TRUE,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    UNIQUE KEY uk_phone_tenant (phone_number, tenant_id),
    INDEX idx_phone_lookup (phone_number),
    CONSTRAINT fk_sender_tenant FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE CASCADE
) ENGINE=InnoDB;

-- 4. LIBRO MAYOR DE PAGOS REALES DE MERCADO PAGO (FUENTE DE LA VERDAD)
CREATE TABLE IF NOT EXISTS mp_payments (
    id CHAR(36) PRIMARY KEY,
    tenant_id CHAR(36) NOT NULL,
    mp_payment_id BIGINT NOT NULL,
    amount DECIMAL(15, 2) NOT NULL,
    currency VARCHAR(5) DEFAULT 'ARS',
    mp_status VARCHAR(30) NOT NULL,
    operation_type VARCHAR(50) NULL,
    payer_cuit VARCHAR(20) NULL,
    payer_name VARCHAR(150) NULL,
    coelsa_id VARCHAR(60) NULL,
    payment_date TIMESTAMP NOT NULL,
    reconciliation_status ENUM('AVAILABLE', 'CONSUMED', 'DISPUTED') DEFAULT 'AVAILABLE',
    consumed_at TIMESTAMP NULL,
    consumed_by_receipt_id CHAR(36) NULL,
    raw_json JSON NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    UNIQUE KEY uk_tenant_mppayment (tenant_id, mp_payment_id),
    INDEX idx_matching_search (tenant_id, reconciliation_status, amount, payment_date)
) ENGINE=InnoDB;

-- 5. AUDITORÍA DE COMPROBANTES PROCESADOS
CREATE TABLE IF NOT EXISTS receipt_validations (
    id CHAR(36) PRIMARY KEY,
    tenant_id CHAR(36) NOT NULL,
    sender_phone VARCHAR(25) NOT NULL,
    wa_message_id VARCHAR(120) UNIQUE NOT NULL,
    file_sha256 CHAR(64) NOT NULL,
    extracted_amount DECIMAL(15, 2) NULL,
    extracted_op_number VARCHAR(100) NULL,
    extracted_coelsa_id VARCHAR(100) NULL,
    extracted_date TIMESTAMP NULL,
    extracted_sender_name VARCHAR(150) NULL,
    extracted_sender_cuit VARCHAR(20) NULL,
    extracted_receiver_alias_or_cbu VARCHAR(150) NULL,
    extracted_source_bank VARCHAR(100) NULL,
    ai_fraud_warnings JSON NULL,
    validation_result ENUM(
        'VERIFIED_EXACT_ID',
        'VERIFIED_HEURISTIC',
        'REJECTED_NOT_FOUND',
        'REJECTED_WRONG_DESTINATION',
        'FRAUD_ALREADY_USED',
        'FRAUD_DUPLICATE_FILE',
        'FRAUD_INCOMPLETE_TRANSFER',
        'INVALID_RECEIPT_IMAGE'
    ) NOT NULL,
    matched_mp_payment_id CHAR(36) NULL,
    processing_time_ms INT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    INDEX idx_tenant_sha (tenant_id, file_sha256),
    INDEX idx_tenant_created (tenant_id, created_at),
    CONSTRAINT fk_val_tenant FOREIGN KEY (tenant_id) REFERENCES tenants(id),
    CONSTRAINT fk_val_mppayment FOREIGN KEY (matched_mp_payment_id) REFERENCES mp_payments(id)
) ENGINE=InnoDB;
