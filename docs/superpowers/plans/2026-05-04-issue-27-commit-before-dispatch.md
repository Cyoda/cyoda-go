# `COMMIT_BEFORE_DISPATCH` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship a fourth processor `executionMode` value `COMMIT_BEFORE_DISPATCH` that commits the cascade transaction before dispatching the processor, opens a fresh transaction on return, and applies the result via `CompareAndSave`. Optional `startNewTxOnDispatch` flag opens TX_post before dispatch.

**Architecture:** The engine takes over per-segment TX boundaries and per-segment SPI writes (today the handler does both). `service.Handler` hands `txMgr` to the engine and lets it own commit/begin around segment boundaries. Apart from one additive SPI struct field for the new `startNewTxOnDispatch` flag (SPI v0.7.0 bump), the mode is implementable with existing SPI primitives. Existing audit events `SMEventProcessingPaused` and `SMEventStateProcessResult` bracket the segment naturally without introducing new event types.

**Tech Stack:** Go 1.26, `log/slog`, postgres `RepeatableRead` + application FCW, sqlite, in-memory plugin, cassandra plugin (no parity work). OpenAPI YAML source at `api/openapi.yaml` regenerated to `api/generated.go` via `go:generate` in `api/generate.go`.

**Spec:** `docs/superpowers/specs/2026-05-04-issue-27-commit-before-dispatch-design.md`

**Worktree:** Already on `feature/issue-27-commit-on-callout` off `release/v0.7.0`.

**Verification gate (Gate 5):** `go test ./... -v` (Docker required for E2E), `go vet ./...`, plus `make test-short-all` for plugin submodules. One-shot `go test -race ./...` immediately before creating the PR per `.claude/rules/race-testing.md` — never per-step.

---

## File Structure

| File | Status | Responsibility |
|---|---|---|
| `api/openapi.yaml` | Modify | Add `COMMIT_BEFORE_DISPATCH` enum value and `startNewTxOnDispatch: bool` field on `ExternalizedProcessorDefinitionDto` |
| `api/generated.go` | Regenerate | Picked up by `go generate ./api/...` |
| `internal/domain/workflow/types.go` (or wherever `ProcessorDefinition` mirror lives) | Modify | Carry the new field into the engine's local DTO if any |
| `internal/domain/workflow/validate.go` | Modify | Reject `startNewTxOnDispatch:true` outside `COMMIT_BEFORE_DISPATCH` |
| `internal/domain/workflow/validate_test.go` | Modify (or create) | Cover the new validator rule |
| `internal/domain/workflow/engine.go` | Modify (substantial) | Engine takes over TX boundaries, per-segment Save flush, new `executeCommitBeforeDispatch` method, plug into `executeProcessors` switch |
| `internal/domain/workflow/engine_test.go` | Modify | Unit tests for the new mode (cases 4, 5, 6, 12, 13, 14 from spec §16) |
| `internal/domain/entity/service.go` | Modify (substantial) | `CreateEntity`, `UpdateEntity`, `UpdateEntityCollection`, `CreateEntityCollection` no longer wrap engine in handler-owned TX; `If-Match` plumbed to engine |
| `internal/e2e/commit_before_dispatch_test.go` | Create | E2E suite covering cases 1, 2, 3, 7, 8, 9, 10, 11, 15 |
| `cmd/cyoda/help/content/workflows.md` | Modify | Document the new mode, the flag, the idempotency requirement, the "no-double-write" best-practice |
| `docs/ARCHITECTURE.md` | Modify | Per spec §15 reconciliation list |
| `docs/CONSISTENCY.md` | Modify | Per spec §15 reconciliation list |
| `docs/CONCURRENCY.md` | Modify | Per spec §15 reconciliation list |

---

## Phase 1 — SPI bump, schema additions, and validation

The Phase-1 spike (already done by the controller before plan execution started) confirmed: SPI `ProcessorConfig` (`spi/types.go:148-154`) is a strongly-typed struct with no flexible map. Carrying `startNewTxOnDispatch` requires an additive field on `ProcessorConfig`. Path chosen: **SPI v0.7.0 bump** with one optional pointer field, additive, backward-compatible.

### Task 1: SPI v0.7.0 — add `StartNewTxOnDispatch` to `ProcessorConfig`

This task lives in the **separate `cyoda-go-spi` repo**. The cyoda-go worktree picks up the bump via `go.mod`. Coordination with the Cassandra plugin is automatic at JSON layer (plugins persist workflow definitions as JSON; the new field flows through with no plugin code changes — only a routine SPI dependency bump in those plugins' next release).

**Files:**
- Modify (in `cyoda-go-spi` repo at `~/go-projects/cyoda-light/cyoda-go-spi` or wherever the local checkout lives): `types.go` near line 148
- Tag a new SPI release v0.7.0
- Update (in cyoda-go worktree): `go.mod` to depend on `cyoda-go-spi v0.7.0`

- [ ] **Step 1: Locate the SPI repo checkout**

Run: `find ~/go-projects -maxdepth 3 -name "cyoda-go-spi" -type d 2>/dev/null`

If no checkout exists, clone it: `git clone https://github.com/Cyoda-platform/cyoda-go-spi ~/go-projects/cyoda-go-spi`

- [ ] **Step 2: In the SPI repo, write the failing test**

Add (or extend) `types_test.go`:

```go
func TestProcessorConfig_StartNewTxOnDispatch_RoundTrips(t *testing.T) {
	tt := true
	cfg := ProcessorConfig{StartNewTxOnDispatch: &tt}
	bs, err := json.Marshal(cfg)
	if err != nil { t.Fatal(err) }
	if !strings.Contains(string(bs), `"startNewTxOnDispatch":true`) {
		t.Errorf("missing field in marshalled JSON: %s", bs)
	}
	var back ProcessorConfig
	if err := json.Unmarshal(bs, &back); err != nil { t.Fatal(err) }
	if back.StartNewTxOnDispatch == nil || !*back.StartNewTxOnDispatch {
		t.Errorf("round-trip dropped the field: %+v", back)
	}

	// Default (nil) does NOT marshal because of omitempty.
	defaultCfg := ProcessorConfig{}
	bs2, _ := json.Marshal(defaultCfg)
	if strings.Contains(string(bs2), "startNewTxOnDispatch") {
		t.Errorf("nil pointer should be omitted, got %s", bs2)
	}
}
```

- [ ] **Step 3: Run test, verify it fails**

Run: `go test -run TestProcessorConfig_StartNewTxOnDispatch -v`

Expected: FAIL — field undefined.

- [ ] **Step 4: Add the field**

In `cyoda-go-spi/types.go`:

```go
type ProcessorConfig struct {
	AttachEntity         bool   `json:"attachEntity,omitempty"`
	CalculationNodesTags string `json:"calculationNodesTags,omitempty"`
	ResponseTimeoutMs    int64  `json:"responseTimeoutMs,omitempty"`
	RetryPolicy          string `json:"retryPolicy,omitempty"`
	Context              string `json:"context,omitempty"`
	// StartNewTxOnDispatch, when true and ExecutionMode is COMMIT_BEFORE_DISPATCH,
	// causes the cascade engine to open a fresh transaction before dispatching
	// the processor (so the processor may perform transactional work via that
	// tx's token). When false (default) the processor runs with no transaction
	// context and the connection is released entirely during dispatch.
	// Ignored for any other ExecutionMode.
	StartNewTxOnDispatch *bool `json:"startNewTxOnDispatch,omitempty"`
}
```

- [ ] **Step 5: Run test, verify pass**

Run: `go test ./... -v`

Expected: PASS for the new test, no regressions in existing SPI tests.

- [ ] **Step 6: Commit and tag v0.7.0 in the SPI repo**

```bash
cd ~/go-projects/cyoda-go-spi
git add types.go types_test.go
git commit -m "feat(types): add ProcessorConfig.StartNewTxOnDispatch (v0.7.0)"
git tag v0.7.0
git push origin main --tags
```

(If the `feedback_go_module_tags_immutable.md` rule is in effect — never force-move tags — verify v0.7.0 doesn't already exist before tagging. If it does for any reason, choose v0.7.1 and update step 7 accordingly.)

- [ ] **Step 7: In the cyoda-go worktree, bump the dependency**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/issue-27-commit-on-callout
go get github.com/cyoda-platform/cyoda-go-spi@v0.7.0
go mod tidy
```

Expected: `go.mod` shows `github.com/cyoda-platform/cyoda-go-spi v0.7.0`; `go.sum` updated.

- [ ] **Step 8: Verify the worktree builds and existing tests pass**

```bash
go build ./...
go test -short ./... -v
```

Expected: PASS. Both should be unaffected — `StartNewTxOnDispatch` is an optional pointer field with `omitempty`, no consumer cares yet.

- [ ] **Step 9: Commit the dependency bump**

```bash
git add go.mod go.sum
git commit -m "chore(deps): bump cyoda-go-spi to v0.7.0 for StartNewTxOnDispatch (#27)"
```

### Task 2: Add `COMMIT_BEFORE_DISPATCH` to the OpenAPI enum

**Files:**
- Modify: `api/openapi.yaml` around line 8451-8456

- [ ] **Step 1: Add the enum value**

Find the `ExternalizedProcessorDefinitionDto` block (`grep -n "ExternalizedProcessorDefinitionDto:" api/openapi.yaml`).

Modify the `executionMode` enum to include `COMMIT_BEFORE_DISPATCH`:

```yaml
            executionMode:
              type: string
              enum:
                - SYNC
                - ASYNC_SAME_TX
                - ASYNC_NEW_TX
                - COMMIT_BEFORE_DISPATCH
              description: |
                Processor execution semantics. SYNC, ASYNC_SAME_TX, ASYNC_NEW_TX
                run inside the cascade transaction. COMMIT_BEFORE_DISPATCH commits
                the cascade transaction before dispatch and opens a new one on
                return; the processor's result is applied via CompareAndSave
                using the prior commit's transaction ID as the expected version.
```

- [ ] **Step 2: Add `startNewTxOnDispatch` field**

Inside the same `properties:` block, add:

```yaml
            startNewTxOnDispatch:
              type: boolean
              default: false
              description: |
                Only meaningful when executionMode is COMMIT_BEFORE_DISPATCH. If
                true, a new transaction is opened before dispatching the
                processor so the processor can perform transactional work
                during its execution. Connection is held in that new
                transaction during dispatch. If false (default), the processor
                runs with no transaction context; the connection is released
                entirely during processor execution.
```

- [ ] **Step 3: Regenerate the Go DTO**

Run: `go generate ./api/...`

Expected: `api/generated.go` updated. Inspect the diff:

```bash
git diff api/generated.go | head -50
```

Confirm the new constant `COMMITBEFOREDISPATCH ExternalizedProcessorDefinitionDtoExecutionMode = "COMMIT_BEFORE_DISPATCH"` appears alongside `SYNC`/`ASYNCNEWTX`/`ASYNCSAMETX` and a new `StartNewTxOnDispatch *bool` field appears on `ExternalizedProcessorDefinitionDto`.

- [ ] **Step 4: Run unit tests touching the DTO**

Run: `go test -short ./api/... ./internal/e2e/openapivalidator/... -v`

Expected: PASS. Any failure here means the DTO regen broke an existing consumer; investigate before proceeding.

- [ ] **Step 5: Commit**

```bash
git add api/openapi.yaml api/generated.go
git commit -m "feat(api): add COMMIT_BEFORE_DISPATCH executionMode + startNewTxOnDispatch (#27)"
```

### Task 3: Add validator rule rejecting `startNewTxOnDispatch=true` outside `COMMIT_BEFORE_DISPATCH`

**Files:**
- Modify: `internal/domain/workflow/validate.go`
- Modify: `internal/domain/workflow/validate_test.go`

- [ ] **Step 1: Write the failing test**

In `validate_test.go`, add:

```go
func TestValidateWorkflows_RejectsStartNewTxOnDispatchOnNonCommitBeforeDispatch(t *testing.T) {
	// startNewTxOnDispatch:true on a SYNC processor must be rejected at registration.
	tt := true
	wf := spi.WorkflowDefinition{
		Name: "test-wf",
		States: []spi.StateDefinition{
			{Name: "S1", Transitions: []spi.TransitionDefinition{
				{Name: "t", Next: "S2", Processors: []spi.ProcessorDefinition{
					{Type: "EXTERNAL", Name: "p", ExecutionMode: "SYNC",
						StartNewTxOnDispatch: &tt},
				}},
			}},
			{Name: "S2"},
		},
	}
	err := validateWorkflows([]spi.WorkflowDefinition{wf})
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "startNewTxOnDispatch") {
		t.Fatalf("error message must mention startNewTxOnDispatch, got: %v", err)
	}
}

func TestValidateWorkflows_AcceptsStartNewTxOnDispatchOnCommitBeforeDispatch(t *testing.T) {
	tt := true
	wf := spi.WorkflowDefinition{
		Name: "test-wf",
		States: []spi.StateDefinition{
			{Name: "S1", Transitions: []spi.TransitionDefinition{
				{Name: "t", Next: "S2", Processors: []spi.ProcessorDefinition{
					{Type: "EXTERNAL", Name: "p", ExecutionMode: "COMMIT_BEFORE_DISPATCH",
						StartNewTxOnDispatch: &tt},
				}},
			}},
			{Name: "S2"},
		},
	}
	if err := validateWorkflows([]spi.WorkflowDefinition{wf}); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}
```

(If `spi.ProcessorDefinition` doesn't have `StartNewTxOnDispatch` because Task 1 chose option (b) — flag carried in `Config` map — adapt the test accordingly: `Config: map[string]any{"startNewTxOnDispatch": true}`.)

- [ ] **Step 2: Run test, verify it fails**

Run: `go test ./internal/domain/workflow/ -run TestValidateWorkflows_Reject -v`

Expected: FAIL — currently no validation for `startNewTxOnDispatch`.

- [ ] **Step 3: Implement the validator rule in `validate.go`**

Inside `validateWorkflows`, after the existing loop validation, add a per-processor check. Locate the iteration over states/transitions in the existing function and add (adapt to file structure if it doesn't exist; if needed, factor a new helper):

```go
// validateProcessorFlags rejects misuse of startNewTxOnDispatch.
func validateProcessorFlags(wf spi.WorkflowDefinition) error {
	for _, st := range wf.States {
		for _, tr := range st.Transitions {
			for _, p := range tr.Processors {
				if p.StartNewTxOnDispatch != nil && *p.StartNewTxOnDispatch &&
					p.ExecutionMode != "COMMIT_BEFORE_DISPATCH" {
					return fmt.Errorf(
						"workflow %q transition %q processor %q: startNewTxOnDispatch=true is only valid with executionMode=COMMIT_BEFORE_DISPATCH (got %q)",
						wf.Name, tr.Name, p.Name, p.ExecutionMode)
				}
			}
		}
	}
	return nil
}
```

Wire it into `validateWorkflows`:

```go
func validateWorkflows(workflows []spi.WorkflowDefinition) error {
	for _, wf := range workflows {
		if err := validateWorkflowLoops(wf); err != nil {
			return err
		}
		if err := validateProcessorFlags(wf); err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 4: Run test, verify it passes**

Run: `go test ./internal/domain/workflow/ -run TestValidateWorkflows -v`

Expected: PASS for both new tests; existing tests still PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/domain/workflow/validate.go internal/domain/workflow/validate_test.go
git commit -m "feat(workflow): validate startNewTxOnDispatch only on COMMIT_BEFORE_DISPATCH (#27)"
```

---

## Phase 2 — Engine TX inversion (foundational, single-segment regression bound)

The engine already takes `txMgr` (`engine.go:54`). What it does NOT do today is call `EntityStore.Save` or `TxMgr.Commit/Begin` itself — the handler does both. Phase 2 introduces those calls inside the engine *only for cascades that reach a `COMMIT_BEFORE_DISPATCH` boundary*. Cascades without one preserve today's behaviour bit-for-bit.

The simplest realisation: a new helper `commitAndReopenSegment(ctx, entity, currentTxID) (newTxID, newCtx, error)` that does Save → Commit → Begin and returns the new TX context. `executeProcessors` calls it at the boundary. The handler doesn't change yet.

### Task 4: Make the engine's segment helper, isolated and tested

**Files:**
- Modify: `internal/domain/workflow/engine.go`
- Modify: `internal/domain/workflow/engine_test.go`

- [ ] **Step 1: Write the failing test**

In `engine_test.go` add (note: many existing tests pre-date the segment helper; do not break them):

```go
func TestEngine_CommitAndReopenSegment_ReturnsNewTxIDAndContext(t *testing.T) {
	ctx := context.Background()
	mem := memory.New() // existing in-memory plugin used by other engine tests
	factory, _ := mem.NewFactory(ctx, func(string) string { return "" })
	es, _ := factory.EntityStore(ctx)
	tm, _ := factory.TransactionManager(ctx)

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil { t.Fatalf("Begin: %v", err) }

	entity := &spi.Entity{Meta: spi.EntityMeta{ID: "e1", State: "S_pre"}, Data: []byte(`{"x":1}`)}
	if _, err := es.Save(txCtx, entity); err != nil {
		t.Fatalf("Save: %v", err)
	}

	eng := NewEngine(factory, &fakeUUIDs{}, tm)
	newTxID, newTxCtx, err := eng.commitAndReopenSegment(txCtx, entity, txID)
	if err != nil { t.Fatalf("commitAndReopenSegment: %v", err) }
	if newTxID == txID {
		t.Fatalf("expected new txID, got same as old: %s", newTxID)
	}
	if newTxCtx == nil { t.Fatalf("expected non-nil new context") }

	// Entity is durable — readable from a fresh TX.
	readTxID, readCtx, _ := tm.Begin(ctx)
	got, err := es.Get(readCtx, "e1")
	if err != nil { t.Fatalf("Get: %v", err) }
	if got.Meta.State != "S_pre" {
		t.Fatalf("entity state = %q, want %q", got.Meta.State, "S_pre")
	}
	tm.Rollback(readCtx, readTxID)
}
```

(Adapt fixtures to existing test helpers in `engine_test.go`; the goal is to exercise commit→begin and assert both the fresh txID and the durable entity.)

- [ ] **Step 2: Run test, verify it fails**

Run: `go test ./internal/domain/workflow/ -run TestEngine_CommitAndReopenSegment -v`

Expected: FAIL — `commitAndReopenSegment` is undefined.

- [ ] **Step 3: Implement the helper in `engine.go`**

Add after `executeAsyncNewTx`:

```go
// commitAndReopenSegment flushes the in-memory entity to TX_pre's buffer,
// commits TX_pre, and begins a fresh TX_post. Returns (newTxID, newCtx, err).
// The caller passes its current entity ref; on return the same ref may be
// continued — read/write semantics for the entity in TX_post follow the
// fresh transaction context.
//
// Used at COMMIT_BEFORE_DISPATCH segment boundaries.
func (e *Engine) commitAndReopenSegment(ctx context.Context, entity *spi.Entity, txID string) (string, context.Context, error) {
	es, err := e.factory.EntityStore(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("commit-before-dispatch: get entity store: %w", err)
	}
	if _, err := es.Save(ctx, entity); err != nil {
		return "", nil, fmt.Errorf("commit-before-dispatch: flush pre-callout state: %w", err)
	}
	if err := e.txMgr.Commit(ctx, txID); err != nil {
		return "", nil, fmt.Errorf("commit-before-dispatch: commit TX_pre: %w", err)
	}
	newTxID, newCtx, err := e.txMgr.Begin(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("commit-before-dispatch: begin TX_post: %w", err)
	}
	return newTxID, newCtx, nil
}
```

- [ ] **Step 4: Run test, verify it passes**

Run: `go test ./internal/domain/workflow/ -run TestEngine_CommitAndReopenSegment -v`

Expected: PASS. Run the full package to confirm no regressions:

Run: `go test ./internal/domain/workflow/ -v`

Expected: PASS for everything.

- [ ] **Step 5: Commit**

```bash
git add internal/domain/workflow/engine.go internal/domain/workflow/engine_test.go
git commit -m "feat(workflow): add commitAndReopenSegment engine helper (#27)"
```

---

## Phase 3 — `COMMIT_BEFORE_DISPATCH` execution branch

The branch lives in `executeProcessors` (engine.go:533). Adding it requires plumbing the *current* TX ID through the dispatch loop so subsequent iterations see the new TX_post. This is where the engine's per-call mutability of `txID` begins.

### Task 5: Implement the `COMMIT_BEFORE_DISPATCH` branch in `executeProcessors` (false branch only)

The default `startNewTxOnDispatch=false` is the easier branch and the primary fault-line-3 mitigation. Implement and test it first.

**Files:**
- Modify: `internal/domain/workflow/engine.go`
- Modify: `internal/domain/workflow/engine_test.go`

- [ ] **Step 1: Write the failing test**

In `engine_test.go` add:

```go
func TestEngine_CommitBeforeDispatch_FalseBranch_HappyPath(t *testing.T) {
	// Single-processor cascade with COMMIT_BEFORE_DISPATCH.
	// Asserts: TX_pre commits with entity in S_pre; processor runs with no
	// tx context; TX_post commits with entity in S_post; cascade completes.

	ctx := context.Background()
	fixture := newEngineFixture(t) // existing helper; produces engine, stores, factory
	defer fixture.cleanup()

	// Workflow: S1 --(t1: COMMIT_BEFORE_DISPATCH proc)--> S2
	wf := spi.WorkflowDefinition{
		Name: "wf",
		States: []spi.StateDefinition{
			{Name: "S1", Transitions: []spi.TransitionDefinition{
				{Name: "t1", Next: "S2", Automated: true, Processors: []spi.ProcessorDefinition{
					{Type: "EXTERNAL", Name: "p1", ExecutionMode: "COMMIT_BEFORE_DISPATCH"},
				}},
			}},
			{Name: "S2"},
		},
	}
	fixture.installWorkflow(wf)

	// Configure the fake external processor to record the tx context it
	// was called with and return a payload mutation.
	var capturedTxID string
	fixture.fakeExtProc.dispatchFn = func(ctx context.Context, e *spi.Entity, p spi.ProcessorDefinition, _, _, txID string) (*spi.Entity, error) {
		capturedTxID = txID
		// Assert no transaction is open during dispatch in the false branch.
		if got := spi.GetTransaction(ctx); got != nil {
			t.Errorf("expected no transaction in dispatch ctx for false branch, got txID=%s", got.ID)
		}
		mod := *e
		mod.Data = []byte(`{"x":42}`)
		return &mod, nil
	}

	// Drive: handler-style Begin → Execute → expect engine to do its own commits.
	txID, txCtx, _ := fixture.txMgr.Begin(ctx)
	entity := &spi.Entity{Meta: spi.EntityMeta{ID: "e1", State: "S1"}, Data: []byte(`{"x":1}`)}
	res, err := fixture.engine.Execute(txCtx, entity, "")
	if err != nil { t.Fatalf("Execute: %v", err) }

	if entity.Meta.State != "S2" {
		t.Errorf("final state = %q, want S2", entity.Meta.State)
	}
	if string(entity.Data) != `{"x":42}` {
		t.Errorf("data = %s, want processor result", entity.Data)
	}
	if capturedTxID == "" {
		t.Errorf("processor was not dispatched")
	}
	_ = txID; _ = res // engine drove its own commit; original txID is closed.

	// Verify durability: the entity is readable in S2 from a fresh TX.
	rTxID, rCtx, _ := fixture.txMgr.Begin(ctx)
	got, _ := fixture.entityStore.Get(rCtx, "e1")
	fixture.txMgr.Rollback(rCtx, rTxID)
	if got.Meta.State != "S2" {
		t.Errorf("durable state = %q, want S2", got.Meta.State)
	}
}
```

(`newEngineFixture` likely needs a small extension to expose `fakeExtProc.dispatchFn` and `txMgr`. Add helpers as needed in the test file's existing fixture builder — keep them confined to test code.)

- [ ] **Step 2: Run test, verify it fails**

Run: `go test ./internal/domain/workflow/ -run TestEngine_CommitBeforeDispatch_FalseBranch_HappyPath -v`

Expected: FAIL — engine doesn't recognize `COMMIT_BEFORE_DISPATCH` yet, falls through to default (SYNC) branch.

- [ ] **Step 3: Implement `executeCommitBeforeDispatch` and wire it into the switch**

In `engine.go`, add the new method:

```go
// executeCommitBeforeDispatch implements processor execution mode
// COMMIT_BEFORE_DISPATCH. The cascade's parent transaction (TX_pre) is
// committed first; the processor is dispatched with no transaction
// context (default) or with TX_post's token (startNewTxOnDispatch=true);
// the result is applied via CompareAndSave against T_pre.
//
// Returns the new transaction's ID and context — the caller MUST replace
// its txID/ctx with these values to continue the cascade in TX_post.
//
// Per spec §3, §10.3: in the startNewTxOnDispatch=true branch, processors
// must not save the cascade-anchor entity themselves and also return
// mutations for it (last-writer-wins inside TX_post's buffer).
func (e *Engine) executeCommitBeforeDispatch(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition, workflow, transition, txID string) (newCtx context.Context, newTxID string, err error) {
	if e.extProc == nil {
		// No external processing configured — degenerate to a no-op
		// segment commit so the cascade can continue from current state.
		newTxID, newCtx, err = e.commitAndReopenSegment(ctx, entity, txID)
		return
	}

	startNewTx := readStartNewTxOnDispatch(proc) // helper added per Task 1's chosen carrier

	tPre := txID
	if !startNewTx {
		// (a) startNewTxOnDispatch=false: commit, dispatch outside any TX, then begin TX_post.
		_, _, err = e.commitAndReopenSegment(ctx, entity, txID)
		if err != nil {
			return nil, "", err
		}
		newTxID, newCtx, err = e.txMgr.Begin(context.WithoutCancel(ctx)) // detach from TX_pre's ctx
		if err != nil {
			return nil, "", fmt.Errorf("commit-before-dispatch: begin TX_post: %w", err)
		}

		// Dispatch with NO tx token in ctx.
		dispatchCtx := context.WithoutCancel(ctx)
		modified, dispatchErr := e.extProc.DispatchProcessor(dispatchCtx, entity, proc, workflow, transition, "")
		if dispatchErr != nil {
			// Rollback TX_post and surface the error.
			e.txMgr.Rollback(newCtx, newTxID)
			return nil, "", dispatchErr
		}

		// Apply result via CAS against tPre.
		if modified != nil && modified.Data != nil {
			entity.Data = modified.Data
		}
		es, err2 := e.factory.EntityStore(newCtx)
		if err2 != nil {
			e.txMgr.Rollback(newCtx, newTxID)
			return nil, "", fmt.Errorf("commit-before-dispatch: get entity store for CAS: %w", err2)
		}
		if _, err2 = es.CompareAndSave(newCtx, entity, tPre); err2 != nil {
			e.txMgr.Rollback(newCtx, newTxID)
			return nil, "", err2 // ErrConflict bubbles to caller
		}
		return newCtx, newTxID, nil
	}

	// (b) startNewTxOnDispatch=true: commit TX_pre, begin TX_post, dispatch with TX_post token.
	if _, _, err = e.commitAndReopenSegment(ctx, entity, txID); err != nil {
		return nil, "", err
	}
	newTxID, newCtx, err = e.txMgr.Begin(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("commit-before-dispatch: begin TX_post: %w", err)
	}

	modified, dispatchErr := e.extProc.DispatchProcessor(newCtx, entity, proc, workflow, transition, newTxID)
	if dispatchErr != nil {
		e.txMgr.Rollback(newCtx, newTxID)
		return nil, "", dispatchErr
	}

	if modified != nil && modified.Data != nil {
		entity.Data = modified.Data
	}
	es, err2 := e.factory.EntityStore(newCtx)
	if err2 != nil {
		e.txMgr.Rollback(newCtx, newTxID)
		return nil, "", fmt.Errorf("commit-before-dispatch: get entity store for CAS: %w", err2)
	}
	if _, err2 = es.CompareAndSave(newCtx, entity, tPre); err2 != nil {
		e.txMgr.Rollback(newCtx, newTxID)
		return nil, "", err2
	}
	return newCtx, newTxID, nil
}

// readStartNewTxOnDispatch reads the boolean flag from the carrier chosen in Task 1.
// If the chosen strategy is option (a) (SPI struct field), this becomes:
//   func readStartNewTxOnDispatch(p spi.ProcessorDefinition) bool {
//       return p.StartNewTxOnDispatch != nil && *p.StartNewTxOnDispatch
//   }
// If option (b) (Config map), this becomes:
//   func readStartNewTxOnDispatch(p spi.ProcessorDefinition) bool {
//       v, ok := p.Config["startNewTxOnDispatch"].(bool)
//       return ok && v
//   }
// Implement per the Task-1 decision.
func readStartNewTxOnDispatch(p spi.ProcessorDefinition) bool {
	// Implementation per Task 1's chosen strategy.
	panic("implement per Task 1 decision")
}
```

Then thread the new mode through `executeProcessors`. The challenge: the existing `executeProcessors` runs in one TX; the new branch needs to swap TX mid-loop. Refactor the loop to track a mutable `currentTxID` and `currentCtx`:

```go
func (e *Engine) executeProcessors(ctx context.Context, processors []spi.ProcessorDefinition, entity *spi.Entity, auditStore spi.StateMachineAuditStore, workflow string, transition string, txID string) (newCtx context.Context, newTxID string, err error) {
	if len(processors) == 0 {
		return ctx, txID, nil
	}

	names := make([]string, len(processors))
	for i, p := range processors {
		names[i] = p.Name
	}
	e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, entity.Meta.State,
		spi.SMEventProcessingPaused,
		fmt.Sprintf("Paused for processors: %v", names), nil)

	currentCtx := ctx
	currentTxID := txID

	for _, proc := range processors {
		var success bool
		var procErr error

		switch proc.ExecutionMode {
		case "ASYNC_NEW_TX":
			procErr = e.executeAsyncNewTx(currentCtx, entity, proc, workflow, transition, currentTxID)
			success = procErr == nil
			if procErr != nil {
				slog.Warn("ASYNC_NEW_TX processor failed, continuing pipeline",
					"pkg", "workflow", "processor", proc.Name, "error", procErr)
			}

		case "COMMIT_BEFORE_DISPATCH":
			var nCtx context.Context
			var nTxID string
			nCtx, nTxID, procErr = e.executeCommitBeforeDispatch(currentCtx, entity, proc, workflow, transition, currentTxID)
			success = procErr == nil
			if procErr == nil {
				currentCtx = nCtx
				currentTxID = nTxID
			}

		default: // SYNC, ASYNC_SAME_TX — both inline in caller's transaction.
			procErr = e.executeSyncProcessor(currentCtx, entity, proc, workflow, transition, currentTxID)
			success = procErr == nil
		}

		auditData := map[string]any{
			"success": success,
			"mode":    proc.ExecutionMode,
		}
		if procErr != nil {
			auditData["error"] = procErr.Error()
		}
		e.recordEvent(auditStore, currentCtx, entity.Meta.ID, currentTxID, entity.Meta.State,
			spi.SMEventStateProcessResult,
			fmt.Sprintf("Processor %q completed", proc.Name), auditData)

		// Failure kills the pipeline for SYNC, ASYNC_SAME_TX, COMMIT_BEFORE_DISPATCH.
		if procErr != nil && proc.ExecutionMode != "ASYNC_NEW_TX" {
			return currentCtx, currentTxID, fmt.Errorf("processor %s failed: %w", proc.Name, procErr)
		}
	}
	return currentCtx, currentTxID, nil
}
```

The signature change (now returns `(ctx, txID, error)`) propagates to callers — `attemptTransition` (engine.go:353) and the `cascadeAutomated` invocation site (engine.go:468). Update both to thread the new `(ctx, txID)` through the cascade:

```go
// attemptTransition updates to:
func (e *Engine) attemptTransition(ctx context.Context, entity *spi.Entity, wf *spi.WorkflowDefinition, transitionName string, auditStore spi.StateMachineAuditStore, txID string) (context.Context, string, error) {
	// ... existing prelude ...
	newCtx, newTxID, err := e.executeProcessors(ctx, transition.Processors, entity, auditStore, wf.Name, transition.Name, txID)
	if err != nil {
		return newCtx, newTxID, err
	}
	// ... rest of function, using newCtx/newTxID ...
	return newCtx, newTxID, nil
}

// cascadeAutomated body updates to thread (ctx, txID) through each iteration's
// executeProcessors call: replace currentCtx, currentTxID before each pass.
```

Update `Execute`, `ManualTransition`, `Loopback` to also accept the returned `(ctx, txID)` from `cascadeAutomated` and pass them onward (for handlers that may need to commit TX_post themselves — see Task 7).

The full diff is large; the Single-Segment Regression test (Task 6) is what proves you didn't break anything. Keep the audit-event placement: `SMEventProcessingPaused` at line 543 stays under TX_pre's `txID` (it's already recorded there before any processor fires); `SMEventStateProcessResult` at line 574 now uses `currentTxID` so it lands in the correct segment's TX. **Important: per spec §8 audit-txID labelling decision, the `txID` field on every event is the cascade-entry txID, NOT the segment's currentTxID.** Update the `recordEvent` call at line 574 to keep the original `txID` parameter, not `currentTxID`:

```go
e.recordEvent(auditStore, currentCtx, entity.Meta.ID, txID /* cascade-entry */, entity.Meta.State,
    spi.SMEventStateProcessResult, ...)
```

The event lands physically in `currentCtx`'s TX, but the `transactionId` field carries the cascade-entry txID for client-correlation continuity.

- [ ] **Step 4: Run test, verify it passes**

Run: `go test ./internal/domain/workflow/ -run TestEngine_CommitBeforeDispatch_FalseBranch_HappyPath -v`

Expected: PASS.

Then run the full workflow package:

Run: `go test ./internal/domain/workflow/ -v`

Expected: PASS for everything. If any existing test fails, investigate — most likely fix is updating the calling site's expectation to handle the new return signature. **Do not bypass the test by removing assertions; fix the contract.**

- [ ] **Step 5: Commit**

```bash
git add internal/domain/workflow/engine.go internal/domain/workflow/engine_test.go
git commit -m "feat(workflow): add COMMIT_BEFORE_DISPATCH execution branch (false flag) (#27)"
```

### Task 6: Single-segment regression bound (cases without `COMMIT_BEFORE_DISPATCH`)

**Files:**
- Modify: `internal/domain/workflow/engine_test.go`

- [ ] **Step 1: Write the test asserting unchanged single-segment behaviour**

```go
func TestEngine_SingleSegment_RegressionBound(t *testing.T) {
	// A cascade with no COMMIT_BEFORE_DISPATCH processors must:
	//   - leave the txID unchanged (same TX from start to end of cascade)
	//   - commit zero engine-side commits (handler does the commit)
	//   - emit exactly the audit events it does today
	// This is the regression bound for Phase 2/3 changes.

	ctx := context.Background()
	fixture := newEngineFixture(t)
	defer fixture.cleanup()

	wf := spi.WorkflowDefinition{
		Name: "wf",
		States: []spi.StateDefinition{
			{Name: "S1", Transitions: []spi.TransitionDefinition{
				{Name: "t1", Next: "S2", Automated: true, Processors: []spi.ProcessorDefinition{
					{Type: "EXTERNAL", Name: "p1", ExecutionMode: "SYNC"},
				}},
			}},
			{Name: "S2", Transitions: []spi.TransitionDefinition{
				{Name: "t2", Next: "S3", Automated: true},
			}},
			{Name: "S3"},
		},
	}
	fixture.installWorkflow(wf)
	fixture.fakeExtProc.dispatchFn = func(_ context.Context, e *spi.Entity, _ spi.ProcessorDefinition, _, _, _ string) (*spi.Entity, error) {
		return e, nil
	}

	// Track engine-side commits (should be zero).
	commitCount := 0
	fixture.txMgr.WrapCommit(func(orig func(context.Context, string) error) func(context.Context, string) error {
		return func(ctx context.Context, txID string) error {
			commitCount++
			return orig(ctx, txID)
		}
	})

	txID, txCtx, _ := fixture.txMgr.Begin(ctx)
	entity := &spi.Entity{Meta: spi.EntityMeta{ID: "e1", State: "S1"}, Data: []byte(`{"x":1}`)}
	_, err := fixture.engine.Execute(txCtx, entity, "")
	if err != nil { t.Fatalf("Execute: %v", err) }

	if entity.Meta.State != "S3" {
		t.Errorf("final state = %q, want S3", entity.Meta.State)
	}
	if commitCount != 0 {
		t.Errorf("engine-side commits = %d, want 0 (handler-driven only)", commitCount)
	}

	// Now finish the handler-side TX so subsequent reads work.
	if _, err := fixture.entityStore.Save(txCtx, entity); err != nil {
		t.Fatalf("handler Save: %v", err)
	}
	if err := fixture.txMgr.Commit(txCtx, txID); err != nil {
		t.Fatalf("handler Commit: %v", err)
	}
}
```

(If the test fixture doesn't expose `WrapCommit`, add a small wrapper in the fixture builder. Goal: confirm the engine doesn't auto-commit anything for non-COMMIT_BEFORE_DISPATCH cascades.)

- [ ] **Step 2: Run test, verify it passes**

Run: `go test ./internal/domain/workflow/ -run TestEngine_SingleSegment_RegressionBound -v`

Expected: PASS. If this fails, the engine is committing where it shouldn't be — fix the COMMIT_BEFORE_DISPATCH branch to only commit when a `COMMIT_BEFORE_DISPATCH` processor is actually encountered.

- [ ] **Step 3: Commit**

```bash
git add internal/domain/workflow/engine_test.go
git commit -m "test(workflow): single-segment cascade regression bound (#27)"
```

### Task 7: CAS-conflict path (case 2)

**Files:**
- Modify: `internal/domain/workflow/engine_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestEngine_CommitBeforeDispatch_CASConflict_BubblesAsErrConflict(t *testing.T) {
	// Concurrent committer between TX_pre.Commit and TX_post's CAS.
	ctx := context.Background()
	fixture := newEngineFixture(t)
	defer fixture.cleanup()

	wf := spi.WorkflowDefinition{
		Name: "wf",
		States: []spi.StateDefinition{
			{Name: "S1", Transitions: []spi.TransitionDefinition{
				{Name: "t1", Next: "S2", Automated: true, Processors: []spi.ProcessorDefinition{
					{Type: "EXTERNAL", Name: "p1", ExecutionMode: "COMMIT_BEFORE_DISPATCH"},
				}},
			}},
			{Name: "S2"},
		},
	}
	fixture.installWorkflow(wf)

	// Hook the dispatch to perform a concurrent commit on the same entity from
	// a different TX *before* returning. This simulates an external manual
	// transition that lands while the dispatch is in flight.
	fixture.fakeExtProc.dispatchFn = func(ctx context.Context, e *spi.Entity, _ spi.ProcessorDefinition, _, _, _ string) (*spi.Entity, error) {
		// Open a fresh TX, mutate the entity, commit. Bumps txID on the row.
		conTxID, conCtx, _ := fixture.txMgr.Begin(context.Background())
		conE, _ := fixture.entityStore.Get(conCtx, e.Meta.ID)
		conE.Data = []byte(`{"interloper":true}`)
		fixture.entityStore.Save(conCtx, conE)
		fixture.txMgr.Commit(conCtx, conTxID)
		// Now return our intended result; engine's CAS will fail.
		mod := *e
		mod.Data = []byte(`{"x":42}`)
		return &mod, nil
	}

	txID, txCtx, _ := fixture.txMgr.Begin(ctx)
	entity := &spi.Entity{Meta: spi.EntityMeta{ID: "e1", State: "S1"}, Data: []byte(`{"x":1}`)}
	if _, err := fixture.entityStore.Save(txCtx, entity); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	_, err := fixture.engine.Execute(txCtx, entity, "")
	if err == nil {
		t.Fatalf("expected ErrConflict, got nil")
	}
	if !errors.Is(err, common.ErrConflict) { // or whatever sentinel the SPI exposes
		t.Fatalf("expected ErrConflict, got: %v", err)
	}

	// Verify the durable state is the interloper's, not the cascade's.
	rTxID, rCtx, _ := fixture.txMgr.Begin(ctx)
	got, _ := fixture.entityStore.Get(rCtx, "e1")
	fixture.txMgr.Rollback(rCtx, rTxID)
	if string(got.Data) != `{"interloper":true}` {
		t.Errorf("durable data = %s, want interloper's", got.Data)
	}
	_ = txID
}
```

- [ ] **Step 2: Run test, verify it fails (or already passes — Task 5 may have wired this)**

Run: `go test ./internal/domain/workflow/ -run TestEngine_CommitBeforeDispatch_CASConflict -v`

Expected: PASS if Task 5's CAS bubbling is correct; FAIL otherwise.

- [ ] **Step 3: If FAIL, fix the CAS bubbling in `executeCommitBeforeDispatch`**

Ensure the function returns `errors.New(...)` wrapping or directly returning `ErrConflict` (or whatever the SPI's CAS conflict sentinel is — `spi.ErrConflict` per the SPI `persistence.go:errors`). Do not transform or swallow.

- [ ] **Step 4: Run test, verify it passes**

Run: `go test ./internal/domain/workflow/ -run TestEngine_CommitBeforeDispatch_CASConflict -v`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/domain/workflow/engine_test.go internal/domain/workflow/engine.go
git commit -m "test(workflow): COMMIT_BEFORE_DISPATCH CAS-conflict surfaces ErrConflict (#27)"
```

### Task 8: `=true` branch happy path (case 3)

**Files:**
- Modify: `internal/domain/workflow/engine_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestEngine_CommitBeforeDispatch_TrueBranch_HappyPath(t *testing.T) {
	// startNewTxOnDispatch=true: processor receives TX_post token; processor
	// performs CRUD on a *different* entity via that TX; engine applies result
	// for the cascade-anchor; both writes commit atomically when TX_post commits.

	ctx := context.Background()
	fixture := newEngineFixture(t)
	defer fixture.cleanup()

	tt := true
	wf := spi.WorkflowDefinition{
		Name: "wf",
		States: []spi.StateDefinition{
			{Name: "S1", Transitions: []spi.TransitionDefinition{
				{Name: "t1", Next: "S2", Automated: true, Processors: []spi.ProcessorDefinition{
					{Type: "EXTERNAL", Name: "p1", ExecutionMode: "COMMIT_BEFORE_DISPATCH",
						StartNewTxOnDispatch: &tt},
				}},
			}},
			{Name: "S2"},
		},
	}
	fixture.installWorkflow(wf)

	fixture.fakeExtProc.dispatchFn = func(ctx context.Context, e *spi.Entity, _ spi.ProcessorDefinition, _, _, txID string) (*spi.Entity, error) {
		// Assert TX is open during dispatch.
		got := spi.GetTransaction(ctx)
		if got == nil || got.ID != txID {
			t.Errorf("expected ctx tx = %q, got %v", txID, got)
		}
		// Processor writes a *different* entity via the supplied TX.
		other := &spi.Entity{Meta: spi.EntityMeta{ID: "e2", State: "ready"}, Data: []byte(`{"y":1}`)}
		if _, err := fixture.entityStore.Save(ctx, other); err != nil {
			return nil, err
		}
		mod := *e
		mod.Data = []byte(`{"x":42}`)
		return &mod, nil
	}

	txID, txCtx, _ := fixture.txMgr.Begin(ctx)
	entity := &spi.Entity{Meta: spi.EntityMeta{ID: "e1", State: "S1"}, Data: []byte(`{"x":1}`)}
	if _, err := fixture.entityStore.Save(txCtx, entity); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	_, err := fixture.engine.Execute(txCtx, entity, "")
	if err != nil { t.Fatalf("Execute: %v", err) }
	_ = txID

	// Verify both entities are durable.
	rTxID, rCtx, _ := fixture.txMgr.Begin(ctx)
	defer fixture.txMgr.Rollback(rCtx, rTxID)
	a, _ := fixture.entityStore.Get(rCtx, "e1")
	b, _ := fixture.entityStore.Get(rCtx, "e2")
	if a.Meta.State != "S2" {
		t.Errorf("anchor state = %q, want S2", a.Meta.State)
	}
	if string(b.Data) != `{"y":1}` {
		t.Errorf("processor's secondary entity not committed: %s", b.Data)
	}
}
```

- [ ] **Step 2: Run test, verify it fails (if branch not yet implemented) or passes (if Task 5's implementation already handled it)**

Run: `go test ./internal/domain/workflow/ -run TestEngine_CommitBeforeDispatch_TrueBranch_HappyPath -v`

If FAIL: Task 5's `executeCommitBeforeDispatch` `=true` branch needs the dispatch to use `newCtx` (with TX state). Verify the implementation routes through.

- [ ] **Step 3: Make it pass**

If implementation gap: in `executeCommitBeforeDispatch`'s `=true` branch (Task 5 step 3), verify the dispatch passes `newTxID` and `newCtx` (which is the TX-bearing context). Adjust if needed.

- [ ] **Step 4: Verify pass**

Run: `go test ./internal/domain/workflow/ -run TestEngine_CommitBeforeDispatch_TrueBranch -v`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/domain/workflow/engine_test.go internal/domain/workflow/engine.go
git commit -m "test(workflow): COMMIT_BEFORE_DISPATCH startNewTxOnDispatch=true happy path (#27)"
```

### Task 9: `=true` branch double-write (case 4)

**Files:**
- Modify: `internal/domain/workflow/engine_test.go`

- [ ] **Step 1: Write the test documenting the LWW behaviour**

```go
func TestEngine_CommitBeforeDispatch_TrueBranch_DoubleWriteIsLastWriterWins(t *testing.T) {
	// Documents the existing best-practice violation per spec §10.3:
	// processor writes the cascade-anchor entity via TX_post AND returns
	// mutations for it. Engine's apply-result overwrites the buffer entry.
	// LWW = engine's apply-result wins. Test documents, does not endorse.

	ctx := context.Background()
	fixture := newEngineFixture(t)
	defer fixture.cleanup()

	tt := true
	wf := spi.WorkflowDefinition{
		Name: "wf",
		States: []spi.StateDefinition{
			{Name: "S1", Transitions: []spi.TransitionDefinition{
				{Name: "t1", Next: "S2", Automated: true, Processors: []spi.ProcessorDefinition{
					{Type: "EXTERNAL", Name: "p1", ExecutionMode: "COMMIT_BEFORE_DISPATCH",
						StartNewTxOnDispatch: &tt},
				}},
			}},
			{Name: "S2"},
		},
	}
	fixture.installWorkflow(wf)

	fixture.fakeExtProc.dispatchFn = func(ctx context.Context, e *spi.Entity, _ spi.ProcessorDefinition, _, _, _ string) (*spi.Entity, error) {
		// Processor writes the SAME entity it's processing.
		violation := *e
		violation.Data = []byte(`{"processor_wrote":true}`)
		fixture.entityStore.Save(ctx, &violation)
		// And returns conflicting mutations.
		mod := *e
		mod.Data = []byte(`{"engine_apply":true}`)
		return &mod, nil
	}

	txID, txCtx, _ := fixture.txMgr.Begin(ctx)
	entity := &spi.Entity{Meta: spi.EntityMeta{ID: "e1", State: "S1"}, Data: []byte(`{"x":0}`)}
	fixture.entityStore.Save(txCtx, entity)
	_, err := fixture.engine.Execute(txCtx, entity, "")
	if err != nil { t.Fatalf("Execute: %v", err) }
	_ = txID

	rTxID, rCtx, _ := fixture.txMgr.Begin(ctx)
	defer fixture.txMgr.Rollback(rCtx, rTxID)
	got, _ := fixture.entityStore.Get(rCtx, "e1")
	if string(got.Data) != `{"engine_apply":true}` {
		t.Errorf("LWW expected engine apply-result to win, got: %s", got.Data)
	}
}
```

- [ ] **Step 2: Run, verify it passes (no implementation change expected)**

Run: `go test ./internal/domain/workflow/ -run TestEngine_CommitBeforeDispatch_TrueBranch_DoubleWrite -v`

Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/domain/workflow/engine_test.go
git commit -m "test(workflow): document LWW for processor double-write in =true mode (#27)"
```

### Task 10: Audit-event placement (case 13)

**Files:**
- Modify: `internal/domain/workflow/engine_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestEngine_CommitBeforeDispatch_AuditEventPlacement(t *testing.T) {
	// Asserts SMEventProcessingPaused lands in TX_pre's audit (durable on
	// TX_pre.Commit), and SMEventStateProcessResult lands in TX_post's audit.
	// Both events carry the cascade-entry txID as their transactionID label
	// (per spec §8 audit-txID labelling decision).

	ctx := context.Background()
	fixture := newEngineFixture(t)
	defer fixture.cleanup()

	wf := spi.WorkflowDefinition{
		Name: "wf",
		States: []spi.StateDefinition{
			{Name: "S1", Transitions: []spi.TransitionDefinition{
				{Name: "t1", Next: "S2", Automated: true, Processors: []spi.ProcessorDefinition{
					{Type: "EXTERNAL", Name: "p1", ExecutionMode: "COMMIT_BEFORE_DISPATCH"},
				}},
			}},
			{Name: "S2"},
		},
	}
	fixture.installWorkflow(wf)
	fixture.fakeExtProc.dispatchFn = func(_ context.Context, e *spi.Entity, _ spi.ProcessorDefinition, _, _, _ string) (*spi.Entity, error) {
		return e, nil
	}

	cascadeEntryTxID, txCtx, _ := fixture.txMgr.Begin(ctx)
	entity := &spi.Entity{Meta: spi.EntityMeta{ID: "e1", State: "S1"}, Data: []byte(`{}`)}
	fixture.entityStore.Save(txCtx, entity)
	_, _ = fixture.engine.Execute(txCtx, entity, "")

	events := fixture.auditStore.AllEventsForEntity("e1")

	// Expectation 1: SMEventProcessingPaused recorded under cascadeEntryTxID and
	// physically committed in TX_pre (i.e., visible after TX_pre.Commit).
	var paused, result *spi.StateMachineEvent
	for i := range events {
		if events[i].EventType == spi.SMEventProcessingPaused {
			paused = &events[i]
		}
		if events[i].EventType == spi.SMEventStateProcessResult {
			result = &events[i]
		}
	}
	if paused == nil { t.Fatalf("missing SMEventProcessingPaused") }
	if result == nil { t.Fatalf("missing SMEventStateProcessResult") }
	if paused.TransactionID != cascadeEntryTxID {
		t.Errorf("paused.TransactionID = %q, want cascade-entry %q", paused.TransactionID, cascadeEntryTxID)
	}
	if result.TransactionID != cascadeEntryTxID {
		t.Errorf("result.TransactionID = %q, want cascade-entry %q", result.TransactionID, cascadeEntryTxID)
	}
}
```

(`fixture.auditStore.AllEventsForEntity` is a helper to collect events for the entity; add to fixture if not present.)

- [ ] **Step 2: Run test, verify it passes**

Run: `go test ./internal/domain/workflow/ -run TestEngine_CommitBeforeDispatch_AuditEventPlacement -v`

Expected: PASS — Task 5 step 3 already kept `txID` (cascade-entry) on the `recordEvent` calls. If FAIL because both events use `currentTxID`, fix the recordEvent call sites to use the original `txID` parameter for the `transactionId` field.

- [ ] **Step 3: Commit**

```bash
git add internal/domain/workflow/engine_test.go
git commit -m "test(workflow): audit-event labelling under COMMIT_BEFORE_DISPATCH (#27)"
```

### Task 11: Validator integration test for `startNewTxOnDispatch` rejection (case 12)

**Files:**
- Modify: `internal/domain/workflow/validate_test.go` (already covered in Task 3; add an integration-style test that drives via the actual workflow registration path if one exists)

- [ ] **Step 1: Locate the workflow registration path**

Run: `grep -rn "validateWorkflows\|RegisterWorkflow\|SaveWorkflows" --include="*.go" internal/domain/workflow/ internal/domain/model/ | head -10`

- [ ] **Step 2: If a registration path exists, write an integration test asserting the validator rejection surfaces with a clear error code through that path**

Adapt to the actual surface — if registration is via `internal/domain/workflow/Service.RegisterWorkflows(...)`, write a test that calls it with a malformed workflow and asserts the returned error contains `startNewTxOnDispatch`.

- [ ] **Step 3: Run, fix if needed, commit**

```bash
git add internal/domain/workflow/validate_test.go
git commit -m "test(workflow): registration rejects startNewTxOnDispatch misuse (#27)"
```

---

## Phase 4 — Handler refactor

The engine now drives its own commit/begin during a `COMMIT_BEFORE_DISPATCH` cascade. The handler must hand `txMgr` (already injected) and `If-Match` to the engine, AND not commit the engine's TX_post itself if the engine has already committed and reopened mid-cascade. The single-segment regression bound (Task 6) ensures non-CBD cascades still see today's handler-driven commit.

The handler does NOT need to know whether a cascade *will* contain a `COMMIT_BEFORE_DISPATCH` processor. It just needs to: (a) commit at the end whichever TX is current after the engine returns; (b) propagate `If-Match` so the engine can apply it to the first segment's flush.

### Task 12: Engine signals current TX through return values

**Files:**
- Modify: `internal/domain/workflow/engine.go`

- [ ] **Step 1: Already done in Task 5 step 3** — `Execute`, `ManualTransition`, `Loopback` should now return `(ctx, txID, *ExecutionResult, error)` or carry the new TX state through `*ExecutionResult`. Confirm.

If not yet done: extend `*spi.ExecutionResult` (or a wrapper in `internal/domain/workflow/`) to include `FinalTxID string` and `FinalCtx context.Context`. Engine sets these. Callers consume them. Update all three entry-points.

- [ ] **Step 2: Test that return values reflect the post-segment TX**

```go
func TestEngine_Execute_ReturnsFinalTxOnSegmentedCascade(t *testing.T) {
	// After a cascade with one COMMIT_BEFORE_DISPATCH segment, Execute returns
	// the FINAL segment's txID (TX_post), not the entry txID.
	// ... fixture setup as in Task 5 ...
	entryTxID, txCtx, _ := fixture.txMgr.Begin(ctx)
	res, err := fixture.engine.Execute(txCtx, entity, "")
	if err != nil { t.Fatalf("Execute: %v", err) }
	if res.FinalTxID == entryTxID {
		t.Errorf("FinalTxID = entryTxID = %q; expected post-segment txID", entryTxID)
	}
}
```

- [ ] **Step 3: Run, fix, commit**

```bash
git add internal/domain/workflow/engine.go internal/domain/workflow/engine_test.go
git commit -m "feat(workflow): engine returns final-segment tx context to handler (#27)"
```

### Task 13: Handler refactor — UpdateEntity

**Files:**
- Modify: `internal/domain/entity/service.go`

- [ ] **Step 1: Write the failing test (or extend an existing UpdateEntity test)**

In `internal/e2e/` (create `commit_before_dispatch_test.go` if not present), add a test that:
1. Configures a workflow with a `COMMIT_BEFORE_DISPATCH` processor.
2. Calls `PUT /api/entity/{id}/{transition}` with `If-Match`.
3. Expects 200 OK and the entity at the post-cascade state.

This is part of Task 16 below; proceed there for the E2E surface. For Task 13, extend the unit-level test in `internal/domain/entity/service_test.go` if one exists, or rely on E2E.

- [ ] **Step 2: Refactor `service.UpdateEntity` (engine.go:867-1014)**

Goal: when the engine returns a `FinalTxID` different from the handler's `txID`, the handler's `Commit(txID)` call is wrong (that TX is already committed). Use `res.FinalTxID` and `res.FinalCtx`:

```go
// Before (today, line ~990):
if _, err := h.entityStore.Save(txCtx, updated); err != nil { ... }
if err := h.txMgr.Commit(txCtx, txID); err != nil { ... }

// After:
res, err := h.engine.ManualTransition(txCtx, updated, input.Transition)
if err != nil { ... }
// If the engine segmented, res.FinalCtx and res.FinalTxID are the open TX
// awaiting the handler's final commit. The engine has already done all
// per-segment writes; the handler does nothing more than commit.
finalCtx, finalTxID := res.FinalCtx, res.FinalTxID
if err := h.txMgr.Commit(finalCtx, finalTxID); err != nil {
    // Rollback semantics: the engine has already committed earlier
    // segments; there is nothing the handler can rollback for those.
    // Surface the error.
    return nil, mapTxCommitErr(err)
}
```

The handler's pre-engine `entityStore.Save` of the updated entity (the line that flushes the cascade-input state — line 1001 area) becomes the **engine's first-segment Save**. Drop it from the handler:

```go
// Before:
if input.IfMatch != "" {
    if _, err := h.entityStore.CompareAndSave(txCtx, updated, input.IfMatch); err != nil { ... }
} else {
    if _, err := h.entityStore.Save(txCtx, updated); err != nil { ... }
}
if _, err := h.engine.ManualTransition(txCtx, updated, input.Transition); err != nil { ... }

// After:
res, err := h.engine.ManualTransitionWithIfMatch(txCtx, updated, input.Transition, input.IfMatch)
```

Add `ManualTransitionWithIfMatch` (or equivalent) to the engine — it differs from `ManualTransition` only in that the engine performs the *first* entity flush as `CompareAndSave(updated, expectedTxID=ifMatch)` instead of `Save(updated)`. The chained-CAS for subsequent segments is unchanged.

- [ ] **Step 3: Run all tests in domain/entity and e2e**

Run: `go test -short ./internal/domain/entity/... ./internal/e2e/... -v`

Expected: PASS for all existing tests. If any fail, the most likely cause is the Save-then-engine ordering changed; fix the call site, do not weaken assertions.

- [ ] **Step 4: Commit**

```bash
git add internal/domain/entity/service.go internal/domain/workflow/engine.go
git commit -m "refactor(entity): hand If-Match + tx ownership to engine (#27)"
```

### Task 14: Handler refactor — CreateEntity, UpdateEntityCollection, CreateEntityCollection

**Files:**
- Modify: `internal/domain/entity/service.go`

- [ ] **Step 1: Apply the same pattern as Task 13 to the other three handlers**

`CreateEntity` (line ~121), `UpdateEntityCollection` (~1020), `CreateEntityCollection` (~752). Each begins a TX, runs the engine, commits. After this task: each begins TX, hands txMgr+IfMatch to engine, commits whichever TX is current at the engine's return.

For collection variants: each item's cascade may have its own segments, so the loop tracks `currentCtx`/`currentTxID` per item. Carefully verify rollback semantics — if item N segments TX into TX_n_post, item N+1 must use TX_n_post for its own Begin? Or fresh? **Recommendation: each collection item is its own cascade; the handler begins a fresh TX per item**, so the segmenting only impacts the per-item commit, not the next item. Confirm by reading the existing collection handler structure.

- [ ] **Step 2: Run all tests**

Run: `go test -short ./internal/domain/entity/... ./internal/e2e/... -v`

Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/domain/entity/service.go
git commit -m "refactor(entity): apply tx-ownership handoff to remaining handlers (#27)"
```

### Task 15: If-Match propagation (case 9 from spec §16)

**Files:**
- Create: `internal/e2e/commit_before_dispatch_test.go` (or extend an existing E2E file)

- [ ] **Step 1: Write the failing E2E test**

```go
func TestE2E_CommitBeforeDispatch_StaleIfMatchAbortsBeforeDispatch(t *testing.T) {
	ts := startTestServer(t)
	defer ts.Close()

	// Install a workflow with a COMMIT_BEFORE_DISPATCH processor that records
	// every dispatch (so we can assert it was NOT called).
	ts.installWorkflow(`{... wf with COMMIT_BEFORE_DISPATCH on transition t1 ...}`)

	// Create entity, get its txID T0.
	resp := ts.POST("/api/entity/myModel/v1", `{"data":{"x":0}}`)
	t0 := resp.JSON("transactionId")

	// Stale If-Match: pretend a prior call's txID, but the entity is at T0.
	stale := "00000000-0000-0000-0000-000000000000" // not equal to T0

	// Attempt manual transition with stale If-Match.
	resp = ts.PUT("/api/entity/{id}/t1", `{"data":{"x":1}}`, "If-Match", stale)
	if resp.Status != 412 && resp.Status != 409 { // expected stale-precondition surface
		t.Fatalf("expected 412/409, got %d body=%s", resp.Status, resp.Body)
	}

	// CRITICAL: the dispatch fake must report ZERO calls.
	if ts.dispatchCallCount("p1") != 0 {
		t.Errorf("processor was dispatched despite stale If-Match — defeats the protection")
	}
}
```

- [ ] **Step 2: Run, verify it fails (likely — until If-Match is fully wired through)**

Run: `go test ./internal/e2e/ -run TestE2E_CommitBeforeDispatch_StaleIfMatch -v`

Expected: FAIL initially.

- [ ] **Step 3: Fix the engine's first-segment CompareAndSave path**

Verify Task 13's `ManualTransitionWithIfMatch` performs the first-segment flush as `CompareAndSave(entity, expectedTxID=ifMatch)`. If the entity's first-segment-Save is happening *after* the COMMIT_BEFORE_DISPATCH dispatch, that's the bug — move the first-segment flush to before any processor execution.

- [ ] **Step 4: Run, verify pass**

Run: `go test ./internal/e2e/ -run TestE2E_CommitBeforeDispatch_StaleIfMatch -v`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/e2e/commit_before_dispatch_test.go internal/domain/workflow/engine.go internal/domain/entity/service.go
git commit -m "fix(workflow): stale If-Match aborts before COMMIT_BEFORE_DISPATCH dispatch (#27)"
```

---

## Phase 5 — Remaining E2E coverage

The remaining cases from spec §16: 1, 7, 8, 10, 11, 14, 15. Some are already partially covered by unit tests in Phase 3; Phase 5 wraps them in E2E flows that exercise the full HTTP stack.

### Task 16: E2E happy path (case 1)

- [ ] **Step 1**: Write E2E test asserting full happy-path through HTTP, including response body shape (`transactionId` is final-segment's txID).
- [ ] **Step 2**: Run, fix, commit.

### Task 17: E2E multi-entity cascade (case 7)

- [ ] **Step 1**: Write E2E test where the COMMIT_BEFORE_DISPATCH processor mutates a secondary entity via TX_post; assert both entities durable.
- [ ] **Step 2**: Run, fix, commit.

### Task 18: E2E hot-entity concurrent cascades (case 8)

- [ ] **Step 1**: Two goroutines each fire a COMMIT_BEFORE_DISPATCH cascade on the same anchor; assert one wins, the other gets `409 retryable`, retries cleanly.
- [ ] **Step 2**: Run, fix, commit.

### Task 19: E2E concurrent search across segment boundary (case 10)

- [ ] **Step 1**: One goroutine drives a COMMIT_BEFORE_DISPATCH cascade with a slow processor; another goroutine `GET /api/entity/{name}/{ver}` during the wait window; assert pre-callout state visible in the search.
- [ ] **Step 2**: Run, fix, commit.

### Task 20: E2E single-segment regression bound (case 11)

- [ ] **Step 1**: Drive a non-CBD cascade end-to-end; assert response shape and entity-version-history row count are byte-for-byte identical to baseline.
- [ ] **Step 2**: Run, fix, commit.

### Task 21: E2E cluster mode TX_post pinning (case 14)

- [ ] **Step 1**: If multi-node cluster harness exists in `internal/e2e/`, drive a CBD cascade and assert via the txMgr registry that both segments' TXs were owned by the same node.
- [ ] **Step 2**: If cluster harness absent, mark the test `t.Skip("requires multi-node test harness")` with a TODO referencing this issue.
- [ ] **Step 3**: Commit.

### Task 22: E2E Loopback() entry-point coverage (case 15)

- [ ] **Step 1**: Trigger Loopback (probably via `GET /api/entity/{id}/transitions` followed by a POST or however the existing API surfaces Loopback) on an entity whose state has an outgoing automated transition with a COMMIT_BEFORE_DISPATCH processor; assert the cascade segments exactly as `Execute` would.
- [ ] **Step 2**: Run, fix, commit.

### Task 23: E2E engine-crash kill-simulation (cases 5, 6)

These require a way to interrupt the engine mid-segment. If the test harness supports a fault-injection hook (a fake processor that panics or deadlocks the goroutine), use it; otherwise mark `t.Skip` with a TODO.

- [ ] **Step 1**: Write the kill-simulation test (or skip with TODO).
- [ ] **Step 2**: Commit.

---

## Phase 6 — Documentation reconciliation

Per spec §15. Each subsection's edits are concrete; the line-citations are pre-edit estimates and may have drifted — re-verify at execute-time.

### Task 24: `cmd/cyoda/help/content/workflows.md`

**Files:**
- Modify: `cmd/cyoda/help/content/workflows.md`

- [ ] **Step 1**: Add the `COMMIT_BEFORE_DISPATCH` enum value to the executionMode list. Document the `startNewTxOnDispatch` flag, the idempotency requirement, and the no-double-write best-practice.

Use this exact paragraph as the no-double-write best-practice (not currently documented anywhere — add as a one-time correction):

```markdown
**Best-practice: a processor must not save the entity it is processing for.**
Processors with TX-callback access (SYNC, ASYNC_SAME_TX, COMMIT_BEFORE_DISPATCH
with startNewTxOnDispatch=true) can write the cascade-anchor entity via the
supplied transaction token, but if they do AND also return mutations for the
same entity in their result, the engine's apply-result will overwrite the
processor's intra-TX writes (last-writer-wins inside the transaction buffer).
Pick one path: let the engine apply the result, OR have the processor write
the entity itself and return no mutations for it.
```

- [ ] **Step 2**: Verify by `cyoda help workflows` rendering (if local CLI build is available) or `git diff`.

- [ ] **Step 3**: Commit.

```bash
git commit -m "docs(workflows): document COMMIT_BEFORE_DISPATCH and no-double-write best-practice (#27)"
```

### Task 25: `docs/ARCHITECTURE.md`

**Files:**
- Modify: `docs/ARCHITECTURE.md`

- [ ] **Step 1**: Re-verify each line citation in spec §15 against the current `docs/ARCHITECTURE.md`. Apply each edit per the spec's reconciliation list. The list is reproduced verbatim in spec §15 — drive from there.

- [ ] **Step 2**: Commit.

```bash
git commit -m "docs(architecture): reconcile with COMMIT_BEFORE_DISPATCH (#27)"
```

### Task 26: `docs/CONSISTENCY.md`

- [ ] **Step 1**: Apply spec §15 edits.
- [ ] **Step 2**: Commit.

### Task 27: `docs/CONCURRENCY.md`

- [ ] **Step 1**: Apply spec §15 edits.
- [ ] **Step 2**: Commit.

---

## Phase 7 — Final verification

### Task 28: Full test suite + race detector

- [ ] **Step 1**: Run unit + integration tests root module:

```bash
go test ./... -v
```

Expected: PASS.

- [ ] **Step 2**: Run plugin submodules:

```bash
make test-short-all
```

Expected: PASS.

- [ ] **Step 3**: `go vet ./...`

Expected: clean.

- [ ] **Step 4**: One-shot race detector before PR per `.claude/rules/race-testing.md`:

```bash
go test -race ./...
```

Expected: PASS, no race warnings.

- [ ] **Step 5**: If anything fails, fix, run again. Do NOT proceed with a failing suite.

### Task 29: PR

- [ ] **Step 1**: Push branch and create PR targeting `release/v0.7.0`.
- [ ] **Step 2**: Body includes spec link, milestone link (v0.7.0), and `Closes #27`.
- [ ] **Step 3**: Apply v0.7.0 milestone to PR (per project convention).
