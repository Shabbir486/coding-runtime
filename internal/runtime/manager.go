package runtime

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"go.uber.org/zap"

	"github.com/mdshabbir-ali/code-runtime/internal/cache"
	"github.com/mdshabbir-ali/code-runtime/internal/database"
	"github.com/mdshabbir-ali/code-runtime/internal/models"
	"github.com/mdshabbir-ali/code-runtime/internal/sandbox"
)

// Manager resolves language metadata and translates submission records into
// fully-formed ExecutionRequests.
type Manager struct {
	mu        sync.RWMutex
	languages map[int]*models.Language // in-memory cache
	db        database.LanguageRepository
	langCache *cache.LanguageCache
	logger    *zap.Logger
}

// NewManager constructs a Manager.  languages can be nil; they will be
// loaded on the first call to GetLanguage.
func NewManager(db database.LanguageRepository, langCache *cache.LanguageCache, logger *zap.Logger) *Manager {
	return &Manager{
		languages: make(map[int]*models.Language),
		db:        db,
		langCache: langCache,
		logger:    logger,
	}
}

// ─── Language lookup ─────────────────────────────────────────────────────────

// GetLanguage returns a language by ID.  It checks the in-memory map first,
// then Redis, then the database.
func (m *Manager) GetLanguage(ctx context.Context, id int) (*models.Language, error) {
	// 1. In-memory.
	m.mu.RLock()
	if lang, ok := m.languages[id]; ok {
		m.mu.RUnlock()
		return lang, nil
	}
	m.mu.RUnlock()

	// 2. Redis.
	if m.langCache != nil {
		lang, err := m.langCache.GetByID(ctx, id)
		if err != nil {
			m.logger.Warn("language cache get failed", zap.Int("id", id), zap.Error(err))
		} else if lang != nil {
			m.store(lang)
			return lang, nil
		}
	}

	// 3. Database.
	lang, err := m.db.GetByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("runtime: language %d not found: %w", id, err)
	}

	// Populate caches.
	m.store(lang)
	if m.langCache != nil {
		if err := m.langCache.Set(ctx, lang); err != nil {
			m.logger.Warn("language cache set failed", zap.Int("id", id), zap.Error(err))
		}
	}
	return lang, nil
}

// GetAllLanguages returns all active languages, using cached data when possible.
func (m *Manager) GetAllLanguages(ctx context.Context) ([]*models.Language, error) {
	// Try Redis first.
	if m.langCache != nil {
		cached, err := m.langCache.GetAll(ctx)
		if err == nil && len(cached) > 0 {
			for _, l := range cached {
				m.store(l)
			}
			return cached, nil
		}
	}

	// Fall back to database.
	langs, err := m.db.GetAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("runtime: list languages: %w", err)
	}

	for _, l := range langs {
		m.store(l)
	}
	result := langs

	// Populate Redis cache.
	if m.langCache != nil {
		if err := m.langCache.SetAll(ctx, langs); err != nil {
			m.logger.Warn("language cache set-all failed", zap.Error(err))
		}
	}

	return result, nil
}

// RefreshCache drops the in-memory language map and re-populates it directly
// from the database, then writes the fresh data back to Redis.  Bypassing Redis
// on read prevents a stale cache (e.g. from a previous deployment) from masking
// DB changes made by SeedLanguages on the API side.
func (m *Manager) RefreshCache(ctx context.Context) error {
	m.mu.Lock()
	m.languages = make(map[int]*models.Language)
	m.mu.Unlock()

	langs, err := m.db.GetAll(ctx)
	if err != nil {
		return fmt.Errorf("runtime: refresh cache: %w", err)
	}
	for _, l := range langs {
		m.store(l)
	}
	if m.langCache != nil {
		if err := m.langCache.SetAll(ctx, langs); err != nil {
			m.logger.Warn("language cache set-all failed during refresh", zap.Error(err))
		}
	}
	return nil
}

// IsDatabase returns true if the language identified by id is a database
// runtime (SQLite, MySQL, PostgreSQL, MongoDB).
func (m *Manager) IsDatabase(id int) bool {
	m.mu.RLock()
	lang, ok := m.languages[id]
	m.mu.RUnlock()
	if !ok {
		return false
	}
	return lang.IsDatabase
}

// IsCompiled returns true if the language identified by id requires a
// compilation step before execution.
func (m *Manager) IsCompiled(id int) bool {
	m.mu.RLock()
	lang, ok := m.languages[id]
	m.mu.RUnlock()
	if !ok {
		return false
	}
	return lang.CompileCommand != nil && *lang.CompileCommand != ""
}

// ─── ExecutionRequest builder ─────────────────────────────────────────────────

// BuildExecutionRequest translates a Submission + Language into an
// ExecutionRequest suitable for sandbox.DockerSandbox.Execute.
func (m *Manager) BuildExecutionRequest(sub *models.Submission, lang *models.Language) *sandbox.ExecutionRequest {
	sourceFile := lang.SourceFile
	if sourceFile == "" {
		sourceFile = "solution.txt"
	}

	// Prefix source file with /sandbox if it does not already start with it.
	sandboxSource := sandboxPath(sourceFile)

	stdin := ""
	if sub.Stdin != nil {
		stdin = *sub.Stdin
	}

	req := &sandbox.ExecutionRequest{
		Image:         lang.Image,
		WorkDir:       "/sandbox",
		SourceFile:    sourceFile,
		SourceCode:    sub.SourceCode,
		Stdin:         stdin,
		CPUTimeLimit:  sub.CPUTimeLimit,
		WallTimeLimit: sub.WallTimeLimit,
		MemoryLimit:   sub.MemoryLimit * 1024, // submission stores KB, sandbox wants bytes
		StackLimit:    sub.StackLimit * 1024,  // submission stores KB, sandbox wants bytes
		MaxProcesses:  sub.MaxProcesses,
		MaxFileSize:   sub.MaxFileSize * 1024, // submission stores KB, sandbox wants bytes
		Env:           buildEnv(lang),
	}

	// Enforce language-level resource minimums.  ExecutionJob.Validate() fills in
	// generic defaults (e.g. 256 MB RAM, 10 s CPU) before the language is known.
	// Languages like Kotlin/Scala need much more; treat the language values as
	// lower bounds so those defaults never starve a compiler.
	langCPU := lang.MaxCPUTime
	langWall := lang.MaxCPUTime * 3
	langMem := lang.MaxMemory * 1024 // lang stores KB, sandbox wants bytes

	if req.CPUTimeLimit <= 0 || req.CPUTimeLimit < langCPU {
		req.CPUTimeLimit = langCPU
	}
	if req.WallTimeLimit <= 0 || req.WallTimeLimit < langWall {
		req.WallTimeLimit = langWall
	}
	if req.MemoryLimit <= 0 || req.MemoryLimit < langMem {
		req.MemoryLimit = langMem
	}

	// Go, Java, and Kotlin compilation spawn many compiler sub-processes.
	// Enforce a minimum pids limit so compilation doesn't fail with EAGAIN.
	const goMinPids = 512
	switch lang.ID {
	case models.LanguageGo, models.LanguageJava, models.LanguageKotlin, models.LanguageScala:
		if req.MaxProcesses < goMinPids {
			req.MaxProcesses = goMinPids
		}
	}

	// Build compile command.
	req.CompileCommand = buildCompileCommand(lang, sandboxSource, sub.CompilerOptions)

	// Build run command.
	req.RunCommand = buildRunCommand(lang, sandboxSource, sub.CommandLineArgs)

	// Some images have an ENTRYPOINT that conflicts with our commands.
	// Kotlin's gmazzo/kotlin image sets ENTRYPOINT=["/bin/kotlin"], which would
	// wrap our compile command as args to /bin/kotlin.
	// COBOL's dagui0/gnucobol image runs /entrypoint.sh that calls groupadd/useradd
	// (requires CAP_SETUID which we drop).  Clear the entrypoint for both.
	switch lang.ID {
	case models.LanguageKotlin, models.LanguageCOBOL:
		req.Entrypoint = []string{}
	}

	return req
}

// ─── internal helpers ────────────────────────────────────────────────────────

func (m *Manager) store(lang *models.Language) {
	m.mu.Lock()
	m.languages[lang.ID] = lang
	m.mu.Unlock()
}

// sandboxPath ensures the file path starts with /sandbox/.
func sandboxPath(file string) string {
	if strings.HasPrefix(file, "/") {
		return file
	}
	return "/sandbox/" + file
}

// buildEnv returns a set of safe environment variables for the container.
func buildEnv(lang *models.Language) []string {
	// Base PATH covers standard system directories.  Language-specific prefixes
	// are prepended so compilers installed outside /usr/bin are reachable.
	basePath := "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

	// Each entry must match the actual install path inside the matching
	// runtime-images/<lang>/Dockerfile. Languages whose binaries live under
	// /usr/bin (apk-installed Alpine images: Nim, OCaml, Lisp, Lua, Prolog,
	// Pascal) need no extraPath because basePath already covers /usr/bin.
	extraPath := ""
	switch lang.ID {
	case models.LanguageRust:
		// rust:1.82-alpine base
		extraPath = "/usr/local/cargo/bin"
	case models.LanguageGo:
		// golang:1.24-alpine base
		extraPath = "/usr/local/go/bin:/go/bin"
	case models.LanguageJava:
		// eclipse-temurin:21-jdk-alpine base
		extraPath = "/opt/java/openjdk/bin"
	case models.LanguageKotlin:
		// our Dockerfile installs kotlinc at /opt/kotlin/bin on top of temurin
		extraPath = "/opt/kotlin/bin:/opt/java/openjdk/bin"
	case models.LanguageGroovy:
		// our Dockerfile installs groovy at /opt/groovy/bin on top of temurin
		extraPath = "/opt/groovy/bin:/opt/java/openjdk/bin"
	case models.LanguageScala:
		// our Dockerfile installs scala 3 at /opt/scala/bin on top of temurin
		extraPath = "/opt/scala/bin:/opt/java/openjdk/bin"
	case models.LanguageHaskell:
		// haskell:9.4 base (debian) installs GHC under /opt/ghc/<version>/bin.
		// Old /root/.cabal entries removed — they aren't writable as sandbox UID.
		extraPath = "/opt/ghc/9.4.8/bin"
	case models.LanguageClojure:
		// our Dockerfile installs clojure via the upstream installer; the launcher
		// script lives at /usr/local/bin (already in basePath) but clj/clojure
		// need rlwrap and tools.deps under /opt — pin defensively.
		extraPath = "/opt/java/openjdk/bin"
	}

	path := basePath
	if extraPath != "" {
		path = extraPath + ":" + basePath
	}

	// Go's compiler forks many processes and writes temp files.  With a
	// read-only rootfs, TMPDIR must point to a writable location (/sandbox is
	// a bind mount) rather than the size-capped /tmp tmpfs.
	tmpDir := "/tmp"
	if lang.ID == models.LanguageGo {
		tmpDir = "/sandbox"
	}

	// Clojure must point HOME at /home/sandbox where the Clojure Dockerfile
	// pre-cached ~/.m2, ~/.gitlibs and ~/.clojure/.cpcache at build time. The
	// sandbox runs with --network none, so any jar not pre-fetched here fails
	// the classpath build with "Error building classpath".
	//
	// OCaml: our local image uses Alpine's apk ocaml package (not opam), so
	// the old HOME=/home/opam special case has been removed — /sandbox works.
	home := "/sandbox"
	if lang.ID == models.LanguageClojure {
		home = "/home/sandbox"
	}

	env := []string{
		"HOME=" + home,
		"TMPDIR=" + tmpDir,
		"PATH=" + path,
	}

	// Go build/module caches must live on a writable path too.
	if lang.ID == models.LanguageGo {
		env = append(env,
			"GOCACHE=/sandbox/.cache",
			"GOPATH=/sandbox/.gopath",
		)
	}

	// Language-specific environment variables.
	switch lang.DBType {
	case "sqlite":
		env = append(env, "SQLITE_TMPDIR=/tmp")
	case "mysql":
		env = append(env, "MYSQL_PWD=root")
	case "postgresql":
		env = append(env, "PGPASSWORD=postgres", "PGUSER=postgres")
	}

	return env
}

// buildCompileCommand constructs the compile command, preferring per-language
// explicit builders over the generic DB-stored tokeniser when the language
// warrants one.  Per-language overrides are checked first so that languages
// whose DB compile_command is intentionally empty (Kotlin, Scala, Swift) are
// still handled correctly.
func buildCompileCommand(lang *models.Language, sandboxSource, compilerOptions string) []string {
	// Per-language overrides (checked before the nil guard so languages with
	// empty DB compile_command still get their explicit builders).
	switch lang.ID {
	case models.LanguageC:
		cmd := compileC(sandboxSource)
		if compilerOptions != "" {
			cmd = append(cmd, tokenise(compilerOptions)...)
		}
		return cmd

	case models.LanguageCPP:
		cmd := compileCPP(sandboxSource)
		if compilerOptions != "" {
			cmd = append(cmd, tokenise(compilerOptions)...)
		}
		return cmd

	case models.LanguageRust:
		cmd := compileRust(sandboxSource)
		if compilerOptions != "" {
			cmd = append(cmd, tokenise(compilerOptions)...)
		}
		return cmd

	case models.LanguageJava:
		cmd := compileJava(sandboxSource)
		if compilerOptions != "" {
			cmd = append(cmd, tokenise(compilerOptions)...)
		}
		return cmd

	case models.LanguageGo:
		return compileGo(sandboxSource)

	case models.LanguageKotlin:
		return compileKotlin(sandboxSource)

	case models.LanguageCSharp:
		return compileCSharp(sandboxSource)

	case models.LanguageHaskell:
		return compileHaskell(sandboxSource)

	case models.LanguageSwift:
		return compileSwift(sandboxSource)

	case models.LanguageScala:
		return compileScala(sandboxSource)
	}

	// Generic: tokenise stored command (returns nil for empty/nil compile_command).
	if lang.CompileCommand == nil || *lang.CompileCommand == "" {
		return nil
	}
	return BuildCompileCommand(lang, sandboxSource)
}

// buildRunCommand constructs the run command.
func buildRunCommand(lang *models.Language, sandboxSource, commandLineArgs string) []string {
	// Per-language overrides.
	switch lang.ID {
	case models.LanguageJava:
		cls := javaClassName(sandboxSource)
		cmd := runJava(cls)
		if commandLineArgs != "" {
			cmd = append(cmd, tokenise(commandLineArgs)...)
		}
		return cmd

	case models.LanguageKotlin:
		cmd := runKotlin()
		if commandLineArgs != "" {
			cmd = append(cmd, tokenise(commandLineArgs)...)
		}
		return cmd

	case models.LanguageCSharp:
		cmd := runCSharp()
		if commandLineArgs != "" {
			cmd = append(cmd, tokenise(commandLineArgs)...)
		}
		return cmd

	case models.LanguageScala:
		cmd := runScala(sandboxSource)
		if commandLineArgs != "" {
			cmd = append(cmd, tokenise(commandLineArgs)...)
		}
		return cmd
	}

	// Generic: tokenise stored command.
	return BuildRunCommand(lang, sandboxSource, commandLineArgs)
}
