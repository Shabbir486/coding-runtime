package middleware

import (
	"time"

	"github.com/gin-gonic/gin"
	"github.com/revature/corems-code-executor/internal/metrics"
)

// Metrics records HTTP request metrics.
func Metrics(m *metrics.APIMetrics) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		
		m.HTTPRequestsInFlight.Inc()
		defer m.HTTPRequestsInFlight.Dec()

		c.Next()

		duration := time.Since(start)
		m.ObserveRequest(c.Request.Method, c.FullPath(), c.Writer.Status(), duration)
	}
}
