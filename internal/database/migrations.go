package database

import (
	"fmt"

	"go.uber.org/zap"
	"gorm.io/gorm/clause"

	"github.com/mdshabbir-ali/code-runtime/internal/models"
)

// RunMigrations performs schema migration using GORM AutoMigrate followed by
// raw SQL statements that add indexes and constraints GORM cannot express.
func RunMigrations(db *DB) error {
	log := db.log
	log.Info("running database migrations")

	// GORM AutoMigrate creates / alters tables for all registered models.
	if err := db.DB.AutoMigrate(
		&models.Status{},
		&models.Language{},
		&models.User{},
		&models.APIKey{},
		&models.Submission{},
		&models.ExecutionLog{},
	); err != nil {
		return fmt.Errorf("migrations: auto-migrate failed: %w", err)
	}

	// Raw SQL for indexes and constraints GORM cannot express declaratively.
	statements := []string{
		// submissions – composite covering index for list queries ordered by time
		`CREATE INDEX IF NOT EXISTS idx_submissions_status_created
			ON submissions (status_id, created_at DESC)`,

		// submissions – partial index for pending / processing work (queue workers)
		`CREATE INDEX IF NOT EXISTS idx_submissions_pending
			ON submissions (created_at ASC)
			WHERE status_id IN (1, 2)`,

		// submissions – worker look-up (only rows with a worker assigned)
		`CREATE INDEX IF NOT EXISTS idx_submissions_worker
			ON submissions (worker_id)
			WHERE worker_id IS NOT NULL AND worker_id <> ''`,

		// execution_logs – token look-up with time ordering
		`CREATE INDEX IF NOT EXISTS idx_exec_logs_token_created
			ON execution_logs (token, created_at DESC)`,

		// api_keys – partial index for active keys only
		`CREATE INDEX IF NOT EXISTS idx_api_keys_active
			ON api_keys (key_hash)
			WHERE is_active = true`,

		// api_keys – expiry sweeper
		`CREATE INDEX IF NOT EXISTS idx_api_keys_expires
			ON api_keys (expires_at)
			WHERE expires_at IS NOT NULL`,

		// users – case-insensitive unique email index
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_users_email_lower
			ON users (LOWER(email))`,
	}

	for _, stmt := range statements {
		if err := db.DB.Exec(stmt).Error; err != nil {
			return fmt.Errorf("migrations: failed to execute SQL %q: %w", stmt, err)
		}
	}

	log.Info("database migrations completed successfully")
	return nil
}

// SeedStatuses inserts all well-known status rows using an upsert so the
// operation is idempotent and safe to call on every start-up.
func SeedStatuses(db *DB) error {
	statuses := models.DefaultStatuses()
	result := db.DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{"description"}),
	}).CreateInBatches(statuses, len(statuses))

	if result.Error != nil {
		return fmt.Errorf("migrations: seed statuses failed: %w", result.Error)
	}
	db.log.Info("statuses seeded", zap.Int64("rows_affected", result.RowsAffected))
	return nil
}

// SeedLanguages inserts all supported languages using an upsert so the
// operation is idempotent – safe to run on every start-up.
func SeedLanguages(db *DB) error {
	log := db.log
	log.Info("seeding languages")

	langs := models.DefaultLanguages()

	result := db.DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"name",
			"version",
			"source_file",
			"compile_command",
			"run_command",
			"image",
			"is_active",
			"max_memory",
			"max_cpu_time",
			"is_database",
			"db_type",
		}),
	}).CreateInBatches(langs, 10)

	if result.Error != nil {
		return fmt.Errorf("migrations: seed languages failed: %w", result.Error)
	}

	log.Info("languages seeded", zap.Int64("rows_affected", result.RowsAffected))
	return nil
}

// MigrateAndSeed is a convenience wrapper that runs migrations then seeds all
// reference data (statuses and languages).
func MigrateAndSeed(db *DB) error {
	if err := RunMigrations(db); err != nil {
		return err
	}
	if err := SeedStatuses(db); err != nil {
		return err
	}
	return SeedLanguages(db)
}
