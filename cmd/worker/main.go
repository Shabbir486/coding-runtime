package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/docker/docker/client"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/mdshabbir-ali/code-runtime/internal/cache"
	"github.com/mdshabbir-ali/code-runtime/internal/config"
	"github.com/mdshabbir-ali/code-runtime/internal/database"
	"github.com/mdshabbir-ali/code-runtime/internal/metrics"
	"github.com/mdshabbir-ali/code-runtime/internal/runtime"
	"github.com/mdshabbir-ali/code-runtime/internal/sandbox"
	"github.com/mdshabbir-ali/code-runtime/internal/worker"
)

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

	natsConn, err := connectNATS(cfg, logger)
	if err != nil {
		return err
	}
	defer drainNATS(natsConn)

	m := metrics.New(cfg.Metrics.Namespace)

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

	pool := worker.NewPool(worker.PoolDeps{
		Sandbox:   sand,
		DBRuntime: dbRuntime,
		Runtime:   mgr,
		DB:        db,
		SubCache:  subCache,
		LangCache: langCache,
		NATSConn:  natsConn,
		Logger:    logger,
		Metrics:   m,
		Cfg:       &cfg.Worker,
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

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

// ─── infrastructure setup helpers ────────────────────────────────────────────

func setupDatabase(cfg *config.Config, logger *zap.Logger) (*database.DB, error) {
	db, err := database.Connect(cfg, logger)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	if err := db.AutoMigrateAll(); err != nil {
		db.Close() //nolint:errcheck
		return nil, fmt.Errorf("auto-migrate: %w", err)
	}
	return db, nil
}

func closeDB(db *database.DB, logger *zap.Logger) {
	if err := db.Close(); err != nil {
		logger.Warn("postgres close error", zap.Error(err))
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
