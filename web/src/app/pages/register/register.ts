import { HttpErrorResponse } from '@angular/common/http';
import { ChangeDetectionStrategy, Component, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { RouterLink } from '@angular/router';
import { ApiService } from '../../core/api.service';
import { UiService } from '../../core/ui.service';
import { Icon } from '../../shared/icon';

@Component({
  selector: 'app-register',
  imports: [FormsModule, Icon, RouterLink],
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

      <div class="w-full max-w-md">
        <div class="mb-6 flex flex-col items-center text-center">
          <div
            class="flex size-10 items-center justify-center rounded-xl bg-zinc-900 text-white dark:bg-zinc-100 dark:text-zinc-900"
          >
            <app-icon name="check" [size]="22" [stroke]="2.5" />
          </div>
          <h1 class="mt-4 text-xl font-semibold tracking-tight">Creá la cuenta de tu comercio</h1>
          <p class="muted mt-1 text-sm">
            Empezá con 14 días de prueba gratis (50 comprobantes incluidos).
          </p>
        </div>

        <form class="card space-y-4 p-6" (ngSubmit)="submit()">
          @if (errorMsg(); as msg) {
            <p class="rounded-lg bg-red-50 px-3 py-2 text-sm text-red-800 dark:bg-red-400/10 dark:text-red-200">
              {{ msg }}
            </p>
          }

          <div>
            <label class="label" for="bname">Nombre del comercio</label>
            <input
              id="bname"
              name="bname"
              class="input"
              maxlength="150"
              placeholder="Ej: Panadería La Esquina"
              required
              autofocus
              [(ngModel)]="businessName"
            />
          </div>

          <div>
            <label class="label" for="email">Email de acceso</label>
            <input
              id="email"
              name="email"
              type="email"
              class="input"
              placeholder="dueno@micomercio.com"
              autocomplete="email"
              required
              [(ngModel)]="email"
            />
          </div>

          <div>
            <label class="label" for="password"
              >Contraseña <span class="muted font-normal">(mínimo 8 caracteres)</span></label
            >
            <input
              id="password"
              name="password"
              type="password"
              class="input"
              minlength="8"
              autocomplete="new-password"
              required
              [(ngModel)]="password"
            />
          </div>

          <div class="grid gap-4 sm:grid-cols-2">
            <div>
              <label class="label" for="cuit">CUIT <span class="muted font-normal">(opcional)</span></label>
              <input
                id="cuit"
                name="cuit"
                class="input font-mono"
                inputmode="numeric"
                placeholder="30-12345678-9"
                [(ngModel)]="cuit"
              />
            </div>
            <div>
              <label class="label" for="phone">Tu WhatsApp <span class="muted font-normal">(opcional)</span></label>
              <input
                id="phone"
                name="phone"
                type="tel"
                class="input"
                placeholder="5491155004444"
                [(ngModel)]="ownerPhone"
              />
            </div>
          </div>

          <button class="btn btn-primary w-full" type="submit" [disabled]="loading()">
            {{ loading() ? 'Creando cuenta...' : 'Crear cuenta y entrar al panel' }}
          </button>
        </form>

        <p class="muted mt-6 text-center text-sm">
          ¿Ya registraste tu comercio?
          <a routerLink="/login" class="font-medium text-zinc-900 underline underline-offset-4 dark:text-zinc-100">
            Ingresar
          </a>
        </p>
      </div>
    </div>
  `,
})
export class RegisterPage {
  private readonly api = inject(ApiService);
  protected readonly ui = inject(UiService);

  protected readonly businessName = signal('');
  protected readonly email = signal('');
  protected readonly password = signal('');
  protected readonly cuit = signal('');
  protected readonly ownerPhone = signal('');
  protected readonly loading = signal(false);
  protected readonly errorMsg = signal<string | null>(null);

  protected async submit(): Promise<void> {
    this.errorMsg.set(null);
    if (this.password().length < 8) {
      this.errorMsg.set('La contraseña debe tener al menos 8 caracteres.');
      return;
    }
    this.loading.set(true);
    try {
      await this.api.register({
        business_name: this.businessName().trim(),
        email: this.email().trim(),
        password: this.password(),
        cuit: this.cuit().trim(),
        owner_phone: this.ownerPhone().trim(),
      });
      location.href = '/';
    } catch (err) {
      const httpErr = err as HttpErrorResponse;
      const msg =
        (httpErr?.error && typeof httpErr.error === 'object' && httpErr.error.error) ||
        'No se pudo completar el registro. Revisá los datos ingresados.';
      this.errorMsg.set(msg);
      this.loading.set(false);
    }
  }
}
