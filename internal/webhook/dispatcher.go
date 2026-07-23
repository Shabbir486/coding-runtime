// Package webhook delivers batch-completion notifications to user-registered
// HTTP endpoints. It is shared by the worker (which fires automatically when a
// batch finishes, with configurable retries) and the api-gateway (which
// re-fires on demand via the callback endpoint), so the assembly and delivery
// logic lives in one place.
package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/revature/corems-code-executor/internal/database"
	"github.com/revature/corems-code-executor/internal/models"
)

// defaultTimeout bounds a single outbound webhook delivery attempt.
const defaultTimeout = 10 * time.Second

// apiKeyHeader is the header carrying the registered webhook's outbound key so
// the receiver can authenticate the call as originating from this platform.
const apiKeyHeader = "X-API-Key"

// ErrNoWebhook is returned when a batch has no linked webhook to deliver to.
var ErrNoWebhook = errors.New("batch has no linked webhook")

// RetryConfig controls how failed webhook deliveries are retried. All fields
// are configurable (see config.WebhookConfig). A delivery is attempted
// 1 + MaxRetries times; the wait before retry N starts at Delay and is
// multiplied by Backoff each time (Backoff <= 1 means a fixed delay), capped at
// MaxDelay.
type RetryConfig struct {
	MaxRetries int           // additional attempts after the first
	Delay      time.Duration // wait before the first retry
	Backoff    float64       // delay multiplier per retry (<=1 → fixed)
	MaxDelay   time.Duration // upper bound on the retry delay
	Timeout    time.Duration // per-attempt HTTP timeout
}

// DefaultRetryConfig returns sensible retry defaults.
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxRetries: 3,
		Delay:      10 * time.Second,
		Backoff:    2.0,
		MaxDelay:   5 * time.Minute,
		Timeout:    defaultTimeout,
	}
}

// normalised fills zero/invalid fields with safe defaults.
func (c RetryConfig) normalised() RetryConfig {
	if c.MaxRetries < 0 {
		c.MaxRetries = 0
	}
	if c.Delay <= 0 {
		c.Delay = 10 * time.Second
	}
	if c.Backoff < 1 {
		c.Backoff = 1
	}
	if c.MaxDelay <= 0 {
		c.MaxDelay = 5 * time.Minute
	}
	if c.Timeout <= 0 {
		c.Timeout = defaultTimeout
	}
	return c
}

// Dispatcher assembles a batch result payload and delivers it to the linked
// webhook, recording the delivery outcome on the batch row.
type Dispatcher struct {
	batches  database.BatchRepository
	webhooks database.WebhookRepository
	httpCli  *http.Client
	retry    RetryConfig
	log      *zap.Logger
}

// NewDispatcher constructs a Dispatcher. A nil client falls back to a sane
// default whose timeout matches retry.Timeout.
func NewDispatcher(
	batches database.BatchRepository,
	webhooks database.WebhookRepository,
	httpCli *http.Client,
	retry RetryConfig,
	log *zap.Logger,
) *Dispatcher {
	retry = retry.normalised()
	if httpCli == nil {
		httpCli = &http.Client{Timeout: retry.Timeout}
	}
	return &Dispatcher{batches: batches, webhooks: webhooks, httpCli: httpCli, retry: retry, log: log}
}

// DispatchBatch performs a SINGLE delivery attempt (no retry) and records the
// outcome. Used by the on-demand callback endpoint, where the caller wants an
// immediate result and the endpoint itself is the manual retry mechanism.
func (d *Dispatcher) DispatchBatch(ctx context.Context, batchID string) error {
	hook, payload, err := d.prepare(ctx, batchID)
	if err != nil {
		return err
	}
	if err := d.deliver(ctx, hook, payload); err != nil {
		_ = d.batches.MarkWebhook(ctx, batchID, models.WebhookStatusFailed, nil, 0)
		return err
	}
	return d.markSent(ctx, batchID, 0)
}

// DispatchBatchWithRetry delivers with the configured retry policy, recording
// "failed" only after all attempts are exhausted. Used by the worker when a
// batch completes. Honours ctx cancellation between attempts.
func (d *Dispatcher) DispatchBatchWithRetry(ctx context.Context, batchID string) error {
	hook, payload, err := d.prepare(ctx, batchID)
	if err != nil {
		return err
	}
	attempts, err := d.deliverWithRetry(ctx, batchID, hook, payload)
	retries := attempts - 1 // retries are attempts beyond the first delivery
	if retries < 0 {
		retries = 0
	}
	if err != nil {
		_ = d.batches.MarkWebhook(ctx, batchID, models.WebhookStatusFailed, nil, retries)
		return err
	}
	return d.markSent(ctx, batchID, retries)
}

// RetryBudget is the worst-case wall-clock time a full retry sequence can take.
// Callers running DispatchBatchWithRetry on a detached context use it to bound
// that context.
func (d *Dispatcher) RetryBudget() time.Duration {
	total := time.Duration(d.retry.MaxRetries+1) * d.retry.Timeout
	delay := d.retry.Delay
	for i := 0; i < d.retry.MaxRetries; i++ {
		total += delay
		delay = nextDelay(delay, d.retry.Backoff, d.retry.MaxDelay)
	}
	return total + 30*time.Second // margin for DB calls / scheduling
}

// prepare loads the batch, its linked webhook and submissions, and builds the
// delivery payload. Returns ErrNoWebhook when no webhook is linked.
func (d *Dispatcher) prepare(ctx context.Context, batchID string) (*models.Webhook, models.BatchResponse, error) {
	batch, err := d.batches.GetByID(ctx, batchID)
	if err != nil {
		return nil, models.BatchResponse{}, fmt.Errorf("dispatcher: load batch %s: %w", batchID, err)
	}
	if batch.WebhookID == nil || *batch.WebhookID == "" {
		return nil, models.BatchResponse{}, ErrNoWebhook
	}
	hook, err := d.webhooks.GetByID(ctx, *batch.WebhookID)
	if err != nil {
		return nil, models.BatchResponse{}, fmt.Errorf("dispatcher: load webhook %s: %w", *batch.WebhookID, err)
	}
	subs, err := d.batches.ListSubmissions(ctx, batchID)
	if err != nil {
		return nil, models.BatchResponse{}, fmt.Errorf("dispatcher: load submissions for batch %s: %w", batchID, err)
	}
	return hook, models.BatchToResponse(batch, subs), nil
}

// deliverWithRetry attempts delivery up to 1+MaxRetries times with the
// configured backoff, returning nil on the first success. The first return
// value is the number of attempts actually made (1-based), so callers can
// persist the retry count regardless of success or failure.
func (d *Dispatcher) deliverWithRetry(ctx context.Context, batchID string, hook *models.Webhook, payload models.BatchResponse) (int, error) {
	maxAttempts := d.retry.MaxRetries + 1
	delay := d.retry.Delay
	var lastErr error

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			d.log.Info("retrying webhook delivery",
				zap.String("batch_id", batchID),
				zap.Int("attempt", attempt),
				zap.Int("max_attempts", maxAttempts),
				zap.Duration("after", delay),
			)
			select {
			case <-ctx.Done():
				// Return the attempts made so far so the count is still recorded.
				return attempt - 1, ctx.Err()
			case <-time.After(delay):
			}
			delay = nextDelay(delay, d.retry.Backoff, d.retry.MaxDelay)
		}

		lastErr = d.deliver(ctx, hook, payload)
		if lastErr == nil {
			if attempt > 1 {
				d.log.Info("webhook delivered after retry",
					zap.String("batch_id", batchID), zap.Int("attempt", attempt))
			}
			return attempt, nil
		}
		d.log.Warn("webhook delivery attempt failed",
			zap.String("batch_id", batchID),
			zap.Int("attempt", attempt),
			zap.Int("max_attempts", maxAttempts),
			zap.Error(lastErr),
		)
	}
	return maxAttempts, fmt.Errorf("dispatcher: webhook delivery failed after %d attempts: %w", maxAttempts, lastErr)
}

// nextDelay computes the next backoff delay, capped at maxDelay.
func nextDelay(cur time.Duration, backoff float64, maxDelay time.Duration) time.Duration {
	if backoff <= 1 {
		return cur
	}
	next := time.Duration(float64(cur) * backoff)
	if next > maxDelay {
		return maxDelay
	}
	return next
}

// markSent records a successful delivery on the batch, including how many
// retries it took to get there.
func (d *Dispatcher) markSent(ctx context.Context, batchID string, retries int) error {
	now := time.Now().UTC()
	if err := d.batches.MarkWebhook(ctx, batchID, models.WebhookStatusSent, &now, retries); err != nil {
		d.log.Warn("failed to mark webhook delivered",
			zap.String("batch_id", batchID), zap.Error(err))
	}
	return nil
}

// deliver performs a single HTTP POST with the outbound API key header.
func (d *Dispatcher) deliver(ctx context.Context, hook *models.Webhook, payload models.BatchResponse) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("dispatcher: marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hook.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("dispatcher: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(apiKeyHeader, hook.APIKey)

	resp, err := d.httpCli.Do(req)
	if err != nil {
		return fmt.Errorf("dispatcher: POST %s: %w", hook.URL, err)
	}
	defer resp.Body.Close()

	// Capture a bounded snippet of the receiver's response so the reason for a
	// success or failure is recorded in the logs, not just the status code.
	respBody := readBodySnippet(resp.Body)

	if resp.StatusCode >= http.StatusBadRequest {
		d.log.Warn("webhook delivery rejected by receiver",
			zap.String("webhook_id", hook.ID),
			zap.String("url", hook.URL),
			zap.Int("status", resp.StatusCode),
			zap.String("response", respBody))
		return fmt.Errorf("dispatcher: webhook %s returned status %d: %s", hook.URL, resp.StatusCode, respBody)
	}

	d.log.Info("webhook delivered",
		zap.String("webhook_id", hook.ID),
		zap.String("url", hook.URL),
		zap.Int("status", resp.StatusCode),
		zap.String("response", respBody))
	return nil
}

// maxResponseSnippet bounds how much of the receiver's response body is read
// and logged, so a misbehaving endpoint cannot flood logs or memory.
const maxResponseSnippet = 2 * 1024

// readBodySnippet reads up to maxResponseSnippet bytes of body for logging.
func readBodySnippet(body io.Reader) string {
	snippet, err := io.ReadAll(io.LimitReader(body, maxResponseSnippet))
	if err != nil {
		return ""
	}
	return string(snippet)
}
