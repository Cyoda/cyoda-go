---
name: deploy-terraform
description: Provision cyoda-go infrastructure on AWS, GCP, or Azure using Terraform. Use when the user asks how to deploy infrastructure, set up an EKS/GKE/AKS cluster, provision a managed PostgreSQL database, run terraform init/plan/apply, or configure terraform.tfvars.
allowed-tools: Bash(terraform *) Bash(aws *) Bash(gcloud *) Bash(az *) Bash(kubectl *) Bash(openssl *) Read Write
---

## How to handle a deploy request

When the user asks to deploy cyoda, follow this sequence — do NOT run any terraform or make commands until step 3 is complete.

**Step 1 — Ask which cloud** (if not already stated): AWS, GCP, or Azure.

**Step 2 — Present parameters and ask for values.**
Show the table for the chosen cloud (see below) and ask the user to confirm or override each value. Required fields must be provided; optional fields show their default and can be left as-is.

**Step 3 — Confirm before proceeding.**
Summarise the chosen values and ask "Shall I write terraform.tfvars and run `make deploy-<cloud>`?" Only proceed after explicit confirmation.

**Step 4 — Write terraform.tfvars and deploy.**
Write the confirmed values to `deploy/terraform/<cloud>/terraform.tfvars`, then run `make deploy-<cloud>`.

---

### AWS parameters

| Parameter | Default | Required |
|-----------|---------|----------|
| `db_password` | — | **yes** |
| `jwt_signing_key_pem` | — | **yes** (contents of signing-key.pem) |
| `bootstrap_client_id` | `cyoda-admin` | **yes** — needed to authenticate after deploy |
| `bootstrap_client_secret` | — | **yes** — used to get OAuth tokens |
| `region` | `us-east-1` | no |
| `cluster_name` | `cyoda-go` | no |
| `kubernetes_version` | `1.31` | no |
| `node_instance_type` | `t3.medium` | no |
| `node_desired_size` | `2` | no |
| `db_instance_class` | `db.t3.micro` | no |
| `db_multi_az` | `false` | no (set `true` for production HA) |
| `cyoda_image_tag` | `` (chart appVersion) | no |

### GCP parameters

| Parameter | Default | Required |
|-----------|---------|----------|
| `project_id` | — | **yes** |
| `db_password` | — | **yes** |
| `jwt_signing_key_pem` | — | **yes** |
| `bootstrap_client_id` | `cyoda-admin` | **yes** — needed to authenticate after deploy |
| `bootstrap_client_secret` | — | **yes** — used to get OAuth tokens |
| `region` | `us-central1` | no |
| `zone` | `us-central1-a` | no |
| `cluster_name` | `cyoda-go` | no |
| `node_machine_type` | `e2-standard-2` | no |
| `node_count` | `2` | no |
| `db_tier` | `db-g1-small` | no |
| `db_availability_type` | `ZONAL` | no (set `REGIONAL` for production HA) |
| `cyoda_image_tag` | `` (chart appVersion) | no |

### Azure parameters

| Parameter | Default | Required |
|-----------|---------|----------|
| `subscription_id` | — | **yes** |
| `db_admin_password` | — | **yes** (must contain uppercase + special char) |
| `jwt_signing_key_pem` | — | **yes** |
| `bootstrap_client_id` | `cyoda-admin` | **yes** — needed to authenticate after deploy |
| `bootstrap_client_secret` | — | **yes** — used to get OAuth tokens |
| `location` | `eastus` | no |
| `resource_group_name` | `cyoda-rg` | no |
| `cluster_name` | `cyoda-go` | no |
| `node_vm_size` | `Standard_D2s_v3` | no |
| `node_count` | `2` | no |
| `db_sku_name` | `B_Standard_B1ms` | no (use `GP_Standard_D2s_v3` for production) |
| `cyoda_image_tag` | `` (chart appVersion) | no |

---

## Current terraform state

Configured provider directories:
- `deploy/terraform/aws/` — EKS + RDS PostgreSQL 16
- `deploy/terraform/gcp/` — GKE + Cloud SQL PostgreSQL 16
- `deploy/terraform/azure/` — AKS + Azure Database for PostgreSQL Flexible Server

Shared module: `deploy/terraform/modules/cyoda-helm/` — creates K8s Secrets + deploys `deploy/helm/cyoda` via Helm.

Active tfvars (if present): !`ls deploy/terraform/aws/terraform.tfvars 2>/dev/null && echo "aws tfvars present" || echo "aws tfvars missing"`

---

## Pre-requisites

```bash
terraform -v          # need >= 1.6
kubectl version --client
helm version          # need >= 3.14
openssl genrsa -traditional -out signing-key.pem 4096   # JWT signing key — keep out of git
```

---

## Quick deploy / destroy (make targets)

The Makefile has one-command targets for each cloud. All targets run non-interactively (`-auto-approve`). Authenticate first (see per-cloud sections below), then:

```bash
make deploy-aws      # init → apply (auto-approve) → kubeconfig → kubectl get pods
make destroy-aws     # disables RDS deletion protection → terraform destroy (auto-approve)

make deploy-gcp
make destroy-gcp     # disables Cloud SQL deletion protection → terraform destroy

make deploy-azure
make destroy-azure
```

Each `deploy-*` target checks that `terraform.tfvars` exists and exits with a clear error if not.

---

## AWS — EKS + RDS PostgreSQL

```bash
# 1. Authenticate
aws configure --profile cyoda
aws sts get-caller-identity --profile cyoda   # verify
export AWS_PROFILE=cyoda

# 2. Set up variables
cp deploy/terraform/aws/terraform.tfvars.example deploy/terraform/aws/terraform.tfvars
# Required: db_password, jwt_signing_key_pem, bootstrap_client_id, bootstrap_client_secret

# 3. Deploy
make deploy-aws
```

Defaults: `us-east-1`, `t3.medium` nodes, `db.t3.micro` RDS.

Production: set `db_multi_az=true`, larger `db_instance_class`, remote S3 backend.

**Note:** AWS SSO requires `sso_start_url`, `sso_region`, `sso_account_id`, and `sso_role_name` in `~/.aws/config`. If not using SSO, use `aws configure --profile cyoda` with IAM access keys instead.

---

## GCP — GKE + Cloud SQL

```bash
# 1. Enable APIs (one-time)
gcloud services enable container.googleapis.com sqladmin.googleapis.com servicenetworking.googleapis.com --project=YOUR_PROJECT_ID

# 2. Authenticate
gcloud auth application-default login

# 3. Set up variables
cp deploy/terraform/gcp/terraform.tfvars.example deploy/terraform/gcp/terraform.tfvars
# Required: project_id, db_password, jwt_signing_key_pem, bootstrap_client_id, bootstrap_client_secret

# 4. Deploy
make deploy-gcp
```

Defaults: `us-central1`, `e2-standard-2` nodes, `db-g1-small` Cloud SQL.

Production: `db_availability_type="REGIONAL"`, larger `db_tier`, GCS backend.

---

## Azure — AKS + PostgreSQL Flexible Server

```bash
# 1. Authenticate
az login && az account set --subscription YOUR_SUBSCRIPTION_ID

# 2. Set up variables
cp deploy/terraform/azure/terraform.tfvars.example deploy/terraform/azure/terraform.tfvars
# Required: subscription_id, db_admin_password, jwt_signing_key_pem, bootstrap_client_id, bootstrap_client_secret
# Note: db_admin_password must contain uppercase letters and special characters

# 3. Deploy
make deploy-azure
```

Defaults: `eastus`, `Standard_D2s_v3` nodes, `B_Standard_B1ms` Postgres SKU.

Production: `GP_Standard_D2s_v3` SKU, Azure Blob remote backend.

---

## Post-deploy verification

```bash
# AWS — hostname
kubectl get svc cyoda -n cyoda -o jsonpath='{.status.loadBalancer.ingress[0].hostname}'

# GCP / Azure — IP
kubectl get svc cyoda -n cyoda -o jsonpath='{.status.loadBalancer.ingress[0].ip}'

# Health check — note: all routes are under /api, NOT at root
curl http://<EXTERNAL_IP_OR_HOSTNAME>:8080/api/health
# → {"status":"UP"}

# Get an OAuth token
curl -s -X POST http://<EXTERNAL_IP_OR_HOSTNAME>:8080/api/oauth/token \
  -u "<bootstrap_client_id>:<bootstrap_client_secret>" \
  -d "grant_type=client_credentials" | jq .
```

**Important:** The app has no UI and no root route. All API endpoints are under `/api`. Hitting `/health` returns 404 — use `/api/health`.

---

## Teardown

```bash
make destroy-aws     # AWS  — disables RDS deletion protection, then terraform destroy
make destroy-gcp     # GCP  — disables Cloud SQL deletion protection, then terraform destroy
make destroy-azure   # Azure — terraform destroy
```

---

## Troubleshooting

| Symptom | Cause | Fix |
|---------|-------|-----|
| `Init:0/1` or migration pod `Error` | Migration job failed to connect to DB | `kubectl logs job/cyoda-migrate-1 -n cyoda` — usually a network/SG issue; re-run `make deploy-*` after DB is fully ready |
| `ImagePullBackOff` | Wrong image tag | Set `cyoda_image_tag` to a published tag |
| `CrashLoopBackOff` | Bad Postgres DSN, wrong HMAC key size, or missing JWT key | `kubectl logs -n cyoda pod/cyoda-0 --previous` |
| `404 page not found` | Hitting root or `/health` — no route there | All routes are under `/api` — use `/api/health`, `/api/oauth/token`, etc. |
| `401 Unauthorized` | `bootstrap_client_id` / `bootstrap_client_secret` not set in tfvars | Add them and re-run `make deploy-*` |
| RDS not reachable from pods | EKS managed nodes use auto-created cluster SG, not a custom one | Already fixed in `aws/main.tf` — RDS ingress references `aws_eks_cluster.main.vpc_config[0].cluster_security_group_id` |
| Cloud SQL not reachable | VPC peering not ready | Wait for `google_service_networking_connection` to complete |
| Azure Postgres not reachable | DNS not propagated | Verify `azurerm_private_dns_zone_virtual_network_link` applied |
| Helm timeout (`context deadline exceeded`) | Pod not ready within 600s | `kubectl describe pod/cyoda-0 -n cyoda` — check events; re-run `make deploy-*` (idempotent) |
| AWS credentials expired mid-deploy | IAM session tokens are short-lived | Re-run `aws configure --profile cyoda` and retry |