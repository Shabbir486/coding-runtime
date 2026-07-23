package database

import (
	"fmt"

	"go.uber.org/zap"
	"gorm.io/gorm/clause"

	"github.com/revature/corems-code-executor/internal/models"
)

// indexSpec describes a secondary index to create if it does not already exist.
type indexSpec struct {
	table   string
	name    string
	columns string // comma-separated column list, e.g. "status_id, created_at"
}

// ensureIndex creates the index only when it is absent. MySQL lacks
// `CREATE INDEX IF NOT EXISTS`, so existence is checked via information_schema
// against the current schema (DATABASE()).
func ensureIndex(db *DB, idx indexSpec) error {
	var count int64
	if err := db.DB.Raw(
		`SELECT COUNT(1) FROM information_schema.statistics
		 WHERE table_schema = DATABASE() AND table_name = ? AND index_name = ?`,
		idx.table, idx.name,
	).Scan(&count).Error; err != nil {
		return fmt.Errorf("check index existence: %w", err)
	}
	if count > 0 {
		return nil
	}
	stmt := fmt.Sprintf("CREATE INDEX %s ON %s (%s)", idx.name, idx.table, idx.columns)
	if err := db.DB.Exec(stmt).Error; err != nil {
		return fmt.Errorf("create index %q: %w", stmt, err)
	}
	return nil
}

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
		&models.Webhook{},
		&models.Batch{},
	); err != nil {
		return fmt.Errorf("migrations: auto-migrate failed: %w", err)
	}

	// Secondary indexes GORM does not declare via tags. MySQL has no
	// `CREATE INDEX IF NOT EXISTS` and no partial (WHERE) indexes, so each
	// index is created only when absent (checked via information_schema) and
	// the partial predicates are dropped — the columns are still indexed, the
	// optimiser simply scans the full index. Redundant ones (unique email,
	// unique key_hash, batch_id) are already covered by GORM `uniqueIndex` /
	// `index` tags and are intentionally omitted here.
	indexes := []indexSpec{
		// submissions – composite covering index for list queries ordered by time
		{table: "submissions", name: "idx_submissions_status_created", columns: "status_id, created_at"},
		// submissions – worker look-up
		{table: "submissions", name: "idx_submissions_worker", columns: "worker_id"},
		// execution_logs – token look-up with time ordering
		{table: "execution_logs", name: "idx_exec_logs_token_created", columns: "token, created_at"},
		// batches – list / sweep by creation time
		{table: "batches", name: "idx_batches_created", columns: "created_at"},
		// batches – bulk webhook re-delivery: filter by webhook_status, keyset on id
		{table: "batches", name: "idx_batches_webhook_status", columns: "webhook_status, id"},
		// batches – bulk start of pending batches: filter by status, keyset on id
		{table: "batches", name: "idx_batches_status", columns: "status, id"},
		// api_keys – expiry sweeper
		{table: "api_keys", name: "idx_api_keys_expires", columns: "expires_at"},
	}

	for _, idx := range indexes {
		if err := ensureIndex(db, idx); err != nil {
			return fmt.Errorf("migrations: ensure index %s: %w", idx.name, err)
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
	if err := SeedLanguages(db); err != nil {
		return err
	}
	return EnforceLanguageActive(db)
}

// EnforceLanguageActive makes the DB's is_active flags match the policy declared
// in models.DefaultLanguages(). It uses explicit UPDATEs rather than the upsert
// because GORM omits zero-value fields (is_active=false) from the insert, so
// `ON DUPLICATE KEY UPDATE is_active=VALUES(is_active)` can never set false.
// Running it on every startup keeps the enabled set source-driven.
func EnforceLanguageActive(db *DB) error {
	langs := models.DefaultLanguages()
	activeIDs := make([]int, 0, len(langs))
	for _, l := range langs {
		if l.IsActive {
			activeIDs = append(activeIDs, l.ID)
		}
	}
	// 1) Reset everything to inactive.
	if err := db.DB.Model(&models.Language{}).
		Where("id > 0").
		Update("is_active", false).Error; err != nil {
		return fmt.Errorf("migrations: reset language active flags: %w", err)
	}
	// 2) Activate exactly the policy's active set.
	if len(activeIDs) > 0 {
		if err := db.DB.Model(&models.Language{}).
			Where("id IN ?", activeIDs).
			Update("is_active", true).Error; err != nil {
			return fmt.Errorf("migrations: set active languages: %w", err)
		}
	}
	db.log.Info("language active policy enforced", zap.Int("active", len(activeIDs)))
	return nil
}
