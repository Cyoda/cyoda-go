# Design: compute-node callbacks join the originating transaction

**Issue:** #287 · **Milestone:** v0.8.2 · **Date:** 2026-07-02

## Problem

When the engine runs a workflow transition it opens a DB transaction `T`
(`TransactionManager.Begin`) and, while holding `T` open, dispatches a processor
(or criteria) to an external compute node over the gRPC bidi stream, blocking on
the response (`internal/grpc/dispatch.go:117`). If that compute node calls **back**
into cyoda-go — over gRPC `EntityManage`/search or the HTTP entity API — to
create/update/read **other** entities, those callbacks today run in their **own
fresh transaction**: every inbound entity service method unconditionally calls
`h.txMgr.Begin(ctx)` (`internal/domain/entity/service.go:259,592,716,927,1069,1390`)
and never consults an incoming transaction. So callback writes are **not atomic**
with `T` and callback reads **cannot see** `T`'s uncommitted writes.

`docs/PROCESSOR_EXECUTION_MODES.md` ("Transaction-bound callbacks") and
`docs/ARCHITECTURE.md` document the opposite as the contract ("callbacks join `T`
by presenting the txID"). This is a **documented-contract violation (bug)**, and
it affects fidelity with Cyoda Cloud, where transaction-bound callbacks are part
of the compute-node contract.

**Design stance:** cyoda-go is **primarily a multi-node system** (see
[Multi-node is primary](#multi-node-is-primary)); the fix delivers cross-node
correctness in the same change, not as a deferred phase.

## Current state (verified)

- Inbound handlers always `Begin`; no `Join`/token read anywhere in
  `internal/domain/entity/` or `internal/grpc/entity.go`. Both transports share the
  same `entity.Handler` methods (gRPC `internal/grpc/entity.go:52,101,148,181,226`;
  HTTP `internal/domain/entity/handler.go:368`).
- The **live engine `txID` is already on the wire** to the compute node:
  `dispatch.go:65` sets `TransactionID: &txID` on every processor calc request;
  `dispatch.go:215` on every criteria request. Only the **echo-back + inbound
  `Join`** is missing.
- `TransactionManager.Join` exists for all three OSS plugins
  (`plugins/memory/txmanager.go:137`, `plugins/sqlite/txmanager.go:149`,
  `plugins/postgres/transaction_manager.go:241`) with a tenant guard, but has **no
  production caller** (only the tracing decorator).
- The HMAC routing token `token.Claims{NodeID, TxRef, Exp}`
  (`internal/cluster/token/token.go:20`) is **never minted in production**
  (`Signer.Issue` has zero non-test callers). The HTTP proxy `proxy.HTTPRouting`
  is wired (`app/app.go:652`) but dormant (nothing sets `X-Tx-Token`). The gRPC
  resolver `proxy.ResolveTarget`/`ExtractGRPCToken` (`internal/cluster/proxy/grpc.go`)
  is coded but **unwired**, and there is **no gRPC reverse-proxy transport** — only
  a decision function.
- Cluster tag-forwarding runs the processor on a **peer** node while `T` lives on
  the origin: `internal/cluster/dispatch/cluster_dispatcher.go:55-98` — origin `A`
  holds `T`, tries local dispatch, and on `ErrNoMatchingMember` forwards a
  `DispatchProcessorRequest{TxID: txID}` **struct** (not a CloudEvent) to peer `B`
  via `forwarder.ForwardProcessor`; `B` re-dispatches to **its** member and
  attaches auth locally (`B`'s `dispatch.go`). `DispatchProcessorRequest` carries
  **no owner NodeID**.
- Phantom error codes: `TX_COORDINATOR_NOT_CONFIGURED`, `TX_NO_STATE`,
  `TX_REQUIRED`, `TX_CONFLICT` are defined (`internal/common/error_codes.go:63-66`)
  but **emitted nowhere**; their help topics describe a distributed 2PC coordinator
  that does not exist and contradicts cyoda-go's stated design.

## The transaction handle: signed tx-token (distinct from `transactionId`)

Two separate concepts, kept clean:

- **`transactionId`** (response body / query param) — the **audit / version /
  temporal identifier**, doubling as the `If-Match` optimistic-concurrency ETag and
  the as-of read key. Client-facing, informational. **Unchanged.**
- **tx-token** (`X-Tx-Token` header / `tx-token` gRPC metadata) — the signed
  **live-transaction routing+join handle**: HMAC over `{NodeID, TxRef=txID, Exp}`.
  Engine-minted, un-forgeable, carries **routing integrity** (which node owns `T`).
  Tenant enforcement rides on `Join`'s existing check at the destination — the
  token is a routing credential, not an authZ grant.

The engine `txID` is already transmitted (`dispatch.go:65/215`); the token wraps it
with the owner NodeID and a signature so a callback can be **routed** to the owning
node in a cluster and cannot be forged/replayed to a different node.

## Design

### 1. Mint on the owner, before the local-vs-forward split (fixes H1)

The token **must** be minted by the node that owns `T` (`A`), with `NodeID = A`,
**before** the cluster dispatcher decides local-vs-forward. Minting where
`AttachAuthContext` runs is **wrong**: in a forwarded dispatch that code runs on
peer `B`, which would stamp `NodeID = B` and misroute callbacks.

- Mint `tok = Issue({NodeID: self, TxRef: txID, Exp: now+dispatchBudget})` at the
  engine→dispatch boundary on `A`.
- Thread `tok` **into both** paths: pass to `local.DispatchProcessor` and add a
  `TxToken` field to `DispatchProcessorRequest` / `DispatchCriteriaRequest` so
  `forwarder.ForwardProcessor` carries it to `B`.
- The calc-request builder (`dispatch.go`) **attaches the provided token
  verbatim** as a CloudEvent attribute (sibling to `AttachAuthContext`) — it does
  **not** mint. On `B` the token still reads `NodeID = A`.

### 2. Echo + route

The compute-node SDK echoes the token it received on **every** callback: as
`tx-token` gRPC metadata or the `X-Tx-Token` HTTP header. On receipt:

- **`NodeID == self`** → resolve `T` locally, `Join`, no network hop (covers
  single-node and same-node cluster dispatch).
- **`NodeID != self`** → proxy the whole callback to the owner:
  - **HTTP** — reuse the wired `proxy.HTTPRouting` `httputil.ReverseProxy`
    (`internal/cluster/proxy/http.go`). The owner re-authenticates the forwarded
    request (tenant re-checked at `A`).
  - **gRPC** — **build the missing transport** (H5): wire `ResolveTarget`/
    `ExtractGRPCToken` into a `CloudEventsService` interceptor; on `shouldProxy`,
    `B` re-issues the unary `EntityManage`/search call to `A`'s
    `CloudEventsService` over a cluster client conn (reusing the gossip-known peer
    address + existing dial infra), forwarding the CloudEvent, auth context, and
    `tx-token` metadata, and returns `A`'s response verbatim.

### 3. Shared-service Join-not-Begin

Placed in the shared `entity.Handler` methods so **HTTP and gRPC** both inherit it,
via `spi.GetTransaction(ctx)` (already the pattern in
`grouped_stats_service.go:99`). On each inbound entity op:

- token/txID present in ctx and **joinable** → **participate**: no nested `Begin`,
  no commit/rollback (§4).
- **empty** txID → standalone `Begin` (correct — no transactional semantics
  requested; this is the CBD-default path).
- **non-empty** txID that **cannot** join (unknown/closed/expired/foreign-after-
  proxy) → **fail loud** with the mapped error (§5). Never silently `Begin`.

### 4. Ownership + concurrency

- The **dispatching goroutine on the owner** owns `T`'s lifecycle and **alone**
  commits/rolls back, based on the transition outcome.
- Joined callbacks **never** commit or roll back `T`.
- **Exclusive per-tx gate (fixes H2).** A new **exclusive** application-level mutex
  per transaction (held in the tx-registry entry) wraps the **entire callback**
  (`Join → business logic → Save`). This is **stronger than and distinct from**
  `tx.OpMu.RLock` — two concurrent `RLock`-holding `Save`s on one tx trigger Go's
  "concurrent map writes" fatal (memory) / concurrent `pgx.Tx` misuse (postgres)
  (`cyoda-go-spi/transaction.go:46-54`, cf. #199). A single processor may
  fire multiple parallel callbacks; each must serialize on this gate.
- **Commit drains callbacks.** The owner's commit path acquires the **same** gate,
  so commit waits for any in-flight callback to complete (not just the `Save`
  instant).
- **Invariant (fixes H3), tested:** *the outbound dispatch must never be executed
  while holding the per-tx gate.* Today the engine holds no `OpMu` across the
  blocking dispatch, which is what lets a proxied-back callback `Join`+`Save` while
  the owner blocks. A regression test asserts this to prevent a future refactor
  from reintroducing a distributed deadlock (owner holds gate → waits on processor
  → processor callback waits on gate).

### 5. Execution-mode semantics

| Mode | Callback behavior |
|---|---|
| `SYNC`, `ASYNC_SAME_TX` | join `T` (held open during dispatch) |
| `COMMIT_BEFORE_DISPATCH`, `startNewTxOnDispatch=true` | join `TX_post` |
| `ASYNC_NEW_TX` | join `T`, **writes scoped to the processor's savepoint** (below) |
| `COMMIT_BEFORE_DISPATCH`, default (no open tx) | standalone `Begin` (regression guard) |

**ASYNC_NEW_TX (resolves H4).** There is no separate transaction —
`executeAsyncNewTx` (`engine_processors.go:190-207`) takes a **savepoint inside
`T`** and dispatches with the same `txID`. A callback therefore joins `T`, but its
writes live inside the processor's savepoint scope: on processor **success** the
savepoint is released (writes persist with `T`); on processor **failure** the
engine `RollbackToSavepoint` **discards the callback's writes** and — because
ASYNC_NEW_TX failure is non-fatal — the pipeline continues and `T` commits
**without** them. This is the **correct** semantics (the callback is part of the
processor's isolated, independently-rollback-able unit), but it means a callback
ack is **provisional** (§6). Terminology corrected in the spec/doc ("joins `T`,
savepoint-scoped" — not "a separate tx"), and covered by an explicit test.

### 6. Provisional-ack contract (H6)

Between a callback `Join`+`Save` succeeding and the owner's commit — including the
**dispatch-timeout** path (`dispatch.go:135`) and ASYNC_NEW_TX savepoint rollback —
the callback's write can be committed-away or discarded with `T`. With the §4 gate
wrapping the whole callback and commit contending on it, this is **correct
atomicity**, but it is a **contract change the SDK docs must state**: *a callback
ack is not durable until the owning transaction commits.* A callback that arrives
**after** `T` is gone → `TRANSACTION_NOT_FOUND`.

### 7. Scope

- **Transports:** gRPC (`EntityManage`, `EntityManageCollection`, search/get) **and**
  HTTP entity API.
- **Processors AND criteria (H7):** criteria dispatch also holds `T` and ships
  `TransactionID` (`dispatch.go:215`); a criteria service doing `Get`/`Search` for
  read-your-writes joins identically.
- **Ops:** writes (`Create`/`Update`/`Delete`/`Transition`) + reads (`Get`/`Search`,
  for read-your-own-writes) + the **`EntityManageCollection` streaming loop (H8)** —
  under a joined outer `T` the per-item loop participates in `T` (atomic-in-`T`),
  rather than each item opening its own tx.
- **If-Match inside a joined tx (H9):** a callback `Update`/`Patch` with `If-Match`
  against a version written earlier in the uncommitted `T` resolves against `T`'s
  write buffer — covered by an explicit parity/e2e case.

### 8. Security / tenant (Gate 3)

- Tenant enforcement is at the destination `Join`: rejects mismatched tenants on all
  three plugins (`memory/txmanager.go:162`, `postgres/transaction_manager.go:251`,
  sqlite mirror). The proxied-to owner re-authenticates the forwarded request, so a
  compute node authenticated as tenant X cannot touch tenant Y's tx.
- The token `Claims` has **no tenant field** — it is a routing hint, not an authZ
  grant; the signature provides **routing integrity**, tenancy rides on `Join`. Same
  tenant: possession of a valid signed token + `Join` prevents guessing/replaying a
  different txID (defense-in-depth; the txID is already cleartext to the compute
  node, so this is not load-bearing security).
- **Redaction (H10):** the token must never be logged — audit the payload-preview
  log sites (`dispatch.go:101`) and ensure the HTTP proxy does not tee `X-Tx-Token`
  / the `tx-token` metadata into access logs.

### 9. Observability

The joined-callback path resolves through the tracing decorator
(`internal/observability/tx_tracing.go`); ensure a joined `Join`/`Save` is traced
under the **owning** `T`'s span, and that a proxied callback records the hop.

## Error / status-code table (per callback entry point)

Applies to every inbound entity op (gRPC `EntityManage` create/update/delete/
transition, `EntityManageCollection`, search/get; HTTP entity endpoints) when a
non-empty tx-token is presented.

| Condition | Code | HTTP |
|---|---|---|
| no token / empty txID (standalone) | — (happy path, `Begin`) | 2xx |
| txID not in any registry (unknown / already committed / rolled back / reaped) | `TRANSACTION_NOT_FOUND` | 404 |
| token/tx past expiry | `TRANSACTION_EXPIRED` | 410 |
| owner node unreachable after proxy attempt | `TRANSACTION_NODE_UNAVAILABLE` | 503 |
| forged / bad-HMAC token | `UNAUTHORIZED` | 401 |
| token/tx tenant ≠ authenticated caller | `FORBIDDEN` | 403 |

**No new error codes.** Each has an existing `errors/*.md` topic, so
`TestErrCode_Parity` stays green. Phantom codes are corrected in docs (below), not
removed.

## Coverage matrix (scenario × layer)

Layers: **U** unit · **E** running-backend e2e (`internal/e2e`, real Postgres) ·
**P** cross-backend parity (`e2e/parity`, memory/sqlite/postgres + commercial) ·
**G** gRPC (`internal/grpc`) · **MN** multi-node (isolated).

| Scenario | U | E | P | G | MN |
|---|---|---|---|---|---|
| SYNC callback write is atomic with `T` (rolls back when transition fails) | ✓ | ✓ | ✓ | ✓ | |
| SYNC callback read sees `T`'s uncommitted cascade write (read-your-writes) | ✓ | ✓ | ✓ | ✓ | |
| CBD `startNewTxOnDispatch=true` callback writes in `TX_post` | ✓ | ✓ | ✓ | | |
| CBD default (no tx) → callback runs standalone (regression guard) | ✓ | ✓ | | ✓ | |
| ASYNC_NEW_TX callback write discarded on processor failure; pipeline continues; `T` commits without it | ✓ | ✓ | ✓ | | |
| Criteria callback read-your-writes joins `T` | ✓ | ✓ | ✓ | ✓ | |
| `EntityManageCollection` under joined `T` is atomic-in-`T` | ✓ | ✓ | | ✓ | |
| If-Match against an in-`T` uncommitted version | ✓ | ✓ | ✓ | | |
| empty txID → `Begin` (unchanged behavior) | ✓ | ✓ | ✓ | ✓ | |
| non-empty unjoinable txID → loud fail (`TRANSACTION_NOT_FOUND`) | ✓ | ✓ | | ✓ | |
| expired token → `TRANSACTION_EXPIRED` | ✓ | | | ✓ | |
| forged token → `UNAUTHORIZED` | ✓ | | | ✓ | |
| cross-tenant token → `FORBIDDEN` (Gate 3) | ✓ | ✓ | | ✓ | ✓ |
| **Local-dispatch callback**: `NodeID==self` → local Join, no hop | ✓ | ✓ | | ✓ | |
| **Forwarded dispatch** (processor on B, `T` on A): token minted on A survives forward; callback lands on B → proxied to A → Join (HTTP) | | | | | ✓ |
| Same, gRPC callback transport (B→A unary forward) | | | | | ✓ |
| owner node down → `TRANSACTION_NODE_UNAVAILABLE` | | | | | ✓ |
| concurrent callbacks on one tx serialize (no torn write / no fatal) | ✓ | ✓ | | | ✓ |
| invariant: dispatch never executed while holding the per-tx gate | ✓ | | | | |

Concurrency/multi-node scenarios are **isolated single-purpose tests**, never in
the shared parity suite (`.claude/rules/test-coverage.md`).

## Gate-4 documentation

1. **Phantom-code correction.** Rewrite `cmd/cyoda/help/content/errors/{TX_COORDINATOR_NOT_CONFIGURED,TX_NO_STATE,TX_REQUIRED,TX_CONFLICT}.md`
   to stop describing a non-existent distributed 2PC coordinator; describe the real
   model (request-scoped tx today; tx-token-routed live transactions as the design).
   Correct the corresponding PRD/ARCHITECTURE sections.
2. **`PROCESSOR_EXECUTION_MODES.md` / `ARCHITECTURE.md`** — update the
   "Transaction-bound callbacks" contract to describe the implemented mechanism
   (signed tx-token echo, owner-mint-before-forward, Join-not-Begin, provisional
   ack, ASYNC_NEW_TX savepoint scoping).
3. <a name="multi-node-is-primary"></a>**Multi-node-is-primary note.** Add a
   standing note — in `.claude/rules/` (so review subagents and agents reading the
   repo see it), in `docs/ARCHITECTURE.md`, and at the `CYODA_CLUSTER_ENABLED`
   default site (the cluster config help topic + `DefaultConfig()` comment) — stating
   that cyoda-go is **primarily multi-node**, that off-by-default is an onboarding
   affordance, and that **cluster/HA correctness must not be descoped or deferred on
   proportionality grounds.**
4. **SDK / contract.** Document the callback echo requirement (`tx-token` /
   `X-Tx-Token`) and the provisional-ack contract; extend `cmd/compute-test-client`
   to echo the token and exercise a callback (it currently only replies over the
   stream). COMPATIBILITY / CHANGELOG per Gate 4.

## Out of scope

- **Client-driven multi-request transactions** — the client-facing open/commit/
  rollback API built on this same substrate is tracked in **#367** (backlog).
- **Internalized processors** (#252/#260) — in-process execution shares context
  natively, no token/proxy needed.
- **Distributed 2PC across shards** — cyoda-go deliberately does not do distributed
  transaction coordination; a tx lives on its owner and dies with it
  (`TRANSACTION_NODE_UNAVAILABLE`).

## Relationships

- **Supersedes** #364's "delete the inert tx-routing token" action — this change
  makes the token load-bearing (#364 updated).
- **Foundation for** #367 (client-driven multi-request transactions).
- **Unblocks** #30 (multi-node proxy-routing E2E).
- **Relates to** #200 (tx-state sentinel errors — late-callback behavior) and #199
  (`tx.OpMu` coverage).
