import { DatePipe, DecimalPipe } from '@angular/common';
import { ChangeDetectionStrategy, Component, computed, effect, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ApiService } from '../../core/api.service';
import { DashboardStore } from '../../core/dashboard.store';
import { Sender, SubscriptionOrder } from '../../core/models';
import { UiService } from '../../core/ui.service';
import { Icon } from '../../shared/icon';

const ROLE: Record<string, string> = { OWNER: 'Dueño/a', CASHIER: 'Cajero/a', DELIVERY: 'Repartidor/a' };
const SUB_STATUS: Record<string, string> = {
  TRIAL: 'Período de prueba',
  ACTIVE: 'Suscripción activa',
  PAST_DUE: 'Pago vencido',
  CANCELLED: 'Cancelada',
};

@Component({
  selector: 'app-settings',
  imports: [FormsModule, Icon, DecimalPipe, DatePipe],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="mb-6">
      <h1 class="text-lg font-semibold tracking-tight">Configuración</h1>
      <p class="muted text-sm">Tu cuenta de cobro, quiénes pueden enviar comprobantes, tu suscripción y seguridad.</p>
    </div>

    <section class="card mb-6 p-5">
      <h2 class="card-title">Datos del comercio</h2>
      <p class="card-help">Se muestran en el panel y se usan para ingresar a tu cuenta.</p>
      <form class="mt-4 grid gap-4 sm:grid-cols-[2fr_1fr_1.5fr_auto] sm:items-end" (ngSubmit)="saveBusiness()">
        <div>
          <label class="label" for="bname">Nombre</label>
          <input
            id="bname"
            name="bname"
            class="input"
            maxlength="150"
            placeholder="Ej: Panadería La Esquina"
            required
            [(ngModel)]="businessName"
          />
        </div>
        <div>
          <label class="label" for="bcuit">CUIT</label>
          <input
            id="bcuit"
            name="bcuit"
            class="input font-mono"
            inputmode="numeric"
            placeholder="30-12345678-9"
            [(ngModel)]="businessCuit"
          />
        </div>
        <div>
          <label class="label" for="bemail">Email</label>
          <input
            id="bemail"
            name="bemail"
            class="input"
            type="email"
            placeholder="hola@micomercio.com"
            [(ngModel)]="businessEmail"
          />
        </div>
        <button class="btn btn-primary" type="submit" [disabled]="!businessName().trim()">Guardar</button>
      </form>
    </section>

    <div class="grid gap-6 lg:grid-cols-2">
      <section class="card p-5">
        <h2 class="card-title">Mercado Pago</h2>
        @if (store.account(); as acc) {
          <p class="card-help">
            <span class="inline-flex items-center gap-1.5"
              ><span class="size-1.5 rounded-full bg-emerald-500"></span> Vinculado</span
            >
            · usuario {{ acc.mp_user_id }}
          </p>
          <a class="btn btn-secondary mt-4" href="/oauth/mp/connect"
            ><app-icon name="link" [size]="14" /> Volver a vincular</a
          >
        } @else {
          <p class="card-help">No vinculado. Sin esto no podemos confirmar los pagos.</p>
          <a class="btn btn-primary mt-4" href="/oauth/mp/connect"
            ><app-icon name="link" [size]="14" /> Vincular Mercado Pago</a
          >
        }
      </section>

      <section class="card p-5">
        <h2 class="card-title">Cuenta para recibir transferencias</h2>
        <p class="card-help">La usamos para comprobar que el dinero llegó a vos y no a otra cuenta.</p>
        <form class="mt-4 space-y-4" (ngSubmit)="saveAccount()">
          <div>
            <label class="label" for="alias">Alias</label>
            <input
              id="alias"
              name="alias"
              class="input"
              placeholder="mi.negocio.mp"
              autocomplete="off"
              [(ngModel)]="alias"
              [disabled]="!store.account()"
            />
          </div>
          <div>
            <label class="label" for="cvu"
              >CVU o CBU <span class="muted font-normal">(22 números)</span></label
            >
            <input
              id="cvu"
              name="cvu"
              class="input font-mono"
              inputmode="numeric"
              placeholder="0000003100000000000000"
              autocomplete="off"
              [(ngModel)]="cvu"
              [disabled]="!store.account()"
            />
          </div>
          <button class="btn btn-primary" type="submit" [disabled]="!store.account()">Guardar</button>
        </form>
      </section>
    </div>

    <section class="card mt-6">
      <div class="card-header">
        <div>
          <h2 class="card-title">Personas autorizadas ({{ store.senders().length }} / {{ maxPhones() }})</h2>
          <p class="card-help">
            Solo estos números de WhatsApp pueden reenviarle comprobantes al bot. No ven tu saldo ni tu cuenta.
          </p>
        </div>
      </div>
      @if (store.senders().length === 0) {
        <p class="muted px-5 py-8 text-center text-sm">Todavía no agregaste a nadie.</p>
      } @else {
        <ul class="divide-soft">
          @for (s of store.senders(); track s.id) {
            <li class="flex items-center gap-3 px-5 py-3">
              <span
                class="flex size-8 shrink-0 items-center justify-center rounded-full bg-zinc-100 text-xs font-semibold text-zinc-600 dark:bg-zinc-800 dark:text-zinc-300"
              >
                {{ (s.employee_name || '?').charAt(0).toUpperCase() }}
              </span>
              <div class="min-w-0 flex-1">
                <div class="truncate text-sm font-medium">{{ s.employee_name }}</div>
                <div class="muted text-xs">+{{ s.phone_number }} · {{ role(s.role) }}</div>
              </div>
              <button
                class="btn btn-ghost btn-sm"
                (click)="removeSender(s)"
                [attr.aria-label]="'Quitar a ' + s.employee_name"
              >
                <app-icon name="trash" [size]="14" /> Quitar
              </button>
            </li>
          }
        </ul>
      }
      <form
        class="grid gap-3 border-t border-zinc-200 px-5 py-4 sm:grid-cols-[1fr_1fr_auto_auto] sm:items-end dark:border-zinc-800"
        (ngSubmit)="addSender()"
      >
        <div>
          <label class="label" for="sname">Nombre</label>
          <input
            id="sname"
            name="sname"
            class="input"
            maxlength="100"
            placeholder="Ej: Marcos"
            required
            [(ngModel)]="newName"
          />
        </div>
        <div>
          <label class="label" for="sphone">WhatsApp <span class="muted font-normal">(con código de país)</span></label>
          <input
            id="sphone"
            name="sphone"
            class="input"
            type="tel"
            placeholder="5491155004444"
            required
            [(ngModel)]="newPhone"
          />
        </div>
        <div>
          <label class="label" for="srole">Qué hace</label>
          <select id="srole" name="srole" class="input" [(ngModel)]="newRole">
            <option value="CASHIER">Cajero/a</option>
            <option value="DELIVERY">Repartidor/a</option>
            <option value="OWNER">Dueño/a (recibe alertas)</option>
          </select>
        </div>
        <button class="btn btn-primary" type="submit" [disabled]="!newName().trim() || !newPhone().trim()">
          <app-icon name="plus" [size]="14" /> Agregar
        </button>
      </form>
    </section>

    <section class="card mt-6 p-5">
      <div class="flex flex-wrap items-start justify-between gap-3">
        <div>
          <div class="flex items-center gap-2">
            <h2 class="card-title">Tu plan y suscripción</h2>
            <span
              class="rounded-full px-2.5 py-0.5 text-xs font-medium"
              [class]="
                store.data()?.tenant?.subscription_status === 'ACTIVE'
                  ? 'bg-emerald-100 text-emerald-800 dark:bg-emerald-400/15 dark:text-emerald-300'
                  : 'bg-amber-100 text-amber-800 dark:bg-amber-400/15 dark:text-amber-300'
              "
            >
              {{ subStatusLabel() }}
            </span>
          </div>
          <p class="card-help">
            {{ quotaText() }}
            @if (store.data()?.tenant?.plan_expires_at; as exp) {
              · Vence el {{ exp | date: 'dd/MM/yyyy' }}
            }
          </p>
        </div>
      </div>

      <div class="mt-3 h-1.5 overflow-hidden rounded-full bg-zinc-100 dark:bg-zinc-800">
        <div
          class="h-full rounded-full bg-zinc-900 transition-all dark:bg-zinc-100"
          [style.width.%]="quotaPct()"
        ></div>
      </div>

      <div class="mt-6 grid gap-4 md:grid-cols-3">
        @for (p of commercialPlans(); track p.plan) {
          <div
            class="flex flex-col justify-between rounded-xl border p-4 transition-colors"
            [class]="
              store.data()?.tenant?.plan_type === p.plan
                ? 'border-zinc-900 bg-zinc-50/60 dark:border-zinc-100 dark:bg-zinc-800/40'
                : 'border-zinc-200 dark:border-zinc-800'
            "
          >
            <div>
              <div class="flex items-center justify-between gap-2">
                <div class="text-sm font-semibold">{{ p.name }}</div>
                @if (store.data()?.tenant?.plan_type === p.plan) {
                  <span
                    class="rounded-md bg-zinc-900 px-2 py-0.5 text-[11px] font-medium text-white dark:bg-zinc-100 dark:text-zinc-900"
                  >
                    Actual
                  </span>
                }
              </div>
              <div class="mt-2 flex items-baseline gap-1.5">
                <span class="text-xl font-semibold tabular">\${{ planPriceArs(p.price_usd) | number: '1.0-0' }}</span>
                <span class="muted text-xs">ARS / mes (USD {{ p.price_usd }})</span>
              </div>
              <ul class="muted mt-3 space-y-1.5 text-xs">
                <li class="flex items-center gap-1.5">
                  <app-icon name="check" [size]="14" /> {{ p.monthly_quota }} comprobantes / mes
                </li>
                <li class="flex items-center gap-1.5">
                  <app-icon name="check" [size]="14" /> Hasta {{ p.max_authorized_phones }} WhatsApp autorizados
                </li>
                @if (p.posnet_virtual) {
                  <li class="flex items-center gap-1.5">
                    <app-icon name="check" [size]="14" /> Posnet virtual y QR de cobro
                  </li>
                }
                @if (p.owner_fraud_alerts) {
                  <li class="flex items-center gap-1.5">
                    <app-icon name="check" [size]="14" /> Alertas de fraude al dueño/a
                  </li>
                }
                @if (p.csv_export) {
                  <li class="flex items-center gap-1.5">
                    <app-icon name="check" [size]="14" /> Exportación de historial CSV
                  </li>
                }
              </ul>
            </div>

            <button
              class="btn mt-4 w-full"
              [class]="store.data()?.tenant?.plan_type === p.plan ? 'btn-secondary' : 'btn-primary'"
              type="button"
              [disabled]="checkoutLoading()"
              (click)="startCheckout(p.plan)"
            >
              {{
                store.data()?.tenant?.plan_type === p.plan
                  ? 'Renovar / Abonar mes'
                  : 'Elegir ' + p.name
              }}
            </button>
          </div>
        }
      </div>

      @if (orders().length > 0) {
        <div class="mt-6 border-t border-zinc-200 pt-4 dark:border-zinc-800">
          <h3 class="text-xs font-semibold tracking-wider uppercase text-zinc-500 dark:text-zinc-400">
            Órdenes de suscripción recientes
          </h3>
          <ul class="divide-soft mt-2">
            @for (o of orders(); track o.id) {
              <li class="flex flex-wrap items-center justify-between gap-3 py-2.5 text-sm">
                <div>
                  <span class="font-medium">Plan {{ planName(o.plan_type) }}</span>
                  <span class="muted ml-2 text-xs">
                    \${{ o.amount_ars | number: '1.0-0' }} ARS · {{ o.created_at | date: 'dd/MM/yyyy HH:mm' }}
                  </span>
                </div>
                <div class="flex items-center gap-2">
                  <span
                    class="rounded-full px-2 py-0.5 text-xs font-medium"
                    [class]="
                      o.status === 'APPROVED'
                        ? 'bg-emerald-100 text-emerald-800 dark:bg-emerald-400/15 dark:text-emerald-300'
                        : 'bg-amber-100 text-amber-800 dark:bg-amber-400/15 dark:text-amber-300'
                    "
                  >
                    {{ o.status === 'APPROVED' ? 'Aprobado' : 'Pendiente de pago' }}
                  </span>
                  @if (o.status === 'PENDING' && o.checkout_url?.startsWith('http')) {
                    <a class="btn btn-secondary btn-sm" [href]="o.checkout_url" target="_blank" rel="noopener">
                      Pagar en Mercado Pago
                    </a>
                  }
                  @if (o.status === 'PENDING' && (store.demo() || store.data()?.is_super_admin)) {
                    <button class="btn btn-primary btn-sm" type="button" (click)="simulatePay(o)">
                      Simular pago aprobado
                    </button>
                  }
                </div>
              </li>
            }
          </ul>
        </div>
      }

      <button
        class="btn btn-ghost btn-sm mt-4 -ml-2.5"
        (click)="advanced.set(!advanced())"
        [attr.aria-expanded]="advanced()"
      >
        <span class="transition-transform" [class.rotate-180]="advanced()"
          ><app-icon name="chevronDown" [size]="14"
        /></span>
        Opciones avanzadas de validación
      </button>
      @if (advanced()) {
        <form class="mt-3 grid gap-4 sm:max-w-xl sm:grid-cols-2" (ngSubmit)="savePlan()">
          @if (store.demo() || store.data()?.is_super_admin) {
            <div>
              <label class="label" for="plan">Plan (cambio directo demo/admin)</label>
              <select id="plan" name="plan" class="input" [(ngModel)]="plan">
                @for (p of allPlans(); track p.plan) {
                  <option [value]="p.plan">{{ p.name }} — {{ p.monthly_quota }}/mes</option>
                }
              </select>
            </div>
          }
          <div>
            <label class="label" for="window"
              >Ventana sin nro. de operación <span class="muted font-normal">(min)</span></label
            >
            <input
              id="window"
              name="window"
              class="input"
              type="number"
              min="5"
              max="1440"
              [(ngModel)]="windowMinutes"
            />
          </div>
          <div class="sm:col-span-2">
            <button class="btn btn-secondary" type="submit">Guardar ventana</button>
          </div>
        </form>
        <p class="muted mt-4 text-xs">{{ systemText() }}</p>
      }
    </section>

    <section class="card mt-6 p-5">
      <h2 class="card-title">Seguridad de la cuenta</h2>
      <p class="card-help">Cambiá la contraseña de acceso web de tu comercio.</p>
      <form class="mt-4 grid gap-4 sm:max-w-xl sm:grid-cols-[1fr_1fr_auto] sm:items-end" (ngSubmit)="updatePassword()">
        <div>
          <label class="label" for="curpass">Contraseña actual</label>
          <input
            id="curpass"
            name="curpass"
            type="password"
            class="input"
            autocomplete="current-password"
            placeholder="Si ya tenías una"
            [(ngModel)]="currentPassword"
          />
        </div>
        <div>
          <label class="label" for="newpass">Nueva contraseña</label>
          <input
            id="newpass"
            name="newpass"
            type="password"
            class="input"
            minlength="8"
            autocomplete="new-password"
            placeholder="Mínimo 8 caracteres"
            required
            [(ngModel)]="newPassword"
          />
        </div>
        <button class="btn btn-secondary" type="submit" [disabled]="newPassword().length < 8">
          Actualizar contraseña
        </button>
      </form>
    </section>
  `,
})
export class SettingsPage {
  private readonly api = inject(ApiService);
  private readonly ui = inject(UiService);
  protected readonly store = inject(DashboardStore);

  protected readonly businessName = signal('');
  protected readonly businessCuit = signal('');
  protected readonly businessEmail = signal('');
  protected readonly alias = signal('');
  protected readonly cvu = signal('');
  protected readonly newName = signal('');
  protected readonly newPhone = signal('');
  protected readonly newRole = signal('CASHIER');
  protected readonly plan = signal('');
  protected readonly windowMinutes = signal(120);
  protected readonly advanced = signal(false);
  protected readonly checkoutLoading = signal(false);
  protected readonly currentPassword = signal('');
  protected readonly newPassword = signal('');
  private loaded = false;

  protected readonly allPlans = computed(() =>
    Object.values(this.store.data()?.plans ?? {}).sort((a, b) => a.monthly_quota - b.monthly_quota),
  );
  protected readonly commercialPlans = computed(() =>
    this.allPlans().filter((p) => p.plan !== 'FREE_TRIAL'),
  );
  protected readonly orders = computed(() => this.store.data()?.subscription_orders ?? []);
  protected readonly maxPhones = computed(() => {
    const d = this.store.data();
    const t = d?.tenant;
    if (!d || !t) return 2;
    return d.plans?.[t.plan_type]?.max_authorized_phones || 2;
  });
  protected readonly subStatusLabel = computed(() => {
    const st = this.store.data()?.tenant?.subscription_status || 'TRIAL';
    return SUB_STATUS[st] || st;
  });
  protected readonly quotaPct = computed(() => {
    const t = this.store.data()?.tenant;
    return t?.monthly_quota ? Math.min(100, Math.round((t.used_quota / t.monthly_quota) * 100)) : 0;
  });
  protected readonly quotaText = computed(() => {
    const d = this.store.data();
    const t = d?.tenant;
    if (!t) return '';
    const name = d.plans?.[t.plan_type]?.name || t.plan_type;
    return `Plan ${name}: usaste ${t.used_quota} de ${t.monthly_quota} comprobantes este mes`;
  });
  protected readonly systemText = computed(() => {
    const s = this.store.data()?.system;
    if (!s) return '';
    return [
      'Base de datos: ' + (s.mysql_connected ? 'conectada' : 'en memoria (se borra al reiniciar)'),
      'Cola de espera: ' + (s.redis_connected ? 'conectada' : 'en memoria'),
      'Lectura de comprobantes: ' + (s.gemini_live_key ? 'activa' : 'sin configurar'),
    ].join(' · ');
  });

  constructor() {
    effect(() => {
      const d = this.store.data();
      if (!d || this.loaded) return;
      this.loaded = true;
      this.businessName.set(d.tenant?.business_name ?? '');
      this.businessCuit.set(d.tenant?.cuit ?? '');
      this.businessEmail.set(d.tenant?.email ?? '');
      this.alias.set(d.mp_account?.expected_alias ?? '');
      this.cvu.set(d.mp_account?.expected_cvu ?? '');
      this.plan.set(d.tenant?.plan_type ?? '');
      this.windowMinutes.set(d.tenant?.time_window_minutes || 120);
    });
  }

  protected role(r: string): string {
    return ROLE[r] ?? r;
  }

  protected planName(code: string): string {
    return this.store.data()?.plans?.[code]?.name || code;
  }

  protected planPriceArs(usd: number): number {
    const rate = this.store.data()?.usd_ars_rate || 1200;
    return usd * rate;
  }

  protected async saveBusiness(): Promise<void> {
    try {
      await this.api.saveBusiness(this.businessName().trim(), this.businessCuit().trim(), this.businessEmail().trim());
      this.ui.toast('Datos del comercio guardados');
      await this.store.refresh();
    } catch {
      /* el interceptor muestra el error */
    }
  }

  protected async saveAccount(): Promise<void> {
    try {
      await this.api.saveAccount(this.alias().trim().toLowerCase(), this.cvu().replace(/\D/g, ''));
      this.ui.toast('Cuenta guardada');
      await this.store.refresh();
    } catch {
      /* el interceptor muestra el error */
    }
  }

  protected async addSender(): Promise<void> {
    try {
      await this.api.addSender(this.newName().trim(), this.newPhone(), this.newRole());
      this.newName.set('');
      this.newPhone.set('');
      this.ui.toast('Persona agregada');
      await this.store.refresh();
    } catch {
      /* el interceptor muestra el error */
    }
  }

  protected async removeSender(s: Sender): Promise<void> {
    const ok = await this.ui.confirm({
      title: 'Quitar persona',
      message: `${s.employee_name} ya no va a poder enviar comprobantes.`,
      confirmText: 'Quitar',
      danger: true,
    });
    if (!ok) return;
    try {
      await this.api.deleteSender(s.id);
      this.ui.toast('Persona quitada');
      await this.store.refresh();
    } catch {
      /* el interceptor muestra el error */
    }
  }

  protected async startCheckout(planCode: string): Promise<void> {
    this.checkoutLoading.set(true);
    try {
      const res = await this.api.checkoutPlan(planCode);
      await this.store.refresh();
      if (res.checkout_url && res.checkout_url.startsWith('http')) {
        location.href = res.checkout_url;
        return;
      }
      this.ui.toast('Orden de suscripción generada. Podés confirmar el pago abajo.');
    } catch {
      /* el interceptor muestra el error */
    } finally {
      this.checkoutLoading.set(false);
    }
  }

  protected async simulatePay(order: SubscriptionOrder): Promise<void> {
    try {
      await this.api.simulateSubscriptionPay(order.id);
      this.ui.toast(`Suscripción al plan ${this.planName(order.plan_type)} activada`);
      await this.store.refresh();
      this.plan.set(this.store.data()?.tenant?.plan_type ?? '');
    } catch {
      /* el interceptor muestra el error */
    }
  }

  protected async savePlan(): Promise<void> {
    try {
      await this.api.savePlan(this.plan(), Number(this.windowMinutes()));
      this.ui.toast('Guardado');
      await this.store.refresh();
    } catch {
      /* el interceptor muestra el error */
    }
  }

  protected async updatePassword(): Promise<void> {
    try {
      await this.api.changePassword(this.currentPassword(), this.newPassword());
      this.currentPassword.set('');
      this.newPassword.set('');
      this.ui.toast('Contraseña actualizada');
    } catch {
      /* el interceptor muestra el error */
    }
  }
}
