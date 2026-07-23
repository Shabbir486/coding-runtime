package middleware

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/ulule/limiter/v3"
	redisstore "github.com/ulule/limiter/v3/drivers/store/redis"
	"go.uber.org/zap"

	"github.com/revature/corems-code-executor/internal/cache"
	"github.com/revature/corems-code-executor/internal/config"
)

// RateLimiters bundles the per-route-class limiter middlewares. Each class has
// its own bucket namespace so high-frequency polling reads cannot exhaust the
// budget reserved for expensive writes (and vice versa).
type RateLimiters struct {
	Read  gin.HandlerFunc // GET / polling endpoints
	Write gin.HandlerFunc // POST / DELETE (submit, batch) endpoints
	Auth  gin.HandlerFunc // /auth endpoints (tight)
}

// NewRateLimiters builds the per-class limiters backed by a shared Redis store
// so multiple api-gateway replicas enforce one global budget. When rate
// limiting is disabled (or the store cannot be created) every class is a
// pass-through, so the API stays available rather than failing closed.
func NewRateLimiters(cfg *config.Config, redisClient *cache.Client, log *zap.Logger) RateLimiters {
	passThrough := func(c *gin.Context) { c.Next() }
	if !cfg.RateLimit.Enabled {
		return RateLimiters{Read: passThrough, Write: passThrough, Auth: passThrough}
	}

	store, err := redisstore.NewStore(redisClient.RawClient())
	if err != nil {
		log.Error("failed to create Redis rate-limit store, falling back to allow-all", zap.Error(err))
		return RateLimiters{Read: passThrough, Write: passThrough, Auth: passThrough}
	}

	trusted := parseTrustedKeys(cfg.RateLimit.TrustedAPIKeys, log)
	rl := cfg.RateLimit

	return RateLimiters{
		Read:  newLimiter(store, "read", tierRate(rl.ReadLimit, rl.ReadPeriod, rl), trusted, log),
		Write: newLimiter(store, "write", tierRate(rl.WriteLimit, rl.WritePeriod, rl), trusted, log),
		// Auth runs before authentication, so trusted-key bypass never applies here.
		Auth: newLimiter(store, "auth", tierRate(rl.AuthLimit, rl.AuthPeriod, rl), nil, log),
	}
}

// tierRate returns the tier's own rate, falling back to the default
// Limit/Period when the tier values are unset.
func tierRate(limit int64, period time.Duration, rl config.RateLimitConfig) limiter.Rate {
	if limit <= 0 {
		limit = rl.Limit
	}
	if period <= 0 {
		period = rl.Period
	}
	return limiter.Rate{Limit: limit, Period: period}
}

// newLimiter returns a Gin middleware enforcing rate within a named bucket
// namespace. Requests from a trusted API key bypass the limit entirely.
func newLimiter(
	store limiter.Store,
	name string,
	rate limiter.Rate,
	trusted map[uint]bool,
	log *zap.Logger,
) gin.HandlerFunc {
	lim := limiter.New(store, rate)

	return func(c *gin.Context) {
		if isTrusted(c, trusted) {
			c.Next()
			return
		}

		key := rateLimitKey(name, c)
		ctx, err := lim.Get(c.Request.Context(), key)
		if err != nil {
			log.Warn("rate limiter error", zap.Error(err))
			// On error, let the request through to avoid cascading failure.
			c.Next()
			return
		}

		resetAt := time.Unix(ctx.Reset, 0)
		c.Header("X-RateLimit-Limit", strconv.FormatInt(ctx.Limit, 10))
		c.Header("X-RateLimit-Remaining", strconv.FormatInt(ctx.Remaining, 10))
		c.Header("X-RateLimit-Reset", strconv.FormatInt(ctx.Reset, 10))

		if ctx.Reached {
			retryAfter := int(time.Until(resetAt).Seconds())
			if retryAfter < 1 {
				retryAfter = 1
			}
			c.Header("Retry-After", strconv.Itoa(retryAfter))
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error":       "rate limit exceeded",
				"retry_after": retryAfter,
			})
			return
		}

		c.Next()
	}
}

// isTrusted reports whether the caller authenticated with a trusted API key.
func isTrusted(c *gin.Context, trusted map[uint]bool) bool {
	if len(trusted) == 0 {
		return false
	}
	v, exists := c.Get(string(ContextKeyAPIKeyID))
	if !exists {
		return false
	}
	id, ok := v.(uint)
	return ok && trusted[id]
}

// rateLimitKey builds the Redis key used to track requests, namespaced per
// route class. API key callers get their own bucket; everyone else is bucketed
// by client IP.
func rateLimitKey(name string, c *gin.Context) string {
	if apiKeyID, exists := c.Get(string(ContextKeyAPIKeyID)); exists {
		return fmt.Sprintf("rl:%s:apikey:%v", name, apiKeyID)
	}
	return fmt.Sprintf("rl:%s:ip:%s", name, c.ClientIP())
}

// parseTrustedKeys converts a comma-separated list of API key IDs into a set.
func parseTrustedKeys(raw string, log *zap.Logger) map[uint]bool {
	out := make(map[uint]bool)
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			log.Warn("ignoring invalid trusted API key ID", zap.String("value", part))
			continue
		}
		out[uint(id)] = true
	}
	return out
}
