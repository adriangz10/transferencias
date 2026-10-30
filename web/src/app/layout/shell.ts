import { ChangeDetectionStrategy, Component, computed, DestroyRef, effect, inject } from '@angular/core';
import { Router, RouterLink, RouterLinkActive, RouterOutlet } from '@angular/router';
import { ApiService } from '../core/api.service';
import { DashboardStore } from '../core/dashboard.store';
import { UiService } from '../core/ui.service';
import { Icon } from '../shared/icon';

@Component({
  selector: 'app-shell',
  imports: [RouterOutlet, RouterLink, RouterLinkActive, Icon],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <header
      class="sticky top-0 z-30 border-b border-zinc-200 bg-white/85 backdrop-blur dark:border-zinc-800 dark:bg-zinc-950/85"
    >
      <div class="mx-auto flex h-14 max-w-6xl items-center justify-between gap-4 px-4 sm:px-6">
        <div class="flex min-w-0 items-center gap-3">
          <div
            class="flex size-8 shrink-0 items-center justify-center rounded-lg bg-zinc-900 text-white dark:bg-zinc-100 dark:text-zinc-900"
          >
            <app-icon name="check" [size]="18" [stroke]="2.5" />
          </div>
          <div class="min-w-0 leading-tight">
            <div class="flex items-center gap-1.5 text-sm font-semibold">
              <span>VeriPago</span>
              @if (store.data()?.is_super_admin) {
                <span
                  class="rounded bg-zinc-900 px-1.5 py-0.5 text-[10px] font-medium text-white dark:bg-zinc-100 dark:text-zinc-900"
                >
                  ADMIN
                </span>
              }
            </div>
            <div class="muted truncate text-xs">{{ businessName() }}</div>
          </div>
        </div>

        <nav class="hidden items-center gap-1 md:flex">
          @for (t of tabs(); track t.path) {
            <a
              [routerLink]="t.path"
              routerLinkActive="!bg-zinc-100 !text-zinc-900 dark:!bg-zinc-800 dark:!text-zinc-100"
              [routerLinkActiveOptions]="{ exact: t.path === '/' }"
              class="rounded-lg px-3 py-1.5 text-sm font-medium text-zinc-500 transition-colors hover:text-zinc-900 dark:text-zinc-400 dark:hover:text-zinc-100"
              >{{ t.label }}</a
            >
          }
        </nav>

        <div class="flex items-center gap-1">
          <button
            class="btn btn-ghost px-2"
            (click)="ui.toggleTheme()"
            [attr.aria-label]="ui.theme() === 'dark' ? 'Usar modo claro' : 'Usar modo oscuro'"
            [title]="ui.theme() === 'dark' ? 'Modo claro' : 'Modo oscuro'"
          >
            <app-icon [name]="ui.theme() === 'dark' ? 'sun' : 'moon'" [size]="18" />
          </button>
          <form method="post" action="/logout">
            <button class="btn btn-ghost px-2" type="submit" title="Cambiar cuenta / Salir" aria-label="Salir">
              <app-icon name="logout" [size]="18" />
            </button>
          </form>
        </div>
      </div>

      <!-- Pestañas en celulares -->
      <nav class="flex gap-1 overflow-x-auto border-t border-zinc-200 px-4 py-2 md:hidden dark:border-zinc-800">
        @for (t of tabs(); track t.path) {
          <a
            [routerLink]="t.path"
            routerLinkActive="!bg-zinc-100 !text-zinc-900 dark:!bg-zinc-800 dark:!text-zinc-100"
            [routerLinkActiveOptions]="{ exact: t.path === '/' }"
            class="rounded-lg px-3 py-1.5 text-sm font-medium whitespace-nowrap text-zinc-500 dark:text-zinc-400"
            >{{ t.label }}</a
          >
        }
      </nav>
    </header>

    <main class="mx-auto max-w-6xl px-4 py-6 sm:px-6 sm:py-8">
      @if (store.data()?.impersonating) {
        <div
          class="mb-4 flex flex-wrap items-center justify-between gap-3 rounded-lg border border-indigo-200 bg-indigo-50 px-4 py-2.5 text-sm text-indigo-900 dark:border-indigo-400/20 dark:bg-indigo-400/10 dark:text-indigo-200"
        >
          <span>
            Estás inspeccionando el panel de <b class="font-semibold">{{ businessName() }}</b> como Super-Admin.
          </span>
          <button class="btn btn-secondary btn-sm" type="button" (click)="stopImpersonating()">
            Volver a Super-Admin
          </button>
        </div>
      }
      @if (store.demo()) {
        <div
          class="mb-4 rounded-lg border border-amber-200 bg-amber-50 px-4 py-2.5 text-sm text-amber-900 dark:border-amber-400/20 dark:bg-amber-400/10 dark:text-amber-200"
        >
          <b class="font-medium">Modo de prueba.</b> Los pagos son simulados; no lo uses con clientes reales.
        </div>
      }
      @if (ui.error(); as err) {
        <div
          role="alert"
          class="mb-4 flex items-start justify-between gap-3 rounded-lg border border-red-200 bg-red-50 px-4 py-2.5 text-sm text-red-800 dark:border-red-400/20 dark:bg-red-400/10 dark:text-red-200"
        >
          <span>{{ err }}</span>
          <button class="shrink-0 opacity-70 hover:opacity-100" (click)="ui.error.set(null)" aria-label="Cerrar">
            <app-icon name="x" />
          </button>
        </div>
      }
      <router-outlet />
    </main>

    @if (ui.toastMessage(); as msg) {
      <div
        role="status"
        class="fixed bottom-6 left-1/2 z-50 -translate-x-1/2 rounded-lg bg-zinc-900 px-4 py-2.5 text-sm text-white shadow-lg dark:bg-zinc-100 dark:text-zinc-900"
      >
        {{ msg }}
      </div>
    }
  `,
})
export class Shell {
  private readonly api = inject(ApiService);
  protected readonly store = inject(DashboardStore);
  protected readonly ui = inject(UiService);
  private readonly router = inject(Router);

  protected readonly businessName = computed(
    () => this.store.data()?.tenant?.business_name || 'Completá los datos del comercio',
  );
  protected readonly tabs = computed(() => {
    const t = [
      { path: '/', label: 'Inicio' },
      { path: '/clientes', label: 'Clientes' },
      { path: '/cobrar', label: 'Cobrar' },
      { path: '/configuracion', label: 'Configuración' },
    ];
    if (this.store.demo()) t.push({ path: '/pruebas', label: 'Pruebas' });
    if (this.store.data()?.is_super_admin) t.push({ path: '/superadmin', label: 'Super-Admin' });
    return t;
  });

  constructor() {
    this.store.start(inject(DestroyRef));
    // Fuera del modo demo no existe la pestaña de pruebas
    effect(() => {
      const d = this.store.data();
      if (d && !d.demo_mode && this.router.url.startsWith('/pruebas')) this.router.navigateByUrl('/');
      if (d && !d.is_super_admin && this.router.url.startsWith('/superadmin')) this.router.navigateByUrl('/');
    });
  }

  protected async stopImpersonating(): Promise<void> {
    try {
      await this.api.superAdminImpersonate('');
      await this.store.refresh();
      await this.router.navigateByUrl('/superadmin');
    } catch {
      /* el interceptor muestra el error */
    }
  }
}
