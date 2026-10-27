// Tipos de la API de Go (/api/v1). Los nombres siguen los tags JSON del backend.

export type ValidationStatus =
  | 'VERIFIED_EXACT_ID'
  | 'VERIFIED_HEURISTIC'
  | 'REJECTED_NOT_FOUND'
  | 'REJECTED_WRONG_DESTINATION'
  | 'REJECTED_AMOUNT_MISMATCH'
  | 'FRAUD_ALREADY_USED'
  | 'FRAUD_DUPLICATE_FILE'
  | 'FRAUD_INCOMPLETE_TRANSFER'
  | 'FRAUD_TAMPERED_RECEIPT'
  | 'INVALID_RECEIPT_IMAGE'
  | 'QUOTA_EXCEEDED'
  | 'MP_NOT_CONNECTED';

export type Category = 'ok' | 'fraud' | 'rejected' | 'not_found';

export interface Tenant {
  id: string;
  business_name: string;
  cuit: string;
  email: string;
  plan_type: string;
  monthly_quota: number;
  used_quota: number;
  time_window_minutes: number;
}

export interface PlanSpec {
  plan: string;
  name: string;
  monthly_quota: number;
}

export interface MPAccount {
  id: string;
  mp_user_id: number;
  expected_alias: string;
  expected_cvu: string;
}

export interface Sender {
  id: string;
  phone_number: string;
  employee_name: string;
  role: 'OWNER' | 'CASHIER' | 'DELIVERY';
}

export interface Payment {
  id: string;
  customer_id?: string;
  mp_payment_id: number;
  amount: number;
  payer_name: string;
  payer_cuit: string;
  payment_date: string;
  reconciliation_status: 'AVAILABLE' | 'CONSUMED' | 'DISPUTED';
}

export interface PendingItem {
  validation_id: string;
  sender_phone: string;
  employee_name: string;
  expected_amount: number;
  sender_name: string;
  created_at: string;
}

export interface ChargeOrder {
  id: string;
  reference: string;
  unique_amount: number;
  alias_or_cvu: string;
  status: 'PENDING' | 'PAID' | string;
  created_at: string;
}

export interface WAMessage {
  direction: 'INBOUND' | 'OUTBOUND';
  sender: string;
  body: string;
}

export interface TodayStats {
  ok: number;
  fraud: number;
  rejected: number;
  not_found: number;
}

export interface Dashboard {
  tenant: Tenant | null;
  plans: Record<string, PlanSpec>;
  mp_account: MPAccount | null;
  senders: Sender[] | null;
  payments: Payment[] | null;
  pending_queue: PendingItem[] | null;
  charge_orders: ChargeOrder[] | null;
  wa_messages: WAMessage[] | null;
  today: TodayStats;
  demo_mode: boolean;
  auth_enabled: boolean;
  system: { mysql_connected: boolean; redis_connected: boolean; gemini_live_key: boolean };
}

export interface Validation {
  id: string;
  sender_phone: string;
  file_sha256: string;
  extracted_amount: number;
  extracted_op_number: string;
  extracted_coelsa_id: string;
  extracted_date?: string;
  extracted_sender_name: string;
  extracted_sender_cuit: string;
  extracted_receiver_alias_or_cbu: string;
  extracted_source_bank: string;
  ai_fraud_warnings: string[] | null;
  validation_result: ValidationStatus;
  matched_mp_payment_id?: string;
  processing_time_ms: number;
  created_at: string;
  category: Category;
  customer_id?: string;
  customer?: CustomerBrief;
}

/** Resumen del cliente que acompaña a cada comprobante del historial */
export interface CustomerBrief {
  id: string;
  name: string;
  cuit?: string;
  receipts: number;
  verified: number;
  fraud: number;
}

export interface Customer {
  id: string;
  cuit?: string;
  name: string;
  email?: string;
  first_seen_at: string;
  last_seen_at: string;
  receipts: number;
  verified: number;
  fraud: number;
  rejected: number;
  not_found: number;
  verified_amount: number;
  payments: number;
  last_fraud_at?: string;
}

export interface CustomerPage {
  items: Customer[];
  total: number;
  page: number;
  page_size: number;
}

export interface ValidationPage {
  items: Validation[];
  total: number;
  page: number;
  page_size: number;
}

export interface Outcome {
  validation_id: string;
  status: ValidationStatus;
  message_for_user: string;
}
