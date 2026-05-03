# SQLite Plugin — `*spi.TransactionState` Locking Audit

Issue #199 PR-C1. Parallels `2026-05-memory-plugin-tx-locking.md` for the
sqlite plugin. Covers every method in `plugins/sqlite/` that touches
`*spi.TransactionState` post-fix, plus the asymmetries with the memory
plugin's locking pattern (intentional and otherwise).

## Why this audit was needed

cyoda-go-spi v0.6.1 (PR-B, merged at `release/v0.7.0` SHA `9e8e0f1`)
formalised the tx-state OpMu contract. The contract requires every plugin
method that reads or writes `tx.ReadSet` / `tx.WriteSet` / `tx.Buffer` /
`tx.Deletes` / `tx.RolledBack` / `tx.Closed` to acquire `tx.OpMu` in the
appropriate posture — RLock for in-flight ops, Lock for closure ops.

When PR-A landed in the memory plugin (#201), the SPI v0.6.1 contract
became the canonical authority. **The sqlite plugin shipped with the
exact pre-PR-A defects:** `Savepoint` and `RollbackToSavepoint` held
`m.mu` only (no `tx.OpMu`), and all three savepoint methods took
`_ context.Context` (no tenant verification). PR-C1 closes those gaps.

## Locking model recap

Same as the memory plugin (see SPI godoc on `TransactionState`):

- `m.mu` (per-`transactionManager`) — protects manager-level maps
  (`m.active`, `m.committedLog`, `m.committing`, `m.submitTimes`,
  `m.savepoints`, `m.lastSubmitTime`).
- `m.commitMu` — **sqlite-specific**: serialises the entire commit path
  to make the SI+FCW conflict check correct under frozen test clocks
  (see `txmanager.go:188-220`). Memory plugin doesn't need this because
  it serialises commits via `factory.entityMu.Lock()` instead.
- `tx.OpMu` (per-tx `sync.RWMutex` on `*spi.TransactionState`) —
  separates the in-flight-op class from the closure class.
- `factory.db` — the underlying SQLite connection pool. Handles
  durability; not the concurrency controller.

## Method-level audit

### `entity_store.go` — tx-path entity ops

All entity-store ops correctly acquire `tx.OpMu.RLock` for tx-state reads
and writes. Verified against the same race-shape pattern as the memory
plugin's `concurrency_inreadops_test.go`.

| Method | Tx-state fields touched | Current locking | Required | Verdict |
|---|---|---|---|---|
| `Save` | reads `tx.RolledBack`; writes `tx.Buffer[id]`, `tx.WriteSet[id]`; deletes `tx.Deletes[id]` if present | `tx.OpMu.RLock` | `tx.OpMu.RLock` | ✅ correct |
| `CompareAndSave` | reads `tx.RolledBack`; writes `tx.Buffer[id]`, `tx.WriteSet[id]` | `tx.OpMu.RLock` + SQL CAS check | `tx.OpMu.RLock` | ✅ correct |
| `Get` | reads `tx.RolledBack`, `tx.Deletes`, `tx.Buffer`; writes `tx.ReadSet[id]` | `tx.OpMu.RLock` | `tx.OpMu.RLock` | ✅ correct |
| `GetAsAt` | reads `tx.RolledBack`; writes `tx.ReadSet[id]` | `tx.OpMu.RLock` | `tx.OpMu.RLock` | ✅ correct |
| `GetAll` | reads `tx.RolledBack`, `tx.Deletes`, iterates `tx.Buffer`; writes `tx.ReadSet[id]` | `tx.OpMu.RLock` | `tx.OpMu.RLock` | ✅ correct |
| `Count` (delegates to `GetAll` in-tx) | inherits | inherits | inherits | ✅ correct |
| `CountByState` (delegates to `GetAll` in-tx) | inherits | inherits | inherits | ✅ correct |
| `Delete` | reads `tx.RolledBack`, `tx.Buffer`; writes `tx.Deletes[id]`, deletes from `tx.Buffer`, writes `tx.WriteSet[id]` | `tx.OpMu.RLock` | `tx.OpMu.RLock` | ✅ correct |
| `DeleteAll` | reads `tx.RolledBack`; iterates and mutates `tx.Buffer`, writes `tx.Deletes`, `tx.WriteSet` | `tx.OpMu.RLock` | `tx.OpMu.RLock` | ✅ correct |
| `Exists` | reads `tx.RolledBack`, `tx.Deletes`, `tx.Buffer` | `tx.OpMu.RLock` | `tx.OpMu.RLock` | ✅ correct |

### `txmanager.go` — transaction lifecycle

| Method | Tx-state fields touched | Current locking (post-PR-C1) | Required | Verdict |
|---|---|---|---|---|
| `Begin` | constructs new `tx`; writes `m.active[txID]` | `m.mu` (brief, only for `m.active` write) | `m.mu` | ✅ correct |
| `Join` | reads `tx.RolledBack`, `tx.Closed`, `tx.TenantID` | `m.mu` held across reads via `defer m.mu.Unlock()` | `m.mu` (sufficient — see asymmetry note below) | ✅ correct |
| `Commit` | reads `tx.SnapshotTime`, `tx.ReadSet`, `tx.WriteSet`, `tx.TenantID`; iterates `tx.Buffer`, `tx.Deletes`; writes `tx.Closed` (defer) | `tx.OpMu.Lock` + `m.commitMu.Lock` + `m.mu` (brief, multiple sections) | `tx.OpMu.Lock` | ✅ correct |
| `Rollback` | reads `tx.TenantID`; writes `tx.RolledBack` (under `m.mu`), `tx.Closed` (defer, under `tx.OpMu.Lock`) | `tx.OpMu.Lock` + `m.mu` (brief, IIFE) | `tx.OpMu.Lock` | ✅ correct |
| `GetSubmitTime` | reads `m.active`, `m.submitTimes`, `submit_times` table | `m.mu` | `m.mu` | ✅ correct |
| `CommittedLogLen` | reads `m.committedLog` only | `m.mu` | `m.mu` | ✅ correct |
| **`Savepoint`** | reads `tx.RolledBack`, `tx.Closed`, `tx.Buffer`, `tx.ReadSet`, `tx.WriteSet`, `tx.Deletes`; reads `tx.TenantID` | **`tx.OpMu.RLock` + `m.mu` for `m.savepoints` write (PR-C1); tenant check from ctx (PR-C1)** | `tx.OpMu.RLock` + tenant check | ✅ **fixed by PR-C1 (#199)** |
| **`RollbackToSavepoint`** | reads `tx.RolledBack`, `tx.Closed`, `tx.TenantID`; writes `tx.Buffer`, `tx.ReadSet`, `tx.WriteSet`, `tx.Deletes` (replace) | **`tx.OpMu.Lock` + `m.mu` for `m.savepoints` write (PR-C1); tenant check from ctx (PR-C1)** | `tx.OpMu.Lock` + tenant check | ✅ **fixed by PR-C1 (#199)** |
| **`ReleaseSavepoint`** | reads `tx.TenantID` for tenant check; does not access any other `tx.*` field — only mutates `m.savepoints` | **`m.mu` + tenant check from ctx (PR-C1)** | `m.mu` + tenant check | ✅ **tenant check added by PR-C1; locking unchanged** |

## Asymmetries with the memory plugin

### `Join` flag-read locking — sqlite is correct without `tx.OpMu.RLock`

The memory plugin's `Join` (pre-PR-A) released `m.mu` BEFORE reading
`tx.RolledBack` / `tx.Closed`, exposing a race against
`Commit`/`Rollback`'s deferred `tx.Closed` write (which runs under
`tx.OpMu.Lock` only, never under `m.mu`). PR-A fixed this by adding a
`tx.OpMu.RLock` IIFE.

Sqlite's `Join` (`txmanager.go:121-140`) holds `m.mu` for the entire
function via `defer m.mu.Unlock()`. The deferred-unlock pattern means
the flag reads happen INSIDE the `m.mu` critical section. Because
`Commit`/`Rollback` delete the tx from `m.active` under `m.mu` BEFORE
their `tx.Closed` defer fires, any subsequent `Join` finds `!ok` and
returns early without ever reading the closure flags. **No tx.OpMu.RLock
needed.**

This asymmetry is intentional and confirmed by `TestJoin_VsRollback_NoRace`
on the memory plugin firing `-race` red-phase, while the same shape on
sqlite passes clean (verified with 200 iterations during PR-C1
investigation).

### Conflict-detection comparator — `!Before` vs `After`

Memory uses `committed.submitTime.After(tx.SnapshotTime)` (strict `>`);
sqlite uses `!committed.submitTime.Before(tx.SnapshotTime)` (`>=`). The
sqlite tightening exists to catch write-write conflicts under a frozen
TestClock where multiple commits get the same timestamp. Memory plugin
sidesteps this by holding `factory.entityMu.Lock()` during commit, which
serialises CompareAndSave reads against the commit until it's visible.
This is documented at `txmanager.go:181-187`.

### `commitMu` — sqlite has it, memory doesn't

Sqlite's `transactionManager.commitMu` (`txmanager.go:42`) serialises the
entire commit path. Required for SI+FCW correctness because sqlite uses
`!Before` (≥) on the timestamp comparison and would otherwise allow two
concurrent commits to both validate against a stale committedLog and
both succeed. Memory plugin gets the equivalent guarantee from
`factory.entityMu.Lock()` (which already serialises the flush phase).

## Summary

| Status | Count | Methods |
|---|---|---|
| ✅ Correct (already) | 14 | All entity-store ops (10), `Begin`, `Commit`, `Rollback`, `Join`, `GetSubmitTime`, `CommittedLogLen` |
| ✅ Fixed by PR-C1 (locking) | 2 | `Savepoint`, `RollbackToSavepoint` |
| ✅ Fixed by PR-C1 (tenant isolation) | 3 | `Savepoint`, `RollbackToSavepoint`, `ReleaseSavepoint` |

After PR-C1 merges, the sqlite plugin has **zero outstanding tx-state
locking gaps** and **zero outstanding tenant-isolation gaps** in the
savepoint surface — same end-state as the memory plugin after PR-A.

## Pre-fix severity (security lens)

Pre-fix, `RollbackToSavepoint(ctxA, txBID, spID)` from tenant A was a
**destructive integrity-loss path on tenant B's tx-state**, replacing
tenant B's `Buffer`/`ReadSet`/`WriteSet`/`Deletes` with a snapshot
tenant A controlled. `Savepoint(ctxA, txBID)` produced a savepoint ID
keyed in tenant B's namespace (information disclosure on tenant B's
tx-namespace structure). `ReleaseSavepoint(ctxA, txBID, spID)` cleared
tenant B's snapshot record (availability impact). Mitigated in practice
by 128-bit txID entropy, but a tenant A caller who learned a tenant B
txID via logs/support channels could exploit this without auth bypass.

Same severity profile as the memory plugin gap PR-A closed. PR-C1
mirrors the fix.

## Regression tests

- `plugins/sqlite/concurrency_savepoint_test.go` — 4 race tests (3 real
  reproducers, 1 contract-pin sentinel + a documentation-comment for the
  Join asymmetry).
- `plugins/sqlite/savepoint_tenant_test.go` — 3 tenant-isolation tests
  (`TestSqliteSavepoint_RejectsCrossTenant`,
  `TestSqliteRollbackToSavepoint_RejectsCrossTenant`,
  `TestSqliteReleaseSavepoint_RejectsCrossTenant`).

All pass green-phase under `go test -race ./plugins/sqlite/...`.
