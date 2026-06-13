# SPI Sentinel Errors for Transaction State — Design

**Issue:** #200
**Milestone:** v0.8.0
**Status:** Spec
**Author:** Paul Schleger / Claude

## Problem

The memory plugin (and, by mirroring, the sqlite plugin) returns transaction-lifecycle errors as plain strings:

- `fmt.Errorf("transaction has been rolled back")` — when a data op runs against a tx that was concurrently rolled back. **8 sites in memory `entity_store.go`, 8 sites in sqlite `entity_store.go`.**
- `fmt.Errorf("transaction already being committed")` — when `Commit` fires on a tx mid-`Commit`.
- `fmt.Errorf("transaction already closed: %s", txID)` — `Join` after `Commit`.
- `fmt.Errorf("transaction not found or already completed: %w", spi.ErrNotFound)` — half-typed; only Commit/Rollback paths in memory + sqlite already wrap a sentinel.
- `fmt.Errorf("tenant mismatch on transaction join")` and four variants. **6 sites in memory, 6 in sqlite — not a niche case.**

This forces callers — including the memory plugin's own race-conditional tests in `plugins/memory/concurrency_*_test.go` — to do `strings.Contains` substring matching alongside `errors.Is(spi.ErrNotFound)`. The substring fallback is brittle, typo-sensitive, and can silently swallow unrelated errors that happen to contain the same substring.

`plugins/memory/concurrency_savepoint_test.go:49-51` already carries a `TODO(#200)` flagging this. This spec discharges that TODO.

## Why this is SPI-level

The error semantics are part of the contract every backend must honour:

- Memory plugin (this repo, `plugins/memory/`)
- SQLite plugin (this repo, `plugins/sqlite/`) — mirrors memory's error strings almost verbatim
- Postgres plugin (this repo, `plugins/postgres/`) — server-side tx; subset of conditions apply
- Cassandra plugin (separate private repo, `../cyoda-go-cassandra`) — uses its own error-code system today; will conform on its next SPI bump

A consistent sentinel set in `cyoda-go-spi` lets:

- Callers do `errors.Is(err, spi.ErrTxRolledBack)` instead of fragile substring matches.
- Cross-plugin parity be enforceable via the existing `spitest/` harness pattern.
- Future API-layer error mapping (HTTP/gRPC) become structural (deferred to a follow-up; out of scope here).

## Sentinel Set

In `cyoda-go-spi/errors.go`, add seven new exported sentinels organized in a small two-level hierarchy. Existing sentinels (`ErrNotFound`, `ErrConflict`, `ErrEpochMismatch`, `ErrRetryExhausted`) are untouched.

```go
// sentinelErr lets a sentinel carry an Unwrap parent so errors.Is matches
// both the specific sentinel and its umbrella/parent. Unexported.
type sentinelErr struct {
    msg    string
    parent error
}

func (e *sentinelErr) Error() string { return e.msg }
func (e *sentinelErr) Unwrap() error { return e.parent }

var (
    // ErrTxNotFound indicates that a transaction handle does not refer to a
    // known transaction — either the txID never existed, or its state has
    // been fully purged. Wraps ErrNotFound so existing
    //   errors.Is(err, spi.ErrNotFound)
    // checks on tx-lifecycle paths continue to match.
    ErrTxNotFound = &sentinelErr{msg: "transaction not found", parent: ErrNotFound}

    // ErrSavepointNotFound indicates that a savepoint identifier does not
    // refer to a known savepoint on the given transaction. Wraps ErrNotFound.
    ErrSavepointNotFound = &sentinelErr{msg: "savepoint not found", parent: ErrNotFound}

    // ErrTxTerminated is the umbrella sentinel for any operation on a
    // transaction that has reached a terminal state. Callers that do not
    // need to distinguish rollback from commit can match this directly.
    //
    // NOTE: Backends that own transaction state in a remote engine
    // (postgres) may surface mid-op rollback as ErrConflict (e.g. via
    // SQLSTATE 25P02) instead of ErrTxRolledBack, where the engine's
    // abort code is already semantically meaningful. The ErrTxTerminated
    // sentinel is required only where the plugin owns its own in-process
    // tx-state buffer (memory, sqlite, cassandra). Consumers writing
    // backend-agnostic code should match both ErrTxTerminated and
    // ErrConflict on data-op paths.
    ErrTxTerminated = errors.New("transaction in terminal state")

    // ErrTxRolledBack indicates that an in-flight operation observed the
    // transaction marked rolled-back (memory/sqlite race) or attempted an op
    // on a transaction whose terminal state is Rollback. Wraps ErrTxTerminated.
    // See ErrTxTerminated godoc for postgres-engine caveat.
    ErrTxRolledBack = &sentinelErr{msg: "transaction rolled back", parent: ErrTxTerminated}

    // ErrTxAlreadyCommitted indicates an attempt to Join, Commit, or
    // otherwise operate on a transaction whose terminal state is Commit.
    // Wraps ErrTxTerminated.
    ErrTxAlreadyCommitted = &sentinelErr{msg: "transaction already committed", parent: ErrTxTerminated}

    // ErrTxCommitInProgress indicates that Commit was called on a
    // transaction another goroutine is already committing. Distinct from
    // ErrTxTerminated because the transaction is not yet terminal — the loser
    // of the race may still observe the committed result.
    ErrTxCommitInProgress = errors.New("transaction commit in progress")

    // ErrTxTenantMismatch indicates a transaction-lifecycle operation
    // (Join, Commit, Rollback, Savepoint, etc.) was attempted with a
    // UserContext whose tenant does not match the transaction's tenant.
    // Tenant isolation invariant — distinct from data-op tenant checks.
    ErrTxTenantMismatch = errors.New("transaction tenant mismatch")
)
```

### Hierarchy contract

| Sentinel | `errors.Is` parents (transitive) |
|---|---|
| `ErrTxNotFound` | `ErrNotFound` |
| `ErrSavepointNotFound` | `ErrNotFound` |
| `ErrTxRolledBack` | `ErrTxTerminated` |
| `ErrTxAlreadyCommitted` | `ErrTxTerminated` |
| `ErrTxTerminated` | — |
| `ErrTxCommitInProgress` | — |
| `ErrTxTenantMismatch` | — |

### What is **not** a sentinel

These remain plain `fmt.Errorf` strings and are documented in SPI godoc as **not part of the sentinel contract**:

- `"no user context — cannot begin transaction"` — SDK misuse / programming error.
- `"user context has no tenant — cannot begin transaction"` — SDK misuse.
- `"memory: TransactionManager not initialized (call NewTransactionManager first)"` — factory misuse.
- `"transaction not yet committed"` (memory `GetSubmitTime` on still-active tx) — operational signal callers don't currently match on; one site; leaving plain to avoid pollution.

Rationale: these signal misuse of the API rather than an operational tx-state condition. Promoting them to sentinels would broaden the contract surface for no consumer benefit.

## Plugin Migration

### Memory plugin

`plugins/memory/txmanager.go`:

| Line(s) | Current | Wrap |
|---|---|---|
| 122 | `"transaction not found: %s"` | `ErrTxNotFound` |
| 131 | `"transaction already closed: %s"` | `ErrTxAlreadyCommitted` |
| 140 | `"tenant mismatch on transaction join"` | `ErrTxTenantMismatch` |
| 156 | `"transaction not found or already completed: %w"` (wraps `ErrNotFound`) | `ErrTxNotFound` (which wraps `ErrNotFound`) |
| 160 | `"transaction tenant mismatch"` | `ErrTxTenantMismatch` |
| 164 | `"transaction already being committed"` | `ErrTxCommitInProgress` |
| 308 | `"transaction not found or already completed: %w"` (Rollback) | `ErrTxNotFound` |
| 312 | `"transaction tenant mismatch"` (Rollback) | `ErrTxTenantMismatch` |
| 346 | `"transaction not found: %s"` (`GetSubmitTime` not-found path — line 339 "not yet committed" stays plain) | `ErrTxNotFound` |
| 379, 382, 392 | Savepoint not found / tenant mismatch / already closed | `ErrSavepointNotFound` / `ErrTxTenantMismatch` / `ErrTxAlreadyCommitted` |
| 449, 452, 459, 467, 471 | RollbackToSavepoint variants | corresponding sentinels |
| 500, 503, 508, 511 | ReleaseSavepoint variants | corresponding sentinels |

`plugins/memory/entity_store.go` — 8 sites (Save 106, CompareAndSave 131, Get 253, GetAll 296, Delete 348, Exists 450, Count 513, DeleteAll 588):

- `"transaction has been rolled back"` → `fmt.Errorf("tx %s: %w", txID, spi.ErrTxRolledBack)` (adds tx ID for debuggability)

### SQLite plugin

`plugins/sqlite/txmanager.go` + `plugins/sqlite/entity_store.go` — same line-by-line mapping as memory; the error strings are identical. Includes the `GetSubmitTime` "not found" site at `plugins/sqlite/txmanager.go:474` → `ErrTxNotFound` for parity with memory line 346. See the survey table in the brainstorm transcript for the full file:line list.

### Postgres plugin

`plugins/postgres/transaction_manager.go`:

| Line(s) | Current | Wrap |
|---|---|---|
| 118 | `"Commit: transaction %s not found"` | `ErrTxNotFound` |
| 122 | `"Commit: tx state for %s not found"` | `ErrTxNotFound` |
| 215, 220 | `"Rollback: transaction %s not found"` | `ErrTxNotFound` |
| 244, 249 | `"Join: transaction %s not found"` (both lookup paths) | `ErrTxNotFound` |
| 318 | `"Savepoint: transaction %s not found"` | `ErrTxNotFound` |
| 344 | `"RollbackToSavepoint: transaction %s not found"` | `ErrTxNotFound` |
| 370 | `"ReleaseSavepoint: transaction %s not found"` | `ErrTxNotFound` |
| **414** | **`verifyTenant()` helper — `"%s: tenant mismatch on transaction %s"`** | **`ErrTxTenantMismatch` — single wrap site, called from Commit (L124), Rollback (L222), Join (L251), Savepoint (L324), RollbackToSavepoint (L350), ReleaseSavepoint (L376)** |

L270 (`GetSubmitTime: ... has no submit time (not yet committed or unknown)`) deliberately conflates "not yet committed" with "unknown txID" into one operational message — stays plain per the SDK-misuse / one-site policy applied to memory `txmanager.go:339`.

`plugins/postgres/txstate.go`:

| Line(s) | Current | Wrap |
|---|---|---|
| 166, 223 | `"unknown savepoint %q"` | `ErrSavepointNotFound` |

Postgres does **not** have:
- In-process `tx.RolledBack` race (server-side tx; mid-op rollback surfaces as `pgconn.PgError` SQLSTATE 25P02, already mapped to `ErrConflict` via `classifyError` at `transaction_manager.go:171-174` — not changed; this is the postgres-engine caveat documented in the SPI godoc on `ErrTxTerminated`)
- `transaction already being committed` (no in-process commit registry)
- `transaction already closed` distinct from `transaction not found` (server-side tx; once committed/rolled back, lookups return "not found")

`verifyTenant()` at `transaction_manager.go:411-417` is the single point that all tx-lifecycle ops route through for tenant-mismatch checking. Wrapping `ErrTxTenantMismatch` there is the only site needed for postgres tenant-mismatch contract conformance.

Postgres `GetSubmitTime` "not yet committed" path (if present and distinct from "not found") follows the memory/sqlite policy: stays plain (one-site operational signal, no consumer match).

### Cassandra plugin (not modified in this PR)

Cassandra picks up the new sentinels when it next bumps its `cyoda-go-spi` pin. Until then its own string-coded error system (`TX_NOT_FOUND`, `TX_NOT_ACTIVE`, `ErrCodeTxNoState`, etc. at `cyoda-go-cassandra/internal/tx/tx_manager.go`) continues unchanged. On the bump, expect approximately six conformance subtest failures: `JoinAfterCommit`, `CommitAfterCommit`, `CommitAfterRollback`, `TenantMismatchOnJoin`, `TenantMismatchOnCommit`, and `SavepointNotFound` — a predictable, scoped conformance prompt. **No courtesy PR; no tracking issue in the Cassandra repo** (per project convention — Cassandra repo is private and out of scope here).

## Parity Test Additions

The contract under test is the SPI's own sentinel surface, so the conformance tests live in `cyoda-go-spi/spitest/` rather than `cyoda-go/e2e/parity/registry.go`. Issue #200's body suggests the latter, but `e2e/parity` is HTTP-only and the issue defers HTTP mapping anyway — the natural assertion surface is the Go-level SPI boundary. `spitest/` is already where every backend (memory, sqlite, postgres, cassandra) imports a shared conformance suite.

### Pure sentinel-graph tests — `cyoda-go-spi/errors_test.go`

Hierarchy and distinctness are properties of the SPI itself, not per-backend behaviour. They go alongside the existing `TestSentinelsAreDistinct` (`errors_test.go:9-18`), running once per CI rather than N times for N backends:

- **Positive pairs** — every `(child, parent)` transitive pair in the hierarchy table:
  - `errors.Is(ErrTxNotFound, ErrNotFound) == true`
  - `errors.Is(ErrSavepointNotFound, ErrNotFound) == true`
  - `errors.Is(ErrTxRolledBack, ErrTxTerminated) == true`
  - `errors.Is(ErrTxAlreadyCommitted, ErrTxTerminated) == true`
- **Negative pairs** — siblings and cross-tree:
  - `errors.Is(ErrTxNotFound, ErrTxRolledBack) == false`
  - `errors.Is(ErrTxRolledBack, ErrNotFound) == false`
  - `errors.Is(ErrTxTenantMismatch, ErrTxTerminated) == false`
  - … etc., covering every off-diagonal pair.
- **Wrap chain** — `fmt.Errorf("ctx: %w", ErrTxNotFound)` is `errors.Is`-detectable as both `ErrTxNotFound` AND `ErrNotFound` (verifying the `sentinelErr.Unwrap` chain composes correctly with `fmt.Errorf`'s wrap).

### Per-backend conformance — `cyoda-go-spi/spitest/transaction.go`

Extend `runTransactionSuite` with subtests that exercise each backend's TransactionManager + EntityStore behaviour:

```go
func runTransactionSuite(t *testing.T, h Harness, tracker *skipTracker) {
    // ... existing subtests ...
    runSubtest(t, h, tracker, "TxStateErrors/JoinAfterCommit", testTxStateJoinAfterCommit)
    runSubtest(t, h, tracker, "TxStateErrors/CommitAfterCommit", testTxStateCommitAfterCommit)
    runSubtest(t, h, tracker, "TxStateErrors/CommitAfterRollback", testTxStateCommitAfterRollback)
    runSubtest(t, h, tracker, "TxStateErrors/OpAfterRollback", testTxStateOpAfterRollback)
    runSubtest(t, h, tracker, "TxStateErrors/TenantMismatchOnJoin", testTxStateTenantMismatchOnJoin)
    runSubtest(t, h, tracker, "TxStateErrors/TenantMismatchOnCommit", testTxStateTenantMismatchOnCommit)
    runSubtest(t, h, tracker, "TxStateErrors/SavepointNotFound", testTxStateSavepointNotFound)
}
```

### Subtest contract sketches

- **JoinAfterCommit**: Begin → Commit → second goroutine `Join(txID)` → assert `errors.Is(err, ErrTxAlreadyCommitted)` AND `errors.Is(err, ErrTxTerminated)`.
- **CommitAfterCommit**: Begin → Commit → Commit again → assert `errors.Is(err, ErrTxNotFound)` OR `ErrTxAlreadyCommitted` (Postgres collapses these; either is acceptable contract).
- **CommitAfterRollback**: Begin → Rollback → Commit → assert `errors.Is(err, ErrTxNotFound)` OR `ErrTxRolledBack` (Postgres collapses).
- **OpAfterRollback**: Begin → Save → Rollback (foreground) → Get on rolled-back txCtx → assert `errors.Is(err, ErrTxTerminated)`. Subtest skipped on postgres via `Harness.Skip` because pgx.Tx does not expose mid-op state — the error surfaces as `ErrConflict`. This skip is documented in SPI godoc on `ErrTxTerminated` so consumers don't assume universal coverage.
- **TenantMismatchOnJoin**: Tenant A begins tx → Tenant B attempts Join → assert `errors.Is(err, ErrTxTenantMismatch)`.
- **TenantMismatchOnCommit**: Tenant A begins tx → Tenant B attempts Commit → assert `errors.Is(err, ErrTxTenantMismatch)`. Postgres' `verifyTenant()` helper carries this contract.
- **SavepointNotFound**: Begin → RollbackToSavepoint with unknown id → assert `errors.Is(err, ErrSavepointNotFound)` AND `errors.Is(err, ErrNotFound)`. Passes on all three plugins including postgres (savepoint paths verified at `plugins/postgres/transaction_manager.go:315,341,367` and `plugins/postgres/txstate.go:166,223`).

### Postgres skips

Configured in `plugins/postgres/conformance_test.go` (the existing wiring point):

```go
h := spitest.Harness{
    Factory: ...,
    NewTenant: ...,
    Skip: map[string]string{
        "Transaction/TxStateErrors/OpAfterRollback": "postgres: pgx.Tx aborts surface as ErrConflict via SQLSTATE 25P02, not as ErrTxRolledBack",
    },
}
```

Only `OpAfterRollback` is skipped on postgres. `SavepointNotFound` is NOT skipped — postgres has full savepoint support. The `Skip` map's typo-detection (unused keys flagged at end of run via `skipTracker.unusedKeys` at `spitest/spitest.go:75-85`) ensures the skip stays honest.

## Concurrency Test Update (memory plugin)

`plugins/memory/concurrency_inreadops_test.go`, `concurrency_cas_test.go`, `concurrency_savepoint_test.go`:

- Replace each `runOpVsRollback` / `runOpVsCommit` helper's `strings.Contains(msg, "rolled back") || strings.Contains(msg, "already completed") || ...` block with a single `errors.Is(err, spi.ErrTxTerminated) || errors.Is(err, spi.ErrTxNotFound)`.
- Drop the `TODO(#200)` comment block at `concurrency_savepoint_test.go:49-51`.

## Migration Sequencing

1. **`cyoda-go-spi` PR** — add sentinels + spitest subtests + godoc. Tag a fresh minor (e.g., `v0.X.Y+1`) — never force-move (per project convention).
2. **`cyoda-go` PR (this issue)** — bump `cyoda-go-spi` pin in:
   - root `go.mod`
   - `plugins/memory/go.mod`
   - `plugins/sqlite/go.mod`
   - `plugins/postgres/go.mod`

   Migrate memory/sqlite/postgres error sites; update memory concurrency tests; configure `Harness.Skip` for postgres-irrelevant subtests; update `COMPATIBILITY.md` matrix (new SPI tag row, listing the seven added sentinels in the "SPI surface" column); run `make test-all` AND each `plugins/{memory,sqlite,postgres}` independently `go test ./...` (root `./...` does not cross plugin submodule boundaries — see `CLAUDE.md` "Plugin submodules need explicit test runs"); run `go vet ./...` per the per-module-hygiene CI job; one-shot `go test -race ./...` before PR.

3. **`cyoda-go-cassandra` (out of scope, separate repo)** — conforms on next SPI bump. Pinning protects existing CI.

## Out of Scope

- HTTP/gRPC status mapping based on the new sentinels (separate follow-up; the issue body explicitly defers this).
- Internationalisation of error messages.
- Restructuring error wrapping conventions broadly.
- Cassandra plugin migration (separate repo, separate cadence).
- Promoting SDK-misuse errors (`no user context`, `no tenant`, `TransactionManager not initialized`) to sentinels.

## Acceptance

- [ ] Sentinel constants defined in `cyoda-go-spi/errors.go` with godoc, including the postgres-engine caveat on `ErrTxTerminated`/`ErrTxRolledBack`.
- [ ] `cyoda-go-spi/errors_test.go` covers every transitive `(child, parent)` pair (positive matches) and every off-diagonal pair (negative — distinct sentinels), plus the `fmt.Errorf` wrap-chain composition.
- [ ] Memory plugin wraps the sentinels at every site listed in the migration table (including the `GetSubmitTime` not-found at L346).
- [ ] SQLite plugin wraps the sentinels at every mirrored site (including the `GetSubmitTime` not-found at L474).
- [ ] Postgres plugin wraps the sentinels at every applicable site, including `verifyTenant()` (L414) which carries the entire postgres tenant-mismatch contract via one wrap, savepoint paths in `transaction_manager.go` (L315/341/367) and `txstate.go` (L166, 223). `Harness.Skip` documents the single non-applicable subtest (`OpAfterRollback`) with reason.
- [ ] `plugins/memory/concurrency_*_test.go` uses `errors.Is` against `ErrTxTerminated` / `ErrTxNotFound`; no `strings.Contains` on tx-state error messages; `TODO(#200)` comment removed.
- [ ] New `spitest/transaction.go` subtests pass on memory, sqlite, AND postgres (with only `OpAfterRollback` skipped on postgres).
- [ ] `COMPATIBILITY.md` matrix updated with the new `cyoda-go-spi` tag row.
- [ ] `make test-all` green AND each `plugins/{memory,sqlite,postgres}` module independently `go test ./...` green.
- [ ] `go test -race ./...` clean per project convention (one-shot before PR; not per-step).
- [ ] `go vet ./...` clean across root + each plugin submodule.
- [ ] SPI godoc documents which conditions each sentinel signals, including which conditions are intentionally left plain (SDK-misuse).

## References

- Issue #200
- `plugins/memory/concurrency_savepoint_test.go:49-51` — original `TODO(#200)` marker
- `plugins/memory/txmanager.go`, `plugins/memory/entity_store.go` — primary migration targets
- `plugins/sqlite/txmanager.go`, `plugins/sqlite/entity_store.go` — mirror migration
- `plugins/postgres/transaction_manager.go`, `plugins/postgres/txstate.go` — migration including full savepoint paths and `verifyTenant()` single-wrap
- `plugins/postgres/conformance_test.go` — wiring point for `Harness.Skip`
- `cyoda-go-spi/spitest/transaction.go` — extension point for parity subtests
- `cyoda-go-spi/errors.go` — sentinel definitions
- `internal/domain/workflow/engine_processors.go` — codebase precedent for `errors.Join` / multi-error chains (not used directly here but cited for design fit)
