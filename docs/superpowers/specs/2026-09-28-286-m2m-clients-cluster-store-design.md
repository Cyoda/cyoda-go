# `cyoda token` replaces the bootstrap client; M2M clients shared by the cluster — design (#286)

Issue: #286. Delivered as two stacked PRs on `release/v0.9.0`: Part A (§4),
then Part B (§5). Part B carries an SPI contract fix (§5.9) with its cassandra
plugin change. Related: cyoda-go-cassandra#104 (consistency levels), #624
(platform-operator role), #622 (stale bootstrap comment, closed by Part A).

## 1. Summary

**Part A.** The bootstrap M2M client (`CYODA_BOOTSTRAP_*`) is removed. It was
added to solve "needing a token to create tokens", but whoever holds
`CYODA_JWT_SIGNING_KEY` can already sign an admin token for any tenant
(`internal/auth/validator.go:133-168`; #285 spec `:143-146`), so it was a
second root credential weaker than the first, and it needed special handling
at every layer. In its place, `cyoda token` signs a short-lived token offline
with the signing key. An operator uses it for the first admin calls, such as
creating M2M clients. Tokens issued by `/oauth/token` gain the `aud` claim,
which they lack today, so a server configured with `CYODA_JWT_AUDIENCE`
accepts them.

**Part B.** M2M clients are stored in the SYSTEM-tenant KV store, one
namespace per tenant plus a global id index. There is no node copy: every
operation, the token endpoint included, reads or writes the store. A client
created, deleted or given a new secret on any node takes effect on every node
when the call returns 2xx, and survives a restart on a persistent backend.

## 2. Terms

- **Signing key**: `CYODA_JWT_SIGNING_KEY`, the bootstrap key pair of the #285
  design. Its KID is derived from its public key (`auth.DeriveKID`).
- **M2M client**: an OAuth `client_credentials` client with an id, a bcrypt
  hash of its secret, a tenant, a user id and roles. Every client is created
  by `POST /clients`.
- **Admin client**: an M2M client whose roles include `ROLE_ADMIN`.
- **KV store**: the SPI `KeyValueStore` of the SYSTEM tenant, already carried
  by `AuthConfig.KV` (`internal/auth/service.go:19`, `app/app.go:279`):
  `Put`, `Get`, `Delete`, `List`, no compare-and-set
  (`cyoda-go-spi persistence.go:545-550`).

## 3. Requirements

1. No credential is defined by configuration except the signing key.
2. An operator holding the signing key can obtain an admin token for a tenant
   without a running client, a network call or the store.
3. A token issued by cyoda-go carries `aud` when `CYODA_JWT_AUDIENCE` is set,
   so it passes cyoda-go's own audience check.
4. A client created, deleted or given a new secret on any node takes effect on
   every node when the call returns 2xx.
5. Clients and every change to them survive a restart on a persistent backend
   (sqlite, postgres, cassandra). The memory backend persists nothing.
6. Tenant isolation holds at the storage layer: a tenant's admin operations
   read and write only that tenant's namespace.
7. The token endpoint's timing does not reveal whether a client id exists.
8. No plaintext secret is stored or logged.
9. A store failure is reported as a store failure (5xx), never as an absent or
   invalid client (401 / 404).
10. `GET /clients` reads only the caller's tenant's records; a tenant's client
    count is capped.
11. No reference to the bootstrap client remains in code, comments, tests,
    fixtures, the Helm chart, scripts or documentation outside
    `docs/superpowers/` and past `CHANGELOG.md` entries (§6).

## 4. Part A — `cyoda token`; the bootstrap client removed

### 4.1 `cyoda token`

```
cyoda token --tenant <tenantId> [--user <userId>] [--roles <r1,r2>] [--ttl <duration>]
```

- A subcommand of the `cyoda` binary, dispatched in `cmd/cyoda/main.go` like
  `migrate` (`:60-76`), in `cmd/cyoda/token.go`.
- Loads configuration the way the server does (`app.LoadEnvFiles`,
  `app.DefaultConfig`): `CYODA_JWT_SIGNING_KEY` (or `_FILE`),
  `CYODA_JWT_ISSUER`, `CYODA_JWT_AUDIENCE`. It opens no store and makes no
  network call.
- Flags:
  - `--tenant`, required, checked with `common.ValidateTenantID`;
  - `--user`, default `operator`, checked with `common.ValidateFirstPartyUserID`;
  - `--roles`, default `ROLE_ADMIN`; comma-separated, trimmed; an empty entry
    is refused;
  - `--ttl`, default `15m`, greater than 0 and at most `24h`.
- Claims: `sub` and `caas_user_id` = user, `caas_org_id` = tenant,
  `user_roles` = roles (the principal is a person, `validator.go:149-157`),
  `iss`, `aud` when `CYODA_JWT_AUDIENCE` is set, `iat`, `exp`, `jti`. Signed by
  the signing key with its derived KID.
- Output: the token and a newline on stdout, nothing else. Errors go to stderr
  and never contain the token or key material. Exit codes: 0 success; 1 missing
  or unparseable signing key; 2 flag error (`migrate`'s convention).
- A token it signs is accepted only while the signing key is active on the
  cluster. If the key was invalidated or deleted through the API, recovery is
  an OIDC admin or a new signing key (#285 spec §4); the command cannot check
  this offline, and the help topic says so.

### 4.2 Removed

- `app.Config.Bootstrap` and `CYODA_BOOTSTRAP_CLIENT_ID`, `_CLIENT_SECRET`,
  `_CLIENT_SECRET_FILE`, `_TENANT_ID`, `_USER_ID`, `_ROLES`
  (`app/config.go:333-346,383-389`); `validateBootstrapConfig`
  (`app/app.go:1084-1123`) and its caller; the creation block
  (`app/app.go:401-427`); the registry entries
  (`cmd/cyoda/help/config_registry.go:100-104`); `app/app_bootstrap_test.go`
  and the bootstrap cases in `app/config_*_test.go`.
- `CreateWithSecret` (its only production caller was the bootstrap block).
- Helm: `bootstrap.*` in `values.yaml:58-88` and `values.schema.json:62`,
  `templates/secret-bootstrap.yaml`, the ConfigMap keys
  (`templates/configmap.yaml:67-73`), the Secret mount
  (`templates/statefulset.yaml:98-100,165-170`), the related `_helpers.tpl`
  helpers, `NOTES.txt:27-29` (replaced by the `cyoda token` instruction below)
  and the chart README sections (`README.md:52-66,211-223`).
- `scripts/multi-node-docker/start-cluster.sh:94-107,344-347` and its README;
  `.env.jwt.example:11-13`.
- There is no check for the removed variables at startup: a leftover
  `CYODA_BOOTSTRAP_*` is ignored like any unknown variable, and the first
  token request that relied on it fails with `401`. The CHANGELOG's
  `### Breaking` entry names the variables and the replacement.

The bootstrap *signing key* (`CYODA_JWT_SIGNING_KEY`, KID, bootstrap key-pair
state) is unrelated and unchanged.

### 4.3 The `aud` claim on issued tokens

`/oauth/token` builds its claims without `aud` for both grants
(`internal/auth/token.go:89-99` and the token-exchange claims), `Sign` adds
none (`internal/auth/jwt.go:18-36`), and the validator requires `aud` whenever
`CYODA_JWT_AUDIENCE` is set (`validator.go:91`, `app/app.go:388-389`). No test
sets that variable. `NewTokenHandler` receives the configured audience and
both grants set `aud` when it is non-empty; `cyoda token` does the same.

### 4.4 Getting started in jwt mode

`README.md` "First real call" (`:78-113`), `quickstart.md`, `helm.md` and the
chart's `NOTES.txt` become:

1. start cyoda-go in jwt mode with a signing key;
2. `TOKEN=$(cyoda token --tenant <tenant>)` — in Kubernetes,
   `kubectl exec <pod> -- cyoda token --tenant <tenant>`, the pod already
   holding the key;
3. `POST /clients` with that token to create the M2M clients applications and
   compute nodes use.

Mock mode (the default) is unchanged.

### 4.5 Tests and fixtures

- `internal/e2e`: TestMain (`e2e_test.go:133-139`) no longer sets bootstrap
  variables. `authRequestRaw` (`helpers_test.go:118-130`) and the harness
  (`callback_harness_test.go:228-234,361-385`) sign their admin tokens with
  the suite's signing key (`e2eSignKey`, `h.signKey`), in the same shape as a
  `client_credentials` token today (`scopes`, tenant `test-tenant`, the same
  user id), so tests that assert on attribution see no change. Tests that
  exercise `/oauth/token` itself create their clients with `POST /clients`.
  `async_stream_test.go:1170,1196` and `clients_test.go:36-45` change
  accordingly.
- `e2e/parity/fixtureutil/fixtureutil.go:492-496`: the bootstrap variables go;
  nothing reads them.
- cyoda-go-cassandra: `cyoda-go-cassandra-docker.sh:53-56` and
  `.env.cassandra.example:37-38` (a courtesy PR, together with §5.9).

| Scenario | unit | e2e (postgres, in-process) |
|---|---|---|
| `cyoda token`: claims, KID, `aud` present only when configured, stdout carries only the token | ✓ | |
| `cyoda token`: each flag refusal (tenant, user, empty role, ttl 0 / > 24h), missing or bad key → exit codes; no token or key in stderr | ✓ | |
| a `cyoda token` token is accepted on HTTP and gRPC; refused after the signing key is invalidated through the API | | ✓ |
| `CYODA_JWT_AUDIENCE` set: `client_credentials` and token-exchange tokens are accepted; a `cyoda token` token is accepted; a token without `aud` is refused | ✓ | ✓ |
| the server starts and serves with no bootstrap variables; a leftover `CYODA_BOOTSTRAP_CLIENT_ID` changes nothing | ✓ | |

## 5. Part B — M2M clients in the store

### 5.1 Layout

| Namespace | Key | Value |
|---|---|---|
| `m2m-clients:<tenantId>` | client id | the client record |
| `m2m-client-ids` | client id | `{"tenantId": …}` — the index |

Client record (JSON): `clientId` (= KV key), `tenantId` (= the namespace's
tenant), `userId`, `roles`, `hashedSecret`, `createdAt`, `updatedAt`
(RFC 3339 UTC).

A record is **undecodable** when the JSON does not parse, `clientId` differs
from its key, `tenantId` differs from its namespace or fails
`common.ValidateTenantID`, `userId` fails `common.ValidateFirstPartyUserID`,
`roles` is empty or holds an empty role, `hashedSecret` is not a bcrypt hash,
or a timestamp is outside `StorableTime`. An index entry is undecodable when
it does not parse or its tenant fails `ValidateTenantID`. The encoder refuses
to write anything the decoder would reject.

A client **exists** when both its index entry and its record exist. The index
entry is written last on create and removed first on delete, so a failure
between the two writes leaves at most a record without an index entry: listed
by `GET /clients`, unable to authenticate, removable by `DELETE`.

### 5.2 Client id grammar

Unchanged: `^[A-Za-z0-9]{1,100}$` (`internal/domain/account/m2m_adapter.go:21`,
`api/openapi.yaml:761,835,9705,11775`). Generated ids are 16-character
base32-hex (`m2m_adapter.go:30`). The token endpoint now applies it too (§5.4).

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

Errors: `ErrInvalidClient` (new; `Authenticate` only), `ErrM2MClientNotFound`,
`ErrM2MClientExists`, `ErrM2MClientCapReached` (new; `Create`),
`ErrM2MAdminRoleDisabled` (new; `ResetSecret`). Any other error
is the store failing and wraps the KV error, so `common.Internal` maps a
storage-unavailable one to 503.

`KVM2MClientStore` (`internal/auth/kv_m2m_store.go`):
`NewKVM2MClientStore(kv spi.KeyValueStore, maxPerTenant int, adminRoleEnabled bool)`,
built by `NewAuthService` from `IAMFeatures`. It loads
nothing at construction. Every method first removes any transaction from the
context (`spi.WithTransaction(ctx, nil)`), so a KV call never joins a caller's
entity transaction (`plugins/postgres/store_factory.go:148-161`).

Removed: `InMemoryM2MClientStore`, `NewInMemoryM2MClientStore`, `Get`,
`VerifySecret`; `clientBelongsToTenant` and the pre-read in the adapter
(`m2m_adapter.go:109,197-203,238-243`).

### 5.4 Operations

- **Authenticate(id, secret)**:
  1. id outside §5.2 → bcrypt against `dummyHash`, `ErrInvalidClient`. No
     store read: the id is attacker-controlled and would otherwise reach the
     store and its error text (`plugins/postgres/kv_store.go:40`).
  2. `Get` the index entry. Absent → dummy bcrypt, `ErrInvalidClient`.
  3. `Get` the record in the entry's tenant namespace. Absent → dummy bcrypt,
     `ErrInvalidClient`.
  4. bcrypt against the record's hash; mismatch → `ErrInvalidClient`; match →
     the client.

  A store failure or an undecodable entry or record at step 2 or 3 returns
  that error without bcrypt, logged at ERROR with the KV key (the id, which
  passed §5.2). Every request that reaches a decision runs one bcrypt.
- **Create(tenant, id, user, roles)**: generate and hash the secret. `List`
  the tenant's namespace; at `maxPerTenant` or more records →
  `ErrM2MClientCapReached` (`maxPerTenant` ≤ 0: no cap). `Get` the index
  entry; present or undecodable → `ErrM2MClientExists`. `Put` the record, then
  the index entry. If the index `Put` fails, delete the record (on a context
  the caller cannot cancel); a failed delete is logged at ERROR with the key.
- **List(tenant)**: `List` the tenant's namespace. Undecodable records are
  skipped and logged at ERROR with their keys, as the replica does at load
  (`internal/auth/replica.go:117-122`).
- **Delete(tenant, id)**: `Get` the record in the tenant's namespace; absent →
  `ErrM2MClientNotFound`; undecodable → store error. Delete the index entry,
  then the record. A client of another tenant is not in this namespace, so it
  is `ErrM2MClientNotFound` by construction.
- **ResetSecret(tenant, id)**: generate and hash the secret first, so the gap
  between read and write is one round trip, not a bcrypt. `Get` the record in
  the tenant's namespace; absent → `ErrM2MClientNotFound`. An admin client
  while admin-role grants are disabled → `ErrM2MAdminRoleDisabled` (§5.6).
  `Put` the record with the new hash and `updatedAt` now.

The token handler (`internal/auth/token.go:59-80,141`) calls `Authenticate`
once and uses the returned client for both grants: `ErrInvalidClient` →
`401 invalid_client`; any other error → `writeTokenServerError`
(`token.go:305`, `500 server_error` with a ticket).

The adapter maps `ErrM2MClientNotFound` → `404 M2M_CLIENT_NOT_FOUND`,
`ErrM2MClientCapReached` → `400 M2M_CLIENT_CAP_REACHED`,
`ErrM2MAdminRoleDisabled` → `404 FEATURE_DISABLED`, anything else →
`common.Internal` (500 with a ticket, or `503 STORAGE_UNAVAILABLE`).

### 5.5 Cap

`CYODA_IAM_M2M_CLIENT_MAX_PER_TENANT`, default 100, 0 = unbounded, a field of
`IAMFeatures` beside `TrustedKeyMaxPerTenant` (`app/config.go:276,434`). The
check reads the tenant's namespace, so concurrent creates on different nodes
can pass it together and exceed the cap by their number (no compare-and-set);
documented.

### 5.6 Resetting an admin client

Creating an admin client needs `CYODA_IAM_M2M_ADMIN_ROLE_ENABLED`
(`m2m_adapter.go:123-126`, off by default, `internal/auth/iam_features.go:17-20`).
Resetting one does not today (`m2m_adapter.go:222-266`), so a tenant admin can
obtain an admin machine credential by resetting an existing admin client while
the flag is off. With Part B, resetting an admin client needs the flag too,
and answers `404 FEATURE_DISABLED` without it — after the tenant check, so
another tenant's client still answers `404 M2M_CLIENT_NOT_FOUND`. Delete is not
gated: revocation always works.

### 5.7 Consistency and races

Requirement 4 holds because every read goes to the store and the store makes a
committed write visible to a read on any node: postgres; cassandra at `QUORUM`
or `LOCAL_QUORUM` (cyoda-go-cassandra#104 makes these the only accepted
levels). Memory and sqlite are single-node.

Documented, not prevented (no compare-and-set): concurrent admin changes to
one client on two nodes — the later write wins, so a reset racing a delete can
leave the client present with the new secret, held by the admin who reset it
(same tenant); and the cap overshoot of §5.5.

### 5.8 Token endpoint cost

Two KV point reads per `POST /oauth/token` (on cassandra each is a metadata
and a data read at the configured level), next to ~100 ms of bcrypt. Tokens
live `CYODA_JWT_EXPIRY_SECONDS` (default 3600, `app/config.go:431`).

### 5.9 SPI: deleting an absent key

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
already-deleted key. Mechanics: an SPI PR into `main`, cyoda-go pseudo-pins
`main` (no tag mid-milestone), a cassandra PR on the same pin. Once pinned,
the `ErrNotFound` tolerance at `internal/auth/replica.go:379` is unreachable
and is removed.

### 5.10 Errors

| Endpoint | Status | Code | Cause | New? |
|---|---|---|---|---|
| `POST /oauth/token` | 401 | `invalid_client` | no Basic credentials; id outside §5.2; unknown id; wrong secret | id check new |
| | 500 | `server_error` | store failure; undecodable entry or record | yes (was 401) |
| | 400 / 500 | | grant validation; signer failure | no |
| all four `/clients` operations | 401 / 403 / 501 | | unauthenticated; not admin; not jwt mode | no |
| | 500 | | store failure; undecodable record (ticket, generic message) | yes (list: new; delete, reset: was 404) |
| | 503 | `STORAGE_UNAVAILABLE` | the store reports itself unavailable | yes; add to OpenAPI for all four |
| `POST /clients` | 200 | | created | stored now |
| | 400 | `M2M_CLIENT_CAP_REACHED` | the tenant is at the cap | yes; new code |
| | 404 | `FEATURE_DISABLED` | `withAdminRole=true` while disabled | no |
| `GET /clients` | 200 | | the caller's tenant's clients | stored now |
| `DELETE /clients/{clientId}` | 200 | | deleted | stored now |
| | 400 | `BAD_REQUEST` | id outside §5.2 | no |
| | 404 | `M2M_CLIENT_NOT_FOUND` | absent; another tenant's | no |
| `PUT /clients/{clientId}/secret` | 200 | | new secret | stored now |
| | 400 | `BAD_REQUEST` | id outside §5.2 | no |
| | 404 | `M2M_CLIENT_NOT_FOUND` | absent; another tenant's | no |
| | 404 | `FEATURE_DISABLED` | an admin client while `CYODA_IAM_M2M_ADMIN_ROLE_ENABLED` is off | yes |

No gRPC surface manages clients (a search of `internal/grpc` and `api/grpc` for
client-management terms is empty); gRPC only verifies tokens.

### 5.11 Tests

Fixture constraints:
- **In-process cross-node**: two stacks on one database (`newSchedDB`,
  `newStackOn`, `internal/e2e/scheduler_harness_test.go:41,109`). With no node
  copy there is nothing to wait for, so cross-node assertions do not poll. A
  restart is a new stack on the same database.
- **Undecodable records**: a raw KV write on a stack's own database (as
  `internal/e2e/signing_keys_test.go:287`), never the shared server.
- **Multi-node runs on postgres only**; cassandra runs the single-node parity
  list (`cyoda-go-cassandra e2e/cassandra_test.go:91`; its multi-node fixture
  is cyoda-go-cassandra#35).
- The remaining callers of removed methods move to `Create`
  (`internal/e2e/callback_txjoin_errors_test.go:75`,
  `internal/e2e/oauth_keys_test.go:598`,
  `internal/auth/local_validator_integration_test.go:29`,
  `internal/auth/token_test.go`, `internal/domain/account/m2m_adapter_test.go`,
  `internal/auth/store_test.go`).

| Scenario | unit | e2e (postgres, in-process) | parity single-node (memory, sqlite, postgres, cassandra) | multi-node (postgres) |
|---|---|---|---|---|
| create → token; list; reset → old secret 401, new 200; delete → 401 | ✓ | ✓ | ✓ | |
| another tenant: absent from list; delete and reset → 404 with the absent-client body | ✓ | ✓ | ✓ | |
| create on A → token on B at once; reset on A → old secret 401 on B; delete on A → 401 on B | | ✓ | | ✓ shared cluster |
| create, reset, delete each survive a restart | | ✓ | | |
| cap: at the cap → 400 `M2M_CLIENT_CAP_REACHED`; 0 → unbounded; a delete frees a slot | ✓ | ✓ | ✓ | |
| reset of an admin client: flag off → 404 `FEATURE_DISABLED`; flag on → 200; another tenant's admin client → 404 `M2M_CLIENT_NOT_FOUND` | ✓ | ✓ | | |
| token endpoint: id with NUL, invalid UTF-8, 101 characters, an encoded `:` → 401, no store read, one bcrypt | ✓ | ✓ | | |
| unknown id; a record without an index entry; wrong secret: each runs one bcrypt → 401 | ✓ | | | |
| create: index `Put` fails → record removed; list never shows a client that authenticates without being listed | ✓ (faulty KV) | | | |
| undecodable index entry or record: token → 500; list skips with ERROR; delete / reset → 500 | ✓ | ✓ (raw KV write) | | |
| failing KV on every method → store error, never not-found / invalid-client; adapter → 500 / 503; token → 500 | ✓ (faulty KV) | | | |
| a caller's transaction in the context is not joined by the store | ✓ | | | |
| codec: round trip; each decode refusal; the encoder refuses what decode rejects | ✓ | | | |
| no plaintext secret in any stored value | ✓ | ✓ (raw KV read) | | |
| SPI: absent-key `Delete` (KV, message, workflow) and `DeleteBatch` → nil | spitest, every backend | | | |
| every `/clients` and `/oauth/token` response conforms to the OpenAPI (enforce-mode validator) | | ✓ | | |

Waivers:
- 503 on `/clients` is not tested end to end: the shared container cannot be
  paused, and the adapter passes errors through `common.Internal`, whose 503
  mapping is tested (`internal/common/errors.go:169-177`); a unit row asserts
  the adapter reaches it.
- Cassandra multi-node: no fixture yet (cyoda-go-cassandra#35).

## 6. Documentation, comments and exit checks

Every document and code comment outside `docs/superpowers/` (plans, specs and
research are records of their time) and past `CHANGELOG.md` entries is brought
in line. Each PR ends with a fresh-context documentation review against this
section, and with the exit checks below, which return nothing outside the
excluded paths.

### 6.1 Part A

- `cyoda help` (`cmd/cyoda/help/content/`): new `cli/token.md`; `cli.md`;
  `auth.md`; `auth/clients.md:34`; `auth/tokens.md`; `config.md:48`;
  `config/auth.md:140-160,370-376` and its `CYODA_JWT_AUDIENCE` entry (issued
  tokens now carry `aud`); `helm.md:110-138,302,340,477-490`;
  `quickstart.md:87-96`; `run.md`; `errors/KEYPAIR_NOT_FOUND.md` and
  `errors/OIDC_INVALID_TENANT.md` (both mention the bootstrap client or
  tenant). `config_registry.go:100-104`.
- `README.md:78-113` (first real call) and any other mention; `docs/PRD.md:560`;
  `docs/ARCHITECTURE.md:1879-1888,2164-2172`; `docs/cyoda/cloud-divergences.md:49-63`
  (the stub-probe recipe uses `cyoda token`); `docs/cloud-parity/tenant-id-grammar.md`
  and `user-id-rule.md` (they list `CYODA_BOOTSTRAP_*` as an entry point);
  `scripts/dev/README.md`; `scripts/multi-node-docker/README.md`.
- Helm chart: `deploy/helm/cyoda/README.md`, `NOTES.txt`, `values.yaml`
  comments.
- Code comments that name the bootstrap client or its variables, among them
  `internal/auth/validator.go:139` ("the other is CYODA_BOOTSTRAP_TENANT_ID"),
  `internal/common/user_id.go:32,70`, `e2e/parity/multinode/attribution.go:74`,
  and every file the exit check finds.
- `COMPATIBILITY.md` if it names the variables; `CHANGELOG.md` `### Breaking`:
  the six variables and the chart's `bootstrap.*` values are removed; use
  `cyoda token`; `/oauth/token` tokens now carry `aud` when
  `CYODA_JWT_AUDIENCE` is set.
- Issues: #622 closed by Part A; a comment on #624 (the bootstrap client is no
  longer a candidate route to a platform operator); a comment on #286.
- `docs/audits/2026-06-e2e-test-catalog.md` is a dated audit record and is
  left as it is.

Exit checks (repo root, both repos):
```
grep -rnE 'CYODA_BOOTSTRAP_|Bootstrap\.Client|bootstrap (M2M )?client|bootstrap\.(clientId|clientSecret|tenantId|userId|roles)|validateBootstrapConfig|CreateWithSecret|secret-bootstrap' \
  --exclude-dir=docs/superpowers --exclude=CHANGELOG.md .
grep -rn 'testclient\|testsecret\|compute-secret' .   # fixtures
```
Past `CHANGELOG.md` entries are the only allowed hits of the first check.

### 6.2 Part B

- `cyoda help`: `auth/clients.md` (clients stored and shared by the cluster;
  the cap; resetting an admin client needs the flag; the documented races;
  500 / 503 on store failure); `auth/tokens.md` (`/oauth/token` 500 on store
  failure); `config/auth.md` and `config_registry.go`
  (`CYODA_IAM_M2M_CLIENT_MAX_PER_TENANT`; `CYODA_IAM_M2M_ADMIN_ROLE_ENABLED`
  now also gates reset); `errors/M2M_CLIENT_CAP_REACHED.md` (new);
  `errors/FEATURE_DISABLED.md` (reset); `errors.md`.
- `README.md` configuration reference; `DefaultConfig()`.
- `docs/ARCHITECTURE.md:1857` (the component table) and §7.2's store text.
- OpenAPI: 503 on the four `/clients` operations, 400
  `M2M_CLIENT_CAP_REACHED` on create, 404 `FEATURE_DISABLED` on reset;
  `go generate ./api`.
- `docs/cloud-parity/m2m-clients.md` (new) and the README index: the cap and
  its error code, and the reset gate, with a CaaS ticket. Clients being
  shared and persistent is not a contract change (Cloud already behaves so).
- `COMPATIBILITY.md`: the cyoda-go-spi pin; the cassandra plugin must include
  the absent-key `Delete` fix.
- `CHANGELOG.md` `### Breaking`: clients stored and shared; token endpoint
  500 (was 401) on store failure; `/clients` delete and reset 500 / 503 (was
  404) on store failure; reset of an admin client needs the flag. `### Added`:
  the cap.
- Code comments: `internal/auth/store.go` (the duplicated section header and
  the in-memory store's comments go with it), `e2e/parity/multinode/signing_keys.go:56-57`
  ("M2M clients are per node"), and every file the exit check finds.

Exit checks:
```
grep -rnE 'InMemoryM2MClientStore|NewInMemoryM2MClientStore|VerifySecret|clientBelongsToTenant|per node' \
  --exclude-dir=docs/superpowers --exclude=CHANGELOG.md . | grep -i 'm2m\|client'
grep -n 'ErrNotFound' internal/auth/replica.go
```

## 7. Out of scope

- A compare-and-set in the KV SPI (would close the §5.7 races).
- The platform-operator role: #624.
- Cassandra consistency levels: cyoda-go-cassandra#104.
- Cassandra multi-node fixture: cyoda-go-cassandra#35.
- Data migration: there are no production instances.
