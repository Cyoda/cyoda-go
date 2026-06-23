# cyoda Helm chart

Production-ready Helm deployment of cyoda-go backed by an external
Postgres, fronted by Gateway API (default) or a still-maintained
Ingress controller (transitional).

See `Chart.yaml` for the chart `version:`. `appVersion:` is pinned to the
binary release by `bump-chart-appversion.yml`.

## Installation

### Prerequisites

- Helm v4.1+ recommended (chart is `apiVersion: v2` and also installs
  cleanly from Helm v3.16+).
- Kubernetes 1.31+ (Gateway API CRDs required if using `gateway.enabled=true`).
- An existing Postgres instance reachable from the cluster, with a
  dedicated database and role for cyoda.
- A JWT RSA signing key. Generate with:
  ```bash
  openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 \
    -out jwt-signing-key.pem
  ```

### Create the required Secrets

```bash
kubectl create namespace cyoda

kubectl -n cyoda create secret generic cyoda-dsn \
  --from-literal=dsn='postgres://cyoda:REDACTED@pg.example.com:5432/cyoda?sslmode=require'

kubectl -n cyoda create secret generic cyoda-jwt \
  --from-file=signing-key.pem=./jwt-signing-key.pem
```

### Install

```bash
helm repo add cyoda https://cyoda.github.io/cyoda-go
helm repo update

helm install cyoda cyoda/cyoda -n cyoda \
  --set postgres.existingSecret=cyoda-dsn \
  --set jwt.existingSecret=cyoda-jwt \
  --set gateway.parentRefs[0].name=platform-gateway \
  --set gateway.parentRefs[0].namespace=gateway-system \
  --set gateway.http.hostnames[0]=cyoda.example.com \
  --set gateway.grpc.hostnames[0]=grpc.cyoda.example.com
```

### Enabling the bootstrap M2M client (optional)

By default the chart does NOT provision a bootstrap M2M client
(`bootstrap.clientId=""`). The binary runs cleanly in jwt mode via
JWKS / external signing keys alone.

To enable bootstrap, set `bootstrap.clientId`:

```bash
helm upgrade cyoda cyoda/cyoda -n cyoda --reuse-values \
  --set bootstrap.clientId=cyoda-bootstrap
```

The chart auto-generates the secret (or use
`bootstrap.clientSecret.existingSecret` for GitOps).

### Separate migration DSN (optional, two-role DB model)

By default the migration Job and the runtime pods share one DSN
(`postgres.existingSecret`). For least-privilege deployments you can run the
migration Job (which performs DDL) as a dedicated **owner** role while the
runtime StatefulSet connects as a non-owner **runtime** role. Set
`migrate.postgres.existingSecret` (and optionally `existingSecretKey`, default
`dsn`) to a Secret holding the owner DSN:

```bash
kubectl -n cyoda create secret generic cyoda-dsn-migrate \
  --from-literal=dsn='postgres://cyoda_owner:REDACTED@pg.example.com:5432/cyoda?sslmode=require'

helm upgrade cyoda cyoda/cyoda -n cyoda \
  --set migrate.postgres.existingSecret=cyoda-dsn-migrate
```

(combine the `--set migrate.postgres.existingSecret=…` flag with the flags
from your original `helm install` above.)

When `migrate.postgres.existingSecret` is unset, the migration Job falls back
to `postgres.existingSecret`, so existing single-DSN installs are unchanged.

### NetworkPolicy (default ON)

The chart ships with `networkPolicy.enabled=true`. This restricts port
9091 (`/livez`/`/readyz`/`/metrics`) ingress to namespaces the operator
declares in `networkPolicy.metricsFromNamespaces`, and restricts
gossip-port (7946) ingress to the chart's own pods. Defense in depth
alongside the bearer-gated `/metrics`: even if the scrape credential
leaks, scrape attempts from unauthorised namespaces don't reach the
pod at all.

Default `metricsFromNamespaces` points at a namespace labelled
`kubernetes.io/metadata.name: monitoring` — the
kube-prometheus-stack default. Override if your monitoring stack lives
elsewhere:

```yaml
networkPolicy:
  enabled: true
  metricsFromNamespaces:
    - matchLabels:
        kubernetes.io/metadata.name: prometheus-system
```

**If your CNI does not enforce NetworkPolicy** (kindnet, default EKS
without a secondary CNI, some on-prem setups) the rendered policy is a
no-op. Set `networkPolicy.enabled=false` explicitly in that case —
shipping with a rendered policy that doesn't actually restrict traffic
is a false-sense-of-security footgun. The bearer on `/metrics` is the
active guardrail there.

### Metrics scraping and `/metrics` authentication

The chart ships with `CYODA_METRICS_REQUIRE_AUTH=true` so the admin
listener's `/metrics` endpoint requires `Authorization: Bearer <token>`.
`/livez` and `/readyz` remain unauthenticated (kubelet probes can't
carry bearers). The chart auto-generates the bearer token as a
Kubernetes Secret on first install (or pass
`monitoring.metricsBearer.existingSecret` to manage it yourself).

When `monitoring.serviceMonitor.enabled=true`, the rendered
`ServiceMonitor` references the same Secret via `bearerTokenSecret` so
Prometheus Operator scrapes authenticate transparently with no
operator wiring. To hand-scrape for a smoke check:

```bash
kubectl -n cyoda port-forward svc/cyoda 9091:metrics &
BEARER=$(kubectl -n cyoda get secret cyoda-metrics-bearer \
  -o jsonpath='{.data.bearer}' | base64 -d)
curl -H "Authorization: Bearer $BEARER" http://localhost:9091/metrics | head
```

Rotation: delete the Kubernetes Secret and `helm upgrade` — the chart's
`lookup`+GitOps-guard pattern will generate a fresh token. Or pre-manage
the Secret via `existingSecret` and rotate it on your own schedule;
both the pod (via `_FILE`) and Prometheus (via `bearerTokenSecret`)
re-read automatically within one scrape interval.

### Scale to 3 replicas (cluster mode)

```bash
helm upgrade cyoda cyoda/cyoda -n cyoda \
  --reuse-values \
  --set replicas=3
```

No mode flip needed — cluster mode is always on; at replicas=1 it runs
as a "cluster of one".

## Using with GitOps (Argo CD)

The chart auto-generates the HMAC Secret via Helm's `lookup` function
on first install. **This does not work with Argo CD's default render
path** (which uses `helm template`, where `lookup` is a no-op). Without
mitigation, Argo CD would re-randomize the HMAC secret on every
reconcile, breaking gossip encryption and inter-node HTTP dispatch auth.

The chart catches this at render time and fails with an actionable
error message. To fix:

**Option A: pre-create the Secrets and pass `existingSecret`.**
Do this for every chart-managed Secret you want to keep stable across
GitOps reconciles — HMAC, the metrics bearer, and (if you set
`bootstrap.clientId`) the bootstrap client secret.

```bash
kubectl -n cyoda create secret generic cyoda-hmac \
  --from-literal=secret=$(openssl rand -hex 32)
kubectl -n cyoda create secret generic cyoda-metrics-bearer \
  --from-literal=bearer=$(openssl rand -base64 36)
```

```yaml
cluster:
  hmacSecret:
    existingSecret: cyoda-hmac
monitoring:
  metricsBearer:
    existingSecret: cyoda-metrics-bearer
```

If you also need the bootstrap M2M client (`bootstrap.clientId` set),
pre-create that Secret too:

```bash
kubectl -n cyoda create secret generic cyoda-bootstrap \
  --from-literal=secret=$(openssl rand -hex 32)
```

```yaml
bootstrap:
  clientId: cyoda-bootstrap
  clientSecret:
    existingSecret: cyoda-bootstrap
```

**Option B: use external-secrets-operator** to sync from a real secret
store (Vault, AWS Secrets Manager, etc.) into the Secret names.

## Reference topology (Gateway API + Cloudflare tunnel)

```
     ┌─────────────────────────┐
     │ External origin         │
     │ (Cloudflare tunnel etc) │
     └──────────┬──────────────┘
                │
     ┌──────────▼──────────────┐
     │ Gateway (platform ns)   │
     │ envoy-gateway, contour, │
     │ cilium, istio…          │
     └──┬────────────────────┬─┘
        │ HTTPRoute          │ GRPCRoute
        │                    │
    ┌───▼───┐            ┌───▼───┐
    │Service│            │Service│
    │cyoda: │            │cyoda: │
    │ http  │            │ grpc  │
    └───┬───┘            └───┬───┘
        │                    │
        └─────────┬──────────┘
                  │
             ┌────▼────┐
             │  cyoda  │
             │ pod(s)  │
             └─────────┘
```

## Migrating from ingress-nginx

`ingress-nginx` was retired by SIG Network in March 2026. The chart
ships `ingress.enabled=true` as a transitional affordance and
`gateway.enabled=true` (default) for Gateway API. For step-by-step
migration using [Ingress2Gateway 1.0][i2g], see
[`docs/migrating-from-ingress.md`](./docs/migrating-from-ingress.md).

[i2g]: https://kubernetes.io/blog/2026/03/20/ingress2gateway-1-0-release/

## Gateway API policy attachments (rate limiting, auth, WAF)

Gateway API 1.2 exposes per-controller policy attachments rather than
cross-controller annotations. The chart renders only `HTTPRoute` and
`GRPCRoute` — policy objects are deliberately operator-owned because
their shape and semantics differ by implementation. For concrete
`BackendTrafficPolicy` (rate limiting, retries) and `SecurityPolicy`
(JWT, CORS, IP allow-list) examples per controller (Envoy Gateway,
Cilium, Contour), see
[`docs/gateway-api-policies.md`](./docs/gateway-api-policies.md).

## Values reference

See [`values.yaml`](./values.yaml). Every key is documented inline.

## Troubleshooting

### "cluster.hmacSecret.existingSecret is required when the chart is rendered without live cluster access"

You're running `helm template`, `helm install --dry-run`, Argo CD
default path, or installing into a not-yet-created namespace. See
"Using with GitOps" above.

### `helm upgrade` hangs on the migration Job

Check logs: `kubectl logs -n cyoda job/cyoda-migrate-<release-revision>`.
If the migration is slow, increase `migrate.activeDeadlineSeconds`.
If the Job fails permanently, Helm rolls back values and old pods keep
serving — investigate before retrying.

### `CYODA_*_FILE` in `extraEnv` causes install to fail with duplicate env

Remove it. The chart sets all five credential env vars (postgres DSN,
JWT signing key, HMAC, bootstrap client secret, metrics bearer); to
change a credential, change the referenced `existingSecret`.
