package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/mdshabbir-ali/code-runtime/internal/cache"
	"github.com/mdshabbir-ali/code-runtime/internal/config"
	"github.com/mdshabbir-ali/code-runtime/internal/database"
	"github.com/mdshabbir-ali/code-runtime/internal/metrics"
	"github.com/mdshabbir-ali/code-runtime/internal/models"
	"github.com/mdshabbir-ali/code-runtime/internal/runtime"
	"github.com/mdshabbir-ali/code-runtime/internal/sandbox"
)

// tokenWhereClause is the GORM WHERE clause used consistently across all
// submission look-ups and updates in this package.
const tokenWhereClause = "token = ?"

// WorkerDeps groups all external dependencies injected into a Worker,
// keeping NewWorker's parameter count within the project limit.
type WorkerDeps struct {
	Sandbox   *sandbox.DockerSandbox
	DBRuntime *runtime.DBRuntime
	Runtime   *runtime.Manager
	DB        *database.DB
	SubCache  *cache.SubmissionCache
	LangCache *cache.LanguageCache
	NATSConn  *nats.Conn
	Logger    *zap.Logger
	Metrics   *metrics.Metrics
	Cfg       *config.WorkerConfig
}

// Worker consumes execution jobs from NATS, runs them in the sandbox, and
// persists the results.
type Worker struct {
	id        string
	sandbox   *sandbox.DockerSandbox
	dbRuntime *runtime.DBRuntime
	runtime   *runtime.Manager
	db        *database.DB
	subCache  *cache.SubmissionCache
	langCache *cache.LanguageCache
	natsConn  *nats.Conn
	logger    *zap.Logger
	metrics   *metrics.Metrics
	cfg       *config.WorkerConfig
	semaphore chan struct{} // limits concurrent jobs
	wg        sync.WaitGroup
	stopCh    chan struct{}
	httpCli   *http.Client
}

// NewWorker constructs a Worker from the given id and dependency bundle.
func NewWorker(id string, deps WorkerDeps) *Worker {
	concurrency := deps.Cfg.Concurrency
	if concurrency <= 0 {
		concurrency = 1
	}
	sem := make(chan struct{}, concurrency)
	for i := 0; i < concurrency; i++ {
		sem <- struct{}{}
	}

	return &Worker{
		id:        id,
		sandbox:   deps.Sandbox,
		dbRuntime: deps.DBRuntime,
		runtime:   deps.Runtime,
		db:        deps.DB,
		subCache:  deps.SubCache,
		langCache: deps.LangCache,
		natsConn:  deps.NATSConn,
		logger:    deps.Logger.With(zap.String("worker_id", id)),
		metrics:   deps.Metrics,
		cfg:       deps.Cfg,
		semaphore: sem,
		stopCh:    make(chan struct{}),
		httpCli: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// Start subscribes to the NATS subject and blocks until ctx is cancelled.
func (w *Worker) Start(ctx context.Context) error {
	subject := w.cfg.NATSSubject
	if subject == "" {
		subject = "submissions"
	}

	sub, err := w.natsConn.QueueSubscribe(subject, "workers", func(msg *nats.Msg) {
		var job models.ExecutionJob
		if err := json.Unmarshal(msg.Data, &job); err != nil {
			w.logger.Error("failed to decode job", zap.Error(err))
			return
		}

		// Validate
		if err := job.Validate(); err != nil {
			w.logger.Warn("invalid job received", zap.Error(err), zap.Any("job", job))
			return
		}

		// Acquire concurrency slot (blocks until a slot is free).
		select {
		case <-w.stopCh:
			return
		case tok := <-w.semaphore:
			w.wg.Add(1)
			go func() {
				defer func() {
					w.semaphore <- tok
					w.wg.Done()
				}()
				jobCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
				defer cancel()
				if err := w.processJob(jobCtx, &job); err != nil {
					w.logger.Error("job processing failed",
						zap.String("token", job.SubmissionToken),
						zap.Error(err),
					)
				}
			}()
		}
	})
	if err != nil {
		return fmt.Errorf("worker %s: subscribe to %q: %w", w.id, subject, err)
	}

	w.logger.Info("worker started", zap.String("subject", subject))

	// Block until context is done.
	<-ctx.Done()
	_ = sub.Drain()
	return nil
}

// Stop signals the worker to stop accepting new jobs and waits for in-flight
// jobs to complete.
func (w *Worker) Stop() error {
	close(w.stopCh)
	w.wg.Wait()
	w.logger.Info("worker stopped")
	return nil
}

// ─── processJob ─────────────────────────────────────────────────────────────

// processJob executes the full lifecycle of a single code execution job.
func (w *Worker) processJob(ctx context.Context, job *models.ExecutionJob) error {
	token := job.SubmissionToken
	start := time.Now()

	w.logger.Info("processing job", zap.String("token", token), zap.Int("language_id", job.LanguageID))

	// ── 1. Mark submission as Processing ────────────────────────────────────
	now := time.Now().UTC()
	if err := w.updateSubmissionStatus(ctx, token, models.StatusProcessing, now); err != nil {
		return fmt.Errorf("mark processing: %w", err)
	}
	w.publishRedisStatus(ctx, token, models.StatusProcessing)
	w.metrics.InFlightJobs.WithLabelValues(w.id).Inc()
	defer w.metrics.InFlightJobs.WithLabelValues(w.id).Dec()

	// ── 2. Resolve language ──────────────────────────────────────────────────
	lang, err := w.runtime.GetLanguage(ctx, job.LanguageID)
	if err != nil {
		return w.failJob(ctx, token, models.StatusInternalError, fmt.Sprintf("language lookup: %v", err))
	}

	// ── 3. Build execution request ───────────────────────────────────────────
	sub := jobToSubmission(job)
	execReq := w.runtime.BuildExecutionRequest(sub, lang)

	// ── 4. Execute ──────────────────────────────────────────────────────────
	// MySQL and PostgreSQL need an ephemeral DB container (no readonly rootfs,
	// no capability drops) so the server can initialise its data directory.
	// SQLite runs fine in the standard sandbox via its RunCommand.
	var execResult *sandbox.ExecutionResult
	var execErr error

	if lang.IsDatabase && lang.DBType != "" && lang.DBType != "sqlite" && w.dbRuntime != nil {
		wallTime := lang.MaxCPUTime * 3
		if sub.WallTimeLimit > wallTime {
			wallTime = sub.WallTimeLimit
		}
		memBytes := lang.MaxMemory * 1024
		if sub.MemoryLimit*1024 > memBytes {
			memBytes = sub.MemoryLimit * 1024
		}
		dbReq := &runtime.DBExecutionRequest{
			DBType:        lang.DBType,
			SQL:           sub.SourceCode,
			WallTimeLimit: wallTime,
			MemoryLimit:   memBytes,
		}
		dbRes, dbErr := w.dbRuntime.Execute(ctx, dbReq)
		if dbErr != nil && dbRes == nil {
			return w.failJob(ctx, token, models.StatusInternalError, dbErr.Error())
		}
		if dbRes == nil {
			return w.failJob(ctx, token, models.StatusInternalError, "nil db execution result")
		}
		execResult = &sandbox.ExecutionResult{
			Stdout:     dbRes.Stdout,
			Stderr:     dbRes.Stderr,
			ExitCode:   dbRes.ExitCode,
			ExitSignal: dbRes.ExitSignal,
			WallTime:   dbRes.WallTime,
			Status:     dbRes.Status,
		}
		execErr = dbErr
	} else {
		execResult, execErr = w.sandbox.Execute(ctx, execReq)
	}

	if execErr != nil && execResult == nil {
		return w.failJob(ctx, token, models.StatusInternalError, execErr.Error())
	}
	if execResult == nil {
		return w.failJob(ctx, token, models.StatusInternalError, "nil execution result")
	}

	// ── 5. Map sandbox status to submission status ───────────────────────────
	statusID := mapSandboxStatus(execResult, job.ExpectedOutput)

	// ── 6. Persist result ────────────────────────────────────────────────────
	finishedAt := time.Now().UTC()
	wallTime := execResult.WallTime
	cpuTime := execResult.CPUTime
	memKB := float64(execResult.MemoryUsed) / 1024.0
	workerID := w.id

	updates := map[string]interface{}{
		"status_id":      statusID,
		"stdout":         strPtr(execResult.Stdout),
		"stderr":         strPtr(execResult.Stderr),
		"compile_output": strPtr(execResult.CompileOutput),
		"exit_code":      &execResult.ExitCode,
		"wall_time":      &wallTime,
		"time":           &cpuTime,
		"memory":         &memKB,
		"worker_id":      &workerID,
		"finished_at":    finishedAt,
	}
	if err := w.db.WithContext(ctx).
		Model(&models.Submission{}).
		Where(tokenWhereClause, token).
		Updates(updates).Error; err != nil {
		w.logger.Error("failed to persist result", zap.String("token", token), zap.Error(err))
	}

	// ── 7. Update Redis cache ────────────────────────────────────────────────
	resp := buildSubmissionResponse(token, lang, execResult, statusID, finishedAt)
	if err := w.subCache.Set(ctx, token, &resp); err != nil {
		w.logger.Warn("redis cache update failed", zap.String("token", token), zap.Error(err))
	}

	// ── 8. Publish completion notification ──────────────────────────────────
	w.publishDoneNotification(token, statusID)

	// ── 9. Log to execution_logs ─────────────────────────────────────────────
	w.writeExecutionLog(ctx, token, statusID, wallTime)

	// ── 10. Callback URL ─────────────────────────────────────────────────────
	if job.CallbackURL != "" {
		go w.sendCallback(job.CallbackURL, token, execResult, statusID, finishedAt)
	}

	// ── 11. Record metrics ────────────────────────────────────────────────────
	langIDStr := fmt.Sprintf("%d", job.LanguageID)
	w.metrics.JobsConsumed.WithLabelValues(w.id, fmt.Sprintf("%d", statusID)).Inc()
	w.metrics.JobDuration.WithLabelValues(w.id, langIDStr).Observe(time.Since(start).Seconds())

	w.logger.Info("job complete",
		zap.String("token", token),
		zap.Int("status_id", statusID),
		zap.Float64("wall_time", wallTime),
	)
	return nil
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func (w *Worker) updateSubmissionStatus(ctx context.Context, token string, statusID int, startedAt time.Time) error {
	return w.db.WithContext(ctx).
		Model(&models.Submission{}).
		Where(tokenWhereClause, token).
		Updates(map[string]interface{}{
			"status_id":  statusID,
			"started_at": startedAt,
			"worker_id":  w.id,
		}).Error
}

func (w *Worker) failJob(ctx context.Context, token string, statusID int, msg string) error {
	w.logger.Error("job failed", zap.String("token", token), zap.Int("status_id", statusID), zap.String("msg", msg))
	finishedAt := time.Now().UTC()
	_ = w.db.WithContext(ctx).
		Model(&models.Submission{}).
		Where(tokenWhereClause, token).
		Updates(map[string]interface{}{
			"status_id":   statusID,
			"message":     msg,
			"finished_at": finishedAt,
		}).Error
	w.publishRedisStatus(ctx, token, statusID)
	w.publishDoneNotification(token, statusID)
	w.metrics.JobsFailed.WithLabelValues(w.id, fmt.Sprintf("%d", statusID)).Inc()
	return fmt.Errorf("job %s failed with status %d: %s", token, statusID, msg)
}

func (w *Worker) publishRedisStatus(ctx context.Context, token string, statusID int) {
	resp := models.SubmissionResponse{
		Token: token,
		Status: models.StatusInfo{
			ID:          statusID,
			Description: models.StatusDescriptions[statusID],
		},
	}
	if err := w.subCache.Set(ctx, token, &resp); err != nil {
		w.logger.Warn("redis status update failed", zap.String("token", token), zap.Error(err))
	}
}

func (w *Worker) publishDoneNotification(token string, statusID int) {
	subject := "done:" + token
	payload, _ := json.Marshal(map[string]interface{}{
		"token":     token,
		"status_id": statusID,
	})
	if err := w.natsConn.Publish(subject, payload); err != nil {
		w.logger.Warn("nats done notification failed", zap.String("subject", subject), zap.Error(err))
	}
}

func (w *Worker) writeExecutionLog(ctx context.Context, token string, statusID int, wallTime float64) {
	level := "info"
	if statusID >= models.StatusWrongAnswer {
		level = "warn"
	}

	// SubmissionID mirrors Token in this schema (Token is the string PK).
	entry := models.ExecutionLog{
		SubmissionID: token,
		Token:        token,
		WorkerID:     w.id,
		Level:        level,
		Message:      fmt.Sprintf("status=%d wall_time=%.3fs", statusID, wallTime),
		CreatedAt:    time.Now().UTC(),
	}
	if err := w.db.WithContext(ctx).Create(&entry).Error; err != nil {
		w.logger.Warn("execution log write failed", zap.String("token", token), zap.Error(err))
	}
}

func (w *Worker) sendCallback(url, token string, res *sandbox.ExecutionResult, statusID int, finishedAt time.Time) {
	payload := models.CallbackPayload{
		Token:         token,
		StatusID:      statusID,
		Stdout:        res.Stdout,
		Stderr:        res.Stderr,
		CompileOutput: res.CompileOutput,
		ExitCode:      res.ExitCode,
		WallTime:      res.WallTime,
		CPUTime:       res.CPUTime,
		Memory:        res.MemoryUsed,
		FinishedAt:    &finishedAt,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		w.logger.Warn("callback marshal failed", zap.String("token", token), zap.Error(err))
		return
	}
	resp, err := w.httpCli.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		w.logger.Warn("callback POST failed", zap.String("url", url), zap.Error(err))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		w.logger.Warn("callback returned error status",
			zap.String("url", url),
			zap.Int("status", resp.StatusCode),
		)
	}
}

// ─── status mapping ──────────────────────────────────────────────────────────

// mapSandboxStatus converts the sandbox execution result into a canonical
// submission status ID, comparing expected output when the run succeeded.
func mapSandboxStatus(res *sandbox.ExecutionResult, expectedOutput string) int {
	switch res.Status {
	case sandbox.StatusTimeLimitExceeded:
		return models.StatusTimeLimitExceeded
	case sandbox.StatusCompilationError:
		return models.StatusCompilationError
	case sandbox.StatusRuntimeError:
		return models.StatusRuntimeError
	case sandbox.StatusInternalError:
		return models.StatusInternalError
	}

	if res.ExitCode != 0 {
		return models.StatusRuntimeError
	}

	// Successful execution: compare output if expected was provided.
	if expectedOutput != "" {
		actual := strings.TrimRight(res.Stdout, "\n\r ")
		expected := strings.TrimRight(expectedOutput, "\n\r ")
		if actual != expected {
			return models.StatusWrongAnswer
		}
	}

	return models.StatusAccepted
}

// ─── DTO adapters ─────────────────────────────────────────────────────────────

// jobToSubmission creates a lightweight Submission from an ExecutionJob so
// that BuildExecutionRequest can be reused without a DB round-trip.
// jobToSubmission creates a lightweight Submission stub from an ExecutionJob
// so BuildExecutionRequest can be called without a DB round-trip.
func jobToSubmission(job *models.ExecutionJob) *models.Submission {
	stdin := job.Stdin
	return &models.Submission{
		LanguageID:      job.LanguageID,
		SourceCode:      job.SourceCode,
		Stdin:           &stdin,
		ExpectedOutput:  job.ExpectedOutput,
		CPUTimeLimit:    job.CPUTimeLimit,
		WallTimeLimit:   job.WallTimeLimit,
		MemoryLimit:     job.MemoryLimit,
		StackLimit:      job.StackLimit,
		MaxProcesses:    job.MaxProcesses,
		MaxFileSize:     job.MaxFileSize,
		CompilerOptions: job.CompilerOptions,
		CommandLineArgs: job.CommandLineArgs,
		CallbackURL:     job.CallbackURL,
	}
}

// buildSubmissionResponse constructs the Redis-cached DTO from execution results.
func buildSubmissionResponse(
	token string,
	lang *models.Language,
	res *sandbox.ExecutionResult,
	statusID int,
	finishedAt time.Time,
) models.SubmissionResponse {
	exitCode := res.ExitCode
	wallTime := res.WallTime
	cpuTime := res.CPUTime
	memKB := float64(res.MemoryUsed) / 1024.0

	resp := models.SubmissionResponse{
		Token:      token,
		LanguageID: lang.ID,
		FinishedAt: &finishedAt,
		ExitCode:   &exitCode,
		Status: models.StatusInfo{
			ID:          statusID,
			Description: models.StatusDescriptions[statusID],
		},
	}
	if res.Stdout != "" {
		resp.Stdout = strPtr(res.Stdout)
	}
	if res.Stderr != "" {
		resp.Stderr = strPtr(res.Stderr)
	}
	if res.CompileOutput != "" {
		resp.CompileOutput = strPtr(res.CompileOutput)
	}
	if wallTime > 0 {
		resp.WallTime = &wallTime
	}
	if cpuTime > 0 {
		resp.CPUTime = &cpuTime
	}
	if memKB > 0 {
		resp.Memory = &memKB
	}
	return resp
}

// strPtr returns a pointer to a copy of s.
func strPtr(s string) *string { return &s }
