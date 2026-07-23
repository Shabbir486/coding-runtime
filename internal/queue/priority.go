package queue

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/revature/corems-code-executor/internal/models"
)

const (
	// highPriorityThreshold defines the minimum priority value for a job to
	// be routed to the high-priority stream.
	highPriorityThreshold = 5
)

// PriorityPublisher wraps a Publisher and routes jobs to either the normal or
// high-priority NATS stream based on a numeric priority value.
type PriorityPublisher struct {
	publisher Publisher
	logger    *zap.Logger
}

// NewPriorityPublisher creates a PriorityPublisher backed by the given Publisher.
func NewPriorityPublisher(publisher Publisher, logger *zap.Logger) *PriorityPublisher {
	if logger == nil {
		logger, _ = zap.NewProduction()
	}
	return &PriorityPublisher{publisher: publisher, logger: logger}
}

// Publish routes job to the appropriate queue based on the priority value.
// Jobs with priority > highPriorityThreshold (5) are sent to the high-priority
// stream; all others go to the normal stream.
//
// The priority value is recorded on the job so workers can log / observe it.
func (pp *PriorityPublisher) Publish(ctx context.Context, job *models.ExecutionJob, priority int) error {
	if job == nil {
		return fmt.Errorf("priority publisher: nil job")
	}

	// Clamp priority to [0, 10].
	if priority < 0 {
		priority = 0
	}
	if priority > 10 {
		priority = 10
	}

	job.Priority = priority
	if job.EnqueuedAt.IsZero() {
		job.EnqueuedAt = time.Now().UTC()
	}

	if priority > highPriorityThreshold {
		pp.logger.Debug("routing job to high-priority queue",
			zap.String("token", job.SubmissionToken),
			zap.Int("priority", priority),
		)
		return pp.publisher.PublishPriorityJob(ctx, job)
	}

	pp.logger.Debug("routing job to normal queue",
		zap.String("token", job.SubmissionToken),
		zap.Int("priority", priority),
	)
	return pp.publisher.PublishJob(ctx, job)
}

// PublishBatch routes a slice of jobs; each is dispatched individually so that
// high-priority and normal jobs can be mixed in the same batch call.
// All jobs are published even if individual ones fail; the first error is returned.
func (pp *PriorityPublisher) PublishBatch(ctx context.Context, jobs []*models.ExecutionJob, priority int) error {
	var firstErr error
	for _, job := range jobs {
		if err := pp.Publish(ctx, job, priority); err != nil {
			pp.logger.Error("priority batch publish failed for job",
				zap.String("token", job.SubmissionToken),
				zap.Error(err),
			)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}
