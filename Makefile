.PHONY: dev-up dev-down dev-reset dev-ps dev-logs dev-run dev-test build test test-full preflight race clean docker-build docker-push todos check-spi-pin-sync check-codegen check-gofmt repin-plugins

# Recipes need pipefail: a `go test | testreport` pipeline must fail on the
# LEFT side too, or a compile error in the test binary reports as success.
SHELL := /bin/bash
.SHELLFLAGS := -o pipefail -c

# Plugin submodules: each has its own go.mod, so `go test ./...` from the
# repo root does not recurse into them. The aggregator targets below close
# that coverage gap (issue #46).
PLUGIN_MODULES := plugins/memory plugins/sqlite plugins/postgres

# --- Docker services ---
#
# Dev-only PostgreSQL for running cyoda-go on bare metal. Not a provisioning
# artifact — deploy/docker/compose.yaml is the packaged app (sqlite-backed);
# this is just the database.
DEV_COMPOSE := scripts/dev/compose.yaml

# Postgres connection for the dev targets, matching scripts/dev/compose.yaml.
# Set explicitly rather than via CYODA_PROFILES=postgres: that would read
# .env.postgres, which is gitignored — on a fresh clone the profile would find
# nothing and cyoda would fall back to the memory backend silently, which is a
# worse failure than an error. The compose file is the single source of truth.
DEV_PG_ENV := CYODA_STORAGE_BACKEND=postgres \
	CYODA_POSTGRES_URL='postgres://minicyoda:minicyoda@localhost:5432/minicyoda?sslmode=disable' \
	CYODA_POSTGRES_AUTO_MIGRATE=true

dev-up:                ## Start local services (PostgreSQL)
	docker compose -f $(DEV_COMPOSE) up -d --wait

dev-down:              ## Stop local services
	docker compose -f $(DEV_COMPOSE) down

dev-reset:             ## Stop services and delete volumes (fresh start)
	docker compose -f $(DEV_COMPOSE) down -v

dev-ps:                ## Show service status
	docker compose -f $(DEV_COMPOSE) ps

dev-logs:              ## Tail service logs
	docker compose -f $(DEV_COMPOSE) logs -f

# --- Build & Run ---

build:                 ## Build the binary
	go build -o bin/cyoda ./cmd/cyoda

dev-run: dev-up build  ## Start services + run cyoda against local postgres
	$(DEV_PG_ENV) ./bin/cyoda

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

# --- Tests ---
#
# Two tiers, and no Docker-free tier by design. Every tier that matters stands
# up real containers; when Docker cannot serve them the answer is to fix
# Docker, not to fall back to a narrower run that reports green.
#
# Both tiers pipe `go test -json` through scripts/testreport rather than using
# -v. That buys two things:
#
#   1. A package that ran NO tests is reported as such and, when the tier
#      required it, fails the run. `go test` cannot express this: a TestMain
#      calling os.Exit(0) — how the parity suites and internal/e2e opt out of
#      -short — emits a package-level "pass" with no test events, printing
#      `ok  pkg  2.80s`, identical to a real pass. A green that cannot be
#      falsified gets escalated to the full suite every round, which is what
#      made verification the slowest part of delivery.
#   2. Failures print verbatim and in full, with the passing majority reduced
#      to a count. -v emitted 45,000 events for the fast tier alone, so output
#      got piped through `tail` and real failures were lost.

# The parity runners. Deliberately explicit rather than an `e2e/parity` prefix:
# externalapi, scheduledfunction and scheduledtransition under that tree are
# scenario libraries with no test files of their own, and requiring them would
# report a hole that is not one.
PARITY_SUITES := e2e/parity/memory,e2e/parity/sqlite,e2e/parity/postgres,e2e/parity/multinode,e2e/parity/fixtureutil

preflight:             ## Verify Docker can actually serve the test suites
	@./scripts/preflight-docker.sh

test: preflight        ## Iteration tier: unit + cross-backend parity (~3 min). Excludes internal/e2e and plugin submodules.
	@echo "==> unit + parity — NOT in this tier: internal/e2e, plugin submodules (see test-full)"
	@pkgs=$$(go list ./... | grep -v '^github.com/cyoda-platform/cyoda-go/internal/e2e$$'); \
	go test -json $$pkgs | go run ./scripts/testreport -must-run '$(PARITY_SUITES)'

test-full: preflight   ## End-of-deliverable: everything, root + every plugin submodule (~15 min)
	@echo "==> root module, including internal/e2e"
	@go test -json -timeout 30m ./... | go run ./scripts/testreport -must-run '$(PARITY_SUITES),internal/e2e'
	@for m in $(PLUGIN_MODULES); do \
	  echo "==> $$m"; \
	  (cd $$m && go test -json ./... | go run $(CURDIR)/scripts/testreport) || exit $$?; \
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
	go test -json -race -timeout=15m $$pkgs | go run ./scripts/testreport

dev-test: dev-up       ## Run all tests against the local postgres from dev-up
	$(DEV_PG_ENV) go test -json -count=1 -timeout 30m ./... | go run ./scripts/testreport \
	  -must-run '$(PARITY_SUITES),internal/e2e'

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

repin-plugins:         ## Pseudo-version-pin plugin submodules to the current (pushed) HEAD in root go.mod (coordinated-release window; see MAINTAINING.md §3)
	@./scripts/repin-plugins.sh

check-codegen:         ## Verify api/generated.go is in sync with api/openapi.yaml (go generate is up to date)
	@./scripts/check-generated-in-sync.sh

check-gofmt:           ## Verify all Go files are gofmt-clean (root + plugin submodules)
	@dirty="$$(gofmt -l .)"; \
	if [ -n "$$dirty" ]; then \
		echo "gofmt-dirty files (run 'gofmt -w .'):"; echo "$$dirty"; exit 1; \
	fi; \
	echo "OK: all Go files are gofmt-clean."

help:                  ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*##' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*## "}; {printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2}'
