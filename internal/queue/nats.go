// Package queue implements NATS JetStream connection management, stream setup,
// job publishing, durable consumer, DLQ handling, result waiting and priority routing
// for the distributed code-execution platform.
package queue

import (
	"context"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"go.uber.org/zap"
)

// NATSConfig holds all tunables for the NATS JetStream connection.
type NATSConfig struct {
	// URL is the NATS server URL, e.g. "nats://localhost:4222".
	URL string
	// MaxReconnects is the maximum number of reconnect attempts (-1 = unlimited).
	MaxReconnects int
	// ReconnectWait is the delay between reconnect attempts.
	ReconnectWait time.Duration
	// PingInterval is how often the client pings the server to check liveness.
	PingInterval time.Duration
	// MaxPingOut is the number of outstanding pings before the connection is
	// considered stale and dropped.
	MaxPingOut int
	// DrainTimeout is the maximum time allowed to drain subscriptions on close.
	DrainTimeout time.Duration
	// RequestTimeout is the default timeout for synchronous request/reply calls.
	RequestTimeout time.Duration
}

// DefaultNATSConfig returns production-ready defaults.
func DefaultNATSConfig(url string) NATSConfig {
	return NATSConfig{
		URL:            url,
		MaxReconnects:  10,
		ReconnectWait:  2 * time.Second,
		PingInterval:   30 * time.Second,
		MaxPingOut:     3,
		DrainTimeout:   10 * time.Second,
		RequestTimeout: 5 * time.Second,
	}
}

// Client wraps a NATS connection and its JetStream context.
type Client struct {
	conn   *nats.Conn
	js     nats.JetStreamContext
	cfg    NATSConfig
	logger *zap.Logger
}

// NewClient dials NATS and creates a JetStream context.
// It configures reconnect handling, ping/pong keep-alives and structured
// connection-lifecycle logging before returning a ready-to-use Client.
func NewClient(cfg NATSConfig, logger *zap.Logger) (*Client, error) {
	if logger == nil {
		logger, _ = zap.NewProduction()
	}

	opts := []nats.Option{
		nats.Name("code-runtime-queue"),
		nats.MaxReconnects(cfg.MaxReconnects),
		nats.ReconnectWait(cfg.ReconnectWait),
		nats.PingInterval(cfg.PingInterval),
		nats.MaxPingsOutstanding(cfg.MaxPingOut),
		nats.DrainTimeout(cfg.DrainTimeout),

		// Reconnect callbacks for observability.
		nats.DisconnectErrHandler(func(nc *nats.Conn, err error) {
			if err != nil {
				logger.Warn("nats disconnected", zap.Error(err))
			} else {
				logger.Info("nats disconnected gracefully")
			}
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			logger.Info("nats reconnected", zap.String("url", nc.ConnectedUrl()))
		}),
		nats.ClosedHandler(func(nc *nats.Conn) {
			logger.Info("nats connection closed")
		}),
		nats.ErrorHandler(func(nc *nats.Conn, sub *nats.Subscription, err error) {
			logger.Error("nats async error",
				zap.String("subject", func() string {
					if sub != nil {
						return sub.Subject
					}
					return "n/a"
				}()),
				zap.Error(err),
			)
		}),
	}

	conn, err := nats.Connect(cfg.URL, opts...)
	if err != nil {
		return nil, fmt.Errorf("queue: nats connect %q: %w", cfg.URL, err)
	}

	js, err := conn.JetStream(nats.PublishAsyncMaxPending(256))
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("queue: jetstream context: %w", err)
	}

	logger.Info("nats connected", zap.String("url", conn.ConnectedUrl()))

	return &Client{conn: conn, js: js, cfg: cfg, logger: logger}, nil
}

// JetStream returns the underlying JetStream context for advanced use.
func (c *Client) JetStream() nats.JetStreamContext { return c.js }

// Conn returns the underlying NATS connection.
func (c *Client) Conn() *nats.Conn { return c.conn }

// CreateStreams idempotently ensures all required JetStream streams exist.
// It upserts each stream definition so that re-deploying with changed settings
// always converges to the desired configuration.
func (c *Client) CreateStreams() error {
	streams := []nats.StreamConfig{
		{
			// SUBMISSIONS: work-queue stream consumed exactly-once by workers.
			Name:      "SUBMISSIONS",
			Subjects:  []string{"submissions.execute"},
			Retention: nats.WorkQueuePolicy,
			Storage:   nats.FileStorage,
			MaxAge:    24 * time.Hour,
			MaxMsgs:   100_000,
			Replicas:  1,
			// Discard oldest messages when the stream is full.
			Discard: nats.DiscardOld,
			// Duplicate window to prevent double-publish of the same token.
			Duplicates: 5 * time.Minute,
		},
		{
			// SUBMISSIONS_DLQ: failed jobs parked for human / automated review.
			Name:      "SUBMISSIONS_DLQ",
			Subjects:  []string{"submissions.dlq.>"},
			Retention: nats.LimitsPolicy,
			Storage:   nats.FileStorage,
			MaxAge:    7 * 24 * time.Hour,
			MaxMsgs:   50_000,
			Replicas:  1,
			Discard:   nats.DiscardOld,
		},
		{
			// SUBMISSIONS_PRIORITY: high-priority work that workers drain first.
			Name:      "SUBMISSIONS_PRIORITY",
			Subjects:  []string{"submissions.priority.>"},
			Retention: nats.WorkQueuePolicy,
			Storage:   nats.FileStorage,
			MaxAge:    12 * time.Hour,
			MaxMsgs:   10_000,
			Replicas:  1,
			Discard:   nats.DiscardOld,
			Duplicates: 5 * time.Minute,
		},
	}

	for _, cfg := range streams {
		if err := c.upsertStream(cfg); err != nil {
			return err
		}
	}
	c.logger.Info("all nats streams ready")
	return nil
}

// upsertStream creates the stream if it doesn't exist, or updates it if the
// configuration has changed.
func (c *Client) upsertStream(cfg nats.StreamConfig) error {
	_, err := c.js.StreamInfo(cfg.Name)
	if err == nil {
		// Stream exists — update to ensure config is current.
		if _, err = c.js.UpdateStream(&cfg); err != nil {
			return fmt.Errorf("queue: update stream %q: %w", cfg.Name, err)
		}
		c.logger.Info("nats stream updated", zap.String("stream", cfg.Name))
		return nil
	}

	if err != nats.ErrStreamNotFound {
		return fmt.Errorf("queue: inspect stream %q: %w", cfg.Name, err)
	}

	// Stream does not exist — create it.
	if _, err = c.js.AddStream(&cfg); err != nil {
		return fmt.Errorf("queue: create stream %q: %w", cfg.Name, err)
	}
	c.logger.Info("nats stream created", zap.String("stream", cfg.Name))
	return nil
}

// HealthCheck verifies the NATS connection is alive and JetStream is responsive.
func (c *Client) HealthCheck(ctx context.Context) error {
	if !c.conn.IsConnected() {
		return fmt.Errorf("queue: nats connection is not connected (status=%s)", c.conn.Status())
	}

	// Use a timeout-bounded RPC to verify JetStream is responding.
	deadline, ok := ctx.Deadline()
	timeout := c.cfg.RequestTimeout
	if ok {
		if remaining := time.Until(deadline); remaining < timeout {
			timeout = remaining
		}
	}

	if _, err := c.js.AccountInfo(nats.MaxWait(timeout)); err != nil {
		return fmt.Errorf("queue: jetstream health check: %w", err)
	}
	return nil
}

// Close drains active subscriptions gracefully then closes the connection.
// Drain allows in-flight messages to be processed before disconnecting.
func (c *Client) Close() error {
	c.logger.Info("draining nats connection")
	if err := c.conn.Drain(); err != nil {
		// Drain failed — force close to avoid hanging.
		c.logger.Warn("nats drain failed, closing forcefully", zap.Error(err))
		c.conn.Close()
		return fmt.Errorf("queue: drain: %w", err)
	}
	return nil
}
