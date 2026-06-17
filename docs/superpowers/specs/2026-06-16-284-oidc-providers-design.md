# Issue #284 — OIDC providers subsystem (greenfield)

**Status:** approved
**Date:** 2026-06-16
**Issue:** [#284](https://github.com/Cyoda-platform/cyoda-go/issues/284)
**Umbrella:** [`2026-06-04-194-decomposition-design.md`](2026-06-04-194-decomposition-design.md) §3.4 (amended 2026-06-16, pulled into v0.8.0)
**Predecessor:** issue #123 (closed as duplicate; content consolidated into #284)
**Scope:** Land a per-tenant OIDC provider registry, the seven REST handlers currently stubbed at HTTP 501 (`internal/domain/account/handler.go:95-121`), a chained multi-issuer JWT validator that consults the registry, single-topic cluster broadcast for cross-node cache eviction, four `CYODA_OIDC_*` configuration env vars (plus one SSRF-override), and 32 parity tests (27 ported from cyoda-cloud + 5 cyoda-go-specific). Net result: zero `h.stub` calls on the OIDC path, seven `NotImplemented` declarations removed from `api/openapi.yaml`, tokens from registered OIDC providers accepted by the existing bearer-auth middleware.

---

## 1. Context

cyoda-go exposes the full OpenAPI surface for OIDC provider management at `/oauth/oidc/providers/*` (`api/openapi.yaml:4762-5160`), but every handler is a stub returning `501 NOT_IMPLEMENTED` (`internal/domain/account/handler.go:95-121`, routed via `internal/api/server.go:507-561`). The reference implementation lives in cyoda-cloud:

- `platform-service-iam/src/main/kotlin/com/cyoda/iam/model/entity/JWKOIDCEntity.kt`
- `platform-service-iam/src/main/kotlin/com/cyoda/iam/service/oidc/JWKOIDCService.kt` (`JWKOIDCServiceImpl`)
- `backend/src/main/kotlin/net/cyoda/saas/controller/OIDCProviderController.kt`
- `backend/src/main/kotlin/net/cyoda/saas/iam/OIDCProviderInteractor.kt`
- `platform-service-iam/src/main/kotlin/com/cyoda/iam/service/oidc/notifications/JWKOIDCCacheIntercom.kt`
- `integration-tests/src/test/kotlin/net/cyoda/saas/controller/OIDCProviderControllerIT.kt` (**27 tests**; the issue body's "28" is off-by-one)

The umbrella decomposition (§3.4) originally deferred this work to v0.9.0; the amendment at the head of that document (2026-06-16) pulls it forward into v0.8.0 as the milestone's headline IAM deliverable. That acceptance-criterion item ("decomp spec updated to v0.8.0") is therefore **already satisfied** in tree; this spec records the satisfaction explicitly so reviewers don't double-look.

Today's `JWKSValidator` (`internal/auth/validator.go:15-50`) is single-issuer: it carries one `issuer string` field validated against the token's `iss` claim at lines 81-84, and consults one `KeySource`. The HTTPJWKSSource cache is already keyed by `(issuer, kid)` per the closed #97 fix (`internal/auth/http_jwks_source.go:38-41`). No persistent JWK storage exists today — `TrustedKey` entities (`internal/auth/store.go:34-44`) cover externally-registered public keys but are independent of OIDC discovery.

Per the umbrella's §4 reconciliation, mutating endpoints require `ROLE_ADMIN` (cyoda-cloud uses `SUPER_USER`; OpenAPI prose currently says the latter).

Issue #34 (trusted-key audit) was investigated for overlap and is independent: its scope is the trusted-key HTTP surface hardening, not the OIDC subsystem. It stays its own issue.

## 2. Decisions

| # | Decision | Choice | Rationale |
|---|---|---|---|
| D1 | Tenancy scope of the OIDC provider registry | **Per-tenant.** Each legal entity owns its providers; management API filters by `tenantFromCtx(r)`; KV namespace per tenant. Token validation iterates providers across all tenants; the resolving provider's `OwnerLegalEntityID` determines the user context's tenant. | The issue body's "tenant isolation via `ownerLegalEntityId`" wording is honoured. cyoda-cloud is system-wide with `SUPER_USER` — we accept the divergence: cyoda-go-light deploys into more varied contexts (single-tenant SaaS, on-prem multi-tenant) where per-tenant ownership is the safer default. Validation-side global iteration is unavoidable in any case (tokens carry no tenant claim). |
| D2 | Persistence backend for `OidcProviderStore` | **`KeyValueStore` SPI**, namespace `oidc:providers:<tenantID>` + secondary index `oidc:uri-idx:<tenantID>`. Index key = SHA-256 hex of `wellKnownConfigUri`. Pattern: `KVTrustedKeyStore` (`internal/auth/kv_trusted_store.go`). | Zero per-plugin work: memory/sqlite/postgres/cassandra get OIDC for free via the generic KV path. SHA-256 keys avoid charset constraints across plugins. Forward-compat `ProviderID` field kept (always `"USER_EXTERNAL"` in v0.8.0) to avoid a later schema migration when system-defined providers arrive. Trade-off explicitly accepted: the (index, blob) write pair is best-effort transactional with a microseconds-wide window; rollback on the second `Put` failing. |
| D3 | Multi-issuer validator integration shape | **Chained validators.** New `auth.Validator` interface (single `Validate(token) (*spi.UserContext, error)` method), new `auth.ChainedValidator`, new `oidc.OIDCValidator`. `JWKSValidator` stays single-issuer; one sentinel-error rename (`ErrUnknownIssuer`) makes the chain fall-through work. | Smallest-interface Go idiom. Zero blast radius on existing call sites (one constructor wrap at HTTP/gRPC auth middleware). Single-responsibility — lifecycle bookkeeping lives in `OIDCValidator` only. Future-proof for SAML / federated dev tokens without touching `JWKSValidator`. Race-safe by construction. |
| D4 | Cluster broadcast wire format | **One topic `oidc.providers`** with a JSON envelope `{op: "reload"|"invalidate"|"reload_all", tenantID, wellKnownConfigUri?}`. Mirrors `JWKOIDCCacheNotification`; mirrors `modelcache`'s single-topic-per-subsystem convention. | One Subscribe call, one switch. Avoids gossip topic-cardinality bloat. Receiver-side handlers idempotent. |
| D5 | Wire shape for "update inactive provider" | **`HTTP 409 Conflict` + error code `OIDC_PROVIDER_INACTIVE`**. Diverges from cyoda-cloud's `IllegalStateException → 5xx generic`. | Matches cyoda-go's project-wide convention: 4xx carries domain detail with an error code; 5xx is reserved for actual server faults (CLAUDE.md). Surfacing a precondition failure as 5xx would also violate Gate 3 output-sanitization. The relevant parity test is rewritten to assert the cyoda-go wire shape, with an explanatory comment recording the deliberate divergence. |
| D6 | Validator key-resolution data structure | **`kid`-indexed fast-path inside `Registry`** (`map[kid][]providerCandidate` + provider-cache main map). Global iteration on miss only. | Worst-case `O(active providers)` is unacceptable for the bearer-auth hot path. The `kid`-fast-path makes resolution `O(1)` average. The miss-path retains the global iteration for correctness when an unseen `kid` first appears (e.g., after a provider's key rotation). |
| D7 | SPI extension — `KeyValueStore.ListNamespaces(prefix string) ([]string, error)` | **Add.** Required for startup-time tenant enumeration in the OIDC registry warmup. Implemented in all three in-tree plugins (memory, sqlite, postgres). Cassandra implementation lands in its own sibling PR (in the `cyoda-go-cassandra` repo) per the coordinated-release procedure. | The v0.8.0 milestone is the SPI release window already (per `project_v0_8_0_milestone_state`: SPI work on `main`, pseudo-version-pinned). A small, justified SPI extension fits the "be ambitious about doing the right thing" direction confirmed during brainstorm. Alternatives (lazy-load on first management-API call; iterate all known KV keys with a prefix filter) trade clarity for an opaque cold-start path and were rejected. |
| D8 | Broadcast failure handling | **Fire-and-forget at the SPI** (`spi.ClusterBroadcaster.Broadcast` is non-blocking). On a broadcaster error or misconfigured broadcaster, the management API returns 500 + ticket UUID per Gate 3. Do **not** extend the SPI with a synchronous `BroadcastSync` ack method. | cyoda-cloud's `notifyAllNodes(awaitResult=true)` is not faithfully portable — the SPI semantics differ deliberately. Per memory `feedback_engine_no_special_rights`, the engine doesn't claim consistency rights existing primitives don't already give. The cyoda-cloud parity test asserting "intercom failure → 500" is rewritten to inject a `Broadcast`-error path. |
| D9 | Startup hook identity | **System user via `spi.WithUserContext(systemCtx, &spi.UserContext{UserID: "system", Tenant: spi.SystemTenantID})`** — same pattern as the existing trusted-key store bootstrap at `app/app.go:217-221`. Registry warmup runs synchronously before HTTP listener start. Discovery/JWKS fetch failures during warmup are WARN-logged but **non-fatal**; the provider becomes usable once its endpoint becomes reachable. Infrastructure failures (KV unavailable) call `os.Exit(1)` per the existing precedent. | Symmetric with `KVTrustedKeyStore` bootstrap; reuse the same idioms. Non-fatal discovery failures match cyoda-cloud's behaviour and avoid an outage cascade when a single upstream IdP is down at startup. |
| D10 | Configuration env vars | **Five.** Four core (mirroring `IAMProperties.OIDCDefaults` from cyoda-cloud): `CYODA_OIDC_REQUIRE_HTTPS` (bool, default `true`), `CYODA_OIDC_CONNECT_TIMEOUT_MS` (int, `5000`), `CYODA_OIDC_SOCKET_TIMEOUT_MS` (int, `5000`), `CYODA_OIDC_CONNECTION_REQUEST_TIMEOUT_MS` (int, `5000`). One SSRF-override: `CYODA_OIDC_ALLOW_PRIVATE_NETWORKS` (bool, default `false`). | Faithful port of cyoda-cloud defaults; SSRF-override is a cyoda-go-specific addition (see D11). |
| D11 | SSRF defence at register-time | **Block** `localhost`, `127.0.0.0/8`, `169.254.0.0/16` (link-local incl. AWS metadata), and RFC1918 ranges (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`) by default. Override via `CYODA_OIDC_ALLOW_PRIVATE_NETWORKS=true` (test/dev). One register-time DNS resolution + IP-range check. | cyoda-cloud relies on its deployment perimeter; cyoda-go-light runs in heterogeneous deployments and the per-tenant management API (D1) gives tenant admins more attack surface than cyoda-cloud's super-user model. Defensive-in-depth alignment with the project's "be ambitious about doing the right thing" direction. The check is `O(1)` per Register and adds one new test. |
| D12 | Parity test count + composition | **32 total.** 27 ported from `OIDCProviderControllerIT.kt` + 5 cyoda-go-specific (D5 wire-shape divergence, cross-tenant 404, tenant-binding via `OwnerLegalEntityID`, SSRF defence, multi-node broadcast-eviction). Registered in `e2e/parity/registry.go` so memory/sqlite/postgres pick them up automatically; cassandra inherits on its next dependency update. | The issue body says 28; cyoda-cloud actually has 27. The 5 additions cover behavioural surfaces cyoda-cloud's system-wide model doesn't have analogues for. |
| D13 | Mock IdP fixture | **`internal/auth/oidc/fixture_test.go`** exposes a `FixtureIdP` type backed by `httptest.Server` with helpers for sign/rotate/revoke. Each parity scenario spins its own instance for isolation; cleanup via `t.Cleanup`. | No existing httptest-IdP precedent in the parity suite. The fixture API is small (5 methods) and lives next to its sole client. |
| D14 | Scope of `#34` (trusted-key audit) inclusion | **Out of scope.** Verified independent during exploration. Stays its own issue. | Per memory `feedback_gate6_no_followups` we resolve in-scope debt without expanding blast radius; #34's items are not in this PR's natural change-footprint. |
| D15 | ADR file under `docs/adr/` | **Add [`0002-federated-identity-provider-architecture.md`](../../adr/0002-federated-identity-provider-architecture.md).** Pins three load-bearing decisions — per-tenant ownership (D1), chained validator composition (D3), KV-backed persistence (D2) — and their consequences. | These three decisions bind future federated-identity work (SAML, dev tokens, social login). They are coupled and worth ADR-level documentation so the "why" survives even if this spec is later archived. The ADR is the durable system of record for the architectural shape; the spec is the implementation specification. |
| D16 | `COMPATIBILITY.md` update | **None in this PR.** SPI pin guidance is updated at v0.8.0 release-prep, not per-issue (per `project_v0_8_0_milestone_state`). | Pseudo-version pin window. |
| D17 | Forward-compatible `ProviderID` field | **Include the field in the persisted struct, set to `"USER_EXTERNAL"` always.** | Avoids a future schema migration when system-defined providers arrive. Two extra lines in the struct; zero validation cost. |
| D18 | Tangential `KeyValueStore.PutBatch` SPI extension | **Flagged in §9 as observed-but-out-of-scope.** Would close the index+blob write-race window on Register. | Affects all four storage plugins; cassandra logged-batch implementation is non-trivial. The microseconds-wide race window in the current design is acceptable for v0.8.0; a follow-up issue can land `PutBatch` if other subsystems also want it. |

## 3. Architecture

### 3.1 Package layout & files touched

| File | Action |
|---|---|
| `internal/auth/errors.go` | **New.** Defines `var ErrUnknownIssuer = errors.New("auth: unknown issuer")`. |
| `internal/auth/chain.go` | **New.** `type Validator interface { Validate(string) (*spi.UserContext, error) }`; `type ChainedValidator struct { ... }`; `NewChainedValidator(...) *ChainedValidator`. |
| `internal/auth/parse.go` | **New.** `parseTokenHeader(tokenString) (kid, alg, iss, aud string, exp int64, err error)` — unauthenticated header+claims peek, used by both `JWKSValidator` and `OIDCValidator` to avoid double-parsing. |
| `internal/auth/validator.go` | Existing single-issuer path swaps its "issuer mismatch" error for the `ErrUnknownIssuer` sentinel. Behaviour-equivalent outside the chain; chain falls through correctly. |
| `internal/auth/oidc/types.go` | **New.** `type OidcProvider struct { ... }` (the persisted struct from §3.3) + error variables (`ErrProviderDuplicate`, `ErrProviderNotFound`, `ErrProviderInactive`, `ErrSSRFBlocked`). |
| `internal/auth/oidc/store.go` | **New.** `type OidcProviderStore interface { ... }` — `Register`, `Get`, `GetByURI`, `Update`, `Delete`, `ListByTenant(activeOnly bool)`, `ListAllTenants()` (the warmup-time enumerator). |
| `internal/auth/oidc/kv_store.go` | **New.** `KVOidcProviderStore` implementing the interface against `spi.KeyValueStore`. Uses `KeyValueStore.ListNamespaces(prefix)` (the D7 SPI extension) in `ListAllTenants`. |
| `internal/auth/oidc/discovery.go` | **New.** `type Discovery interface { Fetch(ctx, uri) (DiscoveryDoc, error) }` and `HTTPDiscovery` impl honouring `RequireHTTPS`, the three timeouts, and the SSRF block list. |
| `internal/auth/oidc/ssrf.go` | **New.** `validateRegisterURI(uri, requireHTTPS, allowPrivate bool) error` — scheme check, DNS resolve, CIDR membership check against the block list. |
| `internal/auth/oidc/jwks_source.go` | **New.** `providerKeySource` — wraps the existing `HTTPJWKSSource` with the provider's discovered `jwks_uri` + lifecycle gate. |
| `internal/auth/oidc/registry.go` | **New.** `Registry` — in-mem cache of active providers per tenant + per-provider key source + `kid`-fast-path index. Subscribes to `oidc.providers` broadcast. Owns the read path. |
| `internal/auth/oidc/validator.go` | **New.** `OIDCValidator` implementing `auth.Validator`. Consults `Registry.ResolveKey(kid, iss)`. Builds `*spi.UserContext` with `Tenant = OwnerLegalEntityID`. |
| `internal/auth/oidc/service.go` | **New.** `Service` — orchestrates the 7 lifecycle ops. Mutates store, mutates registry, broadcasts. Returns domain errors. |
| `internal/auth/oidc/broadcast.go` | **New.** `topicOidcProviders` constant + envelope type + `Service.broadcast(op, tenantID, uri)` helper + the registry's inbound `handleBroadcast(payload)`. |
| `internal/auth/oidc/*_test.go` | **New.** Unit tests for each component; `fixture_test.go` exports the `FixtureIdP`. |
| `internal/domain/account/oidc_adapter.go` | **New.** Seven thin HTTP→`oidc.Service` adapters. Each: authz gate (`auth.RequireAdmin`), input decode + validate, tenant scope, service call, response DTO encode. |
| `internal/domain/account/oidc_adapter_test.go` | **New.** Adapter-level unit tests. |
| `internal/domain/account/handler.go` | Replace lines 95-121: each `h.stub(w, r)` becomes a one-line delegation to the corresponding `oidc_adapter.go` method. Add `oidcService *oidc.Service` field; wire through `NewHandler`. |
| `internal/api/server.go:507-561` | No change — existing nil-check pattern delegates correctly once `Handler.oidcService` is non-nil. |
| `app/app.go` (around line 236, post trusted-key-store bootstrap) | New initialisation block: `kvOidcStore := oidc.NewKVProviderStore(systemCtx, kvStore)`; `discovery := oidc.NewHTTPDiscovery(cfg.IAM.OIDC)`; `oidcRegistry := oidc.NewRegistry(kvOidcStore, discovery, broadcaster, logger)`; `oidcRegistry.LoadAll(systemCtx)`. Pass into `account.NewHandler(...)`. Wire `oidcValidator := oidc.NewValidator(oidcRegistry)` and `chainedValidator := auth.NewChainedValidator(jwksValidator, oidcValidator)`. |
| `app/config.go` | Add `OIDCConfig` struct + `OIDC OIDCConfig` field on `IAMConfig`. Populate from 5 env vars in `DefaultConfig()`. |
| `api/openapi.yaml` | Remove the seven `$ref: "#/components/responses/NotImplemented"` declarations at lines 4806, 4864, 4918, 4970, 5044, 5098, 5160. Reconcile prose: `SUPER_USER` → `ROLE_ADMIN` on the 7 operations (per umbrella §4). |
| `api/generated.go` | Regenerated. |
| `internal/e2e/parity/oidc.go` | **New.** 32 `RunOidc*` scenarios. |
| `e2e/parity/registry.go` | Register the 32 new scenarios in `allTests`. |
| `cmd/cyoda/help/content/config/oidc.md` | **New.** 5 env vars + behaviour notes. |
| `cmd/cyoda/help/content/errors/OIDC_PROVIDER_DUPLICATE.md` | **New.** Error-code help topic. |
| `cmd/cyoda/help/content/errors/OIDC_PROVIDER_NOT_FOUND.md` | **New.** |
| `cmd/cyoda/help/content/errors/OIDC_PROVIDER_INACTIVE.md` | **New.** |
| `cmd/cyoda/help/content/errors/OIDC_SSRF_BLOCKED.md` | **New.** |
| `README.md` | New "OIDC Provider Configuration" subsection in config-reference. |
| `docs/ARCHITECTURE.md` | New §7.3 "OIDC Provider Registry"; rename existing §7.3 → §7.4. |
| `docs/PRD.md` | Expand §8 with OIDC chaining subsection + external-issuer token-claims example. |
| `docs/FEATURES.md` | One bullet in Authentication & Authorization: "OIDC provider per-tenant registry". |
| `CHANGELOG.md` `## [0.8.0]` | `### Added`: OIDC provider per-tenant registry; chained multi-issuer validator; cluster broadcast eviction; 5 env vars; `KeyValueStore.ListNamespaces` SPI extension. |
| **cyoda-go-spi** (sibling repo) | Add `ListNamespaces(prefix string) ([]string, error)` to `KeyValueStore`. Coordinated-release per `MAINTAINING.md:264-295`. |
| **cyoda-go-cassandra** (sibling repo, courtesy PR) | Implement `ListNamespaces` for cassandra. Strictly-in-scope, no drive-by changes (per memory `feedback_courtesy_pr_scope`). |

### 3.2 Component diagram

```
                          ┌─────────────────────────────────┐
                          │   chi router (api.Handler)      │
                          └─────────────────┬───────────────┘
                                            │
                          ┌─────────────────▼───────────────┐
                          │   bearerAuth middleware         │
                          │   (uses ChainedValidator)       │
                          └─────────────────┬───────────────┘
                                            │
              ┌─────────────────────────────┴─────────────────────────────┐
              │                                                           │
              ▼                                                           ▼
   ┌──────────────────────┐                                  ┌───────────────────────────┐
   │ JWKSValidator        │  ErrUnknownIssuer                │ OIDCValidator             │
   │ (first-party, kid    │ ───────────────────────────────► │ (consults Registry)       │
   │  cache per (iss,kid))│                                  └────────────┬──────────────┘
   └──────────────────────┘                                               │
                                                                          ▼
                                                          ┌─────────────────────────────┐
                                                          │ Registry                    │
                                                          │  - providers[T][uri]        │
                                                          │  - sources[T][uri]          │
                                                          │  - kidIndex (fast-path)     │
                                                          └─────┬────────────────┬──────┘
                                                                │                │
                          ┌─────────────────────────────────────▼──┐  ┌──────────▼──────────┐
                          │ KVOidcProviderStore                    │  │ ClusterBroadcaster  │
                          │  oidc:providers:<T>/<uuid>             │  │  topic              │
                          │  oidc:uri-idx:<T>/<sha256(uri)>        │  │   "oidc.providers"  │
                          └────────────────────────────────────────┘  └──────────┬──────────┘
                                                                                 │
                                              ┌──────────────────────────────────┘
                                              │
                          ┌───────────────────▼─────────────────┐
                          │ Service                              │
                          │  (Register/Update/Invalidate/        │
                          │   Reactivate/Delete/Reload[All])     │
                          │  KV mutate → registry mutate →       │
                          │  broadcast                           │
                          └───────────────────┬──────────────────┘
                                              │
                          ┌───────────────────▼──────────────────┐
                          │ HTTPDiscovery                        │
                          │  - SSRF gate (CIDR check)            │
                          │  - .well-known/openid-configuration  │
                          │  - JWKS fetch                        │
                          └──────────────────────────────────────┘
```

### 3.3 Data model

```go
// internal/auth/oidc/types.go
type OidcProvider struct {
    ID                 uuid.UUID  `json:"id"`
    ProviderID         string     `json:"providerId"`         // always "USER_EXTERNAL" in v0.8.0 (D17)
    WellKnownConfigURI string     `json:"wellKnownConfigUri"` // unique per tenant
    Issuers            []string   `json:"issuers,omitempty"`  // nil = accept any iss; max 10
    InvalidatedAt      *time.Time `json:"invalidatedAt,omitempty"`
    CreatedAt          time.Time  `json:"createdAt"`
    OwnerLegalEntityID uuid.UUID  `json:"ownerLegalEntityId"`
}

func (p *OidcProvider) Active() bool { return p.InvalidatedAt == nil }
```

KV layout (per tenant `T`):

```
namespace                  key                            value
oidc:providers:<T>         <provider-uuid>                JSON(OidcProvider)
oidc:uri-idx:<T>           <sha256(wellKnownConfigUri)>   <provider-uuid>
```

Write-race window (Register): the (index, blob) pair is written index-first; on the blob-`Put` failing the index is deleted as best-effort rollback. A crash between the two writes leaves an orphan index entry that is detected and cleaned up on the next `Register` collision check or on the next `LoadAll` warmup. Read-after-write is dominated by the registry's in-memory state for the management API, so this window is observable only in cross-node race conditions which the broadcast eviction protocol corrects within a single gossip cycle.

### 3.4 REST surface

DTOs are already defined in `api/openapi.yaml`:

- `RegisterOidcProviderRequestDto` (lines 8526-8546): `{wellKnownConfigUri, issuers?}`
- `UpdateOidcProviderRequestDto` (lines 9079-9092): `{issuers?}`
- `ReactivateOidcProviderRequestDto` (lines 8547-8554): `{reactivateKeys?}`
- `OidcProviderResponseDto` (lines 8490-8525): `{id, wellKnownConfigUri, issuers?, active, createdAt}`

Wire matrix:

| Method | Path | Handler | Success | Errors |
|---|---|---|---|---|
| POST | `/oauth/oidc/providers` | `RegisterOidcProvider` | 200 `OidcProviderResponseDto` | 400 `OIDC_PROVIDER_DUPLICATE`; 400 `BAD_REQUEST` (validation); 400 `OIDC_SSRF_BLOCKED`; 403 `FORBIDDEN`; 401 `UNAUTHORIZED`; 500 `SERVER_ERROR` (broadcast/KV failure) |
| GET | `/oauth/oidc/providers?activeOnly=<bool>` | `ListOidcProviders` | 200 `[]OidcProviderResponseDto` | 403 / 401 |
| PATCH | `/oauth/oidc/providers/{id}` | `UpdateOidcProvider` | 200 `OidcProviderResponseDto` | 404 `OIDC_PROVIDER_NOT_FOUND`; **409 `OIDC_PROVIDER_INACTIVE`** (D5); 400; 403 / 401 |
| POST | `/oauth/oidc/providers/{id}/invalidate` | `InvalidateOidcProvider` | 200 empty | 404; 403 / 401 (idempotent on already-invalidated: 200 + WARN log) |
| POST | `/oauth/oidc/providers/{id}/reactivate` | `ReactivateOidcProvider` | 200 `OidcProviderResponseDto` | 404; 403 / 401 (idempotent on already-active: 200 + INFO log) |
| DELETE | `/oauth/oidc/providers/{id}` | `DeleteOidcProvider` | 200 empty | 404; 403 / 401 |
| POST | `/oauth/oidc/providers/reload` | `ReloadOidcProviders` | 200 empty | 403 / 401 (explicit in-code ROLE_ADMIN gate per issue body) |

## 4. Runtime mechanics

### 4.1 Registry

```go
type Registry struct {
    mu        sync.RWMutex
    providers map[spi.TenantID]map[string]*OidcProvider // by wellKnownConfigUri
    sources   map[spi.TenantID]map[string]auth.KeySource
    kidIndex  map[string][]providerRef                  // kid -> candidates (D6 fast path)

    store     OidcProviderStore
    discovery Discovery
    broadcast spi.ClusterBroadcaster
    clock     func() time.Time
    logger    *slog.Logger
}

type providerRef struct {
    tenant spi.TenantID
    uri    string
}

type KeyResolution struct {
    PublicKey          *rsa.PublicKey
    OwnerLegalEntityID uuid.UUID
    WellKnownConfigURI string
}

func (r *Registry) ResolveKey(kid, iss string) (*KeyResolution, error)
// Hot path: kidIndex lookup -> candidates filtered by iss (if provider.Issuers set) ->
// source.GetKey(kid) on each; first to resolve wins; non-resolvers WARN-logged (kid + uri
// only, never token).
// Cold path: kidIndex miss -> global iteration -> populates kidIndex on resolution.
// Returns auth.ErrUnknownIssuer on no match.
```

The kid-fast-path covers the steady state. The miss path covers post-rotation tokens where the new `kid` is not yet indexed; the resolution side-effect populates the index, so the next token with the same `kid` hits the fast path.

### 4.2 Broadcast

```go
const topicOidcProviders = "oidc.providers"

type broadcastEnvelope struct {
    Op       string `json:"op"`   // "reload" | "invalidate" | "reload_all"
    TenantID string `json:"t,omitempty"`
    URI      string `json:"u,omitempty"`
}

func (r *Registry) handleBroadcast(payload []byte) {
    var env broadcastEnvelope
    if err := json.Unmarshal(payload, &env); err != nil { return }
    // run under systemCtx; idempotent; dispatch long work to a worker goroutine
    switch env.Op {
    case "reload":      go r.reloadOne(spi.TenantID(env.TenantID), env.URI)
    case "invalidate":  r.invalidateOne(spi.TenantID(env.TenantID), env.URI)
    case "reload_all":  go r.reloadAll()
    }
}
```

Handler invariants: (a) idempotent, (b) never panics (panic-recover at the top), (c) returns quickly so the broadcaster goroutine isn't blocked, (d) all KV access uses `systemCtx`.

### 4.3 Validator chain

```go
// internal/auth/chain.go
type Validator interface {
    Validate(tokenString string) (*spi.UserContext, error)
}

type ChainedValidator struct {
    validators []Validator
}

func (c *ChainedValidator) Validate(tokenString string) (*spi.UserContext, error) {
    for _, v := range c.validators {
        uc, err := v.Validate(tokenString)
        if err == nil {
            return uc, nil
        }
        if !errors.Is(err, ErrUnknownIssuer) {
            return nil, err   // expired/bad-sig/etc. — hard-fail, do not fall through
        }
    }
    return nil, ErrUnknownIssuer
}
```

`OIDCValidator.Validate`:

1. `parseTokenHeader(tokenString)` → `kid, alg, iss, aud, exp, err`. Algorithm not in allow-list → hard-fail with `auth.ErrUnsupportedAlg`. `exp` already past → hard-fail with `auth.ErrTokenExpired`.
2. `r.ResolveKey(kid, iss)` → `(*KeyResolution, error)`. `ErrUnknownIssuer` → return it; the chain falls through (in practice this is also the terminal validator, so it surfaces as 401).
3. Full verify against the resolved public key, standard claims (`exp`, `nbf`, `aud`).
4. Build `*spi.UserContext` with `Tenant.ID = resolution.OwnerLegalEntityID.String()`, roles populated from the token claims per the existing convention.

## 5. Lifecycle behaviour (per operation)

Skeleton for every mutating op: validate input → ROLE_ADMIN authz → tenancy guard → store mutation → registry mutation → broadcast → respond. Store mutation precedes registry mutation; broadcast is best-effort after.

### 5.1 Register

1. `auth.RequireAdmin(w, r)`.
2. Decode request, validate: `wellKnownConfigUri` non-empty, ≤1000 chars, valid absolute URL, scheme matches `CYODA_OIDC_REQUIRE_HTTPS`. `Issuers` if present: ≤10, each ≤1000 chars, deduplicated. SSRF gate (D11).
3. `tenantID := tenantFromCtx(r)`.
4. KV: `Get(oidc:uri-idx:<T>, sha256(uri))`. If present → 400 `OIDC_PROVIDER_DUPLICATE`.
5. KV: `Put` index, then `Put` provider blob. On second `Put` failing: `Delete` index (best-effort rollback) and return 500.
6. `registry.reloadOne(tenantID, uri)` — discovery + JWKS fetch. Failures WARN-logged; the provider is registered in KV regardless and becomes usable when its endpoint is reachable.
7. `broadcast({op: "reload", t: tenantID, u: uri})`.
8. Respond 200 with `OidcProviderResponseDto`.

### 5.2 Update issuers

1. `auth.RequireAdmin`.
2. KV `Get` by `(tenantID, providerID)`. Missing → 404 `OIDC_PROVIDER_NOT_FOUND`.
3. If `InvalidatedAt != nil` → **409 `OIDC_PROVIDER_INACTIVE`** (D5).
4. Validate new `Issuers` list. Empty list with the field present is treated as "clear to nil = accept any" (matches cyoda-cloud test #21).
5. KV `Put` updated blob.
6. `registry.reloadOne(tenantID, uri)` with **JWKS cache flush** — drop the in-memory key cache for this provider so the next token forces a re-fetch.
7. Broadcast reload.
8. Respond 200 with DTO.

### 5.3 Invalidate

1. `auth.RequireAdmin`.
2. KV `Get`. Missing → 404.
3. Already invalidated: WARN log, 200 idempotent, no broadcast.
4. KV `Put` with `InvalidatedAt = time.Now()`.
5. Registry: drop the provider entry + its KeySource. Update `kidIndex` to remove candidates pointing at this provider.
6. `broadcast({op: "invalidate", t: tenantID, u: uri})`.
7. Respond 200 empty.

### 5.4 Reactivate

1. `auth.RequireAdmin`.
2. KV `Get`. Missing → 404.
3. Already active: INFO log, 200 with current DTO, no broadcast.
4. KV `Put` with `InvalidatedAt = nil`.
5. `registry.reloadOne(tenantID, uri)`. Request's `reactivateKeys=true` (default) → full reload including JWKS re-fetch + sync (drop locally cached keys not in remote JWKS). `reactivateKeys=false` → reload discovery only, leave the JWKS cache as-is. (Per brainstorm closure: in the KV-backed world there are no separately persisted JWK records to "reactivate", so the body field's only observable effect is on the in-memory cache sync behaviour.)
6. Broadcast reload.
7. Respond 200 with DTO.

### 5.5 Delete

1. `auth.RequireAdmin`.
2. KV `Get`. Missing → 404.
3. If active, perform invalidate semantics inline (same KV + registry steps, no separate broadcast).
4. KV: `Delete` provider blob AND URI-index entry.
5. Registry: drop the provider entry, update `kidIndex`.
6. `broadcast({op: "invalidate", t: tenantID, u: uri})` — receivers drop their cached entry; on next list/reload it just isn't there.
7. Respond 200 empty.

### 5.6 Reload-all

1. **Explicit in-code ROLE_ADMIN check** (not delegated to entity-ACL, per issue body — no entity is being mutated, only the in-memory cache state).
2. `store.ListAllTenants()` → for each tenant, `store.ListByTenant(activeOnly=true)`.
3. Build fresh `providers`, `sources`, `kidIndex`; swap under `mu.Lock()`.
4. `broadcast({op: "reload_all"})`.
5. Respond 200 empty.

### 5.7 List

1. `auth.RequireAdmin`. (cyoda-cloud allows non-admins to read for their tenant; we err conservative pending a multi-role audit. Spec note flags this for the role-model audit follow-up.)
2. `store.ListByTenant(tenantID, activeOnly)`.
3. Map to DTOs.
4. Respond 200.

## 6. Startup hook

In `app/app.go`, after the `KVTrustedKeyStore` bootstrap (around line 236):

```go
oidcStore, err := oidc.NewKVProviderStore(systemCtx, kvStore)
if err != nil {
    slog.Error("startup failure", "phase", "oidc-store-bootstrap", "error", err.Error())
    os.Exit(1)
}

oidcDiscovery := oidc.NewHTTPDiscovery(cfg.IAM.OIDC)

oidcRegistry := oidc.NewRegistry(oidcStore, oidcDiscovery, broadcaster, logger)
if err := oidcRegistry.LoadAll(systemCtx); err != nil {
    slog.Error("startup failure", "phase", "oidc-registry-warmup", "error", err.Error())
    os.Exit(1)
}
```

`LoadAll` uses the new `KeyValueStore.ListNamespaces("oidc:providers:")` SPI op (D7) to enumerate tenants. Per-tenant discovery/JWKS fetch failures are WARN-logged but non-fatal — startup proceeds; providers become usable when their endpoints are reachable.

`oidcValidator := oidc.NewValidator(oidcRegistry)` is constructed and chained: `chained := auth.NewChainedValidator(jwksValidator, oidcValidator)`. The two existing call sites taking `*auth.JWKSValidator` are migrated to take `auth.Validator` (the new interface): HTTP auth middleware and the gRPC server interceptor. Each is a one-line change.

## 7. Configuration

| Env var | Type | Default | Purpose |
|---|---|---|---|
| `CYODA_OIDC_REQUIRE_HTTPS` | bool | `true` | Reject `http://` `wellKnownConfigUri` at Register-time. |
| `CYODA_OIDC_CONNECT_TIMEOUT_MS` | int | `5000` | TCP connect timeout for discovery + JWKS HTTP requests. |
| `CYODA_OIDC_SOCKET_TIMEOUT_MS` | int | `5000` | HTTP read timeout for the same requests (`http.Transport.ResponseHeaderTimeout` + per-request deadline). |
| `CYODA_OIDC_CONNECTION_REQUEST_TIMEOUT_MS` | int | `5000` | Pool-checkout timeout. |
| `CYODA_OIDC_ALLOW_PRIVATE_NETWORKS` | bool | `false` | Test/dev override: disable the SSRF block list (D11). |

Wired into `app/config.go::IAMConfig.OIDC` (new `OIDCConfig` struct), populated in `DefaultConfig()`. Help topic at `cmd/cyoda/help/content/config/oidc.md` (new). README config-reference section updated.

## 8. Security

| Aspect | Mechanism | Verification |
|---|---|---|
| Authn | Existing bearerAuth middleware — no new wiring | All 7 endpoints already declare `security: [bearerAuth: []]` |
| Authz (mutating + read) | `auth.RequireAdmin(w, r)` on each handler; explicit in-code ROLE_ADMIN check on `Reload` (per issue body) | 6 negative tests (non-admin → 403) |
| Tenant isolation (mgmt API) | KV namespace per tenant; cross-tenant ID lookup returns 404 (no existence leakage) | cyoda-go-specific test: tenant-A's provider ID returns 404 to tenant-B's ROLE_ADMIN |
| Tenant isolation (token validation) | `UserContext.Tenant = OwnerLegalEntityID` from the resolving provider; tenant claims in the token are ignored | cyoda-go-specific test: token signed by tenant-A's IdP yields a UserContext with Tenant = tenant-A even if the JWT contains a different `tid` claim |
| SSRF defence | DNS-resolve at Register, block loopback / link-local / RFC1918 unless `CYODA_OIDC_ALLOW_PRIVATE_NETWORKS=true` | cyoda-go-specific test: `http://169.254.169.254/.well-known/openid-configuration` → 400 |
| Log hygiene (Gate 3) | Never log tokens, JWT contents, private keys, JWKS payloads. **OK**: `kid`, `wellKnownConfigUri`, `iss` claim, tenantID, provider UUID. | Pre-PR grep: `rg -n "tokenString\|Bearer\|RawToken" internal/auth/oidc/` returns no `slog` calls with those identifiers |
| Output sanitization | 4xx with domain code + short message; 5xx with generic message + ticket UUID; never echo upstream JWKS fetch error bodies | Test asserts a JWKS-fetch failure response contains only the ticket UUID, not the upstream error string |
| Race-safety | All registry reads under `mu.RLock()`; all mutations under `mu.Lock()`. Broadcast handlers idempotent + panic-recover at top. | Race detector run pre-PR per `race-testing.md` |

## 9. Out of scope

- **#34 trusted-key audit findings** — independent (verified during exploration). Stays its own issue.
- **`KeyValueStore.PutBatch` SPI extension** — would close the index+blob write-race window on Register (microseconds-wide today). Tangential to #284; affects all four storage plugins; cassandra logged-batch implementation non-trivial. Surface as its own follow-up issue if/when other subsystems also want it. (Per memory `feedback_gate6_no_followups`: this is the rare "stop and surface the choice" case — bounded-but-cross-cutting, not in the natural change-footprint of #284.)
- **Dynamic client registration (OIDC DCR)** — provider registration only, not client registration.
- **OIDC RP / login flow / federated logout / back-channel logout** — we accept tokens, we don't act as an OIDC client.
- **Multi-cloud deploy IaC** — moved to `cyoda-go-terraform` per memory `project_multicloud_deploy_rework`. Not touched here.
- **`ProviderID` semantics beyond `USER_EXTERNAL`** — field is forward-compat; only one value used in v0.8.0 (D17).
- **List-by-non-admin** — cyoda-cloud allows non-admin tenant-scoped reads; we err conservative pending a wider role-model audit. Recorded as a follow-up role-audit consideration, not a PR-time change.

## 10. Acceptance criteria mapping

| Issue body item | Where covered |
|---|---|
| Brainstorm + spec under `docs/superpowers/specs/` | This document |
| Plan under `docs/superpowers/plans/` | Next step (writing-plans skill after spec approval) |
| All 7 handlers return real responses | §3.1 + §5 |
| All 28 → **27** ported test scenarios pass across memory/sqlite/postgres/cassandra | §3.1 + §11 (D12 records the count correction) |
| `JWKSValidator` extended multi-issuer with per-provider lifecycle | §4.3 (chained, not in-place extended — D3 records the divergence-with-rationale) |
| `CYODA_OIDC_*` env vars documented in `cmd/cyoda/help/content/config/` | §7 |
| Cross-node cache invalidation verified via multi-node E2E harness | §11 "broadcast-eviction" scenario |
| No credentials / tokens / private keys in any log line | §8 + pre-PR grep gate |
| 501 declarations removed from OpenAPI for these 7 paths | §3.1 (`api/openapi.yaml` row) |
| `h.stub(w, r)` calls removed for OIDC handlers | §3.1 (`internal/domain/account/handler.go` row) + §5 |
| Decomposition spec §3.4 updated to v0.8.0 | **Already done** in tree (amendment 2026-06-16 at head of `2026-06-04-194-decomposition-design.md`); §1 of this spec records the satisfaction |

## 11. Test plan

32 scenarios total. All run as parity tests (`internal/e2e/parity/oidc.go`) registered in `e2e/parity/registry.go::allTests`. Each parity backend (memory, sqlite, postgres, cassandra) picks them up via its existing wrapper.

| Family | # | Source |
|---|---|---|
| CRUD happy-path | 5 | port: register, list-all/active-only, update, invalidate, reactivate-no-keys, delete |
| CRUD negative (404 + duplicate) | 4 | port |
| Authz negative (non-admin → 403) | 6 | port: register, update, invalidate, reactivate, delete, reload |
| Cluster intercom failure → 500 | 1 | port with D8 rewrite (broadcaster-error path, not ack-failure) |
| JWT validation integration | 4 | port: register-and-validate, invalidate-and-reject, reactivate-and-revalidate, delete-and-permanent-reject |
| Issuer-list update affects validation | 1 | port: cycle issuers list, assert JWT acceptance follows |
| Key rotation / revocation | 5 | port: `validTo` past/future/null, invalidated key, remote-removal sync |
| Multi-provider isolation | 1 | port: two providers, keys don't cross-validate |
| **Inactive-update → 409 + `OIDC_PROVIDER_INACTIVE`** | 1 | cyoda-go-specific (D5) |
| **Cross-tenant 404** | 1 | cyoda-go-specific (D1 isolation) |
| **Tenant binding via `OwnerLegalEntityID`** | 1 | cyoda-go-specific (D1) |
| **SSRF defence on Register** | 1 | cyoda-go-specific (D11) |
| **Cross-node broadcast eviction** | 1 | cyoda-go-specific; multi-node harness (memberlist) — acceptance criterion item |
| **Total** | **32** | |

Fixture IdP shape (§3.1 references):

```go
// internal/auth/oidc/fixture_test.go
type FixtureIdP struct {
    Server   *httptest.Server
    Issuer   string
    JWKSURI  string
    // internal: signing keys, revocation set
}

func NewFixtureIdP(t *testing.T) *FixtureIdP
func (f *FixtureIdP) SignJWT(t *testing.T, kid string, claims jwt.Claims) string
func (f *FixtureIdP) RotateKey(t *testing.T) (newKID string)
func (f *FixtureIdP) RevokeKey(t *testing.T, kid string)
```

Each scenario spins its own `FixtureIdP` for isolation; `t.Cleanup` shuts down the server.

Multi-node broadcast-eviction scenario: two in-process cyoda-go instances over a real `gossip_broadcast.Gossip` ring, sharing a memory-plugin KV store. Register on node A, observe node B's `Registry.providers` map gains the entry within one gossip cycle; invalidate on A, observe B drops it within one cycle. Asserts the broadcast contract end-to-end without depending on JWT validation correctness.

## 12. Documentation updates (Gate 4)

| File | Action |
|---|---|
| `cmd/cyoda/help/content/config/oidc.md` | **New** — 5 env vars + behaviour notes |
| `cmd/cyoda/help/content/errors/OIDC_PROVIDER_DUPLICATE.md` | **New** |
| `cmd/cyoda/help/content/errors/OIDC_PROVIDER_NOT_FOUND.md` | **New** |
| `cmd/cyoda/help/content/errors/OIDC_PROVIDER_INACTIVE.md` | **New** |
| `cmd/cyoda/help/content/errors/OIDC_SSRF_BLOCKED.md` | **New** |
| `app/config.go` `DefaultConfig()` | Populate `IAMConfig.OIDC` from 5 env vars |
| `README.md` | New "OIDC Provider Configuration" subsection under config-reference |
| `docs/ARCHITECTURE.md` | New §7.3 "OIDC Provider Registry"; existing §7.3 → §7.4 |
| `docs/PRD.md` | Expand §8 with OIDC chaining subsection + external-issuer token-claims example |
| `docs/FEATURES.md` | One bullet under Authentication & Authorization: "OIDC provider per-tenant registry" |
| `CHANGELOG.md` `## [0.8.0]` | `### Added`: OIDC subsystem; 5 env vars; `KeyValueStore.ListNamespaces` SPI extension. (Per project memory: refer to the SPI extension by description, not issue ID.) |
| `COMPATIBILITY.md` | No change in this PR — SPI release-prep handles it (per `project_v0_8_0_milestone_state`) |
| `docs/adr/0002-federated-identity-provider-architecture.md` | **New** — pins D1 (per-tenant ownership), D3 (chained validator), D2 (KV-backed persistence) and their consequences |

## 13. Coordinated-release notes

Per `MAINTAINING.md:264-295` the dependency chain is `cyoda-go-spi ← cyoda-go ← cyoda-go-cassandra`. This PR's SPI extension (`KeyValueStore.ListNamespaces`) follows the v0.8.0 milestone's pseudo-version pin pattern (per memory `project_v0_8_0_milestone_state`): land on SPI `main`, cyoda-go pseudo-version-pins across all four `go.mod` files, tag at v0.8.0 release-prep. A courtesy PR to `cyoda-go-cassandra` implements `ListNamespaces` for cassandra — strictly in-scope (per memory `feedback_courtesy_pr_scope`), no drive-by fixes.

The PR landing on `release/v0.8.0` must milestone this issue (per memory `feedback_release_milestone_invariant`) so it appears in the milestone-derived changelog.
