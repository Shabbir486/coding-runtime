package middleware

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/ulule/limiter/v3"
	redisstore "github.com/ulule/limiter/v3/drivers/store/redis"
	"go.uber.org/zap"

	"github.com/mdshabbir-ali/code-runtime/internal/cache"
	"github.com/mdshabbir-ali/code-runtime/internal/config"
)

// RateLimit returns a Gin middleware that enforces a sliding-window rate limit.
// The limit is applied per IP (or per API key when one is present).
// It is backed by Redis so multiple API gateway replicas share state.
func RateLimit(cfg *config.Config, redisClient *cache.Client, log *zap.Logger) gin.HandlerFunc {
	if !cfg.RateLimit.Enabled {
		return func(c *gin.Context) { c.Next() }
	}

	rate := limiter.Rate{
		Limit:  cfg.RateLimit.Limit,
		Period: cfg.RateLimit.Period,
	}

	store, err := redisstore.NewStore(redisClient.RawClient())
	if err != nil {
		log.Error("failed to create Redis rate-limit store, falling back to allow-all", zap.Error(err))
		return func(c *gin.Context) { c.Next() }
	}

	ipLimiter := limiter.New(store, rate)

	return func(c *gin.Context) {
		// Build the rate-limit key — prefer API key ID over raw IP so that
		// legitimate users with many NAT'ed IPs are not unfairly blocked.
		key := rateLimitKey(c)

		ctx, err := ipLimiter.Get(c.Request.Context(), key)
		if err != nil {
			log.Warn("rate limiter error", zap.Error(err))
			// On error, let the request through to avoid cascading failure.
			c.Next()
			return
		}

		// Set informational headers.
		resetAt := time.Now().Add(rate.Period)
		c.Header("X-RateLimit-Limit", strconv.FormatInt(ctx.Limit, 10))
		c.Header("X-RateLimit-Remaining", strconv.FormatInt(ctx.Remaining, 10))
		c.Header("X-RateLimit-Reset", strconv.FormatInt(resetAt.Unix(), 10))

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

// rateLimitKey builds the Redis key used to track requests.
// API key callers get their own bucket; everyone else is bucketed by IP.
func rateLimitKey(c *gin.Context) string {
	if apiKeyID, exists := c.Get(string(ContextKeyAPIKeyID)); exists {
		return fmt.Sprintf("rl:apikey:%v", apiKeyID)
	}
	return fmt.Sprintf("rl:ip:%s", c.ClientIP())
}
