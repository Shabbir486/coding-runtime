// Package batchproc decouples batch execution from batch creation. A batch is
// created and persisted in PENDING state by the API; execution begins ONLY when
// Start is invoked — either by the SQS start-queue consumer (on a
// START_BATCH_PROCESSING event published by the Assessment Service after it
// commits) or by the POST /batches/:id/start endpoint. Start is idempotent.
package batchproc

import (
	"context"
	"errors"
	"fmt"

	"go.uber.org/zap"

	"github.com/revature/corems-code-executor/internal/database"
	"github.com/revature/corems-code-executor/internal/models"
	"github.com/revature/corems-code-executor/internal/queue"
)

// ErrNotPending is returned when Start is called on a batch that is not in the
// PENDING state (already started or finished) — a benign, idempotent no-op.
var ErrNotPending = errors.New("batch is not pending")

// SubmissionSeeder seeds a submission's initial cached state so polling returns
// "in queue" promptly. *cache.SubmissionCache satisfies it.
type SubmissionSeeder interface {
	Set(ctx context.Context, token string, resp *models.SubmissionResponse) error
}

// Starter transitions a batch into processing and fans its submissions out to
// the job queue. It is safe to call concurrently and idempotently.
type Starter struct {
	batches   database.BatchRepository
	languages database.LanguageRepository
	publisher queue.Publisher
	seeder    SubmissionSeeder
	log       *zap.Logger
}

// NewStarter constructs a Starter.
func NewStarter(
	batches database.BatchRepository,
	languages database.LanguageRepository,
	publisher queue.Publisher,
	seeder SubmissionSeeder,
	log *zap.Logger,
) *Starter {
	return &Starter{batches: batches, languages: languages, publisher: publisher, seeder: seeder, log: log}
}

// Start begins execution for a batch. It atomically claims the pending→processing
// transition (so duplicate START events are no-ops), then publishes one job per
// submission. If fan-out fails, the batch is rolled back to pending so the event
// can be retried. Returns ErrNotPending when another caller already started it.
func (s *Starter) Start(ctx context.Context, batchID string) error {
	started, err := s.batches.MarkProcessing(ctx, batchID)
	if err != nil {
		return fmt.Errorf("batchproc: mark processing %s: %w", batchID, err)
	}
	if !started {
		s.log.Info("batch start ignored — not pending (already started or finished)",
			zap.String("batch_id", batchID))
		return ErrNotPending
	}

	subs, err := s.batches.ListSubmissions(ctx, batchID)
	if err != nil {
		_ = s.batches.MarkPending(ctx, batchID) // allow retry
		return fmt.Errorf("batchproc: list submissions %s: %w", batchID, err)
	}

	jobs := make([]*models.ExecutionJob, 0, len(subs))
	for _, sub := range subs {
		// Seed the cache so GET /submissions/:token reports "in queue" promptly.
		resp := models.SubmissionToResponse(sub, false)
		_ = s.seeder.Set(ctx, sub.Token, &resp)
		jobs = append(jobs, s.buildJob(ctx, sub))
	}

	if err := s.publisher.PublishBatch(ctx, jobs); err != nil {
		// Roll back so a redelivered START event re-attempts the fan-out rather
		// than leaving the batch stuck in processing with no jobs enqueued.
		_ = s.batches.MarkPending(ctx, batchID)
		return fmt.Errorf("batchproc: fan out %s: %w", batchID, err)
	}

	s.log.Info("batch processing started",
		zap.String("batch_id", batchID), zap.Int("submissions", len(jobs)))
	return nil
}

// buildJob converts a persisted submission into an execution job, resolving the
// language name for richer worker logs/metrics.
func (s *Starter) buildJob(ctx context.Context, sub *models.Submission) *models.ExecutionJob {
	job := &models.ExecutionJob{}
	job.FromSubmission(sub)
	if lang, err := s.languages.GetByID(ctx, sub.LanguageID); err == nil && lang != nil {
		job.LanguageName = lang.Name
	}
	return job
}
