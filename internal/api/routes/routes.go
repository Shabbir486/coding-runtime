package routes

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"
	"go.uber.org/zap"

	apidocs "github.com/mdshabbir-ali/code-runtime/api"
	"github.com/mdshabbir-ali/code-runtime/internal/api/handlers"
	"github.com/mdshabbir-ali/code-runtime/internal/api/middleware"
	"github.com/mdshabbir-ali/code-runtime/internal/cache"
	"github.com/mdshabbir-ali/code-runtime/internal/config"
	"github.com/mdshabbir-ali/code-runtime/internal/database"
)

// Handlers bundles all HTTP handler structs.
type Handlers struct {
	Submission *handlers.SubmissionHandler
	Language   *handlers.LanguageHandler
	Status     *handlers.StatusHandler
	Auth       *handlers.AuthHandler
}

// Register attaches all routes to the provided Gin engine.
// Auth and rate-limit middleware are applied selectively per route group.
func Register(
	r *gin.Engine,
	h *Handlers,
	cfg *config.Config,
	userRepo database.UserRepository,
	redisClient *cache.Client,
	log *zap.Logger,
) {
	authMW := middleware.Auth(cfg, userRepo, log)
	rateMW := middleware.RateLimit(cfg, redisClient, log)

	// ---- Public endpoints (no auth) ----------------------------------------

	r.GET("/health", h.Status.HealthCheck)
	r.GET("/ready", h.Status.ReadinessCheck)

	// Prometheus metrics
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	// OpenAPI spec — served from embedded swagger.yaml.
	// Lives outside /swagger/* to avoid colliding with the wildcard UI route.
	r.GET("/openapi.yaml", func(c *gin.Context) {
		c.Data(http.StatusOK, "application/yaml; charset=utf-8", apidocs.SwaggerYAML)
	})

	// Swagger UI — point it at the embedded spec above.
	r.GET("/swagger/*any", ginSwagger.WrapHandler(
		swaggerFiles.Handler,
		ginSwagger.URL("/openapi.yaml"),
		ginSwagger.DefaultModelsExpandDepth(-1),
	))

	// Statuses (public reference data)
	r.GET("/statuses", h.Status.GetStatuses)

	// Auth endpoints
	authGroup := r.Group("/auth")
	authGroup.Use(rateMW)
	{
		authGroup.POST("/token", h.Auth.GenerateToken)
		authGroup.POST("/apikey", authMW, h.Auth.GenerateAPIKey)
	}

	// ---- Authenticated endpoints -------------------------------------------

	protected := r.Group("/")
	protected.Use(rateMW, authMW)
	{
		// Submissions — batch must be registered before :token param route.
		protected.POST("/submissions/batch", h.Submission.CreateBatchSubmissions)
		protected.POST("/submissions", h.Submission.CreateSubmission)
		protected.GET("/submissions", h.Submission.ListSubmissions)
		protected.GET("/submissions/:token", h.Submission.GetSubmission)
		protected.DELETE("/submissions/:token", h.Submission.DeleteSubmission)

		// Languages
		protected.GET("/languages", h.Language.GetLanguages)
		protected.GET("/languages/:id", h.Language.GetLanguage)
	}
}
