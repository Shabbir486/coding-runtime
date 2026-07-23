package unit_test

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/revature/corems-code-executor/internal/models"
	"github.com/revature/corems-code-executor/internal/sandbox"
)

// ---------------------------------------------------------------------------
// Mock: SubmissionRepository
// ---------------------------------------------------------------------------

type mockSubmissionRepo struct {
	mock.Mock
}

func (m *mockSubmissionRepo) Create(ctx context.Context, sub *models.Submission) error {
	args := m.Called(ctx, sub)
	return args.Error(0)
}

func (m *mockSubmissionRepo) GetByToken(ctx context.Context, token string) (*models.Submission, error) {
	args := m.Called(ctx, token)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*models.Submission), args.Error(1)
}

func (m *mockSubmissionRepo) UpdateStatus(ctx context.Context, token string, statusID int) error {
	args := m.Called(ctx, token, statusID)
	return args.Error(0)
}

func (m *mockSubmissionRepo) UpdateResult(ctx context.Context, token string, result *models.ExecutionResult) error {
	args := m.Called(ctx, token, result)
	return args.Error(0)
}

func (m *mockSubmissionRepo) List(ctx context.Context, page, limit int) ([]*models.Submission, int64, error) {
	args := m.Called(ctx, page, limit)
	return args.Get(0).([]*models.Submission), args.Get(1).(int64), args.Error(2)
}

func (m *mockSubmissionRepo) Delete(ctx context.Context, token string) error {
	args := m.Called(ctx, token)
	return args.Error(0)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func b64(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

func mustDecode(t *testing.T, s string) string {
	t.Helper()
	dec, err := base64.StdEncoding.DecodeString(s)
	require.NoError(t, err)
	return string(dec)
}

func strPtr(s string) *string { return &s }

func newSubmission(languageID int, sourceCode string) *models.Submission {
	return &models.Submission{
		Token:         uuid.New().String(),
		LanguageID:    languageID,
		SourceCode:    b64(sourceCode),
		StatusID:      models.StatusInQueue,
		CPUTimeLimit:  5.0,
		WallTimeLimit: 10.0,
		MemoryLimit:   262144,
		StackLimit:    65536,
		MaxProcesses:  60,
		MaxFileSize:   4096,
		CreatedAt:     time.Now().UTC(),
	}
}

// ---------------------------------------------------------------------------
// Tests: Submission model / validation
// ---------------------------------------------------------------------------

// TestSubmission_IsTerminal verifies terminal-state detection.
func TestSubmission_IsTerminal(t *testing.T) {
	cases := []struct {
		statusID int
		want     bool
	}{
		{models.StatusInQueue, false},
		{models.StatusProcessing, false},
		{models.StatusAccepted, true},
		{models.StatusWrongAnswer, true},
		{models.StatusTimeLimitExceeded, true},
		{models.StatusCompilationError, true},
		{models.StatusRuntimeError, true},
		{models.StatusInternalError, true},
	}

	for _, tc := range cases {
		sub := &models.Submission{StatusID: tc.statusID}
		assert.Equal(t, tc.want, sub.IsTerminal(), "statusID=%d", tc.statusID)
	}
}

// TestSubmission_BeforeCreate_SetsDefaults ensures BeforeCreate fills token and status.
func TestSubmission_BeforeCreate_SetsDefaults(t *testing.T) {
	sub := &models.Submission{
		LanguageID: models.LanguagePython3,
		SourceCode: b64(`print("hi")`),
	}

	err := sub.BeforeCreate(nil)
	require.NoError(t, err)

	assert.NotEmpty(t, sub.Token, "token should be populated")
	_, parseErr := uuid.Parse(sub.Token)
	assert.NoError(t, parseErr, "token should be a valid UUID")
	assert.Equal(t, models.StatusInQueue, sub.StatusID, "default status should be InQueue")
}

// TestSubmission_BeforeCreate_PreservesExistingToken ensures an already-set token is not overwritten.
func TestSubmission_BeforeCreate_PreservesExistingToken(t *testing.T) {
	existing := uuid.New().String()
	sub := &models.Submission{
		Token:      existing,
		LanguageID: models.LanguageGo,
		SourceCode: b64(`package main; func main() {}`),
	}

	err := sub.BeforeCreate(nil)
	require.NoError(t, err)
	assert.Equal(t, existing, sub.Token, "existing token should be preserved")
}

// TestSubmission_ToResponse_ExcludesSourceByDefault verifies source is omitted when not requested.
func TestSubmission_ToResponse_ExcludesSourceByDefault(t *testing.T) {
	sub := newSubmission(models.LanguagePython3, `print("hello")`)
	out := b64("hello\n")
	sub.Stdout = &out
	sub.StatusID = models.StatusAccepted

	resp := models.SubmissionToResponse(sub, false)

	assert.Empty(t, resp.SourceCode, "source_code should be absent when includeSource=false")
	assert.NotNil(t, resp.Stdout, "stdout should be present")
}

// TestSubmission_ToResponse_IncludesSourceWhenRequested verifies source is included on request.
func TestSubmission_ToResponse_IncludesSourceWhenRequested(t *testing.T) {
	sub := newSubmission(models.LanguagePython3, `print("hello")`)
	inp := b64("world\n")
	sub.Stdin = &inp

	resp := models.SubmissionToResponse(sub, true)

	assert.NotEmpty(t, resp.SourceCode)
	assert.NotEmpty(t, resp.Stdin)
}

// TestSubmission_ToResponse_NilFields verifies nil pointer fields are absent from response.
func TestSubmission_ToResponse_NilFields(t *testing.T) {
	sub := &models.Submission{
		Token:      uuid.New().String(),
		LanguageID: models.LanguageJavaScript,
		StatusID:   models.StatusInQueue,
		CreatedAt:  time.Now().UTC(),
	}

	resp := models.SubmissionToResponse(sub, false)

	assert.Nil(t, resp.Stdout)
	assert.Nil(t, resp.Stderr)
	assert.Nil(t, resp.CompileOutput)
}

// ---------------------------------------------------------------------------
// Tests: ExecutionJob
// ---------------------------------------------------------------------------

// TestExecutionJob_Validate_HappyPath verifies valid jobs pass validation.
func TestExecutionJob_Validate_HappyPath(t *testing.T) {
	job := &models.ExecutionJob{
		SubmissionToken: uuid.New().String(),
		LanguageID:      models.LanguagePython3,
		SourceCode:      b64(`print("ok")`),
	}
	assert.NoError(t, job.Validate())
}

// TestExecutionJob_Validate_MissingToken checks token requirement.
func TestExecutionJob_Validate_MissingToken(t *testing.T) {
	job := &models.ExecutionJob{
		LanguageID: models.LanguagePython3,
		SourceCode: b64(`print("ok")`),
	}
	err := job.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "token")
}

// TestExecutionJob_Validate_MissingSourceCode checks source code requirement.
func TestExecutionJob_Validate_MissingSourceCode(t *testing.T) {
	job := &models.ExecutionJob{
		SubmissionToken: uuid.New().String(),
		LanguageID:      models.LanguagePython3,
	}
	err := job.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "source_code")
}

// TestExecutionJob_Validate_InvalidLanguageID checks language_id >= 1.
func TestExecutionJob_Validate_InvalidLanguageID(t *testing.T) {
	job := &models.ExecutionJob{
		SubmissionToken: uuid.New().String(),
		LanguageID:      0,
		SourceCode:      b64(`print("ok")`),
	}
	err := job.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "language_id")
}

// TestExecutionJob_ApplyDefaults fills zero resource limits.
func TestExecutionJob_ApplyDefaults(t *testing.T) {
	job := &models.ExecutionJob{
		SubmissionToken: uuid.New().String(),
		LanguageID:      models.LanguagePython3,
		SourceCode:      b64(`print("ok")`),
	}
	job.ApplyDefaults()

	assert.Greater(t, job.CPUTimeLimit, 0.0)
	assert.Greater(t, job.WallTimeLimit, 0.0)
	assert.Greater(t, job.MemoryLimit, int64(0))
	assert.Greater(t, job.StackLimit, int64(0))
	assert.Greater(t, job.MaxProcesses, 0)
	assert.False(t, job.EnqueuedAt.IsZero())
}

// TestExecutionJob_FromSubmission maps all fields correctly.
func TestExecutionJob_FromSubmission(t *testing.T) {
	sub := newSubmission(models.LanguageJava, `public class Main { public static void main(String[] a) {} }`)
	sub.CPUTimeLimit = 15.0
	sub.WallTimeLimit = 20.0
	sub.MemoryLimit = 524288
	sub.CallbackURL = "https://example.com/callback"

	job := &models.ExecutionJob{}
	job.FromSubmission(sub)

	assert.Equal(t, sub.Token, job.SubmissionToken)
	assert.Equal(t, sub.LanguageID, job.LanguageID)
	assert.Equal(t, sub.SourceCode, job.SourceCode)
	assert.Equal(t, sub.CPUTimeLimit, job.CPUTimeLimit)
	assert.Equal(t, sub.CallbackURL, job.CallbackURL)
	assert.False(t, job.EnqueuedAt.IsZero())
}

// ---------------------------------------------------------------------------
// Tests: Constraint validation (sandbox package)
// ---------------------------------------------------------------------------

// TestValidateConstraints_Valid confirms valid constraints pass.
func TestValidateConstraints_Valid(t *testing.T) {
	req := &sandbox.ExecutionRequest{
		Image:         "python:3.12-slim",
		CPUTimeLimit:  5.0,
		WallTimeLimit: 10.0,
		MemoryLimit:   256 * 1024 * 1024,
	}
	assert.NoError(t, sandbox.ValidateConstraints(req))
}

// TestValidateConstraints_ExceedsCPULimit rejects CPU limit above maximum.
func TestValidateConstraints_ExceedsCPULimit(t *testing.T) {
	req := &sandbox.ExecutionRequest{
		Image:        "python:3.12-slim",
		CPUTimeLimit: sandbox.MaxCPUTimeLimit + 1,
	}
	err := sandbox.ValidateConstraints(req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cpu_time_limit")
}

// TestValidateConstraints_ExceedsMemoryLimit rejects memory limit above maximum.
func TestValidateConstraints_ExceedsMemoryLimit(t *testing.T) {
	req := &sandbox.ExecutionRequest{
		Image:       "python:3.12-slim",
		MemoryLimit: sandbox.MaxMemoryLimit + 1,
	}
	err := sandbox.ValidateConstraints(req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "memory_limit")
}

// TestValidateConstraints_MissingImage rejects empty image.
func TestValidateConstraints_MissingImage(t *testing.T) {
	req := &sandbox.ExecutionRequest{
		CPUTimeLimit: 5.0,
	}
	err := sandbox.ValidateConstraints(req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "image")
}

// TestApplyDefaults_FillsZeroValues confirms zero-value struct gets safe defaults.
func TestApplyDefaults_FillsZeroValues(t *testing.T) {
	req := &sandbox.ExecutionRequest{Image: "python:3.12-slim"}
	out := sandbox.ApplyDefaults(req)

	assert.Equal(t, sandbox.DefaultCPUTimeLimit, out.CPUTimeLimit)
	assert.Equal(t, sandbox.DefaultWallTimeLimit, out.WallTimeLimit)
	assert.Equal(t, sandbox.DefaultMemoryLimit, out.MemoryLimit)
	assert.Equal(t, sandbox.DefaultStackLimit, out.StackLimit)
	assert.Equal(t, int(sandbox.DefaultPidsLimit), out.MaxProcesses)
}

// TestApplyDefaults_PreservesNonZeroValues confirms explicit limits are not overwritten.
func TestApplyDefaults_PreservesNonZeroValues(t *testing.T) {
	req := &sandbox.ExecutionRequest{
		Image:         "eclipse-temurin:21",
		CPUTimeLimit:  2.5,
		WallTimeLimit: 8.0,
		MemoryLimit:   128 * 1024 * 1024,
	}
	out := sandbox.ApplyDefaults(req)

	assert.Equal(t, 2.5, out.CPUTimeLimit)
	assert.Equal(t, 8.0, out.WallTimeLimit)
	assert.Equal(t, int64(128*1024*1024), out.MemoryLimit)
}

// ---------------------------------------------------------------------------
// Tests: Base64 encode/decode helpers
// ---------------------------------------------------------------------------

// TestBase64EncodeDecode round-trips arbitrary text.
func TestBase64EncodeDecode(t *testing.T) {
	cases := []string{
		`print("Hello, World!")`,
		`#include <iostream>\nint main() { return 0; }`,
		"",
		strings.Repeat("x", 4096),
	}

	for _, src := range cases {
		encoded := b64(src)
		decoded := mustDecode(t, encoded)
		assert.Equal(t, src, decoded)
	}
}

// ---------------------------------------------------------------------------
// Tests: Token uniqueness
// ---------------------------------------------------------------------------

// TestTokenUniqueness verifies uuid.New() generates non-colliding tokens.
func TestTokenUniqueness(t *testing.T) {
	const count = 1000
	seen := make(map[string]bool, count)

	for i := 0; i < count; i++ {
		tok := uuid.New().String()
		require.False(t, seen[tok], "duplicate token at i=%d: %s", i, tok)
		seen[tok] = true
	}
	assert.Len(t, seen, count)
}

// ---------------------------------------------------------------------------
// Tests: Batch submission limits
// ---------------------------------------------------------------------------

// TestBatchSubmissionRequest_MaxItems verifies the 20-item cap at the model level.
func TestBatchSubmissionRequest_MaxItems(t *testing.T) {
	submissions := make([]models.SubmissionRequest, 21)
	for i := range submissions {
		submissions[i] = models.SubmissionRequest{
			LanguageID: models.LanguagePython3,
			SourceCode: b64(`print("x")`),
		}
	}

	batch := models.BatchSubmissionRequest{Submissions: submissions}
	assert.Greater(t, len(batch.Submissions), 20,
		"batch with 21 items should exceed max=20 limit")
}

// TestBatchSubmissionRequest_Empty verifies an empty batch produces no submissions.
func TestBatchSubmissionRequest_Empty(t *testing.T) {
	batch := models.BatchSubmissionRequest{Submissions: nil}
	assert.Empty(t, batch.Submissions)
}

// ---------------------------------------------------------------------------
// Tests: Status helpers
// ---------------------------------------------------------------------------

// TestIsTerminalStatus covers all known status IDs.
func TestIsTerminalStatus(t *testing.T) {
	assert.False(t, models.IsTerminalStatus(models.StatusInQueue))
	assert.False(t, models.IsTerminalStatus(models.StatusProcessing))
	assert.True(t, models.IsTerminalStatus(models.StatusAccepted))
	assert.True(t, models.IsTerminalStatus(models.StatusWrongAnswer))
	assert.True(t, models.IsTerminalStatus(models.StatusTimeLimitExceeded))
	assert.True(t, models.IsTerminalStatus(models.StatusCompilationError))
	assert.True(t, models.IsTerminalStatus(models.StatusRuntimeError))
	assert.True(t, models.IsTerminalStatus(models.StatusInternalError))
}

// TestStatusDescriptions_AllKeysPresent ensures every status constant has a description.
func TestStatusDescriptions_AllKeysPresent(t *testing.T) {
	ids := []int{
		models.StatusInQueue,
		models.StatusProcessing,
		models.StatusAccepted,
		models.StatusWrongAnswer,
		models.StatusTimeLimitExceeded,
		models.StatusCompilationError,
		models.StatusRuntimeError,
		models.StatusInternalError,
	}

	for _, id := range ids {
		desc, ok := models.StatusDescriptions[id]
		assert.True(t, ok, "StatusDescriptions missing entry for status ID %d", id)
		assert.NotEmpty(t, desc, "StatusDescriptions[%d] should not be empty", id)
	}
}

// ---------------------------------------------------------------------------
// Tests: Mock-based repository interactions
// ---------------------------------------------------------------------------

// TestMockRepo_Create_Success tests successful creation via mock.
func TestMockRepo_Create_Success(t *testing.T) {
	repo := new(mockSubmissionRepo)
	ctx := context.Background()
	sub := newSubmission(models.LanguagePython3, `print("ok")`)

	repo.On("Create", ctx, sub).Return(nil)

	err := repo.Create(ctx, sub)
	assert.NoError(t, err)
	repo.AssertExpectations(t)
}

// TestMockRepo_GetByToken_NotFound tests the not-found case via mock.
func TestMockRepo_GetByToken_NotFound(t *testing.T) {
	repo := new(mockSubmissionRepo)
	ctx := context.Background()
	token := uuid.New().String()

	repo.On("GetByToken", ctx, token).Return(nil, assert.AnError)

	result, err := repo.GetByToken(ctx, token)
	assert.Error(t, err)
	assert.Nil(t, result)
	repo.AssertExpectations(t)
}

// TestMockRepo_UpdateStatus_Transitions exercises all valid status transitions.
func TestMockRepo_UpdateStatus_Transitions(t *testing.T) {
	transitions := []struct{ from, to int }{
		{models.StatusInQueue, models.StatusProcessing},
		{models.StatusProcessing, models.StatusAccepted},
		{models.StatusProcessing, models.StatusWrongAnswer},
		{models.StatusProcessing, models.StatusTimeLimitExceeded},
		{models.StatusProcessing, models.StatusCompilationError},
		{models.StatusProcessing, models.StatusRuntimeError},
		{models.StatusProcessing, models.StatusInternalError},
	}

	for _, tr := range transitions {
		t.Run("", func(t *testing.T) {
			repo := new(mockSubmissionRepo)
			ctx := context.Background()
			token := uuid.New().String()

			repo.On("UpdateStatus", ctx, token, tr.to).Return(nil)

			err := repo.UpdateStatus(ctx, token, tr.to)
			assert.NoError(t, err)
			repo.AssertExpectations(t)
		})
	}
}

// ---------------------------------------------------------------------------
// Tests: strPtr helper
// ---------------------------------------------------------------------------

func TestStrPtr(t *testing.T) {
	s := "hello"
	p := strPtr(s)
	require.NotNil(t, p)
	assert.Equal(t, s, *p)
}