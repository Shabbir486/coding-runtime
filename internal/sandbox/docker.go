package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/mdshabbir-ali/code-runtime/internal/metrics"
)

// ─── Types ───────────────────────────────────────────────────────────────────

// SandboxConfig holds host-level sandbox settings that apply to every
// execution request.
type SandboxConfig struct {
	WorkDir         string // base directory for per-execution tmp dirs
	CPUPeriod       int64  // Docker CFS period (µs), default 100 000
	MemoryLimit     int64  // default memory limit (bytes)
	PidsLimit       int64  // default pid limit
	NetworkDisabled bool   // disable container networking
	ReadOnly        bool   // read-only root filesystem
	TmpfsSize       int64  // tmpfs size for /sandbox (bytes)
	SeccompProfile  string // inline JSON seccomp profile
	Registry        string // optional registry prefix for on-demand image pulls
	RegistryAuth    string // optional base64 X-Registry-Auth for a private registry
}

// ExecutionRequest describes a single code-execution job.
type ExecutionRequest struct {
	Image          string
	WorkDir        string
	SourceFile     string
	SourceCode     string
	Stdin          string
	CompileCommand []string // nil for interpreted languages
	RunCommand     []string
	Entrypoint     []string // nil = use image default; []string{} = clear entrypoint
	CPUTimeLimit   float64  // seconds
	WallTimeLimit  float64  // seconds
	MemoryLimit    int64    // bytes
	StackLimit     int64    // bytes
	MaxProcesses   int
	MaxFileSize    int64 // bytes
	Env            []string
}

// ExecutionResult is the output of a completed (or failed) execution.
type ExecutionResult struct {
	Stdout        string
	Stderr        string
	CompileOutput string
	ExitCode      int
	ExitSignal    string
	WallTime      float64
	CPUTime       float64
	MemoryUsed    int64
	Status        int
	Error         string
}

// DockerSandbox executes code inside isolated Docker containers.
type DockerSandbox struct {
	client    *client.Client
	cfg       SandboxConfig
	logger    *zap.Logger
	metrics   *metrics.Metrics
	imagePool *ImagePool
}

// ─── Constructor ─────────────────────────────────────────────────────────────

// NewDockerSandbox creates a DockerSandbox and verifies the Docker daemon is
// reachable.
func NewDockerSandbox(cfg SandboxConfig, logger *zap.Logger, m *metrics.Metrics) (*DockerSandbox, error) {
	if cfg.WorkDir == "" {
		cfg.WorkDir = "/tmp/sandbox"
	}
	if cfg.CPUPeriod == 0 {
		cfg.CPUPeriod = 100_000 // 100 ms
	}
	if cfg.MemoryLimit == 0 {
		cfg.MemoryLimit = DefaultMemoryLimit
	}
	if cfg.PidsLimit == 0 {
		cfg.PidsLimit = DefaultPidsLimit
	}
	if cfg.TmpfsSize == 0 {
		cfg.TmpfsSize = 64 * 1024 * 1024 // 64 MB
	}
	// Leave cfg.SeccompProfile empty to use Docker's built-in default seccomp,
	// which blocks dangerous syscalls while allowing everything needed by common
	// runtimes (Node.js, JVM, etc.).  Set it explicitly to use a custom profile.

	dockerClient, err := client.NewClientWithOpts(
		client.FromEnv,
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, fmt.Errorf("sandbox: create docker client: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := dockerClient.Ping(ctx); err != nil {
		return nil, fmt.Errorf("sandbox: docker daemon unreachable: %w", err)
	}

	pool := NewImagePool(dockerClient, cfg.Registry, cfg.RegistryAuth, logger)

	return &DockerSandbox{
		client:    dockerClient,
		cfg:       cfg,
		logger:    logger,
		metrics:   m,
		imagePool: pool,
	}, nil
}

// Close releases the Docker client.
func (s *DockerSandbox) Close() error {
	return s.client.Close()
}

// ImagePool exposes the image pool for pre-warming.
func (s *DockerSandbox) ImagePool() *ImagePool {
	return s.imagePool
}

// Client exposes the underlying Docker client for callers that need direct
// container management (e.g. DBRuntime ephemeral containers).
func (s *DockerSandbox) Client() *client.Client {
	return s.client
}

// ─── Execute ─────────────────────────────────────────────────────────────────

// Execute runs req inside a secured Docker container and returns the result.
func (s *DockerSandbox) Execute(ctx context.Context, req *ExecutionRequest) (*ExecutionResult, error) {
	req = ApplyDefaults(req)
	if err := ValidateConstraints(req); err != nil {
		return nil, err
	}

	// 1. Create an isolated working directory.
	execID := uuid.New().String()
	workDir := filepath.Join(s.cfg.WorkDir, execID)
	if err := os.MkdirAll(workDir, 0o777); err != nil {
		return nil, fmt.Errorf("sandbox: create workdir: %w", err)
	}
	// The container process runs as a non-root user that varies per image
	// (uid 10001, 1000, …). Compiled languages write build artifacts into
	// /sandbox (e.g. Main.class, a.out), so the bind-mounted dir must be
	// writable by an arbitrary uid. os.MkdirAll is subject to umask, so set
	// the mode explicitly. The dir is per-execution, network-isolated, and
	// removed immediately after, so 0o777 is safe here.
	if err := os.Chmod(workDir, 0o777); err != nil {
		return nil, fmt.Errorf("sandbox: chmod workdir: %w", err)
	}
	defer os.RemoveAll(workDir) // always cleanup

	// 2. Write source code.
	sourceFile := filepath.Join(workDir, req.SourceFile)
	if err := os.WriteFile(sourceFile, []byte(req.SourceCode), 0o644); err != nil {
		return nil, fmt.Errorf("sandbox: write source: %w", err)
	}

	// 3. Write stdin to a file (container reads it via redirect).
	stdinFile := filepath.Join(workDir, "stdin.txt")
	if err := os.WriteFile(stdinFile, []byte(req.Stdin), 0o644); err != nil {
		return nil, fmt.Errorf("sandbox: write stdin: %w", err)
	}

	// 4. Ensure the image is available locally.
	if err := s.imagePool.EnsureImage(ctx, req.Image); err != nil {
		return nil, fmt.Errorf("sandbox: ensure image %q: %w", req.Image, err)
	}

	result := &ExecutionResult{}

	// 5. Compilation step (compiled languages only).
	s.logger.Info("sandbox execute", zap.Strings("compile_cmd", req.CompileCommand), zap.Strings("run_cmd", req.RunCommand))
	if len(req.CompileCommand) > 0 {
		compileOut, err := s.runContainer(ctx, req, workDir, req.CompileCommand, "", true)
		if err != nil {
			return nil, fmt.Errorf("sandbox: start compile container: %w", err)
		}
		result.CompileOutput = truncate(compileOut.combined, MaxOutputSize)
		s.logger.Info("compile step complete",
			zap.Int("exit_code", compileOut.exitCode),
			zap.String("combined", compileOut.combined),
			zap.String("workdir", workDir),
		)

		if compileOut.exitCode != 0 {
			result.Status = StatusCompilationError
			result.ExitCode = compileOut.exitCode
			return result, nil
		}
	}

	// 6. Execute the program.
	wallStart := time.Now()
	execOut, err := s.runContainer(ctx, req, workDir, req.RunCommand, req.Stdin, false)
	wallElapsed := time.Since(wallStart).Seconds()

	if err != nil {
		// runContainer returns a typed error for timeout / OOM.
		if execOut != nil {
			result.Status = execOut.status
			result.ExitCode = execOut.exitCode
			result.ExitSignal = execOut.exitSignal
			result.WallTime = wallElapsed
			result.Stdout = truncate(execOut.stdout, MaxOutputSize)
			result.Stderr = truncate(execOut.stderr, MaxOutputSize)
			result.MemoryUsed = execOut.memoryUsed
		} else {
			s.logger.Error("container run failed", zap.Error(err), zap.String("image", req.Image))
			result.Status = StatusInternalError
			result.Error = err.Error()
		}
		return result, nil
	}

	// 7. Populate result.
	result.Stdout = truncate(execOut.stdout, MaxOutputSize)
	result.Stderr = truncate(execOut.stderr, MaxOutputSize)
	result.ExitCode = execOut.exitCode
	result.ExitSignal = execOut.exitSignal
	result.WallTime = wallElapsed
	result.CPUTime = execOut.cpuTime
	result.MemoryUsed = execOut.memoryUsed
	result.Status = execOut.status

	return result, nil
}

// ─── Status constants (mirrors models.StatusXxx) ─────────────────────────────

const (
	StatusInQueue           = 1
	StatusProcessing        = 2
	StatusAccepted          = 3
	StatusWrongAnswer       = 4
	StatusTimeLimitExceeded = 5
	StatusCompilationError  = 6
	StatusRuntimeError      = 7
	StatusInternalError     = 8
)

// ─── Internal container runner ───────────────────────────────────────────────

type containerOutput struct {
	stdout     string
	stderr     string
	combined   string // used for compile errors
	exitCode   int
	exitSignal string
	cpuTime    float64
	memoryUsed int64
	status     int
}

// buildSecurityOpts returns the Docker security options for the container.
// When no custom seccomp profile is configured, only "no-new-privileges" is
// set and Docker uses its built-in default seccomp policy.
func (s *DockerSandbox) buildSecurityOpts() []string {
	opts := []string{"no-new-privileges"}
	if s.cfg.SeccompProfile != "" {
		opts = append(opts, "seccomp="+s.cfg.SeccompProfile)
	}
	return opts
}

// runContainer creates, starts, waits for and removes a Docker container.
// It enforces the wall-time limit via a deadline context and captures stdout
// and stderr with a size limit.
func (s *DockerSandbox) runContainer(
	ctx context.Context,
	req *ExecutionRequest,
	workDir string,
	cmd []string,
	stdin string,
	isCompile bool,
) (*containerOutput, error) {

	// Build the host configuration with resource constraints.
	pidsLimit := int64(req.MaxProcesses)
	if pidsLimit <= 0 {
		pidsLimit = s.cfg.PidsLimit
	}
	memLimit := req.MemoryLimit
	if memLimit <= 0 {
		memLimit = s.cfg.MemoryLimit
	}
	cpuQuota := CPUTimeToCPUQuota(req.CPUTimeLimit, s.cfg.CPUPeriod)

	tmpMountOpts := "rw,noexec,nosuid,size=33554432" // 32 MB

	hostConfig := &container.HostConfig{
		Resources: container.Resources{
			Memory:     memLimit,
			MemorySwap: memLimit, // disable swap
			CPUPeriod:  s.cfg.CPUPeriod,
			CPUQuota:   cpuQuota,
			PidsLimit:  &pidsLimit,
		},
		NetworkMode:    "none",
		ReadonlyRootfs: s.cfg.ReadOnly,
		Tmpfs: map[string]string{
			// /sandbox is provided by the workdir bind mount below; only /tmp needs tmpfs.
			"/tmp": tmpMountOpts,
		},
		SecurityOpt: s.buildSecurityOpts(),
		CapDrop: []string{"ALL"},
		Mounts: []mount.Mount{
			{
				Type:     mount.TypeBind,
				Source:   workDir,
				Target:   "/sandbox",
				ReadOnly: false, // writable so compile can write output binary
			},
		},
		AutoRemove: false, // we remove manually to capture stats
	}

	containerCfg := &container.Config{
		Image:           req.Image,
		Cmd:             cmd,
		Entrypoint:      req.Entrypoint, // nil = image default; []string{} = override/clear
		Env:             req.Env,
		WorkingDir:      "/sandbox",
		NetworkDisabled: true,
		AttachStdout:    true,
		AttachStderr:    true,
		Tty:             false,
		OpenStdin:       stdin != "",
		StdinOnce:       stdin != "",
	}

	// Create context with wall-time deadline.
	wallLimit := req.WallTimeLimit
	if isCompile {
		wallLimit = req.WallTimeLimit * 2 // give compilation more time
	}
	deadline := time.Duration(wallLimit * float64(time.Second))
	runCtx, cancel := context.WithTimeout(ctx, deadline+2*time.Second) // +2 s buffer
	defer cancel()

	// Create container.
	created, err := s.client.ContainerCreate(runCtx, containerCfg, hostConfig, nil, nil, "")
	if err != nil {
		return nil, fmt.Errorf("container create: %w", err)
	}
	containerID := created.ID
	defer s.removeContainer(context.Background(), containerID)

	// Attach to container streams before starting.
	attachResp, err := s.client.ContainerAttach(runCtx, containerID, container.AttachOptions{
		Stream: true,
		Stdout: true,
		Stderr: true,
		Stdin:  stdin != "",
	})
	if err != nil {
		return nil, fmt.Errorf("container attach: %w", err)
	}
	defer attachResp.Close()

	// Start container.
	wallStart := time.Now()
	if err := s.client.ContainerStart(runCtx, containerID, container.StartOptions{}); err != nil {
		return nil, fmt.Errorf("container start: %w", err)
	}

	var maxMem atomic.Int64
	var totalCPU atomic.Int64

	statsResp, statsErr := s.client.ContainerStats(runCtx, containerID, true)
	if statsErr == nil {
		go func() {
			defer statsResp.Body.Close()
			decoder := json.NewDecoder(statsResp.Body)
			for {
				var v struct {
					MemoryStats struct {
						MaxUsage int64 `json:"max_usage"`
					} `json:"memory_stats"`
					CPUStats struct {
						CPUUsage struct {
							TotalUsage int64 `json:"total_usage"`
						} `json:"cpu_usage"`
					} `json:"cpu_stats"`
				}
				if err := decoder.Decode(&v); err != nil {
					break
				}
				if v.MemoryStats.MaxUsage > maxMem.Load() {
					maxMem.Store(v.MemoryStats.MaxUsage)
				}
				if v.CPUStats.CPUUsage.TotalUsage > totalCPU.Load() {
					totalCPU.Store(v.CPUStats.CPUUsage.TotalUsage)
				}
			}
		}()
	}

	// Write stdin if provided (non-blocking; goroutine writes and closes).
	if stdin != "" {
		go func() {
			_, _ = io.Copy(attachResp.Conn, strings.NewReader(stdin))
			_ = attachResp.CloseWrite()
		}()
	}

	// Stream stdout/stderr with size caps.
	var stdoutBuf, stderrBuf LimitedWriter
	stdoutBuf.SetLimit(MaxOutputSize)
	stderrBuf.SetLimit(MaxOutputSize)

	// Docker multiplexes stdout and stderr over the same stream when not using
	// a TTY.  Use StdCopy to demultiplex.
	streamDone := make(chan error, 1)
	go func() {
		_, err := stdCopy(&stdoutBuf, &stderrBuf, attachResp.Reader)
		streamDone <- err
	}()

	// Wait for container to exit.
	statusCh, errCh := s.client.ContainerWait(runCtx, containerID, container.WaitConditionNotRunning)

	var exitCode int
	var exitSignal string
	var timedOut bool

	select {
	case status := <-statusCh:
		if status.Error != nil {
			exitSignal = status.Error.Message
		}
		exitCode = int(status.StatusCode)
	case err := <-errCh:
		if runCtx.Err() != nil {
			timedOut = true
		} else {
			return nil, fmt.Errorf("container wait: %w", err)
		}
	case <-runCtx.Done():
		timedOut = true
	}

	if timedOut {
		// Kill the container immediately.
		killCtx, killCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = s.client.ContainerKill(killCtx, containerID, "SIGKILL")
		killCancel()

		wallElapsed := time.Since(wallStart).Seconds()
		<-streamDone
		return &containerOutput{
			stdout:     truncate(stdoutBuf.String(), MaxOutputSize),
			stderr:     truncate(stderrBuf.String(), MaxOutputSize),
			exitCode:   -1,
			exitSignal: "SIGKILL",
			status:     StatusTimeLimitExceeded,
			cpuTime:    wallElapsed,
		}, fmt.Errorf("wall time limit exceeded (%.1fs)", wallElapsed)
	}

	<-streamDone

	// Inspect to get memory stats.
	var memUsed int64
	inspect, inspectErr := s.client.ContainerInspect(context.Background(), containerID)
	if inspectErr == nil && inspect.State != nil {
		// OOMKilled is reported in inspect.
		if inspect.State.OOMKilled {
			return &containerOutput{
				stdout:     truncate(stdoutBuf.String(), MaxOutputSize),
				stderr:     truncate(stderrBuf.String(), MaxOutputSize),
				exitCode:   exitCode,
				exitSignal: "SIGKILL",
				status:     StatusRuntimeError,
			}, fmt.Errorf("container OOM killed")
		}
	}

	wallElapsed := time.Since(wallStart).Seconds()

	// Map exit code to status.
	status := mapExitCode(exitCode, exitSignal, wallElapsed, req.WallTimeLimit)

	finalMem := maxMem.Load()
	if finalMem == 0 {
		finalMem = memUsed // fallback to inspect or 0
	}

	finalCPU := float64(totalCPU.Load()) / 1e9 // nano to seconds
	if finalCPU <= 0 {
		finalCPU = wallElapsed // fallback
	}

	out := &containerOutput{
		stdout:     stdoutBuf.String(),
		stderr:     stderrBuf.String(),
		combined:   stdoutBuf.String() + stderrBuf.String(),
		exitCode:   exitCode,
		exitSignal: exitSignal,
		cpuTime:    finalCPU,
		memoryUsed: finalMem,
		status:     status,
	}

	return out, nil
}

// mapExitCode translates an exit code / signal into a status constant.
func mapExitCode(exitCode int, signal string, wallTime, wallLimit float64) int {
	if wallTime >= wallLimit {
		return StatusTimeLimitExceeded
	}
	if exitCode == 0 {
		return StatusAccepted
	}
	if signal != "" && (signal == "SIGXCPU" || strings.Contains(signal, "XCPU")) {
		return StatusTimeLimitExceeded
	}
	return StatusRuntimeError
}

// removeContainer kills and removes a container, ignoring errors (best effort).
func (s *DockerSandbox) removeContainer(ctx context.Context, id string) {
	removeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_ = s.client.ContainerRemove(removeCtx, id, container.RemoveOptions{
		RemoveVolumes: true,
		Force:         true,
	})
}

// ─── truncate helper ─────────────────────────────────────────────────────────

func truncate(s string, maxBytes int64) string {
	if int64(len(s)) <= maxBytes {
		return s
	}
	return s[:maxBytes]
}

// ─── stdCopy ─────────────────────────────────────────────────────────────────
// stdCopy is a local re-export of stdcopy.StdCopy so the caller does not need
// to import the stdcopy package directly.  Docker multiplexes stdout/stderr
// with an 8-byte header; StdCopy demultiplexes them.

func stdCopy(dst, dstErr io.Writer, src io.Reader) (int64, error) {
	// 8-byte stream header: [stream_type(1), pad(3), size(4)]
	const hdrSize = 8
	hdr := make([]byte, hdrSize)
	var written int64

	for {
		_, err := io.ReadFull(src, hdr)
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return written, nil
			}
			return written, err
		}

		streamType := hdr[0]
		// Bytes 1-3 are padding; bytes 4-7 are big-endian uint32 frame size.
		frameSize := int64(hdr[4])<<24 | int64(hdr[5])<<16 | int64(hdr[6])<<8 | int64(hdr[7])

		var w io.Writer
		switch streamType {
		case 1:
			w = dst
		case 2:
			w = dstErr
		default:
			w = io.Discard
		}

		n, err := io.CopyN(w, src, frameSize)
		written += n
		if err != nil {
			if err == io.EOF {
				return written, nil
			}
			return written, err
		}
	}
}
