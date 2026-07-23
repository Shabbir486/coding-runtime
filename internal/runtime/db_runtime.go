package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/revature/corems-code-executor/internal/sandbox"
)

// ─── Types ───────────────────────────────────────────────────────────────────

// DBConfig holds connection parameters for a transient database container.
type DBConfig struct {
	RootPassword string // MySQL root password
	DBName       string // database/schema to create
	User         string // additional database user
	Password     string // additional user password
}

// DBExecutionRequest describes a single database execution job.
type DBExecutionRequest struct {
	DBType   string   // "sqlite" | "mysql" | "postgresql" | "mongodb"
	SQL      string   // SQL or JS to execute
	Schema   string   // optional DDL to run before the main query
	DBConfig DBConfig // container DB config
	// Resource limits (forwarded from the parent ExecutionRequest).
	WallTimeLimit float64
	MemoryLimit   int64
}

// DBExecutionResult is the outcome of a database execution.
type DBExecutionResult struct {
	Stdout     string
	Stderr     string
	ExitCode   int
	ExitSignal string
	WallTime   float64
	Status     int
	Error      string
}

// DBRuntime executes SQL / NoSQL workloads inside ephemeral Docker containers.
type DBRuntime struct {
	docker  *sandbox.DockerSandbox
	client  *client.Client
	logger  *zap.Logger
	workDir string // host-visible base directory for temp SQL workdirs
}

// NewDBRuntime constructs a DBRuntime.  workDir must be a path that is
// bind-mounted from the host into the worker container (e.g. /tmp/sandbox)
// so that child containers created by the Docker daemon can mount it.
func NewDBRuntime(docker *sandbox.DockerSandbox, dockerClient *client.Client, workDir string, logger *zap.Logger) *DBRuntime {
	if workDir == "" {
		workDir = "/tmp/sandbox"
	}
	return &DBRuntime{
		docker:  docker,
		client:  dockerClient,
		logger:  logger,
		workDir: workDir,
	}
}

// Execute routes the request to the correct database executor.
func (r *DBRuntime) Execute(ctx context.Context, req *DBExecutionRequest) (*DBExecutionResult, error) {
	switch strings.ToLower(req.DBType) {
	case "sqlite":
		return r.executeSQLite(ctx, req)
	case "mysql":
		return r.executeMySQL(ctx, req)
	case "postgresql", "postgres":
		return r.executePostgreSQL(ctx, req)
	case "mongodb", "mongo":
		return r.executeMongoDB(ctx, req)
	default:
		return nil, fmt.Errorf("db_runtime: unsupported db type %q", req.DBType)
	}
}

// ─── SQLite ───────────────────────────────────────────────────────────────────

func (r *DBRuntime) executeSQLite(ctx context.Context, req *DBExecutionRequest) (*DBExecutionResult, error) {
	script := buildSQLScript(req.Schema, req.SQL)

	execReq := &sandbox.ExecutionRequest{
		Image:         "code-runtime-sqlite:latest",
		SourceFile:    "query.sql",
		SourceCode:    script,
		RunCommand:    []string{"sh", "-c", "sqlite3 /tmp/db.sqlite < /sandbox/query.sql"},
		CPUTimeLimit:  clampWall(req.WallTimeLimit),
		WallTimeLimit: clampWall(req.WallTimeLimit),
		MemoryLimit:   clampMem(req.MemoryLimit),
		MaxProcesses:  16,
		MaxFileSize:   sandbox.DefaultMaxFileSize,
		Env:           []string{"HOME=/sandbox", "TMPDIR=/tmp", "PATH=/usr/local/bin:/usr/bin:/bin"},
	}

	result, err := r.docker.Execute(ctx, execReq)
	if err != nil {
		return nil, err
	}
	return mapSandboxResult(result), nil
}

// ─── MySQL ────────────────────────────────────────────────────────────────────

func (r *DBRuntime) executeMySQL(ctx context.Context, req *DBExecutionRequest) (*DBExecutionResult, error) {
	workDir, cleanup, err := prepareSQLWorkdir(r.workDir, req.Schema, req.SQL)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	// mysql:8.0 runs as root. Ping check is fully silenced (>/dev/null 2>&1)
	// so the "alive" status line doesn't leak into stdout.
	// -N drops column-name headers; tr converts tab separators to pipes.
	const startCmd = "mysqld --initialize-insecure --user=root 2>/dev/null; " +
		"mysqld --user=root --socket=/tmp/mysql.sock --skip-grant-tables " +
		"--skip-log-bin 2>/dev/null & " +
		"until mysqladmin --socket=/tmp/mysql.sock ping --silent >/dev/null 2>&1; " +
		"do sleep 1; done; " +
		"mysql --socket=/tmp/mysql.sock -N < /sandbox/query.sql | tr '\\t' '|'"

	spec := ephemeralDBSpec{
		image:     "code-runtime-mysql:latest",
		name:      "mysql-exec-" + shortID(),
		workDir:   workDir,
		wallLimit: clampWall(req.WallTimeLimit),
		memLimit:  clampMem(req.MemoryLimit),
		env: []string{
			"HOME=/root",
			"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		},
		cmd: []string{"sh", "-c", startCmd},
	}
	return r.runEphemeralDB(ctx, spec)
}

// ─── PostgreSQL ───────────────────────────────────────────────────────────────

func (r *DBRuntime) executePostgreSQL(ctx context.Context, req *DBExecutionRequest) (*DBExecutionResult, error) {
	workDir, cleanup, err := prepareSQLWorkdir(r.workDir, req.Schema, req.SQL)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	// postgres:16-alpine runs as the postgres OS user.
	// initdb and pg_ctl are fully silenced (>/dev/null 2>&1) so startup noise
	// doesn't leak into stdout. -t -A -F'|' gives tuples-only, unaligned,
	// pipe-separated output with no column headers.
	const startCmd = "initdb -D /var/lib/postgresql/data --auth=trust " +
		"--no-locale --encoding=UTF8 >/dev/null 2>&1 && " +
		"pg_ctl -D /var/lib/postgresql/data start -w " +
		"-o \"-c listen_addresses='' -c unix_socket_directories=/tmp\" >/dev/null 2>&1 && " +
		"psql -h /tmp -U postgres -t -A -F '|' -q -f /sandbox/query.sql"

	spec := ephemeralDBSpec{
		image:     "code-runtime-postgresql:latest",
		name:      "pg-exec-" + shortID(),
		workDir:   workDir,
		user:      "postgres",
		wallLimit: clampWall(req.WallTimeLimit),
		memLimit:  clampMem(req.MemoryLimit),
		env: []string{
			"HOME=/var/lib/postgresql",
			"PATH=/usr/local/bin:/usr/bin:/bin",
			"PGDATA=/var/lib/postgresql/data",
		},
		cmd: []string{"sh", "-c", startCmd},
	}
	return r.runEphemeralDB(ctx, spec)
}

// ─── MongoDB ─────────────────────────────────────────────────────────────────
func (r *DBRuntime) executeMongoDB(ctx context.Context, req *DBExecutionRequest) (*DBExecutionResult, error) {
	workDir, cleanup, err := prepareWorkdir(r.workDir, "solution.js", req.SQL)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	// mongosh writes its snapshot/config dirs to $HOME/.mongodb at startup.
	// /sandbox is bind-mounted read-only, so HOME must point at a writable
	// path (the container's tmpfs /tmp) or mongosh will emit two
	// "Could not access file" warnings to STDOUT before running the script.
	spec := ephemeralDBSpec{
		image:     "code-runtime-mongodb:latest",
		name:      "mongo-exec-" + shortID(),
		workDir:   workDir,
		wallLimit: clampWall(req.WallTimeLimit),
		memLimit:  clampMem(req.MemoryLimit),
		env: []string{
			"HOME=/tmp",
			"PATH=/usr/local/bin:/usr/bin:/bin",
		},
		cmd: []string{
			"sh", "-c",
			"until mongosh --nodb --eval 'quit(0)' 2>/dev/null; do sleep 0.5; done && " +
				"mongosh --nodb --quiet --file /sandbox/solution.js",
		},
	}
	return r.runEphemeralDB(ctx, spec)
}

// ─── Generic ephemeral container runner ──────────────────────────────────────

// ephemeralDBSpec groups all parameters needed to spin up a one-shot DB
// container, keeping runEphemeralDB's parameter count manageable.
type ephemeralDBSpec struct {
	image     string
	name      string
	workDir   string
	user      string // container user override (e.g. "postgres")
	cmd       []string
	env       []string
	wallLimit float64
	memLimit  int64
}

// runEphemeralDB starts a container described by spec, waits for it to exit,
// and returns the captured output.
func (r *DBRuntime) runEphemeralDB(ctx context.Context, spec ephemeralDBSpec) (*DBExecutionResult, error) {
	pidsLimit := int64(256)
	hostConfig := &container.HostConfig{
		Resources: container.Resources{
			Memory:     spec.memLimit,
			MemorySwap: spec.memLimit,
			PidsLimit:  &pidsLimit,
		},
		Mounts: []mount.Mount{
			{
				Type:     mount.TypeBind,
				Source:   spec.workDir,
				Target:   "/sandbox",
				ReadOnly: true,
			},
		},
		NetworkMode: "none",
		AutoRemove:  false,
	}

	containerCfg := &container.Config{
		Image:           spec.image,
		Cmd:             spec.cmd,
		Env:             spec.env,
		User:            spec.user,
		WorkingDir:      "/sandbox",
		NetworkDisabled: true,
		AttachStdout:    true,
		AttachStderr:    true,
	}

	deadline := time.Duration(spec.wallLimit*float64(time.Second)) + 5*time.Second
	runCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	// Ensure the runtime image is present locally before creating the container.
	// Unlike the sandbox path, runEphemeralDB calls ContainerCreate directly, so
	// it must trigger the same registry-aware pull+retag (e.g. pull from ECR and
	// tag to the bare code-runtime-<db>:latest name) that the pool performs.
	if err := r.docker.ImagePool().EnsureImage(runCtx, spec.image); err != nil {
		return nil, fmt.Errorf("db_runtime: ensure image %q: %w", spec.image, err)
	}

	created, err := r.client.ContainerCreate(runCtx, containerCfg, hostConfig, nil, nil, spec.name)
	if err != nil {
		return nil, fmt.Errorf("db_runtime: create container: %w", err)
	}
	containerID := created.ID
	defer r.removeContainer(context.Background(), containerID)

	attachResp, err := r.client.ContainerAttach(runCtx, containerID, container.AttachOptions{
		Stream: true,
		Stdout: true,
		Stderr: true,
	})
	if err != nil {
		return nil, fmt.Errorf("db_runtime: attach container: %w", err)
	}
	defer attachResp.Close()

	wallStart := time.Now()
	if err := r.client.ContainerStart(runCtx, containerID, container.StartOptions{}); err != nil {
		return nil, fmt.Errorf("db_runtime: start container: %w", err)
	}

	var stdoutBuf, stderrBuf sandbox.LimitedWriter
	stdoutBuf.SetLimit(sandbox.MaxOutputSize)
	stderrBuf.SetLimit(sandbox.MaxOutputSize)

	streamDone := make(chan error, 1)
	go func() {
		_, copyErr := sandbox.StdCopyStreams(&stdoutBuf, &stderrBuf, attachResp.Reader)
		streamDone <- copyErr
	}()

	statusCh, errCh := r.client.ContainerWait(runCtx, containerID, container.WaitConditionNotRunning)

	var exitCode int
	var exitSignal string

	select {
	case status := <-statusCh:
		if status.Error != nil {
			exitSignal = status.Error.Message
		}
		exitCode = int(status.StatusCode)

	case waitErr := <-errCh:
		wallElapsed := time.Since(wallStart).Seconds()
		<-streamDone
		if runCtx.Err() != nil {
			return dbTLEResult(&stdoutBuf, &stderrBuf, wallElapsed), nil
		}
		return nil, fmt.Errorf("db_runtime: container wait: %w", waitErr)

	case <-runCtx.Done():
		wallElapsed := time.Since(wallStart).Seconds()
		killCtx, killCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = r.client.ContainerKill(killCtx, containerID, "SIGKILL")
		killCancel()
		<-streamDone
		return dbTLEResult(&stdoutBuf, &stderrBuf, wallElapsed), nil
	}

	<-streamDone
	wallElapsed := time.Since(wallStart).Seconds()

	status := sandbox.StatusAccepted
	if exitCode != 0 {
		status = sandbox.StatusRuntimeError
	}

	return &DBExecutionResult{
		Stdout:     stdoutBuf.String(),
		Stderr:     stderrBuf.String(),
		ExitCode:   exitCode,
		ExitSignal: exitSignal,
		WallTime:   wallElapsed,
		Status:     status,
	}, nil
}

func (r *DBRuntime) removeContainer(ctx context.Context, id string) {
	removeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_ = r.client.ContainerRemove(removeCtx, id, container.RemoveOptions{
		RemoveVolumes: true,
		Force:         true,
	})
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func dbTLEResult(out, errOut *sandbox.LimitedWriter, wallTime float64) *DBExecutionResult {
	return &DBExecutionResult{
		Stdout:     out.String(),
		Stderr:     errOut.String(),
		ExitCode:   -1,
		ExitSignal: "SIGKILL",
		WallTime:   wallTime,
		Status:     sandbox.StatusTimeLimitExceeded,
	}
}

func buildSQLScript(schema, query string) string {
	if schema == "" {
		return query
	}
	return schema + "\n\n" + query
}

// prepareSQLWorkdir creates a temp directory with query.sql written inside.
// Returns the directory path and a cleanup func.
func prepareSQLWorkdir(baseDir, schema, sql string) (dir string, cleanup func(), err error) {
	dir, cleanup, err = prepareWorkdir(baseDir, "query.sql", buildSQLScript(schema, sql))
	return
}

// prepareWorkdir creates a temp dir under baseDir containing a single file.
// baseDir must be a path that is bind-mounted from the Docker host (e.g.
// /tmp/sandbox) so that the Docker daemon can locate it when creating bind
// mounts for ephemeral containers.  Both the directory (0755) and the file
// (0644) are world-readable so non-root container users (e.g. postgres) can
// read them.
func prepareWorkdir(baseDir, filename, content string) (dir string, cleanup func(), err error) {
	if err = os.MkdirAll(baseDir, 0o755); err != nil {
		return "", nil, fmt.Errorf("db_runtime: ensure basedir: %w", err)
	}
	dir, err = os.MkdirTemp(baseDir, "db-sandbox-*")
	if err != nil {
		return "", nil, fmt.Errorf("db_runtime: create tempdir: %w", err)
	}
	cleanup = func() { os.RemoveAll(dir) }

	if err = os.Chmod(dir, 0o755); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("db_runtime: chmod tempdir: %w", err)
	}

	path := filepath.Join(dir, filename)
	if err = os.WriteFile(path, []byte(content), 0o644); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("db_runtime: write script: %w", err)
	}
	return dir, cleanup, nil
}

func mapSandboxResult(res *sandbox.ExecutionResult) *DBExecutionResult {
	return &DBExecutionResult{
		Stdout:     res.Stdout,
		Stderr:     res.Stderr,
		ExitCode:   res.ExitCode,
		ExitSignal: res.ExitSignal,
		WallTime:   res.WallTime,
		Status:     res.Status,
		Error:      res.Error,
	}
}

func clampWall(v float64) float64 {
	if v <= 0 {
		return sandbox.DefaultWallTimeLimit
	}
	if v > sandbox.MaxWallTimeLimit {
		return sandbox.MaxWallTimeLimit
	}
	return v
}

func clampMem(v int64) int64 {
	if v <= 0 {
		return sandbox.DefaultMemoryLimit
	}
	if v > sandbox.MaxMemoryLimit {
		return sandbox.MaxMemoryLimit
	}
	return v
}

func shortID() string {
	return uuid.New().String()[:8]
}

