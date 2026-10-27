import { ChangeDetectionStrategy, Component, computed, inject, signal } from '@angular/core';
import { ApiService } from '../../core/api.service';
import { DashboardStore } from '../../core/dashboard.store';
import { waText } from '../../core/format';
import { Outcome } from '../../core/models';
import { UiService } from '../../core/ui.service';
import { Icon } from '../../shared/icon';
import { StatusBadge } from '../../shared/status-badge';

/** Subir un comprobante (foto o PDF) desde el panel y ver el resultado. */
@Component({
  selector: 'app-upload-card',
  host: { class: 'block' },
  imports: [Icon, StatusBadge],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <section class="card p-5">
      <h2 class="card-title">Verificar un comprobante</h2>
      <p class="card-help">Subí la foto o el PDF y te decimos si el pago entró.</p>

      <label
        class="mt-4 flex cursor-pointer flex-col items-center justify-center gap-2 rounded-lg border border-dashed px-4 py-6 text-center transition-colors"
        [class]="
          dragging()
            ? 'border-zinc-500 bg-zinc-50 dark:border-zinc-400 dark:bg-zinc-800/50'
            : 'border-zinc-300 hover:border-zinc-400 hover:bg-zinc-50 dark:border-zinc-700 dark:hover:border-zinc-600 dark:hover:bg-zinc-800/30'
        "
        (dragover)="$event.preventDefault(); dragging.set(true)"
        (dragleave)="dragging.set(false)"
        (drop)="onDrop($event)"
      >
        <span class="muted"><app-icon [name]="file() ? 'file' : 'upload'" [size]="20" /></span>
        @if (file(); as f) {
          <span class="max-w-full truncate text-sm font-medium">{{ f.name }}</span>
          <span class="muted text-xs">Tocá para cambiarlo</span>
        } @else {
          <span class="text-sm font-medium">Elegí un archivo o arrastralo acá</span>
          <span class="muted text-xs">JPG, PNG o PDF</span>
        }
        <input type="file" class="sr-only" accept="image/*,application/pdf" (change)="onPick($event)" [disabled]="busy()" />
      </label>

      <button class="btn btn-primary mt-3 w-full" [disabled]="!file() || busy()" (click)="verify()">
        {{ busy() ? 'Analizando…' : 'Verificar' }}
      </button>
      @if (busy()) {
        <p class="muted mt-2 text-center text-xs">Puede tardar hasta un minuto. No cierres la página.</p>
      }

      @if (result(); as r) {
        <div class="mt-4 rounded-lg border border-zinc-200 p-4 dark:border-zinc-800">
          <app-status-badge [status]="r.status" />
          @if (r.message_for_user) {
            <div class="mt-3 text-sm leading-relaxed [&_b]:font-semibold" [innerHTML]="messageHtml()"></div>
          }
        </div>
      }
    </section>
  `,
})
export class UploadCard {
  private readonly api = inject(ApiService);
  private readonly store = inject(DashboardStore);
  private readonly ui = inject(UiService);

  protected readonly file = signal<File | null>(null);
  protected readonly busy = signal(false);
  protected readonly dragging = signal(false);
  protected readonly result = signal<Outcome | null>(null);
  protected readonly messageHtml = computed(() => waText(this.result()?.message_for_user));

  protected onPick(e: Event): void {
    const input = e.target as HTMLInputElement;
    this.file.set(input.files?.[0] ?? null);
    this.result.set(null);
    input.value = '';
  }

  protected onDrop(e: DragEvent): void {
    e.preventDefault();
    this.dragging.set(false);
    const f = e.dataTransfer?.files?.[0];
    if (f) {
      this.file.set(f);
      this.result.set(null);
    }
  }

  protected async verify(): Promise<void> {
    const f = this.file();
    if (!f) return;
    this.busy.set(true);
    this.result.set(null);
    try {
      this.result.set(await this.api.uploadReceipt(f));
      this.file.set(null);
      await this.store.refresh();
    } catch (err) {
      if (err instanceof Error && !('status' in err)) this.ui.error.set(err.message);
    } finally {
      this.busy.set(false);
    }
  }
}
