# Issue #282 — OpenAPI conformance for `/clients` (technical users)

**Status:** approved
**Date:** 2026-06-16
**Issue:** [#282](https://github.com/Cyoda-platform/cyoda-go/issues/282)
**Umbrella:** [`2026-06-04-194-decomposition-design.md`](2026-06-04-194-decomposition-design.md) §3.2
**Sibling precedent:** PR #281 / [`2026-06-04-281-oauth-keys-openapi.md`](../plans/2026-06-04-281-oauth-keys-openapi.md)
**Scope:** Surface the existing M2M client management at the spec-conformant `/clients` paths with the spec-conformant DTOs. Retire `/account/m2m`. Drop the dead `getTechnicalUserToken` stub. Five OpenAPI operations flip from `501 NOT_IMPLEMENTED` to `match`.

---

## 1. Context

The umbrella decomposition (§3.2) records that M2M client management is **already implemented** in `internal/auth/m2m.go` + `internal/auth/store.go` (`InMemoryM2MClientStore`), wired at `/account/m2m*` via `authSvc.AdminHandler()`. The OpenAPI spec puts technical users at `/clients` instead, so the spec-conformant path is still `501` in `internal/domain/account/handler.go` (`ListTechnicalUsers`, `CreateTechnicalUser`, `DeleteTechnicalUser`, `ResetTechnicalUserSecret`, `GetTechnicalUserToken`).

`POST /oauth/token` (a.k.a. `getTechnicalUserToken`) is a special case: it is already functional via the public-mux entry `mux.Handle("POST /oauth/token", authSvc.Handler())` in `app/app.go:480`. The stub method in `account.Handler` is dead code that the chi router never reaches. Removing it (and the spec's `501` declaration) is housekeeping.

PR #281 has landed the sibling sub-issue (`/oauth/keys/*` conformance). That work established the in-tree template this spec follows:

- Per-endpoint adapter file at `internal/domain/account/<feature>_adapter.go`, paired with `<feature>_adapter_test.go`.
- IAM feature flags in `internal/auth/iam_features.go::IAMFeatures` plumbed through `app.IAMConfig` → `AuthConfig` → `account.Handler.iam`.
- `gateTrustedKeyFeature`-style `404 FEATURE_DISABLED` for runtime-toggled OpenAPI operations.
- `RequireAdmin` (ROLE_ADMIN) as the role gate. OpenAPI prose updated to match.
- Body-size-limit harness exercised at the adapter level via `boundedJSONDecode`.

This spec lifts that template directly. Decisions below differ from #281 only where the M2M shape differs.

## 2. Decisions

| # | Decision | Choice | Rationale |
|---|---|---|---|
| D1 | Response code when `withAdminRole=true` is sent and the M2M-admin-role feature flag is off | **`404 FEATURE_DISABLED`** | Matches the `gateTrustedKeyFeature` precedent. Reviewers don't have to learn a per-endpoint convention. OpenAPI prose ("a 401 Unauthorized response is returned") is updated to match the wire reality. |
| D2 | `clientId` generation for `POST /clients` (no request body in spec) | **16-char crypto/rand base32-hex, uppercase, no padding** | ~80 bits of entropy. Matches the OpenAPI pattern `^[A-Za-z0-9]+$` (`maxLength: 100`, `minLength: 1`) and is short enough to copy-paste comfortably. UUID-stripped-hyphens (32 chars, 122 bits) is the alternative — entropy gain not worth the 2× string length for an admin-issued credential. |
| D3 | Fate of `internal/auth/m2m.go` (`M2MHandler`) | **Delete the file + its dedicated test + the `/account/m2m` test cases in sibling tests** | Once §4 lands, `NewM2MHandler` has zero non-test callers. Per Gate 6 ("resolve, don't defer") and umbrella §3.2 ("private-surface cleanup with no public-API impact"), removal is bounded and reversible. |
| D4 | Body-size assertion in `internal/auth/integration_test.go:271-286` | **Delete (don't relocate)** | `POST /clients` declares no request body in the OpenAPI. There is no oversized-body case to assert at the chi adapter. The `boundedJSONDecode` harness is exercised at the trusted-key adapter level already (`trusted_adapter_test.go`). |
| D5 | Scope of "SUPER_USER" → "ROLE_ADMIN" prose update | **Five `/clients` operations + `POST /oauth/token` description only** | Umbrella §4 says per-sub-issue, aligning prose to code. Other admin endpoints' prose updates ride their own sub-issues. |
| D6 | Add `UpdatedAt` to `M2MClient` | **Yes, in-scope** | `TechnicalUserDto.lastUpdateDate` requires it. `ResetSecret` mutates state without timestamping today — the existing fix is two lines + a test, satisfies Gate 6's "within reason" bound. |
| D7 | New `Handler` constructor dependency | **Inject the existing `M2MClientStore` accessor** (`AuthService.M2MClientStore()`) — no new SPI | The accessor already exists at `internal/auth/service.go:148`. Pass it into the `account.Handler` constructor next to `trustedKeyStore`. |
| D8 | `withAdminRole` query param schema | **Tighten from `type: string` to `type: boolean`** in `api/openapi.yaml` | The adapter parses `"true"` / `"false"` / absent. `string` was a generator-friendly punt; `boolean` is what callers actually send. Generated DTO updates are trivial. |
| D9 | E2E coverage location | **New `internal/e2e/clients_test.go`** | Symmetric with PR #281's `oauth_keys_test.go`. Same TestMain harness. |
| D10 | v0.8.0 release notes call-out | **In scope of this PR** | M2M creation flips from `501` to functional. A server restart silently wipes credentials (umbrella §3.6 follow-up). Customers need the heads-up; PR-time is the only practical hook. |

## 3. Architecture

### 3.1 Files touched

| File | Action |
|---|---|
| `internal/domain/account/m2m_adapter.go` | **New.** Five `Handler` methods + helpers (`requireM2MStore`, `gateM2MAdminRole`, `generateClientID`, `toTechnicalUserDto`, `toTechnicalUserCredentialsDto`). |
| `internal/domain/account/m2m_adapter_test.go` | **New.** Unit tests per §6.1. |
| `internal/domain/account/handler.go` | Delete 5 stub methods; add `m2mClientStore auth.M2MClientStore` field to `Handler`; wire it through `NewHandler`. |
| `internal/auth/iam_features.go` | Add `M2MAdminRoleEnabled bool` field + zero default in `DefaultIAMFeatures`. |
| `app/config.go` | Add `M2MAdminRoleEnabled` to `IAMConfig`; read `CYODA_IAM_M2M_ADMIN_ROLE_ENABLED` via `envBool`; thread into the `auth.IAMFeatures` build. |
| `app/app.go:470, 491-492` | Remove the two `mux.Handle("/account/m2m...", ...)` lines; update the route-class comment. Pass the M2M client store into `accountHandler := account.NewHandler(...)`. |
| `internal/auth/service.go:96, 106-107` | Remove `m2mHandler := NewM2MHandler(m2mStore)` and the two `adminMux.Handle("/account/m2m...", ...)` lines. |
| `internal/auth/m2m.go` | **Delete entire file.** |
| `internal/auth/m2m_test.go` | **Delete entire file.** Coverage migrates to `m2m_adapter_test.go`. |
| `internal/auth/admin_authz_test.go:64-150` | Delete the four `TestM2MHandler_*` cases. Keypair/trusted equivalents stay. |
| `internal/auth/integration_test.go:265-286` | Delete the `adminSrv` block (the m2m body-size test). |
| `internal/auth/store.go` | Add `UpdatedAt time.Time` to `M2MClient`; set on `Create`, `CreateWithSecret`, `ResetSecret`. |
| `app/app_test.go:198-200` | Drop the three `/account/m2m` entries from the route-presence assertion. |
| `internal/e2e/clients_test.go` | **New.** Full-stack coverage of the five operations. |
| `internal/e2e/e2e_test.go` | Add `cfg.IAM.M2MAdminRoleEnabled = true` in TestMain alongside the existing `TrustedKeyRegistrationEnabled` line at L117. |
| `api/openapi.yaml` | Strip `501` from the five operations; flip SUPER_USER prose to ROLE_ADMIN on those five; update `withAdminRole` schema; rewrite the disabled-flag prose; flip the audit table dispositions. |
| `cmd/cyoda/help/content/config/iam.md` (or current IAM topic) | Add `CYODA_IAM_M2M_ADMIN_ROLE_ENABLED` row. |
| `README.md` | Add the same env-var row in the configuration table. |
| Release-notes draft (location TBD by plan — likely `docs/release-notes/v0.8.0.md` if it exists, else a new file) | Add the M2M-credentials-restart-wipes paragraph. |

### 3.2 Component diagram

```
                              ┌───────────────────────────────┐
                              │   chi router (api.Handler)    │
                              └──────────────┬────────────────┘
                                             │
                              ┌──────────────▼────────────────┐
                              │  account.Handler              │
                              │  (m2m_adapter.go methods)     │
                              │   - RequireAdmin gate         │
                              │   - requireM2MStore gate      │
                              │   - gateM2MAdminRole gate     │
                              │   - tenantFromCtx scoping     │
                              └──────────────┬────────────────┘
                                             │
                              ┌──────────────▼────────────────┐
                              │  auth.M2MClientStore          │
                              │  (InMemoryM2MClientStore)     │
                              └───────────────────────────────┘
```

No new interface. The store boundary already exists. `account.Handler` becomes a thin chi-adapter over it, identically to `trusted_adapter.go`'s relationship with `TrustedKeyStore`.

### 3.3 Endpoint behavior

All five handlers follow the same skeleton:

```go
func (h *Handler) <OperationName>(w http.ResponseWriter, r *http.Request, /*path params, query struct*/) {
    if !auth.RequireAdmin(w, r) { return }
    if !h.requireM2MStore(w, r) { return } // 501 NOT_IMPLEMENTED if nil (mock IAM mode)
    // ... operation logic ...
}
```

Per-operation:

**`ListTechnicalUsers` → `GET /clients`**
1. `tID := tenantFromCtx(r)`
2. `clients := h.m2mClientStore.List()` then filter `client.TenantID == tID`.
3. Map each to `TechnicalUserDto`. Emit JSON array.

**`CreateTechnicalUser` → `POST /clients?withAdminRole=<bool>`**
1. Parse `params.WithAdminRole` — generated as `*string` today, will be `*bool` once D8 lands. Absent → false. Invalid (string mode, transitional) → 400 `BAD_REQUEST`.
2. If true: `gateM2MAdminRole` → 404 `FEATURE_DISABLED` when `!h.iam.M2MAdminRoleEnabled`.
3. `clientID := generateClientID()` (D2).
4. `tenantID := tenantFromCtx(r)`.
5. `roles := []string{"M2M"}`; append `"ADMIN"` iff `withAdminRole && h.iam.M2MAdminRoleEnabled`.
6. `secret, err := h.m2mClientStore.Create(clientID, tenantID, clientID /*userID==clientID*/, roles)`. 500 on store error.
7. Emit `TechnicalUserCredentialsDto{ClientID: clientID, ClientSecret: &secret, GrantType: "client_credentials", ClientSecretExpiresAt: ptr(int64(0))}`.

**`DeleteTechnicalUser` → `DELETE /clients/{clientId}`**
1. Validate `clientId` against `^[A-Za-z0-9]+$`, len 1..100. 400 on miss. (Chi already extracts the path param; this is defense-in-depth + matches the OpenAPI shape.)
2. Tenant scope: `client, err := h.m2mClientStore.Get(clientId)`. 404 `M2M_CLIENT_NOT_FOUND` if err or `client.TenantID != tenantFromCtx(r)`. Same 404 on both → no cross-tenant existence oracle (Gate 3).
3. `h.m2mClientStore.Delete(clientId)`. Map any store-not-found to the same 404.
4. Emit `DeleteTechnicalUser200ResponseDto{Message: "M2M client deleted successfully", ClientId: clientId}`.

**`ResetTechnicalUserSecret` → `PUT /clients/{clientId}/secret`**
1. Path-param validation as Delete.
2. Tenant scope check as Delete (no existence oracle).
3. `secret, err := h.m2mClientStore.ResetSecret(clientId)`. Map store-not-found to 404. Other errors → 500.
4. Emit `TechnicalUserCredentialsDto` (same shape as Create).

**`GetTechnicalUserToken` → `POST /oauth/token`**
1. Delete the stub method from `account.Handler`. The chi router never reaches it (public mux intercepts).
2. Remove `501` declaration from the OpenAPI for this operation. The existing 200/400/401/403/500 responses are correct.

### 3.4 Tenant isolation (Gate 3)

The `M2MClientStore.List()`, `Get`, `Delete`, `ResetSecret` methods are *not* tenant-aware — they index by `clientID` alone. Tenant isolation lives in the adapter, by filtering on `client.TenantID` against `tenantFromCtx(r)`:

- `List`: post-filter.
- `Get`/`Delete`/`ResetSecret`: read-before-write tenant check; same 404 regardless of "not found" vs "found in another tenant".

Verification: an explicit `m2m_adapter_test.go` case per operation covering "client exists in tenant A, request from tenant B → 404, store record untouched".

### 3.5 Boundary with the public `POST /oauth/token` flow

`internal/auth/token.go:55, 87` validates `client_credentials` grant against `m2mStore.VerifySecret(clientID, secret)` and emits a token with `sub = clientID`. The store-level `Create` and `ResetSecret` return the plaintext secret exactly once; the store persists only the bcrypt hash (`internal/auth/store.go:324, 346`). The adapter does not log, return, or otherwise touch the secret outside the immediate response body.

No change to `token.go`. The token-exchange path is unaffected.

## 4. Open questions & non-decisions

- **Persistence.** Out of scope. Umbrella §3.6 tracks the follow-up. Release notes call this out (D10).
- **Audit table physical location.** I'll locate the exact file during the plan step; the umbrella spec describes the disposition flip but does not pin a path. Candidates: `docs/superpowers/specs/2025-*-openapi-audit.*` or an in-spec table.
- **`withAdminRole` regenerated DTO shape.** Switching the OpenAPI from `type: string` to `type: boolean` (D8) changes the generated `CreateTechnicalUserParams.WithAdminRole` from `*string` to `*bool`. Adapter code matches the final shape. If oapi-codegen surfaces an unexpected wrinkle during the plan step (e.g., a custom unmarshaller required), the plan documents a fallback: keep `type: string` in the spec and parse in the adapter.
- **`getTechnicalUserToken` schema gaps.** The OpenAPI body declares only the token-exchange grant fields, not the `client_credentials` `grant_type` form that the current public mux accepts. Reconciling that is outside this sub-issue's scope — `getTechnicalUserToken` ships unchanged at runtime; only the dead stub + 501 declaration come off. A note in the audit-table disposition flip will record the schema gap as a separate ticket if one is warranted.

## 5. Compatibility & migration

- **No SPI change.** `cyoda-go-spi` pin untouched. `COMPATIBILITY.md` untouched.
- **No chart bump.** No env-var on the chart side; the new env var is consumed by the binary only.
- **API surface deletion.** `/account/m2m*` was never OpenAPI-declared. Its removal is private-surface cleanup with no public-API impact.
- **Behavioural change visible to ops:** `POST /clients` flips from `501` to functional. Release-notes paragraph spells out the restart-wipes-credentials limitation pointing at umbrella §3.6.

## 6. Tests

### 6.1 Per-adapter unit tests (`internal/domain/account/m2m_adapter_test.go`)

TDD: each red test below maps to a single response shape.

| # | Case | Expected |
|---|---|---|
| 1 | `ListTechnicalUsers` — admin, empty store | 200, `[]` |
| 2 | `ListTechnicalUsers` — admin, two clients in tenant A + one in tenant B; caller in A | 200, body lists A's two only |
| 3 | `ListTechnicalUsers` — non-admin | 403 `INSUFFICIENT_PERMISSIONS` |
| 4 | `ListTechnicalUsers` — nil store | 501 `NOT_IMPLEMENTED` |
| 5 | `CreateTechnicalUser` — admin, `withAdminRole` absent | 200, body has `roles=["M2M"]` in store, plaintext secret present |
| 6 | `CreateTechnicalUser` — admin, `withAdminRole=true`, flag on | 200, store record has `roles=["M2M","ADMIN"]` |
| 7 | `CreateTechnicalUser` — admin, `withAdminRole=true`, flag off | 404 `FEATURE_DISABLED` |
| 8 | `CreateTechnicalUser` — admin, `withAdminRole=invalid` | 400 `BAD_REQUEST` (transitional, string-mode only) |
| 9 | `CreateTechnicalUser` — non-admin | 403 |
| 10 | `DeleteTechnicalUser` — admin, owned by caller's tenant | 200, body shape matches `DeleteTechnicalUser200ResponseDto`, store no longer has it |
| 11 | `DeleteTechnicalUser` — admin, owned by another tenant | 404, store record untouched |
| 12 | `DeleteTechnicalUser` — admin, unknown id | 404 |
| 13 | `DeleteTechnicalUser` — admin, malformed id (e.g. `bad-id` with hyphen) | 400 |
| 14 | `DeleteTechnicalUser` — non-admin | 403 |
| 15 | `ResetTechnicalUserSecret` — admin, owned by caller | 200, secret in response differs from prior `VerifySecret` input, store hash mutated, `UpdatedAt` advanced |
| 16 | `ResetTechnicalUserSecret` — admin, owned by another tenant | 404, store hash untouched |
| 17 | `ResetTechnicalUserSecret` — admin, unknown id | 404 |
| 18 | `ResetTechnicalUserSecret` — admin, malformed id | 400 |
| 19 | `ResetTechnicalUserSecret` — non-admin | 403 |

The mock `M2MClientStore` used by these tests is the real `InMemoryM2MClientStore` — no fakes — for store-side fidelity (matches `trusted_adapter_test.go`'s pattern of using the real `KVTrustedKeyStore`).

### 6.2 Store change tests

`internal/auth/store_test.go` (or wherever `InMemoryM2MClientStore` is exercised today): add one test asserting `Create` stamps `UpdatedAt = CreatedAt` and `ResetSecret` advances `UpdatedAt` strictly forward.

### 6.3 E2E (`internal/e2e/clients_test.go`)

Through the full HTTP stack with JWT auth (Gate 2). Cases:

- `GET /clients` empty, then `POST /clients` (no `withAdminRole`), then `GET /clients` lists the new client with `roles: ["M2M"]`.
- `POST /clients` followed by a `POST /oauth/token` with `grant_type=client_credentials` and HTTP Basic credentials → 200, JWT decodes with `sub == clientId` and `roles` claim contains `"M2M"`.
- `PUT /clients/{id}/secret` → returned secret authenticates; the *old* secret returns 401 on `POST /oauth/token`.
- `DELETE /clients/{id}` → subsequent `GET /clients/{id}` not in listing; `POST /oauth/token` with the deleted client's credentials → 401.
- `POST /clients?withAdminRole=true` with `cfg.IAM.M2MAdminRoleEnabled = true` (the harness default for E2E) → created client has `"ADMIN"` in its roles claim on the issued token.
- `POST /clients?withAdminRole=true` with `cfg.IAM.M2MAdminRoleEnabled = false` (spin up a second harness instance, or sub-test override) → 404 with `error_code: FEATURE_DISABLED`.
- Tenant isolation: two tenants in the same server, tenant B cannot list/delete/reset tenant A's client.

### 6.4 Verification

- `go test ./... -v` green (root module + e2e).
- `make test-short-all` green (plugin submodules — no plugin changes expected, smoke only).
- `go vet ./...` clean.
- `go test -race ./...` once at the end (per `.claude/rules/race-testing.md`).

## 7. Security checklist (Gate 3)

- **Credentials.** Plaintext client secret returned exactly once by `Create` and `ResetSecret`. Adapter forwards it directly into the JSON response and discards. Never logged. The store retains only the bcrypt hash. The `M2MClient` struct returned by `Get`/`List` does not expose `SecretHash` over the wire — verified during adapter unit tests by asserting the response DTO has no `secret_hash`-shaped key.
- **Tenant isolation.** §3.4. Adapter-level filter on every read/write path. Cross-tenant 404 (no existence oracle).
- **Input validation.** Path params validated against the OpenAPI pattern. Query param parsed strictly.
- **Output sanitization.** Errors flow through `common.WriteError`, which already masks 5xx internals.

## 8. Documentation (Gate 4)

- **`cmd/cyoda/help/content/config/*.md`** — add `CYODA_IAM_M2M_ADMIN_ROLE_ENABLED` to the IAM topic.
- **`README.md`** — add env-var row in config table next to `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED`.
- **`internal/auth/iam_features.go::DefaultIAMFeatures`** — zero-value default; comment notes that "false" is the secure default.
- **`v0.8.0` release notes** — restart-wipes-M2M-credentials known limitation paragraph pointing at the umbrella §3.6 follow-up issue.
- **`COMPATIBILITY.md`** — no update needed (no SPI tag, no chart change, no plugin pin guidance change).
- **Umbrella spec cross-reference** — no edit required; umbrella §3.2 already points "here" implicitly.

## 9. Plan handoff

Per the project workflow (`brainstorming → writing-plans → subagent-driven-development → ...`), the next step is invoking `superpowers:writing-plans` against this spec. The plan should produce subagent-dispatchable tasks in roughly this order:

1. Add `M2MAdminRoleEnabled` flag through `IAMFeatures`, `app.IAMConfig`, env plumbing. RED → GREEN per `iam_features.go` validation tests.
2. Add `UpdatedAt` field + store-test deltas.
3. Create `m2m_adapter.go` + `m2m_adapter_test.go` with all 19 unit-test cases red, then green per-method.
4. Wire `account.Handler` constructor for the new store dependency.
5. Remove `/account/m2m` mux entries (`app/app.go`, `internal/auth/service.go`).
6. Delete `internal/auth/m2m.go`, `internal/auth/m2m_test.go`. Delete `TestM2MHandler_*` cases in `admin_authz_test.go`. Delete the `adminSrv` block in `integration_test.go`. Delete the `/account/m2m` table cases in `app/app_test.go`. Each removal: run tests, confirm green.
7. OpenAPI updates: strip 501s, prose flip, `withAdminRole` boolean tightening, audit-table disposition. Regenerate DTOs; adapter wiring catches any shape drift.
8. E2E: `internal/e2e/clients_test.go` + the `M2MAdminRoleEnabled` harness flip.
9. Docs: README + IAM config topic + release notes.
10. Pre-PR: `go test -race ./...`, `go vet ./...`, full E2E suite.
