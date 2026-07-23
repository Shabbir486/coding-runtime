package sqs

import (
	"context"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"go.uber.org/zap"

	appconfig "github.com/revature/corems-code-executor/internal/config"
)

// FailureRecorder persists the terminal failure of a dead-lettered submission
// (e.g. mark it as internal error in the database). It is supplied by the
// caller so this package stays decoupled from the persistence layer.
type FailureRecorder func(ctx context.Context, token, reason string) error

// dlqReason is the message recorded for jobs that exhausted their retries.
const dlqReason = "dead-lettered: exhausted retries"

// DLQDrainer consumes the SQS dead-letter queue, records each failure via the
// supplied recorder, and deletes the message. It replaces the NATS DLQ
// processor when the queue provider is SQS. Dead-lettering itself is performed
// by the SQS redrive policy; this drainer only reacts to parked messages.
type DLQDrainer struct {
	api      API
	dlqURL   string
	waitTime int32
	record   FailureRecorder
	log      *zap.Logger
}

// NewDLQDrainer constructs a DLQDrainer.
func NewDLQDrainer(api API, cfg appconfig.SQSConfig, record FailureRecorder, log *zap.Logger) *DLQDrainer {
	if log == nil {
		log, _ = zap.NewProduction()
	}
	wait := cfg.WaitTimeSeconds
	if wait <= 0 || wait > 20 {
		wait = 20
	}
	return &DLQDrainer{
		api:      api,
		dlqURL:   cfg.DLQQueueURL,
		waitTime: wait,
		record:   record,
		log:      log,
	}
}

// Start polls the DLQ until ctx is cancelled. A missing DLQ URL disables it.
func (d *DLQDrainer) Start(ctx context.Context) error {
	if d.dlqURL == "" {
		d.log.Warn("sqs dlq drainer disabled: no dlq_queue_url configured")
		<-ctx.Done()
		return ctx.Err()
	}
	d.log.Info("sqs dlq drainer started", zap.String("dlq", d.dlqURL))

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		out, err := d.api.ReceiveMessage(ctx, &awssqs.ReceiveMessageInput{
			QueueUrl:            &d.dlqURL,
			MaxNumberOfMessages: maxReceive,
			WaitTimeSeconds:     d.waitTime,
		})
		if err != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
				d.log.Warn("sqs dlq receive error", zap.Error(err))
			}
			continue
		}
		for i := range out.Messages {
			d.handle(ctx, out.Messages[i])
		}
	}
}

// handle records the failure for one dead-lettered message and deletes it.
func (d *DLQDrainer) handle(ctx context.Context, msg types.Message) {
	token, err := parseToken(msg.Body)
	if err != nil {
		d.log.Error("sqs dlq: bad message body, deleting", zap.Error(err))
		d.delete(ctx, msg.ReceiptHandle)
		return
	}
	if d.record != nil {
		if err := d.record(ctx, token, dlqReason); err != nil {
			// Leave the message for another attempt rather than losing the record.
			d.log.Warn("sqs dlq: record failure failed, leaving message",
				zap.String("token", token), zap.Error(err))
			return
		}
	}
	d.delete(ctx, msg.ReceiptHandle)
	d.log.Info("sqs dlq: parked failed submission", zap.String("token", token))
}

// delete removes a processed message from the DLQ.
func (d *DLQDrainer) delete(ctx context.Context, receipt *string) {
	if _, err := d.api.DeleteMessage(ctx, &awssqs.DeleteMessageInput{
		QueueUrl:      &d.dlqURL,
		ReceiptHandle: receipt,
	}); err != nil {
		d.log.Warn("sqs dlq delete failed", zap.Error(err))
	}
}

// ApproxMessages returns the approximate number of visible messages in a queue,
// used for queue-depth metrics.
func ApproxMessages(ctx context.Context, api API, queueURL string) (int64, error) {
	out, err := api.GetQueueAttributes(ctx, &awssqs.GetQueueAttributesInput{
		QueueUrl:       &queueURL,
		AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameApproximateNumberOfMessages},
	})
	if err != nil {
		return 0, err
	}
	v := out.Attributes[string(types.QueueAttributeNameApproximateNumberOfMessages)]
	var n int64
	for _, c := range v {
		if c < '0' || c > '9' {
			return 0, nil
		}
		n = n*10 + int64(c-'0')
	}
	return n, nil
}
