package unit_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/revature/corems-code-executor/internal/models"
	"github.com/revature/corems-code-executor/internal/webhook"
)

// ---------------------------------------------------------------------------
// Mocks: BatchRepository / WebhookRepository
// ---------------------------------------------------------------------------

type mockBatchRepo struct{ mock.Mock }

func (m *mockBatchRepo) Create(ctx context.Context, b *models.Batch) error {
	return m.Called(ctx, b).Error(0)
}

func (m *mockBatchRepo) GetByID(ctx context.Context, id string) (*models.Batch, error) {
	args := m.Called(ctx, id)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*models.Batch), args.Error(1)
}

func (m *mockBatchRepo) ListSubmissions(ctx context.Context, batchID string) ([]*models.Submission, error) {
	args := m.Called(ctx, batchID)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]*models.Submission), args.Error(1)
}

func (m *mockBatchRepo) IncrementCompleted(ctx context.Context, id string) (*models.Batch, error) {
	args := m.Called(ctx, id)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*models.Batch), args.Error(1)
}

func (m *mockBatchRepo) MarkWebhook(ctx context.Context, id, status string, sentAt *time.Time, retries int) error {
	return m.Called(ctx, id, status, sentAt, retries).Error(0)
}

func (m *mockBatchRepo) CreateWithSubmissions(ctx context.Context, b *models.Batch, subs []*models.Submission) error {
	return m.Called(ctx, b, subs).Error(0)
}

func (m *mockBatchRepo) MarkProcessing(ctx context.Context, id string) (bool, error) {
	args := m.Called(ctx, id)
	return args.Bool(0), args.Error(1)
}

func (m *mockBatchRepo) MarkPending(ctx context.Context, id string) error {
	return m.Called(ctx, id).Error(0)
}

func (m *mockBatchRepo) ListReconcilableBatches(ctx context.Context, grace, maxAge time.Duration, limit int) ([]*models.Batch, error) {
	args := m.Called(ctx, grace, maxAge, limit)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]*models.Batch), args.Error(1)
}

func (m *mockBatchRepo) CountBatchesByWebhookStatus(ctx context.Context, webhookStatus string) (int64, error) {
	args := m.Called(ctx, webhookStatus)
	return args.Get(0).(int64), args.Error(1)
}

func (m *mockBatchRepo) ListBatchIDsByWebhookStatus(ctx context.Context, webhookStatus, afterID string, limit int) ([]string, error) {
	args := m.Called(ctx, webhookStatus, afterID, limit)
	var ids []string
	if v := args.Get(0); v != nil {
		ids = v.([]string)
	}
	return ids, args.Error(1)
}

func (m *mockBatchRepo) CountBatchesByStatus(ctx context.Context, status string) (int64, error) {
	args := m.Called(ctx, status)
	return args.Get(0).(int64), args.Error(1)
}

func (m *mockBatchRepo) ListBatchIDsByStatus(ctx context.Context, status, afterID string, limit int) ([]string, error) {
	args := m.Called(ctx, status, afterID, limit)
	var ids []string
	if v := args.Get(0); v != nil {
		ids = v.([]string)
	}
	return ids, args.Error(1)
}

type mockWebhookRepo struct{ mock.Mock }

func (m *mockWebhookRepo) Create(ctx context.Context, w *models.Webhook) error {
	return m.Called(ctx, w).Error(0)
}

func (m *mockWebhookRepo) GetByID(ctx context.Context, id string) (*models.Webhook, error) {
	args := m.Called(ctx, id)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*models.Webhook), args.Error(1)
}

func (m *mockWebhookRepo) ListByUser(ctx context.Context, userID *uint) ([]*models.Webhook, error) {
	args := m.Called(ctx, userID)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]*models.Webhook), args.Error(1)
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestDispatchBatch_DeliversPayloadWithAPIKey(t *testing.T) {
	// Arrange
	const apiKey = "secret-key-123"
	webhookID := "wh-1"

	var gotKey string
	var gotPayload models.BatchResponse
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-API-Key")
		_ = json.NewDecoder(r.Body).Decode(&gotPayload)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	batch := &models.Batch{ID: "batch-1", WebhookID: &webhookID, Status: models.BatchStatusCompleted, Total: 1, Completed: 1}
	hook := &models.Webhook{ID: webhookID, URL: srv.URL, APIKey: apiKey, IsActive: true}
	subs := []*models.Submission{{Token: "tok-1", LanguageID: 1, StatusID: models.StatusAccepted}}

	batchRepo := new(mockBatchRepo)
	webhookRepo := new(mockWebhookRepo)
	batchRepo.On("GetByID", mock.Anything, "batch-1").Return(batch, nil)
	webhookRepo.On("GetByID", mock.Anything, webhookID).Return(hook, nil)
	batchRepo.On("ListSubmissions", mock.Anything, "batch-1").Return(subs, nil)
	batchRepo.On("MarkWebhook", mock.Anything, "batch-1", models.WebhookStatusSent, mock.Anything, mock.Anything).Return(nil)

	d := webhook.NewDispatcher(batchRepo, webhookRepo, nil, webhook.RetryConfig{}, zap.NewNop())

	// Act
	err := d.DispatchBatch(context.Background(), "batch-1")

	// Assert
	require.NoError(t, err)
	assert.Equal(t, apiKey, gotKey)
	assert.Equal(t, "batch-1", gotPayload.BatchID)
	require.Len(t, gotPayload.Submissions, 1)
	assert.Equal(t, "tok-1", gotPayload.Submissions[0].Token)
	// Delivered on the first attempt → retry count persisted as 0.
	batchRepo.AssertCalled(t, "MarkWebhook", mock.Anything, "batch-1", models.WebhookStatusSent, mock.Anything, 0)
}

func TestDispatchBatch_NoWebhookLinked(t *testing.T) {
	// Arrange
	batch := &models.Batch{ID: "batch-2", WebhookID: nil, Total: 1, Completed: 1}
	batchRepo := new(mockBatchRepo)
	webhookRepo := new(mockWebhookRepo)
	batchRepo.On("GetByID", mock.Anything, "batch-2").Return(batch, nil)

	d := webhook.NewDispatcher(batchRepo, webhookRepo, nil, webhook.RetryConfig{}, zap.NewNop())

	// Act
	err := d.DispatchBatch(context.Background(), "batch-2")

	// Assert
	assert.ErrorIs(t, err, webhook.ErrNoWebhook)
}

func TestDispatchBatch_MarksFailedOnNon2xx(t *testing.T) {
	// Arrange
	webhookID := "wh-3"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	batch := &models.Batch{ID: "batch-3", WebhookID: &webhookID, Total: 1, Completed: 1}
	hook := &models.Webhook{ID: webhookID, URL: srv.URL, APIKey: "k", IsActive: true}

	batchRepo := new(mockBatchRepo)
	webhookRepo := new(mockWebhookRepo)
	batchRepo.On("GetByID", mock.Anything, "batch-3").Return(batch, nil)
	webhookRepo.On("GetByID", mock.Anything, webhookID).Return(hook, nil)
	batchRepo.On("ListSubmissions", mock.Anything, "batch-3").Return([]*models.Submission{}, nil)
	batchRepo.On("MarkWebhook", mock.Anything, "batch-3", models.WebhookStatusFailed, mock.Anything, mock.Anything).Return(nil)

	d := webhook.NewDispatcher(batchRepo, webhookRepo, nil, webhook.RetryConfig{}, zap.NewNop())

	// Act
	err := d.DispatchBatch(context.Background(), "batch-3")

	// Assert
	require.Error(t, err)
	// Single-attempt dispatch failed → retry count persisted as 0.
	batchRepo.AssertCalled(t, "MarkWebhook", mock.Anything, "batch-3", models.WebhookStatusFailed, mock.Anything, 0)
}

func TestDispatchBatchWithRetry_SucceedsAfterTransientFailures(t *testing.T) {
	// Arrange: fail the first 2 attempts (500), succeed on the 3rd.
	webhookID := "wh-retry"
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&attempts, 1) <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	batch := &models.Batch{ID: "batch-r", WebhookID: &webhookID, Total: 1, Completed: 1}
	hook := &models.Webhook{ID: webhookID, URL: srv.URL, APIKey: "k", IsActive: true}

	batchRepo := new(mockBatchRepo)
	webhookRepo := new(mockWebhookRepo)
	batchRepo.On("GetByID", mock.Anything, "batch-r").Return(batch, nil)
	webhookRepo.On("GetByID", mock.Anything, webhookID).Return(hook, nil)
	batchRepo.On("ListSubmissions", mock.Anything, "batch-r").Return([]*models.Submission{}, nil)
	batchRepo.On("MarkWebhook", mock.Anything, "batch-r", models.WebhookStatusSent, mock.Anything, mock.Anything).Return(nil)

	// 3 retries, tiny fixed delay so the test is fast.
	d := webhook.NewDispatcher(batchRepo, webhookRepo, nil, webhook.RetryConfig{
		MaxRetries: 3,
		Delay:      time.Millisecond,
		Backoff:    1,
	}, zap.NewNop())

	// Act
	err := d.DispatchBatchWithRetry(context.Background(), "batch-r")

	// Assert: eventually succeeded after exactly 3 attempts; marked sent (not failed).
	require.NoError(t, err)
	assert.Equal(t, int32(3), atomic.LoadInt32(&attempts))
	// Succeeded on the 3rd attempt → 2 retries persisted alongside "sent".
	batchRepo.AssertCalled(t, "MarkWebhook", mock.Anything, "batch-r", models.WebhookStatusSent, mock.Anything, 2)
	batchRepo.AssertNotCalled(t, "MarkWebhook", mock.Anything, "batch-r", models.WebhookStatusFailed, mock.Anything, mock.Anything)
}

func TestDispatchBatchWithRetry_MarksFailedAfterExhaustion(t *testing.T) {
	// Arrange: always fail. Expect 1 + MaxRetries attempts, then failed.
	webhookID := "wh-exhaust"
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	batch := &models.Batch{ID: "batch-x", WebhookID: &webhookID, Total: 1, Completed: 1}
	hook := &models.Webhook{ID: webhookID, URL: srv.URL, APIKey: "k", IsActive: true}

	batchRepo := new(mockBatchRepo)
	webhookRepo := new(mockWebhookRepo)
	batchRepo.On("GetByID", mock.Anything, "batch-x").Return(batch, nil)
	webhookRepo.On("GetByID", mock.Anything, webhookID).Return(hook, nil)
	batchRepo.On("ListSubmissions", mock.Anything, "batch-x").Return([]*models.Submission{}, nil)
	batchRepo.On("MarkWebhook", mock.Anything, "batch-x", models.WebhookStatusFailed, mock.Anything, mock.Anything).Return(nil)

	d := webhook.NewDispatcher(batchRepo, webhookRepo, nil, webhook.RetryConfig{
		MaxRetries: 2,
		Delay:      time.Millisecond,
		Backoff:    1,
	}, zap.NewNop())

	// Act
	err := d.DispatchBatchWithRetry(context.Background(), "batch-x")

	// Assert: 3 total attempts (1 + 2 retries), marked failed.
	require.Error(t, err)
	assert.Equal(t, int32(3), atomic.LoadInt32(&attempts))
	// 1 + 2 retries exhausted → 2 retries persisted alongside "failed".
	batchRepo.AssertCalled(t, "MarkWebhook", mock.Anything, "batch-x", models.WebhookStatusFailed, mock.Anything, 2)
}

func TestBatchToResponse_AssemblesSubmissions(t *testing.T) {
	// Arrange
	batch := &models.Batch{
		ID:            "batch-4",
		Status:        models.BatchStatusProcessing,
		Total:         2,
		Completed:     1,
		WebhookStatus: models.WebhookStatusPending,
	}
	subs := []*models.Submission{
		{Token: "a", LanguageID: 1, StatusID: models.StatusAccepted},
		{Token: "b", LanguageID: 2, StatusID: models.StatusInQueue},
	}

	// Act
	resp := models.BatchToResponse(batch, subs)

	// Assert
	assert.Equal(t, "batch-4", resp.BatchID)
	assert.Equal(t, 2, resp.Total)
	assert.Equal(t, 1, resp.Completed)
	require.Len(t, resp.Submissions, 2)
	assert.Equal(t, "a", resp.Submissions[0].Token)
	assert.Equal(t, "b", resp.Submissions[1].Token)
}

func TestBatch_IsComplete(t *testing.T) {
	assert.True(t, (&models.Batch{Total: 3, Completed: 3}).IsComplete())
	assert.True(t, (&models.Batch{Total: 3, Completed: 4}).IsComplete())
	assert.False(t, (&models.Batch{Total: 3, Completed: 2}).IsComplete())
	assert.False(t, (&models.Batch{Total: 0, Completed: 0}).IsComplete())
}
