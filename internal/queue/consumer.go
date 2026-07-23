package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/revature/corems-code-executor/internal/metrics"
	"github.com/revature/corems-code-executor/internal/models"
)

const (
	// defaultAckWait gives workers generous time to compile and execute code.
	defaultAckWait = 5 * time.Minute
	// defaultMaxDeliver defines how many times a message is redelivered before
	// the consumer sends it to the DLQ.
	defaultMaxDeliver = 3
	// fetchBatchSize is the maximum number of messages pulled per Fetch call.
	fetchBatchSize = 1
	// fetchWait is how long a Fetch call blocks waiting for a message.
	fetchWait = 5 * time.Second
)

// JobHandler is the function signature that workers implement to process a job.
// Return nil to acknowledge success. Return a retriable error to NAK the message
// (it will be redelivered). Return a permanent error to send the job to the DLQ.
type JobHandler func(ctx context.Context, job *models.ExecutionJob) error

// ConsumerConfig holds tunables for a durable pull consumer.
type ConsumerConfig struct {
	// MaxConcurrent limits the number of jobs processed simultaneously.
	MaxConcurrent int
	// AckWait is how long the server waits for an Ack before redelivering.
	AckWait time.Duration
	// MaxDeliver is the total delivery attempts before a message is considered
	// permanently failed and forwarded to the DLQ.
	MaxDeliver int
	// FilterSubject restricts which subjects this consumer receives.
	// An empty string means all subjects in the stream.
	FilterSubject string
	// StreamName is the JetStream stream to subscribe to.
	StreamName string
}

// DefaultConsumerConfig returns production-ready consumer defaults.
func DefaultConsumerConfig(streamName, filterSubject string) ConsumerConfig {
	return ConsumerConfig{
		MaxConcurrent: 4,
		AckWait:       defaultAckWait,
		MaxDeliver:    defaultMaxDeliver,
		FilterSubject: filterSubject,
		StreamName:    streamName,
	}
}

// Consumer is a durable JetStream pull consumer that processes jobs concurrently.
type Consumer struct {
	client     *Client
	workerID   string
	handler    JobHandler
	cfg        ConsumerConfig
	logger     *zap.Logger
	m          *metrics.Metrics
	dlq        *DLQHandler
	sub        *nats.Subscription
	wg         sync.WaitGroup
	sem        chan struct{}
	cancelFunc context.CancelFunc
	mu         sync.Mutex
	running    bool
}

// NewConsumer creates a Consumer. Pass nil for dlq if DLQ forwarding is not needed.
func NewConsumer(
	client *Client,
	workerID string,
	handler JobHandler,
	cfg ConsumerConfig,
	dlq *DLQHandler,
	m *metrics.Metrics,
	logger *zap.Logger,
) *Consumer {
	if logger == nil {
		logger, _ = zap.NewProduction()
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 4
	}
	if cfg.AckWait <= 0 {
		cfg.AckWait = defaultAckWait
	}
	if cfg.MaxDeliver <= 0 {
		cfg.MaxDeliver = defaultMaxDeliver
	}

	return &Consumer{
		client:   client,
		workerID: workerID,
		handler:  handler,
		cfg:      cfg,
		logger:   logger,
		m:        m,
		dlq:      dlq,
		sem:      make(chan struct{}, cfg.MaxConcurrent),
	}
}

// Start creates (or reattaches to) the durable consumer and begins pulling messages.
// It blocks until ctx is cancelled or Stop is called.
func (c *Consumer) Start(ctx context.Context) error {
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return fmt.Errorf("consumer %q: already running", c.workerID)
	}
	c.running = true
	c.mu.Unlock()

	consumerCtx, cancel := context.WithCancel(ctx)
	c.cancelFunc = cancel

	durableName := fmt.Sprintf("worker-%s", c.workerID)

	sub, err := c.createOrAttachConsumer(durableName)
	if err != nil {
		cancel()
		c.mu.Lock()
		c.running = false
		c.mu.Unlock()
		return err
	}
	c.sub = sub

	c.logger.Info("consumer started",
		zap.String("worker_id", c.workerID),
		zap.String("durable", durableName),
		zap.String("stream", c.cfg.StreamName),
		zap.Int("max_concurrent", c.cfg.MaxConcurrent),
	)

	c.pullLoop(consumerCtx)

	cancel()
	return nil
}

// Stop signals the consumer to stop accepting new messages and waits for all
// in-flight jobs to complete before returning.
func (c *Consumer) Stop() error {
	c.mu.Lock()
	if !c.running {
		c.mu.Unlock()
		return nil
	}
	cancel := c.cancelFunc
	c.mu.Unlock()

	c.logger.Info("consumer stopping, waiting for in-flight jobs", zap.String("worker_id", c.workerID))
	if cancel != nil {
		cancel()
	}
	c.wg.Wait()
	c.logger.Info("consumer stopped", zap.String("worker_id", c.workerID))

	c.mu.Lock()
	c.running = false
	c.mu.Unlock()
	return nil
}

// createOrAttachConsumer creates a durable JetStream consumer if it does not
// exist, or re-uses the existing one (idempotent upsert).
func (c *Consumer) createOrAttachConsumer(durableName string) (*nats.Subscription, error) {
	consumerCfg := &nats.ConsumerConfig{
		Durable:        durableName,
		AckPolicy:      nats.AckExplicitPolicy,
		AckWait:        c.cfg.AckWait,
		MaxDeliver:     c.cfg.MaxDeliver,
		DeliverPolicy:  nats.DeliverAllPolicy,
		ReplayPolicy:   nats.ReplayInstantPolicy,
		// Backoff schedule applied between redeliveries.
		BackOff: []time.Duration{
			10 * time.Second,
			30 * time.Second,
			60 * time.Second,
		},
	}
	if c.cfg.FilterSubject != "" {
		consumerCfg.FilterSubject = c.cfg.FilterSubject
	}

	// Upsert consumer.
	if _, err := c.client.js.AddConsumer(c.cfg.StreamName, consumerCfg); err != nil {
		return nil, fmt.Errorf("consumer %q: add consumer: %w", c.workerID, err)
	}

	sub, err := c.client.js.PullSubscribe(
		c.cfg.FilterSubject,
		durableName,
		nats.Bind(c.cfg.StreamName, durableName),
	)
	if err != nil {
		return nil, fmt.Errorf("consumer %q: pull subscribe: %w", c.workerID, err)
	}
	return sub, nil
}

// pullLoop continuously fetches messages from the subscription and dispatches
// them to goroutines bounded by the semaphore.
func (c *Consumer) pullLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		msgs, err := c.sub.Fetch(fetchBatchSize, nats.MaxWait(fetchWait))
		if err != nil {
			if err == nats.ErrTimeout || err == context.DeadlineExceeded {
				// Normal: no messages arrived within the wait window.
				continue
			}
			select {
			case <-ctx.Done():
				return
			default:
			}
			c.logger.Warn("fetch error", zap.String("worker_id", c.workerID), zap.Error(err))
			continue
		}

		for _, msg := range msgs {
			// Acquire semaphore slot — blocks when MaxConcurrent slots are full.
			select {
			case <-ctx.Done():
				// Nack the message so it gets redelivered by another worker.
				_ = msg.Nak()
				return
			case c.sem <- struct{}{}:
			}

			c.wg.Add(1)
			go func(m *nats.Msg) {
				defer c.wg.Done()
				defer func() { <-c.sem }()
				c.processMessage(ctx, m)
			}(msg)
		}
	}
}

// processMessage is the top-level dispatcher for a single NATS message.
// It extracts metadata and the job payload, then delegates outcome handling
// to one of three focused methods to keep cognitive complexity low.
func (c *Consumer) processMessage(ctx context.Context, msg *nats.Msg) {
	meta, err := msg.Metadata()
	if err != nil {
		c.logger.Error("cannot read message metadata, acking to avoid loop",
			zap.String("worker_id", c.workerID),
			zap.Error(err),
		)
		_ = msg.Ack()
		return
	}

	var job models.ExecutionJob
	if err := json.Unmarshal(msg.Data, &job); err != nil {
		c.logger.Error("cannot unmarshal job, forwarding to DLQ",
			zap.String("worker_id", c.workerID),
			zap.Uint64("seq", meta.Sequence.Consumer),
			zap.Error(err),
		)
		c.forwardToDLQ(ctx, &job, "unmarshal_error: "+err.Error(), int(meta.NumDelivered))
		_ = msg.Ack()
		return
	}

	c.recordInFlight(&job, +1)
	c.logger.Info("processing job",
		zap.String("worker_id", c.workerID),
		zap.String("token", job.SubmissionToken),
		zap.Int("language_id", job.LanguageID),
		zap.Uint64("delivery", meta.NumDelivered),
	)

	start := time.Now()
	handlerErr := c.handler(ctx, &job)
	elapsed := time.Since(start)

	c.recordInFlight(&job, -1)
	c.recordDuration(&job, elapsed)
	c.handleOutcome(ctx, msg, &job, meta, handlerErr, elapsed)
}

// handleOutcome routes the post-execution result to success, permanent-failure,
// or retriable-failure handling. Keeping it separate from processMessage cuts
// the branch count in the parent to well below the complexity threshold.
func (c *Consumer) handleOutcome(
	ctx context.Context,
	msg *nats.Msg,
	job *models.ExecutionJob,
	meta *nats.MsgMetadata,
	handlerErr error,
	elapsed time.Duration,
) {
	switch {
	case handlerErr == nil:
		c.onSuccess(msg, job, elapsed)
	case isPermanentError(handlerErr) || int(meta.NumDelivered) >= c.cfg.MaxDeliver:
		c.onPermanentFailure(ctx, msg, job, meta, handlerErr)
	default:
		c.onRetriableFailure(msg, job, meta, handlerErr)
	}
}

// onSuccess acks the message and records success metrics.
func (c *Consumer) onSuccess(msg *nats.Msg, job *models.ExecutionJob, elapsed time.Duration) {
	if err := msg.Ack(); err != nil {
		c.logger.Warn("ack failed", zap.String("token", job.SubmissionToken), zap.Error(err))
	}
	if c.m != nil {
		c.m.JobsConsumed.WithLabelValues(c.workerID, "success").Inc()
		c.m.AckLatency.WithLabelValues(c.workerID).Observe(elapsed.Seconds())
	}
	c.logger.Info("job completed",
		zap.String("worker_id", c.workerID),
		zap.String("token", job.SubmissionToken),
		zap.Duration("elapsed", elapsed),
	)
}

// onPermanentFailure forwards the job to the DLQ, acks the message, and records
// failure metrics.
func (c *Consumer) onPermanentFailure(
	ctx context.Context,
	msg *nats.Msg,
	job *models.ExecutionJob,
	meta *nats.MsgMetadata,
	handlerErr error,
) {
	c.logger.Error("job permanently failed, forwarding to DLQ",
		zap.String("worker_id", c.workerID),
		zap.String("token", job.SubmissionToken),
		zap.Uint64("deliveries", meta.NumDelivered),
		zap.Error(handlerErr),
	)
	c.forwardToDLQ(ctx, job, handlerErr.Error(), int(meta.NumDelivered))
	if err := msg.Ack(); err != nil {
		c.logger.Warn("ack after DLQ forward failed",
			zap.String("token", job.SubmissionToken), zap.Error(err))
	}
	if c.m != nil {
		c.m.JobsFailed.WithLabelValues(c.workerID, "permanent").Inc()
	}
}

// onRetriableFailure naks the message with a backoff delay and records retry metrics.
func (c *Consumer) onRetriableFailure(
	msg *nats.Msg,
	job *models.ExecutionJob,
	meta *nats.MsgMetadata,
	handlerErr error,
) {
	c.logger.Warn("job failed, will retry",
		zap.String("worker_id", c.workerID),
		zap.String("token", job.SubmissionToken),
		zap.Uint64("deliveries", meta.NumDelivered),
		zap.Error(handlerErr),
	)
	if err := msg.NakWithDelay(nakBackoff(int(meta.NumDelivered))); err != nil {
		c.logger.Warn("nak failed", zap.String("token", job.SubmissionToken), zap.Error(err))
	}
	if c.m != nil {
		c.m.RetryAttempts.WithLabelValues(c.workerID).Inc()
		c.m.JobsFailed.WithLabelValues(c.workerID, "retriable").Inc()
	}
}

// recordInFlight adjusts the in-flight gauge by delta (+1 or -1).
func (c *Consumer) recordInFlight(job *models.ExecutionJob, delta float64) {
	if c.m == nil {
		return
	}
	c.m.InFlightJobs.WithLabelValues(c.workerID).Add(delta)
	_ = job // kept for potential future per-language gauges
}

// recordDuration observes job execution time in the histogram.
func (c *Consumer) recordDuration(job *models.ExecutionJob, elapsed time.Duration) {
	if c.m != nil {
		c.m.JobDuration.
			WithLabelValues(c.workerID, fmt.Sprintf("%d", job.LanguageID)).
			Observe(elapsed.Seconds())
	}
}

// forwardToDLQ publishes the job to the dead-letter queue. If DLQHandler is nil,
// the failure is only logged.
func (c *Consumer) forwardToDLQ(ctx context.Context, job *models.ExecutionJob, reason string, attempts int) {
	if c.dlq == nil {
		c.logger.Warn("no DLQ handler configured, dropping failed job",
			zap.String("token", job.SubmissionToken),
			zap.String("reason", reason),
		)
		return
	}
	if err := c.dlq.PublishToDLQ(ctx, job, reason); err != nil {
		c.logger.Error("failed to publish to DLQ",
			zap.String("token", job.SubmissionToken),
			zap.Error(err),
		)
	}
	if c.m != nil {
		c.m.JobsDLQ.WithLabelValues(reason).Inc()
	}
}

// nakBackoff returns the delay to pass to NakWithDelay based on the delivery attempt.
func nakBackoff(deliveries int) time.Duration {
	switch {
	case deliveries <= 1:
		return 10 * time.Second
	case deliveries == 2:
		return 30 * time.Second
	default:
		return 60 * time.Second
	}
}

// PermanentError wraps an error and marks it as non-retriable.
// Workers should wrap terminal failures (e.g. language not found) with this type
// so the consumer skips the retry cycle and goes directly to the DLQ.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// NewPermanentError wraps err as a PermanentError.
func NewPermanentError(err error) *PermanentError { return &PermanentError{Err: err} }

// isPermanentError reports whether err should bypass retries.
// It uses errors.As so that wrapped PermanentErrors are also detected.
func isPermanentError(err error) bool {
	var pe *PermanentError
	return errors.As(err, &pe)
}
