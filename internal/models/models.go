// Package models defines all domain types, DTOs and constants for code-runtime.
// This file contains the core DB-backed entity types that are not split into
// dedicated files (Submission, User, APIKey, ExecutionLog) plus shared helpers.
package models

import (
	"crypto/rand"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// ---------------------------------------------------------------------------
// JSONB helper
// ---------------------------------------------------------------------------

// JSONB is a custom type for PostgreSQL JSONB columns.
type JSONB map[string]interface{}

func (j JSONB) Value() (driver.Value, error) {
	if j == nil {
		return nil, nil
	}
	b, err := json.Marshal(j)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

func (j *JSONB) Scan(value interface{}) error {
	if value == nil {
		*j = nil
		return nil
	}
	var bytes []byte
	switch v := value.(type) {
	case []byte:
		bytes = v
	case string:
		bytes = []byte(v)
	default:
		return fmt.Errorf("unsupported type: %T", value)
	}
	return json.Unmarshal(bytes, j)
}

// ---------------------------------------------------------------------------
// Submission
// ---------------------------------------------------------------------------

// Submission is the central DB-backed record for a code submission.
// Token is a UUID string set before creation; it is the primary public identifier.
type Submission struct {
	Token           string     `gorm:"type:varchar(36);primaryKey"           json:"token"`
	LanguageID      int        `gorm:"not null;index"                         json:"language_id"`
	SourceCode      string     `gorm:"type:text;not null"                     json:"source_code"`
	Stdin           *string    `gorm:"type:text"                              json:"stdin"`
	ExpectedOutput  string     `gorm:"type:text"                              json:"expected_output"`
	StatusID        int        `gorm:"not null;default:1;index"               json:"status_id"`
	Stdout          *string    `gorm:"type:text"                              json:"stdout"`
	Stderr          *string    `gorm:"type:text"                              json:"stderr"`
	CompileOutput   *string    `gorm:"type:text"                              json:"compile_output"`
	Message         *string    `gorm:"type:text"                              json:"message"`
	ExitCode        *int       `gorm:""                                       json:"exit_code"`
	WallTime        *float64   `gorm:"type:decimal(10,6)"                     json:"wall_time"`
	Time            *float64   `gorm:"type:decimal(10,6)"                     json:"time"`
	Memory          *float64   `gorm:"type:decimal(10,3)"                     json:"memory"`
	CPUTimeLimit    float64    `gorm:"type:decimal(10,4);default:5.0"         json:"cpu_time_limit"`
	WallTimeLimit   float64    `gorm:"type:decimal(10,4);default:10.0"        json:"wall_time_limit"`
	MemoryLimit     int64      `gorm:"default:262144"                         json:"memory_limit"`
	StackLimit      int64      `gorm:"default:65536"                          json:"stack_limit"`
	MaxProcesses    int        `gorm:"default:60"                             json:"max_processes"`
	MaxFileSize     int64      `gorm:"default:4096"                           json:"max_file_size"`
	CompilerOptions string     `gorm:"type:varchar(512)"                      json:"compiler_options"`
	CommandLineArgs string     `gorm:"type:varchar(512)"                      json:"command_line_arguments"`
	CallbackURL     string     `gorm:"type:varchar(2048)"                     json:"callback_url"`
	AdditionalFiles string     `gorm:"type:text"                              json:"additional_files"`
	WorkerID        *string    `gorm:"type:varchar(256)"                      json:"worker_id"`
	StartedAt       *time.Time `                                              json:"started_at"`
	FinishedAt      *time.Time `gorm:"index"                                  json:"finished_at"`
	CreatedAt       time.Time  `gorm:"autoCreateTime;index"                   json:"created_at"`
	UpdatedAt       time.Time  `gorm:"autoUpdateTime"                         json:"updated_at"`

	// Associations loaded via Preload.
	Language *Language `gorm:"foreignKey:LanguageID" json:"language,omitempty"`
	Status   *Status   `gorm:"foreignKey:StatusID"   json:"status_obj,omitempty"`
}

// TableName overrides the default GORM table name.
func (s *Submission) TableName() string { return "submissions" }

// BeforeCreate generates a UUID token when one is not already set.
func (s *Submission) BeforeCreate(_ *gorm.DB) error {
	if s.Token == "" {
		// Import-cycle-safe: generate a UUID without importing google/uuid here.
		// Callers that want a pre-set token (e.g. API handlers) set it before Create.
		s.Token = newUUID()
	}
	if s.StatusID == 0 {
		s.StatusID = StatusInQueue
	}
	return nil
}

// IsTerminal returns true when the submission has reached a final state.
func (s *Submission) IsTerminal() bool { return IsTerminalStatus(s.StatusID) }

// ---------------------------------------------------------------------------
// ExecutionLog
// ---------------------------------------------------------------------------

// ExecutionLog stores structured per-worker log lines for a submission.
type ExecutionLog struct {
	ID           uint      `gorm:"primaryKey;autoIncrement"              json:"id"`
	SubmissionID string    `gorm:"type:varchar(36);not null;index"       json:"submission_id"`
	Token        string    `gorm:"type:varchar(36);not null;index"       json:"token"`
	WorkerID     string    `gorm:"type:varchar(256)"                     json:"worker_id"`
	Level        string    `gorm:"type:varchar(20);not null"             json:"level"` // info | warn | error
	Message      string    `gorm:"type:text;not null"                    json:"message"`
	CreatedAt    time.Time `gorm:"autoCreateTime;index"                  json:"created_at"`
}

// TableName overrides the default GORM table name.
func (ExecutionLog) TableName() string { return "execution_logs" }

// ---------------------------------------------------------------------------
// User / APIKey
// ---------------------------------------------------------------------------

// User represents a platform user account.
type User struct {
	ID           uint      `gorm:"primaryKey;autoIncrement"              json:"id"`
	Email        string    `gorm:"type:varchar(255);uniqueIndex;not null" json:"email"`
	PasswordHash string    `gorm:"type:varchar(255);not null"            json:"-"`
	Name         string    `gorm:"type:varchar(255)"                     json:"name"`
	IsActive     bool      `gorm:"default:true"                          json:"is_active"`
	IsAdmin      bool      `gorm:"default:false"                         json:"is_admin"`
	CreatedAt    time.Time `gorm:"autoCreateTime"                        json:"created_at"`
	UpdatedAt    time.Time `gorm:"autoUpdateTime"                        json:"updated_at"`
}

// TableName overrides the default GORM table name.
func (User) TableName() string { return "users" }

// APIKey represents a programmatic access credential.
type APIKey struct {
	ID         uint       `gorm:"primaryKey;autoIncrement"              json:"id"`
	UserID     *uint      `gorm:"index"                                 json:"user_id"`
	User       *User      `gorm:"foreignKey:UserID"                     json:"user,omitempty"`
	Name       string     `gorm:"type:varchar(255);not null"            json:"name"`
	KeyHash    string     `gorm:"type:varchar(255);not null;uniqueIndex" json:"-"`
	Prefix     string     `gorm:"type:varchar(16);not null"             json:"prefix"`
	IsActive   bool       `gorm:"default:true"                          json:"is_active"`
	LastUsedAt *time.Time `                                             json:"last_used_at"`
	ExpiresAt  *time.Time `gorm:"index"                                 json:"expires_at"`
	CreatedAt  time.Time  `gorm:"autoCreateTime"                        json:"created_at"`
	UpdatedAt  time.Time  `gorm:"autoUpdateTime"                        json:"updated_at"`
}

// TableName overrides the default GORM table name.
func (APIKey) TableName() string { return "api_keys" }

// ---------------------------------------------------------------------------
// Status reference type
// ---------------------------------------------------------------------------

// Status is the GORM model for the statuses reference table.
type Status struct {
	ID          int    `gorm:"primaryKey"                         json:"id"`
	Description string `gorm:"type:varchar(128);not null;unique"  json:"description"`
}

// TableName overrides the default GORM table name.
func (Status) TableName() string { return "statuses" }

// ---------------------------------------------------------------------------
// UUID helper (avoids importing google/uuid in model layer)
// ---------------------------------------------------------------------------

// newUUID generates a random UUID v4 string without external dependencies.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant bits
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
