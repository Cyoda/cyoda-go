# deploy-terraform skill

Use this skill when the user asks how to deploy cyoda-go to AWS, GCP, or Azure, or when they ask about the Terraform infrastructure in `deploy/terraform/`.

---

## What the Terraform configs provision

Each `deploy/terraform/<provider>/` directory is a standalone root module:

| Provider | Compute | Database | Directory |
|----------|---------|----------|-----------|
| AWS | EKS | RDS PostgreSQL 16 (encrypted, private) | `deploy/terraform/aws/` |
| GCP | GKE Standard | Cloud SQL PostgreSQL 16 (private IP) | `deploy/terraform/gcp/` |
| Azure | AKS | PostgreSQL Flexible Server (private DNS) | `deploy/terraform/azure/` |

All three share the `deploy/terraform/modules/cyoda-helm/` module, which creates Kubernetes Secrets and deploys the Helm chart (`deploy/helm/cyoda`).

---

## Common pre-requisites for all providers

1. **Terraform >= 1.6** — `terraform -v`
2. **kubectl** — `kubectl version --client`
3. **Helm >= 3.14** — `helm version`
4. **RSA signing key** for JWT:
   ```bash
   openssl genrsa -out signing-key.pem 4096
   ```
   Keep this file out of version control.

---

## AWS deployment

### Authenticate
```bash
aws configure
# or set AWS_PROFILE / AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY
```

### Deploy
```bash
cd deploy/terraform/aws
cp terraform.tfvars.example terraform.tfvars
# Edit terraform.tfvars — mandatory: db_password, jwt_signing_key_pem
terraform init
terraform plan
terraform apply
$(terraform output -raw kubeconfig_command)   # updates ~/.kube/config
kubectl get pods -n cyoda
```

### Defaults
- Region: `us-east-1`
- Node: `t3.medium`, 2 nodes
- RDS: `db.t3.micro`, 20 GiB, PostgreSQL 16, encryption on

### Production checklist
- Set `db_multi_az = true`
- Increase `db_instance_class` (e.g. `db.t3.small` or higher)
- Increase `node_instance_type` and `node_desired_size`
- Add remote state backend (S3 + DynamoDB lock)

---

## GCP deployment

### Enable APIs (one-time)
```bash
gcloud services enable \
  container.googleapis.com \
  sqladmin.googleapis.com \
  servicenetworking.googleapis.com \
  --project=YOUR_PROJECT_ID
```

### Authenticate
```bash
gcloud auth application-default login
```

### Deploy
```bash
cd deploy/terraform/gcp
cp terraform.tfvars.example terraform.tfvars
# Edit terraform.tfvars — mandatory: project_id, db_password, jwt_signing_key_pem
terraform init
terraform plan
terraform apply
$(terraform output -raw kubeconfig_command)
kubectl get pods -n cyoda
```

### Defaults
- Region: `us-central1`
- Node: `e2-standard-2`, 2 nodes
- Cloud SQL: `db-g1-small`, 20 GiB, PostgreSQL 16, private IP

### Production checklist
- Set `db_availability_type = "REGIONAL"` for HA
- Increase `db_tier` (e.g. `db-custom-2-7680`)
- Add remote state backend (GCS bucket)

---

## Azure deployment

### Authenticate
```bash
az login
az account set --subscription YOUR_SUBSCRIPTION_ID
```

### Deploy
```bash
cd deploy/terraform/azure
cp terraform.tfvars.example terraform.tfvars
# Edit terraform.tfvars — mandatory: subscription_id, db_admin_password, jwt_signing_key_pem
terraform init
terraform plan
terraform apply
$(terraform output -raw kubeconfig_command)
kubectl get pods -n cyoda
```

### Defaults
- Location: `eastus`
- Node: `Standard_D2s_v3`, 2 nodes
- PostgreSQL Flexible Server: `B_Standard_B1ms`, 32 GiB, PostgreSQL 16, private VNet

### Production checklist
- Upgrade `db_sku_name` to `GP_Standard_D2s_v3` or higher
- Set `db_zone` in combination with AKS zone pinning
- Add remote state backend (Azure Blob Storage)

---

## Accessing cyoda after deployment

```bash
# Get the LoadBalancer external IP / hostname
kubectl get svc -n cyoda

# Health check
curl http://<EXTERNAL_IP>:8080/health

# API (mock auth mode — development only)
curl http://<EXTERNAL_IP>:8080/api/v1/entity/MyModel
```

For production, set `CYODA_IAM_MODE=jwt` via `extraEnv` in `terraform.tfvars`:
```hcl
# in terraform.tfvars (all three providers support this via the module's extra_env variable)
# Pass as a Helm values override instead:
# helm upgrade cyoda ./deploy/helm/cyoda --set extraEnv[0].name=CYODA_IAM_MODE --set extraEnv[0].value=jwt
```

---

## Teardown

```bash
# Deletion protection is ON by default — disable it first
# RDS: aws rds modify-db-instance --db-instance-identifier cyoda-postgres --no-deletion-protection
# Cloud SQL: gcloud sql instances patch cyoda-postgres --no-deletion-protection
# Azure: az postgres flexible-server update --name cyoda-postgres ... (no deletion protection in Flexible Server tier)

terraform destroy
```

---

## Variables reference

Full variable descriptions are in each `variables.tf`. The `terraform.tfvars.example` files in each directory are annotated and ready to copy.

The shared `modules/cyoda-helm/variables.tf` lists all Helm-layer variables (replicas, log level, service type, extra env, resource requests/limits, bootstrap M2M client).

---

## Troubleshooting

| Symptom | Likely cause | Fix |
|---------|-------------|-----|
| Pods stuck in `Init:0/1` | Migration job failed | `kubectl logs job/cyoda-migrate -n cyoda` |
| `ImagePullBackOff` | Wrong image tag | Set `cyoda_image_tag` to a published tag |
| `CrashLoopBackOff` | Bad Postgres DSN or missing JWT key | Check `kubectl logs -n cyoda deploy/cyoda` |
| RDS not reachable | Security group mismatch | Verify EKS node SG is in `aws_security_group.rds` ingress |
| Cloud SQL not reachable | VPC peering not ready | Wait for `google_service_networking_connection` to apply |
| Azure Postgres not reachable | DNS not propagated | Verify `azurerm_private_dns_zone_virtual_network_link` is applied |