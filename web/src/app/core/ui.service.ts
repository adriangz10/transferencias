import { Injectable, signal } from '@angular/core';

export type Theme = 'light' | 'dark';

interface ConfirmRequest {
  title: string;
  message: string;
  confirmText: string;
  danger: boolean;
  resolve: (ok: boolean) => void;
}

/** Estado global de la interfaz: tema, avisos, errores y diálogos de confirmación. */
@Injectable({ providedIn: 'root' })
export class UiService {
  readonly theme = signal<Theme>(document.documentElement.classList.contains('dark') ? 'dark' : 'light');
  readonly toastMessage = signal<string | null>(null);
  readonly error = signal<string | null>(null);
  readonly confirmRequest = signal<ConfirmRequest | null>(null);

  private toastTimer?: ReturnType<typeof setTimeout>;

  toggleTheme(): void {
    const next: Theme = this.theme() === 'dark' ? 'light' : 'dark';
    this.theme.set(next);
    document.documentElement.classList.toggle('dark', next === 'dark');
    try {
      localStorage.setItem('theme', next);
    } catch {
      /* sin almacenamiento: el tema dura hasta recargar */
    }
  }

  toast(message: string): void {
    this.toastMessage.set(message);
    clearTimeout(this.toastTimer);
    this.toastTimer = setTimeout(() => this.toastMessage.set(null), 2800);
  }

  confirm(opts: { title: string; message: string; confirmText?: string; danger?: boolean }): Promise<boolean> {
    return new Promise((resolve) => {
      this.confirmRequest.set({
        title: opts.title,
        message: opts.message,
        confirmText: opts.confirmText ?? 'Confirmar',
        danger: opts.danger ?? false,
        resolve,
      });
    });
  }

  answerConfirm(ok: boolean): void {
    const req = this.confirmRequest();
    this.confirmRequest.set(null);
    req?.resolve(ok);
  }
}
