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

	"github.com/mdshabbir-ali/code-runtime/internal/config"
	"github.com/mdshabbir-ali/code-runtime/internal/database"
	"github.com/mdshabbir-ali/code-runtime/internal/metrics"
	"github.com/mdshabbir-ali/code-runtime/internal/queue"
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

// connectDB opens PostgreSQL and returns repository instances.
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
	addr := fmt.Sprintf(":%s", envOrDefault("QUEUE_MANAGER_PORT", "8081"))
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
