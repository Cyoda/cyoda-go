# Gateway API policy attachments

The cyoda chart renders `HTTPRoute` and `GRPCRoute` resources that
attach to an operator-managed `Gateway`. **Policy objects** —
rate-limiting, JWT authn, CORS, IP allow-listing, retries, timeouts —
are deliberately **not** rendered by the chart. They differ in shape
and semantics across Gateway controllers; rendering one
controller's flavour would be silently inert under another.

This guide gives concrete `BackendTrafficPolicy` and `SecurityPolicy`
examples for the most common controllers, applied in the operator's
namespace alongside the chart-rendered routes.

## Envoy Gateway

[Envoy Gateway][eg] is the most common choice for cyoda deployments.
It exposes two policy CRDs that target Gateway API resources via
`targetRefs`:

- `BackendTrafficPolicy` — rate limiting, retries, timeouts, fault
  injection, circuit breakers.
- `SecurityPolicy` — JWT authn, OIDC, CORS, IP allow-list, basic
  auth, ext-authz.

[eg]: https://gateway.envoyproxy.io/docs/

### Rate limiting (BackendTrafficPolicy)

Limit the cyoda HTTP route to 100 requests per minute per client IP:

```yaml
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: BackendTrafficPolicy
metadata:
  name: cyoda-http-ratelimit
  namespace: cyoda
spec:
  targetRefs:
    - group: gateway.networking.k8s.io
      kind: HTTPRoute
      name: cyoda          # the chart-rendered HTTPRoute name
  rateLimit:
    type: Local
    local:
      rules:
        - limit:
            requests: 100
            unit: Minute
          clientSelectors:
            - sourceCIDR:
                value: 0.0.0.0/0
                type: Distinct
```

`type: Local` rate-limits per Envoy proxy instance (no external Redis
needed). For cluster-wide enforcement use `type: Global` and supply a
`backendRefs` to a Redis instance.

### JWT auth (SecurityPolicy)

Require an RS256 JWT signed by your IdP for every request to the HTTP
route, with `aud` claim matching `cyoda-api`:

```yaml
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: SecurityPolicy
metadata:
  name: cyoda-http-jwt
  namespace: cyoda
spec:
  targetRefs:
    - group: gateway.networking.k8s.io
      kind: HTTPRoute
      name: cyoda
  jwt:
    providers:
      - name: idp
        issuer: https://idp.example.com/
        audiences:
          - cyoda-api
        remoteJWKS:
          uri: https://idp.example.com/.well-known/jwks.json
```

`Authorization: Bearer …` requests with a missing or invalid token
get a `401`. The cyoda binary's own JWT validation runs on top — set
`CYODA_REQUIRE_JWT=true` and the trusted-key registry to enforce
defense in depth.

### CORS (SecurityPolicy)

Allow the cyoda admin UI origin to call the API:

```yaml
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: SecurityPolicy
metadata:
  name: cyoda-http-cors
  namespace: cyoda
spec:
  targetRefs:
    - group: gateway.networking.k8s.io
      kind: HTTPRoute
      name: cyoda
  cors:
    allowOrigins:
      - https://admin.cyoda.example.com
    allowMethods:
      - GET
      - POST
      - PUT
      - DELETE
    allowHeaders:
      - Authorization
      - Content-Type
    maxAge: 600s
```

The cyoda binary also has its own CORS middleware
(`CYODA_CORS_ALLOWED_ORIGINS`) — only one layer needs to set CORS
headers, so disable the binary's via `CYODA_CORS_ENABLED=false` if
you handle CORS at the gateway.

## Cilium Gateway

Cilium Gateway implements Gateway API natively but exposes per-route
Envoy customisation via `CiliumEnvoyConfig`. For most policies,
prefer `CiliumNetworkPolicy` at L7:

```yaml
apiVersion: cilium.io/v2
kind: CiliumNetworkPolicy
metadata:
  name: cyoda-l7-policy
  namespace: cyoda
spec:
  endpointSelector:
    matchLabels:
      app.kubernetes.io/name: cyoda
  ingress:
    - fromEndpoints:
        - matchLabels:
            "k8s:io.kubernetes.pod.namespace": gateway-system
      toPorts:
        - ports:
            - port: "8080"
              protocol: TCP
          rules:
            http:
              - method: "GET"
                path: "/api/.*"
              - method: "POST"
                path: "/api/.*"
```

For richer policy (JWT, OIDC, ext-authz), Cilium recommends Envoy
Gateway in front. Don't try to attach Envoy Gateway's CRDs to Cilium —
the `targetRefs` won't resolve.

## Contour

Contour supports Gateway API natively and provides `BackendTLSPolicy`
for upstream TLS:

```yaml
apiVersion: gateway.networking.k8s.io/v1alpha3
kind: BackendTLSPolicy
metadata:
  name: cyoda-backend-tls
  namespace: cyoda
spec:
  targetRefs:
    - group: ""
      kind: Service
      name: cyoda
  validation:
    caCertificateRefs:
      - kind: ConfigMap
        name: cyoda-ca-bundle
    hostname: cyoda.cyoda.svc.cluster.local
```

For richer policies (rate limiting, auth filters), Contour
recommends `HTTPProxy` (its predecessor CRD) attached to the same
backend `Service`. Note that `HTTPProxy` and `HTTPRoute` cannot
coexist on the same backend — pick one.

## Pattern: separate routes per concern

If a single policy needs to apply to a subset of paths only (e.g.
rate-limit `/api/entity/*` but not `/api/health`), split into two
chart-rendered `HTTPRoute`s by extending `gateway.http.routes` in
values. Each can carry its own `BackendTrafficPolicy` /
`SecurityPolicy` via `targetRefs`.

The chart's default rendering is a single route per protocol; the
`gateway.http.routes` extension point allows per-path-prefix routes.
See the chart `values.yaml` for the schema.

## Pitfalls

- **`targetRefs` to a non-existent route.** The policy is silently
  inert. `kubectl get backendtrafficpolicy -o yaml | yq .status` —
  the `Accepted` condition shows the resolution error.
- **Mixing controllers.** A `BackendTrafficPolicy` from Envoy Gateway
  attached to a route owned by Contour will not apply. Match
  controller and policy CRD.
- **Rate limit unit too coarse.** `Hour`-scoped buckets fill up in
  bursts and stall the client. Use `Minute` or `Second` for
  user-facing APIs.

## See also

- [Envoy Gateway tasks](https://gateway.envoyproxy.io/docs/tasks/) —
  comprehensive reference.
- [Gateway API policy attachment][gw-policy] — upstream specification.
- [`migrating-from-ingress.md`](./migrating-from-ingress.md) — moving
  off ingress-nginx.

[gw-policy]: https://gateway-api.sigs.k8s.io/reference/policy-attachment/
