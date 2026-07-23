package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/revature/corems-code-executor/internal/config"
)

// SubmissionMessage is the payload published to NATS for each submission.
type SubmissionMessage struct {
	Token string `json:"token"`
}

// BatchStartMessage is the START_BATCH_PROCESSING event payload. The Assessment
// Service publishes it to the start queue AFTER it has committed its own
// submission/token persistence; the runtime consumes it and only then begins
// execution for the batch.
//
// Both snake_case (batch_id) and camelCase (batchId) are accepted so producers
// in either convention interoperate. The "event"/"type" fields are optional and
// informational. Use ID() to read the batch id.
type BatchStartMessage struct {
	Event        string `json:"event,omitempty"`
	BatchID      string `json:"batch_id,omitempty"`
	BatchIDCamel string `json:"batchId,omitempty"`
}

// ID returns the batch id from whichever key the producer used.
func (m BatchStartMessage) ID() string {
	if m.BatchID != "" {
		return m.BatchID
	}
	return m.BatchIDCamel
}

// LegacyPublisher is the simple token-only publishing interface used by the
// original API-gateway integration. For full JetStream publishing use Publisher
// (defined in publisher.go).
type LegacyPublisher interface {
	Publish(ctx context.Context, token string) error
	Close()
}

// LegacyNATSPublisher is the original simple NATS publisher used by the API gateway.
// For production worker jobs, use NATSPublisher (publisher.go) instead.
type LegacyNATSPublisher struct {
	conn    *nats.Conn
	subject string
	log     *zap.Logger
}

// NewLegacyNATSPublisher connects to NATS and returns a LegacyPublisher.
func NewLegacyNATSPublisher(cfg *config.Config, log *zap.Logger) (*LegacyNATSPublisher, error) {
	opts := []nats.Option{
		nats.Name("api-gateway"),
		nats.MaxReconnects(cfg.NATS.MaxReconnects),
		nats.ReconnectWait(cfg.NATS.ReconnectWait),
		nats.Timeout(cfg.NATS.ConnectTimeout),
		nats.DisconnectErrHandler(func(nc *nats.Conn, err error) {
			if err != nil {
				log.Warn("NATS disconnected", zap.Error(err))
			}
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			log.Info("NATS reconnected", zap.String("url", nc.ConnectedUrl()))
		}),
		nats.ClosedHandler(func(nc *nats.Conn) {
			log.Info("NATS connection closed")
		}),
	}

	conn, err := nats.Connect(cfg.NATS.URL, opts...)
	if err != nil {
		return nil, fmt.Errorf("connect to NATS: %w", err)
	}

	log.Info("connected to NATS", zap.String("url", cfg.NATS.URL))

	return &LegacyNATSPublisher{
		conn:    conn,
		subject: cfg.NATS.WorkerSubject,
		log:     log,
	}, nil
}

// Publish enqueues a submission token on the NATS subject.
func (p *LegacyNATSPublisher) Publish(ctx context.Context, token string) error {
	msg := SubmissionMessage{Token: token}
	b, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal submission message: %w", err)
	}

	if err := p.conn.Publish(p.subject, b); err != nil {
		return fmt.Errorf("publish to NATS: %w", err)
	}

	p.log.Debug("published submission", zap.String("token", token), zap.String("subject", p.subject))
	return nil
}

// Close drains and closes the NATS connection.
func (p *LegacyNATSPublisher) Close() {
	if p.conn != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = ctx
		_ = p.conn.Drain()
		p.conn.Close()
	}
}

// NoopPublisher is a no-op LegacyPublisher for testing.
type NoopPublisher struct{}

func (n *NoopPublisher) Publish(_ context.Context, _ string) error { return nil }
func (n *NoopPublisher) Close() { /* no-op: nothing to close for a stub publisher */ }
