import { ChangeDetectionStrategy, Component, computed, effect, inject, input, signal, untracked } from '@angular/core';
import { RouterLink } from '@angular/router';
import { ApiService } from '../../core/api.service';
import { DashboardStore } from '../../core/dashboard.store';
import { MoneyPipe, WhenPipe } from '../../core/format';
import { Customer } from '../../core/models';
import { Icon } from '../../shared/icon';
import { HistoryCard } from '../home/history-card';

/** Un cliente: sus datos, totales y todos sus comprobantes. */
@Component({
  selector: 'app-customer-detail',
  imports: [RouterLink, Icon, HistoryCard, WhenPipe],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <a routerLink="/clientes" class="btn btn-ghost btn-sm -ml-2.5 mb-4">
      <app-icon name="chevronLeft" [size]="14" /> Clientes
    </a>

    @if (customer(); as c) {
      <div class="mb-6 flex flex-wrap items-start justify-between gap-4">
        <div>
          <h1 class="text-lg font-semibold tracking-tight">{{ c.name || 'Cliente sin nombre' }}</h1>
          <p class="muted text-sm">
            {{ c.cuit ? 'CUIT ' + c.cuit : 'Sin CUIT' }}
            @if (c.email) {
              · {{ c.email }}
            }
            · cliente desde {{ c.first_seen_at | when }}
          </p>
        </div>
        @if (c.fraud) {
          <div class="rounded-lg border border-red-200 bg-red-50 px-3 py-2 text-sm text-red-800 dark:border-red-400/20 dark:bg-red-400/10 dark:text-red-200">
            {{ c.fraud }} intento{{ c.fraud === 1 ? '' : 's' }} de fraude
            @if (c.last_fraud_at) {
              <span class="opacity-75">· último {{ c.last_fraud_at | when }}</span>
            }
          </div>
        }
      </div>

      <div class="mb-6 grid grid-cols-2 gap-3 lg:grid-cols-4">
        @for (k of stats(); track k.label) {
          <div class="card px-4 py-3.5">
            <div class="muted flex items-center gap-1.5 text-xs">
              <span class="size-1.5 rounded-full" [class]="k.dot"></span>{{ k.label }}
            </div>
            <div class="tabular mt-1 text-xl font-semibold tracking-tight">{{ k.value }}</div>
          </div>
        }
      </div>

      <app-history-card [customerId]="c.id" title="Comprobantes del cliente" subtitle="Todo lo que envió este cliente." />
    } @else if (notFound()) {
      <div class="card px-5 py-12 text-center">
        <p class="text-sm font-medium">Este cliente no existe</p>
        <p class="muted mt-1 text-sm">Puede que la base se haya limpiado.</p>
      </div>
    } @else {
      <p class="muted py-12 text-center text-sm">Cargando…</p>
    }
  `,
})
export class CustomerDetailPage {
  private readonly api = inject(ApiService);
  private readonly store = inject(DashboardStore);
  private readonly money = new MoneyPipe();

  /** Viene de la ruta /clientes/:id */
  readonly id = input.required<string>();

  protected readonly customer = signal<Customer | null>(null);
  protected readonly notFound = signal(false);

  protected readonly stats = computed(() => {
    const c = this.customer();
    if (!c) return [];
    return [
      { label: 'Cobrado (comprobantes correctos)', value: this.money.transform(c.verified_amount), dot: 'bg-emerald-500' },
      { label: 'Comprobantes correctos', value: `${c.verified} de ${c.receipts}`, dot: 'bg-emerald-500' },
      { label: 'Fraudes', value: String(c.fraud), dot: 'bg-red-500' },
      { label: 'Pagos recibidos en MP', value: String(c.payments), dot: 'bg-zinc-400' },
    ];
  });

  constructor() {
    effect(() => {
      const id = this.id();
      this.store.tick();
      untracked(() => this.load(id));
    });
  }

  private async load(id: string): Promise<void> {
    try {
      this.customer.set(await this.api.customer(id));
      this.notFound.set(false);
    } catch {
      if (!this.customer()) this.notFound.set(true);
    }
  }
}
