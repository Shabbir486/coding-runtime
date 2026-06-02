package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/mdshabbir-ali/code-runtime/internal/models"
)

var tracer = otel.Tracer("code-runtime/database")

// ErrNotFound is returned when a requested record does not exist.
var ErrNotFound = errors.New("record not found")

// errInvalidTokenFmt is the span status message used when a UUID cannot be parsed.
const errInvalidTokenFmt = "invalid token format"

// -----------------------------------------------------------------------
// Interfaces
// -----------------------------------------------------------------------

// SubmissionRepository defines persistence operations for code submissions.
type SubmissionRepository interface {
	Create(ctx context.Context, sub *models.Submission) error
	GetByToken(ctx context.Context, token string) (*models.Submission, error)
	UpdateStatus(ctx context.Context, token string, statusID int) error
	UpdateResult(ctx context.Context, token string, result *models.ExecutionResult) error
	List(ctx context.Context, page, limit int) ([]*models.Submission, int64, error)
	Delete(ctx context.Context, token string) error
}

// LanguageRepository defines persistence operations for runtime languages.
type LanguageRepository interface {
	GetAll(ctx context.Context) ([]*models.Language, error)
	GetByID(ctx context.Context, id int) (*models.Language, error)
	GetActive(ctx context.Context) ([]*models.Language, error)
}

// ExecutionLogRepository defines persistence operations for worker execution logs.
type ExecutionLogRepository interface {
	Create(ctx context.Context, log *models.ExecutionLog) error
	GetByToken(ctx context.Context, token string) ([]*models.ExecutionLog, error)
}

// -----------------------------------------------------------------------
// submissionRepo
// -----------------------------------------------------------------------

type submissionRepo struct {
	db  *gorm.DB
	log *zap.Logger
}

// NewSubmissionRepository constructs a SubmissionRepository backed by GORM.
func NewSubmissionRepository(db *DB) SubmissionRepository {
	return &submissionRepo{db: db.DB, log: db.log}
}

func (r *submissionRepo) Create(ctx context.Context, sub *models.Submission) error {
	ctx, span := tracer.Start(ctx, "db.submission.Create",
		trace.WithAttributes(attribute.String("submission.token", sub.Token)))
	defer span.End()

	if err := r.db.WithContext(ctx).Create(sub).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("submissionRepo.Create: %w", err)
	}
	return nil
}

func (r *submissionRepo) GetByToken(ctx context.Context, token string) (*models.Submission, error) {
	ctx, span := tracer.Start(ctx, "db.submission.GetByToken",
		trace.WithAttributes(attribute.String("submission.token", token)))
	defer span.End()

	uid, err := uuid.Parse(token)
	if err != nil {
		span.SetStatus(codes.Error, errInvalidTokenFmt)
		return nil, fmt.Errorf("submissionRepo.GetByToken: invalid UUID %q: %w", token, err)
	}

	var sub models.Submission
	err = r.db.WithContext(ctx).
		Preload("Language").
		Where("token = ?", uid.String()).
		First(&sub).Error

	if err != nil {
		span.RecordError(err)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			span.SetStatus(codes.Error, "not found")
			return nil, fmt.Errorf("submissionRepo.GetByToken %s: %w", token, ErrNotFound)
		}
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("submissionRepo.GetByToken: %w", err)
	}
	return &sub, nil
}

func (r *submissionRepo) UpdateStatus(ctx context.Context, token string, statusID int) error {
	ctx, span := tracer.Start(ctx, "db.submission.UpdateStatus",
		trace.WithAttributes(
			attribute.String("submission.token", token),
			attribute.Int("status_id", statusID),
		))
	defer span.End()

	uid, err := uuid.Parse(token)
	if err != nil {
		span.SetStatus(codes.Error, errInvalidTokenFmt)
		return fmt.Errorf("submissionRepo.UpdateStatus: invalid UUID %q: %w", token, err)
	}

	res := r.db.WithContext(ctx).
		Model(&models.Submission{}).
		Where("token = ?", uid.String()).
		Update("status_id", statusID)

	if res.Error != nil {
		span.RecordError(res.Error)
		span.SetStatus(codes.Error, res.Error.Error())
		return fmt.Errorf("submissionRepo.UpdateStatus: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		span.SetStatus(codes.Error, "not found")
		return fmt.Errorf("submissionRepo.UpdateStatus %s: %w", token, ErrNotFound)
	}
	return nil
}

// UpdateResult maps models.ExecutionResult (from execution.go) to submission columns.
// Note: models.ExecutionResult uses non-pointer string/numeric fields.
func (r *submissionRepo) UpdateResult(ctx context.Context, token string, result *models.ExecutionResult) error {
	ctx, span := tracer.Start(ctx, "db.submission.UpdateResult",
		trace.WithAttributes(attribute.String("submission.token", token)))
	defer span.End()

	uid, err := uuid.Parse(token)
	if err != nil {
		span.SetStatus(codes.Error, errInvalidTokenFmt)
		return fmt.Errorf("submissionRepo.UpdateResult: invalid UUID %q: %w", token, err)
	}

	now := time.Now().UTC()

	// Build nullable pointer versions of plain-value fields.
	var stdout, stderr, compileOutput, message, exitSignal *string
	var wallTime, cpuTime *float64
	var memory *int64
	var exitCode *int

	if result.Stdout != "" {
		stdout = &result.Stdout
	}
	if result.Stderr != "" {
		stderr = &result.Stderr
	}
	if result.CompileOutput != "" {
		compileOutput = &result.CompileOutput
	}
	if result.Error != "" {
		message = &result.Error
	}
	if result.ExitSignal != "" {
		exitSignal = &result.ExitSignal
	}
	wallTime = &result.WallTime
	cpuTime = &result.CPUTime
	mem := int64(result.MemoryUsed)
	memory = &mem
	exitCode = &result.ExitCode

	updates := map[string]interface{}{
		"status_id":      result.Status,
		"stdout":         stdout,
		"stderr":         stderr,
		"compile_output": compileOutput,
		"message":        message,
		"exit_code":      exitCode,
		"exit_signal":    exitSignal,
		"wall_time":      wallTime,
		"time":           cpuTime,
		"memory":         memory,
		"finished_at":    &now,
	}

	res := r.db.WithContext(ctx).
		Model(&models.Submission{}).
		Where("token = ?", uid.String()).
		Updates(updates)

	if res.Error != nil {
		span.RecordError(res.Error)
		span.SetStatus(codes.Error, res.Error.Error())
		return fmt.Errorf("submissionRepo.UpdateResult: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		span.SetStatus(codes.Error, "not found")
		return fmt.Errorf("submissionRepo.UpdateResult %s: %w", token, ErrNotFound)
	}
	return nil
}

func (r *submissionRepo) List(ctx context.Context, page, limit int) ([]*models.Submission, int64, error) {
	ctx, span := tracer.Start(ctx, "db.submission.List",
		trace.WithAttributes(
			attribute.Int("page", page),
			attribute.Int("limit", limit),
		))
	defer span.End()

	if page < 1 {
		page = 1
	}
	if limit < 1 || limit > 500 {
		limit = 20
	}
	offset := (page - 1) * limit

	var (
		subs  []*models.Submission
		total int64
	)

	baseQ := r.db.WithContext(ctx).Model(&models.Submission{})

	if err := baseQ.Count(&total).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, 0, fmt.Errorf("submissionRepo.List count: %w", err)
	}

	if err := r.db.WithContext(ctx).
		Preload("Language").
		Preload("Status").
		Order("created_at DESC").
		Offset(offset).
		Limit(limit).
		Find(&subs).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, 0, fmt.Errorf("submissionRepo.List query: %w", err)
	}

	span.SetAttributes(
		attribute.Int64("total", total),
		attribute.Int("returned", len(subs)),
	)
	return subs, total, nil
}

func (r *submissionRepo) Delete(ctx context.Context, token string) error {
	ctx, span := tracer.Start(ctx, "db.submission.Delete",
		trace.WithAttributes(attribute.String("submission.token", token)))
	defer span.End()

	uid, err := uuid.Parse(token)
	if err != nil {
		span.SetStatus(codes.Error, errInvalidTokenFmt)
		return fmt.Errorf("submissionRepo.Delete: invalid UUID %q: %w", token, err)
	}

	res := r.db.WithContext(ctx).
		Where("token = ?", uid.String()).
		Delete(&models.Submission{})

	if res.Error != nil {
		span.RecordError(res.Error)
		span.SetStatus(codes.Error, res.Error.Error())
		return fmt.Errorf("submissionRepo.Delete: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		span.SetStatus(codes.Error, "not found")
		return fmt.Errorf("submissionRepo.Delete %s: %w", token, ErrNotFound)
	}
	return nil
}

// -----------------------------------------------------------------------
// languageRepo
// -----------------------------------------------------------------------

type languageRepo struct {
	db  *gorm.DB
	log *zap.Logger
}

// NewLanguageRepository constructs a LanguageRepository backed by GORM.
func NewLanguageRepository(db *DB) LanguageRepository {
	return &languageRepo{db: db.DB, log: db.log}
}

func (r *languageRepo) GetAll(ctx context.Context) ([]*models.Language, error) {
	ctx, span := tracer.Start(ctx, "db.language.GetAll")
	defer span.End()

	var langs []*models.Language
	if err := r.db.WithContext(ctx).Order("id ASC").Find(&langs).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("languageRepo.GetAll: %w", err)
	}
	span.SetAttributes(attribute.Int("count", len(langs)))
	return langs, nil
}

func (r *languageRepo) GetByID(ctx context.Context, id int) (*models.Language, error) {
	ctx, span := tracer.Start(ctx, "db.language.GetByID",
		trace.WithAttributes(attribute.Int("language.id", id)))
	defer span.End()

	var lang models.Language
	if err := r.db.WithContext(ctx).First(&lang, id).Error; err != nil {
		span.RecordError(err)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			span.SetStatus(codes.Error, "not found")
			return nil, fmt.Errorf("languageRepo.GetByID %d: %w", id, ErrNotFound)
		}
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("languageRepo.GetByID: %w", err)
	}
	return &lang, nil
}

func (r *languageRepo) GetActive(ctx context.Context) ([]*models.Language, error) {
	ctx, span := tracer.Start(ctx, "db.language.GetActive")
	defer span.End()

	var langs []*models.Language
	if err := r.db.WithContext(ctx).
		Where("is_active = true").
		Order("id ASC").
		Find(&langs).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("languageRepo.GetActive: %w", err)
	}
	span.SetAttributes(attribute.Int("count", len(langs)))
	return langs, nil
}

// -----------------------------------------------------------------------
// executionLogRepo
// -----------------------------------------------------------------------

type executionLogRepo struct {
	db  *gorm.DB
	log *zap.Logger
}

// NewExecutionLogRepository constructs an ExecutionLogRepository backed by GORM.
func NewExecutionLogRepository(db *DB) ExecutionLogRepository {
	return &executionLogRepo{db: db.DB, log: db.log}
}

func (r *executionLogRepo) Create(ctx context.Context, entry *models.ExecutionLog) error {
	ctx, span := tracer.Start(ctx, "db.execlog.Create",
		trace.WithAttributes(attribute.String("submission.token", entry.Token)))
	defer span.End()

	if err := r.db.WithContext(ctx).Create(entry).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("executionLogRepo.Create: %w", err)
	}
	return nil
}

func (r *executionLogRepo) GetByToken(ctx context.Context, token string) ([]*models.ExecutionLog, error) {
	ctx, span := tracer.Start(ctx, "db.execlog.GetByToken",
		trace.WithAttributes(attribute.String("submission.token", token)))
	defer span.End()

	var logs []*models.ExecutionLog
	if err := r.db.WithContext(ctx).
		Where("token = ?", token).
		Order("created_at ASC").
		Find(&logs).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("executionLogRepo.GetByToken: %w", err)
	}
	span.SetAttributes(attribute.Int("count", len(logs)))
	return logs, nil
}

// -----------------------------------------------------------------------
// userRepo
// -----------------------------------------------------------------------

// UserRepository defines persistence operations for users and API keys.
type UserRepository interface {
	GetByEmail(ctx context.Context, email string) (*models.User, error)
	Create(ctx context.Context, user *models.User) error
	CreateAPIKey(ctx context.Context, key *models.APIKey) error
	GetAPIKeyByHash(ctx context.Context, hash string) (*models.APIKey, error)
	UpdateAPIKeyLastUsed(ctx context.Context, id uint, t time.Time) error
}

type userRepo struct {
	db  *gorm.DB
	log *zap.Logger
}

// NewUserRepository constructs a UserRepository backed by GORM.
func NewUserRepository(db *DB) UserRepository {
	return &userRepo{db: db.DB, log: db.log}
}

func (r *userRepo) GetByEmail(ctx context.Context, email string) (*models.User, error) {
	ctx, span := tracer.Start(ctx, "db.user.GetByEmail")
	defer span.End()

	var u models.User
	if err := r.db.WithContext(ctx).Where("email = ?", email).First(&u).Error; err != nil {
		span.RecordError(err)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("userRepo.GetByEmail %s: %w", email, ErrNotFound)
		}
		return nil, fmt.Errorf("userRepo.GetByEmail: %w", err)
	}
	return &u, nil
}

func (r *userRepo) Create(ctx context.Context, user *models.User) error {
	ctx, span := tracer.Start(ctx, "db.user.Create")
	defer span.End()

	if err := r.db.WithContext(ctx).Create(user).Error; err != nil {
		span.RecordError(err)
		return fmt.Errorf("userRepo.Create: %w", err)
	}
	return nil
}

func (r *userRepo) CreateAPIKey(ctx context.Context, key *models.APIKey) error {
	ctx, span := tracer.Start(ctx, "db.apikey.Create")
	defer span.End()

	if err := r.db.WithContext(ctx).Create(key).Error; err != nil {
		span.RecordError(err)
		return fmt.Errorf("userRepo.CreateAPIKey: %w", err)
	}
	return nil
}

func (r *userRepo) GetAPIKeyByHash(ctx context.Context, hash string) (*models.APIKey, error) {
	ctx, span := tracer.Start(ctx, "db.apikey.GetByHash")
	defer span.End()

	var key models.APIKey
	if err := r.db.WithContext(ctx).
		Preload("User").
		Where("key_hash = ? AND is_active = true", hash).
		First(&key).Error; err != nil {
		span.RecordError(err)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("userRepo.GetAPIKeyByHash: %w", ErrNotFound)
		}
		return nil, fmt.Errorf("userRepo.GetAPIKeyByHash: %w", err)
	}
	return &key, nil
}

func (r *userRepo) UpdateAPIKeyLastUsed(ctx context.Context, id uint, t time.Time) error {
	ctx, span := tracer.Start(ctx, "db.apikey.UpdateLastUsed")
	defer span.End()

	if err := r.db.WithContext(ctx).
		Model(&models.APIKey{}).
		Where("id = ?", id).
		Update("last_used_at", t).Error; err != nil {
		span.RecordError(err)
		return fmt.Errorf("userRepo.UpdateAPIKeyLastUsed: %w", err)
	}
	return nil
}

// -----------------------------------------------------------------------
// Repositories aggregate
// -----------------------------------------------------------------------

// Repositories bundles all repository instances together for dependency injection.
type Repositories struct {
	Submissions   SubmissionRepository
	Languages     LanguageRepository
	ExecutionLogs ExecutionLogRepository
	Users         UserRepository
}

// NewRepositories constructs all repositories from a single *DB.
func NewRepositories(db *DB) *Repositories {
	return &Repositories{
		Submissions:   NewSubmissionRepository(db),
		Languages:     NewLanguageRepository(db),
		ExecutionLogs: NewExecutionLogRepository(db),
		Users:         NewUserRepository(db),
	}
}
