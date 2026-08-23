# `memory` storage plugin

## Capabilities

Ephemeral, in-process state — no disk I/O, no network round-trips, no
query planner on the hot path. The memory plugin's latency profile
sits an order of magnitude ahead of any persistent backend: a full
SI+FCW transaction (begin → read-modify-write → commit) completes in
the low microseconds rather than the milliseconds a Postgres
round-trip takes.

That performance envelope makes the memory plugin particularly
effective as the **state-backing for high-throughput digital-twin
workloads** — an agentic software factory where an agent swarm drives
thousands of scenario executions per second against a behavioural
twin of a production entity, or a simulation that replays weeks of
production state-machine behaviour in seconds. Same workflow
semantics, same FSM engine, same SI+FCW guarantees as the persistent
backends — without the durability trade-off.

## Concurrency model

The memory plugin implements Snapshot Isolation with
First-Committer-Wins (SI+FCW) entirely in-process. The concurrency
controller is the plugin itself — there is no underlying database
doing conflict detection.

**Locking primitives:**

- **Service-level `sync.RWMutex`** on `StoreFactory` — serializes
  writes to the shared entity data maps during transaction commit.
- **Per-transaction `OpMu`** (`sync.RWMutex` on `TransactionState`) —
  gates concurrent operations within a single transaction and ensures
  in-flight operations complete before commit/rollback.

**Per-transaction state:**

Each transaction captures a `SnapshotTime` at `Begin()`. Reads see
only data committed before the snapshot. Writes are buffered in a
per-transaction `Buffer` map. At commit time, the committed log is
scanned for conflicts.

```go
type TransactionState struct {
    ID           string
    TenantID     TenantID
    SnapshotTime time.Time
    ReadSet      map[string]bool      // entity IDs read
    WriteSet     map[string]bool      // entity IDs written
    Buffer       map[string]*Entity   // buffered writes
    Deletes      map[string]bool      // buffered deletes
    OpMu         sync.RWMutex         // per-tx operation gate
    Closed       bool
    RolledBack   bool
}
```

**Commit sequence (critical section):**

```
1. Acquire tx.OpMu.Lock()         -- wait for in-flight ops to finish
2. Acquire factory.mu.Lock()      -- exclusive access to shared data
3. Acquire tm.mu.Lock()           -- scan committed log
4. FOR EACH committed_tx where submitTime > tx.SnapshotTime:
     IF committed_tx.writeSet intersects (tx.ReadSet UNION tx.WriteSet):
       ABORT -> ErrConflict
5. Flush tx.Buffer to factory.entityData (deep copy)
6. Apply tx.Deletes (append tombstone versions)
7. Append to committedLog: {txID, submitTime, writeSet}
8. Record submitTime in submitTimes map
9. Remove from active map
10. Prune committedLog (entries older than oldest active snapshot)
11. Release all locks
```

This is first-committer-wins: the transaction that reaches step 4
first wins; any concurrent transaction whose read-set or write-set
intersects the winner's write-set is aborted with `common.ErrConflict`
when it attempts to commit.

**TOCTOU guard:** A `committing` map prevents double-commit races.
The lock acquisition order — `tx.OpMu` → `factory.mu` → `tm.mu` — is
uniform across the plugin; all commits take locks in the same order,
so there is no cycle.

**Committed-log pruning:** After each commit, entries older than the
oldest active transaction's snapshot time are removed. When no
transactions are active, the entire log is cleared. The log therefore
grows only as long as there are concurrent transactions in flight.

**`submitTimes` retention:** Submit times survive log pruning so that
late `GetSubmitTime(txID)` lookups still resolve. Entries are evicted
on a 1-hour TTL (`submitTimeTTL` in `txmanager.go`) — the map is
bounded, not growing without limit.

## Transaction manager

The memory plugin implements `spi.TransactionManager` directly —
there is no underlying engine to delegate to, so the plugin IS the
concurrency controller for its SI+FCW contract. The manager owns
the per-transaction read/write sets and the committed-log window
used for conflict detection. Reference: `plugins/memory/txmanager.go`.

## Data model and schema

No persistence. All state lives in Go data structures inside the
process. Restart loses everything. Data structures mirror the
entity / model / workflow / KV / message / audit / search
boundaries defined by the SPI — see `plugins/memory/<type>_store.go`
for each (`entity_store.go`, `model_store.go`, `kv_store.go`,
`message_store.go`, `workflow_store.go`, `sm_audit_store.go`,
`search_store.go`).

## Canonical entity-ID order

`GetPage` (paged entity listing), the entity-ID tie-break under a
user-field `OrderBy`, and an explicit entity-ID `OrderBy` all order by the
memory plugin's canonical entity-ID order: **byte-wise ascending**, Go's
native `<` string comparison. This order is stable and deterministic but is
**not** guaranteed identical to another storage engine's canonical order —
each in-house backend documents byte-wise ascending as its native
behaviour, but a client that depends on cross-backend identical list order
is relying on an accident, not a contract. See `docs/cloud-parity/` for the
public-API-facing statement of this rule.

## Configuration (env vars)

The memory plugin has no plugin-specific environment variables. It is
the default backend when `CYODA_STORAGE_BACKEND` is unset or set to
`memory`. All cyoda-go core env vars (admin listener, cluster,
search, etc.) apply normally — see
[`../ARCHITECTURE.md`](../ARCHITECTURE.md) §9.

## Operational notes and limits

- Process-local; data lost on restart.
- Single-process only — multiple cyoda processes against the "same"
  memory plugin would have independent state (there is no shared store).
- No persistence snapshots. Pair with periodic exports to a durable
  backend if agent/simulation results matter beyond the session.
- Tenant isolation is application-layer only (same trust model as the
  SQLite plugin).

## When to use / when not to use

**Use:** tests, short-lived local dev, parity baselines, high-throughput
digital-twin simulations where durability is delegated to an external
snapshot mechanism.

**Don't use:** production where any restart would lose data;
multi-process deployments; anywhere durable storage is a functional
requirement.
