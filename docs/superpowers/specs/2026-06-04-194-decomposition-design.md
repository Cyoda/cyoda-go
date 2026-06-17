# Issue #194 — decomposition design

**Status:** approved (amended 2026-06-16)
**Date:** 2026-06-04
**Scope:** Replace the umbrella issue [#194 — implement stub OAuth keys / OIDC providers / technical user management endpoints](https://github.com/Cyoda-platform/cyoda-go/issues/194) with four self-contained sub-issues plus one infrastructure follow-up. Close #194 with references.
**Outcome:** four scheduled work items with no bi-directional coupling, each independently brainstorm/spec/plan/implement-able. Avoids a single milestone-blocking monolith and matches the project rule that release-branch PRs stay reviewable.

## Amendments

- **2026-06-16 — OIDC pulled into v0.8.0.** The original decision (§2 row 3, §3.4) deferred sub-issue D (OIDC providers) to v0.9.0+ on multi-week-design-cycle grounds. That decision is overridden: OIDC is now in-scope for v0.8.0 as the milestone's headline IAM deliverable. The release is held until #284 lands. Sister issue #123 (the digital-twin parity port with the full data model, REST table, service-behaviour port, and 28 test scenarios) has been closed as a duplicate; its content is consolidated into the #284 issue body and treated as authoritative input for the implementation spec.

---

## 1. Background

#194 catalogues ~17 OpenAPI operations whose handlers in `internal/domain/account/handler.go` return 501 via `h.stub(w, r)`. The issue notes these span three subsystems — OAuth key management, OIDC providers, technical-user (M2M) management — and an account/subscriptions stub, and explicitly recommends three separate efforts.

A pre-decomposition audit of `internal/auth/` revealed a more important fact than the catalogue suggests:

- **OAuth keypair management is already implemented** (`internal/auth/keys.go` + `InMemoryKeyStore`) and reachable at `/oauth/keys/keypair/*` today, because `app/app.go:482` mounts `authSvc.AdminHandler()` as a more-specific prefix than the chi-routed `/`. The 501 stubs at those paths are dead code.
- **Trusted-key management is already implemented** (`internal/auth/trusted.go` + `TrustedKeyStore`, with both in-memory and persistent variants) and reachable at `/oauth/keys/trusted/*` under the same prefix mux.
- **M2M client management is already implemented** (`internal/auth/m2m.go` + `InMemoryM2MClientStore`) and reachable at `/account/m2m*` — but the OpenAPI spec puts technical users at `/clients` instead, so the spec-conformant path *is* still 501.
- **OIDC providers are genuinely greenfield.** No code references `OidcProvider` outside the generated API stubs. `validator.go` is single-issuer and would need a multi-issuer extension to honour registered providers.
- **`accountSubscriptionsGet` is a trivial stub** — `AccountGet` already inlines the response shape we'd return.
- **`getTechnicalUserToken` is functional today** — `POST /oauth/token` is served by the public `authSvc.Handler()` mux entry, which intercepts before the chi handler. The stub in the account handler is dead code.

The three differences between subsystems (already-built vs greenfield, persistence story, route prefix conflicts) determine the split below.

## 2. Decisions

| Decision | Choice | Rationale |
|---|---|---|
| Number of replacement issues | Four sub-issues (#194-A, #194-B, #194-C, #194-D) plus **two** persistence follow-ups (one per store; rationale in §3.5/§3.6 below). Close #194. | Each sub-issue maps to a single coherent storage/handler shape. None depends on another for correctness; ordering between them is a convenience, not a constraint. |
| Group keypair + trusted under one sub-issue (#194-A) | Yes. | Both live behind the same `/oauth/keys/` prefix mux entry that gets removed once OpenAPI routing takes over. Splitting them creates an ordering requirement (whoever lands second removes the legacy mux entry) without simplifying review. |
| OIDC providers in v0.8.0? | ~~No. Sub-issue D ships in a later release.~~ **Amended 2026-06-16: Yes — in scope for v0.8.0.** Sub-issue D (#284) ships in v0.8.0; the release branch stays open until it lands. | Original rationale (greenfield ~7 endpoints + validator extension + JWKS-per-provider source needing its own brainstorm/spec/plan cycle) remains technically accurate, but the project lead has classified OIDC as the v0.8.0 milestone's headline IAM deliverable and accepts the schedule slip. The work is non-trivial — design via the normal brainstorm → spec → plan gates, expect a multi-week implementation window. |
| Persistence for in-memory stores | Two separate follow-ups — one for signing-key storage (§3.5), one for M2M-client persistence (§3.6) — not in-scope for #194-A or #194-B. Reference each follow-up from its corresponding sub-issue. | The two stores have very different secret-material profiles. `InMemoryKeyStore` holds raw RSA private keys; naïve disk persistence is the exact risk that drives KMS/HSM/Vault designs, so its follow-up is a secrets-management interface design. `InMemoryM2MClientStore` holds bcrypt-hashed secrets; persisting those is the conventional pattern every web app uses, so its follow-up is a straightforward storage-SPI change. Conflating them would force the easier one through the harder design discussion. |
| Role-name reconciliation (`SUPER_USER` vs `ROLE_ADMIN`) | Per sub-issue. Each adapter keeps the existing `requireAdmin` (ROLE_ADMIN) gate; OpenAPI prose is updated to match the wire reality. | The OpenAPI spec descriptions say "SUPER_USER" but the security scheme is just `bearerAuth: []`; the role name is implementation-level. Aligning prose to code (rather than the reverse) is the smaller, less risky change. If we later want a project-wide role rename, that's a tiny separate issue. |

## 3. Sub-issues

Each subsection below is the seed for the GitHub issue body. Implementation specs come later, one per sub-issue, via the normal brainstorming → writing-plans cycle.

### 3.1 #194-A — OpenAPI conformance for `/oauth/keys/*` (keypair + trusted)

**Goal.** Make the JWT keypair and trusted-key admin endpoints respond with the OpenAPI-declared DTOs through the generated chi router, and remove the legacy `/oauth/keys/` prefix mux entry.

**In scope (10 operations):**

- `issueJwtKeyPair`, `getCurrentJwtKeyPair`, `deleteJwtKeyPair`, `invalidateJwtKeyPair`, `reactivateJwtKeyPair`
- `listTrustedKeys`, `registerTrustedKey`, `deleteTrustedKey`, `invalidateTrustedKey`, `reactivateTrustedKey`

**Approach.**

- Add per-operation methods to `internal/domain/account/handler.go` that call into `AuthService.KeyStore()` / `AuthService.TrustedKeyStore()` directly. Skip the existing `keys.go` / `trusted.go` HTTP handlers — their path-parsing is redundant once chi routes per operation.
- Conform response/request bodies to `JwtKeyPairResponseDto`, `IssueJwtKeyPairRequestDto`, `TrustedKeyResponseDto`, `RegisterTrustedKeyRequestDto`.
- Remove `mux.Handle("/oauth/keys/", authMW(authSvc.AdminHandler()))` in `app/app.go`. Update `AdminHandler()` (`internal/auth/service.go` adminMux block) to drop the `/oauth/keys/keypair` and `/oauth/keys/trusted` entries.
- Keep `requireAdmin` (ROLE_ADMIN) at the new adapter; update OpenAPI descriptions from "SUPER_USER" to "ROLE_ADMIN".
- **Trusted-key registration feature flag.** The OpenAPI spec declares `cyoda.security.web.iam.trustedKeyRegistrationEnabled` (default disabled, returns 404 when disabled) for the trusted-key endpoints. The flag is not implemented anywhere in the Go tree today. The sub-issue must decide: implement the flag inline (config plumbing + 404 path), OR strip the prose + 404 declarations from `api/openapi.yaml` and accept the simplification. Do not silently drop the flag.
- **Body-size-limit tests in `internal/auth/integration_test.go:268-300`.** Those tests route through `svc.AdminHandler()` to exercise body-size limits on `/account/m2m` and `/oauth/keys/trusted/registered` paths. Once the adminMux entries are removed, they will 404. The sub-issue must relocate the body-size assertion to the chi-handler level (or delete if redundant under the new adapter's input validation).
- E2E coverage in `internal/e2e/` per Gate 2.
- Remove 501 declarations from these 10 paths in `api/openapi.yaml`; update the audit table disposition from `out-of-scope-not-implemented` to `match`.

**Notes for the implementation spec.**

- The existing `keys.go` / `trusted.go` HTTP handlers may be removable in full once chi takes over; verify no test depends on them at the HTTP layer.
- Persistent storage for signing keys — see follow-up §3.5. Trusted keys already have a persistent variant (`kv_trusted_store.go`); this sub-issue must wire the persistent variant in for `AuthConfig.TrustedKeyStore` if it isn't already, so trusted-key registration via the OpenAPI surface survives restart.
- This sub-issue and #194-B both edit the `adminMux` block in `internal/auth/service.go`. If the two PRs are open concurrently expect a trivial merge conflict; resolve by keeping the union of removals.

**Milestone:** v0.8.0.

---

### 3.2 #194-B — OpenAPI conformance for `/clients` (technical users)

**Goal.** Surface the existing M2M client management at the spec-conformant `/clients` paths with the spec-conformant DTOs. Retire `/account/m2m`.

**In scope (5 operations):**

- `listTechnicalUsers` (`GET /clients`)
- `createTechnicalUser` (`POST /clients`)
- `deleteTechnicalUser` (`DELETE /clients/{clientId}`)
- `resetTechnicalUserSecret` (`PUT /clients/{clientId}/secret`)
- `getTechnicalUserToken` (`POST /oauth/token`) — already functional via the public mux; drop the dead stub in the account handler and remove the 501 declaration from the spec.

**Approach.**

- Add per-operation methods to `internal/domain/account/handler.go` calling `AuthService.M2MClientStore()` directly.
- Conform DTOs to `TechnicalUserDto`, `TechnicalUserCredentialsDto`, `DeleteTechnicalUser200ResponseDto`.
- Handle the `withAdminRole` query parameter and the `cyoda.security.web.jwt.m2m.admin-role-enabled` feature flag described in the spec. If the feature flag mechanism doesn't exist yet, decide: implement minimally inline OR carve out a one-paragraph follow-up issue tracking that single parameter (not the whole subsystem). Do not silently swallow the parameter.
- Remove `mux.Handle("/account/m2m/", …)` and `mux.Handle("/account/m2m", …)` in `app/app.go`, and drop the matching entries from the `adminMux` block in `internal/auth/service.go`. The `/account/m2m` paths are not OpenAPI-declared; their removal is a private-surface cleanup with no public-API impact. `internal/auth/m2m_test.go` exercises the handler directly with mocked request paths and continues to pass — but `internal/auth/integration_test.go:271-286` routes through `svc.AdminHandler()` to assert body-size limits on `POST /account/m2m`. The sub-issue must relocate that assertion to the chi-handler level (or delete if redundant under the new adapter's input validation).
- Keep `requireAdmin` (ROLE_ADMIN); update OpenAPI prose.
- E2E coverage.
- Remove 501 declarations from these 5 paths; audit table disposition → `match`.

**Notes for the implementation spec.**

- Persistent storage — see follow-up §3.6. M2M secrets are bcrypt-hashed at the store boundary, so persistence is a conventional storage-SPI change modelled on `kv_trusted_store.go`; the secrets-management discussion in §3.5 does not gate this work.
- This sub-issue and #194-A both edit the `adminMux` block in `internal/auth/service.go`. If the two PRs are open concurrently expect a trivial merge conflict; resolve by keeping the union of removals.

**Milestone:** v0.8.0.

---

### 3.3 #194-C — `accountSubscriptionsGet` returns a proper `SubscriptionsResponseDto`

**Goal.** Replace the 501 stub with the "unlimited" subscription entry that `AccountGet` already inlines, in the response shape the OpenAPI spec declares.

**In scope (1 operation):** `accountSubscriptionsGet` (`GET /account/subscriptions`).

**Approach.**

- Emit `SubscriptionsResponseDto` — `{ "subscriptions": [SubscriptionDto] }` — with one entry identical to the `currentSubscription` block in `AccountGet` (`internal/domain/account/handler.go:48-55`). (Note: the response is an *object wrapping* the array per `api/openapi.yaml:9959-9967`, not a bare array — easy to get wrong.)
- Extract a shared helper that returns the "unlimited" `SubscriptionDto` from one place, and call it from both `AccountGet` and the new `AccountSubscriptionsGet`. Per Gate 6 ("resolve, don't defer"): this is bounded (~10 lines), reversible (its own commit), and within the sub-issue's scope.
- E2E test verifying response shape.
- Remove the 501 declaration; audit table disposition → `match`.

**Milestone:** v0.8.0.

---

### 3.4 #194-D — OIDC providers subsystem (greenfield)

**Goal.** Implement provider registration, lifecycle, and federated-token acceptance.

**In scope (7 operations):**

- `listOidcProviders`, `registerOidcProvider`, `updateOidcProvider`, `deleteOidcProvider`, `invalidateOidcProvider`, `reactivateOidcProvider`, `reloadOidcProviders`

**Approach (sketch only — full implementation requirements live in the #284 issue body after the 2026-06-16 #123 consolidation; brainstorm/spec land under `docs/superpowers/specs/`).**

- New `OidcProviderStore` interface + per-plugin implementations across memory / sqlite / postgres / cassandra. The cyoda-cloud reference (`JWKOIDCEntity`) is the data-model port target.
- Per-provider JWKS source wired into a multi-issuer extension of `KeySource` / `validator.go`. Today `JWKSValidator.issuer` is a single string; OIDC providers require it to become a registry indexed by `iss` claim. This is the one-way dependency from OIDC into the validator; no other sub-issue is affected.
- Provider lifecycle (`active` / `invalidated`) honoured by the validator: tokens from invalidated providers reject.
- `reloadOidcProviders` semantics: clear and refetch JWKS caches for all active providers; new `spi.ClusterBroadcaster` message types (`oidc.provider.reload`, `oidc.provider.invalidate`, `oidc.provider.reload_all`) for cross-node cache invalidation, modelled on `JWKOIDCCacheIntercom`.
- E2E parity coverage (28 scenarios ported from cyoda-cloud's `OIDCProviderControllerIT.kt`) with a fixture IdP (httptest.Server-backed).
- New env vars `CYODA_OIDC_REQUIRE_HTTPS`, `_CONNECT_TIMEOUT_MS`, `_SOCKET_TIMEOUT_MS`, `_CONNECTION_REQUEST_TIMEOUT_MS` documented in `cmd/cyoda/help/content/config/` per Gate 4.
- Remove 501 declarations from these 7 paths in `api/openapi.yaml`.

**Milestone:** v0.8.0 (amended 2026-06-16; was v0.9.0+).

---

### 3.5 (follow-up) — Secrets-management interface for the signing-key store

**Goal.** Replace `InMemoryKeyStore`'s naïve in-process holding of raw RSA private keys with a secrets-management-aware interface that production deployments can back with KMS, HSM, Vault, AWS Secrets Manager, or equivalent — while simple setups continue to work without external infrastructure.

**Why this is its own design effort.**

`InMemoryKeyStore` holds the raw `*rsa.PrivateKey` of every active and rotated signing keypair (`internal/auth/store.go: KeyPair.PrivateKey`). Naïve disk persistence — writing PEM-encoded private keys to a file the process owns — is the exact failure mode that motivates KMS/HSM design. Establishing the right interface (sign/verify operations rather than key-export operations, where supported), the default no-external-infra implementation, the boundary with the existing `CYODA_JWT_SIGNING_KEY_PEM`/`_FILE` bootstrap mechanism, and the migration story for in-flight rotated keys is a meaningful design discussion in its own right.

**Open design questions to settle before implementation.**

- **Operation-shaped vs storage-shaped interface.** Does the SPI expose `Sign(kid, payload) ([]byte, error)` (operation-shaped, lets KMS implementations never export keys) or `Get(kid) (*KeyPair, error)` (storage-shaped, matches today's call sites but forces every backend to export raw private keys)? Operation-shaped is harder to retrofit but is the only design that lets the secrets-manager-backed implementation actually be more secure than today's in-memory variant.
- **Default implementation.** What ships as the no-external-infra default? Options: stay in-memory (no change in behaviour for rotated keys; bootstrap key still injected at startup as today); ephemeral file with explicit "dev-only" gating; deferred until a secrets-manager-backed implementation is needed.
- **Boundary with bootstrap.** `CYODA_JWT_SIGNING_KEY_PEM` (+ `_FILE`) currently provides the bootstrap signing key. Does the secrets-management interface subsume this, or do they coexist? Affects deterministic-KID derivation for multi-node clusters (ARCHITECTURE.md §7.2 "Deterministic KID derivation").
- **Migration story.** First-boot behaviour, restart-during-rotation safety, what happens to in-flight rotated keys on upgrade.

**Not in scope of #194's replacements.** Today's `main` already runs in-memory; the visible OpenAPI surface for `POST /oauth/keys/keypair` is 501. Shipping #194-A in v0.8.0 changes that — rotated keys created via API are wiped on restart. v0.8.0 release notes should call out the bootstrap key (the `CYODA_JWT_SIGNING_KEY_PEM`-derived KID) survives restart, but API-rotated keys do not, and point at this follow-up issue.

**Milestone:** unscheduled. Pre-design discussion required before brainstorming.

### 3.6 (follow-up) — Persist `InMemoryM2MClientStore` via storage SPI

**Goal.** Make M2M client credentials survive process restart, using the existing storage SPI pattern already established by `kv_trusted_store.go`.

**Why this is separable from §3.5.**

`InMemoryM2MClientStore` holds bcrypt-hashed secrets (`internal/auth/store.go:324, 346`). Bcrypt hashes are the conventional persistence shape every web application uses for credential storage — the secrets-management-vs-disk question that drives §3.5 does not arise here. The follow-up is a straightforward storage-SPI change: model the M2M store after `kv_trusted_store.go`'s pattern, with in-memory cache + write-through to a `KeyValueStore` backend.

**In scope (when scheduled).**

- New `KVM2MClientStore` analogous to `KVTrustedKeyStore`. Same in-memory-cache + KV-write-through pattern.
- Wire `AuthConfig.M2MClientStore` similarly to the existing `AuthConfig.TrustedKeyStore` (optional; default in-memory).
- Backfill on first read.
- E2E coverage: M2M client created via `POST /clients`, server restarted, credentials still valid for `POST /oauth/token`.

**Not in scope of #194's replacements.** Today's `main` already runs in-memory M2M; the visible OpenAPI surface for M2M creation is 501. Shipping #194-B in v0.8.0 changes that — customers can create M2M clients via `POST /clients` and a server restart silently wipes their credentials. v0.8.0 release notes should call this out as a known limitation pointing at this follow-up.

**Milestone:** unscheduled. Smaller scope than §3.5; can land independently and probably sooner.

**Milestone:** unscheduled.

## 4. Horizontal decisions captured once

These cut across the sub-issues. Recording here means each sub-issue's spec can reference this document instead of re-litigating.

| Topic | Decision |
|---|---|
| Role gate at admin endpoints | Keep `requireAdmin` (ROLE_ADMIN). Update OpenAPI prose to match. |
| 501 declarations | Removed per sub-issue as that subsystem's adapters land. |
| Audit table updates | Per sub-issue. Disposition `out-of-scope-not-implemented` → `match`. |
| E2E coverage | Required per Gate 2 for every adapter that touches the HTTP surface. |
| Persistence | Out of scope; tracked in §3.5 (signing keys) and §3.6 (M2M). Each sub-issue's spec links to its corresponding follow-up under "known limitation". |

## 5. Action

1. Raise four sub-issues: #194-A, #194-B, #194-C, #194-D, each milestoned per §3.
2. Raise the two persistence follow-up issues (§3.5 secrets-management for signing keys; §3.6 storage-SPI for M2M), both unmilestoned. §3.5's body captures the secrets-mgmt design ask and the operation-shaped-vs-storage-shaped question; §3.6's body points at `kv_trusted_store.go` as the model.
3. Close #194 with a comment listing the six replacements (four sub-issues + two follow-ups). No code changes against #194 directly.
4. This umbrella spec is committed to `docs/superpowers/specs/`. Sub-issue specs (one per #194-A/B/C/D when they're worked) cross-reference back to it.
