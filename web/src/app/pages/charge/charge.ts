import { ChangeDetectionStrategy, Component, computed, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ApiService } from '../../core/api.service';
import { DashboardStore } from '../../core/dashboard.store';
import { HourPipe, MoneyPipe } from '../../core/format';
import { ChargeOrder } from '../../core/models';
import { UiService } from '../../core/ui.service';
import { Icon } from '../../shared/icon';

@Component({
  selector: 'app-charge',
  imports: [FormsModule, Icon, MoneyPipe, HourPipe],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="mb-6">
      <h1 class="text-lg font-semibold tracking-tight">Cobrar</h1>
      <p class="muted text-sm">Generá un monto único para reconocer al instante la transferencia de ese cliente.</p>
    </div>

    <div class="grid gap-6 lg:grid-cols-2">
      <section class="card p-5">
        <h2 class="card-title">Nuevo cobro</h2>
        <p class="card-help">Le sumamos unos centavos al monto para que sea único.</p>
        <form class="mt-4 space-y-4" (ngSubmit)="create()">
          <div>
            <label class="label" for="amount">Monto a cobrar</label>
            <input id="amount" name="amount" class="input" type="number" step="0.01" min="1" inputmode="decimal" placeholder="10000" required [(ngModel)]="amount" />
          </div>
          <div>
            <label class="label" for="ref">Descripción <span class="muted font-normal">(opcional)</span></label>
            <input id="ref" name="ref" class="input" maxlength="100" placeholder="Ej: Mesa 4" [(ngModel)]="reference" />
          </div>
          <button class="btn btn-primary w-full" type="submit" [disabled]="!amount() || busy()">Generar monto</button>
        </form>

        @if (last(); as o) {
          <div class="mt-5 rounded-lg border border-zinc-200 p-5 text-center dark:border-zinc-800">
            <div class="muted text-sm">Pedile al cliente que transfiera exactamente</div>
            <div class="tabular my-2 text-4xl font-semibold tracking-tight">{{ o.unique_amount | money }}</div>
            <div class="muted text-sm">a <span class="font-medium text-zinc-900 dark:text-zinc-100">{{ o.alias_or_cvu || 'tu cuenta' }}</span></div>
            <div class="mt-4 flex justify-center gap-2">
              <button class="btn btn-secondary btn-sm" (click)="copy(o)"><app-icon name="copy" [size]="14" /> Copiar monto</button>
              @if (store.demo()) {
                <button class="btn btn-secondary btn-sm" (click)="simulate(o)">Simular pago</button>
              }
            </div>
          </div>
        }
      </section>

      <section class="card">
        <div class="card-header">
          <div>
            <h2 class="card-title">Cobros de hoy</h2>
            <p class="card-help">Cuando el cliente paga, el estado cambia solo.</p>
          </div>
        </div>
        @if (orders().length === 0) {
          <p class="muted px-5 py-10 text-center text-sm">
            No generaste cobros todavía. También podés usar <span class="font-mono">/cobrar 15000</span> desde WhatsApp.
          </p>
        } @else {
          <ul class="divide-soft">
            @for (o of orders(); track o.id) {
              <li class="flex items-center gap-4 px-5 py-3">
                <div class="min-w-0 flex-1">
                  <div class="truncate text-sm font-medium">{{ o.reference || 'Cobro' }}</div>
                  <div class="muted text-xs">{{ o.created_at | hour }}</div>
                </div>
                <span class="text-xs" [class]="o.status === 'PAID' ? 'text-emerald-700 dark:text-emerald-300' : 'muted'">
                  {{ o.status === 'PAID' ? 'Pagado' : 'Esperando' }}
                </span>
                <span class="tabular w-28 text-right text-sm font-medium">{{ o.unique_amount | money }}</span>
              </li>
            }
          </ul>
        }
      </section>
    </div>
  `,
})
export class ChargePage {
  private readonly api = inject(ApiService);
  private readonly ui = inject(UiService);
  protected readonly store = inject(DashboardStore);

  protected readonly amount = signal<number | null>(null);
  protected readonly reference = signal('');
  protected readonly busy = signal(false);
  protected readonly last = signal<ChargeOrder | null>(null);
  protected readonly orders = computed(() =>
    [...(this.store.data()?.charge_orders ?? [])].sort((a, b) => +new Date(b.created_at) - +new Date(a.created_at)),
  );

  protected async create(): Promise<void> {
    const amount = this.amount();
    if (!amount) return;
    this.busy.set(true);
    try {
      this.last.set(await this.api.createCharge(Number(amount), this.reference().trim()));
      this.reference.set('');
      await this.store.refresh();
    } catch {
      /* el interceptor muestra el error */
    } finally {
      this.busy.set(false);
    }
  }

  protected async copy(o: ChargeOrder): Promise<void> {
    try {
      await navigator.clipboard.writeText(Number(o.unique_amount).toFixed(2).replace('.', ','));
      this.ui.toast('Monto copiado');
    } catch {
      this.ui.toast('No se pudo copiar');
    }
  }

  protected async simulate(o: ChargeOrder): Promise<void> {
    await this.api.simulatePayment(Number(o.unique_amount));
    this.ui.toast('Pago simulado');
    await this.store.refresh();
  }
}
