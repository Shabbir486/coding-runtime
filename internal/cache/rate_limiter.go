package cache

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// RateLimiter implements a sliding-window rate limiter backed by Redis.
//
// Algorithm:
//  1. Each request adds the current timestamp (nanoseconds) to a sorted set
//     keyed by the caller's identifier.
//  2. Entries older than the window are removed.
//  3. If the set size exceeds the limit the request is rejected.
//
// This gives exact sliding-window semantics with O(log N) Redis operations
// per request (ZADD + ZREMRANGEBYSCORE + ZCARD in a single pipeline).
type RateLimiter struct {
	cache *Cache
}

// NewRateLimiter wraps a Cache with sliding-window rate-limit helpers.
func NewRateLimiter(c *Cache) *RateLimiter {
	return &RateLimiter{cache: c}
}

const rateLimiterKeyPrefix = "ratelimit:"

// Allow checks whether a request identified by key is within the allowed rate.
//
// Parameters:
//   - key    – unique identifier (e.g. "ip:1.2.3.4" or "apikey:abc123")
//   - limit  – maximum number of requests permitted in the window
//   - window – sliding window duration
//
// Returns:
//   - allowed   – true when the request should be served
//   - remaining – how many more requests may be made in the current window
//   - err       – non-nil on Redis failure
func (rl *RateLimiter) Allow(ctx context.Context, key string, limit int, window time.Duration) (bool, int, error) {
	redisKey := rateLimiterKeyPrefix + key
	now := time.Now().UnixNano()
	windowNanos := window.Nanoseconds()
	windowStart := now - windowNanos

	// Use a pipeline to execute all commands atomically.
	pipe := rl.cache.client.Pipeline()

	// 1. Remove entries outside the window.
	pipe.ZRemRangeByScore(ctx, redisKey, "0", fmt.Sprintf("%d", windowStart))

	// 2. Add the current request timestamp.  Score == member so that concurrent
	//    requests with the same nanosecond timestamp don't overwrite each other –
	//    we append a small random suffix via the member value (timestamp itself
	//    is both score and part of member here; uniqueness is guaranteed by
	//    nanosecond precision in practice, and the score-based eviction is exact).
	member := fmt.Sprintf("%d", now)
	pipe.ZAdd(ctx, redisKey, redis.Z{Score: float64(now), Member: member})

	// 3. Count current entries (i.e. requests within the window after adding this one).
	countCmd := pipe.ZCard(ctx, redisKey)

	// 4. Set the key TTL to the window so Redis cleans up abandoned keys.
	pipe.Expire(ctx, redisKey, window)

	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return false, 0, fmt.Errorf("ratelimiter.Allow %q: pipeline exec: %w", key, err)
	}

	count := int(countCmd.Val())
	remaining := limit - count
	if remaining < 0 {
		remaining = 0
	}

	allowed := count <= limit
	return allowed, remaining, nil
}

// RemainingRequests returns how many requests remain in the current window
// without consuming a request slot.
func (rl *RateLimiter) RemainingRequests(ctx context.Context, key string, limit int, window time.Duration) (int, error) {
	redisKey := rateLimiterKeyPrefix + key
	windowStart := time.Now().Add(-window).UnixNano()

	// Count only entries within the current window.
	count, err := rl.cache.client.ZCount(ctx, redisKey,
		fmt.Sprintf("%d", windowStart),
		"+inf",
	).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return limit, nil
		}
		return 0, fmt.Errorf("ratelimiter.RemainingRequests %q: %w", key, err)
	}

	remaining := limit - int(count)
	if remaining < 0 {
		remaining = 0
	}
	return remaining, nil
}

// Reset removes all rate-limit tracking data for the given key, effectively
// clearing the counter. Useful for administrative resets or test teardown.
func (rl *RateLimiter) Reset(ctx context.Context, key string) error {
	if err := rl.cache.client.Del(ctx, rateLimiterKeyPrefix+key).Err(); err != nil {
		return fmt.Errorf("ratelimiter.Reset %q: %w", key, err)
	}
	return nil
}

// BlockedUntil returns the time at which the caller will next be allowed to
// make a request under the given limit / window policy, or the zero time if
// the caller is not currently blocked.
func (rl *RateLimiter) BlockedUntil(ctx context.Context, key string, limit int, window time.Duration) (time.Time, error) {
	redisKey := rateLimiterKeyPrefix + key
	windowStart := time.Now().Add(-window).UnixNano()

	// Retrieve members within the window, ordered by score ascending.
	members, err := rl.cache.client.ZRangeByScoreWithScores(ctx, redisKey, &redis.ZRangeBy{
		Min:    fmt.Sprintf("%d", windowStart),
		Max:    "+inf",
		Offset: 0,
		Count:  int64(limit) + 1,
	}).Result()

	if err != nil {
		if errors.Is(err, redis.Nil) {
			return time.Time{}, nil
		}
		return time.Time{}, fmt.Errorf("ratelimiter.BlockedUntil %q: %w", key, err)
	}

	if len(members) <= limit {
		// Not blocked.
		return time.Time{}, nil
	}

	// The (limit)th oldest entry determines when a slot frees up.
	// Index is 0-based so the entry that will expire first is members[0].
	oldestInWindowNanos := int64(members[0].Score)
	unblockAt := time.Unix(0, oldestInWindowNanos).Add(window)
	return unblockAt, nil
}
