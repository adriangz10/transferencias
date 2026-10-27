import { ChangeDetectionStrategy, Component, computed, input, output } from '@angular/core';
import { Icon } from './icon';

/** Paginador: "1–20 de 134", tamaño de página y botones de página. */
@Component({
  selector: 'app-pagination',
  imports: [Icon],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="flex flex-wrap items-center justify-between gap-3 text-sm">
      <div class="muted flex items-center gap-3">
        <span class="tabular">
          @if (total() === 0) {
            Sin resultados
          } @else {
            {{ from() }}–{{ to() }} de {{ total() }}
          }
        </span>
        @if (pageSizes().length > 1) {
          <label class="flex items-center gap-1.5">
            <span class="sr-only sm:not-sr-only">Por página</span>
            <select
              class="input w-auto py-1 pr-7 text-xs"
              (change)="pageSizeChange.emit(+$any($event.target).value)"
            >
              @for (s of pageSizes(); track s) {
                <option [value]="s" [selected]="s === pageSize()">{{ s }}</option>
              }
            </select>
          </label>
        }
      </div>

      @if (pages() > 1) {
        <nav class="flex items-center gap-1" aria-label="Paginación">
          <button
            class="btn btn-ghost btn-sm"
            [disabled]="page() <= 1"
            (click)="pageChange.emit(page() - 1)"
            aria-label="Página anterior"
          >
            <app-icon name="chevronLeft" />
          </button>
          @for (p of pageList(); track $index) {
            @if (p === 0) {
              <span class="muted px-1">…</span>
            } @else {
              <button
                class="btn btn-sm min-w-8 tabular"
                [class.btn-primary]="p === page()"
                [class.btn-ghost]="p !== page()"
                [attr.aria-current]="p === page() ? 'page' : null"
                (click)="pageChange.emit(p)"
              >
                {{ p }}
              </button>
            }
          }
          <button
            class="btn btn-ghost btn-sm"
            [disabled]="page() >= pages()"
            (click)="pageChange.emit(page() + 1)"
            aria-label="Página siguiente"
          >
            <app-icon name="chevronRight" />
          </button>
        </nav>
      }
    </div>
  `,
})
export class Pagination {
  readonly page = input.required<number>();
  readonly pageSize = input.required<number>();
  readonly total = input.required<number>();
  readonly pageSizes = input<number[]>([10, 20, 50]);
  readonly pageChange = output<number>();
  readonly pageSizeChange = output<number>();

  protected readonly pages = computed(() => Math.max(1, Math.ceil(this.total() / this.pageSize())));
  protected readonly from = computed(() => (this.page() - 1) * this.pageSize() + 1);
  protected readonly to = computed(() => Math.min(this.total(), this.page() * this.pageSize()));

  /** Páginas a mostrar; 0 representa "…". Ej: 1 … 4 5 6 … 12 */
  protected readonly pageList = computed(() => {
    const n = this.pages();
    const cur = this.page();
    if (n <= 7) return Array.from({ length: n }, (_, i) => i + 1);
    const set = new Set([1, n, cur - 1, cur, cur + 1].filter((p) => p >= 1 && p <= n));
    const sorted = [...set].sort((a, b) => a - b);
    const out: number[] = [];
    sorted.forEach((p, i) => {
      if (i > 0 && p - sorted[i - 1] > 1) out.push(0);
      out.push(p);
    });
    return out;
  });
}
