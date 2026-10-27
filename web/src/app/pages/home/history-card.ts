import { ChangeDetectionStrategy, Component, computed, effect, inject, input, signal, untracked } from '@angular/core';
import { RouterLink } from '@angular/router';
import { ApiService } from '../../core/api.service';
import { DashboardStore } from '../../core/dashboard.store';
import { isoDay, MoneyPipe, WhenPipe } from '../../core/format';
import { Validation } from '../../core/models';
import { statusInfo } from '../../core/status';
import { UiService } from '../../core/ui.service';
import { Icon } from '../../shared/icon';
import { Pagination } from '../../shared/pagination';
import { StatusBadge } from '../../shared/status-badge';

const CATEGORIES = [
  { value: 'all', label: 'Todas' },
  { value: 'ok', label: 'Correctas' },
  { value: 'fraud', label: 'Fraude' },
  { value: 'rejected', label: 'Rechazadas' },
  { value: 'not_found', label: 'No ingresó' },
];

/** Historial de comprobantes: filtros por fecha y resultado, paginación, detalle y borrado. */
@Component({
  selector: 'app-history-card',
  host: { class: 'block' },
  imports: [RouterLink, Icon, Pagination, StatusBadge, MoneyPipe, WhenPipe],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <section class="card">
      <div class="card-header">
        <div>
          <h2 class="card-title">{{ title() }}</h2>
          <p class="card-help">{{ subtitle() }}</p>
        </div>
        <a class="btn btn-secondary btn-sm" [href]="exportUrl()" download>
          <app-icon name="download" [size]="14" /> Descargar Excel
        </a>
      </div>

      <!-- Filtros -->
      <div class="flex flex-wrap items-center justify-between gap-x-4 gap-y-2 border-b border-zinc-200 px-5 py-3 dark:border-zinc-800">
        <div class="flex flex-wrap gap-1" role="tablist" aria-label="Filtrar por resultado">
          @for (c of categories; track c.value) {
            <button
              role="tab"
              class="rounded-md px-2.5 py-1 text-xs font-medium transition-colors"
              [class]="
                category() === c.value
                  ? 'bg-zinc-900 text-white dark:bg-zinc-100 dark:text-zinc-900'
                  : 'text-zinc-500 hover:bg-zinc-100 hover:text-zinc-900 dark:text-zinc-400 dark:hover:bg-zinc-800 dark:hover:text-zinc-100'
              "
              [attr.aria-selected]="category() === c.value"
              (click)="setCategory(c.value)"
            >
              {{ c.label }}
            </button>
          }
        </div>

        <div class="flex flex-wrap items-center gap-2">
          @for (q of quickDays; track q.value) {
            <button
              class="btn btn-sm"
              [class]="date() === q.value ? 'bg-zinc-100 text-zinc-900 dark:bg-zinc-800 dark:text-zinc-100' : 'btn-ghost'"
              (click)="setDate(q.value)"
            >
              {{ q.label }}
            </button>
          }
          <label class="relative flex items-center">
            <span class="sr-only">Fecha</span>
            <input
              type="date"
              class="input w-40 py-1.5 text-xs"
              [value]="date()"
              [max]="today"
              (change)="setDate($any($event.target).value)"
            />
          </label>
          @if (date() || category() !== 'all') {
            <button class="btn btn-ghost btn-sm" (click)="clearFilters()">
              <app-icon name="x" [size]="14" /> Limpiar
            </button>
          }
        </div>
      </div>

      <!-- Lista -->
      <div class="relative">
        @if (loading() && !page()) {
          <div class="muted px-5 py-12 text-center text-sm">Cargando…</div>
        } @else if (items().length === 0) {
          <div class="px-5 py-12 text-center">
            <p class="text-sm font-medium">{{ hasFilters() ? 'No hay comprobantes con estos filtros' : 'Todavía no hay comprobantes' }}</p>
            <p class="muted mt-1 text-sm">
              {{ hasFilters() ? 'Probá con otra fecha o resultado.' : 'Cuando alguien reenvíe uno por WhatsApp, aparece acá.' }}
            </p>
          </div>
        } @else {
          <ul class="divide-soft">
            @for (v of items(); track v.id) {
              <li>
                <button
                  class="flex w-full items-center gap-4 px-5 py-3 text-left transition-colors hover:bg-zinc-50 dark:hover:bg-zinc-800/40"
                  [attr.aria-expanded]="expanded() === v.id"
                  (click)="toggle(v.id)"
                >
                  <div class="min-w-0 flex-1">
                    <div class="flex items-center gap-2">
                      <span class="truncate text-sm font-medium">{{ v.extracted_sender_name || v.customer?.name || 'Cliente sin nombre' }}</span>
                      @if (!customerId() && v.customer?.fraud) {
                        <span
                          class="shrink-0 rounded px-1.5 py-px text-[11px] font-medium text-red-700 ring-1 ring-red-600/20 ring-inset dark:text-red-300 dark:ring-red-400/30"
                          [title]="'Este cliente tiene ' + v.customer!.fraud + ' intento(s) de fraude registrados'"
                        >
                          {{ v.customer!.fraud }} fraude{{ v.customer!.fraud === 1 ? '' : 's' }}
                        </span>
                      }
                    </div>
                    <div class="muted truncate text-xs">
                      {{ v.created_at | when }}
                      @if (v.extracted_source_bank) {
                        · {{ v.extracted_source_bank }}
                      }
                      · {{ store.senderLabel(v.sender_phone) }}
                    </div>
                  </div>
                  <div class="hidden sm:block">
                    <app-status-badge [status]="v.validation_result" [pending]="isPending(v)" />
                  </div>
                  <div class="flex flex-col items-end gap-1">
                    <span class="tabular text-sm font-medium">{{ v.extracted_amount | money }}</span>
                    <span class="sm:hidden">
                      <app-status-badge [status]="v.validation_result" [pending]="isPending(v)" />
                    </span>
                  </div>
                  <span class="muted transition-transform" [class.rotate-180]="expanded() === v.id">
                    <app-icon name="chevronDown" />
                  </span>
                </button>

                @if (expanded() === v.id) {
                  <div class="border-t border-zinc-100 bg-zinc-50/60 px-5 py-4 dark:border-zinc-800 dark:bg-zinc-950/40">
                    <p class="text-sm">
                      <span class="font-medium">{{ isPending(v) ? 'Esperando que entre el pago.' : info(v).label + '.' }}</span>
                      <span class="muted"> {{ isPending(v) ? 'Si se acredita en los próximos minutos, se avisa solo por WhatsApp.' : info(v).detail }}</span>
                    </p>

                    <dl class="mt-4 grid grid-cols-1 gap-x-6 gap-y-3 text-sm sm:grid-cols-2 lg:grid-cols-3">
                      @for (f of fields(v); track f.label) {
                        <div class="min-w-0">
                          <dt class="muted text-xs">{{ f.label }}</dt>
                          <dd class="truncate" [class]="f.mono ? 'font-mono text-[13px]' : ''" [title]="f.value">{{ f.value }}</dd>
                        </div>
                      }
                    </dl>

                    @if (v.ai_fraud_warnings?.length) {
                      <div class="mt-4 rounded-lg border border-zinc-200 bg-white px-3 py-2 text-sm dark:border-zinc-800 dark:bg-zinc-900">
                        <div class="muted mb-1 text-xs">Observaciones del análisis</div>
                        <ul class="list-disc space-y-0.5 pl-4">
                          @for (w of v.ai_fraud_warnings; track $index) {
                            <li>{{ w }}</li>
                          }
                        </ul>
                      </div>
                    }

                    <div class="mt-4 flex flex-wrap items-center justify-between gap-2">
                      @if (v.customer && !customerId()) {
                        <a class="btn btn-ghost btn-sm -ml-2.5" [routerLink]="['/clientes', v.customer.id]">
                          Ver historial del cliente ({{ v.customer.receipts }} comprobante{{ v.customer.receipts === 1 ? '' : 's' }})
                          <app-icon name="chevronRight" [size]="14" />
                        </a>
                      } @else {
                        <span></span>
                      }
                      <button class="btn btn-secondary btn-sm text-red-600 dark:text-red-400" (click)="remove(v)" [disabled]="deleting() === v.id">
                        <app-icon name="trash" [size]="14" />
                        {{ deleting() === v.id ? 'Eliminando…' : 'Eliminar' }}
                      </button>
                    </div>
                  </div>
                }
              </li>
            }
          </ul>
        }
      </div>

      @if (total() > 0) {
        <div class="border-t border-zinc-200 px-5 py-3 dark:border-zinc-800">
          <app-pagination
            [page]="pageNum()"
            [pageSize]="pageSize()"
            [total]="total()"
            (pageChange)="goTo($event)"
            (pageSizeChange)="setPageSize($event)"
          />
        </div>
      }
    </section>
  `,
})
export class HistoryCard {
  private readonly api = inject(ApiService);
  private readonly ui = inject(UiService);
  protected readonly store = inject(DashboardStore);

  /** Si se indica, muestra solo los comprobantes de ese cliente */
  readonly customerId = input<string>('');
  readonly title = input('Comprobantes');
  readonly subtitle = input('Todo lo que se verificó, por WhatsApp o desde el panel.');

  protected readonly categories = CATEGORIES;
  protected readonly today = isoDay(new Date());
  protected readonly yesterday = isoDay(new Date(Date.now() - 86_400_000));
  protected readonly quickDays = [
    { value: this.today, label: 'Hoy' },
    { value: this.yesterday, label: 'Ayer' },
  ];

  protected readonly pageNum = signal(1);
  protected readonly pageSize = signal(20);
  protected readonly date = signal('');
  protected readonly category = signal('all');

  protected readonly page = signal<{ items: Validation[]; total: number } | null>(null);
  protected readonly loading = signal(false);
  protected readonly expanded = signal<string | null>(null);
  protected readonly deleting = signal<string | null>(null);

  protected readonly items = computed(() => this.page()?.items ?? []);
  protected readonly total = computed(() => this.page()?.total ?? 0);
  protected readonly hasFilters = computed(() => !!this.date() || this.category() !== 'all');
  protected readonly exportUrl = computed(() =>
    this.api.exportUrl({ date: this.date(), category: this.category(), customerId: this.customerId() }),
  );

  private requestSeq = 0;

  constructor() {
    // Recarga al cambiar filtros/página y en cada refresco automático del panel
    effect(() => {
      const q = {
        page: this.pageNum(),
        pageSize: this.pageSize(),
        date: this.date(),
        category: this.category(),
        customerId: this.customerId(),
      };
      this.store.tick();
      untracked(() => this.load(q));
    });
  }

  private async load(q: { page: number; pageSize: number; date: string; category: string; customerId: string }): Promise<void> {
    const seq = ++this.requestSeq;
    this.loading.set(true);
    try {
      const res = await this.api.validations(q);
      if (seq !== this.requestSeq) return; // llegó una respuesta más nueva
      // Si la página quedó vacía (ej. después de borrar), volver a la última con datos
      const lastPage = Math.max(1, Math.ceil(res.total / q.pageSize));
      if (res.items.length === 0 && q.page > lastPage) {
        this.pageNum.set(lastPage);
        return;
      }
      this.page.set({ items: res.items ?? [], total: res.total });
    } catch {
      /* el interceptor muestra el error */
    } finally {
      if (seq === this.requestSeq) this.loading.set(false);
    }
  }

  protected setCategory(c: string): void {
    this.category.set(c);
    this.pageNum.set(1);
    this.expanded.set(null);
  }

  protected setDate(d: string): void {
    this.date.set(this.date() === d ? '' : d);
    this.pageNum.set(1);
    this.expanded.set(null);
  }

  protected clearFilters(): void {
    this.date.set('');
    this.category.set('all');
    this.pageNum.set(1);
  }

  protected goTo(p: number): void {
    this.pageNum.set(p);
    this.expanded.set(null);
  }

  protected setPageSize(s: number): void {
    this.pageSize.set(s);
    this.pageNum.set(1);
  }

  protected toggle(id: string): void {
    this.expanded.set(this.expanded() === id ? null : id);
  }

  protected isPending(v: Validation): boolean {
    return v.validation_result === 'REJECTED_NOT_FOUND' && this.store.pendingIds().has(v.id);
  }

  protected info(v: Validation) {
    return statusInfo(v.validation_result);
  }

  protected fields(v: Validation): { label: string; value: string; mono: boolean }[] {
    const out: { label: string; value: string; mono: boolean }[] = [];
    const add = (label: string, value: string | number | undefined | null, mono = false) => {
      if (value !== undefined && value !== null && String(value).trim() !== '') out.push({ label, value: String(value), mono });
    };
    add('Número de operación', v.extracted_op_number, true);
    add('ID COELSA', v.extracted_coelsa_id, true);
    add('CUIT del emisor', v.extracted_sender_cuit, true);
    add('Destino en el comprobante', v.extracted_receiver_alias_or_cbu, true);
    add('Banco / billetera', v.extracted_source_bank);
    if (v.extracted_date) add('Fecha del comprobante', new Date(v.extracted_date).toLocaleString('es-AR', { hour12: false }));
    add('Recibido', new Date(v.created_at).toLocaleString('es-AR', { hour12: false }));
    add('Enviado por', this.store.senderLabel(v.sender_phone));
    if (v.processing_time_ms) add('Tiempo de análisis', (v.processing_time_ms / 1000).toFixed(1) + ' s');
    add('ID de verificación', v.id, true);
    return out;
  }

  protected async remove(v: Validation): Promise<void> {
    const verified = this.info(v).category === 'ok';
    const ok = await this.ui.confirm({
      title: 'Eliminar comprobante',
      message:
        `Se va a borrar el comprobante de ${v.extracted_sender_name || 'este cliente'} por ${new Intl.NumberFormat('es-AR', { style: 'currency', currency: 'ARS' }).format(v.extracted_amount)}.` +
        (verified
          ? '\n\nEl pago de Mercado Pago que tenía asignado vuelve a quedar libre, para poder validarlo con el comprobante correcto.'
          : '') +
        '\n\nNo se puede deshacer.',
      confirmText: 'Eliminar',
      danger: true,
    });
    if (!ok) return;
    this.deleting.set(v.id);
    try {
      const res = await this.api.deleteValidation(v.id);
      this.ui.toast(res.payment_released ? 'Comprobante eliminado. El pago quedó libre.' : 'Comprobante eliminado');
      this.expanded.set(null);
      await this.store.refresh(); // también recarga esta lista
    } catch {
      /* el interceptor muestra el error */
    } finally {
      this.deleting.set(null);
    }
  }
}
