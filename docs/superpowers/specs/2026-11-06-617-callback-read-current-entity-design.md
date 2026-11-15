# A callback read returns the current entity; one version per entity per transaction (#617)

## 1. Intent

A processor, function criterion or schedule function runs on a compute node and
can call back into cyoda with the transaction token. A read of the entity the
workflow runs for (the **anchor**) must return the anchor as the workflow
currently has it: with the data the request sent, the state changes so far and
the changes earlier processors made. On a create it must be found.

Today the engine keeps the anchor in memory across the cascade and saves it to
the transaction only at the end (`internal/domain/entity/service.go` create
save at ~:345, update save at ~:2323) and before a `COMMIT_BEFORE_DISPATCH`
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
  commit, with the keys it reads from its own stored model; no caller passes
  keys.
- Model administration and entity writes follow the documented rules
  (§4.5): the API guarantees them, and storage adds no check of its own.

### Builds on callback scopes (#704)

Every joined request runs in a callback scope that is all-or-nothing
(`internal/txcoord`): a scope that fails is rolled back to its savepoint and
marks the scope above it. With §2's saves, a joined write request that fails
after one of its processors has run has already stored its own entity,
half-way through its workflow, in the owner's transaction; the scope's
rollback removes that write. This spec relies on that rule and does not
restate it.

### Builds on the schema-extension change (#710)

A schema extension commits in a short `READ COMMITTED` transaction of its
own, never the entity transaction's. On postgres it runs on the model pool and
takes the `models` row with `SELECT … FOR NO KEY UPDATE`, which writes no row
version (`plugins/postgres/model_store.go` `ExtendSchema`/`extend`, ~:321-407).
Under that lock it refuses a model that is not `LOCKED` with
`spi.ErrModelNotLocked`, which `common.Internal` answers `409
MODEL_NOT_LOCKED` (`internal/common/errors.go` ~:279). So no entity
transaction writes or locks a `models` row.

### A callout never writes the entity it runs for

A processor, a function criterion and a schedule function never write or
delete the entity the workflow runs for (the anchor), with or without the
transaction token. A processor changes its anchor only through the data it
returns. Writing other entities through the token stays supported. This is
the contract of workflow processing; the engine does not detect or guard
against a breach (a guard would add work to every callout to enforce a rule
implementers obey). Everything in this spec assumes it, and the engine keeps
no machinery whose only purpose is to adopt or survive such a write.

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
(`engine.go` ~:1045-1050) and a pass is minted from the id alone
(`internal/grpc/dispatch.go` `mintPass` ~:94), so with an id and no transaction a save
would be an auto-commit write; the read path passes no id but its context can
carry a joined transaction.

The save comes before `beforeDispatch` (`run_guard.go` ~:248, which calls
`MarkUnsafe` at ~:273), as the `readAnchor` before a `SYNC` processor does
today (`engine_processors.go` ~:236-243). A save that fails has dispatched
nothing, so it must not mark the run as having reached unsafe work.

| Callout | Dispatch site | Notes |
|---|---|---|
| `SYNC` / `ASYNC_SAME_TX` processor | `internal/domain/workflow/engine_processors.go` (`executeSyncProcessor`, `Pause` at ~:257, dispatch at ~:261) | |
| `ASYNC_NEW_TX` processor | `engine_processors.go` (`e.coord.OpenChild` at ~:337, dispatch at ~:368) | Save **before** `OpenChild`, in the chain's own scope, so discarding the processor scope (a processor failure) does not roll back the save. |
| Function criterion — workflow selection, manual transition, automated cascade transition | `criterion_eval.go` `dispatchCriterionFunction` (`Pause` at ~:173, dispatch at ~:177), reached from `selectWorkflow` (`engine.go` ~:575) and the transition paths | On create, workflow selection runs before the anchor has a state; the save stores it with an empty state and a callback read sees it so. |
| Schedule function | `arm.go` `armViaFunction` (`Pause` at ~:269, dispatch at ~:273) | Runs from `reconcileScheduledTasks` after the cascade, before the final save. On the client doors that is inside the engine (`engine.go` ~:364, ~:452, ~:554), before the door's `Seal` (`service.go` ~:342, ~:2317). In a scheduled fire it runs today after the fire's `Seal` (`fire_scheduled.go` ~:318, ~:355), so its callbacks are refused `410`; §2.6 fixes that. |

Not covered, because they need no save:

- `COMMIT_BEFORE_DISPATCH`: TX_pre already flushes and commits the anchor
  before the dispatch. With `startNewTxOnDispatch: true`, TX_post reads TX_pre's
  committed version (`requireAnchorCommittedBy`), which is the in-memory anchor.
  With `false`, the dispatch carries no token.
- `resolveWorkflowForQuery` (`engine.go` ~:661): the read path that lists an
  entity's available transitions. It shares `selectWorkflow` (`engine.go` ~:575)
  with the write paths but passes an empty transaction id, and its context can
  still carry a joined transaction. It must not save; the save is keyed on the
  dispatch's transaction id, not on a transaction being present in context.
- A dispatch with no transaction in context (no token is sent).

### 2.2 When

Before each dispatch of §2.1 the engine reads the anchor inside the transaction
(`readAnchor`) and compares it with its in-memory anchor: the `Data` bytes and
`Meta.State`. If either differs it saves the in-memory anchor; otherwise it
saves nothing. If the read finds no anchor, the anchor is a create's that is
not stored yet, and the engine saves it.

- The comparison is against a fresh read of the stored anchor.
- A false "differs" only costs a redundant save, which one version per
  transaction (§3) makes invisible in history. postgres returns re-encoded
  bytes (JSONB, `_meta` merged into `doc`) for the transaction's own pending
  write, so after a save there the next comparison usually differs and saves
  again; that is accepted. memory and sqlite return the buffered bytes
  (`plugins/sqlite/entity_store.go` ~:625-627). A false "equal" cannot happen: equal bytes
  and equal state are the same content.
- Saving nothing when nothing changed is required, not an optimisation. A
  scheduled fire runs function criteria (workflow selection,
  `fire_scheduled.go` ~:239; the transition criterion through `fireTransition`
  → `engine.go` ~:796) and then may cancel or decline and still **commit** its
  task removal and audit event (cancel ~:243-262, decline ~:296-306). A save of an unchanged
  anchor there would commit a version that changes nothing, with a new
  transaction id that fails every client's stored `If-Match`.
- Meta-only changes (the scheduled fire's attribution stamp, `fire_scheduled.go`
  ~:267-269) do not count; they reach storage with the next content save.

### 2.3 How

- The read and the save use the dispatch's transaction context. The save is a
  plain `EntityStore.Save` of the in-memory anchor, with the attribution the
  request already set on the anchor's meta
  (`service.go` ~:282-295 create, ~:2255-2274 update; a scheduled fire keeps
  the stored meta and sets change user and executor, `fire_scheduled.go`
  ~:267-269).
- **Gate.** The save runs in the chain's scope, before `txcoord.Pause`
  releases the chain's gate for the dispatch, and never acquires the gate
  itself. A joined chain holds the gate from `Coordination.Enter`
  (`internal/domain/txjoin/txjoin.go` `enter`, ~:225) until its scope ends,
  except while paused, and the gate is not re-entrant. The owner's chain holds
  no gate (`Pause` is a no-op there) and needs none: no joined callback can act
  on the transaction between two callouts, because ending a callout refuses all
  its passes from then on and waits until no scope admitted under it is still
  open (`Coordination.BeginCallout`'s `end`, `internal/txcoord/txcoord.go`;
  deferred on every exit of `internal/callout/coordinator.go` ~:166). The owner's
  final save and commit are not changed by this work. `Seal` moves only in the
  scheduled fire (§2.6).
- **Errors.** A failure of this read or save is marked with an engine infrastructure
  sentinel, as `readAnchor` failures are (`errors.Join(ErrProcessorOutputInfra,
  err)` at `engine_processors.go` ~:239-242, or a new sentinel of the same family
  named for this save and added both to `engineConflictIsTransactionConflict`
  (`service.go` ~:2775-2782) and to `classifyWorkflowError`'s infrastructure
  list (~:2859-2873), which maps it through `common.Internal`; without the
  second, it falls to `400 WORKFLOW_FAILED`). So a conflict from the save is a transaction
  conflict (retryable `409 CONFLICT`), never `412 ENTITY_MODIFIED`, and the
  collection update loop (`service.go` ~:2601-2612) never isolates it as an
  item failure and carries on in a failed transaction. A storage outage maps to
  `503`, any other failure to `500` with a ticket, never a `400
  WORKFLOW_FAILED` carrying driver text. Unique-key errors cannot arise from
  this save (§4).

### 2.4 No callback write of the anchor is adopted

A callback never writes the anchor (§1), so the engine adopts none:
`adoptCallbackWrite`, the reads around a dispatch that served it, and the
`COMMIT_BEFORE_DISPATCH` TX_post adoption are deleted. A processor's changes
reach the anchor only through its returned data.

### 2.5 What a callback sees

- Under the token: the anchor as saved just before the dispatch, which is the
  anchor at that moment. Its state is the transition's source state while that
  transition's processors run (the state advances after processors,
  `fireTransition`); its data includes earlier processors' changes.
- **The payload's `meta.transactionId` equals the `transactionId` a callback
  read of the anchor under the token returns at dispatch.** After the §2.2 step
  the engine sets `entity.Meta.TransactionID` to the transaction id of the
  stored anchor: the transaction's own id after a save (the id the store stamps; equal to the dispatch's id on every write path), the id the read
  returned otherwise. The same assignment follows every other engine write of
  the anchor: the `COMMIT_BEFORE_DISPATCH` TX_pre flush
  (`flushAndCommitSegment`, `engine_processors.go` ~:594) and the post-dispatch
  apply `CompareAndSave` (~:533), with `COMMIT_BEFORE_DISPATCH` and
  `startNewTxOnDispatch: true` included (the payload carries the id TX_post
  reads). The builder audits every engine reader of `entity.Meta.TransactionID`
  for a dependency on the old value.
- **Scheduled fire precondition.** The fire's `expectedTxID` (the precondition
  of its final persist, `fire_scheduled.go` ~:273) and the attribution stamp
  (~:267-269) move before `resolveWorkflow` (~:239), so they are taken from the
  entity as first read, before any callout. The `expectedTxID == ""` refusal
  stays where it is (its audit event names the selected workflow) and tests
  the captured value.

### 2.6 Scheduled fire: `Seal` after the schedule functions

The client doors reconcile scheduled tasks inside the engine, so a schedule
function's callouts end before the door's `Seal`. The scheduled fire seals
first (`fire_scheduled.go` ~:318) and reconciles after (~:355), so every
callback of its schedule functions is refused `410` (`internal/txcoord/inprocess.go`
~:266 sets `sealed`, ~:440-443 refuses). That is a defect of its own, fixed here
because §2.1's save before a schedule function serves no one while it stands.

The fire's end becomes, in order:

1. the cascade (unchanged);
2. `removeOwnLife`, then `reconcileScheduledTasks` (its schedule functions save
   the anchor first, §2.1, and their callbacks are admitted);
3. `Seal`;
4. an anchor read, which decides `writtenHere`. A fire whose target state
   arms a function schedule has now written the anchor in this transaction, so
   the final `CompareAndSave` names this transaction's id;
5. the final `CompareAndSave` and the commit (unchanged).



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

## 4. Unique keys: computed at commit, from the final entities and the stored model, on every backend

Today the backends disagree:

- postgres writes claims at each save (`plugins/postgres/unique_claims.go`
  `replaceClaims`) against the immediate unique index `unique_claims_uq`.
- memory checks at commit on the final buffered entities, but ignores a
  committed claim only when its holder is in `tx.Deletes`
  (`plugins/memory/txmanager.go`: set built ~:951-955, checked ~:981), so a holder this transaction
  re-keys still blocks the value.
- sqlite runs `replaceClaims` per entity at commit, in map order, against an
  immediate unique index (`plugins/sqlite/txmanager.go`: map loop ~:1037, `replaceClaims`
~:1139-1143).

So a swap of key values between two entities in one transaction, or a chain of
moves, is refused on memory, refused or accepted by map order on sqlite, and
refused on postgres unless the order of saves happens to free first. The help
documents a "free before claim" rule (`cmd/cyoda/help/content/models.md`
"Transactional ordering", `errors/UNIQUE_VIOLATION.md`), and `models.md` claims
re-keying works on commit-time backends, which is false today. With §2's saves,
postgres would also check every intermediate state.

The keys themselves reach storage on the request context: the entity service
attaches the model's keys (`spi.WithUniqueKeys`, `service.go` ~:196, ~:2064,
~:2532) and the store reads them (`spi.UniqueKeysFromContext`). That design
(`docs/superpowers/specs/2026-06-28-composite-unique-keys-design.md` §3.1)
rejected the store reading the keys through the main module's model cache,
which a plugin cannot reach; it did not consider the store reading its own
stored model, which holds the keys (`spi.ModelDescriptor.UniqueKeys`, persisted
by each model store). It has two failure modes:

- A write that does not attach the keys enforces nothing, without error;
  `UniqueKeysFromContext` returns `nil` for "no keys" and "not attached" alike.
  The scheduled fire attaches none (§4.3).
- A caller with an out-of-date cached model enforces the old keys. The design
  accepted this for multi-node postgres (§ "The residual risk" of that spec):
  a node that misses the unlock and relock gossip enforces the old keys until
  its cache lease ends, and data it admits can break the new key and stays.

### 4.1 The rule

At commit, a transaction's claims are computed from its final entities:

1. Read, inside the committing transaction, the unique keys of the model of
   every entity whose final write in the transaction is a save (one read per
   distinct model). The model's state is not checked: the API guarantees that
   an entity is saved only into a `LOCKED` model (§4.5). An entity whose final
   write is a delete needs no model read.
2. Release the claims of every entity the transaction wrote or deleted.
3. Compute the claims of every entity whose final write is a save, from the
   bytes that final write was given (never a re-read of stored data), with
   its model's unique keys as read in step 1. A partial composite
   key fails the commit with `spi.ErrPartialUniqueKey`.
4. Insert those claims. A value claimed twice inside the transaction, or held
   by an entity outside the transaction's writes (committed, or claimed by a
   concurrent transaction that commits first), fails the commit with
   `spi.ErrUniqueViolation`.

A save inside a transaction does no claim work. A write outside a transaction
is its own transaction and does steps 1-4 in its own statement or critical
section (postgres and sqlite `replaceClaims`; memory `saveUnlocked`), with the
model read from its own model store in the same way.

No caller passes keys. The store is the only reader of the keys, from the one
place they are kept, at the moment it enforces them; a write path cannot
forget them or pass out-of-date ones, and the stale-cache residual is gone.

**Call stack cleanup.** Deleted, with their tests rewritten to state keys on
the model instead:

- SPI: `WithUniqueKeys`, `UniqueKeysFromContext` (`unique_context.go` and its
  test).
- `internal/domain/entity/service.go`: the four `spi.WithUniqueKeys`
  assignments with their comments (create ~:272, collection create
  ~:2075-2077, update ~:2263, collection update ~:2533-2534),
  `parsedItem.uniqueKeys` (~:1943, ~:1993), and every comment that describes
  keys riding the context (the builder greps `UniqueKeys` in the file).
  `checkUniqueKeys` (~:187) already returns only an error and stays.
- memory: `recordUniqueKeys`, `txUniqueKeys` and their cleanup and capture
  (`txmanager.go` ~:104, ~:111-116, ~:237, ~:282-292, ~:594, ~:908;
  `entity_store.go` ~:281, ~:349, ~:376); `saveUnlocked` reads the model
  instead of the context (~:420).
- sqlite: `recordUniqueKeys`, `uniqueKeysFor`, `txUniqueKeys`, and the
  savepoint note about not snapshotting them (`txmanager.go` ~:128, ~:147-152,
  ~:231, ~:241-258, ~:686, ~:1140, ~:1448-1455; `entity_store.go` ~:325, ~:537,
  ~:560); the non-transactional `replaceClaims` call takes the model's keys
  (~:469).
- postgres: `replaceClaims` takes the keys from the model read, not the context
  (`unique_claims.go` ~:29), and the comment at `entity_store.go` ~:402 ("no-op
  when no keys are declared in context").
- `internal/e2e/unique_keys_processor_test.go` header (~:8-12, "the cascade's
  final Save computes claims").
- The cassandra plugin uses both SPI helpers (`internal/store/entity_store.go`
  and five integration tests); its build breaks at the SPI bump until it
  follows (§9 issue).

The builder's exit check: `grep -rn --include='*.go' "UniqueKeysFromContext\|WithUniqueKeys\|recordUniqueKeys\|txUniqueKeys\|uniqueKeysFor"`
over cyoda-go, the SPI and the three plugins is empty.

Swaps, chains of moves and free-after-claim all commit when the final state is
unique. The request-body pre-check stays: a body with a partial key is `422`
before the workflow runs (`service.go` `checkUniqueKeys`, ~:187, using the
model the service has already loaded). It validates the client's input; it no
longer passes anything to storage.

### 4.2 Per backend

- **memory:** in commit, under `entityMu`, the release set is every entity in `tx.Buffer` and
  `tx.Deletes`; a committed claim held by an entity in that set does not
  conflict, and the intra-batch check (`pending`) catches a value two buffered
  entities claim. The apply step releases the claims of **every** entity in the
  release set first and only then inserts all new claims. Releasing and
  inserting one entity at a time (`txmanager.go` ~:1110-1117) loses a claim on
  a swap: A x→y then B y→x inserts `y→A`, and `releaseClaims(B)`
  (`store_factory.go` ~:311-317) deletes key `y`.
- **sqlite:** in the commit flush, read the `models` rows with the flush's own
  `sqlTx`, never through the model store: the model store uses the writer
  pool, which has one connection (`store_factory.go` ~:115, ~:377), so a call
  into it from inside the flush would wait for itself. The non-transactional
  `saveDirectlyLocked` does the same with its own transaction. Then release
  the claims of every buffered and
  deleted entity first, then insert all new claims.
- **postgres:** keep the immediate index. Transactional saves stop calling
  `replaceClaims`; they record the entity id in the transaction's state. At
  commit, after read-set validation and **before** `stampCommitInstant` (its
  rule: nothing after `cyoda_stamp` waits on a lock,
  `transaction_manager.go` ~:417-420, relied on by `ConsistencyTime`,
  `consistency_time.go` ~:186-191), run steps 1-4: step 1 as a plain read of
  the `models` rows' unique keys in the transaction's snapshot (no lock: keys
  change only while a model is `UNLOCKED`, and an `UNLOCKED` model has no
  entities, §4.5), steps 2-4 as `DELETE` and `INSERT` statements, inserting in
  a deterministic order (by model, key id, signature). The non-transactional
  path does the same inside `replaceClaims`, before `stampOwnCommitInstant`.

  **Errors.** The step 1-4 statements can now be the first statements
  `Commit` issues (when the read set is empty), so their errors go through
  `abortedCommitError` (`transaction_manager.go` ~:283-288) and then
  `classifyTxError`, as read-set validation's do, mapping `40001`, `40P01` and
  `23505` on `unique_claims_uq`.

  *What the record holds.* For each entity the transaction wrote, the bytes
  of its last write (none for a delete); step 3 computes claims from these.
  A re-read of `entities.doc` at commit would not do: JSONB re-renders
  numbers without an exponent, so a key value of `1e100` comes back with 101
  digits, which `canonNum` refuses (`maxCoeffDigits` 64, SPI
  `unique_signature.go` ~:35, ~:178), and postgres would answer `422` inside a
  transaction where memory, sqlite and postgres's non-transactional path
  (which computes from the given bytes, `unique_claims.go` ~:29-33) accept.
  memory and sqlite hold the same bytes in their buffers already.

  The record must be right after `ROLLBACK TO SAVEPOINT`: it is snapshotted
  with the savepoint, as the read and write sets are (`txstate.go`
  ~:235-262). The claim release in `deleteOn`
  (`entity_store.go` ~:884) **moves to commit** (step 2), and the model-wide
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
  other committed or proceeds if it rolled back. Claims are inserted in a fixed
  order, so commits claiming the same values wait rather than deadlock. Two
  commits that each claim a value the other frees in the same commit meet
  PostgreSQL's deadlock detection (`40P01`), classified as a retryable
  `spi.ErrConflict` (`409 CONFLICT`) where memory and sqlite answer `409
  UNIQUE_VIOLATION`. This is an accepted, transient difference: both
  answers fail closed, the `409 CONFLICT` is retryable, and the retry gets the
  same `UNIQUE_VIOLATION` the other backends gave.

Commit errors already map to the right answers: `common.Internal` turns
`ErrUniqueViolation` into `409 UNIQUE_VIOLATION` and `ErrPartialUniqueKey` into
`422 INVALID_UNIQUE_KEY` (`internal/common/errors.go` ~:277-282); postgres
classifies a 23505 on `unique_claims_uq` by constraint name
(`transaction_manager.go` ~:1031).

### 4.3 Scheduled fires

`fireScheduled` begins its transaction without `spi.WithUniqueKeys`
(`fire_scheduled.go` ~:146). memory then releases the entity's claims and
inserts none (`txmanager.go` ~:958-963, ~:1114-1117); postgres and sqlite skip
claim work and keep stale claims. With §4.1 the fire needs no change for this:
its commit reads the model like any other. A fire whose final entity breaks a
key then fails at commit,
the entity unchanged by that transaction. (A segmented fire, through a
`COMMIT_BEFORE_DISPATCH` processor, has already committed its earlier segments;
each segment's commit is checked on its own final entities, §4.1.) That is a failed run like any other
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
- Tests that attach keys with `spi.WithUniqueKeys` (`service_unique_keys_test.go`;
  in memory `unique_claims_test.go`, `tx_abort_test.go`,
  `sm_audit_txindex_prune_test.go`; in sqlite `unique_claims_test.go`,
  `sm_audit_tx_join_test.go`; in postgres `unique_claims_test.go`,
  `entity_store_nontx_atomic_test.go`) declare the keys on a locked model
  instead.
- The composite-unique-keys help and docs that describe the gossip/TTL
  staleness residual, if any (builder greps `models.md`, `UNIQUE_VIOLATION.md`,
  `docs/`), drop it.

### 4.5 A model and its entities: the API's rules

Entities exist only in a `LOCKED` model; a model is unlocked only when it has
no entities; a model's unique keys (and the rest of its descriptor except the
change level and the schema extensions) change only while it is `UNLOCKED`; a
`LOCKED` model is never deleted or re-imported. The API guarantees these rules
with the checks the services make: an entity write checks the model is
`LOCKED` (`internal/domain/entity/service.go`); unlock counts the model's
entities and refuses `409 MODEL_HAS_ENTITIES`; delete, import and the
unique-key change refuse a `LOCKED` model `409 MODEL_ALREADY_LOCKED`
(`internal/domain/model/service.go`). Storage adds no check of its own and no
machinery for a model changing while entities of it are being written: that
does not occur when model administration runs as documented (from one place,
not while writes to the model are in flight). The rules and that operating
assumption are documented in `cyoda help models`.

Because an `UNLOCKED` model has no entities, delete needs no entity count, and
the extension log of a model that import or delete acts on is always empty
(rows exist only while the model is `LOCKED`; unlock folds and clears the log):
the extension-log `DELETE` and its stale-row warning are removed from `Save` and
`Delete` on every backend. sqlite's `SetChangeLevel`, a separate read and
whole-document write that could put back a stale descriptor, becomes one
statement that sets only the change level, as on postgres.

**Test fixtures.** The spitest suites and the plugin and unit tests that save
entities use a shared fixture that saves and locks the model first
(`spitest.EnsureLockedModel`), as the API guarantees in production.

### 4.6 Rejected alternatives

- **Assert that the caller attached the keys** (`UniqueKeysFromContext`
  returning "not stated", storage refusing a save without it). It closes the
  forgotten-keys hole but keeps the caller as the source of the keys, so the
  out-of-date-keys hole stays, and every write path keeps an obligation.
- **One wrapper in cyoda-go that attaches the keys to every write.** Same: the
  keys still come from the main module's cache, and code that reaches the
  plugin's factory directly bypasses it.

## 5. Behaviour visible to clients

| Area | Before | After |
|---|---|---|
| Callback read of the anchor under the token | last saved copy; not found on create | current anchor |
| Callback search/count/page under the token | anchor at its last saved copy | anchor as current |
| `/entity/{id}/changes`, audit entity-change history | possibly several versions per transaction | one per transaction |
| `GET /entity/{id}?transactionId=X`, X committed | X's first save | X's final entity |
| `GET /entity/{id}?transactionId=X`, X not committed, caller outside X | `404 ENTITY_NOT_FOUND` | unchanged |
| same, caller inside X (callback with X's token) | postgres: X's first uncommitted save; memory/sqlite: 404 | `404 ENTITY_NOT_FOUND` everywhere |
| Unique key violated only by an intermediate state | commits, except on postgres when that state was saved (a callback's write, a `COMMIT_BEFORE_DISPATCH` TX_post write, a claim made before its holder is freed): `409`/`422` | commits everywhere, except a state a `COMMIT_BEFORE_DISPATCH` TX_pre commits: that is a real commit and is checked like any other (§4.1) |
| Two entities swap key values, or a chain of moves, in one transaction | memory: `409`; sqlite: `409` or commits by map order; postgres: `409` unless saves free first | commits everywhere |
| Unique key violated by the final entity | `409 UNIQUE_VIOLATION` (`422` partial) | unchanged codes; postgres reports at commit |
| A joined callback's write whose final entity duplicates a key (postgres) | the callback request gets `409` | the callback request succeeds; the owner's commit fails `409` (as memory/sqlite today) |
| A joined callback whose request body leaves a partial key | `422` at the callback, every backend (`withUniqueKeys`, `service.go` ~:264, ~:2246); the failed write marks the owner (#704) | unchanged |
| A joined callback whose processors' returned data leaves a partial key (postgres) | the callback request gets `422` | the callback request succeeds; the owner's commit fails `422` (as memory/sqlite today) |
| A scheduled fire that changes a key field | memory drops the entity's claims; postgres/sqlite keep stale claims | claims follow the final entity; a fire that breaks a key fails (§4.3) |
| A multi-node postgres node whose cached model is out of date after an unlock, key change and relock | enforces the old keys until its cache lease ends; data it admits can break the new key | enforces the stored keys (§4.1) |
| A scheduled fire that declines or is cancelled after running a function criterion | no entity version | unchanged: no entity version (§2.2) |
| Reading an entity's available transitions under a transaction token (function criteria run) | no write | unchanged: no write (§2.1) |
| Two workflows whose callbacks each write the other's anchor during their callouts (postgres) | each cascade fails `409` at its final save | each callback write waits on the row lock the other cascade's pre-callout save holds; PostgreSQL's deadlock detection (`40P01`) fails one with a retryable `409 CONFLICT`, and the other then proceeds. Fails closed. |
| Another client writes the anchor during a callout (postgres) | client commits; cascade fails `409` | client waits until the cascade ends; one side wins, the other gets `409`. A wait longer than the statement timeout (`CYODA_POSTGRES_STATEMENT_TIMEOUT`, default 5 min) cancels the client's statement. Consistent outcome, not a precise interleave. |
| Another transaction that read the anchor commits during a callout (postgres) | commits | waits at commit (read-set validation takes `FOR SHARE`, `plugins/postgres/commit_validator.go` ~:44) until the cascade ends, then validates as today. A processor's own tokenless transaction that read its anchor therefore waits on its own callout until the callout times out. Documented in `cyoda help workflows`. |

No new error codes. No new status codes on any endpoint.

## 6. Error / status table

Endpoints whose observable behaviour changes. Codes not listed are unchanged.

| Endpoint | Status / code | Scenario |
|---|---|---|
| `GET /entity/{id}?transactionId=X` | `200` | X committed: X's final entity (single version) |
| | `404 ENTITY_NOT_FOUND` | X not committed (inside or outside X); X never wrote the entity; X deleted it |
| `POST /entity/{format}/{name}/{version}`, `PUT/PATCH /entity/...`, transitions, collections — HTTP and gRPC | `409 UNIQUE_VIOLATION` | final entity duplicates a key (now at commit on postgres) |
| | `422 INVALID_UNIQUE_KEY` | final entity has a partial composite key (now at commit on postgres) |
| | `409 UNIQUE_VIOLATION` / `422 INVALID_UNIQUE_KEY` | a joined callback's write left a duplicate key, or its processors' returned data a partial key, in the final entities (postgres: was refused on the callback request) |
| | `409 CONFLICT` (retryable) | the pre-callout save meets a conflict; two commits claim each other's values (`40P01`) |
| | `503` / `500` | the pre-callout save meets a storage outage / other failure |
| Joined callback writes (`X-Tx-Token` / gRPC `tx-token`) | `2xx` instead of `409`/`422` on postgres | a key clash, or a partial key from the callback's processors' returned data, is now found at the owner's commit; a partial key in the callback's body stays `422` at the callback |
| `GET /entity/{id}` and search/count under `X-Tx-Token` / gRPC `tx-token` from a callback | `200` | returns the current anchor |
| `DELETE /model/{entityName}/{modelVersion}` | `409 MODEL_HAS_ENTITIES` | no longer produced: a model with entities is `LOCKED`, and delete answers `409 MODEL_ALREADY_LOCKED` (§4.5); removed from the delete operation's documentation |

## 7. Test coverage matrix

| Scenario | Unit (engine / plugin) | e2e (`internal/e2e`, postgres) | Parity (`e2e/parity`, all backends) | gRPC |
|---|---|---|---|---|
| Update: SYNC processor reads anchor back → sees request data | engine unit | yes | yes | callback over gRPC (`callback_txjoin_grpc_*`) |
| Create: SYNC processor reads anchor → found, current | engine unit | yes | yes | yes |
| Function criterion (cascade) reads anchor → current state/data | engine unit | yes | yes | — |
| Workflow-selection criterion on create reads anchor → found, empty state | engine unit | yes | — | — |
| ASYNC_NEW_TX processor reads anchor → current; failure keeps the pre-dispatch save | engine unit | yes | — | — |
| Schedule function reads anchor → current, on a client door and in a scheduled fire (the fire is `410` today, §2.6) | engine unit | yes | — | — |
| Callback search under token finds anchor by its current data | — | yes | yes | yes |
| Callback count and page under token count/list the anchor by its current data | — | yes | yes | yes |
| One version per transaction: each §3.1 row | spitest + per-plugin unit | — | save, save and create, save through `/changes`; the other rows waived: spitest runs each row on every backend and the HTTP layer adds no per-row logic | — |
| Savepoint rollback restores pending version | spitest | — | — | — |
| `?transactionId` → final entity after commit | — | yes | yes | n/a (no gRPC by-tx read) |
| `?transactionId=X` inside X → 404 | spitest | yes | yes | n/a |
| `?transactionId=X`, X deleted the entity → 404 | spitest | yes | — | n/a |
| Joined callback body with a partial key → 422 at the callback, owner marked | — | existing (#704) | — | existing |
| Joined callback's processor returns data with a partial key → callback 2xx, owner commit 422 | — | yes | — | — |
| postgres: another client writes the anchor during a callout → waits; one winner, loser `409` | — | yes (isolated, not parity) | — | — |
| postgres: another transaction that read the anchor commits during a callout → waits, then validates | — | yes (isolated, not parity) | — | — |
| Unique key: intermediate duplicate then fixed → commits | plugin unit | yes | yes | — |
| Unique key: final duplicate → 409 at commit; partial → 422 | plugin unit | existing (`unique_keys_*`) | existing | existing |
| Unique key: free-after-claim in one tx → commits | plugin unit (`SameTxReclaim*` rewritten) | — | yes | — |
| Scheduled fire changes a key field → claims follow | engine/plugin unit | yes | — | — |
| Pre-callout save conflict → retryable 409, collection not isolated as ENTITY_MODIFIED | service unit | gRPC transition door, isolated; the HTTP doors waived: each HTTP door that sends data saves before its first callout, so no deterministic window for a rival exists, and HTTP and gRPC answer through the same `classifyWorkflowError` | — | yes (envelope `Success=false`, `Error.Code=CLIENT_ERROR` with `CONFLICT` in the message) |
| Pre-callout save storage failure → 503 / 500, never 400 WORKFLOW_FAILED | service unit (failing store double) | waived: a storage outage cannot be produced in the e2e suite without a test hook in production code | — | yes (envelope) |
| Unique key: two entities swap values; chain of moves; same value twice in one tx (refused) | plugin unit | yes | yes | — |
| Joined callback writes a duplicate key → callback 2xx, owner commit 409 | — | yes | — | yes (joined write over gRPC `tx-token`) |
| Two commits claim each other's values → postgres `409 CONFLICT` (40P01) | — | waived: the deadlock needs a precise interleave of two commits; the classification of 40P01 is covered by the existing postgres classifier tests | — | — |
| Scheduled fire declines / is cancelled after a function criterion → no entity version, client `If-Match` still valid | engine unit | waived: cannot go RED (the pre-#617 base writes nothing either) | — | — |
| Available-transitions read under a token runs a function criterion → no write | engine unit | waived: cannot go RED (the base writes nothing either) | — | — |
| Scheduled fire breaks a key → task outcome per §4.3 | scheduler/engine unit | yes | — | — |
| Keys come from the stored model: a transactional and a non-transactional save with no keys anywhere on the call path enforce the model's keys | spitest + per-plugin unit | — | existing `unique_keys` parity (unchanged results) | — |

Workflow selection, `ASYNC_NEW_TX` and schedule-function rows have no parity
scenario: they differ from the `SYNC` rows only in the engine; the storage call
they rely on (a read of the transaction's pending version under the token) is
the one the `SYNC` parity rows cover on every backend.

Concurrency scenarios (row-lock waits) are isolated
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
- The scheduled fire's end (`fire_scheduled.go` ~:316-375), reordered per §2.6.
  The comment on `writtenHere` about which runs take that branch is rewritten.
  The fire's existing tests that pin `Seal` before reconcile, if any, change.
- `SameTxReclaim*` in the three plugins' `unique_claims_test.go`.
- Every spitest suite and plugin/unit test that saves entities of a model it
  never created and locked (spitest `helpers.go` ~:32 and the entity,
  transaction, search and audit suites; most `_test.go` files that use an
  `EntityStore`): a shared fixture saves and locks the model first (§4.5).
- Any e2e/parity test counting `/changes` entries after a joined-callback write
  or a COMMIT_BEFORE_DISPATCH run (to be found by the builder; grep `changes`
  counts in `internal/e2e` and `e2e/parity`).

## 9. Documentation

- `cmd/cyoda/help/content/workflows.md`: rewrite "What a callback read of the
  entity returns" (callback reads see the current anchor; on create, the anchor
  is found; during workflow selection it has no state yet) and the sentence "A
  processor that writes the entity bases the write on the payload in its
  request, not on a callback read". Rewrite "A processor changes the entity it
  runs for in one way only" as the rule of §1: a processor, a function
  criterion and a schedule function never write or delete the entity they run
  for, with or without the token; a processor changes it only through its
  returned data. Drop the "write it yourself and return no mutations" option,
  the startNewTxOnDispatch sentence "the cascade-anchor included", and the
  scheduled-run sentence about a callback that writes the entity being fired.
  `docs/CONSISTENCY.md` rule 9 and `docs/PROCESSOR_EXECUTION_MODES.md` likewise.
- `api/openapi.yaml` `components.parameters.TxToken`: replace the "saved only
  when its chain of transitions has run" sentences; `transactionId` query
  parameter: committed only, the transaction's single version. Then
  `go generate ./api`.
- `docs/PROCESSOR_EXECUTION_MODES.md` "Transaction-bound callbacks".
- `docs/CONSISTENCY.md` rule 9 (~:676-680): "A callback read returns what is
  stored in the transaction, without the mutations an earlier processor
  returned … so the processor bases its write on the payload in its
  request." A callback read now returns the current entity.
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
  writer of that entity, and another transaction that read it, wait at their
  write or commit until the workflow ends.
- `docs/ARCHITECTURE.md` ~:1860 ("returns the *earliest* version").
- `api/openapi.yaml` ~:5770 (free-before-claim in the unique-keys operation).
- The comment on `getEntityByTransactionID` (`service.go` ~:363-371, "earliest
  matching version").
- SPI `unique.go`: the commit-time claim rule (§4.1), keys read by the store
  from its own model; `unique_context.go` and its test deleted.
- `cmd/cyoda/help/content/models.md`, `errors/MODEL_HAS_ENTITIES.md`,
  `docs/CONSISTENCY.md` and `api/openapi.yaml` (unlock and delete operations):
  state the rules of §4.5 and that the API guarantees them; model
  administration runs from one place, not while writes to the model are in
  flight; delete never answers `MODEL_HAS_ENTITIES` (a model with entities is
  `LOCKED`).
- SPI: `EntityStore` doc (Save/CompareAndSave/Delete rule §3.1,
  `GetVersionByTransaction` §3.2) and the SPI CHANGELOG. SPI PR into SPI `main`;
  cyoda-go and the three plugins pseudo-pin it (`make repin-plugins`).
- `COMPATIBILITY.md`, `CHANGELOG.md` `### Breaking`.
- `docs/cloud-parity/617-one-version-per-transaction.md`: the contract (§3, §4,
  §5) in a few lines.
- Issue in `cyoda-go-cassandra`: the contract change, the cases its current code
  does not meet (save-then-delete, delete-then-save, savepoints), the
  commit-time unique-key rule with keys read from the stored model.

## 10. Left to the builder

- The exact sentinel name for the pre-callout save's failure (§2.3), as long as
  it is in `engineConflictIsTransactionConflict`'s list and in
  `classifyWorkflowError`'s infrastructure list.
- The SQL shape of postgres's pending-row detection.
- How postgres snapshots its record of the entities a transaction wrote (and
  their last bytes) with a savepoint (§4.2).

