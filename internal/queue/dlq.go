package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/mdshabbir-ali/code-runtime/internal/database"
	"github.com/mdshabbir-ali/code-runtime/internal/models"
)

const (
	// dlqFetchBatchSize is the number of DLQ messages pulled per batch.
	dlqFetchBatchSize = 10
	// dlqFetchWait is the block time for each Fetch call in the DLQ processor.
	dlqFetchWait = 10 * time.Second
	// dlqConsumerDurable is the durable consumer name for the DLQ processor.
	dlqConsumerDurable = "dlq-processor"
	// dlqAckWait gives the DLQ processor time to update the database.
	dlqAckWait = 2 * time.Minute
)

// DeadLetterJob is the payload stored in the SUBMISSIONS_DLQ stream.
type DeadLetterJob struct {
	OriginalJob   *models.ExecutionJob `json:"original_job"`
	FailureReason string               `json:"failure_reason"`
	Attempts      int                  `json:"attempts"`
	FirstFailedAt time.Time            `json:"first_failed_at"`
	LastFailedAt  time.Time            `json:"last_failed_at"`
}

// DLQHandler manages dead-letter queue publishing and processing.
type DLQHandler struct {
	client *Client
	db     database.SubmissionRepository
	logs   database.ExecutionLogRepository
	logger *zap.Logger
}

// NewDLQHandler creates a DLQHandler.
// db and logs may be nil — in that case, database updates are skipped and
// failures are only logged (useful in tests or standalone queue managers
// without a database connection).
func NewDLQHandler(client *Client, db database.SubmissionRepository, logs database.ExecutionLogRepository, logger *zap.Logger) *DLQHandler {
	if logger == nil {
		logger, _ = zap.NewProduction()
	}
	return &DLQHandler{client: client, db: db, logs: logs, logger: logger}
}

// PublishToDLQ serialises a DeadLetterJob and publishes it to the DLQ stream.
// The token is used as the NATS MsgID to prevent duplicate DLQ entries on
// publisher retries.
func (d *DLQHandler) PublishToDLQ(ctx context.Context, job *models.ExecutionJob, reason string) error {
	now := time.Now().UTC()
	dlqJob := &DeadLetterJob{
		OriginalJob:   job,
		FailureReason: reason,
		Attempts:      job.RetryCount + 1,
		FirstFailedAt: now,
		LastFailedAt:  now,
	}

	data, err := json.Marshal(dlqJob)
	if err != nil {
		return fmt.Errorf("dlq: marshal dead letter job %q: %w", job.SubmissionToken, err)
	}

	msg := &nats.Msg{
		Subject: subjectDLQ,
		Data:    data,
		Header:  nats.Header{},
	}
	msg.Header.Set("token", job.SubmissionToken)
	msg.Header.Set("failure_reason", reason)
	msg.Header.Set("failed_at", now.Format(time.RFC3339Nano))

	// Use token as dedup ID so re-publishing the same failed job is idempotent.
	_, err = d.client.js.PublishMsg(msg,
		nats.MsgId(fmt.Sprintf("dlq-%s-%d", job.SubmissionToken, job.RetryCount)),
		nats.Context(ctx),
	)
	if err != nil {
		return fmt.Errorf("dlq: publish dead letter job %q: %w", job.SubmissionToken, err)
	}

	d.logger.Info("job sent to DLQ",
		zap.String("token", job.SubmissionToken),
		zap.String("reason", reason),
	)
	return nil
}

// StartProcessing subscribes to the DLQ stream and processes failed jobs by
// updating their status in the database and writing an execution log entry.
// It blocks until ctx is cancelled.
func (d *DLQHandler) StartProcessing(ctx context.Context) error {
	consumerCfg := &nats.ConsumerConfig{
		Durable:       dlqConsumerDurable,
		AckPolicy:     nats.AckExplicitPolicy,
		AckWait:       dlqAckWait,
		MaxDeliver:    3,
		DeliverPolicy: nats.DeliverAllPolicy,
		ReplayPolicy:  nats.ReplayInstantPolicy,
		FilterSubject: "submissions.dlq.>",
	}

	if _, err := d.client.js.AddConsumer("SUBMISSIONS_DLQ", consumerCfg); err != nil {
		return fmt.Errorf("dlq: create consumer: %w", err)
	}

	sub, err := d.client.js.PullSubscribe(
		"submissions.dlq.>",
		dlqConsumerDurable,
		nats.Bind("SUBMISSIONS_DLQ", dlqConsumerDurable),
	)
	if err != nil {
		return fmt.Errorf("dlq: pull subscribe: %w", err)
	}
	defer func() {
		_ = sub.Unsubscribe()
	}()

	d.logger.Info("DLQ processor started")

	for {
		select {
		case <-ctx.Done():
			d.logger.Info("DLQ processor stopping")
			return nil
		default:
		}

		msgs, err := sub.Fetch(dlqFetchBatchSize, nats.MaxWait(dlqFetchWait))
		if err != nil {
			if err == nats.ErrTimeout || err == context.DeadlineExceeded {
				continue
			}
			select {
			case <-ctx.Done():
				return nil
			default:
			}
			d.logger.Warn("DLQ fetch error", zap.Error(err))
			continue
		}

		for _, msg := range msgs {
			if processErr := d.processDeadLetter(ctx, msg); processErr != nil {
				d.logger.Error("DLQ process error", zap.Error(processErr))
				// NAK so the message is redelivered.
				_ = msg.Nak()
				continue
			}
			_ = msg.Ack()
		}
	}
}

// processDeadLetter handles a single message from the DLQ stream: it updates
// the submission status to InternalError and writes a failure log entry.
func (d *DLQHandler) processDeadLetter(ctx context.Context, msg *nats.Msg) error {
	var dlqJob DeadLetterJob
	if err := json.Unmarshal(msg.Data, &dlqJob); err != nil {
		return fmt.Errorf("dlq: unmarshal dead letter message: %w", err)
	}

	job := dlqJob.OriginalJob
	if job == nil {
		return fmt.Errorf("dlq: dead letter job has nil OriginalJob")
	}

	d.logger.Error("processing dead-letter job",
		zap.String("token", job.SubmissionToken),
		zap.String("reason", dlqJob.FailureReason),
		zap.Int("attempts", dlqJob.Attempts),
		zap.Time("first_failed", dlqJob.FirstFailedAt),
		zap.Time("last_failed", dlqJob.LastFailedAt),
	)

	// Update submission status to InternalError in the database.
	if d.db != nil {
		if err := d.db.UpdateStatus(ctx, job.SubmissionToken, models.StatusInternalError); err != nil {
			d.logger.Warn("dlq: failed to update submission status",
				zap.String("token", job.SubmissionToken),
				zap.Error(err),
			)
			// Non-fatal — continue to write the execution log.
		}
	}

	// Write a structured failure entry to execution_logs.
	if d.logs != nil {
		logMsg := fmt.Sprintf("job permanently failed after %d attempts: %s",
			dlqJob.Attempts, dlqJob.FailureReason)

		if err := d.logs.Create(ctx, &models.ExecutionLog{
			Token:    job.SubmissionToken,
			WorkerID: "dlq-processor",
			Level:    "error",
			Message:  logMsg,
		}); err != nil {
			d.logger.Warn("dlq: failed to write execution log",
				zap.String("token", job.SubmissionToken),
				zap.Error(err),
			)
		}
	}

	return nil
}

// ReplayJob re-enqueues a previously failed job by looking it up via token
// in the DLQ stream and publishing it back to the normal submissions subject.
// This supports manual or automated replay via the management API.
func (d *DLQHandler) ReplayJob(ctx context.Context, token string) error {
	// Fetch the job from the database so we have the full submission fields.
	if d.db == nil {
		return fmt.Errorf("dlq: replay requires a database connection")
	}

	sub, err := d.db.GetByToken(ctx, token)
	if err != nil {
		return fmt.Errorf("dlq: replay — get submission %q: %w", token, err)
	}

	// Re-build an ExecutionJob from the stored submission.
	// Submission.Stdin is *string; dereference safely.
	var stdin string
	if sub.Stdin != nil {
		stdin = *sub.Stdin
	}
	job := &models.ExecutionJob{
		SubmissionToken: sub.Token,
		LanguageID:      sub.LanguageID,
		SourceCode:      sub.SourceCode,
		Stdin:           stdin,
		ExpectedOutput:  sub.ExpectedOutput,
		CPUTimeLimit:    sub.CPUTimeLimit,
		WallTimeLimit:   sub.WallTimeLimit,
		MemoryLimit:     sub.MemoryLimit,
		StackLimit:      sub.StackLimit,
		MaxProcesses:    sub.MaxProcesses,
		MaxFileSize:     sub.MaxFileSize,
		CompilerOptions: sub.CompilerOptions,
		CommandLineArgs: sub.CommandLineArgs,
		CallbackURL:     sub.CallbackURL,
		RetryCount:      0,
		EnqueuedAt:      time.Now().UTC(),
	}

	// Reset the submission to in-queue state so workers pick it up again.
	if err := d.db.UpdateStatus(ctx, token, models.StatusInQueue); err != nil {
		return fmt.Errorf("dlq: replay — reset status %q: %w", token, err)
	}

	// Publish via the normal publisher path.
	data, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("dlq: replay — marshal job %q: %w", token, err)
	}

	msg := &nats.Msg{
		Subject: subjectExecute,
		Data:    data,
		Header:  buildHeaders(job),
	}

	if _, err = d.client.js.PublishMsg(msg, nats.Context(ctx)); err != nil {
		// Undo status reset on publish failure.
		_ = d.db.UpdateStatus(ctx, token, models.StatusInternalError)
		return fmt.Errorf("dlq: replay — publish job %q: %w", token, err)
	}

	d.logger.Info("job replayed from DLQ",
		zap.String("token", token),
	)

	if d.logs != nil {
		_ = d.logs.Create(ctx, &models.ExecutionLog{
			Token:    token,
			WorkerID: "dlq-handler",
			Level:    "info",
			Message:  "job replayed from DLQ",
		})
	}

	return nil
}
