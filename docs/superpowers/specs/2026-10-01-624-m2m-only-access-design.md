# M2M-only access with on-behalf-of user identity

Companion page: `docs/access-to-the-cyoda-api.html` (swimlane scenarios and
reference). This spec is the implementation contract; where the two differ,
this spec wins and the page is corrected.

## 1. Requirement and boundary

- No regular user accesses cyoda-go directly. cyoda-go has no per-user data
  permissions, and does not acquire any.
- Only M2M clients authenticate to cyoda-go.
- Work an M2M client does on behalf of an application user carries that user's
  identity into the entity history, the audit events and the gRPC callouts.
- The application decides whether a user may perform an action, before it acts.
- cyoda-go records what an authenticated client states about the user; it does
  not verify the user. Like a database, cyoda-go does not protect data from a
  compromised application. That responsibility lies with the application.

The one exception to "only M2M clients" is the platform operator's offline
token (`cyoda token`), signed with `CYODA_JWT_SIGNING_KEY`. It stays a
user-kind principal with no client and no executor (§5).

## 2. Model

- **M2M client**: client id and secret, owned by one tenant. Gets a cyoda token
  with `client_credentials`.
- **On-behalf-of (OBO) client**: an M2M client created with the on-behalf-of
  permission. It only performs token exchanges.
- **Trusted key**: a public key a tenant admin registers for the application.
- **User assertion**: a JWT the application signs with its trusted key, naming
  the user.
- **OBO token**: the cyoda token a token exchange returns. Its rights are the
  OBO client's; its user is the asserted user, recorded and never authorized.
- **Attributed user**: who a change is for.
- **Executor**: who actually made it (the M2M client, or `system`).

| Situation | Attributed user | Executor |
|---|---|---|
| OBO client acts for alice | alice (kind user) | the OBO client (kind service) |
| A client works for no user | the client | the client |
| Compute node writes back during alice's transaction | alice | the compute client |
| Scheduled transition armed by alice fires | alice | `system` |
| Platform operator token | the operator user | the operator user |

## 3. Clients

### 3.1 Permissions

A client record (`auth.M2MClient`) gains `OnBehalfOf bool`, set at creation
and immutable. It is a record flag, not a role, so it never appears in
`authclaims`.

| Client | Roles | `OnBehalfOf` | May use |
|---|---|---|---|
| plain | `ROLE_M2M` | false | `client_credentials` |
| admin | `ROLE_M2M`, `ROLE_ADMIN` | false | `client_credentials` |
| OBO | `ROLE_M2M` | true | token exchange only |

Rules, enforced by cyoda-go:

- An OBO client never holds `ROLE_ADMIN`: `POST /clients?withAdminRole=true&onBehalfOf=true` is refused.
- An OBO client never exists in tenant `PLATFORM`.
- `client_credentials` with an OBO client is refused (`unauthorized_client`).
- A token exchange by a non-OBO client is refused (`unauthorized_client`),
  before the subject token is parsed.

### 3.2 `POST /clients`

New query parameter `onBehalfOf` (boolean, default false). `TechnicalUserDto`
and the list response gain `onBehalfOf`. The codec (`kv_m2m_codec.go`) stores
it; records without it read as false.

### 3.3 Data operations require `ROLE_M2M`

Every data endpoint, HTTP and unary gRPC, requires `ROLE_M2M` in the caller's
roles; otherwise `403 FORBIDDEN`. Account and operator endpoints keep their own
guards. The operator token from `cyoda token` carries the roles it was signed
with; `--roles` decides whether it can touch data. Mock mode's principal
already carries `ROLE_M2M`.

## 4. Token endpoint

### 4.1 `client_credentials`

Unchanged, except:

- an OBO client is refused (§3.1);
- `expires_in` is the issued token's remaining life, computed, not a constant;
- the per-client rate limit applies (§4.4).

Claims: `sub` = `caas_user_id` = the client's user id (its client id),
`caas_org_id` = the client's tenant, `scopes` = the client's roles, `iss`,
`aud` if configured, `iat`, `exp`, `jti`.

### 4.2 Token exchange (OBO)

Request: `grant_type=urn:ietf:params:oauth:grant-type:token-exchange`, client
authentication by HTTP Basic, `subject_token` = the user assertion,
`subject_token_type=urn:ietf:params:oauth:token-type:jwt`.

Processing, in this order:

1. Authenticate the client (store read). Unknown client or bad secret:
   `401 invalid_client`.
2. The client must be an OBO client: else `400 unauthorized_client`.
3. Rate limit (§4.4): `429`.
4. Any of `actor_token`, `actor_token_type`, `resource`, `audience`, `scope`,
   `requested_token_type` present: `400 invalid_request`.
5. `subject_token_type` other than `…:jwt`: `400 invalid_request`.
6. Parse the assertion; `alg` must be RS256 and `kid` present: else
   `400 invalid_request`.
7. Read the trusted key `(client's tenant, kid)` from the shared store (§6.1).
   Not found or invalidated: `400 invalid_request`. Store failure: `503`.
   Not yet valid (`validFrom`): `400 invalid_request`.
8. Verify the signature: else `400 invalid_request`.
9. If the key lists issuers, `iss` must be one: else `400 invalid_request`.
10. `aud` must contain `CYODA_JWT_ISSUER`; `exp` and `iat` required; `exp − iat`
    at most 300 s; not expired and `iat` not in the future (30 s skew):
    else `400 invalid_request`.
11. `caas_org_id` must equal the client's tenant: else `403 access_denied`.
12. `sub` must pass `ValidateUserID` and must not be reserved (§5.3): else
    `400 invalid_request`.
13. Mint the OBO token (§4.3). If its `exp` would not be in the future:
    `400 invalid_request`. Signing failure: `500`, ticketed.

Error bodies are OAuth-shaped (`error`, `error_description`); descriptions
never echo the subject, the tenant or the key id. A `401` carries
`WWW-Authenticate: Basic`.

### 4.3 OBO token claims

| Claim | Value |
|---|---|
| `sub`, `caas_user_id` | the asserted user |
| `act` | `{"sub": "<client id>"}`, one level, never nested |
| `caas_org_id` | the client's tenant |
| `scopes` | the client's roles (assertion roles ignored) |
| `iss`, `aud` (if configured), `jti` | as for `client_credentials` |
| `iat` | now |
| `exp` | min(assertion `exp`, now + `CYODA_JWT_EXPIRY_SECONDS`) |

### 4.4 Rate limit

Per client, per node, token bucket over both grants:
`CYODA_IAM_TOKEN_REQUESTS_PER_MINUTE` (default 120, `0` = unlimited). Over the
limit: `429` with `Retry-After`. It bounds the bcrypt cost one client can
impose on a node.

### 4.5 Lifetime

`CYODA_JWT_EXPIRY_SECONDS`: default 300, range 1–3600 (was default 3600,
maximum 366 days). The Helm chart default and schema follow (chart version
bump, `COMPATIBILITY.md`). `cyoda token --ttl` is capped by the same value.

## 5. Principal and attribution

### 5.1 SPI change

`spi.UserContext` gains `Executor *Principal`. It is set only for an OBO
token; for every other token it is nil.

`spi.AttributionFor(ctx)`:

- `Executor` set: attributed = `{UserID, Kind}`, executor = `*Executor`.
  Transaction-origin inheritance never applies.
- `Executor` nil: unchanged (service and system executors inside a transaction
  inherit `tx.Origin`).

`spi.ResolveOrigin` is unchanged: an OBO request's origin is its user.

This is an SPI change consumed by every backend (memory, sqlite, postgres,
cassandra): a cyoda-go-spi PR into main, cyoda-go pseudo-pins it, the
cassandra plugin bumps its pin, `COMPATIBILITY.md` records it.

### 5.2 Validator mapping (`internal/auth/validator.go`)

| Token | `Kind` | `UserID` | `Roles` | `Executor` |
|---|---|---|---|---|
| has `act` | user | `caas_user_id` | `scopes` | `{act.sub, service}` |
| no `act`, has `scopes` | service | `caas_user_id` | `scopes` | nil |
| neither (`cyoda token`) | user | `caas_user_id` | `user_roles` | nil |

`act.sub` must pass the client-id grammar; otherwise the token is refused. A
token with both `act` and `user_roles` is refused.

### 5.3 Reserved user ids

`ValidateUserID` refuses `system` (case-insensitive). The `oidc:` prefix
reservation and the split between `ValidateFirstPartyUserID` and
`ValidateUserID` are removed: one rule for every user id.

### 5.4 Guards

- `RequireAdmin` and the operator guard refuse any principal with an
  `Executor`.
- The compute-stream guard requires kind service, `ROLE_M2M`, and no
  `Executor`. An OBO token gets `PermissionDenied`.
- Mock mode: `CYODA_IAM_MOCK_KIND` defaults to `service`, so compute nodes join
  in the getting-started mode.

## 6. Trusted keys

### 6.1 Storage and lookup

- Key ids are unique per tenant; the store key is `(tenant, kid)`. The
  cross-tenant `409 KEY_OWNED_BY_DIFFERENT_TENANT` and its error document are
  removed.
- The exchange reads the key from the shared store on every exchange. The
  trusted-key node copy (replica), its broadcast topic and its reconcile
  metrics are removed. `List` reads the store.
- The invalidation grace period is removed: invalidating a key ends it at once.
  Rotation: register the new key, switch the application, invalidate the old
  key.
- `POST /oauth/keys/trusted` and the key operations stay tenant-admin and stay
  behind `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED`; the per-tenant cap stays.
- Register, invalidate, reactivate and delete write INFO lines with tenant,
  kid, attributed user and executor.
- Trusted keys are never accepted as bearer tokens.

## 7. Where identity is recorded

### 7.1 Entity history

`EntityVersionMeta.User`/`AttributedKind` and `Executor` come from
`AttributionFor` (§5.1). `GET /entity/{id}/changes` already returns both.

### 7.2 Audit events

- EntityChange events gain `executedBy {id, kind}` beside `actor` (the
  attributed user). OpenAPI audit schemas change accordingly.
- `spi.StateMachineEvent` gains `Attributed Principal` and
  `Executor Principal`, stamped by the engine from `AttributionFor` at record
  time, persisted by every backend (memory, sqlite, postgres schema, cassandra
  schema), and rendered as `actor` and `executedBy` in StateMachine audit
  events.

### 7.3 Callouts to compute nodes

Every callout carries the attributed user and the executor, from
`AttributionFor` on the callout's context:

| Attribute | Value |
|---|---|
| `authtype` | attributed user's kind |
| `authid` | attributed user's id |
| `authclaims` | roles of the request's caller (`UserContext.Roles`) |
| `authexecid` (new) | executor's id |
| `authexectype` (new) | executor's kind |

Per path:

| Path | `authid` / `authtype` | `authexecid` / `authexectype` |
|---|---|---|
| OBO request, and cascades in the same request | alice / user | OBO client / service |
| Client's own request | client / service | client / service |
| Processor write-back (joined), and its cascades | transaction origin | compute client / service |
| CBD-detached callback by a compute client | that client / service | same |
| Scheduled fire | `ArmedBy` | `system` / system |
| Callout forwarded to another node | as on the forwarding node | as on the forwarding node |

`cluster/dispatch` carries the executor on the wire (`DispatchCalloutRequest`,
`buildContext`, every hand-over). `api/grpc/authctx` gains an executor reader.
`authclaims` documentation changes to "the roles of the client that made the
request".

### 7.4 Scheduled tasks

`spi.ScheduledTask` gains `ArmedVia Principal` (the executor that armed it).
A fire stamps `ChangeUser = ArmedBy`, `ChangeExecutor = system`; its callouts
carry `authid = ArmedBy` and executor `system`.

### 7.5 Messages

The `X-User-ID` request header on `POST /message` is removed. The stored
message header's user is the attributed user of the request, and the executor
is stored beside it.

### 7.6 Async search jobs

A job keeps the submitting request's `UserContext`, executor included.

## 8. Compute streams

- Guard: §5.4.
- Every 60 s (constant `streamClientRecheckInterval`), the stream reads its
  client from the shared store by client id (new `M2MClientStore.Lookup`,
  no secret). It closes when the client is absent, its tenant differs, or
  `client.UpdatedAt` is later than the token's `iat`. A store failure closes
  the stream (fail closed).
- The client id is taken from the token's `sub`, which equals the client id for
  client tokens; the stream refuses a token whose `sub` is not a client id.
- Mock mode has no client store; the re-check is off in mock mode only.

## 9. Removals

No code path that contradicts this design remains. Exit checks are greppable.

| Area | Removed |
|---|---|
| OIDC subsystem | `internal/auth/oidc/` (whole package), `internal/domain/account/oidc_adapter.go`, OIDC wiring in `app/app.go` |
| Validator chain | `ChainedValidator`, the `ErrUnknownKID` fall-through, `ErrKIDCannotVerify` (one validator remains); `http_jwks_source.go`, `NewJWKSValidator` |
| Configuration | `CYODA_OIDC_REQUIRE_HTTPS`, `CYODA_OIDC_CONNECT_TIMEOUT_MS`, `CYODA_OIDC_SOCKET_TIMEOUT_MS`, `CYODA_OIDC_CONNECTION_REQUEST_TIMEOUT_MS`, `CYODA_OIDC_ALLOW_PRIVATE_NETWORKS`, `CYODA_OIDC_ROLES_CLAIM`, and their `DefaultConfig()` fields |
| Error codes and docs | `OIDC_PROVIDER_DUPLICATE`, `OIDC_PROVIDER_INACTIVE`, `OIDC_PROVIDER_NOT_FOUND`, `OIDC_INVALID_TENANT`, `OIDC_SSRF_BLOCKED`, `KEY_OWNED_BY_DIFFERENT_TENANT` |
| OpenAPI | `/oauth/oidc/providers*`; `subject_token_type` value `…:access_token`; `X-User-ID` on messages |
| Trusted keys | node copy, broadcast topic, reconcile metrics, invalidation grace period |
| Key pairs | the `human` audience: key pairs have one purpose, signing cyoda tokens; `audience` leaves the key-pair API and `CYODA_JWT_BOOTSTRAP_AUDIENCE` is removed |
| User ids | `OIDCUserIDPrefix`, `ValidateFirstPartyUserID` |
| Tests | OIDC unit tests, `internal/e2e/oidc_*`, `e2e/parity/oidc.go`, `oidc_fixture.go`, `oidc_cyoda_kid.go`, their registry entries |
| Docs | `help/content/auth/oidc.md` and every OIDC passage (auth, tokens, clients, config/auth, admin, errors, telemetry, openapi, cli/token, README); ARCHITECTURE §7.2–7.3 rewritten; ADR 0002 marked superseded by a new ADR 0004 |

Exit checks: `grep -rn "oidc" --include=*.go internal app cmd` returns only
history-free references that the plan lists explicitly; `grep -rn
"KEY_OWNED_BY_DIFFERENT_TENANT\|CYODA_OIDC_\|X-User-ID\|BOOTSTRAP_AUDIENCE"`
returns nothing outside `CHANGELOG.md` and `docs/superpowers/`.

## 10. Configuration

| Variable | Default | Range | Change |
|---|---|---|---|
| `CYODA_JWT_EXPIRY_SECONDS` | 300 | 1–3600 | default and maximum lowered |
| `CYODA_IAM_TOKEN_REQUESTS_PER_MINUTE` | 120 | ≥ 0 (0 = unlimited) | new |
| `CYODA_IAM_MOCK_KIND` | `service` | user, service, system | default changed |
| `CYODA_JWT_BOOTSTRAP_AUDIENCE` | — | — | removed |
| `CYODA_OIDC_*` (six) | — | — | removed |

Help topics, `README.md` and `DefaultConfig()` change together (Gate 4).

## 11. Error and status tables

### 11.1 `POST /oauth/token`

| Cause | Status | `error` |
|---|---|---|
| no or bad client authentication | 401 | `invalid_client` |
| unsupported `grant_type` | 400 | `unsupported_grant_type` |
| `client_credentials` by an OBO client | 400 | `unauthorized_client` |
| exchange by a non-OBO client | 400 | `unauthorized_client` |
| rate limit | 429 | `slow_down` |
| forbidden RFC 8693 parameter | 400 | `invalid_request` |
| bad `subject_token_type` | 400 | `invalid_request` |
| malformed assertion, wrong `alg`, no `kid` | 400 | `invalid_request` |
| unknown, invalidated or not-yet-valid key | 400 | `invalid_request` |
| bad signature, issuer not listed | 400 | `invalid_request` |
| `aud`, `exp`, `iat` missing or out of bounds | 400 | `invalid_request` |
| `caas_org_id` ≠ client's tenant | 403 | `access_denied` |
| `sub` invalid or reserved | 400 | `invalid_request` |
| capped `exp` not in the future | 400 | `invalid_request` |
| store unavailable | 503 | `temporarily_unavailable` |
| signing failure | 500 | `server_error` (ticketed) |

### 11.2 `POST /clients`

| Cause | Status | Code |
|---|---|---|
| success | 200 | — |
| not a tenant admin | 403 | `FORBIDDEN` |
| `withAdminRole=true` and `onBehalfOf=true` | 400 | `BAD_REQUEST` |
| `onBehalfOf=true` in tenant `PLATFORM` | 400 | `BAD_REQUEST` |
| `withAdminRole=true` while the flag is off | 404 | `FEATURE_DISABLED` |
| cap reached | 400 | `M2M_CLIENT_CAP_REACHED` |

### 11.3 Trusted keys

Unchanged statuses, except: the 409 for another tenant's kid is removed; a
duplicate kid in the same tenant is `409 CONFLICT`; the invalidate request's
grace-period field is removed.

### 11.4 Data endpoints

A caller without `ROLE_M2M`: `403 FORBIDDEN` (HTTP), `PermissionDenied`
(gRPC). An OBO principal on an admin or operator endpoint: `403 FORBIDDEN`.

### 11.5 Compute stream

| Cause | gRPC status |
|---|---|
| no or invalid token | `Unauthenticated` |
| not kind service, no `ROLE_M2M`, or an OBO token | `PermissionDenied` |
| `sub` not a client id | `PermissionDenied` |
| re-check: client gone, tenant differs, token older than reset, store error | stream closed with `Unauthenticated` |

## 12. Test coverage matrix

| Scenario | Unit | E2E (postgres) | Parity | gRPC |
|---|---|---|---|---|
| client_credentials happy path; OBO client refused | ✓ | ✓ | ✓ | — |
| exchange happy path: claims §4.3 | ✓ | ✓ | ✓ | — |
| every §11.1 row | ✓ | ✓ | — | — |
| assertion roles ignored (`ROLE_ADMIN` in assertion) | ✓ | ✓ | ✓ | — |
| validator mapping §5.2, each row | ✓ | — | — | — |
| attribution: OBO write, entity history user+executor | ✓ | ✓ | ✓ | — |
| attribution: OBO write joined into another principal's transaction keeps alice | ✓ | ✓ | ✓ | — |
| audit EntityChange and StateMachine events carry actor+executedBy | ✓ | ✓ | ✓ | — |
| callout attributes per §7.3 row | ✓ | ✓ | — | ✓ |
| forwarded callout carries executor (multi-node) | ✓ | ✓ (multi-node) | — | ✓ |
| scheduled fire: ChangeUser=ArmedBy, executor system, callout attributes | ✓ | ✓ | ✓ | ✓ |
| message user from token; `X-User-ID` gone | ✓ | ✓ | — | — |
| data endpoint without `ROLE_M2M` → 403 | ✓ | ✓ | — | ✓ |
| OBO principal on admin/operator endpoint → 403 | ✓ | ✓ | — | — |
| POST /clients rules §11.2 | ✓ | ✓ | — | — |
| trusted key per-tenant kid; no cross-tenant 409 | ✓ | ✓ | ✓ | — |
| trusted key invalidation ends exchanges at once on every node | ✓ | ✓ (multi-node) | — | — |
| store failure on exchange → 503 | ✓ | — | — | — |
| rate limit → 429 | ✓ | ✓ | — | — |
| stream guard §11.5 | ✓ | — | — | ✓ |
| stream re-check: reset after token iat closes; delete closes; store error closes | ✓ | ✓ | — | ✓ |
| lifetime default 300, max 3600, `expires_in` computed | ✓ | ✓ | — | — |
| reserved user id `system` | ✓ | ✓ | — | — |

Concurrency checks (simultaneous exchange and key invalidation) are isolated
single-backend e2e tests, not parity.

## 13. Cloud parity (Gate 7)

New `docs/cloud-parity/obo-only-user-identity.md`. Cloud actions:

- OBO token roles = the OBO client's roles; no user lookup; `act` one level.
- OBO permission on clients; OBO clients cannot use `client_credentials`.
- Trusted keys are never bearer tokens; key ids per tenant; no grace period.
- OIDC provider endpoints retired.
- Callout attributes `authexecid`/`authexectype`; `authclaims` = client roles.
- Audit events carry `actor` and `executedBy`, StateMachine events included.
- Token lifetime default 300 s, maximum 3600 s.

Update `trusted-key-tenant.md`, `authcontext-attribution.md` and
`user-id-rule.md`. File the CaaS ticket and cite it.

## 14. Security fix carried by this change

Today the exchange copies `user_roles` from the assertion into the issued
token. Anyone holding a tenant's trusted private key and any client of that
tenant can mint `ROLE_ADMIN` tokens for that tenant, and platform-operator
tokens in `PLATFORM`. Under §4.3 the assertion's roles are ignored and §3.1
keeps OBO clients out of `PLATFORM` and away from `ROLE_ADMIN`.

## 15. Applications

`ctcc-management` must:

- create an OBO client and a trusted key pair;
- sign assertions with its users' ids and exchange them, caching one OBO token
  per user;
- stop forwarding Zitadel tokens; delete its OIDC registration scripts;
- read `authexecid`/`authexectype` to tell scheduled work from a user acting
  now, and keep its segregation-of-duties check on `authid`;
- connect its compute node over TLS.

## 16. Documentation

- `docs/access-to-the-cyoda-api.html`: the companion page, kept in step.
- Help topics: `auth`, `auth/tokens`, `auth/clients`, `auth/trusted-keys`,
  `config/auth`, `cli/token`, `errors/*`, `telemetry`, `quickstart`, `admin`.
- `README.md`, `docs/ARCHITECTURE.md` §7, ADR 0004 (superseding 0002),
  `CHANGELOG.md` (Breaking), `COMPATIBILITY.md` (SPI pin, chart).
