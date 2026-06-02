package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/mdshabbir-ali/code-runtime/internal/cache"
	"github.com/mdshabbir-ali/code-runtime/internal/database"
	"github.com/mdshabbir-ali/code-runtime/internal/models"
)

// StatusHandler handles /statuses, /health, and /ready routes.
type StatusHandler struct {
	db    *database.DB
	redis *cache.Client
	log   *zap.Logger
}

// NewStatusHandler constructs a StatusHandler.
func NewStatusHandler(db *database.DB, redis *cache.Client, log *zap.Logger) *StatusHandler {
	return &StatusHandler{db: db, redis: redis, log: log}
}

// GetStatuses handles GET /statuses — returns all known execution statuses.
func (h *StatusHandler) GetStatuses(c *gin.Context) {
	statuses := make([]models.StatusResponse, 0, len(models.StatusDescriptions))
	for id, desc := range models.StatusDescriptions {
		statuses = append(statuses, models.StatusResponse{
			ID:          id,
			Description: desc,
		})
	}
	// Sort by ID for deterministic output.
	for i := 0; i < len(statuses)-1; i++ {
		for j := i + 1; j < len(statuses); j++ {
			if statuses[i].ID > statuses[j].ID {
				statuses[i], statuses[j] = statuses[j], statuses[i]
			}
		}
	}
	c.JSON(http.StatusOK, statuses)
}

// HealthCheck handles GET /health — checks DB and Redis connectivity.
func (h *StatusHandler) HealthCheck(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()

	services := map[string]string{}
	overall := "ok"

	if err := h.db.Ping(ctx); err != nil {
		h.log.Warn("health: postgres unreachable", zap.Error(err))
		services["postgres"] = "unhealthy"
		overall = "degraded"
	} else {
		services["postgres"] = "ok"
	}

	if err := h.redis.Ping(ctx); err != nil {
		h.log.Warn("health: redis unreachable", zap.Error(err))
		services["redis"] = "unhealthy"
		overall = "degraded"
	} else {
		services["redis"] = "ok"
	}

	status := http.StatusOK
	if overall != "ok" {
		status = http.StatusServiceUnavailable
	}

	c.JSON(status, models.HealthResponse{
		Status:   overall,
		Services: services,
	})
}

// ReadinessCheck handles GET /ready — returns 200 only when all dependencies
// are reachable; otherwise 503.
func (h *StatusHandler) ReadinessCheck(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()

	if err := h.db.Ping(ctx); err != nil {
		h.log.Warn("readiness: postgres not ready", zap.Error(err))
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"ready":  false,
			"reason": "postgres unavailable",
		})
		return
	}

	if err := h.redis.Ping(ctx); err != nil {
		h.log.Warn("readiness: redis not ready", zap.Error(err))
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"ready":  false,
			"reason": "redis unavailable",
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{"ready": true})
}
