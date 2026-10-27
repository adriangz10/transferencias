import { Category, ValidationStatus } from './models';

// Tono visual de cada estado. Colores apagados: solo un punto y el texto con un leve fondo.
export type Tone = 'ok' | 'bad' | 'warn' | 'neutral';

export interface StatusInfo {
  /** Texto corto del motivo */
  label: string;
  /** Explicación para el detalle */
  detail: string;
  category: Category;
}

export const STATUS: Record<ValidationStatus, StatusInfo> = {
  VERIFIED_EXACT_ID: {
    label: 'Pago confirmado',
    detail: 'El número de operación del comprobante coincide con un pago acreditado en Mercado Pago.',
    category: 'ok',
  },
  VERIFIED_HEURISTIC: {
    label: 'Pago confirmado',
    detail: 'Coinciden el monto, el CUIT del emisor y el horario con un pago acreditado en Mercado Pago.',
    category: 'ok',
  },
  REJECTED_NOT_FOUND: {
    label: 'No ingresó',
    detail: 'El pago no apareció en tu cuenta de Mercado Pago dentro del tiempo de espera.',
    category: 'not_found',
  },
  REJECTED_WRONG_DESTINATION: {
    label: 'Transferido a otra cuenta',
    detail:
      'El comprobante es de una transferencia a una cuenta que no es la tuya. No parece editado: puede ser un error del cliente.',
    category: 'rejected',
  },
  INVALID_RECEIPT_IMAGE: {
    label: 'No es un comprobante',
    detail: 'No se pudo leer un comprobante de transferencia en el archivo enviado.',
    category: 'rejected',
  },
  FRAUD_TAMPERED_RECEIPT: {
    label: 'Comprobante adulterado',
    detail:
      'El destino del comprobante es casi igual a tu cuenta pero con dígitos cambiados, o el CVU no existe. Es un comprobante editado.',
    category: 'fraud',
  },
  REJECTED_AMOUNT_MISMATCH: {
    label: 'Monto adulterado',
    detail:
      'La operación existe en Mercado Pago, pero por un monto distinto al que muestra el comprobante.',
    category: 'fraud',
  },
  FRAUD_ALREADY_USED: {
    label: 'Pago ya usado',
    detail: 'El pago es real, pero ya se había usado para validar otra venta.',
    category: 'fraud',
  },
  FRAUD_DUPLICATE_FILE: {
    label: 'Comprobante repetido',
    detail: 'Este mismo comprobante ya se había usado para validar una venta.',
    category: 'fraud',
  },
  FRAUD_INCOMPLETE_TRANSFER: {
    label: 'Transferencia no finalizada',
    detail: 'La captura muestra una transferencia programada, en proceso o sin confirmar: el dinero no se envió.',
    category: 'fraud',
  },
  QUOTA_EXCEEDED: {
    label: 'Límite del plan',
    detail: 'Se alcanzó el límite mensual de comprobantes del plan.',
    category: 'rejected',
  },
  MP_NOT_CONNECTED: {
    label: 'Mercado Pago sin vincular',
    detail: 'Vinculá Mercado Pago para poder verificar los pagos.',
    category: 'rejected',
  },
};

export const CATEGORY: Record<Category | 'pending', { label: string; tone: Tone }> = {
  ok: { label: 'Correcta', tone: 'ok' },
  fraud: { label: 'Fraude', tone: 'bad' },
  rejected: { label: 'Rechazada', tone: 'neutral' },
  not_found: { label: 'No ingresó', tone: 'warn' },
  pending: { label: 'Pendiente', tone: 'warn' },
};

export function statusInfo(s: ValidationStatus | string): StatusInfo {
  return (
    STATUS[s as ValidationStatus] ?? { label: 'Sin resultado', detail: '', category: 'rejected' }
  );
}
