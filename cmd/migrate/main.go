// Command migrate runs database schema migrations and reference-data seeding
// against the configured MySQL database, then exits. It reuses the exact same
// GORM migration/seed logic the api-gateway runs on startup, so this is purely
// a standalone entrypoint for CI / pre-deploy steps (e.g. migrate AWS RDS
// before rolling out api + worker).
//
// Connection settings come from the same CODERUNTIME_* env vars / config.yaml
// as the other services. Usage:
//
//	migrate              # migrate schema + seed statuses & languages (default)
//	migrate -mode migrate  # schema migrations only
//	migrate -mode seed     # seed statuses & languages only
package main

import (
	"flag"
	"fmt"
	"os"

	"go.uber.org/zap"

	"github.com/revature/corems-code-executor/internal/config"
	"github.com/revature/corems-code-executor/internal/database"
)

func main() {
	mode := flag.String("mode", "all", "what to run: all | migrate | seed")
	flag.Parse()

	if err := run(*mode); err != nil {
		fmt.Fprintln(os.Stderr, "migrate: fatal:", err)
		os.Exit(1)
	}
}

func run(mode string) error {
	switch mode {
	case "all", "migrate", "seed":
		// valid
	default:
		return fmt.Errorf("unknown -mode %q (want: all | migrate | seed)", mode)
	}

	log, err := zap.NewProduction()
	if err != nil {
		return fmt.Errorf("build logger: %w", err)
	}
	defer func() { _ = log.Sync() }()

	cfg, err := config.LoadConfig(os.Getenv("CONFIG_PATH"))
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	db, err := database.Connect(cfg, log)
	if err != nil {
		return fmt.Errorf("connect database: %w", err)
	}
	defer func() { _ = db.Close() }()

	switch mode {
	case "all":
		if err := database.MigrateAndSeed(db); err != nil {
			return fmt.Errorf("migrate and seed: %w", err)
		}
		log.Info("migrate: schema migrated and reference data seeded")
	case "migrate":
		if err := database.RunMigrations(db); err != nil {
			return fmt.Errorf("run migrations: %w", err)
		}
		log.Info("migrate: schema migrated")
	case "seed":
		if err := database.SeedStatuses(db); err != nil {
			return fmt.Errorf("seed statuses: %w", err)
		}
		if err := database.SeedLanguages(db); err != nil {
			return fmt.Errorf("seed languages: %w", err)
		}
		log.Info("migrate: reference data seeded")
	default:
		return fmt.Errorf("unknown -mode %q (want: all | migrate | seed)", mode)
	}

	return nil
}
