package models

import "time"

// SubmissionRequest is the HTTP request DTO for creating a new submission.
type SubmissionRequest struct {
	SourceCode      string  `json:"source_code"            binding:"required"`
	LanguageID      int     `json:"language_id"            binding:"required,min=1"`
	Stdin           string  `json:"stdin"`
	ExpectedOutput  string  `json:"expected_output"`
	CPUTimeLimit    float64 `json:"cpu_time_limit"`
	WallTimeLimit   float64 `json:"wall_time_limit"`
	MemoryLimit     int64   `json:"memory_limit"`
	StackLimit      int64   `json:"stack_limit"`
	MaxProcesses    int     `json:"max_processes"`
	MaxFileSize     int64   `json:"max_file_size"`
	CompilerOptions string  `json:"compiler_options"`
	CommandLineArgs string  `json:"command_line_arguments"`
	CallbackURL     string  `json:"callback_url"`
	Wait            bool    `json:"wait"`
}

// SubmissionResponse is the HTTP response DTO for a submission.
type SubmissionResponse struct {
	Token         string     `json:"token"`
	SourceCode    string     `json:"source_code,omitempty"`
	LanguageID    int        `json:"language_id"`
	Stdin         string     `json:"stdin,omitempty"`
	Stdout        *string    `json:"stdout"`
	Stderr        *string    `json:"stderr"`
	CompileOutput *string    `json:"compile_output"`
	ExitCode      *int       `json:"exit_code"`
	WallTime      *float64   `json:"wall_time"`
	CPUTime       *float64   `json:"time"`
	Memory        *float64   `json:"memory"`
	Status        StatusInfo `json:"status"`
	CreatedAt     time.Time  `json:"created_at"`
	FinishedAt    *time.Time `json:"finished_at"`
	Message       string     `json:"message,omitempty"`
}

// StatusInfo embeds status ID and description in the response.
type StatusInfo struct {
	ID          int    `json:"id"`
	Description string `json:"description"`
}

// BatchSubmissionRequest holds multiple submission requests.
type BatchSubmissionRequest struct {
	Submissions []SubmissionRequest `json:"submissions" binding:"required,min=1,max=20,dive"`
}

// BatchSubmissionResponse holds multiple submission tokens.
type BatchSubmissionResponse struct {
	Tokens []string `json:"tokens"`
}

// PaginatedSubmissionsResponse wraps a page of submissions.
type PaginatedSubmissionsResponse struct {
	Submissions []SubmissionResponse `json:"submissions"`
	Total       int64                `json:"total"`
	Page        int                  `json:"page"`
	PerPage     int                  `json:"per_page"`
}

// SubmissionToResponse converts a Submission DB model to an API response DTO.
func SubmissionToResponse(s *Submission, includeSource bool) SubmissionResponse {
	resp := SubmissionResponse{
		Token:      s.Token,
		LanguageID: s.LanguageID,
		Stdout:     s.Stdout,
		Stderr:     s.Stderr,
		CompileOutput: s.CompileOutput,
		ExitCode:   s.ExitCode,
		WallTime:   s.WallTime,
		CPUTime:    s.Time,
		Memory:     s.Memory,
		CreatedAt:  s.CreatedAt,
		FinishedAt: s.FinishedAt,
		Status: StatusInfo{
			ID:          s.StatusID,
			Description: StatusDescriptions[s.StatusID],
		},
	}
	if s.Message != nil {
		resp.Message = *s.Message
	}
	if includeSource {
		resp.SourceCode = s.SourceCode
		if s.Stdin != nil {
			resp.Stdin = *s.Stdin
		}
	}
	return resp
}

// LoginRequest is the request body for POST /auth/token.
type LoginRequest struct {
	Email    string `json:"email"    binding:"required,email"`
	Password string `json:"password" binding:"required"`
}

// TokenResponse is the response for a successful JWT issue.
type TokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"` // seconds
}

// CreateAPIKeyRequest is the request body for POST /auth/apikey.
type CreateAPIKeyRequest struct {
	Name string `json:"name" binding:"required,min=1,max=255"`
}

// APIKeyResponse is the response for a newly created API key.
// The full key is returned only on creation.
type APIKeyResponse struct {
	ID     uint   `json:"id"`
	Name   string `json:"name"`
	Prefix string `json:"prefix"`
	Key    string `json:"key"` // full key — shown once
}

// HealthResponse is returned by GET /health.
type HealthResponse struct {
	Status   string            `json:"status"`
	Services map[string]string `json:"services"`
}

// StatusResponse is the response DTO for a single execution status.
type StatusResponse struct {
	ID          int    `json:"id"`
	Description string `json:"description"`
}

// LanguageResponse is the response DTO for a single language.
type LanguageResponse struct {
	ID             int     `json:"id"`
	Name           string  `json:"name"`
	Version        string  `json:"version"`
	IsActive       bool    `json:"is_active"`
	SourceFile     string  `json:"source_file"`
	CompileCommand *string `json:"compile_command,omitempty"`
	RunCommand     string  `json:"run_command"`
	Image          string  `json:"image"`
	MaxCPUTime     float64 `json:"max_cpu_time"`
	MaxMemory      int64   `json:"max_memory"`
}
