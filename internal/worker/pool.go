package worker

import (
	"context"
	"fmt"
	"sync"

	"go.uber.org/zap"

	"github.com/revature/corems-code-executor/internal/cache"
	"github.com/revature/corems-code-executor/internal/config"
	"github.com/revature/corems-code-executor/internal/database"
	"github.com/revature/corems-code-executor/internal/metrics"
	"github.com/revature/corems-code-executor/internal/runtime"
	"github.com/revature/corems-code-executor/internal/sandbox"
	"github.com/revature/corems-code-executor/internal/webhook"

	"github.com/nats-io/nats.go"
)

// PoolDeps carries the shared dependencies forwarded to every Worker in the
// pool, reducing the number of parameters passed to NewPool.
type PoolDeps struct {
	Sandbox   *sandbox.DockerSandbox
	DBRuntime *runtime.DBRuntime
	Runtime   *runtime.Manager
	DB        *database.DB
	SubCache  *cache.SubmissionCache
	LangCache *cache.LanguageCache
	NATSConn  *nats.Conn
	Logger    *zap.Logger
	Metrics   *metrics.Metrics
	Cfg       *config.WorkerConfig
	// WebhookRetry configures batch-completion webhook delivery retries.
	WebhookRetry webhook.RetryConfig
}

// Pool owns a fixed set of Workers and manages their lifecycle.
type Pool struct {
	workers []*Worker
	mu      sync.Mutex
	deps    PoolDeps
	logger  *zap.Logger
}

// NewPool creates a Pool but does not start any workers yet.
func NewPool(deps PoolDeps) *Pool {
	return &Pool{
		deps:   deps,
		logger: deps.Logger.With(zap.String("component", "worker_pool")),
	}
}

// Start launches cfg.Count workers, each consuming from the configured NATS
// subject.  It blocks until ctx is cancelled, then drains all workers.
func (p *Pool) Start(ctx context.Context) error {
	count := p.deps.Cfg.Count
	if count <= 0 {
		count = 1
	}

	p.logger.Info("starting worker pool", zap.Int("count", count))

	if err := p.Scale(ctx, count); err != nil {
		return fmt.Errorf("pool: initial scale to %d: %w", count, err)
	}

	// Block until the context is cancelled.
	<-ctx.Done()
	p.logger.Info("context cancelled, stopping pool")
	return p.Stop()
}

// Stop signals every worker to stop accepting jobs and waits for in-flight
// work to drain.
func (p *Pool) Stop() error {
	p.mu.Lock()
	workers := make([]*Worker, len(p.workers))
	copy(workers, p.workers)
	p.mu.Unlock()

	p.logger.Info("draining workers", zap.Int("count", len(workers)))
	var wg sync.WaitGroup
	for _, w := range workers {
		wg.Add(1)
		go func(w *Worker) {
			defer wg.Done()
			if err := w.Stop(); err != nil {
				p.logger.Warn("worker stop error", zap.String("worker_id", w.id), zap.Error(err))
			}
		}(w)
	}
	wg.Wait()
	p.logger.Info("all workers stopped")
	return nil
}

// Scale adjusts the pool to exactly n running workers.
// Workers are added or removed from the tail of the slice.
// Scale is safe to call concurrently with Start/Stop.
func (p *Pool) Scale(ctx context.Context, n int) error {
	if n <= 0 {
		return fmt.Errorf("pool: worker count must be > 0, got %d", n)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	current := len(p.workers)

	switch {
	case n > current:
		// Add workers.
		for i := current; i < n; i++ {
			id := fmt.Sprintf("worker-%d", i+1)
			w := NewWorker(id, WorkerDeps{
				Sandbox:   p.deps.Sandbox,
				DBRuntime: p.deps.DBRuntime,
				Runtime:   p.deps.Runtime,
				DB:        p.deps.DB,
				SubCache:  p.deps.SubCache,
				LangCache: p.deps.LangCache,
				NATSConn:  p.deps.NATSConn,
				Logger:    p.deps.Logger,
				Metrics:      p.deps.Metrics,
				Cfg:          p.deps.Cfg,
				WebhookRetry: p.deps.WebhookRetry,
			})
			p.workers = append(p.workers, w)

			// Each worker runs its blocking Start loop in its own goroutine.
			go func(w *Worker) {
				if err := w.Start(ctx); err != nil {
					p.logger.Error("worker exited with error",
						zap.String("worker_id", w.id),
						zap.Error(err),
					)
				}
			}(w)

			p.logger.Info("worker started", zap.String("worker_id", id))
		}

	case n < current:
		// Remove workers from the tail (most recently added first).
		toRemove := p.workers[n:]
		p.workers = p.workers[:n]
		for _, w := range toRemove {
			go func(w *Worker) {
				if err := w.Stop(); err != nil {
					p.logger.Warn("scale-down stop error",
						zap.String("worker_id", w.id),
						zap.Error(err),
					)
				}
			}(w)
			p.logger.Info("worker removed", zap.String("worker_id", w.id))
		}
	}

	return nil
}

// Len returns the current number of workers in the pool.
func (p *Pool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.workers)
}
