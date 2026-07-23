package models

import "time"

// ExecutionResult holds the outcome of running a submission inside a container.
type ExecutionResult struct {
	Stdout        string  `json:"stdout"`
	Stderr        string  `json:"stderr"`
	CompileOutput string  `json:"compile_output"`
	ExitCode      int     `json:"exit_code"`
	ExitSignal    string  `json:"exit_signal"`
	WallTime      float64 `json:"wall_time"`
	CPUTime       float64 `json:"cpu_time"`
	MemoryUsed    int64   `json:"memory_used"`
	Error         string  `json:"error,omitempty"`
	Status        int     `json:"status"`
}

// IsSuccessful returns true when the execution exited cleanly with status Accepted.
func (r *ExecutionResult) IsSuccessful() bool {
	return r.Status == StatusAccepted && r.ExitCode == 0
}

// HasCompileError returns true when compilation failed.
func (r *ExecutionResult) HasCompileError() bool {
	return r.Status == StatusCompilationError
}

// ExecutionJob is the message placed on the NATS / Redis queue for worker pickup.
type ExecutionJob struct {
	SubmissionToken string    `json:"submission_token"`
	LanguageID      int       `json:"language_id"`
	LanguageName    string    `json:"language_name,omitempty"`
	SourceCode      string    `json:"source_code"`
	Stdin           string    `json:"stdin"`
	ExpectedOutput  string    `json:"expected_output"`
	CPUTimeLimit    float64   `json:"cpu_time_limit"`
	WallTimeLimit   float64   `json:"wall_time_limit"`
	MemoryLimit     int64     `json:"memory_limit"`
	StackLimit      int64     `json:"stack_limit"`
	MaxProcesses    int       `json:"max_processes"`
	MaxFileSize     int64     `json:"max_file_size"`
	CompilerOptions string    `json:"compiler_options"`
	CommandLineArgs string    `json:"command_line_arguments"`
	CallbackURL     string    `json:"callback_url"`
	BatchID         string    `json:"batch_id,omitempty"`
	RetryCount      int       `json:"retry_count"`
	Priority        int       `json:"priority"`
	EnqueuedAt      time.Time `json:"enqueued_at"`
}

// Validate ensures required fields are present.
func (j *ExecutionJob) Validate() error {
	if j.SubmissionToken == "" {
		return ErrMissingSubmissionToken
	}
	if j.LanguageID <= 0 {
		return ErrInvalidLanguageID
	}
	if j.SourceCode == "" {
		return ErrMissingSourceCode
	}
	return nil
}

// ApplyDefaults fills in zero-value limits with safe defaults.
func (j *ExecutionJob) ApplyDefaults() {
	if j.CPUTimeLimit <= 0 {
		j.CPUTimeLimit = 5.0
	}
	if j.WallTimeLimit <= 0 {
		j.WallTimeLimit = 10.0
	}
	if j.MemoryLimit <= 0 {
		j.MemoryLimit = 262144
	}
	if j.StackLimit <= 0 {
		j.StackLimit = 65536
	}
	if j.MaxProcesses <= 0 {
		j.MaxProcesses = 60
	}
	if j.MaxFileSize <= 0 {
		j.MaxFileSize = 4096
	}
	if j.EnqueuedAt.IsZero() {
		j.EnqueuedAt = time.Now().UTC()
	}
}

// FromSubmission populates an ExecutionJob from a Submission model.
func (j *ExecutionJob) FromSubmission(s *Submission) {
	j.SubmissionToken = s.Token
	j.LanguageID = s.LanguageID
	j.SourceCode = s.SourceCode
	if s.Stdin != nil {
		j.Stdin = *s.Stdin
	}
	j.ExpectedOutput = s.ExpectedOutput
	j.CPUTimeLimit = s.CPUTimeLimit
	j.WallTimeLimit = s.WallTimeLimit
	j.MemoryLimit = s.MemoryLimit
	j.StackLimit = s.StackLimit
	j.MaxProcesses = s.MaxProcesses
	j.MaxFileSize = s.MaxFileSize
	j.CompilerOptions = s.CompilerOptions
	j.CommandLineArgs = s.CommandLineArgs
	j.CallbackURL = s.CallbackURL
	if s.BatchID != nil {
		j.BatchID = *s.BatchID
	}
	j.EnqueuedAt = time.Now().UTC()
	j.ApplyDefaults()
}

// Execution-domain sentinel errors.
type executionError string

func (e executionError) Error() string { return string(e) }

const (
	ErrMissingSubmissionToken executionError = "submission token is required"
	ErrInvalidLanguageID      executionError = "language_id must be a positive integer"
	ErrMissingSourceCode      executionError = "source_code is required"
)

// WorkerHeartbeat is published by workers to signal liveness.
type WorkerHeartbeat struct {
	WorkerID   string    `json:"worker_id"`
	ActiveJobs int       `json:"active_jobs"`
	Timestamp  time.Time `json:"timestamp"`
}

// CallbackPayload is the JSON body sent to CallbackURL when a submission finishes.
type CallbackPayload struct {
	Token         string     `json:"token"`
	StatusID      int        `json:"status_id"`
	Stdout        string     `json:"stdout,omitempty"`
	Stderr        string     `json:"stderr,omitempty"`
	CompileOutput string     `json:"compile_output,omitempty"`
	ExitCode      int        `json:"exit_code"`
	WallTime      float64    `json:"wall_time"`
	CPUTime       float64    `json:"cpu_time"`
	Memory        int64      `json:"memory"`
	FinishedAt    *time.Time `json:"finished_at"`
}
