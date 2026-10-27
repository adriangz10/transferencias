import { ChangeDetectionStrategy, Component, computed, inject, signal } from '@angular/core';
import { DashboardStore } from '../../core/dashboard.store';
import { MoneyPipe, WhenPipe } from '../../core/format';
import { Pagination } from '../../shared/pagination';

/** Pagos que entraron a Mercado Pago (los más recientes), con paginación. */
@Component({
  selector: 'app-payments-card',
  host: { class: 'block' },
  imports: [Pagination, MoneyPipe, WhenPipe],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <section class="card">
      <div class="card-header">
        <div>
          <h2 class="card-title">Pagos recibidos en Mercado Pago</h2>
          <p class="card-help">Los últimos ingresos a tu cuenta y si ya tienen comprobante.</p>
        </div>
      </div>
      @if (all().length === 0) {
        <p class="muted px-5 py-10 text-center text-sm">Todavía no entraron pagos.</p>
      } @else {
        <ul class="divide-soft">
          @for (p of shown(); track p.id) {
            <li class="flex items-center gap-4 px-5 py-3">
              <div class="min-w-0 flex-1">
                <div class="truncate text-sm font-medium">{{ p.payer_name || (p.payer_cuit ? 'CUIT ' + p.payer_cuit : 'Pagador sin nombre') }}</div>
                <div class="muted text-xs">{{ p.payment_date | when }} · #{{ p.mp_payment_id }}</div>
              </div>
              <span
                class="text-xs"
                [class]="p.reconciliation_status === 'CONSUMED' ? 'text-zinc-500 dark:text-zinc-400' : 'font-medium text-amber-700 dark:text-amber-300'"
              >
                {{ p.reconciliation_status === 'CONSUMED' ? 'Con comprobante' : 'Sin comprobante' }}
              </span>
              <span class="tabular w-28 text-right text-sm font-medium">{{ p.amount | money }}</span>
            </li>
          }
        </ul>
        <div class="border-t border-zinc-200 px-5 py-3 dark:border-zinc-800">
          <app-pagination
            [page]="page()"
            [pageSize]="pageSize()"
            [total]="all().length"
            [pageSizes]="[10, 25, 50]"
            (pageChange)="page.set($event)"
            (pageSizeChange)="pageSize.set($event); page.set(1)"
          />
        </div>
      }
    </section>
  `,
})
export class PaymentsCard {
  private readonly store = inject(DashboardStore);
  protected readonly page = signal(1);
  protected readonly pageSize = signal(10);
  protected readonly all = computed(() => this.store.data()?.payments ?? []);
  protected readonly shown = computed(() => {
    const start = (this.page() - 1) * this.pageSize();
    return this.all().slice(start, start + this.pageSize());
  });
}
