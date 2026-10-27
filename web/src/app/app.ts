import { ChangeDetectionStrategy, Component } from '@angular/core';
import { RouterOutlet } from '@angular/router';
import { ConfirmDialog } from './shared/confirm-dialog';

@Component({
  selector: 'app-root',
  imports: [RouterOutlet, ConfirmDialog],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `<router-outlet /><app-confirm-dialog />`,
})
export class App {}
