# Memory Plugin — `*spi.TransactionState` Locking Audit

Issue #199 PR-A. Enumerates every method in `plugins/memory/` that touches
fields on `*spi.TransactionState`, what it touches, the locking it currently
holds, the locking required by the design, and the verdict.

## Locking model recap

- `m.mu` (per-`TransactionManager`) — protects the `m.active`,
  `m.committedLog`, `m.committing`, `m.submitTimes`, `m.savepoints` maps.
  Brief hold; never held across slow I/O.
- `tx.OpMu` (per-transaction `sync.RWMutex`, lives in `*spi.TransactionState`)
  — separates the in-flight-op class from the closure class:
  - In-flight ops on tx state (Save, CompareAndSave, Get, GetAll, GetAsAt,
    Delete, DeleteAll, Exists, Count, Savepoint) hold `tx.OpMu.RLock` so
    multiple readers can run.
  - Closure / structural-mutation ops (Commit, Rollback, RollbackToSavepoint)
    hold `tx.OpMu.Lock` so they wait for in-flight ops to drain and then run
    exclusive.
- `m.factory.entityMu` — protects the committed entity store. Lock order is
  always `tx.OpMu` before `m.factory.entityMu` (matching `Commit`'s flush).
- The application is responsible per `TransactionManager.Join`'s godoc for
  not firing concurrent ops on the same tx; `tx.OpMu` does **not** mutually
  exclude RLock-holders from each other (that is the application's job).

## Method-level audit

### `entity_store.go` — tx-path entity ops

| Method (line) | Tx-state fields touched | Current locking | Required | Verdict |
|---|---|---|---|---|
| `Save` (99–116) | reads `tx.RolledBack`; writes `tx.Buffer[id]`, `tx.WriteSet[id]` | `tx.OpMu.RLock` + entityMu.RLock (lock order: OpMu before entityMu) | `tx.OpMu.RLock` | ✅ correct (PR #153 + #198) |
| `CompareAndSave` (124–161) | reads `tx.RolledBack`; writes `tx.Buffer[id]`, `tx.WriteSet[id]` | `tx.OpMu.RLock` + entityMu.RLock | `tx.OpMu.RLock` | ✅ correct (PR #153) |
| `Get` (246–268) | reads `tx.RolledBack`, `tx.Deletes`, `tx.Buffer`, `tx.SnapshotTime`; writes `tx.ReadSet[id]` | `tx.OpMu.RLock` + entityMu.RLock | `tx.OpMu.RLock` | ✅ correct (#176 / PR #198) |
| `GetAsAt` (287–298) | reads `tx.RolledBack`; writes `tx.ReadSet[id]` | `tx.OpMu.RLock` (conditional on tx) + entityMu.RLock | `tx.OpMu.RLock` | ✅ correct (#176 / PR #198) |
| `GetAll` (341–377) | reads `tx.RolledBack`, `tx.Deletes`, `tx.SnapshotTime`, iterates `tx.Buffer`; writes `tx.ReadSet[id]` | `tx.OpMu.RLock` + entityMu.RLock | `tx.OpMu.RLock` | ✅ correct (#176 / PR #198) |
| `Count` (delegates to `GetAll`) | inherits | inherits | inherits | ✅ correct (#176 / PR #198) |
| `Delete` (443–476) | reads `tx.RolledBack`, `tx.Buffer`; writes `tx.Deletes[id]`, deletes from `tx.Buffer`, writes `tx.WriteSet[id]` | `tx.OpMu.RLock` + entityMu.RLock | `tx.OpMu.RLock` | ✅ correct (#176 / PR #198) |
| `DeleteAll` (506–547) | reads `tx.RolledBack`, `tx.SnapshotTime`; iterates and mutates `tx.Buffer`, writes `tx.Deletes`, `tx.WriteSet` | `tx.OpMu.RLock` + entityMu.RLock | `tx.OpMu.RLock` | ✅ correct (#176 / PR #198) |
| `Exists` (582–601) | reads `tx.RolledBack`, `tx.Deletes`, `tx.Buffer`, `tx.SnapshotTime` | `tx.OpMu.RLock` + entityMu.RLock | `tx.OpMu.RLock` | ✅ correct (#176 / PR #198) |

### `txmanager.go` — transaction lifecycle

| Method (line) | Tx-state fields touched | Current locking | Required | Verdict |
|---|---|---|---|---|
| `Begin` (75–103) | constructs new `tx`; writes `m.active[txID]` | `m.mu` (brief, only for `m.active` write) | `m.mu` | ✅ correct (no contention before tx is published) |
| `Join` (109–139) | reads `tx.RolledBack`, `tx.Closed`, `tx.TenantID` | `m.mu` (brief, for `m.active` lookup) + `tx.OpMu.RLock` (IIFE) for flag reads (PR-A) | `m.mu` for lookup; `tx.OpMu.RLock` for flag reads | ✅ **fixed by PR-A (#199)** |
| `Commit` (133–283) | reads `tx.SnapshotTime`, `tx.ReadSet`, `tx.WriteSet`, `tx.TenantID`; iterates `tx.Buffer`, `tx.Deletes`; writes `tx.Closed` (defer) | `tx.OpMu.Lock` + entityMu.Lock + `m.mu` (brief, multiple sections) | `tx.OpMu.Lock` | ✅ correct |
| `Rollback` (286–314) | reads `tx.TenantID`; writes `tx.RolledBack` (under `m.mu`), `tx.Closed` (defer, under `tx.OpMu.Lock`) | `tx.OpMu.Lock` + `m.mu` (brief) | `tx.OpMu.Lock` | ✅ correct |
| `GetSubmitTime` (318–331) | reads `m.active`, `m.submitTimes`; does not access `tx.*` fields on active txs (early-exits) | `m.mu` | `m.mu` | ✅ correct |
| `CommittedLogLen` (335–339) | reads `m.committedLog` only | `m.mu` | `m.mu` | ✅ correct |
| **`Savepoint` (343–399)** | reads `tx.RolledBack`, `tx.Closed`, `tx.Buffer`, `tx.ReadSet`, `tx.WriteSet`, `tx.Deletes` (deep-copy snapshot) | **`tx.OpMu.RLock` + `m.mu` for `m.savepoints` write (PR-A)** | `tx.OpMu.RLock` | ✅ **fixed by PR-A (#199)** |
| **`RollbackToSavepoint` (403–449)** | reads `tx.RolledBack`, `tx.Closed`; writes `tx.Buffer`, `tx.ReadSet`, `tx.WriteSet`, `tx.Deletes` (replace) | **`tx.OpMu.Lock` + `m.mu` for `m.savepoints` write (PR-A)** | `tx.OpMu.Lock` | ✅ **fixed by PR-A (#199)** |
| `ReleaseSavepoint` (459–479) | does not access any `tx.*` field — only mutates `m.savepoints` | `m.mu` | `m.mu` | ✅ correct (no tx-state to coordinate) |

## Other gaps surfaced by this audit

### `Join` — unsynchronised reads of `tx.RolledBack` and `tx.Closed` (FIXED)

**File:** `plugins/memory/txmanager.go:117` (pre-fix)

**Pattern (pre-fix):**
```go
m.mu.Lock()
tx, ok := m.active[txID]
m.mu.Unlock()
// ...
if tx.RolledBack || tx.Closed {  // ← unsynchronised read
    return nil, fmt.Errorf("transaction already closed: %s", txID)
}
```

**Why it raced:**
- `Rollback` writes `tx.RolledBack` inside its `m.mu` critical section
  (txmanager.go:307–312). `Join` read it **outside** the brief `m.mu` lookup
  region. The `m.mu`-Unlock/Lock sequence does not establish happens-before
  for code outside the critical sections.
- `Commit` and `Rollback` both write `tx.Closed` in a defer that runs under
  `tx.OpMu.Lock` only — never under `m.mu`. `Join` did not hold `tx.OpMu`
  when reading `tx.Closed`, so the read was fully unsynchronised against
  that write.

**Effect:** Per Go's memory model, a data race. Race detector flagged it via
`TestJoin_VsRollback_NoRace`. Semantically the read was benign (TOCTOU
regardless), but the unsynchronised access pattern was unsafe.

**Fix (PR-A):** Wrap the flag reads in an IIFE that holds `tx.OpMu.RLock`,
matching the discipline of every other tx-path method post-#176.

```go
closed := func() bool {
    tx.OpMu.RLock()
    defer tx.OpMu.RUnlock()
    return tx.RolledBack || tx.Closed
}()
```

Regression test: `plugins/memory/concurrency_savepoint_test.go::TestJoin_VsRollback_NoRace`.

## Summary

| Status | Count | Methods |
|---|---|---|
| ✅ Correct | 13 | All entity-store ops (9), `Begin`, `Commit`, `Rollback`, `GetSubmitTime`, `CommittedLogLen`, `ReleaseSavepoint` |
| ✅ Fixed by PR-A | 3 | `Savepoint`, `RollbackToSavepoint`, `Join` |

After PR-A merges, the memory plugin has zero outstanding tx-state locking
gaps. Every method touching `*spi.TransactionState` holds the appropriate
lock for the field-access pattern it performs.
