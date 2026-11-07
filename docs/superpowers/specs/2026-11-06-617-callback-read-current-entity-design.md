# A callback read returns the current entity; one version per entity per transaction (#617)

## 1. Intent

A processor, function criterion or schedule function runs on a compute node and
can call back into cyoda with the transaction token. A read of the entity the
workflow runs for (the **anchor**) must return the anchor as the workflow
currently has it: with the data the request sent, the state changes so far and
the changes earlier processors made. On a create it must be found.

Today the engine keeps the anchor in memory across the cascade and saves it to
the transaction only at the end (`internal/domain/entity/service.go` create
save at ~:348, update save at ~:2344) and before a `COMMIT_BEFORE_DISPATCH`
processor. A callback read returns the last saved copy: the pre-request entity
on update, not found on create.

The fix saves the anchor into the transaction before each callout that carries
the transaction token. So that this does not fill the entity's history with
half-finished versions, storage records **at most one version of each entity
per transaction**: a later write of an entity the transaction already wrote
replaces that transaction's pending version.

### Goals

- Every read under the transaction token (get, search, count, page) sees the
  anchor as it is when the callout is made, on every backend, over HTTP and
  gRPC callbacks.
- One committed version per entity per transaction, on memory, sqlite and
  postgres. `GET /entity/{id}?transactionId=X` returns the entity as X left it,
  which is what the parameter's documentation already promises.
- Every backend enforces unique keys on the transaction's final entity, at
  commit.

### Depends on #702

#702 (a failed joined write request makes the owner's transaction
rollback-only, on every backend) lands first. With §2's saves, a joined write
request that fails after one of its processors has run has already stored its
own entity, half-way through its workflow, in the owner's transaction; without
#702 the owner can commit that. This spec assumes #702's rule and does not
restate it.

### Non-goals

- The cassandra plugin (out of tree). An issue is filed in
  `cyoda-go-cassandra` describing the contract change.
- Cloud alignment beyond the `docs/cloud-parity/` entry Gate 7 requires.
- Any change to what a callout's request payload carries. It already carries
  the current anchor.

### Anti-goals

- A version in history, or a `?transactionId` answer, that shows a state the
  transaction did not end with.
- A workflow whose outcome depends on how many callouts sit between two of its
  steps (e.g. unique-key failure on an intermediate state on one backend only).
- A backend that answers differently from the others on any case in §3.

## 2. Engine: save before each callout that carries the transaction token

### 2.1 Where

Immediately before each of these dispatches, on write paths only, the engine
makes the transaction hold the current anchor (§2.2). "Write path" means both:
the dispatch carries a transaction id (`cc.txID != ""`) **and** a transaction
is in the context (`spi.GetTransaction(ctx) != nil`). Either alone is not
enough: `resolveAuditTxID` mints an id when the entity has none
(`engine.go` ~:1006-1011) and a pass is minted from the id alone
(`internal/grpc/dispatch.go` ~:90), so with an id and no transaction a save
would be an auto-commit write; the read path passes no id but its context can
carry a joined transaction.

| Callout | Dispatch site | Notes |
|---|---|---|
| `SYNC` / `ASYNC_SAME_TX` processor | `internal/domain/workflow/engine_processors.go` (`executeSyncProcessor`, dispatch at ~:270) | |
| `ASYNC_NEW_TX` processor | `engine_processors.go` (savepoint at ~:344, dispatch at ~:364) | Save **before** `Savepoint()`, so a processor failure does not roll back the save. |
| Function criterion — workflow selection, manual transition, automated cascade transition | `criterion_eval.go` (dispatch at ~:179), reached from `engine.go` ~:550, ~:757, ~:880 | On create, workflow selection runs before the anchor has a state; the save stores it with an empty state and a callback read sees it so. |
| Schedule function | `arm.go` (dispatch at ~:275) | Runs from `reconcileScheduledTasks` after the cascade, before the final save. |

Not covered, because they need no save:

- `COMMIT_BEFORE_DISPATCH`: TX_pre already flushes and commits the anchor
  before the dispatch. With `startNewTxOnDispatch: true`, TX_post reads TX_pre's
  committed version (`requireAnchorCommittedBy`), which is the in-memory anchor.
  With `false`, the dispatch carries no token.
- `resolveWorkflowForQuery` (`engine.go` ~:622): the read path that lists an
  entity's available transitions. It shares `selectWorkflow` (`engine.go` ~:550)
  with the write paths but passes an empty transaction id, and its context can
  still carry a joined transaction. It must not save; the save is keyed on the
  dispatch's transaction id, not on a transaction being present in context.
- A dispatch with no transaction in context (no token is sent).

### 2.2 When

Before each dispatch of §2.1 the engine reads the anchor inside the transaction
(`readAnchor`) and compares it with its in-memory anchor: the `Data` bytes and
`Meta.State`. If either differs it saves the in-memory anchor; otherwise it
saves nothing. If the read finds no anchor, it saves only on a create that has
not yet stored the anchor. On any other path "not found" means a callback
deleted the anchor in this transaction; the engine saves nothing, so the
callout and its callbacks see the delete, and the outcome of that delete is
left to the code that already handles it (a scheduled fire's "deleted in this
transaction" outcome, `fire_scheduled.go` ~:299-312; a client door's final
save).

- The comparison is against a fresh read, never against a record the engine
  keeps of what it last saved. Such a record goes stale whenever a callback
  writes the anchor during a dispatch the engine does not read back after (a
  processor that returned data, an `ASYNC_NEW_TX` processor, a function
  criterion, a schedule function, a callback that runs a nested transition on
  the anchor).
- A false "differs" only costs a redundant save, which one version per
  transaction (§3) makes invisible in history. postgres and sqlite return
  re-encoded bytes, so after a save there the next comparison usually differs
  and saves again; that is accepted. A false "equal" cannot happen: equal bytes
  and equal state are the same content.
- Saving nothing when nothing changed is required, not an optimisation. A
  scheduled fire runs function criteria (workflow selection,
  `fire_scheduled.go` ~:224; the transition criterion through `fireTransition`
  → `engine.go` ~:757) and then may decline or cancel and still **commit** its
  task removal and audit event (~:229-239, ~:268-273). A save of an unchanged
  anchor there would commit a version that changes nothing, with a new
  transaction id that fails every client's stored `If-Match`.
- Meta-only changes (the scheduled fire's attribution stamp, `fire_scheduled.go`
  ~:241-244) do not count; they reach storage with the next content save.

### 2.3 How

- The read and the save use the dispatch's transaction context. The save is a
  plain `EntityStore.Save` of the in-memory anchor, with the attribution the
  request already set on the anchor's meta
  (`service.go` ~:282-295 create, ~:2280-2296 update; a scheduled fire keeps
  the stored meta and sets change user and executor, `fire_scheduled.go`
  ~:241-244).
- **Gate.** The save runs before `txgate.Suspend` releases the chain's gate for
  the dispatch, and never acquires the gate itself. A joined chain already holds
  it for its whole handler (`internal/domain/txjoin/txjoin.go` ~:298, :318) and
  the gate is not re-entrant. The owner's chain needs none: no joined callback
  can act on the transaction between two callouts, because ending a callout
  refuses all its passes from then on and waits for a joined write still in
  progress (`internal/fence/fence.go` `end`, ~:121-140; called on every exit of
  `internal/callout/coordinator.go` ~:161). The gate the owner takes around its
  final save and commit is not changed by this work.
- **Errors.** A failure of this read or save is marked with an engine infrastructure
  sentinel, as `readAnchor` failures are (`errors.Join(ErrProcessorOutputInfra,
  err)` at `engine_processors.go` ~:247-250, or a new sentinel of the same family
  named for this save and added to `engineConflictIsTransactionConflict`,
  `service.go` ~:2819-2826). So a conflict from the save is a transaction
  conflict (retryable `409 CONFLICT`), never `412 ENTITY_MODIFIED`, and the
  collection update loop (`service.go` ~:2635-2644) never isolates it as an
  item failure and carries on in a failed transaction. A storage outage maps to
  `503`, any other failure to `500` with a ticket, never a `400
  WORKFLOW_FAILED` carrying driver text. Unique-key errors cannot arise from
  this save (§4).

### 2.4 Detecting a callback's write

`adoptCallbackWrite` (`anchor.go`) keeps a callback's write to the anchor when a
`SYNC`/`ASYNC_SAME_TX` processor returned no data; that is unchanged in scope
(no other dispatch adopts a callback's write today, and none does after). Both
sides of the comparison stay **store reads**: the "before" read
(`readAnchor`, `engine_processors.go` ~:249) now runs after the pre-dispatch
save, and the "after" read after the dispatch, as today. Comparing a store read
with the engine's own bytes is not allowed: postgres (JSONB, `_meta` merged
into `doc`) and sqlite (`jsonb(?)`) return different bytes from the ones saved.
The rule (`adoptCallbackWrite`) is unchanged. A write that leaves the payload byte-identical cannot be told from no
write, as today.

### 2.5 What a callback sees

- Under the token: the anchor as saved just before the dispatch, which is the
  anchor at that moment. Its state is the transition's source state while that
  transition's processors run (the state advances after processors,
  `fireTransition`); its data includes earlier processors' changes.
- **The payload's `meta.transactionId` equals the `transactionId` a callback
  read of the anchor under the token returns at dispatch.** After the §2.2 step
  the engine sets `entity.Meta.TransactionID` to the transaction id of the
  stored anchor: the dispatch's transaction id after a save, the id the read
  returned otherwise. The same assignment follows every other engine write of
  the anchor: the `COMMIT_BEFORE_DISPATCH` TX_pre flush
  (`flushAndCommitSegment`, `engine_processors.go` ~:596) and the post-dispatch
  apply `CompareAndSave` (~:548). Nothing assigns it today: the payload takes
  `entity.Meta.TransactionID` (`internal/grpc/dispatch.go` ~:127), stores copy
  the entity and write nothing back, and the update path sets it to the
  current transaction id up front (`service.go` ~:2286) while the stored anchor
  still carries the previous commit's id, so a callback's `If-Match` with the
  payload's id fails `412` today; a scheduled fire sends the committed id
  (`fire_scheduled.go` ~:248). After this work a callback `If-Match` with the
  payload's id matches on every path, `COMMIT_BEFORE_DISPATCH` with
  `startNewTxOnDispatch: true` included (the payload carries the id TX_post
  reads). The builder audits every engine reader of `entity.Meta.TransactionID`
  for a dependency on the old value.
- **Scheduled fire precondition.** The fire's `expectedTxID` (the precondition
  of its final persist, `fire_scheduled.go` ~:248) and the attribution stamp
  (~:241-244) move before `resolveWorkflow` (~:224), so they are taken from the
  entity as first read, before any callout. The `expectedTxID == ""` refusal
  stays where it is (its audit event names the selected workflow) and tests
  the captured value.

## 3. Storage: one version per entity per transaction (SPI contract)

### 3.1 The rule

Within one transaction, an entity has at most one pending version. A `Save`,
`CompareAndSave` or `Delete` of an entity the transaction already wrote
replaces that pending version; it does not add one. `SaveAll` and `DeleteAll`
follow the same rule per entity. At commit the transaction records that one
version. Outside a transaction (auto-commit writes) every
write is its own transaction and its own version, as today.

The single version carries:

| Field | Value |
|---|---|
| Content | The transaction's last write of the entity. |
| Change type | `DELETED` if the last write is a delete. Otherwise `CREATED` if this is the entity's first version, else `UPDATED`. (A re-create after a delete an earlier transaction committed stays `UPDATED`, as today on every backend.) |
| Version number | The entity's previous version number + 1 (one number per transaction). |
| Creation date, last-modified date, transaction id, submit time | As today for a single save in the transaction. |
| Attribution (user, attributed kind, executor) | The last write's. |

Cases, all backends:

| Sequence in one transaction | Recorded versions |
|---|---|
| save, save (… save) | 1 (last save) |
| save, CompareAndSave (naming the tx's own id) | 1 (the CAS) |
| delete, save (entity existed before) | 1, `UPDATED` |
| save, delete (entity existed before) | 1, `DELETED` |
| create, delete (entity did not exist before) | 1, `DELETED` (today's memory and sqlite behaviour; the spitest model-immutability case at `spitest/entity.go` ~:359 needs this tombstone) |
| create, save | 1, `CREATED` |

**Savepoints.** `RollbackToSavepoint` restores each entity's pending version to
what it was at the savepoint (including "no pending version").

**Read-your-own-writes** is unchanged: reads in the transaction return the
pending version.

### 3.2 `GetVersionByTransaction`

It returns **the** version transaction `txID` wrote (there is at most one), and
it reads committed versions only, also when called inside a transaction —
including inside `txID` itself. Before `txID` commits it is `ErrNotFound` for
every caller. Tombstones never match and an empty `txID` never matches, as
today.

Today postgres runs this read on the caller's transaction connection
(`plugins/postgres/entity_store.go` ~:1192-1210, `s.q`) and returns `txID`'s
first uncommitted save to a caller inside `txID`; memory (`txIndex`, built at
commit) and sqlite (`readDB`) return not found. postgres moves the read off the
transaction, the way `GetAsAt` and `GetVersionMetadata` already are.

### 3.3 Per backend

**memory and sqlite.** Both already keep only the latest copy in the
transaction buffer. Delete `supersededSaves`, its savepoint length record and
its cleanup; commit flushes one row per buffered entity. The earliest-wins
`txIndex` bookkeeping reduces to the single version. The existing defect that a
`save, save, delete, save` sequence flushes the stale first save goes away with
it.

**postgres.** Only when there is an ambient transaction (a non-transactional
`saveOn` writes `transaction_id = ''`, `entity_store.go` ~:228-232, and must
keep adding a version), `saveOn` and `deleteOn` find, in SQL, whether this
transaction already wrote the entity's current pending version (the `entity_versions` row at
`entities.version` whose `transaction_id` is this transaction). When it did:

- update that row in place (doc, model columns, change type per §3.1, wall-clock
  time; tombstone flag for a delete) and do not increment `entities.version`;
- update `entities` (doc, deleted flag) without a version increment;
- derive the change type from whether any version **before** this transaction
  exists, not from the upsert's `xmax`.

The pending-row lookup is done in SQL, not tracked in Go: `txState` savepoints
snapshot only the read and write sets (`txstate.go` ~:235-262), so a Go-side
record would go stale after `ROLLBACK TO SAVEPOINT`. Commit stamping
(`stampCommitInstant`, keyed on `transaction_id`) and creation-date stamping
(keyed on `version = 1` among the transaction's rows,
`transaction_manager.go` ~:441-449) keep working because the version number is
kept. The read-set validation is unaffected (an entity the transaction wrote is
not re-recorded on read; `txstate.go` ~:105-176).

Delete-then-recreate in one transaction becomes one version on postgres too;
the documented difference in `docs/CONSISTENCY.md` §6 is removed.

The `KNOWN ASYMMETRY` comment in `plugins/postgres/entity_store.go` ~:194-218
is wrong (`saveOn` copies the entity first, ~:177-183, so nothing is written
back to the caller) and is corrected.

### 3.4 Conformance (spitest)

- Replace `GetVersionByTransaction/EarliestWins` with a case asserting the last
  save's payload and exactly one version (via `GetVersionMetadata`) for a
  same-tx double save.
- New cases: each row of the §3.1 table (version count, change type, content);
  savepoint rollback restoring a replaced pending version and a removed one;
  `GetVersionByTransaction` inside the writing transaction is `ErrNotFound`.
- Each new case must be shown failing against the current backend code before
  the fix (driving the kernel directly in each plugin, since spitest has no
  harness in the SPI repo).

## 4. Unique keys: computed at commit, from the final entities, on every backend

Today the backends disagree:

- postgres writes claims at each save (`plugins/postgres/unique_claims.go`
  `replaceClaims`) against the immediate unique index `unique_claims_uq`.
- memory checks at commit on the final buffered entities, but ignores a
  committed claim only when its holder is in `tx.Deletes`
  (`plugins/memory/txmanager.go` ~:970-975), so a holder this transaction
  re-keys still blocks the value.
- sqlite runs `replaceClaims` per entity at commit, in map order, against an
  immediate unique index (`plugins/sqlite/txmanager.go` ~:1121-1137).

So a swap of key values between two entities in one transaction, or a chain of
moves, is refused on memory, refused or accepted by map order on sqlite, and
refused on postgres unless the order of saves happens to free first. The help
documents a "free before claim" rule (`cmd/cyoda/help/content/models.md`
"Transactional ordering", `errors/UNIQUE_VIOLATION.md`), and `models.md` claims
re-keying works on commit-time backends, which is false today. With §2's saves,
postgres would also check every intermediate state.

### 4.1 The rule

At commit, a transaction's claims are computed from its final entities:

1. Release the claims of every entity the transaction wrote or deleted.
2. Compute the claims of every entity the transaction wrote, from its final
   data, with the model's unique keys. A partial composite key fails the commit
   with `spi.ErrPartialUniqueKey`.
3. Insert those claims. A value claimed twice inside the transaction, or held
   by an entity outside the transaction's writes (committed, or claimed by a
   concurrent transaction that commits first), fails the commit with
   `spi.ErrUniqueViolation`.

A save inside a transaction does no claim work beyond recording the entity's
model keys, taken from the write's context (`spi.UniqueKeysFromContext`), last
write wins, as memory's `recordUniqueKeys` already does. Because step 1
releases unconditionally, **every write of an entity whose model declares keys
must carry them**; a write without them would drop the entity's claims. The
builder lists every engine and service write site (the scheduled fire is the
only one without keys today, §4.3) and a test per door asserts the claims
survive a write through it. A write outside a transaction keeps its current
claim path (postgres and sqlite `replaceClaims` in the statement; memory
`saveUnlocked`); it is its own transaction, so its claims are its final ones.

Swaps, chains of moves and free-after-claim all commit when the final state is
unique. The request-body pre-check (`service.go` `withUniqueKeys`: a body with a
partial key is `422` before the transaction begins) stays.

### 4.2 Per backend

- **memory:** in commit, the release set is every entity in `tx.Buffer` and
  `tx.Deletes`; a committed claim held by an entity in that set does not
  conflict, and the intra-batch check (`pending`) catches a value two buffered
  entities claim. The apply step releases the claims of **every** entity in the
  release set first and only then inserts all new claims. Releasing and
  inserting one entity at a time (`txmanager.go` ~:1110-1117) loses a claim on
  a swap: A x→y then B y→x inserts `y→A`, and `releaseClaims(B)`
  (`store_factory.go` ~:311-317) deletes key `y`.
- **sqlite:** in the commit flush, release the claims of every buffered and
  deleted entity first, then insert all new claims.
- **postgres:** keep the immediate index. Transactional saves stop calling
  `replaceClaims`; they record the entity id and its model keys in the
  transaction's state. At commit, after read-set validation and **before**
  `stampCommitInstant` (its rule: nothing after `cyoda_stamp` waits on a lock,
  `transaction_manager.go` ~:417-420, relied on by `ConsistencyTime`,
  `consistency_time.go` ~:186-191), run steps 1-3 as `DELETE` and `INSERT`
  statements, inserting in a deterministic order (by model, key id,
  signature). The recorded set must be right after `ROLLBACK TO SAVEPOINT`:
  either snapshot it with the savepoint, or at commit keep only the ids whose
  `entities` row this transaction wrote. The claim release in `deleteOn`
  (`entity_store.go` ~:878) **moves to commit** (step 1), and the model-wide
  claim `DELETE` in `DeleteAll` (~:935) is removed inside a transaction: its
  per-entity deletes (~:927-931) already record every id, and a model-wide
  release at commit would drop the claims of entities a rolled-back savepoint
  restored. Kept mid-transaction, these releases would let another
  transaction's commit wait on this one's uncommitted release and succeed if it
  commits, where memory and sqlite refuse at once because the committed holder
  still holds the value.

  Concurrency on postgres: a transaction whose commit inserts a value that
  another, uncommitted transaction has just inserted at its own commit waits
  for it (this wait is before its stamp), then gets `ErrUniqueViolation` if the
  other committed or proceeds if it rolled back. Two commits inserting each
  other's values in opposite orders meet PostgreSQL's deadlock detection
  (`40P01`), classified as a retryable `spi.ErrConflict` (`409 CONFLICT`)
  where memory and sqlite answer `409 UNIQUE_VIOLATION`; the deterministic
  insert order makes that rare. This is an accepted, transient difference: both
  answers fail closed, the `409 CONFLICT` is retryable, and the retry gets the
  same `UNIQUE_VIOLATION` the other backends gave.

Commit errors already map to the right answers: `common.Internal` turns
`ErrUniqueViolation` into `409 UNIQUE_VIOLATION` and `ErrPartialUniqueKey` into
`422 INVALID_UNIQUE_KEY` (`internal/common/errors.go` ~:277-282); postgres
classifies a 23505 on `unique_claims_uq` by constraint name
(`transaction_manager.go` ~:1031).

### 4.3 Scheduled fires carry the model's unique keys

`fireScheduled` begins its transaction without `spi.WithUniqueKeys`
(`fire_scheduled.go` ~:145; the only callers are in `service.go` ~:195, :2065,
:2560). memory then releases the entity's claims and inserts none
(`txmanager.go` ~:958-963, ~:1114-1117); postgres and sqlite skip claim work and
keep stale claims. The fire attaches the anchor model's keys, as the entity
service does. A fire whose final entity breaks a key then fails at commit,
the entity unchanged. That is a failed run like any other
(`internal/scheduler/bookkeeping.go` `decideBookkeeping`): the task ends
`FAILED` at once if unsafe work was reached, otherwise the attempt is recorded
and retried with backoff, ending `FAILED` (`FailureExpiredAfterFailedAttempts`)
once its `timeoutMs` has passed; the error text is recorded on the task. A task
without `timeoutMs` (it is optional) is retried at the maximum retry delay until
something changes, as any other deterministic failure of a run is today. A test
asserts this outcome.

### 4.4 Tests and docs this changes

- The `SameTxReclaim*` tests in the three plugins' `unique_claims_test.go` are
  rewritten to assert that both orders commit.
- New per-plugin and parity cases: two entities swap key values in one
  transaction; a chain A→B's value, B→C's value; free-after-claim; a value
  claimed by two entities of one transaction (refused).
- "Transactional ordering" in `models.md` and the free-before-claim sentence in
  `UNIQUE_VIOLATION.md` are replaced by: keys are checked on the transaction's
  final entities at commit.

## 5. Behaviour visible to clients

| Area | Before | After |
|---|---|---|
| Callback read of the anchor under the token | last saved copy; not found on create | current anchor |
| Callback search/count/page under the token | anchor at its last saved copy | anchor as current |
| `/entity/{id}/changes`, audit entity-change history | possibly several versions per transaction | one per transaction |
| `GET /entity/{id}?transactionId=X`, X committed | X's first save | X's final entity |
| `GET /entity/{id}?transactionId=X`, X not committed, caller outside X | `404 ENTITY_NOT_FOUND` | unchanged |
| same, caller inside X (callback with X's token) | postgres: X's first uncommitted save; memory/sqlite: 404 | `404 ENTITY_NOT_FOUND` everywhere |
| Unique key violated only by an intermediate state | commits, except on postgres when that state was saved (a callback's write, a `COMMIT_BEFORE_DISPATCH` TX_post write, a claim made before its holder is freed): `409`/`422` | commits everywhere |
| Two entities swap key values, or a chain of moves, in one transaction | memory: `409`; sqlite: `409` or commits by map order; postgres: `409` unless saves free first | commits everywhere |
| Unique key violated by the final entity | `409 UNIQUE_VIOLATION` (`422` partial) | unchanged codes; postgres reports at commit |
| A joined callback's write that duplicates a key or leaves a partial key (postgres) | the callback request gets `409`/`422` | the callback request succeeds; the owner's commit fails `409`/`422` (as memory/sqlite today) |
| A scheduled fire that changes a key field | memory drops the entity's claims; postgres/sqlite keep stale claims | claims follow the final entity; a fire that breaks a key fails (§4.3) |
| A callback's `If-Match` with the payload's `meta.transactionId` on the anchor | update path: `412`; scheduled fire: matches | matches on every path (§2.5) |
| A scheduled fire that declines or is cancelled after running a function criterion | no entity version | unchanged: no entity version (§2.2) |
| Reading an entity's available transitions under a transaction token (function criteria run) | no write | unchanged: no write (§2.1) |
| A processor writes its own anchor **without** the token (a standalone transaction) | the cascade fails `409` (memory/sqlite at commit; postgres at its final save) | memory/sqlite: unchanged. postgres: the stray write waits on the anchor's row lock, which the cascade holds from its first pre-callout save; the callout times out (its callout failure, e.g. `503 DISPATCH_TIMEOUT`), the cascade rolls back, then the stray write commits. Fails closed on all backends. Documented as: a processor writes the entity it runs for only with the token. |
| Another client writes the anchor during a callout (postgres) | client commits; cascade fails `409` | client waits until the cascade ends; one side wins, the other gets `409`. A wait longer than the statement timeout (`CYODA_POSTGRES_STATEMENT_TIMEOUT`, default 5 min) cancels the client's statement. Consistent outcome, not a precise interleave. |

No new error codes. No new status codes on any endpoint.

## 6. Error / status table

Endpoints whose observable behaviour changes. Codes not listed are unchanged.

| Endpoint | Status / code | Scenario |
|---|---|---|
| `GET /entity/{id}?transactionId=X` | `200` | X committed: X's final entity (single version) |
| | `404 ENTITY_NOT_FOUND` | X not committed (inside or outside X); X never wrote the entity; X deleted it |
| `POST /entity/{format}/{name}/{version}`, `PUT/PATCH /entity/...`, transitions, collections — HTTP and gRPC | `409 UNIQUE_VIOLATION` | final entity duplicates a key (now at commit on postgres) |
| | `422 INVALID_UNIQUE_KEY` | final entity has a partial composite key (now at commit on postgres) |
| | `409 UNIQUE_VIOLATION` / `422 INVALID_UNIQUE_KEY` | a joined callback's write left a duplicate or partial key in the final entities (postgres: was refused on the callback request) |
| | `409 CONFLICT` (retryable) | the pre-callout save meets a conflict; two commits claim each other's values (`40P01`) |
| | `503` / `500` | the pre-callout save meets a storage outage / other failure |
| Joined callback writes (`X-Tx-Token` / gRPC `tx-token`) | `2xx` instead of `409`/`422` on postgres | a key clash or partial key is now found at the owner's commit |
| `GET /entity/{id}` and search/count under `X-Tx-Token` / gRPC `tx-token` from a callback | `200` | returns the current anchor |

## 7. Test coverage matrix

| Scenario | Unit (engine / plugin) | e2e (`internal/e2e`, postgres) | Parity (`e2e/parity`, all backends) | gRPC |
|---|---|---|---|---|
| Update: SYNC processor reads anchor back → sees request data | engine unit | yes | yes | callback over gRPC (`callback_txjoin_grpc_*`) |
| Create: SYNC processor reads anchor → found, current | engine unit | yes | yes | yes |
| Function criterion (cascade) reads anchor → current state/data | engine unit | yes | yes | — |
| Workflow-selection criterion on create reads anchor → found, empty state | engine unit | yes | — | — |
| ASYNC_NEW_TX processor reads anchor → current; failure keeps the pre-dispatch save | engine unit | yes | — | — |
| Schedule function reads anchor → current | engine unit | yes | — | — |
| Callback search under token finds anchor by its current data | — | yes | yes | yes |
| Callback write adopted when processor returns no data (rule §2.4) | engine unit (`engine_cbd_callback_test`, `fire_callback_test` updated) | existing | — | — |
| One version per transaction: each §3.1 row | spitest + per-plugin unit | — | yes (history via `/changes`) | — |
| Savepoint rollback restores pending version | spitest | — | — | — |
| `?transactionId` → final entity after commit | — | yes | yes | n/a (no gRPC by-tx read) |
| `?transactionId=X` inside X → 404 | spitest | yes | yes | n/a |
| Unique key: intermediate duplicate then fixed → commits | plugin unit | yes | yes | — |
| Unique key: final duplicate → 409 at commit; partial → 422 | plugin unit | existing (`unique_keys_*`) | existing | existing |
| Unique key: free-after-claim in one tx → commits | plugin unit (`SameTxReclaim*` rewritten) | — | yes | — |
| Scheduled fire changes a key field → claims follow | engine/plugin unit | yes | — | — |
| Pre-callout save conflict → retryable 409, collection not isolated as ENTITY_MODIFIED | service unit | yes (isolated, not parity) | — | yes (envelope `Success=false`, `Error.Code=CONFLICT`) |
| Pre-callout save storage failure → 503 / 500, never 400 WORKFLOW_FAILED | service unit (failing store double) | waived: a storage outage cannot be produced in the e2e suite without a test hook in production code | — | yes (envelope) |
| Unique key: two entities swap values; chain of moves; same value twice in one tx (refused) | plugin unit | yes | yes | — |
| Joined callback writes a duplicate key → callback 2xx, owner commit 409 | — | yes | — | yes (joined write over gRPC `tx-token`) |
| Two commits claim each other's values → postgres `409 CONFLICT` (40P01) | — | waived: the deadlock needs a precise interleave of two commits; the classification of 40P01 is covered by the existing postgres classifier tests | — | — |
| `COMMIT_BEFORE_DISPATCH` (`startNewTxOnDispatch: true`) callback reads anchor and writes it with `If-Match` = payload id → matches | engine unit | yes | — | — |
| Scheduled fire declines / is cancelled after a function criterion → no entity version, client `If-Match` still valid | engine unit | yes | — | — |
| Available-transitions read under a token runs a function criterion → no write | engine unit | yes | — | — |
| A write through each door keeps the entity's claims (keys carried, §4.1) | engine/service unit | — | — | — |
| Callback `If-Match` with payload `meta.transactionId` on the anchor → matches (update path and scheduled fire) | engine unit | yes | — | — |
| Scheduled fire breaks a key → task outcome per §4.3 | scheduler/engine unit | yes | — | — |

Workflow selection, `ASYNC_NEW_TX` and schedule-function rows have no parity
scenario: they differ from the `SYNC` rows only in the engine; the storage call
they rely on (a read of the transaction's pending version under the token) is
the one the `SYNC` parity rows cover on every backend.

Concurrency scenarios (row-lock wait, stray standalone write) are isolated
single-backend e2e tests asserting consistency (one winner, loser `409`, or the
callout failure), never in the parity suite. Each must be shown failing (or
showing the old outcome) on the unfixed code, or it is dropped.

## 8. Tests that assert the current behaviour and change

- `spitest/entity.go` `GetVersionByTransaction/EarliestWins` (§3.4).
- `plugins/sqlite/tx_flush_changetype_test.go`
  `TestFlushToSQLite_ChangeTypeSameTxDoubleSave` (expects 2 versions).
- `internal/domain/workflow/fire_scheduled_test.go` ~:1095-1179 (expects 4
  versions, two in one TX_post; becomes 3).
- `internal/domain/workflow/engine_cbd_callback_test.go`
  `TestProcessorCallback_FirstWriteOfCommittedBytes_IsAdopted` (relies on the
  stale read).
- The scheduled fire's final persist (`fire_scheduled.go` ~:297-337): the
  anchor read and `writtenHere` (~:314) are taken **after**
  `reconcileScheduledTasks` (~:327), whose schedule function (`arm.go` ~:275)
  can now save the anchor. Taken before it, a fire whose target state arms a
  function schedule would `CompareAndSave` against the committed id over its
  own pending write and fail on every attempt. The comment on `writtenHere`
  about which runs take that branch is rewritten.
- `SameTxReclaim*` in the three plugins' `unique_claims_test.go`.
- Any e2e/parity test counting `/changes` entries after a joined-callback write
  or a COMMIT_BEFORE_DISPATCH run (to be found by the builder; grep `changes`
  counts in `internal/e2e` and `e2e/parity`).

## 9. Documentation

- `cmd/cyoda/help/content/workflows.md`: rewrite "What a callback read of the
  entity returns" (callback reads see the current anchor; on create, the anchor
  is found; during workflow selection it has no state yet) and the sentence "A
  processor that writes the entity bases the write on the payload in its
  request, not on a callback read". State that a processor writes its own
  anchor only with the transaction token.
- `api/openapi.yaml` `components.parameters.TxToken`: replace the "saved only
  when its chain of transitions has run" sentences; `transactionId` query
  parameter: committed only, the transaction's single version. Then
  `go generate ./api`.
- `docs/PROCESSOR_EXECUTION_MODES.md` "Transaction-bound callbacks".
- `cmd/cyoda/help/content/cluster.md` (the non-echo sentence; check it reads
  right).
- `cmd/cyoda/help/content/models.md` "Transactional ordering" and
  `cmd/cyoda/help/content/errors/UNIQUE_VIOLATION.md`: remove free-before-claim;
  keys are checked on the transaction's final entities at commit (§4.4).
- `docs/CONSISTENCY.md` §6: remove the delete-then-recreate difference; state
  one version per entity per transaction.
- `cmd/cyoda/help/content/crud.md` where it describes `/changes` or
  `transactionId`, if it says otherwise (builder greps).
- `cmd/cyoda/help/content/workflows.md`: on postgres a callout that carries the
  transaction token holds the entity's row lock while it runs, so another
  writer of that entity waits until the workflow ends.
- `docs/ARCHITECTURE.md` ~:1680 ("returns the *earliest* version").
- `api/openapi.yaml` ~:5551 (free-before-claim in the unique-keys operation).
- The comment on `getEntityByTransactionID` (`service.go` ~:372-380, "earliest
  matching version").
- SPI `unique.go` / `unique_context.go`: state the commit-time claim rule (§4.1)
  and that every write of a keyed entity carries its keys.
- SPI: `EntityStore` doc (Save/CompareAndSave/Delete rule §3.1,
  `GetVersionByTransaction` §3.2) and the SPI CHANGELOG. SPI PR into SPI `main`;
  cyoda-go and the three plugins pseudo-pin it (`make repin-plugins`).
- `COMPATIBILITY.md`, `CHANGELOG.md` `### Breaking`.
- `docs/cloud-parity/617-one-version-per-transaction.md`: the contract (§3, §4,
  §5) in a few lines.
- Issue in `cyoda-go-cassandra`: the contract change, the cases its current code
  does not meet (save-then-delete, delete-then-save, savepoints), and the
  commit-time unique-key rule.

## 10. Left to the builder

- The exact sentinel name for the pre-callout save's failure (§2.3), as long as
  it is in `engineConflictIsTransactionConflict`'s list.
- The SQL shape of postgres's pending-row detection.
- How postgres keeps its record of the entities a transaction wrote exact
  across savepoint rollbacks (§4.2 names two acceptable ways).

