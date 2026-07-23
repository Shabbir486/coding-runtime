package unit_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"go.uber.org/zap"

	appconfig "github.com/revature/corems-code-executor/internal/config"
	"github.com/revature/corems-code-executor/internal/models"
	"github.com/revature/corems-code-executor/internal/queue"
	qsqs "github.com/revature/corems-code-executor/internal/queue/sqs"
)

// mockSQSAPI is an in-memory implementation of sqs.API for black-box tests.
type mockSQSAPI struct {
	mu sync.Mutex

	// publisher capture
	sent       []*awssqs.SendMessageInput
	batchCalls int
	batchTotal int

	// consumer capture
	deleted    int
	visChanges int
	lastVis    int32

	// one-shot ReceiveMessage delivery on the jobs queue
	jobsURL  string
	pending  []types.Message
	returned bool
	done     chan string // "delete" | "backoff"
}

func newMockSQSAPI() *mockSQSAPI { return &mockSQSAPI{done: make(chan string, 4)} }

func (m *mockSQSAPI) SendMessage(_ context.Context, in *awssqs.SendMessageInput, _ ...func(*awssqs.Options)) (*awssqs.SendMessageOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, in)
	return &awssqs.SendMessageOutput{}, nil
}

func (m *mockSQSAPI) SendMessageBatch(_ context.Context, in *awssqs.SendMessageBatchInput, _ ...func(*awssqs.Options)) (*awssqs.SendMessageBatchOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.batchCalls++
	m.batchTotal += len(in.Entries)
	return &awssqs.SendMessageBatchOutput{}, nil
}

func (m *mockSQSAPI) ReceiveMessage(ctx context.Context, in *awssqs.ReceiveMessageInput, _ ...func(*awssqs.Options)) (*awssqs.ReceiveMessageOutput, error) {
	m.mu.Lock()
	deliver := *in.QueueUrl == m.jobsURL && !m.returned && len(m.pending) > 0
	if deliver {
		m.returned = true
		msgs := m.pending
		m.mu.Unlock()
		return &awssqs.ReceiveMessageOutput{Messages: msgs}, nil
	}
	m.mu.Unlock()
	// Throttle the empty-poll loop so the test doesn't spin the CPU.
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Millisecond):
	}
	return &awssqs.ReceiveMessageOutput{}, nil
}

func (m *mockSQSAPI) DeleteMessage(_ context.Context, _ *awssqs.DeleteMessageInput, _ ...func(*awssqs.Options)) (*awssqs.DeleteMessageOutput, error) {
	m.mu.Lock()
	m.deleted++
	m.mu.Unlock()
	m.signal("delete")
	return &awssqs.DeleteMessageOutput{}, nil
}

func (m *mockSQSAPI) ChangeMessageVisibility(_ context.Context, in *awssqs.ChangeMessageVisibilityInput, _ ...func(*awssqs.Options)) (*awssqs.ChangeMessageVisibilityOutput, error) {
	m.mu.Lock()
	m.visChanges++
	m.lastVis = in.VisibilityTimeout
	m.mu.Unlock()
	m.signal("backoff")
	return &awssqs.ChangeMessageVisibilityOutput{}, nil
}

func (m *mockSQSAPI) GetQueueAttributes(_ context.Context, _ *awssqs.GetQueueAttributesInput, _ ...func(*awssqs.Options)) (*awssqs.GetQueueAttributesOutput, error) {
	return &awssqs.GetQueueAttributesOutput{
		Attributes: map[string]string{string(types.QueueAttributeNameApproximateNumberOfMessages): "7"},
	}, nil
}

func (m *mockSQSAPI) signal(s string) {
	select {
	case m.done <- s:
	default:
	}
}

func sqsTestCfg() appconfig.SQSConfig {
	return appconfig.SQSConfig{
		JobsQueueURL:      "https://sqs/jobs",
		PriorityQueueURL:  "https://sqs/priority",
		DLQQueueURL:       "https://sqs/dlq",
		WaitTimeSeconds:   1,
		VisibilityTimeout: 330,
		MaxConcurrency:    4,
	}
}

func strptr(s string) *string { return &s }

// ---------------------------------------------------------------------------
// Publisher (black-box)
// ---------------------------------------------------------------------------

func TestSQSPublisher_RoutesAndTokenOnlyBody(t *testing.T) {
	m := newMockSQSAPI()
	p := qsqs.NewPublisher(m, sqsTestCfg(), zap.NewNop())

	if err := p.PublishJob(context.Background(), &models.ExecutionJob{SubmissionToken: "tok-normal"}); err != nil {
		t.Fatalf("PublishJob: %v", err)
	}
	if err := p.PublishPriorityJob(context.Background(), &models.ExecutionJob{SubmissionToken: "tok-prio"}); err != nil {
		t.Fatalf("PublishPriorityJob: %v", err)
	}

	if len(m.sent) != 2 {
		t.Fatalf("expected 2 sends, got %d", len(m.sent))
	}
	if *m.sent[0].QueueUrl != "https://sqs/jobs" {
		t.Errorf("normal queue = %q", *m.sent[0].QueueUrl)
	}
	if *m.sent[1].QueueUrl != "https://sqs/priority" {
		t.Errorf("priority queue = %q", *m.sent[1].QueueUrl)
	}
	var msg queue.SubmissionMessage
	if err := json.Unmarshal([]byte(*m.sent[0].MessageBody), &msg); err != nil {
		t.Fatalf("body not a SubmissionMessage: %v", err)
	}
	if msg.Token != "tok-normal" {
		t.Errorf("body token = %q, want tok-normal", msg.Token)
	}
}

func TestSQSPublisher_BatchChunksOf10(t *testing.T) {
	m := newMockSQSAPI()
	p := qsqs.NewPublisher(m, sqsTestCfg(), zap.NewNop())

	jobs := make([]*models.ExecutionJob, 23)
	for i := range jobs {
		jobs[i] = &models.ExecutionJob{SubmissionToken: "t"}
	}
	if err := p.PublishBatch(context.Background(), jobs); err != nil {
		t.Fatalf("PublishBatch: %v", err)
	}
	if m.batchCalls != 3 || m.batchTotal != 23 {
		t.Errorf("batch calls=%d total=%d, want 3/23", m.batchCalls, m.batchTotal)
	}
}

func TestSQSPublisher_PriorityFallsBackToJobsQueue(t *testing.T) {
	cfg := sqsTestCfg()
	cfg.PriorityQueueURL = ""
	m := newMockSQSAPI()
	p := qsqs.NewPublisher(m, cfg, zap.NewNop())

	if err := p.PublishPriorityJob(context.Background(), &models.ExecutionJob{SubmissionToken: "x"}); err != nil {
		t.Fatalf("PublishPriorityJob: %v", err)
	}
	if *m.sent[0].QueueUrl != "https://sqs/jobs" {
		t.Errorf("fallback queue = %q, want jobs queue", *m.sent[0].QueueUrl)
	}
}

func TestSQSApproxMessages(t *testing.T) {
	n, err := qsqs.ApproxMessages(context.Background(), newMockSQSAPI(), "https://sqs/jobs")
	if err != nil || n != 7 {
		t.Fatalf("ApproxMessages = %d err=%v, want 7", n, err)
	}
}

// ---------------------------------------------------------------------------
// Consumer (black-box: driven through Start, observed via the mock)
// ---------------------------------------------------------------------------

// runConsumerOnce delivers a single message (body) on the jobs queue, runs the
// consumer until the message is deleted or its visibility changed, then stops.
// It returns the mock for assertion and the outcome ("delete" | "backoff").
func runConsumerOnce(t *testing.T, body string, handler queue.JobHandler) (*mockSQSAPI, string) {
	t.Helper()
	cfg := sqsTestCfg()
	cfg.PriorityQueueURL = "" // poll only the jobs queue

	m := newMockSQSAPI()
	m.jobsURL = cfg.JobsQueueURL
	m.pending = []types.Message{{Body: strptr(body), ReceiptHandle: strptr("rh-1")}}

	loader := func(_ context.Context, token string) (*models.ExecutionJob, error) {
		return &models.ExecutionJob{SubmissionToken: token}, nil
	}
	c := qsqs.NewConsumer(m, cfg, loader, zap.NewNop())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Start(ctx, handler) }()

	var outcome string
	select {
	case outcome = <-m.done:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for message outcome")
	}
	cancel()
	_ = c.Stop()
	return m, outcome
}

func TestSQSConsumer_SuccessDeletes(t *testing.T) {
	m, outcome := runConsumerOnce(t, `{"token":"abc"}`,
		func(_ context.Context, _ *models.ExecutionJob) error { return nil })
	if outcome != "delete" || m.deleted != 1 {
		t.Errorf("success: outcome=%s deleted=%d, want delete/1", outcome, m.deleted)
	}
}

func TestSQSConsumer_PermanentErrorDeletes(t *testing.T) {
	m, outcome := runConsumerOnce(t, `{"token":"abc"}`,
		func(_ context.Context, _ *models.ExecutionJob) error {
			return queue.NewPermanentError(errors.New("boom"))
		})
	if outcome != "delete" || m.deleted != 1 {
		t.Errorf("permanent: outcome=%s deleted=%d, want delete/1", outcome, m.deleted)
	}
}

func TestSQSConsumer_RetriableErrorBacksOff(t *testing.T) {
	m, outcome := runConsumerOnce(t, `{"token":"abc"}`,
		func(_ context.Context, _ *models.ExecutionJob) error { return errors.New("transient") })
	if outcome != "backoff" || m.visChanges != 1 {
		t.Errorf("retriable: outcome=%s visChanges=%d, want backoff/1", outcome, m.visChanges)
	}
	if m.lastVis != 10 { // first-attempt backoff (ApproximateReceiveCount defaults to 1)
		t.Errorf("backoff visibility = %d, want 10", m.lastVis)
	}
}

func TestSQSConsumer_BadBodyDeletes(t *testing.T) {
	m, outcome := runConsumerOnce(t, "not-json",
		func(_ context.Context, _ *models.ExecutionJob) error { return nil })
	if outcome != "delete" || m.deleted != 1 {
		t.Errorf("bad body: outcome=%s deleted=%d, want delete/1", outcome, m.deleted)
	}
}

// ---------------------------------------------------------------------------
// StartConsumer (START_BATCH_PROCESSING events)
// ---------------------------------------------------------------------------

func TestSQSStartConsumer_InvokesHandlerAndDeletes(t *testing.T) {
	cfg := sqsTestCfg()
	cfg.StartQueueURL = "https://sqs/start"

	m := newMockSQSAPI()
	m.jobsURL = cfg.StartQueueURL // mock delivers its one-shot message on this URL
	// Exact payload shape published by the Java Assessment Service (camelCase + event).
	m.pending = []types.Message{{Body: strptr(`{"event":"START_BATCH_PROCESSING","batchId":"batch-xyz"}`), ReceiptHandle: strptr("rh")}}

	var (
		mu      sync.Mutex
		gotID   string
	)
	handler := func(_ context.Context, batchID string) error {
		mu.Lock()
		gotID = batchID
		mu.Unlock()
		return nil
	}
	c := qsqs.NewStartConsumer(m, cfg, handler, zap.NewNop())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Start(ctx) }()

	select {
	case outcome := <-m.done:
		if outcome != "delete" {
			t.Fatalf("expected delete, got %s", outcome)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for start event")
	}
	cancel()
	_ = c.Stop()

	mu.Lock()
	defer mu.Unlock()
	if gotID != "batch-xyz" {
		t.Errorf("handler batch_id = %q, want batch-xyz", gotID)
	}
}
