# Tenant-scoped M2M clients (#650)

## 1. Goal

A client id is unique only inside its tenant. A token request names its tenant
in the URL, a client is found by (tenant, client id), and a tenant may choose
its client ids.

Facts this design rests on, with citations:
`docs/superpowers/research/2026-10-03-650-tenant-scoped-clients-research.md`.

## 2. Terms

- **Client**: an M2M client (technical user). Its record holds its id, tenant,
  roles, on-behalf-of flag, bcrypt secret hash, secret generation and dates.
- **Addressed tenant**: the tenant named by the `{tenant}` path segment of a
  route in the tenant group (§4.2).
- **API tenant**: a tenant id a caller may act in or address. Every id that
  matches the tenant grammar `^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`, except
  `SYSTEM` in any letter case (§4.6).
- **Secret generation** (`cgen`): a number on the client record and on its
  `client_credentials` tokens. A compute-node stream opened with a token stays
  open only while the token's generation equals the record's.
- **Conditional write**: a key-value write that the database applies only if
  the key's current state is the one the caller states (§5).

## 3. Contract changes (all breaking, pre-1.0)

1. The token endpoint moves from `POST {ctx}/oauth/token` to
   `POST {ctx}/tenants/{tenant}/oauth/token` (`{ctx}` = `CYODA_CONTEXT_PATH`,
   default `/api`). The old path is removed. A request to it now falls through
   to the authenticated API and answers as any unknown path does (`401` without
   a bearer token).
2. `POST /clients` takes an optional query parameter `clientId`. A taken id
   answers the new `409 M2M_CLIENT_EXISTS`.
3. The client-id grammar widens to the tenant grammar, minus `system` in any
   letter case (§4.4).
4. `SYSTEM`, in any letter case, is refused as a tenant everywhere a tenant
   enters (§4.6).
5. A secret reset that loses a race with another change to the same client
   answers `409 CONFLICT` (retryable) instead of both resets answering `200`
   (§4.3).
6. Every `401 invalid_client` from the token endpoint is sent no earlier than
   500 ms after the request reached the handler (§4.7).

## 4. Design

### 4.1 Token endpoint

- Registered on the inner mux as `/tenants/{tenant}/oauth/token`, every method,
  before the authenticated catch-all, through the tenant group (§4.2). The
  route lists pinned by `app/route_registration_test.go` and
  `app/route_classification_test.go` change with it. The auth service's own
  public mux registers the same pattern, so `r.PathValue("tenant")` is set on
  the request the token handler sees.
- The handler takes the tenant from the group step, never from the request.
  Everything else is unchanged: Basic authentication only, both grants, every
  check and its order (`internal/auth/token.go:96-148`), the per-client rate
  limit, the error bodies.
- Client lookup is `Authenticate(ctx, tenant, clientID, secret)`. An unknown
  tenant, an unknown client and a wrong secret are the same
  `401 invalid_client`, each after one store read and one bcrypt comparison,
  and each held to the 500 ms floor (§4.7).
- Mock IAM mode: the generated route moves with the OpenAPI path and answers
  `501` for any tenant segment, as today.

### 4.2 The tenant group

Every route under `/tenants/{tenant}/` is registered through one group helper.
Before the route's handler runs, the helper:

1. refuses the request if `r.URL.RawPath` is not empty. Any percent-encoding
   anywhere in the path, in a literal segment as well as in the tenant, is
   refused. Go's mux decodes every segment before matching, so
   `/api/%74enants/acme/oauth/token` otherwise reaches the handler and gets
   past a gateway rule written for the plain path. A valid path never needs
   encoding: the tenant grammar and the literal segments use only unreserved
   characters.
2. reads `r.PathValue("tenant")` verbatim (no case folding, no trimming) and
   refuses it unless it is an API tenant (§4.6).
3. stores the addressed tenant in the request context.

A refusal is `400`, written by an error writer each route passes to the helper,
so each route keeps its own error format: the token endpoint answers
`400 invalid_request` with the fixed description `"invalid tenant"`. The
precedent is the `400` for a malformed `/clients/{clientId}`
(`internal/domain/account/m2m_adapter.go:63`). The refusal is the same for
every tenant id and reads no stored state, so it reveals nothing about any
tenant.

Paths with dot segments or empty segments are redirected by the mux (`307`)
before the group runs. The redirect target is built by the mux from the
request's own path and keeps the method, so it changes nothing.

**Routes that carry a bearer token** are not built now: no planned route needs
one (#645 takes its tenant from the token). The rule is fixed here and in a
comment on the group helper: such a route requires the addressed tenant to
equal the token's tenant, and a mismatch answers `404`, as a missing resource.
A test lists every pattern registered under `/tenants/` and fails when one is
added that is not on its list of token-free routes. That forces whoever adds
the first bearer route to build the rule with its tests.

Data routes, `/.well-known/jwks.json` and `/admin/*` stay outside the group.

### 4.3 The client store

Layout: one namespace per tenant, `m2m-clients:<tenant>`, key = client id,
value = the JSON record. The global index namespace `m2m-client-ids` and the
decoy key are removed. Records written before this change are found at the new
URL unchanged; leftover index rows in development databases are never read.

`decodeClientRecord` keeps binding a record to its key and namespace
(`internal/auth/kv_m2m_codec.go:118-120`). A record read from tenant T's
namespace is T's client, so callers no longer compare tenants.

Operations (`internal/auth/kv_m2m_store.go`):

- **Authenticate(ctx, tenant, id, secret)**. An id outside the grammar burns one
  bcrypt comparison and answers `ErrInvalidClient` without a read, as today.
  Otherwise it does one `Get`. A miss burns one bcrypt comparison; a hit checks
  the secret (verified-secret cache, else bcrypt). A record that does not
  decode is a store failure (`500`).
- **Lookup(ctx, tenant, id)**: one `Get`; absent or outside the grammar →
  `ErrM2MClientNotFound`.
- **Create(ctx, tenant, id, roles, onBehalfOf)**: hash the new secret in a
  secret-check slot (none free → `ErrSecretCheckBusy`, nothing written). Then,
  under the tenant's stripe lock:
  1. `Get` the id: present, decodable or not → `ErrM2MClientExists`.
  2. Cap: `List` the namespace; at the cap → `ErrM2MClientCapReached`.
  3. `PutIfAbsent(rec)`: applied → done; not applied (another node won) →
     `ErrM2MClientExists`; error → undo with `DeleteIfEqual(rec)`, then return
     the error. `rec` contains a fresh bcrypt salt, so it is never equal to
     another create's record. The undo therefore removes only this call's
     write, never a concurrent winner. An undo that fails is logged at `ERROR`.
     The record it leaves holds a secret nobody has: it is listed, blocks the id
     (`409`) and is removed by `DELETE`.

  The stripe lock still makes creates on one node run one at a time. The cap
  can still be exceeded by one client per node (§8).
- **ResetSecret(ctx, tenant, id)**: hash the new secret first, as today. `Get`
  the record bytes `prev`: absent → `ErrM2MClientNotFound`; does not decode →
  store failure. Then `CompareAndPut(prev → next)`, where `next` has the new
  hash and the generation plus one:
  - applied → return the secret;
  - not applied → `Get` again: absent → `ErrM2MClientNotFound`; present →
    `ErrM2MClientChanged` (a concurrent reset or a delete-and-recreate won;
    `409 CONFLICT`, retryable);
  - error → undo with `CompareAndPut(next → prev)`, which restores only if this
    call's write landed and nothing changed since, then return the error. An
    undo that fails is logged at `ERROR`; the stored secret may then be the new
    one, never returned, and the client needs another reset (documented today).
- **Delete(ctx, tenant, id)**: `Get` (absent → `ErrM2MClientNotFound`), then an
  unconditional `Delete`. A delete always wins: a reset racing it fails its
  conditional write and answers `404`, and no undo can bring the client back,
  because every undo is conditional on this call's own bytes.
- **List(ctx, tenant)**: unchanged.

Every branch that handled the index is removed: damaged index entries, index
entries naming another tenant, records without an entry, the decoy read. So is
the matching help text in `auth.clients`.

**Secret generation per incarnation.** A new client's generation starts at a
random integer in `[1, 2^52]`, not at 1. Without this, after "delete `backend`,
recreate `backend`", an unexpired token of the deleted client (`cgen` 1) would
open a compute stream, and that stream would pass every re-check. A random
start makes two incarnations' generations collide with probability 2^-52. The
record codec refuses a generation outside `[1, 2^53)` (the validator's
`genLimit`, `internal/auth/validator.go:241`). Starting at most 2^52 leaves
2^52 resets of headroom.

### 4.4 Client ids

- **Grammar**: `^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`, case significant (every
  backend compares KV keys byte for byte), and not `system` in any letter case.
  A client's id is its user id on its tokens and in audit records
  (`m2m_adapter.go:191` creates with `userID = clientID`), and the user-id rule
  reserves `system` (`internal/common/user_id.go:71`). `auth.ValidClientID`
  applies the whole rule. It is used by the store, by `/clients/{clientId}`, by
  `POST /clients?clientId=`, and by the validator for a `cgen` token's
  `caas_user_id` and an on-behalf-of token's `act.sub`.
- Ids that are valid today stay valid (the old grammar is a subset).
- **Generated ids**: 16 characters of upper-case base32-hex, as today. A
  collision with an existing id in the tenant is retried once; a second
  collision is a `500`, as today.
- Audit records keep the principal kind beside the id
  (`internal/domain/audit/events.go:55-57`), so a client id equal to a user id
  stays distinguishable. Two incarnations of one reused client id are not
  distinguishable in audit; that is the meaning of reusing an id.

### 4.5 `POST /clients`

New optional query parameter `clientId`. Present with any value, including
empty, it must match the client-id grammar (`400 BAD_REQUEST` otherwise).
Absent, an id is generated.

Check order:

1. `403 FORBIDDEN`: not a tenant admin, or an on-behalf-of token.
2. `501 NOT_IMPLEMENTED`: mock IAM mode.
3. `400 BAD_REQUEST`: `clientId` present and outside the grammar.
4. `404 FEATURE_DISABLED`: `withAdminRole=true` while the admin-role flag is
   off.
5. `400 BAD_REQUEST`: `withAdminRole=true` with `onBehalfOf=true`;
   `onBehalfOf=true` in `PLATFORM`.
6. `503 SERVER_BUSY` (`Retry-After: 1`): no secret-check slot for the hash.
7. `409 M2M_CLIENT_EXISTS`: the id exists in the tenant.
8. `400 M2M_CLIENT_CAP_REACHED`: the tenant is at the cap.
9. `409 M2M_CLIENT_EXISTS`: another create of the same id, on any node, wrote
   first.
10. `500 SERVER_ERROR` / `503 STORAGE_UNAVAILABLE`: the store failed.

The `409` body names the id; it is the caller's own tenant, so it reveals
nothing to another tenant.

### 4.6 `SYSTEM` is not an API tenant

`SYSTEM` (`spi.SystemTenantID`) is the machinery's tenant; its KV holds the
cluster's signing keys, trusted keys and client records (`app/app.go:274-281`).
No legitimate caller acts in it. The only minters of `caas_org_id` are the
token endpoint (`internal/auth/token.go:176`, `:282`) and `cyoda token`
(`internal/auth/operator_token.go:80`). The scheduler uses each task's tenant,
and cluster dispatch takes the tenant from peers authenticated by the cluster
key. New `common.ValidateAPITenantID` = `ValidateTenantID` plus "not `SYSTEM` in
any letter case", applied at every door:

| Door | Refusal |
|---|---|
| `caas_org_id` claim (HTTP and gRPC, `internal/auth/validator.go:148`) | `401 UNAUTHORIZED` / gRPC `Unauthenticated` |
| `cyoda token --tenant` (`operator_token.go:32`) | flag error, exit non-zero |
| `{tenant}` path segment (§4.2) | `400` in the route's format |

`ValidateTenantID` itself is unchanged. Stored records and internal contexts
legitimately carry `SYSTEM`. Refusing every letter case matches the user-id rule
and guards a tier that folds case. `internal/e2e/auth_failures_test.go:248`
(which asserts `SYSTEM` tokens are accepted) and the `SYSTEM` case in
`internal/common/tenant_id_test.go` change on purpose.

`PLATFORM` is unchanged: operators get operator tokens at
`/api/tenants/PLATFORM/oauth/token`.

### 4.7 Response-time floor on `401 invalid_client`

With chosen ids and the tenant in the URL, an anonymous caller can guess pairs
such as (`acme`, `backend`). The store's read time can differ between a present
and a missing key: on cassandra a hit is two queries and a miss one
(`../cyoda-go-cassandra/internal/store/data_store.go:125-165`). That difference
would show whether a tenant exists and has the client. No ordering of reads
equalises it on every backend. So every `401 invalid_client` answer of the
token endpoint is held until 500 ms after the handler started. A bcrypt
comparison at the default cost takes about 70 ms, so the floor is several times
the normal path. A request that has already taken longer is answered at once;
past that point the secret-check queue's own variation is far larger than one
read. The wait ends early if the client disconnects. Successful answers are not
held: they need the secret.

### 4.8 Node-local maps

The per-client token rate limit (`internal/auth/client_bucket.go`) and the
verified-secret cache (`internal/auth/secret_check.go`) are keyed by a struct
`{tenant, clientID}` instead of the client id. Otherwise tenant A's `backend`
would share tenant B's `backend` rate limit (one tenant could block another's
token requests) and evict its cache entry.

### 4.9 Compute streams

- The stream check at open and every 60 s (`internal/grpc/streaming.go:79`,
  `:216-235`) calls `Lookup(ctx, streamTenant, clientID)`. The separate tenant
  comparison (`:229`) can no longer fail and is deleted.
- The stream's log lines name the tenant beside the client id
  (`streaming.go:80`, `:203`, `:225`).
- `cmd/compute-test-client` gets the tenant from a new variable
  `CYODA_COMPUTE_TENANT_ID` (required with the client credentials), builds
  `{CYODA_COMPUTE_HTTP_BASE}/api/tenants/{tenant}/oauth/token`, and documents
  it with the other `CYODA_COMPUTE_*` variables
  (`cmd/cyoda/help/content/config/grpc.md`, `cmd/cyoda/help/config_registry.go`).
  The parity fixtures set it.

## 5. SPI: conditional key-value writes

`spi.KeyValueStore` (`cyoda-go-spi/persistence.go:545-552`) gains three
methods:

```go
// PutIfAbsent writes value only if key is absent (never written, or deleted).
// applied=false: key present, nothing written.
PutIfAbsent(ctx context.Context, namespace, key string, value []byte) (applied bool, err error)
// CompareAndPut writes value only if key is present and its stored bytes
// equal expected. applied=false: absent or different, nothing written.
CompareAndPut(ctx context.Context, namespace, key string, expected, value []byte) (applied bool, err error)
// DeleteIfEqual deletes key only if it is present and its stored bytes equal
// expected. applied=false: absent or different, nothing deleted.
DeleteIfEqual(ctx context.Context, namespace, key string, expected []byte) (applied bool, err error)
```

Contract, stated on the interface:

- Each method is atomic against every other write to the key, from any node.
- A non-nil error means the outcome is unknown: the write may or may not have
  been applied. `applied` is meaningful only when the error is nil.
- **No key-value operation joins a transaction**, including the existing four.
  Each one is applied when it returns, whatever transaction the context
  carries. Today postgres joins a transaction found in the context
  (`plugins/postgres/store_factory.go:228-235` uses the transaction-resolving
  querier) while memory and sqlite do not. That is a backend divergence. Only
  the auth stores use the key-value store, and they strip the transaction
  (`noTx`), so nothing observes the change. Postgres moves to the pool querier,
  and the auth stores' `noTx` stripping is removed.

Implementations:

| Backend | `PutIfAbsent` | `CompareAndPut` / `DeleteIfEqual` |
|---|---|---|
| memory | under the store lock | `bytes.Equal` under the store lock |
| sqlite | `INSERT … ON CONFLICT DO NOTHING`, applied = 1 row | `UPDATE`/`DELETE … WHERE … AND value = ?`, applied = 1 row |
| postgres | `INSERT … ON CONFLICT DO NOTHING`, applied = 1 row, on the pool | `UPDATE`/`DELETE … WHERE … AND value = $n`, applied = 1 row, on the pool |
| cassandra | its own design (below) | its own design |

`spitest` conformance cases (`cyoda-go-spi/spitest/keyvalue.go`):

1. Each method on an absent, a present-equal and a present-different key:
   `applied` and the stored state afterwards.
2. `PutIfAbsent` after `Delete` succeeds, and `Get` returns the new value.
3. N concurrent `PutIfAbsent` calls with distinct values: exactly one is
   applied, and `Get` and `List` return that caller's value.
4. N concurrent `CompareAndPut` calls from the same `expected` with distinct
   values: exactly one is applied, and `Get` returns its value.
5. `DeleteIfEqual` with stale bytes leaves the key; with current bytes removes
   it.
6. Tenant isolation: a conditional write in tenant A never sees or changes
   tenant B's key.
7. No transaction join: a write made with a context carrying an open
   transaction is visible outside it at once and survives that transaction's
   rollback, for all seven methods.

The cases run in the in-tree plugins' conformance tests. cyoda-go's auth tests
use KV fakes that embed `spi.KeyValueStore` (e.g.
`internal/auth/kv_m2m_store_test.go:330`, `:413`). The failure-injection tests
for create and reset must override the new methods explicitly, or the injected
fault never fires.

**cassandra.** Its key-value store writes meta, listing and data rows with
`USING TIMESTAMP <HLC>` (`internal/cql/cql.go:149`, `:162`, `:179`). Cassandra
does not accept a client timestamp on a conditional statement, and the data
row's version is `current_version + 1`, computed without coordination
(`internal/store/data_store.go:68-80`). A lightweight transaction on the
existing tables is therefore not a drop-in. The plugin needs its own design
(e.g. a claim table written only by lightweight transactions, or all meta
writes moved to them), driven by the conformance cases above. A dedicated
cassandra issue is filed with the SPI PR. The plugin cannot build against the
new SPI until it implements the methods, so its v0.9.0 dependency bump waits
for it.

**Delivery.** SPI PR into `cyoda-go-spi` `main`. cyoda-go pseudo-pins `main`
in all four `go.mod` files in one commit, with no tag before the release cut
(`MAINTAINING.md`). The in-tree plugins implement the methods in the same
cyoda-go PR.

## 6. Error tables

### 6.1 `POST {ctx}/tenants/{tenant}/oauth/token` (jwt mode)

In check order. OAuth-shaped bodies (`error`, `error_description`), as today.

| Status | `error` / description | When |
|---|---|---|
| `400` | `invalid_request` / `"invalid tenant"` | percent-encoding anywhere in the path; tenant segment not an API tenant (§4.2) |
| `405` | `method_not_allowed` | not `POST` (`Allow: POST`) |
| `400` | `invalid_request` / form media type | `Content-Type` not form-urlencoded |
| `401` | `invalid_client` | no Basic credentials; client id outside the grammar; unknown (tenant, id); wrong secret. Held to the 500 ms floor |
| `503` | `temporarily_unavailable` | store unavailable; no secret-check slot |
| `500` | `server_error [ticket]` | other store failure; a record that does not decode |
| `400` | `invalid_request` / `"malformed request body"` | body over 1 MiB or not a form |
| `400` | `unsupported_grant_type` | missing or unknown grant |
| `400` | `unauthorized_client` | wrong grant for the client kind |
| `429` | `slow_down` | per-(tenant, client) rate limit on this node |
| … | token-exchange rows | unchanged (`cyoda help auth tokens`) |

Mock IAM mode: `501 NOT_IMPLEMENTED` (problem detail), any tenant segment.
Old path `{ctx}/oauth/token`: no route; answers as an unknown path.

### 6.2 `/clients`

`POST /clients`: §4.5. `DELETE /clients/{clientId}` and
`PUT /clients/{clientId}/secret`: as today, plus the widened grammar on
`{clientId}`, minus every index-damage case. Reset gains
`409 CONFLICT` (retryable) for a lost race (§4.3).

### 6.3 Token doors (`SYSTEM`)

| Door | Answer |
|---|---|
| HTTP bearer with `caas_org_id` = `SYSTEM` (any case) | `401 UNAUTHORIZED` |
| gRPC call / stream with such a token | `Unauthenticated` |
| `cyoda token --tenant SYSTEM` | error, non-zero exit |

## 7. Coverage matrix

| Scenario | Unit | e2e (postgres) | Parity (all backends) | gRPC |
|---|---|---|---|---|
| Token at `/tenants/{t}/oauth/token`, both grants | ✓ | ✓ | ✓ | — |
| Same client id in two tenants: each token carries its own tenant; A's secret at B's URL → `401` | ✓ | ✓ | ✓ | — |
| Unknown tenant / unknown client / wrong secret → same `401`, floor held | ✓ | ✓ | — | — |
| Percent-encoded literal segment, encoded tenant, `%2F` in tenant, `SYSTEM`, `system`, grammar violation → `400 invalid tenant` | ✓ | ✓ | — | — |
| Tenant ids equal to route words (`clients`, `oauth`, `model`, `tenants`) work | ✓ | ✓ | — | — |
| Old `/oauth/token` is gone | ✓ | ✓ | — | — |
| Mock mode → `501` | ✓ | — | — | — |
| `POST /clients?clientId=` happy path; generated id when absent | ✓ | ✓ | ✓ | — |
| `clientId` grammar / empty / `SYSTEM` → `400` | ✓ | ✓ | — | — |
| Taken id → `409 M2M_CLIENT_EXISTS`; `409` before cap | ✓ | ✓ | ✓ | — |
| Two concurrent creates of one id on two nodes → one `200` with a working secret, one `409` | — | ✓ (isolated multi-node) | — | — |
| Two concurrent resets → one `200`, one `409 CONFLICT`; the winner's secret works | ✓ | ✓ (isolated) | — | — |
| Reset racing delete → client stays deleted | ✓ | — | — | — |
| Create/reset failure undo touches only its own write (injected faults) | ✓ | — | — | — |
| Delete + recreate same id → old token cannot open a stream; open streams of the old client close | ✓ | ✓ | — | ✓ |
| `SYSTEM` (any case) `caas_org_id` refused | ✓ | ✓ | — | ✓ |
| `cyoda token --tenant SYSTEM` refused | ✓ | — | — | — |
| Rate limit and secret cache keyed by (tenant, id) | ✓ | — | — | — |
| Group registry test: no bearer route under `/tenants/` | ✓ | — | — | — |
| SPI conditional writes (§5 cases 1–7) | spitest on memory, sqlite, postgres | — | — | — |

The isolated concurrency tests stay out of the parity suite
(`.claude/rules/test-coverage.md`).

## 8. What does not change

- Data routes take the tenant from the token. `/clients` acts in the caller's
  tenant. JWKS and `/admin` are outside the group.
- The cap is per node: concurrent creates on several nodes can exceed it by one
  client per node (documented today). Making it exact needs a counter under a
  conditional write; out of scope.
- An issued token stays valid until its `exp` after its client is deleted, as
  today.

## 9. Documentation and contract

- **OpenAPI** (`api/openapi.yaml`): the path with a `tenant` path parameter
  (tenant grammar pattern) and the `400 invalid tenant` description. `POST
  /clients` gains `clientId` and `409`; reset gains `409`. The four client-id
  patterns change. Regenerate `api/generated.go`, and add oasdiff ignore entries
  (`.github/oasdiff-err-ignore.txt`).
- **New error code** `M2M_CLIENT_EXISTS`, with its help topic and an entry in
  the error index.
- **Help**: `auth`, `auth.clients` (chosen ids, `409`, reset race, storage
  section without the index), `auth.tokens` (URL, floor), `auth.integration`,
  `auth.trusted-keys`, `cli.token`, `config.auth` (tenant ids are public
  identifiers that appear in URLs and gateway logs: no personal data or secrets
  in them; `SYSTEM` refused), `config.grpc` (`CYODA_COMPUTE_TENANT_ID`),
  `grpc`, `helm` (ingress rate limit on `^/api/tenants/[^/]+/oauth/token$`),
  `errors.NOT_IMPLEMENTED`, `errors.SERVER_BUSY`, `errors.CONFLICT`.
- `README.md`, `CHANGELOG.md` (`### Breaking`: URL, `SYSTEM`, `409`s, grammar),
  `docs/access-to-the-cyoda-api.html`, `docs/ARCHITECTURE.md`,
  `docs/FEATURES.md`, `docs/PRD.md`, `docs/CONCURRENCY.md`,
  `deploy/helm/cyoda/docs/gateway-api-policies.md`, `COMPATIBILITY.md` (SPI pin).
- **Cloud parity** (Gate 7): `docs/cloud-parity/tenant-scoped-clients.md`, plus
  updates to `m2m-clients.md`, `platform-operator.md`, `signing-key-pairs.md`,
  `trusted-key-tenant.md` and `user-id-rule.md` where they name the URL. CaaS
  ticket:
  - Cloud looks a client up by a global user name, in the same table as human
    users (`TechnicalUserService.kt:249`). Tenant-scoped, caller-chosen ids
    are a data-model change there.
  - Token URL, chosen ids, `409`, the response-time floor.
  - `SYSTEM` refused: on Cloud the SYSTEM legal entity skips entitlement
    checks, so the refusal matters more there.
  - Two Cloud defects: a wrong secret answers `400` and an unknown client `401`
    (an existence oracle); ids and secrets come from `Random.Default`
    (`AccountUtils.kt:14`).
- `test/recon/oauth.go` targets Cloud and keeps Cloud's URL until Cloud moves.

## 10. Out of scope

- The bearer half of the tenant group (§4.2).
- An exact cross-node cap.
- Cloud's implementation (CaaS ticket).
