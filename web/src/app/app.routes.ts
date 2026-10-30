import { Routes } from '@angular/router';
import { Shell } from './layout/shell';

export const routes: Routes = [
  {
    path: 'login',
    title: 'Ingresar · VeriPago',
    loadComponent: () => import('./pages/login/login').then((m) => m.LoginPage),
  },
  {
    path: 'registro',
    title: 'Registrar comercio · VeriPago',
    loadComponent: () => import('./pages/register/register').then((m) => m.RegisterPage),
  },
  {
    path: '',
    component: Shell,
    children: [
      {
        path: '',
        title: 'VeriPago',
        loadComponent: () => import('./pages/home/home').then((m) => m.HomePage),
      },
      {
        path: 'clientes',
        title: 'Clientes · VeriPago',
        loadComponent: () => import('./pages/customers/customers').then((m) => m.CustomersPage),
      },
      {
        path: 'clientes/:id',
        title: 'Cliente · VeriPago',
        loadComponent: () => import('./pages/customers/customer-detail').then((m) => m.CustomerDetailPage),
      },
      {
        path: 'cobrar',
        title: 'Cobrar · VeriPago',
        loadComponent: () => import('./pages/charge/charge').then((m) => m.ChargePage),
      },
      {
        path: 'configuracion',
        title: 'Configuración · VeriPago',
        loadComponent: () => import('./pages/settings/settings').then((m) => m.SettingsPage),
      },
      {
        path: 'pruebas',
        title: 'Pruebas · VeriPago',
        loadComponent: () => import('./pages/demo/demo').then((m) => m.DemoPage),
      },
      {
        path: 'superadmin',
        title: 'Super-Admin · VeriPago',
        loadComponent: () => import('./pages/superadmin/superadmin').then((m) => m.SuperAdminPage),
      },
    ],
  },
  { path: '**', redirectTo: '' },
];
