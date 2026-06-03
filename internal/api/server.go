package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/mdshabbir-ali/code-runtime/internal/api/middleware"
	"github.com/mdshabbir-ali/code-runtime/internal/api/routes"
	"github.com/mdshabbir-ali/code-runtime/internal/cache"
	"github.com/mdshabbir-ali/code-runtime/internal/config"
	"github.com/mdshabbir-ali/code-runtime/internal/database"
	"github.com/mdshabbir-ali/code-runtime/internal/metrics"
)

// Server wraps the Gin engine and net/http server, wiring all middleware and routes.
type Server struct {
	router     *gin.Engine
	httpServer *http.Server
	cfg        *config.Config
	log        *zap.Logger
}

// NewServer constructs and fully configures the HTTP server.
// It sets up all middleware, registers all routes, and attaches handlers.
func NewServer(
	cfg *config.Config,
	repos *database.Repositories,
	db *database.DB,
	redisClient *cache.Client,
	m *metrics.APIMetrics,
	log *zap.Logger,
	handlerBundle *routes.Handlers,
) *Server {
	// Set Gin mode.
	switch cfg.Server.Mode {
	case "debug":
		gin.SetMode(gin.DebugMode)
	case "test":
		gin.SetMode(gin.TestMode)
	default:
		gin.SetMode(gin.ReleaseMode)
	}

	r := gin.New()

	// Recovery — catch panics and return 500.
	r.Use(gin.Recovery())

	// Global middleware stack (order matters).
	r.Use(middleware.Metrics(m))
	r.Use(middleware.SecureHeaders())
	r.Use(middleware.CORS(cfg))
	r.Use(middleware.MaxBodySize(10 * 1024 * 1024)) // 10 MB
	r.Use(middleware.RequestLogger(log))

	// Trust configured proxies for real client IP detection.
	if len(cfg.Server.TrustedProxies) > 0 {
		_ = r.SetTrustedProxies(cfg.Server.TrustedProxies)
	}

	// Register all application routes.
	routes.Register(r, handlerBundle, cfg, repos.Users, redisClient, log)

	httpSrv := &http.Server{
		Addr:         cfg.Server.Address(),
		Handler:      r,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
		IdleTimeout:  cfg.Server.IdleTimeout,
	}

	return &Server{
		router:     r,
		httpServer: httpSrv,
		cfg:        cfg,
		log:        log,
	}
}

// Start begins listening for HTTP connections.
// It blocks until the server is stopped or an unrecoverable error occurs.
// Returns http.ErrServerClosed on graceful shutdown.
func (s *Server) Start() error {
	s.log.Info("HTTP server starting", zap.String("addr", s.cfg.Server.Address()))

	if err := s.httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("server error: %w", err)
	}
	return nil
}

// Stop gracefully drains active connections within the configured timeout.
func (s *Server) Stop(ctx context.Context) error {
	s.log.Info("HTTP server shutting down")
	if err := s.httpServer.Shutdown(ctx); err != nil {
		return fmt.Errorf("server shutdown: %w", err)
	}
	s.log.Info("HTTP server stopped")
	return nil
}
