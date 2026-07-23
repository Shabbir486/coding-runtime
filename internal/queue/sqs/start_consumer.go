package sqs

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"go.uber.org/zap"

	appconfig "github.com/revature/corems-code-executor/internal/config"
	"github.com/revature/corems-code-executor/internal/queue"
)

// BatchStartHandler begins execution for the given batch. Returning nil deletes
// the START event; a non-nil error backs off for redelivery (then DLQ via the
// redrive policy). Idempotent no-ops (already-started batches) must return nil.
type BatchStartHandler func(ctx context.Context, batchID string) error

// StartConsumer consumes START_BATCH_PROCESSING events from the start queue and
// invokes the handler. It is intentionally separate from the per-submission job
// Consumer: this queue carries batch-level start signals, not execution jobs.
type StartConsumer struct {
	api               API
	startURL          string
	waitTime          int32
	visibilityTimeout int32
	handler           BatchStartHandler
	log               *zap.Logger

	sem    chan struct{}
	wg     sync.WaitGroup
	stopCh chan struct{}
	stop   sync.Once
}

// NewStartConsumer constructs a StartConsumer.
func NewStartConsumer(api API, cfg appconfig.SQSConfig, handler BatchStartHandler, log *zap.Logger) *StartConsumer {
	if log == nil {
		log, _ = zap.NewProduction()
	}
	wait := cfg.WaitTimeSeconds
	if wait <= 0 || wait > 20 {
		wait = 20
	}
	vt := cfg.VisibilityTimeout
	if vt <= 0 {
		vt = 330
	}
	return &StartConsumer{
		api:               api,
		startURL:          cfg.StartQueueURL,
		waitTime:          wait,
		visibilityTimeout: vt,
		handler:           handler,
		log:               log,
		sem:               make(chan struct{}, 4),
		stopCh:            make(chan struct{}),
	}
}

// Start polls the start queue until ctx is cancelled or Stop is called. A
// missing start-queue URL disables the consumer (it just blocks on ctx).
func (c *StartConsumer) Start(ctx context.Context) error {
	if c.startURL == "" {
		c.log.Warn("sqs start consumer disabled: no start_queue_url configured")
		<-ctx.Done()
		return nil
	}
	c.log.Info("sqs start consumer started", zap.String("start_queue", c.startURL))

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
		c.receiveAndDispatch(ctx)
	}
}

// Stop signals the poll loop to stop and waits for in-flight events to drain.
func (c *StartConsumer) Stop() error {
	c.stop.Do(func() { close(c.stopCh) })
	c.wg.Wait()
	c.log.Info("sqs start consumer stopped")
	return nil
}

func (c *StartConsumer) receiveAndDispatch(ctx context.Context) {
	out, err := c.api.ReceiveMessage(ctx, &awssqs.ReceiveMessageInput{
		QueueUrl:                    &c.startURL,
		MaxNumberOfMessages:         maxReceive,
		WaitTimeSeconds:             c.waitTime,
		VisibilityTimeout:           c.visibilityTimeout,
		MessageSystemAttributeNames: []types.MessageSystemAttributeName{types.MessageSystemAttributeNameApproximateReceiveCount},
	})
	if err != nil {
		select {
		case <-ctx.Done():
		case <-c.stopCh:
		default:
			c.log.Warn("sqs start receive error", zap.Error(err))
		}
		return
	}

	for i := range out.Messages {
		msg := out.Messages[i]
		select {
		case <-ctx.Done():
			return
		case <-c.stopCh:
			return
		case c.sem <- struct{}{}:
		}
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			defer func() { <-c.sem }()
			c.process(ctx, msg)
		}()
	}
}

func (c *StartConsumer) process(ctx context.Context, msg types.Message) {
	batchID, err := parseBatchStart(msg.Body)
	if err != nil {
		c.log.Error("sqs start: bad message body, deleting", zap.Error(err))
		c.delete(ctx, msg.ReceiptHandle)
		return
	}
	if err := c.handler(ctx, batchID); err != nil {
		c.log.Warn("sqs start: handler failed, backing off",
			zap.String("batch_id", batchID), zap.Error(err))
		c.backoff(ctx, msg)
		return
	}
	c.delete(ctx, msg.ReceiptHandle)
	c.log.Info("sqs start: batch processing triggered", zap.String("batch_id", batchID))
}

func (c *StartConsumer) delete(ctx context.Context, receipt *string) {
	if _, err := c.api.DeleteMessage(ctx, &awssqs.DeleteMessageInput{
		QueueUrl:      &c.startURL,
		ReceiptHandle: receipt,
	}); err != nil {
		c.log.Warn("sqs start delete failed", zap.Error(err))
	}
}

// backoff shortens the message visibility for an attempt-scaled redelivery,
// letting the redrive policy dead-letter it after maxReceiveCount.
func (c *StartConsumer) backoff(ctx context.Context, msg types.Message) {
	if _, err := c.api.ChangeMessageVisibility(ctx, &awssqs.ChangeMessageVisibilityInput{
		QueueUrl:          &c.startURL,
		ReceiptHandle:     msg.ReceiptHandle,
		VisibilityTimeout: nakBackoff(receiveCount(msg)),
	}); err != nil {
		c.log.Warn("sqs start change visibility failed", zap.Error(err))
	}
}

// parseBatchStart extracts the batch_id from a START_BATCH_PROCESSING payload.
func parseBatchStart(body *string) (string, error) {
	if body == nil {
		return "", errors.New("nil message body")
	}
	var m queue.BatchStartMessage
	if err := json.Unmarshal([]byte(*body), &m); err != nil {
		return "", err
	}
	id := m.ID() // accepts batch_id or batchId
	if id == "" {
		return "", errors.New("message missing batch_id/batchId")
	}
	return id, nil
}
