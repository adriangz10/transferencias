import { DatePipe, DecimalPipe } from '@angular/common';
import { ChangeDetectionStrategy, Component, computed, inject, OnInit, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { Router } from '@angular/router';
import { ApiService, SuperAdminTenantPayload } from '../../core/api.service';
import { DashboardStore } from '../../core/dashboard.store';
import { SubscriptionOrder, SuperAdminData, TenantAdminSummary } from '../../core/models';
import { UiService } from '../../core/ui.service';
import { Icon } from '../../shared/icon';

const SUB_LABELS: Record<string, string> = {
  TRIAL: 'Prueba',
  ACTIVE: 'Activa',
  PAST_DUE: 'Vencida',
  CANCELLED: 'Cancelada',
};

@Component({
  selector: 'app-superadmin',
  imports: [FormsModule, Icon, DecimalPipe, DatePipe],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="mb-6 flex flex-wrap items-center justify-between gap-3">
      <div>
        <h1 class="text-lg font-semibold tracking-tight">Super-Admin · Gestión SaaS</h1>
        <p class="muted text-sm">
          Administrá todos los comercios registrados, sus planes, suscripciones y accesos.
        </p>
      </div>
      <div class="flex items-center gap-2">
        <button class="btn btn-secondary" type="button" (click)="load()">Actualizar</button>
        <button class="btn btn-primary" type="button" (click)="openCreate()">
          <app-icon name="plus" [size]="14" /> Nuevo comercio
        </button>
      </div>
    </div>

    @if (data(); as d) {
      <div class="mb-6 grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <div class="card p-4">
          <div class="muted text-xs font-medium">Comercios activos</div>
          <div class="mt-1 text-2xl font-semibold tabular">
            {{ d.stats.active_tenants }}
            <span class="muted text-sm font-normal">/ {{ d.stats.total_tenants }}</span>
          </div>
        </div>
        <div class="card p-4">
          <div class="muted text-xs font-medium">Suscripciones pagas</div>
          <div class="mt-1 text-2xl font-semibold tabular">{{ d.stats.paid_subscriptions }}</div>
        </div>
        <div class="card p-4">
          <div class="muted text-xs font-medium">Comprobantes del mes</div>
          <div class="mt-1 text-2xl font-semibold tabular">
            {{ d.stats.monthly_validations | number: '1.0-0' }}
          </div>
        </div>
        <div class="card p-4">
          <div class="muted text-xs font-medium">Facturación suscripciones</div>
          <div class="mt-1 text-2xl font-semibold tabular">
            \${{ d.stats.approved_revenue_ars | number: '1.0-0' }}
            @if (d.stats.pending_orders_count > 0) {
              <span class="ml-1 text-xs font-normal text-amber-600 dark:text-amber-400">
                ({{ d.stats.pending_orders_count }} pend.)
              </span>
            }
          </div>
        </div>
      </div>
    }

    @if (showForm()) {
      <section class="card mb-6 p-5">
        <div class="flex items-center justify-between gap-2">
          <h2 class="card-title">
            {{ editingId() ? 'Editar comercio' : 'Dar de alta nuevo comercio' }}
          </h2>
          <button class="btn btn-ghost btn-sm" type="button" (click)="showForm.set(false)">
            <app-icon name="x" [size]="14" /> Cerrar
          </button>
        </div>

        <form class="mt-4 grid gap-4 sm:grid-cols-2 lg:grid-cols-3" (ngSubmit)="saveTenant()">
          <div>
            <label class="label" for="sa_name">Nombre del comercio</label>
            <input
              id="sa_name"
              name="sa_name"
              class="input"
              required
              [(ngModel)]="formName"
              placeholder="Ej: Kiosco Central"
            />
          </div>
          <div>
            <label class="label" for="sa_email">Email de acceso</label>
            <input
              id="sa_email"
              name="sa_email"
              type="email"
              class="input"
              required
              [(ngModel)]="formEmail"
              placeholder="dueno@comercio.com"
            />
          </div>
          <div>
            <label class="label" for="sa_cuit">CUIT</label>
            <input
              id="sa_cuit"
              name="sa_cuit"
              class="input font-mono"
              [(ngModel)]="formCuit"
              placeholder="30-12345678-9"
            />
          </div>
          <div>
            <label class="label" for="sa_pass">
              Contraseña {{ editingId() ? '(dejar vacío para no cambiar)' : '(mín. 8 caracteres)' }}
            </label>
            <input
              id="sa_pass"
              name="sa_pass"
              type="password"
              class="input"
              [(ngModel)]="formPassword"
              placeholder="••••••••"
            />
          </div>
          <div>
            <label class="label" for="sa_plan">Plan</label>
            <select
              id="sa_plan"
              name="sa_plan"
              class="input"
              [(ngModel)]="formPlan"
              (ngModelChange)="onPlanChange($event)"
            >
              <option value="FREE_TRIAL">Free Trial (50/mes)</option>
              <option value="EMPRENDEDOR">Emprendedor (300/mes)</option>
              <option value="PRO">Comercio Pro (1500/mes)</option>
              <option value="FULL">Full / Franquicia (5000/mes)</option>
            </select>
          </div>
          <div>
            <label class="label" for="sa_sub">Estado de suscripción</label>
            <select id="sa_sub" name="sa_sub" class="input" [(ngModel)]="formSubStatus">
              <option value="TRIAL">Período de prueba (TRIAL)</option>
              <option value="ACTIVE">Activa (ACTIVE)</option>
              <option value="PAST_DUE">Pago vencido (PAST_DUE)</option>
              <option value="CANCELLED">Cancelada (CANCELLED)</option>
            </select>
          </div>
          <div>
            <label class="label" for="sa_quota">Cupo mensual de comprobantes</label>
            <input
              id="sa_quota"
              name="sa_quota"
              type="number"
              min="1"
              class="input"
              [(ngModel)]="formQuota"
            />
          </div>
          <div class="flex flex-col justify-end gap-2">
            <label class="inline-flex items-center gap-2 text-sm">
              <input type="checkbox" name="sa_active" [(ngModel)]="formActive" />
              <span>Comercio habilitado (puede ingresar y usar el bot)</span>
            </label>
            @if (editingId()) {
              <label class="inline-flex items-center gap-2 text-sm">
                <input type="checkbox" name="sa_reset" [(ngModel)]="formResetQuota" />
                <span>Reiniciar contador de usados a 0</span>
              </label>
            }
          </div>
          <div class="flex items-end justify-end gap-2 sm:col-span-2 lg:col-span-1">
            <button class="btn btn-secondary" type="button" (click)="showForm.set(false)">Cancelar</button>
            <button class="btn btn-primary" type="submit" [disabled]="!formName().trim() || !formEmail().trim()">
              {{ editingId() ? 'Guardar cambios' : 'Crear comercio' }}
            </button>
          </div>
        </form>
      </section>
    }

    <section class="card">
      <div class="card-header">
        <div>
          <h2 class="card-title">Comercios ({{ data()?.tenants?.length || 0 }})</h2>
          <p class="card-help">Podés entrar al panel de cualquier comercio para darle soporte o revisar su cuenta.</p>
        </div>
        <form class="flex items-center gap-2" (ngSubmit)="load()">
          <input
            name="q"
            class="input sm:w-64"
            placeholder="Buscar por nombre, email o CUIT..."
            [(ngModel)]="query"
          />
          <button class="btn btn-secondary" type="submit">Buscar</button>
        </form>
      </div>

      @if (!data() || data()!.tenants.length === 0) {
        <p class="muted px-5 py-8 text-center text-sm">No se encontraron comercios.</p>
      } @else {
        <div class="overflow-x-auto">
          <table class="w-full text-left text-sm">
            <thead class="border-b border-zinc-200 text-xs text-zinc-500 dark:border-zinc-800 dark:text-zinc-400">
              <tr>
                <th class="px-5 py-3 font-medium">Comercio</th>
                <th class="px-4 py-3 font-medium">Plan / Suscripción</th>
                <th class="px-4 py-3 font-medium">Consumo mes</th>
                <th class="px-4 py-3 font-medium">Conexiones</th>
                <th class="px-4 py-3 font-medium">Estado</th>
                <th class="px-5 py-3 text-right font-medium">Acciones</th>
              </tr>
            </thead>
            <tbody class="divide-soft">
              @for (t of data()!.tenants; track t.id) {
                <tr class="hover:bg-zinc-50/60 dark:hover:bg-zinc-800/30">
                  <td class="px-5 py-3.5">
                    <div class="font-medium">{{ t.business_name }}</div>
                    <div class="muted text-xs">
                      {{ t.email || 'Sin email' }}
                      @if (t.cuit) {
                        · CUIT {{ t.cuit }}
                      }
                    </div>
                  </td>
                  <td class="px-4 py-3.5">
                    <div class="font-medium">{{ planName(t.plan_type) }}</div>
                    <div class="muted text-xs">
                      {{ subLabel(t.subscription_status) }}
                      @if (t.plan_expires_at) {
                        · vence {{ t.plan_expires_at | date: 'dd/MM/yy' }}
                      }
                    </div>
                  </td>
                  <td class="px-4 py-3.5 tabular">
                    <div>{{ t.used_quota }} / {{ t.monthly_quota }}</div>
                    <div class="muted text-xs">{{ t.total_validated }} históricos</div>
                  </td>
                  <td class="px-4 py-3.5 text-xs">
                    <div>
                      MP:
                      <span [class]="t.mp_connected ? 'text-emerald-600 dark:text-emerald-400 font-medium' : 'muted'">
                        {{ t.mp_connected ? 'Vinculado' : 'Sin vincular' }}
                      </span>
                    </div>
                    <div class="muted">{{ t.senders_count }} WhatsApp aut.</div>
                  </td>
                  <td class="px-4 py-3.5">
                    <span
                      class="rounded-full px-2 py-0.5 text-xs font-medium"
                      [class]="
                        t.is_active
                          ? 'bg-emerald-100 text-emerald-800 dark:bg-emerald-400/15 dark:text-emerald-300'
                          : 'bg-red-100 text-red-800 dark:bg-red-400/15 dark:text-red-300'
                      "
                    >
                      {{ t.is_active ? 'Activo' : 'Suspendido' }}
                    </span>
                  </td>
                  <td class="px-5 py-3.5 text-right whitespace-nowrap">
                    <div class="inline-flex items-center gap-1.5">
                      <button class="btn btn-secondary btn-sm" type="button" (click)="impersonate(t)">
                        Ver panel
                      </button>
                      <button class="btn btn-ghost btn-sm" type="button" (click)="openEdit(t)">Editar</button>
                      <button
                        class="btn btn-ghost btn-sm"
                        type="button"
                        (click)="toggleActive(t)"
                      >
                        {{ t.is_active ? 'Suspender' : 'Activar' }}
                      </button>
                    </div>
                  </td>
                </tr>
              }
            </tbody>
          </table>
        </div>
      }
    </section>

    @if (data()?.subscription_orders?.length) {
      <section class="card mt-6">
        <div class="card-header">
          <div>
            <h2 class="card-title">Órdenes de suscripción</h2>
            <p class="card-help">
              Pagos de planes iniciados por los comercios. Podés aprobar manualmente transferencias o pagos externos.
            </p>
          </div>
        </div>
        <ul class="divide-soft">
          @for (o of data()!.subscription_orders; track o.id) {
            <li class="flex flex-wrap items-center justify-between gap-3 px-5 py-3 text-sm">
              <div>
                <span class="font-medium">{{ o.business_name || tenantName(o.tenant_id) }}</span>
                <span class="ml-2">→ Plan {{ planName(o.plan_type) }}</span>
                <span class="muted ml-2 text-xs">
                  \${{ o.amount_ars | number: '1.0-0' }} ARS (USD {{ o.price_usd }}) ·
                  {{ o.created_at | date: 'dd/MM/yyyy HH:mm' }}
                </span>
              </div>
              <div class="flex items-center gap-2">
                <span
                  class="rounded-full px-2.5 py-0.5 text-xs font-medium"
                  [class]="
                    o.status === 'APPROVED'
                      ? 'bg-emerald-100 text-emerald-800 dark:bg-emerald-400/15 dark:text-emerald-300'
                      : 'bg-amber-100 text-amber-800 dark:bg-amber-400/15 dark:text-amber-300'
                  "
                >
                  {{ o.status === 'APPROVED' ? 'Aprobada' : 'Pendiente' }}
                </span>
                @if (o.status === 'PENDING') {
                  <button class="btn btn-primary btn-sm" type="button" (click)="approveOrder(o)">
                    Aprobar y activar plan
                  </button>
                }
              </div>
            </li>
          }
        </ul>
      </section>
    }
  `,
})
export class SuperAdminPage implements OnInit {
  private readonly api = inject(ApiService);
  private readonly ui = inject(UiService);
  private readonly router = inject(Router);
  protected readonly store = inject(DashboardStore);

  protected readonly data = signal<SuperAdminData | null>(null);
  protected readonly query = signal('');

  protected readonly showForm = signal(false);
  protected readonly editingId = signal<string | null>(null);
  protected readonly formName = signal('');
  protected readonly formEmail = signal('');
  protected readonly formCuit = signal('');
  protected readonly formPassword = signal('');
  protected readonly formPlan = signal('FREE_TRIAL');
  protected readonly formSubStatus = signal('TRIAL');
  protected readonly formQuota = signal(50);
  protected readonly formActive = signal(true);
  protected readonly formResetQuota = signal(false);

  private readonly tenantMap = computed(() => {
    const map = new Map<string, string>();
    for (const t of this.data()?.tenants ?? []) {
      map.set(t.id, t.business_name);
    }
    return map;
  });

  ngOnInit(): void {
    void this.load();
  }

  protected async load(): Promise<void> {
    try {
      const res = await this.api.superAdminData(this.query());
      this.data.set(res);
    } catch {
      /* el interceptor muestra el error */
    }
  }

  protected planName(code: string): string {
    return this.store.data()?.plans?.[code]?.name || this.data()?.plans?.[code]?.name || code;
  }

  protected subLabel(status: string): string {
    return SUB_LABELS[status] || status || 'Prueba';
  }

  protected tenantName(id: string): string {
    return this.tenantMap().get(id) || id.slice(0, 8);
  }

  protected onPlanChange(planCode: string): void {
    const spec = this.store.data()?.plans?.[planCode] || this.data()?.plans?.[planCode];
    if (spec) {
      this.formQuota.set(spec.monthly_quota);
    }
  }

  protected openCreate(): void {
    this.editingId.set(null);
    this.formName.set('');
    this.formEmail.set('');
    this.formCuit.set('');
    this.formPassword.set('');
    this.formPlan.set('FREE_TRIAL');
    this.formSubStatus.set('TRIAL');
    this.formQuota.set(50);
    this.formActive.set(true);
    this.formResetQuota.set(false);
    this.showForm.set(true);
  }

  protected openEdit(t: TenantAdminSummary): void {
    this.editingId.set(t.id);
    this.formName.set(t.business_name);
    this.formEmail.set(t.email);
    this.formCuit.set(t.cuit);
    this.formPassword.set('');
    this.formPlan.set(t.plan_type);
    this.formSubStatus.set(t.subscription_status || 'TRIAL');
    this.formQuota.set(t.monthly_quota);
    this.formActive.set(t.is_active);
    this.formResetQuota.set(false);
    this.showForm.set(true);
  }

  protected async saveTenant(): Promise<void> {
    const payload: SuperAdminTenantPayload = {
      tenant_id: this.editingId() ?? undefined,
      business_name: this.formName().trim(),
      email: this.formEmail().trim(),
      cuit: this.formCuit().trim(),
      password: this.formPassword() || undefined,
      plan_type: this.formPlan(),
      subscription_status: this.formSubStatus(),
      monthly_quota: Number(this.formQuota()),
      is_active: this.formActive(),
      reset_quota: this.formResetQuota(),
    };
    try {
      await this.api.superAdminSaveTenant(payload);
      this.ui.toast(this.editingId() ? 'Comercio actualizado' : 'Comercio creado');
      this.showForm.set(false);
      await this.load();
      await this.store.refresh();
    } catch {
      /* el interceptor muestra el error */
    }
  }

  protected async toggleActive(t: TenantAdminSummary): Promise<void> {
    try {
      await this.api.superAdminSaveTenant({
        tenant_id: t.id,
        business_name: t.business_name,
        email: t.email,
        cuit: t.cuit,
        plan_type: t.plan_type,
        subscription_status: t.subscription_status || 'TRIAL',
        monthly_quota: t.monthly_quota,
        is_active: !t.is_active,
      });
      this.ui.toast(t.is_active ? 'Comercio suspendido' : 'Comercio reactivado');
      await this.load();
    } catch {
      /* el interceptor muestra el error */
    }
  }

  protected async impersonate(t: TenantAdminSummary): Promise<void> {
    try {
      await this.api.superAdminImpersonate(t.id);
      await this.store.refresh();
      this.ui.toast(`Viendo el panel de ${t.business_name}`);
      await this.router.navigateByUrl('/');
    } catch {
      /* el interceptor muestra el error */
    }
  }

  protected async approveOrder(o: SubscriptionOrder): Promise<void> {
    try {
      await this.api.superAdminApproveSubscription(o.id);
      this.ui.toast('Suscripción aprobada y plan activado');
      await this.load();
      await this.store.refresh();
    } catch {
      /* el interceptor muestra el error */
    }
  }
}
