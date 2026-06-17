# Issue #284 — OIDC providers subsystem (greenfield)

**Status:** approved (rev. 4, post third-review)
**Date:** 2026-06-16
**Issue:** [#284](https://github.com/Cyoda-platform/cyoda-go/issues/284)
**Umbrella:** [`2026-06-04-194-decomposition-design.md`](2026-06-04-194-decomposition-design.md) §3.4 (amended 2026-06-16, pulled into v0.8.0)
**Predecessor:** issue #123 (closed as duplicate; content consolidated into #284)

**Revision history:**

- **rev. 1** — initial design.
- **rev. 2** — first fresh-context review: dropped a misguided SPI extension, introduced four error sentinels, added `iat`-binding, mandatory `iss` validation, two-phase warmup, fetch-time SSRF, explicit register-race semantics, per-provider audience, observability, admin chain-introspection.
- **rev. 3** — second fresh-context review: dropped broadcast-failure handling entirely (mechanically impossible against the SPI contract); specified OIDC `UserContext` extraction with namespaced `UserID` + configurable roles claim; specified `map[string]json.RawMessage` decode for tri-state PATCH; honest scope for `iat`-binding (closes accidental, not adversarial); fixed cold-start contradiction; dropped per-tenant Prometheus label (cardinality footgun); fixed OpenAPI prose contradicting D17; re-keyed broadcast single-flight by `(tenantID, uri)`; specified 30s `iat` skew; dropped the rev. 2 admin chain-introspection endpoint as forward-engineering with no v0.8.0 consumer.
- **rev. 4** — third fresh-context review: (C1) made the `kidIndex` populate-at-resolution-time invariant explicit — self-heal is the *primary* correctness mechanism, not a safety net; (C2) reload_all now acquires the registry write lock around its rebuild so it actually serializes with concurrent `reload(T, uri)`; (I1) `iss` comparison is bytewise per OIDC Core 1.0 §2; (I2) bounded `sub` claim (≤255 chars, no ASCII control chars); `UserContext.UserID` is opaque (no downstream parsing); (I3+I4) replaced the in-memory per-node `previousOwners` cache with a KV-backed cross-tenant `_history:<sha256>` ownership-history entry — cluster-consistent, restart-survivable; (I5) renamed config field to `DefaultRolesClaim` for consistency; (I6) corrected scenario count throughout the spec; (I7) chain order `(JWKSValidator, OIDCValidator)` is now normative; (I8) split the concurrent-Register test into a parity-safe deterministic path and a cyoda-go-specific fault-injection race path; (I9) added scenario + behaviour for broadcast arriving at a node that does not have the (T, uri) in its registry; (I10) split the reactivate-with-keys row into success-with-remote-removal-sync and idempotency; (W1) softened the ADR plugin-parity claim; (W2) inlined the audit emitter into `service.go`; (W3) added `OIDC_CLAIMS_INVALID` parent code for claims-validation subcodes (9 error codes total); (W4) **pushback** — `internal/auth/http_jwks_source.go:93-121` refetches unconditionally on cache miss, so the "self-heal stalls validation until source TTL expires" gotcha doesn't fire; one-line clarifying note added in §4.1 rather than a structural change; (W5) §10 explicitly notes the cassandra-row acceptance gate is verified in the sibling repo's PR. Decision-table preserves stable D1/D2/D3 labels (ADR cross-references); other D-numbers preserved across rev. 3 → rev. 4.

**Scope:** Land a per-tenant OIDC provider registry, the seven REST handlers currently stubbed at HTTP 501 (`internal/domain/account/handler.go:95-121`), a chained multi-issuer JWT validator with four distinct error sentinels, single-topic cluster broadcast for cross-node cache eviction, six `CYODA_OIDC_*` configuration env vars, telemetry, and a 64-scenario parity test suite (§11 is authoritative). Zero changes to any SPI surface. Zero `h.stub` calls remain on the OIDC path; the seven `NotImplemented` declarations come off `api/openapi.yaml`.

---

## 1. Context

cyoda-go exposes the full OpenAPI surface for OIDC provider management at `/oauth/oidc/providers/*` (`api/openapi.yaml:4762-5160`), but every handler is a stub returning `501 NOT_IMPLEMENTED` (`internal/domain/account/handler.go:95-121`, routed via `internal/api/server.go:507-561`). The reference implementation lives in cyoda-cloud:

- `platform-service-iam/src/main/kotlin/com/cyoda/iam/model/entity/JWKOIDCEntity.kt`
- `platform-service-iam/src/main/kotlin/com/cyoda/iam/service/oidc/JWKOIDCService.kt` (`JWKOIDCServiceImpl`)
- `backend/src/main/kotlin/net/cyoda/saas/controller/OIDCProviderController.kt`
- `backend/src/main/kotlin/net/cyoda/saas/iam/OIDCProviderInteractor.kt`
- `platform-service-iam/src/main/kotlin/com/cyoda/iam/service/oidc/notifications/JWKOIDCCacheIntercom.kt`
- `integration-tests/src/test/kotlin/net/cyoda/saas/controller/OIDCProviderControllerIT.kt` (27 tests)

The umbrella decomposition (§3.4) originally deferred this to v0.9.0; the amendment at the head of that document (2026-06-16) pulls it into v0.8.0. That acceptance-criterion item is already satisfied in tree; this spec records the satisfaction.

Today's `JWKSValidator` (`internal/auth/validator.go:15-101`) is single-issuer. **Order of validation matters:** today the signature is verified at line 73 BEFORE the iss-check at line 82. Any change that swaps the iss-mismatch error for a fall-through sentinel must distinguish "kid unknown to me" from "kid known + signature OK + iss mismatch" — these are different cases with different correct hard/soft-fail semantics. D3 honours this.

The `KVTrustedKeyStore` (`internal/auth/kv_trusted_store.go:20, 49-51, 73-141`) is the load-bearing precedent: one namespace `trusted-keys`, composite keys `<tenantID>:<kid>`, runs under the system tenant context (`app/app.go:217-222`), reads back all rows with a single `kv.List("trusted-keys")` at warmup. The SPI's `KeyValueStore` filters every op by tenantID at acquisition (`plugins/postgres/kv_store.go:18-23, 30-42, 44-52, 54-76`); there is no cross-tenant enumeration primitive and there should not be. Single-namespace + composite-keys, with **the system-tenant partition holding the entire OIDC registry**, is the only correct shape.

The SPI's `ClusterBroadcaster.Broadcast(topic, payload)` returns nothing (`cyoda-go-spi/cluster.go:16-19`; `internal/cluster/registry/gossip_broadcast.go:38-44`). It is fire-and-forget by contract. **The management API does not surface broadcast failure** because there is no broadcast failure to surface. cyoda-cloud's `notifyAllNodes(awaitResult=true)` ack semantics are not portable; the related cyoda-cloud test does not port (see §9 divergence register).

The `internal/auth/http_jwks_source.go` source's `GetKey(kid)` (lines 93-121) **refetches the JWKS endpoint unconditionally** when the kid is missing from cache, regardless of cache-TTL staleness. This is load-bearing for D6 self-heal: after evicting a stale providerRef from `kidIndex`, the next cold-path resolution triggers a fresh fetch via the source rather than serving stale data. No additional refetch-trigger machinery is needed.

Per the umbrella's §4 reconciliation, mutating endpoints require `ROLE_ADMIN`; `List` is open to any authenticated member of the owning tenant (D21). OpenAPI prose currently says `SUPER_USER`; this PR aligns prose to `ROLE_ADMIN` on the 6 mutating operations.

Issue #34 (trusted-key audit) is independent.

## 2. Decisions

| # | Decision | Choice | Rationale |
|---|---|---|---|
| D1 | Tenancy scope of the OIDC provider registry | **Per-tenant.** Each legal entity owns its providers; management API filters by `tenantFromCtx(r)`; KV records keyed by `(tenantID, ...)`. Token validation iterates providers across all tenants; the resolving provider's `OwnerLegalEntityID` determines the user context's tenant — subject to the cross-tenant escalation guards in D17. | cyoda-go-light deploys into more varied contexts than cyoda-cloud where per-tenant ownership is the safer default. Validation-side global iteration is unavoidable in any case (tokens carry no tenant claim). |
| D2 | Persistence backend | **`KeyValueStore` SPI, single namespace `oidc-providers`, composite keys.** Three key shapes: (a) provider blob `<tenantID>:<provider-uuid>`; (b) per-tenant URI uniqueness index `<tenantID>:uri:<sha256(wellKnownConfigUri)>`; (c) cross-tenant URI ownership history `_history:<sha256(wellKnownConfigUri)>`. Store acquired with system-tenant context; the entire OIDC registry lives in the system tenant's partition. `LoadAll` enumerates via `kv.List("oidc-providers")` and parses tenantID out of each key. **No SPI extension.** The leading `_` on the history key disambiguates from tenant-UUID-prefixed keys (UUIDs are hex+dashes, never start with `_`). | The trusted-key precedent (`internal/auth/kv_trusted_store.go`) demonstrates the system-tenant + composite-key pattern. The KV SPI is already tenant-scoped at acquisition; per-tenant namespaces would either return empty (honest impl) or require a deliberate isolation bypass. The cross-tenant `_history` key gives D25 its restart-survivable, cluster-consistent audit signal without leaving the system-tenant partition. |
| D3 | Multi-issuer validator integration with four distinct error sentinels | **Chained validators.** New `auth.Validator` interface, new `auth.ChainedValidator`. Four sentinels: `ErrUnknownKID` (chain fall-through), `ErrIssuerMismatch` (hard-fail, kid was resolved but `iss` does not match), `ErrSignatureFailure` (hard-fail), `ErrClaimsFailure` (hard-fail). The chain only falls through on `ErrUnknownKID`. Plus `ErrTokenPreTransition` (D17) and `ErrJWKSUnavailable` (resolution-transient). **Chain order is normatively `(JWKSValidator, OIDCValidator)`** — verified by §11 row 36 (first-party `kid` + foreign `iss` reaches `JWKSValidator` first and hard-fails with `ErrIssuerMismatch` without consulting `OIDCValidator`). | Today `JWKSValidator.Validate` verifies signature before iss-check. Existing behaviour is already correct (hard-fail on iss-mismatch); the chain pattern preserves it via the typed sentinel. Single-sentinel rev. 1 design would have *introduced* an escalation path the existing code does not have. Order matters: putting OIDCValidator first would not be a correctness bug under the four-sentinel rule but would change row 36's wire semantics (which validator-counter increments). |
| D4 | Cluster broadcast wire format | **One topic `oidc.providers`** with envelope `{op: "reload"\|"invalidate"\|"reload_all", tenantID, wellKnownConfigUri?}`. Mirrors `JWKOIDCCacheNotification` and `modelcache`'s single-topic convention. | One Subscribe, one switch. Receiver handlers idempotent. |
| D5 | Wire shape for "update inactive provider" | **`HTTP 409 Conflict` + error code `OIDC_PROVIDER_INACTIVE`**. Diverges from cyoda-cloud's `IllegalStateException → 5xx`. | Matches cyoda-go's 4xx-domain-detail convention (`.claude/rules/error-handling.md`). 5xx generic would violate Gate 3 output sanitization. Recorded in §9 divergence register. |
| D6 | Validator key-resolution with primary-correctness self-heal | **`kid`-indexed fast-path + global-iteration cold path. `kidIndex` is populated at resolution time, BEFORE signature verification.** The caller (OIDCValidator) MUST invoke `EvictKidEntry(kid, ref)` on `ErrSignatureFailure`. Both halves of the contract are load-bearing — self-heal is not a safety net layered on top of a primary mechanism; it IS the primary correctness mechanism for the cross-tenant `kid`-collision case. | `O(active providers)` per token is unacceptable on the bearer-auth hot path. The cold path cannot know which providerRef will verify; it has to populate optimistically and rely on the caller's verify-and-evict loop to converge. This contract is explicit because misunderstanding either half (failing to populate at resolve, or failing to evict at verify-fail) produces wrong behaviour. The source's `GetKey` already refetches the JWKS unconditionally on cache miss (`http_jwks_source.go:93-121`), so post-eviction recovery does not wait on the source's TTL. |
| D7 | Broadcast failure handling | **None at the SPI level — `Broadcast` is fire-and-forget by contract.** The management API does not check or surface broadcast-delivery state. Misconfiguration (`broadcaster == nil`) is a startup invariant: `app/app.go` validates non-nil and `os.Exit(1)` if missing. | rev. 2 specified an "if Broadcast fails" path that is mechanically impossible. cyoda-cloud's "intercom failure → 500" parity test does not port (§9 DV2). |
| D8 | Two-phase startup warmup | **Phase 1 (synchronous, blocks listener):** `kv.List("oidc-providers")` → parse → populate `Registry.providers` and (empty) `kidIndex`. **Phase 2 (asynchronous, post-listener):** per-provider goroutine fetches discovery doc + JWKS, populates `Registry.sources` and `kidIndex` for that provider. Per-provider failures: WARN-log, continue. **Tokens arriving during the Phase-2-pending window** for a provider whose `DiscoveryDoc` has not yet been fetched cause `ResolveKey` to return `ErrUnknownKID` (chain fall-through → 401). | rev. 2 had a contradiction; rev. 3 + 4 keep this resolution: missing discovery → `ErrUnknownKID` (cold-start window is a documented characteristic). |
| D9 | Configuration env vars | **Six.** `CYODA_OIDC_REQUIRE_HTTPS` (bool, `true`), `CYODA_OIDC_CONNECT_TIMEOUT_MS` (int, `5000`), `CYODA_OIDC_SOCKET_TIMEOUT_MS` (int, `5000`), `CYODA_OIDC_CONNECTION_REQUEST_TIMEOUT_MS` (int, `5000`), `CYODA_OIDC_ALLOW_PRIVATE_NETWORKS` (bool, `false`), `CYODA_OIDC_ROLES_CLAIM` (string, `"roles"` — maps to config field `DefaultRolesClaim`; per-provider `RolesClaim` overrides). | Four core mirror cyoda-cloud `IAMProperties.OIDCDefaults`. SSRF override is cyoda-go-specific (D10). Roles-claim default needed because external IdPs vary (Auth0, Cognito `cognito:groups`, Keycloak `realm_access.roles`). |
| D10 | SSRF defence — fetch-time, not just register-time | Custom `http.Transport.DialContext` re-checks the resolved IP on every dial. Blocklist: IPv4 loopback / link-local / RFC1918; IPv6 loopback / link-local / ULA / IPv4-mapped. Redirects disabled. Override via `CYODA_OIDC_ALLOW_PRIVATE_NETWORKS=true`. | rev. 1's register-only check is DNS-rebind vulnerable. Per-dial closes that. |
| D11 | Register write-race semantics | **Accept the race; resolve correctness on read.** Write index then blob; **re-read index after write** — if value ≠ our `<provider-uuid>` we lost, `Delete` our blob, return 409 `OIDC_PROVIDER_DUPLICATE`. **Stale-index defence on every read:** `Get(tenantID, providerID)` and `GetByURI(tenantID, uri)` verify the blob exists AND its `OwnerLegalEntityID` matches the index-key tenant prefix. Mismatch → orphan; best-effort `Delete`; return `ErrProviderNotFound`. | The actual window is "however long the slower node takes between read and second write." Read-time validation is the durable correctness guard. |
| D12 | Mock IdP fixture | `internal/auth/oidc/fixture_test.go` exposes `FixtureIdP` with `httptest.Server` and helpers for sign/rotate/revoke/fake-DNS. Per-scenario isolation; `t.Cleanup`. | No existing httptest-IdP precedent. |
| D13 | Scope of #34 (trusted-key audit) inclusion | **Out of scope.** Independent. | Per `feedback_gate6_no_followups`. |
| D14 | ADR file | Add `0002-federated-identity-provider-architecture.md`. Pins D1, D2, D3. | Durable architectural record vs per-issue implementation contract. |
| D15 | `COMPATIBILITY.md` update | **None.** No SPI changes. | |
| D16 | cyoda-go-spi / cyoda-go-cassandra changes | **None. No coordinated release.** | |
| D17 | Cross-tenant token-routing escalation guards — honest scope, with bytewise `iss` comparison | **`iat`-binding (with 30s clock skew) + mandatory `iss` validation (bytewise).** (a) Reject tokens where `iat < provider.CreatedAt - 30s` (`ErrTokenPreTransition`, hard-fail). The 30s window matches `ValidateClaims`'s existing skew (`internal/auth/validator.go:77`). (b) `iss` validation is mandatory at resolution: if `provider.Issuers` is non-empty, require `iss ∈ Issuers`; if `Issuers` is empty AND the provider's discovery doc has been fetched, require `iss == DiscoveryDoc.Issuer`. **Comparison is strict bytewise equality** per OIDC Core 1.0 §2 — no URL normalization, no trailing-slash tolerance. If the discovery doc has NOT been fetched (Phase-2 cold start), return `ErrUnknownKID` (chain fall-through). **Scope claim:** these guards close the *accidental* cross-tenant spillover case. They do **not** close the *adversarial* case (the IdP operator mints a fresh token with `iat = now` after observing the handover). Mitigation: D25 INFO audit on Register-after-foreign-Delete; operational vigilance. | Bytewise per OIDC Core 1.0; rev. 4 makes this explicit because URL normalization is itself a new attack surface. IdPs that ship `iss` and discovery `issuer` with inconsistent trailing slashes are non-compliant and must use the explicit `Issuers` pin-list to accept the actual JWT iss form (recorded in §9 operational gaps). |
| D18 | Broadcast handler concurrency — single-flight by `(tenantID, uri)`; reload_all takes registry write lock | **Reload and invalidate broadcasts for the same `(tenantID, uri)` serialize through a single-flight worker keyed by `<tenantID>:<uri>`. reload_all broadcasts execute under `Registry.mu.Lock()` for the duration of the rebuild — a brief stop-the-world (whole-registry write lock; readers block, in-flight per-(T,uri) reloads must complete first, no new per-(T,uri) reloads start until the write lock releases).** This makes reload_all the durable inter-node convergence mechanism without races against per-(T,uri) operations. `handleBroadcast` wraps with `defer recover()` and logs panics at ERROR. | rev. 3 keyed single-flight by `(op, tenantID, uri)`; rev. 4 (this revision) further fixes that `reload_all` (key `*`) did not serialize with `reload(T, uri)` (key `T:uri`) — the keys are different so the debouncer ran them in parallel, racing on the same provider entry. Acquiring the registry write lock around reload_all's rebuild is the simplest correct serialization and costs only a brief stop-the-world on what is already an infrequent operation. |
| D19 | Reactivate's JWKS sync is conditional on upstream success | `reactivateKeys=true` (default) attempts JWKS fetch; if HTTP 200 + valid JSON, sync the cache (drop locally cached keys not in remote JWKS); if any failure, leave cache as-is and WARN log. `InvalidatedAt` is cleared regardless. | rev. 1 ported cyoda-cloud's literal semantic; during a key-rotation gap, in-band reactivate would silently invalidate good keys. |
| D20 | Per-provider audience enforcement | Add `ExpectedAudiences []string` to `OidcProvider`. Empty/nil = unchecked; documented. OpenAPI DTOs gain `expectedAudiences` (optional array). Recorded in §9. | Global `CYODA_JWT_AUDIENCE` is wrong default for external IdPs. |
| D21 | List endpoint authz scope | `GET /oauth/oidc/providers` allows any authenticated member of the owning tenant. Mutating endpoints remain ROLE_ADMIN. | Matches cyoda-cloud. |
| D22 | Observability — telemetry on the OIDC hot path | **Metrics:** counters `oidc_kid_cache_hit_total`, `oidc_kid_cache_miss_total`, `oidc_kid_cache_evict_total` (D6 self-heal eviction count), `oidc_jwks_fetch_error_total{outcome}`, `oidc_broadcast_panic_total`, `oidc_unknown_provider_broadcast_total` (I9: broadcast for unknown (T, uri)); histogram `oidc_broadcast_receive_seconds`; gauge `oidc_registry_providers` (aggregate, no tenant label). Non-resolver `kid`-miss WARN demoted to DEBUG; one INFO rollup line per provider per 60s. | Aggregate-only gauge avoids Prometheus cardinality footgun at 1K+ tenants. Per-tenant counts surface via admin API if needed. |
| D23 | UserContext extraction for OIDC tokens — collision-safe, configurable roles, bounded sub | **`UserContext.UserID = fmt.Sprintf("oidc:%s:%s", providerUUID, sub)`** with `sub` constrained: present, non-empty, ≤255 chars, contains no ASCII control characters (`\x00-\x1f`, `\x7f`). Violations → `ErrClaimsFailure` (subcode `missing_sub` or `invalid_sub`). **The composite `UserID` is opaque to downstream components — no consumer is permitted to parse it back into provider-UUID + sub.** **`UserContext.UserName = sub`** (display only; same constraints). **`UserContext.Tenant.ID = OwnerLegalEntityID`** per D1+D17. **`UserContext.Roles`** = extracted from token claim whose name is `provider.RolesClaim` (per-provider, optional), falling back to global `cfg.IAM.OIDC.DefaultRolesClaim` (`CYODA_OIDC_ROLES_CLAIM`, default `"roles"`). Roles parsing accepts JSON array-of-strings OR space-delimited string; empty/missing → empty roles. **Claims `caas_user_id`, `caas_org_id`, `tid`, `tenant`, `org` are ignored** — attacker-controlled when the IdP is external. | rev. 2's silent "existing convention" rejects external tokens. The cross-provider `sub`-collision path requires namespacing. `sub` bounds defend against parser/log-injection footguns from IdPs with permissive `sub` formats. Documenting `UserID` opacity prevents future code from splitting on `:` and assuming structure (an attacker `sub` containing `:` would corrupt that parsing). Per-provider `RolesClaim` accommodates IdP variation; recorded in §9. |
| D24 | PATCH tri-state decoding pattern | Adapter decodes request body as `map[string]json.RawMessage`; checks key presence. For `issuers` and `expectedAudiences`: key absent → unchanged; key present (whether `null` or `[]`) → cleared. For `rolesClaim`: key absent → unchanged; present and `null` → revert to global default (set field to nil); present and non-empty string → override. | Stdlib `json.Unmarshal` into `[]string` collapses absent and null to nil. Map-of-RawMessage pattern is small, idiomatic, isolates the tri-state concern to the adapter. |
| D25 | Ownership-transition audit logging — KV-backed cluster-consistent | **The cross-tenant URI ownership history is persisted in KV** at `oidc-providers/_history:<sha256(uri)>` as a JSON `UriOwnershipHistory` struct (see §3.3). **On Register**: read this entry; if the entry exists AND any past or current owner has a `TenantID` different from the registering tenant, emit `slog.Info("oidc.cross_tenant_uri_registration", ...)` with fields `{registering_tenant, prior_or_concurrent_tenants: [...], wellknown_uri_hash, new_provider_uuid}`. Append the new owner to the history. **On Delete**: mark the current-owner entry's `DeletedAt`. The history grows monotonically per URI; eviction is not required because the per-URI history is bounded by the number of distinct registrations ever made for that URI. **Cluster behaviour:** the log fires at the registering node only (the broadcasting node). Receiving nodes process the broadcast normally — they do not re-emit the audit log. The KV-backed history is cluster-consistent (every node sees the same KV state on demand) and restart-survivable. | rev. 3 had a per-node in-memory `previousOwners` cache that was not cluster-consistent, not restart-survivable, and risked unbounded growth. The KV-backed `_history:<sha256>` entry replaces it cleanly. Logging at the registering node only is intentional — a Register HTTP request has exactly one originating node, and the audit signal belongs to that node's request log. |

## 3. Architecture

### 3.1 Package layout & files touched

| File | Action |
|---|---|
| `internal/auth/errors.go` | **New.** Four chain sentinels (D3): `ErrUnknownKID`, `ErrIssuerMismatch`, `ErrSignatureFailure`, `ErrClaimsFailure`. Plus `ErrTokenPreTransition` (D17), `ErrJWKSUnavailable`. |
| `internal/auth/chain.go` | **New.** `type Validator interface { Validate(string) (*spi.UserContext, error) }`; `type ChainedValidator struct { ... }`; constructor preserves slice order; `Validate` falls through ONLY on `ErrUnknownKID`. |
| `internal/auth/parse.go` | **New.** `parseTokenHeader(tokenString) (kid, alg, iss, aud string, exp, iat int64, sub string, err error)`. |
| `internal/auth/validator.go` | Refine error semantics: line 70 → `ErrUnknownKID`; line 83 → `ErrIssuerMismatch`; line 73 → `ErrSignatureFailure`; line 77 → `ErrClaimsFailure`. Existing single-validator callers see indistinguishable wire behaviour (all 401); chain callers get the right fall-through. |
| `internal/auth/oidc/types.go` | **New.** `type OidcProvider struct { ... }` + `type UriOwnershipHistory struct { ... }` + error variables. |
| `internal/auth/oidc/store.go` | **New.** `type OidcProviderStore interface { ... }` — `Register`, `Get`, `GetByURI`, `Update`, `Delete`, `ListByTenant`, `LoadAll()`, **`GetURIHistory(uriHash) (*UriOwnershipHistory, error)`**, **`PutURIHistory(uriHash, *UriOwnershipHistory) error`**. |
| `internal/auth/oidc/kv_store.go` | **New.** `KVOidcProviderStore` against `spi.KeyValueStore`. Single namespace `oidc-providers`. System-tenant context. Stale-index defence on `Get`/`GetByURI`. |
| `internal/auth/oidc/discovery.go` | **New.** `type Discovery interface { Fetch(ctx, uri) (DiscoveryDoc, error) }`; `HTTPDiscovery` with `safeDialContext`, redirects disabled. Bytewise `Issuer` comparison helper used by `OIDCValidator`. |
| `internal/auth/oidc/ssrf.go` | **New.** `validateRegisterURI(...)` register-time UX; `safeDialContext(allowPrivate bool)` fetch-time security. IPv4 + IPv6 ranges per D10. |
| `internal/auth/oidc/jwks_source.go` | **New.** `providerKeySource` wraps `HTTPJWKSSource` with provider lifecycle gate. |
| `internal/auth/oidc/registry.go` | **New.** `Registry` — provider main map + per-provider source (incl. cached `DiscoveryDoc`) + `kid`-fast-path with self-heal (D6). Subscribes to `oidc.providers`. `ResolveKey(kid, iss)` returns one of the four D3 sentinels (disposition matrix in §4.1). `EvictKidEntry(kid, providerRef)` self-heal. `WarmJWKSAsync(ctx)` for Phase 2. **`ReloadAll(ctx)` takes `mu.Lock()` for the whole rebuild** (D18). |
| `internal/auth/oidc/validator.go` | **New.** `OIDCValidator` implementing `auth.Validator`. Constructor signature: `NewValidator(registry *Registry, defaultRolesClaim string)`. Calls `parseTokenHeader`, performs iat-binding (D17), audience check (D20), `sub` validation + Roles extraction (D23), consults `Registry.ResolveKey`, returns sentinels. |
| `internal/auth/oidc/service.go` | **New.** `Service` — orchestrates the 7 lifecycle ops. **D25 audit emission inlined here** (no separate `audit.go`). KV mutation → registry mutation → broadcast (fire-and-forget, D7). |
| `internal/auth/oidc/broadcast.go` | **New.** `topicOidcProviders` constant + envelope type + `Service.broadcast(op, tenantID, uri)` + `Registry.handleBroadcast(payload)`. Single-flight keyed by `<T>:<uri>` per D18. Top-level `defer recover()`. Broadcast for unknown `(T, uri)` (no entry in registry) → INFO log, increment `oidc_unknown_provider_broadcast_total`, return (I9). |
| `internal/auth/oidc/observability.go` | **New.** Metric definitions per D22 wired against `internal/observability`. |
| `internal/auth/oidc/usercontext.go` | **New.** `buildOIDCUserContext(provider *OidcProvider, claims map[string]any, defaultRolesClaim string) (*spi.UserContext, error)` — implements D23 namespacing + `sub` bounds + roles parsing. |
| `internal/auth/oidc/*_test.go` | **New.** Unit tests per component; `fixture_test.go` exports `FixtureIdP`. **`fault_kv_test.go`**: KV wrapper that injects pauses between operations (for the I8 race-test path). |
| `internal/domain/account/oidc_adapter.go` | **New.** Seven HTTP→`oidc.Service` adapters. PATCH adapter uses `map[string]json.RawMessage` decode per D24. Authz: ROLE_ADMIN on mutating; tenant-member on List per D21. |
| `internal/domain/account/oidc_adapter_test.go` | **New.** |
| `internal/domain/account/handler.go` | Replace lines 95-121 with delegations. Add `oidcService *oidc.Service` field; wire via `NewHandler`. |
| `internal/api/server.go:507-561` | No change. |
| `app/app.go` (around line 236) | Phase-1 warmup; **broadcaster non-nil check (`os.Exit(1)` if missing, D7);** construct chain `auth.NewChainedValidator(jwksValidator, oidcValidator)` — **the order is normative (D3) and must be preserved by future maintainers; row 36 in §11 verifies it.** Phase-2 warmup `go registry.WarmJWKSAsync(systemCtx)` post-listener. |
| `app/config.go` | Add `OIDCConfig` struct + `OIDC OIDCConfig` field on `IAMConfig`. Field `DefaultRolesClaim string`. Populate from 6 env vars in `DefaultConfig()`. |
| `api/openapi.yaml` | Remove seven `$ref: "#/components/responses/NotImplemented"`. Reconcile prose `SUPER_USER` → `ROLE_ADMIN` on 6 mutating; List prose → "any authenticated tenant member." Add `expectedAudiences: array(string)` and `rolesClaim: string` (optional) to `RegisterOidcProviderRequestDto`, `UpdateOidcProviderRequestDto`, and `OidcProviderResponseDto`. **Fix `issuers` description on both request DTOs** to match D17 (`"When omitted, null, or empty array, the 'iss' claim must match the provider's discovery-document `issuer` field via strict bytewise comparison."`). **Drop `minItems: 1` from `issuers`** on both DTOs (pre-existing self-contradiction). |
| `api/generated.go` | Regenerated. |
| `internal/e2e/parity/oidc.go` | **New.** 64 `RunOidc*` scenarios per §11. |
| `e2e/parity/registry.go` | Register the new scenarios. |
| `cmd/cyoda/help/content/config/oidc.md` | **New.** 6 env vars. |
| `cmd/cyoda/help/content/errors/OIDC_PROVIDER_DUPLICATE.md` | **New** — clarifies per-tenant uniqueness scope. |
| `cmd/cyoda/help/content/errors/OIDC_PROVIDER_NOT_FOUND.md` | **New.** |
| `cmd/cyoda/help/content/errors/OIDC_PROVIDER_INACTIVE.md` | **New.** |
| `cmd/cyoda/help/content/errors/OIDC_SSRF_BLOCKED.md` | **New.** |
| `cmd/cyoda/help/content/errors/OIDC_AUDIENCE_MISMATCH.md` | **New.** |
| `cmd/cyoda/help/content/errors/OIDC_TOKEN_PRE_TRANSITION.md` | **New** — documents 30s skew. |
| `cmd/cyoda/help/content/errors/OIDC_DISCOVERY_FAILED.md` | **New.** |
| `cmd/cyoda/help/content/errors/OIDC_JWKS_UNAVAILABLE.md` | **New.** |
| `cmd/cyoda/help/content/errors/OIDC_CLAIMS_INVALID.md` | **New.** Parent code for `missing_sub`, `invalid_sub`, `audience`, `expired`, `unsupported_alg` subcodes. |
| `internal/common/error_codes.go` | **Add 9 codes under the `OIDC_*` family** (the 8 already listed + `OIDC_CLAIMS_INVALID`). Convention note: subsystem prefix without `AUTH_` wrapper, matching `KEYPAIR_NOT_FOUND`, `TRUSTED_KEY_NOT_FOUND`, `M2M_CLIENT_NOT_FOUND`. |
| `README.md` | New "OIDC Provider Configuration" subsection. |
| `docs/ARCHITECTURE.md` | New §7.3 "OIDC Provider Registry"; existing §7.3 → §7.4. |
| `docs/PRD.md` | Expand §8 with OIDC chaining subsection + external-issuer token-claims example. |
| `docs/FEATURES.md` | One bullet: "OIDC provider per-tenant registry." |
| `CHANGELOG.md` `## [0.8.0]` | `### Added`: OIDC subsystem; 6 env vars. `### Changed`: seven `/oauth/oidc/providers/*` endpoints now return real responses; previously 501. Clients that special-cased 501 should update. |

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
              │ (D3 normative chain order: JWKSValidator FIRST)           │
              ▼                                                           ▼
   ┌──────────────────────┐    ErrUnknownKID only                ┌────────────────────────┐
   │ JWKSValidator        │ ──────────────────────────────────►  │ OIDCValidator          │
   │ (first-party)        │                                      │ (sub bounds + iat ±30s │
   │ returns one of:      │    other sentinels = hard-fail       │  + aud + D23 +         │
   │  ErrUnknownKID       │                                      │  Registry.ResolveKey)  │
   │  ErrIssuerMismatch   │                                      │ returns same 4         │
   │  ErrSignatureFailure │                                      │ sentinels +            │
   │  ErrClaimsFailure    │                                      │ ErrTokenPreTransition  │
   └──────────────────────┘                                      │ ErrJWKSUnavailable     │
                                                                 └───────────┬────────────┘
                                                                             │ (calls EvictKidEntry
                                                                             │  on ErrSignatureFailure)
                                                          ┌──────────────────▼──────────┐
                                                          │ Registry                    │
                                                          │  - providers[T][uri]        │
                                                          │  - sources[T][uri] (incl.   │
                                                          │     cached DiscoveryDoc)    │
                                                          │  - kidIndex (populated at   │
                                                          │     resolution; D6)         │
                                                          └─────┬────────────────┬──────┘
                                                                │                │
                          ┌─────────────────────────────────────▼──┐  ┌──────────▼──────────┐
                          │ KVOidcProviderStore (system tenant)    │  │ ClusterBroadcaster  │
                          │  ns: oidc-providers                    │  │  topic              │
                          │   <T>:<provider-uuid>                  │  │   "oidc.providers"  │
                          │   <T>:uri:<sha256(uri)>                │  │  single-flight key  │
                          │   _history:<sha256(uri)>  (D25)        │  │  = <T>:<uri>        │
                          └────────────────────────────────────────┘  │  reload_all → mu.Lock
                                                                      │  fire-and-forget    │
                                                                      └──────────┬──────────┘
                                                                                 │
                                              ┌──────────────────────────────────┘
                                              │
                          ┌───────────────────▼─────────────────┐
                          │ Service                              │
                          │  Register/Update/Invalidate/         │
                          │  Reactivate/Delete/Reload[All]       │
                          │  KV mutate → registry mutate →       │
                          │  D25 audit (inline) → broadcast      │
                          └───────────────────┬──────────────────┘
                                              │
                          ┌───────────────────▼──────────────────┐
                          │ HTTPDiscovery                        │
                          │  - safeDialContext (per-dial CIDR)   │
                          │  - redirects disabled                │
                          │  - .well-known/openid-configuration  │
                          │  - JWKS fetch                        │
                          └──────────────────────────────────────┘
```

### 3.3 Data model

```go
// internal/auth/oidc/types.go
type OidcProvider struct {
    ID                 uuid.UUID  `json:"id"`
    WellKnownConfigURI string     `json:"wellKnownConfigUri"` // unique per tenant
    Issuers            []string   `json:"issuers,omitempty"`  // optional pin list (max 10);
                                                              // empty = require iss == DiscoveryDoc.Issuer (when known)
    ExpectedAudiences  []string   `json:"expectedAudiences,omitempty"` // D20; empty = aud unchecked
    RolesClaim         *string    `json:"rolesClaim,omitempty"`        // D23; nil = use DefaultRolesClaim
    InvalidatedAt      *time.Time `json:"invalidatedAt,omitempty"`
    CreatedAt          time.Time  `json:"createdAt"`           // load-bearing for D17 iat-binding
    OwnerLegalEntityID uuid.UUID  `json:"ownerLegalEntityId"`
}

func (p *OidcProvider) Active() bool { return p.InvalidatedAt == nil }

// D25 — KV-backed cluster-consistent ownership history per URI
type UriOwnershipHistory struct {
    CurrentOwner *Owner   `json:"currentOwner,omitempty"`  // nil after every owner has Deleted
    Past         []Owner  `json:"past"`                    // every owner that has been Deleted, oldest first
}

type Owner struct {
    TenantID     string     `json:"tenantId"`
    ProviderUUID string     `json:"providerUuid"`
    RegisteredAt time.Time  `json:"registeredAt"`
    DeletedAt    *time.Time `json:"deletedAt,omitempty"`
}
```

KV layout (single namespace, all keys live under system tenant's partition):

```
namespace        key                                  value
oidc-providers   <tenantID>:<provider-uuid>           JSON(OidcProvider)
oidc-providers   <tenantID>:uri:<sha256(URI-hex)>     <provider-uuid>              (per-tenant uniqueness)
oidc-providers   _history:<sha256(URI-hex)>           JSON(UriOwnershipHistory)    (cross-tenant audit, D25)
```

**Register race (D11).** Both nodes/clients see no per-tenant URI-index entry; both `Put` the index (one overwrites the other); both `Put` blobs; after `Put` index, Service re-reads the index value; if it doesn't equal the value just written, the loser deletes its own blob and returns 409.

**Stale-index defence on every read.** `Get(tenantID, providerID)` reads blob and verifies `OwnerLegalEntityID` matches. `GetByURI(tenantID, uri)` reads index → blob and verifies tenant prefix matches. Mismatch → orphan; best-effort `Delete`; return `ErrProviderNotFound`.

**D25 ownership-history transitions.** `_history:<sha256>` is updated atomically with each per-tenant index operation (best-effort sequentially — failure to update history is logged as ERROR but does not fail the Register/Delete; the history is an audit signal, not a correctness gate). On Register, if `_history.CurrentOwner != nil` AND `CurrentOwner.TenantID != registering tenant`, the registering tenant is concurrent with another tenant for this URI — the audit log fires with both tenants. On Register, if any `Past` entry has a tenant different from the registering tenant, the registering tenant is taking over from a previously-deleted foreign tenant — the audit log also fires.

### 3.4 REST surface

DTOs extended:

- `RegisterOidcProviderRequestDto`: `{wellKnownConfigUri, issuers?, expectedAudiences?, rolesClaim?}`
- `UpdateOidcProviderRequestDto`: `{issuers?, expectedAudiences?, rolesClaim?}`
- `ReactivateOidcProviderRequestDto`: `{reactivateKeys?}` (unchanged)
- `OidcProviderResponseDto`: `{id, wellKnownConfigUri, issuers?, expectedAudiences?, rolesClaim?, active, createdAt}`

Wire matrix:

| Method | Path | Handler | Authz | Success | Errors |
|---|---|---|---|---|---|
| POST | `/oauth/oidc/providers` | `RegisterOidcProvider` | ROLE_ADMIN | 200 `OidcProviderResponseDto` | 400 `OIDC_PROVIDER_DUPLICATE`; 400 `BAD_REQUEST`; 400 `OIDC_SSRF_BLOCKED`; 403; 401 |
| GET | `/oauth/oidc/providers?activeOnly=<bool>` | `ListOidcProviders` | any authenticated tenant member (D21) | 200 `[]OidcProviderResponseDto` | 401 |
| PATCH | `/oauth/oidc/providers/{id}` | `UpdateOidcProvider` | ROLE_ADMIN | 200 `OidcProviderResponseDto` | 404 `OIDC_PROVIDER_NOT_FOUND`; 409 `OIDC_PROVIDER_INACTIVE`; 400; 403; 401 |
| POST | `/oauth/oidc/providers/{id}/invalidate` | `InvalidateOidcProvider` | ROLE_ADMIN | 200 empty | 404; 403; 401 (idempotent) |
| POST | `/oauth/oidc/providers/{id}/reactivate` | `ReactivateOidcProvider` | ROLE_ADMIN | 200 `OidcProviderResponseDto` | 404; 403; 401 (idempotent) |
| DELETE | `/oauth/oidc/providers/{id}` | `DeleteOidcProvider` | ROLE_ADMIN | 200 empty | 404; 403; 401 |
| POST | `/oauth/oidc/providers/reload` | `ReloadOidcProviders` | explicit in-code ROLE_ADMIN | 200 empty | 403; 401 |

Token-validation-time errors:

| Sentinel | Wire | Meaning |
|---|---|---|
| `ErrUnknownKID` | 401 (after chain exhaustion) | Neither validator recognised the `kid`. Includes Phase-2-pending cold-start (D8). |
| `ErrIssuerMismatch` | 401 + code in `OIDC_*` family | A KeySource resolved `kid` but `iss` did not match (bytewise per D17). |
| `ErrSignatureFailure` | 401 | Signature verification failed. Triggers `kidIndex` self-heal (D6). |
| `ErrClaimsFailure` | 401 + subcode mapped to `OIDC_CLAIMS_INVALID` or `OIDC_AUDIENCE_MISMATCH` | `exp` / `nbf` / `aud` / `sub` / alg failure. Subcodes: `expired`, `nbf`, `audience` (→ `OIDC_AUDIENCE_MISMATCH`), `missing_sub`, `invalid_sub`, `unsupported_alg` (all → `OIDC_CLAIMS_INVALID`). |
| `ErrTokenPreTransition` | 401 + code `OIDC_TOKEN_PRE_TRANSITION` | `iat < provider.CreatedAt - 30s` (D17). |
| `ErrJWKSUnavailable` | 503 + Retry-After | Transient JWKS-endpoint failure. |

## 4. Runtime mechanics

### 4.1 Registry

```go
type Registry struct {
    mu        sync.RWMutex
    providers map[spi.TenantID]map[string]*OidcProvider // by wellKnownConfigUri
    sources   map[spi.TenantID]map[string]*providerSource
    kidIndex  map[string][]providerRef                  // D6: populated at resolution, evicted on verify-fail

    store        OidcProviderStore
    discovery    Discovery
    broadcast    spi.ClusterBroadcaster
    singleflight *singleflightDebouncer  // keyed by <T>:<uri> — D18
    clock        func() time.Time
    metrics      *Metrics                // D22
    logger       *slog.Logger
}

type providerSource struct {
    keySource    auth.KeySource
    discoveryDoc *DiscoveryDoc           // nil during Phase-2-pending (D8)
}

type providerRef struct {
    tenant spi.TenantID
    uri    string
}

type KeyResolution struct {
    PublicKey          *rsa.PublicKey
    Provider           *OidcProvider
    WellKnownConfigURI string
    ProviderRef        providerRef       // returned so caller can self-heal (D6)
}

func (r *Registry) ResolveKey(kid, iss string) (*KeyResolution, error)
func (r *Registry) EvictKidEntry(kid string, ref providerRef)  // D6 self-heal
func (r *Registry) ReloadAll(ctx context.Context) error        // D18: takes mu.Lock for rebuild
```

`ResolveKey` semantics — explicit error-disposition matrix:

1. Hot path: under `r.mu.RLock`, look up `kidIndex[kid]` → candidate refs. Release the read lock before any work that may need the write lock (per Go-mutex-discipline).
2. For each candidate ref → look up provider. If provider's `sources[T][uri].discoveryDoc == nil` (Phase-2-pending), this candidate contributes nothing; continue. Otherwise apply the iss-validation rule (D17, **strict bytewise**):
   - If `provider.Issuers` non-empty and `iss ∈ Issuers` → iss-eligible candidate.
   - If `provider.Issuers` empty and `iss == providerSource.discoveryDoc.Issuer` → iss-eligible candidate.
   - Otherwise → candidate not iss-eligible.
3. **Disposition (hot path):**
   - At least one iss-eligible candidate whose `source.GetKey(kid)` returns a key → return `KeyResolution` with `ProviderRef` populated. **Caller MUST call `EvictKidEntry(kid, ref)` on subsequent `ErrSignatureFailure` (D6 contract).**
   - At least one iss-eligible candidate but all sources return transient errors → `ErrJWKSUnavailable`.
   - No iss-eligible candidates after iterating all kidIndex entries → fall through to cold path (the candidates may be stale; the cold path may find new ones).
4. Cold path: under `r.mu.Lock`, iterate all providers globally. Apply the same iss-eligibility rule. On finding a successful resolution (source returned key, iss-eligible), **append `(tenant, uri)` to `kidIndex[kid]` before returning the `KeyResolution`**. This is D6's load-bearing populate-at-resolution invariant — the cold path cannot know if the caller's subsequent signature verification will succeed; it populates optimistically and the caller's `EvictKidEntry` corrects on failure.
5. Cold-path miss (no providers iss-eligible OR all iss-eligible candidates had Phase-2-pending discovery docs) → `ErrUnknownKID`.

The source's `GetKey(kid)` refetches the JWKS endpoint unconditionally on cache miss (`internal/auth/http_jwks_source.go:93-121`), so a self-heal eviction followed by a new token for the same `kid` triggers a fresh fetch via the source rather than serving stale data. No separate refetch-trigger machinery is needed.

Locking discipline per `.claude/rules/go-mutex-discipline.md`.

### 4.2 Broadcast

```go
const topicOidcProviders = "oidc.providers"

type broadcastEnvelope struct {
    Op       string `json:"op"`   // "reload" | "invalidate" | "reload_all"
    TenantID string `json:"t,omitempty"`
    URI      string `json:"u,omitempty"`
}

// handleBroadcast runs on the memberlist receive goroutine.
// Invariants: never blocks; never panics; idempotent.
func (r *Registry) handleBroadcast(payload []byte) {
    defer func() {
        if rec := recover(); rec != nil {
            r.logger.Error("oidc broadcast handler panic", "pkg", "oidc", "panic", rec)
            r.metrics.PanicTotal.Inc()
        }
    }()

    var env broadcastEnvelope
    if err := json.Unmarshal(payload, &env); err != nil {
        return
    }

    switch env.Op {
    case "reload":
        r.singleflight.Dispatch(env.TenantID+":"+env.URI, func() {
            r.reloadOne(spi.TenantID(env.TenantID), env.URI)
        })
    case "invalidate":
        r.singleflight.Dispatch(env.TenantID+":"+env.URI, func() {
            r.invalidateOne(spi.TenantID(env.TenantID), env.URI)
        })
    case "reload_all":
        // D18: reload_all takes the registry write lock for the rebuild duration.
        // The single-flight here debounces back-to-back reload_all broadcasts;
        // the write-lock serializes with all per-(T,uri) reload/invalidate work.
        r.singleflight.Dispatch("_reload_all", func() {
            _ = r.ReloadAll(context.Background())
        })
    }
}
```

`singleflightDebouncer.Dispatch(key, fn)`: if no work for `key`, spawn one goroutine running `fn`; if work IS in flight for `key`, drop (logged at DEBUG). Small wrapper around a `map[string]struct{}` + mutex.

**reload_all serialization (D18).** `Registry.ReloadAll` acquires `mu.Lock()` around the whole rebuild: any in-flight per-`(T, uri)` reload completes first (because they hold `mu.RLock`/`mu.Lock` themselves and the writer waits for outstanding readers), and no new per-`(T, uri)` operation can begin until the rebuild releases the lock. This is a brief stop-the-world on the OIDC registry only — `modelcache` and other broadcaster subscribers are unaffected. It is intentional: `reload_all` is the cluster's anti-entropy convergence operation; correctness over throughput on this path.

**Broadcast for unknown `(T, uri)` (I9).** When `reloadOne` or `invalidateOne` is called for a `(T, uri)` whose provider does not exist in the registry, `reloadOne` consults `store.GetByURI` — if missing, log at INFO with `{tenant, uri_hash}`, increment `oidc_unknown_provider_broadcast_total`, return. **Do not panic; do not propagate an error to the broadcast handler (there is no caller upstream).** This is the normal cluster-membership scenario: a node joined after a Register completed, or a node missed a Delete broadcast.

### 4.3 Validator chain

```go
// internal/auth/chain.go
type Validator interface {
    Validate(tokenString string) (*spi.UserContext, error)
}

type ChainedValidator struct {
    validators []Validator
}

// NewChainedValidator preserves slice order. Chain order is normative — see app/app.go
// for the canonical construction: (JWKSValidator, OIDCValidator). Reversing the order
// changes which validator processes a token first; row 36 of the parity test plan locks
// this in by asserting a first-party kid with foreign iss reaches JWKSValidator first
// (and hard-fails with ErrIssuerMismatch without consulting OIDCValidator).
func NewChainedValidator(validators ...Validator) *ChainedValidator { ... }

func (c *ChainedValidator) Validate(tokenString string) (*spi.UserContext, error) {
    var lastErr error
    for _, v := range c.validators {
        uc, err := v.Validate(tokenString)
        if err == nil {
            return uc, nil
        }
        if !errors.Is(err, auth.ErrUnknownKID) {
            return nil, err  // hard-fail; do not consult subsequent validators
        }
        lastErr = err
    }
    if lastErr != nil {
        return nil, lastErr
    }
    return nil, auth.ErrUnknownKID
}
```

`OIDCValidator.Validate`:

1. `parseTokenHeader(tokenString)` → `kid, alg, iss, aud, exp, iat, sub, err`. Alg not in allow-list → `ErrClaimsFailure` (subcode `unsupported_alg`). `exp` past → `ErrClaimsFailure` (subcode `expired`).
2. **`sub` claim validation (D23):** must be present, non-empty, ≤255 chars, no ASCII control chars (`\x00-\x1f`, `\x7f`). Missing → `ErrClaimsFailure` (subcode `missing_sub`). Invalid → `ErrClaimsFailure` (subcode `invalid_sub`).
3. `r.ResolveKey(kid, iss)` → `(*KeyResolution, error)`. Propagate `ErrUnknownKID` (chain fall-through), `ErrIssuerMismatch` (hard-fail), `ErrJWKSUnavailable` (hard-fail).
4. **D17 `iat` binding with 30s skew.** If `iat < resolution.Provider.CreatedAt.Unix() - 30` → return `ErrTokenPreTransition`. Audit log fields: provider UUID, tenant, kid; **never the token or iss**.
5. Verify signature against `resolution.PublicKey`. Failure → **call `r.EvictKidEntry(kid, resolution.ProviderRef)` (D6)**, then return `ErrSignatureFailure`.
6. Standard claims: `exp`, `nbf` (30s skew) → `ErrClaimsFailure` on failure.
7. **D20 audience check.** If `resolution.Provider.ExpectedAudiences` non-empty, require token's `aud` to match (RFC 7519 §4.1.3). Failure → `ErrClaimsFailure` (subcode `audience`).
8. **D23 UserContext extraction.** Build:
   - `UserID = "oidc:" + resolution.Provider.ID.String() + ":" + sub` (opaque to downstream; no parsing)
   - `UserName = sub`
   - `Tenant.ID = resolution.Provider.OwnerLegalEntityID.String()`
   - `Roles` = parse from claim named by `provider.RolesClaim` if non-nil else `defaultRolesClaim`. Accept JSON array-of-strings OR space-delimited string. Missing/empty → empty slice.

## 5. Lifecycle behaviour (per operation)

Skeleton: validate input → authz → tenancy guard → store mutation → registry mutation → broadcast → respond. Store mutation precedes registry mutation; broadcast is best-effort, never fails the response (D7).

### 5.1 Register

1. `auth.RequireAdmin(w, r)`.
2. **D24 decode pattern:** read body into `map[string]json.RawMessage`. Validate `wellKnownConfigUri` (required, ≤1000 chars, valid absolute URL, scheme matches `CYODA_OIDC_REQUIRE_HTTPS`). Validate optional `issuers` (≤10, each ≤1000, dedup), `expectedAudiences` (≤10, each ≤1000, dedup), `rolesClaim` (≤100 chars). **Register-time SSRF gate (D10):** DNS-resolve hostname; reject if in blocklist (unless override).
3. `tenantID := tenantFromCtx(r)`.
4. KV: `Get("oidc-providers", "<T>:uri:<sha256>")`. If present → 400 `OIDC_PROVIDER_DUPLICATE`.
5. KV: `Put` per-tenant index `<T>:uri:<sha256>` → `<provider-uuid>`. KV: `Put` blob `<T>:<provider-uuid>` → JSON(OidcProvider).
6. **Race-validation re-read (D11):** `Get("oidc-providers", "<T>:uri:<sha256>")`. If value ≠ our `<provider-uuid>`: `Delete` our blob, return 409 `OIDC_PROVIDER_DUPLICATE`.
7. **D25 ownership-history update.** `GetURIHistory(sha256(uri))` → `*UriOwnershipHistory` (or nil if first registration ever for this URI). Compute audit-trigger boolean: `trigger = (h != nil && (h.CurrentOwner != nil && h.CurrentOwner.TenantID != tenantID) || any past owner has TenantID != tenantID)`. Append `Owner{TenantID, ProviderUUID, RegisteredAt: now()}` to history (set as `CurrentOwner` if previous CurrentOwner is nil; else also keep previous CurrentOwner in `Past` with no `DeletedAt` to mark it as concurrent). `PutURIHistory(...)`. **PutURIHistory failure is ERROR-logged but does NOT fail the Register** (audit signal, not correctness gate). If `trigger`: emit `slog.Info("oidc.cross_tenant_uri_registration", registering_tenant=<T>, prior_or_concurrent_tenants=[...], wellknown_uri_hash=<sha256>, new_provider_uuid=<uuid>)`.
8. `registry.reloadOne(tenantID, uri)` — synchronous discovery + JWKS fetch. Failures WARN-logged; provider stays registered.
9. `broadcast({op: "reload", t: tenantID, u: uri})` — fire-and-forget (D7).
10. Respond 200 with `OidcProviderResponseDto`.

### 5.2 Update issuers / audiences / rolesClaim

1. `auth.RequireAdmin`.
2. KV `Get` by `(tenantID, providerID)` with stale-index defence. Missing → 404 `OIDC_PROVIDER_NOT_FOUND`.
3. If `InvalidatedAt != nil` → 409 `OIDC_PROVIDER_INACTIVE`.
4. **D24 decode + tri-state.** Read body into `map[string]json.RawMessage`. For `issuers` and `expectedAudiences`:
   - Key absent → field unchanged.
   - Key present (both `null` and `[]` cases) → cleared (normalize the nil-after-null case in the unmarshal, then set field to nil).
   - Key present with non-empty array → validate and set.
   
   For `rolesClaim`:
   - Key absent → unchanged.
   - Key present and `null` → set field to nil (revert to global default).
   - Key present with non-empty string → validate length and override.
   
5. KV `Put` updated blob.
6. `registry.reloadOne(tenantID, uri)` with **JWKS cache flush** and **discovery doc refetch**.
7. Broadcast reload.
8. Respond 200 with DTO.

### 5.3 Invalidate

1. `auth.RequireAdmin`.
2. KV `Get` with stale-index defence. Missing → 404.
3. Already invalidated: WARN, 200 idempotent, no broadcast.
4. KV `Put` with `InvalidatedAt = time.Now()`.
5. Registry: drop provider entry + KeySource. Evict matching `kidIndex` candidates.
6. `broadcast({op: "invalidate", ...})`.
7. Respond 200 empty.

### 5.4 Reactivate

1. `auth.RequireAdmin`.
2. KV `Get` with stale-index defence. Missing → 404.
3. Already active: INFO, 200 with current DTO, no broadcast.
4. KV `Put` with `InvalidatedAt = nil`.
5. **D19 conditional sync.** `reactivateKeys=true` (default): `registry.reloadOne(tenantID, uri, syncKeys=true)`; **if fetch succeeds (HTTP 200 + valid JSON)**, sync cache (drop locally cached keys not in remote JWKS); **if fetch fails**, preserve cache, WARN. `reactivateKeys=false`: discovery only.
6. Broadcast reload.
7. Respond 200 with DTO.

### 5.5 Delete

1. `auth.RequireAdmin`.
2. KV `Get` with stale-index defence. Missing → 404.
3. If active, perform invalidate semantics inline.
4. KV: `Delete` blob `<T>:<provider-uuid>` AND `Delete` per-tenant index `<T>:uri:<sha256>`.
5. **D25 ownership-history update.** `GetURIHistory(sha256(uri))`; if `CurrentOwner.TenantID == tenantID && CurrentOwner.ProviderUUID == providerUUID`, set `CurrentOwner.DeletedAt = now()`, move it to `Past`, set `CurrentOwner = nil` (unless there was a concurrent owner, in which case promote that owner from `Past` to `CurrentOwner`). `PutURIHistory(...)`. The history entry persists indefinitely — bounded by the number of distinct registrations of that URI, naturally limited by operational reality.
6. Registry: drop provider entry, evict matching `kidIndex` candidates.
7. `broadcast({op: "invalidate", ...})`.
8. Respond 200 empty.

### 5.6 Reload-all

1. **Explicit in-code ROLE_ADMIN check.**
2. **`Registry.ReloadAll(ctx)`** — acquires `mu.Lock()` for the entire rebuild duration. `store.LoadAll()` → populate fresh `providers` / `sources` / `kidIndex`; swap in place. Releases write lock.
3. `broadcast({op: "reload_all"})`.
4. Respond 200 empty.

### 5.7 List

1. **Any authenticated tenant member (D21).** `tenantID := tenantFromCtx(r)`. No tenant in context → 401.
2. `store.ListByTenant(tenantID, activeOnly)`.
3. Map to DTOs.
4. Respond 200.

## 6. Startup (D8)

In `app/app.go`, after the `KVTrustedKeyStore` bootstrap:

```go
// D7 invariant — broadcaster MUST be configured
if broadcaster == nil {
    slog.Error("startup failure", "phase", "oidc-broadcaster-missing")
    os.Exit(1)
}

// Phase 1: synchronous, blocking listener
oidcStore, err := oidc.NewKVProviderStore(systemCtx, kvStore)
if err != nil {
    slog.Error("startup failure", "phase", "oidc-store-bootstrap", "error", err.Error())
    os.Exit(1)
}

oidcDiscovery := oidc.NewHTTPDiscovery(cfg.IAM.OIDC)
oidcRegistry := oidc.NewRegistry(oidcStore, oidcDiscovery, broadcaster, logger)

if err := oidcRegistry.LoadProvidersFromKV(systemCtx); err != nil {
    slog.Error("startup failure", "phase", "oidc-registry-providers-load", "error", err.Error())
    os.Exit(1)
}

oidcValidator := oidc.NewValidator(oidcRegistry, cfg.IAM.OIDC.DefaultRolesClaim)

// D3 chain order is NORMATIVE: JWKSValidator first, OIDCValidator second.
// Reversing changes which validator processes a token first and breaks the
// "first-party kid + foreign iss" hard-fail invariant tested at §11 row 36.
chainedValidator := auth.NewChainedValidator(jwksValidator, oidcValidator)
// wire chainedValidator into HTTP auth middleware + gRPC interceptor

// (...HTTP listener binds here, well before warm-JWKS finishes...)

// Phase 2: asynchronous, post-listener
go oidcRegistry.WarmJWKSAsync(systemCtx)
```

`LoadProvidersFromKV` does `kv.List("oidc-providers")` (one call, system-tenant context), parses keys: `<T>:<provider-uuid>` → provider blobs, `<T>:uri:<sha256>` → indices (validated against blobs), `_history:<sha256>` → ownership histories (loaded into a small in-memory cache for fast Register-time lookups but the KV is the source of truth).

`WarmJWKSAsync` spawns goroutines bounded by `runtime.NumCPU()` worker pool. Per-provider goroutine fetches discovery + JWKS via the safedialer'd client, populates `sources[T][uri]` (including `discoveryDoc`) and `kidIndex` for that provider. Per-provider failures: WARN-log, do not retry within this warmup pass.

**Cold-start window (D8 + D17):** during Phase 2, tokens for a not-yet-warmed provider get `ErrUnknownKID` (chain fall-through → 401), NOT `ErrIssuerMismatch`. Conservative for correctness — the alternative is blocking the listener on JWKS fetch for all tenants. Documented operational characteristic.

## 7. Configuration

| Env var | Type | Default | Maps to | Purpose |
|---|---|---|---|---|
| `CYODA_OIDC_REQUIRE_HTTPS` | bool | `true` | `IAM.OIDC.RequireHTTPS` | Reject `http://` URIs at Register. |
| `CYODA_OIDC_CONNECT_TIMEOUT_MS` | int | `5000` | `IAM.OIDC.ConnectTimeoutMs` | TCP connect timeout. |
| `CYODA_OIDC_SOCKET_TIMEOUT_MS` | int | `5000` | `IAM.OIDC.SocketTimeoutMs` | HTTP read timeout. |
| `CYODA_OIDC_CONNECTION_REQUEST_TIMEOUT_MS` | int | `5000` | `IAM.OIDC.ConnectionRequestTimeoutMs` | Pool-checkout timeout. |
| `CYODA_OIDC_ALLOW_PRIVATE_NETWORKS` | bool | `false` | `IAM.OIDC.AllowPrivateNetworks` | Test/dev override for SSRF defence (both register-time + fetch-time). |
| `CYODA_OIDC_ROLES_CLAIM` | string | `"roles"` | `IAM.OIDC.DefaultRolesClaim` | Default claim name for role extraction; per-provider `rolesClaim` overrides. |

## 8. Security

| Aspect | Mechanism | Verification |
|---|---|---|
| Authn | Existing bearerAuth middleware — no new wiring | All 7 endpoints already declare `security: [bearerAuth: []]` |
| Authz (mutating) | `auth.RequireAdmin(w, r)`; explicit in-code on `Reload` | 6 negative tests (non-admin → 403) |
| Authz (read) | Any authenticated tenant member (D21); tenant scope via `tenantFromCtx` + `OwnerLegalEntityID` | Tenant-member non-admin lists own providers; cross-tenant member cannot see others |
| Tenant isolation (mgmt API) | Composite-key prefix `<tenantID>:`; stale-index defence on every `Get` | Cross-tenant: tenant-A's provider ID → 404 to tenant-B |
| Tenant isolation (token validation) | `UserContext.Tenant = OwnerLegalEntityID` from resolving provider; `tid`/`tenant`/`org` claims ignored | Test: token with forged `tid=B` claim still yields `Tenant=A` |
| UserContext cross-IdP isolation (D23) | `UserID` namespaced `oidc:<providerUUID>:<sub>`; **`UserID` is opaque to downstream** (no parsing) | Test: two IdPs with colliding `sub` → distinct `UserID`s |
| `sub` bounds (D23) | Required, ≤255 chars, no ASCII control chars | Test: `sub` with `\n` → `ErrClaimsFailure invalid_sub`; `sub` of 300 chars → rejected; `sub` containing `:` (legitimate) accepted; UserID parser is opaque-only by contract |
| Cross-tenant escalation — accidental (D17) | `iat < CreatedAt - 30s` rejects | Test: tenant A registers, JWT issued, A deletes, B registers same URI, A's old JWT rejected |
| Cross-tenant escalation — adversarial (D17) | **Not defended at validator.** D25 INFO audit log on Register-after-different-tenant gives operators a signal | Test: D25 audit log fires with correct fields |
| Cross-tenant routing via kid collision (D17) | Mandatory bytewise `iss` validation; empty Issuers → `iss == DiscoveryDoc.Issuer` bytewise | Test: two AWS Cognito tenants with overlapping kid namespace → tokens route by iss |
| Chain-fall-through escalation (D3) | Four distinct sentinels; chain falls through ONLY on `ErrUnknownKID`; **order is normative** | Test: first-party kid + foreign iss → 401 `ErrIssuerMismatch`, OIDCValidator NOT consulted |
| kid-cache poisoning (D6 self-heal) | `kidIndex` populated at resolution; evicted on signature failure | Test: malicious tenant publishes first-party kid; first-party still validates; malicious ref evicted on next attempt |
| SSRF — register-time (D10) | DNS-resolve + CIDR check | Test: `http://169.254.169.254/...` → 400 `OIDC_SSRF_BLOCKED` |
| SSRF — fetch-time (D10) | `safeDialContext` re-checks every dial | Test: fast-flux DNS → fetch fails |
| SSRF — redirects (D10) | `CheckRedirect → http.ErrUseLastResponse` | Test: discovery 302 → internal URL; fetch fails |
| Per-provider audience (D20) | `ExpectedAudiences` checked; empty = unchecked | Test: aud=["api1"] rejects aud=api2 |
| Cold-start (Phase-2-pending) | Missing discovery doc → `ErrUnknownKID` chain fall-through | Test: token during Phase-2-pending → 401, NOT 200 and NOT `ErrIssuerMismatch` |
| Log hygiene (Gate 3) | Never log tokens / JWT contents / private keys / JWKS payloads / `iss` of rejected tokens. **OK**: `kid`, `wellKnownConfigUri`, tenantID, provider UUID | Pre-PR grep gate over `internal/auth/oidc/` |
| Output sanitization | 4xx with domain code + short message; 5xx with generic + ticket UUID | Test: JWKS-fetch 500 response body contains only ticket UUID |
| Race-safety | Lock discipline per `.claude/rules/go-mutex-discipline.md`; broadcast handlers idempotent + `defer recover()` | Race detector pre-PR |
| Broadcast handler isolation (D18) | `defer recover()`; single-flight; reload_all takes registry write lock | Test: induced panic does NOT kill `modelcache` delivery on same node |
| Observability (D22) | Metrics via `internal/observability`; DEBUG kid-misses; INFO 60s rollup | Verify metrics in E2E; grep `Level=WARN` on hot path → none |
| Ownership-transition audit (D25) | KV-backed `_history` entry; INFO log at registering node on cross-tenant case | Test: tenant-A delete + tenant-B register cycle emits `oidc.cross_tenant_uri_registration` |

## 9. Deliberate divergences from cyoda-cloud + operational gaps

This register captures every divergence from the cyoda-cloud reference. Additions require explicit justification.

**Divergences:**

| # | Surface | cyoda-cloud | cyoda-go | Decision | Rationale |
|---|---|---|---|---|---|
| DV1 | Update inactive provider | `IllegalStateException` → 5xx | 409 + `OIDC_PROVIDER_INACTIVE` | D5 | 4xx-domain-detail convention; 5xx generic violates Gate 3 |
| DV2 | Broadcast failure | `notifyAllNodes(awaitResult=true)` blocks for ack | None — SPI is fire-and-forget | D7 | SPI exposes no failure surface; cyoda-cloud "intercom failure → 500" test does not port |
| DV3 | Audience enforcement | Global audience | Per-provider `ExpectedAudiences` | D20 | Global is wrong default for external IdPs |
| DV4 | iat-binding scope | None | Closes accidental spillover; not adversarial; D25 audit for the adversarial signal | D17 + D25 | Honest scope; mitigation log in scope |
| DV5 | UserContext extraction | cyoda-cloud's own role-extraction | `UserID = "oidc:<providerUUID>:<sub>"` opaque; per-provider `RolesClaim` + global default | D23 | Cross-IdP `sub` collision; configurable for IdP variation |
| DV6 | OpenAPI request DTOs | `{wellKnownConfigUri, issuers?}` | + `expectedAudiences?` + `rolesClaim?` | D20 + D23 | Additive; clients of cyoda-cloud's OpenAPI carry over without code change |
| DV7 | `iss` comparison | Implementation-defined | Bytewise per OIDC Core 1.0 §2 | D17 | Bytewise is spec-compliant; URL normalization is new attack surface |
| DV8 | Ownership history | Not modelled | KV-backed `_history` per URI | D25 | Restart-survivable, cluster-consistent audit signal |

**Operational gaps (not divergences):**

- **IdP `iss`/discovery `issuer` trailing-slash inconsistency.** If an external IdP serves discovery with `issuer: https://idp.example.com` but stamps tokens with `iss: https://idp.example.com/` (or vice versa), the bytewise comparison rejects. cyoda-go does not normalize. **Operator action:** populate the provider's `Issuers` pin-list with the exact JWT `iss` form. cyoda-cloud has the same constraint under the same OIDC-spec-compliance interpretation.
- **IdP issuer migration.** If an external IdP changes its discovery `issuer` field without admin awareness, cached `DiscoveryDoc.Issuer` becomes stale; tokens with the new `iss` reject as `ErrIssuerMismatch` until an admin triggers `PATCH` (refetches discovery) or `POST /reload`. **Operator action:** monitor `oidc_jwks_fetch_error_total{outcome=...}` and INFO kid-miss rollups.
- **D25 audit log evicted entry scenario.** Not applicable — the `_history` entries are KV-persisted indefinitely. The per-URI history bound is "number of distinct registrations that URI has ever had" — naturally limited by operational reality.

## 10. Acceptance criteria mapping

| Issue body item | Where covered |
|---|---|
| Brainstorm + spec under `docs/superpowers/specs/` | This document |
| Plan under `docs/superpowers/plans/` | Next step (writing-plans skill) |
| All 7 handlers return real responses | §3.1 + §5 |
| All 27 ported test scenarios pass across memory/sqlite/postgres/cassandra | §11 (cyoda-cloud's count is 27; rev. 4 adds 37 cyoda-go-specific for 64 total). **Cassandra row of this gate verified in the cassandra repo's PR** (cassandra inherits parity-test registration without code changes; not this PR's scope to confirm directly) |
| `JWKSValidator` extended multi-issuer with per-provider lifecycle | §4.3 (chained, four-sentinel — D3) |
| `CYODA_OIDC_*` env vars documented in `cmd/cyoda/help/content/config/` | §7 + §12 |
| Cross-node cache invalidation verified via multi-node E2E harness | §11 broadcast-eviction scenario |
| No credentials / tokens / private keys in any log line | §8 + pre-PR grep gate |
| 501 declarations removed from OpenAPI for these 7 paths | §3.1 + CHANGELOG `### Changed` |
| `h.stub(w, r)` calls removed for OIDC handlers | §3.1 + §5 |
| Decomp spec §3.4 updated to v0.8.0 | Already done in tree (amendment at head); §1 records the satisfaction |

## 11. Test plan

All scenarios run as parity tests in `internal/e2e/parity/oidc.go`, registered in `e2e/parity/registry.go::allTests`. Each parity backend (memory, sqlite, postgres) picks them up; cassandra inherits on its next dependency update.

**Authoritative inventory (64 scenarios — 27 ported from cyoda-cloud + 37 cyoda-go-specific):**

| # | Scenario | Source |
|---|---|---|
| **CRUD happy-path (ported, 6)** | | |
| 1 | Register | port |
| 2 | List all | port |
| 3 | List activeOnly=true | port |
| 4 | Update issuers | port |
| 5 | Invalidate | port |
| 6 | Delete | port |
| **CRUD negative — 404/duplicate (4)** | | |
| 7 | Update nonexistent → 404 | port |
| 8 | Invalidate nonexistent → 404 | port |
| 9 | Reactivate nonexistent → 404 | port |
| 10 | Duplicate Register → 400 `OIDC_PROVIDER_DUPLICATE` | port |
| **Authz negative — mutating (6)** | | |
| 11 | Non-admin Register → 403 | port |
| 12 | Non-admin Update → 403 | port |
| 13 | Non-admin Invalidate → 403 | port |
| 14 | Non-admin Reactivate → 403 | port |
| 15 | Non-admin Delete → 403 | port |
| 16 | Non-admin Reload → 403 | port |
| **JWT validation integration (4)** | | |
| 17 | Register, sign JWT, validate → 200 | port |
| 18 | Invalidate, same JWT → 401 | port |
| 19 | Reactivate, same JWT → 200 | port |
| 20 | Delete, same JWT → 401 (permanent) | port |
| **Issuer-list update affects validation (1)** | | |
| 21 | Cycle `issuers` list (remove, re-add, set null/empty) | port |
| **Key rotation / revocation (5)** | | |
| 22 | Key `validTo` past → rejected | port |
| 23 | Key `validTo` future → accepted | port |
| 24 | Key `validTo` null → accepted | port |
| 25 | Invalidated key → rejected | port |
| 26a | Reactivate(keys=true), upstream JWKS reachable, upstream removed kid K → local cache drops K (cyoda-cloud port) | port |
| 26b | Reactivate(keys=true), upstream JWKS reachable, JWKS unchanged → local cache unchanged (idempotency) | cyoda-go (I10) |
| **Multi-provider isolation (1)** | | |
| 27 | Two providers, same tenant, keys don't cross-validate | port |
| **Inactive-update divergence (1)** | | |
| 28 | Update inactive → 409 `OIDC_PROVIDER_INACTIVE` (D5/DV1) | cyoda-go |
| **Cross-tenant management isolation (1)** | | |
| 29 | Tenant-A's provider ID → 404 from tenant-B caller | cyoda-go (D1) |
| **Tenant-binding via OwnerLegalEntityID (1)** | | |
| 30 | Token with forged `tid=B` claim still yields `Tenant=A` | cyoda-go (D1) |
| **D17 iat-binding (1)** | | |
| 31 | Delete A → Register B same URI → A's pre-deletion JWT rejected with `ErrTokenPreTransition` | cyoda-go (D17) |
| **D17 mandatory iss-validation (2)** | | |
| 32 | Two providers with overlapping kid namespace; tokens route by iss (bytewise) | cyoda-go (D17) |
| 33 | Provider with empty `issuers`; token iss mismatches `DiscoveryDoc.Issuer` (bytewise; e.g., trailing slash) → `ErrIssuerMismatch` | cyoda-go (D17/I1) |
| **D17 iat clock skew (2)** | | |
| 34 | `iat = CreatedAt - 5s` (within 30s skew) → accepted | cyoda-go (D17/I8) |
| 35 | `iat = CreatedAt - 5min` (outside skew) → `ErrTokenPreTransition` | cyoda-go (D17/I8) |
| **D3 four-sentinel chain fall-through (1)** | | |
| 36 | First-party kid + foreign iss → 401 `ErrIssuerMismatch`, OIDCValidator NOT consulted (verified via mock OIDCValidator call counter) | cyoda-go (D3, normative chain order) |
| **D6 kid-cache (2)** | | |
| 37 | Malicious tenant publishes first-party kid; first-party validation succeeds; malicious entry evicted on next attempt | cyoda-go (D6) |
| 37b | Cold-path populates `kidIndex` with two iss-eligible candidates; first to verify wins; signature-failure on second eviction is recoverable | cyoda-go (D6 populate-at-resolve) |
| **D11 register race (2)** | | |
| 38a | Sequential Register from two clients (parity-safe): first 200, second 409 at duplicate-check | cyoda-go (D11 deterministic path) |
| 38b | Concurrent Register with `FaultInjectingKV` wrapper that pauses between Put and re-read: exactly one 200, exactly one 409, KV ends in single-blob state | cyoda-go (D11 true-race path, cyoda-go-only) |
| 39 | Orphan `<T>:uri:...` index entry with no blob → Get/List skip; index cleaned up | cyoda-go (D11 stale-index defence) |
| **D8 two-phase warmup (3)** | | |
| 40 | 100 providers, slow JWKS endpoints; listener binds within 1s; tokens validate within 30s | cyoda-go (D8) |
| 41 | Per-provider Phase-2 failure → log WARN, no os.Exit, other providers serve traffic | cyoda-go (D8) |
| 42 | Token during Phase-2-pending for its provider → `ErrUnknownKID` (chain fall-through → 401), NOT `ErrIssuerMismatch` | cyoda-go (D8) |
| **D18 broadcast (4)** | | |
| 43 | Panic in OIDC `handleBroadcast` does NOT kill `modelcache` delivery on same node | cyoda-go (D18) |
| 44 | Single-flight debounce: 10 RELOAD broadcasts for same `(T, uri)` in 100ms → one `reloadOne` | cyoda-go (D18) |
| 45 | Reload + Invalidate broadcasts for same `(T, uri)` serialize through one worker; final state deterministic locally | cyoda-go (D18) |
| 46 | reload_all broadcast serializes with concurrent reload(T, uri) — reload_all rebuild completes; subsequent reload(T, uri) sees rebuilt state | cyoda-go (D18 reload_all write-lock) |
| **D10 SSRF (3)** | | |
| 47 | Fast-flux DNS: register-time public IP, fetch-time 127.0.0.1 → fetch fails | cyoda-go (D10) |
| 48 | IPv6 link-local `fe80::1` and ULA `fc00::1` → register 400 | cyoda-go (D10) |
| 49 | Discovery 302 → `http://169.254.169.254/...` → fetch fails (no follow) | cyoda-go (D10) |
| **D19 reactivate (2)** | | |
| 50 | Reactivate(keys=true) JWKS reachable, includes prior keys → cache preserved (success path success-path; see also 26a/26b for sync semantics) | cyoda-go (D19 happy) |
| 51 | Reactivate(keys=true) JWKS down → cache preserved, WARN, InvalidatedAt cleared | cyoda-go (D19 failure) |
| **D20 audience (2)** | | |
| 52 | Per-provider `expectedAudiences=["api1"]` rejects `aud=api2` | cyoda-go (D20) |
| 53 | Empty `expectedAudiences` accepts any aud | cyoda-go (D20) |
| **D23 UserContext (3)** | | |
| 54 | Cross-IdP `sub` collision → distinct `UserID`s (namespacing) | cyoda-go (D23) |
| 55 | Per-provider `rolesClaim` override extracts roles from non-default claim (e.g., `cognito:groups`) | cyoda-go (D23) |
| 56 | Roles parsing handles array-of-strings AND space-delimited-string forms | cyoda-go (D23) |
| **D23 sub bounds (3)** | | |
| 57 | `sub` containing `\n` → `ErrClaimsFailure invalid_sub` → 401 `OIDC_CLAIMS_INVALID` | cyoda-go (D23/I2) |
| 58 | `sub` of 300 chars → rejected; `sub` exactly 255 chars accepted | cyoda-go (D23/I2) |
| 59 | `sub` containing `:` (legitimate) accepted; UserID is opaque (test does not parse) | cyoda-go (D23/I2) |
| **D25 ownership transition (3)** | | |
| 60 | Tenant-A Register, tenant-A Delete, tenant-B Register same URI → tenant-B's Register emits `oidc.cross_tenant_uri_registration` INFO with `prior_or_concurrent_tenants=[A]` | cyoda-go (D25) |
| 61 | KV-backed history survives a registering-node restart: restart the node, then tenant-C Registers same URI → log fires with `prior_or_concurrent_tenants=[A, B]` (B still active) | cyoda-go (D25 restart survival) |
| 62 | Receiving node processes broadcast for cross-tenant Register but does NOT re-emit the audit log | cyoda-go (D25 single-node emission) |
| **D21 list authz (1)** | | |
| 63 | Non-admin tenant member: GET → 200; cross-tenant member → empty list | cyoda-go (D21) |
| **I9 broadcast for unknown provider (1)** | | |
| 64 | Broadcast `reload(T, uri)` for a provider that doesn't exist in receiving node's registry → INFO log, increment `oidc_unknown_provider_broadcast_total`, no panic | cyoda-go (I9) |
| **State-transition pairs (2)** | | |
| 65 | active → invalidated → deleted: state consistent at each step | cyoda-go |
| 66 | invalidated → reactivated → invalidated: kid cache flushed correctly across cycle | cyoda-go |
| **E2E coverage (Gate 2) (2)** | | |
| 67 | `TestE2E_OIDC_TokenValidation`: register + mock IdP + GET /me + invalidate + 401 | cyoda-go |
| 68 | `TestE2E_OIDC_MultiNode_Eviction`: memberlist ring + shared KV; register on A, B reflects within one gossip cycle | cyoda-go |

Row-count audit: 27 ported (rows 1-25, 26a-as-port, 27) + 37 cyoda-go-specific (26b, 28-68 minus the ported ones that share row numbers in their family) = 64. Row numbers above run to 68 but reflect six sub-row splits (26a/26b, 37/37b, 38a/38b); the distinct-test count is 64. Family headers group thematically only.

Fixture IdP shape:

```go
// internal/auth/oidc/fixture_test.go
type FixtureIdP struct {
    Server   *httptest.Server
    Issuer   string
    JWKSURI  string
    // internal: signing keys, revocation set, fake-DNS hook for SSRF tests
}

func NewFixtureIdP(t *testing.T) *FixtureIdP
func (f *FixtureIdP) SignJWT(t *testing.T, kid string, claims jwt.Claims) string
func (f *FixtureIdP) RotateKey(t *testing.T) (newKID string)
func (f *FixtureIdP) RevokeKey(t *testing.T, kid string)
func (f *FixtureIdP) WithFakeDNS(t *testing.T, registerIP, fetchIP net.IP) *FixtureIdP
func (f *FixtureIdP) RegisterFirstPartyKey(t *testing.T, kid string, key *rsa.PrivateKey) // for row 36
```

Fault-injecting KV wrapper for row 38b:

```go
// internal/auth/oidc/fault_kv_test.go
type FaultInjectingKV struct {
    Inner    spi.KeyValueStore
    PauseAfter map[string]chan struct{}  // op-name → release channel
}
// Implements spi.KeyValueStore; pauses after a named op completes,
// releasing when test sends on the channel. Used to deterministically
// interleave two Register goroutines.
```

Multi-node broadcast-eviction E2E: two in-process cyoda-go instances over a real `gossip_broadcast.Gossip` ring, sharing a memory-plugin KV store.

## 12. Documentation updates (Gate 4)

| File | Action |
|---|---|
| `cmd/cyoda/help/content/config/oidc.md` | **New** — 6 env vars |
| `cmd/cyoda/help/content/errors/OIDC_PROVIDER_DUPLICATE.md` | **New** — per-tenant uniqueness scope |
| `cmd/cyoda/help/content/errors/OIDC_PROVIDER_NOT_FOUND.md` | **New** |
| `cmd/cyoda/help/content/errors/OIDC_PROVIDER_INACTIVE.md` | **New** |
| `cmd/cyoda/help/content/errors/OIDC_SSRF_BLOCKED.md` | **New** |
| `cmd/cyoda/help/content/errors/OIDC_AUDIENCE_MISMATCH.md` | **New** |
| `cmd/cyoda/help/content/errors/OIDC_TOKEN_PRE_TRANSITION.md` | **New** — documents 30s skew |
| `cmd/cyoda/help/content/errors/OIDC_DISCOVERY_FAILED.md` | **New** |
| `cmd/cyoda/help/content/errors/OIDC_JWKS_UNAVAILABLE.md` | **New** |
| `cmd/cyoda/help/content/errors/OIDC_CLAIMS_INVALID.md` | **New** — parent code for `missing_sub`, `invalid_sub`, `audience`, `expired`, `nbf`, `unsupported_alg` subcodes |
| `app/config.go` `DefaultConfig()` | Populate `IAMConfig.OIDC` from 6 env vars (`DefaultRolesClaim` field) |
| `README.md` | New "OIDC Provider Configuration" subsection |
| `docs/ARCHITECTURE.md` | New §7.3 "OIDC Provider Registry"; existing §7.3 → §7.4 |
| `docs/PRD.md` | Expand §8 with OIDC chaining + external-issuer token-claims example |
| `docs/FEATURES.md` | One bullet: "OIDC provider per-tenant registry" |
| `CHANGELOG.md` `## [0.8.0]` | `### Added`: OIDC subsystem; 6 env vars. `### Changed`: seven `/oauth/oidc/providers/*` endpoints now return real responses (previously 501); clients that special-cased 501 should update. |
| `COMPATIBILITY.md` | No change — no SPI bump |
| `docs/adr/0002-federated-identity-provider-architecture.md` | **New** — pins D1, D2, D3 |
