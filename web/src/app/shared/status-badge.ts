import { ChangeDetectionStrategy, Component, computed, input } from '@angular/core';
import { Category, ValidationStatus } from '../core/models';
import { CATEGORY, statusInfo, Tone } from '../core/status';

const TONE_CLASSES: Record<Tone, { badge: string; dot: string }> = {
  ok: {
    badge: 'bg-emerald-50 text-emerald-700 ring-emerald-600/15 dark:bg-emerald-400/10 dark:text-emerald-300 dark:ring-emerald-400/20',
    dot: 'bg-emerald-500',
  },
  bad: {
    badge: 'bg-red-50 text-red-700 ring-red-600/15 dark:bg-red-400/10 dark:text-red-300 dark:ring-red-400/20',
    dot: 'bg-red-500',
  },
  warn: {
    badge: 'bg-amber-50 text-amber-800 ring-amber-600/20 dark:bg-amber-400/10 dark:text-amber-300 dark:ring-amber-400/20',
    dot: 'bg-amber-500',
  },
  neutral: {
    badge: 'bg-zinc-100 text-zinc-700 ring-zinc-500/15 dark:bg-zinc-800 dark:text-zinc-300 dark:ring-zinc-700',
    dot: 'bg-zinc-400',
  },
};

/**
 * Etiqueta del resultado de un comprobante: categoría y, si aporta, el motivo.
 * Ej.: "Fraude · Comprobante adulterado", "Rechazada · Transferido a otra cuenta", "Correcta".
 */
@Component({
  selector: 'app-status-badge',
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <span
      class="inline-flex items-center gap-1.5 rounded-full px-2 py-0.5 text-xs font-medium whitespace-nowrap ring-1 ring-inset"
      [class]="classes().badge"
    >
      <span class="size-1.5 rounded-full" [class]="classes().dot"></span>
      {{ text() }}
    </span>
  `,
})
export class StatusBadge {
  readonly status = input.required<ValidationStatus | string>();
  /** El comprobante está esperando que entre el pago */
  readonly pending = input(false);

  private readonly category = computed<Category | 'pending'>(() =>
    this.pending() ? 'pending' : statusInfo(this.status()).category,
  );
  protected readonly classes = computed(() => TONE_CLASSES[CATEGORY[this.category()].tone]);
  protected readonly text = computed(() => {
    const cat = this.category();
    if (cat === 'pending') return 'Pendiente';
    if (cat === 'ok' || cat === 'not_found') return CATEGORY[cat].label;
    return `${CATEGORY[cat].label} · ${statusInfo(this.status()).label}`;
  });
}
