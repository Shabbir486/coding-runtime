package database

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
	"time"

	"go.uber.org/zap"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// DatabaseConfig holds all configuration needed to open a MySQL connection.
type DatabaseConfig struct {
	Host                string
	Port                int
	User                string
	Password            string
	DBName              string
	// SSLMode maps to the go-sql-driver `tls` parameter: "disable"/"false",
	// "true"/"require", "skip-verify", or "preferred". AWS RDS typically uses
	// "true" (or "skip-verify" when the CA bundle is not installed locally).
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

// dsn builds a MySQL DSN: user:pass@tcp(host:port)/db?params.
// parseTime=true is required so DATETIME/TIMESTAMP columns scan into time.Time;
// loc/charset are pinned for deterministic behaviour across hosts.
func (cfg DatabaseConfig) dsn() string {
	return fmt.Sprintf(
		"%s:%s@tcp(%s:%d)/%s?charset=utf8mb4&parseTime=true&loc=UTC&tls=%s",
		cfg.User, cfg.Password, cfg.Host, cfg.Port, cfg.DBName, mysqlTLSParam(cfg.SSLMode),
	)
}

// mysqlTLSParam translates the generic SSLMode setting into a go-sql-driver
// `tls` value. Unknown values are passed through verbatim so a custom TLS
// config name registered by the driver still works.
func mysqlTLSParam(sslMode string) string {
	switch strings.ToLower(strings.TrimSpace(sslMode)) {
	case "", "disable", "disabled", "false", "off":
		return "false"
	case "true", "require", "required", "on":
		return "true"
	case "skip-verify", "skip_verify", "insecure":
		return "skip-verify"
	case "preferred", "prefer":
		return "preferred"
	default:
		return sslMode
	}
}

// defaults fills zero values with sensible production defaults.
func (cfg *DatabaseConfig) defaults() {
	if cfg.Port == 0 {
		cfg.Port = 3306
	}
	if cfg.SSLMode == "" {
		cfg.SSLMode = "false"
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

// NewMySQL opens a MySQL connection with retry + connection pooling.
// It retries up to 5 times with exponential back-off starting at 1 s.
func NewMySQL(cfg DatabaseConfig, log *zap.Logger) (*DB, error) {
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
			log.Warn("mysql connection failed, retrying",
				zap.Int("attempt", attempt),
				zap.Duration("backoff", backoff),
				zap.Error(err),
			)
			time.Sleep(backoff)
		}

		gdb, err = gorm.Open(mysql.New(mysql.Config{
			DSN:                       cfg.dsn(),
			DefaultStringSize:         256,
			DontSupportRenameIndex:    true,
			DontSupportRenameColumn:   true,
			SkipInitializeWithVersion: false,
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

	log.Info("mysql connected",
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
	db.log.Info("closing mysql connection pool")
	return sqlDB.Close()
}
