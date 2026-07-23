package routes

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"
	"go.uber.org/zap"

	apidocs "github.com/revature/corems-code-executor/api"
	"github.com/revature/corems-code-executor/internal/api/handlers"
	"github.com/revature/corems-code-executor/internal/api/middleware"
	"github.com/revature/corems-code-executor/internal/cache"
	"github.com/revature/corems-code-executor/internal/config"
	"github.com/revature/corems-code-executor/internal/database"
)

// serversBlockRe matches the top-level `servers:` block (the `servers:` line plus
// all following indented list lines) in the embedded OpenAPI spec, so it can be
// replaced per-request with a server URL derived from the incoming request.
var serversBlockRe = regexp.MustCompile(`(?m)^servers:\n(?:[ \t]+.*\n)*`)

// Handlers bundles all HTTP handler structs.
type Handlers struct {
	Submission *handlers.SubmissionHandler
	Language   *handlers.LanguageHandler
	Status     *handlers.StatusHandler
	Auth       *handlers.AuthHandler
	Batch      *handlers.BatchHandler
	Webhook    *handlers.WebhookHandler
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
	rl := middleware.NewRateLimiters(cfg, redisClient, log)

	// ---- Public endpoints (no auth) ----------------------------------------

	r.GET("/health", h.Status.HealthCheck)
	r.GET("/ready", h.Status.ReadinessCheck)

	// Prometheus metrics
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	// OpenAPI spec — served from embedded swagger.yaml.
	// Lives outside /swagger/* to avoid colliding with the wildcard UI route.
	r.GET("/openapi.yaml", func(c *gin.Context) {
		// Rewrite the spec's `servers:` block from the incoming request so Swagger
		// UI "Try it out" targets whatever origin + base path the docs were loaded
		// from (localhost, the ELB, or behind an ingress prefix) — no hardcoding.
		scheme := "http"
		if proto := c.GetHeader("X-Forwarded-Proto"); proto != "" {
			scheme = strings.TrimSpace(strings.Split(proto, ",")[0])
		} else if c.Request.TLS != nil {
			scheme = "https"
		}
		host := c.GetHeader("X-Forwarded-Host")
		if host == "" {
			host = c.Request.Host
		}
		serverURL := scheme + "://" + host
		block := "servers:\n  - url: " + serverURL + "\n    description: Current host (auto-detected)\n"
		spec := serversBlockRe.ReplaceAllLiteral(apidocs.SwaggerYAML, []byte(block))
		c.Data(http.StatusOK, "application/yaml; charset=utf-8", spec)
	})

	// Swagger UI — point it at the embedded spec above.
	r.GET("/swagger/*any", ginSwagger.WrapHandler(
		swaggerFiles.Handler,
		ginSwagger.URL("/openapi.yaml"),
		ginSwagger.DefaultModelsExpandDepth(-1),
		// Keep the token entered via "Authorize" attached to requests (and across
		// refresh). Without this swagger-ui drops it on re-render, so the
		// Authorization / X-Auth-Token header never gets sent.
		ginSwagger.PersistAuthorization(true),
	))

	// Statuses (public reference data)
	r.GET("/statuses", h.Status.GetStatuses)

	// Auth endpoints
	authGroup := r.Group("/auth")
	authGroup.Use(rl.Auth)
	{
		authGroup.POST("/token", h.Auth.GenerateToken)
		authGroup.POST("/apikey", authMW, h.Auth.GenerateAPIKey)
	}

	// ---- Authenticated endpoints -------------------------------------------
	//
	// Reads (polling, lookups) and writes (submit, batch) use separate rate-limit
	// budgets via rl.Read / rl.Write, so heavy polling cannot starve submissions.
	protected := r.Group("/")
	protected.Use(authMW)
	{
		// Submissions — batch must be registered before :token param route.
		protected.POST("/submissions/batch", rl.Write, h.Submission.CreateBatchSubmissions)
		protected.POST("/submissions", rl.Write, h.Submission.CreateSubmission)
		protected.GET("/submissions", rl.Read, h.Submission.ListSubmissions)
		protected.GET("/submissions/batch/:tokens", rl.Read, h.Submission.GetBatchSubmissions)
		protected.GET("/submissions/:token", rl.Read, h.Submission.GetSubmission)
		protected.DELETE("/submissions/:token", rl.Write, h.Submission.DeleteSubmission)

		// Batches — group submissions under a single batch_id and retrieve
		// aggregate results; the callback route re-fires the linked webhook.
		protected.POST("/batches", rl.Write, h.Batch.CreateBatch)
		// Bulk-start every batch stuck in the "pending" batch status (admin-only):
		// starts execution so each completes and its webhook fires automatically.
		// Registered before :id so the static segment wins.
		protected.POST("/batches/start-pending", rl.Write, middleware.MustBeAdmin(), h.Batch.BulkStartPendingBatches)
		protected.GET("/batches/:id", rl.Read, h.Batch.GetBatch)
		protected.POST("/batches/:id/start", rl.Write, h.Batch.StartBatch)
		protected.POST("/batches/:id/callback", rl.Write, h.Batch.RetriggerWebhook)

		// Webhooks — register and inspect outbound notification endpoints.
		protected.POST("/webhooks", rl.Write, h.Webhook.RegisterWebhook)
		protected.GET("/webhooks", rl.Read, h.Webhook.ListWebhooks)
		// Bulk re-delivery (admin-only): fire the webhook for EVERY completed batch
		// currently in a given webhook_status ("pending"/"failed"). Paginated +
		// bounded-concurrency, runs in the background. Registered before :id.
		protected.POST("/webhooks/retrigger", rl.Write, middleware.MustBeAdmin(), h.Batch.BulkRetriggerWebhooks)
		protected.GET("/webhooks/:id", rl.Read, h.Webhook.GetWebhook)

		// Languages
		protected.GET("/languages", rl.Read, h.Language.GetLanguages)
		protected.GET("/languages/:id", rl.Read, h.Language.GetLanguage)
		// Activate / deactivate a language by ID (admin-only): PATCH {"is_active": bool}.
		protected.PATCH("/languages/:id", rl.Write, middleware.MustBeAdmin(), h.Language.SetLanguageActive)
	}
}
