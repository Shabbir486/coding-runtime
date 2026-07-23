package queue

import (
	"context"

	"github.com/revature/corems-code-executor/internal/models"
)

// Provider identifies a queue backend implementation.
const (
	ProviderNATS = "nats"
	ProviderSQS  = "sqs"
)

// JobLoader resolves a full ExecutionJob from a submission token. It exists so
// transports that carry a token-only payload (e.g. SQS, to stay under the
// 256 KB message limit) can rebuild the job from the database before handing it
// to a JobHandler. The NATS transport, which carries the full job inline, does
// not need it.
type JobLoader func(ctx context.Context, token string) (*models.ExecutionJob, error)

// JobConsumer is the transport-agnostic job-consumption contract implemented by
// each queue backend (NATS, SQS). Start blocks until ctx is cancelled or Stop
// is called, dispatching each received job to handler.
type JobConsumer interface {
	// Start begins consuming and blocks until ctx is cancelled or Stop is called.
	Start(ctx context.Context, handler JobHandler) error
	// Stop signals the consumer to stop accepting new messages and waits for
	// in-flight jobs to drain.
	Stop() error
}
