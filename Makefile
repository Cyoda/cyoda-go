.PHONY: dev-up dev-down dev-ps dev-logs dev-run dev-test build test test-all test-short-all clean docker-build docker-push todos check-spi-pin-sync \
        deploy-aws destroy-aws deploy-gcp destroy-gcp deploy-azure destroy-azure

# Plugin submodules: each has its own go.mod, so `go test ./...` from the
# repo root does not recurse into them. The aggregator targets below close
# that coverage gap (issue #46).
PLUGIN_MODULES := plugins/memory plugins/sqlite plugins/postgres

TF_AWS   := deploy/terraform/aws
TF_GCP   := deploy/terraform/gcp
TF_AZURE := deploy/terraform/azure

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

# --- Cloud deployment (Terraform) ---

deploy-aws:            ## Deploy to AWS (EKS + RDS). Requires deploy/terraform/aws/terraform.tfvars
	@[ -f $(TF_AWS)/terraform.tfvars ] || \
	  { echo "ERROR: $(TF_AWS)/terraform.tfvars not found — copy from terraform.tfvars.example and set db_password + jwt_signing_key_pem"; exit 1; }
	terraform -chdir=$(TF_AWS) init && \
	terraform -chdir=$(TF_AWS) apply -auto-approve && \
	$$(terraform -chdir=$(TF_AWS) output -raw kubeconfig_command) && \
	kubectl get pods -n cyoda

destroy-aws:           ## Destroy AWS deployment (disables RDS deletion protection first)
	CLUSTER=$$(terraform -chdir=$(TF_AWS) output -raw eks_cluster_name) && \
	aws rds modify-db-instance --db-instance-identifier $$CLUSTER-postgres --no-deletion-protection && \
	terraform -chdir=$(TF_AWS) destroy -auto-approve

deploy-gcp:            ## Deploy to GCP (GKE + Cloud SQL). Requires deploy/terraform/gcp/terraform.tfvars
	@[ -f $(TF_GCP)/terraform.tfvars ] || \
	  { echo "ERROR: $(TF_GCP)/terraform.tfvars not found — copy from terraform.tfvars.example and set project_id + db_password + jwt_signing_key_pem"; exit 1; }
	terraform -chdir=$(TF_GCP) init && \
	terraform -chdir=$(TF_GCP) apply -auto-approve && \
	$$(terraform -chdir=$(TF_GCP) output -raw kubeconfig_command) && \
	kubectl get pods -n cyoda

destroy-gcp:           ## Destroy GCP deployment (disables Cloud SQL deletion protection first)
	CLUSTER=$$(terraform -chdir=$(TF_GCP) output -raw gke_cluster_name) && \
	gcloud sql instances patch $$CLUSTER-postgres --no-deletion-protection && \
	terraform -chdir=$(TF_GCP) destroy -auto-approve

deploy-azure:          ## Deploy to Azure (AKS + PostgreSQL). Requires deploy/terraform/azure/terraform.tfvars
	@[ -f $(TF_AZURE)/terraform.tfvars ] || \
	  { echo "ERROR: $(TF_AZURE)/terraform.tfvars not found — copy from terraform.tfvars.example and set subscription_id + db_admin_password + jwt_signing_key_pem"; exit 1; }
	terraform -chdir=$(TF_AZURE) init && \
	terraform -chdir=$(TF_AZURE) apply -auto-approve && \
	$$(terraform -chdir=$(TF_AZURE) output -raw kubeconfig_command) && \
	kubectl get pods -n cyoda

destroy-azure:         ## Destroy Azure deployment
	terraform -chdir=$(TF_AZURE) destroy -auto-approve

help:                  ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*##' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*## "}; {printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2}'
