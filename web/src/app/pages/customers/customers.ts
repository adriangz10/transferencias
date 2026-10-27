import { ChangeDetectionStrategy, Component, computed, effect, inject, signal, untracked } from '@angular/core';
import { RouterLink } from '@angular/router';
import { ApiService } from '../../core/api.service';
import { DashboardStore } from '../../core/dashboard.store';
import { MoneyPipe, WhenPipe } from '../../core/format';
import { Customer } from '../../core/models';
import { Icon } from '../../shared/icon';
import { Pagination } from '../../shared/pagination';

/** Clientes: quienes le transfirieron al comercio, con su historial resumido. */
@Component({
  selector: 'app-customers',
  imports: [RouterLink, Icon, Pagination, MoneyPipe, WhenPipe],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="mb-6">
      <h1 class="text-lg font-semibold tracking-tight">Clientes</h1>
      <p class="muted text-sm">Cada persona que te transfirió, con su historial. Se arma solo a partir de los comprobantes y los pagos.</p>
    </div>

    <section class="card">
      <div class="flex flex-wrap items-center justify-between gap-3 border-b border-zinc-200 px-5 py-3 dark:border-zinc-800">
        <input
          type="search"
          class="input max-w-xs py-1.5"
          placeholder="Buscar por nombre o CUIT"
          aria-label="Buscar clientes"
          [value]="query()"
          (input)="onSearch($any($event.target).value)"
        />
        <div class="flex gap-1" role="tablist" aria-label="Filtrar clientes">
          @for (opt of filters; track opt.value) {
            <button
              role="tab"
              class="rounded-md px-2.5 py-1 text-xs font-medium transition-colors"
              [class]="
                onlyFraud() === opt.value
                  ? 'bg-zinc-900 text-white dark:bg-zinc-100 dark:text-zinc-900'
                  : 'text-zinc-500 hover:bg-zinc-100 hover:text-zinc-900 dark:text-zinc-400 dark:hover:bg-zinc-800 dark:hover:text-zinc-100'
              "
              [attr.aria-selected]="onlyFraud() === opt.value"
              (click)="onlyFraud.set(opt.value); page.set(1)"
            >
              {{ opt.label }}
            </button>
          }
        </div>
      </div>

      @if (loading() && !result()) {
        <p class="muted px-5 py-12 text-center text-sm">Cargando…</p>
      } @else if (items().length === 0) {
        <div class="px-5 py-12 text-center">
          <p class="text-sm font-medium">{{ query() || onlyFraud() ? 'No hay clientes con estos filtros' : 'Todavía no hay clientes' }}</p>
          <p class="muted mt-1 text-sm">
            {{ query() || onlyFraud() ? 'Probá con otra búsqueda.' : 'Aparecen solos cuando llegan comprobantes o pagos.' }}
          </p>
        </div>
      } @else {
        <ul class="divide-soft">
          @for (c of items(); track c.id) {
            <li>
              <a
                [routerLink]="['/clientes', c.id]"
                class="flex items-center gap-4 px-5 py-3 transition-colors hover:bg-zinc-50 dark:hover:bg-zinc-800/40"
              >
                <span
                  class="flex size-8 shrink-0 items-center justify-center rounded-full bg-zinc-100 text-xs font-semibold text-zinc-600 dark:bg-zinc-800 dark:text-zinc-300"
                >
                  {{ initial(c) }}
                </span>
                <div class="min-w-0 flex-1">
                  <div class="flex items-center gap-2">
                    <span class="truncate text-sm font-medium">{{ c.name || 'Sin nombre' }}</span>
                    @if (c.fraud) {
                      <span class="shrink-0 rounded px-1.5 py-px text-[11px] font-medium text-red-700 ring-1 ring-red-600/20 ring-inset dark:text-red-300 dark:ring-red-400/30">
                        {{ c.fraud }} fraude{{ c.fraud === 1 ? '' : 's' }}
                      </span>
                    }
                  </div>
                  <div class="muted truncate text-xs">
                    {{ c.cuit ? 'CUIT ' + c.cuit : 'Sin CUIT' }} · visto {{ c.last_seen_at | when }}
                  </div>
                </div>
                <div class="hidden text-right text-xs sm:block">
                  <div class="tabular text-sm font-medium">{{ c.verified_amount | money }}</div>
                  <div class="muted">{{ c.verified }} de {{ c.receipts }} correcta{{ c.receipts === 1 ? '' : 's' }}</div>
                </div>
                <span class="muted"><app-icon name="chevronRight" /></span>
              </a>
            </li>
          }
        </ul>
      }

      @if (total() > 0) {
        <div class="border-t border-zinc-200 px-5 py-3 dark:border-zinc-800">
          <app-pagination
            [page]="page()"
            [pageSize]="pageSize()"
            [total]="total()"
            (pageChange)="page.set($event)"
            (pageSizeChange)="pageSize.set($event); page.set(1)"
          />
        </div>
      }
    </section>
  `,
})
export class CustomersPage {
  private readonly api = inject(ApiService);
  private readonly store = inject(DashboardStore);

  protected readonly filters = [
    { value: false, label: 'Todos' },
    { value: true, label: 'Con fraudes' },
  ];
  protected readonly query = signal('');
  protected readonly onlyFraud = signal(false);
  protected readonly page = signal(1);
  protected readonly pageSize = signal(20);
  protected readonly loading = signal(false);
  protected readonly result = signal<{ items: Customer[]; total: number } | null>(null);
  protected readonly items = computed(() => this.result()?.items ?? []);
  protected readonly total = computed(() => this.result()?.total ?? 0);

  private searchTimer?: ReturnType<typeof setTimeout>;
  private seq = 0;

  constructor() {
    effect(() => {
      const q = { page: this.page(), pageSize: this.pageSize(), query: this.query(), onlyFraud: this.onlyFraud() };
      this.store.tick(); // se actualiza con el refresco automático del panel
      untracked(() => this.load(q));
    });
  }

  protected onSearch(value: string): void {
    clearTimeout(this.searchTimer);
    this.searchTimer = setTimeout(() => {
      this.query.set(value);
      this.page.set(1);
    }, 250);
  }

  protected initial(c: Customer): string {
    return (c.name || c.cuit || '?').charAt(0).toUpperCase();
  }

  private async load(q: { page: number; pageSize: number; query: string; onlyFraud: boolean }): Promise<void> {
    const seq = ++this.seq;
    this.loading.set(true);
    try {
      const res = await this.api.customers(q);
      if (seq === this.seq) this.result.set({ items: res.items ?? [], total: res.total });
    } catch {
      /* el interceptor muestra el error */
    } finally {
      if (seq === this.seq) this.loading.set(false);
    }
  }
}
