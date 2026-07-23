package unit_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/revature/corems-code-executor/internal/models"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func encodeB64(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

func timePtr(t time.Time) *time.Time { return &t }

// executionOutcome groups the inputs for mapExitCodeToStatus, keeping the
// parameter count within the linter's 7-parameter limit (S107).
type executionOutcome struct {
	exitCode      int
	exitSignal    string
	compileOutput string
	expected      string
	actual        string
	errMsg        string
	wallTime      float64
	wallLimit     float64
}

// mapExitCodeToStatus mirrors the worker's verdict logic.
//
//	exit 0 + no expected_output  -> Accepted
//	exit 0 + matching expected   -> Accepted
//	exit 0 + differing expected  -> WrongAnswer
//	exit non-zero (no signal)    -> RuntimeError
//	SIGKILL or wall_time breach  -> TimeLimitExceeded
//	compile output non-empty     -> CompilationError
//	internal error string        -> InternalError
func mapExitCodeToStatus(o executionOutcome) int {
	if o.errMsg != "" {
		return models.StatusInternalError
	}
	if o.compileOutput != "" {
		return models.StatusCompilationError
	}
	if o.exitSignal == "SIGKILL" || (o.wallLimit > 0 && o.wallTime >= o.wallLimit) {
		return models.StatusTimeLimitExceeded
	}
	if o.exitCode != 0 {
		return models.StatusRuntimeError
	}
	if o.expected != "" && strings.TrimRight(o.actual, "\n") != strings.TrimRight(o.expected, "\n") {
		return models.StatusWrongAnswer
	}
	return models.StatusAccepted
}

// invokeCallback is the simplified callback-invocation logic tested below.
// The real implementation lives in the worker; this mirrors its behaviour.
func invokeCallback(callbackURL string, payload models.CallbackPayload) error {
	if callbackURL == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return doCallback(ctx, callbackURL, payload)
}

func invokeCallbackWithTimeout(cbURL string, payload models.CallbackPayload, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return doCallback(ctx, cbURL, payload)
}

func doCallback(ctx context.Context, cbURL string, payload models.CallbackPayload) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cbURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("callback returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Tests: Exit code → status mapping
// ---------------------------------------------------------------------------

// TestMapExitCode_Accepted verifies exit code 0 with no expected output yields Accepted.
func TestMapExitCode_Accepted(t *testing.T) {
	status := mapExitCodeToStatus(executionOutcome{wallTime: 0.5, wallLimit: 10.0})
	assert.Equal(t, models.StatusAccepted, status)
}

// TestMapExitCode_RuntimeError verifies non-zero exit code yields RuntimeError.
func TestMapExitCode_RuntimeError(t *testing.T) {
	for _, code := range []int{1, 2, 127, 139, 255} {
		status := mapExitCodeToStatus(executionOutcome{exitCode: code, wallTime: 0.5, wallLimit: 10.0})
		assert.Equal(t, models.StatusRuntimeError, status,
			"exit code %d should produce RuntimeError", code)
	}
}

// TestMapExitCode_CompilationError detects non-empty compile output.
func TestMapExitCode_CompilationError(t *testing.T) {
	compileErr := "main.cpp:3:10: error: 'undeclared' was not declared in this scope"
	status := mapExitCodeToStatus(executionOutcome{exitCode: 1, compileOutput: compileErr, wallTime: 0.1, wallLimit: 10.0})
	assert.Equal(t, models.StatusCompilationError, status)
}

// TestMapExitCode_TimeLimitExceeded_SIGKILL detects SIGKILL as TLE.
func TestMapExitCode_TimeLimitExceeded_SIGKILL(t *testing.T) {
	status := mapExitCodeToStatus(executionOutcome{exitCode: 137, exitSignal: "SIGKILL", wallTime: 10.1, wallLimit: 10.0})
	assert.Equal(t, models.StatusTimeLimitExceeded, status)
}

// TestMapExitCode_TimeLimitExceeded_WallTime detects wall time at or over limit as TLE.
func TestMapExitCode_TimeLimitExceeded_WallTime(t *testing.T) {
	status := mapExitCodeToStatus(executionOutcome{wallTime: 10.5, wallLimit: 10.0})
	assert.Equal(t, models.StatusTimeLimitExceeded, status)
}

// TestMapExitCode_InternalError detects worker-level internal errors.
func TestMapExitCode_InternalError(t *testing.T) {
	status := mapExitCodeToStatus(executionOutcome{errMsg: "docker daemon unreachable", wallLimit: 10.0})
	assert.Equal(t, models.StatusInternalError, status)
}

// ---------------------------------------------------------------------------
// Tests: Wrong Answer detection
// ---------------------------------------------------------------------------

// TestWrongAnswer_MatchingOutput verifies identical output yields Accepted.
func TestWrongAnswer_MatchingOutput(t *testing.T) {
	status := mapExitCodeToStatus(executionOutcome{expected: "42\n", actual: "42\n", wallTime: 0.3, wallLimit: 10.0})
	assert.Equal(t, models.StatusAccepted, status)
}

// TestWrongAnswer_NonMatchingOutput verifies differing output yields WrongAnswer.
func TestWrongAnswer_NonMatchingOutput(t *testing.T) {
	status := mapExitCodeToStatus(executionOutcome{expected: "42\n", actual: "43\n", wallTime: 0.3, wallLimit: 10.0})
	assert.Equal(t, models.StatusWrongAnswer, status)
}

// TestWrongAnswer_TrailingNewline verifies trailing newline is ignored in comparison.
func TestWrongAnswer_TrailingNewline(t *testing.T) {
	status := mapExitCodeToStatus(executionOutcome{expected: "hello", actual: "hello\n", wallTime: 0.1, wallLimit: 10.0})
	assert.Equal(t, models.StatusAccepted, status)
}

// TestWrongAnswer_NoExpectedOutput yields Accepted when expected is empty.
func TestWrongAnswer_NoExpectedOutput(t *testing.T) {
	status := mapExitCodeToStatus(executionOutcome{actual: "anything", wallTime: 0.1, wallLimit: 10.0})
	assert.Equal(t, models.StatusAccepted, status)
}

// TestWrongAnswer_EmptyActualVsNonEmpty verifies empty actual with non-empty expected is WA.
func TestWrongAnswer_EmptyActualVsNonEmpty(t *testing.T) {
	status := mapExitCodeToStatus(executionOutcome{expected: "42\n", wallTime: 0.1, wallLimit: 10.0})
	assert.Equal(t, models.StatusWrongAnswer, status)
}

// ---------------------------------------------------------------------------
// Tests: Timeout handling
// ---------------------------------------------------------------------------

// TestTimeout_ContextCancellation simulates a context deadline exceeded scenario.
func TestTimeout_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(done)
	}()

	select {
	case <-ctx.Done():
		assert.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
	case <-done:
		t.Fatal("expected context to expire before goroutine completed")
	}
}

// TestTimeout_WallTimeExceedsLimit verifies TLE verdict for a wall-time breach.
func TestTimeout_WallTimeExceedsLimit(t *testing.T) {
	status := mapExitCodeToStatus(executionOutcome{exitCode: 137, exitSignal: "SIGKILL", wallTime: 5.001, wallLimit: 5.0})
	assert.Equal(t, models.StatusTimeLimitExceeded, status)
}

// ---------------------------------------------------------------------------
// Tests: Memory limit exceeded
// ---------------------------------------------------------------------------

// TestMemoryLimitExceeded_OOMKill confirms Docker OOM kill (SIGKILL) maps to TLE.
func TestMemoryLimitExceeded_OOMKill(t *testing.T) {
	// Docker delivers SIGKILL on OOM; exit code is 137.
	status := mapExitCodeToStatus(executionOutcome{exitCode: 137, exitSignal: "SIGKILL", wallTime: 0.1, wallLimit: 10.0})
	assert.Equal(t, models.StatusTimeLimitExceeded, status)
}

// TestMemoryLimitExceeded_ExitCode checks non-zero exit without signal → RuntimeError.
func TestMemoryLimitExceeded_ExitCode(t *testing.T) {
	status := mapExitCodeToStatus(executionOutcome{exitCode: 1, wallTime: 0.1, wallLimit: 10.0})
	assert.Equal(t, models.StatusRuntimeError, status)
}

// ---------------------------------------------------------------------------
// Tests: Compile error handling
// ---------------------------------------------------------------------------

// TestCompileError_PyNoCompile verifies Python runtime errors don't set compile output.
func TestCompileError_PyNoCompile(t *testing.T) {
	// Python does not compile; exit 1 with no compile output → RuntimeError.
	status := mapExitCodeToStatus(executionOutcome{exitCode: 1, wallTime: 0.05, wallLimit: 10.0})
	assert.Equal(t, models.StatusRuntimeError, status)
}

// TestCompileError_CPPSyntaxError verifies C++ compile failure detection.
func TestCompileError_CPPSyntaxError(t *testing.T) {
	co := "main.cpp:1:1: error: expected ';' before '}' token"
	status := mapExitCodeToStatus(executionOutcome{exitCode: 1, compileOutput: co, wallLimit: 10.0})
	assert.Equal(t, models.StatusCompilationError, status)
}

// TestCompileError_JavaMissingClass verifies Java class-not-found compile error.
func TestCompileError_JavaMissingClass(t *testing.T) {
	co := "Main.java:1: error: class Solution is public, should be declared in a file named Solution.java"
	status := mapExitCodeToStatus(executionOutcome{exitCode: 1, compileOutput: co, wallLimit: 10.0})
	assert.Equal(t, models.StatusCompilationError, status)
}

// TestCompileError_RustLinker verifies Rust linker error is treated as compile error.
func TestCompileError_RustLinker(t *testing.T) {
	co := "error[E0425]: cannot find value `x` in this scope"
	status := mapExitCodeToStatus(executionOutcome{exitCode: 101, compileOutput: co, wallLimit: 10.0})
	assert.Equal(t, models.StatusCompilationError, status)
}

// ---------------------------------------------------------------------------
// Tests: Callback URL invocation
// ---------------------------------------------------------------------------

// TestCallbackURL_SuccessfulInvocation verifies the callback receives a valid POST.
func TestCallbackURL_SuccessfulInvocation(t *testing.T) {
	var receivedMethod string
	var receivedContentType string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedMethod = r.Method
		receivedContentType = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	payload := models.CallbackPayload{
		Token:      uuid.New().String(),
		StatusID:   models.StatusAccepted,
		Stdout:     encodeB64("42\n"),
		ExitCode:   0,
		WallTime:   0.3,
		CPUTime:    0.28,
		Memory:     8192,
		FinishedAt: timePtr(time.Now().UTC()),
	}

	err := invokeCallback(srv.URL, payload)
	require.NoError(t, err)
	assert.Equal(t, "POST", receivedMethod)
	assert.Equal(t, "application/json", receivedContentType)
}

// TestCallbackURL_404Response verifies the caller treats non-2xx as an error.
func TestCallbackURL_404Response(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	payload := models.CallbackPayload{
		Token:    uuid.New().String(),
		StatusID: models.StatusAccepted,
	}
	err := invokeCallback(srv.URL, payload)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "404")
}

// TestCallbackURL_Timeout verifies context timeout during callback is surfaced.
func TestCallbackURL_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	payload := models.CallbackPayload{
		Token:    uuid.New().String(),
		StatusID: models.StatusAccepted,
	}
	err := invokeCallbackWithTimeout(srv.URL, payload, 10*time.Millisecond)
	require.Error(t, err)
}

// TestCallbackURL_EmptyURL verifies empty callback URL is a no-op (no error).
func TestCallbackURL_EmptyURL(t *testing.T) {
	payload := models.CallbackPayload{
		Token:    uuid.New().String(),
		StatusID: models.StatusAccepted,
	}
	err := invokeCallback("", payload)
	assert.NoError(t, err)
}

// TestCallbackURL_PayloadContainsToken verifies the JSON body includes the token.
func TestCallbackURL_PayloadContainsToken(t *testing.T) {
	token := uuid.New().String()
	var body []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body = make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	payload := models.CallbackPayload{
		Token:    token,
		StatusID: models.StatusAccepted,
	}
	err := invokeCallback(srv.URL, payload)
	require.NoError(t, err)
	assert.Contains(t, string(body), token)
}

// ---------------------------------------------------------------------------
// Tests: ExecutionResult model helpers
// ---------------------------------------------------------------------------

// TestExecutionResult_IsSuccessful_Accepted verifies exit 0 + Accepted status.
func TestExecutionResult_IsSuccessful_Accepted(t *testing.T) {
	result := &models.ExecutionResult{
		ExitCode: 0,
		Status:   models.StatusAccepted,
	}
	assert.True(t, result.IsSuccessful())
}

// TestExecutionResult_IsSuccessful_WrongAnswer verifies WA is not successful.
func TestExecutionResult_IsSuccessful_WrongAnswer(t *testing.T) {
	result := &models.ExecutionResult{
		ExitCode: 0,
		Status:   models.StatusWrongAnswer,
	}
	assert.False(t, result.IsSuccessful())
}

// TestExecutionResult_IsSuccessful_RuntimeError verifies non-zero exit is not successful.
func TestExecutionResult_IsSuccessful_RuntimeError(t *testing.T) {
	result := &models.ExecutionResult{
		ExitCode: 1,
		Status:   models.StatusRuntimeError,
	}
	assert.False(t, result.IsSuccessful())
}

// TestExecutionResult_HasCompileError checks the compile-error predicate.
func TestExecutionResult_HasCompileError(t *testing.T) {
	result := &models.ExecutionResult{
		Status:        models.StatusCompilationError,
		CompileOutput: "error: undeclared identifier",
	}
	assert.True(t, result.HasCompileError())
}

// TestExecutionResult_HasCompileError_False verifies non-compile statuses return false.
func TestExecutionResult_HasCompileError_False(t *testing.T) {
	result := &models.ExecutionResult{
		Status:   models.StatusRuntimeError,
		ExitCode: 1,
	}
	assert.False(t, result.HasCompileError())
}

// ---------------------------------------------------------------------------
// Tests: B64 helper used in callbacks
// ---------------------------------------------------------------------------

// TestEncodeB64_NonEmpty verifies base64 encoding produces non-empty output.
func TestEncodeB64_NonEmpty(t *testing.T) {
	encoded := encodeB64("Hello, World!\n")
	assert.NotEmpty(t, encoded)
	assert.NotContains(t, encoded, "\n")
}

// TestEncodeB64_Empty verifies empty string encodes to empty base64.
func TestEncodeB64_Empty(t *testing.T) {
	encoded := encodeB64("")
	assert.Empty(t, encoded)
}
