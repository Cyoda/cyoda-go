# #281 — OpenAPI conformance for `/oauth/keys/*` (keypair + trusted)

**Status:** approved
**Date:** 2026-06-04
**Issue:** [#281](https://github.com/Cyoda-platform/cyoda-go/issues/281)
**Milestone:** v0.8.0
**Sub-issue of:** #194 (decomposition spec: `docs/superpowers/specs/2026-06-04-194-decomposition-design.md` §3.1)
**Revision:** rev2 (post fresh-context senior-architect review — see review summary in commit log)

---

## 1. Goal

Make the 10 JWT-keypair + trusted-key admin operations respond with the OpenAPI-declared DTOs through the generated chi router, remove the legacy `/oauth/keys/` prefix mux entry that masks the OpenAPI surface today, and reconcile cyoda-go's behaviour with the Cyoda Cloud reference implementation wherever the cloud's choices are reachable without bringing in dependencies (KMS, scheduler, multi-issuer validators) that belong to other follow-ups.

The 10 operations:

| Operation | Method | Path |
|---|---|---|
| `issueJwtKeyPair` | POST | `/oauth/keys/keypair` |
| `getCurrentJwtKeyPair` | GET | `/oauth/keys/keypair/current?audience=...` |
| `deleteJwtKeyPair` | DELETE | `/oauth/keys/keypair/{keyId}` |
| `invalidateJwtKeyPair` | POST | `/oauth/keys/keypair/{keyId}/invalidate` |
| `reactivateJwtKeyPair` | POST | `/oauth/keys/keypair/{keyId}/reactivate` |
| `listTrustedKeys` | GET | `/oauth/keys/trusted` |
| `registerTrustedKey` | POST | `/oauth/keys/trusted` |
| `deleteTrustedKey` | DELETE | `/oauth/keys/trusted/{keyId}` |
| `invalidateTrustedKey` | POST | `/oauth/keys/trusted/{keyId}/invalidate` |
| `reactivateTrustedKey` | POST | `/oauth/keys/trusted/{keyId}/reactivate` |

---

## 2. Cloud reconciliation

A deep scan of the Kotlin reference implementation at `/Users/paul/dev/cyoda/backend/src/main/kotlin/net/cyoda/saas` (controllers under `controller/`, services `JwtKeyPairInteractor.kt`, `StoredJWKService.kt`, `TrustedKeyRegistrationService.kt`, `IAMProperties.kt`) informed the table below. Where cyoda-go can match cloud behaviour without out-of-scope dependencies, it does. Documented divergences are explicit in §3.3.

| Capability | Cloud behaviour | cyoda-go decision |
|---|---|---|
| `algorithm` enum (10 values) | All supported (RS*, PS*, ES*, EdDSA) | RS*+PS* implemented (same RSA-2048 keypair, signing-method dispatch); ES*+EdDSA always rejected with 400 `UNSUPPORTED_ALGORITHM` (follow-up issue tracks adding generators) |
| `audience` (human\|client) | Partitioned via per-audience providerId | Stored on `KeyPair`; `GetActive(audience)` partitions; bootstrap key gets configurable default audience |
| `validFrom`/`validTo` | Explicit timestamps; lazy `isValidKey(now)` filter | Same — `ValidTo *time.Time`; lazy filter at JWKS and trusted-key validator reads |
| `invalidateCurrent` / `invalidatePrevious` + `gracePeriodSec` | Lazy validity (`validTo = now + grace`), no timers | Same — atomic mutex-scoped flip via single `Save`/`Register` call carrying `IssueOptions{InvalidateOthers, GracePeriodSec}` |
| Standalone `POST .../invalidate` body | `InvalidateKeyRequestDto { gracePeriodSec: int? = 0 }` | Same |
| `reactivate` semantics | Clears `validTo` (original expiry lost; TODO in cloud) | Same; documented limitation |
| `jwk` on `TrustedKeyResponseDto` | Decoded then re-serialised via `Jwks.builder()` | Stored as `json.RawMessage` and emitted verbatim (OpenAPI declares `jwk: object`; simpler) |
| `legalEntityId` + tenant scoping | Per-tenant partitioning via `owner = legalEntityId` | Same — `TenantID` on `TrustedKey`; CRUD scoped by `uc.Tenant.ID`; mapped to wire as `legalEntityId = string(tk.TenantID)` |
| Cross-tenant keyId collision on register | `409 Conflict` | Same — `KEY_OWNED_BY_DIFFERENT_TENANT` |
| Cross-tenant lifecycle access | **403 FORBIDDEN** (leaks existence) | **404** (no leakage) — documented divergence (cyoda-go more secure; see §3.3) |
| JWK content checks | `kty` required, `kid ≡ keyId`, max 20 fields | Same — implemented at validate-boundary |
| Trusted-key cap | Per-tenant, default 10, returns **400** | Same — `CYODA_IAM_TRUSTED_KEY_MAX_PER_TENANT` (default 10); 400 `TRUSTED_KEY_CAP_REACHED`. Resolves the existing `TODO(#163)`. |
| `validTo` cap | `validFrom + 365d` max | Same — `CYODA_IAM_TRUSTED_KEY_MAX_VALIDITY_DAYS` (default 365); over-cap requests clamped at insert; explicit reject if past clamp is operationally identical |
| `trustedKeyRegistrationEnabled` flag | Default **false**; gates all 5 trusted-key ops with **404** | Same — `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED` (default `false`); 404 `FEATURE_DISABLED` |
| Private signing-key persistence | Encrypted PKCS#8 blob in DB | Out of scope — held in-memory; runtime-issued keys lost on restart; bootstrap (PEM-derived deterministic KID) survives. Per follow-up §3.5. |
| Role gate | ADMIN ∨ SUPER_USER | **ROLE_ADMIN only**, per issue body. OpenAPI prose updated SUPER_USER → ROLE_ADMIN. |
| Public JWKS endpoint | None — verification uses direct `findKeyEntryByKeyId` | cyoda-go retains `/.well-known/jwks.json`; sourced from `ListForVerification()` so grace-period keys remain discoverable until `ValidTo` |

---

## 3. Bucketing

### 3.1 In scope — implemented in this PR

1. **`algorithm` enum (RS*+PS* subset)** — `RS256, RS384, RS512, PS256, PS384, PS512`. All six use the same RSA-2048 keypair; signing-method dispatch picks the digest+padding at sign time. Default `RS256` when absent. Stored on `KeyPair.Algorithm`. Round-tripped in response. ES*/EdDSA always rejected with 400 `UNSUPPORTED_ALGORITHM`; a follow-up issue (filed at PR-merge time) tracks adding ECDSA + Ed25519 generators.
2. **`publicKey` base64-DER on `JwtKeyPairResponseDto`** — `base64.StdEncoding.EncodeToString(x509.MarshalPKIXPublicKey(pub))`. **No PEM armor.** Matches OpenAPI's "Base64-encoded public key in X.509 SubjectPublicKeyInfo format" and cloud's `Base64.getEncoder().encodeToString(publicKey)` at `JwtKeyPairInteractor.kt:85`.
3. **`validFrom`/`validTo` timestamps on `KeyPair` and `TrustedKey`** — rename `KeyPair.CreatedAt → ValidFrom`; add `KeyPair.ValidTo *time.Time` and confirm/keep `TrustedKey.ValidTo *time.Time`. Nil `ValidTo` = no expiry (signing keypair) or capped at `validFrom + maxValidityDays` (trusted key). Set to `now + gracePeriodSec` at invalidate time. Lazy filter at all verification reads: `ValidTo == nil || now.Before(*ValidTo)`.
4. **`validFrom`/`validTo` overrides on issue/register** — honoured when supplied. Defaults: `validFrom = now()`. Default `validTo`: nil (signing keypair); `validFrom + 365d` (trusted key, matches cloud). User-supplied `validTo` clamped at `validFrom + CYODA_IAM_TRUSTED_KEY_MAX_VALIDITY_DAYS` for trusted keys; if supplied value is past the clamp it is silently clamped (no error) — operator-visible via the response's `validTo`. Reject `validTo < validFrom` with 400 `BAD_REQUEST` for both.
5. **`jwk` round-trip on trusted-key response** — store the raw `json.RawMessage` on `TrustedKey`; emit verbatim. Existing JWK validation (`parseRSAPublicKeyFromJWK`) gates incoming registrations. Three new content checks added at validate-boundary (match cloud `TrustedKeyRegistrationService.kt:119-130, 262-269`):
   - JWK must contain `kty` (already implicit in RSA path; now explicit).
   - JWK `kid` field, if present, must equal request `keyId` — else 400 `BAD_REQUEST` (security: prevents `keyId=trusted-app` with `jwk.kid=attacker-kid` injection).
   - JWK max 20 properties (configurable via `CYODA_IAM_TRUSTED_KEY_MAX_JWK_PROPERTIES`, default 20) — else 400 `BAD_REQUEST`.
6. **`legalEntityId` + per-tenant trusted-key scoping** — add `TenantID spi.TenantID` to `TrustedKey`. CRUD methods scoped by caller's `uc.Tenant.ID`. Cross-tenant `Get`/`Delete`/`Invalidate`/`Reactivate` returns **404** `TRUSTED_KEY_NOT_FOUND` (does not leak existence; cyoda-go diverges from cloud's 403 for this reason — see §3.3). `Register` with keyId-collision against another tenant returns **409** `KEY_OWNED_BY_DIFFERENT_TENANT`. On the wire: `legalEntityId = string(tk.TenantID)`.
7. **`invalidateCurrent` (issue) / `invalidatePrevious` (register) booleans + `invalidateGracePeriodSec`** — folded into the `Save`/`Register` call signatures via `IssueKeyPairOptions{InvalidateCurrent bool, GracePeriodSec int64}` and `RegisterTrustedKeyOptions{InvalidatePrevious bool, GracePeriodSec int64}`. Single mutex acquisition: under one critical section the store inserts the new key and flips siblings' `Active=false`, `ValidTo=now+grace`. (Same partition definition: keypairs = same `audience`; trusted keys = same `tenantID`.) Grace=0 ⇒ immediate. Lazy filter at JWKS endpoint and validator paths means tokens signed with the just-invalidated kid continue to verify until grace passes. `gracePeriodSec < 0` rejected with 400 `BAD_REQUEST` at adapter boundary.
8. **Standalone `POST .../invalidate` request body** — `InvalidateKeyRequestDto { gracePeriodSec int64 }`; absent body, nil, or zero ⇒ immediate. Both keypair and trusted-key invalidate endpoints accept the body. Negative values rejected with 400.
9. **`audience` (human\|client) on keypairs** — add `Audience string` to `KeyPair`. New `KeyStore.GetActive(audience string)` partitions. `issueJwtKeyPair` partitions by request audience. `getCurrentJwtKeyPair?audience=X` returns **404** `KEYPAIR_NOT_FOUND` if no active key for X. Bootstrap key gets audience `CYODA_JWT_BOOTSTRAP_AUDIENCE` (default `client`, alternative `human`); the existing M2M token-signing path (`POST /oauth/token`) becomes `GetActive("client")`. Pre-merge verification step: grep all `KeyStore.GetActive()` call sites and confirm none implicitly sign human-audience tokens today; if any exist, this defaults need revisiting. Adapter must call `params.Audience.Valid()` and reject invalid enums with 400 `BAD_REQUEST`.
10. **Reactivate** — `Reactivate(tenantID, kid)` (trusted) and `Reactivate(kid)` (keypair) clear `ValidTo` to nil and set `Active = true`. Ownership check happens inside the store (`Reactivate(tenantID, kid)` looks up via tenant-scoped Get first; cross-tenant returns 404). Original expiry permanently lost (matches cloud; documented).
11. **`keyId` validation on path-parameter lifecycle ops** — apply `trustedKIDPattern` (`^[A-Za-z0-9._-]{1,128}$`) ONLY to user-controlled path parameters on `deleteTrustedKey`, `invalidateTrustedKey`, `reactivateTrustedKey`. **Not** applied to keypair path-parameter lifecycle ops (`deleteJwtKeyPair`, `invalidateJwtKeyPair`, `reactivateJwtKeyPair`): match cloud, drop the validator, let the store lookup return 404. Server-generated KIDs (16-byte hex from `internal/auth/keys.go:99`) are validated by construction — adding regex checks gains nothing. Per-spec: keypair lifecycle ops declare 401/403/404/500; no 400 on the wire.

### 3.2 Documented divergences from cloud

1. **JWKS endpoint** — cyoda-go retains `/.well-known/jwks.json`; cloud has no public JWKS surface. The cyoda-go endpoint sources from `KeyStore.ListForVerification()` so grace-period keys remain published until `ValidTo` passes.
2. **Role gate** — cyoda-go enforces **ROLE_ADMIN** only; cloud accepts ADMIN ∨ SUPER_USER. Per issue body, the smaller-blast-radius change is to align OpenAPI prose to code (`SUPER_USER` → `ROLE_ADMIN`).
3. **Cross-tenant lifecycle leakage** — cyoda-go returns **404 TRUSTED_KEY_NOT_FOUND**; cloud returns 403 with a body that confirms the key exists in another tenant. cyoda-go's choice prevents tenant-existence enumeration; intentional security improvement.
4. **Trusted-key `audience` round-trip** — cyoda-go honors the request `audience` and round-trips it; cloud always uses `human` (per its single-providerId model). Documented; spec offers more semantic surface than cloud here.
5. **Signing-key persistence** — out of scope; runtime-issued keypairs lost on restart. Bootstrap key (PEM-derived deterministic KID per ARCHITECTURE.md §7.2) survives. v0.8.0 release notes call this out and link follow-up §3.5.

---

## 4. Components

### 4.1 Type extensions (`internal/auth/store.go`)

```go
type KeyPair struct {
    KID        string
    Audience   string          // NEW: "human" | "client"
    Algorithm  string          // NEW: one of RS256/RS384/RS512/PS256/PS384/PS512
    PublicKey  *rsa.PublicKey
    PrivateKey *rsa.PrivateKey
    Active     bool
    ValidFrom  time.Time       // RENAMED from CreatedAt
    ValidTo    *time.Time      // NEW: nil = no expiry; set on invalidate
}

type TrustedKey struct {
    KID       string
    TenantID  spi.TenantID     // NEW
    JWK       json.RawMessage  // NEW: original JWK for round-trip
    PublicKey *rsa.PublicKey
    Audience  string
    Issuers   []string
    Active    bool
    ValidFrom time.Time
    ValidTo   *time.Time
}

type IssueKeyPairOptions struct {
    InvalidateCurrent bool
    GracePeriodSec    int64
}

type RegisterTrustedKeyOptions struct {
    InvalidatePrevious bool
    GracePeriodSec     int64
}
```

### 4.2 Store interface changes

```go
type KeyStore interface {
    Save(kp *KeyPair, opts IssueKeyPairOptions) error      // NEW signature: opts folds in atomic invalidate-current
    Get(kid string) (*KeyPair, error)
    GetActive(audience string) (*KeyPair, error)            // CHANGED: audience param
    List() []*KeyPair                                       // all entries (admin listing)
    ListForVerification() []*KeyPair                        // NEW: lazy ValidTo filter
    Delete(kid string) error
    Invalidate(kid string, gracePeriodSec int64) error      // CHANGED: grace param
    Reactivate(kid string) error
}

type TrustedKeyStore interface {
    Register(tk *TrustedKey, opts RegisterTrustedKeyOptions) error   // 409 on cross-tenant keyId collision; 400 on cap reached
    Get(tenantID spi.TenantID, kid string) (*TrustedKey, error)      // CHANGED: tenant-scoped
    GetForVerification(kid string) (*TrustedKey, error)              // NEW: kid-only lookup for JWT-bearer-assertion grant; lazy ValidTo filter applied; bypasses tenant scoping intentionally — caller responsible for downstream tenant invariant checks
    List(tenantID spi.TenantID) []*TrustedKey                        // CHANGED: tenant-scoped
    ListForVerification() []*TrustedKey                              // NEW: all tenants, ValidTo-filtered
    Delete(tenantID spi.TenantID, kid string) error
    Invalidate(tenantID spi.TenantID, kid string, gracePeriodSec int64) error
    Reactivate(tenantID spi.TenantID, kid string) error
}
```

`Save`/`Register` atomicity: a single mutex acquisition inserts the new entry and (if `opts.InvalidateCurrent`/`InvalidatePrevious`) flips siblings in the same partition to `Active=false`, `ValidTo=now+grace`. This replaces the rev1 `InvalidateOthersInPartition`/`...ForTenant` helpers which invited non-atomic call sequences.

### 4.3 Handler struct changes (`internal/domain/account/handler.go`)

The `Handler` struct gains three fields:
```go
type Handler struct {
    authSvc          contract.AuthenticationService
    authzSvc         contract.AuthorizationService
    keyStore         auth.KeyStore                // NEW: read-only via interface
    trustedKeyStore  auth.TrustedKeyStore         // NEW: read-only via interface
    iam              auth.IAMConfig                // NEW: value-struct feature flags + caps, copied at construction
}
```

`auth.IAMConfig` is a small new value struct (likely added to `internal/auth/service.go` next to `AuthConfig`) exposing the IAM feature-flag subset:
```go
type IAMConfig struct {
    TrustedKeyRegistrationEnabled bool
    TrustedKeyMaxPerTenant        int
    TrustedKeyMaxValidityDays     int
    TrustedKeyMaxJWKProperties    int
    BootstrapAudience             string
}
```

It is derived from the new `AuthConfig` fields (§4.7) and copied into the Handler at construction. Per `.claude/rules/ownership-mutability.md` rule 3, stores are shared via interface; the IAMConfig is value-copied (read-only data, no shared mutable state).

`account.New(...)` signature updated to accept these three new dependencies; `app.go:443` call site passes them through.

### 4.4 Adapter files (handler.go split)

The existing `internal/domain/account/handler.go` keeps the account methods. The 10 new methods live in:

- `internal/domain/account/keys_adapter.go` — 5 keypair handler methods + DTO helpers (`toJwtKeyPairResponse`, `parseIssueRequest`, `boundedJSONDecode`).
- `internal/domain/account/trusted_adapter.go` — 5 trusted-key handler methods + DTO helpers (`toTrustedKeyResponse`, `tenantFromContext`).

Both files take `*Handler` as the receiver.

### 4.5 Removed (dead code)

- `internal/auth/keys.go` — `KeysHandler`, `keysInfoResponse`, all `ServeHTTP` path-parsing. Domain logic moves inline to the new adapter (small enough not to warrant a service abstraction).
- `internal/auth/trusted.go` — `TrustedKeysHandler`, `trustedKeyInfoResponse`, `ServeHTTP`, `handleList/Register/Delete/Invalidate/Reactivate`, `validateLifecycleKID`, `extractKeyID`. The input-validation helpers (`trustedKIDPattern`, `parseRSAPublicKeyFromJWK`, `validateRSAPublicExponent`) move to a small `internal/auth/keyvalidation.go` so the new adapter can reuse them. `decodeBase64URL` lives in `internal/auth/jwt.go` and is unaffected.
- `internal/auth/service.go` adminMux entries for `/oauth/keys/...` (lines 86–89). The `/account/m2m` entries stay (#194-B territory).
- `app/app.go:482` `mux.Handle("/oauth/keys/", ...)` — removed.

### 4.6 Exporting `requireAdmin`

`internal/auth/admin_guard.go:22` `requireAdmin` is currently package-private. Exported as `auth.RequireAdmin(w, r) bool` so the new adapters in `internal/domain/account/` can call it. Existing in-package callers (`keys.go`/`trusted.go`/`m2m.go`) update to the new name during the rename.

### 4.7 Wiring changes (`app/app.go`)

- Bootstrap-key save now sets `Algorithm = "RS256"` and `Audience = config.Bootstrap.Audience` (new field; default `client`).
- New `AuthConfig` fields:
  - `BootstrapAudience string` (default `client`)
  - `TrustedKeyRegistrationEnabled bool` (default `false`)
  - `TrustedKeyMaxPerTenant int` (default `10`)
  - `TrustedKeyMaxValidityDays int` (default `365`)
  - `TrustedKeyMaxJWKProperties int` (default `20`)
- M2M signing call sites switch to `GetActive("client")`.
- Token-verification path (`internal/auth/token.go:137`) switches from `Get(kid)` to `GetForVerification(kid)`.
- JWKS endpoint source switches to `KeyStore.ListForVerification()`.

---

## 5. Error handling

### 5.1 New error codes (`internal/common/error_codes.go`)

| Code | HTTP | Trigger |
|---|---|---|
| `FEATURE_DISABLED` | 404 | Trusted-key endpoint while `TrustedKeyRegistrationEnabled=false` |
| `UNSUPPORTED_ALGORITHM` | 400 | `algorithm ∈ {ES*, EdDSA}` |
| `KEY_OWNED_BY_DIFFERENT_TENANT` | 409 | `registerTrustedKey` keyId collision with another tenant |
| `KEYPAIR_NOT_FOUND` | 404 | Keypair lifecycle on missing kid; `getCurrent` with no key for requested audience |
| `TRUSTED_KEY_CAP_REACHED` | 400 | Per-tenant trusted-key cap reached on register |

`TRUSTED_KEY_NOT_FOUND` already exists and is reused for tenant-scoped lookups (returned uniformly whether the kid doesn't exist or belongs to another tenant — no existence leakage).

### 5.2 Wire shape: ProblemDetail (RFC 9457)

`api/openapi.yaml` currently declares every 4xx/5xx for the 10 operations as `$ref ErrorResponseDto` (OAuth-2 RFC 6749 shape `{error, error_description, error_uri}`). cyoda-go emits **RFC 9457 ProblemDetail** via `common.WriteError`. The OpenAPI spec must be updated to match the wire reality per the established pattern in `docs/superpowers/audits/2026-04-29-openapi-conformance-audit.md:93,101`:

- Replace `$ref ErrorResponseDto` with `$ref ProblemDetail` on every 4xx/5xx response declaration for the 10 operations.
- Switch the content-type to `application/problem+json`.

`ProblemDetail` schema already exists in `api/openapi.yaml`; no new component definition needed.

### 5.3 Input validation

Per `.claude/rules/security.md`, validate at the boundary:

| Field | Check | Failure |
|---|---|---|
| `audience` (request body + query) | `params.Audience.Valid()` from generated enum | 400 `BAD_REQUEST` |
| `algorithm` | enum + in {RS256, RS384, RS512, PS256, PS384, PS512} | ES*/EdDSA → 400 `UNSUPPORTED_ALGORITHM`; non-enum → 400 `BAD_REQUEST` |
| `gracePeriodSec` (anywhere) | `>= 0` | 400 `BAD_REQUEST` |
| `validTo` (request) | `>= validFrom` | 400 `BAD_REQUEST` |
| trusted-key `keyId` (path param) | `trustedKIDPattern` regex `^[A-Za-z0-9._-]{1,128}$` | 400 `BAD_REQUEST` |
| trusted-key `keyId` (request body) | same regex | 400 `BAD_REQUEST` |
| trusted-key JWK | `kty` required; `kid` (if present) == `keyId`; ≤ `MaxJWKProperties` fields | 400 `BAD_REQUEST` |
| keypair `keyId` (path param) | not validated; store lookup returns 404 (matches cloud) | — |
| request body size (all 4 POSTs) | `http.MaxBytesReader(w, r.Body, 1<<20)` via shared `boundedJSONDecode` helper | 413 Request Entity Too Large or 400 |

### 5.4 Security checks (Gate 3)

- **Tenant isolation** — `TenantID` derived from `uc.Tenant.ID` server-side, never from request body. Cross-tenant access returns 404 (lookup) or 409 (collision on register), never 200+leak or 403+confirmation.
- **In-memory cache isolation** (`KVTrustedKeyStore.keys`) — value carries `TenantID`. All cache reads via tenant-scoped methods verify `cached.TenantID == requested.TenantID` before returning; the verification-only `GetForVerification(kid)` is the documented exception, used exclusively by the JWT-bearer-assertion grant path where the resolved key's tenant becomes the assertion's principal-tenant.
- **No secrets in logs** — bootstrap PEM stays unprinted; rotated kids are logged only when an operation references them. Private RSA material never serialised in any response or log line. slog field allowlist: `kid`, `tenant`, `audience`, `algorithm`.
- **No issue IDs in shipped artefacts** — per the standing rule, no `#281` in error messages, response bodies, OpenAPI prose, code comments, or help topics.
- **No stack traces in 5xx responses** — `common.Internal` ticket pattern only.

---

## 6. Persistence

- `InMemoryKeyStore` and `InMemoryTrustedKeyStore` updated for new field/signature shapes.
- `KVTrustedKeyStore` (already wired in production via `app/app.go:207–239`):
  - KV-key encoding changes from `trustedkey:<kid>` to `trustedkey:<tenantID>:<kid>` to make tenant isolation an invariant of the storage layer itself.
  - `trustedKeyRecord` serialization schema gains two fields: `tenantID string` and `jwk json.RawMessage`. Backward-compatible on read: missing fields handled gracefully (existing entries pre-rev2 don't have either; see backfill below).
  - In-memory cache map remains keyed by raw `kid` for O(1) verification path access; the cached value's `TenantID` field is what tenant-scoped methods consult.
  - **Backfill at `loadAll()`** — entries with old key shape `trustedkey:<kid>` (no tenant segment) are skipped. For each skipped entry, log at INFO with `kid`, the affected key path, and a single one-shot startup WARN message pointing operators at v0.8.0 release notes. No silent loss: every skipped kid is visible. cyoda-go has no production users on this surface today (verified during issue triage), so the operational hostility flagged by reviewer is acceptable in exchange for not running migration logic during boot.
- Signing-key persistence — out of scope (follow-up §3.5). Bootstrap key survives restart; runtime-issued keypairs do not.
- **Lazy ValidTo and stale entries** — known limitation: invalidated keys past `ValidTo` are filtered at every read but never pruned. With per-tenant cap of 10, a tenant rotating quarterly accumulates 4 stale entries/year. Documented; periodic prune is a follow-up if it becomes a problem.

---

## 7. Routing wiring

1. `app/app.go:482` `mux.Handle("/oauth/keys/", ...)` — removed.
2. `internal/auth/service.go` adminMux — drop `/oauth/keys/keypair` (×2) and `/oauth/keys/trusted` (×2) entries. `/account/m2m` entries stay until #194-B.
3. Chi router (mounted at `/` via the generated `HandlerFromMux`) now owns all `/oauth/keys/*` paths via the 10 `ServerInterface` methods on `*Handler`.
4. `auth.RequireAdmin(w, r)` called inline at the start of each adapter method (no middleware change; matches existing pattern in `admin_guard.go`).

---

## 8. Body-size relocation

`internal/auth/integration_test.go:268–301` has two sub-tests routing through `svc.AdminHandler()`:

- `m2m create endpoint rejects oversized body` — left untouched (#194-B territory).
- `trusted key register endpoint rejects oversized body` — relocated: a shared helper `account.boundedJSONDecode(r, 1<<20, &dst)` wraps `http.MaxBytesReader` + `json.Decoder.Decode`; called by all four POST adapters (issue keypair, register trusted key, invalidate keypair, invalidate trusted key). E2E test asserts the same 413/400 behaviour by POSTing >1 MB through chi. The integration-level sub-test is deleted.

---

## 9. Testing strategy (Gates 1, 2, 5)

### 9.1 Unit (TDD red-first)

- `internal/auth/store_test.go` — audience-scoped `GetActive`; grace-period `Invalidate` sets `ValidTo`; `ListForVerification` lazy filter; reactivate clears `ValidTo`; cross-tenant trusted-key isolation; same-tenant upsert vs cross-tenant 409; per-tenant cap reached → `TRUSTED_KEY_CAP_REACHED`; `validTo` cap clamped at `validFrom + MaxValidityDays`; JWK `kid` ≠ `keyId` rejected; JWK > `MaxJWKProperties` rejected; `Save`/`Register` with options atomically flip siblings; `Reactivate` cross-tenant returns 404 from the store (not just the adapter).
- `internal/auth/kv_trusted_store_test.go` — tenant-scoped key prefix; old-shape entries skipped with INFO log per entry + one-shot startup WARN; record schema round-trips `tenantID` and `jwk`.
- `internal/auth/keypair_signing_test.go` (new) — sign + verify a sample JWT for each of `RS256/RS384/RS512/PS256/PS384/PS512`; ES256/ES384/ES512/EdDSA rejected.
- `internal/auth/token_test.go` — `GetForVerification` path returns trusted key by kid regardless of tenant; the verification path then enforces principal-tenant matching downstream.

### 9.2 Adapter

- `internal/domain/account/keys_adapter_test.go`, `trusted_adapter_test.go` — per-operation DTO marshalling round-trip; ROLE_ADMIN gate (401 unauth, 403 wrong role); validation error codes; response shape matched against the generated DTOs; `publicKey` is base64-DER, not PEM; `legalEntityId == string(uc.Tenant.ID)`; ProblemDetail wire shape on every 4xx/5xx (content-type `application/problem+json`); audience-query enum validation.
- `algorithm=ES256` → 400 `UNSUPPORTED_ALGORITHM`.
- `TrustedKeyRegistrationEnabled=false` → 404 `FEATURE_DISABLED` on all 5 trusted-key endpoints.
- Body-size limit: POST > 1 MB to any of the 4 POST adapters → 413 or 400.
- Per documented divergence: cross-tenant lifecycle returns 404 (regression-locks the security choice vs cloud's 403).

### 9.3 E2E (Gate 2)

- `internal/e2e/oauth_keys_test.go` — one scenario per operation through the full HTTP stack, authenticated via the existing bootstrap-M2M-client → `POST /oauth/token` → Bearer flow.
- **Grace-period round-trip** — issue keypair A, issue keypair B with `invalidateCurrent=true, invalidateGracePeriodSec=2`. Assert A's kid is in `/.well-known/jwks.json` immediately after; `sleep(3s)`; assert A's kid is no longer in JWKS. (The cross-process token round-trip is covered at the unit-test layer because E2E cannot reach the rotated key's private half.)
- **Persistence** — register a trusted key via `POST /oauth/keys/trusted`, restart the in-process server against the same store factory, assert `GET /oauth/keys/trusted` still returns it.
- **Cross-tenant isolation** — bootstrap two M2M clients in distinct tenants; register the same `keyId` from tenant A; register from tenant B → 409 `KEY_OWNED_BY_DIFFERENT_TENANT`; `GET` from tenant B → 404 (does not list tenant A's key).
- **Feature-flag** — with `TrustedKeyRegistrationEnabled=false`, all 5 trusted-key endpoints return 404 `FEATURE_DISABLED`; keypair endpoints unaffected.
- **JWT-bearer-assertion grant via trusted key** — register trusted key with tenant A, mint a subject token signed by that key, present at `POST /oauth/token` grant=`urn:ietf:params:oauth:grant-type:jwt-bearer` (or the spec's equivalent), assert successful token exchange. Same flow with the trusted key from tenant B but with `caas_org_id` claim of tenant A → rejected (principal-tenant mismatch). Guards the tenant-scoped `GetForVerification` decision.

### 9.4 Verification gates

- `go test ./... -v` (root incl. `internal/e2e`) — green after every step.
- `go vet ./...`.
- `make test-all` once before PR (root + `plugins/memory|sqlite|postgres`).
- `go test -race ./...` once before PR (per `.claude/rules/race-testing.md`).

### 9.5 TDD ordering

1. Store-layer types + tests → green.
2. Adapter methods + tests → green.
3. Wire chi handlers + remove legacy mux entries → existing tests still green; new E2E tests added → green.
4. Documentation + audit-table updates in same PR.

### 9.6 Parity tests (cyoda-go-cassandra)

cyoda-go-cassandra (`../cyoda-go-cassandra`) does not consume `internal/auth` interfaces (verified during review). No parity-registry update needed. No SPI change.

---

## 10. Documentation updates (Gate 4)

### 10.1 OpenAPI spec (`api/openapi.yaml`)

- Remove `501 NotImplemented` declarations from all 10 operations.
- Replace `$ref ErrorResponseDto` (RFC 6749 OAuth-error shape) with `$ref ProblemDetail` + `application/problem+json` content-type on **every** 4xx/5xx response declaration of the 10 operations. Matches established audit pattern.
- Replace `SUPER_USER` → `ROLE_ADMIN` in the 10 operation descriptions (the role-gate divergence from cloud is intentional per §3.2).
- Keep the `trustedKeyRegistrationEnabled` 404 declarations and prose — they accurately describe the implementation.
- Embedded `//go:embed` of `api/openapi.yaml` automatically picks the changes up; no oapi-codegen regeneration needed (per `project_openapi_spec_embed_via_goembed`).

### 10.2 Audit table (`docs/superpowers/audits/2026-04-29-openapi-conformance-audit.md`)

For each of the 10 rows (lines 122–131), change disposition from `out-of-scope-not-implemented (#194)` to `match`. Cite the merge commit at PR-merge time.

### 10.3 Cyoda help (`cmd/cyoda/help/content/`)

**`config/auth.md`** — new entries:
- Under JWT mode: `CYODA_JWT_BOOTSTRAP_AUDIENCE` (default `client`; alt `human`).
- New **IAM features** subsection:
  - `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED` (default `false`; off → 404 `FEATURE_DISABLED`).
  - `CYODA_IAM_TRUSTED_KEY_MAX_PER_TENANT` (default `10`).
  - `CYODA_IAM_TRUSTED_KEY_MAX_VALIDITY_DAYS` (default `365`).
  - `CYODA_IAM_TRUSTED_KEY_MAX_JWK_PROPERTIES` (default `20`).
- New **JWT signing keypair rotation** subsection (3 paragraphs) — bootstrap vs runtime rotation, v0.8.0 persistence limitation.
- New EXAMPLES block — trusted-key flag enabled.

**`errors/`** — 5 new topic files (mirror `TRUSTED_KEY_NOT_FOUND.md` template):
- `FEATURE_DISABLED.md`
- `UNSUPPORTED_ALGORITHM.md`
- `KEY_OWNED_BY_DIFFERENT_TENANT.md`
- `KEYPAIR_NOT_FOUND.md`
- `TRUSTED_KEY_CAP_REACHED.md`

**`errors.md`** — append the 5 new codes to the catalogue table (alphabetical).

**`openapi.md:96`** — clarify which `/oauth/*` operations are live in v0.8.0 (the 10 oauth/keys ops) and which remain 501 (OIDC providers — #194-D, v0.9.0+).

**`quickstart.md` / `helm.md` / `run.md`** — verified during implementation; touch only if existing JWT-bootstrap examples need the audience default mentioned.

### 10.4 README.md

Add the four new env vars to the config-reference table.

### 10.5 `DefaultConfig()`

Five new fields with documented defaults.

### 10.6 v0.8.0 release notes

Three known limitations + operational notes:

- Runtime-issued signing keypairs lost on restart (bootstrap PEM-derived KID survives) — points at follow-up §3.5.
- `trustedKeyRegistrationEnabled` default `false`; existing customers using `/oauth/keys/trusted/*` through the legacy mux must opt in.
- KV trusted-key entries written by versions < v0.8.0 are skipped at startup with INFO logs per `kid`; operators must re-register affected keys. (cyoda-go has no known production users on this surface; documented for completeness.)

---

## 11. Acceptance

- [ ] All 10 operations return OpenAPI-conformant DTOs through the chi router.
- [ ] `mux.Handle("/oauth/keys/", ...)` removed from `app/app.go`.
- [ ] adminMux entries for `/oauth/keys/keypair` and `/oauth/keys/trusted` removed.
- [ ] `internal/auth/keys.go` and `internal/auth/trusted.go` HTTP handlers removed; reusable validators retained in new `internal/auth/keyvalidation.go`.
- [ ] `auth.RequireAdmin` exported; in-package call sites updated.
- [ ] `Handler` struct gains `keyStore`, `trustedKeyStore`, `cfg` fields; `account.New` signature + `app.go:443` call updated.
- [ ] `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED` implemented (default `false`); 404 `FEATURE_DISABLED` on all 5 trusted-key endpoints when off.
- [ ] `CYODA_IAM_TRUSTED_KEY_MAX_PER_TENANT` implemented (default `10`); 400 `TRUSTED_KEY_CAP_REACHED` at cap; resolves `TODO(#163)`.
- [ ] `CYODA_IAM_TRUSTED_KEY_MAX_VALIDITY_DAYS` implemented (default `365`); user `validTo` clamped at insert.
- [ ] `CYODA_IAM_TRUSTED_KEY_MAX_JWK_PROPERTIES` implemented (default `20`).
- [ ] `CYODA_JWT_BOOTSTRAP_AUDIENCE` implemented (default `client`); pre-merge grep verifies no existing path signs human-audience tokens.
- [ ] JWK kid≡keyId check implemented; max-fields check implemented.
- [ ] Tenant-scoped trusted-key store; cross-tenant 404 / register-collision 409.
- [ ] `GetForVerification(kid)` implemented; token-verification path uses it; tenant invariant enforced at verification time.
- [ ] `KVTrustedKeyStore` cache value carries `TenantID`; tenant-scoped methods verify cached TenantID matches caller; serialization round-trips `tenantID` + `jwk`.
- [ ] Grace-period invalidation via lazy `ValidTo` filter; JWKS surfaces grace-period keys; `Save`/`Register` carry `IssueKeyPairOptions`/`RegisterTrustedKeyOptions` for atomic sibling-flip.
- [ ] `publicKey` returned as base64-DER (no PEM armor).
- [ ] Body-size assertion: shared `boundedJSONDecode` helper applied to all 4 POST adapters; integration sub-test relocated to E2E; old `integration_test.go` sub-test for trusted-key deleted.
- [ ] OpenAPI spec: 501s removed from the 10 ops; every 4xx/5xx switched from `ErrorResponseDto` to `ProblemDetail` + `application/problem+json`; `SUPER_USER` → `ROLE_ADMIN` in prose.
- [ ] Audit table dispositions updated.
- [ ] Cyoda help: 5 new error topics, 4 new env vars + bootstrap audience, errors.md + openapi.md index updates.
- [ ] `DefaultConfig()` updated; README config table updated.
- [ ] Full test suite (`go test ./... -v`) + `make test-all` + `go test -race ./...` green.
- [ ] Follow-up issue filed: implement ECDSA + Ed25519 algorithm generators.

---

## 12. Out of scope (tracked elsewhere)

- Signing-key persistence (follow-up §3.5 — secrets-management interface design).
- ECDSA / Ed25519 keypair generators (new follow-up filed at PR-merge time).
- M2M client-store persistence (follow-up §3.6 — picked up by #194-B's spec, not this one).
- `/clients` OpenAPI conformance (#194-B).
- `accountSubscriptionsGet` (#194-C).
- OIDC providers subsystem (#194-D, v0.9.0+).
- Periodic prune of past-`ValidTo` trusted-key entries (lazy filter sufficient at expected volumes).
