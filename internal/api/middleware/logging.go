package middleware

import (
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// skipPaths holds URL prefixes for which request logging is suppressed.
var loggingSkipPaths = []string{
	"/health",
	"/ready",
	"/metrics",
}

// RequestLogger returns a structured Gin logging middleware backed by Zap.
// Every request receives a unique X-Request-ID header (generated if absent).
// Health, readiness, and metrics endpoints are excluded from log output.
func RequestLogger(log *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Attach or generate a request ID.
		reqID := c.GetHeader("X-Request-ID")
		if reqID == "" {
			reqID = uuid.NewString()
		}
		c.Set("request_id", reqID)
		c.Header("X-Request-ID", reqID)

		start := time.Now()
		path := c.Request.URL.Path
		rawQuery := c.Request.URL.RawQuery

		c.Next()

		// Skip noisy health/metrics endpoints.
		for _, skip := range loggingSkipPaths {
			if strings.HasPrefix(path, skip) {
				return
			}
		}

		duration := time.Since(start)
		statusCode := c.Writer.Status()

		fields := []zap.Field{
			zap.String("request_id", reqID),
			zap.String("method", c.Request.Method),
			zap.String("path", path),
			zap.Int("status", statusCode),
			zap.Duration("duration", duration),
			zap.String("ip", c.ClientIP()),
			zap.String("user_agent", c.Request.UserAgent()),
		}

		if rawQuery != "" {
			fields = append(fields, zap.String("query", rawQuery))
		}

		if len(c.Errors) > 0 {
			fields = append(fields, zap.String("errors", c.Errors.String()))
		}

		switch {
		case statusCode >= 500:
			log.Error("request", fields...)
		case statusCode >= 400:
			log.Warn("request", fields...)
		default:
			log.Info("request", fields...)
		}
	}
}
