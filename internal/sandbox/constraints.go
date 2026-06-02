package sandbox

import (
	"errors"
	"fmt"
)

// Hard resource limits enforced by the sandbox.
const (
	DefaultCPUTimeLimit  = 5.0   // seconds
	DefaultWallTimeLimit = 10.0  // seconds
	DefaultMemoryLimit   = int64(256 * 1024 * 1024) // 256 MB
	DefaultPidsLimit     = int64(64)
	DefaultMaxFileSize   = int64(64 * 1024 * 1024) // 64 MB
	DefaultStackLimit    = int64(64 * 1024 * 1024) // 64 MB

	MaxOutputSize    = int64(1024 * 1024)           // 1 MB per stream
	MaxCPUTimeLimit  = 120.0                        // seconds (JVM languages need ~60 s)
	MaxWallTimeLimit = 360.0                        // seconds (3× CPU budget for slow JVMs)
	MaxMemoryLimit   = int64(4 * 1024 * 1024 * 1024) // 4 GB (Kotlin/Scala JVM needs ~2 GB)
)

// ApplyDefaults fills in zero-value resource limits with safe defaults.
// It returns a copy of req with defaults applied so the caller's original is
// never mutated unexpectedly.
func ApplyDefaults(req *ExecutionRequest) *ExecutionRequest {
	out := *req // shallow copy

	if out.CPUTimeLimit <= 0 {
		out.CPUTimeLimit = DefaultCPUTimeLimit
	}
	if out.WallTimeLimit <= 0 {
		out.WallTimeLimit = DefaultWallTimeLimit
	}
	if out.MemoryLimit <= 0 {
		out.MemoryLimit = DefaultMemoryLimit
	}
	if out.StackLimit <= 0 {
		out.StackLimit = DefaultStackLimit
	}
	if out.MaxProcesses <= 0 {
		out.MaxProcesses = int(DefaultPidsLimit)
	}
	if out.MaxFileSize <= 0 {
		out.MaxFileSize = DefaultMaxFileSize
	}

	return &out
}

// ValidateConstraints returns an error if any resource limit in req exceeds the
// hard maximums or is otherwise invalid.
func ValidateConstraints(req *ExecutionRequest) error {
	var errs []error

	if req.CPUTimeLimit < 0 {
		errs = append(errs, errors.New("cpu_time_limit must be >= 0"))
	}
	if req.CPUTimeLimit > MaxCPUTimeLimit {
		errs = append(errs, fmt.Errorf("cpu_time_limit %.1f exceeds maximum %.1f seconds", req.CPUTimeLimit, MaxCPUTimeLimit))
	}

	if req.WallTimeLimit < 0 {
		errs = append(errs, errors.New("wall_time_limit must be >= 0"))
	}
	if req.WallTimeLimit > MaxWallTimeLimit {
		errs = append(errs, fmt.Errorf("wall_time_limit %.1f exceeds maximum %.1f seconds", req.WallTimeLimit, MaxWallTimeLimit))
	}

	if req.MemoryLimit < 0 {
		errs = append(errs, errors.New("memory_limit must be >= 0"))
	}
	if req.MemoryLimit > MaxMemoryLimit {
		errs = append(errs, fmt.Errorf("memory_limit %d exceeds maximum %d bytes", req.MemoryLimit, MaxMemoryLimit))
	}

	if req.Image == "" {
		errs = append(errs, errors.New("image must not be empty"))
	}

	if len(errs) == 0 {
		return nil
	}

	msg := "constraint violations:"
	for _, e := range errs {
		msg += " [" + e.Error() + "]"
	}
	return errors.New(msg)
}

// CPUTimeToCPUQuota converts a CPU time limit (in fractional seconds) to a
// Docker CFS CPU quota value.
//
//	quota = cpuTime * (cpuPeriod / 1 second)
//
// For example, with cpuPeriod = 100_000 µs (100 ms) and cpuTime = 2.5 s:
//
//	quota = 2.5 * 100_000 = 250_000 µs  →  2.5 CPU-seconds of quota per period
//
// This gives the container up to 2.5× a single CPU worth of compute inside
// the window.  Clamped to [1, cpuPeriod * 1024] so the value is always valid.
func CPUTimeToCPUQuota(cpuTime float64, cpuPeriod int64) int64 {
	if cpuTime <= 0 || cpuPeriod <= 0 {
		return int64(float64(100_000) * DefaultCPUTimeLimit) // safe default
	}
	quota := int64(cpuTime * float64(cpuPeriod))
	if quota < 1 {
		quota = 1
	}
	maxQuota := cpuPeriod * 1024
	if quota > maxQuota {
		quota = maxQuota
	}
	return quota
}
