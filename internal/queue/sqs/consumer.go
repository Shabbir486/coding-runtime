package sqs

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"go.uber.org/zap"

	appconfig "github.com/revature/corems-code-executor/internal/config"
	"github.com/revature/corems-code-executor/internal/queue"
)

const (
	// maxReceive is how many messages a single ReceiveMessage call may return.
	maxReceive = 10
	// priorityWaitSeconds is the short long-poll used to check the priority
	// queue before falling back to the normal queue's full long-poll.
	priorityWaitSeconds = 1
	// receiveCountAttr is the SQS system attribute carrying the delivery count.
	receiveCountAttr = "ApproximateReceiveCount"
)

// Consumer implements queue.Consumer on top of Amazon SQS. It polls the
// priority queue first, then the normal queue, rebuilds each job from the
// database via a queue.JobLoader, and dispatches it to the handler with bounded
// concurrency. Retry and dead-lettering are delegated to the SQS redrive policy.
type Consumer struct {
	api               API
	loader            queue.JobLoader
	jobsURL           string
	priorityURL       string
	waitTime          int32
	visibilityTimeout int32
	log               *zap.Logger

	sem    chan struct{}
	wg     sync.WaitGroup
	stopCh chan struct{}
	stop   sync.Once
}

// NewConsumer constructs an SQS-backed Consumer.
func NewConsumer(api API, cfg appconfig.SQSConfig, loader queue.JobLoader, log *zap.Logger) *Consumer {
	if log == nil {
		log, _ = zap.NewProduction()
	}
	concurrency := cfg.MaxConcurrency
	if concurrency <= 0 {
		concurrency = 8
	}
	wait := cfg.WaitTimeSeconds
	if wait <= 0 || wait > 20 {
		wait = 20
	}
	vt := cfg.VisibilityTimeout
	if vt <= 0 {
		vt = 330
	}
	return &Consumer{
		api:               api,
		loader:            loader,
		jobsURL:           cfg.JobsQueueURL,
		priorityURL:       cfg.PriorityQueueURL,
		waitTime:          wait,
		visibilityTimeout: vt,
		log:               log,
		sem:               make(chan struct{}, concurrency),
		stopCh:            make(chan struct{}),
	}
}

// Start polls SQS and dispatches jobs until ctx is cancelled or Stop is called.
func (c *Consumer) Start(ctx context.Context, handler queue.JobHandler) error {
	c.log.Info("sqs consumer started",
		zap.String("jobs_queue", c.jobsURL),
		zap.String("priority_queue", c.priorityURL),
		zap.Int("concurrency", cap(c.sem)),
	)

	for {
		select {
		case <-ctx.Done():
			c.wg.Wait()
			return nil
		case <-c.stopCh:
			c.wg.Wait()
			return nil
		default:
		}

		// Priority first (short poll), then normal queue (full long-poll).
		if c.priorityURL != "" {
			if n := c.receiveAndDispatch(ctx, c.priorityURL, priorityWaitSeconds, handler); n > 0 {
				continue
			}
		}
		c.receiveAndDispatch(ctx, c.jobsURL, c.waitTime, handler)
	}
}

// Stop signals the poll loop to stop and waits for in-flight jobs to drain.
func (c *Consumer) Stop() error {
	c.stop.Do(func() { close(c.stopCh) })
	c.wg.Wait()
	c.log.Info("sqs consumer stopped")
	return nil
}

// receiveAndDispatch performs one ReceiveMessage and dispatches each message to
// a bounded goroutine. It returns the number of messages received.
func (c *Consumer) receiveAndDispatch(ctx context.Context, queueURL string, wait int32, handler queue.JobHandler) int {
	if queueURL == "" {
		return 0
	}
	out, err := c.api.ReceiveMessage(ctx, &awssqs.ReceiveMessageInput{
		QueueUrl:                    &queueURL,
		MaxNumberOfMessages:         maxReceive,
		WaitTimeSeconds:             wait,
		VisibilityTimeout:           c.visibilityTimeout,
		MessageSystemAttributeNames: []types.MessageSystemAttributeName{types.MessageSystemAttributeNameApproximateReceiveCount},
	})
	if err != nil {
		select {
		case <-ctx.Done():
		case <-c.stopCh:
		default:
			c.log.Warn("sqs receive error", zap.String("queue", queueURL), zap.Error(err))
		}
		return 0
	}

	for i := range out.Messages {
		msg := out.Messages[i]
		select {
		case <-ctx.Done():
			return len(out.Messages)
		case <-c.stopCh:
			return len(out.Messages)
		case c.sem <- struct{}{}:
		}
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			defer func() { <-c.sem }()
			c.process(ctx, queueURL, msg, handler)
		}()
	}
	return len(out.Messages)
}

// process handles a single SQS message: rebuild the job, run the handler, then
// delete (success / permanent failure) or back off (retriable failure).
func (c *Consumer) process(ctx context.Context, queueURL string, msg types.Message, handler queue.JobHandler) {
	token, err := parseToken(msg.Body)
	if err != nil {
		c.log.Error("sqs: bad message body, deleting", zap.Error(err))
		c.delete(ctx, queueURL, msg.ReceiptHandle)
		return
	}

	job, err := c.loader(ctx, token)
	if err != nil {
		// Could be a transient DB blip or a deleted submission. Leave it for
		// redelivery; the redrive policy dead-letters it after maxReceiveCount.
		c.log.Warn("sqs: load job failed, will redeliver", zap.String("token", token), zap.Error(err))
		c.backoff(ctx, queueURL, msg)
		return
	}

	switch err := handler(ctx, job); {
	case err == nil:
		c.delete(ctx, queueURL, msg.ReceiptHandle)
	case isPermanent(err):
		// Failure is already persisted by the worker; no point retrying.
		c.log.Error("sqs: permanent job failure, deleting",
			zap.String("token", token), zap.Error(err))
		c.delete(ctx, queueURL, msg.ReceiptHandle)
	default:
		c.log.Warn("sqs: retriable job failure, backing off",
			zap.String("token", token), zap.Error(err))
		c.backoff(ctx, queueURL, msg)
	}
}

// delete removes a fully-handled message from the queue.
func (c *Consumer) delete(ctx context.Context, queueURL string, receipt *string) {
	if _, err := c.api.DeleteMessage(ctx, &awssqs.DeleteMessageInput{
		QueueUrl:      &queueURL,
		ReceiptHandle: receipt,
	}); err != nil {
		c.log.Warn("sqs delete failed", zap.Error(err))
	}
}

// backoff shortens the message's visibility timeout so it is redelivered after
// an attempt-scaled delay, letting the SQS redrive policy dead-letter it once
// the receive count is exhausted.
func (c *Consumer) backoff(ctx context.Context, queueURL string, msg types.Message) {
	delay := nakBackoff(receiveCount(msg))
	if _, err := c.api.ChangeMessageVisibility(ctx, &awssqs.ChangeMessageVisibilityInput{
		QueueUrl:          &queueURL,
		ReceiptHandle:     msg.ReceiptHandle,
		VisibilityTimeout: delay,
	}); err != nil {
		c.log.Warn("sqs change visibility failed", zap.Error(err))
	}
}

// parseToken extracts the submission token from a token-only message body.
func parseToken(body *string) (string, error) {
	if body == nil {
		return "", errors.New("nil message body")
	}
	var m queue.SubmissionMessage
	if err := json.Unmarshal([]byte(*body), &m); err != nil {
		return "", err
	}
	if m.Token == "" {
		return "", errors.New("message missing token")
	}
	return m.Token, nil
}

// receiveCount reads the ApproximateReceiveCount system attribute (default 1).
func receiveCount(msg types.Message) int {
	if v, ok := msg.Attributes[receiveCountAttr]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 1
}

// nakBackoff returns an attempt-scaled visibility delay (seconds).
func nakBackoff(deliveries int) int32 {
	switch {
	case deliveries <= 1:
		return 10
	case deliveries == 2:
		return 30
	default:
		return 60
	}
}

// isPermanent reports whether err is a queue.PermanentError (bypasses retries).
func isPermanent(err error) bool {
	var pe *queue.PermanentError
	return errors.As(err, &pe)
}

