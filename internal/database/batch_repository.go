package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/revature/corems-code-executor/internal/models"
)

// -----------------------------------------------------------------------
// Interfaces
// -----------------------------------------------------------------------

// BatchRepository defines persistence operations for submission batches.
type BatchRepository interface {
	Create(ctx context.Context, batch *models.Batch) error
	// CreateWithSubmissions persists the batch and all its submissions in a
	// single transaction so a partial failure cannot strand a batch.
	CreateWithSubmissions(ctx context.Context, batch *models.Batch, subs []*models.Submission) error
	GetByID(ctx context.Context, id string) (*models.Batch, error)
	ListSubmissions(ctx context.Context, batchID string) ([]*models.Submission, error)
	// MarkProcessing atomically transitions a batch from pending → processing.
	// It returns started=true only for the caller that performed the transition,
	// giving callers an idempotent "start" gate (safe under at-least-once events).
	MarkProcessing(ctx context.Context, id string) (started bool, err error)
	// MarkPending rolls a batch processing → pending (used to allow a retry when
	// fan-out fails after the batch was already marked processing).
	MarkPending(ctx context.Context, id string) error
	// IncrementCompleted atomically bumps the completed counter by one and
	// returns the resulting batch row. It also advances Status to
	// "processing" while work remains and "completed" once all submissions
	// finish, in a single statement to avoid lost updates under concurrency.
	IncrementCompleted(ctx context.Context, id string) (*models.Batch, error)
	// MarkWebhook records the terminal webhook delivery outcome on the batch:
	// its status, the number of retries that were made, and (on success) the
	// delivery timestamp.
	MarkWebhook(ctx context.Context, id, status string, sentAt *time.Time, retries int) error
	// ListReconcilableBatches returns completed batches whose linked webhook was
	// never delivered (pending/failed) — the durable safety net for deliveries
	// lost when a worker restarts mid-dispatch. `grace` skips batches completed
	// too recently (letting the worker's immediate dispatch finish first) and
	// `maxAge` ignores batches too old to bother retrying.
	ListReconcilableBatches(ctx context.Context, grace, maxAge time.Duration, limit int) ([]*models.Batch, error)
	// CountBatchesByWebhookStatus counts COMPLETED batches with a linked webhook
	// currently in the given webhook_status (e.g. "pending"/"failed") — the
	// eligible set for a bulk re-delivery.
	CountBatchesByWebhookStatus(ctx context.Context, webhookStatus string) (int64, error)
	// ListBatchIDsByWebhookStatus returns up to limit eligible batch IDs (see
	// CountBatchesByWebhookStatus) whose id sorts after afterID, ordered by id.
	// This is keyset pagination on the primary key, so a full sweep stays flat in
	// memory and cheap on the DB (indexed range scan, id-only projection). Pass
	// "" for afterID to start from the beginning.
	ListBatchIDsByWebhookStatus(ctx context.Context, webhookStatus, afterID string, limit int) ([]string, error)
	// CountBatchesByStatus counts batches currently in the given BATCH status
	// (e.g. "pending") — the eligible set for a bulk start.
	CountBatchesByStatus(ctx context.Context, status string) (int64, error)
	// ListBatchIDsByStatus returns up to limit batch IDs in the given batch status
	// whose id sorts after afterID, ordered by id (keyset pagination on the PK,
	// id-only projection). Pass "" for afterID to start from the beginning.
	ListBatchIDsByStatus(ctx context.Context, status, afterID string, limit int) ([]string, error)
}

// WebhookRepository defines persistence operations for outbound webhooks.
type WebhookRepository interface {
	Create(ctx context.Context, hook *models.Webhook) error
	GetByID(ctx context.Context, id string) (*models.Webhook, error)
	ListByUser(ctx context.Context, userID *uint) ([]*models.Webhook, error)
}

// -----------------------------------------------------------------------
// batchRepo
// -----------------------------------------------------------------------

type batchRepo struct {
	db  *gorm.DB
	log *zap.Logger
}

// NewBatchRepository constructs a BatchRepository backed by GORM.
func NewBatchRepository(db *DB) BatchRepository {
	return &batchRepo{db: db.DB, log: db.log}
}

func (r *batchRepo) Create(ctx context.Context, batch *models.Batch) error {
	ctx, span := tracer.Start(ctx, "db.batch.Create",
		trace.WithAttributes(attribute.String("batch.id", batch.ID)))
	defer span.End()

	if err := r.db.WithContext(ctx).Create(batch).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("batchRepo.Create: %w", err)
	}
	return nil
}

func (r *batchRepo) CreateWithSubmissions(ctx context.Context, batch *models.Batch, subs []*models.Submission) error {
	ctx, span := tracer.Start(ctx, "db.batch.CreateWithSubmissions",
		trace.WithAttributes(
			attribute.String("batch.id", batch.ID),
			attribute.Int("submissions", len(subs)),
		))
	defer span.End()

	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(batch).Error; err != nil {
			return fmt.Errorf("create batch: %w", err)
		}
		if len(subs) > 0 {
			if err := tx.CreateInBatches(subs, 100).Error; err != nil {
				return fmt.Errorf("create submissions: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("batchRepo.CreateWithSubmissions: %w", err)
	}
	return nil
}

// MarkProcessing flips pending → processing exactly once.
func (r *batchRepo) MarkProcessing(ctx context.Context, id string) (bool, error) {
	ctx, span := tracer.Start(ctx, "db.batch.MarkProcessing",
		trace.WithAttributes(attribute.String("batch.id", id)))
	defer span.End()

	res := r.db.WithContext(ctx).
		Model(&models.Batch{}).
		Where("id = ? AND status = ?", id, models.BatchStatusPending).
		Update("status", models.BatchStatusProcessing)
	if res.Error != nil {
		span.RecordError(res.Error)
		span.SetStatus(codes.Error, res.Error.Error())
		return false, fmt.Errorf("batchRepo.MarkProcessing: %w", res.Error)
	}
	return res.RowsAffected == 1, nil
}

// MarkPending rolls processing → pending so a failed fan-out can be retried.
func (r *batchRepo) MarkPending(ctx context.Context, id string) error {
	ctx, span := tracer.Start(ctx, "db.batch.MarkPending",
		trace.WithAttributes(attribute.String("batch.id", id)))
	defer span.End()

	res := r.db.WithContext(ctx).
		Model(&models.Batch{}).
		Where("id = ? AND status = ?", id, models.BatchStatusProcessing).
		Update("status", models.BatchStatusPending)
	if res.Error != nil {
		span.RecordError(res.Error)
		span.SetStatus(codes.Error, res.Error.Error())
		return fmt.Errorf("batchRepo.MarkPending: %w", res.Error)
	}
	return nil
}

func (r *batchRepo) GetByID(ctx context.Context, id string) (*models.Batch, error) {
	ctx, span := tracer.Start(ctx, "db.batch.GetByID",
		trace.WithAttributes(attribute.String("batch.id", id)))
	defer span.End()

	var batch models.Batch
	err := r.db.WithContext(ctx).
		Preload("Webhook").
		Where("id = ?", id).
		First(&batch).Error
	if err != nil {
		span.RecordError(err)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			span.SetStatus(codes.Error, "not found")
			return nil, fmt.Errorf("batchRepo.GetByID %s: %w", id, ErrNotFound)
		}
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("batchRepo.GetByID: %w", err)
	}
	return &batch, nil
}

func (r *batchRepo) ListSubmissions(ctx context.Context, batchID string) ([]*models.Submission, error) {
	ctx, span := tracer.Start(ctx, "db.batch.ListSubmissions",
		trace.WithAttributes(attribute.String("batch.id", batchID)))
	defer span.End()

	var subs []*models.Submission
	if err := r.db.WithContext(ctx).
		Where("batch_id = ?", batchID).
		Order("created_at ASC").
		Find(&subs).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("batchRepo.ListSubmissions: %w", err)
	}
	span.SetAttributes(attribute.Int("count", len(subs)))
	return subs, nil
}

func (r *batchRepo) IncrementCompleted(ctx context.Context, id string) (*models.Batch, error) {
	ctx, span := tracer.Start(ctx, "db.batch.IncrementCompleted",
		trace.WithAttributes(attribute.String("batch.id", id)))
	defer span.End()

	var batch models.Batch
	// Atomic increment plus status transition in one UPDATE, then re-read the
	// row so the caller sees the authoritative completed/total values.
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&models.Batch{}).
			Where("id = ?", id).
			Updates(map[string]interface{}{
				"completed": gorm.Expr("completed + 1"),
				"status": gorm.Expr(
					"CASE WHEN completed + 1 >= total THEN ? ELSE ? END",
					models.BatchStatusCompleted, models.BatchStatusProcessing),
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("%w", ErrNotFound)
		}
		return tx.Where("id = ?", id).First(&batch).Error
	})
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("batchRepo.IncrementCompleted: %w", err)
	}
	span.SetAttributes(
		attribute.Int("completed", batch.Completed),
		attribute.Int("total", batch.Total),
	)
	return &batch, nil
}

func (r *batchRepo) MarkWebhook(ctx context.Context, id, status string, sentAt *time.Time, retries int) error {
	ctx, span := tracer.Start(ctx, "db.batch.MarkWebhook",
		trace.WithAttributes(
			attribute.String("batch.id", id),
			attribute.String("webhook.status", status),
			attribute.Int("webhook.retries", retries),
		))
	defer span.End()

	updates := map[string]interface{}{
		"webhook_status":      status,
		"webhook_retry_count": retries,
	}
	if sentAt != nil {
		updates["webhook_sent_at"] = sentAt
	}

	res := r.db.WithContext(ctx).
		Model(&models.Batch{}).
		Where("id = ?", id).
		Updates(updates)
	if res.Error != nil {
		span.RecordError(res.Error)
		span.SetStatus(codes.Error, res.Error.Error())
		return fmt.Errorf("batchRepo.MarkWebhook: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("batchRepo.MarkWebhook %s: %w", id, ErrNotFound)
	}
	return nil
}

// ListReconcilableBatches returns completed batches whose webhook is still
// undelivered (pending/failed), old enough to have missed the immediate dispatch
// (updated_at <= now-grace) but not older than maxAge — the durable retry set
// for the queue-manager reconciler.
func (r *batchRepo) ListReconcilableBatches(ctx context.Context, grace, maxAge time.Duration, limit int) ([]*models.Batch, error) {
	ctx, span := tracer.Start(ctx, "db.batch.ListReconcilable")
	defer span.End()

	if limit <= 0 {
		limit = 25
	}
	now := time.Now().UTC()
	var batches []*models.Batch
	err := r.db.WithContext(ctx).
		Where("status = ? AND webhook_status IN ? AND webhook_id IS NOT NULL AND webhook_id <> '' AND updated_at <= ? AND created_at >= ?",
			models.BatchStatusCompleted,
			[]string{models.WebhookStatusPending, models.WebhookStatusFailed},
			now.Add(-grace), now.Add(-maxAge)).
		Order("updated_at").Limit(limit).Find(&batches).Error
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("batchRepo.ListReconcilableBatches: %w", err)
	}
	return batches, nil
}

// eligibleWebhookScope is the shared WHERE for bulk webhook re-delivery: only
// COMPLETED batches (there is a real result to send) that have a linked webhook
// (a call is meaningless otherwise) in the requested webhook_status.
const eligibleWebhookScope = "webhook_status = ? AND status = ? AND webhook_id IS NOT NULL AND webhook_id <> ''"

func (r *batchRepo) CountBatchesByWebhookStatus(ctx context.Context, webhookStatus string) (int64, error) {
	ctx, span := tracer.Start(ctx, "db.batch.CountByWebhookStatus",
		trace.WithAttributes(attribute.String("webhook.status", webhookStatus)))
	defer span.End()

	var n int64
	err := r.db.WithContext(ctx).Model(&models.Batch{}).
		Where(eligibleWebhookScope, webhookStatus, models.BatchStatusCompleted).
		Count(&n).Error
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return 0, fmt.Errorf("batchRepo.CountBatchesByWebhookStatus: %w", err)
	}
	span.SetAttributes(attribute.Int64("count", n))
	return n, nil
}

func (r *batchRepo) ListBatchIDsByWebhookStatus(ctx context.Context, webhookStatus, afterID string, limit int) ([]string, error) {
	ctx, span := tracer.Start(ctx, "db.batch.ListIDsByWebhookStatus",
		trace.WithAttributes(
			attribute.String("webhook.status", webhookStatus),
			attribute.Int("limit", limit),
		))
	defer span.End()

	if limit <= 0 {
		limit = 200
	}
	var ids []string
	// Project id ONLY (Pluck) + keyset on the PK (id > afterID, ORDER BY id) so
	// each page is a cheap indexed range scan, independent of total match count.
	err := r.db.WithContext(ctx).Model(&models.Batch{}).
		Where(eligibleWebhookScope+" AND id > ?", webhookStatus, models.BatchStatusCompleted, afterID).
		Order("id").Limit(limit).Pluck("id", &ids).Error
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("batchRepo.ListBatchIDsByWebhookStatus: %w", err)
	}
	return ids, nil
}

func (r *batchRepo) CountBatchesByStatus(ctx context.Context, status string) (int64, error) {
	ctx, span := tracer.Start(ctx, "db.batch.CountByStatus",
		trace.WithAttributes(attribute.String("batch.status", status)))
	defer span.End()

	var n int64
	err := r.db.WithContext(ctx).Model(&models.Batch{}).
		Where("status = ?", status).Count(&n).Error
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return 0, fmt.Errorf("batchRepo.CountBatchesByStatus: %w", err)
	}
	span.SetAttributes(attribute.Int64("count", n))
	return n, nil
}

func (r *batchRepo) ListBatchIDsByStatus(ctx context.Context, status, afterID string, limit int) ([]string, error) {
	ctx, span := tracer.Start(ctx, "db.batch.ListIDsByStatus",
		trace.WithAttributes(
			attribute.String("batch.status", status),
			attribute.Int("limit", limit),
		))
	defer span.End()

	if limit <= 0 {
		limit = 200
	}
	var ids []string
	// id-only projection + keyset on the PK (id > afterID, ORDER BY id): each page
	// is a cheap indexed range scan regardless of how many batches match.
	err := r.db.WithContext(ctx).Model(&models.Batch{}).
		Where("status = ? AND id > ?", status, afterID).
		Order("id").Limit(limit).Pluck("id", &ids).Error
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("batchRepo.ListBatchIDsByStatus: %w", err)
	}
	return ids, nil
}

// -----------------------------------------------------------------------
// webhookRepo
// -----------------------------------------------------------------------

type webhookRepo struct {
	db  *gorm.DB
	log *zap.Logger
}

// NewWebhookRepository constructs a WebhookRepository backed by GORM.
func NewWebhookRepository(db *DB) WebhookRepository {
	return &webhookRepo{db: db.DB, log: db.log}
}

func (r *webhookRepo) Create(ctx context.Context, hook *models.Webhook) error {
	ctx, span := tracer.Start(ctx, "db.webhook.Create",
		trace.WithAttributes(attribute.String("webhook.id", hook.ID)))
	defer span.End()

	if err := r.db.WithContext(ctx).Create(hook).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("webhookRepo.Create: %w", err)
	}
	return nil
}

func (r *webhookRepo) GetByID(ctx context.Context, id string) (*models.Webhook, error) {
	ctx, span := tracer.Start(ctx, "db.webhook.GetByID",
		trace.WithAttributes(attribute.String("webhook.id", id)))
	defer span.End()

	var hook models.Webhook
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&hook).Error
	if err != nil {
		span.RecordError(err)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			span.SetStatus(codes.Error, "not found")
			return nil, fmt.Errorf("webhookRepo.GetByID %s: %w", id, ErrNotFound)
		}
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("webhookRepo.GetByID: %w", err)
	}
	return &hook, nil
}

func (r *webhookRepo) ListByUser(ctx context.Context, userID *uint) ([]*models.Webhook, error) {
	ctx, span := tracer.Start(ctx, "db.webhook.ListByUser")
	defer span.End()

	q := r.db.WithContext(ctx).Model(&models.Webhook{}).Order("created_at DESC")
	if userID != nil {
		q = q.Where("user_id = ?", *userID)
	}

	var hooks []*models.Webhook
	if err := q.Find(&hooks).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("webhookRepo.ListByUser: %w", err)
	}
	span.SetAttributes(attribute.Int("count", len(hooks)))
	return hooks, nil
}
