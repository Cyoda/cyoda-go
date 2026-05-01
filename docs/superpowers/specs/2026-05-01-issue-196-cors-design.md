# Issue #196 — API-wide CORS support

**Status:** Design
**Issue:** [#196](https://github.com/Cyoda-platform/cyoda-go/issues/196)
**Branch:** `issue-196-cors`
**Date:** 2026-05-01

## Problem

cyoda-go ships CORS support only for the `/help` endpoints (`internal/api/help.go:22-35`). The rest of the API surface — entity, model, search, messaging, audit, account, admin, OAuth, discovery, health — has no CORS handling. From a browser on any other origin (including `file://`):

- **Simple requests** (e.g. `GET /api/entity/{id}`): no `Access-Control-Allow-Origin` header in the response → browser blocks it.
- **Non-simple requests** (POST/PUT/PATCH/DELETE for search, create, send-message, archive, etc.): browser sends an `OPTIONS` preflight first → chi returns `405 Method Not Allowed` (no `OPTIONS` handler registered) → browser blocks the actual request.

Result: a SPA running anywhere other than the same origin as cyoda-go cannot make a single API call. This breaks any browser-based admin or inspector tool against a deployed instance.

## Goal

Make every cyoda-go HTTP endpoint reachable from a cross-origin browser SPA out of the box, with an opt-in tightening path for production deployments. Unify the existing `/help` CORS implementation under the new middleware so there is a single source of truth.

## Non-goals (v1)

- Glob/suffix wildcards for the allowlist (e.g. `https://*.example.com`). Punted to a follow-up issue if multi-tenant deployers ask. Can be added behind a separate `CYODA_CORS_ALLOWED_ORIGIN_SUFFIXES` knob with explicit suffix-match semantics.
- Per-route CORS policy (e.g. different rules for `/admin/*` vs the API). A single global policy serves the use cases driving this issue.
- `Access-Control-Expose-Headers`. No current need; add when a client surfaces a use case.
- Dynamic config reload. Config is read at startup; changes require a restart. Matches the project's existing config-reload story.

## Architecture

A single CORS middleware applied **outermost** on the user-facing handler chain — outside cluster-routing, OTel, Recovery, and Auth. The same middleware covers:

- The generated OpenAPI surface (entity, model, search, messaging, audit, account)
- Admin endpoints (`/admin/log-level`, `/admin/trace-sampler`)
- Entity transition routes (`/entity/{entityId}/transitions`, `/platform-api/entity/fetch/transitions`)
- OAuth/auth endpoints (`/oauth/token`, `/.well-known/`, `/oauth/keys/*`, `/account/m2m/*`)
- Discovery routes (`RegisterDiscoveryRoutes`)
- Health routes (`RegisterHealthRoutes`)
- Help routes (`/help`, `/help/{topic}`)

The internal `/_internal/*` cluster-dispatch surface receives the middleware too, but it is a no-op there because peers do not send `Origin`.

Final handler order, outermost to innermost:

```
CORS → cluster-routing → otelhttp → Recovery → Auth → handler
```

New file: `internal/api/middleware/cors.go`. Wired into `app/app.go`'s `Handler()` construction.

## Configuration

Three env vars, all `CYODA_CORS_*`:

| Var | Default | Effect |
|---|---|---|
| `CYODA_CORS_ENABLED` | `true` | Master switch. When `false`, middleware is not installed; service emits no `Access-Control-*` headers and `OPTIONS` returns the chi default `405`. For deployments that handle CORS at an ingress/proxy layer. |
| `CYODA_CORS_ALLOWED_ORIGINS` | unset (= wildcard mode) | Comma-separated exact-match allowlist per RFC 6454 origin (scheme + host + port). Default-port omission required (`https://x.com`, not `https://x.com:443`). Refuses to start if a configured origin contains path, query, fragment, or trailing slash. |
| `CYODA_CORS_ALLOW_CREDENTIALS` | `false` | Opt-in for `Access-Control-Allow-Credentials: true`. Refuses to start if `true` while in wildcard mode (browser spec violation). |

Loaded in `app/config.go`'s `DefaultConfig()` alongside other `CYODA_*` env reads. New `CORSConfig` struct mirrors the shape of existing config groups (`Auth`, `Bootstrap`, etc.).

## Behaviour

### Preflight detection

A request is a CORS preflight iff:

- Method is `OPTIONS`, AND
- Request carries an `Origin` header, AND
- Request carries an `Access-Control-Request-Method` header.

`OPTIONS` requests not meeting all three conditions fall through to the next handler — chi will 405 them per-route as today.

### Preflight response

Short-circuits at the receiving node, never reaches Auth or any handler. Headers:

| Header | Value |
|---|---|
| `Access-Control-Allow-Origin` | `*` in wildcard mode; the matched origin in allowlist mode; **omitted** in allowlist mode if origin not allowed |
| `Access-Control-Allow-Methods` | `GET, POST, PUT, PATCH, DELETE, OPTIONS` (static) |
| `Access-Control-Allow-Headers` | `Authorization, Content-Type, traceparent, tracestate` (static) |
| `Access-Control-Max-Age` | `86400` |
| `Access-Control-Allow-Credentials` | `true` only if configured AND in allowlist mode AND origin matched |
| `Vary: Origin` | emitted whenever the response varies by origin (i.e. any time we are in allowlist mode) |

Status: `204 No Content`. Empty body.

### Actual request (non-preflight)

Middleware does not block; sets headers on the `ResponseWriter` before calling next:

| Header | Value |
|---|---|
| `Access-Control-Allow-Origin` | same matching rules as preflight; omitted if origin not allowed |
| `Access-Control-Allow-Credentials` | `true` only if configured AND in allowlist mode AND origin matched |
| `Vary: Origin` | emitted in allowlist mode |

No other CORS headers on actual responses. The handler runs as today; the middleware does not interfere with status code or body.

### Failure mode for unknown origins

In allowlist mode, when the request `Origin` does not match any configured allowed origin: the middleware **omits** `Access-Control-Allow-Origin`. We do not send an error response. The browser will block the response and the deployer sees the misconfig in browser devtools. This matches the wider CORS ecosystem's behaviour and avoids leaking allowlist contents in error messages.

### Request without `Origin` header

Middleware short-circuits — no CORS headers added, request continues unmodified. This is what makes the `/_internal/*` peer-to-peer no-op work without a path-prefix special case, and also makes server-to-server `curl` and the cluster proxy's outbound calls invisible to CORS.

## Cluster proxy interaction

Two changes:

1. **Strip `Origin` on outbound peer-to-peer requests.** Without this, a request proxied from node A to node B would cause node B's CORS middleware to also emit `Access-Control-Allow-Origin`, and depending on how the proxy merges response headers we risk a multi-valued header (browsers reject). Stripping `Origin` makes node B's CORS middleware a no-op on proxied requests, and node A's outermost middleware is the sole authority for what reaches the browser.
2. **No special preflight handling needed.** Preflights short-circuit at node A's CORS middleware before cluster-routing is reached. Preflights never traverse the cluster proxy.

The exact file path for the proxy change is determined during implementation; no behavioural surprise expected since the proxy already manipulates per-request headers (e.g. for AEAD signing).

## Startup validation and logging

In `DefaultConfig()` (or an explicit `cfg.Validate()` step):

- If `CYODA_CORS_ALLOW_CREDENTIALS=true` and `CYODA_CORS_ALLOWED_ORIGINS` is unset → fatal startup error naming both vars.
- If any configured origin contains a path, query, fragment, or trailing slash → fatal startup error naming the offending value.
- If `CYODA_CORS_ENABLED=true` and no allowlist configured → log WARN once at startup: `cors: wildcard mode active (Access-Control-Allow-Origin: *) — set CYODA_CORS_ALLOWED_ORIGINS to restrict access`. `pkg=cors`.
- If `CYODA_CORS_ENABLED=false` → log INFO once: `cors: disabled — no Access-Control-* headers will be emitted; configure CORS at your ingress/proxy layer`. `pkg=cors`.

The startup WARN suppresses when the deployer has explicitly set `CYODA_CORS_ALLOWED_ORIGINS` (even if the value is the literal string `*` — a deliberate decision the deployer made and which we do not need to second-guess).

All log lines use `log/slog` per the project's logging policy. No credentials or tokens appear in any CORS-related log line.

## Help endpoint unification

`internal/api/help.go` loses its inline CORS:

- Delete `handleHelpPreflight` and the explicit `Header().Set("Access-Control-Allow-Origin", "*")` calls.
- Help routes register only their `GET` handler; the unified middleware handles preflight and CORS headers.
- Existing tests `TestCORSHeadersPresent` and `TestCORSPreflight_204` in `internal/api/help_test.go` move into the new middleware test or are restructured as integration tests against the wired-up server.

This collapses two CORS implementations into one. Critically, it makes `CYODA_CORS_ENABLED=false` actually disable CORS everywhere — today the inline help CORS would still leak headers regardless of the new switch.

## Testing strategy

### Unit tests (`internal/api/middleware/cors_test.go`)

- Wildcard mode, simple GET: response carries `Access-Control-Allow-Origin: *`, no `Allow-Credentials`, no `Vary`.
- Allowlist mode, matched origin: response carries echoed origin and `Vary: Origin`.
- Allowlist mode, unmatched origin: response carries no `Access-Control-Allow-Origin`, but `Vary: Origin` is still set (cache correctness).
- Preflight short-circuits with `204` and the full preflight header set.
- Preflight detection edge case: `OPTIONS` with `Origin` but no `Access-Control-Request-Method` falls through (not a preflight).
- No `Origin` header on any method: passes through untouched.
- `Allow-Credentials`: emitted only when configured AND allowlist mode AND origin matched. Never emitted in wildcard mode.
- Config validation: refuses startup on `Credentials + wildcard`; refuses on origin with path/query/fragment/trailing-slash; accepts well-formed origins.
- Static header values match the spec exactly (regression guard against accidental edits).

### E2E tests (`internal/e2e/cors_e2e_test.go`)

Through the full HTTP stack with auth middleware in the chain — proves preflight bypasses auth correctly and that the unified middleware reaches every group.

- Preflight + actual request for one representative endpoint per group: entity (POST), search (POST), messaging (POST), account (GET), admin (POST), help (GET), discovery (GET), health (GET), oauth-token (POST).
- One test in allowlist mode where the configured allowed origin matches and the response carries credentials when the corresponding env is set.
- One test with a disallowed origin: response has no `Access-Control-Allow-Origin`; the underlying request still succeeds but the browser would block reading the body.
- One test with `CYODA_CORS_ENABLED=false`: no `Access-Control-*` headers anywhere, including `/help`.

### Cluster test

- Multi-node fixture (or integration test if multi-node E2E fixtures don't currently support this scenario): a request proxied from node A → node B with `Origin` set asserts the response has exactly one `Access-Control-Allow-Origin` value, set by node A. Confirms the `Origin`-stripping change in the proxy.

## Documentation

Per the documentation-hygiene gate, update together:

- New file `cmd/cyoda/help/content/config/cors.md` — full env-var reference, defaults, deployment guidance (in-service vs ingress), worked examples for dev / docker-compose / k8s-with-ingress, troubleshooting (wildcard + credentials, double-CORS-headers from ingress, origin-mismatch).
- `README.md` — add a CORS row to the configuration table; brief note that wildcard is the default and allowlist is the production opt-in.
- `app/config.go` — godoc on the new `CORSConfig` struct fields linking to the help topic.
- `cmd/cyoda/help/content/cli/help.md` — strike the help-only CORS bullet and link to `config/cors.md`.

## Files touched

**New:**
- `internal/api/middleware/cors.go`
- `internal/api/middleware/cors_test.go`
- `internal/e2e/cors_e2e_test.go`
- `cmd/cyoda/help/content/config/cors.md`

**Modified:**
- `app/config.go` — `CORSConfig` struct, env reads, validation
- `app/app.go` — install middleware in handler chain
- `internal/cluster/proxy/...` — strip `Origin` on outbound (exact path determined during implementation)
- `internal/api/help.go` — remove inline CORS
- `internal/api/help_test.go` — move/restructure CORS tests
- `README.md`
- `cmd/cyoda/help/content/cli/help.md`

## Acceptance

- [ ] `OPTIONS` preflight returns `204` with the full preflight header set for every API endpoint, not just `/help`.
- [ ] Every actual response carries `Access-Control-Allow-Origin` matching the configured policy (or omitted for unknown origins in allowlist mode).
- [ ] Preflight is processed before auth middleware.
- [ ] CORS applies through the cluster proxy layer with no duplicate headers.
- [ ] `CYODA_CORS_ENABLED=false` disables CORS everywhere, including `/help`.
- [ ] Wildcard mode emits a startup WARN; disabled mode emits an INFO; misconfigurations refuse to start.
- [ ] Unit + E2E + cluster tests cover all of the above.
- [ ] `cmd/cyoda/help/content/config/cors.md`, `README.md`, `app/config.go` godoc, and `DefaultConfig()` updated together.
- [ ] `make test-all` and `go vet ./...` green.
- [ ] `go test -race ./...` green as the end-of-deliverable sanity check.
