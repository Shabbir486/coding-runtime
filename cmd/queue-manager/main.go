// Command queue-manager is the canonical owner of NATS JetStream topology.
// It:
//   - Connects to NATS and idempotently creates every required stream. Other
//     services (api-gateway, worker) intentionally do NOT create streams —
//     queue-manager must be running before them.
//   - Runs the Dead Letter Queue processor to park permanently-failed jobs.
//   - Exposes /health and /metrics HTTP endpoints.
//   - Shuts down gracefully on SIGINT / SIGTERM.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"

	"github.com/revature/corems-code-executor/internal/config"
	"github.com/revature/corems-code-executor/internal/database"
	"github.com/revature/corems-code-executor/internal/metrics"
	"github.com/revature/corems-code-executor/internal/models"
	"github.com/revature/corems-code-executor/internal/queue"
	qsqs "github.com/revature/corems-code-executor/internal/queue/sqs"
	"github.com/revature/corems-code-executor/internal/webhook"
)

func main() {
	logger, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to init logger: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = logger.Sync() }()

	if err := run(logger); err != nil {
		logger.Fatal("queue-manager exited with error", zap.Error(err))
	}
}

// run is the application entry point. Complexity is kept low by delegating
// each concern to a dedicated helper function.
func run(logger *zap.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// SQS provider: no NATS streams to own; run the SQS DLQ drainer instead.
	if cfg.QueueProvider == queue.ProviderSQS {
		return runSQS(cfg, logger)
	}

	natsClient, err := connectNATS(cfg, logger)
	if err != nil {
		return err
	}
	defer closeNATS(natsClient, logger)

	if err := natsClient.CreateStreams(); err != nil {
		return fmt.Errorf("create streams: %w", err)
	}

	subRepo, logRepo := connectDB(cfg, logger)
	dlqHandler := queue.NewDLQHandler(natsClient, subRepo, logRepo, logger)

	// Metrics are registered globally via promauto — just instantiate.
	globalMetrics := metrics.New(cfg.Metrics.Namespace)

	httpServer := buildHTTPServer(cfg, natsClient, logger)

	rootCtx, rootCancel := context.WithCancel(context.Background())
	defer rootCancel()

	startQueueDepthPoller(rootCtx, natsClient, globalMetrics, logger)

	dlqErrCh := startDLQProcessor(rootCtx, dlqHandler, logger)
	httpErrCh := startHTTPServer(httpServer, logger)

	waitForShutdownSignal(logger, dlqErrCh, httpErrCh)

	rootCancel()
	shutdown(httpServer, dlqErrCh, logger)
	return nil
}

// runSQS is the SQS-mode entry point: it drains the SQS dead-letter queue
// (marking failed submissions in the DB) and publishes queue-depth metrics.
// There is no NATS topology to own in this mode.
func runSQS(cfg *config.Config, logger *zap.Logger) error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	subRepo, _ := connectDB(cfg, logger)

	client, err := qsqs.NewClient(ctx, cfg.SQS)
	if err != nil {
		return fmt.Errorf("sqs client: %w", err)
	}

	recorder := func(ctx context.Context, token, _ string) error {
		if subRepo == nil {
			return nil
		}
		return subRepo.UpdateStatus(ctx, token, models.StatusInternalError)
	}

	globalMetrics := metrics.New(cfg.Metrics.Namespace)
	httpServer := buildSQSHTTPServer()
	httpErrCh := startHTTPServer(httpServer, logger)

	go pollSQSDepth(ctx, client, cfg, globalMetrics, logger)

	// Durable safety net: re-deliver batch webhooks that were lost when a worker
	// restarted mid-dispatch (they stay status=completed but webhook=pending).
	startWebhookReconciler(ctx, cfg, logger)

	drainer := qsqs.NewDLQDrainer(client, cfg.SQS, recorder, logger)
	drainErrCh := make(chan error, 1)
	go func() {
		err := drainer.Start(ctx)
		if errors.Is(err, context.Canceled) {
			err = nil
		}
		drainErrCh <- err
	}()

	logger.Info("queue-manager running in SQS mode", zap.String("dlq", cfg.SQS.DLQQueueURL))

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	case err := <-drainErrCh:
		if err != nil {
			logger.Error("dlq drainer fatal error", zap.Error(err))
		}
	case err := <-httpErrCh:
		if err != nil {
			logger.Error("http server fatal error", zap.Error(err))
		}
	}

	cancel()
	shutCtx, sc := context.WithTimeout(context.Background(), 15*time.Second)
	defer sc()
	_ = httpServer.Shutdown(shutCtx)
	logger.Info("queue-manager (sqs) shutdown complete")
	return nil
}

// startWebhookReconciler launches a background loop that re-delivers batch
// webhooks stranded at pending/failed — the durable complement to the worker's
// best-effort immediate dispatch (which is lost if the worker pod restarts
// mid-delivery). It re-uses the same Dispatcher/retry config as the worker.
func startWebhookReconciler(ctx context.Context, cfg *config.Config, logger *zap.Logger) {
	db, err := database.Connect(cfg, logger)
	if err != nil {
		logger.Warn("webhook reconciler disabled — database unavailable", zap.Error(err))
		return
	}
	batchRepo := database.NewBatchRepository(db)
	webhookRepo := database.NewWebhookRepository(db)
	dispatcher := webhook.NewDispatcher(batchRepo, webhookRepo, nil, webhook.RetryConfig{
		MaxRetries: cfg.Webhook.MaxRetries,
		Delay:      cfg.Webhook.RetryDelay,
		Backoff:    cfg.Webhook.RetryBackoff,
		MaxDelay:   cfg.Webhook.RetryMaxDelay,
		Timeout:    cfg.Webhook.Timeout,
	}, logger)

	// Per-batch attempt timeout: the HTTP timeout plus a margin.
	perBatch := cfg.Webhook.Timeout + 15*time.Second
	if perBatch <= 0 {
		perBatch = 135 * time.Second
	}

	go func() {
		ticker := time.NewTicker(reconcileInterval)
		defer ticker.Stop()
		logger.Info("webhook reconciler started",
			zap.Duration("interval", reconcileInterval), zap.Duration("grace", reconcileGrace))
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				reconcileWebhooksOnce(ctx, batchRepo, dispatcher, perBatch, logger)
			}
		}
	}()
}

// Reconciler tunables.
const (
	reconcileInterval = 60 * time.Second // how often to sweep
	reconcileGrace    = 2 * time.Minute  // skip batches completed within this window (let immediate dispatch win)
	reconcileMaxAge   = 24 * time.Hour   // stop retrying batches older than this
	reconcileLimit    = 25               // batches per sweep
)

// reconcileWebhooksOnce performs one reconciliation sweep: find stuck batches
// and re-deliver each with a single bounded attempt (the ticker is the retry).
func reconcileWebhooksOnce(
	ctx context.Context,
	batches database.BatchRepository,
	dispatcher *webhook.Dispatcher,
	perBatch time.Duration,
	logger *zap.Logger,
) {
	stuck, err := batches.ListReconcilableBatches(ctx, reconcileGrace, reconcileMaxAge, reconcileLimit)
	if err != nil {
		logger.Warn("webhook reconciler: list failed", zap.Error(err))
		return
	}
	if len(stuck) == 0 {
		return
	}
	logger.Info("webhook reconciler: re-delivering stuck batches", zap.Int("count", len(stuck)))
	for _, b := range stuck {
		attemptCtx, cancel := context.WithTimeout(ctx, perBatch)
		err := dispatcher.DispatchBatch(attemptCtx, b.ID)
		cancel()
		if err != nil {
			logger.Warn("webhook reconciler: re-delivery failed",
				zap.String("batch_id", b.ID), zap.Error(err))
		} else {
			logger.Info("webhook reconciler: re-delivered", zap.String("batch_id", b.ID))
		}
	}
}

// buildSQSHTTPServer serves /metrics and a simple /health for SQS mode.
func buildSQSHTTPServer() *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok","provider":"sqs"}`))
	})
	return &http.Server{
		Addr:         fmt.Sprintf(":%s", envOrDefault("QUEUE_MANAGER_PORT", "8083")),
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
}

// pollSQSDepth periodically records the approximate depth of the SQS queues.
func pollSQSDepth(ctx context.Context, api qsqs.API, cfg *config.Config, m *metrics.Metrics, logger *zap.Logger) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	queues := map[string]string{
		"submissions":          cfg.SQS.JobsQueueURL,
		"submissions_priority": cfg.SQS.PriorityQueueURL,
		"submissions_dlq":      cfg.SQS.DLQQueueURL,
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for label, url := range queues {
				if url == "" {
					continue
				}
				if n, err := qsqs.ApproxMessages(ctx, api, url); err == nil {
					m.QueueDepth.WithLabelValues(label).Set(float64(n))
				} else {
					logger.Debug("sqs depth poll failed", zap.String("queue", label), zap.Error(err))
				}
			}
		}
	}
}

// connectNATS dials NATS with settings derived from cfg.
func connectNATS(cfg *config.Config, logger *zap.Logger) (*queue.Client, error) {
	reconnectWait := cfg.NATS.ReconnectWait
	if reconnectWait <= 0 {
		reconnectWait = 2 * time.Second
	}
	natsCfg := queue.NATSConfig{
		URL:            cfg.NATS.URL,
		MaxReconnects:  cfg.NATS.MaxReconnects,
		ReconnectWait:  reconnectWait,
		PingInterval:   30 * time.Second,
		MaxPingOut:     3,
		DrainTimeout:   10 * time.Second,
		RequestTimeout: 5 * time.Second,
	}
	client, err := queue.NewClient(natsCfg, logger)
	if err != nil {
		return nil, fmt.Errorf("connect nats: %w", err)
	}
	return client, nil
}

// closeNATS drains and closes the NATS connection, logging any error.
func closeNATS(client *queue.Client, logger *zap.Logger) {
	if err := client.Close(); err != nil {
		logger.Warn("nats close error", zap.Error(err))
	}
}

// connectDB opens MySQL and returns repository instances.
// On failure it logs a warning and returns nil repositories so the DLQ handler
// can still run in log-only mode.
func connectDB(cfg *config.Config, logger *zap.Logger) (database.SubmissionRepository, database.ExecutionLogRepository) {
	db, err := database.Connect(cfg, logger)
	if err != nil {
		logger.Warn("database unavailable — DLQ will run without DB updates", zap.Error(err))
		return nil, nil
	}
	return database.NewSubmissionRepository(db), database.NewExecutionLogRepository(db)
}

// buildHTTPServer constructs the HTTP server with /health and /metrics routes.
func buildHTTPServer(cfg *config.Config, natsClient *queue.Client, logger *zap.Logger) *http.Server {
	addr := fmt.Sprintf(":%s", envOrDefault("QUEUE_MANAGER_PORT", "8083"))
	_ = cfg // reserved for future per-config tuning (TLS, timeouts override, etc.)

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/health", healthHandler(natsClient, logger))

	return &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
}

// startDLQProcessor launches the DLQ processor goroutine and returns its error channel.
func startDLQProcessor(ctx context.Context, dlq *queue.DLQHandler, logger *zap.Logger) <-chan error {
	ch := make(chan error, 1)
	go func() {
		logger.Info("starting DLQ processor")
		err := dlq.StartProcessing(ctx)
		if errors.Is(err, context.Canceled) {
			err = nil
		}
		ch <- err
	}()
	return ch
}

// startQueueDepthPoller periodically fetches stream info to record queue depth.
func startQueueDepthPoller(ctx context.Context, client *queue.Client, m *metrics.Metrics, logger *zap.Logger) {
	ticker := time.NewTicker(5 * time.Second)
	go func() {
		for {
			select {
			case <-ctx.Done():
				ticker.Stop()
				return
			case <-ticker.C:
				js := client.JetStream()
				if info, err := js.StreamInfo("SUBMISSIONS"); err == nil && info != nil {
					m.QueueDepth.WithLabelValues("submissions").Set(float64(info.State.Msgs))
				}
				if info, err := js.StreamInfo("SUBMISSIONS_PRIORITY"); err == nil && info != nil {
					m.QueueDepth.WithLabelValues("submissions_priority").Set(float64(info.State.Msgs))
				}
			}
		}
	}()
}

// startHTTPServer launches the HTTP server goroutine and returns its error channel.
func startHTTPServer(srv *http.Server, logger *zap.Logger) <-chan error {
	ch := make(chan error, 1)
	go func() {
		logger.Info("queue-manager HTTP server listening", zap.String("addr", srv.Addr))
		err := srv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		ch <- err
	}()
	return ch
}

// waitForShutdownSignal blocks until a OS signal, DLQ error, or HTTP error occurs.
func waitForShutdownSignal(logger *zap.Logger, dlqErrCh, httpErrCh <-chan error) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	select {
	case sig := <-sigCh:
		logger.Info("received signal, shutting down", zap.String("signal", sig.String()))
	case err := <-dlqErrCh:
		if err != nil {
			logger.Error("DLQ processor fatal error", zap.Error(err))
		}
	case err := <-httpErrCh:
		if err != nil {
			logger.Error("HTTP server fatal error", zap.Error(err))
		}
	}
}

// shutdown gracefully stops the HTTP server and waits for the DLQ processor.
func shutdown(srv *http.Server, dlqErrCh <-chan error, logger *zap.Logger) {
	const shutdownTimeout = 30 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	logger.Info("shutting down HTTP server")
	if err := srv.Shutdown(ctx); err != nil {
		logger.Warn("HTTP server shutdown error", zap.Error(err))
	}

	select {
	case <-ctx.Done():
		logger.Warn("DLQ processor shutdown timed out")
	case err := <-dlqErrCh:
		if err != nil {
			logger.Error("DLQ processor exited with error", zap.Error(err))
		} else {
			logger.Info("DLQ processor stopped")
		}
	}

	logger.Info("queue-manager shutdown complete")
}

// healthHandler returns an HTTP handler that reports queue-manager liveness.
func healthHandler(natsClient *queue.Client, logger *zap.Logger) http.HandlerFunc {
	type healthResponse struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks"`
	}

	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		checks, ok := runHealthChecks(ctx, natsClient, logger)

		status, code := "ok", http.StatusOK
		if !ok {
			status, code = "degraded", http.StatusServiceUnavailable
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(healthResponse{Status: status, Checks: checks})
	}
}

// runHealthChecks executes each subsystem check and returns a summary map plus
// a boolean that is false when any check failed.
func runHealthChecks(ctx context.Context, natsClient *queue.Client, logger *zap.Logger) (map[string]string, bool) {
	checks := make(map[string]string)
	ok := true

	if err := natsClient.HealthCheck(ctx); err != nil {
		checks["nats"] = fmt.Sprintf("unhealthy: %v", err)
		logger.Warn("health check: NATS unhealthy", zap.Error(err))
		ok = false
	} else {
		checks["nats"] = "ok"
	}

	return checks, ok
}

// envOrDefault returns the value of the named environment variable, or def
// when the variable is unset or empty.
func envOrDefault(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
