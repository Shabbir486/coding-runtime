package sqs

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"go.uber.org/zap"

	appconfig "github.com/revature/corems-code-executor/internal/config"
	"github.com/revature/corems-code-executor/internal/queue"
	"github.com/revature/corems-code-executor/internal/models"
)

// maxBatchEntries is the SQS SendMessageBatch hard limit.
const maxBatchEntries = 10

// Publisher implements queue.Publisher on top of Amazon SQS. It publishes a
// token-only payload (queue.SubmissionMessage); the worker rebuilds the job
// from the database on receipt.
type Publisher struct {
	api         API
	jobsURL     string
	priorityURL string
	log         *zap.Logger
}

// NewPublisher constructs an SQS-backed Publisher.
func NewPublisher(api API, cfg appconfig.SQSConfig, log *zap.Logger) *Publisher {
	if log == nil {
		log, _ = zap.NewProduction()
	}
	return &Publisher{
		api:         api,
		jobsURL:     cfg.JobsQueueURL,
		priorityURL: cfg.PriorityQueueURL,
		log:         log,
	}
}

// PublishJob sends a job to the normal-priority queue.
func (p *Publisher) PublishJob(ctx context.Context, job *models.ExecutionJob) error {
	return p.send(ctx, p.jobsURL, job)
}

// PublishPriorityJob sends a job to the high-priority queue (falls back to the
// normal queue if no priority queue is configured).
func (p *Publisher) PublishPriorityJob(ctx context.Context, job *models.ExecutionJob) error {
	url := p.priorityURL
	if url == "" {
		url = p.jobsURL
	}
	return p.send(ctx, url, job)
}

// PublishBatch sends jobs to the normal queue using SendMessageBatch in chunks
// of 10. The first failed entry across all chunks is returned.
func (p *Publisher) PublishBatch(ctx context.Context, jobs []*models.ExecutionJob) error {
	for start := 0; start < len(jobs); start += maxBatchEntries {
		end := start + maxBatchEntries
		if end > len(jobs) {
			end = len(jobs)
		}
		if err := p.sendChunk(ctx, jobs[start:end]); err != nil {
			return err
		}
	}
	return nil
}

// Close is a no-op; the SQS client holds no long-lived connection to drain.
func (p *Publisher) Close() {}

// send marshals a token-only message and sends it to the given queue.
func (p *Publisher) send(ctx context.Context, queueURL string, job *models.ExecutionJob) error {
	if queueURL == "" {
		return fmt.Errorf("sqs publisher: queue URL not configured")
	}
	body, err := tokenBody(job.SubmissionToken)
	if err != nil {
		return err
	}
	_, err = p.api.SendMessage(ctx, &awssqs.SendMessageInput{
		QueueUrl:    &queueURL,
		MessageBody: &body,
	})
	if err != nil {
		return fmt.Errorf("sqs publisher: send %q: %w", job.SubmissionToken, err)
	}
	p.log.Debug("job published to sqs",
		zap.String("token", job.SubmissionToken), zap.String("queue", queueURL))
	return nil
}

// sendChunk sends up to maxBatchEntries messages in one SendMessageBatch call.
func (p *Publisher) sendChunk(ctx context.Context, jobs []*models.ExecutionJob) error {
	if p.jobsURL == "" {
		return fmt.Errorf("sqs publisher: jobs queue URL not configured")
	}
	entries := make([]types.SendMessageBatchRequestEntry, 0, len(jobs))
	for i, job := range jobs {
		body, err := tokenBody(job.SubmissionToken)
		if err != nil {
			return err
		}
		id := strconv.Itoa(i)
		b := body
		entries = append(entries, types.SendMessageBatchRequestEntry{
			Id:          &id,
			MessageBody: &b,
		})
	}

	out, err := p.api.SendMessageBatch(ctx, &awssqs.SendMessageBatchInput{
		QueueUrl: &p.jobsURL,
		Entries:  entries,
	})
	if err != nil {
		return fmt.Errorf("sqs publisher: batch send: %w", err)
	}
	if len(out.Failed) > 0 {
		f := out.Failed[0]
		return fmt.Errorf("sqs publisher: batch entry failed: %s", awsStr(f.Message))
	}
	return nil
}

// tokenBody serialises a token-only SubmissionMessage payload.
func tokenBody(token string) (string, error) {
	b, err := json.Marshal(queue.SubmissionMessage{Token: token})
	if err != nil {
		return "", fmt.Errorf("sqs publisher: marshal token %q: %w", token, err)
	}
	return string(b), nil
}

// awsStr safely dereferences an optional AWS string field.
func awsStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
