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

Line numbers omitted from this table — they go stale on every refactor.
Cross-reference by method name in `plugins/memory/entity_store.go`.

| Method | Tx-state fields touched | Current locking | Required | Verdict |
|---|---|---|---|---|
| `Save` | reads `tx.RolledBack`; writes `tx.Buffer[id]`, `tx.WriteSet[id]` | `tx.OpMu.RLock` + entityMu.RLock (lock order: OpMu before entityMu) | `tx.OpMu.RLock` | ✅ correct (PR #153 + #198) |
| `CompareAndSave` | reads `tx.RolledBack`; writes `tx.Buffer[id]`, `tx.WriteSet[id]` | `tx.OpMu.RLock` + entityMu.RLock | `tx.OpMu.RLock` | ✅ correct (PR #153) |
| `Get` | reads `tx.RolledBack`, `tx.Deletes`, `tx.Buffer`, `tx.SnapshotTime`; writes `tx.ReadSet[id]` | `tx.OpMu.RLock` + entityMu.RLock | `tx.OpMu.RLock` | ✅ correct (#176 / PR #198) |
| `GetAsAt` | reads `tx.RolledBack`; writes `tx.ReadSet[id]` | `tx.OpMu.RLock` (conditional on tx) + entityMu.RLock | `tx.OpMu.RLock` | ✅ correct (#176 / PR #198) |
| `GetAll` | reads `tx.RolledBack`, `tx.Deletes`, `tx.SnapshotTime`, iterates `tx.Buffer`; writes `tx.ReadSet[id]` | `tx.OpMu.RLock` + entityMu.RLock | `tx.OpMu.RLock` | ✅ correct (#176 / PR #198) |
| `Count` (delegates to `GetAll`) | inherits | inherits | inherits | ✅ correct (#176 / PR #198) |
| `CountByState` (delegates to `GetAll`) | inherits | inherits | inherits | ✅ correct (#176 / PR #198) |
| `Delete` | reads `tx.RolledBack`, `tx.Buffer`; writes `tx.Deletes[id]`, deletes from `tx.Buffer`, writes `tx.WriteSet[id]` | `tx.OpMu.RLock` + entityMu.RLock | `tx.OpMu.RLock` | ✅ correct (#176 / PR #198) |
| `DeleteAll` | reads `tx.RolledBack`, `tx.SnapshotTime`; iterates and mutates `tx.Buffer`, writes `tx.Deletes`, `tx.WriteSet` | `tx.OpMu.RLock` + entityMu.RLock | `tx.OpMu.RLock` | ✅ correct (#176 / PR #198) |
| `Exists` | reads `tx.RolledBack`, `tx.Deletes`, `tx.Buffer`, `tx.SnapshotTime` | `tx.OpMu.RLock` + entityMu.RLock | `tx.OpMu.RLock` | ✅ correct (#176 / PR #198) |

### `txmanager.go` — transaction lifecycle

| Method | Tx-state fields touched | Current locking | Required | Verdict |
|---|---|---|---|---|
| `Begin` | constructs new `tx`; writes `m.active[txID]` | `m.mu` (brief, only for `m.active` write) | `m.mu` | ✅ correct (no contention before tx is published) |
| `Join` | reads `tx.RolledBack`, `tx.Closed`, `tx.TenantID` | `m.mu` (brief, for `m.active` lookup) + `tx.OpMu.RLock` (IIFE) for flag reads (PR-A) | `m.mu` for lookup; `tx.OpMu.RLock` for flag reads | ✅ **fixed by PR-A (#199)** |
| `Commit` | reads `tx.SnapshotTime`, `tx.ReadSet`, `tx.WriteSet`, `tx.TenantID`; iterates `tx.Buffer`, `tx.Deletes`; writes `tx.Closed` (defer) | `tx.OpMu.Lock` + entityMu.Lock + `m.mu` (brief, multiple sections) | `tx.OpMu.Lock` | ✅ correct |
| `Rollback` | reads `tx.TenantID`; writes `tx.RolledBack` (under `m.mu`), `tx.Closed` (defer, under `tx.OpMu.Lock`) | `tx.OpMu.Lock` + `m.mu` (brief) | `tx.OpMu.Lock` | ✅ correct |
| `GetSubmitTime` | reads `m.active`, `m.submitTimes`; does not access `tx.*` fields on active txs (early-exits) | `m.mu` | `m.mu` | ✅ correct |
| `CommittedLogLen` | reads `m.committedLog` only | `m.mu` | `m.mu` | ✅ correct |
| **`Savepoint`** | reads `tx.RolledBack`, `tx.Closed`, `tx.Buffer`, `tx.ReadSet`, `tx.WriteSet`, `tx.Deletes` (deep-copy snapshot); reads `tx.TenantID` for tenant check | **`tx.OpMu.RLock` + `m.mu` for `m.savepoints` write (PR-A); tenant check from ctx (PR-A I-1)** | `tx.OpMu.RLock` + tenant check | ✅ **fixed by PR-A (#199)** |
| **`RollbackToSavepoint`** | reads `tx.RolledBack`, `tx.Closed`, `tx.TenantID`; writes `tx.Buffer`, `tx.ReadSet`, `tx.WriteSet`, `tx.Deletes` (replace) | **`tx.OpMu.Lock` + `m.mu` for `m.savepoints` write (PR-A); tenant check from ctx (PR-A I-1)** | `tx.OpMu.Lock` + tenant check | ✅ **fixed by PR-A (#199)** |
| **`ReleaseSavepoint`** | reads `tx.TenantID` for tenant check; does not access any other `tx.*` field — only mutates `m.savepoints` | **`m.mu` + tenant check from ctx (PR-A I-1)** | `m.mu` + tenant check | ✅ **tenant check added by PR-A (#199); locking unchanged (no tx-state to coordinate)** |

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

## Tenant isolation finding (review I-1)

The audit also surfaced a tenant-isolation gap in the three savepoint
methods (`Savepoint`, `RollbackToSavepoint`, `ReleaseSavepoint`): pre-fix
they took `_ context.Context` and never compared the caller's tenant
against `tx.TenantID`. A caller authenticated as tenant A who learned a
tenant B txID could record/rollback/release savepoints on tenant B's
tx-state. `Commit` and `Rollback` already had this protection.

**Pre-fix severity (security-auditor assessment):** the most severe path
was `RollbackToSavepoint(ctxA, txBID, spID)` from tenant A —
**destructive integrity loss on tenant B's tx-state**, replacing tenant
B's `Buffer`/`ReadSet`/`WriteSet`/`Deletes` with a snapshot tenant A
controlled. `Savepoint` from the wrong tenant produced a savepointID for
tenant B's state (information disclosure on tenant B's tx-namespace
keying); `ReleaseSavepoint` was an availability-impact path (clearing
tenant B's snapshot). Mitigated in practice by 128-bit txID entropy
(infeasible to enumerate), but a tenant A caller who *learned* a tenant B
txID by any means (logs, support channels, side channels) could exploit
this without authentication bypass. Pre-fix this gap was a high-severity
tenant-isolation flaw on the savepoint surface.

**Fixed by PR-A (#199 review I-1):** all three methods now resolve the
caller's `UserContext` and reject mismatched-tenant calls with a
`"tenant mismatch"` error. Regression tests at
`plugins/memory/txmanager_test.go::TestSavepoint_RejectsCrossTenant`,
`TestRollbackToSavepoint_RejectsCrossTenant`,
`TestReleaseSavepoint_RejectsCrossTenant`.

## Summary

| Status | Count | Methods |
|---|---|---|
| ✅ Correct | 13 | All entity-store ops (10, including `CountByState`), `Begin`, `Commit`, `Rollback`, `GetSubmitTime`, `CommittedLogLen` |
| ✅ Fixed by PR-A (locking) | 3 | `Savepoint`, `RollbackToSavepoint`, `Join` |
| ✅ Fixed by PR-A (tenant isolation, review I-1) | 3 | `Savepoint`, `RollbackToSavepoint`, `ReleaseSavepoint` |

After PR-A merges, the memory plugin has zero outstanding tx-state locking
gaps and zero outstanding tenant-isolation gaps in the savepoint surface.
Every method touching `*spi.TransactionState` holds the appropriate lock
for the field-access pattern it performs and rejects cross-tenant callers.
