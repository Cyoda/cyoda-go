# Issue #284 — OIDC providers subsystem (greenfield)

**Status:** approved (rev. 2, post-review)
**Date:** 2026-06-16
**Issue:** [#284](https://github.com/Cyoda-platform/cyoda-go/issues/284)
**Umbrella:** [`2026-06-04-194-decomposition-design.md`](2026-06-04-194-decomposition-design.md) §3.4 (amended 2026-06-16, pulled into v0.8.0)
**Predecessor:** issue #123 (closed as duplicate; content consolidated into #284)
**Revision note:** rev. 1 introduced an SPI extension (`KeyValueStore.ListNamespaces`) and per-tenant KV namespaces. Fresh-context review demonstrated this misreads the KV-store tenancy model (`KeyValueStore` is tenant-scoped at acquisition; `KVTrustedKeyStore` uses one namespace + composite keys). rev. 2 drops the SPI extension entirely, fixes several cross-tenant escalation paths in the validator chain (deleted-then-re-registered URI promotion; same-`kid` cross-tenant routing; chain-fall-through on iss-mismatch), adds two-phase startup warmup, hardens the SSRF defence to a fetch-time check with IPv6 coverage, makes the (index, blob) write race explicit with a 409-on-inconsistency guarantee, drops the dead `ProviderID` field, and adds per-provider audience enforcement. The decision-table renumbering preserves stable D1/D2/D3 labels (which the ADR cross-references); other D-numbers shifted.

**Scope:** Land a per-tenant OIDC provider registry, the seven REST handlers currently stubbed at HTTP 501 (`internal/domain/account/handler.go:95-121`), a chained multi-issuer JWT validator with four distinct error sentinels, single-topic cluster broadcast for cross-node cache eviction, four `CYODA_OIDC_*` configuration env vars (plus one SSRF-override), telemetry, and a parity test suite (~42 scenarios). Zero changes to any SPI surface. Zero `h.stub` calls remain on the OIDC path; the seven `NotImplemented` declarations come off `api/openapi.yaml`.

---

## 1. Context

cyoda-go exposes the full OpenAPI surface for OIDC provider management at `/oauth/oidc/providers/*` (`api/openapi.yaml:4762-5160`), but every handler is a stub returning `501 NOT_IMPLEMENTED` (`internal/domain/account/handler.go:95-121`, routed via `internal/api/server.go:507-561`). The reference implementation lives in cyoda-cloud:

- `platform-service-iam/src/main/kotlin/com/cyoda/iam/model/entity/JWKOIDCEntity.kt`
- `platform-service-iam/src/main/kotlin/com/cyoda/iam/service/oidc/JWKOIDCService.kt` (`JWKOIDCServiceImpl`)
- `backend/src/main/kotlin/net/cyoda/saas/controller/OIDCProviderController.kt`
- `backend/src/main/kotlin/net/cyoda/saas/iam/OIDCProviderInteractor.kt`
- `platform-service-iam/src/main/kotlin/com/cyoda/iam/service/oidc/notifications/JWKOIDCCacheIntercom.kt`
- `integration-tests/src/test/kotlin/net/cyoda/saas/controller/OIDCProviderControllerIT.kt` (27 tests)

The umbrella decomposition (§3.4) originally deferred this work to v0.9.0; the amendment at the head of that document (2026-06-16) pulls it forward into v0.8.0 as the milestone's headline IAM deliverable. That acceptance-criterion item ("decomp spec updated to v0.8.0") is therefore **already satisfied** in tree; this spec records the satisfaction explicitly.

Today's `JWKSValidator` (`internal/auth/validator.go:15-101`) is single-issuer: it carries one `issuer string` field validated against the token's `iss` claim, and consults one `KeySource`. The HTTPJWKSSource cache is already keyed by `(issuer, kid)` per the closed #97 fix (`internal/auth/http_jwks_source.go:38-41`). **Order of validation matters:** today the signature is verified at line 73 BEFORE the iss-check at line 82. Any change that swaps the iss-mismatch error for a fall-through sentinel must distinguish "kid unknown to me" from "kid known + signature OK + iss mismatch" — these are different cases with different correct hard/soft-fail semantics. D3 honours this.

The `KVTrustedKeyStore` (`internal/auth/kv_trusted_store.go:20, 49-51, 73-106`) is the load-bearing precedent: it uses **one** namespace (`trusted-keys`) with composite keys (`<tenantID>:<kid>`), runs under the system tenant context (`app/app.go:217-222`), and reads back all rows with a single `kv.List("trusted-keys")` at warmup. The SPI's `KeyValueStore` filters every `Put/Get/Delete/List` by the tenantID bound at acquisition (`plugins/postgres/kv_store.go:18-23, 30-42, 44-52, 54-76`). There is no cross-tenant enumeration primitive in the SPI; introducing one would deliberately bypass the isolation contract every plugin enforces. The right shape is the trusted-key precedent — that is what rev. 2 adopts.

Per the umbrella's §4 reconciliation, mutating endpoints require `ROLE_ADMIN`; `List` is open to any authenticated member of the owning tenant (rev. 2 change, see D22). OpenAPI prose currently says `SUPER_USER`; this PR aligns prose to `ROLE_ADMIN` on the 7 operations.

Issue #34 (trusted-key audit) was investigated for overlap and is independent: its scope is the trusted-key HTTP surface hardening, not the OIDC subsystem.

## 2. Decisions

| # | Decision | Choice | Rationale |
|---|---|---|---|
| D1 | Tenancy scope of the OIDC provider registry | **Per-tenant.** Each legal entity owns its providers; management API filters by `tenantFromCtx(r)`; KV records keyed by `(tenantID, ...)`. Token validation iterates providers across all tenants; the resolving provider's `OwnerLegalEntityID` determines the user context's tenant — subject to the cross-tenant escalation guards in D17. | cyoda-go-light deploys into more varied contexts than cyoda-cloud (single-tenant SaaS, on-prem multi-tenant) where per-tenant ownership is the safer default. Validation-side global iteration is unavoidable in any case (tokens carry no tenant claim). |
| D2 | Persistence backend for `OidcProviderStore` | **`KeyValueStore` SPI, single namespace, composite keys.** Namespace `oidc-providers`. Provider blob key: `<tenantID>:<provider-uuid>`. URI uniqueness index key: `<tenantID>:uri:<sha256(wellKnownConfigUri)>`. Store acquired with system-tenant context at startup (matches `app/app.go:217-222`). `LoadAll` enumerates via `kv.List("oidc-providers")` and parses tenantID out of each key. **No SPI extension.** | The trusted-key precedent (`internal/auth/kv_trusted_store.go`) demonstrates the pattern. The KV SPI is already tenant-scoped at acquisition (`plugins/postgres/kv_store.go:13-16` carries `tenantID` and every op filters by it); per-tenant namespaces would either return empty (honest implementation) or require a deliberate isolation bypass. SHA-256 of the URI in the index key avoids cross-plugin key-charset edge cases. Forward-compat `ProviderID` field (rev. 1 D17) is dropped per YAGNI; reinstate when a second `providerId` value lands. |
| D3 | Multi-issuer validator integration shape with four distinct error sentinels | **Chained validators.** New `auth.Validator` interface (`Validate(token) (*spi.UserContext, error)`), new `auth.ChainedValidator`. `JWKSValidator` stays single-issuer with refined error semantics: `ErrUnknownKID` (chain fall-through), `ErrIssuerMismatch` (hard-fail, kid was resolved but `iss` does not match), `ErrSignatureFailure` (hard-fail), `ErrClaimsFailure` (hard-fail). The chain only falls through on `ErrUnknownKID`. The new `oidc.OIDCValidator` returns the same four sentinels. | Today `JWKSValidator.Validate` verifies signature before iss-check (`internal/auth/validator.go:68-83`): a token signed by a key in our KeySource but carrying a foreign `iss` reaches line 82 as "untrusted token issuer". Swapping that to a chain-fall-through sentinel would let a future OIDC validator re-examine a token whose signature is unambiguously first-party — that is iss-mismatch, not unknown-issuer, and the difference must be wire-visible to the chain. The four-sentinel model makes the cases mechanically distinguishable. |
| D4 | Cluster broadcast wire format | **One topic `oidc.providers`** with a JSON envelope `{op: "reload"\|"invalidate"\|"reload_all", tenantID, wellKnownConfigUri?}`. Mirrors `JWKOIDCCacheNotification`; mirrors `modelcache`'s single-topic-per-subsystem convention. | One Subscribe call, one switch. Avoids gossip topic-cardinality bloat. Receiver-side handlers idempotent. |
| D5 | Wire shape for "update inactive provider" | **`HTTP 409 Conflict` + error code `OIDC_PROVIDER_INACTIVE`**. Diverges from cyoda-cloud's `IllegalStateException → 5xx generic`. | Matches cyoda-go's project-wide convention (4xx domain-detail with code; 5xx for server faults — `.claude/rules/error-handling.md`). Surfacing a precondition failure as 5xx would also violate Gate 3 output sanitization. Parity test asserts the cyoda-go wire shape with an explanatory comment recording the deliberate divergence. |
| D6 | Validator key-resolution data structure with self-healing | **`kid`-indexed fast-path inside `Registry`** (`map[kid][]providerCandidate` + provider main map). Global iteration on miss only. **`kidIndex` entries self-heal on signature failure**: when a provider claims a `kid` but `Verify` fails, that providerRef is evicted from `kidIndex[kid]`. | `O(active providers)` on every token is unacceptable for the bearer-auth hot path. Self-healing closes a kid-cache-poisoning vector (a malicious tenant publishing a kid that collides with the first-party validator's namespace could otherwise pin a wrong-tenant candidate). The miss-path retains global iteration for correctness when an unseen kid first appears. |
| D7 | Broadcast failure handling | **Fire-and-forget at the SPI** (`spi.ClusterBroadcaster.Broadcast` is non-blocking). On a broadcaster error or misconfigured broadcaster, the management API returns 500 + ticket UUID per Gate 3. Do **not** add a synchronous-ack SPI method. | cyoda-cloud's `notifyAllNodes(awaitResult=true)` ack semantics are not faithfully portable. Per memory `feedback_engine_no_special_rights`. The cyoda-cloud parity test asserting "intercom failure → 500" is rewritten to inject a `Broadcast`-error path. |
| D8 | Two-phase startup warmup | **Phase 1 (synchronous, blocking listener):** `kv.List("oidc-providers")` → parse → populate `Registry.providers` and (empty) `kidIndex`. Cheap, no network I/O, must complete before HTTP listener binds. **Phase 2 (asynchronous, post-listener):** per-provider goroutine fetches discovery doc + JWKS, populates `Registry.sources` and `kidIndex`. On warmup failure for a single provider: WARN-log, continue. Tokens arriving before Phase 2 completes for the relevant provider fall through to `ErrUnknownKID` → 401 (acceptable cold-start window). | rev. 1 conflated "synchronous before listen" with "non-fatal on per-provider failure" — at scale (1000 tenants × 5s discovery timeouts), the listener never binds and the pod fails liveness. The split keeps cold-start fast while preserving eventual completeness. KV-bootstrap failures remain `os.Exit(1)` per existing precedent. |
| D9 | Configuration env vars | **Five.** Four core (mirroring `IAMProperties.OIDCDefaults` from cyoda-cloud): `CYODA_OIDC_REQUIRE_HTTPS` (bool, default `true`), `CYODA_OIDC_CONNECT_TIMEOUT_MS` (int, `5000`), `CYODA_OIDC_SOCKET_TIMEOUT_MS` (int, `5000`), `CYODA_OIDC_CONNECTION_REQUEST_TIMEOUT_MS` (int, `5000`). One SSRF-override: `CYODA_OIDC_ALLOW_PRIVATE_NETWORKS` (bool, default `false`). | Faithful port of cyoda-cloud defaults; SSRF-override is a cyoda-go-specific addition (D10). |
| D10 | SSRF defence — fetch-time, not just register-time | **Custom `http.Transport.DialContext` re-checks the resolved IP on every dial.** Blocklist: IPv4 loopback `127.0.0.0/8`, IPv4 link-local `169.254.0.0/16`, IPv4 RFC1918 (`10/8`, `172.16/12`, `192.168/16`), IPv6 loopback `::1/128`, IPv6 link-local `fe80::/10`, IPv6 ULA `fc00::/7`, IPv4-mapped IPv6 `::ffff:0.0.0.0/96`. Register-time DNS-resolution check stays (better UX) but is **not** the security boundary — fetch-time is. **HTTP redirects disabled** on the discovery + JWKS client (fail-closed). Override via `CYODA_OIDC_ALLOW_PRIVATE_NETWORKS=true` (test/dev). | rev. 1's register-time-only check is DNS-rebind vulnerable (attacker DNS answers "public IP" at register-time, switches to `127.0.0.1` at fetch-time). Per-dial check closes the rebind window. IPv6 ranges close the dual-stack bypass. Redirect disabling closes the `Location: http://169.254.169.254/...` escape. |
| D11 | Register write-race semantics — explicit | **Accept the race; surface 409 `OIDC_PROVIDER_DUPLICATE` on the loser. Validate on read.** The (index, blob) write pair is best-effort transactional with a window bounded by the slower node's write latency under load (potentially seconds, not microseconds). On `Register`: write index first; on index-`Put` race-loss surface 409; on blob-`Put` failing, `Delete` the index (best-effort rollback) and return 500. **On every `Get` from the index, the resulting blob is read AND its `OwnerLegalEntityID` is verified to match the requesting tenant** — if the blob is missing or owned by a different tenant, the index entry is treated as stale, removed best-effort, and the operation returns 404. This defends against orphan-index reads under concurrent register/delete. | rev. 1 hand-waved this as "microseconds-wide." Realistic estimate is "however long the slower node takes between read and second write under load." The explicit read-time validation is the durable correctness guard; the race becomes observable but harmless. Concurrent-register test mandates exactly-one-200 / exactly-one-409. |
| D12 | Mock IdP fixture | **`internal/auth/oidc/fixture_test.go`** exposes a `FixtureIdP` type backed by `httptest.Server` with helpers for sign/rotate/revoke. Each parity scenario spins its own instance for isolation; cleanup via `t.Cleanup`. | No existing httptest-IdP precedent. The fixture API is small (5 methods) and lives next to its sole client. |
| D13 | Scope of #34 (trusted-key audit) inclusion | **Out of scope.** Verified independent during exploration. Stays its own issue. | Per memory `feedback_gate6_no_followups`. |
| D14 | ADR file under `docs/adr/` | **Add [`0002-federated-identity-provider-architecture.md`](../../adr/0002-federated-identity-provider-architecture.md).** Pins three load-bearing decisions — per-tenant ownership (D1), KV-backed persistence (D2), chained validator composition with four error sentinels (D3) — and their consequences. | These three decisions bind future federated-identity work. The spec is the per-issue implementation contract; the ADR is the durable architectural record. |
| D15 | `COMPATIBILITY.md` update | **None in this PR.** No SPI changes; no pin guidance to update. | rev. 2 removes the SPI extension; the compat-matrix is unchanged. |
| D16 | Cyoda-go-spi / cyoda-go-cassandra changes | **None in this PR. No coordinated release.** Single-namespace + composite-key pattern uses only the existing `KeyValueStore` ops (Put/Get/Delete/List). | rev. 1 introduced `ListNamespaces` as an SPI extension; rev. 2 drops it. The trusted-key precedent already shows the single-namespace pattern is enough. |
| D17 | Cross-tenant token-routing escalation guards | **`iat`-binding + mandatory `iss` validation.** (a) During `OIDCValidator.Validate`: reject tokens whose `iat` claim precedes the resolving provider's `CreatedAt` (`ErrTokenPreTransition`, hard-fail). (b) `iss` validation is mandatory at resolution: if `provider.Issuers` is non-empty, require the token's `iss` to be in that list; if `provider.Issuers` is empty (the "accept any" case), require the token's `iss` to equal the provider's discovery-document `issuer` field. Reject otherwise (`ErrIssuerMismatch`, hard-fail). | Closes the "tenant A deletes provider P, tenant B registers same `wellKnownConfigUri`, A's long-lived tokens promote into B's tenant" escalation by binding token-acceptance to the provider's creation-time. Closes the "two tenants register the same IdP (e.g., AWS Cognito) with overlapping `kid` namespace" routing ambiguity by requiring `iss` to match the provider regardless of whether `Issuers` is set. The discovery doc's `issuer` field is the authoritative `iss` for an OIDC provider per the OIDC Discovery spec §4. |
| D18 | Broadcast handler concurrency + panic isolation | **All four `case` arms dispatch via a bounded per-op single-flight worker.** `case "invalidate"` no longer runs inline. `case "reload" / "reload_all"` no longer spawn unbounded goroutines. `handleBroadcast` wraps with `defer recover()` and logs panics at ERROR. | rev. 1's inline `invalidate` could hold the registry write lock during gossip-receive, starving the memberlist goroutine. Unbounded `go r.reloadAll()` lets repeated `reload_all` broadcasts stack goroutines. A panic in the OIDC handler with no recover kills delivery of subsequent topics (including `modelcache` invalidations) on the same memberlist receive. The single-flight ensures one outstanding operation per (op, tenantID, uri) tuple. |
| D19 | Reactivate's JWKS sync is conditional on upstream success | **`reactivateKeys=true` (default) performs JWKS sync only if the upstream `GET <jwks_uri>` succeeds with HTTP 200 and valid JSON.** On any error (network failure, non-2xx, malformed body), the local cache is preserved as-is and a WARN is logged. The provider's `InvalidatedAt` is still cleared. | rev. 1's spec ported cyoda-cloud's `syncKeys=true` semantic literally. During an upstream key-rotation window (one key removed before the new one is added), an in-band reactivate would silently invalidate good keys. Conditional sync preserves availability when the IdP is briefly inconsistent. |
| D20 | Per-provider audience enforcement | **Add `ExpectedAudiences []string` to `OidcProvider`.** `OIDCValidator` checks the token's `aud` claim against this list (RFC 7519 §4.1.3: aud can be a string or array, both forms accepted). Empty/nil list means audience is unchecked for this provider — documented explicitly as a security-relevant configuration choice (issuer-binding becomes the trust anchor). OpenAPI: extend `RegisterOidcProviderRequestDto` and `UpdateOidcProviderRequestDto` with an optional `expectedAudiences` array. | rev. 1's §4.3 step 3 mentioned `aud` but specified no policy. The global `CYODA_JWT_AUDIENCE` is the wrong default — external IdPs typically issue tokens with their own application-id audience, not cyoda-go's. Per-provider explicit list is the correct shape. |
| D21 | List endpoint authz scope | **`GET /oauth/oidc/providers` allows any authenticated member of the owning tenant** (not ROLE_ADMIN). Mutating endpoints (`POST`/`PATCH`/`DELETE`/`/invalidate`/`/reactivate`/`/reload`) remain ROLE_ADMIN. | rev. 1 was conservatively ROLE_ADMIN-only; cyoda-cloud allows tenant-scoped reads for any authenticated user, and the use case is legitimate (a tenant user discovering "which IdPs can I authenticate against?"). Per memory `feedback_gate6_no_followups`: decide now rather than ship with a known divergence and a "we'll get to it." |
| D22 | Observability — telemetry on the OIDC hot path | **Add metrics:** kid-cache hit rate (counter pair: `oidc_kid_cache_hit_total`, `oidc_kid_cache_miss_total`), JWKS-fetch failure (counter `oidc_jwks_fetch_error_total{outcome}`), broadcast receive latency (histogram `oidc_broadcast_receive_seconds`), registry size (gauge `oidc_registry_providers{tenant=...}` cardinality-capped). Wired through `internal/observability`. Also: demote non-resolver `kid`-miss WARN to DEBUG; emit one INFO per provider per minute summarising miss counts. | Easy at spec-time, expensive to retrofit. Log-volume mitigation (W4) avoids drowning real warnings under bearer-auth-hot-path traffic. |
| D23 | Admin chain-introspection endpoint | **New `GET /api/admin/validators` (ROLE_ADMIN) — returns the validator chain in order with each validator's type name and read-only configuration summary.** Read-only; no token validation; no leakage of private material. | Future federated validators (SAML, social) will accumulate; introspection makes debugging "why does this token validate against the wrong validator?" tractable. Trivial to add now. |

## 3. Architecture

### 3.1 Package layout & files touched

| File | Action |
|---|---|
| `internal/auth/errors.go` | **New.** Defines four sentinel errors per D3: `ErrUnknownKID`, `ErrIssuerMismatch`, `ErrSignatureFailure`, `ErrClaimsFailure`. Plus `ErrTokenPreTransition` (D17) and `ErrJWKSUnavailable` (resolution-transient). |
| `internal/auth/chain.go` | **New.** `type Validator interface { Validate(string) (*spi.UserContext, error) }`; `type ChainedValidator struct { ... }`; `NewChainedValidator(...) *ChainedValidator`. Chain falls through ONLY on `ErrUnknownKID`. |
| `internal/auth/parse.go` | **New.** `parseTokenHeader(tokenString) (kid, alg, iss, aud string, exp, iat int64, err error)` — unauthenticated header+claims peek, used by both `JWKSValidator` and `OIDCValidator`. |
| `internal/auth/validator.go` | Refine error semantics: line 70 ("failed to resolve key for kid") becomes `ErrUnknownKID` (was untyped). Line 83 ("untrusted token issuer") becomes `ErrIssuerMismatch` (was untyped). Line 73 (sig verify) returns `ErrSignatureFailure`. Line 77 (claims) returns `ErrClaimsFailure`. Existing single-validator callers see indistinguishable wire behaviour (all map to 401); chain callers get the right fall-through. |
| `internal/auth/oidc/types.go` | **New.** `type OidcProvider struct { ... }` per §3.3 + error variables (`ErrProviderDuplicate`, `ErrProviderNotFound`, `ErrProviderInactive`, `ErrSSRFBlocked`). |
| `internal/auth/oidc/store.go` | **New.** `type OidcProviderStore interface { ... }` — `Register`, `Get(tenantID, providerID)`, `GetByURI(tenantID, uri)`, `Update`, `Delete`, `ListByTenant(tenantID, activeOnly)`, `LoadAll()` (no-arg; reads single namespace, parses tenant from key). |
| `internal/auth/oidc/kv_store.go` | **New.** `KVOidcProviderStore` against `spi.KeyValueStore`. Single namespace `oidc-providers`. Composite keys per §3.3. Store acquired with system-tenant context. `Get` validates the read-back blob's `OwnerLegalEntityID` matches the index-key's tenant prefix (D11 stale-index defence). |
| `internal/auth/oidc/discovery.go` | **New.** `type Discovery interface { Fetch(ctx, uri) (DiscoveryDoc, error) }` and `HTTPDiscovery` impl honouring `RequireHTTPS`, the three timeouts, redirect-disabled, and a `safedialer` that re-checks dial-time IPs against the blocklist (D10). |
| `internal/auth/oidc/ssrf.go` | **New.** `validateRegisterURI(uri, requireHTTPS, allowPrivate bool) error` for register-time UX. `safeDialContext(allowPrivate bool) func(ctx, network, addr) (net.Conn, error)` for fetch-time security. IPv6 ranges per D10. |
| `internal/auth/oidc/jwks_source.go` | **New.** `providerKeySource` — wraps `HTTPJWKSSource` with the provider's discovered `jwks_uri` + lifecycle gate. |
| `internal/auth/oidc/registry.go` | **New.** `Registry` — provider main map + per-provider key source + `kid`-fast-path index with self-heal (D6). Subscribes to `oidc.providers` broadcast. Owns the read path. `ResolveKey(kid, iss)` returns one of the four D3 sentinels. |
| `internal/auth/oidc/validator.go` | **New.** `OIDCValidator` implementing `auth.Validator`. Calls `parseTokenHeader`, performs `iat`-binding check (D17) and audience check (D20), consults `Registry.ResolveKey`, returns D3 sentinels. |
| `internal/auth/oidc/service.go` | **New.** `Service` — orchestrates the 7 lifecycle ops. Mutates store, mutates registry, broadcasts. Returns domain errors. |
| `internal/auth/oidc/broadcast.go` | **New.** `topicOidcProviders` constant + envelope type + `Service.broadcast(op, tenantID, uri)` helper + the registry's `handleBroadcast(payload)`. Single-flight worker pool per (op, tenantID, uri) per D18. Top-level `defer recover()`. |
| `internal/auth/oidc/observability.go` | **New.** Metric definitions per D22 wired against `internal/observability`. |
| `internal/auth/oidc/*_test.go` | **New.** Unit tests per component; `fixture_test.go` exports the `FixtureIdP`. |
| `internal/domain/account/oidc_adapter.go` | **New.** Seven thin HTTP→`oidc.Service` adapters. Each: authz gate (D21 split for List), input decode + validate, tenant scope, service call, response DTO encode. |
| `internal/domain/account/oidc_adapter_test.go` | **New.** Adapter-level unit tests. |
| `internal/domain/account/handler.go` | Replace lines 95-121: each `h.stub(w, r)` becomes a one-line delegation to the corresponding `oidc_adapter.go` method. Add `oidcService *oidc.Service` field; wire through `NewHandler`. |
| `internal/domain/account/validators_admin.go` | **New.** `ListValidators(w, r)` for `GET /api/admin/validators` (D23). |
| `internal/api/server.go:507-561` | No change — existing nil-check pattern delegates correctly once `Handler.oidcService` is non-nil. New `ListValidators` wiring added below the OIDC stub block. |
| `app/app.go` (around line 236, post trusted-key-store bootstrap) | Phase-1 warmup: `oidc.NewKVProviderStore(systemCtx, kvStore)` → `oidc.NewRegistry(...)` → `registry.LoadProvidersFromKV(systemCtx)` (cheap, blocking). Then construct chain: `oidcValidator := oidc.NewValidator(oidcRegistry)`; `chainedValidator := auth.NewChainedValidator(jwksValidator, oidcValidator)`. Wire into HTTP and gRPC auth middleware. Phase-2 warmup: `go registry.WarmJWKSAsync(systemCtx)` (post-listener). |
| `app/config.go` | Add `OIDCConfig` struct + `OIDC OIDCConfig` field on `IAMConfig`. Populate from 5 env vars in `DefaultConfig()`. |
| `api/openapi.yaml` | Remove the seven `$ref: "#/components/responses/NotImplemented"` declarations. Reconcile prose: `SUPER_USER` → `ROLE_ADMIN` on the 6 mutating operations; `List` prose updated to "any authenticated tenant member." Add `expectedAudiences: array(string)` to `RegisterOidcProviderRequestDto` and `UpdateOidcProviderRequestDto`. Add `expectedAudiences` to `OidcProviderResponseDto`. Add `GET /api/admin/validators` endpoint. |
| `api/generated.go` | Regenerated. |
| `internal/e2e/parity/oidc.go` | **New.** ~42 `RunOidc*` scenarios per §11. |
| `e2e/parity/registry.go` | Register the new scenarios in `allTests`. |
| `cmd/cyoda/help/content/config/oidc.md` | **New.** 5 env vars + behaviour notes. |
| `cmd/cyoda/help/content/errors/OIDC_PROVIDER_DUPLICATE.md` | **New.** |
| `cmd/cyoda/help/content/errors/OIDC_PROVIDER_NOT_FOUND.md` | **New.** |
| `cmd/cyoda/help/content/errors/OIDC_PROVIDER_INACTIVE.md` | **New.** |
| `cmd/cyoda/help/content/errors/OIDC_SSRF_BLOCKED.md` | **New.** |
| `cmd/cyoda/help/content/errors/OIDC_AUDIENCE_MISMATCH.md` | **New.** |
| `cmd/cyoda/help/content/errors/OIDC_TOKEN_PRE_TRANSITION.md` | **New.** |
| `cmd/cyoda/help/content/errors/OIDC_DISCOVERY_FAILED.md` | **New.** |
| `cmd/cyoda/help/content/errors/OIDC_JWKS_UNAVAILABLE.md` | **New.** |
| `README.md` | New "OIDC Provider Configuration" subsection in config-reference. |
| `docs/ARCHITECTURE.md` | New §7.3 "OIDC Provider Registry"; rename existing §7.3 → §7.4. |
| `docs/PRD.md` | Expand §8 with OIDC chaining subsection + external-issuer token-claims example. |
| `docs/FEATURES.md` | One bullet under Authentication & Authorization: "OIDC provider per-tenant registry." |
| `CHANGELOG.md` `## [0.8.0]` | `### Added`: OIDC subsystem; 5 env vars; admin validator-chain introspection. `### Removed`: seven `501 NOT_IMPLEMENTED` declarations from `/oauth/oidc/providers/*` (callers must handle real responses now). |

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
   │ (first-party)        │                                      │ (iat-binding + aud +   │
   │ returns one of:      │    other sentinels = hard-fail       │  Registry.ResolveKey)  │
   │  ErrUnknownKID       │                                      │ returns same 4         │
   │  ErrIssuerMismatch   │                                      │ sentinels              │
   │  ErrSignatureFailure │                                      └───────────┬────────────┘
   │  ErrClaimsFailure    │                                                  │
   └──────────────────────┘                                                  ▼
                                                          ┌─────────────────────────────┐
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
                          │   key: <T>:uri:<sha256(uri)>           │  │  bounded per-op     │
                          └────────────────────────────────────────┘  │  single-flight      │
                                                                      └──────────┬──────────┘
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
                                                              // empty = require iss == DiscoveryDoc.Issuer
    ExpectedAudiences  []string   `json:"expectedAudiences,omitempty"` // D20; empty = aud unchecked
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

**Register race (D11), explicit semantics.** Two nodes can race the index `Put` (or two clients can race in the same node). Resolution:

1. Both readers see no index entry.
2. Node A writes `<T>:uri:<sha256>` → `<uuid-A>`.
3. Node B writes `<T>:uri:<sha256>` → `<uuid-B>` (overwrites).
4. Both write their blobs.
5. **The Service's Register routine, after writing the index, re-reads the index value.** If the re-read does NOT equal the value just written, the loser deletes its own blob and returns 409 `OIDC_PROVIDER_DUPLICATE` to its client. Both clients get a consistent answer; KV ends in a single-blob state.

**Stale-index defence on every read.** `Get(tenantID, providerID)` reads the blob and verifies its `OwnerLegalEntityID` matches the requesting tenant. `GetByURI(tenantID, uri)` reads the index → blob and verifies the blob exists AND the blob's tenant matches the index-key's tenant prefix. Mismatch → treat the index entry as orphaned (best-effort `Delete`, return `ErrProviderNotFound`).

### 3.4 REST surface

DTOs are already defined in `api/openapi.yaml`. rev. 2 extends them with `expectedAudiences` per D20:

- `RegisterOidcProviderRequestDto`: `{wellKnownConfigUri, issuers?, expectedAudiences?}`
- `UpdateOidcProviderRequestDto`: `{issuers?, expectedAudiences?}`
- `ReactivateOidcProviderRequestDto`: `{reactivateKeys?}` (unchanged)
- `OidcProviderResponseDto`: `{id, wellKnownConfigUri, issuers?, expectedAudiences?, active, createdAt}`

Wire matrix:

| Method | Path | Handler | Authz | Success | Errors |
|---|---|---|---|---|---|
| POST | `/oauth/oidc/providers` | `RegisterOidcProvider` | ROLE_ADMIN | 200 `OidcProviderResponseDto` | 400 `OIDC_PROVIDER_DUPLICATE`; 400 `BAD_REQUEST`; 400 `OIDC_SSRF_BLOCKED`; 403; 401; 500 |
| GET | `/oauth/oidc/providers?activeOnly=<bool>` | `ListOidcProviders` | **any authenticated tenant member (D21)** | 200 `[]OidcProviderResponseDto` | 401 |
| PATCH | `/oauth/oidc/providers/{id}` | `UpdateOidcProvider` | ROLE_ADMIN | 200 `OidcProviderResponseDto` | 404 `OIDC_PROVIDER_NOT_FOUND`; 409 `OIDC_PROVIDER_INACTIVE`; 400; 403; 401 |
| POST | `/oauth/oidc/providers/{id}/invalidate` | `InvalidateOidcProvider` | ROLE_ADMIN | 200 empty | 404; 403; 401 (idempotent on already-invalidated) |
| POST | `/oauth/oidc/providers/{id}/reactivate` | `ReactivateOidcProvider` | ROLE_ADMIN | 200 `OidcProviderResponseDto` | 404; 403; 401 (idempotent on already-active) |
| DELETE | `/oauth/oidc/providers/{id}` | `DeleteOidcProvider` | ROLE_ADMIN | 200 empty | 404; 403; 401 |
| POST | `/oauth/oidc/providers/reload` | `ReloadOidcProviders` | **explicit in-code** ROLE_ADMIN | 200 empty | 403; 401 |
| GET | `/api/admin/validators` | `ListValidators` (D23) | ROLE_ADMIN | 200 array of `{name, configSummary}` | 403; 401 |

Token-validation-time errors that surface to the bearer-auth middleware (not OIDC management API):

| Sentinel | Wire | Meaning |
|---|---|---|
| `ErrUnknownKID` | 401 (only after chain exhaustion) | Neither validator recognised the `kid`. |
| `ErrIssuerMismatch` | 401 | A validator's KeySource resolved the `kid` but the `iss` claim does not match. |
| `ErrSignatureFailure` | 401 | Signature verification failed. |
| `ErrClaimsFailure` | 401 (specific subcode where useful — `expired`, `audience`, etc.) | Exp/nbf/aud failure. |
| `ErrTokenPreTransition` | 401 + log line `oidc.token_pre_transition` | `iat < provider.CreatedAt` (D17). |
| `ErrJWKSUnavailable` | 503 + Retry-After | Transient JWKS-endpoint failure; client should retry. |

## 4. Runtime mechanics

### 4.1 Registry

```go
type Registry struct {
    mu        sync.RWMutex
    providers map[spi.TenantID]map[string]*OidcProvider // by wellKnownConfigUri
    sources   map[spi.TenantID]map[string]auth.KeySource
    kidIndex  map[string][]providerRef                  // self-healing per D6

    store     OidcProviderStore
    discovery Discovery
    broadcast spi.ClusterBroadcaster
    clock     func() time.Time
    metrics   *Metrics  // D22
    logger    *slog.Logger
}

type providerRef struct {
    tenant spi.TenantID
    uri    string
}

type KeyResolution struct {
    PublicKey          *rsa.PublicKey
    Provider           *OidcProvider     // includes CreatedAt for D17, ExpectedAudiences for D20
    WellKnownConfigURI string
}

func (r *Registry) ResolveKey(kid, iss string) (*KeyResolution, error)
// Hot path: kidIndex[kid] → candidate refs → for each, look up provider →
//   mandatory iss check (D17) → source.GetKey(kid) → first sig-verifiable wins.
// If a candidate's GetKey returns the key but later signature verification by the
//   caller fails, the caller calls r.EvictKidEntry(kid, ref) which is a no-op if
//   the entry was already evicted. (D6 self-heal.)
// Cold path: kidIndex miss → r.mu.RLock + global iteration → populate kidIndex on
//   first sig-verifiable resolution → r.mu.RUnlock.
// Returns ErrUnknownKID on no match (chain fall-through).
// Returns ErrIssuerMismatch when kid resolved but no candidate's iss matches (hard-fail).
// Returns ErrJWKSUnavailable when the only resolution candidate's source returns a transient
//   network/HTTP error (no candidate available to verify against; not silent fall-through).
```

Concurrent iss-mismatch vs JWKS-unavailable: if at least one candidate matches `iss` but its source returns a transient error, that's `ErrJWKSUnavailable`. If no candidate matches `iss` (or all matching candidates also have transient failures with their sources), the outcome is `ErrJWKSUnavailable` only when at least one matching candidate's source failed transiently; otherwise `ErrIssuerMismatch`. Spelled out for the implementer in §5.

Locking discipline per `.claude/rules/go-mutex-discipline.md`: every `Lock()`/`RLock()` followed immediately by `defer Unlock()`/`defer RUnlock()`; early-release blocks use IIFE.

### 4.2 Broadcast

```go
const topicOidcProviders = "oidc.providers"

type broadcastEnvelope struct {
    Op       string `json:"op"`   // "reload" | "invalidate" | "reload_all"
    TenantID string `json:"t,omitempty"`
    URI      string `json:"u,omitempty"`
}

// handleBroadcast runs on the memberlist receive goroutine. Invariants:
//   - never blocks (single-flight dispatch is non-blocking; in-flight ops are dropped, not queued)
//   - never panics (defer recover() at top; logs ERROR)
//   - all KV access uses systemCtx
//   - idempotent (RELOAD same uri twice = one effective reload)
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
        r.singleflight.Dispatch(envKey(env), func() { r.reloadOne(spi.TenantID(env.TenantID), env.URI) })
    case "invalidate":
        r.singleflight.Dispatch(envKey(env), func() { r.invalidateOne(spi.TenantID(env.TenantID), env.URI) })
    case "reload_all":
        r.singleflight.Dispatch("reload_all", func() { r.reloadAll() })
    }
}
```

`singleflight.Dispatch(key, fn)` semantics: if no work is in flight for `key`, spawn one goroutine running `fn`. If work IS in flight for `key`, drop the call (returning false; logged at DEBUG). Implementation: small wrapper around a `map[string]chan struct{}` guarded by a mutex; not the stdlib `golang.org/x/sync/singleflight` (which queues callers for the result). We don't need results; we need debounce.

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
            return nil, err  // ErrIssuerMismatch / ErrSignatureFailure / ErrClaimsFailure
                             // / ErrTokenPreTransition / ErrJWKSUnavailable → hard-fail
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

1. `parseTokenHeader(tokenString)` → `kid, alg, iss, aud, exp, iat, err`. Alg not in allow-list → `ErrClaimsFailure` (subcode `unsupported_alg`). Token already past `exp` → `ErrClaimsFailure` (subcode `expired`).
2. `r.ResolveKey(kid, iss)` → `(*KeyResolution, error)`. `ErrUnknownKID` → return for chain fall-through. `ErrIssuerMismatch` / `ErrJWKSUnavailable` → return as-is.
3. **D17 iat-binding check.** If `iat < resolution.Provider.CreatedAt` → return `ErrTokenPreTransition`. (Audit log: `oidc.token_pre_transition` with provider UUID, tenant, kid, NOT the token or iss.)
4. Verify signature against `resolution.PublicKey`. Failure → return `ErrSignatureFailure`. **On signature failure, call `r.EvictKidEntry(kid, providerRef)`** to self-heal D6.
5. Standard claims: `exp`, `nbf` → `ErrClaimsFailure` on failure.
6. **D20 audience check.** If `resolution.Provider.ExpectedAudiences` is non-empty, require token's `aud` to match (RFC 7519 §4.1.3). Failure → `ErrClaimsFailure` (subcode `audience`).
7. Build `*spi.UserContext` with `Tenant.ID = resolution.Provider.OwnerLegalEntityID.String()`, roles populated from token claims per existing convention.

## 5. Lifecycle behaviour (per operation)

Skeleton for every mutating op: validate input → authz → tenancy guard → store mutation → registry mutation → broadcast → respond. Store mutation precedes registry mutation; broadcast is best-effort after.

### 5.1 Register

1. `auth.RequireAdmin(w, r)`.
2. Decode request, validate: `wellKnownConfigUri` non-empty, ≤1000 chars, valid absolute URL, scheme matches `CYODA_OIDC_REQUIRE_HTTPS`. `Issuers` if present: ≤10, each ≤1000 chars, deduplicated. `ExpectedAudiences` if present: ≤10, each ≤1000 chars, deduplicated. **Register-time SSRF gate (D10):** DNS-resolve hostname, reject if in blocklist (unless `CYODA_OIDC_ALLOW_PRIVATE_NETWORKS=true`).
3. `tenantID := tenantFromCtx(r)`.
4. KV: `Get("oidc-providers", "<T>:uri:<sha256>")`. If present → 400 `OIDC_PROVIDER_DUPLICATE`.
5. KV: `Put` index `<T>:uri:<sha256>` → `<provider-uuid>`. KV: `Put` blob `<T>:<provider-uuid>` → JSON(OidcProvider).
6. **Race-validation re-read (D11):** `Get("oidc-providers", "<T>:uri:<sha256>")`. If value ≠ `<provider-uuid>` (we lost the race): `Delete` our own blob, return 409 `OIDC_PROVIDER_DUPLICATE`.
7. `registry.reloadOne(tenantID, uri)` — synchronous discovery + JWKS fetch. Failures WARN-logged; the provider is registered regardless and becomes usable when reachable.
8. `broadcast({op: "reload", t: tenantID, u: uri})`. Broadcast failure → 500 `SERVER_ERROR` + ticket UUID + `Delete` both KV entries (best-effort rollback).
9. Respond 200 with `OidcProviderResponseDto`.

### 5.2 Update issuers / audiences

1. `auth.RequireAdmin`.
2. KV `Get` by `(tenantID, providerID)` with stale-index defence. Missing → 404.
3. If `InvalidatedAt != nil` → 409 `OIDC_PROVIDER_INACTIVE`.
4. Validate new `Issuers` list (D17 mandatory-iss check still applies via the runtime fall-back to DiscoveryDoc.Issuer when `Issuers` empty). Validate new `ExpectedAudiences` list. **PATCH tri-state semantics** for both fields: field omitted → unchanged; field present and null → cleared (stored as nil); field present and empty array (`[]`) → cleared (treated identically to null at the runtime — D17's discovery-doc fallback applies). Documented in the error-code help topics so admins are not surprised.
5. KV `Put` updated blob.
6. `registry.reloadOne(tenantID, uri)` with **JWKS cache flush** — drop in-memory key cache, force re-fetch on next token. (No persisted JWK records exist; this is purely an in-memory cache flush.)
7. Broadcast reload.
8. Respond 200 with DTO.

### 5.3 Invalidate

1. `auth.RequireAdmin`.
2. KV `Get` with stale-index defence. Missing → 404.
3. Already invalidated: WARN log, 200 idempotent, no broadcast.
4. KV `Put` with `InvalidatedAt = time.Now()`.
5. Registry: drop the provider entry + its KeySource. Evict matching `kidIndex` candidates (per D6).
6. `broadcast({op: "invalidate", t: tenantID, u: uri})`.
7. Respond 200 empty.

### 5.4 Reactivate

1. `auth.RequireAdmin`.
2. KV `Get` with stale-index defence. Missing → 404.
3. Already active: INFO log, 200 with current DTO, no broadcast.
4. KV `Put` with `InvalidatedAt = nil`.
5. **D19 conditional sync.** `reactivateKeys=true` (default): `registry.reloadOne(tenantID, uri, syncKeys=true)` — performs JWKS fetch; **if fetch succeeds with HTTP 200 + valid JSON**, sync the cache (drop locally cached keys not in remote JWKS); **if fetch fails**, leave cache as-is, log WARN. `reactivateKeys=false`: `registry.reloadOne` discovery only, no JWKS touch.
6. Broadcast reload.
7. Respond 200 with DTO.

### 5.5 Delete

1. `auth.RequireAdmin`.
2. KV `Get` with stale-index defence. Missing → 404.
3. If active, perform invalidate semantics inline (no separate broadcast).
4. KV: `Delete` blob `<T>:<provider-uuid>` AND `Delete` index `<T>:uri:<sha256>`.
5. Registry: drop the provider entry, evict matching `kidIndex` candidates.
6. `broadcast({op: "invalidate", t: tenantID, u: uri})`.
7. Respond 200 empty.

### 5.6 Reload-all

1. **Explicit in-code ROLE_ADMIN check.**
2. `store.LoadAll()` → for each loaded (tenantID, provider) pair, populate fresh `providers` / `sources` / `kidIndex`.
3. Swap under `mu.Lock()`.
4. `broadcast({op: "reload_all"})`.
5. Respond 200 empty.

### 5.7 List

1. **Any authenticated tenant member (D21).** `tenantID := tenantFromCtx(r)`. If no tenant in context → 401.
2. `store.ListByTenant(tenantID, activeOnly)`.
3. Map to DTOs.
4. Respond 200.

### 5.8 Admin chain-introspection (D23)

1. `auth.RequireAdmin`.
2. Iterate `ChainedValidator.validators`; for each, emit `{name, configSummary}` (no private material). E.g. `{"name": "JWKSValidator", "configSummary": {"issuer": "cyoda", "keySource": "local", "audienceCheck": "configured"}}`.
3. Respond 200 with array.

## 6. Startup (D8)

In `app/app.go`, after the `KVTrustedKeyStore` bootstrap (around line 236):

```go
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

oidcValidator := oidc.NewValidator(oidcRegistry)
chainedValidator := auth.NewChainedValidator(jwksValidator, oidcValidator)
// wire chainedValidator into HTTP auth middleware + gRPC interceptor

// (...HTTP listener binds here, well before warm-JWKS finishes...)

// Phase 2: asynchronous, post-listener
go oidcRegistry.WarmJWKSAsync(systemCtx)
```

`LoadProvidersFromKV` does `kv.List("oidc-providers")` (one call, system-tenant context), parses `<T>:<provider-uuid>` keys (skipping the `<T>:uri:<sha256>` index entries), populates `providers` map. No network I/O. Worst case: O(total providers) key parse — cheap.

`WarmJWKSAsync` spawns one goroutine per (tenantID, uri) bounded by a worker pool (size = `runtime.NumCPU()`). Per-provider goroutine fetches discovery + JWKS via the safedialer'd client, populates `sources` and `kidIndex` for that provider. Per-provider failures: WARN-log, do not retry within this warmup pass (subsequent token arrivals will trigger the cold path).

## 7. Configuration

| Env var | Type | Default | Purpose |
|---|---|---|---|
| `CYODA_OIDC_REQUIRE_HTTPS` | bool | `true` | Reject `http://` `wellKnownConfigUri` at Register-time. |
| `CYODA_OIDC_CONNECT_TIMEOUT_MS` | int | `5000` | TCP connect timeout for discovery + JWKS HTTP requests. |
| `CYODA_OIDC_SOCKET_TIMEOUT_MS` | int | `5000` | HTTP read timeout (`http.Transport.ResponseHeaderTimeout` + per-request deadline). |
| `CYODA_OIDC_CONNECTION_REQUEST_TIMEOUT_MS` | int | `5000` | Pool-checkout timeout. |
| `CYODA_OIDC_ALLOW_PRIVATE_NETWORKS` | bool | `false` | Test/dev override: disable both register-time and fetch-time SSRF blocklists (D10). |

Wired into `app/config.go::IAMConfig.OIDC` (new `OIDCConfig` struct), populated in `DefaultConfig()`. Help topic at `cmd/cyoda/help/content/config/oidc.md`. README config-reference section updated.

## 8. Security

| Aspect | Mechanism | Verification |
|---|---|---|
| Authn | Existing bearerAuth middleware — no new wiring | All 7 endpoints already declare `security: [bearerAuth: []]` |
| Authz (mutating) | `auth.RequireAdmin(w, r)` on each handler; explicit in-code ROLE_ADMIN check on `Reload` and `ListValidators` | 6 negative tests (non-admin → 403) |
| Authz (read) | Any authenticated tenant member (D21); tenant scope enforced via `tenantFromCtx` + `OwnerLegalEntityID` filter | Tenant-member-without-ROLE_ADMIN can list own providers; cross-tenant member cannot see others |
| Tenant isolation (mgmt API) | Composite-key prefix `<tenantID>:`; stale-index defence on every `Get` | Cross-tenant test: tenant-A's provider ID returns 404 to tenant-B's caller (whether admin or not) |
| Tenant isolation (token validation) | `UserContext.Tenant = OwnerLegalEntityID` from the resolving provider; `tid`/`tenant`/`org` claims in the token are ignored | Test: token signed by tenant-A's IdP with a `tid=B` claim still yields `Tenant=A` |
| Cross-tenant escalation via re-registration (D17) | `iat < provider.CreatedAt` rejects with `ErrTokenPreTransition` | Test: tenant A registers, JWT issued, A deletes provider, B registers same URI, A's old JWT rejected |
| Cross-tenant routing via kid collision (D17) | Mandatory `iss` validation at resolution; when `Issuers` is empty, fall back to `DiscoveryDoc.Issuer` exact match | Test: two AWS Cognito tenants with overlapping kid namespace; tokens route to correct tenant |
| Chain-fall-through escalation (D3) | Four distinct sentinels; chain falls through ONLY on `ErrUnknownKID` | Test: first-party kid + foreign iss → 401 with `ErrIssuerMismatch`, OIDCValidator not consulted |
| kid-cache poisoning (D6 self-heal) | Failed signature evicts `kidIndex[kid]` providerRef | Test: malicious tenant publishes first-party kid in JWKS; first-party validation continues to succeed; malicious entry evicted |
| SSRF defence — register-time (D10) | DNS-resolve + CIDR check on hostname | Test: `http://169.254.169.254/.well-known/...` → 400 `OIDC_SSRF_BLOCKED` |
| SSRF defence — fetch-time (D10) | `safeDialContext` re-checks every dial against the IPv4+IPv6 blocklist | Test: register with fast-flux DNS that returns public IP at register, `127.0.0.1` at fetch → fetch fails with `OIDC_SSRF_BLOCKED` |
| SSRF defence — redirects (D10) | `http.Client` configured with `CheckRedirect` returning `http.ErrUseLastResponse` (no follow) | Test: discovery URL responds 302 → `Location: http://169.254.169.254/...`; fetch fails (no redirect followed) |
| Per-provider audience (D20) | `ExpectedAudiences` checked at OIDCValidator step 6; empty list documented as explicit "no audience check" | Test: provider with aud=`["api1"]` rejects token with `aud=api2`; provider with empty aud accepts both |
| Log hygiene (Gate 3) | Never log tokens, JWT contents, private keys, JWKS payloads, `iss` of rejected tokens (could be attacker-controlled). **OK**: `kid`, `wellKnownConfigUri`, tenantID, provider UUID. | Pre-PR grep gate over `internal/auth/oidc/` |
| Output sanitization | 4xx with domain code + short message; 5xx with generic + ticket UUID; no upstream error bodies echoed | Test: JWKS-fetch 500 response body contains only ticket UUID, not the upstream string |
| Race-safety | All registry reads under `mu.RLock()`; mutations under `mu.Lock()`; broadcast handlers idempotent + panic-recover (D18). Lock discipline per `.claude/rules/go-mutex-discipline.md`. | Race detector run pre-PR per `.claude/rules/race-testing.md` |
| Broadcast handler isolation (D18) | `defer recover()` at top; non-blocking single-flight dispatch; bounded concurrency | Test: induced panic in OIDC handler does NOT kill `modelcache` invalidation delivery on the same node |
| Observability (D22) | Metric counters/histograms/gauges via `internal/observability`; non-resolver kid-misses at DEBUG with per-provider INFO summary every 60s | Verify metrics are emitted in an E2E run; grep slog calls for `Level=WARN` on hot path returns nothing |

## 9. Out of scope

- **#34 trusted-key audit findings** — independent. Stays its own issue.
- **Dynamic client registration (OIDC DCR)** — provider registration only.
- **OIDC RP / login flow / federated logout / back-channel logout** — we accept tokens, we don't act as a relying party.
- **Multi-cloud deploy IaC** — moved to `cyoda-go-terraform` per memory `project_multicloud_deploy_rework`.
- **`KeyValueStore.ListNamespaces` SPI extension** — rev. 1 included this; rev. 2 dropped it (D2 + D16). The trusted-key precedent covers our needs with no SPI change.
- **`KeyValueStore.PutBatch` SPI extension** — explicit user decision (rev. 2): no SPI changes in this PR. If multi-key atomic writes prove necessary for other subsystems later, that's a separate proposal.
- **Persistent `kidIndex` across restarts** — cold-path scan is acceptable at v0.8.0 scale; revisit if real-world clusters exceed ~10K providers.

## 10. Acceptance criteria mapping

| Issue body item | Where covered |
|---|---|
| Brainstorm + spec under `docs/superpowers/specs/` | This document |
| Plan under `docs/superpowers/plans/` | Next step (writing-plans skill) |
| All 7 handlers return real responses | §3.1 + §5 |
| All 27 ported test scenarios pass across memory/sqlite/postgres/cassandra | §11 (cyoda-cloud has 27; rev. 2 reuses parity registration — cassandra picks up on next dep update) |
| `JWKSValidator` extended multi-issuer with per-provider lifecycle | §4.3 (chained, four-sentinel — D3 records the divergence) |
| `CYODA_OIDC_*` env vars documented in `cmd/cyoda/help/content/config/` | §7 + §12 |
| Cross-node cache invalidation verified via multi-node E2E harness | §11 broadcast-eviction scenario |
| No credentials / tokens / private keys in any log line | §8 + pre-PR grep gate |
| 501 declarations removed from OpenAPI for these 7 paths | §3.1 + `### Removed` CHANGELOG entry |
| `h.stub(w, r)` calls removed for OIDC handlers | §3.1 + §5 |
| Decomposition spec §3.4 updated to v0.8.0 | Already done in tree (amendment at head of `2026-06-04-194-decomposition-design.md`); §1 records the satisfaction |

## 11. Test plan

All scenarios run as parity tests in `internal/e2e/parity/oidc.go`, registered in `e2e/parity/registry.go::allTests`. Each parity backend (memory, sqlite, postgres) picks them up via its existing wrapper; cassandra inherits on its next dependency update.

Test count is **approximate** (~42); the matrix below is the authoritative inventory.

| # | Family | Count | Description |
|---|---|---|---|
| | **CRUD happy-path (ported)** | 5 | Register / List(all+activeOnly) / Update / Invalidate / Reactivate(noKeys) / Delete |
| | **CRUD negative — 404/duplicate (ported)** | 4 | Update/Invalidate/Reactivate/Delete on nonexistent id; duplicate-URI → 400 |
| | **Authz negative — mutating (ported)** | 6 | Non-ROLE_ADMIN attempts Register/Update/Invalidate/Reactivate/Delete/Reload → 403 |
| | **Cluster intercom failure (ported, rewritten)** | 1 | Broadcaster Broadcast error path → 500 (D7 rewrite of cyoda-cloud's ack-failure test) |
| | **JWT validation integration (ported)** | 4 | Register-and-validate; invalidate-and-reject; reactivate-and-revalidate; delete-and-permanent-reject |
| | **Issuer-list update affects validation (ported)** | 1 | Cycle the issuers list; JWT acceptance follows |
| | **Key rotation / revocation (ported)** | 5 | `validTo` past/future/null; invalidated key; remote-removal sync (with D19 success-conditional flush) |
| | **Multi-provider isolation (ported)** | 1 | Two providers in same tenant, keys don't cross-validate |
| | **Inactive-update → 409 OIDC_PROVIDER_INACTIVE** | 1 | D5 |
| | **Cross-tenant 404 on management lookups** | 1 | D1 (mgmt-side isolation) |
| | **Tenant binding via OwnerLegalEntityID (vs token `tid` claim)** | 1 | D1 (token-validation-side isolation, `tid` claim cannot override) |
| | **D17 iat-binding: deleted-then-re-registered token rejected** | 1 | New: tenant A registers, JWT issued, A deletes, B registers same URI, A's pre-deletion JWT rejected with `ErrTokenPreTransition` |
| | **D17 mandatory iss-validation: cross-tenant kid collision routes correctly** | 1 | New: two providers (e.g., two AWS Cognito user pools) with overlapping kid namespace; tokens route to the correct tenant by iss |
| | **D17 mandatory iss-validation: empty-Issuers falls back to DiscoveryDoc.Issuer** | 1 | New: provider registered with empty `issuers` list; token with mismatched iss rejected even when kid matches |
| | **D3 four-sentinel chain fall-through** | 1 | New: first-party kid + foreign iss → 401 ErrIssuerMismatch (OIDCValidator NOT consulted, verified via mock OIDC validator's call counter) |
| | **D6 kid-cache self-heal on signature failure** | 1 | New: malicious tenant publishes first-party kid in its JWKS; first-party validation continues to succeed; malicious entry evicted from kidIndex |
| | **D11 register write-race exactly-one-wins** | 1 | New: concurrent Register from two clients targeting same URI; exactly one 200, exactly one 409; KV ends consistent |
| | **D11 stale-index orphan defence** | 1 | New: induce orphaned `<T>:uri:...` index entry with no matching blob; Get/List skip it; index eventually cleaned up |
| | **D8 two-phase warmup: listener binds before Phase 2 completes** | 1 | New: 100 fake providers with slow JWKS endpoints; listener binds within 1s; first OIDC validation succeeds within 30s |
| | **D8 startup-warmup tolerates per-provider Phase-2 failure** | 1 | New: provider A discovery OK, provider B discovery fails; A serves traffic, B logs WARN, no os.Exit |
| | **D18 broadcast handler panic isolation** | 1 | New: inject a panic in OIDC handleBroadcast; modelcache invalidation delivery on the same memberlist node continues; panic logged at ERROR |
| | **D18 broadcast single-flight debounce** | 1 | New: deliver 10 RELOAD broadcasts for the same (tenantID, uri) in 100ms; observe single reloadOne invocation |
| | **D10 SSRF — fetch-time check via DNS rebind** | 1 | New: `httptest.Server` register-time DNS returns public IP; fetch-time DNS returns 127.0.0.1; fetch fails OIDC_SSRF_BLOCKED |
| | **D10 SSRF — IPv6 link-local + ULA blocked** | 1 | New: register URIs resolving to `fe80::1` and `fc00::1` → both 400 |
| | **D10 SSRF — redirects not followed** | 1 | New: discovery 302 → `http://169.254.169.254/...`; fetch fails (no follow) |
| | **D19 reactivate with JWKS upstream down: cache preserved** | 1 | New: register, invalidate, then reactivate(keys=true) while JWKS endpoint is down; cache preserved, WARN logged, InvalidatedAt cleared |
| | **D19 reactivate(keys=false)** | 1 | New: reactivate(keys=false); no JWKS touch; existing cached keys remain valid |
| | **D20 per-provider audience mismatch** | 1 | New: provider with `ExpectedAudiences=["api1"]`; token with `aud=api2` → 401 ErrClaimsFailure (subcode audience) |
| | **D20 per-provider audience empty = unchecked** | 1 | New: provider with empty `ExpectedAudiences`; token with any aud accepted |
| | **D21 list endpoint open to tenant member** | 1 | New: non-admin tenant member calls GET /oauth/oidc/providers → 200; cross-tenant member → empty list (not 401) |
| | **D23 admin chain introspection** | 1 | New: GET /api/admin/validators returns ordered chain with config summary; non-admin → 403 |
| | **D8 idempotency: RELOAD twice = single effective reload** | 1 | New (broadcast idempotency); paired with D18 debounce |
| | **State-transition pairs** | 2 | New: active→invalidated→deleted; invalidated→reactivated→invalidated (catches reactivate-after-invalidate edge in cache flush) |
| | **E2E coverage (Gate 2)** | 2 | `TestE2E_OIDC_TokenValidation` (full HTTP stack: register + mock IdP + GET /me + invalidate + 401); `TestE2E_OIDC_MultiNode_Eviction` (memberlist gossip ring + KV-shared backend) |
| | **Approximate total** | **~42** | |

Fixture IdP shape (§3.1 references):

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
// for SSRF tests: an override hook that changes DNS resolution between register and fetch
func (f *FixtureIdP) WithFakeDNS(t *testing.T, registerIP, fetchIP net.IP) *FixtureIdP
```

Multi-node broadcast-eviction E2E: two in-process cyoda-go instances over a real `gossip_broadcast.Gossip` ring, sharing a memory-plugin KV store. Register on node A, observe node B's `Registry.providers` map gains the entry within one gossip cycle; invalidate on A, observe B drops it within one cycle.

## 12. Documentation updates (Gate 4)

| File | Action |
|---|---|
| `cmd/cyoda/help/content/config/oidc.md` | **New** — 5 env vars + behaviour notes |
| `cmd/cyoda/help/content/errors/OIDC_PROVIDER_DUPLICATE.md` | **New** |
| `cmd/cyoda/help/content/errors/OIDC_PROVIDER_NOT_FOUND.md` | **New** |
| `cmd/cyoda/help/content/errors/OIDC_PROVIDER_INACTIVE.md` | **New** |
| `cmd/cyoda/help/content/errors/OIDC_SSRF_BLOCKED.md` | **New** |
| `cmd/cyoda/help/content/errors/OIDC_AUDIENCE_MISMATCH.md` | **New** |
| `cmd/cyoda/help/content/errors/OIDC_TOKEN_PRE_TRANSITION.md` | **New** |
| `cmd/cyoda/help/content/errors/OIDC_DISCOVERY_FAILED.md` | **New** |
| `cmd/cyoda/help/content/errors/OIDC_JWKS_UNAVAILABLE.md` | **New** |
| `app/config.go` `DefaultConfig()` | Populate `IAMConfig.OIDC` from 5 env vars |
| `README.md` | New "OIDC Provider Configuration" subsection under config-reference |
| `docs/ARCHITECTURE.md` | New §7.3 "OIDC Provider Registry"; existing §7.3 → §7.4 |
| `docs/PRD.md` | Expand §8 with OIDC chaining subsection + external-issuer token-claims example |
| `docs/FEATURES.md` | One bullet under Authentication & Authorization: "OIDC provider per-tenant registry" |
| `CHANGELOG.md` `## [0.8.0]` | `### Added`: OIDC subsystem; 5 env vars; admin validator-chain introspection. `### Removed`: seven `501 NOT_IMPLEMENTED` declarations on `/oauth/oidc/providers/*` (contract-breaking — callers must handle real responses). |
| `COMPATIBILITY.md` | No change — no SPI bump in this PR |
| `docs/adr/0002-federated-identity-provider-architecture.md` | **New** — pins D1 (per-tenant), D2 (KV single-namespace), D3 (chained four-sentinel) |
