import { HttpClient, HttpErrorResponse, HttpInterceptorFn, HttpParams } from '@angular/common/http';
import { inject, Injectable } from '@angular/core';
import { catchError, firstValueFrom, tap, throwError } from 'rxjs';
import { UiService } from './ui.service';
import {
  ChargeOrder,
  Customer,
  CustomerPage,
  Dashboard,
  Outcome,
  SubscriptionOrder,
  SuperAdminData,
  Tenant,
  ValidationPage,
} from './models';

/** Sesión vencida → login. Cualquier otro error se muestra arriba del panel. */
export const apiErrorInterceptor: HttpInterceptorFn = (req, next) => {
  const ui = inject(UiService);
  return next(req).pipe(
    tap(() => ui.error.set(null)),
    catchError((err: HttpErrorResponse) => {
      if (err.status === 401) {
        location.href = '/login';
      } else {
        const msg =
          (err.error && typeof err.error === 'object' && err.error.error) ||
          (err.status === 0
            ? 'No hay conexión con el servidor.'
            : `Algo salió mal (error ${err.status}). Probá de nuevo.`);
        ui.error.set(msg);
      }
      return throwError(() => err);
    }),
  );
};

export interface HistoryQuery {
  page: number;
  pageSize: number;
  date: string; // AAAA-MM-DD o vacío
  category: string; // all | ok | fraud | rejected | not_found
  customerId?: string;
}

export interface RegisterPayload {
  business_name: string;
  email: string;
  password: string;
  cuit?: string;
  owner_phone?: string;
}

export interface SuperAdminTenantPayload {
  tenant_id?: string;
  business_name: string;
  cuit?: string;
  email: string;
  password?: string;
  plan_type: string;
  subscription_status: string;
  monthly_quota?: number;
  is_active: boolean;
  reset_quota?: boolean;
}

@Injectable({ providedIn: 'root' })
export class ApiService {
  private http = inject(HttpClient);

  dashboard(): Promise<Dashboard> {
    return firstValueFrom(this.http.get<Dashboard>('/api/v1/dashboard'));
  }

  register(payload: RegisterPayload): Promise<{ status: string; tenant: Tenant }> {
    return this.post('/api/v1/register', payload);
  }

  changePassword(currentPassword: string, newPassword: string): Promise<{ status: string }> {
    return this.post('/api/v1/account/password', {
      current_password: currentPassword,
      new_password: newPassword,
    });
  }

  checkoutPlan(plan: string): Promise<{ order: SubscriptionOrder; checkout_url: string; payment_mode: string }> {
    return this.post('/api/v1/billing/checkout', { plan });
  }

  simulateSubscriptionPay(orderId: string): Promise<{ status: string; order: SubscriptionOrder }> {
    return this.post('/api/v1/billing/simulate-pay', { order_id: orderId });
  }

  superAdminData(q = ''): Promise<SuperAdminData> {
    let params = new HttpParams();
    if (q.trim()) params = params.set('q', q.trim());
    return firstValueFrom(this.http.get<SuperAdminData>('/api/v1/superadmin/tenants', { params }));
  }

  superAdminSaveTenant(payload: SuperAdminTenantPayload): Promise<unknown> {
    return this.post('/api/v1/superadmin/tenants', payload);
  }

  superAdminImpersonate(tenantId: string): Promise<{ status: string; impersonating: boolean; active_tenant_id: string }> {
    return this.post('/api/v1/superadmin/impersonate', { tenant_id: tenantId });
  }

  superAdminApproveSubscription(orderId: string): Promise<{ status: string; order: SubscriptionOrder }> {
    return this.post('/api/v1/superadmin/subscriptions/approve', { order_id: orderId });
  }

  validations(q: HistoryQuery): Promise<ValidationPage> {
    return firstValueFrom(this.http.get<ValidationPage>('/api/v1/validations', { params: this.historyParams(q) }));
  }

  customers(q: { page: number; pageSize: number; query: string; onlyFraud: boolean }): Promise<CustomerPage> {
    let p = new HttpParams().set('page', q.page).set('page_size', q.pageSize);
    if (q.query.trim()) p = p.set('q', q.query.trim());
    if (q.onlyFraud) p = p.set('risk', 'fraud');
    return firstValueFrom(this.http.get<CustomerPage>('/api/v1/customers', { params: p }));
  }

  customer(id: string): Promise<Customer> {
    return firstValueFrom(this.http.get<Customer>('/api/v1/customers', { params: { id } }));
  }

  saveBusiness(businessName: string, cuit: string, email: string): Promise<unknown> {
    return this.post('/api/v1/business', { business_name: businessName, cuit, email });
  }

  deleteValidation(id: string): Promise<{ payment_released: boolean }> {
    return firstValueFrom(
      this.http.delete<{ payment_released: boolean }>('/api/v1/validations', { params: { id } }),
    );
  }

  exportUrl(q: Pick<HistoryQuery, 'date' | 'category' | 'customerId'>): string {
    const p = this.historyParams({ ...q, page: 1, pageSize: 20 }).delete('page').delete('page_size');
    const s = p.toString();
    return '/api/v1/export/validations.csv' + (s ? '?' + s : '');
  }

  async uploadReceipt(file: File): Promise<Outcome> {
    const fd = new FormData();
    fd.append('receipt_file', file);
    const job = await firstValueFrom(this.http.post<{ job_id: string }>('/api/v1/receipts/upload', fd));
    // El análisis corre en el servidor: se consulta cada 2 segundos hasta tener el resultado
    const deadline = Date.now() + 3 * 60 * 1000;
    while (Date.now() < deadline) {
      await new Promise((r) => setTimeout(r, 2000));
      const st = await firstValueFrom(
        this.http.get<{ status: string; outcome?: Outcome; error?: string }>('/api/v1/receipts/upload', {
          params: { job: job.job_id },
        }),
      );
      if (st.status === 'done' && st.outcome) return st.outcome;
      if (st.status === 'error') throw new Error(st.error || 'No se pudo analizar el comprobante.');
    }
    throw new Error('El análisis está tardando demasiado. Revisá el historial en unos minutos.');
  }

  createCharge(baseAmount: number, reference: string): Promise<ChargeOrder> {
    return this.post('/api/v1/posnet/charge', { base_amount: baseAmount, reference });
  }

  saveAccount(alias: string, cvu: string): Promise<unknown> {
    return this.post('/api/v1/account', { expected_alias: alias, expected_cvu: cvu });
  }

  addSender(name: string, phone: string, role: string): Promise<unknown> {
    return this.post('/api/v1/senders', { employee_name: name, phone_number: phone, role });
  }

  deleteSender(id: string): Promise<unknown> {
    return firstValueFrom(this.http.delete('/api/v1/senders', { params: { id } }));
  }

  savePlan(plan: string, timeWindowMinutes: number): Promise<unknown> {
    return this.post('/api/v1/plan', { plan, time_window_minutes: timeWindowMinutes });
  }

  // Solo modo demo
  simulateReceipt(scenario: string, senderPhone: string): Promise<Outcome> {
    return this.post('/api/v1/simulate/receipt', { scenario, sender_phone: senderPhone });
  }
  simulatePayment(amount?: number): Promise<unknown> {
    return this.post('/api/v1/simulate/mp-webhook', amount ? { amount, payer_name: 'Cliente de prueba' } : {});
  }
  sendCommand(text: string, senderPhone: string): Promise<unknown> {
    return this.post('/api/v1/simulate/whatsapp-command', { text, sender_phone: senderPhone });
  }

  private post<T>(url: string, body: unknown): Promise<T> {
    return firstValueFrom(this.http.post<T>(url, body));
  }

  private historyParams(q: HistoryQuery): HttpParams {
    let p = new HttpParams().set('page', q.page).set('page_size', q.pageSize);
    if (q.date) p = p.set('date', q.date);
    if (q.category && q.category !== 'all') p = p.set('category', q.category);
    if (q.customerId) p = p.set('customer_id', q.customerId);
    return p;
  }
}
