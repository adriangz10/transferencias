-- 0004: nuevo resultado FRAUD_TAMPERED_RECEIPT (comprobante adulterado: por ejemplo, el CVU
-- de destino difiere en uno o dos dígitos del CVU del comercio). Idempotente.
ALTER TABLE receipt_validations MODIFY COLUMN validation_result ENUM(
    'VERIFIED_EXACT_ID',
    'VERIFIED_HEURISTIC',
    'REJECTED_NOT_FOUND',
    'REJECTED_WRONG_DESTINATION',
    'REJECTED_AMOUNT_MISMATCH',
    'FRAUD_ALREADY_USED',
    'FRAUD_DUPLICATE_FILE',
    'FRAUD_INCOMPLETE_TRANSFER',
    'FRAUD_TAMPERED_RECEIPT',
    'INVALID_RECEIPT_IMAGE'
) NOT NULL;
