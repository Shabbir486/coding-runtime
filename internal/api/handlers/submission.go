package handlers

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/mdshabbir-ali/code-runtime/internal/cache"
	"github.com/mdshabbir-ali/code-runtime/internal/config"
	"github.com/mdshabbir-ali/code-runtime/internal/database"
	"github.com/mdshabbir-ali/code-runtime/internal/metrics"
	"github.com/mdshabbir-ali/code-runtime/internal/models"
	"github.com/mdshabbir-ali/code-runtime/internal/queue"
)

const (
	waitTimeout    = 120 * time.Second
	defaultPage    = 1
	defaultPerPage = 20
	maxPerPage     = 100
)

// SubmissionHandler handles all /submissions routes.
type SubmissionHandler struct {
	submissionRepo database.SubmissionRepository
	languageRepo   database.LanguageRepository
	subCache       *cache.SubmissionCache
	queue          queue.Publisher
	cfg            *config.Config
	metrics        *metrics.APIMetrics
	log            *zap.Logger
}

// NewSubmissionHandler constructs a SubmissionHandler.
func NewSubmissionHandler(
	submissionRepo database.SubmissionRepository,
	languageRepo database.LanguageRepository,
	subCache *cache.SubmissionCache,
	q queue.Publisher,
	cfg *config.Config,
	m *metrics.APIMetrics,
	log *zap.Logger,
) *SubmissionHandler {
	return &SubmissionHandler{
		submissionRepo: submissionRepo,
		languageRepo:   languageRepo,
		subCache:       subCache,
		queue:          q,
		cfg:            cfg,
		metrics:        m,
		log:            log,
	}
}

// CreateSubmission handles POST /submissions.
func (h *SubmissionHandler) CreateSubmission(c *gin.Context) {
	var req models.SubmissionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// ?wait=true query parameter overrides the body field.
	if waitStr := c.Query("wait"); waitStr != "" {
		if v, err := strconv.ParseBool(waitStr); err == nil {
			req.Wait = v
		}
	}

	sub, sourceCode, stdin, err := h.buildSubmission(req)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := h.submissionRepo.Create(c.Request.Context(), sub); err != nil {
		h.log.Error("failed to create submission", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save submission"})
		return
	}

	h.seedCacheAndPublish(c, sub)
	h.metrics.SubmissionsCreated.Inc()

	if req.Wait {
		resp := h.pollSubmissionResult(c, sub.Token, sub, sourceCode, stdin)
		c.JSON(http.StatusCreated, resp)
		return
	}

	c.JSON(http.StatusCreated, gin.H{"token": sub.Token})
}

// buildSubmission decodes fields and constructs the Submission DB model.
func (h *SubmissionHandler) buildSubmission(req models.SubmissionRequest) (
	*models.Submission, string, string, error,
) {
	sourceCode, err := decodeIfBase64(req.SourceCode)
	if err != nil {
		return nil, "", "", errors.New("source_code: invalid base64")
	}
	stdin, err := decodeIfBase64(req.Stdin)
	if err != nil {
		return nil, "", "", errors.New("stdin: invalid base64")
	}
	expectedOutput, err := decodeIfBase64(req.ExpectedOutput)
	if err != nil {
		return nil, "", "", errors.New("expected_output: invalid base64")
	}

	token := uuid.NewString()
	sub := &models.Submission{
		Token:        token,
		LanguageID:   req.LanguageID,
		SourceCode:   sourceCode,
		StatusID:     models.StatusInQueue,
		CPUTimeLimit: req.CPUTimeLimit,
		MemoryLimit:  req.MemoryLimit,
	}
	if stdin != "" {
		sub.Stdin = &stdin
	}
	sub.ExpectedOutput = expectedOutput

	return sub, sourceCode, stdin, nil
}

// seedCacheAndPublish seeds the Redis cache and enqueues the submission.
func (h *SubmissionHandler) seedCacheAndPublish(c *gin.Context, sub *models.Submission) {
	initialResp := models.SubmissionToResponse(sub, false)
	_ = h.subCache.Set(c.Request.Context(), sub.Token, &initialResp)

	job := submissionToJob(sub)
	if err := h.queue.PublishJob(c.Request.Context(), job); err != nil {
		h.log.Error("failed to publish submission",
			zap.String("token", sub.Token), zap.Error(err))
		h.metrics.QueuePublishErrors.Inc()
	}
}

// submissionToJob converts a persisted Submission into an ExecutionJob for the queue.
func submissionToJob(sub *models.Submission) *models.ExecutionJob {
	var stdin string
	if sub.Stdin != nil {
		stdin = *sub.Stdin
	}
	return &models.ExecutionJob{
		SubmissionToken:    sub.Token,
		LanguageID:         sub.LanguageID,
		SourceCode:         sub.SourceCode,
		Stdin:              stdin,
		ExpectedOutput:     sub.ExpectedOutput,
		CPUTimeLimit:       sub.CPUTimeLimit,
		WallTimeLimit:      sub.WallTimeLimit,
		MemoryLimit:        sub.MemoryLimit,
		StackLimit:         sub.StackLimit,
		MaxProcesses:       sub.MaxProcesses,
		MaxFileSize:        sub.MaxFileSize,
		CompilerOptions:    sub.CompilerOptions,
		CommandLineArgs:    sub.CommandLineArgs,
		CallbackURL:        sub.CallbackURL,
		RetryCount:         0,
		Priority:           0,
	}
}

// pollSubmissionResult blocks up to waitTimeout for a terminal result and
// returns whatever response is available (never nil).
func (h *SubmissionHandler) pollSubmissionResult(
	c *gin.Context,
	token string,
	fallback *models.Submission,
	sourceCode, stdin string,
) *models.SubmissionResponse {
	pubsub := h.subCache.SubscribeDone(c.Request.Context(), token)
	defer pubsub.Close()

	// Drive polling; SubscribeDone gives the cache a channel to fast-path on.
	finalResp, err := h.subCache.PollForCompletion(c.Request.Context(), token, waitTimeout)
	if err != nil {
		h.log.Warn("wait error for submission", zap.String("token", token), zap.Error(err))
	}

	if finalResp != nil {
		finalResp.SourceCode = sourceCode
		finalResp.Stdin = stdin
		return finalResp
	}

	// Timeout — return the current in-queue state.
	resp := models.SubmissionToResponse(fallback, true)
	return &resp
}

// GetSubmission handles GET /submissions/:token.
func (h *SubmissionHandler) GetSubmission(c *gin.Context) {
	tokenStr := c.Param("token")
	if _, err := uuid.Parse(tokenStr); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid token format"})
		return
	}

	if resp := h.getFromCache(c, tokenStr); resp != nil {
		c.JSON(http.StatusOK, resp)
		return
	}

	sub, err := h.submissionRepo.GetByToken(c.Request.Context(), tokenStr)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "submission not found"})
		return
	}

	resp := models.SubmissionToResponse(sub, true)
	_ = h.subCache.Set(c.Request.Context(), tokenStr, &resp)
	c.JSON(http.StatusOK, resp)
}

// getFromCache returns a cached SubmissionResponse or nil on miss/error.
func (h *SubmissionHandler) getFromCache(c *gin.Context, token string) *models.SubmissionResponse {
	cached, err := h.subCache.Get(c.Request.Context(), token)
	if err != nil {
		h.log.Warn("cache get error", zap.Error(err))
		h.metrics.CacheMisses.WithLabelValues("submission").Inc()
		return nil
	}
	if cached != nil {
		h.metrics.CacheHits.WithLabelValues("submission").Inc()
		return cached
	}
	h.metrics.CacheMisses.WithLabelValues("submission").Inc()
	return nil
}

// CreateBatchSubmissions handles POST /submissions/batch.
func (h *SubmissionHandler) CreateBatchSubmissions(c *gin.Context) {
	var req models.BatchSubmissionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if len(req.Submissions) > 20 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "maximum 20 submissions per batch"})
		return
	}

	tokens, err := h.processBatch(c, req.Submissions)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	h.metrics.SubmissionsBatch.Inc()
	c.JSON(http.StatusCreated, models.BatchSubmissionResponse{Tokens: tokens})
}

// processBatch creates and enqueues all submissions in a batch request.
func (h *SubmissionHandler) processBatch(c *gin.Context, reqs []models.SubmissionRequest) ([]string, error) {
	tokens := make([]string, 0, len(reqs))

	for i := range reqs {
		sub, _, _, err := h.buildSubmission(reqs[i])
		if err != nil {
			return nil, err
		}
		if err := h.submissionRepo.Create(c.Request.Context(), sub); err != nil {
			h.log.Error("batch: create submission", zap.Error(err))
			return nil, err
		}
		h.seedCacheAndPublish(c, sub)
		tokens = append(tokens, sub.Token)
	}

	return tokens, nil
}

// ListSubmissions handles GET /submissions.
func (h *SubmissionHandler) ListSubmissions(c *gin.Context) {
	page, perPage := parsePagination(c)

	subs, total, err := h.submissionRepo.List(c.Request.Context(), page, perPage)
	if err != nil {
		h.log.Error("list submissions", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list submissions"})
		return
	}

	responses := make([]models.SubmissionResponse, len(subs))
	for i, s := range subs {
		responses[i] = models.SubmissionToResponse(s, false)
	}

	c.JSON(http.StatusOK, models.PaginatedSubmissionsResponse{
		Submissions: responses,
		Total:       total,
		Page:        page,
		PerPage:     perPage,
	})
}

// DeleteSubmission handles DELETE /submissions/:token.
func (h *SubmissionHandler) DeleteSubmission(c *gin.Context) {
	tokenStr := c.Param("token")
	if _, err := uuid.Parse(tokenStr); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid token format"})
		return
	}

	if err := h.submissionRepo.Delete(c.Request.Context(), tokenStr); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "submission not found"})
		return
	}

	_ = h.subCache.Delete(c.Request.Context(), tokenStr)
	c.Status(http.StatusNoContent)
}

// ---- package-level helpers --------------------------------------------------

// parsePagination extracts and clamps page/per_page query parameters.
func parsePagination(c *gin.Context) (page, perPage int) {
	page, _ = strconv.Atoi(c.DefaultQuery("page", strconv.Itoa(defaultPage)))
	perPage, _ = strconv.Atoi(c.DefaultQuery("per_page", strconv.Itoa(defaultPerPage)))
	if page < 1 {
		page = 1
	}
	if perPage < 1 || perPage > maxPerPage {
		perPage = defaultPerPage
	}
	return page, perPage
}

// decodeIfBase64 attempts to base64-decode s; returns the original string if
// s is not valid base64 (treats it as plain text).
func decodeIfBase64(s string) (string, error) {
	if s == "" {
		return s, nil
	}
	trimmed := strings.TrimSpace(s)
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding,
		base64.URLEncoding,
		base64.RawStdEncoding,
		base64.RawURLEncoding,
	} {
		if decoded, err := enc.DecodeString(trimmed); err == nil {
			return string(decoded), nil
		}
	}
	return s, nil
}

