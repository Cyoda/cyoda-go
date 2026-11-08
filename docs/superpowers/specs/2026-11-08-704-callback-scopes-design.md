# Callback scopes: each callback all-or-nothing, transaction coordination behind one interface

Issue: #704 (fixes #702). Milestone v0.9.0.

## Intent

A compute node's callback joins the transaction of the operation that made the
callout. Today a callback that fails leaves its writes in that transaction, an
`ASYNC_NEW_TX` rollback can undo another callback's acknowledged write, and the
transaction's coordination exists only in the memory of the pnode that owns it.

After this change:

- Every joined request runs in a **scope**. A scope is all-or-nothing: either
  all its writes become part of the transaction, or none do.
- The answer a compute node gets always says which: a `2xx` is sent only for a
  scope that was kept.
- A retryable failure leaves the transaction usable, so the compute node can
  retry the callback in it.
- A non-retryable failure of a joined write makes the transaction (or the
  `ASYNC_NEW_TX` processor it ran under) rollback-only.
- The coordination of a transaction (callout fence, per-transaction gate, the
  owner's waits, scopes) sits behind one interface with two implementations:
  the in-process one used for memory, SQLite and PostgreSQL, and one a storage
  plugin supplies through an optional SPI capability. A plugin that supplies it
  has location-independent transactions: a callback is served by whichever
  pnode it reaches.

## Goals

- All backends give the same outcome for the same sequence of callbacks.
- A retryable failure never forces an abort.
- A pnode that dies or is partitioned does not force an abort where the backend
  allows the work to continue elsewhere (capability path).
- Every outcome is fixed before the answer is sent: no `2xx` for discarded work,
  and no error answer for kept work.

## Non-goals

- A durable implementation of the capability. The Cassandra plugin builds it
  (Cyoda/cyoda-go-cassandra#121).
- Cassandra transaction recovery (decision D2, open; needed only by
  cassandra#121).
- PostgreSQL `ExtendSchema` joining the caller's transaction (#705).
- Changing the order of a transition's processors. They still run one after
  another (`internal/domain/workflow/engine_processors.go:126`).

## Anti-goals

- A fallback that keeps partial work because a dependency is unavailable.
- A retryable answer to a request that cannot succeed when retried in the same
  place.
- Different behaviour per backend for the same contract.

## Terms

- **Owner**: the pnode and request that began the transaction and commit or
  roll it back.
- **Callout**: one processor, criterion, function or schedule-function call to a
  compute node. It has a fencing number (major, minor) that only rises
  (`internal/fence/fence.go`).
- **Pass**: the transaction token a callout carries. It names its callout and
  number and every enclosing callout (`token.Claims.Outer`).
- **Joined request**: a request carrying a pass.
- **Scope**: the unit of all-or-nothing work in a transaction. The tree is:
  - the **root scope**: the owner's own work;
  - a **callback scope**: one joined request;
  - a **processor scope**: one `ASYNC_NEW_TX` processor.
- **Parent of a callback scope**: the scope the callout its pass names was begun
  from.
- **Paused**: a scope waiting on a callout of its own (nested callout).
- **Marked**: a scope that is rollback-only. It cannot complete.
- **Seal**: the owner's cut-off before commit or rollback. After it no scope
  opens or completes.

## Rulings and decisions

From #704 (rulings 2026-11-05 to 2026-11-08) and the decisions of 2026-11-08:

- Callback scopes with a savepoint per scope in-process, and the stack rule.
- An optional SPI capability supplies durable coordination. Its presence makes
  transactions location-independent; routing follows from it; no routing flag.
- Callbacks of one transaction may run concurrently on a capable backend. Two
  that write the same entity from the same version conflict: the loser keeps
  nothing and answers retryable `409 CONFLICT`. Serialising backends never give
  this answer.
- **D-A.** An owner whose transaction is rollback-only fails with
  `409 TRANSACTION_ROLLBACK_ONLY`, not retryable (section 9).
- **D-B.** A joined request whose scope would sit under a marked scope is
  refused at once with the same code, not retryable. This includes reads.
- **D-C.** The SPI capability surface is in section 8.
- **D-D.** Marks live on scopes. A processor scope stops a mark from climbing
  further.
- **D-E.** #705 is out of scope.

## Design

### 1. The scope tree

- **Root.** Opened when the owner begins the transaction. It takes no
  savepoint: its end is the transaction's commit or rollback.
- **Callout registration.** A callout is registered with the scope that began
  it: the scope on the chain's context when `Coordinator.run` begins the
  callout (`internal/callout/coordinator.go:161`). The owner's chain carries the
  root, and a callback's chain carries its callback scope.
- **Every callout site goes through `Coordinator.run`:** processors
  (`engine_processors.go:270,364,465`), function criteria
  (`criterion_eval.go:179`) and scheduled-transition functions (`arm.go:275`).
  Search makes no callouts: it refuses function conditions
  (`internal/domain/search/operators.go:163-171`).
- **Callback scope.** Opened by the join layer for each admitted joined request.
  Its parent is the scope registered with the callout named first in its pass.
- **Processor scope.** Opened by the engine for an `ASYNC_NEW_TX` processor, as
  a child of the chain's current scope. It replaces the direct `Savepoint` call
  (`engine_processors.go:344-385`).
- **Where completed work goes.** A completed scope's writes become part of its
  parent. A later discard of an ancestor removes them too. "All-or-nothing" is
  relative to the enclosing scope: a callback answered `2xx` is still undone if
  the scope above it is discarded (for example a failed `ASYNC_NEW_TX`
  processor, or the transaction's rollback).

### 2. How a scope ends

When a callback scope ends, the first matching row decides the outcome.
Precedence runs top to bottom:

| # | Condition at the end of the scope | Scope | Mark | Answer to the compute node |
|---|---|---|---|---|
| 1 | Its pass is no longer current (callout advanced or ended) | discarded | none | `410 CALLOUT_SUPERSEDED`, not retryable |
| 2 | The transaction lost a write race (`TransactionManager.LostRace`) and the request failed | discarded | none | `409 CONFLICT`, **not retryable** |
| 3 | The transaction's session has ended (PostgreSQL dead transaction, both shapes, section 9) | discarded if possible | none (root records it as broken, section 3) | `404 TRANSACTION_NOT_FOUND`, not retryable |
| 4 | The scope is marked (a descendant's write failed) | discarded | parent, if this request is a write | `409 TRANSACTION_ROLLBACK_ONLY`, not retryable |
| 5 | The handler succeeded | completed | none | the handler's answer |
| 6 | Retryable failure | discarded | none | the handler's answer |
| 7 | Non-retryable failure of a write | discarded | parent | the handler's answer |
| 8 | Non-retryable `4xx` of a read | **completed** | none | the handler's answer |
| 9 | Non-retryable `5xx` of a read | discarded | none | the handler's answer |

Notes on the rows:

- **Row 1** comes first: a superseded scope never passes a mark up. The owner
  has already moved to another compute node or ended the callout.
- **Row 2.** Retrying the callback inside a transaction that has lost a race
  cannot succeed, so the answer is not retryable for the callback. The owner's
  operation still gets the retryable `409 CONFLICT`
  (`conflictOverDispatchFailure`, `engine_processors.go:312-321`, and the commit
  refusal). A lost race takes precedence over a mark because a stale snapshot
  may be what made the write fail.
- **Row 5.** "Completed" means the completion step itself succeeded (in-process:
  `ReleaseSavepoint`). A completion that fails is a failed request, judged again
  from row 1: rows 1-4 first, so a lost race still answers by row 2. Otherwise
  the scope is discarded and the request is answered `500 SERVER_ERROR`. As a
  non-retryable failure of a write, that marks the parent. This happens on
  PostgreSQL when a handler swallowed a statement error and answered `2xx`:
  `RELEASE` fails with `25P02`.
  - On the capability path a refused `CompleteScope` maps by its error:
    - `ErrCalloutSuperseded` → row 1;
    - `ErrTxRollbackOnly` → row 4;
    - `ErrScopeConflict` → scope discarded, no mark, retryable
      `409 CONFLICT` (the ruled answer for two callbacks that wrote the same
      entity from the same version);
    - any other error → as the in-process completion failure above.
- **Row 8 is my call.** A read that answered (`2xx`, or a `4xx` such as `404`)
  keeps its scope. That keeps its read-set entries, so they are validated at
  commit (fail-closed). Discarding them would drop validation of data the
  compute node already holds.
- **Row 9.** On PostgreSQL a failed statement aborts the transaction, and only
  the rollback to the scope's savepoint makes it usable again.

Where the answer becomes an error, the door sends only the error: held gRPC
frames and the buffered HTTP body are dropped.

### 3. Marks and broken transactions

- **Marking.** A mark is set on a scope (rows 4 and 7). The transaction is
  rollback-only when the root is marked.
- **Callouts of a marked scope stop.** Marking a scope cancels the callouts
  begun from it, with the cause `ErrRollbackOnly`. The owner's loop then makes
  no further try or hand-over, and the callout fails with
  `TRANSACTION_ROLLBACK_ONLY`. The in-process fence already cancels nested
  callouts the same way (`takeInner`, `fence.go:107,136,160,204`). The plan
  verifies that cancelling the owner's context also ends a hand-over in progress
  on a peer pnode.
- **A processor scope stops the climb.** When an `ASYNC_NEW_TX` processor's
  callout ends with its scope marked:
  - the processor counts as failed: non-fatal, logged and recorded as today;
  - its scope is discarded with everything under it;
  - the mark goes with it, and the operation continues.
- **Every other callout site.** When a callout made from a marked scope ends,
  the processor (`SYNC`, `ASYNC_SAME_TX`), criterion or function fails with
  `TRANSACTION_ROLLBACK_ONLY`:
  - on the root, the operation fails with it;
  - on a callback scope, the callback ends by row 4.
- **A lost race comes before a mark, for the owner too.** Wherever the owner
  would report `TRANSACTION_ROLLBACK_ONLY`, it first asks
  `TransactionManager.LostRace`. If the transaction has lost, it reports the
  retryable `409 CONFLICT` (`spi.ErrTxAborted`), with the mark as context. The
  places are:
  - Seal;
  - the end of a callout made from a marked scope, at every callout site
    (processor, function criterion, schedule function), not only the processor
    path's `conflictOverDispatchFailure` (`engine_processors.go:312-321`).

  This is the reason row 2 gives: a stale snapshot may be what made the
  callback's write fail, and on a fresh transaction it may succeed.
- **Broken transaction.**
  - A Discard whose rollback fails, or a dead session (row 3), leaves the
    transaction unusable. That is an infrastructure failure, not a joined
    write's failure.
  - The root records the cause as **broken**, which is separate from marked.
  - Seal then fails with that cause, classified as today. For example a dead
    PostgreSQL session gives the owner the retryable
    `503 STORAGE_UNAVAILABLE`.
  - A broken transaction refuses further joined requests with
    `404 TRANSACTION_NOT_FOUND`.

### 4. The coordination interface (cyoda-go internal)

This is a new package that the fence, the gate, the join layer, the owner's loop,
the engine and the entity service use. The names below are suggestions; the
behaviour is what the spec fixes.

```go
type Coordination interface {
    // Owner side (the pnode running the callout's loop).
    BeginCallout(ctx context.Context, txID, calloutID string, outer []fence.Pair) (context.Context, func(), error)
    Advance(ctx context.Context, calloutID string, major uint32) error
    Seal(ctx context.Context, txID string) error // ErrRollbackOnly, or the broken cause

    // Any pnode that serves the request.
    Enter(ctx context.Context, txID string, pairs []fence.Pair, write bool) (Scope, context.Context, error)
    // An ASYNC_NEW_TX processor's scope, a child of the scope on ctx.
    OpenChild(ctx context.Context) (Scope, context.Context, error)
}

type Scope interface {
    Pause() (resume func() error) // around a nested callout
    Check() error                 // superseded? marked?
    End(o Outcome) Answer         // applies section 2's table; returns what to send
}
```

Every site that uses the fence, the gate or `Savepoint` directly moves behind it:

- `internal/callout/coordinator.go:105-110,141,161-162`
- `internal/domain/txjoin/txjoin.go:199-329`
- `internal/domain/workflow/engine_processors.go:162,266-273,344-385`
- `internal/domain/workflow/criterion_eval.go:175-182`
- `internal/domain/workflow/arm.go:271-278`
- `internal/domain/workflow/engine.go:1045` (`recordEvent`'s fence check)
- the owner's gate around every final save and commit in
  `internal/domain/entity/service.go` (346, 728, 937, 1328, 1776, 2096, 2121,
  2339, 2672, 2701)
- `internal/domain/entity/txscope.go:131`

After the change, no package outside the coordination package imports
`internal/txgate`, and only it and the doors use `fence.Check`. The plan writes
this as a greppable exit check.

### 5. In-process implementation (memory, SQLite, PostgreSQL)

#### 5.1 Enter

1. Verify the pass (as today).
2. `Join`, which checks the tenant (as today).
3. On-behalf-of check (as today).
4. `Admit` (as today).
5. Take the gate (5.3).
6. Fence check.
7. Refuse if the transaction is broken (404), sealed (410), or if the parent or
   any ancestor scope is marked (409, D-B).
8. `Savepoint`. If it fails, the request is refused: the handler did not run, so
   nothing is marked. A savepoint refused because the transaction lost a race is
   answered by row 2. Any other refusal gets the classified error, not
   retryable.
9. Handler.
10. End (section 2).
11. Release the gate.
12. The door sends the answer.

The scope ends, including its savepoint's release or rollback, while the gate is
still held.

#### 5.2 Savepoints

- **Completion and discard.** Complete is `ReleaseSavepoint` and Discard is
  `RollbackToSavepoint`.
- **Why a savepoint undoes exactly its scope.** The stack rule keeps one scope's
  writes, and its descendants' writes, contiguous. Nothing else writes in the
  transaction while a callout of the root is out: the owner's chain is blocked
  in the callout.
- **One savepoint semantics on all three backends** (fixes found while
  researching):
  - **Rolling back to a savepoint drops every savepoint taken after it.**
    Memory and SQLite keep the later records today (`plugins/memory/txmanager.go:1542`,
    `plugins/sqlite/txmanager.go:1635`), and a later rollback to one restores
    discarded writes. PostgreSQL already trims them (`plugins/postgres/txstate.go:257`).
  - **A savepoint survives its own rollback, as in SQL.** Memory and SQLite
    delete it today (`memory txmanager.go:1542`, `sqlite :1635`); PostgreSQL
    keeps it (`txstate.go:216,257`).
  - **Releasing a savepoint also releases every savepoint taken after it, as in
    SQL.** PostgreSQL's `txState.ReleaseSavepoint` removes only the one named
    (`txstate.go:308`). Memory and SQLite do the same (`memory :1577`,
    `sqlite :1669`).
  - New spitest cases pin each rule, plus "a rollback to the outer savepoint
    after a nested one was taken and released undoes everything since the
    outer". The SPI `Savepoint` text states the rules.
- **The superseded case.** `executeAsyncNewTx` no longer "abandons" the savepoint
  of a superseded chain (`engine_processors.go:366-372`): the stack rule makes
  discarding it safe.

#### 5.3 The gate and the stack rule

The gate becomes a gate with selective admission. Today's one-slot channel with
an unordered `select` (`internal/txgate/txgate.go:107,129`) cannot express it.

- It keeps the holder slot, the queue, and a stack of paused scopes.
- While a scope is paused in callout N, only requests whose pass pairs (own and
  outer) include N may queue for or take the slot. Those are N's descendants.
- **A sibling that is not a descendant is refused at once:** `503
  TRANSACTION_BUSY`, retryable. Siblings already queued when a scope pauses are
  refused at that moment. A queued sibling would hold its request, and the
  compute node's connection, for the whole of the nested callout. With a
  bounded client connection pool that stops the nested callout's own callbacks
  from being sent. **Pending decision; see the end of this document.**
- While no scope is paused, requests queue for the slot as today, with the
  waiter cap (`CYODA_CALLOUT_JOINED_MAX_WAITERS`) unchanged.
- **Resume.**
  - A paused scope resumes by taking the slot uncapped and without refusal.
  - It is then the top of the stack: its nested callout's end has waited for all
    of the callout's scopes (5.4), and those are the scopes above it.
- **Owner.** The owner's chain no longer takes the gate (5.5).

#### 5.4 The waits

They replace "take and release the gate" (`fence.go:175-178`):

- `Advance(callout, major)`: raises the number, then waits until no open scope
  admitted under that callout at a lower major remains. Each such scope ends by
  row 1. A paused one is released because its nested callouts are cancelled
  (already the case, `fence.go:160`).
- End of a callout: waits until no open scope admitted under that callout
  remains.
- Seal: waits until no open callback scope remains.

The waits never need the gate themselves. A paused scope must take the gate to
resume and end, so a wait that held the gate would deadlock.

**Consequence.** A callback still running when its callout advances or ends is
discarded and answered `410 CALLOUT_SUPERSEDED`. Today it is waited for and kept
(`TestCalloutFence_CallbackPastItsLastCheckLandsWhole`, `cluster.md:165`). A
callback that completed before the callout moved on stays kept
(`TestCalloutFence_WriteUnderTheEarlierPassIsKept`).

#### 5.5 Seal and the owner's commit sites

- Every owner commit and rollback seals first: `txScope.Commit` and
  `txScope.Release` (entity service), `flushAndCommitSegment` (CBD `TX_pre`,
  `engine_processors.go:591-658`), and `commitRun` (scheduled fire,
  `fire_scheduled.go:349-361`). The last two take no gate today.
- **Order: Seal → final save → commit.** Seal fails with `ErrRollbackOnly` or
  the broken cause. The owner then rolls back. A rollback seals and ignores the
  rollback-only answer.
- **The owner's gate goes.** After Seal no joined request can touch the
  transaction (Enter refuses a sealed transaction), so the owner's
  `h.gate.Acquire` around final save, commit and rollback has nothing left to
  exclude. The plan deletes those sites rather than keeping them as guards.
- The two commits that make no callouts (`import_tasks.go:100`,
  `engine.go:1107`) seal like every other: one rule, no exceptions.

#### 5.6 Routing

Unchanged. The pass names the owner pnode (`internal/grpc/dispatch.go:96`), and
both proxies forward to it.

### 6. Outcome capture at the doors

The join layer must know the outcome before the gate is released.

- **Outcome holder.**
  - `RunVerified` puts a holder on the joined context.
  - The shared error funnels fill it: `common.WriteError`
    (`internal/common/errors.go:403-431`) and the gRPC envelope builder
    (`internal/grpc/errors.go:66-90`).
  - The holder keeps the status, the code, the retryable flag **and the error
    itself**, so `errors.Is` works on the cause chain (rows 2 and 3).
- **Backstops**, for answers that do not pass a funnel:
  - HTTP: a buffered status of `400` or above;
  - gRPC: a non-nil handler error (raw status errors, for example
    `internal/grpc/entity.go:31,44`);
  - a panic: `RunVerified` recovers, ends the scope as a non-retryable failure,
    then re-panics. Recovery stays the outermost layer, and the gRPC recovery
    interceptor sits outside the routing interceptor
    (`internal/grpc/server.go:124-135`).
- **Write or read.** The door decides before the handler runs.
  - **HTTP:** from the route, method and path pattern, matched the way
    `internal/adminroute` matches administration routes.
  - **gRPC:** `EntityManage` and `EntityManageCollection` are writes, whatever
    the CloudEvent type and including one that does not parse. `EntitySearch`
    and `EntitySearchCollection` are reads, except `SnapshotSearch` and
    `SnapshotCancel` (section 7).
  - Entity writes (HTTP):
    - `POST /entity/{format}/{entityName}/{modelVersion}`
    - `POST /entity/{format}` and `PUT /entity/{format}`
    - `PUT` and `PATCH /entity/{format}/{entityId}[/{transition}]`
    - `DELETE /entity/{entityId}` and `DELETE /entity/{entityName}/{modelVersion}`
  - Every other route that reaches the join layer is a read or lies outside the
    transaction (section 7). The plan lists them from `api/generated.go`, and a
    test fails when a route is added without a class.
- **The response ceiling** (`413 JOINED_RESPONSE_TOO_LARGE`) is decided inside
  the handler closure (today after `RunVerified` returns:
  `internal/httpmw/txjoin_mw.go:96`, `internal/grpc/txroute_interceptor.go:190,288`),
  so it is an outcome before End.
  - It is non-retryable, so on a write it discards and marks (row 7): the
    compute node cannot know what its write did.
  - The request-body cap (`txjoin_mw.go:79`) refuses before any scope opens, so
    it marks nothing.
- **Rewritten answers.** When End rewrites the answer (rows 1-4, or a failed
  completion), the door sends the rewritten error and drops what the handler
  produced.

### 7. Joined requests outside the transaction

Some routes the join layer wraps write outside the transaction:

- messages (`POST /message/new/{subject}`, `DELETE /message/{id}`,
  `DELETE /message`);
- async search jobs (`POST /search/async/...`, `PUT /search/async/{jobId}/cancel`,
  gRPC `SnapshotSearch`, `SnapshotCancel`);
- the key-value-backed administration routes.

They are classified **outside the scope**:

- They are admitted by the fence as today
  (`TestCalloutFence_NonEntityJoinedRequests`), and D-B and the stack rule apply
  to them.
- They open no savepoint and are never rewritten by End's rows 1-4: their effect
  is not in the transaction, so the handler's answer is the truth.
- They never mark.

**The PostgreSQL message store** joins the transaction today
(`plugins/postgres/store_factory.go:264-270`, via `f.querier()`); memory and
SQLite write outside it. It gets the unjoined querier, as the key-value store has
(`store_factory.go:256-262`). Messaging has no transaction
(`internal/domain/messaging/handler.go:38-41`). The plan checks the PostgreSQL
workflow store the same way: it is built on `f.querier()` (`store_factory.go:272-279`).
If no path reaches it inside a transaction, it gets the unjoined querier too, so
the backends agree.

### 8. The optional SPI capability

In `cyoda-go-spi`. It is an SPI PR into `main`, pseudo-pinned while the milestone
is open (no tag).

```go
// TransactionCoordinationCapable is OPTIONAL on a StoreFactory, in the
// pattern of CompositeUniqueKeyCapable. A factory that implements it has
// location-independent transactions.
type TransactionCoordinationCapable interface {
    TransactionCoordinator(ctx context.Context) (TransactionCoordinator, error)
}

type CalloutPair struct {
    Callout string
    Major   uint32
    Minor   uint32
}

type TransactionCoordinator interface {
    BeginCallout(ctx context.Context, txID, calloutID string, outer []CalloutPair) error
    AdvanceCallout(ctx context.Context, txID, calloutID string, major uint32) error
    EndCallout(ctx context.Context, txID, calloutID string) error

    OpenScope(ctx context.Context, txID string, pairs []CalloutPair) (context.Context, error)
    OpenChildScope(ctx context.Context) (context.Context, error)
    CheckScope(ctx context.Context) error
    CompleteScope(ctx context.Context) error
    DiscardScope(ctx context.Context, rollbackOnly bool) error

    Seal(ctx context.Context, txID string) error
}

var (
    ErrCalloutSuperseded = errors.New("callout superseded")
    ErrTxRollbackOnly    = errors.New("transaction is rollback-only")
    ErrScopeConflict     = errors.New("scope conflicts with another scope of the transaction")
)
```

**Contract text** (in the SPI's doc comments, each line pinned by an spitest case):

0. **Not capable.** `TransactionCoordinator` returning a nil coordinator and a
   nil error means the factory is not capable. A wrapper that forwards the
   capability by hand (`CachingStoreFactory`) returns exactly that when the
   factory it wraps does not implement the interface. cyoda-go calls the method
   once at start-up, and picks the in-process implementation on a nil
   coordinator.
1. **Ownership of writes.** Every tx-path write made on a scope's context belongs
   to that scope.
2. **Completion and discard.**
   - `CompleteScope` makes the scope's writes part of its parent scope.
   - A later discard of the parent, or of any ancestor, removes them.
   - `DiscardScope` removes the scope's writes and those of every scope under it.
3. **Visibility.** A completed scope's writes are visible to later reads of its
   parent and of the parent's later scopes, from any pnode.
4. **Fencing numbers.**
   - `OpenScope` refuses with `ErrCalloutSuperseded` unless every pair is current.
   - Under the same atomic step it raises a callout's minor to the pair's minor
     when that is higher (the semantics of `fence.Admit`, `fence.go:186-216`).
   - `BeginCallout` refuses with `ErrCalloutSuperseded` when an outer pair is not
     current.
   - A pair is current when its callout is registered and not ended, its major
     equals the registered major, and its minor is not lower than the highest
     seen.
5. **Advance and end.**
   - After `AdvanceCallout(c, m)` returns, no scope admitted under `c` at a major
     below `m` can complete.
   - After `EndCallout(c)` returns, no scope admitted under `c` can complete or
     open.
   - Neither call waits for scopes in progress: a scope that was in progress
     ends discarded by its own `CompleteScope`, which refuses with
     `ErrCalloutSuperseded`.
6. **Completion order.** `CompleteScope` refuses, in this order:
   - `ErrCalloutSuperseded` when a pair of the scope is not current;
   - `ErrTxRollbackOnly` when the scope or an ancestor is marked;
   - `ErrScopeConflict` when a completed scope of the same transaction wrote an
     entity this scope wrote, from the same version.
   - A refused completion discards the scope.
7. **Rollback-only.**
   - `DiscardScope(ctx, true)` marks the scope's parent.
   - `OpenScope` refuses with `ErrTxRollbackOnly` when the parent or an ancestor
     is marked.
   - `CheckScope` reports `ErrCalloutSuperseded` and `ErrTxRollbackOnly` as
     `CompleteScope` would, without completing.
8. **Seal.**
   - `Seal` fails with `ErrTxRollbackOnly` when the root is marked.
   - After it returns, `OpenScope` refuses and `CompleteScope` refuses with
     `ErrCalloutSuperseded`.
   - Commit after a successful Seal commits the root and every completed scope.
9. **Concurrency.** `Join` on a capable store allows concurrent operations on one
   transaction from any number of goroutines and pnodes. This changes the
   SPI's `Join` text, "only one operation at a time per transaction"
   (`cyoda-go-spi/transaction.go:35-41`), for capable stores.

**Nested callouts on another pnode.** Nothing cancels a nested callout running
on another pnode when its enclosing scope is superseded or marked.

- The owner's loop for that nested callout calls `CheckScope` on its enclosing
  scope before each try and each hand-over, and stops when it fails.
- A try already in progress runs until it is answered or reaches its time limit.
  Its scope cannot complete either way.

**cyoda-go's side:**

- The coordination interface gets a second implementation over
  `TransactionCoordinator`:
  - no gate and no stack rule;
  - `Pause` is a no-op, and its resume is a `CheckScope`;
  - the waits are not needed (contract 5).
- It is chosen at start-up when the store factory implements the capability.
  `CachingStoreFactory` forwards it by hand
  (`internal/cluster/modelcache/factory.go:87-92`).
- **Routing.**
  - `mintPass` leaves the owner out of the pass (`internal/grpc/dispatch.go:92-96`).
  - Both proxies serve a pass without an owner locally
    (`internal/cluster/proxy/http.go:66-69`, `internal/cluster/proxy/grpc.go:49-75`).
  - A hand-over request accepts an empty owner (`internal/cluster/dispatch/handover.go:169`).
- **Tests.**
  - No in-tree plugin implements the capability. cyoda-go's side is tested
    against a test double: a coordinator over the memory plugin that serialises
    scopes but follows the contract text.
  - That covers selection, forwarding, routing with no owner (an e2e with two
    pnodes, the callback reaching the non-owner), and the adapter's mapping of
    each SPI error.
  - The concurrent-conflict `409` is tested in cassandra#121.

### 9. Errors

**New: `TRANSACTION_ROLLBACK_ONLY`** (`internal/common/error_codes.go`,
`knownErrorCodes`, `cmd/cyoda/help/content/errors/TRANSACTION_ROLLBACK_ONLY.md`)

| | |
|---|---|
| Status | `409 Conflict` |
| gRPC | `CLIENT_ERROR` envelope, code as message prefix |
| Retryable | no |
| Owner's message | names the code the failed callback got (same tenant) |
| Joined request's message | "the transaction, or the part of it this request would run in, can only be rolled back" |

**New, pending decision: `TRANSACTION_BUSY`** (`503`, retryable). A sibling
callback refused while another callback of the transaction is paused in a
callout of its own (5.3).

**Changed answers:**

| Answer | Today | After |
|---|---|---|
| `503 TRANSACTION_NODE_UNAVAILABLE` when the request was not sent to the owner (registry says not live, `http.go:77-83`, `txroute_interceptor.go:118-121`; the address guard refused; the dial failed) | not retryable | **retryable** |
| `503 TRANSACTION_NODE_UNAVAILABLE` after the request may have reached the owner (proxy `ErrorHandler` past the dial, `http.go:145-158`; gRPC forward timeout, `txroute_interceptor.go:155,410`) | not retryable | not retryable: the outcome is unknown, and a retry could repeat a kept write |
| A callback whose PostgreSQL session ended under it (`deadTxQuerier`, `plugins/postgres/classifying_querier.go:136-157`; `idleInTxAbortError`, `plugins/postgres/ceilings.go:150-166`) | retryable `503 STORAGE_UNAVAILABLE` | `404 TRANSACTION_NOT_FOUND`, not retryable (the owner keeps the retryable `503`) |
| A callback in a transaction that lost a write race | retryable `409 CONFLICT` | `409 CONFLICT`, not retryable (the owner keeps the retryable `409`) |
| A callback still in progress when its callout advanced or ended | its own answer, writes kept | `410 CALLOUT_SUPERSEDED`, writes discarded |
| A failed joined write (non-retryable) | its own answer; the owner may commit what it staged | its own answer; scope discarded; owner refused with `409 TRANSACTION_ROLLBACK_ONLY` |
| A failed joined request (retryable) | its own answer; the owner may commit what it staged | its own answer; scope discarded; the transaction goes on |

- **Pre-send vs post-send.** Each `TRANSACTION_NODE_UNAVAILABLE` site is sorted
  by whether the request may have been sent. The HTTP proxy's `ErrorHandler`
  tells a dial failure from a later failure by its error. The rule follows the
  callout's `NoHandOffProof` precedent: retryable only with proof that nothing
  was sent.
- **Dead PostgreSQL sessions.** The plugin makes both shapes match
  `spi.ErrTxNotFound` under `errors.Is`, while keeping the storage-unavailable
  marker that gives the owner its `503`.

### 10. Also in this change

- `docs/cloud-parity/nested-join-tx-serialisation.md:41-45` says state and audit
  writes happen only after the processor pipeline returns. The engine writes
  `STARTED` (`engine.go:302-304`) and `PROCESSING_PAUSED`
  (`engine_processors.go:103-110`) before the dispatch. Correct the text.
- `cmd/cyoda/help/content/grpc.md:656` sits next to the callback list and says
  "all `503`, retryable". Check that it cannot be read as covering
  `TRANSACTION_NODE_UNAVAILABLE`.

## Behaviour that changes (tests that flip)

- `TestCalloutFence_CallbackPastItsLastCheckLandsWhole`: discarded whole,
  answered `410`.
- `TestE2E_JoinedCallbackFailure_DoesNotRollBackOwner`: re-read against the
  rows. A non-retryable failed joined write now makes the owner fail.
- `TestCallback_ConcurrentCallbacks_Serialise`: still serialises. Siblings of a
  paused callback get `503 TRANSACTION_BUSY` (if decided).
- `TestCallback_CommitBeforeDispatchInJoinedTransaction_Refused` and
  `RunCallbackJoinedCommitBeforeDispatchRefused`:
  - the refused write is now discarded, and as a non-retryable failure of a
    joined write it makes the owner's transaction (or the enclosing
    `ASYNC_NEW_TX` processor) rollback-only;
  - `_OwnerStillCommits` flips: the owner now fails with
    `409 TRANSACTION_ROLLBACK_ONLY`.
  - The plan reads each and states the new assertion.
- Every e2e and parity scenario listed in the research under "Existing tests" is
  re-read against section 2's table. The plan names each one that changes.

## Error table

Joined requests, both doors. Scope rows apply to every route that opens a scope
(section 6); "outside" routes (section 7) get only the entry refusals.

| Request | Condition | Status | Code | Retryable |
|---|---|---|---|---|
| any joined request | parent or ancestor scope marked | 409 | `TRANSACTION_ROLLBACK_ONLY` | no |
| any joined request | another callback paused, request not its descendant | 503 | `TRANSACTION_BUSY` (pending) | yes |
| any joined request | transaction broken or session ended | 404 | `TRANSACTION_NOT_FOUND` | no |
| any joined request | owner not live / not reachable before send | 503 | `TRANSACTION_NODE_UNAVAILABLE` | yes |
| any joined request | proxied, failed after send | 503 | `TRANSACTION_NODE_UNAVAILABLE` | no |
| scope request | in progress when its callout advanced or ended | 410 | `CALLOUT_SUPERSEDED` | no |
| scope request | failed, transaction lost a write race | 409 | `CONFLICT` | no |
| scope request | marked by a descendant's failed write | 409 | `TRANSACTION_ROLLBACK_ONLY` | no |
| write | handler answered `2xx`, completion failed | 500 | `SERVER_ERROR` | no |
| write | answer over the response ceiling | 413 | `JOINED_RESPONSE_TOO_LARGE` | no |

Owner operations:

| Operation | Condition | Status / outcome | Code | Retryable |
|---|---|---|---|---|
| HTTP and gRPC entity create, create collection, update, update collection, patch, transition (loopback and named) | root marked | 409 / `CLIENT_ERROR` envelope | `TRANSACTION_ROLLBACK_ONLY` | no |
| same | transaction broken by a dead session | 503 | `STORAGE_UNAVAILABLE` | yes |
| same | transaction lost a race | 409 | `CONFLICT` | yes (unchanged) |
| scheduled fire | root marked | fire fails and rolls back, then bookkeeping | `TRANSACTION_ROLLBACK_ONLY` in the fire's failure | per bookkeeping |
| `ASYNC_NEW_TX` processor | its scope marked | processor failed (non-fatal), operation continues | recorded in `STATE_PROCESS_RESULT` | — |
| CBD `TX_pre` / `TX_post` | the segment's root marked | operation fails | `TRANSACTION_ROLLBACK_ONLY` | no |

## Coverage matrix

Legend: U unit, E e2e (`internal/e2e`, PostgreSQL), P parity (`e2e/parity`,
memory/SQLite/PostgreSQL + commercial), G gRPC envelope test
(`internal/grpc` or gRPC e2e), S spitest, M multi-node parity.

| Scenario | U | E | P | G | S | M |
|---|---|---|---|---|---|---|
| Failed joined write (non-retryable) → nothing of it kept, owner `409 TRANSACTION_ROLLBACK_ONLY`, HTTP owner + HTTP callback | | ✓ | ✓ | | | |
| Same, gRPC owner + gRPC callback | | ✓ | | ✓ | | |
| Retryable joined failure after a write → nothing kept, the retry in the same transaction succeeds, the owner commits | | ✓ | ✓ | ✓ | | |
| Failed joined read `404` → scope kept, owner commits | | ✓ | ✓ | | | |
| Later joined read and write on a rollback-only transaction → `409`, both doors | | ✓ | | ✓ | | |
| `ASYNC_NEW_TX` processor whose callback write failed → processor failed, nothing of it kept, operation commits | | ✓ | ✓ | | | |
| Owner processor modes with a marked root: SYNC, ASYNC_SAME_TX, function criterion, schedule function, CBD `TX_post` → operation `409` | | ✓ | | | | |
| Scheduled fire with a marked root → fire fails, nothing committed | | ✓ | | | | |
| Problem 2: sibling's acknowledged write survives an `ASYNC_NEW_TX` rollback (stack rule); shown RED on the unfixed code | | ✓ | | | | |
| Sibling refused `503 TRANSACTION_BUSY` while a callback is paused; descendant admitted | U (gate) | ✓ | | | | |
| Callback in progress when its callout advanced → `410`, discarded | U (waits) | ✓ | | | | |
| Superseded scope with a marked descendant → no mark climbs, operation can commit | U | ✓ | | | | |
| Lost race → callback `409` not retryable, owner `409` retryable, both doors | | ✓ | | ✓ | | |
| Panic in a joined write → scope discarded, parent marked | U | | | | | |
| Write over the response ceiling → `413`, discarded, marked | | ✓ | | ✓ | | |
| Completion fails (handler swallowed an aborting statement) → `500`, discarded | U (PostgreSQL) | | | | | |
| PostgreSQL dead session, both shapes → callback `404`, owner `503` retryable | U | ✓ | | | | |
| `TRANSACTION_NODE_UNAVAILABLE` pre-send retryable, post-send not, both doors | U | | | | | ✓ |
| Hand-over: minor numbers and a mark cancel tries on the peer | | | | | | ✓ |
| Capability path: routing with no owner, callback served on a non-owner pnode (test double) | U | | | | | ✓ |
| Capability adapter maps each SPI error (test double) | U | | | | | |
| Savepoint semantics: rollback drops later savepoints; survives own rollback; release releases later ones; outer rollback after a released nested one | | | | | S | |
| Message write under a token on PostgreSQL survives the owner's rollback (outside the transaction) | | ✓ | ✓ | | | |
| Every route that reaches the join layer has a class | U | | | | | |
| Messages and async-search writes under a token: not rewritten by a supersession | | ✓ | | | | |

Notes on the matrix:

- **Parity scenarios** avoid writes that extend a model's schema (#705).
- **Race and concurrency scenarios** (stack rule, siblings, in-progress
  supersession) are isolated single-backend e2e, never parity.
- **Every race e2e** is shown RED on the unfixed code, or dropped if it cannot go
  RED.

## Docs and contract

**Gate 4 (help, API docs, changelog):**

- `cmd/cyoda/help/content/cluster.md:165,169`: "served one at a time", the
  waiting queue, the wait-and-keep sentence.
- `cmd/cyoda/help/content/grpc.md:240-243`: one after another; "What the
  workflow did before it stays in the joined transaction".
- `cmd/cyoda/help/content/grpc.md:471-480`: write outcome after a `503`.
- `cmd/cyoda/help/content/workflows.md`: `ASYNC_NEW_TX`, failed callbacks.
- Error topics:
  - new: `TRANSACTION_ROLLBACK_ONLY`, `TRANSACTION_BUSY`;
  - changed: `TRANSACTION_NODE_UNAVAILABLE`, `CONFLICT`,
    `COMMIT_IN_JOINED_TRANSACTION` ("stays in the joined transaction" no longer
    holds), `TRANSACTION_NOT_FOUND`, `CALLOUT_SUPERSEDED`,
    `TOO_MANY_JOINED_REQUESTS`.
- The conditional delete's help, OpenAPI description and CHANGELOG state the
  rule.
- OpenAPI: response codes on the entity routes for `409` and `503` where they
  are new. Then `go generate ./api`.
- `CHANGELOG.md` `### Breaking`:
  - callback answers that changed;
  - a failed joined write dooms the owner;
  - `410` instead of keep for an in-progress callback.
- `COMPATIBILITY.md`: the SPI pin bump.
- `docs/ARCHITECTURE.md`: the transaction coordination section, audited as a
  whole on touch.

**Gate 7 (cloud parity):**

- A `docs/cloud-parity/` entry:
  - callbacks are all-or-nothing;
  - a failed joined write makes the operation fail with
    `TRANSACTION_ROLLBACK_ONLY`;
  - callbacks may run concurrently on a location-independent backend and may get
    `409 CONFLICT`;
  - the callback answer changes in the table of section 9.
- A Cyoda Cloud ticket (`[CaaS]`).

## Costs

- **Memory and SQLite.** A savepoint is a deep copy of the transaction's buffer
  (`plugins/memory/txmanager.go:1379-1399`). A transaction with N callbacks
  copies O(N × size). This is accepted with D3.
- **PostgreSQL.** Every writing callback is a subtransaction. Past 64 writing
  subtransactions in one transaction, PostgreSQL's per-backend subxid cache
  overflows, which slows snapshot taking server-wide while that transaction is
  open. This is accepted with D3. Help for operators states it.

## Cassandra

- The Cassandra plugin will fail the new savepoint spitest case. Its savepoint
  commits and publishes the transaction's writes
  (`cyoda-go-cassandra/internal/tx/tx_manager.go:732-744`), and
  `RollbackToSavepoint` does not reset `state.Ancestors` (`:910-918`).
- Record this on cassandra#121, which replaces that path with the capability.
- No release gating: the plugin has no users.

## Left to the builder

- Package and type names, and the exact shape of `Outcome` and `Answer`.
- How the gate's selective admission is built, within 5.3's rules.
- The route-class table's form, within section 6.
- The test double's form, within section 8.

## My calls (for review)

1. Row 8: a read that answered `4xx` keeps its scope (read-set validation kept).
2. Section 7: requests outside the transaction are admitted and never rewritten
   or marked, rather than refused under a token.
3. 5.5: Seal replaces the owner's gate; the gate sites are deleted.
4. 5.5: commits that make no callouts seal too.
5. Section 9: `TRANSACTION_NODE_UNAVAILABLE` is retryable only with proof that
   nothing was sent.
6. Section 3: a broken transaction is separate from a marked one, and keeps the
   cause's own classification for the owner.
7. Section 3: a lost race comes before a mark for the owner too: Seal and every
   callout site ask `LostRace` first, and answer the retryable `409 CONFLICT`.
8. Row 5: a refused `CompleteScope` on the capability path maps by its error;
   `ErrScopeConflict` is the ruled retryable `409 CONFLICT`, never a `500`.
9. Contract 0: a nil coordinator with a nil error means "not capable", so that
   a forwarding wrapper can answer for a factory that is not capable.

## Pending decision

**Siblings of a paused callback (5.3).** While callback A is paused in a callout
of its own, a sibling callback B (not A's descendant) cannot take the
transaction without breaking A's savepoint.

- **Option 1 (recommended): refuse B at once, `503 TRANSACTION_BUSY`,
  retryable.** B holds nothing; a compute node with a bounded connection pool can
  still send A's nested callbacks. B retries once A is answered.
- **Option 2: B waits in the queue until A finishes.** No new code. But B holds
  its request and connection for the whole nested callout. With a bounded client
  pool, A's nested callbacks may not be sendable at all; then A's nested callout
  runs into its time limit and fails, and the same happens on every retry.
