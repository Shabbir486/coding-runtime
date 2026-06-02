package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/mdshabbir-ali/code-runtime/internal/config"
	"github.com/mdshabbir-ali/code-runtime/internal/models"
)

const (
	// subjectExecute is the subject for normal-priority job messages.
	subjectExecute = "submissions.execute"
	// subjectPriorityExecute is the subject for high-priority job messages.
	subjectPriorityExecute = "submissions.priority.execute"
	// subjectDLQ is the subject for dead-letter messages.
	subjectDLQ = "submissions.dlq.failed"

	// publishMaxRetries is the number of publish attempts before giving up.
	publishMaxRetries = 3
)

// Publisher defines the interface for pushing jobs onto the queue.
type Publisher interface {
	// PublishJob sends a normal-priority job to the SUBMISSIONS stream.
	PublishJob(ctx context.Context, job *models.ExecutionJob) error
	// PublishPriorityJob sends a high-priority job to the SUBMISSIONS_PRIORITY stream.
	PublishPriorityJob(ctx context.Context, job *models.ExecutionJob) error
	// PublishBatch sends multiple jobs in a single round-trip using async publish.
	PublishBatch(ctx context.Context, jobs []*models.ExecutionJob) error
}

// NATSPublisher implements Publisher using NATS JetStream.
type NATSPublisher struct {
	client *Client
	logger *zap.Logger
}

// NewPublisher creates a NATSPublisher backed by the given Client.
func NewPublisher(client *Client, logger *zap.Logger) *NATSPublisher {
	if logger == nil {
		logger, _ = zap.NewProduction()
	}
	return &NATSPublisher{client: client, logger: logger}
}

// PublishJob serialises job as JSON and publishes it to the normal submissions
// subject. It retries up to publishMaxRetries times on transient NATS errors
// using exponential back-off.
func (p *NATSPublisher) PublishJob(ctx context.Context, job *models.ExecutionJob) error {
	return p.publishWithRetry(ctx, subjectExecute, job)
}

// PublishPriorityJob serialises job as JSON and publishes it to the high-priority
// subject. The same retry logic as PublishJob applies.
func (p *NATSPublisher) PublishPriorityJob(ctx context.Context, job *models.ExecutionJob) error {
	return p.publishWithRetry(ctx, subjectPriorityExecute, job)
}

// PublishBatch sends every job asynchronously via JetStream async publish.
// It waits for all acknowledgements or for ctx to be cancelled.
// A single failed message causes the batch to return an error after all
// futures have been collected.
func (p *NATSPublisher) PublishBatch(ctx context.Context, jobs []*models.ExecutionJob) error {
	if len(jobs) == 0 {
		return nil
	}

	type result struct {
		token string
		err   error
	}
	futures := make([]struct {
		token  string
		future nats.PubAckFuture
	}, 0, len(jobs))

	for _, job := range jobs {
		data, err := json.Marshal(job)
		if err != nil {
			return fmt.Errorf("publisher: marshal job %q: %w", job.SubmissionToken, err)
		}

		msg := &nats.Msg{
			Subject: subjectExecute,
			Data:    data,
			Header:  buildHeaders(job),
		}

		future, err := p.client.js.PublishMsgAsync(msg)
		if err != nil {
			return fmt.Errorf("publisher: async publish job %q: %w", job.SubmissionToken, err)
		}
		futures = append(futures, struct {
			token  string
			future nats.PubAckFuture
		}{token: job.SubmissionToken, future: future})
	}

	// Collect acknowledgements.
	var firstErr error
	for _, f := range futures {
		select {
		case <-ctx.Done():
			return fmt.Errorf("publisher: batch cancelled: %w", ctx.Err())
		case pubAck := <-f.future.Ok():
			p.logger.Debug("batch job published",
				zap.String("token", f.token),
				zap.Uint64("seq", pubAck.Sequence),
			)
		case err := <-f.future.Err():
			p.logger.Error("batch job publish failed",
				zap.String("token", f.token),
				zap.Error(err),
			)
			if firstErr == nil {
				firstErr = fmt.Errorf("publisher: batch job %q: %w", f.token, err)
			}
		}
	}
	return firstErr
}

// publishWithRetry attempts to publish to subject with exponential back-off.
// Only transient NATS errors (connection not connected, slow consumer, etc.)
// are retried; permanent errors like serialisation failures are returned immediately.
func (p *NATSPublisher) publishWithRetry(ctx context.Context, subject string, job *models.ExecutionJob) error {
	data, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("publisher: marshal job %q: %w", job.SubmissionToken, err)
	}

	msg := &nats.Msg{
		Subject: subject,
		Data:    data,
		Header:  buildHeaders(job),
	}

	backoff := 100 * time.Millisecond
	var lastErr error

	for attempt := 0; attempt < publishMaxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return fmt.Errorf("publisher: context cancelled after %d attempts: %w", attempt, ctx.Err())
			case <-time.After(backoff):
				backoff *= 2
			}
		}

		pubCtx := ctx
		if _, hasDeadline := ctx.Deadline(); !hasDeadline {
			var cancel context.CancelFunc
			pubCtx, cancel = context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
		}

		_, lastErr = p.client.js.PublishMsg(msg, nats.Context(pubCtx))
		if lastErr == nil {
			p.logger.Debug("job published",
				zap.String("token", job.SubmissionToken),
				zap.String("subject", subject),
				zap.Int("attempt", attempt+1),
			)
			return nil
		}

		if !isTransientNATSError(lastErr) {
			return fmt.Errorf("publisher: permanent error publishing job %q to %q: %w", job.SubmissionToken, subject, lastErr)
		}

		p.logger.Warn("transient publish error, retrying",
			zap.String("token", job.SubmissionToken),
			zap.Int("attempt", attempt+1),
			zap.Error(lastErr),
		)
	}

	return fmt.Errorf("publisher: job %q to %q failed after %d attempts: %w",
		job.SubmissionToken, subject, publishMaxRetries, lastErr)
}

// buildHeaders constructs NATS message headers carrying job metadata.
// These headers are used by consumers for routing, monitoring and tracing
// without needing to deserialise the payload.
func buildHeaders(job *models.ExecutionJob) nats.Header {
	h := nats.Header{}
	h.Set("token", job.SubmissionToken)
	h.Set("language_id", strconv.Itoa(job.LanguageID))
	h.Set("retry_count", strconv.Itoa(job.RetryCount))
	h.Set("timestamp", job.EnqueuedAt.UTC().Format(time.RFC3339Nano))
	if job.CallbackURL != "" {
		h.Set("callback_url", job.CallbackURL)
	}
	return h
}

// isTransientNATSError reports whether err represents a transient NATS condition
// that is safe to retry (e.g. no-responders, connection closed transiently).
func isTransientNATSError(err error) bool {
	if err == nil {
		return false
	}
	switch err {
	case nats.ErrConnectionClosed,
		nats.ErrConnectionDraining,
		nats.ErrConnectionReconnecting,
		nats.ErrNoResponders,
		nats.ErrTimeout:
		return true
	}
	return false
}

// NewNATSPublisher is a convenience constructor that creates a NATS JetStream
// Client from application config and returns a ready-to-use NATSPublisher.
// The caller should defer publisher.Close() to drain the connection on shutdown.
//
// Stream topology is owned by cmd/queue-manager — start it before api-gateway.
// If a required stream does not yet exist, the first Publish() call will fail
// fast with a JetStream "no stream matches subject" error, which is the
// desired signal to operators that queue-manager has not run yet.
func NewNATSPublisher(cfg *config.Config, logger *zap.Logger) (*NATSPublisher, error) {
	nCfg := DefaultNATSConfig(cfg.NATS.URL)
	nCfg.MaxReconnects = cfg.NATS.MaxReconnects
	nCfg.ReconnectWait = cfg.NATS.ReconnectWait
	nCfg.RequestTimeout = cfg.NATS.ConnectTimeout

	client, err := NewClient(nCfg, logger)
	if err != nil {
		return nil, fmt.Errorf("queue: connect to NATS: %w", err)
	}

	return NewPublisher(client, logger), nil
}

// Close drains and closes the underlying NATS connection gracefully.
func (p *NATSPublisher) Close() {
	if p.client != nil {
		p.client.Close() //nolint:errcheck
	}
}
