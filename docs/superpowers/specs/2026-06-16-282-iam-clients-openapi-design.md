# Issue #282 — OpenAPI conformance for `/clients` (technical users)

**Status:** approved (rev. 2, post-review)
**Date:** 2026-06-16
**Issue:** [#282](https://github.com/Cyoda-platform/cyoda-go/issues/282)
**Umbrella:** [`2026-06-04-194-decomposition-design.md`](2026-06-04-194-decomposition-design.md) §3.2
**Sibling precedent:** PR #281 / [`2026-06-04-281-oauth-keys-openapi.md`](../plans/2026-06-04-281-oauth-keys-openapi.md)
**Scope:** Surface the existing M2M client management at the spec-conformant `/clients` paths with the spec-conformant DTOs. Retire `/account/m2m`. Trim the dead `getTechnicalUserToken` chi stub to a defensive interface-satisfying stub. Five OpenAPI operations flip from `501 NOT_IMPLEMENTED` to `match`.

---

## 1. Context

The umbrella decomposition (§3.2) records that M2M client management is **already implemented** in `internal/auth/m2m.go` + `internal/auth/store.go` (`InMemoryM2MClientStore`), wired at `/account/m2m*` via `authSvc.AdminHandler()`. The OpenAPI spec puts technical users at `/clients` instead, so the spec-conformant paths are still `501` in `internal/domain/account/handler.go` (`ListTechnicalUsers`, `CreateTechnicalUser`, `DeleteTechnicalUser`, `ResetTechnicalUserSecret`, `GetTechnicalUserToken`).

`POST /oauth/token` (a.k.a. `getTechnicalUserToken`) is a special case: it is already functional via the public-mux entry `mux.Handle("POST /oauth/token", authSvc.Handler())` in `app/app.go:480`. The public mux precedes the chi handler, so the generated `Account.GetTechnicalUserToken` method is unreachable in normal operation. But the method itself **cannot be deleted** — `api/generated.go` declares `GetTechnicalUserToken` on `ServerInterface` (line ~4796) and `internal/api/server.go` dispatches to it. The method stays as a defensive interface stub returning `500 SERVER_ERROR` with a server-side warning log explaining "should be unreachable"; only the `501` declaration comes off the OpenAPI.

PR #281 has landed the sibling sub-issue (`/oauth/keys/*` conformance). That work established the in-tree template this spec follows:

- Per-endpoint adapter file at `internal/domain/account/<feature>_adapter.go`, paired with `<feature>_adapter_test.go`.
- IAM feature flags in `internal/auth/iam_features.go::IAMFeatures` plumbed through `app.IAMConfig` → `AuthConfig` → `account.Handler.iam`.
- `gateTrustedKeyFeature`-style `404 FEATURE_DISABLED` for runtime-toggled OpenAPI operations.
- `RequireAdmin` (ROLE_ADMIN) as the role gate — emits `403 FORBIDDEN` or `401 UNAUTHORIZED` directly. OpenAPI prose updated to match.
- Body-size-limit harness exercised at the adapter level via `boundedJSONDecode`.

This spec lifts that template directly. Decisions below differ from #281 only where the M2M shape differs.

## 2. Decisions

| # | Decision | Choice | Rationale |
|---|---|---|---|
| D1 | Response code when `withAdminRole=true` is sent and the M2M-admin-role feature flag is off | **`404` with error code `FEATURE_DISABLED`** | Matches the `gateTrustedKeyFeature` precedent. Reviewers don't have to learn a per-endpoint convention. OpenAPI prose ("a 401 Unauthorized response is returned") is updated to match the wire reality. |
| D2 | `clientId` generation for `POST /clients` (no request body in spec) | **16-char crypto/rand base32-hex, uppercase, no padding**, with a one-shot in-memory `store.Get` collision check (retry once on the astronomical collision; on a second collision return `500 SERVER_ERROR` with ticket) | ~80 bits of entropy. Matches the OpenAPI pattern `^[A-Za-z0-9]+$` (`maxLength: 100`, `minLength: 1`) and is short enough to copy-paste comfortably. UUID-stripped-hyphens (32 chars, 122 bits) is the alternative — entropy gain not worth the 2× string length for an admin-issued credential. The collision check is defensive against the silent-upsert behaviour of `InMemoryM2MClientStore.Create` (`store.go:474`). |
| D3 | Fate of `internal/auth/m2m.go` (`M2MHandler`) | **Delete the file + its dedicated test + the `/account/m2m` test cases in sibling tests** | Once §4 lands, `NewM2MHandler` has zero non-test callers. Per Gate 6 ("resolve, don't defer") and umbrella §3.2 ("private-surface cleanup with no public-API impact"), removal is bounded and reversible. |
| D4 | Body-size assertion in `internal/auth/integration_test.go:271-286` | **Delete (don't relocate)** | `POST /clients` declares no request body in the OpenAPI. There is no oversized-body case to assert at the chi adapter. The `boundedJSONDecode` harness is exercised at the trusted-key adapter level already (`trusted_adapter_test.go`). Implication: an oversized POST to `/clients` is read-and-discarded by Go's net/http; documented in §3.3 as expected behaviour, not a vulnerability. |
| D5 | Scope of "SUPER_USER" → "ROLE_ADMIN" prose update | **The five `/clients` operations + the `getTechnicalUserToken` description** | Umbrella §4 says per-sub-issue, aligning prose to code. Other admin endpoints' prose updates ride their own sub-issues. |
| D6 | Timestamp fields on `M2MClient` | **Add both `CreatedAt time.Time` and `UpdatedAt time.Time`** | `TechnicalUserDto.creationDate` is **required** in the generated DTO (`time.Time`, non-pointer). `TechnicalUserDto.lastUpdateDate` is optional. `Create` and `CreateWithSecret` set both to the same `time.Now()`. `ResetSecret` advances `UpdatedAt` only. Two lines of store change + two tests, satisfies Gate 6's "within reason" bound. |
| D7 | New `Handler` constructor dependency | **Inject the existing `M2MClientStore` accessor** (`AuthService.M2MClientStore()`) — no new SPI | The accessor already exists at `internal/auth/service.go:148`. Pass it into the `account.Handler` constructor next to `trustedKeyStore`. |
| D8 | `withAdminRole` query param schema | **Tighten from `type: string` to `type: boolean`** in `api/openapi.yaml` | The adapter interprets `true`/`false` (and `nil`/absent → false). `string` was a generator-friendly punt; `boolean` is what callers actually send. The Cyoda Cloud upstream OpenAPI declares this field as `string`; this is a deliberate (small, locally-justified) divergence from the upstream contract — flagged here so reviewers see it on purpose, not by accident. |
| D9 | E2E coverage location | **New `internal/e2e/clients_test.go`** | Symmetric with PR #281's `oauth_keys_test.go`. Same TestMain harness. The flag-off `FEATURE_DISABLED` case is exercised at the **unit-test** level (where `trusted_adapter_test.go` already toggles `feats.TrustedKeyRegistrationEnabled`), not E2E — spinning a second `TestMain` for one assertion is disproportionate. |
| D10 | v0.8.0 release-notes call-out | **In scope of this PR. File: `CHANGELOG.md`, `## [0.8.0]` block** | M2M creation flips from `501` to functional. A server restart silently wipes credentials (umbrella §3.6 follow-up). Customers need the heads-up; PR-time is the only practical hook. Per project memory: refer to the follow-up by description, not issue ID. |
| D11 | Role strings written into M2M client records | **`"ROLE_M2M"` (always) plus `"ROLE_ADMIN"` (iff `withAdminRole=true` and flag on)** | Matches the system-wide convention (`admin_guard.go:29` checks `"ROLE_ADMIN"`; `streaming.go:58`, `cloudevent.go:48` check `"ROLE_M2M"`; bootstrap default at `app/config.go:160` is `"ROLE_ADMIN,ROLE_M2M"`). The token issuer at `internal/auth/token.go:91` emits these via the `scopes` claim; the validator reads them back into `UserContext.Roles`. Anything else (`"M2M"`, `"ADMIN"`) issues tokens that fail every downstream gate. |
| D12 | Error codes the adapter emits | **Use existing codes only.** `403 FORBIDDEN`, `401 UNAUTHORIZED`, `404 FEATURE_DISABLED`, `400 BAD_REQUEST`, `501 NOT_IMPLEMENTED`, `500 SERVER_ERROR` come from `internal/common/error_codes.go`. Add **one** new code `ErrCodeM2MClientNotFound = "M2M_CLIENT_NOT_FOUND"` mirroring the `ErrCodeTrustedKeyNotFound` precedent. | `RequireAdmin` already emits `FORBIDDEN`/`UNAUTHORIZED` directly — the adapter does not. `INSUFFICIENT_PERMISSIONS` is not a registered code. The new `M2M_CLIENT_NOT_FOUND` rides the per-domain-not-found convention so the help topic is discoverable. |
| D13 | `M2MClient.TenantID` field type | **Promote from `string` to `spi.TenantID`** | Consistent with `TrustedKey.TenantID spi.TenantID` (`store.go:35`) and the `tenantFromCtx(r) spi.TenantID` return. Avoids a forest of `string()` casts at every comparison site. Tiny, Gate-6-bounded refactor; touches the store struct, the store methods that thread the field, the bootstrap call in `app/app.go:266`, and the one or two existing store tests. |
| D14 | `GetTechnicalUserToken` method on `account.Handler` | **Keep as defensive 500 stub** with a `slog.Warn` "should be unreachable; public mux at POST /oauth/token intercepts". Add a `_ = h` comment-line documenting why the method exists. | Interface satisfaction. The public mux always handles `POST /oauth/token` (`app/app.go:480`); arriving here means a routing regression. 500 + log is the safest failure mode. The `501` declaration comes off the OpenAPI per the issue body. |

## 3. Architecture

### 3.1 Files touched

| File | Action |
|---|---|
| `internal/domain/account/m2m_adapter.go` | **New.** Four `Handler` methods (`ListTechnicalUsers`, `CreateTechnicalUser`, `DeleteTechnicalUser`, `ResetTechnicalUserSecret`) + helpers (`requireM2MStore`, `gateM2MAdminRole`, `generateClientID`, `toTechnicalUserDto`, `toTechnicalUserCredentialsDto`). |
| `internal/domain/account/m2m_adapter_test.go` | **New.** Unit tests per §6.1. |
| `internal/domain/account/handler.go` | Delete 4 stub methods (`ListTechnicalUsers`, `CreateTechnicalUser`, `DeleteTechnicalUser`, `ResetTechnicalUserSecret`). Rewrite `GetTechnicalUserToken` to the defensive 500 stub per D14. Add `m2mClientStore auth.M2MClientStore` field to `Handler`; wire it through `NewHandler`. |
| `internal/auth/iam_features.go` | Add `M2MAdminRoleEnabled bool` field + zero default in `DefaultIAMFeatures`. No validation entry needed (boolean). |
| `app/config.go` | Add `M2MAdminRoleEnabled` to `IAMConfig`; read `CYODA_IAM_M2M_ADMIN_ROLE_ENABLED` via `envBool`; thread into the `auth.IAMFeatures` build. |
| `app/app.go:266, 470, 491-492` | Remove the two `mux.Handle("/account/m2m...", ...)` lines; update the route-class comment. Pass the M2M client store into `accountHandler := account.NewHandler(...)`. Bootstrap call (`CreateWithSecret`) sees the `TenantID` field type promotion (D13) — adjust the literal. |
| `internal/auth/service.go:96, 106-107` | Remove `m2mHandler := NewM2MHandler(m2mStore)` and the two `adminMux.Handle("/account/m2m...", ...)` lines. |
| `internal/auth/m2m.go` | **Delete entire file.** |
| `internal/auth/m2m_test.go` | **Delete entire file.** Coverage migrates to `m2m_adapter_test.go`. |
| `internal/auth/admin_authz_test.go` | Delete the three `TestM2MHandler_*` cases (`_NonAdminForbidden` at L64, `_NoUserContextUnauthorized` at L83, `_AdminCanList` at L95). Rewrite the two `TestRequireAdmin_*` cases (L131, L147) to use the keypair handler (`NewKeyPairHandler`) or trusted handler as the fixture instead of `NewM2MHandler` — the guard behaviour they test is handler-agnostic. |
| `internal/auth/integration_test.go:265-286` | Delete the `adminSrv` block (the m2m body-size test). |
| `internal/auth/store.go` | Add `CreatedAt` + `UpdatedAt time.Time` to `M2MClient`. Promote `TenantID` to `spi.TenantID` (D13). Set `CreatedAt`/`UpdatedAt = time.Now()` in `Create` and `CreateWithSecret`. Advance `UpdatedAt` in `ResetSecret`. |
| `app/app_test.go:198-200` | Replace the three `/account/m2m` entries with three `/clients` entries: `{"GET", "/clients"}`, `{"POST", "/clients"}`, `{"DELETE", "/clients/some-client"}`, `{"PUT", "/clients/some-client/secret"}`. Maintains "admin endpoint must require auth" coverage on the new surface. |
| `internal/e2e/clients_test.go` | **New.** Full-stack coverage of the four chi-routed operations + the token-exchange roundtrip. |
| `internal/e2e/e2e_test.go:117` | Add `cfg.IAM.M2MAdminRoleEnabled = true` alongside the existing `TrustedKeyRegistrationEnabled` line. |
| `api/openapi.yaml` | Strip `501` from the five operations; flip SUPER_USER prose to ROLE_ADMIN on those five; tighten `withAdminRole` to `type: boolean`; rewrite the disabled-flag prose to "404 FEATURE_DISABLED". |
| `api/generated.go` | Regenerated from `api/openapi.yaml`. Regenerate via the project's existing generator command (`make generate` or whatever the Makefile target is — to be confirmed in the plan step). |
| `cmd/cyoda/help/content/config/auth.md` | Add `CYODA_IAM_M2M_ADMIN_ROLE_ENABLED` entry next to `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED`. |
| `cmd/cyoda/help/content/errors/FEATURE_DISABLED.md` | Add a `/clients` paragraph listing `POST /clients?withAdminRole=true` as a code-emitting endpoint. |
| `cmd/cyoda/help/content/errors/M2M_CLIENT_NOT_FOUND.md` | **New.** Help topic for the new error code (mirrors `TRUSTED_KEY_NOT_FOUND.md`). |
| `README.md` | Add env-var row in config table next to `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED`. |
| `CHANGELOG.md` (`## [0.8.0]` block) | Add `### Added` row for the four `/clients` endpoints + the new env var. Add `### Removed` row for `/account/m2m*`. Add `### Known limitations` note for restart-wipes-M2M-credentials. |

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

### 3.3 Endpoint behaviour

All four chi-routed handlers follow the same skeleton:

```go
func (h *Handler) <OperationName>(w http.ResponseWriter, r *http.Request, /*path params, query struct*/) {
    if !auth.RequireAdmin(w, r) { return }  // 401 UNAUTHORIZED or 403 FORBIDDEN
    if !h.requireM2MStore(w, r) { return }  // 501 NOT_IMPLEMENTED (nil store = mock IAM mode)
    // ... operation logic ...
}
```

Per-operation:

**`ListTechnicalUsers` → `GET /clients`**
1. `tID := tenantFromCtx(r)` (`spi.TenantID`).
2. `clients := h.m2mClientStore.List()` then filter `client.TenantID == tID`.
3. Map each to `TechnicalUserDto`. Emit JSON array (`[]TechnicalUserDto{}` when empty, not `null`).

**`CreateTechnicalUser` → `POST /clients?withAdminRole=<bool>`**
1. Parse `params.WithAdminRole` (`*bool` after D8 lands). Absent → false.
2. If true: `gateM2MAdminRole` → 404 `FEATURE_DISABLED` when `!h.iam.M2MAdminRoleEnabled`. The flag is read **once** at the top of the handler; if a hot-reload flipped the flag mid-request, the request honours the value observed at function entry. (Hot-reload is not currently a documented behaviour; the field is set at startup.)
3. `clientID := generateClientID()` — 16-char crypto/rand base32-hex uppercase. Run one `store.Get(clientID)` collision check; on collision (≈ 0% probability), regenerate once; on a second collision return `500 SERVER_ERROR` with a ticket. Defensive against the silent-upsert at `store.go:474`.
4. `tenantID := tenantFromCtx(r)`.
5. `roles := []string{"ROLE_M2M"}`; append `"ROLE_ADMIN"` iff `withAdminRole && h.iam.M2MAdminRoleEnabled`.
6. `secret, err := h.m2mClientStore.Create(clientID, tenantID, clientID /*userID==clientID*/, roles)`. Map store errors to `500 SERVER_ERROR` with a ticket; the plaintext secret is never logged.
7. Emit `TechnicalUserCredentialsDto{ClientId: clientID, ClientSecret: secret, GrantType: "client_credentials", ClientSecretExpiresAt: 0}`. All fields are non-pointer in the generated DTO; the `0` for `ClientSecretExpiresAt` means "never expires" per RFC 7591 §3.2.1.

Bodies on this request: declared as none. Go's `net/http` reads and discards an oversized body; we apply no explicit `MaxBytesReader` here because there's no decoder to bound. Documented as expected.

**`DeleteTechnicalUser` → `DELETE /clients/{clientId}`**
1. Validate `clientId` against `^[A-Za-z0-9]+$`, length 1–100. Empty (trailing-slash chi behaviour produces empty path-param) → 400. Pattern miss → 400. (Chi already extracts the param; this is defence-in-depth + matches the OpenAPI schema constraints.)
2. Tenant scope: `client, err := h.m2mClientStore.Get(clientId)`. If err **or** `client.TenantID != tenantFromCtx(r)` → 404 `M2M_CLIENT_NOT_FOUND`. Identical 404 on both → no cross-tenant existence oracle (Gate 3). Log lines on this branch include `clientId` and the caller's tenant — never the cross-tenant owner.
3. `h.m2mClientStore.Delete(clientId)`. A `not found` returned here (race with concurrent delete) maps to the same 404; other errors → 500.
4. Emit `DeleteTechnicalUser200ResponseDto{Message: "M2M client deleted successfully", ClientId: clientId}`.

**`ResetTechnicalUserSecret` → `PUT /clients/{clientId}/secret`**
1. Path-param validation as Delete.
2. Tenant scope check as Delete (same 404 / no existence oracle).
3. `secret, err := h.m2mClientStore.ResetSecret(clientId)`. Store-not-found → 404 `M2M_CLIENT_NOT_FOUND`. Other errors → 500.
4. Emit `TechnicalUserCredentialsDto` (same shape as Create).

**`GetTechnicalUserToken` → `POST /oauth/token`** (per D14)
1. Public-mux at `app/app.go:480` intercepts and handles this. The chi-routed method is unreachable in normal operation.
2. The `account.Handler.GetTechnicalUserToken` method body becomes:
   ```go
   slog.WarnContext(r.Context(), "chi /oauth/token reached — should be intercepted by public mux; routing regression?",
       "method", r.Method, "path", r.URL.Path)
   common.WriteError(w, r, common.Internal("getTechnicalUserToken-unreachable", errors.New("routing regression")))
   ```
3. The `501` declaration is removed from `api/openapi.yaml` for this operation. The 200/400/401/403/500 responses stay accurate for the public-mux implementation.

### 3.4 Tenant isolation (Gate 3)

The `M2MClientStore.List()`, `Get`, `Delete`, `ResetSecret` methods are *not* tenant-aware — they index by `clientID` alone. Tenant isolation lives in the adapter, by filtering on `client.TenantID` against `tenantFromCtx(r)`:

- `List`: post-filter.
- `Get`/`Delete`/`ResetSecret`: read-before-write tenant check; same 404 regardless of "not found" vs "found in another tenant". Same log shape on both branches.

Verification: explicit `m2m_adapter_test.go` cases per operation cover "client exists in tenant A, request from tenant B → 404, store record untouched". After D13 lands `client.TenantID` is `spi.TenantID`, so the comparison is type-direct.

### 3.5 Boundary with the public `POST /oauth/token` flow

`internal/auth/token.go:55, 87, 91` validates `client_credentials` grant via `m2mStore.VerifySecret(clientID, secret)` and emits a token with `sub = clientID` and `scopes = client.Roles`. The store-level `Create` and `ResetSecret` return the plaintext secret exactly once; the store persists only the bcrypt hash (`internal/auth/store.go:324, 346`). The adapter does not log, return, or otherwise touch the secret outside the immediate response body.

D11 ensures the `scopes` claim shape (`["ROLE_M2M", "ROLE_ADMIN"]`) composes correctly with the validator and downstream gates. The `sub = clientID` convention is safe given that `clientID` is now random and not caller-supplied — the token-issuance code does not care about provenance.

No change to `token.go`. The token-exchange path is unaffected.

## 4. Open questions & non-decisions

- **Persistence.** Out of scope. Umbrella §3.6 tracks the follow-up. `CHANGELOG.md` calls this out (D10).
- **Audit table physical location.** I'll locate the exact file during the plan step; the umbrella spec describes the disposition flip but does not pin a path. Candidates: an `audit` block inside the umbrella spec, an inline section in `api/openapi.yaml`, or a free-standing audit file. The plan locates and edits accordingly.
- **`withAdminRole` boolean tightening (D8) = Cloud-spec divergence.** Cyoda Cloud's OpenAPI declares the field as `string`. The umbrella's "API and behavioural fidelity with Cyoda Cloud" goal (CLAUDE.md line 4) is in mild tension here. Two reasons to ship anyway: (i) `boolean` is what real clients send, (ii) `string` is an OpenAPI smell (Cyoda Cloud's spec inherits it from a generator quirk, not a design choice). If Cloud objects we revert the spec and parse in the adapter — adapter code already handles both forms cleanly. The plan flags this for explicit reviewer attention.
- **Transitional state between landing the adapter and removing `/account/m2m`.** Steps 3–5 of §9 leave both surfaces (`/clients/*` chi + `/account/m2m*` adminMux) live simultaneously. Benign — the two surfaces share the same underlying store. Worth one sentence in the cover-letter of the PR so reviewers reading a mid-stack commit don't think the cleanup got skipped.

## 5. Compatibility & migration

- **No SPI change.** `cyoda-go-spi` pin untouched. `COMPATIBILITY.md` untouched.
- **No chart bump.** No env-var on the chart side; the new env var is consumed by the binary only.
- **API surface deletion.** `/account/m2m*` was never OpenAPI-declared. Its removal is private-surface cleanup with no public-API impact.
- **Behavioural change visible to ops:** `POST /clients` flips from `501` to functional. `CHANGELOG.md` paragraph spells out the restart-wipes-credentials limitation referencing the persistence follow-up by description (no issue ID per project convention).
- **Pre-existing operator scripts (if any) hitting `/account/m2m`.** Customer-facing docs never referenced this surface (umbrella §3.2 notes it was not in OpenAPI). Internal/test scripts use either the store directly or `/clients` going forward.

## 6. Tests

### 6.1 Per-adapter unit tests (`internal/domain/account/m2m_adapter_test.go`)

TDD: each red test below maps to a single response shape. Mock IAM store used: real `InMemoryM2MClientStore` (matching `trusted_adapter_test.go`'s pattern of using the real `KVTrustedKeyStore`).

| # | Case | Expected |
|---|---|---|
| 1 | `ListTechnicalUsers` — admin, empty store | 200, `[]` (not `null`) |
| 2 | `ListTechnicalUsers` — admin, two clients in tenant A + one in tenant B; caller in A | 200, body lists A's two only |
| 3 | `ListTechnicalUsers` — non-admin | 403 `FORBIDDEN` |
| 4 | `ListTechnicalUsers` — no user context | 401 `UNAUTHORIZED` |
| 5 | `ListTechnicalUsers` — nil store | 501 `NOT_IMPLEMENTED` |
| 6 | `CreateTechnicalUser` — admin, `withAdminRole` absent | 200, store record has `roles=["ROLE_M2M"]`, plaintext secret present, `CreatedAt == UpdatedAt`, `clientID` matches `^[A-Z0-9]{16}$` |
| 7 | `CreateTechnicalUser` — admin, `withAdminRole=true`, flag on | 200, store record has `roles=["ROLE_M2M","ROLE_ADMIN"]` |
| 8 | `CreateTechnicalUser` — admin, `withAdminRole=true`, flag off | 404 `FEATURE_DISABLED` |
| 9 | `CreateTechnicalUser` — admin, `withAdminRole=false` | 200, roles do **not** include `ROLE_ADMIN` |
| 10 | `CreateTechnicalUser` — non-admin | 403 |
| 11 | `CreateTechnicalUser` — collision on first attempt, success on retry (mock `clientID` source to force one collision) | 200, store record present |
| 12 | `DeleteTechnicalUser` — admin, owned by caller's tenant | 200, body shape matches `DeleteTechnicalUser200ResponseDto`, store no longer has it |
| 13 | `DeleteTechnicalUser` — admin, owned by another tenant | 404 `M2M_CLIENT_NOT_FOUND`, store record untouched |
| 14 | `DeleteTechnicalUser` — admin, unknown id | 404 `M2M_CLIENT_NOT_FOUND` |
| 15 | `DeleteTechnicalUser` — admin, malformed id (e.g. `bad-id` with hyphen) | 400 `BAD_REQUEST` |
| 16 | `DeleteTechnicalUser` — admin, empty id (trailing slash → empty chi param) | 400 `BAD_REQUEST` |
| 17 | `DeleteTechnicalUser` — non-admin | 403 |
| 18 | `ResetTechnicalUserSecret` — admin, owned by caller | 200, secret in response is fresh (`VerifySecret` accepts new + rejects old), store hash mutated, `UpdatedAt > CreatedAt` |
| 19 | `ResetTechnicalUserSecret` — admin, owned by another tenant | 404, store hash untouched |
| 20 | `ResetTechnicalUserSecret` — admin, unknown id | 404 |
| 21 | `ResetTechnicalUserSecret` — admin, malformed id | 400 |
| 22 | `ResetTechnicalUserSecret` — admin, empty id (trailing slash) | 400 |
| 23 | `ResetTechnicalUserSecret` — non-admin | 403 |
| 24 | Response field hygiene: response DTOs serialised across cases 6/7/9/11/12/18 never carry `secret_hash`, `SecretHash`, or any private store field — asserted by JSON-Marshal/round-trip-compare against the OpenAPI-generated DTO type | matches DTO |
| 25 | `CreateTechnicalUser` — admin, `withAdminRole=true`, flag on; observe issued token via the public `/oauth/token` mux: `scopes` claim contains `ROLE_ADMIN`. (Adapter test with an in-package token-handler call; not a full E2E.) | token decodes with both roles |

### 6.2 Store change tests (extending existing `internal/auth/store_test.go` or m2m-specific store test file)

- `Create` stamps `CreatedAt = UpdatedAt = time.Now()` (within a small tolerance).
- `CreateWithSecret` same.
- `ResetSecret` advances `UpdatedAt` strictly forward and does not touch `CreatedAt`.
- `TenantID` field is `spi.TenantID`; all method signatures and bootstrap callers compile (compile-time check).

### 6.3 E2E (`internal/e2e/clients_test.go`)

Through the full HTTP stack with JWT auth (Gate 2). Cases:

- `GET /clients` empty.
- `POST /clients` (no `withAdminRole`) → `GET /clients` lists the new client with `roles: ["ROLE_M2M"]`, `creationDate` populated.
- `POST /clients` → `POST /oauth/token` with `grant_type=client_credentials` and HTTP Basic credentials → 200, JWT decodes with `sub == clientId` and `scopes` claim contains `"ROLE_M2M"`.
- `PUT /clients/{id}/secret` → returned secret authenticates at `POST /oauth/token`; the *old* secret returns 401.
- `DELETE /clients/{id}` → subsequent `GET /clients` not in listing; `POST /oauth/token` with the deleted client's credentials → 401.
- `POST /clients?withAdminRole=true` (E2E harness sets `M2MAdminRoleEnabled = true`) → issued token's `scopes` contains `"ROLE_ADMIN"`; admin-gated endpoint (e.g. `GET /clients`) accepts the token.
- Tenant isolation: two tenants in the same server, tenant B cannot list/delete/reset tenant A's client (all return 404).
- Flag-off `FEATURE_DISABLED` case lives in the unit-test suite (D9), not E2E.

### 6.4 Verification

- `go test ./... -v` green (root module + e2e).
- `make test-short-all` green (plugin submodules — no plugin changes expected, smoke only).
- `go vet ./...` clean.
- `go test -race ./...` once at the end of the deliverable per `.claude/rules/race-testing.md`.

## 7. Security checklist (Gate 3)

- **Credentials.** Plaintext client secret returned exactly once by `Create` and `ResetSecret`. Adapter forwards it directly into the JSON response and discards (no intermediate logging of `secret`). The store retains only the bcrypt hash. The `M2MClient` struct returned by `Get`/`List` exposes `SecretHash` as a struct field; the DTO mapper (`toTechnicalUserDto`) reads only `ClientID`, `Roles`, `CreatedAt`, `UpdatedAt`. Verified by case #24 in §6.1.
- **Tenant isolation.** §3.4. Adapter-level filter on every read/write path. Cross-tenant 404 (no existence oracle). Same log shape on both branches (Q: tenant-A-owns vs no-such-client) so log scraping yields no oracle either.
- **Input validation.** Path params validated against the OpenAPI pattern. Query param parsed strictly (boolean after D8). Body intentionally not decoded on `POST /clients` (no body declared).
- **Output sanitisation.** Errors flow through `common.WriteError` / `common.Internal`, which masks 5xx internals and emits a ticket UUID.
- **Race on flag toggle.** The `withAdminRole`-vs-flag gate reads `h.iam.M2MAdminRoleEnabled` once at function entry. The `IAMFeatures` struct is set at startup; a future hot-reload path would need to either snapshot at startup or use atomic loads — out of scope here.

## 8. Documentation (Gate 4)

- `cmd/cyoda/help/content/config/auth.md` — add `CYODA_IAM_M2M_ADMIN_ROLE_ENABLED` to the IAM section (precedent: `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED` block).
- `cmd/cyoda/help/content/errors/FEATURE_DISABLED.md` — add a paragraph listing `POST /clients?withAdminRole=true` as a code-emitting endpoint.
- `cmd/cyoda/help/content/errors/M2M_CLIENT_NOT_FOUND.md` — new file mirroring `TRUSTED_KEY_NOT_FOUND.md`.
- `README.md` — add env-var row in config table next to `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED`.
- `internal/auth/iam_features.go::DefaultIAMFeatures` — zero-value default; comment notes that `false` is the secure default.
- `CHANGELOG.md` `## [0.8.0]` block — `### Added` (the four `/clients` endpoints, the new env var, the `M2M_CLIENT_NOT_FOUND` error code); `### Removed` (the `/account/m2m*` private surface); `### Known limitations` (the restart-wipes-M2M-credentials note referencing the persistence follow-up by description). No issue IDs in the body per project convention.
- `COMPATIBILITY.md` — no update needed (no SPI tag, no chart change, no plugin pin guidance change).

## 9. Plan handoff

Per the project workflow (`brainstorming → writing-plans → subagent-driven-development → ...`), the next step is invoking `superpowers:writing-plans` against this spec. The plan should produce subagent-dispatchable tasks in roughly this order:

1. **Add `M2MAdminRoleEnabled` through `IAMFeatures`, `app.IAMConfig`, env plumbing.** RED → GREEN per the existing `iam_features.go` validation tests.
2. **Add `CreatedAt` + `UpdatedAt` + promote `TenantID` to `spi.TenantID` on `M2MClient`.** RED via the store tests in §6.2. GREEN by editing the store and the **bootstrap** caller in `app/app.go` (`CreateWithSecret`) to use `spi.TenantID(...)`. Compile-clean before moving on.
3. **Extend `account.Handler` constructor with the M2M store dependency + create `m2m_adapter.go` + `m2m_adapter_test.go`.** Cases 1–25 from §6.1 written RED in one go (per `.claude/rules/tdd.md` "Write the full test suite for the feature first"), then driven GREEN per operation. Constructor change and adapter ship in the same step so the tests can compile.
4. **Remove the `/account/m2m` mux entries** from `app/app.go` and the matching `adminMux` block in `internal/auth/service.go`. Update the route-class comment.
5. **Delete `internal/auth/m2m.go` + `m2m_test.go`. Rewrite/remove `admin_authz_test.go` cases per §3.1. Delete the `adminSrv` block in `integration_test.go`. Update `app/app_test.go` route-presence table to the new `/clients` paths.** Each removal: run `go test ./internal/auth/... ./app/...`, confirm green.
6. **Delete the four stub methods on `account.Handler` and rewrite `GetTechnicalUserToken` to the defensive 500-stub per D14.** Run `go test ./internal/domain/account/... ./internal/api/...`, confirm green.
7. **OpenAPI updates:** strip 501s on the five operations, prose flip to ROLE_ADMIN, tighten `withAdminRole` to `boolean`, rewrite disabled-flag prose. Regenerate `api/generated.go` via the project generator command (to be confirmed during plan step). Adapter wiring catches any shape drift; rebuild + tests.
8. **E2E (`internal/e2e/clients_test.go`)** + the `M2MAdminRoleEnabled = true` line in the harness.
9. **Docs:** README + `auth.md` + `FEATURE_DISABLED.md` + new `M2M_CLIENT_NOT_FOUND.md` + `CHANGELOG.md`.
10. **Pre-PR:** `go test -race ./...`, `go vet ./...`, full E2E suite. Then `git commit` history scrub for any accidental `#282` strings in shipped artefacts (per project memory).
