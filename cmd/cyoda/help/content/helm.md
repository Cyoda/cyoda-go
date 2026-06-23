---
topic: helm
title: "helm — Helm chart for Kubernetes deployment"
stability: stable
see_also:
  - run
  - config
  - config.database
  - config.auth
  - quickstart
---

# helm

## NAME

helm — the `deploy/helm/cyoda` Helm chart: values, Kubernetes objects, secrets provisioning, and install/upgrade commands.

## SYNOPSIS

```
helm install cyoda ./deploy/helm/cyoda \
  --set postgres.existingSecret=cyoda-pg \
  --set jwt.existingSecret=cyoda-jwt
```

## DESCRIPTION

The chart at `deploy/helm/cyoda` deploys cyoda-go on Kubernetes as a StatefulSet backed by an external PostgreSQL database. The chart requires Kubernetes `>=1.31.0`. Chart version: `0.1.0`. App version: `0.1.0` (synchronized to the binary by the `bump-chart-appversion.yml` CI workflow).

The chart is not published to a Helm repository. Install directly from a local checkout or from the GitHub repository source tree. The canonical install form is `helm install cyoda ./deploy/helm/cyoda`.

Cyoda-go pods are stateless: all persistent state is in PostgreSQL. The chart renders a StatefulSet (not a Deployment) to give each pod a stable DNS identity for gossip peer discovery. Pod management policy is `Parallel` — pods start simultaneously rather than sequentially. Cluster mode is always enabled at the chart level; at `replicas=1` the binary runs as a cluster of one.

Credentials (Postgres DSN, JWT signing key, HMAC secret, metrics bearer token, optional bootstrap client secret) are never stored in the ConfigMap. They are mounted via projected Secret volumes and read by the binary through `CYODA_*_FILE` env vars.

## CHART REPOSITORY

The chart is not yet published to an OCI registry or a tgz repository. Install from a local path:

```
helm install cyoda ./deploy/helm/cyoda [--set ...]
```

For a GitOps workflow, package the chart manually:

```
helm package ./deploy/helm/cyoda
helm install cyoda cyoda-0.1.0.tgz [--set ...]
```

## VALUES

All top-level keys in `deploy/helm/cyoda/values.yaml`, with type, default, and purpose.

**`replicas`** — integer — default `1`
Number of cyoda pods. Scale up for multi-node cluster. At `replicas=1` the binary runs as a "cluster of one". When `autoscaling.enabled=true`, the HPA owns the replica count and the `replicas` value becomes the initial scale only.

**`logLevel`** — string — default `info`
Log level. Accepted values: `debug`, `info`, `warn`, `error`. Written to the ConfigMap as `CYODA_LOG_LEVEL`.

**`image.repository`** — string — default `ghcr.io/cyoda/cyoda`
Container image repository.

**`image.tag`** — string — default `""` (resolved to `.Chart.AppVersion`)
Image tag. When empty, the chart uses `.Chart.AppVersion`.

**`image.pullPolicy`** — string — default `IfNotPresent`
Image pull policy. Accepted values: `Always`, `IfNotPresent`, `Never`.

**`imagePullSecrets`** — list — default `[]`
List of `{name: <secretName>}` entries for private registries. Example: `[{name: ghcr-pull-secret}]`.

**`resources.requests.cpu`** — string — default `100m`
CPU request for cyoda containers.

**`resources.requests.memory`** — string — default `256Mi`
Memory request for cyoda containers.

**`resources.limits.cpu`** — string — default `1000m`
CPU limit for cyoda containers.

**`resources.limits.memory`** — string — default `512Mi`
Memory limit for cyoda containers.

**`postgres.existingSecret`** — string — default `""` — **REQUIRED**
Name of the Kubernetes Secret containing the Postgres DSN. The Secret must exist before install. The DSN value must be a full connection string: `postgres://user:pass@host:5432/db?sslmode=require`.

**`postgres.existingSecretKey`** — string — default `dsn`
Key within `postgres.existingSecret` whose value is the Postgres DSN.

**`jwt.existingSecret`** — string — default `""` — **REQUIRED**
Name of the Kubernetes Secret containing the PEM-encoded RSA private key for JWT signing.

**`jwt.existingSecretKey`** — string — default `signing-key.pem`
Key within `jwt.existingSecret` whose value is the PEM-encoded RSA private key.

**`jwt.issuer`** — string — default `cyoda`
JWT issuer claim. Written to ConfigMap as `CYODA_JWT_ISSUER`.

**`jwt.expirySeconds`** — integer — default `3600`
JWT token expiry in seconds. Written to ConfigMap as `CYODA_JWT_EXPIRY_SECONDS`.

**`cluster.hmacSecret.existingSecret`** — string — default `""`
Name of an operator-managed Secret containing the HMAC secret. When empty, the chart auto-generates the Secret on first install using `lookup` to detect existing state. GitOps controllers (Argo CD) must set this to a pre-created Secret; the chart fails with an error if rendered without live cluster access (e.g. `helm template`, `--dry-run`) and no `existingSecret` is provided.

**`cluster.hmacSecret.existingSecretKey`** — string — default `secret`
Key within the HMAC Secret whose value is the hex-encoded HMAC secret. The binary reads it via `CYODA_HMAC_SECRET_FILE` and decodes hex to raw bytes.

**`bootstrap.clientId`** — string — default `""`
Bootstrap M2M client ID. Bootstrap provisioning is opt-in. When empty, no bootstrap Secret is rendered and no bootstrap credential is set. When non-empty, the chart provisions the bootstrap M2M client. The binary's coupled predicate (both ID and Secret set, or both empty) applies.

**`bootstrap.clientSecret.existingSecret`** — string — default `""`
Name of an operator-managed Secret containing the bootstrap client secret. When `bootstrap.clientId` is non-empty and this is empty, the chart auto-generates the Secret. GitOps safety guard applies (same pattern as HMAC).

**`bootstrap.clientSecret.existingSecretKey`** — string — default `secret`
Key within the bootstrap client Secret.

**`bootstrap.tenantId`** — string — default `default-tenant`
Bootstrap tenant ID. Written to ConfigMap as `CYODA_BOOTSTRAP_TENANT_ID`.

**`bootstrap.userId`** — string — default `admin`
Bootstrap user ID. Written to ConfigMap as `CYODA_BOOTSTRAP_USER_ID`.

**`bootstrap.roles`** — string — default `ROLE_ADMIN,ROLE_M2M`
Comma-separated roles for the bootstrap client. Written to ConfigMap as `CYODA_BOOTSTRAP_ROLES`.

**`extraEnv`** — list — default `[]`
Arbitrary additional env vars injected into the StatefulSet container. Each entry is `{name, value}` or `{name, valueFrom}`. Use for OTel configuration (`CYODA_OTEL_ENABLED`, `OTEL_EXPORTER_OTLP_ENDPOINT`, etc.), feature flags, and tuning knobs. Do not set `CYODA_*_FILE` credential vars or the four chart-managed credential env vars here — the chart sets those and Kubernetes rejects duplicates.

**`service.type`** — string — default `ClusterIP`
Kubernetes Service type for the main Service (ports 8080, 9090, 9091). Accepted values: `ClusterIP`, `NodePort`, `LoadBalancer`.

**`gateway.enabled`** — boolean — default `true`
Enable Gateway API routing. Renders `HTTPRoute` (port 8080) and `GRPCRoute` (port 9090). Mutually exclusive with `ingress.enabled`; both enabled triggers a `fail`. When `true`, `gateway.parentRefs` MUST be set to a non-empty list of operator-provided Gateway references; an empty list causes install to fail.

**`gateway.parentRefs`** — list — default `[]` — **REQUIRED when `gateway.enabled=true`**
List of Gateway API parent references (operator-provided Gateway). The chart does not render the Gateway itself.

**`gateway.http.hostnames`** — list — default `[]`
HTTP hostnames for the `HTTPRoute`. When empty, the route matches all hostnames.

**`gateway.grpc.hostnames`** — list — default `[]`
gRPC hostnames for the `GRPCRoute`. When empty, the route matches all hostnames.

**`ingress.enabled`** — boolean — default `false`
Enable Ingress routing (transitional; `gateway.enabled=true` is preferred). Mutually exclusive with `gateway.enabled`.

**`ingress.className`** — string — default `""`
`ingressClassName` for the Ingress objects.

**`ingress.http.host`** — string — default `""`
Hostname for the HTTP Ingress rule.

**`ingress.http.paths`** — list — default `[{path: /, pathType: Prefix}]`
Path rules for the HTTP Ingress.

**`ingress.http.annotations`** — map — default `{}`
Annotations on the HTTP Ingress object.

**`ingress.http.tls`** — list — default `[]`
TLS configuration for the HTTP Ingress.

**`ingress.grpc.host`** — string — default `""`
Hostname for the gRPC Ingress rule.

**`ingress.grpc.paths`** — list — default `[{path: /, pathType: Prefix}]`
Path rules for the gRPC Ingress.

**`ingress.grpc.annotations`** — map — default `{nginx.ingress.kubernetes.io/backend-protocol: GRPC}`
Annotations on the gRPC Ingress object.

**`ingress.grpc.tls`** — list — default `[]`
TLS configuration for the gRPC Ingress.

**`monitoring.metricsBearer.existingSecret`** — string — default `""`
Name of an operator-managed Secret containing the static bearer token for `GET /metrics` authentication. When empty, the chart auto-generates the Secret. GitOps safety guard applies.

**`monitoring.metricsBearer.existingSecretKey`** — string — default `bearer`
Key within the metrics bearer Secret.

**`monitoring.serviceMonitor.enabled`** — boolean — default `false`
Render a `ServiceMonitor` (Prometheus Operator CRD). When enabled, Prometheus Operator scrapes `GET :9091/metrics` using the bearer token from `monitoring.metricsBearer` Secret.

**`monitoring.serviceMonitor.interval`** — string — default `30s`
Prometheus scrape interval.

**`monitoring.serviceMonitor.labels`** — map — default `{}`
Additional labels on the `ServiceMonitor` (used by Prometheus Operator's `serviceMonitorSelector`).

**`networkPolicy.enabled`** — boolean — default `true`
Render a `NetworkPolicy`. Restricts port 9091 (admin/metrics) ingress to namespaces declared in `metricsFromNamespaces`. Restricts port 7946 (gossip) ingress to chart-managed pods. Ports 8080 and 9090 are unrestricted at the NetworkPolicy layer (boundary is the Gateway/Ingress). Requires a CNI that enforces NetworkPolicy (Calico, Cilium, Weave). kindnet and some managed CNIs do not enforce NetworkPolicy — set `enabled=false` on those clusters.

**`networkPolicy.metricsFromNamespaces`** — list — default `[{matchLabels: {kubernetes.io/metadata.name: monitoring}}]`
Namespace selectors permitted to reach port 9091. Must be non-empty when `networkPolicy.enabled=true`.

**`autoscaling.enabled`** — boolean — default `false`
Render a `HorizontalPodAutoscaler` targeting the StatefulSet. When enabled, the HPA owns the replica count; the static `replicas` value becomes the initial count only.

**`autoscaling.minReplicas`** — integer — default `1`
HPA minimum replica count.

**`autoscaling.maxReplicas`** — integer — default `3`
HPA maximum replica count.

**`autoscaling.targetCPUUtilizationPercentage`** — integer — default `80`
HPA CPU utilization target. Omit or set to `0` to disable CPU-based scaling.

**`autoscaling.targetMemoryUtilizationPercentage`** — integer — not set by default
HPA memory utilization target. Uncomment in `values.yaml` to enable memory-based scaling.

**`autoscaling.behavior`** — map — default `{}`
HPA `behavior` block for stabilization windows and scale-up/scale-down policies (`autoscaling/v2` schema).

**`migrate.activeDeadlineSeconds`** — integer — default `600`
`activeDeadlineSeconds` for the pre-upgrade migration Job.

**`migrate.backoffLimit`** — integer — default `2`
`backoffLimit` for the migration Job.

**`migrate.resources.requests.cpu`** — string — default `100m`
CPU request for the migration Job container.

**`migrate.resources.requests.memory`** — string — default `128Mi`
Memory request for the migration Job container.

**`migrate.resources.limits.cpu`** — string — default `500m`
CPU limit for the migration Job container.

**`migrate.resources.limits.memory`** — string — default `256Mi`
Memory limit for the migration Job container.

**`podDisruptionBudget.enabled`** — boolean — default `true`
Render a `PodDisruptionBudget`. Rendered only when `replicas > 1` or `autoscaling.enabled=true` with `maxReplicas > 1`.

**`podDisruptionBudget.minAvailable`** — integer — default `1`
Minimum available pods during voluntary disruptions.

**`serviceAccount.create`** — boolean — default `true`
Create a dedicated `ServiceAccount`. The ServiceAccount is a Helm pre-install/pre-upgrade hook with weight `-10` so it exists before the migration Job (weight `0`).

**`serviceAccount.name`** — string — default `""` (resolved to the chart fullname)
Name of the ServiceAccount. When empty, the chart fullname is used.

**`serviceAccount.annotations`** — map — default `{}`
Annotations on the ServiceAccount (e.g. for IRSA/Workload Identity).

**`podAnnotations`** — map — default `{}`
Annotations added to all pods in the StatefulSet.

**`podLabels`** — map — default `{}`
Labels added to all pods in the StatefulSet.

**`nodeSelector`** — map — default `{}`
Node selector for the StatefulSet pod spec.

**`tolerations`** — list — default `[]`
Tolerations for the StatefulSet pod spec.

**`affinity`** — map — default `{}`
Affinity rules for the StatefulSet pod spec.

**`nameOverride`** — string — default `""`
Override the chart name component of generated resource names.

**`fullnameOverride`** — string — default `""`
Override the full generated resource name prefix.

## CRDS / OBJECTS

The chart renders the following Kubernetes objects. Conditional objects note their enabling value.

**Always rendered:**

- `StatefulSet` (`apps/v1`) — the cyoda workload. `podManagementPolicy: Parallel`. `updateStrategy: RollingUpdate`. No `volumeClaimTemplates` (cyoda is stateless vs. PostgreSQL). Mounts a projected Secret volume at `/etc/cyoda/secrets` (mode `0400`) and an `emptyDir` at `/tmp`. Runs as UID/GID 65532, non-root, `readOnlyRootFilesystem: true`, `allowPrivilegeEscalation: false`, all capabilities dropped, `seccompProfile: RuntimeDefault`.
- `Service` (`v1`) — ClusterIP Service exposing ports `8080` (http), `9090` (grpc), `9091` (metrics).
- `Service` (headless, `v1`) — `clusterIP: None`, `publishNotReadyAddresses: true`. Exposes port `7946` TCP and UDP for gossip (memberlist). Used as the `serviceName` for the StatefulSet.
- `ConfigMap` (`v1`) — non-sensitive env vars loaded via `envFrom`. Contains: `CYODA_HTTP_PORT`, `CYODA_GRPC_PORT`, `CYODA_ADMIN_PORT`, `CYODA_ADMIN_BIND_ADDRESS`, `CYODA_METRICS_REQUIRE_AUTH`, `CYODA_IAM_MODE`, `CYODA_REQUIRE_JWT`, `CYODA_STORAGE_BACKEND`, `CYODA_POSTGRES_AUTO_MIGRATE`, `CYODA_CLUSTER_ENABLED`, `CYODA_SEED_NODES`, `CYODA_LOG_LEVEL`, `CYODA_JWT_ISSUER`, `CYODA_JWT_EXPIRY_SECONDS`, `CYODA_BOOTSTRAP_TENANT_ID`, `CYODA_BOOTSTRAP_USER_ID`, `CYODA_BOOTSTRAP_ROLES`, and `CYODA_BOOTSTRAP_CLIENT_ID` (when `bootstrap.clientId` is set). Is a Helm pre-install/pre-upgrade hook with weight `-10`.
- `Job` (`batch/v1`) — migration Job running `cyoda migrate`. Helm pre-install/pre-upgrade hook (weight `0`, delete policy `before-hook-creation,hook-succeeded`). Mounts only the Postgres DSN Secret (principle of least privilege). Uses `restartPolicy: Never`.
- `NetworkPolicy` (`networking.k8s.io/v1`) — rendered when `networkPolicy.enabled=true`. (Conditional.)
- `Secret` (HMAC) — rendered when `cluster.hmacSecret.existingSecret=""`. Manages the hex-encoded HMAC secret. Auto-generates on first install; reuses on re-render via `lookup`. GitOps safety guard: fails if rendered without live cluster access. (Conditional.)
- `Secret` (metrics bearer) — rendered when `monitoring.metricsBearer.existingSecret=""`. Manages the static bearer token for `/metrics`. Auto-generates (48-char alphanumeric). GitOps safety guard applies. (Conditional.)

**Conditional:**

- `ServiceAccount` (`v1`) — rendered when `serviceAccount.create=true`. Helm hook weight `-10`.
- `Secret` (bootstrap) — rendered when `bootstrap.clientId != ""` and `bootstrap.clientSecret.existingSecret=""`. Auto-generates (48-char alphanumeric). GitOps safety guard applies.
- `HorizontalPodAutoscaler` (`autoscaling/v2`) — rendered when `autoscaling.enabled=true`.
- `PodDisruptionBudget` (`policy/v1`) — rendered when `podDisruptionBudget.enabled=true` and `replicas > 1` or `autoscaling.maxReplicas > 1`.
- `HTTPRoute` (`gateway.networking.k8s.io/v1`) — rendered when `gateway.enabled=true`. Routes port 8080.
- `GRPCRoute` (`gateway.networking.k8s.io/v1`) — rendered when `gateway.enabled=true`. Routes port 9090.
- `Ingress` (HTTP, `networking.k8s.io/v1`) — rendered when `ingress.enabled=true`. Mutually exclusive with `gateway.enabled`.
- `Ingress` (gRPC, `networking.k8s.io/v1`) — rendered when `ingress.enabled=true`.
- `ServiceMonitor` (`monitoring.coreos.com/v1`) — rendered when `monitoring.serviceMonitor.enabled=true`. References the metrics bearer Secret for scrape authentication.

## GENERATING SECRETS

**JWT signing key** — generate an RSA-2048 private key and load it into a Kubernetes Secret:

```
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out signing.pem
kubectl create secret generic cyoda-jwt -n cyoda \
  --from-file=signing-key.pem=signing.pem
```

**HMAC secret** — generate 32 bytes of entropy (64 hex chars) and load into a Kubernetes Secret:

```
kubectl create secret generic cyoda-hmac -n cyoda \
  --from-literal=secret="$(openssl rand -hex 32)"
```

See `quickstart` for accepted key formats and format-specific `openssl` commands.

## SECRETS PROVISIONING

The chart never stores credentials in the ConfigMap. All credentials are mounted via a projected Secret volume at `/etc/cyoda/secrets` and read by the binary through `CYODA_*_FILE` env vars. The file paths and corresponding env vars are set in the StatefulSet env block:

- `CYODA_POSTGRES_URL_FILE=/etc/cyoda/secrets/postgres-dsn` — sourced from `postgres.existingSecret` key `postgres.existingSecretKey`.
- `CYODA_JWT_SIGNING_KEY_FILE=/etc/cyoda/secrets/jwt-signing-key.pem` — sourced from `jwt.existingSecret` key `jwt.existingSecretKey`.
- `CYODA_HMAC_SECRET_FILE=/etc/cyoda/secrets/hmac-secret` — sourced from the chart-managed or operator-provided HMAC Secret.
- `CYODA_METRICS_BEARER_FILE=/etc/cyoda/secrets/metrics-bearer` — sourced from the chart-managed or operator-provided metrics bearer Secret.
- `CYODA_BOOTSTRAP_CLIENT_SECRET_FILE=/etc/cyoda/secrets/bootstrap-client-secret` — sourced from the chart-managed or operator-provided bootstrap Secret. Mounted only when `bootstrap.clientId` is non-empty.

The projected volume `defaultMode` is `0400` (owner read-only). The pod `securityContext.fsGroup=65532` ensures mounted Secret files are readable by the non-root container user.

The migration Job mounts only the Postgres DSN Secret (principle of least privilege). It does not receive JWT, HMAC, metrics bearer, or bootstrap credentials.

**GitOps safety guard (HMAC, metrics bearer, bootstrap secrets):** When `existingSecret` is empty, the chart uses `lookup` to detect whether the Secret already exists. On first install with live cluster access, it generates a random value. On subsequent renders, it reuses the existing value. When `lookup` returns no result (because Helm is run without live cluster access — `helm template`, `--dry-run`, Argo CD, first-time `--create-namespace`), the chart fails with an explicit error message. To avoid this: either pre-create the Secret and set `existingSecret`, use `external-secrets-operator`, or create the namespace first with `kubectl create namespace`.

**CYODA_METRICS_REQUIRE_AUTH:** The ConfigMap always sets `CYODA_METRICS_REQUIRE_AUTH=true` on Helm deployments. This forces the binary's coupled-predicate validator to refuse startup if the metrics bearer Secret is absent or empty, providing a belt-and-braces guard against chart misconfiguration.

## INSTALL / UPGRADE / UNINSTALL

**Install (pre-create secrets, then install):**

```
kubectl create secret generic cyoda-pg \
  --from-literal=dsn="postgres://cyoda:secret@pg-host:5432/cyoda?sslmode=require"
kubectl create secret generic cyoda-jwt \
  --from-literal=signing-key.pem="$(cat signing.pem)"

helm install cyoda ./deploy/helm/cyoda \
  --namespace cyoda \
  --create-namespace \
  --set postgres.existingSecret=cyoda-pg \
  --set jwt.existingSecret=cyoda-jwt \
  --set cluster.hmacSecret.existingSecret=cyoda-hmac
```

Note: `--create-namespace` triggers the GitOps safety guard (namespace does not yet exist when Helm renders). Pre-create the namespace first:

```
kubectl create namespace cyoda
kubectl create secret generic cyoda-pg -n cyoda \
  --from-literal=dsn="postgres://cyoda:secret@pg-host:5432/cyoda?sslmode=require"
kubectl create secret generic cyoda-jwt -n cyoda \
  --from-literal=signing-key.pem="$(cat signing.pem)"

helm install cyoda ./deploy/helm/cyoda \
  --namespace cyoda \
  --set postgres.existingSecret=cyoda-pg \
  --set jwt.existingSecret=cyoda-jwt
```

**Upgrade:**

```
helm upgrade --install cyoda ./deploy/helm/cyoda \
  --namespace cyoda \
  --set postgres.existingSecret=cyoda-pg \
  --set jwt.existingSecret=cyoda-jwt \
  --reuse-values
```

**Uninstall:**

```
helm uninstall cyoda --namespace cyoda
```

Uninstall does not delete PersistentVolumeClaims, Secrets not managed by the chart, or the namespace. Delete chart-managed Secrets manually if desired:

```
kubectl delete secret cyoda-hmac cyoda-metrics-bearer -n cyoda
```

**Dry-run (template rendering without cluster access — HMAC/metrics secret must be pre-created):**

```
kubectl create secret generic cyoda-hmac -n cyoda --from-literal=secret=placeholder
kubectl create secret generic cyoda-metrics-bearer -n cyoda --from-literal=bearer=placeholder
helm template cyoda ./deploy/helm/cyoda \
  --namespace cyoda \
  --set postgres.existingSecret=cyoda-pg \
  --set jwt.existingSecret=cyoda-jwt \
  --set cluster.hmacSecret.existingSecret=cyoda-hmac \
  --set monitoring.metricsBearer.existingSecret=cyoda-metrics-bearer
```

## EXAMPLES

**Minimal install (pre-created postgres + jwt secrets):**

**Bare-cluster install** — `gateway.enabled=false` disables the Gateway API route objects. Use this form on clusters without Gateway API CRDs (kind, minikube, most vanilla clusters). The chart falls back to the built-in `Service` for traffic ingress.

```
kubectl create namespace cyoda
kubectl create secret generic cyoda-pg -n cyoda \
  --from-literal=dsn="postgres://cyoda:pass@db.example.com:5432/cyoda?sslmode=require"
kubectl create secret generic cyoda-jwt -n cyoda \
  --from-literal=signing-key.pem="$(cat signing.pem)"

helm install cyoda ./deploy/helm/cyoda \
  --namespace cyoda \
  --set postgres.existingSecret=cyoda-pg \
  --set jwt.existingSecret=cyoda-jwt \
  --set gateway.enabled=false
```

**Multi-replica cluster with HPA:**

```
helm install cyoda ./deploy/helm/cyoda \
  --namespace cyoda \
  --set postgres.existingSecret=cyoda-pg \
  --set jwt.existingSecret=cyoda-jwt \
  --set replicas=3 \
  --set autoscaling.enabled=true \
  --set autoscaling.minReplicas=3 \
  --set autoscaling.maxReplicas=10
```

**With OTel tracing (via extraEnv):**

```
helm install cyoda ./deploy/helm/cyoda \
  --namespace cyoda \
  --set postgres.existingSecret=cyoda-pg \
  --set jwt.existingSecret=cyoda-jwt \
  --set extraEnv[0].name=CYODA_OTEL_ENABLED \
  --set extraEnv[0].value=true \
  --set extraEnv[1].name=OTEL_EXPORTER_OTLP_ENDPOINT \
  --set extraEnv[1].value=http://otel-collector.monitoring.svc.cluster.local:4318 \
  --set extraEnv[2].name=OTEL_SERVICE_NAME \
  --set extraEnv[2].value=cyoda
```

**With ServiceMonitor for Prometheus Operator:**

```
helm install cyoda ./deploy/helm/cyoda \
  --namespace cyoda \
  --set postgres.existingSecret=cyoda-pg \
  --set jwt.existingSecret=cyoda-jwt \
  --set monitoring.serviceMonitor.enabled=true \
  --set monitoring.serviceMonitor.labels.release=prometheus
```

**With bootstrap M2M client:**

```
kubectl create secret generic cyoda-bootstrap -n cyoda \
  --from-literal=secret="$(openssl rand -base64 36)"

helm install cyoda ./deploy/helm/cyoda \
  --namespace cyoda \
  --set postgres.existingSecret=cyoda-pg \
  --set jwt.existingSecret=cyoda-jwt \
  --set bootstrap.clientId=m2m-api-client \
  --set bootstrap.clientSecret.existingSecret=cyoda-bootstrap \
  --set bootstrap.tenantId=acme \
  --set-string 'bootstrap.roles=ROLE_ADMIN\,ROLE_M2M'
```

Helm treats commas in `--set` as array separators. Use `--set-string` with an escaped comma (`\,`) or provide the value via a `values.yaml` file to preserve the single string.

**With Gateway API (requires operator-provided Gateway):**

```
helm install cyoda ./deploy/helm/cyoda \
  --namespace cyoda \
  --set postgres.existingSecret=cyoda-pg \
  --set jwt.existingSecret=cyoda-jwt \
  --set "gateway.parentRefs[0].name=prod-gateway" \
  --set "gateway.parentRefs[0].namespace=gateway-system" \
  --set "gateway.http.hostnames[0]=api.example.com" \
  --set "gateway.grpc.hostnames[0]=grpc.example.com"
```

## SEE ALSO

- run
- config
- config.database
- config.auth
- quickstart
