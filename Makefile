.PHONY: all build test lint docker-build docker-push compose-up compose-down \
        compose-logs migrate seed pull-images swagger generate clean load-test \
        test-integration fmt vet help

# ---------------------------------------------------------------------------
# Variables
# ---------------------------------------------------------------------------
APP_NAME    := code-runtime
VERSION     := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
BUILD_TIME  := $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')
GOFLAGS     := -ldflags="-X main.Version=$(VERSION) -X main.BuildTime=$(BUILD_TIME) -w -s"
DOCKER_REPO ?= ghcr.io/mdshabbir-ali/code-runtime

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
# ---------------------------------------------------------------------------
compose-up: ## Pull infra images, build api/worker, then start all services
	@echo ">> Pulling infrastructure images..."
	docker-compose pull postgres redis nats prometheus grafana jaeger
	@echo ">> Building API and Worker images..."
	docker-compose build --parallel api worker
	@echo ">> Starting services..."
	docker-compose up -d
	@echo ">> Services started. API available at http://localhost:8002"
	@echo ">> Metrics:  http://localhost:9090"
	@echo ">> Grafana:  http://localhost:3000 (admin/admin)"
	@echo ">> Tip: run 'make pull-images' once to pre-pull language runtime images"

compose-down: ## Stop all services
	docker-compose down

compose-logs: ## Tail docker-compose logs
	docker-compose logs -f

compose-restart: compose-down compose-up ## Restart all services

# ---------------------------------------------------------------------------
# Database
# ---------------------------------------------------------------------------
migrate: ## Run database migrations
	go run ./scripts/migrate.go

seed: ## Seed database with languages and statuses
	go run ./scripts/seed.go

migrate-down: ## Roll back the last migration
	go run ./scripts/migrate.go -down

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
