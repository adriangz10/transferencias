package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// DemoMasterKey es la clave pública de ejemplo. Nunca debe usarse en producción.
const DemoMasterKey = "VeriPagoMasterKey32BytesSecret!!"

// Config almacena las variables de entorno tipadas del sistema VeriPago SaaS.
type Config struct {
	Port          string
	PublicBaseURL string

	// DemoMode habilita el simulador, los datos de demostración, los pagos simulados
	// y el respaldo en memoria. Nunca activarlo en producción.
	DemoMode bool

	// Acceso al panel web y a /api/v1 (HTTP Basic Auth)
	AdminUser     string
	AdminPassword string

	// Base de datos MySQL 8.0 (InnoDB ACID)
	DBHost       string
	DBPort       string
	DBUser       string
	DBPassword   string
	DBName       string
	DBRootDSN    string // sin base seleccionada, para crearla (multiStatements)
	DBDSN        string // conexión de la aplicación (sin multiStatements)
	DBMigrateDSN string // conexión solo para migraciones (multiStatements)

	// Redis (Cola de Espera Reactiva y Centavos Únicos)
	RedisAddr     string
	RedisPassword string
	RedisDB       int

	// Seguridad Criptográfica (AES-256-GCM Token Vault)
	MasterKey string // Exactamente 32 bytes

	// Google Gemini (Extractor Forense Multimodal)
	GeminiAPIKey string
	GeminiModel  string

	// Mercado Pago API & OAuth 2.0
	MPClientID            string
	MPClientSecret        string
	MPRedirectURI         string
	MPWebhookSecret       string
	MPPlatformAccessToken string  // Token propio de la plataforma SaaS para cobrar suscripciones (Checkout Pro)
	USDToARSRate          float64 // Tipo de cambio USD -> ARS para las suscripciones (default 1200)

	// Meta WhatsApp Cloud API
	WAVerifyToken   string
	WAAppSecret     string
	WAAccessToken   string
	WAPhoneNumberID string
}

// Load carga la configuración desde archivo .env (si existe) y variables de entorno.
func Load() *Config {
	loadDotEnv(".env")

	demo := parseBool(getEnv("DEMO_MODE", "false"))

	// En modo demo hay valores de ejemplo para poder probar sin configurar nada;
	// fuera de él, los secretos no tienen valor por defecto.
	demoDefault := func(v string) string {
		if demo {
			return v
		}
		return ""
	}

	port := getEnv("PORT", "8080")
	dbHost := getEnv("DB_HOST", "127.0.0.1")
	dbPort := getEnv("DB_PORT", "3306")
	dbUser := getEnv("DB_USER", "")
	dbPassword := getEnv("DB_PASSWORD", "")
	dbName := getEnv("DB_NAME", "veripago_db")
	const dsnParams = "parseTime=true&loc=Local&charset=utf8mb4&collation=utf8mb4_unicode_ci"
	dbRootDSN := dbUser + ":" + dbPassword + "@tcp(" + dbHost + ":" + dbPort + ")/?" + dsnParams + "&multiStatements=true"
	dbDSN := getEnv("DB_DSN", "")
	if dbDSN == "" {
		dbDSN = dbUser + ":" + dbPassword + "@tcp(" + dbHost + ":" + dbPort + ")/" + dbName + "?" + dsnParams
	}
	migrateDSN := dbDSN
	if !strings.Contains(migrateDSN, "multiStatements=") {
		if strings.Contains(migrateDSN, "?") {
			migrateDSN += "&multiStatements=true"
		} else {
			migrateDSN += "?multiStatements=true"
		}
	}

	redisDB, _ := strconv.Atoi(getEnv("REDIS_DB", "0"))
	usdRate, err := strconv.ParseFloat(getEnv("USD_ARS_RATE", "1200"), 64)
	if err != nil || usdRate <= 0 {
		usdRate = 1200
	}

	return &Config{
		Port:                  port,
		PublicBaseURL:         getEnv("PUBLIC_BASE_URL", "http://localhost:"+port),
		DemoMode:              demo,
		AdminUser:             getEnv("ADMIN_USER", "admin"),
		AdminPassword:         getEnv("ADMIN_PASSWORD", ""),
		DBHost:                dbHost,
		DBPort:                dbPort,
		DBUser:                dbUser,
		DBPassword:            dbPassword,
		DBName:                dbName,
		DBRootDSN:             dbRootDSN,
		DBDSN:                 dbDSN,
		DBMigrateDSN:          migrateDSN,
		RedisAddr:             getEnv("REDIS_ADDR", "127.0.0.1:6379"),
		RedisPassword:         getEnv("REDIS_PASSWORD", ""),
		RedisDB:               redisDB,
		MasterKey:             getEnv("MASTER_KEY", demoDefault(DemoMasterKey)),
		GeminiAPIKey:          getEnv("GEMINI_API_KEY", ""),
		GeminiModel:           getEnv("GEMINI_MODEL", "gemini-3.8-flash"),
		MPClientID:            getEnv("MP_CLIENT_ID", demoDefault("MP_DEMO_CLIENT_ID")),
		MPClientSecret:        getEnv("MP_CLIENT_SECRET", demoDefault("MP_DEMO_CLIENT_SECRET")),
		MPRedirectURI:         getEnv("MP_REDIRECT_URI", "http://localhost:"+port+"/oauth/mp/callback"),
		MPWebhookSecret:       getEnv("MP_WEBHOOK_SECRET", demoDefault("veripago_mp_webhook_secret_demo")),
		MPPlatformAccessToken: getEnv("MP_PLATFORM_ACCESS_TOKEN", ""),
		USDToARSRate:          usdRate,
		WAVerifyToken:         getEnv("WA_VERIFY_TOKEN", demoDefault("veripago_wa_verify_token")),
		WAAppSecret:           getEnv("WA_APP_SECRET", ""),
		WAAccessToken:         getEnv("WA_ACCESS_TOKEN", ""),
		WAPhoneNumberID:       getEnv("WA_PHONE_NUMBER_ID", ""),
	}
}

// Validate devuelve los errores que impiden arrancar y las advertencias a mostrar.
func (c *Config) Validate() (errs []string, warnings []string) {
	if len(c.MasterKey) != 32 {
		errs = append(errs, fmt.Sprintf("MASTER_KEY debe tener exactamente 32 bytes (tiene %d)", len(c.MasterKey)))
	}
	if c.DemoMode {
		warnings = append(warnings, "DEMO_MODE=true: simulador, datos de prueba y pagos simulados ACTIVOS. No usar en producción.")
		if c.AdminPassword == "" {
			warnings = append(warnings, "ADMIN_PASSWORD vacío: el panel queda abierto (solo aceptable en modo demo local).")
		}
		return errs, warnings
	}

	if c.AdminPassword == "" {
		errs = append(errs, "ADMIN_PASSWORD es obligatorio fuera del modo demo (protege el panel y /api/v1)")
	} else if len(c.AdminPassword) < 12 {
		warnings = append(warnings, "ADMIN_PASSWORD tiene menos de 12 caracteres")
	}
	if c.MasterKey == DemoMasterKey {
		warnings = append(warnings, "MASTER_KEY es la clave pública de ejemplo: cambiala y volvé a vincular Mercado Pago (los tokens guardados quedarán ilegibles)")
	}
	if c.DBUser == "" {
		errs = append(errs, "DB_USER es obligatorio fuera del modo demo")
	}
	if c.GeminiAPIKey == "" {
		errs = append(errs, "GEMINI_API_KEY es obligatorio fuera del modo demo")
	}
	if c.WAAppSecret == "" {
		warnings = append(warnings, "WA_APP_SECRET vacío: se rechazarán todos los webhooks de WhatsApp")
	}
	if c.WAVerifyToken == "" {
		warnings = append(warnings, "WA_VERIFY_TOKEN vacío: no se podrá verificar el webhook de WhatsApp en Meta")
	}
	if c.MPWebhookSecret == "" {
		warnings = append(warnings, "MP_WEBHOOK_SECRET vacío: se rechazarán todos los webhooks de Mercado Pago")
	}
	if c.MPClientID == "" || c.MPClientID == "MP_DEMO_CLIENT_ID" {
		warnings = append(warnings, "MP_CLIENT_ID no configurado: no se podrá vincular Mercado Pago por OAuth")
	}
	return errs, warnings
}

func parseBool(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "si", "sí", "on":
		return true
	}
	return false
}

func getEnv(key, fallback string) string {
	if val, ok := os.LookupEnv(key); ok && strings.TrimSpace(val) != "" {
		return strings.TrimSpace(val)
	}
	return fallback
}

func loadDotEnv(filename string) {
	f, err := os.Open(filename)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.Trim(strings.TrimSpace(parts[1]), `"'`)
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, val)
		}
	}
}
