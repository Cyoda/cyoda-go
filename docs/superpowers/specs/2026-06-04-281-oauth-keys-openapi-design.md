# #281 — OpenAPI conformance for `/oauth/keys/*` (keypair + trusted)

**Status:** approved
**Date:** 2026-06-04
**Issue:** [#281](https://github.com/Cyoda-platform/cyoda-go/issues/281)
**Milestone:** v0.8.0
**Sub-issue of:** #194 (decomposition spec: `docs/superpowers/specs/2026-06-04-194-decomposition-design.md` §3.1)

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

A deep scan of the Kotlin reference implementation at `/Users/paul/dev/cyoda/backend/src/main/kotlin/net/cyoda/saas/controller` and its services informed the bucketing below. Where cyoda-go can match cloud behaviour without out-of-scope dependencies, it does. Documented divergences are explicit.

| Capability | Cloud behaviour | cyoda-go decision |
|---|---|---|
| `algorithm` enum (10 values) | All fully supported (RS*, PS*, ES*, EdDSA) | RS*+PS* implemented (same RSA-2048 keypair, signing-method dispatch); ES*+EdDSA gated by `UnsupportedFeatureBehavior` env knob |
| `audience` (human\|client) | Partitioned via per-audience providerId | Stored on `KeyPair`; `GetActive(audience)` partitions; bootstrap key gets configurable default audience |
| `validFrom`/`validTo` | Explicit timestamps; lazy `isValidKey(now)` filter | Same — `ValidTo *time.Time`; lazy filter at JWKS and trusted-key validator reads |
| `invalidateCurrent` / `invalidatePrevious` + `gracePeriodSec` | Lazy validity (`validTo = now + grace`), no timers | Same — atomic mutex-scoped flip of siblings' `ValidTo`; lazy filter handles verification window |
| Standalone `POST .../invalidate` body | `InvalidateKeyRequestDto { gracePeriodSec: int? = 0 }` | Same |
| `reactivate` semantics | Clears `validTo` (original expiry lost; TODO in cloud) | Same; documented limitation |
| `jwk` on `TrustedKeyResponseDto` | Decoded then re-serialised via `Jwks.builder()` | Stored as `json.RawMessage` and emitted verbatim (simpler; OpenAPI-conformant since DTO declares `jwk: object`) |
| `legalEntityId` + tenant scoping | Per-tenant partitioning via `owner = legalEntityId` | Same — `TenantID` on `TrustedKey`; CRUD scoped by `uc.Tenant.ID` |
| Cross-tenant keyId collision | `409 Conflict` | Same — `KEY_OWNED_BY_DIFFERENT_TENANT` |
| `trustedKeyRegistrationEnabled` flag | Default **false**; gates all 5 trusted-key ops with **404** | Same — `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED` (default `false`); 404 `FEATURE_DISABLED` |
| Private signing-key persistence | Encrypted PKCS#8 blob in DB | Out of scope — held in-memory; runtime-issued keys lost on restart; bootstrap (PEM-derived deterministic KID) survives. Per follow-up §3.5. |
| Role gate | ADMIN ∨ SUPER_USER | **ROLE_ADMIN only**, per issue body. OpenAPI prose updated SUPER_USER → ROLE_ADMIN. |
| Public JWKS endpoint | None — verification uses direct `findKeyEntryByKeyId` | cyoda-go retains `/.well-known/jwks.json`; sourced from `ListForVerification()` so grace-period keys remain discoverable until `ValidTo` |

---

## 3. Bucketing

### 3.1 Easy bucket — implemented in this PR

1. **`algorithm` enum (RS*+PS* subset)** — `RS256, RS384, RS512, PS256, PS384, PS512`. All six use the same RSA-2048 keypair; signing-method dispatch picks the digest+padding at sign time. Default `RS256` when absent. Stored on `KeyPair.Algorithm`. Round-tripped in response.
2. **`publicKey` PEM** — `x509.MarshalPKIXPublicKey` → `pem.Encode` from the existing `*rsa.PublicKey`. Returned in `JwtKeyPairResponseDto.publicKey`.
3. **`validFrom`/`validTo` timestamps on `KeyPair` and `TrustedKey`** — rename `KeyPair.CreatedAt → ValidFrom`; add `KeyPair.ValidTo *time.Time` and confirm/keep `TrustedKey.ValidTo *time.Time`. Nil `ValidTo` = no expiry. Set to `now + gracePeriodSec` at invalidate time. Lazy filter at all verification reads.
4. **`validFrom`/`validTo` overrides on issue/register** — honoured when supplied; defaults `validFrom = now()`, `validTo = nil`. (Cloud defaults `validTo = now + 365d`; cyoda-go diverges to nil and documents it — operators can opt in to expiry via the request body.)
5. **`jwk` round-trip on trusted-key response** — store the raw `json.RawMessage` on `TrustedKey`; emit verbatim. Existing JWK validation (`parseRSAPublicKeyFromJWK`) still gates incoming registrations.
6. **`legalEntityId` + per-tenant trusted-key scoping** — add `TenantID spi.TenantID` to `TrustedKey`. CRUD methods scoped by caller's `uc.Tenant.ID`. Cross-tenant `Get`/`Delete`/`Invalidate`/`Reactivate` returns **404** `TRUSTED_KEY_NOT_FOUND` (does not leak existence). `Register` with keyId-collision against another tenant returns **409** `KEY_OWNED_BY_DIFFERENT_TENANT`.
7. **`invalidateCurrent` (issue) / `invalidatePrevious` (register) booleans + `invalidateGracePeriodSec`** — atomic under the store mutex: locate other active keys in the same partition (keypairs: same `audience`; trusted keys: same `tenantID`), set `ValidTo = now + gracePeriodSec` and `Active = false`. Grace=0 ⇒ immediate. Lazy filter at JWKS endpoint and trusted-key validator paths means tokens signed with the just-invalidated kid continue to verify until grace passes.
8. **Standalone `POST .../invalidate` request body** — `InvalidateKeyRequestDto { gracePeriodSec int64 }`; absent body or nil ⇒ 0 (immediate). Both keypair and trusted-key invalidate endpoints accept the body.
9. **`audience` (human\|client) on keypairs** — add `Audience string` to `KeyPair`. New `KeyStore.GetActive(audience string)` partitions. `issueJwtKeyPair` partitions by request audience. `getCurrentJwtKeyPair?audience=X` returns **404** `KEYPAIR_NOT_FOUND` if no active key for X. Bootstrap key gets audience `CYODA_JWT_BOOTSTRAP_AUDIENCE` (default `client`, alternative `human`); the existing M2M token-signing path (`POST /oauth/token`) becomes `GetActive("client")`.
10. **Reactivate** — `Reactivate(kid)` clears `ValidTo` to nil and sets `Active = true`. Original expiry permanently lost (matches cloud; documented).

### 3.2 Hard bucket — env-toggled, single capability

`algorithm ∈ {ES256, ES384, ES512, EdDSA}` requires non-RSA generators (ECDSA / Ed25519) and the matching JWK encoding paths. Behind `CYODA_IAM_UNSUPPORTED_FEATURE_BEHAVIOR` (default `reject` → 400 `UNSUPPORTED_ALGORITHM`; `warn` → fall back to RS256 with `common.AddWarning(ctx, ...)`). Follow-up issue (to be filed at PR-merge time) tracks adding ECDSA and Ed25519 generators.

### 3.3 Documented divergences

1. **JWKS endpoint** — cyoda-go retains `/.well-known/jwks.json`; cloud has no public JWKS surface. The cyoda-go endpoint sources from `KeyStore.ListForVerification()` so grace-period keys remain published until `ValidTo` passes.
2. **Role gate** — cyoda-go enforces **ROLE_ADMIN** only; cloud accepts ADMIN ∨ SUPER_USER. Per issue body, the smaller-blast-radius change is to align OpenAPI prose to code (`SUPER_USER` → `ROLE_ADMIN`).
3. **Signing-key persistence** — out of scope; runtime-issued keypairs lost on restart. Bootstrap key (PEM-derived deterministic KID per ARCHITECTURE.md §7.2) survives. v0.8.0 release notes call this out and link follow-up §3.5.
4. **`validTo` default** — cyoda-go defaults to nil (no expiry); cloud defaults to `validFrom + 365d`. Operator can supply an explicit `validTo` to match cloud behaviour.

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
```

### 4.2 Store interface changes

```go
type KeyStore interface {
    Save(kp *KeyPair) error
    Get(kid string) (*KeyPair, error)
    GetActive(audience string) (*KeyPair, error)            // CHANGED: audience param
    List() []*KeyPair                                       // all entries (admin listing)
    ListForVerification() []*KeyPair                        // NEW: lazy ValidTo filter
    Delete(kid string) error
    Invalidate(kid string, gracePeriodSec int64) error      // CHANGED: grace param
    Reactivate(kid string) error
    InvalidateOthersInPartition(audience string, except string, gracePeriodSec int64) error  // NEW: atomic helper
}

type TrustedKeyStore interface {
    Register(tk *TrustedKey) error                          // 409 on cross-tenant keyId collision
    Get(tenantID spi.TenantID, kid string) (*TrustedKey, error)         // CHANGED
    List(tenantID spi.TenantID) []*TrustedKey                            // CHANGED
    ListForVerification() []*TrustedKey                                  // NEW: all tenants, ValidTo-filtered
    Delete(tenantID spi.TenantID, kid string) error
    Invalidate(tenantID spi.TenantID, kid string, gracePeriodSec int64) error
    Reactivate(tenantID spi.TenantID, kid string) error
    InvalidateOthersForTenant(tenantID spi.TenantID, except string, gracePeriodSec int64) error  // NEW: atomic helper
}
```

### 4.3 Adapter files (handler.go split)

The existing `internal/domain/account/handler.go` keeps the account methods. The 10 new methods live in:

- `internal/domain/account/keys_adapter.go` — 5 keypair handler methods + DTO helpers (`toJwtKeyPairResponse`, `parseIssueRequest`, etc.).
- `internal/domain/account/trusted_adapter.go` — 5 trusted-key handler methods + DTO helpers (`toTrustedKeyResponse`, `tenantFromContext`, etc.).

Both files take `*Handler` as the receiver — no struct-field changes.

### 4.4 Removed (dead code)

- `internal/auth/keys.go` — `KeysHandler`, `keysInfoResponse`, all `ServeHTTP` path-parsing. Domain logic (`issueKeyPair`, `getCurrent`, `deleteKeyPair`, `invalidateKeyPair`, `reactivateKeyPair`) moves to `internal/auth/store.go` helpers if reusable, or inlines into the new adapter.
- `internal/auth/trusted.go` — `TrustedKeysHandler`, `trustedKeyInfoResponse`, `ServeHTTP`, `handleList/Register/Delete/Invalidate/Reactivate`, `validateLifecycleKID`, `extractKeyID`. The input-validation helpers (`trustedKIDPattern`, `parseRSAPublicKeyFromJWK`, `validateRSAPublicExponent`) stay — moved to a small `internal/auth/keyvalidation.go` so both the trusted-key store and the new adapter can reuse them. `decodeBase64URL` lives in `internal/auth/jwt.go` and is unaffected.
- `internal/auth/service.go` adminMux entries for `/oauth/keys/...` (lines 86–89). The `/account/m2m` entries stay (#194-B territory).
- `app/app.go:482` `mux.Handle("/oauth/keys/", ...)` — removed.

### 4.5 Wiring changes (`app/app.go`)

- Bootstrap-key save now sets `Algorithm = "RS256"` and `Audience = config.Bootstrap.Audience` (new field; default `client`).
- New `AuthConfig` fields:
  - `BootstrapAudience string`
  - `TrustedKeyRegistrationEnabled bool`
  - `UnsupportedFeatureBehavior string` (`reject` | `warn`)
- M2M signing call sites switch to `GetActive("client")`.
- JWKS endpoint source switches to `ListForVerification()`.

---

## 5. Error handling

### 5.1 New error codes (`internal/common/error_codes.go`)

| Code | HTTP | Trigger |
|---|---|---|
| `FEATURE_DISABLED` | 404 | Trusted-key endpoint while `TrustedKeyRegistrationEnabled=false` |
| `UNSUPPORTED_ALGORITHM` | 400 | `algorithm ∈ {ES*, EdDSA}` while `UnsupportedFeatureBehavior=reject` |
| `KEY_OWNED_BY_DIFFERENT_TENANT` | 409 | `registerTrustedKey` keyId collision with another tenant |
| `KEYPAIR_NOT_FOUND` | 404 | Keypair lifecycle on missing kid; `getCurrent` with no key for requested audience |

`TRUSTED_KEY_NOT_FOUND` already exists and is reused for tenant-scoped lookups (returned uniformly whether the kid doesn't exist or belongs to another tenant — no existence leakage).

### 5.2 Error-shape convention

Per `.claude/rules/error-handling.md` and the existing pattern in `internal/auth/`:

- 4xx via `common.Operational(status, code, "short message")` — full domain detail, error code prefix on detail.
- 5xx via `common.Internal("description", err)` — generic ticket message; raw error in slog at ERROR.
- Tenant-scoped 404s return the generic `key not found` — never echo `kid`, `tenant`, or sibling-existence hints in the body.

### 5.3 Security checks (Gate 3)

- **Tenant isolation** — `TenantID` derived from `uc.Tenant.ID` server-side, never from request body. Cross-tenant access returns 404 (lookup) or 409 (collision on register), never 200+leak or 403+confirmation.
- **Input validation** — `algorithm`/`audience`/`validFrom`/`validTo` validated at the decode boundary; reject malformed early with 400 `BAD_REQUEST`. `keyId` validated via existing `trustedKIDPattern` regex (`^[A-Za-z0-9._-]{1,128}$`); now applied to keypair `keyId` as well.
- **Output sanitisation** — private RSA material never serialised in any response or log line. slog field allowlist: `kid`, `tenant`, `audience`, `algorithm`. No stack traces in 5xx responses.
- **No secrets in logs** — bootstrap PEM stays unsigned/unprinted; rotated kids are logged only when an operation references them.
- **No issue IDs in shipped artefacts** — per the standing rule, no `#281` in error messages, response bodies, OpenAPI prose, code comments, or help topics.

---

## 6. Persistence

- `InMemoryKeyStore` and `InMemoryTrustedKeyStore` updated for new field/signature shapes.
- `KVTrustedKeyStore` (already wired in production via `app/app.go:207–239`):
  - KV-key encoding changes from `trustedkey:<kid>` to `trustedkey:<tenantID>:<kid>` to make tenant isolation an invariant of the storage layer itself.
  - **Backfill** for pre-v0.8.0 entries (no tenant in key): on first scan, log a WARN-level event and skip — we can't safely promote them to a tenant. Operators with existing trusted keys must re-register them post-upgrade. Default-tenant single-tenant deployments are unaffected because the bootstrap tenant id is fixed; the migration step lives in v0.8.0 release notes.
- Signing-key persistence — out of scope (follow-up §3.5). Bootstrap key survives restart; runtime-issued keypairs do not.

---

## 7. Routing wiring

1. `app/app.go:482` `mux.Handle("/oauth/keys/", ...)` — removed.
2. `internal/auth/service.go` adminMux — drop `/oauth/keys/keypair` (×2) and `/oauth/keys/trusted` (×2) entries. `/account/m2m` entries stay until #194-B.
3. Chi router (mounted at `/` via the generated `HandlerFromMux`) now owns all `/oauth/keys/*` paths via the 10 `ServerInterface` methods on `*Handler`.
4. `requireAdmin` called inline at the start of each adapter method (no middleware change; matches existing pattern in `admin_guard.go`).

---

## 8. Body-size relocation

`internal/auth/integration_test.go:268–301` has two sub-tests routing through `svc.AdminHandler()`:

- `m2m create endpoint rejects oversized body` — left untouched (#194-B territory).
- `trusted key register endpoint rejects oversized body` — relocated: the body-size limit (`http.MaxBytesReader(w, r.Body, 1<<20)`) moves into the new `RegisterTrustedKey` adapter, and an **E2E** test asserts the same 413/400 behaviour by POSTing >1 MB through chi. The integration-level sub-test is deleted.

---

## 9. Testing strategy (Gates 1, 2, 5)

### 9.1 Unit (TDD red-first)

- `internal/auth/store_test.go` — audience-scoped `GetActive`; grace-period `Invalidate` sets `ValidTo`; `ListForVerification` lazy filter; reactivate clears `ValidTo`; cross-tenant trusted-key isolation; same-tenant upsert vs cross-tenant 409; `InvalidateOthersInPartition`/`...ForTenant` atomic helpers.
- `internal/auth/kv_trusted_store_test.go` — tenant-scoped key prefix; old-shape entries skipped with warning.
- `internal/auth/keypair_signing_test.go` (new) — sign + verify a sample JWT for each of `RS256/RS384/RS512/PS256/PS384/PS512`.

### 9.2 Adapter

- `internal/domain/account/keys_adapter_test.go`, `trusted_adapter_test.go` — per-operation DTO marshalling round-trip; ROLE_ADMIN gate (401 unauth, 403 wrong role); validation error codes; response shape matched against the generated DTOs.
- `algorithm=ES256` with `UnsupportedFeatureBehavior=reject` → 400 `UNSUPPORTED_ALGORITHM`; with `warn` → 200 + warning + `algorithm=RS256` in response.
- `TrustedKeyRegistrationEnabled=false` → 404 `FEATURE_DISABLED` on all 5 trusted-key endpoints.
- Body-size limit: POST > 1 MB to `RegisterTrustedKey` → 413 or 400.

### 9.3 E2E (Gate 2)

- `internal/e2e/oauth_keys_test.go` — one scenario per operation through the full HTTP stack, authenticated via the existing bootstrap-M2M-client → `POST /oauth/token` → Bearer flow.
- **Grace-period round-trip** — issue keypair A, issue keypair B with `invalidateCurrent=true, invalidateGracePeriodSec=2`. Assert A's kid is in `/.well-known/jwks.json` immediately after; `sleep(3s)`; assert A's kid is no longer in JWKS. (The cross-process token round-trip is covered at the unit-test layer because E2E cannot reach the rotated key's private half.)
- **Persistence** — register a trusted key via `POST /oauth/keys/trusted`, restart the in-process server against the same store factory, assert `GET /oauth/keys/trusted` still returns it.
- **Cross-tenant isolation** — bootstrap two M2M clients in distinct tenants; register the same `keyId` from tenant A; register from tenant B → 409 `KEY_OWNED_BY_DIFFERENT_TENANT`; `GET` from tenant B → 404 (does not list tenant A's key).
- **Feature-flag** — with `TrustedKeyRegistrationEnabled=false`, all 5 trusted-key endpoints return 404 `FEATURE_DISABLED`; keypair endpoints unaffected.

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

---

## 10. Documentation updates (Gate 4)

### 10.1 OpenAPI spec (`api/openapi.yaml`)

- Remove `501 NotImplemented` declarations from all 10 operations.
- Replace `SUPER_USER` → `ROLE_ADMIN` in the 10 operation descriptions (the role-gate divergence from cloud is intentional per §3.3).
- Keep the `trustedKeyRegistrationEnabled` 404 declarations and prose — they accurately describe the implementation.
- Embedded `//go:embed` of `api/openapi.yaml` automatically picks the changes up; no oapi-codegen regeneration needed (per `project_openapi_spec_embed_via_goembed`).

### 10.2 Audit table (`docs/superpowers/audits/2026-04-29-openapi-conformance-audit.md`)

For each of the 10 rows (lines 122–131), change disposition from `out-of-scope-not-implemented (#194)` to `match`. Cite the merge commit at PR-merge time.

### 10.3 Cyoda help (`cmd/cyoda/help/content/`)

**`config/auth.md`** — new entries:
- Under JWT mode: `CYODA_JWT_BOOTSTRAP_AUDIENCE` (default `client`; alt `human`).
- New **IAM features** subsection:
  - `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED` (default `false`; off → 404 `FEATURE_DISABLED`).
  - `CYODA_IAM_UNSUPPORTED_FEATURE_BEHAVIOR` (default `reject`; alt `warn`).
- New **JWT signing keypair rotation** subsection (3 paragraphs) — bootstrap vs runtime rotation, v0.8.0 persistence limitation.
- New EXAMPLES block — trusted-key flag enabled.

**`errors/`** — 4 new topic files (mirror `TRUSTED_KEY_NOT_FOUND.md` template):
- `FEATURE_DISABLED.md`
- `UNSUPPORTED_ALGORITHM.md`
- `KEY_OWNED_BY_DIFFERENT_TENANT.md`
- `KEYPAIR_NOT_FOUND.md`

**`errors.md`** — append the 4 new codes to the catalogue table (alphabetical).

**`openapi.md:96`** — clarify which `/oauth/*` operations are live in v0.8.0 (the 10 oauth/keys ops) and which remain 501 (OIDC providers — #194-D, v0.9.0+).

**`quickstart.md` / `helm.md` / `run.md`** — verified during implementation; touch only if existing JWT-bootstrap examples need the audience default mentioned.

### 10.4 README.md

Add the three new env vars to the config-reference table.

### 10.5 `DefaultConfig()`

Three new fields with documented defaults.

### 10.6 v0.8.0 release notes

Two known limitations called out:

- Runtime-issued signing keypairs lost on restart (bootstrap PEM-derived KID survives) — points at follow-up §3.5.
- `trustedKeyRegistrationEnabled` default `false`; existing customers using `/oauth/keys/trusted/*` through the legacy mux must opt in.

---

## 11. Acceptance

- [ ] All 10 operations return OpenAPI-conformant DTOs through the chi router.
- [ ] `mux.Handle("/oauth/keys/", ...)` removed from `app/app.go`.
- [ ] adminMux entries for `/oauth/keys/keypair` and `/oauth/keys/trusted` removed.
- [ ] `internal/auth/keys.go` and `internal/auth/trusted.go` HTTP handlers removed; reusable validators retained.
- [ ] `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED` implemented (default `false`); 404 `FEATURE_DISABLED` on all 5 trusted-key endpoints when off.
- [ ] `CYODA_IAM_UNSUPPORTED_FEATURE_BEHAVIOR` implemented (default `reject`; `warn` falls back with warning).
- [ ] `CYODA_JWT_BOOTSTRAP_AUDIENCE` implemented (default `client`).
- [ ] Tenant-scoped trusted-key store; cross-tenant 404 / register-collision 409.
- [ ] Grace-period invalidation via lazy `ValidTo` filter; JWKS surfaces grace-period keys.
- [ ] Body-size assertion relocated from `integration_test.go` to the new adapter + E2E.
- [ ] OpenAPI spec: 501s removed from the 10 ops; `SUPER_USER` → `ROLE_ADMIN` in prose.
- [ ] Audit table dispositions updated.
- [ ] Cyoda help: 4 new error topics, 3 new env vars, openapi/errors index updates.
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
