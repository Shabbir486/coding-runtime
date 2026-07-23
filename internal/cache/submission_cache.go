package cache

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/revature/corems-code-executor/internal/models"
)

const (
	// SubmissionTTL is how long a cached submission lives in Redis.
	SubmissionTTL = 24 * time.Hour

	// StatusTTL is how long a cached status integer lives in Redis.
	// Shorter than SubmissionTTL because status changes frequently during execution.
	StatusTTL = 5 * time.Minute

	// QueuePositionTTL is how long a queue-position entry is kept.
	QueuePositionTTL = 30 * time.Minute

	// keyPrefix is prepended to every submission-related Redis key.
	keyPrefix = "submission:"

	// statusSuffix distinguishes the status key from the full submission key.
	statusSuffix = ":status"

	// queuePosSuffix distinguishes the queue-position key.
	queuePosSuffix = ":qpos"
)

// SubmissionCache provides submission-specific Redis operations built on top
// of the generic Cache.
type SubmissionCache struct {
	cache *Cache
}

// NewSubmissionCache wraps an existing Cache with submission-specific helpers.
func NewSubmissionCache(c *Cache) *SubmissionCache {
	return &SubmissionCache{cache: c}
}

// submissionKey returns the Redis key used to cache a full Submission document.
func submissionKey(token string) string {
	return keyPrefix + token
}

// statusKey returns the Redis key used to cache just the status integer.
func statusKey(token string) string {
	return keyPrefix + token + statusSuffix
}

// queuePosKey returns the Redis key used to cache the queue position integer.
func queuePosKey(token string) string {
	return keyPrefix + token + queuePosSuffix
}

// SetSubmission serialises sub and stores it under its token with SubmissionTTL.
// It also writes the status integer to a separate, short-lived key.
func (sc *SubmissionCache) SetSubmission(ctx context.Context, token string, sub *models.Submission) error {
	if err := sc.cache.Set(ctx, submissionKey(token), sub, SubmissionTTL); err != nil {
		return fmt.Errorf("submissionCache.SetSubmission %s: %w", token, err)
	}
	// Mirror the status into its own key for cheap status polls.
	if err := sc.cache.Set(ctx, statusKey(token), sub.StatusID, StatusTTL); err != nil {
		// Non-fatal: the full submission key is the source of truth.
		sc.cache.log.Sugar().Warnf("submissionCache.SetSubmission: failed to mirror status for %s: %v", token, err)
	}
	return nil
}

// GetSubmission retrieves and deserialises a full Submission from Redis.
// Returns ErrCacheMiss (via IsCacheMiss) when the key is absent.
func (sc *SubmissionCache) GetSubmission(ctx context.Context, token string) (*models.Submission, error) {
	var sub models.Submission
	if err := sc.cache.Get(ctx, submissionKey(token), &sub); err != nil {
		return nil, fmt.Errorf("submissionCache.GetSubmission %s: %w", token, err)
	}
	return &sub, nil
}

// SetStatus stores only the status integer for quick polling without fetching
// the full submission document.
func (sc *SubmissionCache) SetStatus(ctx context.Context, token string, status int) error {
	if err := sc.cache.Set(ctx, statusKey(token), status, StatusTTL); err != nil {
		return fmt.Errorf("submissionCache.SetStatus %s: %w", token, err)
	}
	return nil
}

// GetStatus retrieves the cached status integer.
// Returns ErrCacheMiss (via IsCacheMiss) when the key is absent.
func (sc *SubmissionCache) GetStatus(ctx context.Context, token string) (int, error) {
	var status int
	if err := sc.cache.Get(ctx, statusKey(token), &status); err != nil {
		return 0, fmt.Errorf("submissionCache.GetStatus %s: %w", token, err)
	}
	return status, nil
}

// InvalidateSubmission deletes all cached keys for the given submission token
// (full document, status, and queue position).
func (sc *SubmissionCache) InvalidateSubmission(ctx context.Context, token string) error {
	keys := []string{
		submissionKey(token),
		statusKey(token),
		queuePosKey(token),
	}
	for _, key := range keys {
		if err := sc.cache.Delete(ctx, key); err != nil {
			return fmt.Errorf("submissionCache.InvalidateSubmission %s key=%s: %w", token, key, err)
		}
	}
	return nil
}

// SetQueuePosition stores the queue position for the given submission.
func (sc *SubmissionCache) SetQueuePosition(ctx context.Context, token string, position int) error {
	if err := sc.cache.Set(ctx, queuePosKey(token), position, QueuePositionTTL); err != nil {
		return fmt.Errorf("submissionCache.SetQueuePosition %s: %w", token, err)
	}
	return nil
}

// GetQueuePosition retrieves the cached queue position.
// Returns ErrCacheMiss (via IsCacheMiss) when the key is absent.
func (sc *SubmissionCache) GetQueuePosition(ctx context.Context, token string) (int, error) {
	var pos int
	if err := sc.cache.Get(ctx, queuePosKey(token), &pos); err != nil {
		return 0, fmt.Errorf("submissionCache.GetQueuePosition %s: %w", token, err)
	}
	return pos, nil
}

// RefreshTTL resets the TTL on the full submission document back to SubmissionTTL.
// Call this whenever the submission is accessed to implement an LRU-like policy.
func (sc *SubmissionCache) RefreshTTL(ctx context.Context, token string) error {
	if err := sc.cache.Expire(ctx, submissionKey(token), SubmissionTTL); err != nil {
		return fmt.Errorf("submissionCache.RefreshTTL %s: %w", token, err)
	}
	return nil
}

// ── SubmissionResponse cache (used by worker and result-waiter) ──────────────

const responseKeySuffix = ":resp"

func responseKey(token string) string {
	return keyPrefix + token + responseKeySuffix
}

// Set stores a SubmissionResponse (the API-facing shape) under its token.
// Workers call this after execution so the API layer can serve cached results.
func (sc *SubmissionCache) Set(ctx context.Context, token string, resp *models.SubmissionResponse) error {
	if err := sc.cache.Set(ctx, responseKey(token), resp, SubmissionTTL); err != nil {
		return fmt.Errorf("submissionCache.Set %s: %w", token, err)
	}
	return nil
}

// Get retrieves a cached SubmissionResponse. Returns (nil, nil) on cache miss.
func (sc *SubmissionCache) Get(ctx context.Context, token string) (*models.SubmissionResponse, error) {
	var resp models.SubmissionResponse
	if err := sc.cache.Get(ctx, responseKey(token), &resp); err != nil {
		if IsCacheMiss(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("submissionCache.Get %s: %w", token, err)
	}
	return &resp, nil
}

// PublishDone publishes a completion notification to the "done:{token}" Redis
// pub/sub channel so that result-waiters are unblocked immediately.
func (sc *SubmissionCache) PublishDone(ctx context.Context, token string) error {
	ch := "done:" + token
	return sc.cache.client.Publish(ctx, ch, token).Err()
}

// SubscribeDone returns a Redis PubSub handle for the "done:{token}" channel.
// The caller must close the subscription when it is no longer needed.
func (sc *SubmissionCache) SubscribeDone(ctx context.Context, token string) *redis.PubSub {
	ch := "done:" + token
	return sc.cache.client.Subscribe(ctx, ch)
}

// PollForCompletion blocks until the submission reaches a terminal status or
// the timeout elapses. It uses exponential back-off between polls.
func (sc *SubmissionCache) PollForCompletion(ctx context.Context, token string, timeout time.Duration) (*models.SubmissionResponse, error) {
	deadline := time.Now().Add(timeout)
	backoff := 200 * time.Millisecond

	for time.Now().Before(deadline) {
		resp, err := sc.Get(ctx, token)
		if err != nil {
			return nil, err
		}
		if resp != nil && isTerminalStatus(resp.Status.ID) {
			return resp, nil
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
			if backoff < 2*time.Second {
				backoff = time.Duration(float64(backoff) * 1.5)
			}
		}
	}

	return sc.Get(ctx, token)
}

func isTerminalStatus(id int) bool {
	return id >= models.StatusAccepted
}

// Delete removes all cached keys for the given token (response, status, queue position).
func (sc *SubmissionCache) Delete(ctx context.Context, token string) error {
	keys := []string{
		responseKey(token),
		statusKey(token),
		queuePosKey(token),
	}
	for _, k := range keys {
		_ = sc.cache.Delete(ctx, k)
	}
	return nil
}
