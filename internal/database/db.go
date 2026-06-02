package database

import (
	"context"

	"go.uber.org/zap"

	"github.com/mdshabbir-ali/code-runtime/internal/config"
	"github.com/mdshabbir-ali/code-runtime/internal/models"
)

// Connect opens a PostgreSQL connection using the application config.
// It delegates to NewPostgres (defined in postgres.go) with converted parameters.
func Connect(cfg *config.Config, log *zap.Logger) (*DB, error) {
	return NewPostgres(DatabaseConfig{
		Host:                cfg.Database.Host,
		Port:                cfg.Database.Port,
		User:                cfg.Database.User,
		Password:            cfg.Database.Password,
		DBName:              cfg.Database.Name,
		SSLMode:             cfg.Database.SSLMode,
		MaxOpenConns:        cfg.Database.MaxOpenConns,
		MaxIdleConns:        cfg.Database.MaxIdleConns,
		ConnMaxLifetimeSecs: int(cfg.Database.ConnMaxLifetime.Seconds()),
	}, log)
}

// Ping checks if the database is reachable (convenience wrapper around HealthCheck).
func (d *DB) Ping(ctx context.Context) error {
	return d.HealthCheck(ctx)
}

// AutoMigrateAll runs GORM auto-migrations for all registered domain models.
func (d *DB) AutoMigrateAll() error {
	return d.AutoMigrate(
		&models.Language{},
		&models.Submission{},
		&models.ExecutionLog{},
		&models.User{},
		&models.APIKey{},
	)
}
