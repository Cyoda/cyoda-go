# Cyoda-Go Concurrency Model

**Version:** 1.0
**Date:** 2026-05-03
**Status:** Architectural grounding for in-process concurrency, lock
discipline, and per-node state. Companion to
[ARCHITECTURE.md](ARCHITECTURE.md) and [CONSISTENCY.md](CONSISTENCY.md).

This document is the source of truth for what locks exist in cyoda-go,
what each protects, what is per-node-process vs durable vs per-request,
and how the cluster proxy preserves correctness across nodes.

CONSISTENCY.md is the canonical contract for the **isolation model**
(SI+FCW). ARCHITECTURE.md §4 is the canonical reference for **cluster
routing** (HMAC tokens, gossip, dispatch). This document fills the gap
between them: where state lives, what locks gate access, and what
cluster mode does and does not promise.

---

## 1. Deployment shape

cyoda-go runs as **N stateless nodes against a shared durable backend**.
The default is single-node; cluster mode is opt-in
(`CYODA_CLUSTER_ENABLED=true`). All nodes are functionally identical —
any node can receive any request — but transactions are pinned to the
node that began them via HMAC-signed routing tokens (see
ARCHITECTURE.md §4.2).

Three open-source storage plugins (`memory`, `sqlite`, `postgres`) plus
the commercial `cassandra` plugin all deliver the same SI+FCW contract
(CONSISTENCY.md §1) but use very different concurrency mechanisms.

| Plugin | Topology | Durability | Concurrency model |
|---|---|---|---|
| `memory` | **single-node single-process only** (convention; not enforced at startup) | none | In-process SI+FCW with `committedLog` window scan |
| `sqlite` | **single-node single-process only** (enforced via `flock` on the DB file at startup) | local file | In-process SI+FCW (same engine as memory); SQLite is durability layer |
| `postgres` | **multi-node** (cluster proxy + gossip registry + transaction-routing tokens) | shared PostgreSQL primary | Delegates SI to PG `REPEATABLE READ` + tuple locks; application-layer FCW via read-set validation |
| `cassandra` (commercial) | **multi-cluster** | Cassandra cluster | Plugin-internal; same SI+FCW contract |

Memory and sqlite are explicitly out-of-scope for cluster mode — running
them with `CYODA_CLUSTER_ENABLED=true` is unsupported. The flock on
sqlite enforces single-process at startup; memory has no equivalent
gate (multiple memory-backed processes would silently maintain
independent in-memory state).

## 2. State scope

Every piece of mutable state in cyoda-go falls into one of three
scopes. The scope determines what protects it.

| State | Scope | Protection |
|---|---|---|
| **Entity rows + version history** | Durable (per-tenant) | Storage engine (PG MVCC + tuple locks; SQLite write lock; in-memory `factory.entityMu` for memory). Tenant-scoped at every layer (PG RLS, SQL `WHERE tenant_id=$1`, in-process map keyed by tenant). |
| **Model registry, KV, messages, audit, search jobs** | Durable (per-tenant) | Same per-plugin storage as entities. |
| **Active transactions table** (`m.active`) | Per-node-process | Plugin manager mutex (`m.mu`) — brief holds for lookup/insert/delete |
| **Per-tx state** (`*spi.TransactionState` for memory/sqlite; internal `txState` for postgres) | Per-node-process, per-tx | `tx.OpMu` for memory/sqlite (RLock=in-flight ops, Lock=closure ops); internal `txState.mu` for postgres |
| **Committed-log window** (memory/sqlite only) | Per-node-process | `m.mu` — pruned on each commit |
| **Submit-time cache** | Per-node-process | `m.mu`; sqlite also persists to a table |
| **Model cache** (cluster broadcast invalidation) | Per-node | `modelcache.mu` (RWMutex); gossip-driven invalidation |
| **Path-validation cache** | Per-node | `path_validation_cache.mu` (RWMutex) |
| **Gossip member registry** | Per-node | `gossip.mu` + `gossipDelegate.subsMu` |
| **Transaction lifecycle** (cluster `active`/`outcomes` maps) | Per-node | `lifecycle.Manager.mu` (RWMutex); TTL-evicted |
| **AEAD nonce-replay cache** | Per-node | `nonce_cache.mu`; bounded TTL eviction |
| **HTTP `pgxpool.Pool`** (postgres only) | Per-node | Vendored pool internals |
| **`UserContext`, `txCtx`** | Per-request | `context.Context` (immutable values) |
| **HMAC tx-routing token** | Per-request (issued at Begin, expires) | Cryptographic — caller cannot forge without `CYODA_HMAC_SECRET` |

## 3. In-process lock inventory and order

The lock-order invariant for the SPI tx-state surface
(`cyoda-go-spi/txcontext.go` godoc):

```
tx.OpMu  →  factory's per-store mutex  →  manager's per-tx-table mutex
```

Manager mutex appears at TWO points: (a) brief lookup in `m.active`
released BEFORE acquiring `tx.OpMu`, (b) optional re-acquisition INSIDE
the OpMu region for `m.savepoints` / committed-log maintenance. Holding
the manager mutex across `tx.OpMu` acquisition is a deadlock-bug.

**Per-plugin lock detail:**
- Memory: see `docs/audits/2026-05-memory-plugin-tx-locking.md`
- SQLite: see `docs/audits/2026-05-sqlite-plugin-tx-locking.md`
- Postgres: see `docs/audits/2026-05-postgres-plugin-tx-locking.md`

**Postgres-plugin internal locks** (postgres uses a different shape — the
SPI's OpMu contract is satisfied trivially, so equivalent bookkeeping
lives in plugin-internal mutexes):
- `plugins/postgres/transaction_manager.go:30` — `mu` (Mutex) on `submitTimes` and `tenants`.
- `plugins/postgres/transaction_manager.go:38` — `txStatesMu` (RWMutex) on the `txStates` map keying internal txState by txID.
- `plugins/postgres/txstate.go:30` — per-tx `mu` (Mutex) on the internal `txState` (`readSet`, `writeSet`, `savepoints`).
- `plugins/postgres/tx_registry.go:12` — `mu` (RWMutex) on the `txID → pgx.Tx` registry.

**Memory-plugin per-store mutexes** (separate from `factory.entityMu`;
each non-entity store has its own RWMutex):
- `plugins/memory/store_factory.go:46-51` — `entityMu`, `modelMu`, `kvMu`, `msgMu`, `wfMu`, `smAuditMu` (RWMutex each).

**Non-tx-state locks** (separate from the SPI contract; documented here
for completeness):
- `internal/cluster/modelcache/cache.go` — RWMutex on cache entries; Lock for invalidation, RLock for Get.
- `internal/cluster/registry/gossip.go` — multiple mutexes: `mu` on node meta, `subsMu` on subscription map, plus the gossipDelegate's broadcast queue.
- `internal/cluster/lifecycle/manager.go` — RWMutex on `active`/`outcomes` maps; TTL-evicted.
- `internal/cluster/dispatch/nonce_cache.go` — Mutex; AEAD replay-window enforcement.
- `internal/domain/search/path_validation_cache.go` — RWMutex on validation results.
- `internal/grpc/members.go` — multiple mutexes governing gRPC member streams.

These locks are local to their owning component and do not interact
with the tx-state lock order. They each have brief, bounded critical
sections; none is held across slow operations.

Test infrastructure (`internal/testing/...`, `internal/e2e/...`,
`internal/common/diagnostics.go`) and per-package caches in `internal/auth/`
are excluded from the inventory — they are local to JWT/JWKS caching or
test setup and do not interact with the tx-state surface.

## 4. The SPI tx-state locking contract

Defined formally in [cyoda-go-spi `TransactionState` godoc][spi-godoc]
and [`.claude/rules/tx-state-locking.md`][spi-rule] (in the
cyoda-go-spi repository). Summary:

- **Cross-class serialisation** (plugin's responsibility, enforced via
  `tx.OpMu`): in-flight tx-path operations hold `RLock`; closure
  operations (Commit, Rollback, RollbackToSavepoint) hold `Lock`.
- **Within-class serialisation** (application's responsibility, NOT
  enforced): if the application fires two RLock-holding ops on the
  same tx concurrently, Go's "concurrent map writes" runtime fatal
  fires on `tx.Buffer`/etc. The plugin does not detect or recover.
- **Tenant isolation** (every plugin enforces): every TM lifecycle
  method rejects mismatched-tenant callers (Commit, Rollback, Join,
  Savepoint, RollbackToSavepoint, ReleaseSavepoint).

Postgres satisfies the OpMu contract trivially because its
`*spi.TransactionState` carries only `ID` and `TenantID` — the
OpMu-protected fields on the SPI struct are unused. Equivalent per-tx
FCW bookkeeping lives in an internal `txState` struct (with its own
per-tx `mu`) independent of the SPI surface.

[spi-godoc]: https://github.com/Cyoda-platform/cyoda-go-spi/blob/v0.6.1/txcontext.go
[spi-rule]: https://github.com/Cyoda-platform/cyoda-go-spi/blob/v0.6.1/.claude/rules/tx-state-locking.md

## 5. Per-plugin pointer

Plugin-specific concurrency detail lives in each plugin's doc:

- [`docs/plugins/IN_MEMORY.md`](plugins/IN_MEMORY.md) — in-process SI+FCW with detailed lock sequence.
- [`docs/plugins/SQLITE.md`](plugins/SQLITE.md) — application-layer SI+FCW; `flock` startup gate; `commitMu` serialises whole commit path.
- [`docs/plugins/POSTGRES.md`](plugins/POSTGRES.md) — `REPEATABLE READ` + commit-time read-set validation; `pgx.Tx` single-owner property.

Per-plugin tx-state locking audits (PR-C of #199):

- [`docs/audits/2026-05-memory-plugin-tx-locking.md`](audits/2026-05-memory-plugin-tx-locking.md)
- [`docs/audits/2026-05-sqlite-plugin-tx-locking.md`](audits/2026-05-sqlite-plugin-tx-locking.md)
- [`docs/audits/2026-05-postgres-plugin-tx-locking.md`](audits/2026-05-postgres-plugin-tx-locking.md)

## 6. Cluster routing — what it covers and what it doesn't

Cluster routing is fully described in [ARCHITECTURE.md §4](ARCHITECTURE.md#4-multi-node-routing-architecture).
This section enumerates only what cluster mode promises and what it does not.

**Covered (cluster mode promises):**
- Every transaction is pinned to its home node via HMAC-signed token
  (`{NodeID, TxRef, ExpiresAt}`). Subsequent requests for the same tx
  are routed to the home node — both the **HTTP reverse proxy**
  (`internal/cluster/proxy/http.go`, transparent re-proxying) and the
  **gRPC routing helper** (`internal/cluster/proxy/grpc.go`,
  resolve-target + redirect-error pattern) follow the same pinning
  semantics, just with different transports.
- Gossip registry tracks node liveness and broadcasts lifecycle events
  (model invalidation, member tags).
- AEAD-encrypted inter-node forwarding with nonce-replay protection.
- Tenant isolation is preserved end-to-end: home node holds the tx's
  tenant, and every TM method enforces tenant equality before any
  state mutation (PR-A/PR-C1/PR-C2 of #199).

**Not covered (failure modes the application/operator must handle):**
- **Home-node crash mid-transaction.** The pgx.Tx connection is lost;
  PostgreSQL rolls the tx back automatically. Subsequent client
  requests with the original token receive `503 TRANSACTION_NODE_UNAVAILABLE`.
  The client must restart with a fresh `Begin()` on a surviving node.
  No automatic transaction recovery.
- **Network partition.** If the home node becomes unreachable but
  remains alive, the proxy returns `503` until gossip declares it
  dead or the partition heals. In-flight transactions on the home
  node may still commit; clients on the other side cannot observe.
- **Token expiry.** The token has a bounded `ExpiresAt`. If the
  client holds it past expiry, the proxy returns
  `400 TRANSACTION_EXPIRED`. The client must abandon the tx and
  retry with a fresh `Begin()`.
- **Double-commit / idempotent retry.** First-committer-wins +
  TOCTOU guards (`m.committing[txID]` for memory/sqlite; `pgxTx`
  single-owner for postgres) prevent a successful tx from
  committing twice. The second attempt receives `ErrConflict` or
  "transaction already completed".
- **Cross-node failure of the cluster proxy itself.** Nodes that lose
  gossip connectivity stop receiving forwarded requests, but their
  local transactions continue. Partition detection is governed by
  hashicorp/memberlist's probe interval, suspicion multiplier, and
  dead-node timers — not by the startup-time `StabilityWindow` (which
  is only consulted during `Register()` to wait for join convergence
  before opening the gRPC server). Recovery time depends on memberlist
  configuration; gossip propagates eventually.

cluster routing is **independent of the storage backend.** It applies
only to the postgres plugin (multi-node-capable) and the commercial
cassandra plugin. Memory and sqlite, being single-node, do not need
cluster routing — but if `CYODA_CLUSTER_ENABLED=true` were set with
those backends (unsupported), each node would independently maintain
its own in-memory state and the cluster would not be coherent.

## 7. The application's responsibility

The plugin's tx-state contract handles **two of the three** classes of
concurrent access on a single transaction:

1. **In-flight ops vs. Commit/Rollback/RollbackToSavepoint** — gated
   by `tx.OpMu` (RLock vs. Lock).
2. **Cross-tenant lifecycle disruption** — rejected by every TM
   method's tenant gate.

The third class is the application's responsibility:

3. **Multiple concurrent in-flight ops on the same tx from different
   goroutines.** `OpMu.RLock` permits multiple holders concurrently —
   it does not mutually exclude RLock holders from each other.
   In-flight ops mutate `tx.Buffer` / `tx.WriteSet` / `tx.Deletes`
   while holding RLock; two concurrent `Save` calls from different
   goroutines on the same tx will trigger Go's "concurrent map writes"
   runtime fatal regardless of key overlap. The plugin does NOT detect
   or recover from this. The application must serialise its own per-tx
   ops externally.

In practice cyoda's domain layer ensures this naturally: HTTP/gRPC
handlers process requests sequentially per request; the workflow
engine executes processors sequentially within a single transition.
At the time of writing no production callsite outside the
`internal/observability/tx_tracing.go` pass-through decorator invokes
`TransactionManager.Join`. Any future feature that fans out work across
multiple goroutines on a single tx must either add its own per-tx
mutex or split work across transactions, AND must update this section.

## 8. Further reading

- [ARCHITECTURE.md §3](ARCHITECTURE.md#3-transaction-model) — Transaction Model overview.
- [ARCHITECTURE.md §4](ARCHITECTURE.md#4-multi-node-routing-architecture) — Multi-node routing, gossip, cluster proxy.
- [CONSISTENCY.md](CONSISTENCY.md) — SI+FCW isolation contract, anomaly classes, operational rules.
- [`docs/superpowers/specs/2026-04-15-postgres-si-first-committer-wins-design.md`](superpowers/specs/2026-04-15-postgres-si-first-committer-wins-design.md) — design rationale for postgres plugin's REPEATABLE READ + application-layer FCW.
- [`docs/audits/`](audits/) — per-plugin tx-state locking audits surfaced by issue #199.
