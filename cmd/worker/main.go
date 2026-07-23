package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/docker/docker/client"
	"github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"

	"github.com/revature/corems-code-executor/internal/batchproc"
	"github.com/revature/corems-code-executor/internal/cache"
	"github.com/revature/corems-code-executor/internal/config"
	"github.com/revature/corems-code-executor/internal/database"
	"github.com/revature/corems-code-executor/internal/metrics"
	"github.com/revature/corems-code-executor/internal/models"
	"github.com/revature/corems-code-executor/internal/queue"
	qsqs "github.com/revature/corems-code-executor/internal/queue/sqs"
	"github.com/revature/corems-code-executor/internal/runtime"
	"github.com/revature/corems-code-executor/internal/sandbox"
	"github.com/revature/corems-code-executor/internal/webhook"
	"github.com/revature/corems-code-executor/internal/worker"
)

// webhookRetryConfig maps the app config into the dispatcher's retry policy.
func webhookRetryConfig(cfg *config.Config) webhook.RetryConfig {
	return webhook.RetryConfig{
		MaxRetries: cfg.Webhook.MaxRetries,
		Delay:      cfg.Webhook.RetryDelay,
		Backoff:    cfg.Webhook.RetryBackoff,
		MaxDelay:   cfg.Webhook.RetryMaxDelay,
		Timeout:    cfg.Webhook.Timeout,
	}
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "worker: fatal:", err)
		os.Exit(1)
	}
}

// run is the top-level orchestrator.  Each infrastructure concern is
// initialised through a focused helper so this function stays flat.
func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	logger, err := buildLogger(cfg.Server.Mode)
	if err != nil {
		return fmt.Errorf("build logger: %w", err)
	}
	defer logger.Sync() //nolint:errcheck

	logger.Info("worker service starting",
		zap.String("mode", cfg.Server.Mode),
		zap.Int("pool_size", cfg.Worker.Count),
		zap.Int("concurrency", cfg.Worker.Concurrency),
	)

	db, err := setupDatabase(cfg, logger)
	if err != nil {
		return err
	}
	defer closeDB(db, logger)

	redisClient, subCache, langCache, err := setupCache(cfg, logger)
	if err != nil {
		return err
	}
	defer closeCache(redisClient, logger)

	// NATS is only needed for the NATS queue provider; SQS uses no broker conn.
	var natsConn *nats.Conn
	if cfg.QueueProvider != queue.ProviderSQS {
		natsConn, err = connectNATS(cfg, logger)
		if err != nil {
			return err
		}
		defer drainNATS(natsConn)
	}

	m := metrics.New(cfg.Metrics.Namespace)

	// Start metrics + health server (Kubernetes liveness/readiness probe /health on :8085)
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.Handler())
	healthHandler := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}
	metricsMux.HandleFunc("/health", healthHandler)
	metricsMux.HandleFunc("/ready", healthHandler)
	metricsServer := &http.Server{
		Addr:    ":8085",
		Handler: metricsMux,
	}
	go func() {
		logger.Info("worker metrics server listening", zap.String("addr", metricsServer.Addr))
		if err := metricsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("metrics server failed", zap.Error(err))
		}
	}()
	defer metricsServer.Close()

	sand, err := setupSandbox(cfg, logger, m)
	if err != nil {
		return err
	}
	defer sand.Close()

	mgr, err := setupRuntimeManager(cfg, db, langCache, logger)
	if err != nil {
		return err
	}

	prewarmImages(cfg, sand, logger)

	dbRuntime := runtime.NewDBRuntime(sand, sand.Client(), cfg.Worker.TmpDir, logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	deps := worker.PoolDeps{
		Sandbox:      sand,
		DBRuntime:    dbRuntime,
		Runtime:      mgr,
		DB:           db,
		SubCache:     subCache,
		LangCache:    langCache,
		NATSConn:     natsConn,
		Logger:       logger,
		Metrics:      m,
		Cfg:          &cfg.Worker,
		WebhookRetry: webhookRetryConfig(cfg),
	}

	if cfg.QueueProvider == queue.ProviderSQS {
		if err := runSQSConsumer(ctx, cfg, deps); err != nil {
			return fmt.Errorf("sqs consumer exited: %w", err)
		}
		logger.Info("worker service shutdown complete")
		return nil
	}

	pool := worker.NewPool(deps)
	logger.Info("worker pool starting",
		zap.Int("count", cfg.Worker.Count),
		zap.String("nats_subject", cfg.Worker.NATSSubject),
	)
	if err := pool.Start(ctx); err != nil {
		return fmt.Errorf("pool exited: %w", err)
	}

	logger.Info("worker service shutdown complete")
	return nil
}

// runSQSConsumer drives job processing from Amazon SQS. It reuses the full
// Worker execution pipeline (via ProcessJob) as the handler and rebuilds each
// job from the database from the token-only SQS message.
func runSQSConsumer(ctx context.Context, cfg *config.Config, deps worker.PoolDeps) error {
	w := worker.NewWorker("sqs-worker", worker.WorkerDeps{
		Sandbox:   deps.Sandbox,
		DBRuntime: deps.DBRuntime,
		Runtime:   deps.Runtime,
		DB:        deps.DB,
		SubCache:  deps.SubCache,
		LangCache: deps.LangCache,
		NATSConn:     nil, // SQS path uses no NATS connection
		Logger:       deps.Logger,
		Metrics:      deps.Metrics,
		Cfg:          deps.Cfg,
		WebhookRetry: deps.WebhookRetry,
	})

	submissionRepo := database.NewSubmissionRepository(deps.DB)
	loader := func(ctx context.Context, token string) (*models.ExecutionJob, error) {
		sub, err := submissionRepo.GetByToken(ctx, token)
		if err != nil {
			return nil, err
		}
		job := &models.ExecutionJob{}
		job.FromSubmission(sub)
		return job, nil
	}

	client, err := qsqs.NewClient(ctx, cfg.SQS)
	if err != nil {
		return fmt.Errorf("sqs client: %w", err)
	}

	// Job consumer: executes per-submission jobs fanned out from a started batch.
	jobConsumer := qsqs.NewConsumer(client, cfg.SQS, loader, deps.Logger)

	// Start consumer: on a START_BATCH_PROCESSING event, fan the batch's
	// submissions out to the jobs queue. Execution begins ONLY here — never at
	// batch-creation time.
	publisher := qsqs.NewPublisher(client, cfg.SQS, deps.Logger)
	starter := batchproc.NewStarter(
		database.NewBatchRepository(deps.DB),
		database.NewLanguageRepository(deps.DB),
		publisher,
		deps.SubCache,
		deps.Logger,
	)
	startHandler := func(ctx context.Context, batchID string) error {
		// Already-started batches are an idempotent no-op (delete the event).
		if err := starter.Start(ctx, batchID); err != nil && !errors.Is(err, batchproc.ErrNotPending) {
			return err
		}
		return nil
	}
	startConsumer := qsqs.NewStartConsumer(client, cfg.SQS, startHandler, deps.Logger)

	deps.Logger.Info("worker consuming from sqs",
		zap.String("jobs_queue", cfg.SQS.JobsQueueURL),
		zap.String("priority_queue", cfg.SQS.PriorityQueueURL),
		zap.String("start_queue", cfg.SQS.StartQueueURL),
		zap.Int("concurrency", cfg.SQS.MaxConcurrency),
	)

	// Run both consumers until ctx is cancelled.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = jobConsumer.Start(ctx, w.ProcessJob) }()
	go func() { defer wg.Done(); _ = startConsumer.Start(ctx) }()
	wg.Wait()
	return nil
}

// ─── infrastructure setup helpers ────────────────────────────────────────────

func setupDatabase(cfg *config.Config, logger *zap.Logger) (*database.DB, error) {
	db, err := database.Connect(cfg, logger)
	if err != nil {
		return nil, fmt.Errorf("connect mysql: %w", err)
	}
	if err := db.AutoMigrateAll(); err != nil {
		db.Close() //nolint:errcheck
		return nil, fmt.Errorf("auto-migrate: %w", err)
	}
	return db, nil
}

func closeDB(db *database.DB, logger *zap.Logger) {
	if err := db.Close(); err != nil {
		logger.Warn("mysql close error", zap.Error(err))
	}
}

func setupCache(cfg *config.Config, logger *zap.Logger) (*cache.Client, *cache.SubmissionCache, *cache.LanguageCache, error) {
	redisClient, err := cache.NewClient(cfg, logger)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("connect redis: %w", err)
	}
	return redisClient, cache.NewSubmissionCache(redisClient), cache.NewLanguageCache(redisClient), nil
}

func closeCache(c *cache.Client, logger *zap.Logger) {
	if err := c.Close(); err != nil {
		logger.Warn("redis close error", zap.Error(err))
	}
}

func drainNATS(conn *nats.Conn) {
	_ = conn.Drain()
	conn.Close()
}

func setupSandbox(cfg *config.Config, logger *zap.Logger, m *metrics.Metrics) (*sandbox.DockerSandbox, error) {
	workDir := cfg.Worker.TmpDir
	if workDir == "" {
		workDir = "/tmp/sandbox"
	}

	sandboxCfg := sandbox.SandboxConfig{
		WorkDir:         workDir,
		CPUPeriod:       cfg.Docker.CPUPeriod,
		MemoryLimit:     cfg.Docker.MemorySwap,
		PidsLimit:       cfg.Docker.PidsLimit,
		NetworkDisabled: true,
		ReadOnly:        cfg.Docker.ReadOnly,
		Registry:        cfg.Docker.Registry,
		RegistryAuth:    cfg.Docker.RegistryAuth,
	}

	// Validate Docker is reachable before creating the sandbox.
	dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}
	dockerClient.Close()

	return sandbox.NewDockerSandbox(sandboxCfg, logger, m)
}

func setupRuntimeManager(
	cfg *config.Config,
	db *database.DB,
	langCache *cache.LanguageCache,
	logger *zap.Logger,
) (*runtime.Manager, error) {
	langRepo := database.NewLanguageRepository(db)
	mgr := runtime.NewManager(langRepo, langCache, logger)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := mgr.RefreshCache(ctx); err != nil {
		// Non-fatal: warn and continue; the cache will populate on first use.
		logger.Warn("initial language cache refresh failed", zap.Error(err))
	}
	return mgr, nil
}

func prewarmImages(cfg *config.Config, sand *sandbox.DockerSandbox, logger *zap.Logger) {
	images := cfg.Worker.PrewarmImages
	if len(images) == 0 {
		images = defaultPrewarmImages()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	logger.Info("pre-warming docker images", zap.Int("count", len(images)))
	if err := sand.ImagePool().PrewarmImages(ctx, images); err != nil {
		logger.Warn("image prewarm had failures", zap.Error(err))
	}
}

// ─── misc helpers ─────────────────────────────────────────────────────────────

func buildLogger(mode string) (*zap.Logger, error) {
	if mode == "debug" {
		return zap.NewDevelopment()
	}
	return zap.NewProduction()
}

func connectNATS(cfg *config.Config, logger *zap.Logger) (*nats.Conn, error) {
	opts := []nats.Option{
		nats.Name("code-runtime-worker"),
		// Retry initial connect so the worker doesn't require NATS to be up first.
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(cfg.NATS.MaxReconnects),
		nats.ReconnectWait(cfg.NATS.ReconnectWait),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if err != nil {
				logger.Warn("NATS disconnected", zap.Error(err))
			}
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			logger.Info("NATS reconnected", zap.String("url", nc.ConnectedUrl()))
		}),
		nats.ClosedHandler(func(_ *nats.Conn) {
			logger.Info("NATS connection closed")
		}),
	}

	conn, err := nats.Connect(cfg.NATS.URL, opts...)
	if err != nil {
		return nil, fmt.Errorf("nats connect %q: %w", cfg.NATS.URL, err)
	}
	logger.Info("connected to NATS", zap.String("url", cfg.NATS.URL))
	return conn, nil
}

func defaultPrewarmImages() []string {
	return []string{
		"code-runtime-bash:latest",
		"code-runtime-python:latest",
		"code-runtime-nodejs:latest",
		"code-runtime-golang:latest",
		"code-runtime-java:latest",
	}
}
