---
topic: config.cors
title: "CORS configuration"
stability: stable
see_also:
  - config
  - run
---

# config.cors

## NAME

config.cors — Cross-Origin Resource Sharing (CORS) controls for the public HTTP surface.

## SYNOPSIS

cyoda supports four CORS modes: `disabled`, `loopback` (default), `wildcard`, and `allowlist`.
Configure the mode via `CYODA_CORS_ENABLED` and `CYODA_CORS_ALLOWED_ORIGINS`.

## OPTIONS

- `CYODA_CORS_ENABLED` — enable CORS middleware; set to `false` to disable and handle CORS at
  an upstream ingress/proxy layer (default: `true`)
- `CYODA_CORS_ALLOWED_ORIGINS` — comma-separated list of allowed origins, or `*` for wildcard
  mode (default: empty — loopback mode)

## MODES

The effective mode is determined by the combination of the two env vars:

- `CYODA_CORS_ENABLED=false` — **disabled** (regardless of `CYODA_CORS_ALLOWED_ORIGINS`)
- `CYODA_CORS_ENABLED=true`, `CYODA_CORS_ALLOWED_ORIGINS` empty — **loopback** (default)
- `CYODA_CORS_ENABLED=true`, `CYODA_CORS_ALLOWED_ORIGINS=*` — **wildcard**
- `CYODA_CORS_ENABLED=true`, `CYODA_CORS_ALLOWED_ORIGINS=https://example.com,...` — **allowlist**

### disabled

CORS middleware is not installed. No `Access-Control-*` headers are emitted. OPTIONS requests
return chi's default 405. Use this when CORS is handled at your ingress layer (nginx, Envoy,
cloud load balancer).

### loopback (default)

Only loopback origins are permitted: `http(s)://localhost`, `http(s)://127.0.0.1`, and
`http(s)://[::1]` on any port. Suitable for local development. Set
`CYODA_CORS_ALLOWED_ORIGINS` to permit additional origins.

### wildcard

`Access-Control-Allow-Origin: *` is emitted for all cross-origin requests. Credentials
(cookies, `Authorization` header) cannot be used with wildcard mode. Appropriate only for
fully public, stateless read APIs.

### allowlist

Only the origins listed in `CYODA_CORS_ALLOWED_ORIGINS` are permitted. Exact scheme+host+port
matching (no wildcards in individual entries). Origins must be absolute URIs with scheme and
host; paths and query strings are not permitted.

## BEHAVIOUR

The following headers are emitted by the CORS middleware when it is installed
(`CYODA_CORS_ENABLED=true`):

**On every response from the installed middleware (preflight, CORS request, or no-`Origin` pass-through):**

- `Vary: Origin` — always appended (never overwrites an existing `Vary` value).
  This instructs intermediate caches to key by `Origin` so that a mode change
  does not cause a stale no-`Origin` response to be served to an `Origin`-bearing
  request.

**Access-Control-Allow-Origin:**

- loopback mode: the matched origin is echoed literally; omitted if no match.
- allowlist mode: the matched origin is echoed literally; omitted if no match.
- wildcard mode: literal `*` for every request, never reflective of `Origin`.
- disabled mode: not emitted.

**On preflight responses only** (`OPTIONS` with `Origin` and
`Access-Control-Request-Method`):

- `Access-Control-Allow-Methods: GET, POST, PUT, PATCH, DELETE, OPTIONS` (static)
- `Access-Control-Allow-Headers: Authorization, Content-Type, traceparent, tracestate` (static)
- `Access-Control-Max-Age: 86400` (static)

These three headers are emitted on every preflight regardless of whether
the origin matched the policy. Only `Access-Control-Allow-Origin` is
omitted when the origin is rejected — a deployer debugging an allowlist
miss will see the static three present alongside the absent ACAO.

**`Access-Control-Allow-Credentials` is NOT emitted in v1.** Authentication is
bearer-in-`Authorization`; cookies and HTTP-auth are not used. Credentials mode
adds attack surface without functional benefit for this auth model.

## TENANT ISOLATION

CORS is a browser-side defence against unauthorized cross-origin reads. It is
**not** a tenant-isolation control. JWT claims and per-request authorization
checks in the data path enforce tenant boundaries. An allowlisted SPA serving
multiple tenants relies entirely on the auth layer, not CORS, to prevent
cross-tenant access. No CORS rule substitutes for or displaces JWT-based authz.

## DEPLOYMENT

**Local dev / docker compose**

No configuration needed. Loopback mode allows `http://localhost`, `http://127.0.0.1`,
and `http://[::1]` on any port by default. Suitable for Vite/webpack dev servers
and local docker-compose SPAs.

**Behind an ingress that handles its own CORS**

Set `CYODA_CORS_ENABLED=false` and configure CORS at the ingress (nginx, Envoy,
cloud load balancer). Do not let both the ingress and cyoda-go emit
`Access-Control-Allow-Origin`: a browser receiving two `Access-Control-Allow-Origin`
values will reject the response.

**Behind a reverse proxy with no CORS handling**

Set `CYODA_CORS_ALLOWED_ORIGINS=https://your.spa.host`. The proxy forwards
requests unchanged; cyoda-go's allowlist middleware emits the correct header
for the matching origin.

## PNA AND CSRF

**Private Network Access (PNA):** cyoda-go does not handle
`Access-Control-Request-Private-Network` / `Access-Control-Allow-Private-Network`.
Deployers needing browsers on a public origin to reach cyoda-go on a private
network should configure PNA at the ingress.

**CSRF:** CSRF is not a threat for bearer-in-header authentication. The SPA
explicitly attaches the bearer on each request rather than relying on ambient
credentials. No anti-CSRF token is required or provided.

## TOGGLING CORS_ENABLED

Toggling `CYODA_CORS_ENABLED` between `true` and `false` requires a
downstream-cache flush. Responses cached during the disabled period lack
`Vary: Origin` and could be served to origins for which the post-toggle policy
disagrees.

## TROUBLESHOOTING

- **Browser logs a CORS error but the service logs the request as `200`** —
  the origin was rejected by the allowlist. The middleware omits
  `Access-Control-Allow-Origin` and the browser blocks reading the body. Add
  the origin to `CYODA_CORS_ALLOWED_ORIGINS` with exact scheme+host+port.

- **Multi-valued `Access-Control-Allow-Origin`** — both the ingress and
  cyoda-go are emitting the header. Set `CYODA_CORS_ENABLED=false` and handle
  CORS entirely at the ingress.

- **Startup failure: `cors: origin "..." has non-ASCII host; convert to punycode`**
  — IDN host names must be supplied in punycode form (`xn--...`).

- **Startup failure: default port rejected** — drop the port from the origin:
  use `https://example.com`, not `https://example.com:443`; use `http://example.com`,
  not `http://example.com:80`.

- **Startup WARN about wildcard mode** — if wildcard is unintended, set a
  specific allowlist with `CYODA_CORS_ALLOWED_ORIGINS=https://your.app.host`.

- **Local SPA on `file://` cannot reach cyoda-go** — `file://` produces
  `Origin: null`, which is not auto-allowed in any mode (in wildcard mode,
  `null` receives `Access-Control-Allow-Origin: *`, which browsers honour for
  non-credentialed requests). Serve the SPA via a local HTTP server (e.g.
  `python3 -m http.server`) so a normal `http://localhost` origin is used instead.

## EXAMPLES

**Loopback only (local dev, default):**

```
# nothing to set — loopback is the default when CYODA_CORS_ENABLED=true
```

**Single production origin:**

```
CYODA_CORS_ENABLED=true
CYODA_CORS_ALLOWED_ORIGINS=https://app.example.com
```

**Multiple origins:**

```
CYODA_CORS_ENABLED=true
CYODA_CORS_ALLOWED_ORIGINS=https://app.example.com,https://admin.example.com
```

**Wildcard (public read API):**

```
CYODA_CORS_ENABLED=true
CYODA_CORS_ALLOWED_ORIGINS=*
```

**Disabled (CORS at ingress):**

```
CYODA_CORS_ENABLED=false
```

## SEE ALSO

- config
- run
