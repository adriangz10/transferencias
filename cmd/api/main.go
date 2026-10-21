package main

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"veripago-saas/internal/application"
	"veripago-saas/internal/config"
	"veripago-saas/internal/infrastructure/ai"
	"veripago-saas/internal/infrastructure/crypto"
	"veripago-saas/internal/infrastructure/mp"
	"veripago-saas/internal/infrastructure/persistence"
	"veripago-saas/internal/infrastructure/queue"
	"veripago-saas/internal/infrastructure/whatsapp"
	httpInterface "veripago-saas/internal/interfaces/http"
)

const migrationsDir = "migrations"

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	cfg := config.Load()

	errs, warnings := cfg.Validate()
	for _, w := range warnings {
		log.Printf("⚠️ %s", w)
	}
	if len(errs) > 0 {
		for _, e := range errs {
			log.Printf("❌ %s", e)
		}
		log.Fatalf("Configuración inválida: corregí el .env (o usá DEMO_MODE=true solo para pruebas locales)")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1. TokenVault AES-256-GCM (32 bytes)
	vault, err := crypto.NewTokenVault(cfg.MasterKey)
	if err != nil {
		log.Fatalf("Error inicializando TokenVault AES-256-GCM: %v", err)
	}

	// 2. MySQL: crear la base si no existe, migrar y conectar
	activeDB, dbErr := connectMySQL(ctx, cfg)
	if dbErr != nil {
		if !cfg.DemoMode {
			log.Fatalf("❌ MySQL no disponible (%v). Fuera del modo demo no se arranca sin base de datos: se perdería la protección contra doble uso.", dbErr)
		}
		log.Printf("⚠️ MySQL no disponible (%v) — modo demo: usando almacenamiento en memoria (se pierde al reiniciar)", dbErr)
	}
	repo := persistence.NewMySQLRepository(activeDB)

	// 3. Redis (Cola de Espera Reactiva y órdenes /cobrar)
	queueStore := queue.NewRedisQueueStore(cfg.RedisAddr, cfg.RedisPassword, cfg.RedisDB)
	if queueStore.IsUsingRedis() {
		log.Printf("✅ Conectado a Redis en %s", cfg.RedisAddr)
	} else {
		log.Printf("⚠️ Redis no disponible en %s — la Cola de Espera y las órdenes /cobrar quedan en memoria (se pierden al reiniciar)", cfg.RedisAddr)
	}

	// 4. Extractor forense Gemini
	extractor, err := ai.NewGeminiReceiptExtractor(ctx, cfg.GeminiAPIKey, cfg.GeminiModel, cfg.DemoMode)
	if err != nil {
		log.Fatalf("Error inicializando GeminiReceiptExtractor: %v", err)
	}
	if extractor.HasLiveAPIKey() {
		log.Printf("✅ Extractor forense Gemini (%s) inicializado", cfg.GeminiModel)
	} else {
		log.Printf("ℹ️ GEMINI_API_KEY no definida — modo demo: extractor local de prueba")
	}

	// 5. Clientes de Mercado Pago y WhatsApp Cloud API
	mpClient := mp.NewMPClient(cfg.DemoMode)
	waClient := whatsapp.NewClient(cfg.WAAccessToken, cfg.WAPhoneNumberID)

	// 6. Datos de demostración SOLO en modo demo. Fuera de él, se desactiva la cuenta MP demo
	// que pudo haber quedado de arranques anteriores para que no tape a la cuenta real.
	if cfg.DemoMode {
		encAccess, _ := vault.Encrypt("DEMO_MP_ACCESS_TOKEN_DON_MATEO")
		encRefresh, _ := vault.Encrypt("DEMO_MP_REFRESH_TOKEN_DON_MATEO")
		if err := repo.SeedDefaultDemoData(ctx, encAccess, encRefresh, cfg.MPWebhookSecret); err != nil {
			log.Printf("Advertencia sembrando datos de demostración: %v", err)
		}
	} else if err := repo.RevokeDemoMPAccount(ctx); err != nil {
		log.Printf("⚠️ No se pudo desactivar la cuenta MP de demostración: %v", err)
	}

	// 7. Caso de uso principal de conciliación y antifraude
	verifyUC := application.NewVerifyReceiptUseCase(repo, extractor, mpClient)

	// 8. Router HTTP
	mux := http.NewServeMux()
	webhooksHandler := httpInterface.NewWebhooksHandler(cfg, repo, vault, extractor, mpClient, waClient, queueStore, verifyUC)
	webhooksHandler.RegisterRoutes(mux)
	webhooksHandler.StartBackgroundJobs()

	server := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      90 * time.Second, // la subida desde el panel espera el análisis completo
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	go func() {
		log.Printf("🚀 VeriPago SaaS corriendo en http://localhost:%s (demo=%v)", cfg.Port, cfg.DemoMode)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Error en servidor HTTP: %v", err)
		}
	}()

	// Apagado ordenado: dejamos terminar los comprobantes en proceso
	stopChan := make(chan os.Signal, 1)
	signal.Notify(stopChan, os.Interrupt, syscall.SIGTERM)
	<-stopChan

	log.Println("🛑 Apagando servidor VeriPago SaaS...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()
	_ = server.Shutdown(shutdownCtx)
	if err := webhooksHandler.Shutdown(shutdownCtx); err != nil {
		log.Printf("⚠️ Algunos procesamientos no terminaron a tiempo: %v", err)
	}
	if activeDB != nil {
		_ = activeDB.Close()
	}
}

// connectMySQL crea la base (si hace falta), aplica las migraciones pendientes y abre el pool de la app.
func connectMySQL(ctx context.Context, cfg *config.Config) (*sql.DB, error) {
	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	// a) Crear la base con la migración inicial (sin base seleccionada)
	if rootDB, err := sql.Open("mysql", cfg.DBRootDSN); err == nil {
		if rootDB.PingContext(pingCtx) == nil {
			if body, err := os.ReadFile(migrationsDir + "/0001_init_schema.sql"); err == nil {
				if _, err := rootDB.ExecContext(ctx, string(body)); err != nil {
					log.Printf("Nota creando la base inicial: %v", err)
				}
			}
		}
		_ = rootDB.Close()
	}

	// b) Migraciones pendientes con una conexión dedicada (multiStatements)
	migDB, err := sql.Open("mysql", cfg.DBMigrateDSN)
	if err != nil {
		return nil, err
	}
	migDB.SetMaxOpenConns(1)
	if err := migDB.PingContext(pingCtx); err != nil {
		_ = migDB.Close()
		return nil, err
	}
	applied, migErr := persistence.RunMigrations(ctx, migDB, migrationsDir)
	_ = migDB.Close()
	for _, v := range applied {
		log.Printf("✅ Migración aplicada: %s", v)
	}
	if migErr != nil {
		return nil, migErr
	}

	// c) Pool de la aplicación (sin multiStatements)
	db, err := sql.Open("mysql", cfg.DBDSN)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(5 * time.Minute)
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, err
	}
	log.Printf("✅ Conectado a MySQL en %s:%s/%s", cfg.DBHost, cfg.DBPort, cfg.DBName)
	return db, nil
}
