package entity

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	wfengine "github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
	"github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

// TestFlows_ErrorPathsReleaseTheTransaction asserts that every converted flow
// leaves no open transaction behind on an ordinary (non-panic) error.
//
// This is the highest-value test in the change. The existing write suites assert
// response codes and never observe transaction state, so a defect in Release
// would leave a transaction open on every error path in this file with nothing
// noticing.
//
// Every case provokes the same failure — the flow's own post-Begin
// factory.EntityStore lookup — because that is the earliest handler-side site
// each of the seven flows reaches after its transaction is open. The engine
// keeps the unhooked factory, so the failure is the handler's, not a workflow
// error standing in for one.
func TestFlows_ErrorPathsReleaseTheTransaction(t *testing.T) {
	cases := []struct {
		name string
		// drive provokes an error inside the flow after the transaction is open.
		drive func(t *testing.T, hn *rollbackHarness) error
	}{
		{"CreateEntity", driveCreateFailure},
		{"DeleteEntity", driveDeleteFailure},
		{"DeleteAllEntities", driveDeleteAllFailure},
		{"DeleteEntitiesConditional", driveDeleteConditionalFailure},
		{"CreateEntityCollection", driveCreateCollectionFailure},
		{"updateEntityCore", driveUpdateFailure},
		{"UpdateEntityCollection", driveUpdateCollectionFailure},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hn := newTrackingHandler(t)
			hn.failEntityStoreAfterBegin()

			if err := tc.drive(t, hn); err == nil {
				t.Fatal("expected the flow to fail")
			}
			if !hn.tracker.beganAny() {
				t.Fatal("the flow never opened a transaction; the case proves nothing")
			}
			if open := hn.tracker.openTxIDs(); len(open) != 0 {
				t.Fatalf("%s left %d transaction(s) open on an error path: %v", tc.name, len(open), open)
			}
		})
	}
}

// TestFlows_PanicReleasesTheTransaction is coverage row 1's unit half: a panic
// between Begin and Commit must not leave the transaction open.
func TestFlows_PanicReleasesTheTransaction(t *testing.T) {
	hn := newTrackingHandler(t)
	hn.registerPanickingCriterionWorkflow(t)

	expectPanicked(t, func() { _, _ = hn.h.CreateEntity(hn.ctx, rollbackPanickyInput()) })

	if !hn.tracker.beganAny() {
		t.Fatal("the flow never opened a transaction; the case proves nothing")
	}
	if open := hn.tracker.openTxIDs(); len(open) != 0 {
		t.Fatalf("panic leaked %d transaction(s): %v", len(open), open)
	}
}

// TestJoinedCallbackPanic_DoesNotRollBackOwner is coverage row 3's unit half. A
// participating write that blows up must surface the panic to its caller and
// leave the owner's transaction exactly as it found it — the owner decides its
// fate.
func TestJoinedCallbackPanic_DoesNotRollBackOwner(t *testing.T) {
	hn := newTrackingHandler(t)
	hn.registerPanickingCriterionWorkflow(t)

	ownerTxID, _, err := hn.tracker.Begin(hn.ctx)
	if err != nil {
		t.Fatalf("owner Begin: %v", err)
	}
	joinedCtx, err := hn.tracker.Join(hn.ctx, ownerTxID)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}

	expectPanicked(t, func() { _, _ = hn.h.CreateEntity(joinedCtx, rollbackPanickyInput()) })

	if hn.tracker.wasRolledBack(ownerTxID) {
		t.Fatal("a joined callback rolled back its owner's transaction")
	}
	if !hn.tracker.isOpen(ownerTxID) {
		t.Fatal("owner's transaction is no longer open; the owner must decide its fate")
	}
}

// TestPanickingWrite_ReleasesBufferedState is coverage row 6. On memory and
// sqlite the harm is not a held connection — sqlite opens no *sql.Tx at all —
// but a leaked buffer plus a pinned committedLog prune floor, which makes every
// later commit's conflict scan slower without bound.
//
// The floor is read through the effect it has: a commit prunes the committedLog
// back to the oldest STILL-ACTIVE transaction's snapshot, and empties it outright
// when nothing is active. So a clean write landing on a quiescent node leaves
// CommittedLogLen()==0 — unless an abandoned transaction is pinning the floor,
// in which case the entry survives and the log only grows from there.
func TestPanickingWrite_ReleasesBufferedState(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			hn := newTrackingHandlerFor(t, backend)
			hn.registerPanickingCriterionWorkflow(t)

			// Baseline: a clean write on a quiescent node prunes to empty.
			hn.createPlainWidget(t)
			if got := hn.committedLogLen(t); got != 0 {
				t.Fatalf("baseline committedLog length = %d, want 0; the assertion below would prove nothing", got)
			}

			expectPanicked(t, func() { _, _ = hn.h.CreateEntity(hn.ctx, rollbackPanickyInput()) })

			if open := hn.tracker.openTxIDs(); len(open) != 0 {
				t.Fatalf("leaked buffer for %v", open)
			}
			hn.createPlainWidget(t)
			if got := hn.committedLogLen(t); got != 0 {
				t.Fatalf("committedLog prune floor pinned by an abandoned transaction: length = %d, want 0", got)
			}
		})
	}
}

// TestJoinedSegmentedFlow_FreesTheGateBeforeReleaseRollsBack pins the defer
// ordering the conversion introduces.
//
//	defer scope.Release()   // registered FIRST
//	...
//	defer releaseGate()     // registered SECOND
//
// LIFO frees the joined gate before Release runs, which is what lets Release
// re-acquire a gate on the same registry without hold-and-wait. Registering them
// the other way round leaves the flow holding gate(entry) while Release takes
// gate(segment) — two gates at once, and a self-deadlock the moment Release is
// hardened to gate the entry transaction too.
//
// The observation is an event ordering, not a sleep: a competitor for
// gate(entry) is launched from inside the rollback and the rollback does not
// return until that competitor has reached a decided outcome — either it holds
// the gate (freed first: correct) or it is parked inside txgate.Acquire (still
// held: the defect). A reversal therefore FAILS rather than hangs.
//
// The scenario is the joined-segmented can't-happen branch, which is also the
// only shape where Release rolls anything back on a joined call: the engine's
// COMMIT_BEFORE_DISPATCH processor commits the entry transaction and opens a
// segment that belongs to nobody, the handler's guard rejects the call, and the
// segment must not survive it.
func TestJoinedSegmentedFlow_FreesTheGateBeforeReleaseRollsBack(t *testing.T) {
	hn := newTrackingHandler(t)
	hn.registerSegmentingWorkflow(t)

	ownerTxID, _, err := hn.tracker.Begin(hn.ctx)
	if err != nil {
		t.Fatalf("owner Begin: %v", err)
	}
	joinedCtx, err := hn.tracker.Join(hn.ctx, ownerTxID)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}

	competitorDone := make(chan struct{})
	hn.tracker.onRollback = func(txID string) {
		if txID == ownerTxID {
			return // not the segment; nothing to observe
		}
		hn.tracker.record("rollback-start")
		started := make(chan struct{})
		go func() {
			close(started)
			release := hn.h.gate.Acquire(ownerTxID)
			hn.tracker.record("competitor-acquired")
			release()
			close(competitorDone)
		}()
		<-started
		waitForGateContention(t, competitorDone)
	}

	_, err = hn.h.CreateEntity(joinedCtx, rollbackWidgetInput())
	if err == nil {
		t.Fatal("a joined call that segmented must be rejected, not committed")
	}
	var appErr *common.AppError
	if !errors.As(err, &appErr) || appErr.Status != 500 {
		t.Fatalf("joined-segmented guard returned %v, want a 500 AppError", err)
	}

	select {
	case <-competitorDone:
	case <-time.After(10 * time.Second):
		t.Fatal("joined flow never freed gate(entry); a hardened Release would deadlock here")
	}

	want := []string{"rollback-start", "competitor-acquired", "rollback-end"}
	if got := hn.tracker.trace(); !slices.Equal(got, want) {
		t.Fatalf("gate(entry) was still held while Release rolled the segment back: events = %v, want %v", got, want)
	}
	// ...and the segment the guard rejected is gone, which is the behaviour the
	// guard-plus-scope pairing exists to deliver.
	if open := hn.tracker.openTxIDs(); len(open) != 0 {
		t.Fatalf("joined-segmented guard leaked %d transaction(s): %v", len(open), open)
	}
}

// TestJoinedFlows_ErrorPath_DoNotDeadlock is the bounded-timeout guard for the
// ordinary joined shape, where Release declines to roll anything back. It exists
// so a regression in the defer ordering surfaces as a failure with a diagnosis
// rather than a hung package.
func TestJoinedFlows_ErrorPath_DoNotDeadlock(t *testing.T) {
	hn := newTrackingHandler(t)
	hn.failEntityStoreAfterBegin()

	ownerTxID, _, err := hn.tracker.Begin(hn.ctx)
	if err != nil {
		t.Fatalf("owner Begin: %v", err)
	}
	joinedCtx, err := hn.tracker.Join(hn.ctx, ownerTxID)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, cErr := hn.h.CreateEntity(joinedCtx, rollbackWidgetInput())
		done <- cErr
	}()

	select {
	case cErr := <-done:
		if cErr == nil {
			t.Fatal("expected the joined call to fail")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("joined error path did not return; the gate and Release are deadlocked")
	}

	if hn.tracker.wasRolledBack(ownerTxID) {
		t.Fatal("a joined callback rolled back its owner's transaction")
	}
	if !hn.tracker.isOpen(ownerTxID) {
		t.Fatal("owner's transaction is no longer open; the owner must decide its fate")
	}
	// The gate must be free for the owner to finish its own work.
	freed := make(chan struct{})
	go func() { hn.h.gate.Acquire(ownerTxID)(); close(freed) }()
	select {
	case <-freed:
	case <-time.After(10 * time.Second):
		t.Fatal("joined call returned still holding gate(entry)")
	}
}

// --- harness ---

// rollbackModel is the locked model every flow in this file writes against.
var rollbackModel = spi.ModelRef{EntityName: "RollbackWidget", ModelVersion: "1"}

// rollbackPanickyModel carries the workflow whose FUNCTION criterion panics.
// It is a separate model so a flow can choose between blowing up and not.
var rollbackPanickyModel = spi.ModelRef{EntityName: "RollbackBoom", ModelVersion: "1"}

// errArmedEntityStore is the injected post-Begin failure. It is deliberately a
// store-lookup failure rather than a workflow error: every one of the seven
// flows performs exactly one of these after its transaction is open.
var errArmedEntityStore = errors.New("entity store unavailable")

// trackingTxMgr wraps a real TransactionManager so a test can ask which
// transactions are still open without trusting the handler's bookkeeping:
// liveness is probed through Join, which succeeds only while the plugin still
// holds the transaction active.
type trackingTxMgr struct {
	spi.TransactionManager
	probeCtx context.Context

	mu         sync.Mutex
	begun      []string
	rolledBack []string
	events     []string

	onBegin    func(txID string)
	onRollback func(txID string)
}

func (m *trackingTxMgr) Begin(ctx context.Context) (string, context.Context, error) {
	txID, txCtx, err := m.TransactionManager.Begin(ctx)
	if err != nil {
		return txID, txCtx, err
	}
	m.mu.Lock()
	m.begun = append(m.begun, txID)
	cb := m.onBegin
	m.mu.Unlock()
	if cb != nil {
		cb(txID)
	}
	return txID, txCtx, nil
}

func (m *trackingTxMgr) Rollback(ctx context.Context, txID string) error {
	m.mu.Lock()
	m.rolledBack = append(m.rolledBack, txID)
	cb := m.onRollback
	m.mu.Unlock()
	if cb != nil {
		cb(txID)
	}
	err := m.TransactionManager.Rollback(ctx, txID)
	m.record("rollback-end")
	return err
}

func (m *trackingTxMgr) record(ev string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, ev)
}

func (m *trackingTxMgr) trace() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.events)
}

// isOpen reports whether the plugin still holds txID active. Join is
// non-destructive and is the only SPI-level liveness probe available.
func (m *trackingTxMgr) isOpen(txID string) bool {
	_, err := m.TransactionManager.Join(m.probeCtx, txID)
	return err == nil
}

func (m *trackingTxMgr) openTxIDs() []string {
	m.mu.Lock()
	ids := slices.Clone(m.begun)
	m.mu.Unlock()
	var open []string
	for _, id := range ids {
		if m.isOpen(id) {
			open = append(open, id)
		}
	}
	return open
}

// beganAny reports whether any transaction was opened, under the mutex — the
// flows under test run on their own goroutine in the deadlock cases.
func (m *trackingTxMgr) beganAny() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.begun) > 0
}

func (m *trackingTxMgr) wasRolledBack(txID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Contains(m.rolledBack, txID)
}

// armedFactory fails the handler's own EntityStore lookups once armed. The
// engine is wired to the unhooked factory, so the injected failure is always the
// handler's own, never a workflow error standing in for one.
type armedFactory struct {
	spi.StoreFactory
	armed atomic.Bool
}

func (f *armedFactory) EntityStore(ctx context.Context) (spi.EntityStore, error) {
	if f.armed.Load() {
		return nil, errArmedEntityStore
	}
	return f.StoreFactory.EntityStore(ctx)
}

// stubExternalProc is the harness's contract.ExternalProcessingService.
type stubExternalProc struct {
	dispatchProcessor func(ctx context.Context, e *spi.Entity, proc spi.ProcessorDefinition, workflow, transition, txID string) (*spi.Entity, error)
	dispatchCriteria  func(ctx context.Context, e *spi.Entity, criterion json.RawMessage, target, workflow, transition, processor, txID string) (bool, string, error)
}

func (p *stubExternalProc) DispatchProcessor(ctx context.Context, e *spi.Entity, proc spi.ProcessorDefinition, workflow, transition, txID string) (*spi.Entity, error) {
	if p.dispatchProcessor != nil {
		return p.dispatchProcessor(ctx, e, proc, workflow, transition, txID)
	}
	return nil, nil
}

func (p *stubExternalProc) DispatchCriteria(ctx context.Context, e *spi.Entity, criterion json.RawMessage, target, workflow, transition, processor, txID string) (bool, string, error) {
	if p.dispatchCriteria != nil {
		return p.dispatchCriteria(ctx, e, criterion, target, workflow, transition, processor, txID)
	}
	return true, "", nil
}

func (p *stubExternalProc) DispatchFunction(context.Context, *spi.Entity, spi.ScheduleFunction, string, string, string) (contract.FunctionResult, error) {
	return contract.FunctionResult{}, nil
}

type rollbackHarness struct {
	h       *Handler
	tracker *trackingTxMgr
	raw     spi.StoreFactory
	armed   *armedFactory
	proc    *stubExternalProc
	ctx     context.Context
}

func newTrackingHandler(t *testing.T) *rollbackHarness {
	t.Helper()
	return newTrackingHandlerFor(t, "memory")
}

func newTrackingHandlerFor(t *testing.T, backend string) *rollbackHarness {
	t.Helper()
	ctx := rollbackTestCtx()

	var raw spi.StoreFactory
	switch backend {
	case "memory":
		f := memory.NewStoreFactory()
		t.Cleanup(func() { f.Close() })
		raw = f
	case "sqlite":
		f, err := sqlite.NewStoreFactoryForTest(ctx, filepath.Join(t.TempDir(), "rollback.db"))
		if err != nil {
			t.Fatalf("sqlite.NewStoreFactoryForTest: %v", err)
		}
		t.Cleanup(func() { _ = f.Close() })
		raw = f
	default:
		t.Fatalf("unknown backend %q", backend)
	}

	tm, err := raw.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	tracker := &trackingTxMgr{TransactionManager: tm, probeCtx: ctx}

	proc := &stubExternalProc{}
	engine := wfengine.NewEngine(raw, common.NewDefaultUUIDGenerator(), tracker,
		wfengine.WithExternalProcessing(proc))

	searchStore, err := raw.AsyncSearchStore(ctx)
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	searchSvc := search.NewSearchService(raw, common.NewDefaultUUIDGenerator(), searchStore)

	armed := &armedFactory{StoreFactory: raw}
	h := New(armed, tracker, common.NewDefaultUUIDGenerator(), engine, txgate.New(), searchSvc)

	hn := &rollbackHarness{h: h, tracker: tracker, raw: raw, armed: armed, proc: proc, ctx: ctx}
	hn.registerModel(t, rollbackModel)
	return hn
}

func rollbackTestCtx() context.Context {
	return spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID:   "rollback-user",
		UserName: "Rollback",
		Tenant:   spi.Tenant{ID: "rollback-tenant", Name: "Rollback"},
		Roles:    []string{"user"},
	})
}

func (hn *rollbackHarness) registerModel(t *testing.T, ref spi.ModelRef) {
	t.Helper()
	node := schema.NewObjectNode()
	node.SetChild("name", schema.NewLeafNode(schema.String))
	raw, err := schema.Marshal(node)
	if err != nil {
		t.Fatalf("schema.Marshal: %v", err)
	}
	ms, err := hn.raw.ModelStore(hn.ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	if err := ms.Save(hn.ctx, &spi.ModelDescriptor{Ref: ref, State: spi.ModelLocked, Schema: raw}); err != nil {
		t.Fatalf("ModelStore.Save: %v", err)
	}
}

func (hn *rollbackHarness) saveWorkflow(t *testing.T, ref spi.ModelRef, wf spi.WorkflowDefinition) {
	t.Helper()
	ws, err := hn.raw.WorkflowStore(hn.ctx)
	if err != nil {
		t.Fatalf("WorkflowStore: %v", err)
	}
	if err := ws.Save(hn.ctx, ref, []spi.WorkflowDefinition{wf}); err != nil {
		t.Fatalf("WorkflowStore.Save: %v", err)
	}
}

// failEntityStoreAfterBegin arms the handler-side EntityStore failure the moment
// the flow's transaction is open, so every flow fails at its first post-Begin
// store lookup with the transaction live.
func (hn *rollbackHarness) failEntityStoreAfterBegin() {
	hn.tracker.onBegin = func(string) { hn.armed.armed.Store(true) }
}

// registerPanickingCriterionWorkflow gives rollbackPanickyModel an automated
// transition out of the initial state whose FUNCTION criterion panics. Only
// DispatchCriteria reaches the handler intact — DispatchProcessor and
// DispatchFunction recover panics and convert them to errors.
func (hn *rollbackHarness) registerPanickingCriterionWorkflow(t *testing.T) {
	t.Helper()
	hn.registerModel(t, rollbackPanickyModel)
	hn.saveWorkflow(t, rollbackPanickyModel, spi.WorkflowDefinition{
		Version: "1.1", Name: "RollbackBoomWF", InitialState: "A", Active: true,
		States: map[string]spi.StateDefinition{
			"A": {Transitions: []spi.TransitionDefinition{{
				Name: "gate", Next: "B",
				Criterion: json.RawMessage(`{"type":"function","function":{"name":"boom"}}`),
			}}},
			"B": {},
		},
	})
	hn.proc.dispatchCriteria = func(context.Context, *spi.Entity, json.RawMessage, string, string, string, string, string) (bool, string, error) {
		panic("criterion callout panicked: boom")
	}
}

// registerSegmentingWorkflow puts a COMMIT_BEFORE_DISPATCH processor on the
// initial state's automated transition, so the engine commits the entry
// transaction and hands back a fresh segment.
func (hn *rollbackHarness) registerSegmentingWorkflow(t *testing.T) {
	t.Helper()
	startNewTx := true
	hn.saveWorkflow(t, rollbackModel, spi.WorkflowDefinition{
		Version: "1.1", Name: "RollbackSegmentWF", InitialState: "A", Active: true,
		States: map[string]spi.StateDefinition{
			"A": {Transitions: []spi.TransitionDefinition{{
				Name: "segment", Next: "B",
				Processors: []spi.ProcessorDefinition{{
					Type:          wfengine.ProcessorTypeExternalized,
					Name:          "segmenter",
					ExecutionMode: wfengine.ExecutionModeCommitBeforeDispatch,
					Config:        spi.ProcessorConfig{StartNewTxOnDispatch: &startNewTx},
				}},
			}}},
			"B": {},
		},
	})
}

// createPlainWidget performs a clean, committed write — used to establish and
// then re-check the committedLog prune baseline.
func (hn *rollbackHarness) createPlainWidget(t *testing.T) {
	t.Helper()
	if _, err := hn.h.CreateEntity(hn.ctx, rollbackWidgetInput()); err != nil {
		t.Fatalf("clean create: %v", err)
	}
}

// committedLogLen reads the plugin's committed-log length. Both stock SI+FCW
// backends export it for exactly this purpose.
func (hn *rollbackHarness) committedLogLen(t *testing.T) int {
	t.Helper()
	l, ok := hn.tracker.TransactionManager.(interface{ CommittedLogLen() int })
	if !ok {
		t.Fatalf("%T does not expose CommittedLogLen", hn.tracker.TransactionManager)
	}
	return l.CommittedLogLen()
}

func rollbackWidgetInput() CreateEntityInput {
	return CreateEntityInput{
		EntityName:   rollbackModel.EntityName,
		ModelVersion: rollbackModel.ModelVersion,
		Format:       "JSON",
		Data:         json.RawMessage(`{"name":"w"}`),
	}
}

func rollbackPanickyInput() CreateEntityInput {
	return CreateEntityInput{
		EntityName:   rollbackPanickyModel.EntityName,
		ModelVersion: rollbackPanickyModel.ModelVersion,
		Format:       "JSON",
		Data:         json.RawMessage(`{"name":"w"}`),
	}
}

func expectPanicked(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if rec := recover(); rec == nil {
			t.Error("expected the callout panic to reach the caller; recovery is the door's job")
		}
	}()
	fn()
}

// --- per-flow drivers ---

const rollbackAbsentEntityID = "00000000-0000-4000-8000-000000000042"

var rollbackCondition = []byte(`{"type":"simple","jsonPath":"$.name","operatorType":"EQUALS","value":"w"}`)

func driveCreateFailure(_ *testing.T, hn *rollbackHarness) error {
	_, err := hn.h.CreateEntity(hn.ctx, rollbackWidgetInput())
	return err
}

func driveDeleteFailure(_ *testing.T, hn *rollbackHarness) error {
	_, err := hn.h.DeleteEntity(hn.ctx, rollbackAbsentEntityID)
	return err
}

func driveDeleteAllFailure(_ *testing.T, hn *rollbackHarness) error {
	_, err := hn.h.DeleteAllEntities(hn.ctx, rollbackModel.EntityName, rollbackModel.ModelVersion)
	return err
}

func driveDeleteConditionalFailure(_ *testing.T, hn *rollbackHarness) error {
	_, err := hn.h.DeleteEntitiesConditional(hn.ctx, rollbackModel.EntityName, rollbackModel.ModelVersion,
		rollbackCondition, nil, false)
	return err
}

func driveCreateCollectionFailure(_ *testing.T, hn *rollbackHarness) error {
	_, err := hn.h.CreateEntityCollection(hn.ctx, []CollectionItem{{
		ModelName:    rollbackModel.EntityName,
		ModelVersion: 1,
		Payload:      json.RawMessage(`{"name":"w"}`),
	}})
	return err
}

func driveUpdateFailure(_ *testing.T, hn *rollbackHarness) error {
	_, err := hn.h.UpdateEntity(hn.ctx, UpdateEntityInput{
		EntityID: rollbackAbsentEntityID,
		Format:   "JSON",
		Data:     json.RawMessage(`{"name":"w"}`),
	})
	return err
}

func driveUpdateCollectionFailure(_ *testing.T, hn *rollbackHarness) error {
	_, err := hn.h.UpdateEntityCollection(hn.ctx, []UpdateCollectionItem{{
		EntityID: rollbackAbsentEntityID,
		Payload:  json.RawMessage(`{"name":"w"}`),
	}})
	return err
}
