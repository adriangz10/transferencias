import { afterRenderEffect, ChangeDetectionStrategy, Component, computed, ElementRef, inject, signal, viewChild } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ApiService } from '../../core/api.service';
import { DashboardStore } from '../../core/dashboard.store';
import { waText } from '../../core/format';
import { Outcome } from '../../core/models';
import { UiService } from '../../core/ui.service';
import { StatusBadge } from '../../shared/status-badge';

const SCENARIOS = [
  { id: 'EXACT_MP_VALID', label: 'Pago válido' },
  { id: 'HEURISTIC_CVU_VALID', label: 'Transferencia de otro banco' },
  { id: 'FRAUD_DUPLICATE_SHA256', label: 'Comprobante repetido' },
  { id: 'FRAUD_DOUBLE_SPEND_ID', label: 'Pago ya usado' },
  { id: 'FRAUD_INCOMPLETE_TRANSFER', label: 'Transferencia sin terminar' },
  { id: 'REJECTED_WRONG_DESTINATION', label: 'Enviado a otra cuenta' },
  { id: 'DELAYED_COELSA_QUEUE', label: 'El banco demora' },
];

/** Solo en modo demo: escenarios simulados y chat del bot. */
@Component({
  selector: 'app-demo',
  imports: [FormsModule, StatusBadge],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="mb-6">
      <h1 class="text-lg font-semibold tracking-tight">Pruebas</h1>
      <p class="muted text-sm">Simula que un empleado reenvía un comprobante. No usa APIs reales.</p>
    </div>

    <div class="grid gap-6 lg:grid-cols-2">
      <section class="card p-5">
        <h2 class="card-title">Probar casos</h2>
        <label class="label mt-4" for="sender">Quién lo envía</label>
        <select id="sender" name="sender" class="input" [(ngModel)]="senderPhone">
          @for (s of store.senders(); track s.id) {
            <option [value]="s.phone_number">{{ s.employee_name }}</option>
          }
        </select>
        <div class="mt-4 grid gap-2 sm:grid-cols-2">
          @for (sc of scenarios; track sc.id) {
            <button class="btn btn-secondary justify-start" (click)="run(sc.id)">{{ sc.label }}</button>
          }
          <button class="btn btn-secondary justify-start" (click)="pay()">Simular que entra un pago</button>
        </div>
        @if (result(); as r) {
          <div class="mt-4 rounded-lg border border-zinc-200 p-4 dark:border-zinc-800">
            <app-status-badge [status]="r.status" />
            <div class="mt-3 text-sm leading-relaxed [&_b]:font-semibold" [innerHTML]="resultHtml()"></div>
          </div>
        }
      </section>

      <section class="card flex flex-col p-5">
        <h2 class="card-title">Chat del bot</h2>
        <div #chat class="mt-4 flex h-96 flex-col gap-2 overflow-y-auto rounded-lg bg-zinc-50 p-3 dark:bg-zinc-950">
          @for (m of messages(); track $index) {
            <div
              class="max-w-[85%] rounded-lg px-3 py-2 text-sm"
              [class]="m.direction === 'OUTBOUND'
                ? 'self-end bg-zinc-900 text-white dark:bg-zinc-100 dark:text-zinc-900'
                : 'border border-zinc-200 bg-white dark:border-zinc-800 dark:bg-zinc-900'"
            >
              <div class="mb-0.5 text-[11px] opacity-60">{{ m.direction === 'OUTBOUND' ? 'Bot' : m.sender }}</div>
              <div [innerHTML]="html(m.body)"></div>
            </div>
          } @empty {
            <p class="muted m-auto text-sm">Todavía no hay mensajes.</p>
          }
        </div>
        <form class="mt-3 flex gap-2" (ngSubmit)="send()">
          <input name="cmd" class="input flex-1" placeholder="Probá: /cobrar 15000 Mesa 2" [(ngModel)]="command" />
          <button class="btn btn-primary" type="submit">Enviar</button>
        </form>
      </section>
    </div>
  `,
})
export class DemoPage {
  private readonly api = inject(ApiService);
  private readonly ui = inject(UiService);
  protected readonly store = inject(DashboardStore);
  private readonly chat = viewChild<ElementRef<HTMLDivElement>>('chat');

  protected readonly scenarios = SCENARIOS;
  protected readonly senderPhone = signal('');
  protected readonly command = signal('');
  protected readonly result = signal<Outcome | null>(null);
  protected readonly resultHtml = computed(() => waText(this.result()?.message_for_user));
  protected readonly messages = computed(() => this.store.data()?.wa_messages ?? []);
  protected readonly html = waText;

  constructor() {
    afterRenderEffect(() => {
      this.messages();
      const el = this.chat()?.nativeElement;
      if (el) el.scrollTop = el.scrollHeight;
      if (!this.senderPhone() && this.store.senders().length) this.senderPhone.set(this.store.senders()[0].phone_number);
    });
  }

  protected async run(scenario: string): Promise<void> {
    try {
      this.result.set(await this.api.simulateReceipt(scenario, this.senderPhone()));
      await this.store.refresh();
    } catch {
      /* el interceptor muestra el error */
    }
  }

  protected async pay(): Promise<void> {
    await this.api.simulatePayment();
    this.ui.toast('Pago simulado');
    await this.store.refresh();
  }

  protected async send(): Promise<void> {
    const text = this.command().trim();
    if (!text) return;
    this.command.set('');
    await this.api.sendCommand(text, this.senderPhone());
    await this.store.refresh();
  }
}
