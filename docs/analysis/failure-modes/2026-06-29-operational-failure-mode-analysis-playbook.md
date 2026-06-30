# Re‑run Playbook — Cyoda‑Go Operational Failure‑Mode Analysis

Companion to [`2026-06-29-operational-failure-mode-analysis.md`](./2026-06-29-operational-failure-mode-analysis.md). Use this to reproduce the analysis on a later revision. Keep it short; keep it honest; ground every claim in code.

---

## 1. When to re‑run

- Before a release that touches the transaction/pool/dispatch path, storage plugin, cluster code, or workflow engine.
- After remediating any **High** finding (confirm it's gone *and* didn't shift severity elsewhere).
- On any change to the load‑bearing facts in §6 (those are the hinges the conclusions hang on).

## 2. Scope & sources (re‑confirm each run)

- **Target:** `cyoda-go` core + `plugins/postgres`. Check out the release branch under analysis (last run: `release/v0.8.2`).
- **SPI contract:** the pinned `github.com/cyoda-platform/cyoda-go-spi` module — read its source from `$(go env GOMODCACHE)/github.com/cyoda-platform/cyoda-go-spi@<version>` (last run: `v0.8.1`). The `spi` module version moves independently of the binary — re‑resolve it from `plugins/postgres/go.mod`.
- **Out of scope:** the Cassandra plugin / `cyoda-go-cassandra`; `memory`/`sqlite` plugins except where they expose a shared‑contract issue.
- **Lens:** operational failure only — *unavailability, inconsistency, data corruption/loss, uncontrolled breakdown (panic/fatal/leak)*. Not throughput. Calibrate severity to **moderate volume**: tens of concurrent requests, 2–3 nodes, an occasionally slow/dead compute node, clients that disconnect.

## 3. Method (the pattern that worked)

Fan‑out → adversarial verify → synthesize. Drive it with the Workflow tool (ultracode), or run the dimensions as parallel subagents.

1. **Scout first (inline).** Read the consistency model and lifecycle yourself before fanning out, so finders share grounding: `transaction_manager.go`, `txstate.go`, `commit_validator.go`, `entity_store.go`, `store_factory.go`, `engine_processors.go`, `internal/domain/entity/service.go`, migration SQL, `internal/cluster/*`, `internal/grpc/*`. Seed the finder prompt with the verified facts in §6.
2. **Hunt — one finder per dimension** (§5). Each returns structured findings: `{id, title, severity, failure_class, claim, evidence[{file,lines,quote}], trigger, blast_radius, confidence, recommended_fix}`. Quote 1–6 pivotal lines per evidence item.
3. **Verify — one adversarial verifier per finding.** Instruction: *try to refute it; re‑read the cited code; hunt for a mitigation (timeout, reaper, recover, lock, guard, config default, tenant filter); default to refuted/partial unless code proves it real **and** reachable.* Returns `{verdict, corrected_severity, proof[], refutation_or_nuance, corrected_trigger, assessment}`. **The verified severity, not the finder's guess, is what ships.**
4. **Synthesize yourself** (main loop) — don't let an agent echo the full JSON (see §8). Consolidate overlapping findings, separate confirmed/partial/refuted, and diff against the prior baseline (§7).

**Disciplines (non‑negotiable):** every claim cites `file:line` + quote; convert hypotheses to facts; an "absence" finding (missing timeout/reaper) must be grounded by showing the search came up empty; state default‑config reachability vs. opt‑in/cluster‑only/latent.

## 4. Severity calibration

`critical` = node‑wide, permanent (restart‑required), or silent corruption, reachable by default. `high` = node‑wide but transient/self‑healing, or reachable‑but‑narrower corruption. `medium` = bounded blast radius, or default‑off/opt‑in. `low` = latent / defense‑in‑depth / unreachable‑today. Watch for **mitigation‑dependent severities**: e.g. pool‑starvation is *high not critical* only because the 30s dispatch timeout caps it — if that cap is removed, severity jumps back. Flag every such dependency explicitly.

## 5. The ten dimensions (reuse verbatim)

1. Transaction lifetime & connection‑pool exhaustion
2. Snapshot‑isolation / first‑committer‑wins correctness
3. Tenant isolation & cross‑tenant leakage
4. Concurrency hazards & process‑fatal conditions (panics, races, goroutine leaks)
5. Cluster coordination & multi‑node consistency
6. gRPC external compute‑node integration (hangs, leaks, backpressure)
7. Resource exhaustion & unbounded in‑memory growth (OOM)
8. Schema evolution, model locking & migrations
9. API boundary, request lifecycle & error handling
10. Cross‑cutting interactions & emergent failure modes

## 6. Load‑bearing facts — re‑verify these every run

Conclusions hinge on these. For each, the analysis assumes the **current** value; if it changed, re‑score the dependent findings. "Remediated when" = what to look for to mark the risk closed.

| # | Invariant (as of last run) | Anchor | Remediated when |
|---|---|---|---|
| F1 | Default pool `MaxConns=25`, `MinConns=5`; no `AcquireTimeout` | `plugins/postgres/config.go` (`CYODA_POSTGRES_MAX_CONNS`); `newPool` | Pool sized + `AcquireTimeout` set |
| F2 | A `pgx.Tx` (pooled conn) is held for the whole app‑tx; tx chosen via `resolveRaw` | `transaction_manager.go` `Begin` (`pool.BeginTx`, RepeatableRead); `store_factory.go:resolveRaw` | External work no longer inside the tx (commit‑before‑dispatch default) |
| F3 | SYNC/ASYNC_SAME_TX dispatch to gRPC **inline in the tx** (default mode) | `engine_processors.go` (`default:` → `executeSyncProcessor` → `DispatchProcessor`) | Default mode commits before dispatch |
| F4 | Dispatch timeout default **30s**, from workflow config, **uncapped** | `internal/grpc/dispatch.go` (`defaultResponseTimeoutMs`, `ResponseTimeoutMs`) | Server‑side max clamp applied |
| F5 | **No** `statement_timeout` / `idle_in_transaction_session_timeout` / `lock_timeout` anywhere | grep plugin + `plugins/postgres/migrations/*` | Set via DSN `RuntimeParams` / `AfterConnect` |
| F6 | Orphaned‑tx reaper **unwired**: no `Manager.Register` caller; reaper cluster‑gated; rollback uses `context.Background()` rejected by `verifyTenant` | `app/app.go` (reaper goroutine, `SetTransactionManager`); `internal/cluster/lifecycle/manager.go`; `transaction_manager.go:verifyTenant` | `Register`/`RecordOutcome` wired, reaper unconditional, privileged rollback path |
| F7 | **No** `defer`‑rollback in the entity service (explicit rollback only) | grep `defer` in `internal/domain/entity/service.go` | `defer` rollback guard around every `Begin` |
| F8 | HTTP server has no Read/Write/Idle/Header timeouts; no per‑request deadline | `cmd/cyoda/run.go` (`&http.Server{...}`) | Timeouts + per‑request context deadline added |
| F9 | RLS `ENABLE`d but **not `FORCE`d**; app connects as owner → isolation is `WHERE tenant_id=$1` only; pool path never sets `app.current_tenant` | `migrations/000001_*.up.sql`; `transaction_manager.go` `set_config`; `store_factory.go` resolvers | `FORCE` + non‑owner role + tenant set on pool path |
| F10 | gRPC server has **no recovery interceptor**; keep‑alive response bypasses `sendMu` | `internal/grpc/server.go`; `internal/grpc/streaming.go` (raw `stream.Send`) vs `members.go` (`Member.Send`) | Recovery interceptor added; all sends go through `sendMu` |
| F11 | List/`GetAll` runs **on the pool, not a tx**, and materializes the whole model; sync search emits `LIMIT` only when `Limit>0`; async search uses detached `context.Background()` | `internal/domain/entity/service.go` (`ListEntities`→`GetAll`); `plugins/postgres/searcher.go`; `internal/domain/search/service.go` | `LIMIT`/`OFFSET` pushed to SQL; default+max cap on Searcher path; bounded async worker pool |
| F12 | Schema‑extension log has **no conflict protection**; savepoint folds under the tx snapshot | `plugins/postgres/model_store.go` (fold + savepoint); `txstate.go` (read‑set keyed by entity‑id only) | `ExtendSchema` serialized per `(tenant,model)` (e.g. `FOR UPDATE` on `models`) |

## 7. Diff against the baseline

1. Keep each run's machine‑readable ledger (the workflow's `data` array → `verified-findings.json`). The current baseline is the [Appendix table](./2026-06-29-operational-failure-mode-analysis.md#appendix--full-finding-ledger) (54 findings; 27 confirmed / 26 partial / 1 refuted).
2. For every prior finding, classify: **remediated** (re‑verify against its "Remediated when" in §6 / its `recommended_fix`), **persists**, or **regressed‑in‑severity**.
3. Re‑test the **"what is sound"** list (analysis §7) for *regressions* — those are the silent ones (e.g. a new query missing `WHERE tenant_id`, a new `Count`‑based invariant inside a tx, removal of the `FOR SHARE` validation).
4. Re‑check **mitigation‑dependent downgrades** (§4): if a mitigation that lowered a severity was removed, raise it back.
5. Hunt net‑new findings (dimensions §5), especially around code that changed since the last run.
6. Write a new **dated** analysis doc; don't overwrite the prior one — the trend across runs is itself a signal.

## 8. Pitfalls from the last run

- **Don't have an agent echo the full findings JSON** — it overran the 64k output limit and truncated the scratchpad file. Pull the structured result from the workflow's task‑output `result.data` (or have each finder write its own small file).
- A dead/slow compute node mostly **self‑heals** (30s dispatch timeout) — the *permanent* leak is the **panic‑between‑Begin‑and‑Commit** path. Don't conflate them.
- Cluster mode is **off by default** — tag cluster findings as cluster‑only.
- Verify mitigations by reading them, not assuming: several "critical" guesses were correctly downgraded only because a verifier found the cap/guard.
