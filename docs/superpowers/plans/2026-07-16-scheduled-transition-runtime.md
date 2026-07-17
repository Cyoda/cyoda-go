# Scheduled State Transition Runtime — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make workflow scheduled transitions actually fire — a transition carrying `Schedule{DelayMs, TimeoutMs}` fires automatically `DelayMs` after the entity enters its source state, durably and correctly across cluster members.

**Architecture:** A new durable `ScheduledTask` SPI store, armed/cancelled atomically inside the entity write (a reconcile), scanned by a coordinator node that round-robin-delegates a fire-and-forget peer RPC to a worker; the worker guards by re-reading the task and fires via ordinary read-then-`CompareAndSave`. Idempotency + an expiry grace band replace any dispatch lease.

**Tech Stack:** Go 1.26, `log/slog`, manual DI, `uuid.UUID`, testcontainers-go (postgres e2e), hashicorp/memberlist (gossip), the `cyoda-go-spi` SPI module (sibling repo, coordinated release).

**Design doc:** `docs/superpowers/specs/2026-07-16-scheduled-transition-runtime-design.md`. Read it — every task's rationale lives there; section refs below (§N) point into it.

## Global Constraints

- Go 1.26+. `log/slog` only — never `log.Printf`/`fmt.Printf`. Wrap errors `fmt.Errorf("...: %w", err)`. `uuid.UUID` not `string`. Config via `CYODA_`-prefixed env with defaults in `DefaultConfig()`.
- 4xx: full domain detail + error code. 5xx: generic + ticket UUID. Never log/emit credentials, tokens, keys.
- **No issue IDs in shipped artefacts** (error messages, logs, response bodies, code comments, help/OpenAPI content). Issue IDs only in commits/PRs/spec docs.
- **TDD**: every task is RED (failing test) → GREEN (minimal impl) → commit. Never write impl without a failing test.
- **SPI coordinated release** (§12): SPI work lands on `cyoda-go-spi` main; cyoda-go pins the pseudo-version of that HEAD in all four `go.mod` files; **no committed `replace`** (use `go.work` locally, which already lists `.`, `plugins/*`). The real SPI tag is deferred to milestone-end.
- **Plugins are separate modules**: `go test ./...` from root skips `plugins/*`. Use `make test-all` / run per-plugin. Docker required for postgres testcontainers.
- Commercial (Cassandra) backend is **out of scope** — tracked separately (cyoda-go-cassandra#68).
- Cluster mode is the primary target (`.claude/rules/multi-node-primary.md`): design cross-node correctness in, never descope it.

---

## File Structure

**cyoda-go-spi** (sibling repo `../cyoda-go-spi`):
- `types.go` — add `ScheduledTask`, `ScheduledTaskType`, new `StateMachineEventType` constants.
- `persistence.go` — add `ScheduledTaskStore` interface + `StoreFactory.ScheduledTaskStore(ctx)` accessor.
- `scheduled_task_store_conformance.go` (new) — a shared conformance test helper (like existing store conformance suites) each plugin runs.

**cyoda-go** (this repo):
- `plugins/memory/scheduled_task_store.go` (new) + tx-buffer staging in `plugins/memory/txstate.go`/`txmanager.go` + accessor in `store_factory.go`.
- `plugins/sqlite/scheduled_task_store.go` (new) + staging in `txstate.go`/`txmanager.go` (`flushToSQLite`) + migration + accessor.
- `plugins/postgres/scheduled_task_store.go` (new) + migration + accessor (context-resolving querier).
- `internal/domain/workflow/engine.go` — extract `fireTransition`; add `FireScheduledTransition`; reword reject.
- `internal/domain/workflow/arm.go` (new) — the arm/cancel reconcile called from the transition tx.
- `internal/scheduler/` (new package) — `service.go` (scan loop), `coordinator.go`, `distribution.go`, `clock.go`.
- `internal/cluster/scheduler_rpc.go` (new) — `ExecuteScheduledTask` peer RPC (client + server) over existing PeerAuth.
- `app/app.go` — construct + start scheduler in `app.New`, stop in `Shutdown`.
- `app/config.go` + `DefaultConfig()` — scheduler config fields.
- `e2e/parity/scheduledtransition/` (new) — parity scenarios + registry entry.
- `internal/e2e/scheduled_transition_test.go` (new) — running-backend e2e.
- `internal/grpc/*_test.go` — gRPC reject test.
- Docs: `cmd/cyoda/help/content/workflows.md`, `cmd/cyoda/help/content/config/*.md`, `README.md`, `COMPATIBILITY.md`, `CHANGELOG.md`, `docs/cloud-parity/scheduled-transitions.md` (new).

---

## Phase A — SPI additions (`../cyoda-go-spi`)

> Work in `../cyoda-go-spi` on its `main`. After Phase A, pin cyoda-go to the new pseudo-version (Task A5). Do NOT tag.

### Task A1: `ScheduledTask` type + `ScheduledTaskType`

**Files:**
- Modify: `../cyoda-go-spi/types.go`
- Test: `../cyoda-go-spi/types_test.go`

**Interfaces:**
- Produces: `type ScheduledTask struct{...}`, `type ScheduledTaskType string`, const `ScheduledTaskFireTransition ScheduledTaskType = "fire-transition"`.

- [ ] **Step 1: Write failing round-trip test**

```go
func TestScheduledTask_RoundTrips(t *testing.T) {
	to := int64(5000)
	rd := int64(1_700_000_030_000)
	task := ScheduledTask{
		ID:             "e1:S:T",
		TenantID:       "t1",
		Type:           ScheduledTaskFireTransition,
		ScheduledTime:  1_700_000_000_000,
		TimeoutMs:      &to,
		RedispatchAfter: &rd,
		EntityID:       "e1",
		ModelName:      "order",
		ModelVersion:   2,
		Transition:     "AutoClose",
		SourceState:    "OPEN",
		ArmedAt:        1_699_999_999_000,
		AttemptCount:   1,
	}
	b, err := json.Marshal(task)
	if err != nil { t.Fatal(err) }
	var back ScheduledTask
	if err := json.Unmarshal(b, &back); err != nil { t.Fatal(err) }
	if back.Type != ScheduledTaskFireTransition || back.ScheduledTime != task.ScheduledTime ||
		back.TimeoutMs == nil || *back.TimeoutMs != 5000 || back.SourceState != "OPEN" {
		t.Fatalf("round-trip lost fields: %+v", back)
	}
	// nil TimeoutMs (no timeout) must round-trip as absent.
	task.TimeoutMs = nil
	b2, _ := json.Marshal(task)
	if strings.Contains(string(b2), "timeoutMs") {
		t.Errorf("nil TimeoutMs must be omitted: %s", b2)
	}
}
```

- [ ] **Step 2: Run — expect FAIL** (`ScheduledTask` undefined)

Run: `cd ../cyoda-go-spi && go test ./... -run TestScheduledTask_RoundTrips`
Expected: FAIL (compile error).

- [ ] **Step 3: Add the type** to `types.go` (near `TransitionSchedule`)

```go
// ScheduledTaskType discriminates ScheduledTask variants. Only
// fire-transition is implemented today; the runtime is generic so
// future variants (delayed export, async-result crossover) reuse it.
type ScheduledTaskType string

const ScheduledTaskFireTransition ScheduledTaskType = "fire-transition"

// ScheduledTask is a durable "do something at ScheduledTime, with
// TimeoutMs lateness tolerance" record. For fire-transition, the
// payload fields identify the entity+transition to fire. See the
// cyoda-go scheduled-transition-runtime design for semantics.
type ScheduledTask struct {
	// ID is deterministic for fire-transition:
	// hash(type, entityID, sourceState, transition) — so re-arm upserts
	// in place and never collides across states.
	ID       string           `json:"id"`
	TenantID TenantID         `json:"tenantId"`
	Type     ScheduledTaskType `json:"type"`
	// ScheduledTime is unix-millis; due when <= now.
	ScheduledTime int64 `json:"scheduledTime"`
	// TimeoutMs is the lateness tolerance in ms; nil = never expires.
	TimeoutMs *int64 `json:"timeoutMs,omitempty"`
	// RedispatchAfter is a unix-millis best-effort throttle; the scan
	// excludes rows still inside it. Not a lease, not conditional.
	RedispatchAfter *int64 `json:"redispatchAfter,omitempty"`

	// --- fire-transition payload ---
	EntityID     string `json:"entityId,omitempty"`
	ModelName    string `json:"modelName,omitempty"`
	ModelVersion int    `json:"modelVersion,omitempty"`
	Transition   string `json:"transition,omitempty"`
	SourceState  string `json:"sourceState,omitempty"`

	ArmedAt      int64 `json:"armedAt,omitempty"`
	AttemptCount int   `json:"attemptCount,omitempty"`
}
```

Add `"strings"` to `types_test.go` imports if missing.

- [ ] **Step 4: Run — expect PASS**

Run: `cd ../cyoda-go-spi && go test ./... -run TestScheduledTask_RoundTrips`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd ../cyoda-go-spi
git add types.go types_test.go
git commit -m "feat: add ScheduledTask type for scheduled-transition runtime"
```

### Task A2: `ScheduledTaskStore` interface + factory accessor

**Files:**
- Modify: `../cyoda-go-spi/persistence.go`
- Test: `../cyoda-go-spi/persistence_test.go` (compile-time interface assertion only)

**Interfaces:**
- Produces:
  - `StoreFactory.ScheduledTaskStore(ctx context.Context) (ScheduledTaskStore, error)`
  - `ScheduledTaskStore` with: `Upsert(ctx, ScheduledTask) error`; `Get(ctx, id string) (*ScheduledTask, bool, error)`; `ScanDue(ctx, nowMs int64, limit int) ([]ScheduledTask, error)`; `MarkRedispatch(ctx, id string, redispatchAfterMs int64) error`; `Delete(ctx, id string) (bool, error)` (bool = row removed, for delete-gated audit); `ReconcileForEntity(ctx, ReconcileRequest) ([]ScheduledTask, error)` (returns the cancelled tasks so the engine can audit them).
  - `type ReconcileRequest struct { TenantID TenantID; EntityID, CurrentState string; Arm []ScheduledTask }`.

- [ ] **Step 1: Write failing compile-time assertion test**

```go
func TestScheduledTaskStore_InterfaceShape(t *testing.T) {
	// Compile-time only: a nil typed factory must satisfy the accessor.
	var _ StoreFactory = (StoreFactory)(nil)
	var f func(StoreFactory, context.Context) (ScheduledTaskStore, error) =
		func(sf StoreFactory, ctx context.Context) (ScheduledTaskStore, error) {
			return sf.ScheduledTaskStore(ctx)
		}
	_ = f
	var _ ReconcileRequest
}
```

- [ ] **Step 2: Run — expect FAIL** (`ScheduledTaskStore`/accessor undefined)

Run: `cd ../cyoda-go-spi && go test ./... -run TestScheduledTaskStore_InterfaceShape`
Expected: FAIL (compile error).

- [ ] **Step 3: Add to `persistence.go`**

Add the accessor to `StoreFactory` (after `AsyncSearchStore`):

```go
	// ScheduledTaskStore accesses durable scheduled tasks. Unlike the
	// per-tenant stores, its ScanDue is cross-tenant (obtain with a
	// background/tenant-less context); Upsert/Delete/Reconcile carry the
	// tenant on the task/request. Participates in the entity write's
	// transaction so arm/cancel are atomic with the state change.
	ScheduledTaskStore(ctx context.Context) (ScheduledTaskStore, error)
```

Then:

```go
// ReconcileForEntity input: arm the CurrentState's scheduled transitions,
// cancel (delete) any pending task for this entity whose SourceState !=
// CurrentState. Returns the cancelled tasks (for audit).
type ReconcileRequest struct {
	TenantID     TenantID
	EntityID     string
	CurrentState string
	Arm          []ScheduledTask // tasks to Upsert (current state's schedules)
}

// ScheduledTaskStore persists ScheduledTasks. Arm/Delete/Reconcile MUST
// participate in the caller's transaction (atomic with the entity write).
// ScanDue is a read across all tenants and is called outside any tenant tx.
type ScheduledTaskStore interface {
	Upsert(ctx context.Context, task ScheduledTask) error
	Get(ctx context.Context, id string) (task *ScheduledTask, found bool, err error)
	// ScanDue returns up to limit tasks with ScheduledTime <= nowMs AND
	// (RedispatchAfter is null OR <= nowMs), ordered by ScheduledTime, across tenants.
	ScanDue(ctx context.Context, nowMs int64, limit int) ([]ScheduledTask, error)
	// MarkRedispatch sets RedispatchAfter = redispatchAfterMs (plain write) and bumps AttemptCount.
	MarkRedispatch(ctx context.Context, id string, redispatchAfterMs int64) error
	// Delete removes the task, returning whether a row was actually removed
	// (delete-gated terminal audit relies on this).
	Delete(ctx context.Context, id string) (removed bool, err error)
	// ReconcileForEntity upserts req.Arm and deletes the entity's other-state
	// pending tasks; returns the deleted (cancelled) tasks.
	ReconcileForEntity(ctx context.Context, req ReconcileRequest) (cancelled []ScheduledTask, err error)
}
```

- [ ] **Step 4: Run — expect PASS**

Run: `cd ../cyoda-go-spi && go test ./...`
Expected: PASS (all existing SPI tests still green).

- [ ] **Step 5: Commit**

```bash
cd ../cyoda-go-spi
git add persistence.go persistence_test.go
git commit -m "feat: add ScheduledTaskStore SPI interface + factory accessor"
```

### Task A3: `StateMachineEventType` constants

**Files:**
- Modify: `../cyoda-go-spi/types.go` (the `StateMachineEventType` const block ~line 268)
- Test: `../cyoda-go-spi/types_test.go`

**Interfaces:**
- Produces: `SMEventScheduledTransitionArmed = "SCHEDULED_TRANSITION_ARM"`, `SMEventScheduledTransitionFired = "SCHEDULED_TRANSITION_FIRE"`, `SMEventScheduledTransitionExpired = "SCHEDULED_TRANSITION_EXPIRE"`, `SMEventScheduledTransitionCancelled = "SCHEDULED_TRANSITION_CANCEL"`.

- [ ] **Step 1: Write failing test**

```go
func TestScheduledTransitionEventTypes(t *testing.T) {
	cases := map[StateMachineEventType]string{
		SMEventScheduledTransitionArmed:     "SCHEDULED_TRANSITION_ARM",
		SMEventScheduledTransitionFired:     "SCHEDULED_TRANSITION_FIRE",
		SMEventScheduledTransitionExpired:   "SCHEDULED_TRANSITION_EXPIRE",
		SMEventScheduledTransitionCancelled: "SCHEDULED_TRANSITION_CANCEL",
	}
	for got, want := range cases {
		if string(got) != want { t.Errorf("got %q want %q", got, want) }
	}
}
```

- [ ] **Step 2: Run — expect FAIL** (constants undefined)

- [ ] **Step 3: Add constants** to the `StateMachineEventType` const block

```go
	SMEventScheduledTransitionArmed     StateMachineEventType = "SCHEDULED_TRANSITION_ARM"
	SMEventScheduledTransitionFired     StateMachineEventType = "SCHEDULED_TRANSITION_FIRE"
	SMEventScheduledTransitionExpired   StateMachineEventType = "SCHEDULED_TRANSITION_EXPIRE"
	SMEventScheduledTransitionCancelled StateMachineEventType = "SCHEDULED_TRANSITION_CANCEL"
```

- [ ] **Step 4: Run — expect PASS**

- [ ] **Step 5: Commit**

```bash
cd ../cyoda-go-spi
git add types.go types_test.go
git commit -m "feat: add SCHEDULED_TRANSITION_* state-machine event types"
```

### Task A4: Store conformance helper (shared test)

**Files:**
- Create: `../cyoda-go-spi/scheduled_task_store_conformance.go`

**Interfaces:**
- Produces: `func RunScheduledTaskStoreConformance(t *testing.T, newFactory func() StoreFactory)` — exercised by each plugin's test package.

- [ ] **Step 1: Write the conformance helper** (no separate RED — it *is* the shared test; plugins drive it)

```go
package spi

import (
	"context"
	"testing"
)

// RunScheduledTaskStoreConformance exercises the ScheduledTaskStore contract
// against any StoreFactory. Each plugin calls this from its test package.
func RunScheduledTaskStoreConformance(t *testing.T, newFactory func() StoreFactory) {
	ctx := context.Background()
	f := newFactory()
	t.Cleanup(func() { _ = f.Close() })

	// Arm two tasks in one tenant, one due, one future.
	sts, err := f.ScheduledTaskStore(ctx)
	if err != nil { t.Fatal(err) }
	due := ScheduledTask{ID: "e1:S:T", TenantID: "t1", Type: ScheduledTaskFireTransition,
		ScheduledTime: 1000, EntityID: "e1", SourceState: "S", Transition: "T"}
	future := ScheduledTask{ID: "e2:S:T", TenantID: "t1", Type: ScheduledTaskFireTransition,
		ScheduledTime: 9_000_000, EntityID: "e2", SourceState: "S", Transition: "T"}
	if err := sts.Upsert(ctx, due); err != nil { t.Fatal(err) }
	if err := sts.Upsert(ctx, future); err != nil { t.Fatal(err) }

	// ScanDue at now=2000 returns only the due one.
	got, err := sts.ScanDue(ctx, 2000, 10)
	if err != nil { t.Fatal(err) }
	if len(got) != 1 || got[0].ID != "e1:S:T" {
		t.Fatalf("ScanDue: want [e1:S:T], got %+v", got)
	}

	// MarkRedispatch hides it from a subsequent scan.
	if err := sts.MarkRedispatch(ctx, "e1:S:T", 5000); err != nil { t.Fatal(err) }
	got, _ = sts.ScanDue(ctx, 2000, 10)
	if len(got) != 0 { t.Fatalf("MarkRedispatch should hide task, got %+v", got) }

	// Get returns it; AttemptCount bumped.
	tk, found, err := sts.Get(ctx, "e1:S:T")
	if err != nil || !found { t.Fatalf("Get: found=%v err=%v", found, err) }
	if tk.AttemptCount != 1 { t.Errorf("AttemptCount want 1 got %d", tk.AttemptCount) }

	// Delete returns removed=true once, false the second time.
	if ok, _ := sts.Delete(ctx, "e1:S:T"); !ok { t.Error("first Delete want removed=true") }
	if ok, _ := sts.Delete(ctx, "e1:S:T"); ok { t.Error("second Delete want removed=false") }

	// ReconcileForEntity: entity e2 moved S->S2; arm S2:T2, cancel S:T.
	arm := []ScheduledTask{{ID: "e2:S2:T2", TenantID: "t1", Type: ScheduledTaskFireTransition,
		ScheduledTime: 100, EntityID: "e2", SourceState: "S2", Transition: "T2"}}
	cancelled, err := sts.ReconcileForEntity(ctx, ReconcileRequest{
		TenantID: "t1", EntityID: "e2", CurrentState: "S2", Arm: arm})
	if err != nil { t.Fatal(err) }
	if len(cancelled) != 1 || cancelled[0].ID != "e2:S:T" {
		t.Fatalf("Reconcile cancelled: want [e2:S:T], got %+v", cancelled)
	}
	if _, found, _ := sts.Get(ctx, "e2:S:T"); found { t.Error("old-state task should be deleted") }
	if _, found, _ := sts.Get(ctx, "e2:S2:T2"); !found { t.Error("new-state task should be armed") }

	// Tenant isolation: a t2 task is not returned to a t1-scoped delete but IS
	// visible to the cross-tenant ScanDue with its own TenantID.
	other := ScheduledTask{ID: "e9:S:T", TenantID: "t2", Type: ScheduledTaskFireTransition,
		ScheduledTime: 100, EntityID: "e9", SourceState: "S", Transition: "T"}
	if err := sts.Upsert(ctx, other); err != nil { t.Fatal(err) }
	all, _ := sts.ScanDue(ctx, 2000, 10)
	var sawT2 bool
	for _, x := range all { if x.TenantID == "t2" { sawT2 = true } }
	if !sawT2 { t.Error("cross-tenant ScanDue must include t2 task") }
}
```

- [ ] **Step 2: Verify it compiles**

Run: `cd ../cyoda-go-spi && go build ./...`
Expected: builds.

- [ ] **Step 3: Commit**

```bash
cd ../cyoda-go-spi
git add scheduled_task_store_conformance.go
git commit -m "test: add ScheduledTaskStore conformance helper"
```

### Task A5: Pin cyoda-go to the new SPI pseudo-version

**Files:**
- Modify: `go.mod`, `plugins/memory/go.mod`, `plugins/sqlite/go.mod`, `plugins/postgres/go.mod`

- [ ] **Step 1: Get the SPI HEAD pseudo-version**

```bash
cd ../cyoda-go-spi && SPI_SHA=$(git rev-parse HEAD)
cd - >/dev/null
# from the worktree root:
go list -m -json github.com/cyoda-platform/cyoda-go-spi@$SPI_SHA 2>/dev/null | grep Version
```
Record the `vX.Y.Z-0.<timestamp>-<sha12>` pseudo-version string.

- [ ] **Step 2: Update all four go.mod files** to that pseudo-version (edit the `require` line for `github.com/cyoda-platform/cyoda-go-spi`). Then:

```bash
go mod tidy
(cd plugins/memory && go mod tidy)
(cd plugins/sqlite && go mod tidy)
(cd plugins/postgres && go mod tidy)
```

- [ ] **Step 3: Verify go.work composition resolves**

Run: `go build ./... && (cd plugins/memory && go build ./...)`
Expected: builds against the local SPI via `go.work`.

- [ ] **Step 4: Commit**

```bash
git add go.mod go.sum plugins/*/go.mod plugins/*/go.sum
git commit -m "chore: pin cyoda-go-spi pseudo-version for ScheduledTaskStore"
```

---

## Phase B — Store implementations (cyoda-go plugins)

Each store: a `scheduledTime`-indexed structure; `Upsert`/`Delete`/`ReconcileForEntity` participate in the tx; `ScanDue` is cross-tenant and tenant-less. Memory + sqlite stage ops in the tx buffer and flush on commit (§5.1/§15 F2); postgres uses the context-resolving querier.

### Task B1: memory `ScheduledTaskStore` + tx-buffer staging

**Files:**
- Create: `plugins/memory/scheduled_task_store.go`
- Modify: `plugins/memory/store_factory.go` (accessor + backing map), `plugins/memory/txstate.go` (stage slice), `plugins/memory/txmanager.go` (flush in `Commit`)
- Test: `plugins/memory/scheduled_task_store_test.go`

**Interfaces:**
- Consumes: `spi.ScheduledTaskStore`, `spi.ScheduledTask`, `spi.ReconcileRequest` (Task A2).
- Produces: `func (f *StoreFactory) ScheduledTaskStore(ctx) (spi.ScheduledTaskStore, error)`.

- [ ] **Step 1: Write the failing conformance test**

```go
package memory_test

import (
	"testing"
	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

func TestMemory_ScheduledTaskStoreConformance(t *testing.T) {
	spi.RunScheduledTaskStoreConformance(t, func() spi.StoreFactory {
		return memory.NewStoreFactory() // adjust to actual constructor
	})
}
```

- [ ] **Step 2: Run — expect FAIL** (accessor undefined)

Run: `cd plugins/memory && go test ./... -run ScheduledTaskStoreConformance`
Expected: FAIL.

- [ ] **Step 3: Add backing map + staging + store**

In `store_factory.go` add to the factory struct: `scheduledTasks map[string]spi.ScheduledTask` (keyed by task ID) guarded by the factory mutex, initialised in the constructor. Add accessor:

```go
func (f *StoreFactory) ScheduledTaskStore(ctx context.Context) (spi.ScheduledTaskStore, error) {
	return &scheduledTaskStore{f: f}, nil
}
```

In `txstate.go` add to `TransactionState`: `ScheduledTaskOps []scheduledTaskOp`, where:

```go
type scheduledTaskOp struct {
	kind  string // "upsert" | "delete"
	id    string
	task  spi.ScheduledTask // for upsert
}
```

Create `scheduled_task_store.go`:

```go
package memory

import (
	"context"
	"sort"
	spi "github.com/cyoda-platform/cyoda-go-spi"
)

type scheduledTaskStore struct{ f *StoreFactory }

func (s *scheduledTaskStore) stage(ctx context.Context, op scheduledTaskOp) error {
	tx := spi.GetTransaction(ctx)
	if tx == nil {
		// No tx: apply immediately (ScanDue path never mutates).
		s.f.mu.Lock(); defer s.f.mu.Unlock()
		s.apply(op); return nil
	}
	tx.OpMu.RLock(); defer tx.OpMu.RUnlock()
	if tx.RolledBack { return spi.ErrTxRolledBack }
	tx.ScheduledTaskOps = append(tx.ScheduledTaskOps, op)
	return nil
}

// apply mutates the committed map under f.mu (caller holds it).
func (s *scheduledTaskStore) apply(op scheduledTaskOp) {
	switch op.kind {
	case "upsert": s.f.scheduledTasks[op.id] = op.task
	case "delete": delete(s.f.scheduledTasks, op.id)
	}
}

func (s *scheduledTaskStore) Upsert(ctx context.Context, t spi.ScheduledTask) error {
	return s.stage(ctx, scheduledTaskOp{kind: "upsert", id: t.ID, task: t})
}

func (s *scheduledTaskStore) Delete(ctx context.Context, id string) (bool, error) {
	// Existence is checked against committed state (tx-buffered deletes are
	// rare here; conformance covers the committed case).
	s.f.mu.RLock(); _, ok := s.f.scheduledTasks[id]; s.f.mu.RUnlock()
	if err := s.stage(ctx, scheduledTaskOp{kind: "delete", id: id}); err != nil { return false, err }
	return ok, nil
}

func (s *scheduledTaskStore) Get(ctx context.Context, id string) (*spi.ScheduledTask, bool, error) {
	s.f.mu.RLock(); defer s.f.mu.RUnlock()
	t, ok := s.f.scheduledTasks[id]
	if !ok { return nil, false, nil }
	cp := t; return &cp, true, nil
}

func (s *scheduledTaskStore) ScanDue(ctx context.Context, nowMs int64, limit int) ([]spi.ScheduledTask, error) {
	s.f.mu.RLock(); defer s.f.mu.RUnlock()
	var out []spi.ScheduledTask
	for _, t := range s.f.scheduledTasks {
		if t.ScheduledTime <= nowMs && (t.RedispatchAfter == nil || *t.RedispatchAfter <= nowMs) {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ScheduledTime < out[j].ScheduledTime })
	if limit > 0 && len(out) > limit { out = out[:limit] }
	return out, nil
}

func (s *scheduledTaskStore) MarkRedispatch(ctx context.Context, id string, after int64) error {
	s.f.mu.Lock(); defer s.f.mu.Unlock()
	t, ok := s.f.scheduledTasks[id]
	if !ok { return nil }
	t.RedispatchAfter = &after; t.AttemptCount++
	s.f.scheduledTasks[id] = t
	return nil
}

func (s *scheduledTaskStore) ReconcileForEntity(ctx context.Context, req spi.ReconcileRequest) ([]spi.ScheduledTask, error) {
	s.f.mu.RLock()
	var cancelled []spi.ScheduledTask
	for id, t := range s.f.scheduledTasks {
		if t.EntityID == req.EntityID && t.TenantID == req.TenantID && t.SourceState != req.CurrentState {
			cancelled = append(cancelled, t)
			_ = id
		}
	}
	s.f.mu.RUnlock()
	for _, t := range req.Arm {
		if err := s.Upsert(ctx, t); err != nil { return nil, err }
	}
	for _, c := range cancelled {
		if _, err := s.Delete(ctx, c.ID); err != nil { return nil, err }
	}
	return cancelled, nil
}
```

In `txmanager.go` `Commit`, after the entity-buffer flush, apply staged task ops **under the same `f.mu`** as the entity flush:

```go
	sts := &scheduledTaskStore{f: m.factory}
	for _, op := range tx.ScheduledTaskOps { sts.apply(op) }
```

(Place inside the existing critical section that flushes `tx.Buffer` so the task ops commit atomically with entities.)

- [ ] **Step 4: Run — expect PASS**

Run: `cd plugins/memory && go test ./... -run ScheduledTaskStoreConformance -v`
Expected: PASS.

- [ ] **Step 5: Add an atomicity test** (arm rolled back with entity)

```go
func TestMemory_ScheduledTaskArm_RollbackIsAtomic(t *testing.T) {
	// Begin tx, Upsert a task, roll back → task must NOT be present.
	// (Use the factory's TransactionManager Begin/Rollback + tx context.)
}
```
Implement the body using the plugin's `TransactionManager` Begin/Rollback and a tx-scoped context; assert `Get` after rollback returns found=false. Run → PASS.

- [ ] **Step 6: Commit**

```bash
git add plugins/memory/scheduled_task_store.go plugins/memory/store_factory.go \
  plugins/memory/txstate.go plugins/memory/txmanager.go plugins/memory/scheduled_task_store_test.go
git commit -m "feat(memory): ScheduledTaskStore with atomic tx-buffer co-commit (#251)"
```

### Task B2: sqlite `ScheduledTaskStore` + flush staging + migration

**Files:**
- Create: `plugins/sqlite/scheduled_task_store.go`, migration in the sqlite schema init.
- Modify: `plugins/sqlite/store_factory.go`, `plugins/sqlite/txstate.go`, `plugins/sqlite/txmanager.go` (`flushToSQLite`)
- Test: `plugins/sqlite/scheduled_task_store_test.go`

**Interfaces:** same as B1.

- [ ] **Step 1: Failing conformance test** (mirror B1 test, `sqlite.NewStoreFactory(...)`). Run → FAIL.

- [ ] **Step 2: Add the table** to the sqlite schema DDL (where other `CREATE TABLE` live):

```sql
CREATE TABLE IF NOT EXISTS scheduled_tasks (
  id                TEXT PRIMARY KEY,
  tenant_id         TEXT NOT NULL,
  type              TEXT NOT NULL,
  scheduled_time    INTEGER NOT NULL,
  timeout_ms        INTEGER,
  redispatch_after  INTEGER,
  entity_id         TEXT NOT NULL,
  model_name        TEXT NOT NULL,
  model_version     INTEGER NOT NULL,
  transition        TEXT NOT NULL,
  source_state      TEXT NOT NULL,
  armed_at          INTEGER NOT NULL,
  attempt_count     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS scheduled_tasks_due_idx
  ON scheduled_tasks (scheduled_time);
CREATE INDEX IF NOT EXISTS scheduled_tasks_entity_idx
  ON scheduled_tasks (tenant_id, entity_id);
```

- [ ] **Step 3: Stage in tx buffer, flush in `flushToSQLite`.** Add `ScheduledTaskOps []scheduledTaskOp` to sqlite `txState` (same shape as B1). The store's `Upsert`/`Delete`/`Reconcile` append to `tx.ScheduledTaskOps` when in a tx (mirroring `entityStore.Save` at `entity_store.go:206`). In `flushToSQLite` (`txmanager.go:331`), inside the single `sqlTx`, after entity flush, execute each op:

```go
for _, op := range tx.ScheduledTaskOps {
	switch op.kind {
	case "upsert":
		_, err = sqlTx.ExecContext(ctx, `INSERT INTO scheduled_tasks
		 (id,tenant_id,type,scheduled_time,timeout_ms,redispatch_after,entity_id,model_name,model_version,transition,source_state,armed_at,attempt_count)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET scheduled_time=excluded.scheduled_time,
		   timeout_ms=excluded.timeout_ms, redispatch_after=excluded.redispatch_after,
		   source_state=excluded.source_state, armed_at=excluded.armed_at,
		   attempt_count=excluded.attempt_count`,
		 op.task.ID, string(op.task.TenantID), string(op.task.Type), op.task.ScheduledTime,
		 op.task.TimeoutMs, op.task.RedispatchAfter, op.task.EntityID, op.task.ModelName,
		 op.task.ModelVersion, op.task.Transition, op.task.SourceState, op.task.ArmedAt, op.task.AttemptCount)
	case "delete":
		_, err = sqlTx.ExecContext(ctx, `DELETE FROM scheduled_tasks WHERE id=?`, op.id)
	}
	if err != nil { return err }
}
```

- [ ] **Step 4: Implement the store** (`scheduled_task_store.go`): `ScanDue`/`Get`/`MarkRedispatch`/`Delete`-existence-check query the DB directly (these are read/standalone; `ScanDue` uses `WHERE scheduled_time<=? AND (redispatch_after IS NULL OR redispatch_after<=?) ORDER BY scheduled_time LIMIT ?`, cross-tenant, no tenant filter). `Delete` first `SELECT` to compute `removed`, then stages the delete op. `ReconcileForEntity` `SELECT`s the entity's other-state tasks for the cancelled list, then stages upserts + deletes. `MarkRedispatch` is a standalone `UPDATE ... SET redispatch_after=?, attempt_count=attempt_count+1 WHERE id=?`.

- [ ] **Step 5: Accessor** in `store_factory.go` (mirror B1). Run conformance → PASS.

- [ ] **Step 6: Atomicity + rollback test** (mirror B1 Step 5). Run → PASS.

- [ ] **Step 7: Commit**

```bash
git add plugins/sqlite/
git commit -m "feat(sqlite): ScheduledTaskStore with atomic flush co-commit (#251)"
```

### Task B3: postgres `ScheduledTaskStore` + migration

**Files:**
- Create: `plugins/postgres/scheduled_task_store.go`, a migration file under the postgres migrations dir.
- Modify: `plugins/postgres/store_factory.go` (accessor via `f.querier()`)
- Test: `plugins/postgres/scheduled_task_store_test.go`

**Interfaces:** same as B1.

- [ ] **Step 1: Failing conformance test** (testcontainers postgres; mirror existing postgres store tests' setup). Run → FAIL (Docker required).

- [ ] **Step 2: Migration** (new numbered file in the postgres migration sequence):

```sql
CREATE TABLE scheduled_tasks (
  id                text PRIMARY KEY,
  tenant_id         text NOT NULL,
  type              text NOT NULL,
  scheduled_time    bigint NOT NULL,
  timeout_ms        bigint,
  redispatch_after  bigint,
  entity_id         text NOT NULL,
  model_name        text NOT NULL,
  model_version     int NOT NULL,
  transition        text NOT NULL,
  source_state      text NOT NULL,
  armed_at          bigint NOT NULL,
  attempt_count     int NOT NULL DEFAULT 0
);
CREATE INDEX scheduled_tasks_due_idx ON scheduled_tasks (scheduled_time);
CREATE INDEX scheduled_tasks_entity_idx ON scheduled_tasks (tenant_id, entity_id);
```

**RLS note (§15 F7):** if the postgres plugin applies row-level security on tenant tables, either exempt `scheduled_tasks` from RLS or run `ScanDue` under a role/GUC that sees all tenants (the scan is a trusted system read). Verify against `plugins/postgres/rls_test.go` conventions and add a test asserting cross-tenant `ScanDue` returns rows from ≥2 tenants.

- [ ] **Step 3: Implement the store** with `q: f.querier()` (context-resolving — Upsert/Delete run in the entity tx automatically). `Upsert` = `INSERT ... ON CONFLICT (id) DO UPDATE ...`. `Delete` = `DELETE ... WHERE id=$1` then read `rowsAffected` for `removed`. `ScanDue` = `SELECT ... WHERE scheduled_time<=$1 AND (redispatch_after IS NULL OR redispatch_after<=$1) ORDER BY scheduled_time LIMIT $2` (no tenant filter; obtained with a background context so no tenant scoping). `MarkRedispatch` = `UPDATE ... SET redispatch_after=$2, attempt_count=attempt_count+1 WHERE id=$1`. `ReconcileForEntity`: `SELECT` other-state tasks (`WHERE tenant_id=$1 AND entity_id=$2 AND source_state<>$3`) for the cancelled list, then upsert `Arm` + delete the cancelled ids — all through `f.querier()` so they join the entity tx.

- [ ] **Step 4: Accessor** in `store_factory.go`:

```go
func (f *StoreFactory) ScheduledTaskStore(ctx context.Context) (spi.ScheduledTaskStore, error) {
	return &scheduledTaskStore{q: f.querier()}, nil
}
```

Run conformance → PASS.

- [ ] **Step 5: Atomicity test** — arm a task inside an entity tx, roll back the tx, assert `Get` returns not-found (proves it joined the pgx.Tx). Run → PASS.

- [ ] **Step 6: Commit**

```bash
git add plugins/postgres/
git commit -m "feat(postgres): ScheduledTaskStore atomic via context-resolving querier (#251)"
```

---

## Phase C — Engine integration

### Task C1: Extract `fireTransition` (pure refactor, no behavior change)

**Files:**
- Modify: `internal/domain/workflow/engine.go` (`attemptTransition` ~496)
- Test: existing engine tests must stay green (this is a refactor).

**Interfaces:**
- Produces: `func (e *Engine) fireTransition(ctx, entity *spi.Entity, wf *spi.WorkflowDefinition, transition *spi.TransitionDefinition, auditStore spi.StateMachineAuditStore, txID string) (context.Context, string, error)` — criterion eval → processors → TRANSITION_MAKE + advance → returns; **no** disabled/scheduled/manual policy inside.

- [ ] **Step 1: Run the existing engine suite (baseline green)**

Run: `go test ./internal/domain/workflow/... -short`
Expected: PASS (record the count).

- [ ] **Step 2: Extract the mechanism.** Move the body of `attemptTransition` *after* the disabled/scheduled rejects (criterion eval through state advance) into `fireTransition`. `attemptTransition` becomes: find transition → reject if `Disabled` → reject if `Schedule != nil` → `return e.fireTransition(...)`.

- [ ] **Step 3: Run the suite — expect same PASS** (pure refactor)

Run: `go test ./internal/domain/workflow/... -short`
Expected: PASS, same count.

- [ ] **Step 4: Commit**

```bash
git add internal/domain/workflow/engine.go
git commit -m "refactor(workflow): extract fireTransition mechanism from attemptTransition (#251)"
```

### Task C2: Arm/cancel reconcile helper

**Files:**
- Create: `internal/domain/workflow/arm.go`
- Modify: `internal/domain/workflow/engine.go` (call reconcile at the end of a transition/loopback, in-tx) + `Engine` struct (hold a clock — Task D-provided `Now func() time.Time`, default `time.Now`).
- Test: `internal/domain/workflow/arm_test.go`

**Interfaces:**
- Consumes: `spi.ScheduledTaskStore` (via `e.factory.ScheduledTaskStore(ctx)`), `spi.ReconcileRequest`, `taskID(entityID, sourceState, transition)`.
- Produces: `func (e *Engine) reconcileScheduledTasks(ctx, entity *spi.Entity, wf *spi.WorkflowDefinition, txID string) error`; `func taskID(entityID, sourceState, transition string) string`.

- [ ] **Step 1: Failing test — arm on entry**

```go
func TestReconcile_ArmsCurrentStateSchedules(t *testing.T) {
	// Engine with memory factory + a workflow: state OPEN has a scheduled
	// transition AutoClose (DelayMs 1000). Put an entity in OPEN, run
	// reconcileScheduledTasks in a tx, commit. Assert a task exists with
	// id == taskID(entityID,"OPEN","AutoClose"), scheduledTime == now+1000.
}
```

- [ ] **Step 2: Run — expect FAIL**

- [ ] **Step 3: Implement `arm.go`**

```go
package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func taskID(entityID, sourceState, transition string) string {
	h := sha256.Sum256([]byte("fire-transition|" + entityID + "|" + sourceState + "|" + transition))
	return hex.EncodeToString(h[:16])
}

// reconcileScheduledTasks arms the current state's scheduled transitions and
// cancels the entity's other-state tasks — atomic within the caller's tx.
// No-op (no store I/O) when the workflow has no scheduled transitions.
func (e *Engine) reconcileScheduledTasks(ctx context.Context, entity *spi.Entity, wf *spi.WorkflowDefinition, txID string) error {
	if !workflowHasSchedule(wf) {
		return nil
	}
	nowMs := e.now().UnixMilli()
	state := entity.Meta.State
	var arm []spi.ScheduledTask
	for _, tr := range wf.States[state].Transitions {
		if tr.Schedule == nil || tr.Manual || tr.Disabled {
			continue
		}
		var to *int64
		if tr.Schedule.TimeoutMs != nil { v := *tr.Schedule.TimeoutMs; to = &v }
		arm = append(arm, spi.ScheduledTask{
			ID: taskID(entity.Meta.ID, state, tr.Name), TenantID: entity.Meta.TenantID,
			Type: spi.ScheduledTaskFireTransition, ScheduledTime: nowMs + tr.Schedule.DelayMs,
			TimeoutMs: to, EntityID: entity.Meta.ID, ModelName: entity.Meta.ModelRef.Name,
			ModelVersion: entity.Meta.ModelRef.Version, Transition: tr.Name, SourceState: state,
			ArmedAt: nowMs,
		})
	}
	sts, err := e.factory.ScheduledTaskStore(ctx)
	if err != nil { return fmt.Errorf("scheduled task store: %w", err) }
	cancelled, err := sts.ReconcileForEntity(ctx, spi.ReconcileRequest{
		TenantID: entity.Meta.TenantID, EntityID: entity.Meta.ID, CurrentState: state, Arm: arm})
	if err != nil { return fmt.Errorf("reconcile scheduled tasks: %w", err) }
	// Audit arm + cancel via recordEvent (best-effort).
	if auditStore, aerr := e.factory.StateMachineAuditStore(ctx); aerr == nil {
		for _, a := range arm {
			e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, state,
				spi.SMEventScheduledTransitionArmed,
				fmt.Sprintf("armed %q at +%dms", a.Transition, a.ScheduledTime-nowMs), nil)
		}
		for _, c := range cancelled {
			e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, c.SourceState,
				spi.SMEventScheduledTransitionCancelled,
				fmt.Sprintf("cancelled %q (left state %q)", c.Transition, c.SourceState), nil)
		}
	}
	return nil
}

func workflowHasSchedule(wf *spi.WorkflowDefinition) bool {
	for _, st := range wf.States {
		for _, tr := range st.Transitions {
			if tr.Schedule != nil { return true }
		}
	}
	return false
}
```

Add `now func() time.Time` to `Engine` (default `time.Now` in `NewEngine`; `func (e *Engine) now() time.Time { if e.now_ != nil { return e.now_() }; return time.Now() }` — use a field name that avoids clashing with the method; e.g. store `clock func() time.Time`).

- [ ] **Step 4: Call the reconcile** at the end of `ManualTransition` and `Execute` (after the cascade settles, within the same tx), and in the loopback path (`LoopbackWithIfMatch`). Each calls `e.reconcileScheduledTasks(ctx, entity, wf, txID)` before returning success.

- [ ] **Step 5: Run — expect PASS**; also add and pass `TestReconcile_LoopbackReArmsNoCancel` (loopback in same state re-arms, cancelled list empty → no `SCHEDULED_TRANSITION_CANCEL`) and `TestReconcile_TransitionCancelsOldState`.

- [ ] **Step 6: Commit**

```bash
git add internal/domain/workflow/arm.go internal/domain/workflow/engine.go internal/domain/workflow/arm_test.go
git commit -m "feat(workflow): arm/cancel scheduled-task reconcile in transition tx (#251)"
```

### Task C3: `FireScheduledTransition` (guard + grace band + one-shot)

**Files:**
- Modify: `internal/domain/workflow/engine.go`
- Test: `internal/domain/workflow/fire_scheduled_test.go`

**Interfaces:**
- Produces: `func (e *Engine) FireScheduledTransition(ctx context.Context, task spi.ScheduledTask) (ScheduledOutcome, error)` where `type ScheduledOutcome string` ∈ {`Fired`,`Declined`,`Expired`,`Dropped`}.
- Consumes: `e.factory.{EntityStore,ScheduledTaskStore,TransactionManager}`, `e.fireTransition`, `e.clockNow`, grace/`e` config.

- [ ] **Step 1: Failing tests** (fake clock injected):

```go
func TestFireScheduled_FiresOnTime(t *testing.T) { /* lateness 0 → Fired, entity in Next, task gone */ }
func TestFireScheduled_DeclineOnCriterionFalse(t *testing.T) { /* criterion false → Declined, entity stays, task gone */ }
func TestFireScheduled_ExpireBeyondGrace(t *testing.T) { /* now = sched + timeout + 2*grace → Expired, delete-gated audit once */ }
func TestFireScheduled_DropInGraceBand(t *testing.T) { /* timeout < lateness <= timeout+grace → Dropped, task remains, no audit */ }
func TestFireScheduled_GuardEntityMovedOn(t *testing.T) { /* entity.state != sourceState → Dropped silently, task deleted */ }
func TestFireScheduled_GuardReArmedToFuture(t *testing.T) { /* task.scheduledTime > now (re-armed) → Dropped */ }
```

- [ ] **Step 2: Run — expect FAIL**

- [ ] **Step 3: Implement**

```go
type ScheduledOutcome string
const (
	OutcomeFired    ScheduledOutcome = "fired"
	OutcomeDeclined ScheduledOutcome = "declined"
	OutcomeExpired  ScheduledOutcome = "expired"
	OutcomeDropped  ScheduledOutcome = "dropped"
)

func (e *Engine) FireScheduledTransition(ctx context.Context, task spi.ScheduledTask) (ScheduledOutcome, error) {
	txMgr, err := e.factory.TransactionManager(ctx)
	if err != nil { return OutcomeDropped, err }
	txCtx, txID, err := beginTx(ctx, txMgr) // helper mirroring existing tx begin usage
	if err != nil { return OutcomeDropped, err }
	commit := false
	defer func() { if !commit { _ = txMgr.Rollback(txCtx, txID) } }()

	sts, _ := e.factory.ScheduledTaskStore(txCtx)
	cur, found, err := sts.Get(txCtx, task.ID)
	if err != nil { return OutcomeDropped, err }
	if !found { commit = true; _ = txMgr.Commit(txCtx, txID); return OutcomeDropped, nil }

	es, _ := e.factory.EntityStore(txCtx)
	entity, err := es.Get(txCtx, task.EntityID)
	if err != nil || entity == nil { return OutcomeDropped, err }

	// Guard: entity moved on, or re-armed to the future.
	if entity.Meta.State != cur.SourceState || cur.ScheduledTime > e.clockNow().UnixMilli() {
		_, _ = sts.Delete(txCtx, task.ID) // stale (moved on); re-armed future left in place if state matches
		if entity.Meta.State != cur.SourceState {
			commit = true; _ = txMgr.Commit(txCtx, txID); return OutcomeDropped, nil
		}
		// re-armed to future: do not delete — roll back so the future row stands.
		return OutcomeDropped, nil
	}

	// Grace-band lateness decision.
	nowMs := e.clockNow().UnixMilli()
	lateness := nowMs - cur.ScheduledTime
	if cur.TimeoutMs != nil {
		if lateness > *cur.TimeoutMs + e.expiryGraceMs {
			if removed, _ := sts.Delete(txCtx, task.ID); removed {
				e.auditScheduled(txCtx, entity, txID, spi.SMEventScheduledTransitionExpired,
					fmt.Sprintf("expired %q (lateness %dms)", cur.Transition, lateness))
			}
			commit = true; return OutcomeExpired, txMgr.Commit(txCtx, txID)
		}
		if lateness > *cur.TimeoutMs {
			// grace band: drop and wait (roll back, leave row).
			return OutcomeDropped, nil
		}
	}

	// Fire: find workflow+transition, guarded read-then-CAS via fireTransition.
	wf, tr, err := e.resolveScheduled(txCtx, entity, cur.Transition) // finds wf for entity model+state, locates tr
	if err != nil { return OutcomeDropped, err }
	auditStore, _ := e.factory.StateMachineAuditStore(txCtx)
	// fireTransition evaluates the criterion (false → no advance) then advances state.
	matched, ferr := e.fireScheduledInner(txCtx, entity, wf, tr, auditStore, txID)
	if ferr != nil { return OutcomeDropped, ferr }
	if !matched {
		// criterion false → Declined (one-shot): remove the task, entity stays.
		if removed, _ := sts.Delete(txCtx, task.ID); removed {
			e.auditScheduled(txCtx, entity, txID, spi.SMEventTransitionCriterionNoMatch,
				fmt.Sprintf("declined %q (criterion false)", cur.Transition))
		}
		commit = true; return OutcomeDeclined, txMgr.Commit(txCtx, txID)
	}
	// Fired: fireTransition advanced state → the in-tx reconcile (Task C2) has
	// already deleted this task and armed the next state. Persist the entity.
	if _, err := es.CompareAndSave(txCtx, entity, entity.Meta.TransactionID); err != nil {
		return OutcomeDropped, err // concurrent write → retried on next scan
	}
	e.auditScheduled(txCtx, entity, txID, spi.SMEventScheduledTransitionFired,
		fmt.Sprintf("fired %q", cur.Transition))
	commit = true
	return OutcomeFired, txMgr.Commit(txCtx, txID)
}
```

Add helpers: `clockNow()` (injected clock, default `time.Now`), `expiryGraceMs int64` (from config, Task D4), `auditScheduled(...)` (thin wrapper over `recordEvent`), `resolveScheduled(...)` (reuse `findWorkflowForState`), and `fireScheduledInner` which runs `fireTransition` and reports whether the criterion matched (may need `fireTransition` to return a `matched bool`; thread it through from `evaluateCriterion`). Ensure the entity write applies the CAS at first flush (reuse the `*WithIfMatch` machinery: pass `entity.Meta.TransactionID` as the expected value).

- [ ] **Step 4: Run — expect PASS** (all six tests)

- [ ] **Step 5: Commit**

```bash
git add internal/domain/workflow/engine.go internal/domain/workflow/fire_scheduled_test.go
git commit -m "feat(workflow): FireScheduledTransition — guard, grace band, one-shot (#251)"
```

### Task C4: Reword the explicit-fire reject

**Files:**
- Modify: `internal/domain/workflow/engine.go` (`scheduledNotYetImplementedReason` const ~45, the reject in `attemptTransition` ~522)
- Test: `internal/domain/workflow/engine_test.go` (existing reject test — update the expected message)

- [ ] **Step 1: Update the existing reject test** to expect the new message:

```go
want := `transition "AutoClose" in state "OPEN" is scheduled and fires automatically; it is not manually fireable`
```

- [ ] **Step 2: Run — expect FAIL** (old message)

- [ ] **Step 3: Reword** the const + the wrapped error:

```go
const scheduledReason = "scheduled and fires automatically; it is not manually fireable"
// ... in attemptTransition:
return ctx, txID, fmt.Errorf("transition %q in state %q is %s: %w",
	transitionName, entity.Meta.State, scheduledReason, ErrTransitionNotFound)
```
Update the audit `Details` string likewise. Remove the now-stale `scheduledNotYetImplementedReason`.

- [ ] **Step 4: Run — expect PASS**

- [ ] **Step 5: Commit**

```bash
git add internal/domain/workflow/engine.go internal/domain/workflow/engine_test.go
git commit -m "feat(workflow): reword scheduled explicit-fire reject now runtime exists (#251)"
```

---

## Phase D — Scheduler service

### Task D1: Coordinator + distribution strategies

**Files:**
- Create: `internal/scheduler/coordinator.go`, `internal/scheduler/distribution.go`, `internal/scheduler/clock.go`
- Test: `internal/scheduler/coordinator_test.go`, `internal/scheduler/distribution_test.go`

**Interfaces:**
- Produces: `type Clock interface { Now() time.Time }` + `realClock`; `type CoordinatorStrategy interface { IsCoordinator(members []contract.NodeInfo, selfID string) bool }` + `LowestLiveNodeID`; `type DistributionStrategy interface { Pick(members []contract.NodeInfo, selfID string, task spi.ScheduledTask) string }` + `RoundRobin` (cursor) and `Self`.

- [ ] **Step 1: Failing tests**

```go
func TestLowestLiveNodeID(t *testing.T) {
	c := LowestLiveNodeID{}
	m := []contract.NodeInfo{{NodeID:"n3"},{NodeID:"n1"},{NodeID:"n2"}}
	if !c.IsCoordinator(m, "n1") { t.Error("n1 should be coordinator") }
	if c.IsCoordinator(m, "n2") { t.Error("n2 should not") }
	if !c.IsCoordinator(nil, "n1") { t.Error("empty membership → self is coordinator") }
}
func TestRoundRobin_Cycles(t *testing.T) {
	d := NewRoundRobin()
	m := []contract.NodeInfo{{NodeID:"a"},{NodeID:"b"}}
	got := []string{d.Pick(m,"a",spi.ScheduledTask{}), d.Pick(m,"a",spi.ScheduledTask{}), d.Pick(m,"a",spi.ScheduledTask{})}
	// deterministic cycling over sorted member ids
	if got[0]==got[1] { t.Errorf("round-robin should alternate, got %v", got) }
}
func TestSelf_AlwaysSelf(t *testing.T) {
	if (Self{}).Pick(nil, "x", spi.ScheduledTask{}) != "x" { t.Error() }
}
```

- [ ] **Step 2: Run — expect FAIL**

- [ ] **Step 3: Implement.** `LowestLiveNodeID.IsCoordinator` = `selfID == min(NodeID over members)`, and true when members empty/only-self. `RoundRobin` holds a `sync.Mutex`-guarded `uint64` cursor; `Pick` sorts member IDs, returns `members[cursor++ % len]`. `Self.Pick` returns `selfID`. `realClock.Now()` = `time.Now()`.

- [ ] **Step 4: Run — expect PASS**

- [ ] **Step 5: Commit**

```bash
git add internal/scheduler/coordinator.go internal/scheduler/distribution.go internal/scheduler/clock.go internal/scheduler/*_test.go
git commit -m "feat(scheduler): coordinator + distribution strategies (#251)"
```

### Task D2: Scan loop `Service`

**Files:**
- Create: `internal/scheduler/service.go`
- Test: `internal/scheduler/service_test.go`

**Interfaces:**
- Consumes: `spi.StoreFactory` (`ScheduledTaskStore`), `contract.NodeRegistry`, `CoordinatorStrategy`, `DistributionStrategy`, `Clock`, an `Executor` (local or RPC) and a `Dispatcher` seam.
- Produces: `type Service struct{...}`; `func NewService(cfg Config, deps Deps) *Service`; `func (s *Service) Start()`; `func (s *Service) Stop()`; `type Executor interface { Execute(ctx, spi.ScheduledTask) }`; `type Config struct { ScanInterval, RedispatchBackoff time.Duration; BatchSize int; Enabled bool }`.

- [ ] **Step 1: Failing test** (fake clock, in-memory store, single node, `Self` distribution, a capturing `Executor`):

```go
func TestService_ScansAndDispatchesDueTasks(t *testing.T) {
	// Arm one due + one future task. Tick once. Assert the executor saw only
	// the due one, and MarkRedispatch was applied (future scan excludes it).
}
func TestService_NonCoordinatorDoesNothing(t *testing.T) {
	// selfID not the min over a 2-member registry → no scan, executor sees nothing.
}
```

- [ ] **Step 2: Run — expect FAIL**

- [ ] **Step 3: Implement.** `Start()` launches a goroutine: `ticker := clock-driven interval` (use `time.NewTicker(cfg.ScanInterval)` in prod; the test injects a manual tick channel). Each tick, if `!cfg.Enabled` skip; if `!coordinator.IsCoordinator(registry.List(), selfID)` skip; else `tasks := store.ScanDue(now, batch)`, and for each: `store.MarkRedispatch(id, now+backoff)`, `target := dist.Pick(members, selfID, task)`, `executor.Execute` (the executor routes local vs RPC by target — Task D3). `Stop()` closes a stop channel. Follow the reaper pattern (`app/app.go:412`). Log at DEBUG per dispatch, ERROR on scan failure.

- [ ] **Step 4: Run — expect PASS**

- [ ] **Step 5: Commit**

```bash
git add internal/scheduler/service.go internal/scheduler/service_test.go
git commit -m "feat(scheduler): coordinator scan loop with redispatch throttle (#251)"
```

### Task D3: Peer RPC `ExecuteScheduledTask` + system identity

**Files:**
- Create: `internal/cluster/scheduler_rpc.go` (client + server handler), `internal/scheduler/executor.go`
- Modify: gRPC service registration (where peer RPCs are registered), proto if a new RPC message is needed (reuse CloudEvent envelope or add a small message consistent with existing peer RPCs).
- Test: `internal/cluster/scheduler_rpc_test.go`

**Interfaces:**
- Produces: `type ClusterExecutor struct{...}` implementing `scheduler.Executor` — if `target == selfID` calls `engine.FireScheduledTransition` locally; else forwards over the PeerAuth channel; `func systemUserContext(tenant spi.TenantID) context.Context` (a service principal, `changeUser="scheduler"`).

- [ ] **Step 1: Failing tests**

```go
func TestExecutor_LocalWhenSelf(t *testing.T) { /* target==self → engine called directly */ }
func TestExecutor_ForwardsWithPeerAuth(t *testing.T) { /* target peer → uses PeerAuth-wrapped client; unauthenticated call rejected */ }
func TestSystemUserContext_HasTenant(t *testing.T) { /* Begin succeeds with the synthesised context (tenant present) */ }
```

- [ ] **Step 2: Run — expect FAIL**

- [ ] **Step 3: Implement.** `systemUserContext` builds a `spi.UserContext{UserID:"scheduler", Tenant: {ID: tenant}}` (match the real UserContext shape) and injects it via `spi.WithUserContext`. `ClusterExecutor.Execute`: build the system context from `task.TenantID`; if `target==selfID` → `engine.FireScheduledTransition(sysCtx, task)`; else marshal the task and send via the existing peer forwarder (`internal/cluster/dispatch/forwarder.go` — reuse `PeerAuth`/AEAD, **do not** add an unauthenticated path). Server handler authenticates (PeerAuth), rebuilds the system context from the task's tenant, and calls `engine.FireScheduledTransition`. Never log the task payload beyond ids at DEBUG.

- [ ] **Step 4: Run — expect PASS**

- [ ] **Step 5: Commit**

```bash
git add internal/cluster/scheduler_rpc.go internal/scheduler/executor.go internal/cluster/scheduler_rpc_test.go
git commit -m "feat(cluster): authenticated ExecuteScheduledTask peer RPC + system identity (#251)"
```

### Task D4: Config + wire into `app.New`/`Shutdown`

**Files:**
- Modify: `app/config.go` (+ `DefaultConfig()`), `app/app.go` (construct + start; stop in `Shutdown`), `internal/domain/workflow/engine.go` (accept `expiryGraceMs` + clock via `EngineOption`)
- Test: `app/config_test.go` (defaults), `internal/e2e` covers wiring end-to-end (Phase F)

**Interfaces:**
- Consumes: everything from D1–D3, `scheduler.Config`.
- Produces: config fields `Scheduler struct { Enabled bool; ScanInterval, RedispatchBackoff, ExpiryGrace time.Duration; BatchSize int; Distribution, Coordinator string }`.

- [ ] **Step 1: Failing defaults test**

```go
func TestDefaultConfig_Scheduler(t *testing.T) {
	c := DefaultConfig()
	if !c.Scheduler.Enabled { t.Error("enabled default true") }
	if c.Scheduler.ScanInterval != time.Second { t.Error("scan interval 1s") }
	if c.Scheduler.BatchSize != 100 { t.Error("batch 100") }
	if c.Scheduler.RedispatchBackoff != 30*time.Second { t.Error("backoff 30s") }
	if c.Scheduler.ExpiryGrace != 100*time.Millisecond { t.Error("grace 100ms") }
	if c.Scheduler.Distribution != "round-robin" { t.Error("dist round-robin") }
	if c.Scheduler.Coordinator != "lowest-node-id" { t.Error("coord lowest-node-id") }
}
```

- [ ] **Step 2: Run — expect FAIL**

- [ ] **Step 3: Implement** the config fields (env: `CYODA_SCHEDULER_ENABLED` bool, `_SCAN_INTERVAL`/`_REDISPATCH_BACKOFF`/`_EXPIRY_GRACE` via `envDuration`/`envMillis`, `_BATCH_SIZE` via `envInt`, `_DISTRIBUTION`/`_COORDINATOR` via `envString`). Add an `EngineOption` `WithScheduledClock(func() time.Time)` and `WithExpiryGrace(time.Duration)`; set `e.clockNow`/`e.expiryGraceMs` in `NewEngine`. In `app.New`, after the engine + factory + registry exist, construct the `scheduler.Service` (map config strings to strategies; select `Self` when cluster disabled or `Distribution=="self"`), store `a.scheduler`, and `a.scheduler.Start()`. In `Shutdown()`, `a.scheduler.Stop()` (before `nodeRegistry.Deregister`).

- [ ] **Step 4: Run — expect PASS** (`go test ./app/... -run Scheduler`)

- [ ] **Step 5: Commit**

```bash
git add app/config.go app/app.go internal/domain/workflow/engine.go app/config_test.go
git commit -m "feat: wire scheduler service + config into app lifecycle (#251)"
```

---

## Phase E — Cross-backend parity + running-backend e2e + gRPC

> Coverage matrix (§11) → tests. Concurrency isolated (never parity). Exact lateness/expiry boundaries are unit (fake clock, Phase C); e2e uses small `DelayMs` + a short scan interval for coarse happy-path only.

### Task E1: Parity scenarios + registry

**Files:**
- Create: `e2e/parity/scheduledtransition/scheduledtransition.go`
- Modify: `e2e/parity/registry.go`
- Test: runs across memory/sqlite/postgres via the parity harness.

**Interfaces:**
- Produces: `RunScheduledTransition_FiresOnTime`, `_DeclineCriterionFalse`, `_NoExpireWhenNilTimeout`, `_CancelOnStateExit`, `_LoopbackReArmsNoCancel`, `_ReArmClockReset`, `_CascadeAfterFire` — each `func(t *testing.T, h *parity.Harness)` registered in `registry.go`.

- [ ] **Step 1: Write the scenarios** (each: import a workflow, create/drive an entity via the harness store+engine with a small `DelayMs` and the harness's controllable clock, run the scheduler once, assert the outcome — state advanced / task gone / audit event present). Register all in `registry.go` so every backend (incl. commercial on its next bump) picks them up.

- [ ] **Step 2: Run** `go test ./e2e/parity/... -run ScheduledTransition -v` (memory+sqlite) and the postgres parity job. Expected: PASS across backends.

- [ ] **Step 3: Commit**

```bash
git add e2e/parity/scheduledtransition/ e2e/parity/registry.go
git commit -m "test(parity): scheduled-transition backend-agnostic scenarios (#251)"
```

### Task E2: Running-backend e2e (HTTP)

**Files:**
- Create: `internal/e2e/scheduled_transition_test.go`

**Interfaces:**
- Consumes: the e2e harness (`TestMain` postgres + httptest server).

- [ ] **Step 1: Write tests**
  - `TestE2E_ScheduledTransition_FiresThroughHTTPStack`: import workflow (state OPEN → AutoClose scheduled, small DelayMs), `POST` create entity into OPEN, poll `GET` until state advances (bounded, generous timeout), assert audit shows `SCHEDULED_TRANSITION_FIRE`.
  - `TestE2E_ExplicitFireOfScheduled_Returns400`: `POST …/transition/AutoClose` → 400, `errorCode == TRANSITION_NOT_FOUND`, message contains `"scheduled and fires automatically"`.
  - `TestE2E_LoopbackReArms_TimerDefers`: create into OPEN, keep `PUT`-updating faster than DelayMs, assert it does NOT fire within the window (documents the settled-interval semantic, §5.4/F6).
  - `TestE2E_RestartDurability` (if the harness supports app restart) or a scheduler-restart variant: armed task survives a scheduler `Stop()`/`Start()` and still fires.

- [ ] **Step 2: Run** `go test ./internal/e2e/... -run ScheduledTransition -v` (Docker). Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/e2e/scheduled_transition_test.go
git commit -m "test(e2e): scheduled transitions through the full HTTP stack (#251)"
```

### Task E3: gRPC reject + isolated concurrency

**Files:**
- Create/modify: `internal/grpc/…_test.go` (reject envelope), `internal/e2e/scheduled_transition_concurrency_test.go`

- [ ] **Step 1: gRPC test** — `ManualTransition` on a scheduled transition returns `Success=false`, `Error.Code == TRANSITION_NOT_FOUND`, message contains `"scheduled and fires automatically"`.

- [ ] **Step 2: Isolated concurrency test** (single backend, not parity): fan out two scheduler `Service` instances (simulating dual-coordinator) over one memory store against a due task; assert exactly one `Fired` (one state advance), no torn write, at most one `SCHEDULED_TRANSITION_FIRE`, and (delete-gated) at most one `SCHEDULED_TRANSITION_EXPIRE` audit for any expired task. Assert consistency, not a precise interleave (`.claude/rules/test-coverage.md`).

- [ ] **Step 3: Run** both. Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/grpc/ internal/e2e/scheduled_transition_concurrency_test.go
git commit -m "test: gRPC scheduled reject + isolated dual-coordinator consistency (#251)"
```

---

## Phase F — Documentation (Gate 4 + Gate 7)

### Task F1: Help topic — workflows

**Files:**
- Modify: `cmd/cyoda/help/content/workflows.md` (the "runtime not yet implemented" section ~250)

- [ ] **Step 1:** Rewrite the "Engine behaviour (runtime not yet implemented)" section: scheduled transitions now fire on a timer `DelayMs` after state entry; `TimeoutMs` is lateness tolerance (drop if too late); one-shot criterion (false → not fired, entity stays); explicit fire-by-name still 400 (reworded); document the three §5.4 patterns (unconditional cycle = heartbeat, conditional scheduled = one-shot deadline, poll = tick + conditional cascade exits) and the settled-interval semantic (any in-place write resets the timer — F6). Keep it compact (state the actionable core; detail lives in the spec).

- [ ] **Step 2:** Verify no issue IDs in the content. Run `go test ./cmd/cyoda/... -run Help` if a help-content test exists.

- [ ] **Step 3: Commit**

```bash
git add cmd/cyoda/help/content/workflows.md
git commit -m "docs(help): document scheduled-transition runtime behaviour (#251)"
```

### Task F2: Config help + README + COMPATIBILITY + CHANGELOG

**Files:**
- Modify: `cmd/cyoda/help/content/config/*.md` (the relevant config topic), `README.md`, `COMPATIBILITY.md`, `CHANGELOG.md`

- [ ] **Step 1:** Add the six `CYODA_SCHEDULER_*` vars (name, default, meaning) to the config help topic and the README config reference — matching `DefaultConfig()` exactly.
- [ ] **Step 2:** `COMPATIBILITY.md`: record the cyoda-go-spi pin bump (ScheduledTaskStore surface).
- [ ] **Step 3:** `CHANGELOG.md`: add the feature entry (scheduled state transitions now fire).
- [ ] **Step 4: Commit**

```bash
git add cmd/cyoda/help/content/config/ README.md COMPATIBILITY.md CHANGELOG.md
git commit -m "docs: scheduler config reference + compatibility + changelog (#251)"
```

### Task F3: Cloud-parity (Gate 7)

**Files:**
- Create: `docs/cloud-parity/scheduled-transitions.md`

- [ ] **Step 1:** Document the runtime contract Cloud mirrors: arm-on-entry/cancel-on-exit, one-shot criterion, grace-band expiry, explicit-fire 400, audit event types. One file, per Gate 7.
- [ ] **Step 2: Commit**

```bash
git add docs/cloud-parity/scheduled-transitions.md
git commit -m "docs(cloud-parity): scheduled-transition runtime contract (#251)"
```

---

## Phase G — Verification

### Task G1: Full verification pass

- [ ] **Step 1:** `go build ./... && (cd plugins/memory && go build ./...) && (cd plugins/sqlite && go build ./...) && (cd plugins/postgres && go build ./...)`
- [ ] **Step 2:** `go vet ./...` and per-plugin `go vet`.
- [ ] **Step 3:** `make test-all` (root + all plugins; Docker up). All green.
- [ ] **Step 4:** `go test ./internal/e2e/... -v` — full HTTP stack incl. the new scheduled tests.
- [ ] **Step 5:** `make race` once (CI-parity scope). Green.
- [ ] **Step 6:** `make todos` — confirm no stray `TODO(...)` left by this change.
- [ ] **Step 7:** Re-read the design doc §11 matrix; tick every cell against a test that exists. Fill any gap before declaring done.

---

## Self-Review (completed by plan author)

**Spec coverage:** SPI type/store/events → Phase A; atomicity on all 3 backends → B1/B2/B3 (memory+sqlite staging, postgres querier — F2); guard re-read + grace band + one-shot → C3; arm/cancel reconcile → C2 (F5 loopback-no-cancel covered); explicit-fire reject → C4; coordinator/distribution/scan/RPC/system-identity/PeerAuth → D1–D4 (H1/H2); config → D4/F2; cross-tenant scan + RLS → B3; audit events (delete-gated) → C2/C3; coverage matrix → E1–E3 (HTTP + gRPC + parity + isolated concurrency); docs (help/config/README/COMPAT/CHANGELOG/cloud-parity) → F1–F3. F6 → F1 doc line. Back-pressure (#416) and commercial store (cyoda-go-cassandra#68) intentionally out of scope.

**No new HTTP error codes** → no `errors/<CODE>.md` task needed (reject reuses `TRANSITION_NOT_FOUND`; validation reuses `VALIDATION_FAILED`). Confirmed against §10.

**Placeholder scan:** no TBD/TODO; every code step shows code. Mechanical store replication (B2/B3) gives concrete DDL + SQL rather than "similar to B1".

**Type consistency:** `ScheduledTask`, `ScheduledTaskStore` methods, `ReconcileRequest`, `ScheduledOutcome`, `taskID`, `reconcileScheduledTasks`, `FireScheduledTransition`, `scheduler.{Service,Executor,Config,CoordinatorStrategy,DistributionStrategy,Clock}` used consistently across tasks.
