import { ChangeDetectionStrategy, Component, inject } from '@angular/core';
import { RouterLink } from '@angular/router';
import { UiService } from '../../core/ui.service';
import { Icon } from '../../shared/icon';

/**
 * Login del panel. El formulario se envía de forma tradicional a /api/v1/login: el servidor
 * crea la sesión (para el comercio o Super-Admin) y redirige a "/" (o vuelve acá con ?error=...).
 */
@Component({
  selector: 'app-login',
  imports: [Icon, RouterLink],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="flex min-h-dvh items-center justify-center px-4 py-8">
      <div class="absolute top-4 right-4">
        <button
          class="btn btn-ghost px-2"
          (click)="ui.toggleTheme()"
          [attr.aria-label]="ui.theme() === 'dark' ? 'Usar modo claro' : 'Usar modo oscuro'"
        >
          <app-icon [name]="ui.theme() === 'dark' ? 'sun' : 'moon'" [size]="18" />
        </button>
      </div>

      <div class="w-full max-w-sm">
        <div class="mb-8 flex flex-col items-center text-center">
          <div
            class="flex size-10 items-center justify-center rounded-xl bg-zinc-900 text-white dark:bg-zinc-100 dark:text-zinc-900"
          >
            <app-icon name="check" [size]="22" [stroke]="2.5" />
          </div>
          <h1 class="mt-4 text-xl font-semibold tracking-tight">Ingresá a VeriPago</h1>
          <p class="muted mt-1 text-sm">Verificación de transferencias para comercios</p>
        </div>

        <form class="card space-y-4 p-6" method="post" action="/api/v1/login">
          @if (error === 'locked') {
            <p class="rounded-lg bg-red-50 px-3 py-2 text-sm text-red-800 dark:bg-red-400/10 dark:text-red-200">
              Demasiados intentos. Esperá unos minutos y probá de nuevo.
            </p>
          } @else if (error === 'suspended') {
            <p class="rounded-lg bg-amber-50 px-3 py-2 text-sm text-amber-800 dark:bg-amber-400/10 dark:text-amber-200">
              Esta cuenta se encuentra suspendida. Contactá al administrador de la plataforma.
            </p>
          } @else if (error) {
            <p class="rounded-lg bg-red-50 px-3 py-2 text-sm text-red-800 dark:bg-red-400/10 dark:text-red-200">
              Email/usuario o contraseña incorrectos.
            </p>
          }
          <div>
            <label class="label" for="username">Email del comercio (o usuario admin)</label>
            <input
              id="username"
              name="username"
              class="input"
              placeholder="hola@micomercio.com"
              autocomplete="username"
              required
              autofocus
            />
          </div>
          <div>
            <label class="label" for="password">Contraseña</label>
            <input
              id="password"
              name="password"
              type="password"
              class="input"
              autocomplete="current-password"
              required
            />
          </div>
          <button class="btn btn-primary w-full" type="submit">Ingresar</button>
        </form>

        <p class="muted mt-6 text-center text-sm">
          ¿Todavía no tenés cuenta?
          <a routerLink="/registro" class="font-medium text-zinc-900 underline underline-offset-4 dark:text-zinc-100">
            Registrá tu comercio gratis
          </a>
        </p>
      </div>
    </div>
  `,
})
export class LoginPage {
  protected readonly ui = inject(UiService);
  protected readonly error = new URLSearchParams(location.search).get('error');
}
