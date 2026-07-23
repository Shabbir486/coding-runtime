package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/revature/corems-code-executor/internal/database"
	"github.com/revature/corems-code-executor/internal/models"
)

// WebhookHandler handles all /webhooks routes.
type WebhookHandler struct {
	webhookRepo database.WebhookRepository
	log         *zap.Logger
}

// NewWebhookHandler constructs a WebhookHandler.
func NewWebhookHandler(webhookRepo database.WebhookRepository, log *zap.Logger) *WebhookHandler {
	return &WebhookHandler{webhookRepo: webhookRepo, log: log}
}

// RegisterWebhook handles POST /webhooks. It stores a webhook endpoint and its
// outbound X-API-Key. When the caller does not supply an api_key, one is
// generated and returned once in the response.
func (h *WebhookHandler) RegisterWebhook(c *gin.Context) {
	var req models.RegisterWebhookRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	apiKey := req.APIKey
	if apiKey == "" {
		generated, err := generateWebhookKey()
		if err != nil {
			h.log.Error("failed to generate webhook key", zap.Error(err))
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate webhook key"})
			return
		}
		apiKey = generated
	}

	hook := &models.Webhook{
		ID:       uuid.NewString(),
		UserID:   currentUserID(c),
		Name:     req.Name,
		URL:      req.URL,
		APIKey:   apiKey,
		IsActive: true,
	}

	if err := h.webhookRepo.Create(c.Request.Context(), hook); err != nil {
		h.log.Error("failed to create webhook", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create webhook"})
		return
	}

	// Include the key only on the create response (shown once).
	c.JSON(http.StatusCreated, models.WebhookToResponse(hook, true))
}

// ListWebhooks handles GET /webhooks — returns webhooks owned by the caller.
func (h *WebhookHandler) ListWebhooks(c *gin.Context) {
	hooks, err := h.webhookRepo.ListByUser(c.Request.Context(), currentUserID(c))
	if err != nil {
		h.log.Error("failed to list webhooks", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list webhooks"})
		return
	}

	responses := make([]models.WebhookResponse, len(hooks))
	for i, hook := range hooks {
		responses[i] = models.WebhookToResponse(hook, false)
	}
	c.JSON(http.StatusOK, gin.H{"webhooks": responses})
}

// GetWebhook handles GET /webhooks/:id.
func (h *WebhookHandler) GetWebhook(c *gin.Context) {
	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid webhook id format"})
		return
	}

	hook, err := h.webhookRepo.GetByID(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, database.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "webhook not found"})
			return
		}
		h.log.Error("failed to load webhook", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load webhook"})
		return
	}

	c.JSON(http.StatusOK, models.WebhookToResponse(hook, false))
}

// generateWebhookKey returns a cryptographically random hex key for outbound
// webhook authentication.
func generateWebhookKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
