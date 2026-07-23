package queue

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/revature/corems-code-executor/internal/cache"
	"github.com/revature/corems-code-executor/internal/models"
)

const (
	// DefaultWaitTimeout is used when the caller does not specify a timeout.
	DefaultWaitTimeout = 30 * time.Second
	// maxPollInterval caps the exponential back-off used during polling.
	maxPollInterval = 500 * time.Millisecond
)

// ErrTimeout is returned by WaitForResult when the deadline is exceeded before
// a terminal result is available.
var ErrTimeout = errors.New("queue: result wait timeout exceeded")

// ResultWaiter waits for a submission result to become available in Redis.
// It implements two strategies:
//
//  1. Redis Pub/Sub notification (preferred — zero-latency, no busy-wait).
//     The worker publishes "done:{token}" after writing results; the waiter
//     receives the notification immediately.
//
//  2. Polling fallback when the subscription is unavailable or the notification
//     is missed. Poll interval starts at 100 ms and doubles up to 500 ms.
type ResultWaiter struct {
	cache  *cache.SubmissionCache
	logger *zap.Logger
}

// NewResultWaiter creates a ResultWaiter backed by the given SubmissionCache.
func NewResultWaiter(sc *cache.SubmissionCache, logger *zap.Logger) *ResultWaiter {
	if logger == nil {
		logger, _ = zap.NewProduction()
	}
	return &ResultWaiter{cache: sc, logger: logger}
}

// WaitForResult blocks until the submission identified by token reaches a
// terminal state, ctx is cancelled, or timeout elapses.
//
// It subscribes to the Redis "done:{token}" channel for immediate notification
// and concurrently polls the cache as a fallback, returning whichever resolves
// first. ErrTimeout is returned when the deadline is exceeded.
func (w *ResultWaiter) WaitForResult(ctx context.Context, token string, timeout time.Duration) (*models.SubmissionResponse, error) {
	if timeout <= 0 {
		timeout = DefaultWaitTimeout
	}

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Fast path: result may already be cached (job finished before the caller
	// started waiting).
	if resp, err := w.cache.Get(waitCtx, token); err == nil && resp != nil && isTerminalResp(resp) {
		return resp, nil
	}

	// Subscribe BEFORE the second fast-path check to avoid a race between the
	// first Get and the subscribe call.
	sub := w.cache.SubscribeDone(waitCtx, token)
	defer func() { _ = sub.Close() }()

	// Second fast path — result may have arrived in the window between the first
	// check and the subscription being established.
	if resp, err := w.cache.Get(waitCtx, token); err == nil && resp != nil && isTerminalResp(resp) {
		return resp, nil
	}

	return w.waitWithPubSubAndPoll(waitCtx, token, sub)
}

// waitWithPubSubAndPoll listens on the pub/sub channel and polls concurrently.
// Whichever delivers a terminal result first wins.
func (w *ResultWaiter) waitWithPubSubAndPoll(
	ctx context.Context,
	token string,
	sub *redis.PubSub,
) (*models.SubmissionResponse, error) {
	type result struct {
		resp *models.SubmissionResponse
		err  error
	}

	// Launch polling goroutine as a safety net in case the pub/sub message
	// is missed (e.g. process restart, Redis failover).
	pollDone := make(chan result, 1)
	go func() {
		resp, err := w.pollUntilDone(ctx, token)
		pollDone <- result{resp, err}
	}()

	msgCh := sub.Channel()

	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, ErrTimeout
			}
			return nil, fmt.Errorf("queue: wait cancelled: %w", ctx.Err())

		case msg, ok := <-msgCh:
			if !ok {
				// Channel closed — fall through to polling result.
				w.logger.Debug("pub/sub channel closed, relying on poll",
					zap.String("token", token),
				)
				pr := <-pollDone
				return pr.resp, pr.err
			}
			if msg == nil {
				continue
			}
			// Notification received — fetch the authoritative result from cache.
			resp, err := w.cache.Get(ctx, token)
			if err != nil {
				w.logger.Warn("cache get after pub/sub notification failed",
					zap.String("token", token),
					zap.Error(err),
				)
				continue // keep waiting; transient Redis error
			}
			if resp != nil && isTerminalResp(resp) {
				return resp, nil
			}
			// Notification arrived before cache was populated — keep waiting.

		case pr := <-pollDone:
			return pr.resp, pr.err
		}
	}
}

// pollUntilDone polls the Redis cache with exponential back-off until a terminal
// result is available or ctx expires.
// Intervals: 100 ms → 200 ms → 400 ms → 500 ms (capped).
func (w *ResultWaiter) pollUntilDone(ctx context.Context, token string) (*models.SubmissionResponse, error) {
	interval := 100 * time.Millisecond
	timer := time.NewTimer(interval)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, ErrTimeout
			}
			return nil, fmt.Errorf("queue: wait cancelled: %w", ctx.Err())

		case <-timer.C:
			resp, err := w.cache.Get(ctx, token)
			if err != nil {
				w.logger.Warn("cache poll error",
					zap.String("token", token),
					zap.Error(err),
				)
				// Continue polling; transient Redis errors should not abort the wait.
			} else if resp != nil && isTerminalResp(resp) {
				return resp, nil
			}

			// Advance exponential back-off.
			interval *= 2
			if interval > maxPollInterval {
				interval = maxPollInterval
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(interval)
		}
	}
}

// isTerminalResp returns true when the response status indicates finished execution.
func isTerminalResp(resp *models.SubmissionResponse) bool {
	return resp != nil && resp.Status.ID >= models.StatusAccepted
}
