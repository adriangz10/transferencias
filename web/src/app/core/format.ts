import { Pipe, PipeTransform } from '@angular/core';

const moneyFmt = new Intl.NumberFormat('es-AR', {
  style: 'currency',
  currency: 'ARS',
  minimumFractionDigits: 2,
});

export function money(v: unknown): string {
  const n = Number(v);
  return moneyFmt.format(Number.isFinite(n) ? n : 0);
}

export function hour(v: string | Date | null | undefined): string {
  if (!v) return '';
  const d = new Date(v);
  return isNaN(d.getTime()) ? '' : d.toLocaleTimeString('es-AR', { hour: '2-digit', minute: '2-digit', hour12: false });
}

/** "Hoy 14:05", "Ayer 09:10" o "26/10 14:05" */
export function when(v: string | Date | null | undefined): string {
  if (!v) return '';
  const d = new Date(v);
  if (isNaN(d.getTime())) return '';
  const today = new Date();
  const yesterday = new Date();
  yesterday.setDate(today.getDate() - 1);
  if (d.toDateString() === today.toDateString()) return 'Hoy ' + hour(d);
  if (d.toDateString() === yesterday.toDateString()) return 'Ayer ' + hour(d);
  const sameYear = d.getFullYear() === today.getFullYear();
  return (
    d.toLocaleDateString('es-AR', { day: '2-digit', month: '2-digit', year: sameYear ? undefined : '2-digit' }) +
    ' ' +
    hour(d)
  );
}

/** Fecha local en formato AAAA-MM-DD (para el filtro y el input type=date). */
export function isoDay(d: Date): string {
  const p = (n: number) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`;
}

/** Texto con formato de WhatsApp (*negrita*) a HTML seguro. Angular sanitiza el resultado igual. */
export function waText(s: string | null | undefined): string {
  const esc = String(s ?? '')
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');
  return esc
    .replace(/\*(.*?)\*/g, '<b>$1</b>')
    .replace(/_(.*?)_/g, '<i>$1</i>')
    .replace(/\n/g, '<br>');
}

@Pipe({ name: 'money' })
export class MoneyPipe implements PipeTransform {
  transform = money;
}

@Pipe({ name: 'when' })
export class WhenPipe implements PipeTransform {
  transform = when;
}

@Pipe({ name: 'hour' })
export class HourPipe implements PipeTransform {
  transform = hour;
}
