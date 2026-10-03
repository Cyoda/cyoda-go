# M2M-only access with on-behalf-of user identity

Companion page: `docs/access-to-the-cyoda-api.html` (swimlane scenarios and
reference). This spec is the implementation contract; where the two differ,
this spec wins and the page is corrected.

## 1. Requirement and boundary

- No regular user accesses cyoda-go directly. cyoda-go has no per-user data
  permissions.
- Only M2M clients authenticate to cyoda-go.
- Work an M2M client does on behalf of an application user carries that user's
  identity into the entity history, the audit events and the gRPC callouts.
- The application decides whether a user may perform an action, before it acts.
- cyoda-go records what an authenticated client states about the user; it does
  not verify the user. Like a database, cyoda-go does not protect data from a
  compromised application. That responsibility lies with the application.

The one exception to "only M2M clients" is the platform operator's offline
token (`cyoda token`), signed with `CYODA_JWT_SIGNING_KEY`: a user-kind
principal with no client and no separate executor (§5).

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
| Scheduled transition armed for alice fires | alice | `system` |
| Platform operator token | the operator user | the operator user |

## 3. Clients

### 3.1 Permissions

A client record (`auth.M2MClient`) gains:

- `OnBehalfOf bool`: set at creation, immutable. A record flag, not a role, so
  it never appears in `authclaims`.
- `SecretGen uint64`: 1 at creation, incremented by every successful secret
  reset (§8).

| Client | Roles | `OnBehalfOf` | May use |
|---|---|---|---|
| plain | `ROLE_M2M` | false | `client_credentials` |
| admin | `ROLE_M2M`, `ROLE_ADMIN` | false | `client_credentials` |
| OBO | `ROLE_M2M` | true | token exchange only |

Rules, enforced by cyoda-go:

- An OBO client never holds `ROLE_ADMIN`.
- An OBO client never exists in tenant `PLATFORM`.
- `client_credentials` with an OBO client is refused.
- A token exchange by a non-OBO client is refused, before the subject token is
  parsed.

### 3.2 `POST /clients`

New query parameter `onBehalfOf` (boolean, default false). Precedence of the
checks: tenant admin (403) → `withAdminRole=true` while the admin-role flag is
off (404 `FEATURE_DISABLED`) → `withAdminRole=true` with `onBehalfOf=true`
(400) → `onBehalfOf=true` in `PLATFORM` (400) → cap (400).

`TechnicalUserDto`, the list response and `TechnicalUserCredentialsDto` gain
`onBehalfOf`. `TechnicalUserCredentialsDto.grant_type` is
`urn:ietf:params:oauth:grant-type:token-exchange` for an OBO client and
`client_credentials` otherwise.

### 3.3 Every route requires `ROLE_M2M`, except an allow-list

Every authenticated HTTP route (generated router and hand-registered mux
routes) and every unary gRPC method requires `ROLE_M2M` in the caller's roles;
otherwise `403 FORBIDDEN` / `PermissionDenied`. The allow-list:

- routes guarded by `RequireAdmin` (clients, trusted keys);
- routes guarded by the operator guard (key pairs, `/admin/*`);
- `GET /account`.

A test enumerates every OpenAPI operation, every hand-registered mux route and
every gRPC method, and asserts each is either `ROLE_M2M`-guarded or on the
allow-list, so no new route can escape the rule. The operator token carries the
roles it was signed with; `cyoda token --roles` decides whether it can reach
data. Mock mode's principal carries `ROLE_M2M`.

## 4. Token endpoint

### 4.1 Client authentication cost

- **Verified-secret cache.** Per node, keyed by client id, holding the client
  record's `HashedSecret` and the SHA-256 of the secret that matched it. A
  request whose client record still has the same `HashedSecret` and whose
  presented secret has the cached SHA-256 (constant-time compare) is
  authenticated without bcrypt. The client record is read from the store on
  every request, so a reset or delete takes effect at once.
- **bcrypt bound.** At most `CYODA_IAM_TOKEN_MAX_CONCURRENT_SECRET_CHECKS`
  bcrypt comparisons run at once per node (default: the CPUs the process may
  use, `GOMAXPROCS`, which follows a container CPU limit). A
  request that cannot get a slot within 1 s answers `503` with `Retry-After`.
  Unknown client ids and wrong secrets keep paying one bcrypt, so a lookup
  costs the same either way. Hashing a new secret (`POST /clients`, secret
  reset) takes a slot from the same bound; when none is free within 1 s it
  answers `503 SERVER_BUSY` with `Retry-After` and writes nothing.
- **Per-client fairness.** After authentication, a token bucket per client per
  node: `CYODA_IAM_TOKEN_REQUESTS_PER_MINUTE` (default 600, `0` = unlimited),
  shared by both grants. Over the limit: `429` with `Retry-After`. At 600 per
  minute and 300 s tokens, one client sustains about 3000 concurrently active
  users per node.

### 4.2 `client_credentials`

Processing: method must be POST (`405`) → authenticate (§4.1) → refuse an OBO
client (`400 unauthorized_client`) → per-client bucket → mint.

Claims: `sub` = `caas_user_id` = the client's user id (its client id),
`caas_org_id` = the client's tenant, `scopes` = the client's roles, `cgen` =
the client's `SecretGen`, `caas_tier`, `iss`, `aud` if configured, `iat`,
`exp`, `jti`. `expires_in` is the token's remaining life.

### 4.3 Token exchange (OBO)

Request: `grant_type=urn:ietf:params:oauth:grant-type:token-exchange`, client
authentication by HTTP Basic, `subject_token` = the user assertion,
`subject_token_type=urn:ietf:params:oauth:token-type:jwt`.

Processing, in this order:

1. Method must be POST: else `405`.
2. Authenticate the client (§4.1).
3. The client must be an OBO client: else `400 unauthorized_client`.
4. Per-client bucket: `429`.
5. Any of `actor_token`, `actor_token_type`, `resource`, `audience`, `scope`,
   `requested_token_type` present: `400 invalid_request`.
6. `subject_token_type` other than `…:jwt`: `400 invalid_request`.
7. Parse the assertion; `alg` must be RS256 and `kid` present: else
   `400 invalid_request`.
8. Read the trusted key `(client's tenant, kid)` from the shared store (§6).
   Not found, invalidated, or before `validFrom`: `400 invalid_request`.
9. Verify the signature: else `400 invalid_request`.
10. If the key lists issuers, `iss` must be one: else `400 invalid_request`.
11. `aud` must contain `CYODA_JWT_ISSUER`; `exp` and `iat` required; `exp − iat`
    at most 300 s; not expired; `iat` not in the future; `nbf` honoured (30 s
    skew throughout): else `400 invalid_request`.
12. `caas_org_id` must equal the client's tenant: else `403 access_denied`.
13. `sub` must pass `ValidateUserID` (§5.3): else `400 invalid_request`.
14. Mint the OBO token (§4.4). If its `exp` would not be in the future:
    `400 invalid_request`.

Store errors at steps 2 and 8: an error marked storage-unavailable answers
`503 temporarily_unavailable` with `Retry-After`; any other store error answers
`500 server_error` with a ticket. Error bodies are OAuth-shaped (`error`,
`error_description`); descriptions never echo the subject, the tenant or the
key id. A `401` carries `WWW-Authenticate: Basic`. `slow_down` and
`temporarily_unavailable` are used on the token endpoint deliberately, with
the meaning of RFC 8628 and RFC 6749 §4.1.2.1 respectively.

### 4.4 OBO token claims

| Claim | Value |
|---|---|
| `sub`, `caas_user_id` | the asserted user |
| `act` | `{"sub": "<client id>"}`, one level, never nested |
| `caas_org_id` | the client's tenant |
| `scopes` | the client's roles (the assertion's roles are ignored) |
| `caas_tier`, `iss`, `aud` (if configured), `jti` | as for `client_credentials` |
| `iat` | now |
| `exp` | min(assertion `exp`, now + `CYODA_JWT_EXPIRY_SECONDS`) |

No `cgen`: an OBO token cannot open a stream. `expires_in` is the token's
remaining life.

### 4.5 Lifetime

`CYODA_JWT_EXPIRY_SECONDS`: default 300, range 1–3600. The Helm chart default
and schema follow (chart version bump, `COMPATIBILITY.md`). `cyoda token --ttl`
is capped by the same value.

## 5. Principal and attribution

### 5.1 SPI change

`spi.UserContext` gains `Executor *Principal`, set only for an OBO token.

`spi.AttributionFor(ctx)`:

- `Executor` set: attributed = `{UserID, Kind}`, executor = `*Executor`.
  Transaction-origin inheritance never applies.
- `Executor` nil: as before (a service or system executor inside a transaction
  attributes to `tx.Origin`).

`spi.ResolveOrigin` is unchanged. Scheduled-task arming uses `AttributionFor`
(§7.4), not `ResolveOrigin`.

SPI release: a cyoda-go-spi PR into main; cyoda-go pseudo-pins it; the
cassandra plugin bumps its pin; `COMPATIBILITY.md` records it.

### 5.2 Validator mapping (`internal/auth/validator.go`)

| Token | `Kind` | `UserID` | `Roles` | `Executor` |
|---|---|---|---|---|
| has `act` | user | `caas_user_id` | `scopes` | `{act.sub, service}` |
| no `act`, has `scopes` | service | `caas_user_id` | `scopes` | nil |
| neither (`cyoda token`) | user | `caas_user_id` | `user_roles` | nil |

Refused, so that the mapping is total: `act` without a non-empty `sub`; `act`
without `scopes`; `act` with `user_roles`; `scopes` with `user_roles`.

The auth layer also puts a client-token marker in the request context, holding
the client id (`caas_user_id`) and `cgen`, when the token has `scopes`, no
`act`, and a `cgen` claim. Streams read it (§8).

### 5.3 User ids

One rule, `ValidateUserID`, for every user id: the existing grammar, plus the
reserved id `system` (case-insensitive). `ValidateFirstPartyUserID` and the
`oidc:` prefix reservation do not exist.

### 5.4 Guards

- `RequireAdmin` and the operator guard refuse any principal with an
  `Executor` (`403 FORBIDDEN`).
- The compute-stream guard requires kind service, `ROLE_M2M`, no `Executor`,
  and the client-token marker: else `PermissionDenied`.
- Mock mode: `CYODA_IAM_MOCK_KIND` defaults to `service`; the mock context
  carries a client-token marker with no store check (§8).

### 5.5 Joining a transaction

An OBO request may join only a transaction whose origin is its own user
(`{UserID, user}`); otherwise `403 FORBIDDEN` (HTTP) / the RPC's error
envelope (gRPC: code `CLIENT_ERROR`, message `FORBIDDEN: …`, as for every
operational error). A compute client's write-back join is unchanged and attributes to the
transaction's origin.

## 6. Trusted keys

- Storage: one KV namespace per tenant (as M2M clients), key = kid. Key ids are
  unique within a tenant only; `List` and the per-tenant cap read one tenant's
  namespace.
- Register stays an upsert on `(tenant, kid)`.
- The exchange reads the key from the store on every exchange; `List` reads the
  store and can fail (`503` / `500`). The trusted-key node copy, its broadcast
  topic and its reconcile metrics do not exist.
- Invalidating a key ends it at once. The invalidate request has no body;
  `invalidatePrevious` on register invalidates the previous key at once; no
  grace period exists for trusted keys. Key pairs keep their own grace period,
  so the invalidate request schema is split.
- Trusted keys have no `audience`.
- The endpoints stay tenant-admin and behind
  `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED`; the per-tenant cap stays.
- Register, invalidate, reactivate and delete write INFO lines with tenant,
  kid, attributed user and executor.
- Trusted keys are never accepted as bearer tokens.

## 7. Where identity is recorded

### 7.1 Entity history

`EntityVersionMeta.User`, `AttributedKind` and `Executor` come from
`AttributionFor`. `GET /entity/{id}/changes` returns them.

### 7.2 Audit events

- EntityChange events: `actor {id, name, kind, legalId}` (the attributed user,
  `kind` added) and `executedBy {id, kind}`.
- `spi.StateMachineEvent` gains `Attributed Principal` and `Executor Principal`,
  stamped by the engine from `AttributionFor` at record time and rendered as
  `actor` and `executedBy` in StateMachine audit events. The events are stored
  as JSON documents in every backend; no schema migration.

### 7.3 Callouts to compute nodes

The node that dispatches a callout computes
`(attributed, executor) := AttributionFor(ctx)` once and attaches:

| Attribute | Value |
|---|---|
| `authtype` | attributed kind |
| `authid` | attributed id |
| `authexectype` (new) | executor kind |
| `authexecid` (new) | executor id |
| `authclaims` | the executor's roles (`UserContext.Roles` of the request's caller) |

Per path:

| Path | `authid` / `authtype` | `authexecid` / `authexectype` |
|---|---|---|
| OBO request, and cascades in the same request | alice / user | OBO client / service |
| Client's own request | client / service | client / service |
| Processor write-back (joined), and its cascades | transaction origin | compute client / service |
| CBD-detached callback by a compute client | that client / service | same |
| Scheduled fire | `ArmedBy` | `system` / system |
| Callout forwarded to another node | as computed on the dispatching node | as computed on the dispatching node |

Forwarding: `DispatchCalloutRequest` carries `AttributedID`, `AttributedKind`,
`ExecutorID`, `ExecutorKind` and the roles. The peer attaches them as received
and never recomputes them; local and peer dispatch read the same context value.

`api/grpc/authctx`: `Require(role)` gates on `authexectype ∈ {service}` and the
role in `authclaims`; new readers return the attributed and executor
principals.

### 7.4 Scheduled tasks

Arming stamps `ArmedBy` = the attributed principal from `AttributionFor`. A
fire stamps `ChangeUser = ArmedBy`, `ChangeExecutor = system`; its callouts
carry `authid = ArmedBy` and executor `system`.

### 7.5 Messages

`POST /message` has no `X-User-ID` header. The stored message header records
the attributed user and the executor of the request; the message GET response
returns both.

### 7.6 Async search jobs

A job keeps the submitting request's `UserContext`, executor included.

## 8. Compute streams

- Guard: §5.4.
- Once when the stream opens, before the member is registered, and then
  every 60 s (constant), the stream reads its client from the store by the
  marker's client id (new `M2MClientStore.Lookup(clientID)`: record without
  secret check). It refuses or closes the stream with `Unauthenticated` when
  the client is absent, its tenant differs from the stream's, or its
  `SecretGen` differs from the marker's `cgen`. A store error refuses or
  closes the stream with `Unavailable`. A refused stream registers no member
  and logs no join.
- Each read is bounded by the 60 s interval; a read that does not answer in
  time is a store error (`Unavailable`).
- Mock mode has no client store; the re-check does not run.

## 9. Removals

| Area | Removed |
|---|---|
| OIDC subsystem | `internal/auth/oidc/`; `internal/domain/account/oidc_adapter.go`; OIDC wiring in `app/app.go`; OIDC methods in `internal/api/server.go` and `internal/api/unimplemented.go` |
| Validator chain | `ChainedValidator`, the `ErrUnknownKID` fall-through, `ErrKIDCannotVerify`; `http_jwks_source.go`, `NewJWKSValidator` |
| Configuration | `CYODA_OIDC_REQUIRE_HTTPS`, `CYODA_OIDC_CONNECT_TIMEOUT_MS`, `CYODA_OIDC_SOCKET_TIMEOUT_MS`, `CYODA_OIDC_CONNECTION_REQUEST_TIMEOUT_MS`, `CYODA_OIDC_ALLOW_PRIVATE_NETWORKS`, `CYODA_OIDC_ROLES_CLAIM`, `CYODA_JWT_BOOTSTRAP_AUDIENCE`, their `DefaultConfig()` fields and `cmd/cyoda/help/config_registry.go` entries |
| Error codes and docs | `OIDC_PROVIDER_DUPLICATE`, `OIDC_PROVIDER_INACTIVE`, `OIDC_PROVIDER_NOT_FOUND`, `OIDC_INVALID_TENANT`, `OIDC_SSRF_BLOCKED`, `KEY_OWNED_BY_DIFFERENT_TENANT` |
| Trusted keys | node copy (replica use), broadcast topic, `TrustedKeyMetrics` and its wiring, grace period, `audience`, global namespace |
| Key pairs | the `audience` concept: `isValidKeyPairAudience`, audience in signing records, `KeyStore.Signer(audience)` → `Signer()`, `?audience=` on `/current`, audience in issue and list |
| User ids | `OIDCUserIDPrefix`, `ValidateFirstPartyUserID` |
| Messages | `X-User-ID` |
| Tests | OIDC unit tests; `internal/e2e/oidc_*`; `internal/e2e/keys_trusted_reconciliation_test.go`; `internal/auth/kv_trusted_store_reconcile_test.go`; `e2e/parity/oidc.go`, `oidc_fixture.go`, `oidc_cyoda_kid.go`, `client/oidc.go`, their registry entries and fixture hooks |
| Docs | `help/content/auth/oidc.md`; ADR 0002 marked superseded by a new ADR 0004 |

Staying: `kvReplica`, `coalescingRunner` and `ReconcileMetrics` remain for
signing keys.

Also updated: `docs/ARCHITECTURE.md`, `docs/CONCURRENCY.md`,
`docs/FEATURES.md`, `.claude/rules/race-testing.md`. Deleted:
`docs/proposals/cyoda-go-oidc-zitadel-roles-claim.md`.

Exit checks, run from the repo root. The excluded paths are history
(`docs/superpowers`, `docs/audits`, `docs/PRD.md`, `docs/release-notes`,
`docs/analysis`, `docs/adr`, `docs/cloud-parity`, `CHANGELOG.md`,
`.github/oasdiff-err-ignore.txt`), Cloud's file (`docs/cyoda`), the external
API vocabulary (`e2e/externalapi`), or a different OIDC (cosign keyless
signing in `.github/workflows`, `.goreleaser.yaml`, `scripts/install.sh`; the
gateway's own OIDC in `deploy/helm/cyoda/docs/gateway-api-policies.md`),
or the test that proves the removed provider routes stay removed
(`app/removed_routes_test.go`):

```
X=(':!docs/superpowers' ':!docs/audits' ':!docs/PRD.md' ':!docs/release-notes'
   ':!docs/analysis' ':!docs/adr' ':!docs/cloud-parity' ':!CHANGELOG.md'
   ':!.github' ':!docs/cyoda' ':!e2e/externalapi' ':!.goreleaser.yaml'
   ':!scripts/install.sh' ':!deploy/helm/cyoda/docs/gateway-api-policies.md'
   ':!app/removed_routes_test.go')
git grep -niI 'oidc' -- . "${X[@]}"
git grep -nI -e KEY_OWNED_BY_DIFFERENT_TENANT -e CYODA_OIDC_ -e X-User-ID \
  -e BOOTSTRAP_AUDIENCE -e ValidateFirstPartyUserID -e ChainedValidator \
  -e TrustedKeyMetrics -- . "${X[@]}"
```

Both return nothing.

## 10. Configuration

| Variable | Default | Range |
|---|---|---|
| `CYODA_JWT_EXPIRY_SECONDS` | 300 | 1–3600 |
| `CYODA_IAM_TOKEN_REQUESTS_PER_MINUTE` | 600 | ≥ 0 (0 = unlimited) |
| `CYODA_IAM_TOKEN_MAX_CONCURRENT_SECRET_CHECKS` | `GOMAXPROCS` | ≥ 1 |
| `CYODA_IAM_MOCK_KIND` | `service` | user, service, system |

Help topics, `README.md`, `DefaultConfig()` and the config registry change
together.

## 11. OpenAPI changes

| Path / schema | Change |
|---|---|
| `/oauth/oidc/providers*` | removed |
| `POST /oauth/token` | 400 descriptions: `invalid_request`, `unauthorized_client`; add `403 access_denied`, `405`, `429`, `503` with `Retry-After`; `subject_token_type` enum is `…:jwt` only |
| `POST /clients` | `onBehalfOf` query parameter |
| `TechnicalUserDto`, list item, `TechnicalUserCredentialsDto` | `onBehalfOf`; `grant_type` per §3.2 |
| `/oauth/keys/keypair*` | `audience` removed from issue, list and `/current` |
| `/oauth/keys/trusted*` | `audience` removed; invalidate has no body; register's grace field removed; list adds `500`, `503` |
| invalidate schemas | split: key pairs keep the grace field |
| audit EntityChange | `actor.kind`, `executedBy` |
| audit StateMachine | `actor`, `executedBy` |
| `POST /message` | `X-User-ID` removed |
| message GET | attributed user and executor |
| every route outside the allow-list | `403 FORBIDDEN` without `ROLE_M2M` |
| transaction join | `403 FORBIDDEN` for an OBO request joining another user's transaction |

`go generate ./api` after the schema edits; oasdiff reports the breaking
changes, which `CHANGELOG.md` lists under Breaking.

## 12. Error and status tables

### 12.1 `POST /oauth/token`

| Cause | Status | `error` |
|---|---|---|
| method not POST | 405 | `method_not_allowed` |
| no or bad client authentication | 401 | `invalid_client` |
| bcrypt slots full | 503 | `temporarily_unavailable` |
| unsupported `grant_type` | 400 | `unsupported_grant_type` |
| `client_credentials` by an OBO client | 400 | `unauthorized_client` |
| exchange by a non-OBO client | 400 | `unauthorized_client` |
| per-client bucket empty | 429 | `slow_down` |
| forbidden RFC 8693 parameter | 400 | `invalid_request` |
| bad `subject_token_type` | 400 | `invalid_request` |
| malformed assertion, wrong `alg`, no `kid` | 400 | `invalid_request` |
| unknown, invalidated or not-yet-valid key | 400 | `invalid_request` |
| bad signature, issuer not listed | 400 | `invalid_request` |
| `aud`, `exp`, `iat`, `nbf` missing or out of bounds | 400 | `invalid_request` |
| `caas_org_id` ≠ client's tenant | 403 | `access_denied` |
| `sub` invalid or reserved | 400 | `invalid_request` |
| capped `exp` not in the future | 400 | `invalid_request` |
| store unavailable (client or key read) | 503 | `temporarily_unavailable` |
| other store error | 500 | `server_error` (ticketed) |
| signing failure | 500 | `server_error` (ticketed) |

### 12.2 `POST /clients`

| Cause | Status | Code |
|---|---|---|
| success | 200 | — |
| unauthenticated | 401 | `UNAUTHORIZED` |
| not a tenant admin, or an OBO principal | 403 | `FORBIDDEN` |
| `withAdminRole=true` while the flag is off | 404 | `FEATURE_DISABLED` |
| `withAdminRole=true` and `onBehalfOf=true` | 400 | `BAD_REQUEST` |
| `onBehalfOf=true` in tenant `PLATFORM` | 400 | `BAD_REQUEST` |
| cap reached | 400 | `M2M_CLIENT_CAP_REACHED` |
| mock mode | 501 | `NOT_IMPLEMENTED` |
| bcrypt slots full | 503 | `SERVER_BUSY` (`Retry-After`) |
| store failure | 500 / 503 | ticketed |

### 12.3 Trusted keys

Statuses as in the current OpenAPI document, except: no cross-tenant 409; `GET` list adds `500` / `503`;
invalidate takes no body. Register adds these `400` causes:

| Cause | Status | Code |
|---|---|---|
| JWK carries a private member (`d`, `p`, `q`, `dp`, `dq`, `qi`, `oth`); the detail names it | 400 | `BAD_REQUEST` |
| RSA modulus under 2048 bits | 400 | `BAD_REQUEST` |
| `alg` or `use` present and not a string | 400 | `BAD_REQUEST` |

Register stores, and register and list return, only the public members
`kty`, `kid` (the `keyId`), `n`, `e`, and `alg` / `use` when given; every
other member of the request is dropped.

### 12.4 Other routes

| Cause | Status |
|---|---|
| no `ROLE_M2M`, route not on the allow-list | 403 `FORBIDDEN` / `PermissionDenied` |
| OBO principal on an admin or operator route | 403 `FORBIDDEN` |
| OBO request joining another user's transaction | 403 `FORBIDDEN` / envelope `CLIENT_ERROR`, message `FORBIDDEN: …` |

### 12.5 Compute stream

| Cause | gRPC status |
|---|---|
| no or invalid token | `Unauthenticated` |
| not kind service, no `ROLE_M2M`, an OBO token, or no client-token marker | `PermissionDenied` |
| check at open: client gone, tenant differs, generation changed | stream refused, `Unauthenticated` |
| check at open: store error or read past its deadline | stream refused, `Unavailable` |
| re-check: client gone, tenant differs, generation changed | stream closed, `Unauthenticated` |
| re-check: store error or read past its deadline | stream closed, `Unavailable` |

## 13. Test coverage matrix

| Scenario | Unit | E2E (postgres) | Parity | gRPC |
|---|---|---|---|---|
| §12.1, every row | ✓ | ✓ except waived | — | — |
| §12.2, every row | ✓ | ✓ except mock 501 (unit) | — | — |
| §12.3 changed rows | ✓ | ✓ | ✓ (per-tenant kid) | — |
| §12.4, every row | ✓ | ✓ | — | ✓ |
| §12.5, every row | ✓ | ✓ | — | ✓ |
| verified-secret cache: reset and delete take effect at once | ✓ | ✓ | — | — |
| exchange happy path, claims §4.4, assertion roles ignored | ✓ | ✓ | ✓ | — |
| validator mapping §5.2, every row and refusal | ✓ | — | — | — |
| route allow-list enumeration test | ✓ | — | — | — |
| OBO write: entity history user + executor | ✓ | ✓ | ✓ | ✓ |
| compute write-back in alice's transaction: alice + compute client | ✓ | ✓ | ✓ | ✓ |
| audit EntityChange and StateMachine events: actor + executedBy | ✓ | ✓ | ✓ | — |
| callout attributes, every §7.3 row | ✓ | ✓ | — | ✓ |
| forwarded callout (scheduled fire, write-back cascade) | ✓ | ✓ multi-node | — | ✓ |
| scheduled fire armed by an OBO request, directly and inside its own joined transaction | ✓ | ✓ | ✓ | ✓ |
| scheduled fire armed by a compute write-back in alice's transaction: `ArmedBy` alice | ✓ | ✓ | ✓ | — |
| message stores user + executor; `X-User-ID` gone | ✓ | ✓ | ✓ | — |
| trusted-key invalidation ends exchanges at once on every node | ✓ | ✓ multi-node | — | — |
| lifetime default 300, maximum 3600, `expires_in` computed | ✓ | ✓ | — | — |
| key-pair `audience` removed | ✓ | ✓ | ✓ | — |
| mock kind default `service`: compute node joins in mock mode | ✓ | — | — | — |

Waivers:

- `503` on store unavailability and `500` on other store and signing failures:
  unit only; a running backend cannot be made to fail these reads on demand.
- Mock-mode `501` on `POST /clients`: unit only; the e2e suite runs in JWT mode.
- Concurrency (exchange racing key invalidation, secret reset racing a cached
  grant): isolated single-backend e2e, not parity.
- §12.4, an OBO principal on an admin or operator route: the gRPC column
  does not apply. The admin, client, trusted-key, key-pair and `/admin`
  routes are HTTP-only; no gRPC entry point exists.
- Scheduled fire armed by an OBO request inside its own joined transaction:
  parity covers the direct case only; the compute test client has no OBO token
  support. Unit, e2e and gRPC cover the joined case.
- Cassandra: attribution persistence (`AttributedKind`, `Executor`, `ArmedBy`)
  is not recorded by the cassandra backend. The cassandra pin bump to this SPI
  waits for that support; until then the attribution parity rows run on
  memory, sqlite and postgres only.

## 14. Cloud parity (Gate 7)

New `docs/cloud-parity/obo-only-user-identity.md`. Cloud actions:

- Data access requires `ROLE_M2M`; human principals (`ROLE_USER`) do not
  reach data endpoints. OBO tokens carry the OBO client's roles, never the
  user's; no user lookup; `act` one level.
- OBO permission on clients; OBO clients cannot use `client_credentials`.
- Trusted keys: never bearer tokens; per-tenant ids; register is an upsert; no
  grace period; no `audience`.
- OIDC provider endpoints retired.
- Callouts: `authid`/`authtype` = attributed principal, new
  `authexecid`/`authexectype`, `authclaims` = executor's roles.
- Audit events carry `actor` (with `kind`) and `executedBy`, StateMachine
  events included.
- Token lifetime default 300 s, maximum 3600 s.
- Key pairs have no `audience`.

Update `trusted-key-tenant.md`, `authcontext-attribution.md`,
`user-id-rule.md`, `m2m-clients.md`, `platform-operator.md`,
`signing-key-pairs.md`, `audit-event-identity-and-order.md` and
`scheduled-transitions.md`. File the CaaS ticket and cite it.

## 15. Security properties

- The assertion's roles never reach the issued token, so a trusted private key
  and a plain client together cannot mint `ROLE_ADMIN`, and nothing can mint a
  platform-operator token through an exchange (§3.1, §4.4, §5.4).
- Tenant isolation: the tenant comes from the stored client on both grants;
  trusted keys are read in the client's tenant only; no response reveals
  another tenant's keys or clients.
- Revocation: a reset or delete stops new tokens at once on every node (store
  read per grant, generation check on streams); issued tokens end within the
  configured lifetime.
- Token-endpoint CPU is bounded per node; one client's load cannot starve
  another's beyond the bcrypt bound.

## 16. Applications

`ctcc-management` must:

- split its single client into an OBO client (BFF), a plain client per compute
  node, and an admin client;
- create a trusted key pair; sign assertions with its users' ids and exchange
  them, caching one OBO token per user;
- stop forwarding Zitadel tokens; delete its OIDC registration scripts,
  `CYODA_OIDC_*` from `docker-compose.yml`, and `subOf`'s `oidc:` handling;
- read `authexecid`/`authexectype`; expect write-back cascade callouts with
  `authtype=user`; keep its segregation-of-duties check on `authid`, comparing
  `(id, kind)`;
- connect its compute node over TLS.

## 17. Documentation

- `docs/access-to-the-cyoda-api.html`, kept in step with this spec.
- Help topics: `auth`, `auth/tokens`, `auth/clients`, `auth/trusted-keys`,
  `config/auth`, `cli/token`, `errors/*`, `telemetry`, `quickstart`, `admin`,
  `grpc`, `messages`, `audit`, `scheduled-tasks`, `workflows`, `helm`, `run`;
  `cmd/cyoda/help/config_registry.go`.
- `README.md` (the OIDC section included), `docs/ARCHITECTURE.md` (§7 and the
  user-id rule), ADR 0004 superseding 0002, `CHANGELOG.md` (Breaking),
  `COMPATIBILITY.md` (SPI pin, chart).
