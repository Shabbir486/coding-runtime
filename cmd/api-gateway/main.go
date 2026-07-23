package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	api "github.com/revature/corems-code-executor/internal/api"
	"github.com/revature/corems-code-executor/internal/api/handlers"
	"github.com/revature/corems-code-executor/internal/api/routes"
	"github.com/revature/corems-code-executor/internal/batchproc"
	"github.com/revature/corems-code-executor/internal/cache"
	"github.com/revature/corems-code-executor/internal/config"
	"github.com/revature/corems-code-executor/internal/database"
	"github.com/revature/corems-code-executor/internal/metrics"
	"github.com/revature/corems-code-executor/internal/models"
	"github.com/revature/corems-code-executor/internal/queue"
	qsqs "github.com/revature/corems-code-executor/internal/queue/sqs"
	"github.com/revature/corems-code-executor/internal/tracing"
	"github.com/revature/corems-code-executor/internal/webhook"
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

	// ---- MySQL ----------------------------------------------------------
	db, err := database.Connect(cfg, log)
	if err != nil {
		log.Fatal("failed to connect to MySQL", zap.Error(err))
	}
	defer func() {
		if err := db.Close(); err != nil {
			log.Warn("mysql close error", zap.Error(err))
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

	// Flush the language cache on every startup so a redeploy never serves a
	// stale list (e.g. after the active-language policy or a new language changes
	// in DefaultLanguages()). Automated per redeploy — no manual invalidation.
	{
		allLangs := models.DefaultLanguages()
		ids := make([]int, 0, len(allLangs))
		for _, l := range allLangs {
			ids = append(ids, l.ID)
		}
		if err := langCache.InvalidateAll(context.Background(), ids); err != nil {
			log.Warn("startup language cache flush failed", zap.Error(err))
		} else {
			log.Info("language cache flushed on startup", zap.Int("languages", len(ids)))
		}
	}

	// ---- Metrics -------------------------------------------------------------
	queueMetrics := metrics.New(cfg.Metrics.Namespace)
	apiMetrics := metrics.NewAPIMetrics(cfg.Metrics.Namespace)

	// ---- Job queue publisher (NATS or SQS, by CODERUNTIME_QUEUE_PROVIDER) -----
	publisher, err := newPublisher(cfg, log, queueMetrics)
	if err != nil {
		log.Fatal("failed to init queue publisher", zap.Error(err))
	}
	defer publisher.Close()

	// ---- Webhook dispatcher (shared payload assembly + delivery) -------------
	dispatcher := webhook.NewDispatcher(repos.Batches, repos.Webhooks, nil, webhook.RetryConfig{
		MaxRetries: cfg.Webhook.MaxRetries,
		Delay:      cfg.Webhook.RetryDelay,
		Backoff:    cfg.Webhook.RetryBackoff,
		MaxDelay:   cfg.Webhook.RetryMaxDelay,
		Timeout:    cfg.Webhook.Timeout,
	}, log)

	// ---- Batch starter (fans submissions out only when the batch is started) -
	starter := batchproc.NewStarter(repos.Batches, repos.Languages, publisher, subCache, log)

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
		Batch: handlers.NewBatchHandler(handlers.BatchHandlerDeps{
			BatchRepo:    repos.Batches,
			WebhookRepo:  repos.Webhooks,
			Starter:      starter,
			Dispatcher:   dispatcher,
			Metrics:      apiMetrics,
			MaxBatchSize: cfg.Batch.MaxSize,
			Log:          log,
		}),
		Webhook: handlers.NewWebhookHandler(repos.Webhooks, log),
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

// newPublisher selects the job-queue publisher based on cfg.QueueProvider:
// "sqs" for AWS SQS, anything else (default "nats") for NATS JetStream.
func newPublisher(cfg *config.Config, log *zap.Logger, m *metrics.Metrics) (queue.Publisher, error) {
	if cfg.QueueProvider == queue.ProviderSQS {
		client, err := qsqs.NewClient(context.Background(), cfg.SQS)
		if err != nil {
			return nil, fmt.Errorf("sqs publisher: %w", err)
		}
		log.Info("queue provider: sqs",
			zap.String("jobs_queue", cfg.SQS.JobsQueueURL),
			zap.String("priority_queue", cfg.SQS.PriorityQueueURL))
		return qsqs.NewPublisher(client, cfg.SQS, log), nil
	}
	log.Info("queue provider: nats")
	return queue.NewNATSPublisher(cfg, log, m)
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
