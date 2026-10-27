# 🛡️ VeriPago SaaS — Prevención de Fraude y Conciliación Instantánea de Transferencias

Implementación completa de **VeriPago SaaS** en **Go (Arquitectura Hexagonal)** según el *Documento de Especificación Maestra y Código Base (PRD & SDD)*.

## 🚀 Inicio Rápido

Copiá `.env.example` a `.env` y completalo. El sistema tiene dos modos:

### Modo demo (pruebas locales, sin APIs externas)
```bash
DEMO_MODE=true go run ./cmd/api
```
Habilita el simulador y los pagos simulados. Trabaja **solo en memoria** (no toca MySQL ni Redis) y arranca vacío:
la vinculación con Mercado Pago es simulada y el comercio, el alias/CVU y los empleados se cargan desde el panel.
**Nunca usarlo con clientes reales**: cualquiera con acceso al panel puede "acreditar" pagos simulados.

### Modo producción (`DEMO_MODE=false`, el valor por defecto)
```bash
docker compose up -d   # MySQL 8.0 + Redis 7 (solo escuchan en 127.0.0.1)
go run ./cmd/api
```
Requisitos para arrancar: `ADMIN_PASSWORD`, `MASTER_KEY` de 32 bytes, `DB_USER`/`DB_PASSWORD`, `GEMINI_API_KEY` y MySQL disponible.
Para recibir mensajes y pagos hacen falta además `WA_APP_SECRET`, `WA_VERIFY_TOKEN`, `MP_WEBHOOK_SECRET` y `MP_CLIENT_ID`/`MP_CLIENT_SECRET`.

Panel: 👉 **[http://localhost:8080](http://localhost:8080)**. Te pide iniciar sesión con `ADMIN_USER` / `ADMIN_PASSWORD`.
La sesión dura 12 horas (o hasta reiniciar el servidor). Después de 5 intentos fallidos, esa IP queda bloqueada 15 minutos.
No hay datos precargados: en el primer arranque se crea un comercio vacío. Desde *Configuración* cargá
los **datos del comercio**, vinculá **Mercado Pago**, el **alias y el CVU** de cobro y las **personas autorizadas** de WhatsApp.

Las migraciones de `migrations/` se aplican solas al arrancar (quedan registradas en `schema_migrations`).

### Base de datos
Esquema normalizado (MySQL 8, InnoDB, claves foráneas):

| Tabla | Qué guarda |
|---|---|
| `tenants` | el comercio (nombre, CUIT y email opcionales, plan y cuota) |
| `mp_accounts` | cuentas de Mercado Pago vinculadas (tokens encriptados) y alias/CVU de cobro |
| `authorized_senders` | números de WhatsApp que pueden enviar comprobantes |
| `customers` | un registro por cliente y comercio, identificado por CUIT (o nombre normalizado si no hay CUIT) |
| `mp_payments` | cada ingreso a Mercado Pago, con `customer_id` |
| `receipt_validations` | cada comprobante analizado (lo que dice el comprobante, como evidencia) con `customer_id` |

Los totales por cliente (correctos, fraudes, rechazos, monto cobrado) no se guardan: se calculan con consultas
indexadas por `(tenant_id, customer_id)`, así nunca quedan desactualizados.

### Panel web (Angular + Tailwind)
El panel está en `web/` y se compila **dentro** del binario de Go (`internal/interfaces/http/panel`).
Después de cambiar algo en `web/`:
```bash
cd web && npm install && npm run build   # actualiza internal/interfaces/http/panel
cd .. && go build ./cmd/api
```
Para desarrollar con recarga automática: dejá el servidor de Go corriendo en `:8080` y en otra terminal
`cd web && npm start` → [http://localhost:4200](http://localhost:4200) (la API se reenvía a Go).

### Reglas de conciliación
1. **Archivo repetido:** solo cuenta si ese mismo archivo ya *verificó* una venta (reenviar una foto tras "no impactó" no es fraude).
2. **Comprobante con ID** (COELSA, o número de operación de un comprobante de Mercado Pago): solo se valida con *ese* pago. Si no aparece, queda en espera 3 minutos; nunca se busca otro pago "parecido" por monto. Si el ID existe pero el monto difiere → `REJECTED_AMOUNT_MISMATCH`.
3. **Comprobante sin ID verificable:** monto exacto dentro de la ventana de tiempo y, si ambos lo informan, mismo CUIT del pagador.
4. **Destinatario:** se compara contra el alias *y* el CBU/CVU (también enmascarado, ej. `0000003100****4821`).
   Si no coincide pero es *casi* igual (1–2 dígitos/letras cambiados) o el CVU no pasa los dígitos verificadores → `FRAUD_TAMPERED_RECEIPT` (comprobante adulterado), no "otra cuenta".
5. **Mismo comprobante en otro formato** (PDF y captura): aunque no se lea el número de operación, se reconoce por emisor (CUIT o nombre) + monto (+ hora si se ve) y se responde "comprobante ya enviado".
6. **Cruce con el historial del cliente:** cada comprobante se asocia a su cliente (por CUIT, o por nombre si no hay CUIT) y se avisa por WhatsApp y en el panel si ese cliente tiene intentos de fraude previos, varios rechazos en las últimas 24 h, si su CUIT apareció antes con otro nombre o si no coincide con el pagador que informa Mercado Pago.

---

## 🏗️ Estructura del Proyecto (Arquitectura Hexagonal)

```text
veripago-saas/
├── cmd/
│   └── api/
│       └── main.go                       # Punto de entrada: Servidor HTTP + Workers
├── internal/
│   ├── config/
│   │   └── config.go                     # Variables de entorno tipadas (.env)
│   ├── domain/
│   │   ├── models.go                     # Entidades de negocio (Tenant, MPPayment, ExtractedReceiptData)
│   │   └── ports.go                      # Interfaces para desacoplar infraestructura
│   ├── application/
│   │   ├── verify_receipt_uc.go          # Caso de uso principal (4 Escudos + Auditoría)
│   │   └── verify_receipt_uc_test.go     # Tests de los 4 escudos y concurrencia
│   ├── infrastructure/
│   │   ├── crypto/
│   │   │   ├── aes_gcm.go                # Encriptación de Tokens OAuth (AES-256-GCM)
│   │   │   └── aes_gcm_test.go           # Tests criptográficos de TokenVault
│   │   ├── ai/
│   │   │   └── gemini_extractor.go       # Integración Gemini 2.5 Flash con ResponseSchema JSON
│   │   ├── mp/
│   │   │   └── client.go                 # Cliente HTTP Mercado Pago + Search + OAuth 2.0
│   │   ├── persistence/
│   │   │   └── mysql_repo.go             # Repositorio MySQL con transacciones SELECT ... FOR UPDATE
│   │   ├── queue/
│   │   │   └── redis_queue.go            # Cola de Espera Reactiva (3m) + /cobrar Centavos Únicos
│   │   └── whatsapp/
│   │       └── client.go                 # Cliente Meta WhatsApp Cloud API
│   └── interfaces/
│       └── http/
│           ├── webhooks_handler.go       # Endpoints WhatsApp, Mercado Pago, OAuth, API y CSV
│           ├── middleware_hmac.go        # Validación criptográfica HMAC-SHA256 de Webhooks
│           ├── middleware_hmac_test.go   # Tests de validación de firma x-signature
│           └── ui_handler.go             # Panel Web B2B, Posnet Virtual y Simulador
├── migrations/
│   └── 0001_init_schema.sql              # Esquema transaccional completo MySQL 8.0
├── docker-compose.yml                    # MySQL 8.0 + Redis 7
├── .env.example                          # Plantilla de configuración
└── go.mod
```
