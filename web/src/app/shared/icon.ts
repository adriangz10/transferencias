import { ChangeDetectionStrategy, Component, computed, input } from '@angular/core';

// Íconos de trazo fino (24x24). Cada entrada son uno o más paths "d".
const ICONS: Record<string, string[]> = {
  check: ['M5 12.5l4.5 4.5L19 7.5'],
  x: ['M6 6l12 12', 'M18 6L6 18'],
  alert: ['M12 9v4', 'M12 17h.01', 'M10.3 3.9L1.8 18a2 2 0 001.7 3h17a2 2 0 001.7-3L13.7 3.9a2 2 0 00-3.4 0z'],
  clock: ['M12 7v5l3 2', 'M21 12a9 9 0 11-18 0 9 9 0 0118 0z'],
  sun: [
    'M12 3v1.5', 'M12 19.5V21', 'M4.2 4.2l1.1 1.1', 'M18.7 18.7l1.1 1.1', 'M3 12h1.5', 'M19.5 12H21',
    'M4.2 19.8l1.1-1.1', 'M18.7 5.3l1.1-1.1', 'M16 12a4 4 0 11-8 0 4 4 0 018 0z',
  ],
  moon: ['M20 14.5A8 8 0 019.5 4a8 8 0 1010.5 10.5z'],
  trash: ['M4 7h16', 'M10 11v6', 'M14 11v6', 'M5 7l1 12a2 2 0 002 2h8a2 2 0 002-2l1-12', 'M9 7V4h6v3'],
  download: ['M12 4v11', 'M7 10l5 5 5-5', 'M5 20h14'],
  upload: ['M12 20V9', 'M7 14l5-5 5 5', 'M5 4h14'],
  chevronLeft: ['M15 6l-6 6 6 6'],
  chevronRight: ['M9 6l6 6-6 6'],
  chevronDown: ['M6 9l6 6 6-6'],
  logout: ['M15 4h3a2 2 0 012 2v12a2 2 0 01-2 2h-3', 'M10 17l-5-5 5-5', 'M5 12h11'],
  calendar: ['M4 7a2 2 0 012-2h12a2 2 0 012 2v12a2 2 0 01-2 2H6a2 2 0 01-2-2V7z', 'M4 10h16', 'M8 3v4', 'M16 3v4'],
  file: ['M14 3H7a2 2 0 00-2 2v14a2 2 0 002 2h10a2 2 0 002-2V8l-5-5z', 'M14 3v5h5'],
  copy: ['M9 9h10v10H9z', 'M5 15V5h10'],
  plus: ['M12 5v14', 'M5 12h14'],
  link: ['M10 14a4 4 0 005.7 0l3-3a4 4 0 00-5.7-5.7l-1 1', 'M14 10a4 4 0 00-5.7 0l-3 3a4 4 0 005.7 5.7l1-1'],
  dot: ['M12 12h.01'],
};

@Component({
  selector: 'app-icon',
  changeDetection: ChangeDetectionStrategy.OnPush,
  host: { class: 'inline-flex shrink-0', 'aria-hidden': 'true' },
  template: `
    <svg
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      [attr.stroke-width]="stroke()"
      stroke-linecap="round"
      stroke-linejoin="round"
      [attr.width]="size()"
      [attr.height]="size()"
    >
      @for (d of paths(); track $index) {
        <path [attr.d]="d" />
      }
    </svg>
  `,
})
export class Icon {
  readonly name = input.required<string>();
  readonly size = input(16);
  readonly stroke = input(1.75);
  protected readonly paths = computed(() => ICONS[this.name()] ?? []);
}
