# `cyoda token` replaces the bootstrap client; M2M clients shared by the cluster — design (#286)

Issue: #286. Delivered as two stacked PRs on `release/v0.9.0`: Part A (§4),
then Part B (§5). Part B carries an SPI contract fix (§5.10) with its cassandra
plugin change. Related: #632 (the machine-to-machine authentication model,
which decides admin machine credentials, token-exchange role limits and
stream lifetime), #624 (platform-operator role, same release),
cyoda-go-cassandra#104 (consistency levels), #622 (stale bootstrap comment,
closed by Part A).

## 1. Summary

**Part A.** The bootstrap M2M client (`CYODA_BOOTSTRAP_*`) is removed. It was
added to solve "needing a token to create tokens", but whoever holds
`CYODA_JWT_SIGNING_KEY` can already sign an admin token for any tenant
(`internal/auth/validator.go:104-179`; #285 spec `:143-146`), so it was a
second root credential weaker than the first, and it needed special handling
at every layer. In its place, `cyoda token` signs a short-lived token offline
with the signing key; an operator uses it for the first admin calls, such as
creating M2M clients. Tokens issued by `/oauth/token` gain the `aud` claim,
which they lack today, so a server configured with `CYODA_JWT_AUDIENCE`
accepts them. A rotation no longer ends the signing key, so routine key
management never takes `cyoda token` away; only invalidating or deleting the
signing key by its key id does. An invalidated key pair keeps verifying until
the end of its grace period, as in Cloud, and never signs again.

**Part B.** M2M clients are stored in the SYSTEM-tenant KV store, one
namespace per tenant plus a global id index. There is no node copy: every
operation, the token endpoint included, reads or writes the store. A client
created, deleted or given a new secret on any node takes effect on every node
when the call returns 2xx, and survives a restart on a persistent backend.

## 2. Terms

- **Signing key**: `CYODA_JWT_SIGNING_KEY`, the bootstrap key pair of the #285
  design; its KID is derived from its public key (`auth.DeriveKID`). Not to be
  confused with the bootstrap *client*, which this design removes.
- **M2M client**: an OAuth `client_credentials` client with an id, a bcrypt
  hash of its secret, a tenant, a user id and roles. Every client is created
  by `POST /clients`.
- **Admin client**: an M2M client whose roles include `ROLE_ADMIN`.
- **KV store**: the SPI `KeyValueStore` of the SYSTEM tenant, carried by
  `AuthConfig.KV` (`internal/auth/service.go:19`, `app/app.go:279`): `Put`,
  `Get`, `Delete`, `List`, no compare-and-set
  (`cyoda-go-spi persistence.go:545-550`).

## 3. Requirements

1. No credential is defined by configuration except the signing key.
2. An operator holding the signing key can obtain an admin token for a tenant
   with no running client, no network call and no store access.
3. A token issued by cyoda-go carries `aud` when `CYODA_JWT_AUDIENCE` is set.
4. A client created, deleted or given a new secret on any node takes effect on
   every node when the call returns 2xx.
5. Clients and every change to them survive a restart on a persistent backend
   (sqlite, postgres, cassandra). The memory backend persists nothing.
6. Tenant isolation holds at the storage layer: a tenant's admin operations
   read and write only that tenant's namespace, and touch the global index
   only for entries naming that tenant.
7. The token endpoint's timing does not reveal whether a client id exists:
   every request that reaches a decision makes the same store reads and one
   bcrypt comparison.
8. No plaintext secret is stored or logged.
9. A store failure is reported as a store failure (5xx), never as an absent or
   invalid client (401 / 404).
10. `GET /clients` reads only the caller's tenant's records; a tenant's client
    count is capped.
11. A rotation (`invalidateCurrent`) does not end the signing key; only an
    invalidate or delete naming its key id does.
12. An invalidated key pair verifies tokens until the end of its grace period
    and never signs again; a grace period of 0 ends it at once.
13. No reference to the bootstrap client remains in code, comments, tests,
    fixtures, the Helm chart, scripts, error messages or documentation outside
    `docs/superpowers/`, dated audit records and past `CHANGELOG.md` entries
    (§6).

## 4. Part A — `cyoda token`; the bootstrap client removed

### 4.1 `cyoda token`

```
cyoda token --tenant <tenantId> [--user <userId>] [--roles <r1,r2>] [--ttl <duration>]
```

- A subcommand of the `cyoda` binary (`cmd/cyoda/token.go`), dispatched in
  `cmd/cyoda/main.go` like `migrate` (`:60-76`). In the container image the
  binary is `/cyoda` (`deploy/docker/Dockerfile:29,35`).
- Configuration: it loads the env files (`app.LoadEnvFiles`) and resolves only
  what it needs — `CYODA_JWT_SIGNING_KEY` (or `_FILE`, through the same PEM
  normalisation as the server, `app/config.go:505`), `CYODA_JWT_ISSUER`,
  `CYODA_JWT_AUDIENCE`, `CYODA_JWT_EXPIRY_SECONDS`. It does not call
  `app.DefaultConfig`, which panics on any unreadable secret file
  (`app/config_secret_env.go:50-54`), and does not call `logging.Init`, which
  writes to stdout (`internal/logging/logging.go:16`). It opens no store and
  makes no network call.
- Flags:
  - `--tenant`, required, checked with `common.ValidateTenantID`;
  - `--user`, default `operator`, checked with `common.ValidateFirstPartyUserID`;
  - `--roles`, default `ROLE_ADMIN`; comma-separated, trimmed; an empty entry
    is refused;
  - `--ttl`, default `15m`; greater than 0 and at most
    `CYODA_JWT_EXPIRY_SECONDS` (default 3600, `app/config.go:431`), the
    lifetime of a token from `/oauth/token`.
- Claims: `sub` and `caas_user_id` = user, `caas_org_id` = tenant,
  `user_roles` = roles (a person principal, `validator.go:149-157`), `iss`,
  `aud` when `CYODA_JWT_AUDIENCE` is set, `iat`, `exp`, `jti`. Signed by the
  signing key with its derived KID.
- Output: the token and a newline on stdout, nothing else. Errors go to stderr
  and never contain the token or key material. Exit codes: 0 success; 1
  missing, unreadable or unparseable signing key or configuration; 2 flag
  error (`migrate`'s convention).
- The token serves HTTP and unary gRPC calls. gRPC streaming needs `ROLE_M2M`
  (`internal/grpc/streaming.go:33`); compute nodes use an M2M client, not a
  `cyoda token` token.
- A token it signs is accepted while the signing key verifies on the cluster.
  Rotations do not end it (§4.4); only an invalidate or delete that names its
  key id does, and that is what revoking the root key means: `cyoda token`
  then stops granting access (after the grace period, if one was given). The
  operator's admin access then comes from an OIDC admin or an admin M2M client
  created beforehand; with neither, the recovery is a new signing key, which
  retires every issued key pair and every token (#285 spec §4). The help topic
  says so. #285's WARN when the signing key is invalidated or deleted
  (`kv_key_store.go` `logRevokedBootstrap`) names `cyoda token`. #624 (same
  release) restricts the key-pair endpoints to platform operators, so a tenant
  admin cannot revoke the signing key.

### 4.2 Removed

- `app.Config.Bootstrap` and `CYODA_BOOTSTRAP_CLIENT_ID`, `_CLIENT_SECRET`,
  `_CLIENT_SECRET_FILE`, `_TENANT_ID`, `_USER_ID`, `_ROLES`
  (`app/config.go:333-346,383-389`); `validateBootstrapConfig`
  (`app/app.go:1084-1123`) and its caller; the creation block
  (`app/app.go:401-427`); the registry entries
  (`cmd/cyoda/help/config_registry.go:100-104`); `app/app_bootstrap_test.go`,
  the bootstrap cases in `app/app_test.go:174-175` and `app/config_*_test.go`.
- `CreateWithSecret` (its only production caller was the bootstrap block).
- Helm: `bootstrap.*` in `values.yaml:58-88` and `values.schema.json:62`,
  `templates/secret-bootstrap.yaml`, the ConfigMap keys
  (`templates/configmap.yaml:67-73`), the Secret mount
  (`templates/statefulset.yaml:98-100,165-170`), the related `_helpers.tpl`
  helpers, `NOTES.txt:27-29`, the chart README sections
  (`README.md:52-66,211-223`), `Chart.yaml:25` (artifacthub changes), and
  `internal/common/user_id_chart_test.go`, which parses
  `bootstrap.userId` from the schema.
- `scripts/multi-node-docker/start-cluster.sh:94-107,344-347` and its README;
  `.env.jwt.example:11-13`.
- No check for the removed variables at startup: a leftover
  `CYODA_BOOTSTRAP_*` is ignored like any unknown variable, and a token request
  that relied on it fails with `401`. The CHANGELOG's `### Breaking` entry
  names the variables and the replacement.

The signing key (`CYODA_JWT_SIGNING_KEY`, its KID and its stored key-pair
state) is unchanged.

With `CYODA_BOOTSTRAP_TENANT_ID` gone, the `caas_org_id` claim is the only
place a tenant id enters the server from outside (`validator.go:137-141`
"Door 1"); `cyoda token --tenant` is checked in the CLI and again by that door
when the token is used. The documents that describe "two doors" change with it
(§6.1).

### 4.3 The `aud` claim on issued tokens

`/oauth/token` builds its claims without `aud` for both grants
(`internal/auth/token.go:89-99` and the token-exchange claims; the only
production signing sites are `token.go:102,241`), `Sign` adds none
(`internal/auth/jwt.go:18-36`), and the validator requires `aud` whenever
`CYODA_JWT_AUDIENCE` is set (`validator.go:91`, wired at
`app/app.go:388-389`). The validator's audience check has unit tests
(`internal/auth/jwt_audience_test.go:33-97`); nothing tests the configured
server against its own tokens. `AuthConfig` gains `Audience`, `NewTokenHandler`
receives it, and both grants set `aud` when it is non-empty; `cyoda token`
does the same.

### 4.4 The signing key in rotations; grace periods

**Rotation.** `Issue` with `invalidateCurrent` ends its siblings: the issued
key pairs of the audience whose window is open, and today also the signing
key (`internal/auth/kv_key_store_admin.go:139-160`). The signing key stops
being a sibling. After a rotation it no longer signs anyway — the newest
active key pair signs — but it keeps verifying, so `cyoda token` keeps
working. This loses nothing: a rotation cannot protect against an exposed
signing key, because that key opens every issued key pair, and replacing it
is the only response (#285 spec §4). `invalidateCurrent` is documented as
ending issued key pairs only, the first rotation included.

**Grace.** Today an invalidated key pair stops verifying at once and the
grace period only keeps its public key in JWKS (`VerificationKey` requires
`Active`, `internal/auth/kv_key_store.go:206-217`; contract in
`docs/cloud-parity/signing-key-window.md:13-16`). Cloud keeps an invalidated
key valid until `validTo` = now + grace
(`platform-service-iam/.../StoredJWKService.kt:607-624,710-713`). cyoda-go
adopts that for verification: an invalidated key pair — issued or the signing
key — verifies until its `validTo`, the end of the grace period. It never
signs again: signer selection keeps requiring `Active`. A grace period of 0,
the default (`internal/domain/account/keys_adapter.go:71-73`), ends
verification at once, so revocation stays immediate when asked for. A deleted
key pair never verifies. Trusted keys already behave this way
(`GetForVerification` checks the window, not `Active`,
`internal/auth/kv_trusted_store.go`).

### 4.5 Getting started in jwt mode

`README.md` "First real call" (`:78-113`), `quickstart.md`, `helm.md`,
`deploy/docker/README.md:58-60`, `scripts/multi-node-docker/README.md` and the
chart's `NOTES.txt` become:

1. start cyoda-go in jwt mode with a signing key;
2. `TOKEN=$(cyoda token --tenant <tenant>)`; in Kubernetes
   `kubectl exec <pod> -- /cyoda token --tenant <tenant>`, in Docker Compose
   `docker compose exec <service> /cyoda token --tenant <tenant>` — the
   container already holds the key;
3. `POST /clients` with that token to create the M2M clients that
   applications and compute nodes use.

Mock mode (the default) is unchanged.

### 4.6 Tests and fixtures

- `internal/e2e`: TestMain (`e2e_test.go:133-139`) and the harness
  (`callback_harness_test.go:228-234`) no longer set bootstrap variables.
  Every token that came from `testclient`/`testsecret` becomes one signed with
  the suite's key (`e2eSignKey`, `h.signKey`) in the shape of today's
  `client_credentials` token (`scopes`, tenant `test-tenant`, the same user
  id), so attribution assertions do not change. That covers `authRequestRaw`
  (`helpers_test.go:118-130`), the harness token (`:361-385`) and every direct
  `getToken`/`postToken`/`adminRequestAs` call: `account_test.go:9`,
  `entity_patch_test.go:34,66`, `ifmatch_callback_self_write_test.go:255`,
  `oauth_keys_test.go:27,154,188,224,529,554`, `oidc_providers_test.go:314`,
  `oidc_reconciliation_test.go:191`, `signing_keys_test.go:300`,
  `unique_keys_test.go:131`, `unique_keys_concurrency_test.go:69`. Tests of
  `/oauth/token` itself (`token_exchange_test.go:79`,
  `token_reconciliation_test.go:67-146`) create their clients with
  `POST /clients`. `async_stream_test.go:1170,1196` and
  `clients_test.go:36-45` change accordingly.
- `e2e/parity/fixtureutil/fixtureutil.go:492-496`: the bootstrap variables go;
  nothing reads them.
- cyoda-go-cassandra: `cyoda-go-cassandra-docker.sh:53-56` and
  `.env.cassandra.example:35-42` (a courtesy PR, with §5.10).

| Scenario | unit | e2e (postgres, in-process) |
|---|---|---|
| `cyoda token`: claims, KID, `aud` only when configured; stdout carries only the token | ✓ | |
| `cyoda token`: each flag refusal (tenant, user, empty role, ttl 0, ttl above the expiry) → 2; missing, unreadable or bad key → 1, no panic; stderr carries no token or key | ✓ | |
| a `cyoda token` token is accepted on HTTP and on a unary gRPC call | | ✓ |
| after the signing key is invalidated through the API with grace 0, a `cyoda token` token is refused (own stack, `newKeyStackOnUnseeded`, `signing_keys_test.go:291`) | | ✓ |
| rotation with `invalidateCurrent` on the signing key's audience: the new key pair signs; the signing key is not written, still verifies, and a `cyoda token` token is accepted; issued siblings are ended | ✓ | ✓ |
| an invalidated key pair (issued, and the signing key) with grace N: verifies before `validTo`, refused after, never selected as signer; grace 0 → refused at once; a deleted key pair never verifies | ✓ | ✓ |
| multi-node (postgres, shared cluster): an issued key pair invalidated on A with a grace period still verifies on B until its `validTo` | | multi-node |
| `CYODA_JWT_AUDIENCE` set: `client_credentials` and token-exchange tokens accepted; a `cyoda token` token accepted; a token without `aud` refused (own stack) | ✓ | ✓ |
| the server starts and serves with no bootstrap variables; a leftover `CYODA_BOOTSTRAP_CLIENT_ID` creates nothing | ✓ | |

## 5. Part B — M2M clients in the store

### 5.1 Layout

| Namespace | Key | Value |
|---|---|---|
| `m2m-clients:<tenantId>` | client id | the client record |
| `m2m-client-ids` | client id | `{"tenantId": …}` — the index entry |

Tenant ids cannot contain `:` (`internal/common/tenant_id.go:36-53`) and every
backend matches a namespace exactly, so namespaces cannot alias.

Client record (JSON): `clientId` (= KV key), `tenantId` (= the namespace's
tenant), `userId`, `roles`, `hashedSecret`, `createdAt`, `updatedAt`
(RFC 3339 UTC).

A record is **undecodable** when the JSON does not parse, `clientId` differs
from its key or is outside §5.2, `tenantId` differs from its namespace or fails
`common.ValidateTenantID`, `userId` fails `common.ValidateFirstPartyUserID`,
`roles` is empty or holds an empty role, `hashedSecret` is not a bcrypt hash,
or a timestamp is outside `StorableTime`. An index entry is undecodable when it
does not parse or its tenant fails `ValidateTenantID`. The encoder refuses to
write anything the decoder would reject.

A client **exists** when its record exists and the index entry for its id
names its tenant. Only an existing client authenticates or can be given a new
secret.

Write order: create writes the record, then the index entry; delete removes
the index entry, then the record. A create whose index write fails removes the
index entry and then the record (§5.4), so an ambiguous failure — a write that
committed but reported an error — leaves nothing behind. A crash between the
two writes leaves a record without an index entry: listed, unable to
authenticate, removable by `DELETE`.

### 5.2 Client id grammar

Unchanged: `^[A-Za-z0-9]{1,100}$` (`internal/domain/account/m2m_adapter.go:21`,
`api/openapi.yaml:761,835,9705,11775`). Generated ids are 16-character
base32-hex (`m2m_adapter.go:30`). The codec, `Create` and the token endpoint
apply it too.

### 5.3 Interface (package `internal/auth`)

```go
type M2MClientStore interface {
	Create(ctx context.Context, tenantID spi.TenantID, clientID, userID string, roles []string) (secret string, err error)
	Authenticate(ctx context.Context, clientID, secret string) (*M2MClient, error)
	List(ctx context.Context, tenantID spi.TenantID) ([]*M2MClient, error)
	Delete(ctx context.Context, tenantID spi.TenantID, clientID string) error
	ResetSecret(ctx context.Context, tenantID spi.TenantID, clientID string) (secret string, c *M2MClient, err error)
}
```

Errors: `ErrInvalidClient` (new; `Authenticate`), `ErrM2MClientNotFound`,
`ErrM2MClientExists`, `ErrM2MClientCapReached` (new; `Create`). Any other error is the store
failing and wraps the KV error, so `common.Internal` maps a
storage-unavailable one to 503.

`KVM2MClientStore` (`internal/auth/kv_m2m_store.go`):
`NewKVM2MClientStore(kv spi.KeyValueStore, maxPerTenant int)`,
built by `NewAuthService` from `IAMFeatures`. It loads nothing at
construction. Every method first removes any transaction from the context
(`spi.WithTransaction(ctx, nil)`), because the postgres KV store joins a
transaction it finds there (`plugins/postgres/store_factory.go:148-161`).

Removed: `InMemoryM2MClientStore`, `NewInMemoryM2MClientStore`, `Get`,
`VerifySecret`; `clientBelongsToTenant` and the adapter's pre-read
(`m2m_adapter.go:109,197-203,238-243`).

### 5.4 Operations

- **Authenticate(id, secret)** — every request that reaches a decision makes
  two KV reads and one bcrypt comparison:
  1. id outside §5.2 → bcrypt against `dummyHash`, `ErrInvalidClient`, no
     store read: the id is attacker-controlled and would otherwise reach the
     store and its error text (`plugins/postgres/kv_store.go:39`).
  2. `Get` the index entry.
  3. Present: `Get` the record in the entry's tenant namespace. Absent: `Get`
     a fixed key that is never written (`m2m-client-ids`, key `-`, outside
     §5.2), so both paths make two reads.
  4. A record → bcrypt against its hash; mismatch → `ErrInvalidClient`; match
     → the client. No record → bcrypt against `dummyHash`, `ErrInvalidClient`.

  A store failure, or an undecodable entry or record, returns that error
  without bcrypt and is logged at ERROR with the KV key (an id that passed
  §5.2).
- **Create(tenant, id, user, roles)**: id outside §5.2 → `ErrInvalidClient`
  (the adapter only passes generated ids). Generate and hash the secret. Under
  a per-node, per-tenant mutex: `List` the tenant's namespace; at
  `maxPerTenant` or more records → `ErrM2MClientCapReached` (`maxPerTenant`
  ≤ 0: no cap); `Get` the index entry; present or undecodable →
  `ErrM2MClientExists`; `Put` the record, then the index entry. If the index
  `Put` fails, delete the index entry, then the record, on a context the
  caller cannot cancel; a failed removal is logged at ERROR with the key.
- **List(tenant)**: `List` the tenant's namespace. Undecodable records are
  skipped and logged at ERROR with their keys, as the replica does at load
  (`internal/auth/replica.go:117-122`).
- **Delete(tenant, id)**: `Get` the record in the tenant's namespace and the
  index entry. Neither a record (decodable or not) nor an index entry naming
  this tenant → `ErrM2MClientNotFound`. Otherwise remove the index entry if it
  names this tenant, then the record if present. This also removes an
  undecodable record, and a record left without its index entry; the
  namespace proves ownership. An index entry naming another tenant is never
  touched.
- **ResetSecret(tenant, id)**: generate and hash the secret first, so the gap
  between read and write is one round trip, not a bcrypt. `Get` the record in
  the tenant's namespace and the index entry; the client does not exist
  (§5.1) → `ErrM2MClientNotFound`; `Put` the record with the new hash and
  `updatedAt` now.

The token handler (`internal/auth/token.go:59-80,141`) calls `Authenticate`
once and uses the returned client for both grants: `ErrInvalidClient` →
`401 invalid_client`; any other error → `writeTokenServerError`
(`token.go:305`, `500 server_error` with a ticket).

The adapter maps `ErrM2MClientNotFound` → `404 M2M_CLIENT_NOT_FOUND`,
`ErrM2MClientCapReached` → `400 M2M_CLIENT_CAP_REACHED`, anything else →
`common.Internal` (500 with a ticket, or `503 STORAGE_UNAVAILABLE`).

### 5.5 Cap

`CYODA_IAM_M2M_CLIENT_MAX_PER_TENANT`, default 100, 0 = unbounded, a field of
`IAMFeatures` beside `TrustedKeyMaxPerTenant` (`app/config.go:276,434`). The
per-node mutex serialises the check on one node; creates on different nodes
at the same moment can each pass it, so the cap can be exceeded by at most one
record per node (no compare-and-set). Documented.

### 5.6 Admin machine credentials

Unchanged by this design. Whether admin M2M clients should exist, and the
rule for creating and resetting them and for minting admin power through
token exchange, belong to #632. The record's credential field
(`hashedSecret`) is the one place that design extends to hold public keys.

### 5.7 Consistency and races

Requirement 4 holds because every read goes to the store and the store makes a
committed write visible to a read on any node: postgres; cassandra at `QUORUM`
or `LOCAL_QUORUM` (cyoda-go-cassandra#104 makes these the only accepted
levels). Memory and sqlite are single-node.

Documented, not prevented (no compare-and-set): concurrent admin changes to
one client on two nodes. A reset racing a delete can write the record back
after the delete removed it, leaving a record without an index entry: listed,
unable to authenticate, removable by `DELETE`. And the cap overshoot of §5.5.

On cassandra, a write adds a version row and a delete keeps earlier data
(`cyoda-go-cassandra internal/store/data_store.go:90-97,166-168`): hashes of
deleted clients and of reset secrets remain in the store and its backups
until compaction. They are bcrypt hashes of 256-bit random secrets.

### 5.8 Token endpoint cost

Two KV point reads per `POST /oauth/token` (on cassandra each is a metadata
and a data read at the configured level), next to ~100 ms of bcrypt. Tokens
live `CYODA_JWT_EXPIRY_SECONDS` (default 3600, `app/config.go:431`).

### 5.9 Errors

| Endpoint | Status | Code | Cause | New? |
|---|---|---|---|---|
| `POST /oauth/token` | 401 | `invalid_client` | no Basic credentials; id outside §5.2; unknown id; wrong secret | id check new |
| | 500 | `server_error` | store failure; undecodable entry or record | yes (was 401) |
| | 400 / 500 | | grant validation; signer failure | no |
| all four `/clients` operations | 401 / 403 / 501 | | unauthenticated; not admin; not jwt mode | no |
| | 500 | | store failure (ticket, generic message); for delete and reset also an undecodable index entry | yes (list: new; delete, reset: was 404) |
| | 503 | `STORAGE_UNAVAILABLE` | the store reports itself unavailable | yes; add to OpenAPI for all four |
| `POST /clients` | 200 | | created | stored now |
| | 400 | `M2M_CLIENT_CAP_REACHED` | the tenant is at the cap | yes; new code |
| | 404 | `FEATURE_DISABLED` | `withAdminRole=true` while disabled | no |
| `GET /clients` | 200 | | the caller's tenant's clients; undecodable records skipped | stored now |
| `DELETE /clients/{clientId}` | 200 | | deleted (an undecodable or unindexed record of the caller's tenant included) | stored now |
| | 400 | `BAD_REQUEST` | id outside §5.2 | no |
| | 404 | `M2M_CLIENT_NOT_FOUND` | absent; another tenant's | no |
| `PUT /clients/{clientId}/secret` | 200 | | new secret | stored now |
| | 400 | `BAD_REQUEST` | id outside §5.2 | no |
| | 404 | `M2M_CLIENT_NOT_FOUND` | absent; another tenant's; a record without its index entry | no |
| | 500 | | an undecodable record | yes |

No gRPC surface manages clients (a search of `internal/grpc` and `api/grpc` for
client-management terms is empty); gRPC only verifies tokens.

### 5.10 SPI: deleting an absent key

The SPI does not say what `Delete` of an absent key returns, and `spitest`
does not test it (`cyoda-go-spi spitest/keyvalue.go:48-55`,
`message.go:48-66`, `workflow.go:56-64`). Memory, sqlite and postgres return
`nil` for KV, message and workflow `Delete`; cassandra returns
`spi.ErrNotFound` for all three through its shared `dataStore.delete`
(`cyoda-go-cassandra internal/store/data_store.go:170-180,274,327,388,393`).
Visible today: a `DELETE /message` batch containing an absent id answers 500
on cassandra and 200 elsewhere (`internal/domain/messaging/handler.go:332`);
`KVOidcProviderStore.Delete` (`internal/auth/oidc/kv_store.go:104-111`) fails
on cassandra when its index key is already gone.

Contract: `Delete` (KV, message, workflow) and `DeleteBatch` of an absent key
return `nil`. SPI doc comments state it; `spitest` gains a case per method; the
cassandra plugin's `dataStore.delete` returns `nil` for an absent or
already-deleted key; a parity scenario covers the `DELETE /message` batch.
Mechanics: an SPI PR into `main`, cyoda-go pseudo-pins `main` (no tag
mid-milestone), a cassandra PR on the same pin. Once pinned, the `ErrNotFound`
tolerance at `internal/auth/replica.go:379` is unreachable and is removed,
with the comment that describes it (`:352`).

### 5.11 Tests

Fixture constraints:
- **In-process cross-node**: two stacks on one database (`newSchedDB`,
  `newStackOn`, `internal/e2e/scheduler_harness_test.go:41,109`). With no node
  copy there is nothing to wait for, so cross-node assertions do not poll. A
  restart is a new stack on the same database.
- **Undecodable records**: a raw KV write on a stack's own database (as
  `internal/e2e/signing_keys_test.go:287`), never the shared server.
- **Parity cap**: `fixtureutil.CyodaEnv` (`e2e/parity/fixtureutil/fixtureutil.go:483`,
  also used by `cyoda-go-cassandra e2e/fixture.go:76`) sets
  `CYODA_IAM_M2M_CLIENT_MAX_PER_TENANT` to a small value; every parity tenant
  is fresh (`NewTenant`), and the plan checks that no scenario creates more
  clients in one tenant.
- **Multi-node runs on postgres only**; cassandra runs the single-node parity
  list (`cyoda-go-cassandra e2e/cassandra_test.go:91`; its multi-node fixture
  is cyoda-go-cassandra#35).
- Callers of removed methods move to `Create`
  (`internal/e2e/callback_txjoin_errors_test.go:75`,
  `internal/e2e/oauth_keys_test.go:598`,
  `internal/auth/local_validator_integration_test.go:29`,
  `internal/auth/token_test.go`, `internal/domain/account/m2m_adapter_test.go`,
  `internal/auth/store_test.go`), and ids outside §5.2 in tests become valid
  ones (`internal/auth/integration_test.go:50,177,237`, `delegating_test.go:49`,
  `store_test.go:214,241,701,722,742`).

| Scenario | unit | e2e (postgres, in-process) | parity single-node (memory, sqlite, postgres, cassandra) | multi-node (postgres) |
|---|---|---|---|---|
| create → token; list; reset → old secret 401, new 200; delete → 401 | ✓ | ✓ | ✓ | |
| another tenant: absent from list; delete and reset → 404 with the absent-client body | ✓ | ✓ | ✓ | |
| create on A → token on B at once; reset on A → old secret 401 on B; delete on A → 401 on B | | ✓ | | ✓ shared cluster |
| create, reset, delete each survive a restart | | ✓ | | |
| cap: at the cap → 400 `M2M_CLIENT_CAP_REACHED`; 0 → unbounded; a delete frees a slot; concurrent creates on one node stop at the cap | ✓ | ✓ | ✓ | |
| token endpoint: id with NUL, invalid UTF-8, 101 characters, an encoded `:` → 401, no store read, one bcrypt | ✓ | ✓ | | |
| unknown id; a record without an index entry; wrong secret: each makes two reads and one bcrypt → 401 | ✓ | | | |
| create: index `Put` fails, and a faulty KV whose `Put` commits then errors → index entry and record both gone | ✓ (faulty KV) | | | |
| delete removes an undecodable record and a record without its index entry; never an index entry naming another tenant | ✓ | ✓ (raw KV write) | | |
| reset of a record without its index entry → 404 | ✓ | | | |
| undecodable index entry or record: token → 500; list skips with ERROR | ✓ | ✓ (raw KV write) | | |
| failing KV on every method → store error, never not-found / invalid-client; adapter → 500 / 503; token → 500 | ✓ (faulty KV) | | | |
| a caller's transaction in the context is not joined by the store | ✓ | | | |
| codec: round trip; each decode refusal; the encoder refuses what decode rejects | ✓ | | | |
| no plaintext secret in any stored value | ✓ | ✓ (raw KV read) | | |
| SPI: absent-key `Delete` (KV, message, workflow) and `DeleteBatch` → nil | spitest, every backend | | ✓ (`DELETE /message` batch with an absent id → 200) | |
| every `/clients` and `/oauth/token` response conforms to the OpenAPI (enforce-mode validator) | | ✓ | | |

Waivers:
- 503 on `/clients` is not tested end to end: the shared container cannot be
  paused, and the adapter passes errors through `common.Internal`, whose 503
  mapping is tested (`internal/common/errors.go:169-177`); a unit row asserts
  the adapter reaches it.
- Cassandra multi-node: no fixture yet (cyoda-go-cassandra#35).

## 6. Documentation, comments and exit checks

Every document, code comment and message outside `docs/superpowers/` (plans,
specs and research are records of their time), dated audit records
(`docs/audits/`) and past `CHANGELOG.md` entries is brought in line. Each PR
ends with its exit checks returning nothing, and with a fresh-context review
of documentation and comments against this section.

### 6.1 Part A

- `cyoda help` (`cmd/cyoda/help/content/`):
  - new `cli/token.md` (usage, claims, exit codes, the Kubernetes and Docker
    forms, the signing-key revocation consequence and the advice of §4.1,
    compute nodes need an M2M client); `cli.md`;
  - `auth.md`; `auth/clients.md:34,133`; `auth/tokens.md:136` (and `aud`);
    `auth/oidc.md:44,197` (`default-tenant`);
  - `config.md:48`; `config/auth.md:79` ("the two places"), `:91,127,131`,
    `:140-160`, `:370-378`, and the `CYODA_JWT_AUDIENCE` entry;
  - `helm.md:35,110-138,302,340,477-490`; `quickstart.md:87-96`; `run.md`;
    `errors/OIDC_INVALID_TENANT.md`;
  - `config_registry.go:100-104`.
- Messages and code: `internal/domain/account/oidc_adapter.go:110` (a 400
  message naming `default-tenant` bootstrap deployments);
  `internal/common/error_codes.go:266`.
- Code comments: `internal/auth/validator.go:139`;
  `internal/common/user_id.go:32,70`; `e2e/parity/multinode/attribution.go:74`;
  `internal/e2e/e2e_test.go:41`; `internal/e2e/oidc_providers_test.go:20,282,312`;
  `internal/e2e/oidc_reconciliation_test.go:184`; `internal/e2e/attribution_test.go:41`;
  `internal/e2e/token_exchange_test.go:59`; `internal/common/tenant_id_test.go:17`;
  `internal/common/user_id_test.go:11`; `internal/domain/account/oidc_adapter_test.go:774`;
  and every file the exit checks find.
- `README.md:78-113` and every other mention; `docs/PRD.md:560`;
  `docs/ARCHITECTURE.md:230,232` (the tenant and user-id doors),
  `:1879-1888,2043,2164-2172`; `docs/cyoda/cloud-divergences.md:49-63` (the
  stub probe uses `cyoda token`); `docs/cloud-parity/README.md:58,60`,
  `tenant-id-grammar.md:62,72` ("two doors"), `user-id-rule.md`;
  `deploy/docker/README.md:58-60`; `scripts/dev/README.md`;
  `scripts/multi-node-docker/README.md`.
- Helm: `deploy/helm/cyoda/README.md`, `NOTES.txt`, `values.yaml` comments,
  `Chart.yaml:25`; `COMPATIBILITY.md:184` (the 0.9.0 chart row).
- Signing-key rules (§4.4): `config/auth.md` §"JWT signing keypair rotation"
  (`:236-262`: `invalidateCurrent` ends issued key pairs only; an invalidated
  key verifies through its grace period and never signs); the OpenAPI
  descriptions of `invalidateCurrent`, `invalidateGracePeriodSec` and
  `gracePeriodSec` (`api/openapi.yaml:5542,5759,5903` and the schemas at
  `:10586,10603,10708`), then `go generate ./api`;
  `docs/cloud-parity/signing-key-window.md:13-16` and `signing-key-pairs.md`
  (the grace rule now matches Cloud; the signing key is not a rotation
  sibling — Cloud has no signing key from configuration);
  `docs/ARCHITECTURE.md` §7.2; the `logRevokedBootstrap` WARN.
- `CHANGELOG.md` `### Breaking`: the six variables and the chart's
  `bootstrap.*` values are removed; use `cyoda token`; `invalidateCurrent` no
  longer ends the signing key; an invalidated key pair verifies until the end
  of its grace period. `### Fixed`: tokens from `/oauth/token` carry `aud`
  when `CYODA_JWT_AUDIENCE` is set.
- Issues: #622 closed by Part A; a comment on #624 (the bootstrap client is no
  longer a route to a platform operator; `cyoda token` depends on the signing
  key staying active); a comment on #286.

Exit checks (both repos, from the root; `--exclude-dir` matches a directory's
base name):
```
grep -rnE 'CYODA_BOOTSTRAP_|Bootstrap\.Client|bootstrap (M2M )?client|bootstrap tenant|default-tenant|bootstrap\.(clientId|clientSecret|tenantId|userId|roles)|validateBootstrapConfig|CreateWithSecret|secret-bootstrap' \
  --exclude-dir=superpowers --exclude-dir=audits --exclude-dir=.git --exclude=CHANGELOG.md .
grep -rnE 'testclient|testsecret|compute-secret|two (doors|places)' \
  --exclude-dir=superpowers --exclude-dir=audits --exclude-dir=.git .
```
Past `CHANGELOG.md` entries are the only allowed hits; a hit that is not about
the bootstrap client (e.g. an unrelated "two places") is recorded in the PR
with the reason.

### 6.2 Part B

- `cyoda help`: `auth/clients.md` (clients stored and shared by the cluster;
  the cap; the documented races;
  500 / 503 on store failure); `auth/tokens.md` (`/oauth/token` 500 on store
  failure); `config/auth.md` and `config_registry.go`
  (`CYODA_IAM_M2M_CLIENT_MAX_PER_TENANT`); `errors/M2M_CLIENT_CAP_REACHED.md`
  (new); `errors.md`.
- `README.md` configuration reference; `DefaultConfig()`.
- `docs/ARCHITECTURE.md:1857` (the component table), §7.2's store text, and
  `:230` ("the M2M client table … does not re-check it" — the decoder now
  does).
- OpenAPI: 503 on the four `/clients` operations, 400
  `M2M_CLIENT_CAP_REACHED` on create;
  `go generate ./api`.
- `docs/cloud-parity/m2m-clients.md` (new) and the README index: the cap and
  its error code, with a CaaS ticket. Clients being shared
  and persistent is not a contract change (Cloud already behaves so).
- `COMPATIBILITY.md`: the cyoda-go-spi pin; the cassandra plugin must include
  the absent-key `Delete` fix.
- `CHANGELOG.md` `### Breaking`: clients stored and shared; the token endpoint
  answers 500 (was 401) on a store failure; `/clients` delete and reset answer
  500 / 503 (was 404) on a store failure. `### Added`: the cap. `### Fixed`: absent-key `Delete` on cassandra.
- Code comments: `internal/auth/store.go` (the duplicated section header and
  the in-memory store go), `e2e/parity/multinode/signing_keys.go:56-57` and
  `e2e/parity/postgres/signing_keys_cluster_test.go:158` ("M2M clients are per
  node"), `internal/auth/replica.go:352`, and every file the exit checks find.

Exit checks:
```
grep -rnE 'InMemoryM2MClientStore|NewInMemoryM2MClientStore|VerifySecret|clientBelongsToTenant|M2M clients are per node|per-node .*M2M' \
  --exclude-dir=superpowers --exclude-dir=audits --exclude-dir=.git --exclude=CHANGELOG.md .
grep -nE 'Delete\(.*ErrNotFound|errors\.Is\(err, spi\.ErrNotFound\)' internal/auth/replica.go   # only the Get at :317 remains
```

## 7. Out of scope

- A compare-and-set in the KV SPI (would close the §5.7 races).
- The machine-to-machine authentication model — client authentication by
  public key or federation, stream lifetime, principal types and scopes,
  admin machine credentials, token-exchange and OIDC role limits: #632.
- The platform-operator role: #624.
- Cassandra consistency levels: cyoda-go-cassandra#104.
- Cassandra multi-node fixture: cyoda-go-cassandra#35.
- Data migration: there are no production instances.
