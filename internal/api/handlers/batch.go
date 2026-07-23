package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/revature/corems-code-executor/internal/api/middleware"
	"github.com/revature/corems-code-executor/internal/batchproc"
	"github.com/revature/corems-code-executor/internal/database"
	"github.com/revature/corems-code-executor/internal/metrics"
	"github.com/revature/corems-code-executor/internal/models"
	"github.com/revature/corems-code-executor/internal/webhook"
)

// defaultMaxBatchSize caps how many submissions a single batch may contain when
// no value is configured.
const defaultMaxBatchSize = 20

// Bulk webhook re-delivery tuning. Kept deliberately modest so a sweep over a
// very large backlog stays flat on CPU/memory and gentle on the DB + receivers.
const (
	// bulkRetriggerPageSize is how many batch IDs are pulled per DB round-trip.
	// Small = cheap, index-only queries and bounded memory; large enough to amortise.
	bulkRetriggerPageSize = 200
	// bulkRetriggerConcurrency caps simultaneous in-flight webhook deliveries.
	bulkRetriggerConcurrency = 8
	// bulkRetriggerMaxRuntime bounds the whole detached sweep as a safety net.
	bulkRetriggerMaxRuntime = 2 * time.Hour
)

// BatchHandler handles all /batches routes.
type BatchHandler struct {
	batchRepo    database.BatchRepository
	webhookRepo  database.WebhookRepository
	starter      *batchproc.Starter
	dispatcher   *webhook.Dispatcher
	metrics      *metrics.APIMetrics
	maxBatchSize int
	log          *zap.Logger
}

// BatchHandlerDeps groups BatchHandler dependencies to keep the constructor flat.
type BatchHandlerDeps struct {
	BatchRepo   database.BatchRepository
	WebhookRepo database.WebhookRepository
	Starter     *batchproc.Starter
	Dispatcher  *webhook.Dispatcher
	Metrics     *metrics.APIMetrics
	// MaxBatchSize caps submissions per batch; <=0 falls back to the default.
	MaxBatchSize int
	Log          *zap.Logger
}

// NewBatchHandler constructs a BatchHandler.
func NewBatchHandler(deps BatchHandlerDeps) *BatchHandler {
	maxBatchSize := deps.MaxBatchSize
	if maxBatchSize <= 0 {
		maxBatchSize = defaultMaxBatchSize
	}
	return &BatchHandler{
		batchRepo:    deps.BatchRepo,
		webhookRepo:  deps.WebhookRepo,
		starter:      deps.Starter,
		dispatcher:   deps.Dispatcher,
		metrics:      deps.Metrics,
		maxBatchSize: maxBatchSize,
		log:          deps.Log,
	}
}

// CreateBatch handles POST /batches. It atomically persists the batch and all
// its submissions in PENDING state and returns the batch_id + tokens. It does
// NOT start execution — that happens only when the batch is later started (via
// a START_BATCH_PROCESSING event or POST /batches/:id/start), so a consumer of
// the result webhook can never observe results before the caller has committed
// its own token/batch persistence.
func (h *BatchHandler) CreateBatch(c *gin.Context) {
	var req models.CreateBatchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if len(req.Submissions) > h.maxBatchSize {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf("maximum %d submissions per batch", h.maxBatchSize),
		})
		return
	}

	ctx := c.Request.Context()

	webhookID, err := h.resolveWebhook(c, req.WebhookID)
	if err != nil {
		return // resolveWebhook already wrote the response
	}

	batch := &models.Batch{
		ID:            uuid.NewString(),
		UserID:        currentUserID(c),
		WebhookID:     webhookID,
		Status:        models.BatchStatusPending,
		Total:         len(req.Submissions),
		WebhookStatus: models.WebhookStatusPending,
	}

	subs, tokens, err := buildBatchSubmissions(batch.ID, req.Submissions)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Batch + all submissions persist atomically; no execution is triggered.
	if err := h.batchRepo.CreateWithSubmissions(ctx, batch, subs); err != nil {
		h.log.Error("failed to create batch", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create batch"})
		return
	}

	h.metrics.SubmissionsBatch.Inc()
	c.JSON(http.StatusCreated, models.CreateBatchResponse{
		BatchID: batch.ID,
		Tokens:  tokens,
		Total:   batch.Total,
		Status:  batch.Status, // pending
	})
}

// StartBatch handles POST /batches/:id/start — the explicit, post-persistence
// trigger that begins execution. Idempotent: starting an already-started or
// finished batch is a no-op. The SQS start-queue consumer invokes the same
// underlying Starter, so this endpoint and START_BATCH_PROCESSING are equivalent.
func (h *BatchHandler) StartBatch(c *gin.Context) {
	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid batch id format"})
		return
	}

	// Surface a clear 404 for unknown batches.
	if _, err := h.batchRepo.GetByID(c.Request.Context(), id); err != nil {
		if errors.Is(err, database.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "batch not found"})
			return
		}
		h.log.Error("start: load batch", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load batch"})
		return
	}

	switch err := h.starter.Start(c.Request.Context(), id); {
	case err == nil:
		c.JSON(http.StatusAccepted, gin.H{"batch_id": id, "status": models.BatchStatusProcessing})
	case errors.Is(err, batchproc.ErrNotPending):
		// Already started/finished — idempotent.
		c.JSON(http.StatusOK, gin.H{"batch_id": id, "status": "already_started"})
	default:
		h.log.Error("failed to start batch", zap.String("batch_id", id), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start batch"})
	}
}

// resolveWebhook validates an optional webhook_id and returns a pointer to it.
// It writes an error response and returns a non-nil error when the id is given
// but does not resolve to an existing webhook.
func (h *BatchHandler) resolveWebhook(c *gin.Context, id string) (*string, error) {
	if id == "" {
		return nil, nil
	}
	hook, err := h.webhookRepo.GetByID(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, database.ErrNotFound) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "webhook_id not found"})
		} else {
			h.log.Error("failed to look up webhook", zap.Error(err))
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to validate webhook"})
		}
		return nil, err
	}
	hid := hook.ID
	return &hid, nil
}

// buildBatchSubmissions decodes each request into a Submission tagged with the
// batch_id and returns the submissions plus their tokens (request order). It
// performs no I/O — persistence happens atomically in CreateWithSubmissions and
// execution is deferred until the batch is started.
func buildBatchSubmissions(batchID string, reqs []models.SubmissionRequest) ([]*models.Submission, []string, error) {
	subs := make([]*models.Submission, 0, len(reqs))
	tokens := make([]string, 0, len(reqs))
	for i := range reqs {
		sub, _, _, err := buildSubmission(reqs[i])
		if err != nil {
			return nil, nil, err
		}
		bid := batchID
		sub.BatchID = &bid
		subs = append(subs, sub)
		tokens = append(tokens, sub.Token)
	}
	return subs, tokens, nil
}

// GetBatch handles GET /batches/:id — returns batch progress plus every
// submission's current result.
func (h *BatchHandler) GetBatch(c *gin.Context) {
	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid batch id format"})
		return
	}

	ctx := c.Request.Context()
	batch, err := h.batchRepo.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, database.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "batch not found"})
			return
		}
		h.log.Error("failed to load batch", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load batch"})
		return
	}

	subs, err := h.batchRepo.ListSubmissions(ctx, id)
	if err != nil {
		h.log.Error("failed to load batch submissions", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load batch submissions"})
		return
	}

	c.JSON(http.StatusOK, models.BatchToResponse(batch, subs))
}

// RetriggerWebhook handles POST /batches/:id/callback — re-delivers the batch
// result to the linked webhook on demand.
func (h *BatchHandler) RetriggerWebhook(c *gin.Context) {
	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid batch id format"})
		return
	}

	err := h.dispatcher.DispatchBatch(c.Request.Context(), id)
	switch {
	case err == nil:
		c.JSON(http.StatusOK, gin.H{"batch_id": id, "webhook_status": models.WebhookStatusSent})
	case errors.Is(err, webhook.ErrNoWebhook):
		c.JSON(http.StatusBadRequest, gin.H{"error": "batch has no linked webhook"})
	case errors.Is(err, database.ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "batch not found"})
	default:
		h.log.Warn("webhook re-trigger failed", zap.String("batch_id", id), zap.Error(err))
		c.JSON(http.StatusBadGateway, gin.H{
			"batch_id":       id,
			"webhook_status": models.WebhookStatusFailed,
			"error":          "webhook delivery failed",
		})
	}
}

// BulkRetriggerRequest is the body for POST /webhooks/retrigger.
type BulkRetriggerRequest struct {
	// Status selects which batches to re-notify: "pending" (webhook never
	// succeeded) or "failed" (webhook attempted but failed).
	Status string `json:"status" binding:"required"`
}

// BulkRetriggerResponse is returned (202) when a bulk re-delivery is accepted.
type BulkRetriggerResponse struct {
	Status      string `json:"status"`
	Matched     int64  `json:"matched"`
	PageSize    int    `json:"page_size"`
	Concurrency int    `json:"concurrency"`
	Message     string `json:"message"`
}

// BulkRetriggerWebhooks handles POST /webhooks/retrigger — re-delivers the
// webhook for EVERY completed batch currently in the given webhook_status
// ("pending" or "failed"). It is optimised for large backlogs: batch IDs are
// pulled in keyset-paginated, id-only pages (cheap indexed scans, flat memory)
// and delivered with bounded concurrency, so it never spikes CPU/memory or the
// DB. The work runs in the background and the endpoint returns 202 with the
// matched count immediately. A single successful delivery flips a batch to
// "sent" (leaving the set); failures stay "failed", so the call is safely
// re-runnable to drain stragglers.
func (h *BatchHandler) BulkRetriggerWebhooks(c *gin.Context) {
	var req BulkRetriggerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	status := strings.ToLower(strings.TrimSpace(req.Status))
	if status != models.WebhookStatusPending && status != models.WebhookStatusFailed {
		c.JSON(http.StatusBadRequest, gin.H{"error": "status must be 'pending' or 'failed'"})
		return
	}

	matched, err := h.batchRepo.CountBatchesByWebhookStatus(c.Request.Context(), status)
	if err != nil {
		h.log.Error("bulk retrigger: count failed", zap.String("status", status), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to count batches"})
		return
	}
	if matched == 0 {
		c.JSON(http.StatusOK, BulkRetriggerResponse{
			Status: status, Matched: 0, PageSize: bulkRetriggerPageSize,
			Concurrency: bulkRetriggerConcurrency, Message: "no eligible batches for this status",
		})
		return
	}

	// Detached context: the HTTP request returns now; the sweep continues.
	go h.runBulkRetrigger(status, matched)

	c.JSON(http.StatusAccepted, BulkRetriggerResponse{
		Status: status, Matched: matched, PageSize: bulkRetriggerPageSize,
		Concurrency: bulkRetriggerConcurrency,
		Message:     "webhook re-delivery started in the background",
	})
}

// runBulkRetrigger sweeps every eligible batch for the given webhook_status and
// re-delivers its webhook with bounded concurrency. It walks the set with
// keyset pagination (id > lastID), draining each page before fetching the next
// so memory and DB pressure stay flat regardless of backlog size.
func (h *BatchHandler) runBulkRetrigger(status string, matched int64) {
	ctx, cancel := context.WithTimeout(context.Background(), bulkRetriggerMaxRuntime)
	defer cancel()

	start := time.Now()
	var sent, failed int64
	afterID := ""
	sem := make(chan struct{}, bulkRetriggerConcurrency)

	for {
		ids, err := h.batchRepo.ListBatchIDsByWebhookStatus(ctx, status, afterID, bulkRetriggerPageSize)
		if err != nil {
			h.log.Error("bulk retrigger: page fetch failed",
				zap.String("status", status), zap.String("after_id", afterID), zap.Error(err))
			break
		}
		if len(ids) == 0 {
			break
		}
		// Advance the keyset cursor past this whole page (ids are ordered by id),
		// so successful rows flipping to "sent" never cause a revisit or a skip.
		afterID = ids[len(ids)-1]

		var wg sync.WaitGroup
		for _, id := range ids {
			select {
			case <-ctx.Done():
				h.log.Warn("bulk retrigger: context done, stopping", zap.String("status", status))
				wg.Wait()
				h.logBulkDone(status, matched, &sent, &failed, start)
				return
			case sem <- struct{}{}:
			}
			wg.Add(1)
			go func(batchID string) {
				defer wg.Done()
				defer func() { <-sem }()
				if err := h.dispatcher.DispatchBatch(ctx, batchID); err != nil {
					atomic.AddInt64(&failed, 1)
					return
				}
				atomic.AddInt64(&sent, 1)
			}(id)
		}
		wg.Wait() // drain this page before pulling the next (steady, bounded load)

		if len(ids) < bulkRetriggerPageSize {
			break // last page
		}
	}
	h.logBulkDone(status, matched, &sent, &failed, start)
}

func (h *BatchHandler) logBulkDone(status string, matched int64, sent, failed *int64, start time.Time) {
	h.log.Info("bulk webhook re-delivery finished",
		zap.String("status", status),
		zap.Int64("matched", matched),
		zap.Int64("sent", atomic.LoadInt64(sent)),
		zap.Int64("failed", atomic.LoadInt64(failed)),
		zap.Duration("took", time.Since(start)))
}

// BulkStartResponse is returned by POST /batches/start-pending.
type BulkStartResponse struct {
	Matched     int64  `json:"matched"`
	PageSize    int    `json:"page_size"`
	Concurrency int    `json:"concurrency"`
	Message     string `json:"message"`
}

// BulkStartPendingBatches handles POST /batches/start-pending — starts execution
// for EVERY batch stuck in the "pending" batch status (created but never
// started). Starting fans the batch's submissions onto the queue; when the batch
// later completes, its linked webhook fires automatically — so this is the fix
// for batches that never ran (the webhook re-delivery endpoint only touches
// already-completed batches). Optimised identically: batch IDs are walked in
// keyset-paginated, id-only pages and started with bounded concurrency, so it
// does not spike CPU/memory or the database. Runs in the background and returns
// 202 immediately with the matched count. Starting flips pending→processing
// (leaving the set) and Start is idempotent, so it is safely re-runnable.
// Admin-only.
func (h *BatchHandler) BulkStartPendingBatches(c *gin.Context) {
	matched, err := h.batchRepo.CountBatchesByStatus(c.Request.Context(), models.BatchStatusPending)
	if err != nil {
		h.log.Error("bulk start: count failed", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to count batches"})
		return
	}
	if matched == 0 {
		c.JSON(http.StatusOK, BulkStartResponse{
			Matched: 0, PageSize: bulkRetriggerPageSize,
			Concurrency: bulkRetriggerConcurrency, Message: "no pending batches to start",
		})
		return
	}

	// Detached context: the request returns now; the sweep continues.
	go h.runBulkStart(matched)

	c.JSON(http.StatusAccepted, BulkStartResponse{
		Matched: matched, PageSize: bulkRetriggerPageSize,
		Concurrency: bulkRetriggerConcurrency,
		Message:     "batch processing started in the background",
	})
}

// runBulkStart sweeps every pending batch and starts it with bounded
// concurrency, keyset-paginating (id > lastID) and draining each page before the
// next so memory and DB pressure stay flat regardless of backlog size.
func (h *BatchHandler) runBulkStart(matched int64) {
	ctx, cancel := context.WithTimeout(context.Background(), bulkRetriggerMaxRuntime)
	defer cancel()

	start := time.Now()
	var started, skipped, failed int64
	afterID := ""
	sem := make(chan struct{}, bulkRetriggerConcurrency)

	done := func() {
		h.log.Info("bulk batch start finished",
			zap.Int64("matched", matched),
			zap.Int64("started", atomic.LoadInt64(&started)),
			zap.Int64("skipped", atomic.LoadInt64(&skipped)),
			zap.Int64("failed", atomic.LoadInt64(&failed)),
			zap.Duration("took", time.Since(start)))
	}

	for {
		ids, err := h.batchRepo.ListBatchIDsByStatus(ctx, models.BatchStatusPending, afterID, bulkRetriggerPageSize)
		if err != nil {
			h.log.Error("bulk start: page fetch failed",
				zap.String("after_id", afterID), zap.Error(err))
			break
		}
		if len(ids) == 0 {
			break
		}
		afterID = ids[len(ids)-1] // advance the cursor past the whole page

		var wg sync.WaitGroup
		for _, id := range ids {
			select {
			case <-ctx.Done():
				h.log.Warn("bulk start: context done, stopping")
				wg.Wait()
				done()
				return
			case sem <- struct{}{}:
			}
			wg.Add(1)
			go func(batchID string) {
				defer wg.Done()
				defer func() { <-sem }()
				switch err := h.starter.Start(ctx, batchID); {
				case err == nil:
					atomic.AddInt64(&started, 1)
				case errors.Is(err, batchproc.ErrNotPending):
					atomic.AddInt64(&skipped, 1) // already started/finished — idempotent
				default:
					atomic.AddInt64(&failed, 1)
					h.log.Warn("bulk start: start failed", zap.String("batch_id", batchID), zap.Error(err))
				}
			}(id)
		}
		wg.Wait() // drain this page before the next (steady, bounded load)

		if len(ids) < bulkRetriggerPageSize {
			break // last page
		}
	}
	done()
}

// currentUserID extracts the authenticated user id from the gin context, if any.
func currentUserID(c *gin.Context) *uint {
	v, ok := c.Get(string(middleware.ContextKeyUserID))
	if !ok {
		return nil
	}
	if uid, ok := v.(uint); ok {
		return &uid
	}
	return nil
}
