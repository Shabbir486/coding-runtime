package models

import "time"

// ---------------------------------------------------------------------------
// Batch lifecycle states
// ---------------------------------------------------------------------------

// Batch status values describe the aggregate progress of all submissions in a
// batch. They are stored as short strings rather than integers because batches
// have their own lifecycle, distinct from the per-submission status table.
const (
	BatchStatusPending    = "pending"    // created, submissions still running
	BatchStatusProcessing = "processing" // at least one submission has started
	BatchStatusCompleted  = "completed"  // every submission reached a terminal state
)

// Webhook delivery states track the outbound call made when a batch completes.
const (
	WebhookStatusPending = "pending" // not yet attempted
	WebhookStatusSent    = "sent"    // delivered with a 2xx response
	WebhookStatusFailed  = "failed"  // delivery attempted but failed
)

// ---------------------------------------------------------------------------
// Webhook — outbound notification endpoint registered by a user
// ---------------------------------------------------------------------------

// Webhook is a user-registered HTTP endpoint that receives the batch result
// payload once all submissions in a linked batch finish. APIKey is the value
// sent in the outbound X-API-Key header so the receiver can authenticate the
// call as originating from this platform.
type Webhook struct {
	ID        string    `gorm:"type:varchar(36);primaryKey"            json:"id"`
	UserID    *uint     `gorm:"index"                                  json:"user_id"`
	Name      string    `gorm:"type:varchar(255);not null"             json:"name"`
	URL       string    `gorm:"type:varchar(2048);not null"            json:"url"`
	APIKey    string    `gorm:"type:varchar(255);not null"             json:"-"`
	IsActive  bool      `gorm:"default:true"                           json:"is_active"`
	CreatedAt time.Time `gorm:"autoCreateTime"                         json:"created_at"`
	UpdatedAt time.Time `gorm:"autoUpdateTime"                         json:"updated_at"`
}

// TableName overrides the default GORM table name.
func (Webhook) TableName() string { return "webhooks" }

// ---------------------------------------------------------------------------
// Batch — groups multiple submissions under a single retrievable identifier
// ---------------------------------------------------------------------------

// Batch maps a single batch_id to the set of submissions created together.
// Total/Completed track aggregate progress so a worker can detect when the
// final submission finishes and fire the linked webhook exactly once.
type Batch struct {
	ID            string  `gorm:"type:varchar(36);primaryKey"        json:"batch_id"`
	UserID        *uint   `gorm:"index"                              json:"user_id"`
	WebhookID     *string `gorm:"type:varchar(36);index"             json:"webhook_id"`
	Status        string  `gorm:"type:varchar(20);not null;default:'pending'" json:"status"`
	Total         int     `gorm:"not null"                           json:"total"`
	Completed     int     `gorm:"not null;default:0"                 json:"completed"`
	WebhookStatus string  `gorm:"type:varchar(20);not null;default:'pending'" json:"webhook_status"`
	// WebhookRetryCount records how many retry attempts (beyond the first
	// delivery) were made before the webhook reached its terminal state. 0 means
	// it succeeded on the first attempt (or was never attempted).
	WebhookRetryCount int        `gorm:"not null;default:0"                 json:"webhook_retry_count"`
	WebhookSentAt     *time.Time `                                          json:"webhook_sent_at"`
	CreatedAt         time.Time  `gorm:"autoCreateTime;index"               json:"created_at"`
	UpdatedAt         time.Time  `gorm:"autoUpdateTime"                     json:"updated_at"`

	// Webhook is loaded via Preload when present.
	Webhook *Webhook `gorm:"foreignKey:WebhookID" json:"webhook,omitempty"`
}

// TableName overrides the default GORM table name.
func (Batch) TableName() string { return "batches" }

// IsComplete reports whether every submission in the batch has finished.
func (b *Batch) IsComplete() bool { return b.Total > 0 && b.Completed >= b.Total }

// ---------------------------------------------------------------------------
// DTOs — batch
// ---------------------------------------------------------------------------

// CreateBatchRequest is the HTTP body for POST /batches. It reuses the standard
// SubmissionRequest shape per item and optionally links a webhook fired on
// completion.
type CreateBatchRequest struct {
	// The upper bound is enforced at the handler from config (batch.max_size /
	// BATCH_MAX_SIZE), not as a fixed binding tag, so it can be tuned per env.
	Submissions []SubmissionRequest `json:"submissions" binding:"required,min=1,dive"`
	WebhookID   string              `json:"webhook_id"`
}

// CreateBatchResponse is returned by POST /batches.
type CreateBatchResponse struct {
	BatchID string   `json:"batch_id"`
	Tokens  []string `json:"tokens"`
	Total   int      `json:"total"`
	Status  string   `json:"status"`
}

// BatchResponse is the full batch detail returned by GET /batches/:id and used
// as the body delivered to a registered webhook.
type BatchResponse struct {
	BatchID           string               `json:"batch_id"`
	Status            string               `json:"status"`
	Total             int                  `json:"total"`
	Completed         int                  `json:"completed"`
	WebhookID         *string              `json:"webhook_id,omitempty"`
	WebhookStatus     string               `json:"webhook_status"`
	WebhookRetryCount int                  `json:"webhook_retry_count"`
	WebhookSentAt     *time.Time           `json:"webhook_sent_at,omitempty"`
	CreatedAt         time.Time            `json:"created_at"`
	Submissions       []SubmissionResponse `json:"submissions"`
}

// BatchToResponse assembles a BatchResponse from a Batch and its submissions.
func BatchToResponse(b *Batch, subs []*Submission) BatchResponse {
	responses := make([]SubmissionResponse, len(subs))
	for i, s := range subs {
		responses[i] = SubmissionToResponse(s, false)
	}
	resp := BatchResponse{
		BatchID:           b.ID,
		Status:            b.Status,
		Total:             b.Total,
		Completed:         b.Completed,
		WebhookID:         b.WebhookID,
		WebhookStatus:     b.WebhookStatus,
		WebhookRetryCount: b.WebhookRetryCount,
		WebhookSentAt:     b.WebhookSentAt,
		CreatedAt:         b.CreatedAt,
		Submissions:       responses,
	}
	return resp
}

// ---------------------------------------------------------------------------
// DTOs — webhook
// ---------------------------------------------------------------------------

// RegisterWebhookRequest is the HTTP body for POST /webhooks. APIKey is
// optional; when omitted the server generates one and returns it once.
type RegisterWebhookRequest struct {
	Name   string `json:"name"    binding:"required,min=1,max=255"`
	URL    string `json:"url"     binding:"required,url"`
	APIKey string `json:"api_key"`
}

// WebhookResponse is returned when a webhook is registered or fetched. The
// APIKey is included only on the registration response (shown once).
type WebhookResponse struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	URL       string    `json:"url"`
	APIKey    string    `json:"api_key,omitempty"`
	IsActive  bool      `json:"is_active"`
	CreatedAt time.Time `json:"created_at"`
}

// WebhookToResponse converts a Webhook model to its API response shape.
// includeKey controls whether the secret API key is exposed (only on create).
func WebhookToResponse(w *Webhook, includeKey bool) WebhookResponse {
	resp := WebhookResponse{
		ID:        w.ID,
		Name:      w.Name,
		URL:       w.URL,
		IsActive:  w.IsActive,
		CreatedAt: w.CreatedAt,
	}
	if includeKey {
		resp.APIKey = w.APIKey
	}
	return resp
}
