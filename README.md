# 🛡️ VeriPago SaaS — Prevención de Fraude y Conciliación Instantánea de Transferencias

Implementación completa de **VeriPago SaaS** en **Go (Arquitectura Hexagonal)** según el *Documento de Especificación Maestra y Código Base (PRD & SDD)*.

## 🚀 Inicio Rápido

Copiá `.env.example` a `.env` y completalo. El sistema tiene dos modos:

### Modo demo (pruebas locales, sin APIs externas)
```bash
DEMO_MODE=true go run ./cmd/api
```
Habilita el simulador, los datos de prueba, los pagos simulados y el respaldo en memoria si no hay MySQL/Redis.
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
Después de vincular Mercado Pago, cargá el **alias y el CVU** de cobro en *Empleados & Planes → Cuenta de Cobro*.

Las migraciones de `migrations/` se aplican solas al arrancar (quedan registradas en `schema_migrations`).

### Reglas de conciliación
1. **Archivo repetido:** solo cuenta si ese mismo archivo ya *verificó* una venta (reenviar una foto tras "no impactó" no es fraude).
2. **Comprobante con ID** (COELSA, o número de operación de un comprobante de Mercado Pago): solo se valida con *ese* pago. Si no aparece, queda en espera 3 minutos; nunca se busca otro pago "parecido" por monto. Si el ID existe pero el monto difiere → `REJECTED_AMOUNT_MISMATCH`.
3. **Comprobante sin ID verificable:** monto exacto dentro de la ventana de tiempo y, si ambos lo informan, mismo CUIT del pagador.
4. **Destinatario:** se compara contra el alias *y* el CBU/CVU (también enmascarado, ej. `0000003100****4821`).

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
