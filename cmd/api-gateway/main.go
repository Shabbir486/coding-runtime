package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	api "github.com/mdshabbir-ali/code-runtime/internal/api"
	"github.com/mdshabbir-ali/code-runtime/internal/api/handlers"
	"github.com/mdshabbir-ali/code-runtime/internal/api/routes"
	"github.com/mdshabbir-ali/code-runtime/internal/cache"
	"github.com/mdshabbir-ali/code-runtime/internal/config"
	"github.com/mdshabbir-ali/code-runtime/internal/database"
	"github.com/mdshabbir-ali/code-runtime/internal/metrics"
	"github.com/mdshabbir-ali/code-runtime/internal/queue"
	"github.com/mdshabbir-ali/code-runtime/internal/tracing"
)

func main() {
	// ---- Logger (bootstrap before config so we can log init errors) ---------
	log := buildLogger()
	defer func() { _ = log.Sync() }()

	// ---- Configuration -------------------------------------------------------
	cfgPath := os.Getenv("CONFIG_PATH")
	cfg, err := config.LoadConfig(cfgPath)
	if err != nil {
		log.Fatal("failed to load config", zap.Error(err))
	}
	log.Info("config loaded", zap.String("mode", cfg.Server.Mode))

	// ---- Tracing -------------------------------------------------------------
	tracer, err := tracing.Init(cfg, log)
	if err != nil {
		log.Fatal("failed to init tracing", zap.Error(err))
	}
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tracer.Shutdown(shutCtx); err != nil {
			log.Warn("tracing shutdown error", zap.Error(err))
		}
	}()

	// ---- PostgreSQL ----------------------------------------------------------
	db, err := database.Connect(cfg, log)
	if err != nil {
		log.Fatal("failed to connect to PostgreSQL", zap.Error(err))
	}
	defer func() {
		if err := db.Close(); err != nil {
			log.Warn("postgres close error", zap.Error(err))
		}
	}()

	if cfg.Database.MigrateOnStart {
		if err := database.MigrateAndSeed(db); err != nil {
			log.Fatal("migration failed", zap.Error(err))
		}
	}

	repos := database.NewRepositories(db)

	// ---- Redis ---------------------------------------------------------------
	redisClient, err := cache.NewClient(cfg, log)
	if err != nil {
		log.Fatal("failed to connect to Redis", zap.Error(err))
	}
	defer func() {
		if err := redisClient.Close(); err != nil {
			log.Warn("redis close error", zap.Error(err))
		}
	}()

	subCache := cache.NewSubmissionCache(redisClient)
	langCache := cache.NewLanguageCache(redisClient)

	// ---- Metrics -------------------------------------------------------------
	queueMetrics := metrics.New(cfg.Metrics.Namespace)
	apiMetrics := metrics.NewAPIMetrics(cfg.Metrics.Namespace)

	// ---- NATS ----------------------------------------------------------------
	publisher, err := queue.NewNATSPublisher(cfg, log, queueMetrics)
	if err != nil {
		log.Fatal("failed to connect to NATS", zap.Error(err))
	}
	defer publisher.Close()

	// ---- Handlers ------------------------------------------------------------
	handlerBundle := &routes.Handlers{
		Submission: handlers.NewSubmissionHandler(
			repos.Submissions,
			repos.Languages,
			subCache,
			publisher,
			cfg,
			apiMetrics,
			log,
		),
		Language: handlers.NewLanguageHandler(
			repos.Languages,
			langCache,
			log,
		),
		Status: handlers.NewStatusHandler(db, redisClient, log),
		Auth:   handlers.NewAuthHandler(repos.Users, cfg, log),
	}

	// ---- Server --------------------------------------------------------------
	srv := api.NewServer(cfg, repos, db, redisClient, apiMetrics, log, handlerBundle)

	// Run server in background goroutine.
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- srv.Start()
	}()

	// ---- Graceful shutdown ---------------------------------------------------
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)

	select {
	case sig := <-quit:
		log.Info("shutdown signal received", zap.String("signal", sig.String()))
	case err := <-serverErr:
		if err != nil {
			log.Error("server error", zap.Error(err))
		}
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()

	if err := srv.Stop(shutCtx); err != nil {
		log.Error("server stop error", zap.Error(err))
		os.Exit(1)
	}

	log.Info("api-gateway stopped cleanly")
}

// buildLogger creates a production-grade Zap logger.
// In debug mode (LOG_LEVEL=debug), it uses the development preset.
func buildLogger() *zap.Logger {
	level := zapcore.InfoLevel
	if os.Getenv("LOG_LEVEL") == "debug" {
		level = zapcore.DebugLevel
	}

	encoderCfg := zap.NewProductionEncoderConfig()
	encoderCfg.TimeKey = "ts"
	encoderCfg.EncodeTime = zapcore.ISO8601TimeEncoder

	zapCfg := zap.Config{
		Level:             zap.NewAtomicLevelAt(level),
		Development:       level == zapcore.DebugLevel,
		Encoding:          "json",
		EncoderConfig:     encoderCfg,
		OutputPaths:       []string{"stdout"},
		ErrorOutputPaths:  []string{"stderr"},
		DisableCaller:     false,
		DisableStacktrace: level != zapcore.DebugLevel,
	}

	log, err := zapCfg.Build()
	if err != nil {
		// Absolute fallback — should never happen with valid config above.
		log, _ = zap.NewProduction()
	}
	return log
}
