import { ChangeDetectionStrategy, Component, inject } from '@angular/core';
import { UiService } from '../core/ui.service';

/** Diálogo de confirmación propio (no usa window.confirm). */
@Component({
  selector: 'app-confirm-dialog',
  changeDetection: ChangeDetectionStrategy.OnPush,
  host: { '(document:keydown.escape)': 'ui.confirmRequest() && ui.answerConfirm(false)' },
  template: `
    @if (ui.confirmRequest(); as req) {
      <div class="fixed inset-0 z-50 flex items-end justify-center p-4 sm:items-center">
        <div class="absolute inset-0 bg-zinc-950/40 backdrop-blur-[1px]" (click)="ui.answerConfirm(false)"></div>
        <div
          class="card relative w-full max-w-md p-6 shadow-xl"
          role="alertdialog"
          aria-modal="true"
          aria-labelledby="confirm-title"
        >
          <h2 id="confirm-title" class="text-base font-semibold">{{ req.title }}</h2>
          <p class="muted mt-2 text-sm whitespace-pre-line">{{ req.message }}</p>
          <div class="mt-6 flex justify-end gap-2">
            <button class="btn btn-secondary" (click)="ui.answerConfirm(false)">Cancelar</button>
            <button
              class="btn"
              [class.btn-danger]="req.danger"
              [class.btn-primary]="!req.danger"
              (click)="ui.answerConfirm(true)"
              autofocus
            >
              {{ req.confirmText }}
            </button>
          </div>
        </div>
      </div>
    }
  `,
})
export class ConfirmDialog {
  protected readonly ui = inject(UiService);
}
