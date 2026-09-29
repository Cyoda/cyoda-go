# Platform operator for platform-wide admin endpoints (#624)

## 1. Goal

Some admin endpoints change state that every tenant shares. Only a **platform
operator** may call them: the party that runs the deployment, never an admin
of an ordinary tenant.

## 2. Terms

- **Tenant admin**: a caller whose `UserContext` carries `ROLE_ADMIN`, in any
  tenant.
- **Platform operator**: a caller whose `UserContext` carries `ROLE_ADMIN` and
  whose tenant id is exactly `PLATFORM` (bytewise, case-sensitive). In mock IAM
  mode, see §4.4.
- **Operator endpoint**: an endpoint that requires a platform operator (§4.2).
- **Signing key / bootstrap key**: the key in `CYODA_JWT_SIGNING_KEY`.
  `cyoda token` signs a token with it offline, for any tenant and any roles
  (`internal/auth/operator_token.go`).
- **Tenant binding**: every token source other than the signing key fixes the
  token's tenant to the tenant that set the source up (§3).

## 3. Why tenant binding makes the rule safe

A tenant admin can give their own principals any roles:
- an OIDC provider's roles claim is read unfiltered (`internal/auth/oidc/usercontext.go:47-51`);
- trusted-key token exchange copies the subject token's roles
  (`internal/auth/token.go:213-216`).

A tenant admin cannot get a principal in another tenant:

| Token source | Tenant comes from | Evidence |
|---|---|---|
| OIDC provider | the provider's owner; tenant claims are ignored | `oidc/usercontext.go:23-25,57-59` |
| M2M `client_credentials` | the client's stored tenant = the creating admin's tenant | `token.go:106`, `m2m_adapter.go:131` |
| Trusted-key token exchange | `caas_org_id` must equal the exchanging client's tenant | `token.go:153-157,222-225` |
| First-party JWT signed by cyoda's keys | `caas_org_id`; minted only by `/oauth/token` (rows above) or by the signing-key holder | `validator.go:133-147` |
| Cluster dispatch callout | request body, but only after cluster-key AEAD; never reaches an admin handler | `cluster/dispatch/handler.go:52,140-148` |

An OIDC provider needs a canonical-UUID tenant (`oidc_adapter.go:103-115`), so
`PLATFORM` cannot own one today. A `PLATFORM` principal therefore comes only
from:
- the signing-key holder: `cyoda token --tenant PLATFORM`;
- an admin M2M client that a `PLATFORM` admin creates in `PLATFORM`
  (`POST /clients?withAdminRole=true`, needs `CYODA_IAM_M2M_ADMIN_ROLE_ENABLED`);
- a trusted key that a `PLATFORM` admin registers in `PLATFORM`.

Every future token source must keep tenant binding for tenant isolation to
hold at all, so it keeps this rule safe too. A role-based rule would need
every token source to filter a reserved role, and one missed source would let a
tenant admin grant it to themselves.

`PLATFORM` is not `SYSTEM`. `SYSTEM` is the internal machinery's tenant: the
system principal and the store that holds the cluster's auth state
(`app/app.go:263-271`). It keeps its meaning and gets no rights from this
change.

## 4. Design

### 4.1 The rule

`internal/auth` gets:

- `PlatformTenantID spi.TenantID = "PLATFORM"`.
- `OperatorGuard`, a value built once at wiring time. `Require(w, r) bool`:
  - no `UserContext` → `401 UNAUTHORIZED`, "authentication failed";
  - not `ROLE_ADMIN` → `403 FORBIDDEN`, "platform operator required";
  - tenant ≠ `PLATFORM` (JWT mode) → `403 FORBIDDEN`, "platform operator required";
  - otherwise the caller may proceed.
- Its zero value applies the JWT rule, so a guard that was not wired fails
  closed.

The 403 is the same for both refusal reasons. `auth.RequireAdmin` stays, for
the tenant-scoped admin endpoints.

### 4.2 Operator endpoints

| Endpoint | Handler | Why it is platform-wide |
|---|---|---|
| `POST /oauth/keys/keypair` | `account.IssueJwtKeyPair` | key pairs sign every tenant's tokens (`kv_key_store.go:67-69`) |
| `GET /oauth/keys/keypair/current` | `account.GetCurrentJwtKeyPair` | same key set (public key only; gated for a uniform surface) |
| `POST /oauth/keys/keypair/{keyId}/invalidate` | `account.InvalidateJwtKeyPair` | can end the bootstrap key for the cluster |
| `POST /oauth/keys/keypair/{keyId}/reactivate` | `account.ReactivateJwtKeyPair` | same |
| `DELETE /oauth/keys/keypair/{keyId}` | `account.DeleteJwtKeyPair` | same; deleting the bootstrap key is permanent |
| `POST /oauth/oidc/providers/reload` | `oidcAdapter.ReloadOidcProviders` | rebuilds every tenant's providers and refetches every tenant's JWKS on every node (`oidc/service.go:250-256`) |
| `GET` / `POST /admin/log-level` | `internal/api` admin handlers | process-wide log level of the node |
| `GET` / `POST /admin/trace-sampler` | `internal/api` admin handlers | process-wide trace sampler of the node |

The guard runs where `RequireAdmin` runs today, first in each handler, so each
endpoint's other responses keep their order. Checks that run before the
handler are unchanged: the auth middleware's 401, and the generated wrapper's
400 for a missing `audience` on `GET .../current`. So a 403 test on that
endpoint sends `?audience=human`.

Tenant-scoped admin endpoints stay on `RequireAdmin`: trusted keys, M2M
clients, OIDC provider register / update / invalidate / reactivate / delete,
and model and workflow administration.

A tenant admin who needs their own provider's keys refetched sends a `PATCH`
that changes nothing. `Update` reloads that one provider on every node with no
gap in service (`oidc/service.go:143-146`).

### 4.3 Wiring

`app/app.go` builds one guard early in `New`, before the OIDC subsystem is
wired, and passes the same value to three consumers:
- `account.New` takes the guard; the five key-pair adapters call it in place
  of `RequireAdmin`.
- `account.NewOidcAdapter` (`app/app.go:324`, built before `account.New` at
  `:582`) takes the guard; `oidcAdapter.ReloadOidcProviders`
  (`oidc_adapter.go:434`) calls it in place of `RequireAdmin`. The guard does
  not go in the `Handler.ReloadOidcProviders` dispatch (`handler.go:121-127`),
  so the unwired stub still answers 501 before any auth check.
- The four `/admin/*` handlers in `internal/api/admin.go` become methods of a
  small constructed type holding the guard. `app/app.go` builds it and
  registers its methods (`app/app.go:625-628`). Their inline check, which
  answers 403 when no `UserContext` is present, is removed; the guard answers
  401.

`auth.RequireAdmin`'s doc comment (`admin_guard.go:10-11`) and the reload
adapter's comment (`oidc_adapter.go:433`) are updated to match.

### 4.4 Mock IAM mode

Mock mode has one fixed principal, in tenant `mock-tenant`, with roles from
`CYODA_IAM_MOCK_ROLES` (`app/config.go:410-412`). In mock mode the guard
applies only the `ROLE_ADMIN` check. The guard uses the mock rule if and only
if `cfg.IAM.Mode == "mock"`; it never compares a tenant id with `mock-tenant`.
Effects:
- the key-pair endpoints still answer 501 (the guard passes, then
  `requireKeyStore` answers), as `TestGated_MockIAM_All21Return501` requires;
- OIDC reload still answers 501 from the unwired stub, before any auth check
  (`account/handler.go:121-127`);
- `/admin/*` keeps working for a mock principal that holds `ROLE_ADMIN`,
  and answers 403 for one that does not, as today.

**The IAM mode must be `mock` or `jwt`.** Today any other value, for example
`CYODA_IAM_MODE=JWT`, passes `ValidateIAM` (`app/config.go:1000-1023`, which
special-cases only `"mock"`), wires mock authentication (`app/app.go:256`
tests `== "jwt"`), and skips the mock-mode warning (`cmd/cyoda/main.go:296`
tests `!= "mock"`). Every request is then a mock admin. `ValidateIAM` rejects
any other value, so startup fails. This keeps the two-way guard rule exact and
closes that fail-open.

### 4.5 Response codes

No new error code. `403 FORBIDDEN` already exists and is declared on every
operator endpoint in the OpenAPI spec (`/admin/*` is not in the spec). 404 is
not used: the endpoints are documented publicly, so a 404 would hide nothing.

### 4.6 Multi-node

The guard reads only the caller's `UserContext` and the IAM mode, so every
node gives the same answer. There is no new state.

### 4.7 Recovery after the bootstrap key is revoked

`PLATFORM` cannot own an OIDC provider, so an operator's ways back to the
key-pair endpoints are:
- a token from `cyoda token --tenant PLATFORM`, while the bootstrap key
  verifies (including its grace period);
- an admin M2M client in `PLATFORM`, created before the bootstrap key is
  revoked, while an issued `client` key pair signs its tokens;
- a new `CYODA_JWT_SIGNING_KEY` on every node, which retires every issued key
  pair and every token cyoda-go signed.

The help text states this and tells operators to create the `PLATFORM` admin
M2M client before revoking the bootstrap key. When #593 lets any tenant own an
OIDC provider, "an OIDC admin in `PLATFORM`" becomes a fourth way (noted on
#593).

## 5. Error and status table (operator endpoints)

Only the authorisation rows change. The endpoints' other codes (400, 404
`KEYPAIR_NOT_FOUND`, 500, 503) stay as they are.

| Endpoint | 401 `UNAUTHORIZED` | 403 `FORBIDDEN` | 501 `NOT_IMPLEMENTED` | Success |
|---|---|---|---|---|
| `POST /oauth/keys/keypair` | no or invalid token | not a platform operator | mock mode | 200 |
| `GET /oauth/keys/keypair/current` | same | same | mock mode | 200 |
| `POST .../{keyId}/invalidate` | same | same | mock mode | 200 |
| `POST .../{keyId}/reactivate` | same | same | mock mode | 200 |
| `DELETE .../{keyId}` | same | same | mock mode | 200 |
| `POST /oauth/oidc/providers/reload` | same | same | mock mode (before auth) | 200 |
| `GET /admin/log-level` | same (today 403 when no `UserContext`; now 401) | same | — | 200 |
| `POST /admin/log-level` | same | same | — | 200 |
| `GET /admin/trace-sampler` | same | same | — | 200 |
| `POST /admin/trace-sampler` | same | same | — | 200 |

The auth middleware answers 401 before any handler for a missing or invalid
token. The guard's own 401 branch is unreachable behind it and needs no E2E
cell of its own.

## 6. Coverage matrix

| Scenario | Unit | Running-backend E2E (`internal/e2e`, postgres) | Cross-backend parity (`e2e/parity`) | gRPC |
|---|---|---|---|---|
| Guard: no `UserContext` → 401; no `ROLE_ADMIN` → 403; `ROLE_ADMIN` in another tenant → 403; `ROLE_ADMIN` in `platform` (lower case) → 403; `ROLE_ADMIN` in `PLATFORM` → allowed; `PLATFORM` without `ROLE_ADMIN` → 403; zero value = JWT rule; mock rule: `ROLE_ADMIN` in any tenant → allowed, no `ROLE_ADMIN` → 403 | `internal/auth` | — | — | n/a |
| Tenant admin refused, each of the ten operator endpoint/method pairs → 403 `FORBIDDEN` | — | one test per endpoint | key-pair endpoints + reload | n/a |
| `PLATFORM` principal without `ROLE_ADMIN` refused → 403 | — | one representative endpoint per handler family (key pairs, reload, `/admin/*`) | — | n/a |
| Platform operator (`cyoda token`-shaped token, tenant `PLATFORM`) allowed on each endpoint | — | one test per endpoint | key-pair lifecycle + reload | n/a |
| Platform operator through a `PLATFORM` admin M2M client allowed (the recovery route) | — | issue a key pair with its `/oauth/token` token; `TestSigningKeys_OwnCluster` (`e2e/parity/postgres/signing_keys_cluster_test.go`) keeps key-pair control through such a client after the bootstrap key is gone | — | n/a |
| `ValidateIAM` refuses an IAM mode other than `mock` or `jwt` (for example `JWT`, `""`, `none`) and accepts both | `app` | — | — | n/a |
| An OIDC principal with `ROLE_ADMIN` refused on an operator endpoint (the self-grant route) | — | — | reload and issue → 403 | n/a |
| No token → 401 on each endpoint | — | one test per endpoint | — | n/a |
| Mock mode: key-pair endpoints 501, `/admin/log-level` works for the mock admin | — | existing `TestGated_MockIAM_All21Return501` + one `/admin` test | — | n/a |
| Key-pair lifecycle across nodes, as operator | — | — | multinode `RunSigningKeyPairFollowsTheCluster` switched to the operator | n/a |

gRPC waiver: cyoda-go has no gRPC entry point for any operator endpoint. The
only service is `CloudEventsService` (`proto/cyoda/cyoda-cloud-api.proto:9-15`).
Concurrency: not applicable; the guard is stateless.

## 7. Test fixtures

### 7.1 Fixture contract

- `fixtureutil` gets `MintPlatformOperatorJWT(t, keySet)`: `ROLE_ADMIN` in
  tenant `PLATFORM`.
- `parity.BackendFixture` and `multinode.MultiNodeFixture` (a separate
  interface, `e2e/parity/multinode/fixture.go:14`) each get a **required**
  method `PlatformOperator(t) parity.Tenant`. An optional capability with
  `t.Skip` would drop key-pair coverage silently on a backend that does not
  implement it. The in-tree fixtures (memory, sqlite, postgres, postgres
  multinode `pgMultiNode`) implement it with the `fixtureutil` helper.
- `PLATFORM` is one shared tenant, unlike `NewTenant`'s "fresh per call". A
  scenario uses the operator token only on operator endpoints, never for tenant
  data. The `parity.Tenant.ID` doc, which says the id is a UUID, is corrected.
- Cassandra (`../cyoda-go-cassandra/e2e/fixture.go`) implements only
  `BackendFixture`; a matching PR adds `PlatformOperator`. Cassandra `main` pins
  cyoda-go v0.8.3, so nothing breaks there until its v0.9.0 pin bump; the PR
  lands with that bump (cassandra#108).

### 7.2 Callers that switch to an operator token

A scenario that sets up tenant state (a provider, an M2M client, a model)
keeps its tenant-admin token for that and uses the operator token only for
the operator call.

Parity (`e2e/parity`):
- `RunSigningKeyPairLifecycle` (`signing_keys.go:21`): operator for every call.
- `multinode.RunSigningKeyPairFollowsTheCluster` (`multinode/signing_keys.go`):
  tenant for `newM2MClient` and `modelListStatus`; operator for issue,
  invalidate, reactivate and delete (`:113-175`).
- Reload from a tenant admin: `RunOidcD18_ReloadInvalidateSerializeLocally`
  (`oidc.go:1839`), `RunOidcD18_ReloadAllSerializesWithReloadOne` (`:1910`),
  `RunOidcReload_PreservesTokenAcceptance` (`:3192`),
  `RunOidcReload_AfterReactivateKeepsTokenAcceptance` (`:3235`): the tenant
  admin registers, the operator reloads. `RunOidcNonAdminReload` (`:479`) keeps
  its 403.
- `RunOidcD23_PerProviderRolesClaim` (`oidc.go:2316-2350`) probes an OIDC
  `ROLE_ADMIN` through reload. The probe moves to a tenant-scoped admin
  endpoint, as `RunOidcD23_RolesParsingMultiFormat` already does.
- `postgres/signing_keys_cluster_test.go` `TestSigningKeys_OwnCluster`: key-pair
  calls and its admin M2M client (`:30`, `:45`) move to `PLATFORM`. Its later
  calls run after the bootstrap key is gone, so this is the recovery-route test.

E2E (`internal/e2e`):
- `oauth_keys_test.go`: `adminRequest` (`:22`) stays for tenant endpoints; a new
  `operatorRequest` serves the key-pair tests.
- `keys_trusted_reconciliation_test.go`: its key-pair calls use
  `operatorRequest` (they would otherwise get 403 where they assert 400/404).
- `signing_keys_test.go`: `createKeyStackClient` (`:74`) creates the key
  stack's admin M2M client with a `PLATFORM` token, not the harness tenant's
  `h.token`; `bootstrapToken` (`:187`) mints for `PLATFORM`.
- `cyoda_token_test.go`: `operatorToken` (`:28`) mints for `PLATFORM` where the
  test calls an operator endpoint.
- `oidc_providers_test.go:119`: reload with an operator token.
- `admin_loglevel_test.go` and any trace-sampler test: operator token.
- `auth_failures_test.go`, `cors_e2e_test.go`: checked by the plan's grep for
  operator-endpoint calls with a tenant token.

Unit tests:
- `internal/api/admin_test.go`: package functions become methods; the
  nil-context tests (`:50`, `:96`, `:109`, `:134`, `:230`, `:400`) expect 401.
- Key-pair adapter tests with tenant `t1`/`t`: `account/keys_adapter_test.go:28`,
  `keys_adapter_errors_test.go:42`, `grace_field_name_test.go`,
  `internal/auth/keypair_signing_test.go:18,38`.
- Reload adapter tests: `account/oidc_adapter_test.go:696-760`,
  `handler_test.go:88`.
- Every `account.New(...)` and `account.NewOidcAdapter(...)` call site takes
  the guard (about 35, all in `internal/domain/account/*_test.go` and
  `internal/auth/keypair_signing_test.go`).

## 8. Documentation

- `cmd/cyoda/help/content/cli/token.md`: key-pair management needs
  `--tenant PLATFORM`; the recovery text of §4.7; an example.
- `cmd/cyoda/help/content/config/auth.md`: the key-pair section names the
  platform operator; the recovery text (`:325-329`, `:387`, `:397-409`) as in
  §4.7; `CYODA_IAM_MODE` accepts only `mock` or `jwt`, and any other value
  fails startup.
- `cmd/cyoda/help/content/auth.md` and `auth/tokens.md`: where they name who
  may manage key pairs.
- `cmd/cyoda/help/content/admin.md:28` and `telemetry.md:233`: `/admin/*`
  needs a platform operator.
- `cmd/cyoda/help/content/errors/FORBIDDEN.md` (including `:24`, "role claims
  determine access"): the platform-operator cause.
- `cmd/cyoda/help/content/auth/oidc.md`: reload needs a platform operator;
  `:76` and `:187` tell tenants to use reload, and become the no-op `PATCH`.
- `api/openapi.yaml`:
  - the six operator operations' descriptions and 403 texts;
  - `:126` and `:147` say M2M client creation needs `SUPER_USER`; it needs
    `ROLE_ADMIN`, and cyoda-go has no `SUPER_USER`.
  - Run `go generate ./api` after the edits.
- `README.md`: the endpoint table (`:128-134`), any key-pair rows, and `:136`,
  which says reload refetches "for the tenant" (it reloads every tenant).
- `docs/ARCHITECTURE.md`: the OIDC endpoints (`:1914`), `:1897`, `:2095`, the
  trace-sampler (`:2318`) and `cyoda token` (`:1886`) passages; the document is
  audited as a whole on touch.
- `CHANGELOG.md` `### Breaking`: the operator endpoints need `ROLE_ADMIN` in
  tenant `PLATFORM`, and a tenant admin now gets 403; `CYODA_IAM_MODE` other
  than `mock` or `jwt` now fails startup.
- `COMPATIBILITY.md` (v0.9.0 obligations, `:139-165`): an out-of-tree plugin's
  parity fixture must implement `PlatformOperator`.
- `docs/cloud-parity/platform-operator.md` and its row in
  `docs/cloud-parity/README.md` (§9).

No new env var and no default changes, so `DefaultConfig()` and the config
registry are untouched; `CYODA_IAM_MODE`'s accepted values are documented in
its help topic.

## 9. Cloud parity

The contract Cloud follows:
- The six OpenAPI operations (the five key-pair operations and OIDC reload)
  require `ROLE_ADMIN` in the tenant (legal entity) `PLATFORM`. A tenant admin
  gets `403`. `/admin/log-level` and `/admin/trace-sampler` are cyoda-go only;
  Cloud has no such endpoints.
- The rule depends on tenant binding. Cloud does not bind tenants today: it
  takes the legal entity of any JWT, including one from an external OIDC
  provider or a trusted key, from its `caas_org_id` claim
  (`backend/.../iam/integration/AbstractCaasOidcComponents.kt:47-54,158-168`).
  So these are **prerequisites**, not follow-ups:
  1. the legal entity of an externally issued token comes from the provider's
     or key's owner, not from a claim;
  2. `PLATFORM` and `SYSTEM` cannot be claimed by an external token. In Cloud
     `SYSTEM` also skips entitlement enforcement
     (`entitlements-common/.../CyodaEntitlements.kt:72-73`).
- Found in Cloud during this work, for the same ticket:
  - the key-pair invalidate / reactivate / delete endpoints look a key up by
    `keyId` only (`platform-service-iam/.../jwk/StoredJWKService.kt:587-596`), so they also reach other
    tenants' trusted keys and OIDC keys;
  - a denied role answers 401, not 403 (`tree-node/tree-node-api/.../TdbRestControllerAdvice.kt:41-58`),
    while Cloud's own OpenAPI documents 403;
  - `SUPER_USER` gives no platform-level right in the contract. Cloud's Auth0
    login action adds it to every user
    (`scripts/auth0/action-setup-cyoda-token-claims.js:22`).
  - OIDC providers are not scoped to a tenant (`platform-service-iam/.../oidc/JWKOIDCService.kt:188-200`).
- The parity doc gets the CaaS ticket number once it is filed.

## 10. Out of scope

- #593: OIDC tenant as an opaque string. It needs no change for this design;
  when it lands, `PLATFORM` can own an OIDC provider (commented on #593).
- #638: a tenant can break another tenant's OIDC login by registering the same
  identity provider. It is a tenant-scoped endpoint's defect, not a
  platform-wide resource.
- Operator sign-in through a configurable operator tenant: rejected; the
  tenant is fixed.
