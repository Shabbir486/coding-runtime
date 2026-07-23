package handlers

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/revature/corems-code-executor/internal/cache"
	"github.com/revature/corems-code-executor/internal/database"
	"github.com/revature/corems-code-executor/internal/models"
)

// LanguageHandler handles /languages routes.
type LanguageHandler struct {
	repo    database.LanguageRepository
	langCache *cache.LanguageCache
	log     *zap.Logger
}

// NewLanguageHandler constructs a LanguageHandler.
func NewLanguageHandler(
	repo database.LanguageRepository,
	langCache *cache.LanguageCache,
	log *zap.Logger,
) *LanguageHandler {
	return &LanguageHandler{repo: repo, langCache: langCache, log: log}
}

// GetLanguages handles GET /languages.
// It returns all non-archived languages, using a 1-hour Redis cache.
func (h *LanguageHandler) GetLanguages(c *gin.Context) {
	// Optional filter: ?isActive=true (active only) | false (inactive only).
	// Absent => return all. Any other value is a 400.
	var activeFilter *bool
	if s := c.Query("isActive"); s != "" {
		v, parseErr := strconv.ParseBool(s)
		if parseErr != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "isActive must be true or false"})
			return
		}
		activeFilter = &v
	}

	// 1. Try cache (always the FULL list). 2. Fall back to DB and cache it.
	full, err := h.langCache.GetAll(c.Request.Context())
	if err != nil {
		h.log.Warn("language cache GetAll error", zap.Error(err))
	}
	if full == nil {
		full, err = h.repo.GetAll(c.Request.Context())
		if err != nil {
			h.log.Error("GetLanguages DB error", zap.Error(err))
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch languages"})
			return
		}
		// Cache the full list asynchronously (filter is applied per-request below).
		toCache := full
		go func() {
			if cacheErr := h.langCache.SetAll(c.Request.Context(), toCache); cacheErr != nil {
				h.log.Warn("language cache SetAll error", zap.Error(cacheErr))
			}
		}()
	}

	// 3. Apply the active/inactive filter (if any) after caching the full set.
	result := full
	if activeFilter != nil {
		result = make([]*models.Language, 0, len(full))
		for _, l := range full {
			if l.IsActive == *activeFilter {
				result = append(result, l)
			}
		}
	}

	c.JSON(http.StatusOK, languagesToResponse(result))
}

// GetLanguage handles GET /languages/:id.
func (h *LanguageHandler) GetLanguage(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.Atoi(idStr)
	if err != nil || id < 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid language id"})
		return
	}

	// 1. Try cache.
	lang, err := h.langCache.GetByID(c.Request.Context(), id)
	if err != nil {
		h.log.Warn("language cache GetByID error", zap.Error(err))
	}
	if lang != nil {
		c.JSON(http.StatusOK, languageToResponse(lang))
		return
	}

	// 2. Cache miss.
	lang, err = h.repo.GetByID(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "language not found"})
		return
	}

	go func() {
		if cacheErr := h.langCache.Set(c.Request.Context(), lang); cacheErr != nil {
			h.log.Warn("language cache Set error", zap.Error(cacheErr))
		}
	}()

	c.JSON(http.StatusOK, languageToResponse(lang))
}

// SetLanguageActive handles PATCH /languages/:id/active — activate or
// deactivate a language by ID. Body: {"is_active": true|false}. Admin-only.
// A deactivated language is hidden from the active list and can be used to
// enable/disable a runtime without deleting it.
func (h *LanguageHandler) SetLanguageActive(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.Atoi(idStr)
	if err != nil || id < 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid language id"})
		return
	}

	var req models.SetLanguageActiveRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "body must be {\"is_active\": true|false}"})
		return
	}

	lang, err := h.repo.SetActive(c.Request.Context(), id, *req.IsActive)
	if err != nil {
		if errors.Is(err, database.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "language not found"})
			return
		}
		h.log.Error("SetLanguageActive DB error", zap.Int("id", id), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update language"})
		return
	}

	// Invalidate the list + single-language cache so readers see the change
	// immediately (GET /languages is cached for 1h).
	if cacheErr := h.langCache.Invalidate(c.Request.Context(), id); cacheErr != nil {
		h.log.Warn("language cache invalidate error", zap.Int("id", id), zap.Error(cacheErr))
	}

	h.log.Info("language active flag updated", zap.Int("id", id), zap.Bool("is_active", lang.IsActive))
	c.JSON(http.StatusOK, languageToResponse(lang))
}

// languageToResponse converts a Language model to its response DTO.
func languageToResponse(l *models.Language) models.LanguageResponse {
	return models.LanguageResponse{
		ID:             l.ID,
		Name:           l.Name,
		Version:        l.Version,
		IsActive:       l.IsActive,
		SourceFile:     l.SourceFile,
		CompileCommand: l.CompileCommand,
		RunCommand:     l.RunCommand,
		Image:          l.Image,
		MaxCPUTime:     l.MaxCPUTime,
		MaxMemory:      l.MaxMemory,
	}
}

// languagesToResponse converts a slice of Language models to response DTOs.
func languagesToResponse(langs []*models.Language) []models.LanguageResponse {
	out := make([]models.LanguageResponse, len(langs))
	for i := range langs {
		out[i] = languageToResponse(langs[i])
	}
	return out
}
