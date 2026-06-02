package handlers

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/mdshabbir-ali/code-runtime/internal/cache"
	"github.com/mdshabbir-ali/code-runtime/internal/database"
	"github.com/mdshabbir-ali/code-runtime/internal/models"
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
	// 1. Try cache.
	langs, err := h.langCache.GetAll(c.Request.Context())
	if err != nil {
		h.log.Warn("language cache GetAll error", zap.Error(err))
	}
	if langs != nil {
		c.JSON(http.StatusOK, languagesToResponse(langs))
		return
	}

	// 2. Cache miss — query DB.
	langs, err = h.repo.GetAll(c.Request.Context())
	if err != nil {
		h.log.Error("GetLanguages DB error", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch languages"})
		return
	}

	// 3. Populate cache asynchronously so we don't block the response.
	go func() {
		if cacheErr := h.langCache.SetAll(c.Request.Context(), langs); cacheErr != nil {
			h.log.Warn("language cache SetAll error", zap.Error(cacheErr))
		}
	}()

	c.JSON(http.StatusOK, languagesToResponse(langs))
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
