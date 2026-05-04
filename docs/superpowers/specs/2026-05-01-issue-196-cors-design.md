# Issue #196 — API-wide CORS support

**Status:** Design v2.1 (post-second-review)
**Issue:** [#196](https://github.com/Cyoda-platform/cyoda-go/issues/196)
**Branch:** `issue-196-cors`
**Date:** 2026-05-02 (v2, v2.1); 2026-05-01 (v1)

## Problem

cyoda-go ships CORS support only for the `/help` endpoints (`internal/api/help.go:22-35`). The rest of the API surface — entity, model, search, messaging, audit, account, admin, OAuth, discovery, health — has no CORS handling. From a browser on any other origin (including `file://`):

- **Simple requests** (e.g. `GET /api/entity/{id}`): no `Access-Control-Allow-Origin` header in the response → browser blocks it.
- **Non-simple requests** (POST/PUT/PATCH/DELETE for search, create, send-message, archive, etc.): browser sends an `OPTIONS` preflight first → chi returns `405 Method Not Allowed` (no `OPTIONS` handler registered) → browser blocks the actual request.

Result: a SPA running anywhere other than the same origin as cyoda-go cannot make a single API call. This breaks any browser-based admin or inspector tool against a deployed instance.

## Goal

Make every cyoda-go HTTP endpoint reachable from a cross-origin browser SPA, secure by default, with zero-config developer ergonomics and an explicit opt-in for production deployments. Unify the existing `/help` CORS implementation under the new middleware so there is a single source of truth.

## Non-goals (v1)

- **`Access-Control-Allow-Credentials`.** Auth is bearer-token in the `Authorization` header; cookies/HTTP-auth/TLS client certs are not used. Enabling credentials mode adds attack surface (subdomain takeover of an allowlisted origin, XSS in an allowlisted SPA) without functional benefit. If a future change introduces cookie-based auth, that proposal owns its own credentials story.
- **Glob/suffix wildcards** for the allowlist (e.g. `https://*.example.com`). Punted to a follow-up issue if multi-tenant deployers ask. Can be added behind a separate `CYODA_CORS_ALLOWED_ORIGIN_SUFFIXES` knob with explicit suffix-match semantics.
- **Per-route CORS policy** (e.g. different rules for `/admin/*` vs the API). A single global policy serves the use cases driving this issue.
- **`Access-Control-Expose-Headers`.** No current need.
- **Dynamic config reload.** Config is read at startup; changes require a restart.
- **Private Network Access (PNA).** cyoda-go does not handle the `Access-Control-Request-Private-Network` / `Access-Control-Allow-Private-Network` headers. Deployers needing browsers on a public origin to reach cyoda-go on a private network front us with an ingress that handles PNA.
- **CSRF tokens.** CSRF is not a threat for bearer-in-header auth; the SPA explicitly attaches the bearer on each request rather than relying on ambient credentials. No anti-CSRF token is required and none will be added.
- **Preflight route validation.** `OPTIONS /any/path` with valid preflight headers returns `204` regardless of whether the path is a registered route. Anything else would leak routing structure to unauthenticated callers.

## Architecture

A single CORS middleware applied **outside `outerMux`** (the topmost mux that wraps both the context-path-rooted API and the root-rooted help/discovery/dispatch routes), and outside the cluster-routing middleware. This is the only placement that covers every group: the existing chain has `Recovery`/`Auth` only over the inner `mux`, while help/discovery/dispatch are registered directly on `outerMux` and are *not* wrapped by `Recovery` or `Auth` today.

The middleware covers:

- The generated OpenAPI surface (entity, model, search, messaging, audit, account)
- Admin endpoints (`/admin/log-level`, `/admin/trace-sampler`)
- Entity transition routes (`/entity/{entityId}/transitions`, `/platform-api/entity/fetch/transitions`)
- OAuth/auth endpoints (`/oauth/token`, `/.well-known/`, `/oauth/keys/*`, `/account/m2m/*`)
- Discovery routes (`RegisterDiscoveryRoutes`)
- Health routes (`RegisterHealthRoutes`)
- Help routes (`/help`, `/help/{topic}`)

The internal `/internal/dispatch/*` cluster-dispatch surface is **explicitly excluded** from CORS by a path-prefix check at the top of the middleware: regardless of `Origin` presence, no `Access-Control-*` headers are emitted on requests whose path begins with `/internal/dispatch/`. This is defence-in-depth alongside the cluster proxy stripping `Origin` on outbound peer hops; either control alone would suffice, but together they prevent a forged peer-side request from eliciting a CORS response.

### Tenant isolation

CORS is a browser-side defence against unauthorized cross-origin reads; it is **not** a tenant-isolation control. All tenant boundaries are enforced by JWT claims and per-request authorization checks in the data path. An allowlisted SPA serving multiple tenants relies entirely on the auth layer, not CORS, to prevent cross-tenant access. This is load-bearing for Gate 3 and is repeated in the help topic, but is asserted here as a design principle: no CORS rule, however restrictive, is permitted to substitute for or displace JWT-based authz.

Final handler order, outermost to innermost:

```
CORS → cluster-routing → outerMux
                         ├── /<contextPath>/* → mux → otelhttp → Recovery → Auth → genapi
                         ├── /help, /help/{topic}        (no Recovery/Auth/otelhttp today)
                         ├── /<discovery routes>          (no Recovery/Auth/otelhttp today)
                         └── /internal/dispatch/*        (AEAD-auth, peer-only; CORS no-op)
```

The middleware writes CORS headers to `w.Header()` **before** calling `next.ServeHTTP`. It does not wrap the `ResponseWriter`, does not observe the downstream status code, and does not buffer the response — preserving `Flush`/`Hijack`/`CloseNotify` interfaces and avoiding any hidden interaction with `otelhttp` or chi.

New file: `internal/api/middleware/cors.go`. Wired into `app/app.go`'s `Handler()` construction. Pure `net/http`; no third-party CORS library. Expected to be ~80 lines.

## Configuration

Two env vars, both `CYODA_CORS_*`:

| Var | Default | Effect |
|---|---|---|
| `CYODA_CORS_ENABLED` | `true` | Master switch. When `false`, middleware is not installed; service emits no `Access-Control-*` headers (including `Vary: Origin`) and `OPTIONS` returns the chi default `405`. For deployments that handle CORS at an ingress/proxy layer. **Deployers must ensure their ingress handles `OPTIONS` preflights before requests reach cyoda-go in this mode.** **Toggling `CYODA_CORS_ENABLED` between `true` and `false` requires a downstream-cache flush** — responses cached during the disabled period lack `Vary: Origin` and could be served to origins for which the post-toggle policy disagrees. |
| `CYODA_CORS_ALLOWED_ORIGINS` | unset (= **loopback mode**) | Comma-separated exact-match allowlist per RFC 6454 origin (scheme + host + port). When unset, only loopback origins are allowed (see "Loopback mode" below). The literal value `*` opts into wildcard mode. |

`CYODA_CORS_ALLOW_CREDENTIALS` is **not part of v1** — see non-goals.

### Loopback mode (default)

When `CYODA_CORS_ALLOWED_ORIGINS` is unset, the middleware allows requests whose `Origin` header matches one of:

- `http://localhost[:PORT]`
- `https://localhost[:PORT]`
- `http://127.0.0.1[:PORT]`
- `https://127.0.0.1[:PORT]`
- `http://[::1][:PORT]`
- `https://[::1][:PORT]`

Any port (or no port) is accepted. The matched origin is echoed in `Access-Control-Allow-Origin`; `Vary: Origin` is emitted. All other origins (including `null` from `file://`, sandboxed iframes, and arbitrary remote origins) are unmatched — `Access-Control-Allow-Origin` is omitted, the browser blocks the response.

This gives zero-config dev ergonomics (vite/webpack/local docker SPAs all work) while remaining secure by default — a remote attacker hosting `evil.example.com` cannot read responses without explicit allowlist configuration. DNS rebinding does not bypass loopback mode: CORS keys on the `Origin` header (the attacker's actual domain), not the resolved destination IP.

`file://` is not auto-allowed because `Origin: null` is also emitted by sandboxed iframes and certain redirect chains, materially widening the attack surface. Deployers explicitly needing `file://` access add `null` to the allowlist (the documentation calls this out as a footgun).

#### Loopback matching contract

The incoming `Origin` header is matched as follows:

1. Parse with `net/url`. Reject (treat as no match) if `url.Parse` returns an error.
2. Lowercase `u.Scheme` and `u.Hostname()`. Browsers already emit lowercase scheme and host; this normalization is a defensive belt-and-braces step.
3. Reject (no match) unless **all** of the following hold:
   - `u.Scheme` is exactly `http` or `https`.
   - `u.Hostname()` is exactly one of the strings `localhost`, `127.0.0.1`, `::1` — string compare, not IP-equivalence. Non-canonical IPv4 (`127.0.0.0.1`, `127.000.000.001`, `127.1`, `0x7f000001`) does **not** match. Non-canonical IPv6 (`0:0:0:0:0:0:0:1`, `[0::1]`) does **not** match. The implementation uses `u.Hostname()` (which strips IPv6 brackets) — so the configured `[::1]` strings produce a hostname of `::1`, and incoming `Origin: http://[::1]:8080` parses to a hostname of `::1` — these compare equal.
   - `u.User` is `nil` (no userinfo).
   - `u.Path` is empty.
   - `u.RawQuery` is empty.
   - `u.Fragment` is empty.
   - `u.Port()` may be empty or any decimal port number; both are accepted.
4. If all checks pass, the matched origin is echoed in `Access-Control-Allow-Origin` exactly as received (so that `http://Localhost:3000` is echoed as `http://Localhost:3000`, not the lowercased form — this matches what the browser will be checking against).

Concrete attacker examples this rejects: `http://localhost.evil.example` (hostname mismatch), `http://localhost@evil.example` (userinfo present), `http://127.0.0.0.1:3000` (host string mismatch), `http://xn--lcalhost-...` (host string mismatch — IDN homograph defeated by exact-string compare), `null` (`url.Parse` returns an empty scheme — fails check 3).

### Wildcard mode

Set `CYODA_CORS_ALLOWED_ORIGINS=*` to opt into wildcard. The middleware emits `Access-Control-Allow-Origin: *` literally — never reflective of the request `Origin`. A startup WARN announces wildcard mode is active.

### Allowlist mode

Set `CYODA_CORS_ALLOWED_ORIGINS=https://admin.example.com,https://docs.example.com` to opt into an exact-match allowlist. Loopback origins are **not** automatically included in this mode — the deployer's list is authoritative.

### Allowlist normalization and validation

Applied at config load by `ValidateCORS(cfg.CORS) error` (called from `cmd/cyoda/main.go` after slog init, returning an error that the binary slogs and `os.Exit(1)`s on). No `panic`, no `log.Fatal`, no `os.Exit` inside `app/`.

Each origin in the comma-separated list is parsed with `net/url`, then normalized:

- **Lowercase** scheme and host. Configured origins must already be lowercase; uppercase characters cause startup failure with a clear error naming the value (avoids surprises about case sensitivity on match).
- **Strip default ports** — `https://x.com:443` and `http://x.com:80` are rejected at startup; configure as `https://x.com` / `http://x.com`.
- **Reject path, query, fragment, trailing slash, userinfo** — these are not valid origin components per RFC 6454.
- **Reject non-ASCII hosts** — IDN must be supplied in punycode form (`xn--...`); the error message points to this.
- **IPv6** — bracketed form required (`https://[::1]`). The validator confirms `net/url` round-trips correctly.
- **Reject empty entries** — leading/trailing commas, double commas, whitespace-only entries error out.
- **Reject literal `null`** — the `null` origin is never a valid allowlist entry. (Wildcard mode does not echo `null` either; in wildcard mode only the literal `*` is emitted.)
- **`*` is mutually exclusive** — `CYODA_CORS_ALLOWED_ORIGINS=*` opts into wildcard mode. Any other value containing `*` is rejected (no glob semantics in v1).

`r.Header.Get("Origin")` is used to read the incoming origin (canonical key). **Incoming `Origin` is compared byte-equal verbatim** against the post-normalization allowlist, stored as `map[string]struct{}` built once in the constructor for O(1) lookup. No per-request URL parsing or normalization of the incoming `Origin` is performed in allowlist mode — browsers reliably emit lowercase scheme/host with non-default ports, so verbatim compare avoids a hot-path parse step. (Loopback mode requires URL parsing because the rules are structural, not string-equality; this only fires when no allowlist is configured.)

## Behaviour

### Preflight detection

A request is a CORS preflight iff:

- Method is `OPTIONS`, AND
- Request carries an `Origin` header, AND
- Request carries an `Access-Control-Request-Method` header.

`OPTIONS` requests not meeting all three conditions are passed through unchanged. Today, chi will 405 them per-route; if a future chi version adds auto-`OPTIONS` behaviour, the middleware's pass-through still does the right thing. Specifically, an `OPTIONS` request that carries `Origin` but no `Access-Control-Request-Method` (which browsers send only rarely, e.g. for non-CORS purposes) falls through to chi and 405s as today — this is intentional. The middleware does not validate `Access-Control-Request-Method` or `Access-Control-Request-Headers` — preflight is a static yes/no based on `Origin` only. The actual request is method-checked by chi as today.

### Preflight response

Short-circuits at the receiving node, never reaches Auth or any handler. Headers (in all CORS-enabled modes):

| Header | Value |
|---|---|
| `Access-Control-Allow-Origin` | wildcard mode: literal `*`. Loopback / allowlist mode: matched origin, or **omitted** if no match |
| `Access-Control-Allow-Methods` | `GET, POST, PUT, PATCH, DELETE, OPTIONS` (static, package `const`) |
| `Access-Control-Allow-Headers` | `Authorization, Content-Type, traceparent, tracestate` (static, package `const`) |
| `Access-Control-Max-Age` | `86400` (static) |
| `Vary` | `Origin` (always **appended** via `w.Header().Add`, never overwriting an existing `Vary`) |

Status: `204 No Content`. Empty body.

`Vary: Origin` is emitted on **every response** that passes through an installed CORS middleware — preflight, actual request, and even pass-throughs where `Origin` is absent. The middleware's *decision* depends on `Origin`; intermediate caches must therefore key by `Origin` per RFC 7234 §4.1. Always emitting (rather than only-when-`Origin`-is-present) prevents the inverse cache-poisoning vector: a CDN caching a no-`Origin` response without `Vary: Origin` and later serving it to an `Origin`-bearing request whose policy has changed. This is required even in wildcard mode (constant response value) so that a mode-flip during a deployment cannot cause a CDN to serve a stale `Origin`-specific response to a different origin.

In wildcard mode, an `Origin: null` request also receives `Access-Control-Allow-Origin: *` — wildcard means literal `*` for every origin including `null`. This is consistent and correct: browsers do not honour `*` for credentialed reads, and credentials are off in v1.

### Actual request (non-preflight)

Middleware does not block; sets headers on the `ResponseWriter` before calling next:

| Header | Value |
|---|---|
| `Access-Control-Allow-Origin` | same matching rules as preflight; omitted if origin not allowed |
| `Vary` | `Origin` (appended) |

No other CORS headers on actual responses. The handler runs as today; the middleware does not interfere with status code, body, or response writer wrapping.

### Failure mode for unknown origins

In loopback or allowlist mode, when the request `Origin` does not match: the middleware **omits** `Access-Control-Allow-Origin` (still emits `Vary: Origin`). No error response. The browser blocks reading the response and the deployer sees the misconfig in browser devtools. We do not log the rejected origin — that would create a noisy attack-tunable log channel.

### Request without `Origin` header

The middleware adds `Vary: Origin` to the response (for the cache-poisoning reason given above) but emits no `Access-Control-*` headers. The request continues unmodified through the rest of the chain. This is what makes server-to-server `curl` and the cluster proxy's outbound calls invisible to CORS in terms of policy, while still leaving the cache-key signalling intact for any intermediary that might later receive an `Origin`-bearing request for the same URL.

`/internal/dispatch/*` is the sole exception: no headers at all (not even `Vary: Origin`) are emitted on requests whose path begins with `/internal/dispatch/`, regardless of `Origin` presence. This is consistent with the path-prefix exclusion rule in §"Architecture" — the internal dispatch surface is peer-only, never browser-visible, and the spec deliberately keeps it free of any CORS-related response state.

## Cluster proxy interaction

Two changes in `internal/cluster/proxy/http.go`:

1. **In the `httputil.ReverseProxy.Director` (`internal/cluster/proxy/http.go:123`)**, strip the following headers on every outbound peer-to-peer request:
   - `Origin`
   - `Access-Control-Request-Method`
   - `Access-Control-Request-Headers`

   `req.Header.Del(...)` is idempotent. Stripping all three is defence-in-depth: the CORS middleware short-circuits preflights at node A so peer-B should never see a preflight, but a future refactor that moves CORS below cluster routing would otherwise leak preflight signalling to peers. A one-line code comment in the `Director` references the CORS middleware as the upstream owner of these headers.

2. **No special preflight handling.** Preflights short-circuit at node A's CORS middleware before cluster-routing is reached. The `Director` strip is purely a guarantee, not a correctness dependency.

`ReverseProxy` strips hop-by-hop headers automatically but does not strip CORS response headers. With `Origin` stripped on outbound and node B's CORS middleware no-opping when `Origin` is absent, peer-B never emits `Access-Control-*`. Node A's outermost middleware is the sole authority for what reaches the browser.

A unit test on the proxy's outbound-headers helper asserts all three headers are removed, independent of any CORS-middleware test. This is in addition to the multi-node E2E test described under "Testing strategy".

## Startup validation and logging

`ValidateCORS(cfg.CORS) error` called from `cmd/cyoda/main.go` **after** slog initialization (so log-level config is honoured for any WARN/INFO emitted):

- Any normalization rule violation (uppercase, default port, path/query/fragment/trailing-slash, userinfo, non-ASCII, empty entry, literal `null` in allowlist, `*` mixed with other values) → returned error; binary slogs and `os.Exit(1)`s. Error message names the offending value. The non-ASCII case binds a discoverable wording specifically: `cors: origin %q has non-ASCII host; convert to punycode (e.g. xn--…) before configuring`.
- If `CYODA_CORS_ENABLED=true` and `CYODA_CORS_ALLOWED_ORIGINS=*` → log WARN once: `cors: wildcard mode active (Access-Control-Allow-Origin: *)`. `pkg=cors`.
- If `CYODA_CORS_ENABLED=true` and an explicit allowlist is configured → log INFO once: `cors: allowlist mode active (N origins)` (count only, never the values; values logged only at DEBUG via `pkg=cors`).
- If `CYODA_CORS_ENABLED=true` and no allowlist (loopback mode) → log INFO once: `cors: loopback mode active — only http(s)://localhost, 127.0.0.1, [::1] are allowed; set CYODA_CORS_ALLOWED_ORIGINS to permit additional origins`. `pkg=cors`.
- If `CYODA_CORS_ENABLED=false` → log INFO once: `cors: disabled — no Access-Control-* headers will be emitted; configure CORS at your ingress/proxy layer`. `pkg=cors`.

All log lines use `log/slog`. No credentials, tokens, request headers, or rejected-origin values appear in any CORS-related log line.

## Help endpoint unification

`internal/api/help.go` loses its inline CORS:

- Delete `handleHelpPreflight` and the explicit `Header().Set("Access-Control-Allow-Origin", "*")` calls.
- Help routes register only their `GET` handler; the unified middleware handles preflight and CORS headers.
- **Delete** `TestCORSHeadersPresent` and `TestCORSPreflight_204` from `internal/api/help_test.go`. The E2E group-coverage test for `/help` (see "Testing strategy") subsumes them.

After unification, `/help` in wildcard mode has the same externally-observable CORS behaviour as today's `/help`. In loopback or allowlist mode, `/help` is stricter than today — only loopback or explicitly allowed origins can read help content cross-origin. This is a deliberate behaviour change: a single CORS policy is the whole point of unification.

`CYODA_CORS_ENABLED=false` now disables CORS on `/help` too — today's inline implementation would still leak headers; v2 honours the master switch.

## Testing strategy

### Unit tests (`internal/api/middleware/cors_test.go`)

- Loopback mode: matched origins (`http://localhost:3000`, `https://127.0.0.1:8080`, `http://[::1]:5173`) get echoed; unmatched (`https://evil.example`, `http://localhost.evil.example`, `null`) are omitted.
- Wildcard mode: response carries literal `*` regardless of request `Origin`; never reflective. Explicit test that an `Origin: https://evil.example` request gets `*`, not echoed.
- Allowlist mode, matched origin: echoed + `Vary: Origin`.
- Allowlist mode, unmatched origin: no `Access-Control-Allow-Origin`, but `Vary: Origin` still set.
- Preflight short-circuits with `204` and the full preflight header set.
- Preflight detection edge cases:
  - `OPTIONS` with `Origin` but no `Access-Control-Request-Method` → falls through.
  - `OPTIONS` without `Origin` → falls through.
  - `GET` with `Origin` → handled as actual request, not preflight.
- No `Origin` header on any method: `Vary: Origin` is emitted; no `Access-Control-*` headers are emitted; downstream handler runs normally.
- `Vary: Origin` is **appended** via `w.Header().Add`, not overwritten — when a downstream handler also sets `Vary: Accept`, both values are preserved.
- `/internal/dispatch/*` path: **no** headers emitted (not even `Vary: Origin`) regardless of `Origin` presence.
- `CYODA_CORS_ENABLED=false`: middleware is not installed (constructor returns identity wrapper); no `Access-Control-*` and no `Vary: Origin` are emitted.
- **Wired-up assertion test:** with `CYODA_CORS_ENABLED=true` and the full `app.Handler()` constructed, a synthetic preflight against an arbitrary registered route returns `204` with the preflight header set. This proves the middleware is actually installed in the chain (not merely that the middleware code is reachable in isolation). A future refactor that drops the install site would fail this test loud.
- Wildcard mode + `Origin: null`: response carries `Access-Control-Allow-Origin: *`.
- Config validation (table-driven):
  - Reject: uppercase character, default port (`:443`, `:80`), trailing slash, path, query, fragment, userinfo, non-ASCII host, empty entry, literal `null`, `*` mixed with other values, malformed URL.
  - Accept: `https://x.com`, `http://x.com:8080`, `https://[::1]:8443`, `https://xn--e1afmkfd.example`.
  - Single value `*` accepted as wildcard mode.

### E2E tests (`internal/e2e/cors_e2e_test.go`)

Through the full HTTP stack with auth middleware in the chain — proves preflight bypasses auth correctly and that the unified middleware reaches every group.

- Preflight + actual request for one representative endpoint per group: entity (POST), search (POST), messaging (POST), account (GET), admin (POST), help (GET), discovery (GET), health (GET), oauth-token (POST).
- One test in allowlist mode where the configured allowed origin matches.
- One test with a disallowed origin: response has no `Access-Control-Allow-Origin`; the underlying request still succeeds but the browser would block reading the body. Asserts `Vary: Origin` is still present.
- One test with `CYODA_CORS_ENABLED=false`: no `Access-Control-*` headers anywhere, including `/help`.
- One test with default loopback mode: `Origin: http://localhost:3000` succeeds; `Origin: https://evil.example` is omitted.

### Cluster proxy test

- Unit test on `internal/cluster/proxy/http.go`'s outbound `Director`: assert `Origin`, `Access-Control-Request-Method`, `Access-Control-Request-Headers` are all removed from the proxied request, regardless of input.
- Multi-node integration test (or, if multi-node E2E fixtures don't currently support this scenario, an integration test against the proxy): a request proxied from node A → node B with `Origin` set asserts the response has exactly one `Access-Control-Allow-Origin` value, set by node A.

## Documentation

Per the documentation-hygiene gate, update together:

- New file `cmd/cyoda/help/content/config/cors.md` — full env-var reference, defaults, deployment guidance (in-service vs ingress), worked examples for dev / docker-compose / k8s-with-ingress, troubleshooting (loopback-mode mismatch, double-CORS-headers from ingress, origin case sensitivity, `null` origin from `file://`, IDN/punycode), explicit "CORS does not provide tenant isolation — always JWT-gate" note, explicit "no PNA, no CSRF tokens" stance.
- `README.md` — add a CORS row to the configuration table; brief note that loopback mode is the default and allowlist is the production opt-in.
- `app/config.go` — godoc on the new `CORSConfig` struct fields linking to the help topic.
- `cmd/cyoda/help/content/cli/help.md` — strike the help-only CORS bullet and link to `config/cors.md`.

## Files touched

**New:**
- `internal/api/middleware/cors.go`
- `internal/api/middleware/cors_test.go`
- `internal/e2e/cors_e2e_test.go`
- `cmd/cyoda/help/content/config/cors.md`

**Modified:**
- `app/config.go` — `CORSConfig` struct, env reads, exported `ValidateCORS`
- `cmd/cyoda/main.go` — call `ValidateCORS` after slog init, before `app.New`
- `app/app.go` — install middleware outside `outerMux`, outside cluster-routing
- `internal/cluster/proxy/http.go` — strip `Origin`, `Access-Control-Request-Method`, `Access-Control-Request-Headers` in the proxy `Director`
- `internal/api/help.go` — remove inline CORS
- `internal/api/help_test.go` — delete `TestCORSHeadersPresent`, `TestCORSPreflight_204`
- `README.md`
- `cmd/cyoda/help/content/cli/help.md`

## Acceptance

- [ ] `OPTIONS` preflight returns `204` with the full preflight header set for every API endpoint, not just `/help`.
- [ ] Every actual response carries `Access-Control-Allow-Origin` matching the configured policy (or omitted for unmatched origins) and `Vary: Origin`.
- [ ] Preflight is processed before auth middleware.
- [ ] CORS applies through the cluster proxy layer with no duplicate headers.
- [ ] Cluster proxy `Director` strips `Origin`, `Access-Control-Request-Method`, `Access-Control-Request-Headers` on outbound peer requests, with a unit test on the helper.
- [ ] `/internal/dispatch/*` requests never carry CORS response headers regardless of `Origin`.
- [ ] `CYODA_CORS_ENABLED=false` disables CORS everywhere, including `/help`.
- [ ] Loopback mode is the default; wildcard mode requires explicit `=*` and emits a startup WARN.
- [ ] Loopback mode matches `localhost`/`127.0.0.1`/`::1` exactly per the matching contract; rejects `localhost.evil.example`, non-canonical IPv4/IPv6 forms, userinfo-bearing origins, and IDN homographs.
- [ ] Wired-up assertion: `CYODA_CORS_ENABLED=true` actually installs the middleware in `app.Handler()`, verified by a test that issues a synthetic preflight against the assembled handler.
- [ ] Allowlist normalization rejects uppercase, default ports, path/query/fragment/trailing-slash, userinfo, non-ASCII, empty entries, literal `null`, `*` mixed with other values.
- [ ] Wildcard mode emits literal `*`, never reflective of `Origin`.
- [ ] `Vary: Origin` is appended (not overwritten) and present on every response that passes through the installed CORS middleware, including no-`Origin` pass-throughs. Sole exception: `/internal/dispatch/*` paths.
- [ ] Startup logs report mode at INFO/WARN and never log allowlist values above DEBUG.
- [ ] Unit + E2E + cluster proxy tests cover all of the above.
- [ ] `cmd/cyoda/help/content/config/cors.md`, `README.md`, `app/config.go` godoc, and `DefaultConfig()` updated together.
- [ ] `make test-all` and `go vet ./...` green.
- [ ] `go test -race ./...` green as the end-of-deliverable sanity check.
