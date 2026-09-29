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
| M2M `client_credentials` | the client's stored tenant = the creating admin's tenant | `token.go:106`, `m2m_adapter.go:130` |
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
| `POST /oauth/keys/keypair` | `account.IssueJwtKeyPair` | key pairs sign every tenant's tokens (`kv_key_store.go:66-68`) |
| `GET /oauth/keys/keypair/current` | `account.GetCurrentJwtKeyPair` | same key set (public key only; gated for a uniform surface) |
| `POST /oauth/keys/keypair/{keyId}/invalidate` | `account.InvalidateJwtKeyPair` | can end the bootstrap key for the cluster |
| `POST /oauth/keys/keypair/{keyId}/reactivate` | `account.ReactivateJwtKeyPair` | same |
| `DELETE /oauth/keys/keypair/{keyId}` | `account.DeleteJwtKeyPair` | same; deleting the bootstrap key is permanent |
| `POST /oauth/oidc/providers/reload` | `oidcAdapter.ReloadOidcProviders` | rebuilds every tenant's providers and refetches every tenant's JWKS on every node (`oidc/service.go:250-256`) |
| `GET` / `POST /admin/log-level` | `internal/api` admin handlers | process-wide log level of the node |
| `GET` / `POST /admin/trace-sampler` | `internal/api` admin handlers | process-wide trace sampler of the node |

The guard runs where `RequireAdmin` runs today, before every other check, so
each endpoint's other responses keep their order.

Tenant-scoped admin endpoints stay on `RequireAdmin`: trusted keys, M2M
clients, OIDC provider register / update / invalidate / reactivate / delete,
and model and workflow administration.

A tenant admin who needs their own provider's keys refetched sends a `PATCH`
that changes nothing. `Update` reloads that one provider on every node with no
gap in service (`oidc/service.go:143-146`).

### 4.3 Wiring

- `account.New` takes an `auth.OperatorGuard`. The key-pair adapters and the
  OIDC reload adapter call it in place of `RequireAdmin`.
- The four `/admin/*` handlers in `internal/api/admin.go` become methods of a
  small constructed type holding the guard. `app/app.go` builds it and
  registers its methods (`app/app.go:625-628`). Their inline check, which
  answers 403 when no `UserContext` is present, is removed; the guard answers
  401.
- `app/app.go` builds the guard from `cfg.IAM.Mode` and passes the same value
  to both.

### 4.4 Mock IAM mode

Mock mode has one fixed principal, in tenant `mock-tenant`, with roles from
`CYODA_IAM_MOCK_ROLES` (`app/config.go:410-412`). In mock mode the guard
applies only the `ROLE_ADMIN` check. The mode is chosen at wiring time from
`cfg.IAM.Mode`, never by comparing a tenant id with `mock-tenant`. Effects:
- the key-pair endpoints still answer 501 (the guard passes, then
  `requireKeyStore` answers), as `TestGated_MockIAM_All21Return501` requires;
- OIDC reload still answers 501 from the unwired stub, before any auth check
  (`account/handler.go:41-50`);
- `/admin/*` keeps working for a mock principal that holds `ROLE_ADMIN`,
  and answers 403 for one that does not, as today.

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
| Platform operator through a `PLATFORM` admin M2M client allowed (the recovery route) | — | issue a key pair with its `/oauth/token` token | — | n/a |
| An OIDC principal with `ROLE_ADMIN` refused on an operator endpoint (the self-grant route) | — | — | reload and issue → 403 | n/a |
| No token → 401 on each endpoint | — | one test per endpoint | — | n/a |
| Mock mode: key-pair endpoints 501, `/admin/log-level` works for the mock admin | — | existing `TestGated_MockIAM_All21Return501` + one `/admin` test | — | n/a |
| Key-pair lifecycle across nodes, as operator | — | — | multinode `RunSigningKeyPairFollowsTheCluster` switched to the operator | n/a |

gRPC waiver: cyoda-go has no gRPC entry point for any operator endpoint. The
only service is `CloudEventsService` (`proto/cyoda/cyoda-cloud-api.proto:9-15`).
Concurrency: not applicable; the guard is stateless.

## 7. Test fixtures

- `fixtureutil` gets `MintPlatformOperatorJWT(t, keySet)`: `ROLE_ADMIN` in
  tenant `PLATFORM`.
- `parity.BackendFixture` gets a **required** method `PlatformOperator(t)
  Tenant`. An optional capability with `t.Skip` would drop key-pair coverage
  silently on a backend that does not implement it. Every in-tree fixture
  (memory, sqlite, postgres, postgres multinode) implements it with the
  `fixtureutil` helper.
- `PLATFORM` is one shared tenant, which differs from `NewTenant`'s "fresh per
  call" contract. Scenarios use the operator token only on operator endpoints,
  never for tenant data.
- `RunSigningKeyPairLifecycle` (`e2e/parity/signing_keys.go:21`) and the
  multinode `RunSigningKeyPairFollowsTheCluster`
  (`e2e/parity/multinode/signing_keys.go:115`) switch to the operator token.
- `RunOidcD23_PerProviderRolesClaim` (`e2e/parity/oidc.go:2316-2350`) uses
  reload to probe that an OIDC `ROLE_ADMIN` is honoured. The probe moves to a
  tenant-scoped admin endpoint, as `RunOidcD23_RolesParsingMultiFormat`
  already does.
- `internal/e2e`: the tests that call operator endpoints with `suiteToken`,
  `bootstrapToken`, `operatorToken` or the key stack's M2M client switch to
  `PLATFORM` tokens (`oauth_keys_test.go`, `signing_keys_test.go`,
  `admin_loglevel_test.go`, `cyoda_token_test.go`, `auth_failures_test.go`,
  `cors_e2e_test.go`, `keys_trusted_reconciliation_test.go`, plus any other
  caller the plan's grep finds).
- Cassandra (`../cyoda-go-cassandra/e2e/fixture.go`): a matching PR adds
  `PlatformOperator`. It lands with cassandra's v0.9.0 pin bump (cassandra#108).

## 8. Documentation

- `cmd/cyoda/help/content/cli/token.md`: key-pair management needs
  `--tenant PLATFORM`; the recovery text of §4.7; an example.
- `cmd/cyoda/help/content/config/auth.md`: the key-pair section names the
  platform operator; the recovery text (`:325-329`, `:397-409`) as in §4.7.
- `cmd/cyoda/help/content/auth.md` and `auth/tokens.md`: where they name who
  may manage key pairs.
- `cmd/cyoda/help/content/admin.md:28`: `/admin/*` needs a platform operator.
- `cmd/cyoda/help/content/errors/FORBIDDEN.md`: the platform-operator cause.
- `cmd/cyoda/help/content/auth/oidc.md`: reload needs a platform operator; a
  tenant refreshes its own provider with a no-op `PATCH`.
- `api/openapi.yaml`:
  - the six operator operations' descriptions and 403 texts;
  - `:126` and `:147` say M2M client creation needs `SUPER_USER`; it needs
    `ROLE_ADMIN`, and cyoda-go has no `SUPER_USER`.
  - Run `go generate ./api` after the edits.
- `README.md`: the endpoint table (`:128-134`) and any key-pair rows.
- `docs/ARCHITECTURE.md`: the trace-sampler (`:2318`) and `cyoda token`
  (`:1886`) passages, audited as a whole on touch.
- `CHANGELOG.md` `### Breaking`: the operator endpoints need `ROLE_ADMIN` in
  tenant `PLATFORM`; a tenant admin now gets 403.
- `docs/cloud-parity/platform-operator.md` and its row in
  `docs/cloud-parity/README.md` (§9).

No env var changes, so `DefaultConfig()` and the config registry are
untouched.

## 9. Cloud parity

The contract Cloud follows:
- The ten operator endpoint/method pairs (§4.2) require `ROLE_ADMIN` in the
  tenant (legal entity) `PLATFORM`. A tenant admin gets `403`.
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
