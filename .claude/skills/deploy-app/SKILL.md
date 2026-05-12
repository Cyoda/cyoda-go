---
name: deploy-app
description: Phase-2 cyoda-go app operations on Kubernetes: update image, scale replicas, roll back, rotate secrets, debug pods, run migrations, get API tokens, uninstall. Use after infrastructure already exists. For initial infrastructure provisioning use /deploy-terraform.
allowed-tools: Bash(kubectl *) Bash(helm *) Bash(curl *) Read
---

## Current app state

!`kubectl get pods -n cyoda 2>/dev/null || echo "kubectl not configured or cluster not reachable"`

---

## Connect kubectl

```bash
# AWS
aws eks update-kubeconfig --region us-east-1 --name cyoda --profile cyoda

# GCP
gcloud container clusters get-credentials cyoda --region us-central1 --project YOUR_PROJECT_ID

# Azure
az aks get-credentials --resource-group cyoda-rg --name cyoda
```

---

## How the app is deployed

`terraform apply` runs `helm install cyoda ./deploy/helm/cyoda`, which creates:
- **StatefulSet** — stable pod identity for gossip clustering
- **Headless Service** — peer discovery
- **Service** (LoadBalancer) — external traffic
- **Migration Job** (`cyoda-migrate`) — runs DB migrations before pods start

Credentials live only in Kubernetes Secrets (`cyoda-postgres`, `cyoda-jwt`, `cyoda-hmac`).

---

## Verify

```bash
kubectl get all -n cyoda
kubectl get svc cyoda -n cyoda              # get EXTERNAL-IP
curl http://<EXTERNAL_IP>:8080/health
kubectl logs job/cyoda-migrate -n cyoda     # check migration job
```

---

## Update image

Via Terraform (preferred — keeps state in sync):
```bash
# set cyoda_image_tag = "v0.8.0" in terraform.tfvars, then:
terraform apply
```

Via Helm (faster, state drifts):
```bash
helm upgrade cyoda ./deploy/helm/cyoda --namespace cyoda --reuse-values --set image.tag=v0.8.0
kubectl rollout status statefulset/cyoda -n cyoda
```

---

## Scale

```bash
# Via Terraform: set cyoda_replicas = 3 in terraform.tfvars, then terraform apply

# Via Helm:
helm upgrade cyoda ./deploy/helm/cyoda --namespace cyoda --reuse-values --set replicas=3
```

Cluster mode is always on — new pods join via gossip automatically.

---

## Roll back

```bash
helm history cyoda -n cyoda
helm rollback cyoda -n cyoda          # previous revision
helm rollback cyoda 2 -n cyoda        # specific revision
kubectl rollout status statefulset/cyoda -n cyoda
```

---

## Change config (log level, env vars)

```bash
helm upgrade cyoda ./deploy/helm/cyoda --namespace cyoda --reuse-values \
  --set logLevel=debug \
  --set "extraEnv[0].name=CYODA_IAM_MODE" \
  --set "extraEnv[0].value=jwt"
```

---

## Run migrations manually

```bash
kubectl delete job cyoda-migrate -n cyoda
helm upgrade cyoda ./deploy/helm/cyoda --namespace cyoda --reuse-values
kubectl logs job/cyoda-migrate -n cyoda -f
```

---

## Rotate secrets

```bash
# Postgres DSN
kubectl create secret generic cyoda-postgres \
  --from-literal=dsn="postgres://user:newpass@host:5432/cyoda?sslmode=require" \
  --namespace cyoda --dry-run=client -o yaml | kubectl apply -f -
kubectl rollout restart statefulset/cyoda -n cyoda

# JWT signing key (invalidates all existing tokens)
kubectl create secret generic cyoda-jwt \
  --from-file=signing-key.pem=new-signing-key.pem \
  --namespace cyoda --dry-run=client -o yaml | kubectl apply -f -
kubectl rollout restart statefulset/cyoda -n cyoda
```

---

## Debug a failing pod

```bash
kubectl describe pod -l app.kubernetes.io/name=cyoda -n cyoda
kubectl logs -l app.kubernetes.io/name=cyoda -n cyoda --tail=100
kubectl exec -it statefulset/cyoda -n cyoda -- /bin/sh
```

---

## Get API token (JWT mode)

```bash
curl -s -X POST http://<EXTERNAL_IP>:8080/api/v1/auth/token \
  -H "Content-Type: application/json" \
  -d '{"clientId":"<CLIENT_ID>","clientSecret":"<CLIENT_SECRET>"}'
```

Mock mode (`CYODA_IAM_MODE=mock`) requires no token.

---

## Uninstall app (keep infra)

```bash
helm uninstall cyoda -n cyoda
kubectl delete namespace cyoda
```

To destroy infrastructure too: use `/deploy-terraform` → teardown section.
