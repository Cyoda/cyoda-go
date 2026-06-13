# #281 — OpenAPI conformance for `/oauth/keys/*` (keypair + trusted)

**Status:** approved
**Date:** 2026-06-04
**Issue:** [#281](https://github.com/Cyoda-platform/cyoda-go/issues/281)
**Milestone:** v0.8.0
**Sub-issue of:** #194 (decomposition spec: `docs/superpowers/specs/2026-06-04-194-decomposition-design.md` §3.1)
**Revision:** rev4 (third fresh-context architect review folded in; rev1→rev2→rev3→rev4 changelog in commit log)

---

## 1. Goal

Make the 10 JWT-keypair + trusted-key admin operations respond with the OpenAPI-declared DTOs through the generated chi router, remove the legacy `/oauth/keys/` prefix mux entry that masks the OpenAPI surface today, and reconcile cyoda-go's behaviour with the Cyoda Cloud reference implementation wherever the cloud's choices are reachable without bringing in dependencies (KMS, scheduler, multi-issuer validators, multi-algorithm signing) that belong to other follow-ups.

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

A deep scan of the Kotlin reference implementation at `/Users/paul/dev/cyoda/backend/src/main/kotlin/net/cyoda/saas` (controllers under `controller/`, services `JwtKeyPairInteractor.kt`, `StoredJWKService.kt`, `TrustedKeyRegistrationService.kt`, `TechnicalUserService.kt`, `IAMProperties.kt`) informed the table below. Where cyoda-go can match cloud behaviour without out-of-scope dependencies, it does. Documented divergences are explicit in §3.2.

| Capability | Cloud behaviour | cyoda-go decision |
|---|---|---|
| `algorithm` enum (10 values) | All supported (RS*, PS*, ES*, EdDSA) | **RS256 only** for sign/verify (current `jwt.go` capability). All other enum values rejected with 400 `UNSUPPORTED_ALGORITHM` at adapter boundary. Multi-algorithm dispatch deferred to a v0.8.1 follow-up issue covering RS384/RS512/PS*/ES*/EdDSA. Documented §3.2 #10. |
| `audience` (human\|client) | Partitioned via per-audience providerId | Stored on `KeyPair`; `GetActive(audience)` partitions; bootstrap key gets configurable default audience |
| `validFrom`/`validTo` | Explicit timestamps; lazy `isValidKey(now)` filter | Same — `ValidTo *time.Time`; lazy filter at JWKS and trusted-key validator reads |
| `validTo` default for keypairs | `validFrom + keyPairDefaultValidity` (365d) | Same — `CYODA_IAM_KEYPAIR_DEFAULT_VALIDITY_DAYS` (default 365). Applies to runtime-issued keypairs AND the bootstrap key. Startup logs WARN if the active bootstrap key expires within 30 days. |
| `validTo` default for trusted keys | `validFrom + trustedKeyMaxValidity` (365d); user-supplied accepted as-is (no clamp) | Same — `CYODA_IAM_TRUSTED_KEY_MAX_VALIDITY_DAYS` (default 365) used as **default only** (no clamp); user-supplied honoured |
| `invalidateCurrent` / `invalidatePrevious` + `gracePeriodSec` | Lazy validity (`validTo = now + grace`), no timers | Same — atomic mutex-scoped flip via single `Save`/`Register` call carrying `RotateOptions{Invalidate, GracePeriodSec}` |
| Standalone `POST .../invalidate` body | `InvalidateKeyRequestDto { gracePeriodSec: int? = 0 }` | Same |
| `reactivate` semantics | Clears `validTo` to nil (acknowledged TODO bug; zombie keys) | **Diverges** — cyoda-go requires fresh `validTo` body; rejects with 400 if absent or past. Documented in §3.2 #5 as security fix. |
| `jwk` on `TrustedKeyResponseDto` | Decoded then re-serialised via `Jwks.builder()` | Stored as `map[string]any` and emitted verbatim. OpenAPI schema bug (`additionalProperties: { type: object }`) fixed in §10.1 so generated type becomes `map[string]any` (was `map[string]map[string]any`). |
| `legalEntityId` + tenant scoping | Per-tenant partitioning via `owner = legalEntityId` | Same — `TenantID` on `TrustedKey`; CRUD scoped by `uc.Tenant.ID`; mapped to wire as `legalEntityId = string(tk.TenantID)` |
| Cross-tenant keyId collision on register | `409 Conflict` | Same — `KEY_OWNED_BY_DIFFERENT_TENANT` |
| Cross-tenant lifecycle access | **403 FORBIDDEN** (leaks existence) | **404** (no leakage) — documented divergence (cyoda-go more secure; see §3.2) |
| Same-tenant re-register with same keyId | Delete-and-replace atomically (`TrustedKeyRegistrationService.kt:91-101`) | **Diverges** — cyoda-go silently upserts (current `KVTrustedKeyStore.Register` behaviour preserved). Observable behaviour identical; transactional surgery deferred. Documented §3.2. |
| JWK content checks | `kty` required, `kid ≡ keyId`, max 20 fields; supports `RSA`/`EC`/`OKP` kty | Same `kty`/`kid`/max-fields checks. **Only `kty=RSA` supported**; `EC`/`OKP` rejected with 400 `UNSUPPORTED_KEY_TYPE`. Follow-up issue tracks EC/OKP (same follow-up as multi-algorithm). |
| Trusted-key cap | Per-tenant, default 10, returns **400**; counts only currently-valid keys | Same — `CYODA_IAM_TRUSTED_KEY_MAX_PER_TENANT` (default 10); 400 `TRUSTED_KEY_CAP_REACHED`; counts only `Active && (ValidTo == nil || now.Before(*ValidTo))` (matches cloud). Resolves the existing `TODO(#163)`. |
| `keyPairInvalidateGracePeriodSec` default | 3600s (cloud's safer default for non-explicit grace) | **Diverges** — cyoda-go defaults absent/nil/zero to **immediate** (0s). Operators wanting grace must specify explicitly. Documented §3.2. |
| `trustedKeyRegistrationEnabled` flag | Default **false**; gates all 5 trusted-key ops with **404** | Same — `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED` (default `false`); 404 `FEATURE_DISABLED` |
| Token-exchange tenant invariant | `techUser.legalEntityId == subjectToken.caas_org_id` only; no check on `trustedKey.owner` (TechnicalUserService.kt:339-341, 352-354) | Same — `client.TenantID == subOrgID` only (existing `token.go:203` check); no new check on `trustedKey.TenantID`. Forging requires private-key material which stays with the registering tenant. |
| Selection rule when multiple keys active for same audience | Latest `validFrom` (`StoredJWKService.kt:378`) | Same — `GetActive(audience)` returns the entry with the maximum `ValidFrom`. |
| Private signing-key persistence | Encrypted PKCS#8 blob in DB | Out of scope — held in-memory; runtime-issued keys lost on restart; bootstrap (PEM-derived deterministic KID) survives. Per follow-up §3.5. |
| Role gate | ADMIN ∨ SUPER_USER | **ROLE_ADMIN only**, per issue body. OpenAPI prose updated SUPER_USER → ROLE_ADMIN. |
| Public JWKS endpoint | None — verification uses direct `findKeyEntryByKeyId` | cyoda-go retains `/.well-known/jwks.json`; sourced from `ListForVerification()` so grace-period keys remain discoverable until `ValidTo` |

---

## 3. Bucketing

### 3.1 In scope — implemented in this PR

1. **`algorithm` enum — RS256 only signs/verifies; others rejected at adapter** — All algorithm enum values other than `RS256` (i.e. `RS384, RS512, PS256, PS384, PS512, ES256, ES384, ES512, EdDSA`) are rejected with 400 `UNSUPPORTED_ALGORITHM` at the adapter boundary. The store's `KeyPair.Algorithm` field exists and stores `"RS256"`. `internal/auth/jwt.go` is **not modified** in this PR (its existing RS256-only `Sign`/`Verify`/`EnsureAlgRS256` stays). A v0.8.1 follow-up issue (filed at PR-merge time) covers adding multi-algorithm sign/verify dispatch — that issue is the single channel for RS384/RS512/PS*/ES*/EdDSA + `kty=EC`/`kty=OKP` registration.
2. **`publicKey` base64-DER on `JwtKeyPairResponseDto`** — `base64.StdEncoding.EncodeToString(x509.MarshalPKIXPublicKey(pub))`. **No PEM armor.** Matches OpenAPI's "Base64-encoded public key in X.509 SubjectPublicKeyInfo format" and cloud's `Base64.getEncoder().encodeToString(publicKey)` at `JwtKeyPairInteractor.kt:85`.
3. **`validFrom`/`validTo` timestamps on `KeyPair` and `TrustedKey`** — rename `KeyPair.CreatedAt → ValidFrom`; add `KeyPair.ValidTo *time.Time` and confirm/keep `TrustedKey.ValidTo *time.Time`. Trusted-key default at register-time: `validFrom + TrustedKeyMaxValidityDays`. Keypair default at issue-time and bootstrap: `validFrom + KeypairDefaultValidityDays`. Set to `now + gracePeriodSec` at invalidate time. Lazy filter at all verification reads: `Active && (ValidTo == nil || now.Before(*ValidTo))`. For both stores, **`GetActive(audience)` selects the entry with the maximum `ValidFrom`** when multiple are active (matches cloud).
4. **`validFrom`/`validTo` overrides on issue/register** — honoured when supplied. Defaults: `validFrom = now()`. User-supplied `validTo` accepted as-is (no clamp). Reject `validTo < validFrom` with 400 `BAD_REQUEST` (see §3.2 — stricter than cloud).
5. **`jwk` round-trip on trusted-key response** — store as `map[string]any` on `TrustedKey`; emit verbatim. Existing JWK validation (`parseRSAPublicKeyFromJWK`) gates incoming registrations. Three content checks at validate-boundary (match cloud `TrustedKeyRegistrationService.kt:119-130, 262-269`):
   - JWK must contain `kty`; only `kty="RSA"` supported (others → 400 `UNSUPPORTED_KEY_TYPE`).
   - JWK `kid` field, if present, must equal request `keyId` — else 400 `BAD_REQUEST` (security: prevents `keyId=trusted-app` with `jwk.kid=attacker-kid` injection).
   - JWK max `MaxJWKProperties` properties (default 20) — else 400 `BAD_REQUEST`.
6. **`legalEntityId` + per-tenant trusted-key scoping** — add `TenantID spi.TenantID` to `TrustedKey`. CRUD methods scoped by caller's `uc.Tenant.ID`. Cross-tenant `Get`/`Delete`/`Invalidate`/`Reactivate` returns **404** `TRUSTED_KEY_NOT_FOUND` (does not leak existence; cyoda-go diverges from cloud's 403 for this reason — see §3.2). `Register` with keyId-collision against another tenant returns **409** `KEY_OWNED_BY_DIFFERENT_TENANT`. On the wire: `legalEntityId = string(tk.TenantID)`.
7. **`invalidateCurrent` (issue) / `invalidatePrevious` (register) booleans + `invalidateGracePeriodSec`** — folded into the `Save`/`Register` call signatures via a single shared `RotateOptions{Invalidate bool, GracePeriodSec int64}`. Single mutex acquisition: under one critical section the store inserts the new key and flips siblings' `Active=false`, `ValidTo=now+grace`. (Same partition definition: keypairs = same `audience`; trusted keys = same `tenantID`.) Grace=0 ⇒ immediate. **No-op cases**: `Invalidate=true` with no existing active siblings ⇒ successful insert, nothing to flip; `InvalidateCurrent=false` + non-zero `invalidateGracePeriodSec` ⇒ the grace value is ignored (no flip happens; documented at adapter). For `KVTrustedKeyStore`, the mutex is held across N + 1 `kv.Put` calls during rotation; this trades concurrent-read latency during a (rare) rotation for transactional atomicity. **Rollback ordering**: in-memory map mutation runs **last**, so a partial KV failure mid-rotation leaves the in-memory cache untouched and the caller observes an `Internal` 5xx with no half-applied state. The adapter populates `RotateOptions.Invalidate` from `req.InvalidateCurrent` (keypair) or `req.InvalidatePrevious` (trusted) and clamps grace per `req.InvalidateGracePeriodSec` (nil/zero ⇒ 0; negative ⇒ 400 `BAD_REQUEST`, see §3.2).
8. **Standalone `POST .../invalidate` request body** — `InvalidateKeyRequestDto { gracePeriodSec int64 }`; absent body, nil, or zero ⇒ immediate. Both keypair and trusted-key invalidate endpoints accept the body. Negative values rejected with 400 `BAD_REQUEST` (§3.2).
9. **`audience` (human\|client) on keypairs** — add `Audience string` to `KeyPair`. New `KeyStore.GetActive(audience string)` partitions. `issueJwtKeyPair` partitions by request audience. `getCurrentJwtKeyPair?audience=X` returns **404** `KEYPAIR_NOT_FOUND` if no active key for X. Bootstrap key gets audience `CYODA_JWT_BOOTSTRAP_AUDIENCE` (default `client`, alternative `human`); the existing M2M token-signing path (`POST /oauth/token`) becomes `GetActive("client")`. Pre-merge verification step: grep all `KeyStore.GetActive()` call sites and confirm none implicitly sign human-audience tokens today; if any exist, this default needs revisiting. Adapter calls `params.Audience.Valid()` and rejects invalid enums with 400 `BAD_REQUEST`. The bootstrap-audience env var is validated at boot — invalid values (e.g. `robot`) fail startup with a clear error.
10. **Reactivate — requires fresh validity window (diverges from cloud)** — `Reactivate(kid, validFrom, validTo)` (keypair) and `Reactivate(tenantID, kid, validFrom, validTo)` (trusted) — keypair signature drops audience (kid is unique). Body of the `POST .../reactivate` endpoints accepts a new schema (added to OpenAPI in this PR):
   ```yaml
   ReactivateKeyRequestDto:
     type: object
     properties:
       validFrom:
         type: string
         format: date-time
         description: Optional; defaults to now.
       validTo:
         type: string
         format: date-time
         description: Required. Must be > now and > validFrom.
     required: [validTo]
   ```
   Single shared schema used by both reactivate endpoints. Operation `requestBody` set to `required: true` for both. New `400` response declaration added to both. Ownership check happens inside the store. Validation: `validTo` required, parses RFC3339, must be `> now` and `> validFrom` — else 400 `BAD_REQUEST`. **Reactivate on already-active key**: idempotent; sets the new validity window and remains `Active=true`. Prevents zombie-key resurrection that cloud's TODO at `TrustedKeyRegistrationService.kt:202-204` acknowledges. Documented §3.2.
11. **`keyId` validation on path-parameter lifecycle ops** — apply `trustedKIDPattern` (`^[A-Za-z0-9._-]{1,128}$`) ONLY to user-controlled path parameters on `deleteTrustedKey`, `invalidateTrustedKey`, `reactivateTrustedKey`. **Not** applied to keypair path-parameter lifecycle ops (`deleteJwtKeyPair`, `invalidateJwtKeyPair`, `reactivateJwtKeyPair`): match cloud, drop the validator, let the store lookup return 404. Server-generated KIDs (16-byte hex from `internal/auth/keys.go:99`) are validated by construction. Per-spec: keypair lifecycle ops declare 401/403/404/500; no 400 on the wire **except for reactivate**, which gets a 400 declaration because of the new request-body validation in #10.
12. **OpenAPI JWK schema fix** — `api/openapi.yaml` lines 8413-8416 and 8457-8460 declare `jwk` with `additionalProperties: { type: object }`, which oapi-codegen generates as `map[string]map[string]interface{}` — a nested-map type that's wrong for flat JWKs. Replace with `additionalProperties: true` so the generated type is `map[string]any`. The fix is part of this PR; regen `api/generated.go`.

### 3.2 Documented divergences from cloud

1. **JWKS endpoint** — cyoda-go retains `/.well-known/jwks.json`; cloud has no public JWKS surface. The cyoda-go endpoint sources from `KeyStore.ListForVerification()` so grace-period keys remain published until `ValidTo` passes.
2. **Role gate** — cyoda-go enforces **ROLE_ADMIN** only; cloud accepts ADMIN ∨ SUPER_USER. Per issue body, the smaller-blast-radius change is to align OpenAPI prose to code (`SUPER_USER` → `ROLE_ADMIN`).
3. **Cross-tenant lifecycle leakage** — cyoda-go returns **404 TRUSTED_KEY_NOT_FOUND**; cloud returns 403 with a body that confirms the key exists in another tenant. cyoda-go's choice prevents tenant-existence enumeration; intentional security improvement.
4. **Trusted-key `audience` round-trip** — cyoda-go honors the request `audience` and round-trips it; cloud always uses `human` (per its single-providerId model). Documented; spec offers more semantic surface than cloud here.
5. **Reactivate semantics** — cyoda-go requires a fresh `validTo` (and optional `validFrom`) in the request body and validates `validTo > now`. Cloud clears `ValidTo` to nil and produces an immortal key; cloud's own TODO at `TrustedKeyRegistrationService.kt:202-204` flags this as a bug. cyoda-go fixes it.
6. **Strict-input validation** — cyoda-go rejects `validTo < validFrom`, `gracePeriodSec < 0`, and invalid `audience` enum values with 400 `BAD_REQUEST`. Cloud accepts each silently and produces always-invalid keys or immediate-invalidation behaviour. Per `.claude/rules/security.md` ("validate user-supplied input at system boundaries"), the project rule trumps parity for clearly-broken inputs.
7. **`gracePeriodSec` default** — cyoda-go defaults absent/nil/zero to **immediate** (0s); cloud defaults to 3600s. Operators wanting a grace period must specify it explicitly. Surfaces in the OpenAPI prose for the two invalidate endpoints.
8. **Same-tenant idempotent re-register** — cyoda-go preserves the existing silent upsert semantics in `KVTrustedKeyStore.Register`. Cloud explicitly does delete-and-replace atomically. Observable wire behaviour is identical (200 + new entry); transactional surgery is deferred. Documented for completeness.
9. **Signing-key persistence** — out of scope; runtime-issued keypairs lost on restart. Bootstrap key (PEM-derived deterministic KID per ARCHITECTURE.md §7.2) survives. v0.8.0 release notes call this out and link follow-up §3.5.
10. **Algorithm support** — cyoda-go signs and verifies RS256 only in v0.8.0; cloud supports the full enum (RS*, PS*, ES*, EdDSA) plus `kty=EC`/`kty=OKP` for trusted keys. Adapter rejects non-RS256 with 400 `UNSUPPORTED_ALGORITHM` and non-RSA `kty` with 400 `UNSUPPORTED_KEY_TYPE`. v0.8.1 follow-up issue tracks adding the rest.

---

## 4. Components

### 4.1 Type extensions (`internal/auth/store.go`)

```go
type KeyPair struct {
    KID        string
    Audience   string          // NEW: "human" | "client"
    Algorithm  string          // NEW: stored value (always "RS256" in v0.8.0)
    PublicKey  *rsa.PublicKey
    PrivateKey *rsa.PrivateKey
    Active     bool
    ValidFrom  time.Time       // RENAMED from CreatedAt
    ValidTo    *time.Time      // NEW: nil = no expiry; set on invalidate
}

type TrustedKey struct {
    KID       string
    TenantID  spi.TenantID     // NEW
    JWK       map[string]any   // NEW: original JWK for round-trip
    PublicKey *rsa.PublicKey
    Audience  string
    Issuers   []string
    Active    bool
    ValidFrom time.Time
    ValidTo   *time.Time
}

// RotateOptions carries the invalidate-siblings option for both issueKeyPair
// (sibling-flip across same audience) and registerTrustedKey (sibling-flip
// across same tenantID). DTO field-names differ (`invalidateCurrent` vs
// `invalidatePrevious`); the store layer uses a single struct.
type RotateOptions struct {
    Invalidate     bool
    GracePeriodSec int64
}
```

### 4.2 Store interface changes

```go
type KeyStore interface {
    Save(kp *KeyPair, opts RotateOptions) error             // NEW signature: opts folds in atomic sibling-flip
    Get(kid string) (*KeyPair, error)
    GetActive(audience string) (*KeyPair, error)             // CHANGED: audience param; selects max-ValidFrom when multiple active
    List() []*KeyPair                                        // all entries (admin listing)
    ListForVerification() []*KeyPair                         // NEW: lazy ValidTo filter
    Delete(kid string) error
    Invalidate(kid string, gracePeriodSec int64) error       // CHANGED: grace param
    Reactivate(kid string, validFrom time.Time, validTo time.Time) error  // CHANGED: requires fresh window; signature drops audience (kid is unique)
}

type TrustedKeyStore interface {
    Register(tk *TrustedKey, opts RotateOptions) error                   // 409 on cross-tenant keyId collision; 400 on cap reached
    Get(tenantID spi.TenantID, kid string) (*TrustedKey, error)          // CHANGED: tenant-scoped
    List(tenantID spi.TenantID) []*TrustedKey                            // CHANGED: tenant-scoped
    ListForVerification() []*TrustedKey                                  // NEW: all tenants, ValidTo-filtered; used by JWT-bearer-assertion grant path via in-package helper
    Delete(tenantID spi.TenantID, kid string) error
    Invalidate(tenantID spi.TenantID, kid string, gracePeriodSec int64) error
    Reactivate(tenantID spi.TenantID, kid string, validFrom time.Time, validTo time.Time) error
}
```

`Save`/`Register` atomicity: a single mutex acquisition inserts the new entry and (if `opts.Invalidate`) flips siblings in the same partition to `Active=false`, `ValidTo=now+grace`. For `KVTrustedKeyStore`, the mutex is held across N + 1 `kv.Put` calls during rotation; in-memory map mutation runs LAST so partial KV failure leaves the cache untouched.

**In-package verification helper (unexported)** — `token.go`'s JWT-bearer/token-exchange flow needs to resolve a trusted key by kid without the caller's tenant. Instead of an interface method, an unexported helper `getTrustedKeyByKID(store TrustedKeyStore, kid string) (*TrustedKey, error)` lives in `internal/auth/` and iterates `ListForVerification()`. Same package as both the store and `token.go` — no new interface surface; no contract that external consumers might rely on.

**Bootstrap call-site update** — `internal/auth/service.go` currently calls `keyStore.Save(kp)` (single-arg). With the new signature, all `Save` call sites pass `RotateOptions{}` (zero-value) for the no-flip case. Same for `KVTrustedKeyStore.Register` if any in-tree caller exists.

**New accessor on `AuthService`** — `service.go` already exposes `KeyStore()`; add `TrustedKeyStore()` accessor for the new adapters' construction-time dependency.

### 4.3 Handler struct changes (`internal/domain/account/handler.go`)

The `Handler` struct gains three fields:
```go
type Handler struct {
    authSvc          contract.AuthenticationService
    authzSvc         contract.AuthorizationService
    keyStore         auth.KeyStore                // NEW: read-only via interface
    trustedKeyStore  auth.TrustedKeyStore         // NEW: read-only via interface
    iam              auth.IAMConfig               // NEW: value-struct feature flags, copied at construction
}
```

`auth.IAMConfig` is a new value-struct **peer to (not nested under) `auth.AuthConfig`** in `internal/auth/service.go`:
```go
type IAMConfig struct {
    TrustedKeyRegistrationEnabled bool
    TrustedKeyMaxPerTenant        int
    TrustedKeyMaxValidityDays     int
    TrustedKeyMaxJWKProperties    int
    KeypairDefaultValidityDays    int
    BootstrapAudience             string
}
```

`AuthConfig` gains a single field `IAM IAMConfig` (composition over flat fields) so call sites can pass the bundle as `cfg.IAM`. Per `.claude/rules/ownership-mutability.md` rule 3: stores shared via interface; `IAMConfig` is value-copied (read-only data, no shared mutable state).

`account.New(...)` signature updated to accept these three new dependencies; `app.go:443` call site passes them through.

### 4.4 Adapter files (handler.go split)

The existing `internal/domain/account/handler.go` keeps the account methods. The 10 new methods live in:

- `internal/domain/account/keys_adapter.go` — 5 keypair handler methods + DTO helpers (`toJwtKeyPairResponse`, `parseIssueRequest`).
- `internal/domain/account/trusted_adapter.go` — 5 trusted-key handler methods + DTO helpers (`toTrustedKeyResponse`, `tenantFromContext`).
- `internal/domain/account/io.go` — shared `boundedJSONDecode(r *http.Request, max int64, dst any) error` helper used by all four POST adapters. Returns `400 BAD_REQUEST` on body-too-large (oversized bodies hit the `MaxBytesReader` cap which surfaces to `json.Decoder` as a decode error; the helper translates to 400). The OpenAPI spec declares `400` (not `413`) for body-too-large.

Both adapter files take `*Handler` as the receiver.

### 4.5 Removed (dead code) + new helper file

- `internal/auth/keys.go` — `KeysHandler`, `keysInfoResponse`, all `ServeHTTP` path-parsing. Domain logic moves inline to the new adapter.
- `internal/auth/trusted.go` — `TrustedKeysHandler`, `trustedKeyInfoResponse`, `ServeHTTP`, `handleList/Register/Delete/Invalidate/Reactivate`, `validateLifecycleKID`, `extractKeyID`. The input-validation helpers (`trustedKIDPattern`, `parseRSAPublicKeyFromJWK`, `validateRSAPublicExponent`) move to a small `internal/auth/keyvalidation.go` so the new adapter can reuse them. `decodeBase64URL` lives in `internal/auth/jwt.go` and is unaffected.
- `internal/auth/service.go` adminMux entries for `/oauth/keys/...` (lines 86–89). The `/account/m2m` entries stay (#194-B territory).
- `app/app.go:482` `mux.Handle("/oauth/keys/", ...)` — removed.

**`internal/auth/jwt.go` — NOT modified** in this PR. Existing `Sign`/`Verify`/`EnsureAlgRS256` (RS256-only) stay. Multi-algorithm dispatch is the v0.8.1 follow-up.

### 4.6 Exporting `requireAdmin`

`internal/auth/admin_guard.go:22` `requireAdmin` is currently package-private. Exported as `auth.RequireAdmin(w, r) bool` so the new adapters in `internal/domain/account/` can call it. Existing in-package callers (`keys.go`/`trusted.go` deleted; `m2m.go` updated to the new name).

### 4.7 Wiring changes (`app/app.go`)

- Bootstrap-key save now sets `Algorithm = "RS256"`, `Audience = config.IAM.BootstrapAudience`, `ValidFrom = now()`, `ValidTo = &(now + KeypairDefaultValidityDays·day)`. After save, startup logs a WARN if `ValidTo - now < 30 days` (operator must rotate soon).
- New `AuthConfig.IAM` (value struct, peer to existing fields). Fields parsed from env vars and validated at boot:
  - `BootstrapAudience string` (env `CYODA_JWT_BOOTSTRAP_AUDIENCE`, default `client`; invalid enum ⇒ startup error)
  - `TrustedKeyRegistrationEnabled bool` (env `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED`, default `false`)
  - `TrustedKeyMaxPerTenant int` (env `CYODA_IAM_TRUSTED_KEY_MAX_PER_TENANT`, default `10`; `0` ⇒ unbounded; negative ⇒ startup error)
  - `TrustedKeyMaxValidityDays int` (env `CYODA_IAM_TRUSTED_KEY_MAX_VALIDITY_DAYS`, default `365`; ≤ 0 ⇒ startup error; default-only at register-time, no clamp)
  - `TrustedKeyMaxJWKProperties int` (env `CYODA_IAM_TRUSTED_KEY_MAX_JWK_PROPERTIES`, default `20`; ≤ 0 ⇒ startup error)
  - `KeypairDefaultValidityDays int` (env `CYODA_IAM_KEYPAIR_DEFAULT_VALIDITY_DAYS`, default `365`; ≤ 0 ⇒ startup error; applies to bootstrap + runtime-issued keypairs)
- M2M signing call sites switch to `GetActive("client")`.
- Token-verification path (`internal/auth/token.go:137`) switches from `Get(kid)` to in-package helper `getTrustedKeyByKID(trustedKeyStore, kid)`.
- JWKS endpoint source switches to `KeyStore.ListForVerification()`.

---

## 5. Error handling

### 5.1 New error codes (`internal/common/error_codes.go`)

| Code | HTTP | Trigger |
|---|---|---|
| `FEATURE_DISABLED` | 404 | Trusted-key endpoint while `TrustedKeyRegistrationEnabled=false` |
| `UNSUPPORTED_ALGORITHM` | 400 | `algorithm ≠ RS256` (anywhere in the spec's enum) |
| `UNSUPPORTED_KEY_TYPE` | 400 | JWK `kty ≠ RSA` on register |
| `KEY_OWNED_BY_DIFFERENT_TENANT` | 409 | `registerTrustedKey` keyId collision with another tenant |
| `KEYPAIR_NOT_FOUND` | 404 | Keypair lifecycle on missing kid; `getCurrent` with no key for requested audience |
| `TRUSTED_KEY_CAP_REACHED` | 400 | Per-tenant trusted-key cap reached on register (counts only currently-valid keys) |

`TRUSTED_KEY_NOT_FOUND` already exists and is reused for tenant-scoped lookups (returned uniformly whether the kid doesn't exist or belongs to another tenant — no existence leakage). The existing `kv_trusted_store.go:157` 409-on-registry-full path is **replaced** (not augmented) by the new 400 + `TRUSTED_KEY_CAP_REACHED` code.

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
| `algorithm` | enum + `== RS256` | non-RS256 → 400 `UNSUPPORTED_ALGORITHM`; non-enum → 400 `BAD_REQUEST` |
| `gracePeriodSec` (anywhere) | `>= 0` | 400 `BAD_REQUEST` (see §3.2 stricter-than-cloud) |
| `validTo` on issue/register (request) | `>= validFrom` | 400 `BAD_REQUEST` (see §3.2 stricter-than-cloud) |
| `validTo` on reactivate (request) | required; `> now`; `> validFrom` | 400 `BAD_REQUEST` (see §3.2 reactivate fix) |
| trusted-key `keyId` (path param) | `trustedKIDPattern` regex `^[A-Za-z0-9._-]{1,128}$` | 400 `BAD_REQUEST` |
| trusted-key `keyId` (request body) | same regex | 400 `BAD_REQUEST` |
| trusted-key JWK | `kty` required AND `kty == "RSA"`; `kid` (if present) == `keyId`; ≤ `MaxJWKProperties` fields | non-RSA → 400 `UNSUPPORTED_KEY_TYPE`; others → 400 `BAD_REQUEST` |
| keypair `keyId` (path param) | not validated; store lookup returns 404 (matches cloud) | — |
| request body size (all 4 POSTs) | `http.MaxBytesReader(w, r.Body, 1<<20)` via shared `boundedJSONDecode` helper | 400 `BAD_REQUEST` (decoder fails on oversized body; OpenAPI declares 400 not 413) |
| `BootstrapAudience` env var | parsed at boot; in {`human`, `client`} | startup error |
| `TrustedKeyMaxPerTenant` env var | `>= 0`; `0` means **unbounded** | startup error if negative |
| `TrustedKeyMaxValidityDays` env var | `> 0` | startup error if `≤ 0` |
| `TrustedKeyMaxJWKProperties` env var | `> 0` | startup error if `≤ 0` |
| `KeypairDefaultValidityDays` env var | `> 0` | startup error if `≤ 0` |

### 5.4 Security checks (Gate 3)

- **Tenant isolation** — `TenantID` derived from `uc.Tenant.ID` server-side, never from request body. Cross-tenant access returns 404 (lookup) or 409 (collision on register), never 200+leak or 403+confirmation.
- **In-memory cache isolation** (`KVTrustedKeyStore.keys`) — cache value carries `TenantID`. All tenant-scoped methods verify `cached.TenantID == requested.TenantID` after both cache hit AND post-`loadOne` re-cache; mismatch returns 404 (treats it as if cache contained nothing for this tenant). The unexported `getTrustedKeyByKID` verification helper is the documented exception; it returns the raw entry (TenantID embedded). A unit test asserts the race: tenant-A `Get` triggers cache-load of tenant-B's key, tenant-B `Get` then succeeds for itself (no cross-pollination), tenant-A `Get` still returns 404.
- **No secrets in logs** — bootstrap PEM stays unprinted; rotated kids are logged only when an operation references them. Private RSA material never serialised in any response or log line. slog field allowlist: `kid`, `tenant`, `audience`, `algorithm`.
- **No issue IDs in shipped artefacts** — per the standing rule, no `#281` in error messages, response bodies, OpenAPI prose, code comments, or help topics.
- **No stack traces in 5xx responses** — `common.Internal` ticket pattern only.

---

## 6. Persistence

- `InMemoryKeyStore` and `InMemoryTrustedKeyStore` updated for new field/signature shapes.
- `KVTrustedKeyStore` (already wired in production via `app/app.go:207–239`):
  - KV-key encoding changes from `trustedkey:<kid>` to `trustedkey:<tenantID>:<kid>` to make tenant isolation an invariant of the storage layer itself.
  - `trustedKeyRecord` serialization schema gains two fields: `tenantID string` and `jwk map[string]any`. Backward-compatible on read: missing fields handled gracefully.
  - In-memory cache map remains keyed by raw `kid` for O(1) `getTrustedKeyByKID` access; the cached value's `TenantID` field is what tenant-scoped methods consult.
  - **Old-prefix entries (pre-v0.8.0)** — `loadAll()` enumerates all entries under the namespace. Entries whose key shape doesn't match `trustedkey:<tenantID>:<kid>` are skipped silently (not loaded into cache, not deleted from KV). At end of `loadAll`, if any old-shape entries were observed, a **single one-shot startup WARN** is emitted: `"pre-v0.8.0 trusted-key entries found in KV (count=N); not loaded under new key shape — re-register via /oauth/keys/trusted; see v0.8.0 release notes"`. The count is the only data emitted; specific kids are NOT logged (cyoda-go has no production users on this surface today and per-entry log noise is not warranted).
  - **Rollback hazard** (v0.8.0 → pre-v0.8.0 binary): pre-v0.8.0 binary lists `trustedkey:*` and would observe entries with shape `trustedkey:<tenantID>:<kid>`. Pre-v0.8.0's parser treats the segment after `trustedkey:` as the kid — so it sees a "kid" literally equal to `<tenantID>:<kid>`. Not corrupt, but visible as a mangled-kid entry. Documented in release notes; operators rolling back must purge new-shape entries out-of-band.
- Signing-key persistence — out of scope (follow-up §3.5). Bootstrap key survives restart; runtime-issued keypairs do not.
- **Lazy ValidTo and stale entries** — known limitation: invalidated keys past `ValidTo` are filtered at every read but never pruned. With per-tenant cap of 10 (counting only currently-valid keys), the cap itself naturally bounds growth; stale entries past `ValidTo` are excluded from the cap count, so they accumulate freely. With a tenant rotating quarterly, ~4 stale entries/year. Documented; periodic prune is a follow-up if it becomes a problem.

---

## 7. Routing wiring

1. `app/app.go:482` `mux.Handle("/oauth/keys/", ...)` — removed.
2. `internal/auth/service.go` adminMux — drop `/oauth/keys/keypair` (×2) and `/oauth/keys/trusted` (×2) entries. `/account/m2m` entries stay until #194-B.
3. Chi router (mounted at `/` via the generated `HandlerFromMux`) now owns all `/oauth/keys/*` paths via the 10 `ServerInterface` methods on `*Handler`.
4. `auth.RequireAdmin(w, r)` called inline at the start of each adapter method (no middleware change; matches existing pattern in `admin_guard.go`).

### 7.1 Token-exchange tenant invariant

The grant at `internal/auth/token.go:65` (`urn:ietf:params:oauth:grant-type:token-exchange`) currently flows: extract `kid` → `trustedKeyStore.Get(kid)` (line 137) → verify signature → read `subOrgID = parsed.Claims["caas_org_id"]` (line 191) → reject if `client.TenantID != subOrgID` (line 203).

Post-rev4:

- The trusted-key lookup switches to `getTrustedKeyByKID(trustedKeyStore, kid)` (unexported, in-package; iterates `ListForVerification()` with lazy `ValidTo` filter).
- **No new check on `trustedKey.TenantID` is added.** This matches cloud (`TechnicalUserService.kt:339-341, 352-354`), which enforces `techUser.legalEntityId == subjectToken.caas_org_id` but does not consult the trusted key's owner field at verification time. Forging a token signed by tenant-A's key requires tenant-A's private-key material; without it, no other tenant can mount the attack. The existing line-203 check (`client.TenantID != subOrgID`) closes the only practically reachable vector.

This invariant is explicitly tested (§9.3) so a future change to either the cloud or cyoda-go verification path surfaces the assumption.

---

## 8. Body-size relocation

`internal/auth/integration_test.go:268–301` has two sub-tests routing through `svc.AdminHandler()`:

- `m2m create endpoint rejects oversized body` — left untouched (#194-B territory).
- `trusted key register endpoint rejects oversized body` — relocated: a shared helper `account.boundedJSONDecode(r, 1<<20, &dst)` wraps `http.MaxBytesReader` + `json.Decoder.Decode`; called by all four POST adapters (issue keypair, register trusted key, invalidate keypair, invalidate trusted key). E2E test asserts the 400 behaviour by POSTing >1 MB through chi. The integration-level sub-test is deleted.

---

## 9. Testing strategy (Gates 1, 2, 5)

### 9.1 Unit (TDD red-first)

- `internal/auth/store_test.go` — audience-scoped `GetActive` selects max-ValidFrom when multiple active; grace-period `Invalidate` sets `ValidTo`; `ListForVerification` lazy filter (including past-`ValidTo` filtered out); reactivate with fresh window sets values; reactivate on already-active key is idempotent (sets new window); reactivate with missing or past `validTo` rejected; cross-tenant trusted-key isolation; same-tenant upsert vs cross-tenant 409; per-tenant cap reached → `TRUSTED_KEY_CAP_REACHED` (counts only currently-valid keys); JWK `kid` ≠ `keyId` rejected; JWK `kty ≠ RSA` rejected with `UNSUPPORTED_KEY_TYPE`; JWK > `MaxJWKProperties` rejected; `Save`/`Register` with `RotateOptions{Invalidate: true}` atomically flip siblings; `Save`/`Register` with `Invalidate: true` and no siblings is a no-op flip; `Reactivate` cross-tenant returns 404 from the store; two concurrent `Save(opts)` with `Invalidate: true` for the same audience produce exactly one active key.
- `internal/auth/kv_trusted_store_test.go` — tenant-scoped key prefix; tenant-A cache load does not let tenant-B see tenant-A's key (cross-pollination guard); record schema round-trips `tenantID` and `jwk`; old-shape entries cause one-shot WARN at `loadAll` end (count only); partial KV failure during rotation leaves in-memory cache untouched (rollback ordering).
- `internal/auth/keypair_signing_test.go` (new) — RS256 sign+verify round-trip; **every other algorithm enum value (RS384/RS512/PS256/PS384/PS512/ES256/ES384/ES512/EdDSA) at adapter rejected with 400 `UNSUPPORTED_ALGORITHM`** (`jwt.go` is RS256-only).
- `internal/auth/token_test.go` — `getTrustedKeyByKID` returns trusted key by kid regardless of tenant; lazy ValidTo filter applied (past-validity returns nil); the existing `client.TenantID != subOrgID` check still enforces principal tenancy.
- `internal/auth/config_validation_test.go` — `BootstrapAudience=robot` → startup error; `TrustedKeyMaxPerTenant=-1` → startup error; `TrustedKeyMaxPerTenant=0` interpreted as unbounded; `TrustedKeyMaxValidityDays=0` → startup error; `KeypairDefaultValidityDays=0` → startup error.

### 9.2 Adapter

- `internal/domain/account/keys_adapter_test.go`, `trusted_adapter_test.go` — per-operation DTO marshalling round-trip; ROLE_ADMIN gate (401 unauth, 403 wrong role); validation error codes; response shape matched against the generated DTOs; `publicKey` is base64-DER (no PEM); `legalEntityId == string(uc.Tenant.ID)`; ProblemDetail wire shape on every 4xx/5xx (content-type `application/problem+json`); audience-query enum validation.
- `algorithm=RS384` (or any non-RS256) → 400 `UNSUPPORTED_ALGORITHM`.
- JWK with `kty=EC` → 400 `UNSUPPORTED_KEY_TYPE`.
- `TrustedKeyRegistrationEnabled=false` → 404 `FEATURE_DISABLED` on all 5 trusted-key endpoints.
- Body-size limit: POST > 1 MB to any of the 4 POST adapters → 400.
- **Per-divergence regression-lock tests** (one per §3.2 item to prevent silent drift toward cloud):
  - JWKS retained (publishes grace-period keys).
  - ROLE_ADMIN only (not ADMIN ∨ SUPER_USER).
  - Cross-tenant lifecycle 404 (not 403).
  - Trusted-key `audience` honored on round-trip.
  - Reactivate rejects absent/past `validTo`.
  - `validTo < validFrom` rejected with 400.
  - `gracePeriodSec < 0` rejected with 400.
  - `gracePeriodSec` default is 0 (not 3600).
  - Same-tenant re-register succeeds (silent upsert; no 409, no 400).
  - Non-RS256 algorithm rejected (regression-locks the v0.8.0 limit).

### 9.3 E2E (Gate 2)

- `internal/e2e/oauth_keys_test.go` — one scenario per operation through the full HTTP stack, authenticated via the existing bootstrap-M2M-client → `POST /oauth/token` → Bearer flow.
- **Grace-period round-trip** — issue keypair A, issue keypair B with `invalidateCurrent=true, invalidateGracePeriodSec=2`. Assert A's kid is in `/.well-known/jwks.json` immediately after; `sleep(3s)`; assert A's kid is no longer in JWKS. (The cross-process token round-trip is covered at the unit-test layer because E2E cannot reach the rotated key's private half.)
- **Persistence** — register a trusted key via `POST /oauth/keys/trusted`, restart the in-process server against the same store factory, assert `GET /oauth/keys/trusted` still returns it.
- **Cross-tenant isolation** — bootstrap two M2M clients in distinct tenants; register the same `keyId` from tenant A; register from tenant B → 409 `KEY_OWNED_BY_DIFFERENT_TENANT`; `GET` from tenant B → 404 (does not list tenant A's key).
- **Feature-flag** — with `TrustedKeyRegistrationEnabled=false`, all 5 trusted-key endpoints return 404 `FEATURE_DISABLED`; keypair endpoints unaffected.
- **Token-exchange via trusted key** — register trusted key with tenant A, mint a subject token signed by that key with `caas_org_id=A`, present at `POST /oauth/token` grant=`urn:ietf:params:oauth:grant-type:token-exchange` from tenant A's M2M client → token issued. Same flow with `caas_org_id=B` claim but called by tenant A's M2M client → rejected (existing line-203 check). Same flow with tenant B's M2M client calling against subject token claiming `caas_org_id=A` signed by tenant A's key → rejected. Locks in the no-new-trusted-key-tenant-check decision from §7.1.

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

cyoda-go-cassandra (`/Users/paul/go-projects/cyoda-light/cyoda-go-cassandra`) does not consume `internal/auth` interfaces (verified during review). No parity-registry update needed. No SPI change.

---

## 10. Documentation updates (Gate 4)

### 10.1 OpenAPI spec (`api/openapi.yaml`) + codegen

**Spec changes:**

- Remove `501 NotImplemented` declarations from all 10 operations.
- Replace `$ref ErrorResponseDto` (RFC 6749 OAuth-error shape) with `$ref ProblemDetail` + `application/problem+json` content-type on **every** 4xx/5xx response declaration of the 10 operations. Matches established audit pattern.
- Replace `SUPER_USER` → `ROLE_ADMIN` in the 10 operation descriptions (role-gate divergence intentional per §3.2).
- **JWK schema fix**: change `additionalProperties: { type: object }` to `additionalProperties: true` at lines 8413-8416 and 8457-8460 so the generated Go type for `jwk` becomes `map[string]any` (was `map[string]map[string]any`).
- **Add `ReactivateKeyRequestDto` schema** (new component, shared by both reactivate operations):
  ```yaml
  ReactivateKeyRequestDto:
    type: object
    properties:
      validFrom:
        type: string
        format: date-time
        description: Optional; defaults to now if absent.
      validTo:
        type: string
        format: date-time
        description: Required. Must be > now and > validFrom.
    required: [validTo]
  ```
- **Update `reactivateJwtKeyPair` + `reactivateTrustedKey`** operations: add `requestBody` with `required: true` referencing `ReactivateKeyRequestDto`; add `"400"` response declaration `$ref ProblemDetail`.
- Add brief prose to `issueJwtKeyPair`, `registerTrustedKey`, and both invalidate operations noting the cyoda-go-specific defaults (`algorithm` defaults `RS256`; `audience` no default — required; `gracePeriodSec` default `0` immediate; `validTo` default `validFrom + 365d` for keypairs and trusted keys).

**Codegen regen required**:

The previous "no regen needed" claim was wrong. The following spec changes are STRUCTURAL and force `api/generated.go` to be regenerated:
- JWK type change (`map[string]map[string]any` → `map[string]any`).
- New `ReactivateKeyRequestDto` schema + `requestBody` on both reactivate operations changes their `ServerInterface` method signatures (gain a JSON-decoded payload).

Run `go generate ./api/...` (or whatever the project's codegen command is) after spec edits; verify the regen surface in PR review.

### 10.2 Audit table (`docs/superpowers/audits/2026-04-29-openapi-conformance-audit.md`)

For each of the 10 rows (lines 122–131), change disposition from `out-of-scope-not-implemented (#194)` to `match`. Cite the merge commit at PR-merge time.

### 10.3 Cyoda help (`cmd/cyoda/help/content/`)

**`config/auth.md`** — new entries:
- Under JWT mode: `CYODA_JWT_BOOTSTRAP_AUDIENCE` (default `client`; alt `human`).
- New **IAM features** subsection:
  - `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED` (default `false`; off → 404 `FEATURE_DISABLED`).
  - `CYODA_IAM_TRUSTED_KEY_MAX_PER_TENANT` (default `10`; `0` means unbounded; counts currently-valid keys only).
  - `CYODA_IAM_TRUSTED_KEY_MAX_VALIDITY_DAYS` (default `365`; default-only — user-supplied `validTo` honored as-is).
  - `CYODA_IAM_TRUSTED_KEY_MAX_JWK_PROPERTIES` (default `20`).
  - `CYODA_IAM_KEYPAIR_DEFAULT_VALIDITY_DAYS` (default `365`; applies to bootstrap + runtime-issued keypairs; startup logs WARN if active key expires within 30 days).
- New **JWT signing keypair rotation** subsection (3 paragraphs) — bootstrap vs runtime rotation, v0.8.0 persistence limitation, expiry behaviour.
- New EXAMPLES block — trusted-key flag enabled.

**`errors/`** — 6 new topic files (mirror `TRUSTED_KEY_NOT_FOUND.md` template):
- `FEATURE_DISABLED.md`
- `UNSUPPORTED_ALGORITHM.md`
- `UNSUPPORTED_KEY_TYPE.md`
- `KEY_OWNED_BY_DIFFERENT_TENANT.md`
- `KEYPAIR_NOT_FOUND.md`
- `TRUSTED_KEY_CAP_REACHED.md`

**`errors.md`** — append the 6 new codes to the catalogue table (alphabetical).

**`errors/TRUSTED_KEY_NOT_FOUND.md`** (existing) — update DESCRIPTION to add: "Returned uniformly for kids that don't exist AND kids owned by another tenant; the response does not distinguish — by design, to prevent cross-tenant existence enumeration."

**`errors/NOT_FOUND.md`** (existing) — add SEE ALSO entry pointing at the new `KEYPAIR_NOT_FOUND` for keypair-specific 404s.

**`openapi.md:96`** — clarify which `/oauth/*` operations are live in v0.8.0 (the 10 oauth/keys ops) and which remain 501 (OIDC providers — #194-D, v0.9.0+).

**`quickstart.md` / `helm.md` / `run.md`** — verified during implementation; touch only if existing JWT-bootstrap examples need the audience default mentioned.

### 10.4 README.md

Add the five new env vars to the config-reference table.

### 10.5 `DefaultConfig()`

Six new fields with documented defaults (composed under `auth.IAMConfig`).

### 10.6 v0.8.0 release notes

Four operational notes + one limitation:

- Runtime-issued signing keypairs lost on restart (bootstrap PEM-derived KID survives) — points at follow-up §3.5.
- `trustedKeyRegistrationEnabled` default `false`; existing customers using `/oauth/keys/trusted/*` through the legacy mux must opt in via env var.
- KV trusted-key entries written by versions < v0.8.0 use a different key prefix and are not visible to v0.8.0's OpenAPI surface (orphaned, not deleted). Operators must re-register affected keys. Inspection: `grep "^trustedkey:[^:]*$" <kvdump>`. cyoda-go has no known production users on this surface.
- v0.8.0 → pre-v0.8.0 rollback: trusted keys created under v0.8.0 will be visible to pre-v0.8.0 binary as **mangled-kid entries** (`<tenantID>:<kid>` treated as the kid). Operators must purge them out-of-band before rollback if visibility matters.
- **Algorithm support**: cyoda-go v0.8.0 signs and verifies RS256 only. The OpenAPI declares the full enum (RS*, PS*, ES*, EdDSA); non-RS256 values are rejected with 400 `UNSUPPORTED_ALGORITHM`. Trusted-key registration accepts only `kty=RSA` JWKs (`kty=EC`/`OKP` rejected with 400 `UNSUPPORTED_KEY_TYPE`). v0.8.1 follow-up issue tracks multi-algorithm + non-RSA `kty` support.

---

## 11. Acceptance

- [ ] All 10 operations return OpenAPI-conformant DTOs through the chi router.
- [ ] `mux.Handle("/oauth/keys/", ...)` removed from `app/app.go`.
- [ ] adminMux entries for `/oauth/keys/keypair` and `/oauth/keys/trusted` removed.
- [ ] `internal/auth/keys.go` and `internal/auth/trusted.go` HTTP handlers removed; reusable validators retained in new `internal/auth/keyvalidation.go`. `internal/auth/jwt.go` NOT modified.
- [ ] `auth.RequireAdmin` exported; in-package call sites updated.
- [ ] `auth.AuthService.TrustedKeyStore()` accessor added.
- [ ] `Handler` struct gains `keyStore`, `trustedKeyStore`, `iam` fields; `account.New` signature + `app.go:443` call updated.
- [ ] All `Save`/`Register` call sites (including bootstrap path in `service.go`) updated to pass `RotateOptions{}` when no flip needed.
- [ ] `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED` implemented (default `false`); 404 `FEATURE_DISABLED` on all 5 trusted-key endpoints when off.
- [ ] `CYODA_IAM_TRUSTED_KEY_MAX_PER_TENANT` implemented (default `10`; `0` unbounded; negative rejected at boot); 400 `TRUSTED_KEY_CAP_REACHED` at cap; counts only currently-valid keys; existing 409-on-registry-full path replaced; resolves `TODO(#163)`.
- [ ] `CYODA_IAM_TRUSTED_KEY_MAX_VALIDITY_DAYS` implemented (default `365`; default-only at register; ≤ 0 rejected at boot).
- [ ] `CYODA_IAM_TRUSTED_KEY_MAX_JWK_PROPERTIES` implemented (default `20`; ≤ 0 rejected at boot).
- [ ] `CYODA_IAM_KEYPAIR_DEFAULT_VALIDITY_DAYS` implemented (default `365`; applies to bootstrap + runtime keys; ≤ 0 rejected at boot; startup WARN if active key expires within 30 days).
- [ ] `CYODA_JWT_BOOTSTRAP_AUDIENCE` implemented (default `client`; invalid enum rejected at boot); pre-merge grep verifies no existing path signs human-audience tokens.
- [ ] JWK validation: `kty=RSA` required (else 400 `UNSUPPORTED_KEY_TYPE`); `kid≡keyId` check; max-fields check.
- [ ] Algorithm validation: only `RS256` accepted at adapter; all other enum values → 400 `UNSUPPORTED_ALGORITHM`; `jwt.go` unchanged.
- [ ] Tenant-scoped trusted-key store; cross-tenant 404 / register-collision 409.
- [ ] In-package `getTrustedKeyByKID(store, kid)` helper implemented; token-verification path uses it; no new check on `trustedKey.TenantID` added (matches cloud per §7.1).
- [ ] `KVTrustedKeyStore` cache value carries `TenantID`; tenant-scoped methods verify cached TenantID matches caller after cache hit AND post-`loadOne` re-cache; serialization round-trips `tenantID` + `jwk`.
- [ ] `RotateOptions{Invalidate, GracePeriodSec}` introduced; `Save`/`Register` carry it for atomic sibling-flip; in-memory mutation runs last for rollback safety.
- [ ] `GetActive(audience)` selects max-`ValidFrom` when multiple active (matches cloud).
- [ ] Reactivate accepts `ReactivateKeyRequestDto { validFrom?, validTo }`; rejects absent/past `validTo` with 400; idempotent on already-active key.
- [ ] `publicKey` returned as base64-DER (no PEM armor).
- [ ] **OpenAPI codegen regen**: `api/generated.go` regenerated after spec changes (new ReactivateKeyRequestDto + JWK schema fix changes type/method signatures).
- [ ] OpenAPI spec: 501s removed from the 10 ops; every 4xx/5xx switched from `ErrorResponseDto` to `ProblemDetail` + `application/problem+json`; `SUPER_USER` → `ROLE_ADMIN` in prose; JWK schema fixed; `ReactivateKeyRequestDto` added + both reactivate ops gain required request body + 400 response; default-behaviour prose added to issue/register/invalidate ops.
- [ ] Body-size assertion: shared `boundedJSONDecode` helper applied to all 4 POST adapters; integration sub-test relocated to E2E; old `integration_test.go` sub-test for trusted-key deleted.
- [ ] Audit table dispositions updated.
- [ ] Cyoda help: 6 new error topics, 5 new env vars + bootstrap audience, errors.md + openapi.md index updates, existing `TRUSTED_KEY_NOT_FOUND.md` updated for cross-tenant case, `NOT_FOUND.md` cross-ref to `KEYPAIR_NOT_FOUND`.
- [ ] `DefaultConfig()` updated; README config table updated.
- [ ] Release notes: 5 entries (restart loss, flag default, orphan story with grep, rollback hazard, algorithm scope).
- [ ] Per-divergence regression-lock tests in `9.2` cover all 10 §3.2 items.
- [ ] Full test suite (`go test ./... -v`) + `make test-all` + `go test -race ./...` green.
- [ ] Follow-up issue filed: multi-algorithm signing (RS384/RS512/PS*/ES*/EdDSA) + non-RSA JWK (`kty=EC`/`OKP`).

---

## 12. Out of scope (tracked elsewhere)

- Multi-algorithm signing (RS384/RS512/PS*/ES*/EdDSA) — v0.8.1 follow-up filed at PR-merge time.
- Non-RSA JWK kty (`EC`/`OKP`) — same follow-up.
- Signing-key persistence (follow-up §3.5 — secrets-management interface design).
- M2M client-store persistence (follow-up §3.6 — picked up by #194-B's spec, not this one).
- `/clients` OpenAPI conformance (#194-B).
- `accountSubscriptionsGet` (#194-C).
- OIDC providers subsystem (#194-D, v0.9.0+).
- Periodic prune of past-`ValidTo` trusted-key entries (lazy filter sufficient at expected volumes).
- Cleanup of orphan pre-v0.8.0 `trustedkey:<kid>` KV entries (operators handle out-of-band per release notes).
- Same-tenant idempotent re-register transactional semantics (cyoda-go preserves silent upsert; cloud's atomic delete-and-replace deferred).
