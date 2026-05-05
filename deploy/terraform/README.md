# cyoda-go — Terraform Infrastructure

Terraform configurations to deploy cyoda-go on **AWS**, **GCP**, and **Azure** using PostgreSQL as the storage backend and the existing Helm chart (`deploy/helm/cyoda`).

## Layout

```
deploy/terraform/
├── modules/
│   └── cyoda-helm/        # shared module: K8s secrets + Helm release
├── aws/                   # EKS + RDS PostgreSQL
├── gcp/                   # GKE + Cloud SQL (PostgreSQL 16)
└── azure/                 # AKS + Azure Database for PostgreSQL Flexible Server
```

Each cloud directory is a self-contained root module. Pick the one matching your cloud and follow its quickstart below.

## Prerequisites

| Tool | Version |
|------|---------|
| [Terraform](https://developer.hashicorp.com/terraform/install) | >= 1.6 |
| [kubectl](https://kubernetes.io/docs/tasks/tools/) | >= 1.28 |
| [Helm](https://helm.sh/docs/intro/install/) | >= 3.14 |
| Cloud CLI (aws / gcloud / az) | latest |

You also need an RSA private key for JWT signing. Generate one with:

```bash
openssl genrsa -out signing-key.pem 4096
```

> **Security:** Keep `signing-key.pem` and `terraform.tfvars` out of version control. Add both to `.gitignore`.

---

## AWS — EKS + RDS PostgreSQL

**What gets created:** VPC, public/private subnets, NAT gateways, EKS cluster with a managed node group, RDS PostgreSQL 16 instance (encrypted, private), and the cyoda Helm release.

### Quickstart

```bash
# 1. Authenticate
aws configure   # or: export AWS_PROFILE=my-profile

# 2. Enter the AWS directory
cd deploy/terraform/aws

# 3. Copy and edit variables
cp terraform.tfvars.example terraform.tfvars
# Edit terraform.tfvars: set db_password and jwt_signing_key_pem

# 4. Initialise and apply
terraform init
terraform plan
terraform apply

# 5. Connect kubectl
$(terraform output -raw kubeconfig_command)

# 6. Verify
kubectl get pods -n cyoda
```

### Key variables

| Variable | Default | Description |
|----------|---------|-------------|
| `region` | `us-east-1` | AWS region |
| `cluster_name` | `cyoda` | EKS cluster / resource prefix |
| `node_instance_type` | `t3.medium` | EC2 type for worker nodes |
| `db_instance_class` | `db.t3.micro` | RDS instance class |
| `db_password` | — | **Required**, sensitive |
| `jwt_signing_key_pem` | — | **Required**, sensitive |

---

## GCP — GKE + Cloud SQL

**What gets created:** VPC with secondary ranges, Cloud SQL PostgreSQL 16 (private IP only, VPC-peered), GKE Standard cluster with a node pool, Workload Identity, and the cyoda Helm release.

### Prerequisites

```bash
# Enable required APIs
gcloud services enable \
  container.googleapis.com \
  sqladmin.googleapis.com \
  servicenetworking.googleapis.com \
  --project=YOUR_PROJECT_ID
```

### Quickstart

```bash
# 1. Authenticate
gcloud auth application-default login

# 2. Enter the GCP directory
cd deploy/terraform/gcp

# 3. Copy and edit variables
cp terraform.tfvars.example terraform.tfvars
# Edit terraform.tfvars: set project_id, db_password, jwt_signing_key_pem

# 4. Initialise and apply
terraform init
terraform plan
terraform apply

# 5. Connect kubectl
$(terraform output -raw kubeconfig_command)

# 6. Verify
kubectl get pods -n cyoda
```

### Key variables

| Variable | Default | Description |
|----------|---------|-------------|
| `project_id` | — | **Required** GCP project ID |
| `region` | `us-central1` | GCP region |
| `node_machine_type` | `e2-standard-2` | GKE node machine type |
| `db_tier` | `db-g1-small` | Cloud SQL tier |
| `db_password` | — | **Required**, sensitive |
| `jwt_signing_key_pem` | — | **Required**, sensitive |

---

## Azure — AKS + PostgreSQL Flexible Server

**What gets created:** Resource group, VNet with delegated PostgreSQL subnet, Azure Database for PostgreSQL Flexible Server (private DNS), AKS cluster with cluster-autoscaler, Log Analytics workspace, and the cyoda Helm release.

### Quickstart

```bash
# 1. Authenticate
az login
az account set --subscription YOUR_SUBSCRIPTION_ID

# 2. Enter the Azure directory
cd deploy/terraform/azure

# 3. Copy and edit variables
cp terraform.tfvars.example terraform.tfvars
# Edit terraform.tfvars: set subscription_id, db_admin_password, jwt_signing_key_pem

# 4. Initialise and apply
terraform init
terraform plan
terraform apply

# 5. Connect kubectl
$(terraform output -raw kubeconfig_command)

# 6. Verify
kubectl get pods -n cyoda
```

### Key variables

| Variable | Default | Description |
|----------|---------|-------------|
| `subscription_id` | — | **Required** Azure subscription ID |
| `location` | `eastus` | Azure region |
| `node_vm_size` | `Standard_D2s_v3` | AKS node VM size |
| `db_sku_name` | `B_Standard_B1ms` | PostgreSQL Flexible Server SKU |
| `db_admin_password` | — | **Required**, sensitive |
| `jwt_signing_key_pem` | — | **Required**, sensitive |

---

## Remote State (recommended for teams)

Store Terraform state in a backend before running in CI or with teammates.

**AWS S3:**
```hcl
terraform {
  backend "s3" {
    bucket         = "my-tf-state"
    key            = "cyoda/aws/terraform.tfstate"
    region         = "us-east-1"
    encrypt        = true
    dynamodb_table = "terraform-locks"
  }
}
```

**GCP GCS:**
```hcl
terraform {
  backend "gcs" {
    bucket = "my-tf-state"
    prefix = "cyoda/gcp"
  }
}
```

**Azure Blob:**
```hcl
terraform {
  backend "azurerm" {
    resource_group_name  = "tf-state-rg"
    storage_account_name = "mytfstate"
    container_name       = "tfstate"
    key                  = "cyoda.terraform.tfstate"
  }
}
```

---

## Teardown

```bash
# Disable deletion protection first (RDS / Cloud SQL / Azure)
terraform apply -var="db_deletion_protection=false"
terraform destroy
```

> **Warning:** `terraform destroy` deletes all provisioned resources including the database. Ensure you have a backup before destroying a production deployment.

---

## Security notes

- Postgres DSN and JWT signing key are stored only in Kubernetes Secrets, never in ConfigMaps or environment variables.
- RDS / Cloud SQL / Azure PostgreSQL instances are configured as private (no public endpoint).
- EKS/GKE/AKS node-to-control-plane communication is encrypted.
- `deletion_protection` is enabled on database instances. Disable explicitly before `terraform destroy`.
- Never commit `terraform.tfvars` or `signing-key.pem` to version control.
