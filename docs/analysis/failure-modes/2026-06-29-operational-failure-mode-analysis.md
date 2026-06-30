# Cyoda‑Go — Operational Failure‑Mode Analysis

**Subject:** `cyoda-go` core + the PostgreSQL storage plugin
**Source analysed:** sibling checkout `../cyoda-go`, branch `release/v0.8.2` (next release), plus its embedded plugin `plugins/postgres` and the contract module `github.com/cyoda-platform/cyoda-go-spi v0.8.1`.
**Out of scope (by request):** the Cassandra plugin / `cyoda-go-cassandra` repo, and the `memory`/`sqlite` plugins except where they reveal a shared‑contract issue.
**Date:** 2026‑06‑29
**Re‑running this analysis:** see [`2026-06-29-operational-failure-mode-analysis-playbook.md`](./2026-06-29-operational-failure-mode-analysis-playbook.md) for the reproducible method, the load‑bearing facts to re‑verify, and how to diff against this baseline.

**Question this answers:** *Not* “is it fast?” — but “can it break down uncontrollably, produce inconsistent results, corrupt data, or become unavailable at moderate volume?” (tens of concurrent requests, 2–3 nodes, an occasionally slow/dead compute node, clients that disconnect).

**Method.** Ten independent reviewers each took one failure‑mode dimension and read the actual code; every finding they produced was then handed to a separate **adversarial verifier** instructed to *refute* it by re‑reading the cited source and hunting for mitigations (a timeout, a reaper, a lock, a recover, a config default). A claim is reported as *confirmed* only when a verifier could quote code proving the failure mode is real **and** reachable. 54 candidate findings were produced; the verifiers **confirmed 27, partially‑confirmed 26, and refuted 1**, and downgraded the severity of 18 of them once mitigations were found. Every claim below carries `file:line` evidence that was independently re‑read.

> **How to read severities.** “Verified sev” is the *post‑adversarial* severity, not the reviewer’s first guess. Several headline items started as “critical” and were lowered to “high” because a 30‑second dispatch timeout makes the worst case a *transient* stall rather than a permanent wedge. Conversely, many “high” guesses dropped to “low” once a guard was found. The full ledger of all 54 is in the [Appendix](#appendix--full-finding-ledger).

> **Maintainer disposition (2026‑06‑30).** The connection‑held‑across‑callout behaviour analysed in §2 is **by design and engine‑specific** — it follows from PostgreSQL's connection‑per‑transaction model; the Cassandra plugin does not hold a connection this way. `cyoda-go` is infrastructure *for* services, not a client‑facing gatekeeper: the ring‑fencing facilities already exist (`COMMIT_BEFORE_DISPATCH`, `ASYNC_NEW_TX`), and **the application owns the decision** to use them for side‑effecting or long‑running call‑outs — exactly as it owns writing well‑behaved processors and criteria. The default assumption is that processors and criteria are properly designed; capping a held transaction by default would be like capping a table at 100 rows in case the disk fills. Accordingly, the remediation for this *by‑design* class (`grpc-compute-1`, `tx-pool-exhaustion-1`, `cross-cutting-1`, `cluster-coordination-2`, `ssi-correctness-5`) is **documentation** — making the contract, its impact, and its engine‑specificity explicit — **not** a change of default execution mode or a server‑imposed `responseTimeoutMs` ceiling. The DB‑side backstops in #354 (timeouts, `defer`‑rollback) remain valid, because they protect against **panics and genuine faults**, which are not the application's call. Where the prose below recommends “make `COMMIT_BEFORE_DISPATCH` the default” (§2.5, §8), read it as **“document the tradeoff and the facility”** per this disposition.

---

## 1. Bottom line

**Your worry is half‑right, and it’s worth acting on — but it is not the half people usually fear.**

* **Will it silently corrupt data or give inconsistent results?** *Largely no, under the default single‑node configuration.* The snapshot‑isolation + first‑committer‑wins design is **correct**: the commit‑time `FOR SHARE` read‑set validation genuinely enforces first‑committer‑wins, write‑write conflicts surface as `40001 → ErrConflict`, and the most alarming consistency hypotheses (whole‑model lock storms, write‑skew through the platform’s own code paths) were **refuted or shown unreachable**. There is **one** genuine silent‑corruption path — concurrent schema‑extension folding in auto‑evolving schema mode (§4.5) — and a family of *by‑design* tradeoffs around external side effects (§5).

* **Can it become unavailable at moderate volume?** **Yes — this is the real and dominant risk, and it is structural.** A single architectural choice plus a set of missing operational guardrails produce several independent, confirmed paths to a node stalling, leaking until it stalls, crashing, or OOM‑ing. They are clustered, not scattered, which is good news for remediation: a handful of fixes neutralise most of them.

* **Will it split‑brain across nodes?** **No true split‑brain corruption was found.** Multi‑node clustering is **off by default** (`CYODA_CLUSTER_ENABLED=false`), and when enabled the shared PostgreSQL keeps the data layer consistent; the cluster issues are bounded availability/inconsistency *windows*, not data divergence.

### The one sentence

> The system **will not silently corrupt your database**, but at moderate volume it has several realistic, code‑proven paths to becoming **unavailable** — and they nearly all converge on a single design decision: **it holds a scarce pooled database connection open across unbounded external work**, with the operational backstops that should contain that (DB statement/idle timeouts, an orphaned‑transaction reaper, a gRPC panic guard, bounded result sets) **either missing or non‑functional**.

### The five things that actually matter

| # | Failure | Class | Verified | Default‑config reachable? |
|---|---------|-------|----------|---------------------------|
| 1 | A DB transaction (one of only **25** pooled connections) is held open across the external gRPC compute call; a slow/dead/frozen compute node stalls the whole node for all tenants | Unavailability | **High** | **Yes** |
| 2 | A panic between `Begin` and `Commit` **permanently leaks** a pooled connection — no `defer` rollback, the reaper is dead code, and there is no DB‑side idle timeout | Unavailability / leak | **High** | **Yes** |
| 3 | The gRPC server has **no panic‑recovery interceptor**, and the compute‑member stream has a **concurrent‑`Send` data race** — either can crash the entire process | Uncontrolled breakdown | **High** | **Yes** |
| 4 | `GET` list and unbounded search **materialise an entire model into heap** → node OOM at moderate data volume | Resource exhaustion / OOM | **High** | **Yes** |
| 5 | In auto‑evolving schema mode, a periodic schema‑fold savepoint can **permanently drop a concurrent schema delta**, silently corrupting the model schema | Inconsistency / corruption | **High** | Yes (auto‑schema models) |

The rest of this document explains each, with the code. **[§9](#9-residual-picture-if-transaction-duration-limits-are-enforced-pool-starvation-remediated)** re‑scores the whole picture under the assumption that the (already‑present but unwired) transaction‑duration backstops are connected and pool starvation is remediated.

---

## 2. The dominant failure mode — “a database connection held hostage by an external system”

This is the spine of the analysis. Understanding it makes most of the other availability findings fall out as corollaries.

### 2.1 The mechanism (confirmed)

Every entity create/update/patch/delete/transition:

1. **Begins a real PostgreSQL transaction** at `REPEATABLE READ`, which **pins one connection from the pool for the entire application‑level transaction**:
   ```go
   // plugins/postgres/transaction_manager.go:68
   pgxTx, err := tm.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
   ```
   ```go
   // internal/domain/entity/service.go:228,257
   txID, txCtx, err := h.txMgr.Begin(ctx)
   ...
   result, err := h.engine.Execute(txCtx, entity, "")   // whole workflow runs inside the held tx
   ```

2. **Runs the workflow engine inside that transaction context.** `SYNC` and `ASYNC_SAME_TX` processors — **the default execution mode** — are dispatched to an external gRPC compute node *inline, while the connection is held*:
   ```go
   // internal/domain/workflow/engine_processors.go:105
   default: // SYNC, ASYNC_SAME_TX — both inline in caller's transaction.
       procErr = e.executeSyncProcessor(currentCtx, entity, proc, workflow, transition, currentTxID)
   ```
   ```go
   // internal/domain/workflow/engine_processors.go:160-164
   func (e *Engine) executeSyncProcessor(...) error {
       ...
       modifiedEntity, err := e.extProc.DispatchProcessor(ctx, entity, proc, workflow, transition, txID)
   ```
   (`ASYNC_NEW_TX` dispatches inside a **savepoint** on the same held connection; `COMMIT_BEFORE_DISPATCH`, which commits first, exists precisely to avoid this — but it is **opt‑in**, not the default.)

3. **The pool is small and has no escape valve.** Default `MaxConns=25`, and `newPool` configures *only* sizing and idle time — **no `AcquireTimeout`, no `statement_timeout`, no `idle_in_transaction_session_timeout`, no `lock_timeout`** (the latter three were confirmed absent across the entire plugin and migrations):
   ```go
   // plugins/postgres/config.go:36-37 / 135-137
   MaxConns: envInt32(getenv, "CYODA_POSTGRES_MAX_CONNS", 25),
   MinConns: envInt32(getenv, "CYODA_POSTGRES_MIN_CONNS", 5),
   ...
   poolCfg.MaxConns = cfg.MaxConns
   poolCfg.MinConns = cfg.MinConns
   poolCfg.MaxConnIdleTime = cfg.MaxConnIdleTime   // only reaps *idle* conns, never an in‑tx conn
   ```

4. **The HTTP server has no timeouts** and there is no per‑request deadline anywhere, so a request that arrives when the pool is empty blocks on `Acquire` indefinitely (released only if the client itself disconnects):
   ```go
   // cmd/cyoda/run.go:84  — no ReadTimeout/WriteTimeout/IdleTimeout/TimeoutHandler
   httpServer := &http.Server{Addr: httpAddr, Handler: a.Handler()}
   ```

5. **Non‑transactional reads share the same pool** (`resolveRaw` returns `f.pool` when no tx is in context — `store_factory.go:106-114`), so once writes drain the pool, **`GET`s and searches block too**.

### 2.2 What actually happens at moderate volume

A single connected‑but‑slow (or dead) compute node serving a workflow tag, plus a sustained stream of transitions routed to it, pins connections 30 s at a time (the default dispatch timeout). With only 25 connections, the pool saturates within the first wave, and **every subsequent `Begin` — and every pooled read — blocks for all tenants.** The blast radius is the **entire data plane of the node, cross‑tenant.**

This is **bounded and self‑healing** for an ordinary slow node, because the dispatch has a real ceiling:
```go
// internal/grpc/dispatch.go:24,135-139
const defaultResponseTimeoutMs = 30000
...
case <-time.After(timeout):
    return nil, fmt.Errorf("processor dispatch timed out after %dms", timeoutMs)
case <-ctx.Done():
    return nil, ctx.Err()
```
So the honest severity is **High, not Critical**: a degraded compute node produces a *transient rolling stall* that recovers when it recovers or load drops — not a permanent wedge or data loss. *(Findings `tx-pool-exhaustion-1`, `grpc-compute-1`, `cross-cutting-1`, `tx-no-request-deadline-3` — all confirmed; the inflated “critical” guesses were corrected to high precisely because of this 30 s ceiling.)*

### 2.3 …except when it is **not** self‑healing (the sharper edge — `grpc-compute-2`, confirmed High)

The 30 s ceiling assumes the dispatch can *start*. But `DispatchProcessor` calls `member.Send` **before** entering the timeout `select`, and `Send` writes the gRPC stream under a mutex:
```go
// internal/grpc/members.go:56-60
func (m *Member) Send(ce *cepb.CloudEvent) error {
    m.sendMu.Lock(); defer m.sendMu.Unlock()
    return m.send(ce)
}
```
If a compute node is **frozen but TCP‑alive** (GC/OOM thrash, host network black‑hole — kernel still ACKs, app never reads), the server‑side HTTP/2 write window fills and `stream.Send` blocks in `writeQuota.get` — bounded only by the connection lifetime (gRPC default server keepalive is **2 hours**, and no `KeepaliveParams` is configured), **not** the 30 s dispatch timeout. Worse, the keep‑alive eviction loop sends over the **same `sendMu`**, so it self‑blocks and **never evicts the dead member**. Every dispatcher then piles onto `sendMu`, each holding an open DB transaction. The transient stall becomes a **durable pool‑exhaustion outage from one frozen node**, verified against grpc‑go internals (`flowcontrol.go`, `defaults.go: defaultServerKeepaliveTime = 2h`).

### 2.4 Amplifiers (confirmed)

* **No server‑side cap on the dispatch timeout.** `responseTimeoutMs` is read straight from the (tenant‑authored) workflow/criteria config with only a `<= 0 → 30000` default and **no maximum** (`dispatch.go:110-115, 260-265`). One workflow definition can pin shared connections for **hours**. *(`grpc-compute-3`, `tx-client-tunable-dispatch-timeout-5` — medium.)*
* **Cluster forwarding inherits the hold.** When the local node has no compute member for a tag, it forwards to a peer over HTTP (5 s find + 30 s forward) **while still holding its own pgx.Tx** — so a peer’s slow compute node exhausts the *origin’s* pool too. *(`cluster-coordination-2` — confirmed High; cluster‑mode only.)*

### 2.5 Remediation (kills most of §2 at once)

* **Document the connection‑hold contract and the ring‑fencing facility** (per the *Maintainer disposition* above): `SYNC`/`ASYNC_SAME_TX` hold a pooled connection across the gRPC dispatch, while `COMMIT_BEFORE_DISPATCH`/`ASYNC_NEW_TX` let the application avoid that. Choosing the right mode for side‑effecting/long call‑outs is the application's responsibility; this hold is postgres‑engine‑specific (the Cassandra plugin is unaffected).
* Set **`statement_timeout` and `idle_in_transaction_session_timeout`** on the pool connection (RuntimeParams) and a **pool `AcquireTimeout`** so a stuck node can’t pin connections and `Begin` fails fast with 503.
* Configure **`grpc.KeepaliveParams`/`EnforcementPolicy`** and make `member.Send` deadline‑bounded (bounded per‑member queue, drop+evict) so a frozen consumer can’t hold `sendMu`.
* Add **HTTP server timeouts + a per‑request context deadline** so a request can’t block forever on `Acquire`.
* Export `pgxpool.Stat()` to metrics — pool saturation is currently **invisible** (no acquire‑wait/empty‑pool metric), so this outage has no alarm.

---

## 3. Orphaned transactions leak forever — the documented backstop does not exist (Findings `cross-cutting-2/3`, `api-boundary-tx-orphan-panic-1` — confirmed High)

§2 self‑heals because the dispatch times out and the handler rolls back. **There is one path where it does not heal: a panic between `Begin` and `Commit`.** Three independent verifiers confirmed this with overlapping evidence.

* **Rollback is explicit, never deferred.** Every `h.txMgr.Rollback` in `internal/domain/entity/service.go` sits inside an `if err != nil` branch (lines 259, 286, 290, 568, …); a repo‑wide search found **zero `defer …Rollback`** at the service/handler layer (the only deferred rollback in the codebase is a self‑contained local tx in `model_store.go:361`).
* **The recovery middleware does not roll back.** It writes a 500 and flips a health flag, but has no knowledge of the open transaction:
  ```go
  // internal/api/middleware/recovery.go:17-24
  if rec := recover(); rec != nil { ... healthFlag.Store(false); common.WriteError(w, r, appErr) }
  ```
* **The connection is released only by `Commit`/`Rollback`.** A panicked‑past `pgx.Tx` is never returned to the pool; `MaxConnLifetime`/`MaxConnIdleTime` do **not** reclaim a checked‑out connection.
* **The reaper that is supposed to catch this is dead code.** `app.go` wires `lifecycle.Manager` + a 10 s `ReapExpired` ticker and even comments that it will “roll back the underlying transaction … otherwise the plugin’s physical handle is orphaned.” But **nothing in production ever calls `Manager.Register`**, so its `active` map is always empty — confirmed by the repo’s *own* comment:
  ```go
  // e2e/parity/multinode/cbd_tx_pinning.go:54-57
  // internal/cluster/lifecycle.Manager is designed for exactly this but is
  // not yet wired into the runtime — its Register/IsAlive surface ...
  ```
  And even if a tx *were* registered, the reaper calls `tm.Rollback(context.Background(), txID)`, which the `#199` tenant gate **rejects** because a background context carries no `UserContext`:
  ```go
  // plugins/postgres/transaction_manager.go:425-430
  uc := spi.GetUserContext(ctx)
  if uc == nil || uc.Tenant.ID != txTenantID { return ...ErrTxTenantMismatch... }
  ```
  (It also only starts in cluster mode.)
* **The engine’s comment relies on a backstop that isn’t configured.** `engine_processors.go:144` literally says a leaked segment “would leak the connection until postgres’ idle‑in‑transaction timeout reclaimed it” — but that timeout is **never set** (confirmed; grep across plugin + migrations is empty).

**Net:** a recurring panic path (a nil‑deref deep in the engine, a bad response decode, an edge‑case input) hit by retrying clients **leaks one of 25 connections per occurrence, permanently**, until the process restarts — and orphaned row locks can block other writers meanwhile. (Several reviewers over‑claimed this for the *slow‑node* case; the verifiers correctly narrowed the **permanent** leak to the **panic** trigger, since the slow‑node case self‑heals at 30 s — see the downgraded `tx-no-timeout-or-reaper-2`, `resource-exhaustion-4`, `cluster-coordination-1`.)

> **A latent twist (`api-boundary-health-1`, partial→Low):** that same recovery path latches `healthFlag` to `false` **forever** (only set `true` once at startup; a test even asserts “no auto‑recovery”). On `/health` this is permanent‑DOWN after one panic. It’s rated Low only because the shipped Helm/Docker probes target `/livez` and `/readyz` (which ignore the flag), not `/health` — so the self‑takedown does **not** fire in the default deployment. It will bite anyone who points an LB at `/health`.

**Fix:** wrap every `Begin` in a `defer` that rolls back unless committed (idempotent guard), **and** set `idle_in_transaction_session_timeout`/`statement_timeout` as a database‑level backstop. Then either wire the reaper for real (with a privileged system‑tenant rollback path) or delete it so it stops implying protection it doesn’t provide.

---

## 4. Other confirmed High‑severity findings

### 4.1 gRPC has no panic recovery → a handler/goroutine panic crashes the whole node (`concurrency-fatal-2`, confirmed High)

The HTTP stack installs `middleware.Recovery`; the **gRPC server registers only auth interceptors**:
```go
// internal/grpc/server.go:53-57 — UnaryAuthInterceptor + StreamAuthInterceptor, no recovery
```
grpc‑go does **not** recover handler‑goroutine panics (`server.go` spawns `go f() → handleStream` with no recover). The async‑search goroutine, the keep‑alive loop, and the stream receive loop also have **no `recover`** (the only `recover` in `internal/grpc` is the `onChange` callback at `members.go:219`). So any panic on a gRPC path or in those background goroutines **terminates the whole OS process** — taking down the HTTP API for every tenant. This is *asymmetric*: the same panic on the HTTP path would have been a contained 500. *(`api-boundary-grpc-recovery-1` partially‑confirms the structural gap but notes the specific compute‑message handlers are defensively coded, so there’s no demonstrated reachable trigger today — this is a missing defense‑in‑depth guard, not a live crash.)*

### 4.2 Concurrent `stream.Send` on the compute‑member stream — data race / framing corruption (`concurrency-fatal-1`, confirmed High)

`member.Send` serialises writes via `sendMu`, but the stream **receive loop answers keep‑alive pings by calling the raw `stream.Send` closure directly, bypassing the mutex**:
```go
// internal/grpc/streaming.go:95-97  sendFn := func(ce) error { return stream.Send(ce) }
// internal/grpc/streaming.go:170-175 (receive loop) if err := stream.Send(kaCE); ...  // ← no sendMu
```
That bare send races the keep‑alive *ticker* goroutine (`member.Send` at `streaming.go:232`) and every dispatcher (`member.Send` at `dispatch.go:105`) — **three goroutines writing one bidi `ServerStream`**, with only two serialised. grpc‑go (`stream.go:1613`) explicitly documents concurrent `SendMsg` as **not safe**: it can corrupt HTTP/2 framing or panic the transport writer — and, with no recovery interceptor (§4.1), a panic there is a whole‑node crash. **One connected compute member under steady traffic is enough to expose it.** **Fix:** route the keep‑alive response through `member.Send`.

### 4.3 List/`GetAll` materialises the entire model into heap (`resource-exhaustion-1`, confirmed High)

`GET /entity/{name}/{version}` calls `EntityStore.GetAll`, which runs an **unbounded `SELECT`** (no `LIMIT`), `scanEntities` collects **every** row into a slice, and pagination is applied **in Go, after the full load**:
```go
// plugins/postgres/entity_store.go:216  SELECT doc FROM entities WHERE tenant_id=$1 AND model_name=$2 AND model_version=$3 AND NOT deleted   (no LIMIT)
// internal/domain/entity/service.go:763  entities, err := entityStore.GetAll(ctx, ref); ... sort.Slice(...); start := ...  (slice after load)
```
`pagination.ValidateOffset` caps `pageSize` at 10000 but runs **before** the load and only trims the response — it never reaches the SQL. So memory is **O(all entities of the model) per request regardless of `pageSize`**, and the client cannot protect the server. At moderate data volume (tens/hundreds of thousands of entities) one list call — multiplied by concurrency — drives GC thrash / **OOM kill**, taking down all tenants. **Fix:** push `LIMIT/OFFSET/ORDER BY` into the SQL.

### 4.4 Unbounded search materialisation + unbounded detached goroutines (`resource-exhaustion-2/3`, `grpc-compute-7`, confirmed High)

* **Sync search with no `limit`** is fully unbounded on Postgres. The `limit` query param is optional; when omitted, `opts.Limit` stays 0, the `Searcher` pushdown path emits a `LIMIT` clause **only when `Limit>0`**, and the 1000‑row default that protects the in‑memory fallback is **not** applied on the Postgres path. Omitting `limit` even bypasses the 10000 `MaxPageSize` ceiling (only checked inside `if params.Limit != nil`). Result: a broad search streams **every matching document into heap**. (`search/handler.go:130-144`, `service.go:121-131,168-175`, `searcher.go:54-82`.)
* **Async search** (`POST` async, or gRPC `EntitySnapshotSearchRequest`) runs with **no limit** and spawns a **detached `go func()` on `context.Background()` per request — no worker pool, no semaphore, no timeout, no mid‑flight cancellation** (the `CANCELLED` check happens only *after* the search returns):
  ```go
  // internal/domain/search/service.go:245-251
  bgCtx := spi.WithUserContext(context.Background(), uc)
  go func() { ... results, searchErr := s.Search(bgCtx, modelRef, cond, opts) ... }()
  ```
  Postgres is explicitly **not** a `SelfExecutingSearchStore` (SPI docs confirm), so this detached path is live. A cheap authenticated request spawns expensive, uncancellable work competing for the same 25‑connection pool; abandoned searches run to completion. *(`tx-unbounded-async-search-goroutine-6`, `concurrency-fatal-3`, `cross-cutting-4` are the same root cause at medium severity, adding: results are **lost** and jobs stuck `RUNNING` until the 1 h TTL reap if a search is in flight at shutdown, because `pool.Close()` does not wait for it.)*

**Fix:** apply the default cap (and `MaxPageSize`) before delegating to the `Searcher`; run async searches through a **bounded worker pool with a cancellable, deadline‑bounded context** tied to job‑cancel and shutdown.

### 4.5 Schema‑extension savepoint can silently drop a concurrent delta (`schema-model-migration-1`, confirmed High — the one genuine corruption path)

In auto‑evolving schema mode, additive schema changes are appended to a `model_schema_extensions` log inside the **ambient entity transaction**, and folded on read; every 64 deltas the plugin writes a `savepoint` row. **There is no concurrency protection on this log** — first‑committer‑wins validates only the `entities` read‑set (`txstate.go` keys are entity‑IDs; schema rows are never tracked), and there is no advisory lock / `SELECT … FOR UPDATE` / per‑model mutex:
```go
// plugins/postgres/model_store.go:335-338  "Under REPEATABLE READ there is no schema-write conflict surface: concurrent writers both succeed..."
// plugins/postgres/model_store.go:431-445  if newSeq-lastSP >= SchemaSavepointInterval { ... fold ... write savepoint }
```
When the every‑64 savepoint fires inside a transaction whose `REPEATABLE READ` snapshot does **not yet see** a concurrently‑inserted *lower‑seq* delta, the savepoint payload is folded **without it**; future folds only replay `seq > savepointSeq`, so the missed delta is **excluded forever**. Per `(tenant, model)`, the folded schema **permanently loses an additive field**, and strict‑validation paths then reject otherwise‑valid documents containing that field. **Trigger:** an unlocked, auto‑evolving model under concurrent entity writes (different entities — no FCW conflict) that each introduce a new field, crossing a 64‑delta boundary. **Fix:** serialise `ExtendSchema` per `(tenant,model)` with `SELECT … FOR UPDATE` on the `models` row (or include the extension log in the FCW read‑set).

---

## 5. Confirmed Medium findings (real, bounded, often opt‑in)

* **External side effects fire before commit‑time validation (`ssi-correctness-5`, Medium, default‑reachable).** `SYNC`/`ASYNC_SAME_TX` processors dispatch to the compute node **before** the commit‑time `FOR SHARE` read‑set check. A pure read‑set conflict (read X, write Y, X changed concurrently) is detected only at commit → the DB rolls back **after the external side effect already happened**, and a client that honours the retryable‑409 re‑runs the workflow, **double‑firing non‑idempotent effects** (double charge/send). This is an inherent property of doing non‑transactional external work inside a DB transaction. **Fix:** steer side‑effecting work to `COMMIT_BEFORE_DISPATCH`, or validate/lock the read‑set before dispatch.
* **Auto‑migrate can stall every node’s startup (`schema-model-migration-4`, Medium, concrete today).** `CYODA_POSTGRES_AUTO_MIGRATE` defaults `true` and runs on **every** node boot; golang‑migrate holds a session `pg_advisory_lock` that **waits indefinitely** (`context.Background`), and the migration connection sets **no `lock_timeout`/`statement_timeout`**. Migration `000002` already ships a **non‑`CONCURRENT` `CREATE INDEX` on `entities`** — which needs a `SHARE` lock that conflicts with any in‑flight write transaction. During a rolling upgrade, an old node holding a long write tx can block the migrating node’s DDL **forever**, and every other booting node queues behind the advisory lock → **cluster‑wide startup stall** with no timeout to break it. **Fix:** set `lock_timeout`/`statement_timeout` on the migration connection; gate auto‑migrate to one designated node.
* **Random peer selection, no failover (`cluster-coordination-3`, Medium, cluster‑only).** `RandomSelector` picks one tag‑matching peer; on error or `Success==false` it returns the failure with **no retry against other healthy peers**, so ~1/N of cross‑node transitions fail when one peer is degraded.
* **No cap on `responseTimeoutMs` (`grpc-compute-3`) and the dead reaper (`cluster-coordination-1`)** — covered in §2.4 and §3.

---

## 6. Tenant isolation — sound by default, with latent traps (all Low)

This deserved its own scrutiny (Gate 3). The verdict is reassuring **for the default configuration** and a caution for hardening:

* **RLS is inert by default (`tenant-isolation-3`).** Row‑Level Security is `ENABLE`d but **not `FORCE`d**, and the app connects as the **table owner**, who bypasses policies. The `set_config('app.current_tenant', …)` in `Begin` is effectively a no‑op for isolation. **So the *sole* live isolation mechanism is application‑level `WHERE tenant_id = $1`.** The audit checked every query in the plugin and **confirmed they all carry the tenant predicate, bound from a server‑derived `s.tenantID` (never user input)** — so there is **no live cross‑tenant leak today**. It is a defense‑in‑depth gap: one future query that forgets the predicate would leak with no backstop, and `TestRLS_PoliciesExist` gives false assurance.
* **The documented RLS hardening is a landmine (`tenant-isolation-2`).** The pool (non‑tx) path **never sets `app.current_tenant`**, and async search runs entirely on the pool. If an operator follows the in‑repo guidance to “connect as a non‑owner role” or adds `FORCE ROW LEVEL SECURITY`, `current_setting('app.current_tenant', true)` is `NULL` on every pooled connection → policies deny all rows → **non‑tx reads silently return empty and async search breaks**. The repo itself marks this mode “deferred to Plan 5”.
* **`GetSubmitTime` is not tenant‑gated (`tenant-isolation-1`).** It’s the one txID‑keyed `TransactionManager` method that ignores its context, reachable via `GET /entity/{id}/transitions?transactionId=…`. It yields a **cross‑tenant existence/timing oracle** for committed txIDs (no data content; bounded by the 1 h TTL and time‑UUID guessing). (Note: the `memory`/`sqlite` plugins and the SPI interface share this gap — it’s a contract‑level omission.)
* **`search_job_results` PK omits `tenant_id` (`tenant-isolation-4`).** A cross‑tenant `(job_id, seq)` collision is *mechanically* possible but **not reachable in production**, which mints unique server‑side time‑UUIDs per job.

**Fix priority:** tenant‑gate `GetSubmitTime`; set `app.current_tenant` on the pool path (a `pgxpool` `AfterConnect`/`BeforeAcquire` hook) so RLS can actually be turned on as a real backstop; then make `FORCE` + non‑owner a supported, tested mode.

---

## 7. What is **sound** (verified — to keep the picture honest)

You asked specifically not to assume. These were investigated and either **refuted** or shown **correct/unreachable**:

* **Snapshot‑isolation / first‑committer‑wins is correct.** The commit‑time `FOR SHARE` read‑set lock genuinely enforces first‑committer‑wins (raising `40001` on a concurrent committed change), and write‑write conflicts surface via tuple locks. The application‑layer version comparison (`ValidateReadSet`) is *redundant* under `REPEATABLE READ` rather than wrong (`ssi-correctness-1`, Low) — a latent code‑quality concern, not a live bug, and it is unit‑tested.
* **The “whole‑model lock storm on `DeleteAll`” hypothesis is refuted (`ssi-correctness-3`).** `GetAll` records reads, but `DeleteAll` then **writes every one of them**, and `RecordWrite` *promotes* each entry from read‑set to write‑set; by commit the read‑set is empty, so `validateInChunks` is skipped entirely (`len(readIDs)==0`). No whole‑model `FOR SHARE` lock occurs.
* **Phantom / write‑skew (`ssi-correctness-2`, Low).** `REPEATABLE READ` + read‑set‑only validation genuinely doesn’t prevent phantoms — but **no core platform path** builds a `Count`/`Exists`‑based invariant inside a transaction, so the “silent business‑invariant violation” is reachable only by *application‑defined* logic, not by the engine.
* **The OpMu “concurrent map writes” fatal does not apply to Postgres (`concurrency-fatal-4`, Low).** Postgres builds a `TransactionState` carrying only `ID`/`TenantID`; the SPI maps that trigger the runtime fatal are nil/unused, and its real bookkeeping is mutex‑guarded. No core path drives two concurrent ops on one txID.
* **Schema fold‑on‑read SPOF and the “delete the extension log” data‑loss hypotheses (`schema-model-migration-2/3`) were narrowed to Low** — the `applyFunc` is provably always wired on the concrete factory, and state‑machine guards (LOCKED/UNLOCKED mutual exclusion, the `ErrCodeModelHasEntities` count check) are the real, production‑live defenses, not the dead dev assertion.
* **`checkSchemaCompat` refusing to start on a newer schema (`schema-model-migration-5`, Low)** is an *intentional* anti‑corruption fail‑safe, and the shipped migrations are additive, so running old nodes are unaffected.
* **Multi‑node:** the proxy tx‑affinity token is never minted (`cluster-coordination-5`) — inert dead code, but transactions are request‑scoped so there’s no present hazard; model‑cache staleness (`cluster-coordination-4`) survives single‑packet loss (memberlist retransmits ~4·log(N) times) and is bounded by a 5 min lease; the shared‑secret rotation issue (`cluster-coordination-6`) fails **loudly** (`os.Exit`), not as silent split‑brain.

---

## 8. Prioritised remediation

The findings cluster, so a small number of changes neutralise most of the risk. In order of impact‑per‑effort:

1. **Document the connection‑hold contract (don't change the default).** Make explicit that `SYNC`/`ASYNC_SAME_TX` hold a pooled connection across external dispatch and that `COMMIT_BEFORE_DISPATCH`/`ASYNC_NEW_TX` are the application's tools to ring‑fence side‑effecting/long callouts (see *Maintainer disposition*). Engine‑specific to postgres. *(Addresses the by‑design part of §2, §3’s leak window, and `ssi-correctness-5` through guidance; #354 covers the panic/fault backstop.)*
2. **Add database‑level safety valves:** `statement_timeout`, `idle_in_transaction_session_timeout`, `lock_timeout` (incl. on the migration connection), pool `AcquireTimeout`, and a `MaxConnLifetime`. *(Backstops §2, §3, §5‑migrate.)*
3. **Add a `defer`‑based rollback guard** around every `Begin` (idempotent), independent of the recovery middleware. *(Closes §3’s permanent leak.)*
4. **Add a gRPC panic‑recovery interceptor** (unary+stream) and wrap long‑lived background goroutines in `recover`; **route keep‑alive responses through `member.Send`.** *(Closes §4.1, §4.2.)*
5. **Bound all result sets:** push `LIMIT/OFFSET` into list/search SQL; apply the default + max cap on the `Searcher` path; run async search through a bounded, cancellable worker pool. *(Closes §4.3, §4.4.)*
6. **Serialise `ExtendSchema` per `(tenant,model)`** so the savepoint can’t fold over a missing concurrent delta. *(Closes §4.5 — the one corruption path.)*
7. **Operability:** HTTP server timeouts + per‑request deadline; export `pgxpool.Stat()` to metrics; tenant‑gate `GetSubmitTime`; decide whether the `lifecycle.Manager` reaper should be wired or deleted (today it implies protection it doesn’t give).
8. **Raise the default pool size** (`MaxConns=25` is low for a connection‑per‑transaction model) — but this only widens the window; (1)–(2) are the real fixes.

---

## 9. Residual picture **if transaction‑duration limits are enforced** (pool‑starvation remediated)

> *This chapter assesses the failure modes under the assumption that the missing backstops are connected and a hard ceiling on transaction lifetime is enforced. It first verifies the premise — that those backstops are already present in the tree but unwired — then re‑scores the findings under the assumption, separating what the fix genuinely eliminates from what it leaves untouched (and what it newly aggravates).*

### 9.1 The premise is correct: the backstops exist, the dots aren’t connected

The machinery for bounding transaction lifetime is **already in the codebase and is explicitly designed for this purpose** — it is simply not wired into the runtime:

* The config surface exists: `CYODA_TX_TTL` (default **60s**), `CYODA_TX_REAP_INTERVAL` (10s), `CYODA_TX_OUTCOME_TTL` (5m) are all defined (`app/config.go:243-246`).
* `lifecycle.Manager` is constructed and bound to the TransactionManager **unconditionally**, with a comment stating the exact intent:
  ```go
  // app/app.go:418-422
  a.txLifecycle = lifecycle.NewManager(cfg.Cluster.OutcomeTTL)
  // Wire the TM so the TTL reaper can roll back the underlying transaction
  // when a cluster-level timeout fires; otherwise the plugin's physical
  // handle is orphaned until the database's own idle timeout catches it.
  a.txLifecycle.SetTransactionManager(a.transactionManager)
  ```
  The reaper itself (`Manager.ReapExpired`) already rolls back every TTL‑expired tx via `tm.Rollback`.
* The database‑side ceiling is one connection parameter away: the DSN flows straight into `pgxpool.ParseConfig` (`config.go:131`), so `statement_timeout` / `idle_in_transaction_session_timeout` / `lock_timeout` are settable as session `RuntimeParams` (via the DSN or a pool `AfterConnect` hook) **without restructuring the pool**.

Only **three small dots** are unconnected (confirmed in §3): (a) nothing ever calls `Manager.Register`/`RecordOutcome`, so the reaper’s `active` map is always empty; (b) the reaper goroutine only starts `if cfg.Cluster.Enabled` (`app.go:423`), so single‑node — the default — has no reaper at all; (c) the reaper’s `tm.Rollback(context.Background(), …)` is rejected by the `#199` tenant gate (`transaction_manager.go:425`) because a background context carries no `UserContext`. The repo’s own comment concedes the subsystem “is not yet wired into the runtime” (`e2e/parity/multinode/cbd_tx_pinning.go:54`).

**So the assertion stands:** the remediation is genuinely small — wire `Register`/`RecordOutcome` into `Begin`/close, start the reaper unconditionally, give it a privileged system‑tenant rollback path, set the DB‑side timeouts as connection params, and add a `defer`‑rollback guard. The analysis below assumes exactly that.

### 9.2 The remediation assumed (made precise)

“Pool starvation remediated” is taken to mean all four of these — each realisable from existing building blocks:

| | Remediation | Built from |
|---|---|---|
| **R1** | Register every tx with `lifecycle.Manager` on `Begin` (TTL = `CYODA_TX_TTL`), `RecordOutcome` on close; run the reaper **unconditionally**; give it a privileged rollback context. | `lifecycle.Manager` (already wired to the TM) |
| **R2** | Set `statement_timeout` + `idle_in_transaction_session_timeout` (+ `lock_timeout` on the **migration** connection) as the authoritative DB‑side ceiling the application cannot dodge. | DSN → `pgxpool.ParseConfig` `RuntimeParams` |
| **R3** | `defer`‑based rollback guard around every `Begin` (idempotent) so a panic cannot orphan a tx. | trivial |
| **R4** | Pool `AcquireTimeout` + per‑request context deadline → a saturated pool fails fast with 503 instead of blocking unbounded. | `pgxpool` config + middleware |

### 9.3 What this **eliminates** (the genuine win)

**The entire orphaned‑transaction leak class collapses.** R1+R2+R3 mean no `pgx.Tx` can outlive the TTL (or the DB timeout), and a panic rolls back. These findings are **neutralised**:

* `api-boundary-tx-orphan-panic-1`, `cross-cutting-2`, `cross-cutting-3` (confirmed High) — the permanent panic‑leak path is closed by R3, backstopped by R1/R2.
* `cluster-coordination-1`, `resource-exhaustion-4`, `tx-no-timeout-or-reaper-2` (the dead‑reaper / no‑DB‑timeout findings) — directly resolved by R1+R2.

**The pool‑starvation class is bounded and made recoverable** (it is *contained*, not removed):

* `tx-pool-exhaustion-1`, `grpc-compute-1`, `cross-cutting-1`, `cluster-coordination-2`, `tx-no-request-deadline-3`, `tx-client-tunable-dispatch-timeout-5` / `grpc-compute-3` — downgrade from **“node‑wide stall for all tenants, recovers only when the bad compute node does”** to **“bounded backlog with fast 503s and a hard cap on connection‑hold time, self‑healing.”** A slow/dead compute node now degrades a *fraction* of capacity for at most the TTL, instead of wedging the whole data plane.

**One migration hazard is resolved *if* R2 is scoped correctly:** `schema-model-migration-4` (auto‑migrate advisory‑lock stall) is neutralised **only if** R2’s `lock_timeout`/`statement_timeout` is applied to the **migration connection** (`migrate.go`), which is separate from the app pool. If R2 covers only app connections, the startup‑stall survives — a scope detail worth being explicit about.

This is a large improvement: it removes the only *permanent* (restart‑required) failure path and converts the dominant availability risk from “indefinite outage” to “graceful, bounded degradation.”

### 9.4 What it does **not** fix — and what becomes the new top risk

A transaction‑duration limit bounds *time spent holding a database connection*. Three confirmed‑High failure classes are **orthogonal to that** and survive completely:

**(a) OOM via unbounded materialisation — now the #1 residual availability risk.**
`resource-exhaustion-1/2/3` and `grpc-compute-7` are untouched, for a structural reason verified in code:

* **List/`GetAll` does not run in a transaction at all.** `ListEntities` calls `EntityStore.GetAll(ctx, …)` directly on the request context — there is no `Begin` (`internal/domain/entity/service.go:753-765`). It executes on the **pool**, so a transaction‑duration limit is simply inapplicable.
* **Synchronous search is pool‑bound too.** The pushdown path requires `tx == nil` (`search/service.go:121`); the in‑transaction fallback is the *even hungrier* `GetAll`‑then‑filter‑in‑Go path. Either way, memory is unbounded.
* **Async search runs on a detached `context.Background()` goroutine** (`search/service.go:247`), so a per‑request/per‑tx deadline (R4) never reaches it.

Critically, **`statement_timeout` bounds query *time*, not query *memory*.** A query that returns two million rows *quickly* still materialises two million documents into heap before any timeout would fire. So OOM remains fully live — and because the starvation path is now contained, **OOM becomes the dominant way the node still goes down at moderate data volume.** Fixing pool starvation *promotes* this finding to the top of the list. It needs its own fix (push `LIMIT`/`OFFSET` into SQL; cap the `Searcher` path; bound async search with a worker pool) — transaction limits do nothing for it.

**(b) Process‑fatal conditions — untouched, and relatively more prominent.**
`concurrency-fatal-2` (no gRPC panic‑recovery interceptor → a handler/goroutine panic crashes the whole process) and `concurrency-fatal-1` (concurrent `stream.Send` race) cannot be prevented by any TTL or timeout. If anything, R2 makes the missing gRPC recover *more* salient: `statement_timeout` introduces new mid‑handler error returns (`57014 query_canceled`), widening the surface of code that must not panic. After remediation, a single unrecovered gRPC panic remains a whole‑node, all‑tenant crash.

**(c) The consistency findings — untouched, and one is aggravated (see §9.5).**
`schema-model-migration-1` (concurrent schema‑fold drops a delta → silent corruption) is a bug in the extension log, unrelated to duration. `ssi-correctness-5` (external side‑effect fires before commit) is not only unaffected — it gains a **new trigger** from the remediation itself.

**Partial improvement worth noting — `grpc-compute-2` (frozen‑node `stream.Send` wedge):** R1/R2 abort the *transaction* and free its *connection*, so the pool‑exhaustion aspect is removed. But the goroutine wedged in `member.Send` is blocked on a full HTTP/2 write window — on the *gRPC stream*, not the DB — and still holds `sendMu`, so the member is still never evicted and dispatches to it still wedge at the application layer (now without consuming DB connections). Transaction limits convert this from “node‑wide DB outage” to “that member’s workflows fail until the TCP connection dies.” The gRPC keepalive/send‑deadline fix is still required.

### 9.5 The new tension the limit introduces (the non‑obvious cost)

Enforcing a transaction‑duration ceiling creates a hard constraint: **the maximum legitimate workflow‑transaction duration must be shorter than the TTL.** But the architecture deliberately runs synchronous external compute call‑outs *inside* the transaction, each bounded by `responseTimeoutMs` — default **30s, uncapped, and stackable** across multiple processors in one transition (§2.4). These two facts collide:

* **TTL longer than the slowest legitimate workflow** → orphan/starvation protection is weak (a stuck tx can still hold a connection for, say, 60s+).
* **TTL shorter than the slowest legitimate workflow** → the reaper / `statement_timeout` / `idle_in_transaction_session_timeout` will **abort live, correct transactions mid‑flight** (e.g. a transition with two 30s SYNC processors against the default 60s TTL). The client sees a spurious failure on a workflow that was doing nothing wrong.

And because external side‑effects fire **before** commit (`ssi-correctness-5`), a timeout‑driven abort lands **after** the external system was already mutated — so the compute node did its work, but the entity is rolled back and never reflects it. **Enforcing the limit therefore turns a slow‑but‑correct workflow into a silent data‑divergence event**, adding a timeout trigger to a finding that previously fired only on concurrency conflicts. A transaction‑duration limit, applied to the system *as it is currently structured*, is double‑edged.

The tension is **unresolvable while external dispatch happens inside the transaction** — and it dissolves the moment it doesn’t. If external call‑outs are moved outside the transaction (the `COMMIT_BEFORE_DISPATCH` recommendation from §2.5/§8), then no transaction ever waits on external work, a **tight** TTL (5–10s) becomes safe, and the timeout‑trigger for `ssi-correctness-5` disappears. So the correct framing is:

> **Transaction‑duration limits and commit‑before‑dispatch are complementary, not alternatives.** A TTL *alone* is a partial, double‑edged fix (it bounds the connection hold but can abort legitimate long workflows and amplify side‑effect divergence). A TTL *plus* committing before external dispatch is the clean one — and only the pair lets you choose a TTL tight enough to actually protect a 25‑connection pool.

### 9.6 Revised risk ranking under the assumption

| Risk | Before remediation | After R1–R4 (tx‑duration limits only) |
|---|---|---|
| Connection‑per‑tx starvation from a slow/dead compute node | **#1 — node‑wide stall, all tenants** | **Contained** — bounded backlog, fast 503s, self‑healing |
| Permanent orphaned‑tx leak (panic path) | High — restart required | **Eliminated** |
| Unbounded list/search materialisation (OOM) | High | **#1 — unchanged; now the dominant outage path** |
| gRPC panic crash + concurrent‑`Send` race | High | **#2 — unchanged; whole‑node crash** |
| Concurrent schema‑fold delta drop (corruption) | High | **Unchanged** |
| Long synchronous workflow vs. TTL | (n/a) | **New — force‑aborts legitimate txs + amplifies side‑effect divergence** unless paired with commit‑before‑dispatch |

**Net.** Wiring the transaction‑duration backstops that already exist is the right move and removes the scariest *permanent* failure path while converting the dominant *availability* risk from “indefinite, restart‑only outage” into “bounded, recoverable degradation.” But on its own it neither prevents the node from being **OOM‑ed by one unbounded list/search**, nor from being **crashed by one unrecovered gRPC panic**, nor from the **schema‑fold corruption** — and it adds a genuine tension with long synchronous workflows. To actually move the system from *“can become unavailable at moderate volume”* to *“degrades gracefully,”* the transaction‑duration limit must be accompanied by **(i) committing before external dispatch** (which both makes a tight TTL safe and removes the new tension), **(ii) bounding all result sets**, and **(iii) the gRPC panic‑recovery interceptor + `sendMu` fix**. Those four together — not the TTL alone — are what close the picture.

---

## Appendix — full finding ledger

All 54 candidate findings with their adversarial verdict and **verified** (post‑refutation) severity. “Finder sev” is the reviewer’s initial guess; “Verified sev” is what survived scrutiny. Confirmed findings are operationally real and reachable; partial findings are real mechanisms with overstated severity, narrower triggers, or opt‑in/cluster‑only/latent reachability; the single refuted finding is included for completeness.

<!-- table generated from the verified-findings dataset; sorted by verdict then verified severity -->

| ID | Class | Finder sev | **Verified sev** | Verdict | Title |
|---|---|---|---|---|---|
| `api-boundary-tx-orphan-panic-1` | resource-exhaustion | high | **high** | ✅ confirmed | Panic between Begin and Commit orphans a pooled pgx.Tx (no defer-rollback; recovery does not roll back) |
| `cluster-coordination-2` | resource-exhaustion | high | **high** | ✅ confirmed | Synchronous cross-node dispatch holds the origin's DB transaction for the full forward timeout |
| `concurrency-fatal-1` | uncontrolled-breakdown | high | **high** | ✅ confirmed | Concurrent stream.Send on the compute-member gRPC stream (keep-alive response bypasses sendMu) |
| `concurrency-fatal-2` | uncontrolled-breakdown | high | **high** | ✅ confirmed | gRPC server has no panic-recovery interceptor; a handler/goroutine panic crashes the whole node |
| `cross-cutting-2` | resource-exhaustion | high | **high** | ✅ confirmed | Cluster orphaned-tx reaper is non-functional: nothing registers txs; its Rollback is tenant-gated out |
| `cross-cutting-3` | resource-exhaustion | high | **high** | ✅ confirmed | Panic between Begin and Commit leaks the pooled connection (no defer-rollback; reaper dead) |
| `grpc-compute-1` | unavailability | critical | **high** | ✅ confirmed | SYNC/ASYNC_SAME_TX dispatch holds a pooled DB connection across the external gRPC round-trip |
| `grpc-compute-2` | unavailability | critical | **high** | ✅ confirmed | A black-holed compute node makes stream.Send block past the timeout; the member is never evicted |
| `grpc-compute-7` | resource-exhaustion | high | **high** | ✅ confirmed | Async snapshot search (via gRPC) spawns an unbounded detached goroutine with no timeout/cancellation |
| `resource-exhaustion-1` | resource-exhaustion | high | **high** | ✅ confirmed | List/GetAll materialises the entire model in memory regardless of page size → OOM |
| `resource-exhaustion-2` | resource-exhaustion | high | **high** | ✅ confirmed | Synchronous search with no limit is fully unbounded on postgres (bypasses both caps) |
| `resource-exhaustion-3` | resource-exhaustion | high | **high** | ✅ confirmed | Async search applies no limit and spawns an unbounded detached goroutine per submission |
| `schema-model-migration-1` | inconsistency | high | **high** | ✅ confirmed | Periodic savepoint can permanently drop a concurrent lower-seq delta → silent schema corruption |
| `tx-no-request-deadline-3` | unavailability | high | **high** | ✅ confirmed | No HTTP server timeout / per-request deadline; a blocked Begin/Acquire waits unbounded, no pool metric |
| `tx-pool-exhaustion-1` | unavailability | critical | **high** | ✅ confirmed | A pooled pgx.Tx held across external gRPC dispatch lets one slow node drain the 25-conn pool |
| `cluster-coordination-1` | resource-exhaustion | high | **medium** | ✅ confirmed | Cluster orphaned-tx reaper is inert — transactions are never registered |
| `cluster-coordination-3` | unavailability | medium | **medium** | ✅ confirmed | Cross-node dispatch picks one random peer with no health awareness and no failover |
| `concurrency-fatal-3` | resource-exhaustion | medium | **medium** | ✅ confirmed | Unbounded, detached async-search goroutine with no recover and no concurrency limit |
| `cross-cutting-4` | data-loss | medium | **medium** | ✅ confirmed | Async-search goroutines unbounded/detached/not awaited at shutdown → lost results, stuck-RUNNING jobs |
| `grpc-compute-3` | resource-exhaustion | high | **medium** | ✅ confirmed | No upper bound/validation on responseTimeoutMs lets a workflow pin a DB connection arbitrarily long |
| `schema-model-migration-4` | unavailability | medium | **medium** | ✅ confirmed | Auto-migrate on every node + indefinite advisory lock + no lock/statement timeout can stall startups |
| `ssi-correctness-5` | inconsistency | medium | **medium** | ✅ confirmed | External side-effects execute before commit-time validation; conflict-retry double-fires them |
| `tx-client-tunable-dispatch-timeout-5` | unavailability | medium | **medium** | ✅ confirmed | Per-dispatch timeout is caller/config-controlled with no upper clamp → arbitrarily long conn pin |
| `tx-unbounded-async-search-goroutine-6` | resource-exhaustion | medium | **medium** | ✅ confirmed | Async search spawns an unbounded, uncancellable detached goroutine per request |
| `api-boundary-http-no-timeouts-1` | resource-exhaustion | medium | **low** | ✅ confirmed | HTTP server has no Read/ReadHeader/Write/Idle timeouts (Slowloris/idle-conn hardening gap) |
| `api-boundary-recovery-coverage-2` | inconsistency | low | **low** | ✅ confirmed | Panic recovery inconsistent — directly-mounted routes bypass the Recovery middleware |
| `tenant-isolation-1` | cross-tenant-leak | low | **low** | ✅ confirmed | GetSubmitTime is not tenant-gated — cross-tenant existence/timing oracle for committed txIDs |
| `cross-cutting-1` | unavailability | critical | **high** | 🟡 partial | Held REPEATABLE READ tx spans external dispatch over a 25-conn pool (bounded/self-healing at 30s) |
| `api-boundary-grpc-recovery-1` | uncontrolled-breakdown | high | **medium** | 🟡 partial | No gRPC recovery interceptor (structural gap real; cited compute-message trigger is panic-safe) |
| `api-boundary-cbd-partial-durability-1` | inconsistency | medium | **low** | 🟡 partial | COMMIT_BEFORE_DISPATCH leaves durable intermediate state on failure (opt-in; conflict is non-retryable) |
| `api-boundary-health-1` | unavailability | high | **low** | 🟡 partial | One recovered panic latches /health DOWN forever (but shipped probes use /livez,/readyz) |
| `cluster-coordination-4` | inconsistency | medium | **low** | 🟡 partial | Model-cache invalidation rides best-effort gossip (survives single-packet loss; 5-min lease) |
| `cluster-coordination-5` | inconsistency | low | **low** | 🟡 partial | Tx-routing token never minted in production — proxy affinity primitive is inert dead code |
| `cluster-coordination-6` | unavailability | medium | **low** | 🟡 partial | One shared secret for gossip/AEAD/token; rotation needs full downtime (fails loud, not split-brain) |
| `cluster-coordination-7` | unavailability | low | **low** | 🟡 partial | Dispatch nonce cache fail-closed at 100k (unreachable at moderate volume; per-node, self-clearing) |
| `concurrency-fatal-4` | inconsistency | medium | **low** | 🟡 partial | Postgres doesn't use OpMu (satisfies contract trivially — its TransactionState maps are unused) |
| `cross-cutting-5` | inconsistency | medium | **low** | 🟡 partial | Shutdown abandons in-flight workflows past 10s; pool.Close doesn't wait (SYNC rolls back cleanly) |
| `grpc-compute-4` | resource-exhaustion | medium | **low** | 🟡 partial | Pending-request map entries not removed on timeout/cancel (small leak; reclaimed on disconnect) |
| `grpc-compute-5` | inconsistency | medium | **low** | 🟡 partial | Tag-change propagation goroutine-per-event, version-less overwrite → stale routing under flap |
| `grpc-compute-6` | unavailability | medium | **low** | 🟡 partial | Member-death race in a sub-ms window; self-heals at dispatch timeout (CBD partial-apply is by design) |
| `resource-exhaustion-4` | resource-exhaustion | high | **low** | 🟡 partial | Orphaned-tx map/connection leak (real defects, but headline slow-node/disconnect triggers self-heal) |
| `resource-exhaustion-5` | resource-exhaustion | medium | **low** | 🟡 partial | In-tx GetAll/DeleteAll readSet bloat (readSet drained by write-promotion; second pass skipped) |
| `schema-model-migration-2` | data-loss | high | **low** | 🟡 partial | Dev-only operator-contract guard is dead code (but state-machine guards are the real defense) |
| `schema-model-migration-3` | unavailability | high | **low** | 🟡 partial | Fold-on-read SPOF (applyFunc provably always wired on the concrete factory; pre-persist validates) |
| `schema-model-migration-5` | unavailability | medium | **low** | 🟡 partial | checkSchemaCompat refuses newer schema (intentional anti-corruption guard; shipped migrations additive) |
| `ssi-correctness-1` | inconsistency | medium | **low** | 🟡 partial | ValidateReadSet redundant under REPEATABLE READ (FOR SHARE is the real mechanism; no live bug) |
| `ssi-correctness-2` | inconsistency | high | **low** | 🟡 partial | Phantom/write-skew not prevented (by-design; no core path builds Count/Exists invariants in a tx) |
| `ssi-correctness-4` | inconsistency | medium | **low** | 🟡 partial | Bi-temporal times stamped at tx-start, not commit (only valid-time PIT API; effect bounded) |
| `tenant-isolation-2` | inconsistency | medium | **low** | 🟡 partial | Documented RLS hardening (FORCE/non-owner) silently breaks all non-tx reads + async search |
| `tenant-isolation-3` | cross-tenant-leak | low | **low** | 🟡 partial | RLS inert by default → WHERE tenant_id is sole isolation (no live leak; defense-in-depth gap) |
| `tenant-isolation-4` | unavailability | low | **low** | 🟡 partial | search_job_results PK omits tenant_id (collision unreachable — production uses unique UUIDs) |
| `tx-commit-global-lock-4` | unavailability | medium | **low** | 🟡 partial | Commit scans the whole submitTimes map under tm.mu (sub-ms at moderate volume; conn already released) |
| `tx-no-timeout-or-reaper-2` | unavailability | high | **low** | 🟡 partial | No idle_in_transaction/statement timeout/reaper (real gap; slow-node path self-heals via 30s timeout) |
| `ssi-correctness-3` | unavailability | medium | **none** | ❌ refuted | "Whole-model FOR SHARE lock on DeleteAll" — refuted: readSet→writeSet promotion empties the read-set |

---

### Method note & confidence

This report was produced by a deterministic multi‑agent workflow: 10 dimension reviewers → 53 per‑finding adversarial verifiers → consolidation. Every `file:line` quotation in §§2–7 was re‑read by a verifier against the actual `release/v0.8.2` source (and, where relevant, the pinned `pgx`, `grpc-go`, `memberlist`, and `golang-migrate` module sources). Where a reviewer over‑reached, the verifier’s correction is reflected in the severity and the prose (e.g. the pool‑exhaustion items are High not Critical because of the 30 s dispatch ceiling; the permanent‑leak claim is scoped to the *panic* trigger; the `DeleteAll` lock‑storm claim is refuted). Residual uncertainty is concentrated in the cluster‑mode findings (cluster mode is off by default and was exercised by reading, not running) and in exact OOM/stall thresholds, which depend on per‑deployment data volume, pool size, and compute‑node behaviour.
