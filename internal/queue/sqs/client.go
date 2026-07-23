// Package sqs implements the queue.Publisher and queue.Consumer contracts on
// top of Amazon SQS. It is selected at runtime when CODERUNTIME_QUEUE_PROVIDER
// is "sqs". Messages carry only the submission token (queue.SubmissionMessage);
// the consumer rebuilds the full ExecutionJob from the database via a
// queue.JobLoader, which keeps payloads well under the SQS 256 KB limit.
package sqs

import (
	"context"
	"fmt"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	appconfig "github.com/revature/corems-code-executor/internal/config"
	"github.com/revature/corems-code-executor/internal/queue"
)

// Compile-time checks that the SQS types satisfy the queue contracts.
var (
	_ queue.Publisher   = (*Publisher)(nil)
	_ queue.JobConsumer = (*Consumer)(nil)
)

// API is the subset of the SQS client used by this package. Declaring it as an
// interface lets unit tests inject a mock without a live AWS endpoint.
type API interface {
	SendMessage(ctx context.Context, in *sqs.SendMessageInput, optFns ...func(*sqs.Options)) (*sqs.SendMessageOutput, error)
	SendMessageBatch(ctx context.Context, in *sqs.SendMessageBatchInput, optFns ...func(*sqs.Options)) (*sqs.SendMessageBatchOutput, error)
	ReceiveMessage(ctx context.Context, in *sqs.ReceiveMessageInput, optFns ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	DeleteMessage(ctx context.Context, in *sqs.DeleteMessageInput, optFns ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
	ChangeMessageVisibility(ctx context.Context, in *sqs.ChangeMessageVisibilityInput, optFns ...func(*sqs.Options)) (*sqs.ChangeMessageVisibilityOutput, error)
	GetQueueAttributes(ctx context.Context, in *sqs.GetQueueAttributesInput, optFns ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error)
}

// NewClient builds an SQS client from application config. Credentials are
// resolved from the standard AWS chain (IAM role, env, shared profile). An
// optional endpoint override targets a local emulator (ElasticMQ / LocalStack).
func NewClient(ctx context.Context, cfg appconfig.SQSConfig) (*sqs.Client, error) {
	loadOpts := []func(*awsconfig.LoadOptions) error{}
	if cfg.Region != "" {
		loadOpts = append(loadOpts, awsconfig.WithRegion(cfg.Region))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("sqs: load aws config: %w", err)
	}

	return sqs.NewFromConfig(awsCfg, func(o *sqs.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = &cfg.Endpoint
		}
	}), nil
}
