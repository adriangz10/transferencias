package persistence

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// RunMigrations aplica en orden los archivos .sql de dir que todavía no figuran en schema_migrations.
// db debe abrirse con multiStatements=true y usarse solo para migrar (los archivos pueden hacer USE).
func RunMigrations(ctx context.Context, db *sql.DB, dir string) ([]string, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no se encontraron migraciones en %s", dir)
	}
	sort.Strings(files)

	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version VARCHAR(150) PRIMARY KEY,
		applied_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	) ENGINE=InnoDB`); err != nil {
		return nil, fmt.Errorf("creando schema_migrations: %w", err)
	}

	var applied []string
	for _, f := range files {
		version := filepath.Base(f)
		var one int
		err := db.QueryRowContext(ctx, `SELECT 1 FROM schema_migrations WHERE version = ?`, version).Scan(&one)
		if err == nil {
			continue
		}
		if err != sql.ErrNoRows {
			return applied, err
		}

		body, err := os.ReadFile(f)
		if err != nil {
			return applied, err
		}
		if strings.TrimSpace(string(body)) == "" {
			continue
		}
		if _, err := db.ExecContext(ctx, string(body)); err != nil {
			return applied, fmt.Errorf("migración %s: %w", version, err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations (version) VALUES (?)`, version); err != nil {
			return applied, err
		}
		applied = append(applied, version)
	}
	return applied, nil
}
