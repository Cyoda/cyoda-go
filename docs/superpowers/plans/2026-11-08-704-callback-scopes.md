# Callback Scopes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every compute-node callback that joins a transaction runs in an all-or-nothing scope. A non-retryable failed joined write makes the owner fail with `409 TRANSACTION_ROLLBACK_ONLY`. Transaction coordination sits behind one interface, with an in-process implementation and an optional SPI capability.

**Architecture:**
- A new package `internal/txcoord` owns the scope tree, the callout fence (`internal/fence`, kept as its leaf), the per-transaction gate (`internal/txgate`, kept as its leaf) and savepoints.
- Every other package talks to it: the join layer, both doors, the owner's callout loop, the workflow engine, the entity service and the scheduler.
- A second implementation adapts a plugin's `spi.TransactionCoordinator` when the store factory offers one.

**Tech Stack:** Go 1.27, `log/slog`, testcontainers PostgreSQL, the in-tree memory/SQLite/PostgreSQL plugins, `cyoda-go-spi` and its `spitest`.

**Spec:** `docs/superpowers/specs/2026-11-08-704-callback-scopes-design.md`. Read it first. Section and row numbers below ("row 4", "5.3") are the spec's.

## Global Constraints

- **Logging and time.** Use `log/slog` only. A real-time read carries `// realtime: <reason>`. Business time comes from `spi.Clock`.
- **No issue IDs in shipped artefacts:** code, comments, errors, logs, help, OpenAPI. Commit messages and PR bodies only.
- **Mutexes.** Every `Lock()` is followed on the next line by `defer Unlock()`. Use an IIFE for early release.
- **No test hooks in production code.** No injected counters or seams.
- **New error codes.** Each one goes in `internal/common/error_codes.go` and `knownErrorCodes`, and gets a `cmd/cyoda/help/content/errors/<CODE>.md` topic (`TestErrCode_Parity`).
- **Error messages.** A 4xx carries the code and domain detail. A 5xx carries a generic message and a ticket.
- **Tests.**
  - Iterate with `go test ./<pkg>/...` and `make test`. Never add `-count=1`. Never hand-roll `go test ./...` as verification.
  - Concurrency tests are isolated single-backend e2e (`internal/e2e`), never parity.
  - Every race e2e is shown RED on the unfixed code, or dropped.
- **Parity scenarios** avoid writes that extend a model's schema: lock the model before the scenario writes.
- **SPI changes.** They go in an SPI PR into `cyoda-go-spi` `main`, with no tag.
  - cyoda-go pins the pseudo-version in one pin commit, with `make repin-plugins` and then `go mod tidy` in each plugin.
  - The `go.work` `use` line for the local SPI stays uncommitted.
  - Update `COMPATIBILITY.md` in the pin commit.
- **Help text** uses plain, short sentences in the style of the existing topics.

## Review Focus

1. **A compute node answers its callout before its own callback returns.** The callback must end discarded and be answered `410 CALLOUT_SUPERSEDED`, and nothing of it may commit. Task 9 (unit) and Task 12 (e2e) pin it.
2. **A gRPC streaming read fails after some frames.** Only the error envelope may be sent, never frames 1…n-1. Task 11 pins it.
3. **A handler panics inside a joined write.** The scope must be discarded and the parent marked. The gate must be released, so the next callback of the transaction proceeds and is not stuck. Task 11 pins it.
4. **Repeated owner operations.** The coordination registry must hold no entry for a transaction after its commit or rollback, including after a refused Seal, so memory does not grow. Task 9 pins it.
5. **A callback for a transaction that was sealed but not yet committed** is answered `410 CALLOUT_SUPERSEDED`, never `2xx`. Task 9 pins it.

---

## Dependency map

| Stream | Tasks | Depends on |
|---|---|---|
| SPI | 1, 2 | — |
| Plugins | 3 (needs 1's pin), 4, 5 | 1 |
| Core leaves | 6, 7, 8, 10 | — |
| Core | 9 | 6, 8, 10 |
| Callers | 11 | 9, 10 |
| | 12 | 9 |
| | 13 | 9, 12 |
| Capability | 14 | 2, 13 |
| Parity | 15 | 11-13 |
| Docs | 16 | everything |

The SPI, plugin and core-leaf streams are disjoint and may run in parallel worktrees. Tasks 9 and 11-13 are sequential.

---

### Task 1: SPI — savepoint semantics, stated and pinned

**Files (repo `../cyoda-go-spi`):**
- Modify: `transaction.go` (the `Savepoint`, `RollbackToSavepoint` and `ReleaseSavepoint` doc comments, lines ~110-163)
- Modify: `spitest/transaction.go` (`runTransactionSuite`, line ~38)
- Create: `spitest/transaction_savepoint_nesting.go`
- Modify: `CHANGELOG.md` (`[Unreleased]`)

**Interfaces:**
- Produces: four conformance cases that every plugin must pass (Task 3):
  - `Savepoint/RollbackDropsLaterSavepoints`
  - `Savepoint/SurvivesOwnRollback`
  - `Savepoint/ReleaseReleasesLaterSavepoints`
  - `Savepoint/OuterRollbackAfterReleasedNested`

- [ ] **Step 1: Write the four cases** in `transaction_savepoint_nesting.go`, registered in `runTransactionSuite` after `Savepoint/ReleasedThenRolledBackKeepsLostWriteRace`. Entities are saved in one transaction `tx`.
  - `testTxSavepointRollbackDropsLater`:
    1. Take sp A, save e1, take sp B, save e2.
    2. `RollbackToSavepoint(A)`.
    3. `RollbackToSavepoint(B)` must return `ErrSavepointNotFound` (`errors.Is`).
    4. Commit. e1 and e2 are both absent.
  - `testTxSavepointSurvivesOwnRollback`:
    1. Take sp A, save e1, `RollbackToSavepoint(A)`.
    2. Save e2, then `RollbackToSavepoint(A)` again: nil.
    3. `ReleaseSavepoint(A)`: nil.
    4. Commit. Neither e1 nor e2 exists.
  - `testTxSavepointReleaseReleasesLater`:
    1. Take sp A, take sp B, save e1.
    2. `ReleaseSavepoint(A)`: nil.
    3. `RollbackToSavepoint(B)` returns `ErrSavepointNotFound`.
    4. Commit. e1 exists.
  - `testTxSavepointOuterRollbackAfterReleasedNested`:
    1. Save e0, take sp A, save e1, take sp B, save e2.
    2. `ReleaseSavepoint(B)`, save e3, `RollbackToSavepoint(A)`.
    3. Commit. e0 exists; e1, e2 and e3 do not.
- [ ] **Step 2: Doc text.** Each of the three method comments states its rule in one sentence, in SQL terms:
  - a rollback to a savepoint destroys every savepoint taken after it and keeps the savepoint itself;
  - a release destroys the savepoint and every savepoint taken after it, and keeps their work.
- [ ] **Step 3: Verify** with `go vet ./... && go build ./...` in the SPI. spitest does not run in the SPI; it runs in the plugins (Task 3). Commit:
  `feat(spitest): savepoint nesting — later savepoints die with a rollback or release`.

### Task 2: SPI — the transaction-coordination capability

**Files (repo `../cyoda-go-spi`):**
- Create: `coordination.go`
- Create: `spitest/coordination.go`
- Modify: `spitest/spitest.go` (register `runCoordinationSuite`; it skips with "factory is not TransactionCoordinationCapable" when the factory's `TransactionCoordinator` is absent or returns `(nil, nil)`)
- Modify: `transaction.go` (`Join` comment: concurrency on a capable store, contract 9)
- Modify: `CHANGELOG.md`, `README.md` (optional capabilities list)

**Interfaces:**
- Produces the spec's section 8 code block, exactly: `TransactionCoordinationCapable`, `CalloutPair`, `TransactionCoordinator`, and the sentinels `ErrCalloutSuperseded`, `ErrTxRollbackOnly`, `ErrScopeConflict`.
  - The doc comments carry contract items 0-9 of spec section 8 verbatim in substance.
  - The "nested callouts on another pnode" paragraph goes on `BeginCallout`.

- [ ] **Step 1: Write `coordination.go`.** Types and doc comments only.
- [ ] **Step 2: Write `spitest/coordination.go`.** One case per contract item 1-8. Each case begins a transaction with `TransactionManager.Begin` and drives the coordinator directly. Name each case `Coordination/<Item>`:
  - `ScopeWritesBelongToScope`: a write on an `OpenScope` context that is discarded is absent after commit.
  - `CompleteMergesIntoParent`:
    - A child under a processor scope completes, then the processor scope is discarded: the child's write is absent.
    - With the processor scope completed instead, the write is present.
  - `CompletedVisibleToParent`: after `CompleteScope`, a read on the root context sees the write.
  - `OpenScopeChecksAndAbsorbsMinor`:
    - Open with `{c,1,2}`. A later open with `{c,1,1}` returns `ErrCalloutSuperseded`.
    - An `OpenScope` with a pair under an ended callout returns `ErrCalloutSuperseded`.
  - `AdvanceStopsLowerMajor`: a scope opened at major 1, then `AdvanceCallout(c,2)`: `CompleteScope` returns `ErrCalloutSuperseded`, and the write is absent after commit.
  - `CompleteRefusalOrder`: a scope that is both superseded and under a marked parent returns `ErrCalloutSuperseded`.
  - `RollbackOnlyMarksParent`: `DiscardScope(ctx, true)`, then:
    - `OpenScope` under the same callout returns `ErrTxRollbackOnly`;
    - `Seal` returns `ErrTxRollbackOnly`.
  - `ProcessorScopeAbsorbsMark`: a callback scope under a processor scope is discarded with `rollbackOnly=true`, then the processor scope is discarded with `rollbackOnly=false`: `Seal` returns nil.
  - `SealCutsOff`: after `Seal`, `OpenScope` refuses, and `CompleteScope` of a scope opened before it returns `ErrCalloutSuperseded`.
  - `ScopeConflict`: two scopes write the same entity from the same version. The first `CompleteScope` succeeds and the second returns `ErrScopeConflict`.
- [ ] **Step 3: Verify** with `go vet ./... && go build ./...`. Commit:
  `feat: optional TransactionCoordinationCapable on StoreFactory`.
- [ ] **Step 4: Open the SPI PR into `main`** with `gh pr create` in `../cyoda-go-spi`, then merge it per `MAINTAINING.md` (no tag).
- [ ] **Step 5: Pin in cyoda-go** in one commit:
  1. `go get github.com/cyoda-platform/cyoda-go-spi@<main sha>`.
  2. `make repin-plugins`, then `go mod tidy` in each `plugins/*`.
  3. Update the SPI row in `COMPATIBILITY.md`.

  Commit: `chore(deps): pin cyoda-go-spi main (savepoint nesting, coordination capability)`.

### Task 3: Plugins — one savepoint semantics

**Files:**
- Modify: `plugins/memory/txmanager.go:1343-1580` (`Savepoint`, `RollbackToSavepoint`, `ReleaseSavepoint`)
- Modify: `plugins/sqlite/txmanager.go:1453-1680` (same three)
- Modify: `plugins/postgres/txstate.go:181-310` (`ReleaseSavepoint` drops later entries)
- Test: the plugins' existing spitest harness tests (they run `StoreFactoryConformance`)

**Interfaces:** Consumes the Task 1 cases.

- [ ] **Step 1: Run the new cases RED.** In each plugin run `go test ./... -run 'StoreFactoryConformance/.*/Savepoint/'`.
  - Expected FAIL on memory and SQLite: `RollbackDropsLaterSavepoints`, `SurvivesOwnRollback`, `ReleaseReleasesLaterSavepoints`.
  - Expected FAIL on PostgreSQL: `ReleaseReleasesLaterSavepoints`.
- [ ] **Step 2: Implement.**
  - **Memory and SQLite.** Savepoints are kept in an ordered slice per transaction instead of the unordered map (`memory txmanager.go:106`, `sqlite :129`).
    - Rollback to `id` truncates the slice to `[:idx+1]` and keeps the entry.
    - Release truncates it to `[:idx]`.
    - A lookup of a missing id returns `spi.ErrSavepointNotFound`.
  - **PostgreSQL.** `txState.ReleaseSavepoint` truncates to `[:idx]`, matching SQL `RELEASE`.
- [ ] **Step 3: Run green** in each plugin: `go test ./...`. Then run `make test` at the root.
- [ ] **Step 4: Commit:** `fix(plugins): savepoints nest as in SQL on every backend`.

### Task 4: PostgreSQL message (and workflow) store outside the transaction

**Files:**
- Modify: `plugins/postgres/store_factory.go:264-279`
- Test: `plugins/postgres/message_store_tx_test.go` (create)
- Test: `internal/e2e/callback_message_outside_tx_test.go` (create)

- [ ] **Step 1: Failing plugin test** `TestMessageStore_SaveInsideTransactionSurvivesRollback`:
  1. Begin a transaction, `MessageStore(txCtx).Save(msg)`, roll back.
  2. Assert that `Get(msg.ID)` on a fresh context finds it.

  Run it: FAIL, because the message joins the transaction.
- [ ] **Step 2: Implement.** The message store takes the unjoined querier, as the key-value store does (`store_factory.go:256-262`).
  - Search the tree for a workflow-store call that runs on a context carrying a transaction (`grep -rn "WorkflowStore(" internal app`, and check each caller's context).
  - If none does, give the workflow store the unjoined querier too, and add `TestWorkflowStore_SaveInsideTransactionSurvivesRollback` the same way.
  - If one does, leave the workflow store as it is and record the caller in the commit message.
- [ ] **Step 3: e2e** `TestCallback_MessageUnderToken_OutsideTheTransaction`. A SYNC processor's callback posts `POST /message/new/{subject}` with the token, then the compute node fails the processor (non-retryable).
  - Assert: the owner fails (status 400 `WORKFLOW_FAILED`, as today), and `GET /message/{id}` returns 200.
- [ ] **Step 4: Run** `go test ./plugins/postgres/... ./internal/e2e/ -run 'MessageStore_SaveInside|MessageUnderToken'`: PASS. Commit:
  `fix(postgres): messages never join a transaction, as on the other backends`.

### Task 5: PostgreSQL dead-transaction errors are identifiable

**Files:**
- Modify: `plugins/postgres/ceilings.go:150-166` (`idleInTxAbortError`)
- Test: `plugins/postgres/ceilings_test.go`

- [ ] **Step 1: Failing test** `TestIdleInTxAbortError_IsTxNotFound`. For an `idleInTxAbortError`:
  - `errors.Is(err, spi.ErrTxNotFound)` is true;
  - the `StorageUnavailable()` marker is still true;
  - `common`-style classification is unaffected.

  `deadTxError` already unwraps to the sentinel (`classifying_querier.go:149-158`); add the matching assertion for it as `TestDeadTxError_IsTxNotFound`.
- [ ] **Step 2: Implement** `func (e *idleInTxAbortError) Is(target error) bool { return target == spi.ErrTxNotFound }`.
- [ ] **Step 3: Run** `go test ./plugins/postgres/ -run 'IdleInTxAbortError|DeadTxError'`: PASS. Commit:
  `fix(postgres): a reclaimed transaction reads as not found`.

### Task 6: Error codes `TRANSACTION_ROLLBACK_ONLY` and `TRANSACTION_BUSY`

**Files:**
- Modify: `internal/common/error_codes.go` (constants and `knownErrorCodes`)
- Create: `internal/common/txcoord_errors.go`
- Create: `cmd/cyoda/help/content/errors/TRANSACTION_ROLLBACK_ONLY.md`
- Create: `cmd/cyoda/help/content/errors/TRANSACTION_BUSY.md`
- Modify: `cmd/cyoda/help/content/errors.md` (index lines)
- Test: `internal/common/txcoord_errors_test.go`

**Interfaces — Produces:**

```go
const ErrCodeTransactionRollbackOnly = "TRANSACTION_ROLLBACK_ONLY"
const ErrCodeTransactionBusy = "TRANSACTION_BUSY"

// ErrRollbackOnly is the cause every TRANSACTION_ROLLBACK_ONLY error carries.
var ErrRollbackOnly = errors.New("transaction is rollback-only")

// TransactionRollbackOnly is the owner's refusal: 409, not retryable. failedCode is the code the failed callback got.
func TransactionRollbackOnly(failedCode string) *AppError
// TransactionRollbackOnlyJoined refuses a joined request under a marked scope: 409, not retryable.
func TransactionRollbackOnlyJoined() *AppError
// TransactionBusy refuses a sibling of a paused callback: 503, retryable.
func TransactionBusy() *AppError
```

Messages:
- `TransactionRollbackOnly`: `a request that joined the transaction failed with <failedCode>, so the transaction can only be rolled back`.
- `TransactionRollbackOnlyJoined`: `the transaction, or the part of it this request would run in, can only be rolled back`.
- `TransactionBusy`: `another callback of this transaction is waiting for a callout of its own — send this callback again once that one has been answered`.

- [ ] **Step 1: Failing test** `TestTxCoordErrors`. For each constructor assert `Status`, `Code`, `Retryable` and `errors.Is(err, ErrRollbackOnly)` (the two rollback-only constructors).
  - `TransactionRollbackOnly("VALIDATION_FAILED").Message` contains `VALIDATION_FAILED`.
- [ ] **Step 2: Implement.** Write the two help topics in the form of `COMMIT_IN_JOINED_TRANSACTION.md`: NAME, SYNOPSIS (status and retryable), DESCRIPTION and SEE ALSO. Content comes from spec section 9 and D-B/D-F.
  - `TRANSACTION_BUSY` says: send the callbacks of one transaction in series.
- [ ] **Step 3: Run** `go test ./internal/common/ ./cmd/cyoda/...`: PASS, including `TestErrCode_Parity`. Commit:
  `feat(errors): TRANSACTION_ROLLBACK_ONLY and TRANSACTION_BUSY`.

### Task 7: `TRANSACTION_NODE_UNAVAILABLE` retryable only before the request is sent

**Files:**
- Modify: `internal/cluster/proxy/http.go:77-83, 121-125, 145-158`
- Modify: `internal/grpc/txroute_interceptor.go:112-121, 466-513`
- Test: `internal/cluster/proxy/http_test.go`
- Test: `internal/grpc/txroute_interceptor_test.go`

- [ ] **Step 1: Failing tests.**
  - `TestHTTPRouting_OwnerNotLive_Retryable`: the registry returns `alive=false`. The body has `retryable: true`.
  - `TestHTTPRouting_ForbiddenPeerAddress_Retryable`.
  - `TestHTTPRouting_DialRefused_Retryable`: the owner address is a closed port.
  - `TestHTTPRouting_OwnerTimesOutAfterSend_NotRetryable`: an owner handler that sleeps past `ResponseHeaderTimeout`. The body has no `retryable`.
  - `TestTxRoute_OwnerNotLive_Retryable` (gRPC, `classifyRouteErr`).
  - `TestTxRoute_ForwardDeadline_NotRetryable`: a gRPC forward hitting `DeadlineExceeded` returns an envelope with `Retryable == nil`.
- [ ] **Step 2: Implement.** `.AsRetryable()` only where the request provably did not leave this pnode:
  - registry not live;
  - address-guard refusal;
  - a dial failure: `errors.As(err, &opErr) && opErr.Op == "dial"` on the HTTP proxy;
  - on gRPC, a failure of the pool's connection set-up before the call is made.

  Where proof is missing (`codes.Unavailable` or `DeadlineExceeded` from the call itself), the answer stays not retryable. Name the rule in a comment: "retryable only with proof that nothing was sent".
- [ ] **Step 2b: Multinode scenario** `RunCallback_OwnerDownIsRetryableNodeUnavailable`, on both doors, if the multinode fixture can stop a node (look for a stop/kill helper in `e2e/parity/multinode`).
  - A callback is sent to the surviving pnode after the owner was stopped. It gets 503 `TRANSACTION_NODE_UNAVAILABLE` with `retryable: true`.
  - If the fixture has no such helper, the unit tests above carry the cell. Record that under "Coverage waivers".
- [ ] **Step 3: Run** `go test ./internal/cluster/proxy/ ./internal/grpc/ -run 'OwnerNotLive|ForbiddenPeer|DialRefused|AfterSend|ForwardDeadline'`: PASS. Commit:
  `fix(cluster): an owner that was never reached is a retryable 503`.

### Task 8: The gate with selective admission

**Files:**
- Modify: `internal/txgate/txgate.go` (rewrite the API; keep the package a leaf)
- Test: `internal/txgate/txgate_test.go` (replace the cases of removed functions; keep the cap and AtCapacity cases)
- Delete: `internal/txgate/suspend_call_sites_test.go` (its subject moves to Task 12's exit check)

**Interfaces — Produces** (consumed only by `internal/txcoord`):

```go
var ErrTooManyWaiters = errors.New(...)   // unchanged
var ErrNotAdmitted = errors.New("the transaction's gate does not admit this caller now")

// AcquireJoined takes the slot for a joined request. admitted is evaluated under
// the registry's lock on entry and again each time Reevaluate is called while the
// caller queues; when it is false the caller gets ErrNotAdmitted at once, holding
// nothing. maxWaiters as AcquireCtx today.
func (r *Registry) AcquireJoined(ctx context.Context, txID string, maxWaiters int, admitted func() bool) (release func(), err error)
// Reevaluate re-runs admitted for every caller queued on txID; those now refused return ErrNotAdmitted.
func (r *Registry) Reevaluate(txID string)
// AcquireResume takes the slot for a paused scope resuming: uncapped, never refused, never cancelled.
func (r *Registry) AcquireResume(txID string) (release func())
func (r *Registry) AtCapacity(txID string, maxWaiters int) bool // unchanged
```

Removed: `Acquire`, `AcquireCtx`, `WithHeld`, `Suspend`, `held`. Their callers move in Tasks 9-13.

- [ ] **Step 1: Failing tests.**
  - `TestAcquireJoined_NotAdmittedOnEntry`: `admitted` returns false. `ErrNotAdmitted` comes back at once, and `AtCapacity` reads the queue as empty afterwards.
  - `TestAcquireJoined_QueuedThenRefusedOnReevaluate`:
    1. The holder holds the slot.
    2. A second caller queues with `admitted` true, then the test flips it to false and calls `Reevaluate`.
    3. The queued caller gets `ErrNotAdmitted`. The holder still holds the slot.
  - `TestAcquireResume_IgnoresCapAndAdmission`: with `maxWaiters` waiters queued, `AcquireResume` still takes the slot once it is released.
  - `TestAcquireJoined_CtxEndsWhileQueued`: returns `ctx.Err()` and holds nothing (keep today's behaviour).
  - Keep the existing cap tests, ported to `AcquireJoined` with `admitted` always true.
- [ ] **Step 2: Implement.** Replace the one-slot channel with a slot flag and a FIFO of waiters, each waiting on its own channel. Grant the slot to the first admitted waiter. Reevaluate refuses the waiters no longer admitted. Keep the `gate.refs` clean-up.
- [ ] **Step 3: Run** `go test ./internal/txgate/`: PASS. The callers do not compile yet; do not commit until Task 9 compiles. Tasks 8 and 9 land as one commit pair on one green build. If executed separately, keep the old functions as thin wrappers until Task 13's exit check deletes them.

### Task 9: `internal/txcoord` — scope tree and in-process coordination

**Files:**
- Create: `internal/txcoord/txcoord.go`: package doc, `RequestClass`, `Coordination`, `Scope`, and the ctx helpers `Pause`, `Check`, `Pairs`
- Create: `internal/txcoord/inprocess.go`: `New(txMgr spi.TransactionManager, f *fence.Fence, gate *txgate.Registry) Coordination` and the tree, waits, marks and broken state
- Create: `internal/txcoord/end.go`: section 2's table as `(*callbackScope).End`
- Modify: `internal/fence/fence.go`. Remove `wait` and the gate field: `New()` takes no argument. `Begin`, `Advance` and `end` no longer wait; txcoord does. Keep `Admit`, `Check`, `Pairs`, `currentLocked` and `takeInner`.
- Test: `internal/txcoord/inprocess_test.go`, `internal/txcoord/end_test.go`, and an adapted `internal/fence/fence_wait_test.go` (the wait cases move to txcoord)

**Interfaces — Produces:**

```go
type RequestClass uint8
const ( Read RequestClass = iota + 1; Write; Outside )

type Coordination interface {
	// BeginCallout registers callout calloutID on txID under the scope on ctx (the
	// root of txID when ctx carries none) and the pairs ctx was admitted under. The
	// returned ctx is cancelled with cause fence.ErrSuperseded when an outer pair
	// stops being current, and with cause common.ErrRollbackOnly when the scope it
	// was begun from is marked. end unregisters the callout and waits until no open
	// scope admitted under it remains; idempotent.
	BeginCallout(ctx context.Context, txID, calloutID string) (context.Context, func())
	// Advance raises the callout's major, then waits until no open scope admitted
	// under it at a lower major remains.
	Advance(calloutID string, major uint32)
	// Seal waits until no callback scope of txID is open, then cuts the transaction
	// off: Enter refuses it from then on. err is, in order: spi.ErrTxAborted
	// (wrapped) when LostRace says the transaction lost; the broken cause; or
	// common.TransactionRollbackOnly(code) when the root is marked. finish forgets
	// the transaction; the caller defers it after its commit or rollback, also when
	// err != nil. Seal of a transaction never seen returns (finish, nil).
	Seal(ctx context.Context, txID string) (finish func(), err error)
	// Enter opens a callback scope (5.1 steps 5-8). Refusals are *common.AppError.
	Enter(ctx context.Context, txID string, pairs []fence.Pair, class RequestClass, maxWaiters int) (Scope, context.Context, error)
	// OpenChild opens an ASYNC_NEW_TX processor scope under the scope on ctx.
	OpenChild(ctx context.Context) (Scope, context.Context, error)
	// AtCapacity is the reading Joiner.CheckRoom takes before a door reads its
	// body (txgate.AtCapacity in-process; always false on the durable path).
	AtCapacity(txID string, maxWaiters int) bool
}

type Scope interface {
	// End ends a callback scope by the spec's section 2 table: the rewritten
	// answer, or nil to send the handler's own. Releases the gate.
	End(o common.Outcome) *common.AppError
	// Close ends a processor scope. procErr nil and the scope current and
	// unmarked: kept, returns nil. Otherwise discarded; returns procErr, or the
	// scope's own refusal (410, or 409 TRANSACTION_ROLLBACK_ONLY after
	// LostRace), or an error carrying workflow.ErrSavepointInfra-equivalent
	// txcoord.ErrScopeInfra when the savepoint could not be released or undone.
	Close(procErr error) error
}

var ErrScopeInfra = errors.New("transaction scope could not be ended")

// Pause marks the scope on ctx paused (no-op on a root or with no scope) and
// releases the gate it holds; resume retakes it (AcquireResume) and returns
// Check(ctx). Callers defer resume and call it explicitly before touching the
// transaction.
func Pause(ctx context.Context) (resume func() error)
// Check reports, for the scope on ctx and its ancestors: 410 CALLOUT_SUPERSEDED
// when a pair is not current; else, when a scope on the chain is marked, the
// lost-race conflict (spi.ErrTxAborted) if LostRace says so, otherwise
// common.TransactionRollbackOnly(code). nil for a ctx with no scope.
func Check(ctx context.Context) error
// Pairs returns the pairs the callback scope on ctx was admitted under (outer of
// a callout begun from it); nil otherwise. Replaces fence.Pairs for callers.
func Pairs(ctx context.Context) []fence.Pair
```

`common.Outcome` comes from Task 10, which lands first.

**In-process rules** (spec 5.1-5.5, section 3):
- **Per-transaction state:** root, open scopes, sealed, broken, and calloutID → parent scope.
- **Admission.** `admitted` for the gate is: no scope paused, or the top paused scope is an ancestor of the new scope's parent chain. A refusal maps to `common.TransactionBusy()`.
- **Pausing.** `Pause` pushes the scope, releases the slot, and calls `gate.Reevaluate`.
- **Marking.** Marking a scope cancels, with cause `common.ErrRollbackOnly`, every callout registered with it as parent.
- **Savepoints.** Enter takes `txMgr.Savepoint` after the gate (Outside: none). `End` and `Close` release or roll back.
- **A failed rollback** sets broken, holding the cause.

- [ ] **Step 1: Failing table tests** in `end_test.go`, one per spec section 2 row. They use the memory plugin's transaction manager (`plugins/memory` is importable from tests via the root module's `replace`). If it is not importable, use a fake `spi.TransactionManager` recording `Savepoint`, `Release` and `Rollback` calls and `LostRace` answers.
  - `TestEnd_Row1_SupersededDiscardsNoMark`:
    1. Advance the callout after Enter.
    2. `End(Outcome{Status:200})` returns 410 `CALLOUT_SUPERSEDED`.
    3. The savepoint was rolled back. The parent is not marked: a following `Seal` returns nil.
  - `TestEnd_Row1_SupersededWithMarkedDescendantNoClimb`: the scope is marked and superseded: 410, and `Seal` returns nil.
  - `TestEnd_Row2_LostRaceNotRetryable`: `LostRace` is true and the outcome is a 400 write: 409 `CONFLICT` with `Retryable == false`, and no mark.
  - `TestEnd_Row3_DeadSession404`: `Outcome{Err: wrapped spi.ErrTxNotFound, Status:503}` → 404 `TRANSACTION_NOT_FOUND`, not retryable. `Seal` returns the cause, and `common.Internal(...)` of it is 503 retryable.
  - `TestEnd_Row4_MarkedScope409AndClimbsForWrite`; `TestEnd_Row4_ReadDoesNotClimb`.
  - `TestEnd_Row5_SuccessReleases`. `TestEnd_Row5_ReleaseFails500AndMarks`: the fake's `ReleaseSavepoint` errors, on a write → 500 `SERVER_ERROR`, and the parent is marked.
  - `TestEnd_Row6_RetryableDiscardsNoMark`; `TestEnd_Row7_NonRetryableWriteMarksParent` (`Seal` → 409 `TRANSACTION_ROLLBACK_ONLY` naming the failed code); `TestEnd_Row8_Read4xxKeeps`; `TestEnd_Row9_Read5xxDiscards`.
  - `TestEnd_Outside_NeverRewrittenNeverMarks`: an Outside scope that is superseded returns nil (no rewrite), and the parent is not marked.
  - `TestEnd_PanicOutcome_DiscardsAndMarks`: `Outcome{Panicked:true}` on a write.
- [ ] **Step 2: Failing behaviour tests** in `inprocess_test.go`:
  - `TestEnter_RefusedUnderMarkedAncestor`: 409 `TRANSACTION_ROLLBACK_ONLY`, `Retryable == false`.
  - `TestEnter_SiblingOfPausedScopeIsBusy`:
    1. Scope A pauses.
    2. Sibling B's Enter returns 503 `TRANSACTION_BUSY`.
    3. Descendant D, whose pass outer names A's nested callout, enters.
  - `TestEnter_QueuedSiblingRefusedWhenScopePauses`.
  - `TestEnter_SealedTransactionRefused410`.
  - `TestAdvance_WaitsForLowerMajorScopes`: Advance returns only after the open major-1 scope ended. That scope's End returns 410.
  - `TestCalloutEnd_WaitsForScopesAndCancelsNested`: a scope paused in a nested callout. The outer callout's end cancels the nested ctx, the scope resumes and ends 410, and end returns.
  - `TestCalloutEnd_AnsweredBeforeCallbackReturned` (Review Focus 1): end runs while a callback is mid-handler. Its End returns 410 and its savepoint is rolled back.
  - `TestMark_CancelsCalloutsBegunFromScope`: the ctx returned by `BeginCallout` is cancelled with cause `common.ErrRollbackOnly`.
  - `TestSeal_LostRaceBeforeMark`: root marked and `LostRace` true → `errors.Is(err, spi.ErrTxAborted)`.
  - `TestSeal_FinishForgetsTransaction` (Review Focus 4): after `finish()` the internal map holds no entry. Read it through an unexported `len()` in an in-package test, not a production hook.
  - `TestOpenChild_CloseMarkedAbsorbs`: a callback under a processor scope fails a write, so the processor scope is marked. `Close(nil)` returns a `TRANSACTION_ROLLBACK_ONLY` error, the scope is discarded, and `Seal` returns nil.
  - `TestPause_RootIsNoOp`.
- [ ] **Step 3: Run** `go test ./internal/txcoord/ ./internal/fence/`: FAIL (no package).
- [ ] **Step 4: Implement** `txcoord.go`, `inprocess.go` and `end.go` to the interfaces and rules above. Adapt `fence.New` and its tests.
- [ ] **Step 5: Run** `go test ./internal/txcoord/ ./internal/fence/ ./internal/txgate/`: PASS. Commit Tasks 8 and 9 together:
  `feat(txcoord): callback scopes, the stack rule and Seal behind one coordination interface`.

### Task 10: The outcome holder in the error funnels

**Files:**
- Create: `internal/common/outcome.go`
- Modify: `internal/common/errors.go:403-431` (`WriteError` records)
- Modify: `internal/grpc/errors.go:66-90` (`buildErrorFields` records the classified `appErr`)
- Test: `internal/common/outcome_test.go`, `internal/grpc/errors_test.go`

**Interfaces — Produces:**

```go
type Outcome struct {
	Status    int   // 0: nothing recorded
	Code      string
	Retryable bool
	Err       error // the answered error, cause chain intact
	Panicked  bool
}
// WithOutcome installs a holder on ctx.
func WithOutcome(ctx context.Context) (context.Context, *Outcome)
// RecordOutcome records appErr on ctx's holder; no-op without one; the first failure recorded wins.
func RecordOutcome(ctx context.Context, appErr *AppError)
```

- [ ] **Step 1: Failing tests.**
  - `TestRecordOutcome_FirstFailureWins`.
  - `TestWriteError_RecordsOutcome`: status, code, retryable, and `errors.Is(o.Err, cause)`.
  - `TestBuildErrorFields_RecordsOutcome`: a raw `spi.ErrConflict` records `Code: CONFLICT`, `Retryable: true`.
- [ ] **Step 2: Implement.**
- [ ] **Step 3: Run** `go test ./internal/common/ ./internal/grpc/ -run 'Outcome'`: PASS. Commit:
  `feat(common): error funnels record the answered outcome`.

### Task 11: The join layer and both doors

**Files:**
- Modify: `internal/domain/txjoin/txjoin.go`:
  - `Joiner` holds a `txcoord.Coordination` instead of the fence and gate;
  - `NewJoiner(signer, txMgr, coord txcoord.Coordination, maxResponseBytes, maxWaiters int, meter)`;
  - `RunVerified` and `Run` change as below.
- Create: `internal/httpmw/routeclass.go`, the HTTP route classes (spec section 6 list; Outside = section 7)
- Modify: `internal/httpmw/txjoin_mw.go:46-105`
- Modify: `internal/httpmw/buffered_writer.go` (expose the status and `over` to the closure)
- Modify: `internal/grpc/txroute_interceptor.go:130-195, 228-301` (class per RPC/CE type; the ceiling inside the closure; held frames dropped on a rewrite or a handler failure)
- Modify: `app/app.go:422-425, 579` (construct `txcoord.New(...)`, pass it to `NewJoiner`)
- Test: `internal/domain/txjoin/txjoin_test.go`, `run_test.go`
- Test: `internal/httpmw/routeclass_test.go`
- Test: `internal/grpc/txroute_interceptor_test.go`
- Test (e2e): `internal/e2e/callback_scope_test.go` (create)

**Interfaces:**
- Consumes `txcoord.Coordination`, `Scope.End` and `common.Outcome`.
- Produces:

```go
// RunVerified: verify → Join → OBO → coord.Enter (admits, gates, savepoint) →
// handler (panics recovered: End(Outcome{Panicked:true}) then re-panic) →
// scope.End(merged outcome). The outcome recorded by a funnel wins over the
// door's backstop. rewrite non-nil: the door sends rewrite and drops what the
// handler produced.
func (j *Joiner) RunVerified(ctx context.Context, pass *Pass, class txcoord.RequestClass, handler func(ctx context.Context) common.Outcome) (rewrite *common.AppError, err error)
func (j *Joiner) Run(ctx context.Context, tok string, class txcoord.RequestClass, handler func(ctx context.Context) common.Outcome) (*common.AppError, error)

// internal/httpmw
func joinedRequestClass(r *http.Request) txcoord.RequestClass // 0 for an unknown route
```

- **The HTTP backstop:** the buffered status (400 or above gives `Status` and `Retryable: false`) and `over` (gives the ceiling's `ResponseTooLargeError()` as the outcome).
- **The gRPC backstop:**
  - a non-nil `herr` gives `Status: 500` unless it is a `status` error (InvalidArgument gives 400);
  - a unary answer over the ceiling, or `held.tooLarge()`, gives the ceiling error;
  - a stream frame carrying `Success:false` was already recorded by `buildErrorFields`.

- [ ] **Step 1: Failing unit tests.**
  - `TestJoinedRequestClass_EveryRouteClassified`: walk the chi router `internal/api.NewChiMux` builds (with `chi.Walk`). Every method and pattern returns a non-zero class, and each spec section 6 write route returns `Write`. Each section 7 route returns `Outside`.
  - `TestRunVerified_PanicDiscardsAndReleasesGate` (Review Focus 3): the handler panics; RunVerified re-panics; a second RunVerified on the same transaction then acquires and runs.
  - `TestTxRoute_StreamReadFailsMidway_OnlyErrorSent` (Review Focus 2): a streaming handler sends 2 frames, then fails. The client receives exactly one frame, the error envelope.
  - `TestTxRoute_RewriteDropsHeldFrames`.
- [ ] **Step 2: Failing e2e** in `callback_scope_test.go`, on the `newCalloutHarness` / `AttachCnode` pattern of `callout_fencing_test.go`.
  - **Model setup.** Model `p` has a SYNC processor whose compute node makes the callbacks below. Model `x` has a SYNC processor `xproc` on a second tag, whose compute node answers as the test directs.
  - `TestCallbackScope_FailedJoinedWrite_OwnerRollbackOnly_HTTP`:
    1. The callback creates `x`; `xproc` answers `success:false`, not retryable. The callback gets 400 `WORKFLOW_FAILED`.
    2. The `p` compute node then answers success.
    3. Assert: owner 409, `errorCode == TRANSACTION_ROLLBACK_ONLY`, no `retryable`, and the message contains `WORKFLOW_FAILED`.
    4. No `x` entity and no audit event for it.
    5. No `p` entity.
  - `TestCallbackScope_FailedJoinedWrite_OwnerRollbackOnly_GRPC`: the same through gRPC on both sides. The owner gets a `CLIENT_ERROR` envelope with the message prefix `TRANSACTION_ROLLBACK_ONLY` and `Retryable == nil`.
  - `TestCallbackScope_RetryableFailure_RetriedInTransaction`:
    1. The first callback's `xproc` answers a retryable failure, so the callback gets 400 with `retryable: true`.
    2. The compute node sends the same callback again, and `xproc` succeeds.
    3. Assert: owner 200, exactly one `x`, and the audit holds events for one run only.
  - `TestCallbackScope_FailedRead404_TransactionGoesOn`: the callback GETs an unknown id and gets 404. The owner gets 200.
  - Later joined requests on a rollback-only transaction: carried by unit tests on both doors (see Coverage waivers).
  - `TestCallbackScope_LostRace_CallbackNotRetryable_OwnerRetryable`: adapt `joined_callback_conflict_test.go`'s set-up.
    - The callback's update loses to a concurrent commit and gets 409 `CONFLICT` with no `retryable`.
    - The owner gets 409 `CONFLICT` with `retryable: true`.
    - Cover both doors.
  - `TestCallbackScope_WriteOverResponseCeiling_413AndMarked`: set `CYODA_CALLOUT_JOINED_RESPONSE_MAX_BYTES` small. A collection-create callback gets 413, and the owner gets 409 `TRANSACTION_ROLLBACK_ONLY`. Cover both doors.
  - `TestCallbackScope_PostgresSessionReclaimed`: set `idle_in_transaction_session_timeout` low for the test database. The callback waits past it, then writes.
    - The callback gets 404 `TRANSACTION_NOT_FOUND` with no `retryable`.
    - The owner gets 503 `STORAGE_UNAVAILABLE` with `retryable: true`.
- [ ] **Step 3: Run RED:** `go test -timeout 30m ./internal/e2e/ -run CallbackScope` and the unit tests. Expected: the e2e fail on the owner's status (200 or 503 instead of 409), and the unit tests fail to compile.
- [ ] **Step 4: Implement** the join layer, the route classes and both doors.
- [ ] **Step 5: Flip the existing tests this changes** (each a RED before Step 4):
  - `TestCallback_CommitBeforeDispatchInJoinedTransaction_OwnerStillCommits` (`callback_joined_commit_test.go`): the owner now gets 409 `TRANSACTION_ROLLBACK_ONLY`. Rename it `_OwnerRollbackOnly`.
  - `TestE2E_JoinedCallbackFailure_DoesNotRollBackOwner` (`tx_lifecycle_e2e_test.go`): read its failure kind. A non-retryable write failure now fails the owner; a retryable one keeps today's assertion. Rename to match.
  - `TestCallbackErr_LoudFailCodes` (`callback_txjoin_errors_test.go`): re-check each code's `retryable` against spec section 9.
- [ ] **Step 6: Run** `go test ./internal/domain/txjoin/ ./internal/httpmw/ ./internal/grpc/` and `go test -timeout 30m ./internal/e2e/ -run 'CallbackScope|Callback_|CalloutFence|JoinedCallback|E2E_Joined'`: PASS. Commit:
  `feat(txjoin): every joined request runs in a scope; a failed joined write dooms the owner`.

### Task 12: Engine and owner's callout loop on the coordination interface

**Files:**
- Modify: `internal/domain/workflow/engine_processors.go:162, 266-273, 340-389`. `executeAsyncNewTx` uses `OpenChild` and `Close`, and no longer calls `Savepoint` directly or abandons a savepoint. `ErrSavepointInfra` is matched through `txcoord.ErrScopeInfra`.
- Modify: `internal/domain/workflow/criterion_eval.go:175-182`, `arm.go:271-278`. `txgate.Suspend` + `fence.Check` become `resume := txcoord.Pause(ctx); defer resume() … if err := resume(); err != nil { return …, err }`.
- Modify: `internal/domain/workflow/engine.go:1045`: `txcoord.Check(ctx) != nil`.
- Modify: `internal/callout/coordinator.go:100-170, 455-470`:
  - `Coordinator` holds `txcoord.Coordination`;
  - `BeginCallout` replaces `fence.Begin`, and `Advance` replaces `fence.Advance`;
  - `ended` maps the cause `common.ErrRollbackOnly` to `txcoord.Check(ctx)`'s error (lost race first).
- Modify: `app/app.go:532` (pass the coordination to `callout.New`)
- Test: `internal/domain/workflow/engine_savepoint_test.go` (adapt), `internal/callout/coordinator_test.go`
- Test (e2e): `internal/e2e/callback_scope_engine_test.go` (create)

- [ ] **Step 1: Failing e2e.**
  - `TestCallbackScope_AsyncNewTxAbsorbsFailedCallbackWrite`:
    1. The owner's `ASYNC_NEW_TX` processor's callback fails a write non-retryably.
    2. The compute node answers the processor success.
    3. Assert: owner 200. The processor's `STATE_PROCESS_RESULT` is `failed`. Nothing of the callback exists.
  - `TestCallbackScope_SiblingAckedWriteSurvivesAsyncNewTxRollback` (problem 2):
    1. Callback A's write runs an `ASYNC_NEW_TX` processor. While it is out, sibling B (sent by the same compute node) creates `y`.
    2. A's processor fails.
    3. Assert: every callback answered 2xx has its entity committed. B is either answered 503 `TRANSACTION_BUSY` and retried until 200, or answered 200, and `y` exists in both cases.
    4. **Run it on the commit before Task 9 to show it RED** (B 200 and `y` missing). Record the RED run's output in the commit message.
  - `TestCallbackScope_SiblingOfPausedCallbackBusy`: a sibling sent while A is paused gets 503 `TRANSACTION_BUSY` with `retryable: true`; a retry after A's answer succeeds.
  - `TestCallbackScope_InProgressWhenCalloutAdvances_410`: flip `TestCalloutFence_CallbackPastItsLastCheckLandsWhole` (`callout_fencing_wait_test.go:286`). The collection callback is answered 410 `CALLOUT_SUPERSEDED`, and neither victim is updated. Rename it `_DiscardedWhole`.
  - `TestCallbackScope_AnsweredBeforeCallbackReturned_410` (Review Focus 1): the compute node answers the processor while its own callback is still in the handler (held by a row lock as in `holdRowLock`). The callback gets 410, and nothing of it commits.
  - Marked-root owner modes, each asserting owner 409 `TRANSACTION_ROLLBACK_ONLY`:
    - `TestCallbackScope_OwnerModes_SyncProcessor`
    - `…_AsyncSameTx`
    - `…_FunctionCriterion`
    - `…_ScheduleFunction`
    - `…_CBDTxPost`
  - `TestCallbackScope_MarkStopsFurtherTries`: a processor with `FIXED` retries whose first member's callback fails a write. The second member is never called out, so its script records no callout.
  - Multinode scenario `RunCallout_HandOverStopsOnMark` (`e2e/parity/multinode/callout_handover.go` pattern), registered in the multinode registry:
    - The callout is handed over to the peer's member, whose callback fails a write non-retryably.
    - The peer makes no further try: its second member records no callout.
    - The owner gets 409 `TRANSACTION_ROLLBACK_ONLY`.
    - Check here that cancelling the owner's callout context ends a hand-over in progress on the peer. If it does not, make it do so in `internal/cluster/dispatch` in this task.
- [ ] **Step 2: Run RED:** `go test -timeout 30m ./internal/e2e/ -run CallbackScope_`.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Unit tests.**
  - `TestCoordinator_RollbackOnlyCauseMapsTo409`.
  - `TestExecuteAsyncNewTx_SupersededDiscardsScope`: replaces the abandon test in `engine_savepoint_test.go`.
- [ ] **Step 5: Run** `go test ./internal/domain/workflow/ ./internal/callout/` and the e2e above: PASS. Run `make test`. Commit:
  `feat(workflow): callouts and ASYNC_NEW_TX processors run in coordination scopes`.

### Task 13: Owner commit sites seal; the owner's gate goes

**Files:**
- Modify: `internal/domain/entity/txscope.go`:
  - add `func (s *txScope) Seal() error`, which calls `coord.Seal` and keeps `finish`;
  - `Commit` requires a prior Seal;
  - `Release` seals (ignoring the answer), rolls back, then finishes;
  - drop the gate.
- Modify: `internal/domain/entity/handler.go:90-97` (`New` takes `txcoord.Coordination` instead of `*txgate.Registry`)
- Modify: `internal/domain/entity/service.go`. At every gate site (346, 728, 937, 1328, 1776, 2096, 2121, 2339, 2672, 2701):
  - delete `defer h.gate.Acquire(...)()`;
  - before the final save, add `if err := scope.Seal(); err != nil { return classifyCommitErr(err) }`;
  - the per-item collection saves at 2096 and 2672 take no gate and no seal (the seal comes before the collection's commit).
  - `classifyCommitErr` passes an `*AppError` through, maps `spi.ErrConflict` and `spi.ErrTxAborted` as today, else `common.Internal`.
- Modify: `internal/domain/workflow/engine_processors.go:591-658` (`flushAndCommitSegment` seals `TX_pre` first)
- Modify: `internal/domain/workflow/fire_scheduled.go:349-361` (`commitRun` seals; a rollback-only answer is the fire's failure)
- Modify: `internal/domain/workflow/engine.go:1088-1114` (`recordEventInOwnTx` seals)
- Modify: `internal/domain/entity/import_tasks.go:75-104` (seals)
- Modify: `app/app.go` (wiring; delete `a.txGate` from the App struct)
- Create: `internal/txcoord/exitcheck_test.go`
- Test (e2e): `internal/e2e/callback_scope_owner_test.go`

- [ ] **Step 1: Failing exit check** `TestOnlyTxcoordUsesGateAndFence`. It parses every non-test `.go` file under the root module (`go/parser`, imports only).
  - Fail when a package other than `internal/txcoord` imports `internal/txgate`.
  - Fail when a package other than `internal/txcoord` and `internal/cluster/token` (which uses `fence.Pair`) imports `internal/fence`.
  - Run it: FAIL (`entity`, `app`, `workflow`, …).
- [ ] **Step 2: Failing e2e.**
  - `TestCallbackScope_ScheduledFire_RootMarked`:
    1. A scheduled transition's SYNC processor's callback fails a write.
    2. The fire fails: the task records a failed attempt naming `TRANSACTION_ROLLBACK_ONLY`.
    3. Nothing of the fire is committed: the entity is still in its state and has no new audit event.
  - `TestCallbackScope_OwnerOps_RootMarked`: table over the HTTP create, create collection, update (loopback), update collection, patch and named transition, plus gRPC `EntityManage` create/update/transition and `EntityManageCollection` create/update. Each asserts 409 `TRANSACTION_ROLLBACK_ONLY` (`CLIENT_ERROR` and prefix on gRPC) and nothing committed.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test ./internal/txcoord/ ./internal/domain/... ./app/...` and `go test -timeout 30m ./internal/e2e/ -run 'CallbackScope|Scheduled|Sched'`: PASS. Run `make test` and `go vet ./...`. Commit:
  `refactor(entity): the owner seals before every commit; the owner's gate is gone`.

### Task 14: The capability path

**Files:**
- Create: `internal/txcoord/durable.go`: `NewDurable(c spi.TransactionCoordinator, txMgr spi.TransactionManager) Coordination`
  - no gate, no stack rule, no waits;
  - `Pause` returns a `resume` that is `Check`;
  - `End` maps a refused `CompleteScope` by spec row 5's capability list.
- Create: `internal/txcoord/select.go`: `func Select(ctx context.Context, factory spi.StoreFactory, inProcess func() Coordination) (Coordination, bool, error)`. The bool reports location independence: `true` when the factory offers a non-nil coordinator.
- Modify: `internal/cluster/modelcache/factory.go:87-92` (forward `TransactionCoordinator`; `(nil, nil)` when the inner factory does not implement it)
- Modify: `internal/grpc/dispatch.go:88-109` (`mintPass` leaves `NodeID` empty when location-independent; drop the empty-owner refusal there)
- Modify: `internal/cluster/proxy/http.go:66-69`, `internal/cluster/proxy/grpc.go:49-75` (an empty `NodeID` is served locally)
- Modify: `internal/cluster/dispatch/handover.go:169` (an empty `OwnerNodeID` is accepted only when this pnode is location-independent)
- Modify: `app/app.go` (call `Select` after the factory is built; pass the bool to the dispatcher and the hand-over server)
- Create: `internal/txcoord/txcoordtest/double.go`. A test double: `NewFactory(inner spi.StoreFactory) spi.StoreFactory` whose `TransactionCoordinator` follows the SPI contract over a memory factory, serialising scopes with one mutex per transaction and taking a savepoint per scope.
- Create: `internal/txcoord/txcoordtest/double_conformance_test.go` (runs Task 2's `spitest` coordination suite on the double)
- Create: `e2e/parity/multinode/callback_location_independent.go`, registered in the multinode registry. It skips with "backend is not location-independent" unless the fixture's backend offers the capability. It runs on Cassandra later.
- Test: `internal/txcoord/durable_test.go`, `select_test.go`; `internal/cluster/modelcache/factory_test.go`; `internal/grpc/dispatch_test.go`; `internal/cluster/proxy/http_test.go`, `grpc_test.go`; `internal/cluster/dispatch/handover_test.go`

- [ ] **Step 1: Failing tests.**
  - `TestCachingFactory_ForwardsCoordinator` and `TestCachingFactory_NotCapableReturnsNilNil`.
  - `TestSelect_PicksDurableWhenCapable` and `TestSelect_InProcessOnNilCoordinator`.
  - `TestMintPass_NoOwnerWhenLocationIndependent`.
  - `TestHTTPRouting_PassWithoutOwnerServedLocally` and `TestResolveNodeInfo_PassWithoutOwnerLocal`.
  - `TestHandOver_EmptyOwnerAcceptedOnlyWhenLocationIndependent`.
  - `TestDurableEnd_MapsCompleteRefusals`: `ErrCalloutSuperseded` → 410; `ErrTxRollbackOnly` → 409 `TRANSACTION_ROLLBACK_ONLY`; `ErrScopeConflict` → 409 `CONFLICT` retryable, no mark; any other error → 500, marks on a write.
  - `TestDouble_Conformance`: the spitest coordination suite passes.
  - Multinode scenario `RunCallback_LocationIndependent_ServedOnNonOwner`:
    1. A callback sent to the non-owner pnode is served there: its response header `X-Cyoda-Node`, or the node attribute the fixture already reads, names the non-owner.
    2. Its write commits with the owner.
- [ ] **Step 2: Run RED**, then implement, then run `go test ./internal/txcoord/... ./internal/cluster/... ./internal/grpc/` green. Commit:
  `feat(txcoord): location-independent transactions through the optional SPI capability`.

### Task 15: Cross-backend parity scenarios

**Files:**
- Create: `e2e/parity/callback_scope.go`
- Modify: `e2e/parity/registry.go`

Scenarios. They are backend-agnostic, lock their models first (#705), and use the compute-node pattern of `callback_txjoin.go`:
- `RunCallbackScope_FailedJoinedWriteDoomsOwner`: owner 409 `TRANSACTION_ROLLBACK_ONLY`, and nothing of the owner or the callback is committed.
- `RunCallbackScope_RetryableFailureRetriedInTx`: the owner commits exactly one `x`.
- `RunCallbackScope_FailedReadKeepsTx`: owner 200.
- `RunCallbackScope_AsyncNewTxAbsorbs`: owner 200, and nothing of the callback exists.
- `RunCallbackScope_MessageOutsideTx`: a message posted under the token survives the owner's failure.

- [ ] **Step 1:** Write and register the scenarios, then run `make test`. They pass on memory, SQLite and PostgreSQL, because Tasks 3, 4 and 11-13 have landed. Show RED by running against the commit before Task 11 and record the output in the commit message.
- [ ] **Step 2: Commit:** `test(parity): callback scopes behave the same on every backend`.

### Task 16: Documentation, contract and external records

**Files:**
- Modify: `cmd/cyoda/help/content/cluster.md:160-170`:
  - drop "served one at a time" as a general rule;
  - drop the wait-and-keep sentence;
  - add: callbacks are all-or-nothing; a callback still running when its callout moves on is discarded and answered `410`; the location-independent backend runs callbacks concurrently.
- Modify: `cmd/cyoda/help/content/grpc.md:236-244`: send the callbacks of one transaction in series; on a serialising backend, `503 TRANSACTION_BUSY`; remove "What the workflow did before it stays in the joined transaction". Also `:471-480` (write outcome after a `503`, pre-send vs post-send) and `:656` (make sure "all `503`, retryable" cannot read as covering `TRANSACTION_NODE_UNAVAILABLE`).
- Modify: `cmd/cyoda/help/content/workflows.md` (`ASYNC_NEW_TX` absorbs a failed callback; a failed joined write dooms the operation)
- Modify the error topics:
  - `errors/TRANSACTION_NODE_UNAVAILABLE.md`: retryable before send only.
  - `errors/CONFLICT.md:40`: the callback is not retryable, the owner is.
  - `errors/COMMIT_IN_JOINED_TRANSACTION.md`: the refused write is discarded and makes the operation rollback-only.
  - `errors/TRANSACTION_NOT_FOUND.md`: the session-reclaimed callback.
  - `errors/CALLOUT_SUPERSEDED.md`: the in-progress callback is discarded.
  - `errors/TOO_MANY_JOINED_REQUESTS.md`: see `TRANSACTION_BUSY`.
- Modify: the conditional delete's help topic, its OpenAPI description in `api/openapi.yaml` and the CHANGELOG line (grep `commits nothing itself`): state the rollback-only rule.
- Modify: `api/openapi.yaml`. Add the `409` / `503` responses on the entity write routes where they are missing. Then run `go generate ./api`.
- Modify: `CHANGELOG.md`, under `[Unreleased]` → `### Breaking`. One bullet each:
  - callback scopes;
  - `TRANSACTION_ROLLBACK_ONLY`;
  - `TRANSACTION_BUSY`;
  - the changed callback answers (spec section 9 table);
  - `TRANSACTION_NODE_UNAVAILABLE` retryability;
  - PostgreSQL messages outside the transaction.
- Modify: `docs/ARCHITECTURE.md`, the transaction-coordination text: audit the whole document for gate, fence and "one at a time" claims, and write it in the present tense.
- Modify: `docs/cloud-parity/nested-join-tx-serialisation.md:41-45` (the audit-order sentence)
- Create: `docs/cloud-parity/callback-scopes.md`, the Gate-7 entry (spec "Docs and contract").
- Modify: `cmd/cyoda/help/content/config/*.md` for the PostgreSQL topic. Add one operator line: more than 64 writing callbacks in one transaction overflow PostgreSQL's subtransaction cache.

- [ ] **Step 1: Checks.**
  - `grep -rn "one at a time\|one after another\|stays in the joined transaction" cmd/cyoda/help docs/ARCHITECTURE.md` returns only intended lines.
  - `go test ./cmd/cyoda/... ./api/...` passes, including the OpenAPI conformance and the help-topic tests.
- [ ] **Step 2: Commit:** `docs: callback scopes, rollback-only and the changed callback answers`.
- [ ] **Step 3: External records.** These are not commits.
  - File the Cyoda Cloud ticket (`[CaaS]` prefix, component CaaS) via the Rovo connector, linking the cloud-parity entry.
  - Comment on cassandra#121: the Task 1 savepoint case Cassandra will fail (`tx_manager.go:732-744`, `:910-918`), and the coordination spitest suite it must pass.
  - Comment on #702 that #704 fixes it.

## Coverage waivers

Spec coverage-matrix cells carried by another layer, one line each:

- "Messages and async-search writes under a token: not rewritten by a supersession" (e2e): carried by `TestEnd_Outside_NeverRewrittenNeverMarks` (Task 9), the route-class walk (Task 11) and `TestCallback_MessageUnderToken_OutsideTheTransaction` (Task 4). A supersession landing mid-way through a message POST cannot be timed through HTTP without a test hook.
- "Completion fails (PostgreSQL)" (unit): carried by `TestEnd_Row5_ReleaseFails500AndMarks` with a fake transaction manager. A real swallowed statement error needs a handler bug to provoke.
- "Later joined read and write on a rollback-only transaction → 409, both doors" (e2e): carried by `TestTxJoin_FailedWriteMakesTheTransactionRollbackOnly` (HTTP) and `TestTxRoute_RawHandlerErrorMarks` (gRPC), both on live coordination. End to end the 409 exists only between the mark and the callout's end: the mark cancels the callout begun from the marked scope, and Enter checks the fence (410) before the mark (5.1 steps 6-7).
- Task 7 Step 2b: waived — `e2e/parity/multinode` has no node stop helper; the unit tests on both doors carry the cell.
- "Superseded scope with a marked descendant → no mark climbs, operation can commit" (e2e): carried by `TestEnd_Row1_SupersededWithMarkedDescendantNoClimb` (in-process) and `TestDurableEnd_MapsCompleteRefusals` (a superseded completion passes no mark). End to end the scope must be marked and then superseded before it ends, and on the in-tree backends nothing a test can hold without a test hook keeps it open in between: the mark cancels the scope's nested callout, the scope's resume takes the gate as soon as the failed callback lets it go, and later callbacks under it are refused at open; once its own callout ends, the fence refuses every pass under it, so no mark can arrive after. The two-level climb of the same shape, without the supersession, is end to end in `TestCallbackScope_MarkedByDescendant_ClimbsToTheOwner`.

## Verification before the PR

- `make test`, `go vet ./...`, and `go vet` in each `plugins/*`.
- Push. CI (`test`, `plugins`, `race`, `smoke`) must be green on the exact head. Do not run `make test-full` or `make race` locally.
- The whole-branch code review (fresh reviewer) and `cyoda-go-security-audit` on the final head. Check tenant isolation of the scope registry: every map key includes the transaction id, which `Join` has already tenant-checked.
- The PR targets `release/v0.9.0`.
