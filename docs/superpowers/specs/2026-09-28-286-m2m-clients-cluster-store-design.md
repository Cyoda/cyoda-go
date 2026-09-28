# M2M clients shared by the cluster — design (#286)

Issue: #286. Prerequisite work in the same change: an SPI contract for
deleting an absent key (§5.8), with its cassandra plugin fix.

## 1. Summary

M2M clients are stored in the SYSTEM-tenant KV store. There is no node copy:
every operation, including the token endpoint, reads or writes the store
directly. A client created, deleted or given a new secret on any node takes
effect on every node when the call returns 2xx, and survives a restart on a
persistent backend. The bootstrap client is defined by configuration on every
node; a record in the store pins its identity and holds the state the API gave
it (a reset secret, or deletion), so its revocation is cluster-wide and
survives a restart.

## 2. Terms

- **M2M client**: an OAuth `client_credentials` client, with an id, a bcrypt
  hash of its secret, a tenant, a user id and roles.
- **Bootstrap client**: the client configured by `CYODA_BOOTSTRAP_CLIENT_ID`,
  `_SECRET`, `_TENANT_ID`, `_USER_ID` and `_ROLES` (`app/config.go:333`,
  `app/app.go:404-426`).
- **Ordinary client**: a client created by `POST /clients`.
- **KV store**: the SPI `KeyValueStore` of the SYSTEM tenant, the one
  `AuthConfig.KV` already carries (`internal/auth/service.go:20`,
  `app/app.go:279`). Its interface is `Put`, `Get`, `Delete`, `List`; there is
  no compare-and-set (`cyoda-go-spi persistence.go:545-550`).
- **Identity** of a client: its tenant, user id and roles.

## 3. Requirements

1. A client created, deleted or given a new secret on any node takes effect on
   every node when the call returns 2xx.
2. Clients and every change to them survive a restart on a persistent backend
   (sqlite, postgres, cassandra). The memory backend persists nothing.
3. Deleting the bootstrap client or resetting its secret takes effect on every
   node and survives a restart.
4. A client's identity never changes after it is created, the bootstrap
   client's included.
5. Tenant isolation is enforced by the store: an admin of one tenant can
   neither see nor change another tenant's client, and cannot tell it exists.
6. The token endpoint's timing does not reveal whether a client id exists.
7. No plaintext secret is stored or logged; a configured bootstrap secret is
   held only as its bcrypt hash after startup.
8. A store failure is reported as a store failure (5xx), never as an absent
   client (401 / 404).

## 4. Threat model

| Threat | Addressed by |
|---|---|
| A revoked or re-secreted client keeps working on another node | No node copy: every token request reads the store (§5.4). |
| An admin reaches another tenant's client | Every admin method takes the caller's tenant; another tenant's client is `ErrM2MClientNotFound`, the same as an absent one (§5.3). |
| An admin who reset the bootstrap secret gains a different identity when the operator changes the bootstrap tenant, user or roles | The bootstrap record pins the identity; a node whose configuration differs refuses to start (§5.5). |
| Client-id enumeration through `/oauth/token` timing | Every request that reaches the store runs exactly one bcrypt comparison, against the dummy hash when there is no usable client (§5.4). |
| Read exposure of the store (a backup, a replica) | Secrets are stored only as bcrypt hashes, the conventional persistence shape. |
| Write access to the store | Out of scope, as for the key stores: the same access can change any stored data. |

## 5. Design

### 5.1 Records

Namespace `m2m-clients`, one record per client id; the KV key is the client
id. JSON:

| Field | Ordinary (`kind: "client"`) | Bootstrap (`kind: "bootstrap"`) |
|---|---|---|
| `kind` | `"client"` | `"bootstrap"` |
| `clientId` | = KV key | = KV key |
| `tenantId`, `userId`, `roles` | identity | the identity pinned at first start |
| `hashedSecret` | bcrypt hash, required | bcrypt hash after a reset; empty until then |
| `deleted` | absent | `true` after a delete |
| `createdAt`, `updatedAt` | RFC 3339 UTC | same |

Decoding fails — the record is **undecodable** — when the JSON does not parse,
`kind` is unknown, `clientId` differs from the KV key, `tenantId` fails
`common.ValidateTenantID`, `hashedSecret` is not a bcrypt hash where one is
required or present, or a timestamp is outside `StorableTime`. The encoder
refuses to write any record the decoder would reject.

### 5.2 Client id grammar

`^[A-Za-z0-9_-]{1,100}$`, widened from `^[A-Za-z0-9]{1,100}$`:

- the adapter pattern (`internal/domain/account/m2m_adapter.go:21`);
- the four OpenAPI sites: the `clientId` path parameter of
  `deleteTechnicalUser` and `resetTechnicalUserSecret` (`api/openapi.yaml:761,835`),
  `TechnicalUserCredentialsDto.client_id` (`:9705`) and
  `TechnicalUserDto.clientId` (`:11775`); then `go generate ./api`.

`validateBootstrapConfig` (`app/app.go:1086-1122`) checks
`CYODA_BOOTSTRAP_CLIENT_ID` against the same grammar; an id outside it refuses
to start. `.` is excluded because routers normalise dot segments; `:` and `/`
because they break HTTP Basic credentials and the path. Generated ids
(16-character base32-hex, `m2m_adapter.go` `generateClientID`) are unchanged.

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
`ErrM2MClientExists`. Any other error is the store failing, and carries the
KV error so `common.Internal` can map a storage-unavailable one to 503.

Removed: `InMemoryM2MClientStore`, `CreateWithSecret`, `Get`, `VerifySecret`.
`Get` has no caller once the tenant check is in the store and the token
endpoint uses `Authenticate`.

`KVM2MClientStore` (`internal/auth/kv_m2m_store.go`) implements it:

```go
type BootstrapClientConfig struct {
	ClientID string
	Secret   string
	TenantID spi.TenantID
	UserID   string
	Roles    []string
}

func NewKVM2MClientStore(ctx context.Context, kv spi.KeyValueStore, boot *BootstrapClientConfig) (*KVM2MClientStore, error)
```

`AuthConfig` gains `BootstrapClient *BootstrapClientConfig` (nil: none);
`NewAuthService` builds the store from it and `app.go` no longer creates the
client (`app/app.go:404-426` goes; the "registered" INFO line moves into the
store's constructor).

### 5.4 Operations

A **usable client** for id X is, after one `Get` of X:
- an ordinary record; or
- when X is this node's bootstrap id: the bootstrap record with this node's
  identity and `deleted` false. Its secret hash is the stored one if set, else
  the configured secret's hash. A bootstrap record whose identity differs from
  this node's configuration is not usable, and is logged at ERROR (it can
  exist only if a node with other configuration wrote it; §5.5 refuses such a
  node at startup). An absent record at the bootstrap id — only possible if
  the store was changed outside the API after startup — is the client as
  configured, dated with this store's construction time.

Anything else — absent, a bootstrap record at an id that is not this node's
bootstrap id, a deleted bootstrap record — is **no client**. An undecodable
record is a store failure, logged at ERROR with its KV key.

- **Authenticate(id, secret)**: `Get` id. Store failure or undecodable → that
  error, no bcrypt. Usable client → bcrypt against its hash; mismatch →
  `ErrInvalidClient`. No client → bcrypt against `dummyHash`
  (`internal/auth/store.go:215-221`), then `ErrInvalidClient`. So every request
  that reaches a decision runs one bcrypt comparison.
- **Create(tenant, id, user, roles)**: generate and hash the secret; `Get` id;
  any record, undecodable included, or id = this node's bootstrap id →
  `ErrM2MClientExists`; else `Put` an ordinary record.
- **List(tenant)**: `List` the namespace; ordinary records of `tenant`, plus
  the bootstrap client when it is usable and in `tenant`. Undecodable records
  are skipped and logged at ERROR with their KV keys, as the replica does at
  load (`internal/auth/replica.go:117-122`): one corrupt record does not fail
  every tenant's list.
- **Delete(tenant, id)**: `Get` id; no usable client of `tenant` →
  `ErrM2MClientNotFound`. Ordinary → KV `Delete`. Bootstrap → `Put` the record
  with `deleted: true`, `hashedSecret` cleared, `updatedAt` now.
- **ResetSecret(tenant, id)**: generate and hash the secret first (so the
  window between read and write is one round trip, not a bcrypt); `Get` id; no
  usable client of `tenant` → `ErrM2MClientNotFound`; else `Put` the record
  with the new hash and `updatedAt` now. Returns the client for the response
  body (roles).

The token handler (`internal/auth/token.go:59-80,141`) calls `Authenticate`
once and uses the returned client in both grants: `ErrInvalidClient` → `401
invalid_client`; any other error → `writeTokenServerError` (`token.go:305`,
`500 server_error` with a ticket).

The adapter (`m2m_adapter.go`) drops its `Get`-then-check pre-reads
(`:197-203,238-243`) and `clientBelongsToTenant` (`:109`): `ErrM2MClientNotFound`
→ `404 M2M_CLIENT_NOT_FOUND`; any other error → `common.Internal` (500 with a
ticket, or 503 `STORAGE_UNAVAILABLE`).

### 5.5 Bootstrap client

At construction, with a bootstrap configuration:

1. Hash the configured secret (bcrypt, default cost) and drop the plaintext.
2. `Get` the record at the bootstrap id:
   - store failure or undecodable → error (startup fails);
   - absent → `Put` a bootstrap record with this node's identity, no hash,
     `createdAt` = `updatedAt` = now;
   - an ordinary record → error: "CYODA_BOOTSTRAP_CLIENT_ID names an existing
     client";
   - a bootstrap record whose identity differs (tenant, user id, or roles as a
     set) → error naming the differing fields and saying that changing the
     bootstrap client's identity needs a new `CYODA_BOOTSTRAP_CLIENT_ID`;
   - a bootstrap record with this identity → used. `deleted` → WARN "the
     bootstrap client was deleted through the API; configure another
     CYODA_BOOTSTRAP_CLIENT_ID to have one"; a stored hash → WARN "the
     bootstrap client's secret was reset through the API;
     CYODA_BOOTSTRAP_CLIENT_SECRET is not used".
3. `NewAuthService` returns the error; `app.go` exits as for every other
   auth-service startup failure (`app/app.go:374-378`).

The configured secret may change freely while no reset is stored: it is never
stored. Recovery from a deleted bootstrap client, or a change of its identity,
is a new client id; the old record stays inert.

### 5.6 Consistency and races

The store adds no consistency mechanism of its own. Requirement 1 holds
because every read goes to the store and the store makes a committed write
visible to a read on any node: postgres; cassandra at its default `QUORUM`
reads and writes (`cyoda-go-cassandra internal/config/config.go:201`).
Memory and sqlite are single-node.

Two races are documented, not prevented (the KV SPI has no compare-and-set):

- **Concurrent admin changes to one client on two nodes.** The later write
  wins. A reset racing a delete can leave the client present with the new
  secret, held only by the admin who reset it (same tenant).
- **A node's first start racing a bootstrap delete.** A node that finds no
  bootstrap record while another node deletes the bootstrap client can write
  the record back undeleted. This needs the bootstrap client to be deleted
  within its cluster's first start.

Both need two actions on the same id within milliseconds and are recorded in
the help topic (§9).

### 5.7 Token endpoint load

One KV point read per `POST /oauth/token`, next to ~100 ms of bcrypt. Tokens
live `CYODA_JWT_EXPIRY_SECONDS` (default 3600, `app/config.go:431`); nothing
in cyoda-go calls the endpoint per request.

### 5.8 SPI: deleting an absent key

The SPI does not say what `Delete` of an absent key returns, and `spitest`
does not test it (`cyoda-go-spi spitest/keyvalue.go:48-55`, `message.go:48-66`,
`workflow.go:56-64`). The backends disagree:

- memory, sqlite, postgres: `nil` for KV, message and workflow `Delete`
  (`plugins/*/kv_store.go`, `message_store.go`, `workflow_store.go`);
- cassandra: `spi.ErrNotFound` for all three, through the shared
  `dataStore.delete` (`cyoda-go-cassandra internal/store/data_store.go:170-180,
  274,327,388,393`). Visible today: `DELETE /message` with a batch that
  contains an absent id answers 500 on cassandra and 200 elsewhere
  (`internal/domain/messaging/handler.go:332`), and
  `KVOidcProviderStore.Delete` (`internal/auth/oidc/kv_store.go:104-111`)
  fails on cassandra when its index key is already gone.

Contract: `Delete` (KV, message, workflow) and `DeleteBatch` of an absent key
return `nil`. SPI doc comments state it; `spitest` gains a case per method.
The cassandra plugin's `dataStore.delete` returns `nil` for an absent or
already-deleted key. Mechanics: an SPI PR into `main`, cyoda-go pseudo-pins
`main` (no tag mid-milestone), a cassandra PR on the same pin.
`replica.go:379` keeps tolerating `ErrNotFound` until the pin lands, then the
tolerance is removed (it guards a case the contract rules out).

## 6. Errors

| Endpoint | Status | Code | Cause | New? |
|---|---|---|---|---|
| `POST /oauth/token` | 401 | `invalid_client` | no Basic credentials; unknown id; wrong secret; deleted bootstrap client; bootstrap record of another id or identity | no |
| | 500 | `server_error` | store failure; undecodable record | yes (was 401) |
| | 400 / 500 | | grant validation, signer failure | no |
| all four `/clients` operations | 401 / 403 / 501 | | unauthenticated / not admin / not jwt mode | no |
| | 500 | | store failure; undecodable record (ticket, generic message) | yes (list: new; delete, reset: was 404) |
| | 503 | `STORAGE_UNAVAILABLE` | the store reports itself unavailable | yes; add to OpenAPI for all four |
| `POST /clients` | 200 | | created | stored now |
| | 404 | `FEATURE_DISABLED` | `withAdminRole=true` while disabled | no |
| `GET /clients` | 200 | | caller's tenant's clients, bootstrap client included when usable and in that tenant | stored now |
| `DELETE /clients/{clientId}` | 200 | | deleted (bootstrap: `deleted` stored) | stored now |
| | 400 | `BAD_REQUEST` | id outside §5.2 | ids with `_` / `-` now valid |
| | 404 | `M2M_CLIENT_NOT_FOUND` | absent, another tenant's, deleted bootstrap, bootstrap record of another id or identity | no |
| `PUT /clients/{clientId}/secret` | 200 | | new secret (bootstrap: hash stored) | stored now |
| | 400 / 404 | | as `DELETE` | as `DELETE` |
| startup (jwt mode, bootstrap configured) | exit 1 | | id outside §5.2; ordinary client at the bootstrap id; identity differs from the stored bootstrap record; undecodable record at the bootstrap id; store failure | yes |

No new error code. No gRPC surface manages clients (a search of
`internal/grpc` and `api/grpc` for client-management terms is empty); gRPC
only verifies tokens, through the signing-key store.

## 7. Tests

### 7.1 Fixture constraints

- **In-process cross-node.** Two stacks on one database (`newSchedDB`,
  `newStackOn`, `internal/e2e/scheduler_harness_test.go:41,109`) are two
  nodes: with no node copy there is no gossip to wait for, so every
  cross-node assertion runs without polling. A restart is a new stack on the
  same database.
- **Bootstrap tests use their own database.** The shared TestMain server's
  bootstrap client (`testclient`, `internal/e2e/helpers_test.go:119`) is used by
  every test.
- **Undecodable record** is a raw KV write on a stack's own database (as
  `internal/e2e/signing_keys_test.go:287`), never the shared server.
- **Parity must not touch the fixture's bootstrap client** (`compute-test`,
  `e2e/parity/fixtureutil/fixtureutil.go:492`).
- **Multi-node runs on postgres only**; cassandra runs the single-node parity
  list (`cyoda-go-cassandra e2e/cassandra_test.go:91`), its multi-node fixture
  is cyoda-go-cassandra#35.
- **Callers of the removed methods move to `Create`**, which returns the
  secret: `internal/e2e/callback_txjoin_errors_test.go:75`,
  `internal/e2e/oauth_keys_test.go:598`,
  `internal/auth/local_validator_integration_test.go:29`,
  `internal/auth/token_test.go`, `internal/domain/account/m2m_adapter_test.go`,
  `internal/auth/store_test.go` (M2M part).

### 7.2 Coverage matrix

| Scenario | unit | e2e (postgres, in-process) | parity single-node (memory, sqlite, postgres, cassandra) | multi-node (postgres) |
|---|---|---|---|---|
| create → authenticate; list; reset → old secret invalid, new valid; delete → invalid | ✓ | ✓ | ✓ | |
| another tenant: list excludes, delete / reset → 404, identical body to absent | ✓ | ✓ | ✓ | |
| create on A → token on B at once; reset on A → old secret 401 on B; delete on A → 401 on B | | ✓ | | ✓ shared cluster |
| create, reset, delete each survive a restart | | ✓ | | |
| ids with `_` and `-`: accepted on delete / reset; `.`, `:`, `/`, 101 chars → 400 | ✓ | ✓ | | |
| bootstrap: absent → record written with identity; config secret authenticates | ✓ | ✓ | | |
| bootstrap: reset → config secret 401, new secret 200, on the other stack and after restart; WARN at start | ✓ | ✓ | | |
| bootstrap: delete → 401 and 404 on reset, on the other stack and after restart; WARN at start | ✓ | ✓ | | |
| bootstrap: a changed config secret authenticates while no reset is stored | ✓ | ✓ | | |
| bootstrap: identity differs from the record → constructor error; ordinary client at the id → error; undecodable at the id → error; id outside §5.2 → config error | ✓ | | | |
| bootstrap record of another id: no client on authenticate, list, delete, reset; create refuses the id | ✓ | | | |
| create refuses an existing id, an undecodable record's id and this node's bootstrap id | ✓ | | | |
| undecodable record: authenticate → store error (token 500); list skips it with ERROR; delete / reset → 500 | ✓ | ✓ (raw KV write) | | |
| failing KV on every method → store error, never not-found / invalid-client; adapter → `common.Internal` (500 / 503); token → 500 | ✓ (faulty KV) | | | |
| unknown id and wrong secret both run one bcrypt (dummy hash on the unknown path) | ✓ | | | |
| codec: round trip; each decode refusal; encoder refuses what decode rejects | ✓ | | | |
| no plaintext secret in the stored record | ✓ | ✓ (raw KV read) | | |
| SPI: absent-key `Delete` (KV, message, workflow) and `DeleteBatch` → nil | spitest, all backends | | | |
| OpenAPI conformance of every `/clients` response (enforce-mode validator) | | ✓ | | |

Waivers:
- 503 on `/clients` is not tested end to end: the shared container cannot be
  paused and the adapter passes errors through `common.Internal`, whose 503
  mapping is tested (`internal/common/errors.go:169-177`); the unit row asserts
  the adapter reaches it.
- Startup refusal is tested at the constructor, not end to end: `app.go`
  exits the process, and the exit path is shared with every other auth-service
  startup failure.
- Cassandra multi-node: no fixture yet (cyoda-go-cassandra#35).

## 8. Deletions and exit checks

- `InMemoryM2MClientStore`, `NewInMemoryM2MClientStore`, `CreateWithSecret`,
  `VerifySecret`, the M2M `Get`, `clientBelongsToTenant`: `grep -rn` over the
  repo returns nothing outside `docs/superpowers/`.
- `app/app.go` has no bootstrap-client creation block.
- The note "M2M clients are per node" (`e2e/parity/multinode/signing_keys.go:56-57`)
  is gone, and `newM2MClient` callers no longer need to fetch tokens from the
  creating node.

## 9. Documentation and parity

- `cmd/cyoda/help/content/auth/clients.md`: clients are stored and shared by
  the cluster; the id grammar; the bootstrap client (configuration defines it,
  the API's reset and delete are stored and permanent, identity pinned,
  recovery by a new id); the two races (§5.6); 500 / 503 on store failure.
- `cmd/cyoda/help/content/config/auth.md` and `config_registry.go`:
  `CYODA_BOOTSTRAP_CLIENT_ID` grammar and the identity pin; the secret is not
  used after an API reset.
- `helm.md:110-138`, `quickstart.md:87-96`: "provisioned at startup" → defined
  by configuration, stored state as above.
- `docs/ARCHITECTURE.md:1857` (component table) and `:1879-1888` (bootstrap
  M2M client).
- `README.md`: checked for bootstrap and client wording.
- OpenAPI: the widened pattern (§5.2) and 503 on the four `/clients`
  operations; `go generate ./api`.
- `docs/cloud-parity/m2m-client-id-grammar.md` (new) and the README index:
  the widened grammar. Cloud answers 400 where cyoda-go answers 404 for such
  an id until it widens its pattern; a CaaS ticket asks it to. The bootstrap
  client is cyoda-go only. Clients being shared and persistent is not a
  contract change (Cloud already behaves so).
- `COMPATIBILITY.md`: the cyoda-go-spi pin, and that the cassandra plugin must
  include the absent-key `Delete` fix.
- `CHANGELOG.md` `### Breaking`: clients stored and shared; the bootstrap
  client's API delete is permanent and a reset retires the configured secret;
  a changed bootstrap identity needs a new id; the bootstrap id grammar is
  checked at startup; the token endpoint answers 500, not 401, on a store
  failure; `/clients` delete and reset answer 500 / 503, not 404, on a store
  failure. `### Changed`: the client id grammar allows `_` and `-`.
- Issue #286: a comment recording that the store is read directly, not
  replicated, and why.

## 10. Out of scope

- A compare-and-set in the KV SPI (would close the §5.6 races).
- A per-tenant cap on M2M clients.
- Cassandra multi-node fixture: cyoda-go-cassandra#35.
- Data migration: there are no production instances.
