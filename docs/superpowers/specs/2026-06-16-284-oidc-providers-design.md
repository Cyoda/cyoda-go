# Issue #284 — OIDC providers subsystem (greenfield)

**Status:** approved (rev. 3, post second-review)
**Date:** 2026-06-16
**Issue:** [#284](https://github.com/Cyoda-platform/cyoda-go/issues/284)
**Umbrella:** [`2026-06-04-194-decomposition-design.md`](2026-06-04-194-decomposition-design.md) §3.4 (amended 2026-06-16, pulled into v0.8.0)
**Predecessor:** issue #123 (closed as duplicate; content consolidated into #284)

**Revision history:**

- **rev. 1** — initial design.
- **rev. 2** — fresh-context review demonstrated rev. 1 misread the KV-store tenancy model and missed cross-tenant escalation paths. Dropped the proposed SPI extension; introduced four error sentinels; added `iat`-binding and mandatory `iss` validation; two-phase warmup; fetch-time SSRF; explicit register-race semantics; per-provider audience; observability; admin chain-introspection endpoint.
- **rev. 3** — second fresh-context review caught: (a) broadcast failure semantics that are mechanically impossible against the SPI contract (`Broadcast` returns nothing); (b) undefined `UserContext.UserID` extraction for OIDC tokens — a real cross-provider `sub`-collision path; (c) PATCH tri-state semantics not implementable with stdlib JSON decoding; (d) `iat`-binding scope overstated (defends accidental spillover, not adversarial IdP operator); (e) cold-start window contradiction (Phase-2 pending vs mandatory iss-check); (f) Prometheus tenant-label cardinality footgun; (g) OpenAPI prose contradicting D17 already in tree; (h) cross-op single-flight race; (i) `iat` clock-skew unspecified. rev. 3 drops broadcast-failure handling entirely, specifies UserContext extraction (with new env `CYODA_OIDC_ROLES_CLAIM` + per-provider `RolesClaim` override), defines the `map[string]json.RawMessage` decode pattern for tri-state PATCH, rephrases iat-binding to accidental-spillover-only, adds ownership-transition INFO audit logging in scope, makes Phase-2-pending tokens fall through with `ErrUnknownKID`, drops the tenant label from the gauge, fixes the OpenAPI prose, re-keys single-flight by `(tenantID, uri)`, specifies the 30s `iat` skew, and **drops D23 (admin chain introspection)** as forward-engineering with no v0.8.0 consumer. The decision-table preserves stable D1/D2/D3 labels (ADR cross-references).

**Scope:** Land a per-tenant OIDC provider registry, the seven REST handlers currently stubbed at HTTP 501 (`internal/domain/account/handler.go:95-121`), a chained multi-issuer JWT validator with four distinct error sentinels, single-topic cluster broadcast for cross-node cache eviction, six `CYODA_OIDC_*` configuration env vars, telemetry, and a parity test suite (50 scenarios — see §11 matrix). Zero changes to any SPI surface. Zero `h.stub` calls remain on the OIDC path; the seven `NotImplemented` declarations come off `api/openapi.yaml`.

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

The `KVTrustedKeyStore` (`internal/auth/kv_trusted_store.go:20, 49-51, 73-106`) is the load-bearing precedent: one namespace `trusted-keys`, composite keys `<tenantID>:<kid>`, runs under the system tenant context (`app/app.go:217-222`), reads back all rows with a single `kv.List("trusted-keys")` at warmup. The SPI's `KeyValueStore` filters every op by tenantID at acquisition (`plugins/postgres/kv_store.go:18-23, 30-42, 44-52, 54-76`); there is no cross-tenant enumeration primitive and there should not be. Single-namespace + composite-keys is the only correct shape.

The SPI's `ClusterBroadcaster.Broadcast(topic, payload)` returns nothing (`cyoda-go-spi/cluster.go:16-19`; `internal/cluster/registry/gossip_broadcast.go:38-44`). It is fire-and-forget by contract. **The management API does not surface broadcast failure** because there is no broadcast failure to surface. cyoda-cloud's `notifyAllNodes(awaitResult=true)` ack semantics are not portable; the related cyoda-cloud test does not port (see §9 divergence register).

Per the umbrella's §4 reconciliation, mutating endpoints require `ROLE_ADMIN`; `List` is open to any authenticated member of the owning tenant (D21). OpenAPI prose currently says `SUPER_USER`; this PR aligns prose to `ROLE_ADMIN` on the 6 mutating operations.

Issue #34 (trusted-key audit) is independent.

## 2. Decisions

| # | Decision | Choice | Rationale |
|---|---|---|---|
| D1 | Tenancy scope of the OIDC provider registry | **Per-tenant.** Each legal entity owns its providers; management API filters by `tenantFromCtx(r)`; KV records keyed by `(tenantID, ...)`. Token validation iterates providers across all tenants; the resolving provider's `OwnerLegalEntityID` determines the user context's tenant — subject to the cross-tenant escalation guards in D17. | cyoda-go-light deploys into more varied contexts than cyoda-cloud where per-tenant ownership is the safer default. Validation-side global iteration is unavoidable in any case (tokens carry no tenant claim). |
| D2 | Persistence backend | **`KeyValueStore` SPI, single namespace, composite keys.** Namespace `oidc-providers`. Provider blob key: `<tenantID>:<provider-uuid>`. URI uniqueness index key: `<tenantID>:uri:<sha256(wellKnownConfigUri)>`. Store acquired with system-tenant context at startup. `LoadAll` enumerates via `kv.List("oidc-providers")` and parses tenantID out of each key. **No SPI extension.** | The trusted-key precedent (`internal/auth/kv_trusted_store.go`) demonstrates the pattern. The KV SPI is already tenant-scoped at acquisition; per-tenant namespaces would either return empty (honest impl) or require a deliberate isolation bypass. |
| D3 | Multi-issuer validator integration with four distinct error sentinels | **Chained validators.** New `auth.Validator` interface, new `auth.ChainedValidator`. Four sentinels: `ErrUnknownKID` (chain fall-through), `ErrIssuerMismatch` (hard-fail, kid was resolved but `iss` does not match), `ErrSignatureFailure` (hard-fail), `ErrClaimsFailure` (hard-fail). The chain only falls through on `ErrUnknownKID`. Plus `ErrTokenPreTransition` (D17) and `ErrJWKSUnavailable` (resolution-transient). | Today `JWKSValidator.Validate` verifies signature before iss-check. Existing behaviour is already correct (hard-fail on iss-mismatch); the chain pattern preserves it via the typed sentinel. Single-sentinel rev-1 design would have *introduced* an escalation path the existing code does not have. |
| D4 | Cluster broadcast wire format | **One topic `oidc.providers`** with envelope `{op: "reload"\|"invalidate"\|"reload_all", tenantID, wellKnownConfigUri?}`. Mirrors `JWKOIDCCacheNotification` and `modelcache`'s single-topic convention. | One Subscribe, one switch. Receiver handlers idempotent. |
| D5 | Wire shape for "update inactive provider" | **`HTTP 409 Conflict` + error code `OIDC_PROVIDER_INACTIVE`**. Diverges from cyoda-cloud's `IllegalStateException → 5xx`. | Matches cyoda-go's 4xx-domain-detail convention (`.claude/rules/error-handling.md`). 5xx generic would violate Gate 3 output sanitization. Recorded in §9 divergence register. |
| D6 | Validator key-resolution with self-healing | **`kid`-indexed fast-path** + global-iteration cold path. `kidIndex` entries self-heal on signature failure (evict offending providerRef). | `O(active providers)` per token is unacceptable on the bearer-auth hot path. Self-healing closes kid-cache poisoning. |
| D7 | Broadcast failure handling | **None at the SPI level — `Broadcast` is fire-and-forget by contract** (`cyoda-go-spi/cluster.go:16-19` returns nothing). The management API does **not** check or surface broadcast-delivery state. Misconfiguration (`broadcaster == nil`) is a startup invariant: `app/app.go` validates non-nil and `os.Exit(1)` if missing. There is no run-time failure to handle, no rollback to perform, no error code to return. | rev. 2 specified a "broadcast failure → 500 + rollback" path that is mechanically impossible: the SPI exposes no failure surface. The KV-rollback would also be wrong (peers may have already received the broadcast). Per `feedback_engine_no_special_rights` we do not extend the SPI with a sync-ack variant. The cyoda-cloud "intercom failure → 500" parity test does not port — recorded in §9. |
| D8 | Two-phase startup warmup | **Phase 1 (synchronous, blocks listener):** `kv.List("oidc-providers")` → parse → populate `Registry.providers` and (empty) `kidIndex`. Cheap, no network I/O, must complete before HTTP listener binds. **Phase 2 (asynchronous, post-listener):** per-provider goroutine fetches discovery doc + JWKS, populates `Registry.sources` and `kidIndex`. Failures: WARN-log, continue. **Tokens arriving for a provider during the Phase-2-pending window:** the absence of a fetched `DiscoveryDoc` for that provider causes `ResolveKey` to return `ErrUnknownKID` (chain fall-through → 401), not `ErrIssuerMismatch`. This is the documented cold-start window. | rev. 2 had a contradiction between §3.1's "fall through to ErrUnknownKID" cold-start claim and D17's "iss must equal cached DiscoveryDoc.Issuer" which would have been empty pre-fetch → ErrIssuerMismatch (hard-fail). rev. 3 resolves: missing-doc → ErrUnknownKID. |
| D9 | Configuration env vars | **Six.** Four core discovery-defaults (mirror cyoda-cloud `IAMProperties.OIDCDefaults`): `CYODA_OIDC_REQUIRE_HTTPS` (bool, `true`), `CYODA_OIDC_CONNECT_TIMEOUT_MS` (int, `5000`), `CYODA_OIDC_SOCKET_TIMEOUT_MS` (int, `5000`), `CYODA_OIDC_CONNECTION_REQUEST_TIMEOUT_MS` (int, `5000`). One SSRF override: `CYODA_OIDC_ALLOW_PRIVATE_NETWORKS` (bool, `false`). One default roles-claim name: `CYODA_OIDC_ROLES_CLAIM` (string, `"roles"`). Per-provider `RolesClaim` field overrides the global (see D23). | The four core mirror cyoda-cloud. The SSRF override is a cyoda-go-specific addition (D10). The roles-claim default is needed because external IdPs vary (Auth0: `roles` or namespaced; Cognito: `cognito:groups`; Keycloak: `realm_access.roles`). |
| D10 | SSRF defence — fetch-time, not just register-time | **Custom `http.Transport.DialContext` re-checks the resolved IP on every dial.** Blocklist: IPv4 loopback, IPv4 link-local, IPv4 RFC1918, IPv6 loopback, IPv6 link-local, IPv6 ULA, IPv4-mapped IPv6. Register-time DNS check stays as UX. **HTTP redirects disabled** on the discovery + JWKS client (fail-closed). Override via `CYODA_OIDC_ALLOW_PRIVATE_NETWORKS=true`. | rev. 1's register-only check is DNS-rebind vulnerable. Per-dial closes that. IPv6 closes dual-stack bypass. Redirect-disabling closes redirect-to-internal. |
| D11 | Register write-race semantics | **Accept the race; resolve correctness on read.** Write index then blob; **re-read index after write** — if value ≠ our `<provider-uuid>` we lost the race, `Delete` our blob, return 409 `OIDC_PROVIDER_DUPLICATE`. **Stale-index defence on every read:** `Get(tenantID, providerID)` and `GetByURI(tenantID, uri)` verify the blob exists AND its `OwnerLegalEntityID` matches the index-key tenant prefix. Mismatch → orphan; best-effort `Delete`; return `ErrProviderNotFound`. | The actual window is "however long the slower node takes between read and second write." Read-time validation is the durable correctness guard; the race is observable but harmless. |
| D12 | Mock IdP fixture | **`internal/auth/oidc/fixture_test.go`** exposes `FixtureIdP` with `httptest.Server` and sign/rotate/revoke/fake-DNS helpers. Per-scenario isolation; `t.Cleanup`. | No existing httptest-IdP precedent in the parity suite. |
| D13 | Scope of #34 (trusted-key audit) inclusion | **Out of scope.** Independent. | Per `feedback_gate6_no_followups`. |
| D14 | ADR file | **Add `0002-federated-identity-provider-architecture.md`.** Pins D1, D2, D3. | Durable architectural record vs per-issue implementation contract. |
| D15 | `COMPATIBILITY.md` update | **None.** No SPI changes; no pin guidance to update. | |
| D16 | cyoda-go-spi / cyoda-go-cassandra changes | **None. No coordinated release.** Single-namespace + composite-key pattern uses only existing KV ops. | |
| D17 | Cross-tenant token-routing escalation guards — honest scope | **`iat`-binding (with 30s clock skew) + mandatory `iss` validation.** (a) `OIDCValidator` rejects tokens where `iat < provider.CreatedAt - 30s` (`ErrTokenPreTransition`, hard-fail). The 30s window matches `ValidateClaims`'s existing skew (`internal/auth/validator.go:77`). (b) `iss` validation is mandatory at resolution: if `provider.Issuers` is non-empty, require `iss ∈ Issuers`; if `Issuers` is empty AND the provider's discovery doc has been fetched, require `iss == DiscoveryDoc.Issuer`. If the discovery doc has NOT been fetched (Phase-2 cold start), return `ErrUnknownKID` (chain fall-through) — see D8. **Scope claim:** these guards close the *accidental* cross-tenant spillover case (long-lived legitimate JWT surviving an ownership handover). They do **not** close the *adversarial* case (the IdP operator mints a fresh token with `iat = time.Now()` after observing the handover). Mitigation for the adversarial case is INFO-level audit logging on every provider-ownership transition (see §5.1 step 7) — an operational observability lever, not a security property. | Two real attack-vector classes; this PR closes the accidental one cleanly and surfaces the adversarial one to operators. Recorded in §9 divergence register. |
| D18 | Broadcast handler concurrency — single-flight by `(tenantID, uri)` | **All broadcast operations for the same `(tenantID, uri)` serialize through a single-flight worker**, regardless of `op`. The single-flight key is `<tenantID>:<uri>` for reload/invalidate; `*` for reload_all. While work is in flight for a key, additional broadcasts for that key drop (debounce). `handleBroadcast` wraps with `defer recover()` and logs panics at ERROR. | rev. 2 keyed single-flight by `(op, T, uri)` — a reload and an invalidate for the same provider raced under different keys, end-state depended on which finished last, and unordered gossip made inter-node states diverge. Re-keying by `(T, uri)` serializes per-provider locally; inter-node convergence is provided by the periodic `reload_all` (and by D11's read-time validation). Tested via §11 row 26. |
| D19 | Reactivate's JWKS sync is conditional on upstream success | `reactivateKeys=true` (default) attempts JWKS fetch; if HTTP 200 + valid JSON, sync the cache (drop locally cached keys not in remote JWKS); if any failure, leave cache as-is and WARN log. `InvalidatedAt` is cleared regardless. | rev. 1 ported cyoda-cloud's literal `syncKeys=true` semantic. During an upstream key-rotation gap (one key removed before the next is added), an in-band reactivate would silently invalidate good keys. |
| D20 | Per-provider audience enforcement | **Add `ExpectedAudiences []string` to `OidcProvider`.** Empty/nil means audience is unchecked for this provider; documented explicitly. OpenAPI request/response DTOs gain `expectedAudiences` (optional array). Recorded in §9 divergence register. | rev. 1 left audience policy unspecified. The global `CYODA_JWT_AUDIENCE` is the wrong default for external IdPs. |
| D21 | List endpoint authz scope | **`GET /oauth/oidc/providers` allows any authenticated member of the owning tenant.** Mutating endpoints remain ROLE_ADMIN. | Matches cyoda-cloud. Legitimate tenant-user use case: discovering which IdPs they can authenticate against. |
| D22 | Observability — telemetry on the OIDC hot path | **Metrics:** counters `oidc_kid_cache_hit_total`, `oidc_kid_cache_miss_total`, `oidc_jwks_fetch_error_total{outcome}`, `oidc_broadcast_panic_total`; histogram `oidc_broadcast_receive_seconds`; gauge `oidc_registry_providers` (aggregate, **no tenant label**). Wired through `internal/observability`. Also: non-resolver `kid`-miss WARN demoted to DEBUG; one INFO-level rollup line per provider per 60s summarising miss counts. | rev. 2 had a per-tenant gauge — a Prometheus cardinality footgun at 1K+ tenants. Aggregate gauge is the right shape; per-tenant counts surface via admin API if/when needed (not in this PR). |
| D23 | UserContext extraction for OIDC tokens — collision-safe, configurable roles | **`UserContext.UserID = fmt.Sprintf("oidc:%s:%s", providerUUID, sub)`** (mandatory; `sub` claim must be present and non-empty, else `ErrClaimsFailure`). **`UserContext.UserName = sub`** (display only). **`UserContext.Tenant.ID = OwnerLegalEntityID`** per D1+D17. **`UserContext.Roles`** = extracted from token claim whose name is `provider.RolesClaim` (per-provider, optional), falling back to global `CYODA_OIDC_ROLES_CLAIM` (default `"roles"`). Roles parsing accepts JSON array-of-strings OR space-delimited string (RFC 8693 §4.2 / OAuth2 scope convention); empty/missing → empty roles. **Claims `caas_user_id`, `caas_org_id`, `tid`, `tenant`, `org`, etc. are ignored** — they are attacker-controlled when the IdP is external. | rev. 2 silently delegated to "existing convention" — that convention rejects external tokens (`internal/auth/validator.go:104-134` requires `caas_org_id`). It also created a cross-provider `sub`-collision path: two IdPs' opaque `sub` values can collide; the spec must namespace them. Provider-UUID namespacing makes UserID globally unique. Per-provider `RolesClaim` accommodates IdP variation; global default keeps single-IdP deployments simple. Documented in §9 divergence register (cyoda-cloud has different role-extraction). |
| D24 | PATCH tri-state decoding pattern | **Adapter decodes the request body as `map[string]json.RawMessage` to check field presence before delegating to typed parsing.** For each PATCH-able field: `_, present := body[name]`. If absent → don't touch. If present, `json.Unmarshal(body[name], &target)` — both `null` and `[]` parse to a non-nil empty slice (after explicit `if target == nil { target = []string{} }` for the null case); both clear at the runtime per D17's empty-list semantics. | Stdlib JSON decoding into `[]string` collapses `absent` and `null` to nil — the adapter cannot distinguish them without explicit presence checking. The map-of-RawMessage pattern is small, idiomatic, and isolates the tri-state concern to the adapter. |
| D25 | Ownership-transition audit logging — in scope | **On Register, if the URI-index entry for this `wellKnownConfigUri` previously existed under a different tenant (gleaned from a brief KV-history check during the same Register transaction — see §5.1 step 7), emit `slog.Info` "oidc.ownership_transition"** with fields `{from_tenant, to_tenant, wellknown_uri_hash, new_provider_uuid}`. Resolves the D17 adversarial-IdP residual risk via operational observability. | Per `feedback_gate6_no_followups`: ADR's "Mitigation follow-up: log the ownership transition" cannot be a follow-up; resolve now. The audit log is cheap, surfaces operationally meaningful events, and gives operators a signal to monitor without claiming a security property the spec doesn't deliver. |

## 3. Architecture

### 3.1 Package layout & files touched

| File | Action |
|---|---|
| `internal/auth/errors.go` | **New.** Four chain sentinels (D3): `ErrUnknownKID`, `ErrIssuerMismatch`, `ErrSignatureFailure`, `ErrClaimsFailure`. Plus `ErrTokenPreTransition` (D17) and `ErrJWKSUnavailable` (resolution-transient). |
| `internal/auth/chain.go` | **New.** `type Validator interface { Validate(string) (*spi.UserContext, error) }`; `type ChainedValidator struct { ... }`. Falls through ONLY on `ErrUnknownKID`. |
| `internal/auth/parse.go` | **New.** `parseTokenHeader(tokenString) (kid, alg, iss, aud string, exp, iat int64, err error)` — unauthenticated header+claims peek. |
| `internal/auth/validator.go` | Refine error semantics: line 70 ("failed to resolve key for kid") becomes `ErrUnknownKID`; line 83 ("untrusted token issuer") becomes `ErrIssuerMismatch`; line 73 becomes `ErrSignatureFailure`; line 77 becomes `ErrClaimsFailure`. Existing single-validator callers see indistinguishable wire behaviour (all 401); chain callers get the right fall-through. |
| `internal/auth/oidc/types.go` | **New.** `type OidcProvider struct { ... }` per §3.3 + error variables (`ErrProviderDuplicate`, `ErrProviderNotFound`, `ErrProviderInactive`, `ErrSSRFBlocked`). |
| `internal/auth/oidc/store.go` | **New.** `type OidcProviderStore interface { ... }` — `Register`, `Get`, `GetByURI`, `Update`, `Delete`, `ListByTenant`, `LoadAll()`. |
| `internal/auth/oidc/kv_store.go` | **New.** `KVOidcProviderStore` against `spi.KeyValueStore`. Single namespace `oidc-providers`. System-tenant context. Stale-index defence on `Get`/`GetByURI`. |
| `internal/auth/oidc/discovery.go` | **New.** `type Discovery interface { Fetch(ctx, uri) (DiscoveryDoc, error) }` and `HTTPDiscovery` impl with `safeDialContext`, redirect-disabled. |
| `internal/auth/oidc/ssrf.go` | **New.** `validateRegisterURI(...)` for register-time UX. `safeDialContext(allowPrivate bool)` for fetch-time security. IPv4 + IPv6 ranges per D10. |
| `internal/auth/oidc/jwks_source.go` | **New.** `providerKeySource` wraps `HTTPJWKSSource` with provider lifecycle gate. |
| `internal/auth/oidc/registry.go` | **New.** `Registry` — provider main map + per-provider key source + `kid`-fast-path with self-heal (D6). Subscribes to `oidc.providers`. `ResolveKey(kid, iss)` returns one of the four D3 sentinels. **`EvictKidEntry(kid, providerRef)`** for self-heal. **`WarmJWKSAsync(ctx)`** for Phase-2 (D8). |
| `internal/auth/oidc/validator.go` | **New.** `OIDCValidator` implementing `auth.Validator`. Calls `parseTokenHeader`, performs iat-binding (D17), audience check (D20), Roles extraction (D23), consults `Registry.ResolveKey`, returns D3 sentinels. |
| `internal/auth/oidc/service.go` | **New.** `Service` — orchestrates the 7 lifecycle ops. KV mutation → registry mutation → broadcast (fire-and-forget). |
| `internal/auth/oidc/broadcast.go` | **New.** `topicOidcProviders` constant + envelope type + `Service.broadcast(op, tenantID, uri)` + `Registry.handleBroadcast(payload)`. Single-flight keyed by `(tenantID, uri)` per D18. Top-level `defer recover()`. |
| `internal/auth/oidc/observability.go` | **New.** Metric definitions per D22 wired against `internal/observability`. |
| `internal/auth/oidc/usercontext.go` | **New.** `buildOIDCUserContext(provider *OidcProvider, claims map[string]any, defaultRolesClaim string) (*spi.UserContext, error)` — implements D23 namespacing + roles parsing. |
| `internal/auth/oidc/audit.go` | **New.** `EmitOwnershipTransition(slog, fromTenant, toTenant spi.TenantID, uri, providerUUID string)` — D25 audit log. |
| `internal/auth/oidc/*_test.go` | **New.** Unit tests per component; `fixture_test.go` exports `FixtureIdP`. |
| `internal/domain/account/oidc_adapter.go` | **New.** Seven HTTP→`oidc.Service` adapters. PATCH adapter uses `map[string]json.RawMessage` decode per D24. Authz: ROLE_ADMIN on mutating; tenant-member on List per D21. |
| `internal/domain/account/oidc_adapter_test.go` | **New.** Adapter-level unit tests. |
| `internal/domain/account/handler.go` | Replace lines 95-121 with one-line delegations to `oidc_adapter.go`. Add `oidcService *oidc.Service` field; wire through `NewHandler`. |
| `internal/api/server.go:507-561` | No change — existing nil-check pattern delegates correctly once `Handler.oidcService` is non-nil. |
| `app/app.go` (around line 236, post trusted-key-store bootstrap) | Phase-1 warmup: `oidc.NewKVProviderStore(systemCtx, kvStore)` → `oidc.NewRegistry(...)` → `registry.LoadProvidersFromKV(systemCtx)`. Construct chain: `oidcValidator := oidc.NewValidator(oidcRegistry, cfg.IAM.OIDC.RolesClaim)`; `chainedValidator := auth.NewChainedValidator(jwksValidator, oidcValidator)`. Wire into HTTP and gRPC auth middleware. Phase-2 warmup: `go registry.WarmJWKSAsync(systemCtx)` (post-listener). Broadcaster nil-check at startup (D7) — `os.Exit(1)` if nil. |
| `app/config.go` | Add `OIDCConfig` struct + `OIDC OIDCConfig` field on `IAMConfig`. Populate from 6 env vars in `DefaultConfig()`. |
| `api/openapi.yaml` | Remove the seven `$ref: "#/components/responses/NotImplemented"` declarations. Reconcile prose: `SUPER_USER` → `ROLE_ADMIN` on the 6 mutating operations; `List` prose updated to "any authenticated tenant member." Add `expectedAudiences: array(string)` to `RegisterOidcProviderRequestDto` and `UpdateOidcProviderRequestDto` (and the response DTO). Add `rolesClaim: string` (optional) to both request DTOs and the response. **Fix `issuers` description on both DTOs** (currently: *"When set to null or empty array, the 'iss' claim validation is skipped"*) → *"When omitted, null, or empty array, the 'iss' claim must match the provider's discovery-document `issuer` field."* **Drop `minItems: 1` from `issuers`** in both DTOs (pre-existing self-contradiction with the "or empty array" prose). |
| `api/generated.go` | Regenerated. |
| `internal/e2e/parity/oidc.go` | **New.** 50 `RunOidc*` scenarios per §11. |
| `e2e/parity/registry.go` | Register the new scenarios in `allTests`. |
| `cmd/cyoda/help/content/config/oidc.md` | **New.** 6 env vars + behaviour notes. |
| `cmd/cyoda/help/content/errors/OIDC_PROVIDER_DUPLICATE.md` | **New.** |
| `cmd/cyoda/help/content/errors/OIDC_PROVIDER_NOT_FOUND.md` | **New.** |
| `cmd/cyoda/help/content/errors/OIDC_PROVIDER_INACTIVE.md` | **New.** |
| `cmd/cyoda/help/content/errors/OIDC_SSRF_BLOCKED.md` | **New.** |
| `cmd/cyoda/help/content/errors/OIDC_AUDIENCE_MISMATCH.md` | **New.** |
| `cmd/cyoda/help/content/errors/OIDC_TOKEN_PRE_TRANSITION.md` | **New.** Documents the 30s skew window. |
| `cmd/cyoda/help/content/errors/OIDC_DISCOVERY_FAILED.md` | **New.** |
| `cmd/cyoda/help/content/errors/OIDC_JWKS_UNAVAILABLE.md` | **New.** |
| `internal/common/error_codes.go` | Add 8 new codes under the `OIDC_*` family. Convention note: existing precedent is subsystem-prefix without an `AUTH_` wrapper (`KEYPAIR_NOT_FOUND`, `TRUSTED_KEY_NOT_FOUND`); `OIDC_*` follows that. Validation-time codes (`OIDC_AUDIENCE_MISMATCH`, `OIDC_TOKEN_PRE_TRANSITION`, `OIDC_DISCOVERY_FAILED`, `OIDC_JWKS_UNAVAILABLE`) share the prefix to keep the help-topic tree contiguous. |
| `README.md` | New "OIDC Provider Configuration" subsection in config-reference. |
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
              │                                                           │
              ▼                                                           ▼
   ┌──────────────────────┐    ErrUnknownKID only                ┌────────────────────────┐
   │ JWKSValidator        │ ──────────────────────────────────►  │ OIDCValidator          │
   │ (first-party)        │                                      │ (iat ± 30s + aud +     │
   │ returns one of:      │    other sentinels = hard-fail       │  D23 UserContext +     │
   │  ErrUnknownKID       │                                      │  Registry.ResolveKey)  │
   │  ErrIssuerMismatch   │                                      │ returns same 4         │
   │  ErrSignatureFailure │                                      │ sentinels +            │
   │  ErrClaimsFailure    │                                      │ ErrTokenPreTransition  │
   └──────────────────────┘                                      │ ErrJWKSUnavailable     │
                                                                 └───────────┬────────────┘
                                                                             │
                                                          ┌──────────────────▼──────────┐
                                                          │ Registry                    │
                                                          │  - providers[T][uri]        │
                                                          │  - sources[T][uri]          │
                                                          │  - kidIndex (self-healing)  │
                                                          └─────┬────────────────┬──────┘
                                                                │                │
                          ┌─────────────────────────────────────▼──┐  ┌──────────▼──────────┐
                          │ KVOidcProviderStore (system tenant)    │  │ ClusterBroadcaster  │
                          │  ns: oidc-providers                    │  │  topic              │
                          │   key: <T>:<provider-uuid>             │  │   "oidc.providers"  │
                          │   key: <T>:uri:<sha256(uri)>           │  │  single-flight key  │
                          └────────────────────────────────────────┘  │  = (T, uri)         │
                                                                      │  fire-and-forget    │
                                                                      └──────────┬──────────┘
                                                                                 │
                                              ┌──────────────────────────────────┘
                                              │
                          ┌───────────────────▼─────────────────┐
                          │ Service                              │
                          │  (Register/Update/Invalidate/        │
                          │   Reactivate/Delete/Reload[All])     │
                          │  KV mutate → registry mutate →       │
                          │  broadcast (no-fail)                 │
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
    RolesClaim         *string    `json:"rolesClaim,omitempty"`        // D23; nil = use global default
    InvalidatedAt      *time.Time `json:"invalidatedAt,omitempty"`
    CreatedAt          time.Time  `json:"createdAt"`           // load-bearing for D17 iat-binding
    OwnerLegalEntityID uuid.UUID  `json:"ownerLegalEntityId"`
}

func (p *OidcProvider) Active() bool { return p.InvalidatedAt == nil }
```

KV layout (single namespace, composite keys; store runs under system tenant):

```
namespace        key                                  value
oidc-providers   <tenantID>:<provider-uuid>           JSON(OidcProvider)
oidc-providers   <tenantID>:uri:<sha256(URI-hex)>     <provider-uuid>
```

**Register race (D11).** Both nodes/clients see no index entry; both `Put` the index (one overwrites the other); both `Put` blobs (two distinct UUIDs); after `Put` index, the Service re-reads the index value; if it doesn't equal the value just written, the loser deletes its own blob and returns 409. Both clients get consistent answers; KV ends in single-blob state.

**Stale-index defence on every read.** `Get(tenantID, providerID)` reads the blob and verifies `OwnerLegalEntityID` matches the requesting tenant. `GetByURI(tenantID, uri)` reads index → blob and verifies blob's tenant matches the index-key tenant prefix. Mismatch → orphan; best-effort `Delete`; return `ErrProviderNotFound`.

### 3.4 REST surface

DTOs extended per rev. 3:

- `RegisterOidcProviderRequestDto`: `{wellKnownConfigUri, issuers?, expectedAudiences?, rolesClaim?}`
- `UpdateOidcProviderRequestDto`: `{issuers?, expectedAudiences?, rolesClaim?}`
- `ReactivateOidcProviderRequestDto`: `{reactivateKeys?}` (unchanged)
- `OidcProviderResponseDto`: `{id, wellKnownConfigUri, issuers?, expectedAudiences?, rolesClaim?, active, createdAt}`

Wire matrix:

| Method | Path | Handler | Authz | Success | Errors |
|---|---|---|---|---|---|
| POST | `/oauth/oidc/providers` | `RegisterOidcProvider` | ROLE_ADMIN | 200 `OidcProviderResponseDto` | 400 `OIDC_PROVIDER_DUPLICATE`; 400 `BAD_REQUEST`; 400 `OIDC_SSRF_BLOCKED`; 403; 401 |
| GET | `/oauth/oidc/providers?activeOnly=<bool>` | `ListOidcProviders` | **any authenticated tenant member (D21)** | 200 `[]OidcProviderResponseDto` | 401 |
| PATCH | `/oauth/oidc/providers/{id}` | `UpdateOidcProvider` | ROLE_ADMIN | 200 `OidcProviderResponseDto` | 404 `OIDC_PROVIDER_NOT_FOUND`; 409 `OIDC_PROVIDER_INACTIVE`; 400; 403; 401 |
| POST | `/oauth/oidc/providers/{id}/invalidate` | `InvalidateOidcProvider` | ROLE_ADMIN | 200 empty | 404; 403; 401 (idempotent) |
| POST | `/oauth/oidc/providers/{id}/reactivate` | `ReactivateOidcProvider` | ROLE_ADMIN | 200 `OidcProviderResponseDto` | 404; 403; 401 (idempotent) |
| DELETE | `/oauth/oidc/providers/{id}` | `DeleteOidcProvider` | ROLE_ADMIN | 200 empty | 404; 403; 401 |
| POST | `/oauth/oidc/providers/reload` | `ReloadOidcProviders` | **explicit in-code** ROLE_ADMIN | 200 empty | 403; 401 |

Token-validation-time errors that surface to the bearer-auth middleware:

| Sentinel | Wire | Meaning |
|---|---|---|
| `ErrUnknownKID` | 401 (only after chain exhaustion) | Neither validator recognised the `kid`. Includes the Phase-2-pending cold-start window (D8). |
| `ErrIssuerMismatch` | 401 with code `OIDC_*` family | A validator's KeySource resolved the `kid` but the `iss` claim does not match. |
| `ErrSignatureFailure` | 401 | Signature verification failed. Triggers `kidIndex` self-heal (D6). |
| `ErrClaimsFailure` | 401 + subcode | `exp`/`nbf`/`aud`/`sub`/alg failure. `aud` failure surfaces `OIDC_AUDIENCE_MISMATCH`. |
| `ErrTokenPreTransition` | 401 + code `OIDC_TOKEN_PRE_TRANSITION` | `iat < provider.CreatedAt - 30s` (D17). |
| `ErrJWKSUnavailable` | 503 + Retry-After | Transient JWKS-endpoint failure; client should retry. |

## 4. Runtime mechanics

### 4.1 Registry

```go
type Registry struct {
    mu        sync.RWMutex
    providers map[spi.TenantID]map[string]*OidcProvider // by wellKnownConfigUri
    sources   map[spi.TenantID]map[string]*providerSource // includes cached DiscoveryDoc
    kidIndex  map[string][]providerRef                  // self-healing per D6

    store        OidcProviderStore
    discovery    Discovery
    broadcast    spi.ClusterBroadcaster
    singleflight *singleflightDebouncer  // keyed by (T, uri) — D18
    clock        func() time.Time
    metrics      *Metrics  // D22
    logger       *slog.Logger
}

type providerSource struct {
    keySource     auth.KeySource
    discoveryDoc  *DiscoveryDoc          // nil during Phase-2-pending (D8)
}

type providerRef struct {
    tenant spi.TenantID
    uri    string
}

type KeyResolution struct {
    PublicKey          *rsa.PublicKey
    Provider           *OidcProvider
    WellKnownConfigURI string
}

func (r *Registry) ResolveKey(kid, iss string) (*KeyResolution, error)
```

`ResolveKey` semantics — explicit error-disposition matrix:

1. Hot path: `kidIndex[kid]` → candidate refs.
2. For each candidate ref → look up provider. If provider's `sources[T][uri].discoveryDoc == nil` (Phase-2-pending), this candidate contributes nothing; continue. Otherwise apply the iss-validation rule (D17):
   - If `provider.Issuers` non-empty and `iss ∈ Issuers` → iss-eligible candidate.
   - If `provider.Issuers` empty and `iss == providerSource.discoveryDoc.Issuer` → iss-eligible candidate.
   - Otherwise → candidate not iss-eligible.
3. **Disposition:**
   - At least one iss-eligible candidate whose `source.GetKey(kid)` returns a public key → take it; if signature verification by the caller fails, caller invokes `EvictKidEntry(kid, ref)`.
   - At least one iss-eligible candidate but all their sources return transient errors (network/timeout) → `ErrJWKSUnavailable` (503).
   - No iss-eligible candidates after considering all kidIndex entries → `ErrIssuerMismatch` if at least one candidate was kid-eligible-but-iss-rejected; else fall through to cold path.
   - Cold path: kidIndex miss → global iteration. Same disposition matrix. On a successful resolution, populate `kidIndex[kid] += ref`. Cold-path miss with no providers in the registry, or all candidates Phase-2-pending → `ErrUnknownKID`.

This matrix resolves the rev. 2 §4.1 hand-wave "spelled out for the implementer in §5".

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

    // D18: single-flight keyed by (tenantID, uri) regardless of op.
    // This serializes reload/invalidate for the same provider locally.
    // Inter-node convergence is provided by periodic reload_all and by D11 read-time validation.
    var key string
    if env.Op == "reload_all" {
        key = "*"
    } else {
        key = env.TenantID + ":" + env.URI
    }

    r.singleflight.Dispatch(key, func() {
        switch env.Op {
        case "reload":
            r.reloadOne(spi.TenantID(env.TenantID), env.URI)
        case "invalidate":
            r.invalidateOne(spi.TenantID(env.TenantID), env.URI)
        case "reload_all":
            r.reloadAll()
        }
    })
}
```

`singleflightDebouncer.Dispatch(key, fn)`: if no work for `key`, spawn one goroutine running `fn`; if work IS in flight for `key`, drop the call (logged at DEBUG). Small wrapper around a `map[string]struct{}` guarded by a mutex; not `golang.org/x/sync/singleflight` (which queues callers for results we don't need).

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

1. `parseTokenHeader(tokenString)` → `kid, alg, iss, aud, exp, iat, err`. Alg not in allow-list → `ErrClaimsFailure` (subcode `unsupported_alg`). `exp` already past → `ErrClaimsFailure` (subcode `expired`).
2. `r.ResolveKey(kid, iss)` → `(*KeyResolution, error)`. Propagate `ErrUnknownKID` (chain fall-through), `ErrIssuerMismatch` (hard-fail), `ErrJWKSUnavailable` (hard-fail).
3. **D17 `iat` binding with 30s skew.** If `iat < resolution.Provider.CreatedAt.Unix() - 30` → return `ErrTokenPreTransition`. Audit log fields: provider UUID, tenant, kid; **never the token or iss**.
4. Verify signature against `resolution.PublicKey`. Failure → return `ErrSignatureFailure`. **On signature failure, call `r.EvictKidEntry(kid, providerRef)`** to self-heal D6.
5. Standard claims: `exp`, `nbf` (30s skew) → `ErrClaimsFailure` on failure.
6. **D20 audience check.** If `resolution.Provider.ExpectedAudiences` is non-empty, require token's `aud` to match (RFC 7519 §4.1.3). Failure → `ErrClaimsFailure` (subcode `audience`).
7. **D23 UserContext extraction.** `sub` claim must be present and non-empty → else `ErrClaimsFailure` (subcode `missing_sub`). Build:
   - `UserID = "oidc:" + resolution.Provider.ID.String() + ":" + sub`
   - `UserName = sub`
   - `Tenant.ID = resolution.Provider.OwnerLegalEntityID.String()`
   - `Roles` = parse from claim named by `provider.RolesClaim` if non-nil else `defaultRolesClaim` (from `CYODA_OIDC_ROLES_CLAIM`, default `"roles"`). Accept both array-of-strings and space-delimited string form. Missing/empty → empty roles slice.
   - `caas_user_id`, `caas_org_id`, `tid`, `tenant`, `org` claims explicitly ignored.

## 5. Lifecycle behaviour (per operation)

Skeleton for every mutating op: validate input → authz → tenancy guard → store mutation → registry mutation → broadcast → respond. Store mutation precedes registry mutation; broadcast is best-effort, never fails the response (D7).

### 5.1 Register

1. `auth.RequireAdmin(w, r)`.
2. **D24 decode pattern:** read body into `map[string]json.RawMessage`; validate required fields present; decode each field. Required: `wellKnownConfigUri` (non-empty, ≤1000 chars, valid absolute URL, scheme matches `CYODA_OIDC_REQUIRE_HTTPS`). Optional with validation: `issuers` (≤10, each ≤1000, dedup), `expectedAudiences` (≤10, each ≤1000, dedup), `rolesClaim` (≤100 chars). **Register-time SSRF gate (D10):** DNS-resolve hostname, reject if in blocklist (unless allow-override).
3. `tenantID := tenantFromCtx(r)`.
4. KV: `Get("oidc-providers", "<T>:uri:<sha256>")`. If present → 400 `OIDC_PROVIDER_DUPLICATE`.
5. KV: `Put` index `<T>:uri:<sha256>` → `<provider-uuid>`. KV: `Put` blob `<T>:<provider-uuid>` → JSON(OidcProvider).
6. **Race-validation re-read (D11):** `Get("oidc-providers", "<T>:uri:<sha256>")`. If value ≠ `<provider-uuid>`: `Delete` our own blob, return 409 `OIDC_PROVIDER_DUPLICATE`.
7. **D25 ownership-transition check:** look up the URI prefix `:uri:<sha256>` across all known tenants. If a different `<tenantID>` previously held this URI (visible via the kv.List enumeration at warmup, kept as an in-memory `previousOwners map[uriHash]tenantID` cache), emit `slog.Info("oidc.ownership_transition", from_tenant=<prev>, to_tenant=<T>, wellknown_uri_hash=<sha256>, new_provider_uuid=<uuid>)`. Update `previousOwners[uriHash] = T`.
8. `registry.reloadOne(tenantID, uri)` — synchronous discovery + JWKS fetch. Failures WARN-logged; provider stays registered.
9. `broadcast({op: "reload", t: tenantID, u: uri})` — fire-and-forget (D7); no error to handle, no rollback to perform.
10. Respond 200 with `OidcProviderResponseDto`.

### 5.2 Update issuers / audiences / rolesClaim

1. `auth.RequireAdmin`.
2. KV `Get` by `(tenantID, providerID)` with stale-index defence. Missing → 404 `OIDC_PROVIDER_NOT_FOUND`.
3. If `InvalidatedAt != nil` → 409 `OIDC_PROVIDER_INACTIVE`.
4. **D24 decode + tri-state:** read body into `map[string]json.RawMessage`. For each of `issuers`, `expectedAudiences`, `rolesClaim`:
   - Key absent → field unchanged.
   - Key present:
     - `issuers` / `expectedAudiences`: `json.Unmarshal` into `[]string`. Both `null` and `[]` parse to a non-nil empty slice (normalize the nil-after-null case explicitly); both clear the field at runtime per D17 (discovery-doc fallback).
     - `rolesClaim`: `json.Unmarshal` into `*string`. `null` → nil pointer (revert to global default); non-empty string → override.
   - Validate per the §5.1 step 2 rules.
5. KV `Put` updated blob.
6. `registry.reloadOne(tenantID, uri)` with **JWKS cache flush** and **discovery doc refetch**. Discovery refetch is significant for D17 — if the IdP migrated its `issuer`, this is the operational path to pick up the change. (See §9 "Operational gaps" for the case where the IdP migrates without admin awareness.)
7. Broadcast reload.
8. Respond 200 with DTO.

### 5.3 Invalidate

1. `auth.RequireAdmin`.
2. KV `Get` with stale-index defence. Missing → 404.
3. Already invalidated: WARN log, 200 idempotent, no broadcast.
4. KV `Put` with `InvalidatedAt = time.Now()`.
5. Registry: drop provider entry + KeySource. Evict matching `kidIndex` candidates.
6. `broadcast({op: "invalidate", t: tenantID, u: uri})`.
7. Respond 200 empty.

### 5.4 Reactivate

1. `auth.RequireAdmin`.
2. KV `Get` with stale-index defence. Missing → 404.
3. Already active: INFO log, 200 with current DTO, no broadcast.
4. KV `Put` with `InvalidatedAt = nil`.
5. **D19 conditional sync.** `reactivateKeys=true` (default): `registry.reloadOne(tenantID, uri, syncKeys=true)` — performs JWKS fetch; **if fetch succeeds with HTTP 200 + valid JSON**, sync the cache (drop locally cached keys not in remote JWKS); **if fetch fails**, leave cache as-is, log WARN. `reactivateKeys=false`: discovery only, no JWKS touch.
6. Broadcast reload.
7. Respond 200 with DTO.

### 5.5 Delete

1. `auth.RequireAdmin`.
2. KV `Get` with stale-index defence. Missing → 404.
3. If active, perform invalidate semantics inline (no separate broadcast).
4. KV: `Delete` blob and index.
5. Registry: drop provider entry, evict matching `kidIndex` candidates.
6. `broadcast({op: "invalidate", t: tenantID, u: uri})`.
7. Respond 200 empty.

### 5.6 Reload-all

1. **Explicit in-code ROLE_ADMIN check.**
2. `store.LoadAll()` → populate fresh `providers` / `sources` / `kidIndex`.
3. Swap under `mu.Lock()`.
4. `broadcast({op: "reload_all"})`.
5. Respond 200 empty.

### 5.7 List

1. **Any authenticated tenant member (D21).** `tenantID := tenantFromCtx(r)`. If no tenant in context → 401.
2. `store.ListByTenant(tenantID, activeOnly)`.
3. Map to DTOs.
4. Respond 200.

## 6. Startup (D8)

In `app/app.go`, after the `KVTrustedKeyStore` bootstrap:

```go
// D7 invariant — broadcaster MUST be configured at this point
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
chainedValidator := auth.NewChainedValidator(jwksValidator, oidcValidator)
// wire chainedValidator into HTTP auth middleware + gRPC interceptor

// (...HTTP listener binds here, well before warm-JWKS finishes...)

// Phase 2: asynchronous, post-listener
go oidcRegistry.WarmJWKSAsync(systemCtx)
```

`LoadProvidersFromKV` does `kv.List("oidc-providers")` (one call, system-tenant context), parses `<T>:<provider-uuid>` keys (skipping the `<T>:uri:<sha256>` index entries), populates `providers` map. Also builds the `previousOwners` cache for D25.

`WarmJWKSAsync` spawns goroutines bounded by `runtime.NumCPU()` worker pool. Per-provider goroutine fetches discovery + JWKS via the safedialer'd client, populates `sources[T][uri]` (including `discoveryDoc`) and `kidIndex` for that provider. Per-provider failures: WARN-log, do not retry within this warmup pass.

**Cold-start window behaviour (D8 + D17):** during Phase 2, tokens for a not-yet-warmed provider get `ErrUnknownKID` (chain fall-through → 401), NOT `ErrIssuerMismatch`. The rationale is correctness: we genuinely don't know yet whether the kid would have resolved, so falling through is the conservative choice — a token that would legitimately validate gets a 401 during the cold-start window, which is the same outcome as before the provider was registered. This is a documented operational characteristic, not a defect.

## 7. Configuration

| Env var | Type | Default | Purpose |
|---|---|---|---|
| `CYODA_OIDC_REQUIRE_HTTPS` | bool | `true` | Reject `http://` `wellKnownConfigUri` at Register-time. |
| `CYODA_OIDC_CONNECT_TIMEOUT_MS` | int | `5000` | TCP connect timeout. |
| `CYODA_OIDC_SOCKET_TIMEOUT_MS` | int | `5000` | HTTP read timeout. |
| `CYODA_OIDC_CONNECTION_REQUEST_TIMEOUT_MS` | int | `5000` | Pool-checkout timeout. |
| `CYODA_OIDC_ALLOW_PRIVATE_NETWORKS` | bool | `false` | Test/dev override: disable register-time AND fetch-time SSRF blocklists. |
| `CYODA_OIDC_ROLES_CLAIM` | string | `"roles"` | Default claim name to extract roles from for OIDC tokens. Per-provider override available via the `rolesClaim` field. |

Wired into `app/config.go::IAMConfig.OIDC` (new `OIDCConfig` struct), populated in `DefaultConfig()`. Help topic at `cmd/cyoda/help/content/config/oidc.md`. README config-reference section updated.

## 8. Security

| Aspect | Mechanism | Verification |
|---|---|---|
| Authn | Existing bearerAuth middleware — no new wiring | All 7 endpoints already declare `security: [bearerAuth: []]` |
| Authz (mutating) | `auth.RequireAdmin(w, r)` on each handler; explicit in-code ROLE_ADMIN check on `Reload` | 6 negative tests (non-admin → 403) |
| Authz (read) | Any authenticated tenant member (D21); tenant scope enforced via `tenantFromCtx` + `OwnerLegalEntityID` | Tenant-member-without-ROLE_ADMIN can list own providers; cross-tenant member cannot see others |
| Tenant isolation (mgmt API) | Composite-key prefix `<tenantID>:`; stale-index defence on every `Get` | Cross-tenant test: tenant-A's provider ID returns 404 to tenant-B's caller |
| Tenant isolation (token validation) | `UserContext.Tenant = OwnerLegalEntityID` from the resolving provider; `tid`/`tenant`/`org` claims in the token are ignored | Test: token signed by tenant-A's IdP with a `tid=B` claim still yields `Tenant=A` |
| UserContext cross-IdP isolation (D23) | `UserID` namespaced `oidc:<providerUUID>:<sub>`; `caas_user_id` and `caas_org_id` claims explicitly ignored (attacker-controlled) | Test: two IdPs with colliding `sub` values yield distinct `UserID`s; test: token with forged `caas_user_id` is ignored, UserID still derived from `sub` |
| Cross-tenant escalation via re-registration (D17) — **accidental case** | `iat < provider.CreatedAt - 30s` rejects with `ErrTokenPreTransition` | Test: tenant A registers, JWT issued, A deletes, B registers same URI, A's old JWT rejected |
| Cross-tenant escalation via re-registration (D17) — **adversarial case** | **Not defended at the validator.** The IdP operator can mint a fresh token with `iat = time.Now()` after observing the handover; this is accepted. Observability via D25 ownership-transition INFO audit log gives operators a signal. | Test: D25 audit log fires on the handover scenario with the right fields |
| Cross-tenant routing via kid collision (D17) | Mandatory `iss` validation at resolution; when `Issuers` empty AND DiscoveryDoc fetched, fall back to `DiscoveryDoc.Issuer` exact match | Test: two AWS Cognito tenants with overlapping kid namespace; tokens route to correct tenant |
| Chain-fall-through escalation (D3) | Four distinct sentinels; chain falls through ONLY on `ErrUnknownKID` | Test: first-party kid + foreign iss → 401 with `ErrIssuerMismatch`, OIDCValidator NOT consulted |
| kid-cache poisoning (D6 self-heal) | Failed signature evicts `kidIndex[kid]` providerRef | Test: malicious tenant publishes first-party kid in JWKS; first-party validation continues; malicious entry evicted |
| SSRF defence — register-time (D10) | DNS-resolve + CIDR check on hostname | Test: `http://169.254.169.254/.well-known/...` → 400 `OIDC_SSRF_BLOCKED` |
| SSRF defence — fetch-time (D10) | `safeDialContext` re-checks every dial against IPv4+IPv6 blocklist | Test: fast-flux DNS returning public IP at register, `127.0.0.1` at fetch → fetch fails |
| SSRF defence — redirects (D10) | `http.Client` with `CheckRedirect` returning `http.ErrUseLastResponse` (no follow) | Test: discovery 302 → internal URL; fetch fails |
| Per-provider audience (D20) | `ExpectedAudiences` checked at OIDCValidator step 6; empty = unchecked | Test: aud=["api1"] rejects aud=api2; empty list accepts any |
| Cold-start (Phase-2-pending) — no escalation | Missing discovery doc → `ErrUnknownKID` (chain fall-through), never silent accept | Test: token arrives before Phase 2 completes for its provider → 401, NOT 200 |
| Log hygiene (Gate 3) | Never log tokens, JWT contents, private keys, JWKS payloads, `iss` of rejected tokens. **OK**: `kid`, `wellKnownConfigUri`, tenantID, provider UUID. | Pre-PR grep gate over `internal/auth/oidc/` |
| Output sanitization | 4xx with domain code + short message; 5xx with generic + ticket UUID; no upstream error bodies echoed | Test: JWKS-fetch 500 response body contains only ticket UUID |
| Race-safety | `mu.RLock`/`mu.Lock` discipline per `.claude/rules/go-mutex-discipline.md`. Broadcast handlers idempotent + `defer recover()` (D18). | Race detector run pre-PR per `.claude/rules/race-testing.md` |
| Broadcast handler isolation (D18) | `defer recover()`; single-flight by `(T, uri)`; bounded concurrency | Test: induced panic in OIDC handler does NOT kill `modelcache` invalidation delivery on the same node |
| Observability (D22) | Metrics via `internal/observability`; non-resolver kid-misses at DEBUG with per-provider INFO summary every 60s | Verify metrics in E2E; grep `Level=WARN` on hot path returns nothing |
| Ownership-transition audit (D25) | INFO log on Register where the URI was previously owned by a different tenant | Test: tenant-A delete + tenant-B register cycle emits the log line with both tenant IDs |

## 9. Deliberate divergences from cyoda-cloud

This register captures every wire-, semantics-, or data-model-level divergence from the cyoda-cloud reference implementation. Each is intentional and explained; reviewers should treat additions to this list as requiring explicit justification.

| # | Surface | cyoda-cloud behaviour | cyoda-go behaviour | Decision ref | Rationale |
|---|---|---|---|---|---|
| DV1 | Update inactive provider | `IllegalStateException` → 5xx generic | 409 + `OIDC_PROVIDER_INACTIVE` | D5 | Matches our 4xx-domain-detail convention; 5xx generic would violate Gate 3 |
| DV2 | Broadcast failure handling | `notifyAllNodes(awaitResult=true)` blocks for ack; failure → 500 | None — broadcast is fire-and-forget by SPI contract; management API ignores delivery state | D7 | The SPI exposes no failure surface; per `feedback_engine_no_special_rights` we do not add `BroadcastSync`. The corresponding cyoda-cloud parity test does not port. |
| DV3 | Audience enforcement | Global audience (via `CYODA_JWT_AUDIENCE` equivalent), or no per-provider audience field | Per-provider `ExpectedAudiences []string` on the OIDC entity; empty = unchecked-and-documented | D20 | Global audience is the wrong default for external IdPs (they issue tokens with their own application-id audience) |
| DV4 | iat-binding scope (D17 accidental-only) | None — cyoda-cloud does not have iat-binding | Closes accidental cross-tenant spillover; does NOT defend against adversarial IdP-operator; surfaces ownership transitions via D25 INFO audit | D17 + D25 | Honest scope; per `feedback_gate6_no_followups` the mitigation log is in scope, not a follow-up |
| DV5 | UserContext extraction for OIDC tokens | Cyoda-cloud has its own role-extraction logic; uses the IdP's user identity directly | `UserID = "oidc:<providerUUID>:<sub>"` namespaced; per-provider `RolesClaim` + global `CYODA_OIDC_ROLES_CLAIM` default | D23 | Cross-provider `sub` collision is a real attack vector; configurable role-claim accommodates IdP variation (Auth0 / Cognito / Keycloak) |
| DV6 | List endpoint authz | Allows tenant-scoped reads for authenticated users | Allows tenant-scoped reads for authenticated tenant members (parity, not divergence; recorded for the audit trail) | D21 | Match |
| DV7 | OpenAPI request DTOs | `RegisterOidcProviderRequestDto` and `UpdateOidcProviderRequestDto` carry `wellKnownConfigUri` and `issuers` only | rev. 3 adds `expectedAudiences` and `rolesClaim` to both request DTOs and the response DTO | D20 + D23 | Additive divergence in OpenAPI; clients of cyoda-cloud's OpenAPI carry over without code changes (new fields are optional). |

**Operational gaps (not divergences):** if an external IdP migrates its `issuer` field in the discovery document without an admin-triggered `PATCH` or `POST /reload`, cyoda-go will reject the new tokens with `ErrIssuerMismatch` until the admin acts. This is a known operational pattern; cyoda-cloud has the same constraint. Mitigation: periodic operator dashboard monitoring of the `oidc_jwks_fetch_error_total{outcome=...}` metric and the `kid`-miss INFO rollup.

## 10. Acceptance criteria mapping

| Issue body item | Where covered |
|---|---|
| Brainstorm + spec under `docs/superpowers/specs/` | This document |
| Plan under `docs/superpowers/plans/` | Next step (writing-plans skill) |
| All 7 handlers return real responses | §3.1 + §5 |
| All 27 ported test scenarios pass across memory/sqlite/postgres/cassandra | §11 (cyoda-cloud's count is 27; rev. 3 adds 23 cyoda-go-specific for a total of 50) |
| `JWKSValidator` extended multi-issuer with per-provider lifecycle | §4.3 (chained, four-sentinel — D3) |
| `CYODA_OIDC_*` env vars documented in `cmd/cyoda/help/content/config/` | §7 + §12 |
| Cross-node cache invalidation verified via multi-node E2E harness | §11 broadcast-eviction scenario |
| No credentials / tokens / private keys in any log line | §8 + pre-PR grep gate |
| 501 declarations removed from OpenAPI for these 7 paths | §3.1 + CHANGELOG `### Changed` |
| `h.stub(w, r)` calls removed for OIDC handlers | §3.1 + §5 |
| Decomp spec §3.4 updated to v0.8.0 | Already done in tree (amendment at head); §1 records the satisfaction |

## 11. Test plan

All scenarios run as parity tests in `internal/e2e/parity/oidc.go`, registered in `e2e/parity/registry.go::allTests`. Each parity backend (memory, sqlite, postgres) picks them up; cassandra inherits on its next dependency update.

**Authoritative inventory (59 scenarios — 27 ported from cyoda-cloud + 32 cyoda-go-specific):**

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
| 21 | Cycle `issuers` list (remove issuer1, add back, set null) | port |
| **Key rotation / revocation (5)** | | |
| 22 | Key `validTo` past → rejected | port |
| 23 | Key `validTo` future → accepted | port |
| 24 | Key `validTo` null → accepted | port |
| 25 | Invalidated key → rejected | port |
| 26 | Remote-removal sync via reactivate(keys=true) **success path** (I7 fix) | port + cyoda-go addition |
| **Multi-provider isolation (1)** | | |
| 27 | Two providers, same tenant, keys don't cross-validate | port |
| **Inactive-update divergence (1)** | | |
| 28 | Update inactive → 409 `OIDC_PROVIDER_INACTIVE` (D5/DV1) | cyoda-go |
| **Cross-tenant management isolation (1)** | | |
| 29 | Tenant-A's provider ID → 404 from tenant-B caller | cyoda-go (D1) |
| **Tenant-binding via OwnerLegalEntityID (1)** | | |
| 30 | Token with forged `tid=B` claim still yields `Tenant=A` | cyoda-go (D1) |
| **D17 iat-binding (accidental case) (1)** | | |
| 31 | Delete A → Register B same URI → A's pre-deletion JWT rejected | cyoda-go (D17) |
| **D17 mandatory iss-validation: kid collision (1)** | | |
| 32 | Two providers with overlapping kid namespace; tokens route by iss | cyoda-go (D17) |
| **D17 mandatory iss-validation: empty-Issuers falls back to DiscoveryDoc.Issuer (1)** | | |
| 33 | Provider with empty `issuers`; mismatched iss rejected | cyoda-go (D17) |
| **D17 iat skew (2)** | | |
| 34 | `iat = CreatedAt - 5s` (within 30s skew) → accepted | cyoda-go (D17/I8) |
| 35 | `iat = CreatedAt - 5min` (outside skew) → `ErrTokenPreTransition` | cyoda-go (D17/I8) |
| **D3 four-sentinel chain fall-through (1)** | | |
| 36 | First-party kid + foreign iss → 401 `ErrIssuerMismatch`, OIDCValidator NOT consulted (assertion via mock call counter) | cyoda-go (D3) |
| **D6 kid-cache self-heal (1)** | | |
| 37 | Malicious tenant publishes first-party kid; first-party validation continues; malicious entry evicted | cyoda-go (D6) |
| **D11 register race (2)** | | |
| 38 | Concurrent Register from two clients, same URI → exactly one 200, exactly one 409 | cyoda-go (D11) |
| 39 | Orphan `<T>:uri:...` index entry with no blob → Get/List skip; index cleaned up | cyoda-go (D11) |
| **D8 two-phase warmup (3)** | | |
| 40 | 100 providers, slow JWKS endpoints; listener binds within 1s; tokens validate within 30s | cyoda-go (D8) |
| 41 | Per-provider Phase-2 failure → log WARN, no os.Exit, other providers serve traffic | cyoda-go (D8) |
| 42 | Token arrives during Phase-2-pending for its provider → `ErrUnknownKID` (chain fall-through → 401), NOT `ErrIssuerMismatch` (I2 cold-start contradiction fix) | cyoda-go (D8/I2) |
| **D18 broadcast (3)** | | |
| 43 | Panic in OIDC `handleBroadcast` does NOT kill `modelcache` delivery on same node | cyoda-go (D18) |
| 44 | Single-flight debounce: 10 RELOAD broadcasts for same `(T, uri)` in 100ms → one `reloadOne` invocation | cyoda-go (D18) |
| 45 | Reload + Invalidate broadcasts for same `(T, uri)` serialize through one worker; final state deterministic locally | cyoda-go (D18/I6) |
| **D10 SSRF (3)** | | |
| 46 | Fast-flux DNS: register-time public IP, fetch-time 127.0.0.1 → fetch fails | cyoda-go (D10) |
| 47 | IPv6 link-local `fe80::1` and ULA `fc00::1` → register 400 | cyoda-go (D10) |
| 48 | Discovery 302 → `http://169.254.169.254/...` → fetch fails (no follow) | cyoda-go (D10) |
| **D19 reactivate (1)** | | |
| 49 | Reactivate(keys=true) while JWKS down → cache preserved, WARN, InvalidatedAt cleared | cyoda-go (D19) |
| **D20 audience (1)** | | |
| 50 | Per-provider `expectedAudiences=["api1"]` rejects `aud=api2`; empty list accepts any | cyoda-go (D20) |
| **D23 UserContext (3)** | | |
| (subsumed into 30 + new) | | |
| 51 | Cross-IdP `sub` collision → distinct `UserID`s (namespacing) | cyoda-go (D23) |
| 52 | Per-provider `rolesClaim` override extracts roles from non-default claim (e.g., `cognito:groups`) | cyoda-go (D23) |
| 53 | Roles parsing handles array-of-strings AND space-delimited-string forms | cyoda-go (D23) |
| **D25 ownership transition (1)** | | |
| 54 | Register-after-Delete-by-different-tenant emits `oidc.ownership_transition` INFO log with `{from_tenant, to_tenant, wellknown_uri_hash, new_provider_uuid}` | cyoda-go (D25) |
| **D21 list authz (1)** | | |
| 55 | Non-admin tenant member: GET `/oauth/oidc/providers` → 200; cross-tenant member → empty list | cyoda-go (D21) |
| **State-transition pairs (2)** | | |
| 56 | active → invalidated → deleted: state consistent at each step | cyoda-go |
| 57 | invalidated → reactivated → invalidated: kid cache flushed correctly across the cycle | cyoda-go |
| **E2E coverage (Gate 2) (2)** | | |
| 58 | `TestE2E_OIDC_TokenValidation`: register + mock IdP + GET /me + invalidate + 401 | cyoda-go |
| 59 | `TestE2E_OIDC_MultiNode_Eviction`: memberlist ring + shared KV; register on A, B reflects within one gossip cycle | cyoda-go |

Row-count: 27 ported + 32 cyoda-go-specific = 59. The row numbers in the matrix above are the test IDs (1..59) and are stable; family headers group thematically but do not affect numbering. Per Gate 2: every cyoda-go-specific row is a behaviour change introduced by this PR and must have explicit coverage.

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
// For SSRF tests: an override hook that changes IP between register and fetch
func (f *FixtureIdP) WithFakeDNS(t *testing.T, registerIP, fetchIP net.IP) *FixtureIdP
// For first-party-kid + foreign-iss test: inject a key into LocalKeySource
func (f *FixtureIdP) RegisterFirstPartyKey(t *testing.T, kid string, key *rsa.PrivateKey)
```

Multi-node broadcast-eviction E2E: two in-process cyoda-go instances over a real `gossip_broadcast.Gossip` ring, sharing a memory-plugin KV store.

## 12. Documentation updates (Gate 4)

| File | Action |
|---|---|
| `cmd/cyoda/help/content/config/oidc.md` | **New** — 6 env vars + behaviour notes |
| `cmd/cyoda/help/content/errors/OIDC_PROVIDER_DUPLICATE.md` | **New** — clarifies per-tenant uniqueness scope |
| `cmd/cyoda/help/content/errors/OIDC_PROVIDER_NOT_FOUND.md` | **New** |
| `cmd/cyoda/help/content/errors/OIDC_PROVIDER_INACTIVE.md` | **New** |
| `cmd/cyoda/help/content/errors/OIDC_SSRF_BLOCKED.md` | **New** |
| `cmd/cyoda/help/content/errors/OIDC_AUDIENCE_MISMATCH.md` | **New** |
| `cmd/cyoda/help/content/errors/OIDC_TOKEN_PRE_TRANSITION.md` | **New** — documents the 30s skew |
| `cmd/cyoda/help/content/errors/OIDC_DISCOVERY_FAILED.md` | **New** |
| `cmd/cyoda/help/content/errors/OIDC_JWKS_UNAVAILABLE.md` | **New** |
| `app/config.go` `DefaultConfig()` | Populate `IAMConfig.OIDC` from 6 env vars |
| `README.md` | New "OIDC Provider Configuration" subsection under config-reference |
| `docs/ARCHITECTURE.md` | New §7.3 "OIDC Provider Registry"; existing §7.3 → §7.4 |
| `docs/PRD.md` | Expand §8 with OIDC chaining subsection + external-issuer token-claims example |
| `docs/FEATURES.md` | One bullet under Authentication & Authorization: "OIDC provider per-tenant registry" |
| `CHANGELOG.md` `## [0.8.0]` | `### Added`: OIDC subsystem (per-tenant provider registry, chained multi-issuer JWT validator, cluster broadcast eviction); 6 env vars. `### Changed`: seven `/oauth/oidc/providers/*` endpoints now return real responses (previously 501 `NOT_IMPLEMENTED`); clients that special-cased 501 should update. |
| `COMPATIBILITY.md` | No change — no SPI bump in this PR |
| `docs/adr/0002-federated-identity-provider-architecture.md` | **New** — pins D1 (per-tenant), D2 (KV single-namespace), D3 (chained four-sentinel) |
