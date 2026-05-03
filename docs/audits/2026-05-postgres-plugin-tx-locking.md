# Postgres Plugin — `*spi.TransactionState` Locking + Tenant Isolation Audit

Issue #199 PR-C2. Parallels the memory and sqlite audits but covers a
plugin with a fundamentally different concurrency model. Documents what
the SPI v0.6.1 OpMu contract requires, why postgres trivially satisfies
it without any OpMu acquisition, and the tenant-isolation gap PR-C2
closes.

## Why this audit was needed

cyoda-go-spi v0.6.1 (PR-B, merged) formalised the tx-state OpMu contract
for plugin authors. PR-A closed the memory plugin's gaps; PR-C1 closed
the sqlite plugin's. The natural completion of the audit pass was
postgres — but two findings during the deep-dive made PR-C2 substantially
larger than the originally-planned "audit only, no code change":

1. **Postgres uses a different concurrency model entirely.** The OpMu
   contract is satisfied trivially because the protected fields are
   unused.
2. **Postgres lacked application-layer tenant isolation on its TM
   lifecycle methods.** Memory and sqlite have always had it on
   Commit/Rollback (PR-A/PR-C1 extended it to Savepoint/etc.); postgres
   relied solely on RLS, which doesn't extend to lifecycle commands.

PR-C2 closes both — formally documents (1), fixes (2).

## How postgres' concurrency model differs from memory/sqlite

Memory and sqlite are **in-process SI+FCW** plugins. They maintain:
- An in-memory `committedLog` window for conflict detection.
- A per-tx `Buffer` map staging writes until commit.
- A per-tx `OpMu` (`sync.RWMutex` on `*spi.TransactionState`) separating
  in-flight tx-path ops from closure ops.

Postgres delegates SI+FCW to PostgreSQL's MVCC engine plus an
application-layer read-set validation:
- `Begin` opens a real `pgx.Tx` via `pool.BeginTx(ctx, IsoLevel: RepeatableRead)`.
  PostgreSQL handles the snapshot.
- Writes go directly to the database via SQL inside the pgx.Tx — there
  is no application-layer Buffer.
- Reads execute SQL inside the pgx.Tx — RLS-bound to the tx's tenant
  via `set_config('app.current_tenant', $1, true)`.
- Read-set validation at commit time (`txstate.ValidateReadSet`) is the
  only application-layer FCW check; write-write conflicts are caught by
  PostgreSQL's tuple-level locks (`SQLSTATE 40001`).

**Consequence for the SPI OpMu contract:** the postgres TM never reads
or writes `tx.Buffer`/`tx.ReadSet`/`tx.WriteSet`/`tx.Deletes`/
`tx.RolledBack`/`tx.Closed` on `*spi.TransactionState`. The
`spi.TransactionState` value postgres exposes via Begin/Join carries
ONLY `ID` and `TenantID` — the rest of the struct's fields are
unread/unwritten. The OpMu contract therefore holds vacuously: there
are no fields whose access requires OpMu.

Per-tx bookkeeping for FCW lives in a separate internal `txState` struct
(`plugins/postgres/txstate.go`) with its own `sync.Mutex`. That mutex is
fully encapsulated inside the postgres plugin and is invisible at the
SPI surface.

## OpMu posture per method (postgres)

| Method | Touches `*spi.TransactionState` mutable fields? | OpMu posture |
|---|---|---|
| `Begin` | No (only writes immutable `ID`/`TenantID`). | N/A |
| `Commit` | No. Reads internal `txState` (separate struct, own mutex). | N/A |
| `Rollback` | No. | N/A |
| `Join` | No (constructs new `*spi.TransactionState` with `ID`/`TenantID` only). | N/A |
| `GetSubmitTime` | No. | N/A |
| `Savepoint` | No. Operates on internal `txState.savepoints`. | N/A |
| `RollbackToSavepoint` | No. | N/A |
| `ReleaseSavepoint` | No. | N/A |
| All `EntityStore` ops (`Save`, `CompareAndSave`, `Get`, `GetAll`, `GetAsAt`, `Delete`, `DeleteAll`, `Exists`, `Count`, `CountByState`) | No. Use `tm.recordReadIfInTx` / `tm.recordWriteIfInTx` which target internal `txState`. | N/A |

The OpMu contract is **trivially satisfied** for the postgres plugin
because no plugin code path accesses any of the OpMu-protected fields.
A future plugin contributor must NOT add OpMu acquisitions speculatively
to the postgres plugin — the contract is satisfied by the structural
property that those fields are unused, and any added OpMu would be
spurious.

## Internal locks postgres uses (for completeness)

These are plugin-internal locks; they are NOT the SPI's OpMu contract.

| Mutex | File:line | Protects | Notes |
|---|---|---|---|
| `TransactionManager.mu` | `transaction_manager.go:30` | `submitTimes`, `tenants` maps | Brief holds, multiple critical sections |
| `TransactionManager.txStatesMu` | `transaction_manager.go:38` (RWMutex) | `txStates` map keying internal txState by txID | RLock for lookup, Lock for insert/delete |
| `txState.mu` | `txstate.go:30` | `readSet`, `writeSet`, `savepoints` slice — per-tx FCW bookkeeping | One per active tx |
| `txRegistry.mu` (internal) | `txregistry.go` | `txID → pgx.Tx` map | Single source of truth for the pgx.Tx handle |
| `pgxpool.Pool` (vendored) | n/a | connection pool | Managed by pgx |

Lock order across the postgres plugin is bounded by acquire-and-release
discipline — every method acquires a lock briefly, does its bookkeeping,
releases. Manager-side mutexes are never held across slow operations
(SQL, network). No nested or composite lock acquisitions exist that
could create cycles.

## Tenant isolation finding (PR-C2)

### Pre-fix gap

Pre-PR-C2, the postgres TM lifecycle methods (`Commit`, `Rollback`,
`Join`, `Savepoint`, `RollbackToSavepoint`, `ReleaseSavepoint`) did not
verify the caller's `UserContext` tenant against the transaction's
tenant. Memory and sqlite plugins always had this check on
Commit/Rollback (and PR-A/PR-C1 extended it to the savepoint trio);
postgres lacked it across the entire surface.

### Why RLS does NOT cover this

PostgreSQL's row-level security is a **row-level** check. It enforces
policies on `SELECT`/`INSERT`/`UPDATE`/`DELETE` against tables. RLS does
NOT extend to:

- Transaction lifecycle commands (`BEGIN`, `COMMIT`, `ROLLBACK`,
  `SAVEPOINT`, `ROLLBACK TO SAVEPOINT`, `RELEASE SAVEPOINT`). These are
  connection/session-level operations. They access no row. They trigger
  no policy.
- Session variables and `set_config`. `app.current_tenant` is set once
  at Begin time on the pgx.Tx connection. It does not change based on
  who's calling `pgxTx.Commit()` later.
- The Go-level `pgx.Tx` handle. Postgres has no concept of "who is the
  current Go-level caller" — it sees only the connection that owns the
  tx.

So a caller authenticated as tenant A who learned a tenant B txID could
trigger:
- `Commit(ctxA, txBID)` — commits tenant B's tx prematurely.
- `Rollback(ctxA, txBID)` — aborts tenant B's in-flight work.
- `Savepoint(ctxA, txBID)` — creates a SAVEPOINT inside tenant B's tx.
- `RollbackToSavepoint(ctxA, txBID, spID)` — destructively undoes
  tenant B's DML state to the savepoint.
- `ReleaseSavepoint(ctxA, txBID, spID)` — drops tenant B's savepoint.
- `Join(ctxA, txBID)` — receives a context driving tenant B's tx.

RLS continues to enforce data-path isolation correctly throughout —
DML inside the pgx.Tx is RLS-scoped to tenant B (the tenant set at
Begin time). But the lifecycle disruption is real.

### Why this isn't covered by "RLS is intrinsic"

The "RLS is intrinsic by design, application-layer is defense-in-depth"
framing in `docs/plugins/POSTGRES.md` is correct **for data-path
operations.** It is structurally not extendable to lifecycle: PostgreSQL
has no row-level identity to compare against for lifecycle commands.
Application-layer tenant gating is the only enforcement mechanism for
lifecycle, across all three plugins (memory, sqlite, postgres).

Pre-PR-C2, postgres was uniquely vulnerable on lifecycle despite having
the strongest data-path protection.

### Fix (PR-C2)

A new `verifyTenant` helper in `transaction_manager.go` compares
`uc.Tenant.ID` against the transaction's tenant (from
`txState.tenantID` or `tm.tenants[txID]`). Every TM lifecycle method
now calls it before performing any state mutation.

```go
func verifyTenant(ctx context.Context, txTenantID spi.TenantID, op string, txID string) error {
    uc := spi.GetUserContext(ctx)
    if uc == nil || uc.Tenant.ID != txTenantID {
        return fmt.Errorf("%s: tenant mismatch on transaction %s", op, txID)
    }
    return nil
}
```

This brings postgres to parity with memory and sqlite on lifecycle
gating. RLS continues to provide defense-in-depth on the data path —
it's not redundant with the lifecycle check, it's complementary.

## Summary

| Status | Methods |
|---|---|
| ✅ OpMu contract trivially satisfied (no OpMu-protected fields touched) | All TM methods, all EntityStore methods |
| ✅ Tenant isolation present (already, before PR-C2) | `Begin` (resolves tenant from ctx) |
| ✅ Tenant isolation added by PR-C2 | `Commit`, `Rollback`, `Join`, `Savepoint`, `RollbackToSavepoint`, `ReleaseSavepoint` |

After PR-C2, the postgres plugin has zero outstanding tenant-isolation
gaps on its TM lifecycle surface — same end-state as memory (post-PR-A)
and sqlite (post-PR-C1). The OpMu contract is trivially satisfied due
to postgres' fundamentally different concurrency model.

## Pre-fix severity (security lens)

`RollbackToSavepoint(ctxA, txBID, spID)` from tenant A was a
**destructive integrity-loss path on tenant B's tx-state** at the
PostgreSQL transaction level. RLS protected the data layer but not the
lifecycle layer. Same blast radius as the parallel gaps PR-A and PR-C1
closed in memory and sqlite. Mitigated in practice by 128-bit txID
entropy, but a tenant A caller who learned a tenant B txID via
logs/support channels could have exploited this without any auth
bypass.

## Regression tests

`plugins/postgres/transaction_manager_tenant_test.go` — 6 tests, one per
lifecycle method:
- `TestPostgresCommit_RejectsCrossTenant`
- `TestPostgresRollback_RejectsCrossTenant`
- `TestPostgresJoin_RejectsCrossTenant`
- `TestPostgresSavepoint_RejectsCrossTenant`
- `TestPostgresRollbackToSavepoint_RejectsCrossTenant`
- `TestPostgresReleaseSavepoint_RejectsCrossTenant`

All FAIL pre-fix (verified by red-phase TDD), PASS post-fix. Require
`CYODA_TEST_DB_URL` to point at a running PostgreSQL.

## Pre-existing test cleanup

PR-C2 also fixed one pre-existing test (`fcw_test.go:544`) that passed
`context.Background()` to `tm.Commit`. With the new tenant gate this
returns "tenant mismatch" (because `context.Background()` has no
UserContext); the test now passes the test's own `ctx` (which has
UserContext set up at the top), making it consistent with the contract
that memory and sqlite have always enforced on Commit/Rollback.
