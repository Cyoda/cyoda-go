# Callout Failover, Fencing and Cluster Membership — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** When a callout cannot be delivered to a compute node, or a compute node goes quiet, cyoda-go tries another one where that is safe, shuts the replaced one out of the transaction, tells the client honestly what happened — and pnodes stop vanishing from the cluster view when many tenants attach.

**Architecture:** Every pnode runs one local procedure over its own cnodes (`internal/grpc.RunLocal`); the owner's loop (`internal/callout.Coordinator`) runs it first and then hands the callout, with the tries left, to one peer after another over an authenticated, encrypted exchange (`internal/cluster/dispatch.PeerRouter`). A leaf arbiter (`internal/fence`) numbers every try; every callback takes its transaction's lock where it joins (`internal/domain/txjoin`), is checked under it, and the owner waits on that lock before the work moves on. Membership metadata shrinks to identity plus a list version; tag lists travel by reliable message.

**Tech Stack:** Go 1.26, `log/slog`, hashicorp/memberlist, pgx v5, gRPC + CloudEvents, testcontainers-go (PostgreSQL), OpenTelemetry metrics.

**Spec:** `docs/superpowers/specs/2026-09-19-254-callout-failover-design.md` (technical, governs). The design in plain language: `docs/superpowers/research/2026-09-19-254-design-brief.md`. Evidence: `docs/superpowers/research/2026-09-18-254-retry-policy-member-failover-research.md`. Executors read the spec sections their task names.

## Global Constraints

- **TDD is mandatory.** No implementation code without a failing test driving it (`.claude/rules/tdd.md`). A task's RED must be observed before its GREEN.
- **Verification tiers.** One package while iterating: `go test ./path/...`. Never add `-count=1`, never add `-v`, never read `go test ./...` as verification. `make test` per merged stream; `make test-full` + `go vet ./...` at the end; `make race` once before the PR. Docker is required; `make preflight` first.
- **Fail closed.** No fallback that returns a wrong-but-available result; a value over a bound fails, it is never clamped (`.claude/rules/correctness-over-availability.md`).
- **Multi-node is the primary target** (`.claude/rules/multi-node-primary.md`).
- **Go conventions.** `log/slog` only; `fmt.Errorf("failed to X: %w", err)`; constructor injection; every `Lock()` followed by `defer Unlock()` on the next line (IIFE for early release); no test hooks in production code.
- **Security.** Never log a pass, token, secret or key at any level; 4xx carry a code and domain detail, 5xx a generic message and a ticket; every data path keeps tenants apart.
- **No issue numbers** in code, comments, logs, errors, help or OpenAPI. Commit messages may carry them.
- **Deleting the old path is part of the task that replaces it.** Every deletion has a greppable exit check in its task.
- **`go.work` is tracked.** The local `go work edit -use …/cyoda-go-spi` line stays uncommitted: stage files explicitly, never `git add -A`.
- **Commits** end with `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`.
- **Vocabulary.** Design docs and code comments: pnode, cnode, callout, owner, callback, pass, try, hand-off, hand-over. User-facing help, README, CHANGELOG, errors: "node", "compute member", as the existing help does.
- **New settings, exact names and defaults:** `CYODA_RETRY_FIXED_NUM_RETRIES=3`, `CYODA_CALLOUT_RESPONSE_TIMEOUT_MS=30000`, `CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS=60000`, `CYODA_DISPATCH_CONNECT_TIMEOUT=2s`, `CYODA_CALLOUT_HANDOVER_ALLOWANCE=30s`, `CYODA_CALLOUT_PASS_ALLOWANCE=30s`; `CYODA_DISPATCH_WAIT_TIMEOUT=5s` kept (the patience); `CYODA_DISPATCH_FORWARD_TIMEOUT` kept (scheduler RPC only); `CYODA_TX_TOKEN_TTL` removed.
- **New error codes:** `CALLOUT_FAILED` (503, retryable), `CALLOUT_SUPERSEDED` (410). Workflow schema `1.4 → 1.5`.
- **Nothing under `internal/scheduler` changes**, except the one-line `Changed()` method on a test fake that the `NodeRegistry` interface forces (M-5).

## The sections

| File | Stream | Tasks | What it delivers |
|---|---|---|---|
| `H-harness.md` | H | H-1 … H-11 | several scripted cnodes in `internal/e2e`; `compute-test-client` tags + behaviours; optional parity capability to start compute clients |
| `C-config-spi-import.md` | C | C-1 … C-11 | settings, the two SPI fields, import validation, schema 1.5, OpenAPI, the SPI pin |
| `M-membership.md` | M | M-1 … M-9 | tags leave the 512-byte metadata; versioned lists; catch-up; `Changed()` |
| `L-local-procedure.md` | L | L-1 … L-11 | `CalloutFailure`, `RunLocal`, round robin, per-try passes |
| `F-fencing.md` | F | F-1 … F-10 | `internal/fence`, pass claims, the join-layer lock, engine checks, savepoint failures |
| `P-handover.md` | P | P-1 … P-8 | sealed request and answer, `PeerRouter`, own connection per hand-over |
| `O-owners-loop.md` | O | O-1 … O-9 | `callout.Coordinator`, `CALLOUT_FAILED`, the cnode's verdict to the client, wiring, deletions |
| `S-scenarios.md` | S | S-1 … S-15 | every E / P / M cell of the spec's coverage matrix |
| `D-docs.md` | D | D-1 … D-9 | help, ARCHITECTURE, cloud-parity, CHANGELOG assembly |
| `interfaces.md` | — | — | the names streams share; binding |
| this file | X | X-1, X-2, X-3 | two chores and the one counter no section owns; ordering; corrections |

Each section ends with a `## Stream interface summary` (binding names) and `## Open points`. Where an open point needed a decision it is decided under **Corrections** below; the corrections govern over the section text.

## Order of work

An arrow means "must have landed first".

```
wave 1 (independent, parallel worktrees)
  H-1→H-2→H-3→H-4                      internal/e2e harness
  X-2→H-5→H-6→H-7→H-8→H-9→H-10→H-11    compute-test-client, parity fixtures
  M-1→M-2→M-3→M-4→M-5→M-6→M-7→M-9      membership            (M-8 after H-10)
  C-1→C-2→C-3→C-4→C-5→C-6→C-7→C-8      config / SPI / import (C-9 after H-9: both move the parity scenario count)
  X-1→F-1→F-2→F-3                      internal/fence
  L-1→L-2→L-3                          contract + registry   (L-4 after M-1: it uses common.ChangeSignal)

wave 2
  F-4 (needs F-2)                      pass claims — before L-10
  L-4→L-5 (needs C-1)→L-6 (needs C-5)→L-7→L-8→L-9→L-10 (needs F-4, C-1)
  F-5→F-6 (need F-3, F-4)              one fence per process; the check on entry
  F-7→F-8→F-9                          join-layer lock; engine checks; savepoints
      F-7, F-9 and O-2 all edit internal/domain/entity/service.go — one after another, never in parallel
  X-3 (needs F-7, F-8)                 the counter of superseded callbacks

wave 3
  P-1→P-2→P-3 (needs L-6)→P-4→P-5 (needs M-5, C-2)→P-6 (needs L-8, L-10)→P-7→P-8
  O-1→O-2 (after F-9)→O-3 (needs L-10, F-5)→O-4→O-5 (needs P-5)→O-6→O-7→O-8 (needs P-6, P-7, F-6)→O-9

wave 4
  L-11 (needs P-6, O-8)   →   C-11 (needs O-8: nothing reads cfg.Cluster.TxTokenTTL)
  S-1 … S-11 (need H-4, O-8, F-8, F-9);  S-12, S-13 (need H-9, H-11, O-8);  S-14, S-15 (need H-10, M-4, P-7, O-8)
  D-1 … D-8 (each names the tasks it needs);  F-10, M-9, P-8, O-9 before D-9

wave 5
  D-9 (CHANGELOG assembly)  →  C-10 (LEAD pushes the SPI branch, merged to the SPI's main first; pin; make repin-plugins as a NEW commit)
  make test-full · go vet ./... · make race · fresh-context code review · security audit · PR against release/v0.9.0
```

**Files more than one stream edits** — merge by hand, in the order the streams land: `app/app.go` (C-1, C-2, M-4, M-7, F-5, F-7, P-6, O-8), `CHANGELOG.md` (fragments from C, L, M, F, P, O; D-9 rewrites the section as one), `cmd/cyoda/help/content/errors.md` (F-1, O-1, O-9), `telemetry.md` (M-7, P-5, O-6), `docs/ARCHITECTURE.md` (L-3, M-9, P-8, O-8, then D-6 audits the whole), `internal/grpc/callout.go` (L-6, then P-3 and O-3 add one field each), `internal/domain/entity/service.go` (F-7, F-9, O-2), `e2e/parity/registry.go` and the scenario count (H-9, C-9, S-13).

## Stream X — two chores and one counter

### Task X-1: `txgate.Registry.Acquire` follows the mutex rule

**Files:** Modify `internal/txgate/txgate.go` (`Acquire`, ~:30-54). Test: `internal/txgate/txgate_test.go` (existing tests are the guard; behaviour does not change).

- [ ] **Step 1: Confirm the guard is green.** Run: `go test ./internal/txgate/...` — Expected: PASS. (No new test: this is a refactor with no behavioural change; the existing tests `TestAcquire*` and `len()`-based cleanup tests pin the behaviour. Recorded as a TDD waiver for a pure refactor.)
- [ ] **Step 2: Replace the two bare critical sections with IIFEs.**

```go
func (r *Registry) Acquire(txID string) func() {
	if txID == "" {
		return func() {}
	}
	g := func() *gate {
		r.mu.Lock()
		defer r.mu.Unlock()
		g := r.gates[txID]
		if g == nil {
			g = &gate{}
			r.gates[txID] = g
		}
		g.refs++
		return g
	}()

	g.mu.Lock() // held until the returned release runs; not a critical section of this func

	return func() {
		g.mu.Unlock()
		r.mu.Lock()
		defer r.mu.Unlock()
		g.refs--
		if g.refs == 0 {
			delete(r.gates, txID)
		}
	}
}
```

- [ ] **Step 3:** Run: `go test ./internal/txgate/...` — Expected: PASS.
- [ ] **Step 4: Commit.** `git add internal/txgate/txgate.go && git commit -m "refactor(txgate): Acquire's registry sections release by defer"`

### Task X-2: the test compute client's `slow-configurable` sleeps

`config` reaches a catalog entry as the processor's pass-through context, a JSON **string**; unmarshalling it straight into a struct fails silently and `sleep_ms` is always 0 (`cmd/compute-test-client/catalog.go:107-120`).

**Files:** Modify `cmd/compute-test-client/catalog.go`. Test: `cmd/compute-test-client/catalog_test.go` (create if absent; open a neighbouring `_test.go` in the package and match it).

- [ ] **Step 1: Write the failing test.**

```go
func TestSlowConfigurable_SleepsForSleepMS(t *testing.T) {
	fn, ok := newCatalog(nil, nil).processor("slow-configurable")
	if !ok {
		t.Fatal("slow-configurable is not in the catalog")
	}
	// The server delivers the processor's context as a JSON string in
	// `parameters` (dispatch.go passes req.Parameters through unchanged).
	cfg, _ := json.Marshal(`{"sleep_ms": 120}`)
	start := time.Now()
	if _, err := fn(context.Background(), &Entity{ID: "e"}, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d := time.Since(start); d < 100*time.Millisecond {
		t.Fatalf("slept %v, want at least 100ms", d)
	}
}
```

- [ ] **Step 2:** Run: `go test ./cmd/compute-test-client/... -run TestSlowConfigurable` — Expected: FAIL with "slept … want at least 100ms".
- [ ] **Step 3: Unwrap the string before reading the struct** (keep accepting a bare object, which unit callers may pass):

```go
"slow-configurable": func(ctx context.Context, entity *Entity, config json.RawMessage) (*Entity, error) {
	var cfg struct {
		SleepMS int `json:"sleep_ms"`
	}
	raw := []byte(config)
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		raw = []byte(asString)
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("slow-configurable: invalid config: %w", err)
		}
	}
	if cfg.SleepMS > 0 {
		select {
		case <-time.After(time.Duration(cfg.SleepMS) * time.Millisecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return entity, nil
},
```

- [ ] **Step 4:** Run: `go test ./cmd/compute-test-client/...` — Expected: PASS. Then `grep -rn 'slow-configurable' e2e/ internal/e2e/` and run each scenario that names it (it now really sleeps; a scenario that relied on it returning at once must be read, not assumed).
- [ ] **Step 5: Commit.** `git add cmd/compute-test-client && git commit -m "fix(compute-test-client): slow-configurable reads its sleep from the context string"`

### Task X-3: the counter of superseded callbacks (spec §12)

No section owns it. It lives in the join layer, the one place every callback passes, so nothing in `internal/fence` or the engine is instrumented. **Needs F-7** (the `Joiner`) and F-8.

**Files:** Modify `internal/domain/txjoin/joiner.go` (the file F-7 creates), `app/app.go` (pass `observability.Meter()`), `cmd/cyoda/help/content/telemetry.md`. Test: `internal/domain/txjoin/joiner_metrics_test.go`.

**Interfaces:**
- Consumes: `txjoin.NewJoiner`, `(*Joiner).Run`, `fence.Check`, `fence.ErrSuperseded` (F); `observability.Meter()`; the metric test pattern of M-7 (`sdkmetric.ManualReader`).
- Produces: `txjoin.NewJoiner(signer, txMgr, f, gate, meter metric.Meter) (*Joiner, error)` — a nil meter is the no-op meter; the counter `cyoda.callout.superseded{outcome}` with `outcome` = `refused_on_entry` | `refused_at_lock` | `superseded_in_progress`. No tenant, callout id or pass in any attribute.

- [ ] **Step 1: Write the failing tests** — three tests in `joiner_metrics_test.go`, each building a `Joiner` over the memory backend's transaction manager, a real `*fence.Fence` and a `ManualReader` meter (copy the reader helper from M-7's `startMeteredGossip` test), minting the pass with `signer.Issue(token.Claims{…})`:

```go
func TestJoiner_CountsRefusalOnEntry(t *testing.T) {
	env := newJoinerEnv(t)                       // signer, memory txMgr with one open tx, fence, gate, reader
	pass := env.passFor("callout-1", 1, 0)       // the callout was never begun
	err := env.joiner.Run(env.tenantCtx, pass, func(context.Context) { t.Fatal("handler must not run") })
	if !errors.Is(err, fence.ErrSuperseded) {
		t.Fatalf("got %v, want ErrSuperseded", err)
	}
	env.assertCounter(t, "cyoda.callout.superseded", "refused_on_entry", 1)
}

func TestJoiner_CountsRefusalAtLock(t *testing.T) {
	env := newJoinerEnv(t)
	_, end := env.fence.Begin(context.Background(), "callout-1", env.txID, nil)
	env.fence.Advance("callout-1", 1)
	pass := env.passFor("callout-1", 1, 1)       // minor 1: Admit absorbs it, which the test can observe
	hold := env.gate.Acquire(env.txID)           // the test holds the lock, so Run queues behind it after Admit
	done := make(chan error, 1)
	go func() { done <- env.joiner.Run(env.tenantCtx, pass, func(context.Context) { t.Error("handler must not run") }) }()
	env.awaitAbsorbed(t, "callout-1", 1)         // Run has passed Admit: a pass with minor 0 is now refused
	endDone := make(chan struct{})
	go func() { end(); close(endDone) }()        // the callout ends; its wait queues behind Run
	env.awaitEnded(t, "callout-1", 1, 1)
	hold()
	if err := <-done; !errors.Is(err, fence.ErrSuperseded) {
		t.Fatalf("got %v, want ErrSuperseded", err)
	}
	<-endDone
	env.assertCounter(t, "cyoda.callout.superseded", "refused_at_lock", 1)
}

func TestJoiner_CountsSupersededInProgress(t *testing.T) {
	env := newJoinerEnv(t)
	_, end := env.fence.Begin(context.Background(), "callout-1", env.txID, nil)
	env.fence.Advance("callout-1", 1)
	pass := env.passFor("callout-1", 1, 0)
	endDone := make(chan struct{})
	err := env.joiner.Run(env.tenantCtx, pass, func(context.Context) {
		go func() { end(); close(endDone) }()    // ends while the handler holds the lock; its wait blocks until Run releases
		env.awaitEnded(t, "callout-1", 1, 0)     // end has unregistered the callout and reached its wait
	})
	if err != nil {
		t.Fatalf("the handler ran, Run must return nil, got %v", err)
	}
	<-endDone
	env.assertCounter(t, "cyoda.callout.superseded", "superseded_in_progress", 1)
}
```

Both waits observe the fence through its public API, with a 2 s ceiling and a 2 ms poll, so no production seam is needed: `awaitAbsorbed(t, callout, major)` polls `env.fence.Admit(ctx, []fence.Pair{{Callout: callout, Major: major, Minor: 0}})` until it is refused — true only once `Run`'s `Admit` has absorbed minor 1, i.e. `Run` is past `Admit` and queued on the lock the test holds; `awaitEnded(t, callout, major, minor)` polls the same call with the given pair until it is refused — `end` unregisters the callout *before* it waits, so the refusal is the signal that `end` has reached its wait. In the second test, after `awaitAbsorbed`, also wait for `end` to have unregistered (`awaitEnded(t, "callout-1", 1, 1)`) before `hold()` releases the lock.

- [ ] **Step 2:** Run: `go test ./internal/domain/txjoin/... -run TestJoiner_Counts` — Expected: FAIL to compile (`NewJoiner` takes four arguments; no counter).
- [ ] **Step 3: Implement.** In `NewJoiner`, take `meter metric.Meter` (nil → `noop.NewMeterProvider().Meter("")`), create `meter.Int64Counter("cyoda.callout.superseded", metric.WithDescription("Callbacks refused or overtaken because their compute member was replaced or its callout ended"))`, return the error if it cannot be created. In `Run`:

```go
	joined, err := JoinFromToken(ctx, j.signer, j.txMgr, j.fence, tok)
	if err != nil {
		if errors.Is(err, fence.ErrSuperseded) {
			j.count(ctx, "refused_on_entry")
		}
		return err
	}
	// … Acquire, WithHeld as F-7 wrote them …
	if err := fence.Check(joined); err != nil {
		j.count(ctx, "refused_at_lock")
		return err
	}
	handler(joined)
	if fence.Check(joined) != nil {
		j.count(ctx, "superseded_in_progress")
	}
	return nil
```

with `func (j *Joiner) count(ctx context.Context, outcome string) { j.superseded.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome))) }`. Update the one `NewJoiner` call in `app/app.go` to pass `observability.Meter()` and handle the error like the neighbouring constructors do.
- [ ] **Step 4:** Run: `go test ./internal/domain/txjoin/... ./app/...` — Expected: PASS.
- [ ] **Step 5: Document.** Add the counter to `cmd/cyoda/help/content/telemetry.md` beside `cyoda.callout.tries` (O-6), with its three `outcome` values in plain words; run `go test ./cmd/cyoda/help/...`.
- [ ] **Step 6: Commit.** `git add internal/domain/txjoin app/app.go cmd/cyoda/help/content/telemetry.md && git commit -m "feat(txjoin): count callbacks refused or overtaken by the fence"`

## Corrections — these govern over the section text

The sections were drafted in parallel while the spec was still being checked; four independent checks of §7 and the planning itself changed the spec after some sections were written. Where a section disagrees with the spec at the head of this branch, **the spec wins**; the known cases:

1. **P-6 — a duplicate nonce keeps its bare 403** (spec §6). Only a *full replay cache* is answered with a sealed `no_handoff`, and the duplicate check runs before the fullness check. P-6 as drafted seals both; seal only the full-cache refusal, and keep (adapt) the existing test that a replayed request is refused with 403. Add the unit test: a replayed request's answer cannot be opened by the owner as `no_handoff` — the owner reads the 403 as `no_answer`.
2. **P-4 / P-5 — a peer address that fails validation skips that peer** (spec §6): not connected, no try used, WARN logged with the node id (never the address in a client-visible text), the loop asks the next peer. It is not `Terminal`. `provedBeforeConnecting` keeps marshal and sign failures as `Terminal`.
3. **One hand-over counter.** `cyoda.callout.handovers{outcome}` is recorded by `PeerRouter.HandOver` (P-5 — rename from `cyoda.dispatch.handovers`); O-6 registers no second hand-over counter and documents P-5's in `telemetry.md` beside `cyoda.callout.tries` and `cyoda.callout.wait.duration`.
4. **F-7 — a held gRPC stream sends what it held, then returns the handler's error.** A joined chunked collection that fails at chunk *n* still delivers the responses of chunks 1…*n*-1, as it does today; nothing is dropped. Test it.
5. **F's coverage table** assigns the E and M cells of the fencing rows to "stream O"; they are stream **S**'s (S-6 … S-10, S-15).
6. **Names added after `interfaces.md` was written**, all binding: `fence.NewSupersededError()` (F-2); `grpc.Callout.Source grpc.CalloutSource` (P-3); `grpc.Callout.RetryPolicy string` (O-3); `callout.PeerRouter` — the small interface `internal/callout` defines and `*dispatch.PeerRouter` satisfies (O-5); `callout.Config{SelfNodeID, FixedNumRetries, Patience, HandoverAllowance}`, `callout.New(local, members, peers, fence, uuids, cfg)` (O-3); `txjoin.NewJoiner(signer, txMgr, fence, gate)`, `(*Joiner).Run` (F-7); `workflow.ErrSavepointInfra` (F-9); `contract.CalloutStats` (O-3). Seam precisions from P that O-5 must honour: read `Failure.Kind`, not `Connected` alone (`Connected == false` also covers a `Terminal` proved before connecting); a peer that tried cnodes and handed off to none answers `no_handoff` with `TriesUsed ≥ 1` and `Connected == true`; `HandOver` does not report the caller's context ending — the owner checks its own `ctx` and `context.Cause` after the call; `HandOver` returns the peer's warnings for the caller to add and adds the peer's error diagnostics to `ctx` itself.
7. **L-4 uses `common.NewChangeSignal()`** from M-1 for `MemberRegistry.Changed()`; if L-4's text builds its own closed-and-replaced channel, use M-1's type instead — one implementation, not two.
8. **M-8 calls H-10's helper** `multinode.StartComputeClientOrSkip(t, fixture, node, parity.ComputeClientSpec{TenantID, Tags, Behaviour})`, not the `ExtraComputeFixture.StartCompute` name M proposed.
9. **S — no further pass once the patience has run out** (spec §5), so the number of attempts is exact: S-13's `CalloutEveryTryUsed` and S-14's two-tries scenario assert the exact count, not a range. The failure list's angle brackets are literal (`[member<3f2a…>: cause]`, `member<->` for a lost hand-over answer; spec §8.2): `parseCalloutFailed` accepts that one form.
10. **O-9 — OpenAPI.** Broaden the description of the shared `ServiceUnavailable` response component (`api/openapi.yaml` ~:11889) from "the storage layer could not serve the request" to cover a callout that could not be completed (`CALLOUT_FAILED`, `NO_COMPUTE_MEMBER_FOR_TAG`, `DISPATCH_TIMEOUT`, `COMPUTE_MEMBER_DISCONNECTED`, `DISPATCH_FORWARD_FAILED`), then `go generate ./api`. No `410` entry is added here: the document mentions neither `X-Tx-Token` nor any callback status today (`TRANSACTION_EXPIRED` is already an undeclared 410), and documenting the callback contract across the API is a scope decision recorded under "For the product owner".
11. **F-5 moves the `txgate.Registry` construction up** in `app/app.go` (it is built at ~:644 today, after the dispatch wiring block) so that the fence and the dispatch wiring can both take it.
12. **D-4 step 2** (the `retryable` description in `BaseEvent.json`, a schema tree that mirrors Cloud's) is carried out only with the product owner's agreement; skip it otherwise and leave D-5's parity note to say what the flag means in cyoda-go.
13. **S-8 / S-9 hold the `messages` table** under `ACCESS EXCLUSIVE` for about two seconds to keep a joined read inside PostgreSQL. That is accepted for these isolated scenarios; never do it to `entities` (`storage_ceilings_e2e_test.go:340-344`).

## Running it

Subagent-driven development, one fresh implementer per task, streams in parallel **git worktrees** where the order above shows no arrow between them; each stream's reviewer sees that stream's diff; the lead merges streams back in wave order and runs `make test` on the merge. A whole-branch fresh-context code review and the security audit (`antigravity-bundle-security-engineer:security-auditor`, Gate 3: no credential logged at any level, tenant isolation on **every** data path, input validated at boundaries, 4xx with detail and 5xx generic with a ticket) come after wave 5 — the audit's first targets are the pass claims and `Admit`'s tenant ordering (F-4, F-6), the sealed answer and the replay rule (P-1, P-6), member ids in client-visible text (O-1), and tag lists between tenants in membership messages (M-3, M-4).

Steps marked **LEAD** (pushing the SPI branch and this branch) are outward-facing and are the session lead's, with the product owner's say-so.

## For the product owner — decided here, and not decided here

Decided during the checks, beyond the brief as approved (the brief and spec say how and why): the fence works by checks under the transaction's lock and by the owner's wait, never by cancelling a statement; **every** callback, read or write, takes that lock where it joins, because two users of one transaction stop the process on memory and SQLite and collide on PostgreSQL today; a callback is not interrupted by its compute member going away; a savepoint that cannot be created, undone or released fails the operation; a replayed hand-over is never answered "nothing was handed over".

Not decided here: documenting the callback contract (`X-Tx-Token`, 401/403/404/410 on joined requests) in `api/openapi.yaml`, which is absent today across some sixty operations; the `retryable` wording in the shared `BaseEvent.json`; two defects seen and not verified, to be filed — interleaved savepoints of two callbacks on one PostgreSQL transaction (spec §16), and a `COMMIT_BEFORE_DISPATCH` processor reached inside a callback keeping the transaction's lock across its callout (part of the defect already filed for that mode).
