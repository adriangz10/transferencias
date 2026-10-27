import { ChangeDetectionStrategy, Component, computed, inject } from '@angular/core';
import { RouterLink } from '@angular/router';
import { DashboardStore } from '../../core/dashboard.store';
import { HourPipe, MoneyPipe } from '../../core/format';
import { Icon } from '../../shared/icon';
import { HistoryCard } from './history-card';
import { PaymentsCard } from './payments-card';
import { UploadCard } from './upload-card';

@Component({
  selector: 'app-home',
  imports: [RouterLink, Icon, HistoryCard, PaymentsCard, UploadCard, MoneyPipe, HourPipe],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    @if (oauthConnected) {
      <div class="mb-4 rounded-lg border border-zinc-200 bg-white px-4 py-2.5 text-sm dark:border-zinc-800 dark:bg-zinc-900">
        Mercado Pago quedó vinculado.
      </div>
    }

    <!-- Primeros pasos: solo mientras falte algo -->
    @if (store.data() && !setupDone()) {
      <section class="card mb-6 p-5">
        <h2 class="card-title">Primeros pasos</h2>
        <p class="card-help">Completá esto para empezar a verificar cobros.</p>
        <ol class="mt-4 space-y-3">
          @for (s of steps(); track s.text; let i = $index) {
            <li class="flex items-center gap-3">
              <span
                class="flex size-6 shrink-0 items-center justify-center rounded-full text-xs font-medium"
                [class]="s.done ? 'bg-zinc-900 text-white dark:bg-zinc-100 dark:text-zinc-900' : 'border border-zinc-300 text-zinc-500 dark:border-zinc-700'"
              >
                @if (s.done) {
                  <app-icon name="check" [size]="12" [stroke]="3" />
                } @else {
                  {{ i + 1 }}
                }
              </span>
              <span class="flex-1 text-sm" [class.muted]="s.done" [class.line-through]="s.done">{{ s.text }}</span>
              @if (!s.done) {
                @if (s.href) {
                  <a class="btn btn-secondary btn-sm" [href]="s.href">{{ s.action }}</a>
                } @else {
                  <a class="btn btn-secondary btn-sm" routerLink="/configuracion">{{ s.action }}</a>
                }
              }
            </li>
          }
        </ol>
      </section>
    }

    <!-- Resumen de hoy -->
    <div class="mb-6 grid grid-cols-2 gap-3 lg:grid-cols-4">
      @for (k of kpis(); track k.label) {
        <div class="card px-4 py-3.5">
          <div class="muted flex items-center gap-1.5 text-xs">
            <span class="size-1.5 rounded-full" [class]="k.dot"></span>{{ k.label }}
          </div>
          <div class="tabular mt-1 text-2xl font-semibold tracking-tight">{{ k.value }}</div>
        </div>
      }
    </div>

    <div class="grid gap-6 lg:grid-cols-3">
      <div class="space-y-6 lg:col-span-2">
        <app-history-card />
        <app-payments-card />
      </div>

      <div class="space-y-6">
        <app-upload-card />

        @if (store.pending().length) {
          <section class="card">
            <div class="card-header">
              <div>
                <h2 class="card-title">Esperando el dinero</h2>
                <p class="card-help">Si el pago entra, se avisa solo por WhatsApp.</p>
              </div>
            </div>
            <ul class="divide-soft">
              @for (p of store.pending(); track p.validation_id) {
                <li class="flex items-center gap-3 px-5 py-3">
                  <span class="size-1.5 shrink-0 animate-pulse rounded-full bg-amber-500"></span>
                  <div class="min-w-0 flex-1">
                    <div class="truncate text-sm font-medium">{{ p.sender_name || 'Cliente' }}</div>
                    <div class="muted text-xs">{{ store.senderLabel(p.sender_phone) }} · {{ p.created_at | hour }}</div>
                  </div>
                  <span class="tabular text-sm font-medium">{{ p.expected_amount | money }}</span>
                </li>
              }
            </ul>
          </section>
        }
      </div>
    </div>
  `,
})
export class HomePage {
  protected readonly store = inject(DashboardStore);
  protected readonly oauthConnected = new URLSearchParams(location.search).get('oauth') === 'connected';

  protected readonly kpis = computed(() => {
    const t = this.store.data()?.today ?? { ok: 0, fraud: 0, rejected: 0, not_found: 0 };
    const pending = this.store.pending().length;
    return [
      { label: 'Correctas hoy', value: t.ok, dot: 'bg-emerald-500' },
      { label: 'Fraudes hoy', value: t.fraud, dot: 'bg-red-500' },
      { label: 'Rechazadas o sin ingresar', value: t.rejected + Math.max(0, t.not_found - pending), dot: 'bg-zinc-400' },
      { label: 'Esperando el dinero', value: pending, dot: 'bg-amber-500' },
    ];
  });

  protected readonly steps = computed(() => {
    const acc = this.store.account();
    return [
      { text: 'Completá los datos del comercio', done: !!this.store.data()?.tenant?.business_name, action: 'Completar', href: '' },
      { text: 'Vinculá tu cuenta de Mercado Pago', done: !!acc, action: 'Vincular', href: '/oauth/mp/connect' },
      { text: 'Cargá tu alias y CVU', done: !!(acc && (acc.expected_alias || acc.expected_cvu)), action: 'Cargar', href: '' },
      { text: 'Agregá a quienes te reenvían los comprobantes', done: this.store.senders().length > 0, action: 'Agregar', href: '' },
    ];
  });
  protected readonly setupDone = computed(() => this.steps().every((s) => s.done));

  constructor() {
    if (this.oauthConnected) history.replaceState(null, '', '/');
  }
}
