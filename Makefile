.PHONY: dev-up dev-down dev-ps dev-logs dev-run dev-test build test test-all test-short-all race clean docker-build docker-push todos check-spi-pin-sync

# Plugin submodules: each has its own go.mod, so `go test ./...` from the
# repo root does not recurse into them. The aggregator targets below close
# that coverage gap (issue #46).
PLUGIN_MODULES := plugins/memory plugins/sqlite plugins/postgres

# --- Docker services ---

dev-up:                ## Start local services (PostgreSQL)
	docker compose up -d --wait

dev-down:              ## Stop local services
	docker compose down

dev-reset:             ## Stop services and delete volumes (fresh start)
	docker compose down -v

dev-ps:                ## Show service status
	docker compose ps

dev-logs:              ## Tail service logs
	docker compose logs -f

# --- Build & Run ---

build:                 ## Build the binary
	go build -o bin/cyoda ./cmd/cyoda

dev-run: dev-up build  ## Start services + run cyoda with postgres KV
	set -a && . .env.dev && set +a && ./bin/cyoda

# --- Docker image ---

TAG        ?= dev
COMMIT     := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
IMAGE      := cyoda

docker-build:          ## Build Docker image (TAG=dev)
	docker build \
		--build-arg VERSION=$(TAG) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_DATE=$(BUILD_DATE) \
		-t $(IMAGE):$(TAG) .

docker-push:           ## Tag and push to registry (TAG=, REGISTRY= required)
ifndef REGISTRY
	$(error REGISTRY is required. Usage: make docker-push TAG=1.0.0 REGISTRY=your-registry.example.com)
endif
	docker tag $(IMAGE):$(TAG) $(REGISTRY)/cyoda/$(IMAGE):$(TAG)
	docker push $(REGISTRY)/cyoda/$(IMAGE):$(TAG)

# --- Testing ---

test:                  ## Run root-module tests only (plugin submodules skipped — see test-all)
	go test ./... -v

test-all:              ## Run root + every plugin submodule (requires Docker for postgres)
	go test ./... -v
	@for m in $(PLUGIN_MODULES); do \
	  echo "==> go test ./... in $$m"; \
	  (cd $$m && go test ./... -v) || exit $$?; \
	done

test-short-all:        ## Run root + every plugin submodule with -short (quick coverage check)
	go test -short ./... -v
	@for m in $(PLUGIN_MODULES); do \
	  echo "==> go test -short ./... in $$m"; \
	  (cd $$m && go test -short ./... -v) || exit $$?; \
	done

# Race detector — run once before opening a PR, not on every iteration.
# Race instrumentation makes tests 2-10× slower (see .claude/rules/race-testing.md).
# `internal/e2e` is excluded because under race it exceeds the default per-package
# 10m timeout; the production paths it covers (engine, cluster, store mutexes) are
# also exercised by the workflow/cluster/plugin unit tests below — those keep race
# coverage. CI invokes this same target so local and CI stay in lock-step.
race:                  ## Run race detector on race-sensitive packages (CI parity; excludes internal/e2e)
	@pkgs=$$(go list ./... | grep -v '^github.com/cyoda-platform/cyoda-go/internal/e2e$$'); \
	echo "race-testing $$(echo "$$pkgs" | wc -l | tr -d ' ') packages"; \
	go test -race -timeout=15m $$pkgs

dev-test: dev-up       ## Run all tests against local postgres
	set -a && . .env.dev && set +a && go test ./... -v -count=1

# --- TODOs ---

todos:                 ## List all TODO(Pn) deferred work items
	@grep -rn "TODO(P" --include="*.go" . | sort || echo "No TODOs found"

todos-p%:              ## List TODOs for a specific plan (e.g. make todos-p6)
	@grep -rn "TODO(P$*" --include="*.go" . | sort || echo "No TODOs for P$*"

# --- Cleanup ---

clean:                 ## Remove build artifacts
	rm -rf bin/ coverage.out

check-spi-pin-sync:    ## Verify cyoda-go-spi is pinned to the same version across root and all plugin go.mods
	@./scripts/check-spi-pin-sync.sh

help:                  ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*##' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*## "}; {printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2}'
