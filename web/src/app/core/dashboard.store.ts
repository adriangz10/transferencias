import { computed, DestroyRef, inject, Injectable, signal } from '@angular/core';
import { ApiService } from './api.service';
import { Dashboard } from './models';

const REFRESH_MS = 15_000;

/** Estado del comercio compartido por todas las pantallas. Se refresca solo cada 15 s. */
@Injectable({ providedIn: 'root' })
export class DashboardStore {
  private api = inject(ApiService);

  readonly data = signal<Dashboard | null>(null);
  /** Cambia en cada refresco; las pantallas lo usan para recargar sus propias listas. */
  readonly tick = signal(0);

  readonly senders = computed(() => this.data()?.senders ?? []);
  readonly pending = computed(() => this.data()?.pending_queue ?? []);
  readonly pendingIds = computed(() => new Set(this.pending().map((p) => p.validation_id)));
  readonly account = computed(() => this.data()?.mp_account ?? null);
  readonly demo = computed(() => !!this.data()?.demo_mode);
  readonly nameByPhone = computed(() => {
    const m: Record<string, string> = {};
    for (const s of this.senders()) m[s.phone_number] = s.employee_name;
    return m;
  });

  private timer?: ReturnType<typeof setInterval>;

  start(destroyRef: DestroyRef): void {
    this.refresh();
    this.timer = setInterval(() => {
      if (!document.hidden) this.refresh();
    }, REFRESH_MS);
    destroyRef.onDestroy(() => clearInterval(this.timer));
  }

  async refresh(): Promise<void> {
    try {
      this.data.set(await this.api.dashboard());
      this.tick.update((n) => n + 1);
    } catch {
      /* el interceptor ya mostró el error */
    }
  }

  /** Quién reenvió el comprobante, para mostrar en las listas. */
  senderLabel(phone: string): string {
    if (phone === 'panel') return 'el panel';
    return this.nameByPhone()[phone] || '+' + phone;
  }
}
