package database

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"time"

	"go.uber.org/zap"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// DatabaseConfig holds all configuration needed to open a PostgreSQL connection.
type DatabaseConfig struct {
	Host                string
	Port                int
	User                string
	Password            string
	DBName              string
	SSLMode             string
	MaxOpenConns        int
	MaxIdleConns        int
	ConnMaxLifetimeSecs int
}

// DB wraps gorm.DB with helper methods.
type DB struct {
	*gorm.DB
	log *zap.Logger
}

// dsn builds a PostgreSQL connection string.
func (cfg DatabaseConfig) dsn() string {
	return fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s TimeZone=UTC",
		cfg.Host, cfg.Port, cfg.User, cfg.Password, cfg.DBName, cfg.SSLMode,
	)
}

// defaults fills zero values with sensible production defaults.
func (cfg *DatabaseConfig) defaults() {
	if cfg.Port == 0 {
		cfg.Port = 5432
	}
	if cfg.SSLMode == "" {
		cfg.SSLMode = "disable"
	}
	if cfg.MaxOpenConns == 0 {
		cfg.MaxOpenConns = 25
	}
	if cfg.MaxIdleConns == 0 {
		cfg.MaxIdleConns = 10
	}
	if cfg.ConnMaxLifetimeSecs == 0 {
		cfg.ConnMaxLifetimeSecs = 300 // 5 minutes
	}
}

// NewPostgres opens a PostgreSQL connection with retry + connection pooling.
// It retries up to 5 times with exponential back-off starting at 1 s.
func NewPostgres(cfg DatabaseConfig, log *zap.Logger) (*DB, error) {
	cfg.defaults()

	if log == nil {
		log, _ = zap.NewProduction()
	}

	gormCfg := &gorm.Config{
		Logger:                                   logger.Default.LogMode(logger.Silent),
		PrepareStmt:                              true,
		DisableForeignKeyConstraintWhenMigrating: false,
	}

	const maxRetries = 5
	var (
		gdb *gorm.DB
		err error
	)

	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(math.Pow(2, float64(attempt-1))) * time.Second
			log.Warn("postgres connection failed, retrying",
				zap.Int("attempt", attempt),
				zap.Duration("backoff", backoff),
				zap.Error(err),
			)
			time.Sleep(backoff)
		}

		gdb, err = gorm.Open(postgres.New(postgres.Config{
			DSN:                  cfg.dsn(),
			PreferSimpleProtocol: false,
		}), gormCfg)

		if err == nil {
			break
		}
	}

	if err != nil {
		return nil, fmt.Errorf("database: failed to connect after %d attempts: %w", maxRetries, err)
	}

	// Configure the underlying *sql.DB connection pool.
	sqlDB, err := gdb.DB()
	if err != nil {
		return nil, fmt.Errorf("database: failed to retrieve sql.DB: %w", err)
	}
	sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
	sqlDB.SetMaxIdleConns(cfg.MaxIdleConns)
	sqlDB.SetConnMaxLifetime(time.Duration(cfg.ConnMaxLifetimeSecs) * time.Second)
	sqlDB.SetConnMaxIdleTime(5 * time.Minute)

	// Verify the connection is actually alive.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err = sqlDB.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("database: initial ping failed: %w", err)
	}

	log.Info("postgres connected",
		zap.String("host", cfg.Host),
		zap.Int("port", cfg.Port),
		zap.String("db", cfg.DBName),
		zap.Int("max_open", cfg.MaxOpenConns),
		zap.Int("max_idle", cfg.MaxIdleConns),
	)

	return &DB{DB: gdb, log: log}, nil
}

// AutoMigrate runs GORM auto-migrations for all registered models.
// Prefer RunMigrations() from migrations.go for full schema control.
func (db *DB) AutoMigrate(models ...interface{}) error {
	if err := db.DB.AutoMigrate(models...); err != nil {
		return fmt.Errorf("database: auto-migrate failed: %w", err)
	}
	return nil
}

// HealthCheck pings the database and returns an error if it is unreachable.
func (db *DB) HealthCheck(ctx context.Context) error {
	sqlDB, err := db.DB.DB()
	if err != nil {
		return fmt.Errorf("database: health check – cannot get sql.DB: %w", err)
	}
	if err = sqlDB.PingContext(ctx); err != nil {
		return fmt.Errorf("database: health check ping failed: %w", err)
	}
	return nil
}

// Stats returns the current connection pool statistics.
func (db *DB) Stats() (sql.DBStats, error) {
	sqlDB, err := db.DB.DB()
	if err != nil {
		return sql.DBStats{}, fmt.Errorf("database: cannot get sql.DB for stats: %w", err)
	}
	return sqlDB.Stats(), nil
}

// Close cleanly closes all idle connections in the pool.
func (db *DB) Close() error {
	sqlDB, err := db.DB.DB()
	if err != nil {
		return fmt.Errorf("database: cannot get sql.DB for close: %w", err)
	}
	db.log.Info("closing postgres connection pool")
	return sqlDB.Close()
}
