# Migrating from ingress-nginx to Gateway API

`ingress-nginx` was retired by SIG Network in March 2026. The cyoda
chart ships both `ingress.enabled=true` (transitional) and
`gateway.enabled=true` (recommended, default ON) so operators can
adopt Gateway API on their own timeline. This guide walks through the
migration end-to-end using the [Ingress2Gateway 1.0][i2g] tool.

[i2g]: https://kubernetes.io/blog/2026/03/20/ingress2gateway-1-0-release/

## Prerequisites

Before starting, you need:

- A Kubernetes 1.31+ cluster with Gateway API CRDs installed
  (`kubectl get crd | grep gateway.networking.k8s.io` returns at least
  `gatewayclasses`, `gateways`, `httproutes`, `grpcroutes`).
- A Gateway API implementation deployed in a platform namespace —
  Envoy Gateway, Contour, Cilium, or Istio. All four ship
  production-ready `HTTPRoute` + `GRPCRoute` support.
- The Ingress2Gateway 1.0 binary on your PATH:
  ```bash
  go install sigs.k8s.io/ingress2gateway/cmd/ingress2gateway@latest
  ```

## Step 1 — Render the existing Ingress objects

If you're already running cyoda with `ingress.enabled=true`, dump the
rendered `Ingress` objects so Ingress2Gateway can convert them:

```bash
helm template cyoda cyoda/cyoda -n cyoda --reuse-values \
  --show-only templates/ingress.yaml \
  --show-only templates/ingress-grpc.yaml \
  > /tmp/cyoda-ingress.yaml
```

These are what your in-cluster `Ingress` resources look like today —
including the `nginx.ingress.kubernetes.io/backend-protocol: GRPC`
annotation on the gRPC path that ingress-nginx-compatible controllers
need.

## Step 2 — Install a Gateway API implementation

Pick one of the supported controllers. Envoy Gateway is the most
common choice for cyoda deployments:

```bash
helm repo add eg https://gateway.envoyproxy.io/charts
helm install eg eg/gateway-helm -n envoy-gateway-system --create-namespace
```

Once installed, create a `Gateway` in a platform namespace that the
cyoda routes will attach to:

```yaml
# platform/gateway.yaml — operator-managed, NOT chart-rendered
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: platform-gateway
  namespace: gateway-system
spec:
  gatewayClassName: eg
  listeners:
    - name: http
      port: 80
      protocol: HTTP
      allowedRoutes:
        namespaces:
          from: All
    - name: grpc
      port: 8080
      protocol: HTTP
      allowedRoutes:
        namespaces:
          from: All
```

`kubectl apply -f platform/gateway.yaml`. Verify:

```bash
kubectl -n gateway-system get gateway platform-gateway -o yaml | yq .status
```

The `Programmed` condition should read `True`.

## Step 3 — Convert with Ingress2Gateway

Run Ingress2Gateway against the dumped Ingress resources. This is a
**hint generator**, not the source of truth — the chart's own Gateway
API templates render the canonical shape. Use the output to confirm
your understanding of what the cyoda chart will render:

```bash
ingress2gateway print --input-file /tmp/cyoda-ingress.yaml \
  --providers ingress-nginx > /tmp/cyoda-routes.yaml
```

Inspect `/tmp/cyoda-routes.yaml`. You'll see two route resources:

- An `HTTPRoute` carrying the cyoda HTTP API paths.
- A `GRPCRoute` (translated from the `backend-protocol: GRPC`
  annotation) for the gRPC service.

Compare against `helm template cyoda cyoda/cyoda --set
gateway.enabled=true --show-only templates/httproute.yaml` to see the
chart's canonical shape. Differences are usually:

- The chart attaches via `parentRefs` you supply via values — adjust
  `gateway.parentRefs[]` to match your `Gateway` name and namespace.
- The chart sets `hostnames` separately for HTTP and gRPC — populate
  `gateway.http.hostnames` and `gateway.grpc.hostnames`.

## Step 4 — Flip the chart values

With the Gateway in place, switch the chart from `ingress` to
`gateway`:

```bash
helm upgrade cyoda cyoda/cyoda -n cyoda --reuse-values \
  --set ingress.enabled=false \
  --set gateway.enabled=true \
  --set 'gateway.parentRefs[0].name=platform-gateway' \
  --set 'gateway.parentRefs[0].namespace=gateway-system' \
  --set gateway.http.hostnames[0]=cyoda.example.com \
  --set gateway.grpc.hostnames[0]=grpc.cyoda.example.com
```

The chart now renders `HTTPRoute` and `GRPCRoute` resources that
attach to your platform Gateway. The old `Ingress` resources are
deleted on this upgrade.

## Step 5 — Verify routing end-to-end

```bash
# HTTP
curl -fsSL https://cyoda.example.com/api/health
# expected: 200 OK

# gRPC (via grpcurl)
grpcurl -insecure grpc.cyoda.example.com:443 list
# expected: cyoda's gRPC services listed
```

If either fails, check:

- `kubectl -n cyoda get httproute,grpcroute -o yaml | yq .status` —
  the `Accepted` and `ResolvedRefs` conditions should both be `True`.
- `kubectl -n gateway-system get gateway platform-gateway -o yaml`
  for listener-level errors.
- Controller logs (varies by implementation —
  `kubectl -n envoy-gateway-system logs deploy/envoy-gateway` for
  Envoy Gateway).

## Step 6 — Decommission ingress-nginx

Once routing is stable for at least one full deploy cycle:

```bash
helm uninstall ingress-nginx -n ingress-nginx
kubectl delete namespace ingress-nginx
```

The transitional `Ingress` templates can stay in this chart's values
schema for any other workloads still mid-migration; cyoda itself no
longer needs them.

## Common pitfalls

- **Forgot to install Gateway API CRDs.** The chart fails to install
  with `no matches for kind "HTTPRoute"`. Install the CRDs from
  `https://github.com/kubernetes-sigs/gateway-api/releases`.
- **Cross-namespace `parentRefs` blocked by `ReferenceGrant`.** Some
  Gateway implementations require an explicit `ReferenceGrant` in the
  Gateway's namespace pointing back at cyoda's namespace. Render the
  policy alongside the Gateway, not from this chart.
- **gRPC routing returns HTTP 200 with empty body.** Almost always a
  protocol-mismatch — confirm the `Service` port targeting gRPC is
  named `grpc` and the route is a `GRPCRoute`, not `HTTPRoute`.

## See also

- [Gateway API project](https://gateway-api.sigs.k8s.io/) — official
  spec and documentation.
- [`gateway-api-policies.md`](./gateway-api-policies.md) — recommended
  `BackendTrafficPolicy` / `SecurityPolicy` overlays per controller.
- The chart's [`README.md`](../README.md) for the full values
  reference.
