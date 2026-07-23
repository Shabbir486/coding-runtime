.PHONY: all build test lint docker-build docker-push \
        compose-build compose-up compose-up-prod compose-down compose-logs compose-ps compose-restart \
        migrate migrate-schema seed pull-images swagger generate clean load-test \
        test-integration fmt vet help

# ---------------------------------------------------------------------------
# Variables
# ---------------------------------------------------------------------------
APP_NAME    := code-runtime
VERSION     := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
BUILD_TIME  := $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')
GOFLAGS     := -ldflags="-X main.Version=$(VERSION) -X main.BuildTime=$(BUILD_TIME) -w -s"
DOCKER_REPO ?= ghcr.io/revature/corems-code-executor

# ---------------------------------------------------------------------------
# Default target
# ---------------------------------------------------------------------------
all: lint test build ## Run lint, test, and build

# ---------------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------------
build: ## Build API gateway, worker, and queue-manager binaries
	@echo ">> Building API gateway ($(VERSION))..."
	@mkdir -p bin
	go build $(GOFLAGS) -o bin/api ./cmd/api-gateway/
	@echo ">> Building Worker ($(VERSION))..."
	go build $(GOFLAGS) -o bin/worker ./cmd/worker/
	@echo ">> Building Queue Manager ($(VERSION))..."
	go build $(GOFLAGS) -o bin/queue-manager ./cmd/queue-manager/
	@echo ">> Binaries ready in ./bin/"

# ---------------------------------------------------------------------------
# Test
# ---------------------------------------------------------------------------
test: ## Run unit tests with race detector and coverage
	go test -v -race -coverprofile=coverage.out -covermode=atomic ./...
	go tool cover -html=coverage.out -o coverage.html
	@echo ">> Coverage report written to coverage.html"

test-integration: ## Run integration tests (requires running infra)
	go test -v -tags=integration -timeout=300s ./tests/integration/...

test-short: ## Run tests without slow subtests
	go test -short -race ./...

coverage: test ## Open coverage report in browser
	open coverage.html 2>/dev/null || xdg-open coverage.html 2>/dev/null || true

# ---------------------------------------------------------------------------
# Code quality
# ---------------------------------------------------------------------------
lint: ## Run golangci-lint
	golangci-lint run ./...

fmt: ## Format Go source with gofmt and goimports
	gofmt -w .
	goimports -w .

vet: ## Run go vet
	go vet ./...

# ---------------------------------------------------------------------------
# Docker
# ---------------------------------------------------------------------------
docker-build: ## Build Docker images for API gateway, worker, and queue-manager
	docker build -f docker/Dockerfile.api \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg BUILD_TIME=$(BUILD_TIME) \
	  -t $(DOCKER_REPO)-api:$(VERSION) \
	  -t $(DOCKER_REPO)-api:latest .
	docker build -f docker/Dockerfile.worker \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg BUILD_TIME=$(BUILD_TIME) \
	  -t $(DOCKER_REPO)-worker:$(VERSION) \
	  -t $(DOCKER_REPO)-worker:latest .
	docker build -f docker/Dockerfile.queue-manager \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg BUILD_TIME=$(BUILD_TIME) \
	  -t $(DOCKER_REPO)-queue-manager:$(VERSION) \
	  -t $(DOCKER_REPO)-queue-manager:latest .
	@echo ">> Images built: $(DOCKER_REPO)-api:$(VERSION), $(DOCKER_REPO)-worker:$(VERSION), $(DOCKER_REPO)-queue-manager:$(VERSION)"

docker-push: docker-build ## Push Docker images to registry
	docker push $(DOCKER_REPO)-api:$(VERSION)
	docker push $(DOCKER_REPO)-api:latest
	docker push $(DOCKER_REPO)-worker:$(VERSION)
	docker push $(DOCKER_REPO)-worker:latest
	docker push $(DOCKER_REPO)-queue-manager:$(VERSION)
	docker push $(DOCKER_REPO)-queue-manager:latest

# ---------------------------------------------------------------------------
# Docker Compose
#   local : bundled MySQL + Redis (`localinfra` profile). NATS (`natsinfra`)
#           runs ONLY when QUEUE_PROVIDER=nats — with QUEUE_PROVIDER=sqs it is
#           not pulled or started.
#   prod  : single host against AWS RDS + ElastiCache/SQS — local infra stays OFF.
#   (The Swarm path is the separate docker-compose.prod.yml overlay.)
# ---------------------------------------------------------------------------
COMPOSE       ?= docker compose
APP_SERVICES  := api queue-manager worker

# Queue provider drives whether the local NATS container is needed. Read from
# .env (default nats); override with `make compose-up QUEUE_PROVIDER=sqs`.
QUEUE_PROVIDER := $(shell awk -F= '/^QUEUE_PROVIDER=/{gsub(/[ \t\r]/,"",$$2); print $$2}' .env 2>/dev/null)
ifeq ($(strip $(QUEUE_PROVIDER)),)
QUEUE_PROVIDER := nats
endif

ifeq ($(QUEUE_PROVIDER),nats)
NATS_PROFILE := --profile natsinfra
NATS_INFRA   := nats
else
NATS_PROFILE :=
NATS_INFRA   :=
endif

# localinfra (mysql/redis) + natsinfra only for the nats provider.
LOCAL_PROFILE := --profile localinfra $(NATS_PROFILE)
# All profiles — used by down/ps so cleanup covers whatever is running.
ALL_PROFILES  := --profile localinfra --profile natsinfra
INFRA_LOCAL   := mysql redis $(NATS_INFRA) prometheus grafana jaeger
INFRA_PROD    := $(NATS_INFRA) prometheus grafana jaeger

compose-build: ## Build api/queue-manager/worker images
	@echo ">> Building application images ($(VERSION))..."
	$(COMPOSE) build --parallel $(APP_SERVICES)

compose-up: ## [local] Start full stack (bundled MySQL + Redis; NATS only if QUEUE_PROVIDER=nats)
	@echo ">> [local] queue_provider=$(QUEUE_PROVIDER) (NATS local container: $(if $(NATS_INFRA),on,off))"
	@echo ">> [local] Pulling infrastructure images..."
	$(COMPOSE) $(LOCAL_PROFILE) pull $(INFRA_LOCAL)
	@echo ">> [local] Building application images..."
	$(COMPOSE) build --parallel $(APP_SERVICES)
	@echo ">> [local] Starting services..."
	$(COMPOSE) $(LOCAL_PROFILE) up -d
	@echo ">> API:     http://localhost:8002"
	@echo ">> Metrics: http://localhost:9090"
	@echo ">> Grafana: http://localhost:3000 (admin/admin)"
	@echo ">> Tip: run 'make pull-images' once to pre-pull language runtime images"

compose-up-prod: ## [prod] Start app against AWS RDS + ElastiCache (no local DB/cache)
	@test -f .env && echo ">> [prod] Using .env for RDS/ElastiCache settings" || \
	  echo ">> [prod] WARNING: no .env found — ensure DATABASE_HOST/REDIS_HOST (+REDIS_TLS_ENABLED) are set via env or hardcoded, else the app will resolve the local 'mysql'/'redis' names which are OFF in prod."
	@echo ">> [prod] Pulling infrastructure images..."
	$(COMPOSE) pull $(INFRA_PROD)
	@echo ">> [prod] Building application images..."
	$(COMPOSE) build --parallel $(APP_SERVICES)
	@echo ">> [prod] Starting services (external MySQL/Redis; local DB/cache OFF)..."
	$(COMPOSE) up -d
	@echo ">> API up on :8002 — front it with your load balancer / ingress."

compose-down: ## Stop all services and remove containers (all profiles)
	$(COMPOSE) $(ALL_PROFILES) down --remove-orphans

compose-logs: ## Tail docker compose logs
	$(COMPOSE) logs -f

compose-ps: ## Show running compose services
	$(COMPOSE) $(ALL_PROFILES) ps

compose-restart: compose-down compose-up ## Restart the local stack

# ---------------------------------------------------------------------------
# Database
#   Schema + reference data are auto-applied on api-gateway startup
#   (database.MigrateAndSeed when migrate_on_start=true). These targets run the
#   same logic standalone via cmd/migrate — handy for CI / pre-deploy against
#   AWS RDS. Connection comes from CODERUNTIME_* env / config.yaml (or .env).
# ---------------------------------------------------------------------------
migrate: ## Apply schema migrations + seed reference data (RDS-safe, idempotent)
	go run ./cmd/migrate -mode all

migrate-schema: ## Apply schema migrations only
	go run ./cmd/migrate -mode migrate

seed: ## Seed statuses and languages only
	go run ./cmd/migrate -mode seed

# ---------------------------------------------------------------------------
# Runtime images
# ---------------------------------------------------------------------------
pull-images: ## Pull all sandbox language Docker images
	./scripts/pull-images.sh

# ---------------------------------------------------------------------------
# Docs / Code generation
# ---------------------------------------------------------------------------
swagger: ## Regenerate OpenAPI/Swagger docs from annotations
	swag init -g cmd/api-gateway/main.go -o api/ --parseInternal

generate: ## Run go generate for all packages
	go generate ./...

# ---------------------------------------------------------------------------
# Load testing
# ---------------------------------------------------------------------------
load-test: ## Run k6 load test (requires k6 installed)
	k6 run tests/load/submission_test.js

load-test-smoke: ## Quick smoke load test (5 VUs, 30s)
	k6 run --vus 5 --duration 30s tests/load/submission_test.js

# ---------------------------------------------------------------------------
# Clean
# ---------------------------------------------------------------------------
clean: ## Remove build artifacts
	rm -rf bin/ coverage.out coverage.html

# ---------------------------------------------------------------------------
# Help
# ---------------------------------------------------------------------------
help: ## Show this help message
	@echo "$(APP_NAME) — available targets:"
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
	  | sort \
	  | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'
