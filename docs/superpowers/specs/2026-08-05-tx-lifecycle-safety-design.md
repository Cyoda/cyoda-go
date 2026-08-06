# Transaction lifecycle safety — design

Issue: #471 (supersedes #466, #354, #362). Milestone v0.8.4.

A transaction that cannot complete correctly must be rolled back and its
connection returned on **every** exit path, including a panic, under a ceiling
the application cannot dodge. Today none of that holds and there is no backstop
underneath it. Four independent pieces of work, one slice because two of them
would otherwise be written twice.

## Status

Design agreed. Reviewed by four independent fresh-context reviewers; every
blocking finding is incorporated, and the file/line citations throughout were
verified against the tree rather than carried from the issue. **Next step is
`superpowers:writing-plans`, then TDD execution — not another review round.**

Settled with the maintainer; do not re-open without new evidence:

- Ceiling defaults **5m / 5m / 10s**, and migration `lock_timeout` **5m**.
- `internal/cluster/lifecycle` and the three `CYODA_TX_*` vars are **deleted**,
  not wired (§4).
- Backward compatibility is **not** a constraint — the user base is
  approximately zero. Pick the correct default and record it under `### Breaking`.
  Do not build upgrade machinery for instances below schema version 2 (v0.7.x);
  none exist (§3.4).
- `docs/ARCHITECTURE.md` gets a full audit in this change (§4.4).

Alternatives already rejected with reasons in-place — §1.2 (`Advance` cannot
precede the error check), §2.1 (no acquire-deadline exemption; it would need an
SPI change), §3.3 (no second advisory lock, no `pg_locks` probing), §4 (wiring the
reaper), §6 (no parity cells — the harness cannot express them).

---

## 1. Deferred rollback (all backends)

### 1.1 The defect

`internal/domain/entity/service.go` has 40 `rollbackOwned` call sites, all plain
calls on error branches, zero deferred. A panic between `Begin` and `Commit`
unwinds straight past them: the transaction is neither committed nor rolled
back, and its pooled connection never returns.

The workflow engine has the same defect on the segments it opens itself.
`executeCommitBeforeDispatch` opens TX_post (`engine_processors.go:333`, `:409`)
and rolls it back only through plain calls (`:292`, `:341`, `:349`, `:353`);
`rollbackOpenSegmentOnFailure` (`:150`) covers the error path out of
`executeProcessors` and nothing covers a panic. The handler cannot cover this:
once the engine has segmented, `flushAndCommitSegment` has already committed the
entry transaction, so a handler-side guard naming the entry txID is a guaranteed
`ErrTxNotFound` no-op while TX_post leaks.

`engine_processors.go:146` already asserts that "the caller-side handler tracks
the original entryTxID and will roll that back via its own deferred-rollback
path". No such path exists. This spec creates it and makes the comment true.

`internal/domain/workflow/fire_scheduled.go:112-135` is the one place that got it
right — a `committed` flag, a deferred rollback, and `curCtx/curTxID` advanced on
segmentation. That is the discipline being extracted.

### 1.2 `txScope`

A value type in `internal/domain/entity`:

```go
scope, err := h.beginScope(ctx)   // beginOrJoin only — does NOT touch the gate
defer scope.Release()
...
scope.Advance(result.FinalCtx, result.FinalTxID)   // FIRST statement after every engine call
...
err := scope.Commit()
```

All 40 explicit `rollbackOwned` calls are **deleted**, not duplicated.

Required properties:

- **`beginScope` does not touch the joined gate.** The seven existing
  `defer releaseGate()` calls (`service.go:269`, `:652`, `:792`, `:916`, `:1179`,
  `:1370`, `:1719`) stay exactly as they are — and `defer scope.Release()` is
  registered *before* them, so LIFO frees the gate first. Folding the gate into the scope while
  `Release` is a rollback no-op for joined calls would leave the gate permanently
  held — it is a non-reentrant mutex (`internal/txgate/txgate.go:43`), so every
  later joined callback on that txID would block forever.
- **Joined callbacks never roll back their owner's transaction.** `Release` is a
  no-op when `owned == false`, matching `rollbackOwned` (`handler.go:107`).
- **Except a segment the engine opened, which is nobody else's.** When a joined
  call detects that the engine unexpectedly segmented (`service.go:330`, `:1262`,
  `:1510`, `:1853`) it returns `common.Internal` today with no rollback, so TX_post
  leaks. That segment is not the owner's transaction — it was opened by the engine
  during this call — so `Release` rolls it back regardless of `owned` whenever the
  scope has advanced past its entry txID. It is a can't-happen branch; fail-closed
  says handle it anyway.
- **`Release` targets the segment actually open.** `Advance` goes immediately after
  the engine call's `if err != nil` check — it cannot go before it, because the
  engine returns a **nil** `EngineResult` on every error path (§1.3), so
  `result.FinalCtx` would nil-dereference. The panic window between the call and
  the advance therefore cannot be closed on the handler side at all; §1.3's
  engine-side guard is what covers it, which is the correct place since the segment
  is the engine's until it is handed back. The existing `result != nil` guards at
  `:321`, `:1248`, `:1256` are dead — they sit after the error check and the engine
  never returns `(nil, nil)` — and `:325` already dereferences unguarded; delete
  them (Gate 6).
- **`Release` acquires the per-tx gate before rolling back.** Ten of the 40
  rollbacks run inside `h.gate.Acquire(...)` today (`service.go:356`, `:680`,
  `:833`, `:1279`, `:1283`, `:1548`, `:1556`, `:1567`, `:1872`, `:1911`); the other 30 are
  already ungated, so for those `Release` is a strengthening. For the ten, the
  safety property that must survive is mutual exclusion on the `pgx.Tx`, and
  re-acquiring preserves it. What does *not* survive is failed-Save→rollback as one
  atomic gated section: a joined callback can win the gate in the window between
  the IIFE releasing it and `Release` re-acquiring, Save successfully, return 200
  to its caller, and then have its write discarded by the rollback. That is
  strictly better than today's alternative — a leaked transaction — and the joined
  caller's write was doomed either way once the owner failed, but it is a real
  change and is stated rather than glossed. No self-deadlock: every
  `defer h.gate.Acquire(...)()` site is inside an IIFE, so the gate is free by
  outer-defer time.
- **`Commit()` marks the scope done regardless of outcome.** This preserves
  today's behaviour exactly — no path rolls back after a failed commit
  (`service.go:980` documents why) — and avoids aborting a commit another
  goroutine is running on memory's `ErrTxCommitInProgress` path
  (`plugins/memory/txmanager.go:222`).
- **Rollback runs on a fresh, bounded context**:
  `context.WithTimeout(context.WithoutCancel(ctx), 5s)`. `WithoutCancel` keeps
  the `UserContext` that `verifyTenant` reads
  (`plugins/postgres/transaction_manager.go:486`) while dropping cancellation, so a
  timed-out request still returns its connection; the timeout stops a wedged
  `Rollback` from blocking the unwinding goroutine forever. Applied
  unconditionally — `FinalCtx` is `WithoutCancel`-wrapped only in the
  `startNewTxOnDispatch=false` case (`engine_result.go:25`).

  **The 5s bound covers the rollback call, not the wait to reach it, on any
  backend.** `Release` must first acquire the per-tx gate, and
  `txgate.Registry.Acquire` (`internal/txgate/txgate.go:30`, `:43`) is a plain
  `sync.Mutex` with no context — that is core code, so it applies to postgres too,
  not just to memory and sqlite. Those two additionally take `tx.OpMu.Lock()` in
  `Rollback` (`plugins/memory/txmanager.go:513`, `plugins/sqlite/txmanager.go:567`).

  What actually bounds the wait is that the holder's work terminates: the gate is
  never held across a dispatch (`txgate.Suspend`), and every in-flight operation it
  can be waiting on is itself bounded — by `responseTimeoutMs` for a callout, by
  §2's ceilings for a statement. So the total is bounded but not by the 5s context,
  and rows 1 and 8 assert it on that basis. Making it a hard bound means giving
  `Registry.Acquire` a context-aware variant and `OpMu` the same, which is a change
  to the concurrency model of core plus two plugins — out of scope here (§8), and
  stated so the guarantee is not read as stronger than it is.

Flows converted: `CreateEntity`, `DeleteEntity`, `DeleteAllEntities`,
`DeleteEntitiesConditional`, `CreateEntityCollection`, `updateEntityCore` (and
`UpdateEntity` / `PatchEntity` through it), `UpdateEntityCollection`.

`internal/domain/model` opens no transaction of its own, and the message paths do
not either — `beginOrJoin` is the entity service's alone. That answers the
question the issue left open: they do not share the shape and need no change.

**Same-class defect fixed alongside.** `UpdateEntityCollection`'s per-item
isolation (`service.go:1815-1828`) treats any `spi.ErrConflict` from the engine as
an If-Match precondition failure and `continue`s the loop — with no `segmented`
check, unlike the handler-side isolation at `:1893-1905`, which does gate on it.
But `executeCommitBeforeDispatch`'s apply-result CAS bubbles `ErrConflict`
unwrapped (`engine_processors.go:352`) and is reachable on any CBD segment *after*
TX_pre has committed. When that fires, `engineResult` is nil, so `currentCtx` and
`currentTxID` are never advanced and every later item saves into a transaction that
`flushAndCommitSegment` already committed. §1.3's guard makes the orphaned segment's
rollback certain, which would otherwise leave the loop writing into a closed
transaction and losing those items silently. Isolation must therefore apply only
when the engine did not segment.

### 1.3 Engine-side guard

The handler-side scope cannot cover the engine's segments, and not only because of
panics: **`Execute`, `ManualTransition` and `Loopback` return a nil `EngineResult`
on every error** (`engine.go:273`, `:282`, `:290`, `:354`, `:359`, `:364`, `:370`,
`:424`, `:451`, `:457`). There is nothing for the handler to `Advance` from, so on
any engine error the scope still names the cascade-entry transaction — already
committed once a COMMIT_BEFORE_DISPATCH segment flushed — while TX_post is dropped.

`rollbackOpenSegmentOnFailure` (`engine_processors.go:150`) only sees failures
raised *inside* `executeProcessors`. These all occur outside it, after
`currentTxID` has advanced:

- `engine.go:754` — `maxStateVisits` abort in `cascadeAutomated`
- `engine.go:775` — criterion evaluation error, which includes a FUNCTION-criterion
  compute-node dispatch failure: an ordinary, expected occurrence
- `engine.go:834` — `maxCascadeDepth` exceeded
- `engine.go:272` — `attemptTransition` error (`currentTxID` is assigned at `:271`,
  then discarded by `return nil, err`)
- `engine.go:290`, `:370`, `:457` — `reconcileScheduledTasks` failure
- `engine.go:685`, `:714` — `fireTransition`'s own error returns

So the leak this issue is about is reachable **without any panic at all**, by a
compute node failing a criterion callout mid-cascade. On memory and sqlite there is
no DB-side ceiling underneath, so it is permanent rather than 5-minute-bounded.

`fire_scheduled.go:405-412` already documents exactly this and fixes it for the
scheduler door — "may fail entirely outside executeProcessors … Advance
curCtx/curTxID before checking err so the deferred rollback always targets the
segment actually open". The three public entry points never got the same
treatment.

**Fix:** one panic-safe guard at each of `Execute`, `ManualTransition` and
`Loopback`, over the segment that entry point owns:

```go
entryTxID := txID
openCtx, openTxID := ctx, txID   // dedicated locals, NOT the named returns
handedOff := false
defer func() {
    if !handedOff && openTxID != entryTxID {
        _ = e.txMgr.Rollback(rollbackCtx(openCtx), openTxID)
    }
}()
```

`openCtx/openTxID` are advanced wherever the engine segments, and `handedOff` is
set only where the segment is returned to the caller. The guard therefore covers
the error paths above **and** panics, in one mechanism.

The locals are load-bearing: every failure path in `executeCommitBeforeDispatch`
is `return nil, "", err` (`engine_processors.go:286`, `:293`, `:313`, `:326`,
`:335`, `:342`, `:350`, `:354`), so a guard reading the named `newTxID` return
would see `""` on exactly the paths that need it and skip the rollback — turning
four working rollbacks into four leaks.

This **subsumes** `rollbackOpenSegmentOnFailure` and the four plain rollbacks at
`engine_processors.go:292`, `:341`, `:349`, `:353`, which are all removed. Net
result is fewer moving parts than today, not more, and it makes
`engine_processors.go:146`'s existing claim about a caller-side deferred-rollback
path true for the first time.

`executeCommitBeforeDispatch` keeps its own guard as well, since it opens TX_post
(`:333`, `:409`) and can panic before returning it to `executeProcessors`.

The guard is nil-safe on `e.txMgr`, mirroring `rollbackOpenSegmentOnFailure`'s
check (`engine_processors.go:151`) — unreachable in production, since segmentation
implies a transaction manager, but the engine is constructed without one in unit
tests.

`entryTxID` is `e.resolveAuditTxID(entity)` (`engine.go:251`, `:344`, `:417` →
`:922`), which returns `entity.Meta.TransactionID` or mints a fresh UUID when it is
empty. It equals the handler's transaction only because every handler stamps
`Meta.TransactionID = txID` before calling the engine (`service.go:288`, `:1212`,
`:1447`, `:1786`). The guard inherits that assumption, and so does
`flushAndCommitSegment`'s `Commit(ctx, txID)` (`engine_processors.go:390`) — which
already commits on it today. Record the invariant next to the guard; deriving the
engine's transaction identity from `spi.GetTransaction(ctx)` and leaving
`resolveAuditTxID` to audit correlation is the right end state but is a wider
change than this issue carries.

The guard's `openTxID != entryTxID` test is sound because every early return in
`attemptTransition` (`engine.go:628`, `:642`, `:648`, `:655`) and `fireTransition`
(`:685`, `:702`, `:704`) returns the *input* ctx/txID — which is safe only because
no segmentation can have occurred at those points. Processors run after criteria,
so nothing has segmented yet. That is an invariant, not an accident: it is recorded
here because reordering processors before criteria would break the guard silently.

**Same-class defect fixed here.** `engine.go:772` builds the `criterionContext`
with `ctx: currentCtx` but `txID: txID` — the cascade-*entry* txID. After a CBD
segment that names a **committed** transaction, and it is the value handed to
`DispatchCriteria` (`:869`) and on to the compute node as its join token, so a
criterion callback attempting to join gets `ErrTxNotFound`. It is the same
transaction-identity-versus-segment-identity confusion §1.2 and §1.3 exist to fix,
in the same file, so it is fixed here rather than left for someone to rediscover.

### 1.4 Panic recovery on the doors that lack it

The two doors differ in severity, and it is worth being precise about which.

**gRPC — a panic kills the process.** `internal/grpc/server.go:69` installs auth
and tx-route interceptors only, and grpc-go does not recover handler panics; there
is no equivalent of net/http's per-connection recover. Several operations are
genuinely gRPC-only (`internal/grpc/model.go:85`, `:119`, `:156`, `:186`, `:202`,
`:240`), so this is not merely a second door onto HTTP-reachable code. Add a
recovery interceptor, unary and stream.

**HTTP routes outside `middleware.Recovery` — a panic loses the contract, not the
process.** `net/http` recovers handler panics itself (`server.go:1904`, "http:
panic serving"), logs, and closes the connection. So these do not take a node down;
what they lose is the project's own contract — no ProblemDetail 500 with a ticket,
no `healthFlag` marking, and a connection dropped mid-response instead of an error
body.

`Recovery` wraps only the `/` catch-all (`app/app.go:729`), and **every** pattern
more specific than `/` wins over it. That is not a short list: the peer scheduler
RPC (`:747`, `:760`) and cluster dispatch (`:746`, `:759`), the entity-transition
and grouped-stats routes (`:671`, `:672`, `:718`), the four admin log-level and
trace-sampler routes (`:663-666`), health (`:641`), `/.well-known/` and
`POST /oauth/token` (`:657-658`), and the discovery and help routes (`:735`,
`:739`, `:756`, `:760`). The scheduler RPC is the one that matters most — it opens
a transaction and runs a full fire plus cascade including compute-node callouts
(`internal/cluster/scheduler_rpc.go:273` → `fire_scheduled.go:112`), reachable by
any peer — but enumerating doors is the wrong shape of fix, and any route added
later would silently join the list.

So: wrap `a.handler` **once**, after the mux is assembled, and delete the
`/`-specific wrap at `:729`. One call site instead of a dozen, and no way for a new
route to escape it.

**Background goroutines.** `internal/domain/search/service.go:433` runs the async
search job unrecovered, so a panic there does take the process down — same class as
the gRPC door, and inconsistent with the scheduler's own dispatch goroutine, which
recovers (`internal/scheduler/service.go:189-194`). It gets the same treatment.

Both recoveries mirror `internal/api/middleware/recovery.go`: log with stack, mark
the health flag, return a generic internal error with a ticket UUID. The
scheduler's dispatch goroutine latches too, though it already recovered: its
`Executor.Execute` runs a full fire plus cascade **in-process** whenever
distribution picks this node (`internal/cluster/scheduler_rpc.go`
`ClusterExecutor.Execute`, `target == selfID`), so leaving it unlatched would make
withdrawal depend on which node `Pick` chose — the identical panicking fire takes
the node out of service when the pick is a peer (it arrives over the HTTP door) and
leaves it serving when the pick is self.

The criterion for latching is what the recovered code was doing, not where it
entered from: engine or store work on the application's behalf leaves state nothing
has verified. Recoveries that wrap notification callbacks — the member-registry
`onChange` fan-out (`internal/grpc/members.go`) and the OIDC broadcast handler and
its dispatch goroutines (`internal/auth/oidc/broadcast.go`, which counts panics on
its own metric) — hold no transaction and self-heal on the next event, so they log
and do not latch.

Marking the health flag means the first recovered panic at any of the four latching
sites takes the node to `503 DOWN` permanently. That flag has to reach something that decides whether the
node receives traffic, and originally nothing probed it: the shipped chart's
liveness probe hits `/livez` (unconditional) and its readiness probe hits
`/readyz`, which checked only the store factory. So `ReadinessCheck`
(`app/app.go`) reads the flag as a second, independent failure condition. Fail
closed: a node whose state nothing has verified stops taking traffic.

Be precise about how much that buys, because the shipped chart hardcodes
`CYODA_CLUSTER_ENABLED=true` (`deploy/helm/cyoda/templates/configmap.yaml`), so
every install is a cluster:

- **Stopped.** New client connections through the `ClusterIP` Service. Readiness
  at `periodSeconds: 5, failureThreshold: 3` drops the pod from its endpoints in
  ~10-15s, and both the Gateway `HTTPRoute` and the `Ingress` route through that
  Service.
- **Not stopped.** Peer-forwarded work. Peers resolve each other from the gossip
  registry, not the Service, so tx-affinity proxying
  (`internal/cluster/proxy/http.go`), gRPC proxying, cluster dispatch and the
  peer scheduler RPC all still arrive — and `RoundRobin.Pick`
  (`internal/scheduler/distribution.go`) never reads `NodeInfo.Alive`, so the
  node keeps its 1/N share of every scan and opens a transaction and runs a full
  cascade for each. Established connections are not closed either.
- **Not restarted.** `/livez` is deliberately not wired to the flag: a
  deterministic panic (a poisoned entity, a bad workflow definition) recurs on
  the next request and would turn a restart into a loop.

Two questions are therefore left open for the maintainer, and the code states the
current behaviour rather than pretending either is settled: whether a recovered
panic should also fail liveness, and whether a tainted node should self-eject
from gossip (the plumbing exists — `UpdateTags`, `Deregister` — and both the
proxy and the scheduler RPC already have defined `!alive` behaviour, so this is a
posture decision, not a design obstacle) or whether drain-plus-manual-replacement
is the intended posture. §6 row 2's "still serving" assertion is about request
handling, which continues.

Recovery must not land without §1.2 and §1.3. On its own — on the gRPC door — it
would convert a process crash, which PostgreSQL cleans up by killing every session,
into a silent connection leak.

### 1.5 Corrections to the issue's framing

Verified against the tree; the spec's tests depend on getting these right.

- **The node does not "look healthy".** `recovery.go:23` calls
  `healthFlag.Store(false)` and nothing resets it, so the health endpoint reports
  `503 DOWN` after the first recovered panic. The acceptance test asserts that
  *requests still succeed*, not that health is green.
- **The scheduled path is not a leak path for the entity service.** It enters via
  `FireScheduledTransition`, which already has the deferred guard.
- **sqlite holds no connection.** `plugins/sqlite/txmanager.go:149` opens no
  `*sql.Tx`; writes buffer in `spi.TransactionState.Buffer` and the real
  `db.BeginTx` happens inside `Commit` (`:386`). `SetMaxOpenConns(1)` is therefore
  irrelevant to an orphaned transaction. The real harm on memory and sqlite is the
  leaked buffer plus a pinned `committedLog` prune floor
  (`plugins/memory/txmanager.go:469`, `plugins/sqlite/txmanager.go:348`), which
  makes every later commit's conflict scan slower without bound.

---

## 2. DB-side ceilings (postgres)

| Var | Default | Limits | Mechanism |
|---|---|---|---|
| `CYODA_POSTGRES_STATEMENT_TIMEOUT` | `5m` | how long one SQL statement may run | server GUC |
| `CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT` | `5m` | how long a connection may sit inside an open transaction doing nothing | server GUC |
| `CYODA_POSTGRES_ACQUIRE_TIMEOUT` | `10s` | how long a request waits for a free pooled connection | Go-side deadline (§2.1) |

`0` disables any of them, matching PostgreSQL's own convention.

The two GUCs are set on the app pool via `ConnConfig.RuntimeParams` at connect
time (no `AfterConnect` round-trip). `pgxpool.Config` has no `AcquireTimeout`
field, which is why the third is a deadline rather than a fourth setting.

**Encoding.** Values go into the startup packet, so a malformed one fails
`pool.Ping` at boot for every deployment. PostgreSQL's time units are
`us`/`ms`/`s`/`min`/`h`/`d` — **`m` is not among them**, and Go's
`(5*time.Minute).String()` is `"5m0s"`, which is also invalid. The config values
are parsed as Go durations and emitted as bare integer milliseconds
(`strconv.FormatInt(d.Milliseconds(), 10)`), which is the default unit for all
three GUCs. A value in `(0, 1ms)` truncates to `"0"`, which PostgreSQL reads as
*disabled* — the exact inversion of intent, so parsing rejects it rather than
silently removing a ceiling. A test asserts the rendered form.

**Precedence.** `pgxpool.ParseConfig` folds unrecognised DSN keys into
`RuntimeParams` (`plugins/postgres/config.go:131`). Since these settings now have
non-zero defaults, writing them unconditionally would let a default the operator
never set override a value they did set in `CYODA_POSTGRES_URL`. So the plugin
writes only when the env var is explicitly set, leaves a DSN-supplied value alone
otherwise, and logs at WARN when both are present and it is overriding.

The idle limit is the one that plugs the leak: an abandoned transaction is idle
by definition. It must clear the longest legitimate idle gap, which is a
compute-node callout — `responseTimeoutMs` defaults to 30s
(`internal/grpc/dispatch.go:32`), so 5m clears it tenfold. All cluster timeouts
sit well under it (`CYODA_TX_TOKEN_TTL` 90s, proxy and dispatch-forward 30s).

The margin is per-gap, not cumulative — a deep cascade (`maxCascadeDepth` is 100,
`engine.go:97`) can spend far more than 5m in total across many callouts. It stays
safe because the postgres audit store issues a real `INSERT` inside the transaction
between every processor (`engine_processors.go:129` →
`plugins/postgres/sm_audit_store.go:39`), which resets the idle timer. That is
load-bearing and was previously undesigned, so it is recorded here and asserted by
a test: a multi-processor cascade whose total callout time exceeds the ceiling
must still commit.

When PostgreSQL aborts a session, the handler's next operation on that `pgx.Tx`
fails cleanly and runs its existing error path — nothing is yanked out from under
a live goroutine.

**Async search** is the one workload whose purpose is to run long, and on postgres
it is bounded by **nothing at all** today: the scan budget raising
`spi.ErrScanBudgetExhausted` exists only in `plugins/sqlite/searcher.go:104` and
`:240` — `internal/domain/search/service.go:227` is just the error mapping, and
`plugins/postgres/searcher.go` has no equivalent — while the job goroutine
(`service.go:433`) runs on `context.Background()`. So it is the strongest case for
a ceiling and the worst fit for a shared one: a single knob would force operators
to choose between fast-failing interactive writes and long analytical scans.

The async-search path is pool-direct and separable (`search_store.go`), so it gets
its own: `CYODA_POSTGRES_SEARCH_STATEMENT_TIMEOUT`, default `30m`, applied as
`SET LOCAL statement_timeout` on that path rather than as a second pool. The
interactive ceiling stays at 5m.

### 2.1 Acquire-timeout scope

Applied at `TransactionManager.Begin`'s `pool.BeginTx` (`transaction_manager.go:79`)
and `model_store.go:359`'s `pool.Begin`. Both return immediately, so the deadline
bounds the acquire and does not leak into the returned handle.

**The deadline must be scoped to the acquire alone, inside the plugin.**
`Begin` returns `spi.WithTransaction(ctx, …)` derived from its *input* context
(`transaction_manager.go:126`), so wrapping that input in
`context.WithTimeout` would give every later operation on the transaction a 10s
deadline and cancel it the moment `Begin` returns. The plugin builds a separate
acquire-only context for `pool.BeginTx` (and for the `set_config` round-trip at
`:87`) and returns the transaction context derived from the original. Same
requirement at `model_store.go:359`. This is the single easiest thing in the whole
change to implement wrongly, and it fails in a way ordinary tests would not catch —
so §6 row 11e asserts a transaction stays usable past the acquire timeout.

**No exemption for the engine's segment `Begin`.** `executeCommitBeforeDispatch`
opens TX_post through the same `Begin` (`engine_processors.go:333`, `:409`) after
the external dispatch has fired, so a failed acquire there leaves the side effect
executed and the post-dispatch state unapplied. That outcome is not introduced by
the deadline — `:335` already returns an error if `Begin` fails for any reason —
and exempting it is not free: `plugins/postgres/go.mod` depends only on
`cyoda-go-spi`, with no path back to the cyoda-go root module, so there is no
channel for the engine to signal "skip the deadline" short of a new SPI context key
plus a coordinated cross-repo release. Buying a marginal reduction in the
likelihood of a pre-existing failure mode with an SPI tag is the wrong trade. The
deadline is generous relative to this seam anyway: TX_pre's connection was returned
by `flushAndCommitSegment` immediately before, so the segment `Begin` is contending
for a connection the same goroutine just released.

`model_store.go:359` self-wraps only when there is no ambient transaction (`:355`)
and already has `defer tx.Rollback` (`:364`), so it cannot nest. Its only reachable
callers are `ValidateOrExtend`'s five entity/workflow call sites — model import does
**not** reach it. Note its acquire failure surfaces as **500, not 503**:
`validate.go:124` wraps the error in `ErrInternalSchema`, and
`classifyValidateOrExtendErr` (`handler.go:203`) maps that to `common.Internal`.
That is left as-is rather than threaded through the schema classifier — it is the
schema-extension path reporting that it could not extend the schema, and the cause
is in the log either way. §5's table is unaffected.

It is **not** applied to `pool.Query` / `Exec` / `QueryRow` / `CopyFrom`. For
`Query`, `pgxpool` holds the connection for the returned `pgx.Rows` under the same
context, so a deadline there caps statement execution and row iteration too — it
would break `search_store.go:113`'s `CopyFrom` of a whole async-search result set
and every non-transactional read routed through `StoreFactory.resolveRaw`.
(`Exec`/`QueryRow`/`CopyFrom` release before returning, so the objection is
narrower for them, but splitting the rule by method would be a trap for the next
reader.)
Bounding those properly means an explicit `Acquire`/`Release` restructure that
reimplements what `pgxpool` already does internally, with a connection-leak
failure mode of its own — disproportionate here, and unnecessary: those
statements are bounded server-side by `statement_timeout`, so the connection
returns within that ceiling regardless.

Net: every connection is released within `max(statement_timeout,
idle_in_tx_timeout)`, and interactive writes fail fast instead of queueing.

### 2.2 Error classification

Pool exhaustion surfaces as **503 `STORAGE_UNAVAILABLE`, retryable**.

The plugin must not classify on `context.DeadlineExceeded` alone: `pool.BeginTx`
returns it both when the acquire wait expired and when the *caller's* request
context expired, and mislabelling a client timeout as a retryable server 503 is
wrong. The plugin owns the acquire context, so it returns an error satisfying

```go
interface{ StorageUnavailable() bool }
```

matched with `errors.As`, not a type assertion — every `Begin` error is already
wrapped by the time a classifier sees it. No `cyoda-go-spi` change, so no
coordinated cross-repo release; the commercial backend can opt in later by
returning the same shape.

**Where the check goes.** `common.Internal(...)` fixes the status at 500, so the
classification has to run *before* it at each site that wraps a `Begin` error:
`service.go:264`, `:647`, `:787`, `:911`, `:1174`, `:1365`, `:1713`,
`fire_scheduled.go:114`, and `engine_processors.go:335`, `:411`. A
`classifyBeginErr(err)` helper replaces the bare `common.Internal("failed to begin
transaction", err)` at all ten; `AppError.Unwrap` (`internal/common/errors.go:48`)
means a later `errors.As` would still find the cause, but by then the status is
already wrong.

Two server-side aborts also need classifying, or they surface as opaque 500s:

- **`idle_in_transaction_session_timeout`** → SQLSTATE `25P03`. It *terminates the
  session*, it does not merely abort the transaction, so the next operation may
  return either a `*pgconn.PgError` carrying `25P03` or a transport error
  (unexpected EOF, broken pipe) depending on whether pgx reads the buffered
  `ErrorResponse` before noticing the closed socket. Classify **both** shapes.
  Which one actually arrives is settled by test, not by reading — same standard
  §3.2 applies to its own load-bearing claim.
- **`statement_timeout`** → SQLSTATE `57014` (`query_canceled`), on any statement,
  on any endpoint including reads. Nothing classifies it today, so it falls
  through `classifyError` (`plugins/postgres/transaction_manager.go:507`) as an
  unexplained error.

They are classified differently, because they differ in whether retrying helps:

- `25P03` and pool exhaustion → **503 `STORAGE_UNAVAILABLE`, retryable**. Both are
  transient contention; the same request may well succeed on a second attempt.
  `25P03` only arises inside a transaction, so it is reachable only on the write
  operations §5 already covers.
- `57014` → **500 with a ticket UUID**, per the project's 5xx convention (generic
  message to the client, full detail logged server-side). Re-running a statement
  that just exceeded the ceiling will exceed it again, so advertising it as
  retryable would be a lie. Every operation already declares
  `default: InternalServerError`, so this adds no wire-contract change and keeps
  §5's table at nine rows. What changes is that the log line names the ceiling
  instead of reporting an unexplained failure.

An async search job is the exception to the sanitization rule, deliberately:
`internal/domain/search/service.go:451` persists `searchErr.Error()` into the job
record, which `GetJob` serves back. A raw pgx string there would leak internals
(Gate 3), so the 57014 classification produces a fixed, non-revealing message
naming the ceiling and nothing else — the job is the caller's own work, and
"exceeded the search statement ceiling" is exactly what they need to know.

`25P03` also needs `cleanupTx`. PostgreSQL kills the *session*, but
`TransactionManager.cleanupTx` (`transaction_manager.go:321`) runs only from
`Commit`/`Rollback`, so the `registry`, `tenants`, `origins` and `txStates` entries
— the last carrying the read and write sets — would survive indefinitely. The
DB-side ceiling reclaims the connection; only §1's guard, or an explicit cleanup on
this classification path, reclaims the application-side state. Both are in scope
here.

---

## 3. Migration startup safety (postgres)

### 3.1 Migration-connection settings

`openDB` builds the migration connection from `pool.Config().ConnConfig`
(`migrate.go:23`), so it would inherit the app pool's `statement_timeout` and kill
a legitimate long index build. It needs the opposite settings:

| Setting | Value |
|---|---|
| `lock_timeout` | `CYODA_POSTGRES_MIGRATE_LOCK_TIMEOUT`, default `5m` |
| `statement_timeout` | `0` |
| `idle_in_transaction_session_timeout` | `0` |

Doing work for a long time is fine; waiting is what must be bounded. `5m` rather
than something tighter because a migration's own DDL lock waits are legitimate
during a rolling upgrade — an old node's in-flight write transaction is itself
bounded by the ceilings in §2, so a bounded wait succeeds where a 30s one would
abort a healthy upgrade.

Applying it at `openDB` (`migrate.go:24`) covers every caller: `runMigrations` via
`plugin.go:50`, `checkSchemaCompat`'s own handle (`plugin.go:42`), and the
test-only `migrateDown` (`migrate.go:231`). `RunMigrateWithDSN` — the `cyoda
migrate` subcommand — builds an independent pool (`migrate.go:88`) that inherits
nothing from the app pool, though it *does* inherit any `RuntimeParams` embedded in
the DSN, so it sets the same three explicitly.

`pool.Config()` deep-copies `RuntimeParams` (`pgxpool/pool.go:711` →
`pgconn/config.go:162`), so these overrides cannot leak back into the app pool.

### 3.2 Single migrator

Serialisation across nodes is not new: golang-migrate's `Lock()` already blocks on
`SELECT pg_advisory_lock($1)` (`golang-migrate/v4@v4.19.1
database/pgx/v5/pgx.go:229`) and followers get `ErrNoChange` once the winner
finishes. What is missing is a *bound* — that call uses `context.Background()`, so
the wait is indefinite at the Go level.

The bound comes from the session `lock_timeout` in §3.1: advisory locks go through
PostgreSQL's regular lock manager, so it aborts the wait. A single-node install is
unaffected because its lock is uncontended.

A node whose lock wait times out fails startup with an actionable message and is
restarted by its supervisor. No leader election, no second lock, no polling.

The `lock_timeout`-aborts-an-advisory-lock-wait claim is load-bearing and will be
proven by test, not taken from documentation.

### 3.3 Concurrent-boot dirty-flag false alarm

golang-migrate sets `dirty=true` **before** each migration step and clears it after
(`migrate.go:738`, `:750`) in separate committed transactions, with the migration
body itself running in a third (`pgx.go:283`) — that non-atomicity is what the flag
is for. A booting node that reads `dirty=true` gets *"schema compat: database
migration state is dirty at version %d — manual intervention required"*
(`plugins/postgres/migrate.go:196`) and exits (`plugin.go:45-48`,
`cmd/cyoda/migrate.go:84-91`) — a false alarm that invites an operator to hand-edit
`schema_migrations` while a live migration is running.

**The window is a race, not the general case.** `pgxmigrate.WithInstance` calls
`ensureVersionTable`, which takes the same advisory lock unconditionally
(`pgx.go:437-440`) before checking whether the table exists. So
`checkSchemaCompat`'s own `WithInstance` (`plugins/postgres/migrate.go:170`)
already blocks behind a migrating peer for the duration of its `m.Up()`. To lose,
node B must clear that lock/unlock and then have node A acquire the lock and stamp
`dirty` before B's `m.Version()` round-trip (`:188`) returns. That matches the
intermittent CI symptom rather than a deterministic failure, and it means §6 row 15
has to be written as a fault-injected interleave that fails on HEAD — "boot two
nodes concurrently" would pass green today and prove nothing (Gate 1).

It also means §3.1's `lock_timeout` now bounds that pre-existing wait inside
`checkSchemaCompat`. A migration legitimately running longer than 5m therefore
turns a slow-but-successful concurrent boot into a startup failure. That is the
correct trade — a bounded, logged, supervisor-retried failure beats an unbounded
stall — but it is a behaviour change and is stated rather than discovered.

The bug exists only because `checkSchemaCompat` runs **before** `runMigrations`
(`plugin.go:42` then `:50`) and therefore reads `dirty` outside any lock. **Swap
the order when `AutoMigrate` is true**: run `runMigrations` first, then
`checkSchemaCompat`.

That is the whole fix, and it needs no lock of our own. `m.Up()` takes
golang-migrate's advisory lock *before* reading the dirty flag, so a follower
blocks until the winner has finished and cleared it, then applies nothing and gets
`ErrNoChange`. The subsequent compat check runs on a settled schema, where
`dirty == true` unambiguously means a migration genuinely died and stays fatal
exactly as today. `lock_timeout` from §3.1 bounds the wait.

Running migrations before the compat check does not weaken the newer-than-code
guard: `m.Up()` on a database ahead of the binary finds no migration to apply and
returns `ErrNoChange`, and the compat check that follows still refuses to start.

**`ErrDirty` must be translated, or the actionable message becomes unreachable.**
`m.Up()` locks, reads the version, and returns `ErrDirty` *before applying
anything* (`migrate.go:265-277`), so with migrations first a genuinely dirty schema
now fails inside `runMigrations` with golang-migrate's bare *"Dirty database
version N. Fix and force version."* — and `checkSchemaCompat`'s message becomes
dead code on the auto-migrate path, taking with it the pointer to the INVALID-index
recovery procedure §3.4 depends on. `runMigrations` maps `migrate.ErrDirty` onto
the same actionable text, so the operator-facing contract is unchanged by the
reorder.

The ordering swap is needed in **two independent places** —
`plugins/postgres/plugin.go:42-53` and `plugins/postgres/migrate.go:114-121` are
separate copies, not a shared helper. They are extracted into one so the claim
"the CLI is covered by the same sequence" becomes structurally true rather than
maintained by hand.

With `AutoMigrate=false` the ordering is unchanged, because nothing is migrating
from this binary. A dirty read in that window is accurate information — the schema
really is mid-migration under someone else's control, and this node should not
start.

Rejected alternatives, recorded so they are not revisited: a second cyoda-owned
advisory lock around the pair (more machinery, and its own "which connection holds
it" question), and recomputing golang-migrate's lock id via
`database.GenerateAdvisoryLockId` to probe `pg_locks` — that id is a 32-bit CRC
over `schema\x00table\x00database` (`database/util.go:10-20`), so it couples us to
the hash, its argument order, and the driver's internal name derivation
(`pgx.go:75-108`), and it collides across unrelated databases on the same cluster.

### 3.4 Index migrations

Migrations `000001`–`000006` are **not** modified.

The one migration whose index could block live writers is `000002`, which adds
`entities_state_idx` to `entities` — a table that already holds data by then, and
whose writers a non-concurrent `CREATE INDEX` locks out for the duration of the
build (SHARE conflicts with the ROW EXCLUSIVE every INSERT/UPDATE/DELETE holds).
But `000002` only runs on a database below schema version 2, which means an
instance last on v0.7.x; v0.8.1 is version 2, v0.8.2 is 3, v0.8.3 is 6. There are
no such instances, so the case is not worth engineering for. Nothing else in
`000003`–`000006` blocks a hot table: `000003` and `000004` create their own new
tables, `000005` is functions only, and `000006` adds two defaulted columns to
`scheduled_tasks`, needing only a brief AccessExclusive lock that
`lock_timeout` now bounds.

The exposure that remains is the *next* index migration. Deliverables:

- **Pattern**: `CREATE INDEX CONCURRENTLY` alone in its own migration file. The
  driver sends the whole file through one `Exec` with `MultiStatementEnabled`
  false (`pgx.go:270`), and PostgreSQL wraps a multi-statement simple query in an
  implicit transaction, in which `CREATE INDEX CONCURRENTLY` cannot run.
  `000002_grouped_stats.up.sql` is the proof: a function plus an index in one
  file.
- **Guard test rule**, two clauses: (a) an index added to a table created in an
  *earlier* migration must be `CONCURRENTLY` — an index created in the same
  migration as its own table need not be, because that table is empty and
  unreachable by writers; (b) a file containing `CREATE INDEX CONCURRENTLY` must
  contain no other statement, or the implicit transaction above makes it fail at
  runtime. Clause (a) passes `000001`'s indexes on their merits rather than by
  exemption; `000002_grouped_stats.up.sql:44` is the sole grandfathered entry.
- **Recovery**: a failed `CREATE INDEX CONCURRENTLY` leaves an INVALID index and a
  dirty version. Document the `DROP INDEX` + re-run procedure in
  `cmd/cyoda/help/content/cli/migrate.md` and point at it from the dirty-state
  message, which is produced at `plugins/postgres/migrate.go:196` (the function
  returns the error; the exit happens at `plugin.go:45-48` and
  `cmd/cyoda/migrate.go:84-91`).

---

## 4. Remove `internal/cluster/lifecycle`

### 4.1 Evidence

- **It is dead structurally, not merely unused.** `Register` is the only writer to
  the `active` map, and it has zero callers. So `ReapExpired` cannot reap anything —
  not "does not today", but cannot, for any input. Production reaches exactly three
  methods: `NewManager` (`app/app.go:440`), `SetTransactionManager` (`:444`) and
  `ReapExpired` (`:467`) — a constructor, a setter that exists only to serve the
  reaper, and a loop over a map nothing can populate. `RecordOutcome`, `IsAlive`,
  `GetOutcome`, `ListByNode` and `Remove` have no callers at all.
- **It was never used here — there is no moment of disuse to find.** The package
  arrived whole in `d1f6875` (2026-04-14), a root commit with no parents: "Initial
  import from cyoda-light-go @ `ab90677`". It came in with all eight methods, a
  test file, and `app.go` already constructing it, exposing `TxLifecycle()` and
  running the reaper goroutine — and already with no caller of `Register`. How it
  behaved in the prototype cannot be determined from this repository; that history
  is not in the import and the source repo is not in the workspace.
- **It then became more convincing rather than less, which is why the docs are
  wrong in good faith.** At import, `ReapExpired` only expired map entries and
  recorded outcomes; it never touched a transaction. `b665800` ("Plan 3: lift
  memory + postgres into plugin modules") added the `txMgr` field,
  `SetTransactionManager`, and the `tm.Rollback(ctx, txID)` loop — reconnecting a
  reaper that had lost its reach into the TransactionManager when storage moved
  into plugins, and moving the rollback outside the mutex "to avoid holding it
  across network I/O". Careful work on a path that cannot execute. Afterwards the
  code has a constructor, a TTL, a ticking goroutine and a rollback call: it reads
  as a working backstop at every level a reader normally checks, which is how
  `PRD.md`, `ARCHITECTURE.md` and issues #31/#32 came to describe it as one. The
  single missing link is a call to `Register`, and no commit ever added it.
- The reaper goroutine only starts when `cfg.Cluster.Enabled` (`app/app.go:445`),
  so single-node never had one at all.
- Even if a transaction were registered, `ReapExpired` calls
  `tm.Rollback(context.Background(), …)`, which `verifyTenant` rejects on all three
  in-tree plugins (`plugins/postgres/transaction_manager.go:486`,
  `plugins/memory/txmanager.go:513`, `plugins/sqlite/txmanager.go:567`) — a
  background context carries no `UserContext`.
- Transaction affinity does not use it. Routing is driven by a signed token
  carrying node ID and expiry (`internal/cluster/proxy/http.go`,
  `internal/cluster/token`), with its own live knob `CYODA_TX_TOKEN_TTL`.
- When a node dies, its PostgreSQL sessions die with it and the server rolls those
  transactions back. No cluster bookkeeping is required for that.
- `CYODA_TX_TTL` has zero consumers, not even the reaper — `Register`'s `ttl`
  argument is never supplied. `CYODA_TX_REAP_INTERVAL` and `CYODA_TX_OUTCOME_TTL`
  feed only the dead Manager.
- The commercial cassandra backend cannot reference it — Go forbids importing
  another module's `internal/` — and does not: it has its own unrelated
  `internal/tx.TxLifecycle` enum and its own `CYODA_CASSANDRA_TX_*` timers, and
  from cyoda-go's root module it imports only `app.Config`, `app.DefaultConfig`,
  `app.LoadEnvFiles`, `app.New`, `app.ProfileBanner`. Neither it nor cyoda-go-paas
  references `TxTTL`, `OutcomeTTL`, `TxReapInterval` or the three env vars.

Nothing is lost. The one capability forgone is a test's ability to assert *which*
node holds a transaction (`e2e/parity/multinode/cbd_tx_pinning.go:54`); that test
already asserts the observable signature through version history.

The alternative — wiring the reaper — is rejected on the grounds the issue
records: `tm.Rollback` → `cleanupTx` → `registry.Remove` means a transaction reaped
out from under a still-running handler makes the handler's remaining writes fall
through to the pool (`resolveRaw` returns `f.pool` on a registry miss) and
auto-commit as standalone statements. Partial, non-atomic application is worse
than the leak, and making it safe needs a cancel handshake the DB-side ceiling
does not.

### 4.2 Removal list

Code: `internal/cluster/lifecycle/` (both files), its import at `app/app.go:28`
(a hard compile break if missed), `App.TxLifecycle()`
(`app/app.go:822`), the `txLifecycle` field and its construction/wiring
(`app/app.go:70`, `:440-444`), the reaper goroutine (`:459-477`) and `stopReaper`
(`:72`, `:887-888`), the three `cluster.Config` fields
(`internal/cluster/config.go:16,17,19`) and their `app/config.go` bindings
(`:298`, `:299`, `:301`).

Tests: `internal/cluster/lifecycle/manager_test.go`, and
`TestEndToEnd_LifecycleTracking` in `internal/cluster/integration_test.go:103-128`
plus its import at `:11` — it constructs `lifecycle.NewManager` and asserts
`OutcomeRolledBack`, so it will not compile otherwise.

Config surface: `cmd/cyoda/help/config_registry.go:59-61`,
`app/config_registry_binding_test.go:101-103`,
`cmd/cyoda/help/content/config.md:91-93` plus the `### Search and transaction
internals` heading at `:86`, which needs renaming rather than deleting — `:88-90`
are the three `CYODA_SEARCH_*` vars and they stay —
`cmd/cyoda/help/config_registry_test.go:22` (`"tx": true` in the `validTopic`
whitelist, dead once the three vars go), and
`scripts/multi-node-docker/start-cluster.sh:418`, which emits
`CYODA_TX_TTL: "60s"` into the generated compose file for every cluster node.

### 4.3 Claims in the tree that must be corrected

These ship today and describe a capability that has never existed:

- `docs/PRD.md:346` — "A background reaper goroutine periodically scans for
  expired transactions and rolls them back", under its own
  `### Transaction Timeout and Reaper` heading at `:344`.
- `docs/PRD.md:319` — the `ROLLBACK ◄──── timeout (TTL reaper)` state diagram.
- `docs/ARCHITECTURE.md:365-380` — an entire section, "3.4 Transaction Lifecycle
  Manager", with a struct listing. Deleting it orphans the unrelated CBD paragraph
  at `:382` into §3.3, and breaks the live cross-references at `:566` and `:742`;
  both need rehoming, not just removal.
- `docs/ARCHITECTURE.md:123` — `lifecycle/ Transaction lifecycle manager (TTL,
  reaper, outcomes)` in the package tree.
- `docs/ARCHITECTURE.md:1425-1426`, `:1428` — the three env vars as live knobs.
  `:1427` is `CYODA_PROXY_TIMEOUT` and stays.
- `docs/ARCHITECTURE.md:1569` — the DD-2 design-decision rationale.
- `docs/ARCHITECTURE.md:1650-1651` — "Workflow chains that exceed TTL are reaped.
  Long-running processors must complete within this window", and the companion row
  advising that `idle_in_transaction_session_timeout` should *exceed* the TTL.
  Both are rewritten around §2's ceilings, which invert that advice: the DB-side
  limit is now the authority. `:1652-1655` are unrelated rows and stay.
- `docs/CONCURRENCY.md:63` and `:105` — the latter cites a path that stops
  existing.
- `docs/analysis/failure-modes/…-playbook.md:59` — the actionable companion to the
  analysis document dispositioned below. A playbook that instructs an operator to
  rely on a reaper that does not run is worse than no playbook.
- `e2e/parity/multinode/cbd_tx_pinning.go:54` — "not yet wired into the runtime".
  Comment-only, no compile dependency.

`docs/analysis/failure-modes/2026-06-29-operational-failure-mode-analysis.md:288,311`
names `lifecycle.Manager` as remediation R1 for this exact issue. That directory is
a historical record like `docs/plans/`, so it is not rewritten; the spec records
the disposition so a future reader does not re-propose the wired reaper.

### 4.4 `docs/ARCHITECTURE.md` — full audit

The removal touches six places in this document, which is the wrong way to treat
it. `ARCHITECTURE.md` is the reference a reader trusts to know what the system
does; a stale claim there is worse than no claim, because it is acted on. So the
whole document is audited in this change, not just the lifecycle references.

**Scope.** All 14 sections, claim by claim, against the code. A claim that cannot
be verified is **deleted**, not softened — an unverifiable assertion in a reference
document is a liability, and the project's own stance is to fail closed.

**Editing rule: current state only, present tense.** Describe the system as it is.
No "previously X, now Y", no "this was changed", no migration notes. The delta
belongs in `CHANGELOG.md`; the state belongs here. This applies to the lifecycle
removal specifically — §3.4 and its references come out cleanly, with the DB-side
ceilings described on their own terms rather than as a replacement for something.

**Already known wrong** (each verified while writing this spec):

- §3.4 "Transaction Lifecycle Manager" (`:365-380`) and the package-tree entry
  (`:123`) — describe a component being deleted. Removing §3.4 orphans the CBD
  paragraph at `:382` and breaks live cross-references at `:566` and `:742`.
- `:1425-1426`, `:1428` — three env vars presented as live knobs; they are being
  removed. (`:1427` is `CYODA_PROXY_TIMEOUT` and stays.)
- `:1650-1651` — "Workflow chains that exceed TTL are reaped. Long-running
  processors must complete within this window", plus the row advising that
  `idle_in_transaction_session_timeout` should *exceed* the TTL. Both are false
  today and inverted by §2, which makes the DB-side limit the authority.
- DD-2 (`:1569`) — "The lifecycle manager provides TTL, registry, and
  observability" is false. Its other premise, "PostgreSQL rolls back the
  transaction automatically via idle timeout", is *also* false today (no such
  timeout is set anywhere) and becomes true for the first time under §2. The
  decision itself — fencing tokens not required — stands on the `pgx.Tx`
  single-owner property alone; the rationale needs rewriting to say so.
- §12 "Planned Features (Not Yet Implemented)" — at least three of six rows ship
  today: batch `SaveResults` with `pgx.CopyFrom` (`search_store.go:113`), the
  `cyoda-go-spi/spitest/` conformance harness (present in the pinned SPI), and
  multi-node E2E tests with proxy routing (`e2e/parity/multinode/`). Every row is
  re-verified. The section's framing — "items carried forward from the
  `cyoda-light-go` predecessor repository" — is exactly the historical narrative
  this document should not carry, and goes; a list of what the system does *not*
  do is current-state information and stays.

**§13 stays.** A design-decisions log records why the system is shaped as it is,
which is current-state rationale, not a change narrative. What it must not do is
justify a decision with a component that does not exist — hence the DD-2 rewrite.

The audit runs as its own commit, separate from the lifecycle removal, so the
mechanical deletion stays reviewable on its own.

---

## 5. Error and status codes

New code: `STORAGE_UNAVAILABLE` — 503, retryable. Emitted when the pool cannot
supply a connection within `CYODA_POSTGRES_ACQUIRE_TIMEOUT`, or when an operation
finds its transaction aborted by the idle-in-transaction ceiling (SQLSTATE
`25P03`). Needs `cmd/cyoda/help/content/errors/STORAGE_UNAVAILABLE.md`
(`TestErrCode_Parity` is a strict bijection).

No status code changes to any existing endpoint. `503` becomes newly *reachable*
on the entity write operations, which must therefore declare it in
`api/openapi.yaml` — the e2e validator runs `ValidateResponse` with
`IncludeResponseStatus=true` (`internal/e2e/openapivalidator/validator.go:168`), so
an undeclared 503 fails conformance.

| Operation | Method + path | Declared today | Add |
|---|---|---|---|
| `create` | POST `/entity/{format}/{entityName}/{modelVersion}` | 200, 400, 401, 403, 404, 409, 422 | 503 |
| `createCollection` | POST `/entity/{format}` | 200, 400, 401, 403, 404, 409, 422 | 503 |
| `updateSingle` | PUT `/entity/{format}/{entityId}/{transition}` | 200, 400, 401, 403, 404, 409, 412, 422 | 503 |
| `updateSingleWithLoopback` | PUT `/entity/{format}/{entityId}` | 200, 400, 401, 403, 404, 409, 412, 422 | 503 |
| `updateCollection` | PUT `/entity/{format}` | 200, 400, 401, 403, 404, 409, 422 | 503 |
| `patchSingle` | PATCH `/entity/{format}/{entityId}/{transition}` | 200, 400, 401, 403, 404, 409, 412, 415, 422, 428, 501 | 503 |
| `patchSingleWithLoopback` | PATCH `/entity/{format}/{entityId}` | 200, 400, 401, 403, 404, 409, 412, 415, 422, 428, 501 | 503 |
| `deleteSingleEntity` | DELETE `/entity/{entityId}` | 200, 400, 401, 403, 404 | 503 |
| `deleteEntities` | DELETE `/entity/{entityName}/{modelVersion}` | 200, 400, 401, 403, 404 | 503 |

Every one of these also carries `default: InternalServerError` (500), which is
unchanged. The "declared today" column is extracted from `api/openapi.yaml` at
HEAD; no existing entry is modified, and the added 503 is the only change to any
operation's response set.

gRPC: the same failure surfaces in the envelope as `Success=false` with
`Error.Code = CLIENT_ERROR` and `Error.Retryable = true`. `Error.Code` is the
envelope *class*, not the domain code — `buildErrorFields`
(`internal/grpc/errors.go`) maps every operational `AppError` to `CLIENT_ERROR`
and carries the domain code in `Error.Message`, here
`STORAGE_UNAVAILABLE: storage is temporarily unavailable — retry`. That is the
established convention for this surface, so the gRPC assertion is on the message
prefix plus the retryable flag, not on `Error.Code`.

---

## 6. Coverage matrix

| # | Scenario | Unit | Running-backend e2e | Cross-backend parity | gRPC |
|---|---|---|---|---|---|
| 1 | Panic in an owned write path rolls back; pool returns to baseline | — | postgres | — | — |
| 2 | Repeated panics beyond pool size leave the node serving requests | — | postgres | — | — |
| 3 | Panic in a joined callback does **not** roll back the owner's tx | ✔ | postgres | — | — |
| 4 | Panic after engine segmentation rolls back TX_post, not the entry tx | ✔ | postgres | — | — |
| 4a | **Non-panic** engine error after segmentation (criterion callout fails mid-cascade) rolls back TX_post | ✔ | postgres | — | — |
| 4b | Every `executeCommitBeforeDispatch` `return nil, "", err` path rolls its segment back | ✔ | — | — | — |
| 4c | Criterion dispatched after a CBD segment carries the *current* segment's txID, joinable by the callback | ✔ | postgres | — | — |
| 5 | Committed-transaction behaviour unchanged (existing write suites) | ✔ | ✔ | ✔ | ✔ |
| 5a | **Ordinary error paths still roll back** — one case per converted flow, asserting the tx is gone, not just the status code | ✔ | postgres | — | — |
| 6 | Panicking write on memory/sqlite releases its tx state — no leaked buffer, `committedLog` prune floor advances | ✔ | — | — | — |
| 7 | gRPC handler panic is recovered; process survives; tx rolled back | — | — | — | ✔ |
| 7a | Peer scheduler-RPC panic is recovered; process survives; fire tx rolled back | — | postgres | — | — |
| 7b | A recovered panic fails readiness — `/readyz` 503, healthy node still 200, reason distinguishable from a storage fault, `/livez` unchanged | ✔ | postgres | — | — |
| 7c | A panic in the async-search goroutine latches the same flag; a successful job does not | ✔ | — | — | — |
| 7d | A panic in the scheduler's dispatch goroutine latches the same flag; a normal dispatch does not | ✔ | — | — | — |
| 7e | `app.New` hands its own flag to every latching site — a dropped wiring line fails | ✔ | — | — | — |
| 8 | `Release` holds the per-tx gate while rolling back | ✔ | — | — | — |
| 8a | `Release` on a **cancelled** request context still rolls back — `WithoutCancel` keeps the `UserContext` `verifyTenant` needs | ✔ | postgres | — | — |
| 8b | Joined call that unexpectedly segments rolls the engine-opened segment back despite `owned == false` (stubbed engine) | ✔ | — | — | — |
| 8c | Engine-side guard on memory and sqlite, not only postgres | ✔ | — | — | — |
| 9 | Idle-in-tx beyond the ceiling: session aborted, next op returns 503 `STORAGE_UNAVAILABLE` | — | postgres | — | ✔ |
| 10 | Saturated pool: a **write** returns 503 within the acquire timeout rather than queueing | — | postgres | — | ✔ |
| 11 | Caller-cancelled request is **not** mislabelled `STORAGE_UNAVAILABLE` | ✔ | postgres | — | — |
| 11a | GUC values render in a form PostgreSQL accepts (bare ms integers, never `5m`) | ✔ | postgres | — | — |
| 11c | Async search job aborted by its own ceiling records FAILED with a fixed, non-revealing message | — | postgres | — | — |
| 11e | A transaction stays usable well past the acquire timeout — the deadline does not leak into the context `Begin` returns | ✔ | postgres | — | — |
| 11f | `25P03` classification also clears `registry`/`tenants`/`origins`/`txStates` | ✔ | — | — | — |
| 11g | Sub-millisecond ceiling values are rejected at parse time, not truncated to "disabled" | ✔ | — | — | — |
| 11h | A ceiling set only in the DSN survives; one set in both logs a WARN and takes the env var | ✔ | — | — | — |
| 11b | Deep cascade whose total callout time exceeds the idle ceiling still commits (per-gap, not cumulative) | — | postgres | — | — |
| 12 | `statement_timeout` fires → SQLSTATE `57014` → 500 with a ticket, cause named in the log | ✔ | postgres | — | — |
| 13 | `lock_timeout` aborts a `pg_advisory_lock` wait (needs a live server — not a unit test) | — | postgres | — | — |
| 13a | A genuinely dirty schema reports the actionable message, not golang-migrate's bare `ErrDirty` | ✔ | postgres | — | — |
| 14 | Migration connection inherits neither `statement_timeout` nor `idle_in_transaction_session_timeout` from the app pool | ✔ | — | — | — |
| 15 | Fault-injected interleave — node B reads the version exactly while node A holds the lock and has stamped dirty — proceeds instead of reporting dirty. **Must fail on HEAD** | — | postgres | — | — |
| 15a | `cyoda migrate` concurrent with a node boot: same, via the shared ordering | — | postgres | — | — |
| 16 | Genuinely dirty schema (no concurrent migrator) still fails fast | ✔ | postgres | — | — |
| 16a | Database newer than the binary still refuses to start, despite migrations now running first | ✔ | postgres | — | — |
| 17 | Single-node install migrates itself | — | postgres | — | — |
| 18 | Guard test rejects a new non-concurrent index on a hot table | ✔ | — | — | — |
| 19 | `STORAGE_UNAVAILABLE` declared in OpenAPI on every write op | — | conformance | — | — |

Row 5a is the highest-value row in the table. Row 5 leans on the existing write
suites, which assert response codes and never observe transaction state — so a
single defect in `Release` would leave a transaction open on *every* error path in
the entity service with no test noticing. 5a asserts the transaction is actually
gone.

Scenarios 1, 2 and 10 are concurrency/fault tests: isolated single-backend e2e,
never the shared parity suite, and they assert consistency (pool returns to
baseline, one winner, no torn write) rather than a precise interleave.

Row 7b's e2e cell rides on 7a: the same real panic through the assembled handler,
then `/readyz` 503 and `/livez` 200 on that node. Rows 7c, 7d and 7e are unit-only
by waiver — reaching the async-search or dispatch goroutine's recover from e2e
means panicking the store layer mid-scan or mid-fire, which needs in-process
injection an e2e stack has no seam for, and 7e is a wiring assertion with no
runtime surface at all. Each drives the production goroutine directly, and 7b
already proves the flag reaches `/readyz`. 7e is the row that keeps the other two
honest: both sites take the flag through a silent seam (a chained option, a struct
field), and a dropped wiring line compiles and passes everything else — which is
how the async-search site came to be missing it.

**No parity cells.** Every scenario here is single-backend, and that is forced by
the harness rather than chosen: `BackendFixture` (`e2e/parity/fixture.go:17-48`)
exposes only `BaseURL`, `GRPCEndpoint`, `NewTenant` and `ComputeTenant` — "no
storage handle — verification is API-only", and the compute client is a separate
subprocess reached over gRPC, so a parity scenario can neither inject an in-process
panic nor observe whether a transaction is still open. Rows 3, 4a and 5a were
originally marked for parity and are not implementable there. The behaviour they
cover is core code that every backend shares, so the loss is coverage of *backend
divergence in the guard*, which no backend can express: `Release` never reaches a
plugin except through `Rollback`, whose contract is already parity-tested.

**Fixture.** The shared e2e suite builds one `app.App` and one pool in `TestMain`
with `CYODA_POSTGRES_MAX_CONNS=5` (`internal/e2e/e2e_test.go:106`, `:156`).
Scenarios 1, 2 and 10 need their own app with a deliberately tiny pool — run
against the shared one they cannot isolate, and row 10 would stall the rest of the
suite for a full acquire timeout. `newCallbackHarnessConfigured`
(`internal/e2e/callback_harness_test.go:175`, `:226`) already builds a separate
postgres-backed `app.App` with a `configure func(*app.Config)` hook; these
scenarios reuse it rather than inventing a second harness.

Pool statistics do **not** come from `App.StoreFactory()`: that returns the
`*modelcache.CachingStoreFactory` wrapper (`app/app.go:191`, `:801`), which holds
the real factory in an unexported field and forwards only the `spi.StoreFactory`
methods, so a type assertion to `interface{ Pool() *pgxpool.Pool }` fails. The
tests open their own pool from the same DSN and read `Stat()` there.

**Panic injection** goes through `cfg.ExternalProcessing`
(`internal/e2e/e2e_test.go:140`, `internal/testing/localproc`) — but **not** via a
processor: `localproc.DispatchProcessor` recovers panics and converts them to
errors (`localproc.go:104-108`), as does `DispatchFunction` (`:149-153`). Only
`DispatchCriteria` (`:114-135`) has no recover, so a panicking **criteria**
callback is the one that reaches `engine.Execute` intact. Where a test needs the
panic on the processor side specifically, `localproc` gains an explicit opt-in
`RegisterPanickingProcessor` that skips the recover — test-only code in a
test-only package, with nothing compiled into the binary.

---

## 7. Documentation (Gate 4)

- New vars in `plugins/postgres/plugin.go` `ConfigVars()`, `parseConfig`, and
  `cmd/cyoda/help/content/config/database.md`:
  `CYODA_POSTGRES_STATEMENT_TIMEOUT`, `CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT`,
  `CYODA_POSTGRES_ACQUIRE_TIMEOUT`, `CYODA_POSTGRES_MIGRATE_LOCK_TIMEOUT`,
  `CYODA_POSTGRES_SEARCH_STATEMENT_TIMEOUT`.
- The config-surface change is CI-atomic in both directions:
  `TestConfig_EnvVarCoverage` (`cmd/cyoda/help/help_test.go:488`) and
  `TestRootConfigVars_MatchDefaults` (`app/config_registry_binding_test.go:171`)
  both fail on a partial add or a partial removal, so the five additions and three
  deletions land in one commit each, not spread across the branch.
- Removed vars: `cmd/cyoda/help/config_registry.go`,
  `cmd/cyoda/help/content/config.md`, `docs/ARCHITECTURE.md`.
- `cmd/cyoda/help/content/errors/STORAGE_UNAVAILABLE.md`.
- Migration guidance (CONCURRENT pattern, INVALID-index recovery) in
  `cmd/cyoda/help/content/cli/migrate.md`.
- Name-level parity is already enforced: `TestConfigAll_Complete`
  (`cmd/cyoda/help/config_registry_test.go:73`) scans `cmd`, `app`, `plugins` and
  `internal` for `CYODA_*` and diffs against the merged registry, so adding a var
  to `parseConfig` alone fails CI. The real gap is **default-value** parity —
  `plugin.go:19`'s `"25"` and `config.go:36`'s `25` are unbound literals, and root
  vars get `TestRootConfigVars_MatchDefaults`
  (`app/config_registry_binding_test.go:171`) while plugin vars get no equivalent.
  A plugin-side counterpart is added alongside the four new vars (Gate 6), so a
  documented default that drifts from the code fails rather than misinforms.
- `docs/ARCHITECTURE.md` full audit — see §4.4. Current state, present tense, no
  change narrative; unverifiable claims deleted rather than softened.
- `CHANGELOG.md` needs a `### Breaking` section: three env vars are removed, and
  three ceilings that did not exist now apply by default. Per `COMPATIBILITY.md`
  that section, not the version digit, is what consumers are told to read.
  v0.8.4 remains a PATCH — the HTTP/wire API is unchanged.

## 8. Out of scope

- Capping `responseTimeoutMs` against the idle ceiling. It is per-processor
  workflow config and currently uncapped, so a workflow may configure a callout
  longer than 5m and have its transaction aborted. This is reconciled here by
  classifying SQLSTATE `25P03` into a clear, actionable error (§2.2) and
  documenting the relationship, rather than by adding cross-layer validation
  between workflow import and storage-plugin config. A cap belongs with the gRPC
  hardening work.
- Request-level context deadlines (#32), which would bound pool acquisition on
  read paths as well. Until then, non-transactional reads still queue unboundedly
  on acquire — every statement through `store_factory.go:114`'s pool fallback and
  the thirteen direct pool calls in `search_store.go`. §2.1's "net" claim is about
  connection *release*, not about waiting.
- Context-aware acquisition for `txgate.Registry.Acquire` and `tx.OpMu` (see
  §1.2). Both would turn the rollback's bound from "terminates because the holder
  terminates" into a hard deadline, and both are changes to the concurrency model
  of core plus two plugins rather than to transaction lifecycle.
- `fetchEntityTransitions` lacks the 503 that its documented alias
  `getEntityTransitions` declares (`api/openapi.yaml:1584`,
  `transitions_handler.go:117`). Pre-existing drift on the same surface, unrelated
  to this change's mechanism; flagged so it is not mistaken for something this
  change introduced.
