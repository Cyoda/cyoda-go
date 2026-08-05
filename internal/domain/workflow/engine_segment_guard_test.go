package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
	"github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

// countingTxMgr wraps a real TransactionManager and records which txIDs were
// rolled back, so a test can assert the engine released the segment it opened
// without reaching into plugin internals.
type countingTxMgr struct {
	spi.TransactionManager
	rolledBack []string
}

func (c *countingTxMgr) Rollback(ctx context.Context, txID string) error {
	c.rolledBack = append(c.rolledBack, txID)
	return c.TransactionManager.Rollback(ctx, txID)
}

func (c *countingTxMgr) sawRollbackOf(txID string) bool {
	for _, id := range c.rolledBack {
		if id == txID {
			return true
		}
	}
	return false
}

// TestEngine_CriterionFailureAfterSegment_RollsBackOpenSegment is coverage row 4a.
// A FUNCTION criterion that fails mid-cascade is an ordinary occurrence — a
// compute node being down is enough — and it returns from cascadeAutomated,
// outside executeProcessors, after currentTxID has advanced past a
// COMMIT_BEFORE_DISPATCH segment.
func TestEngine_CriterionFailureAfterSegment_RollsBackOpenSegment(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} { // coverage row 8c
		t.Run(backend, func(t *testing.T) {
			h := newSegmentGuardHarness(t, backend)

			// Workflow: state A --[CBD processor]--> B --[FUNCTION criterion]--> C.
			// The CBD processor commits TX_pre and opens TX_post; the criterion
			// on the next automated transition then fails.
			h.registerCBDProcessor("segmenter")
			h.registerFailingCriterion("gatekeeper", errors.New("compute node unavailable"))

			entryTxID, entryCtx := h.begin(t)
			_, err := h.engine.Execute(entryCtx, h.entity, "")
			if err == nil {
				t.Fatal("expected the criterion failure to surface")
			}

			openTxID := h.lastSegmentTxID(t) // TX_post, recorded by the CBD stub
			if openTxID == entryTxID {
				t.Fatal("test did not segment; it proves nothing")
			}
			if !h.txMgr.sawRollbackOf(openTxID) {
				t.Fatalf("engine leaked segment %s after a criterion failure", openTxID)
			}
			if h.txMgr.sawRollbackOf(entryTxID) {
				t.Fatalf("engine rolled back the caller's entry transaction %s; that is the caller's to own", entryTxID)
			}
		})
	}
}

// TestEngine_PanicAfterSegment_RollsBackOpenSegment is coverage row 4. The guard
// must not swallow the panic — the door's recovery middleware owns that decision.
func TestEngine_PanicAfterSegment_RollsBackOpenSegment(t *testing.T) {
	h := newSegmentGuardHarness(t, "memory")
	h.registerCBDProcessor("segmenter")
	h.registerPanickingCriterion("boom")

	entryTxID, entryCtx := h.begin(t)

	func() {
		defer func() {
			if rec := recover(); rec == nil {
				t.Error("guard swallowed the panic; recovery is the door's job, not the engine's")
			}
		}()
		_, _ = h.engine.Execute(entryCtx, h.entity, "")
	}()

	openTxID := h.lastSegmentTxID(t)
	if !h.txMgr.sawRollbackOf(openTxID) {
		t.Fatalf("engine leaked segment %s through a panic", openTxID)
	}
	if h.txMgr.sawRollbackOf(entryTxID) {
		t.Fatalf("engine rolled back the caller's entry transaction %s", entryTxID)
	}
}

// TestEngine_PanicInsideSegmentingFrames_RollsBackOpenSegment extends coverage
// row 4 to the other two frames that can be holding a live segment when the
// stack unwinds. A panic skips every `openCtx, openTxID = ...` advance above it,
// so the entry-point guard still sees the caller's entry txID and cannot help:
// whichever frame owns the open segment has to release it on the way out.
func TestEngine_PanicInsideSegmentingFrames_RollsBackOpenSegment(t *testing.T) {
	t.Run("later processor in the same pipeline", func(t *testing.T) {
		h := newSegmentGuardHarness(t, "memory")
		h.registerCBDProcessor("segmenter")
		h.registerPanickingProcessorAfterCBD("boom-proc")

		entryTxID, entryCtx := h.begin(t)
		expectPanic(t, func() { _, _ = h.engine.Execute(entryCtx, h.entity, "") })

		h.assertSegmentReleased(t, entryTxID)
	})

	t.Run("audit record after the transition fired", func(t *testing.T) {
		h := newSegmentGuardHarness(t, "memory")
		h.registerCBDProcessor("segmenter")
		// Fired by name so the CBD runs under attemptTransition/fireTransition;
		// TRANSITION_MADE is the first event recorded after that hand-off.
		h.makeSegmentTransitionManual()
		h.panicOnAuditEvent = spi.SMEventTransitionMade

		entryTxID, entryCtx := h.begin(t)
		expectPanic(t, func() { _, _ = h.engine.Execute(entryCtx, h.entity, "segment") })

		h.assertSegmentReleased(t, entryTxID)
	})
}

func expectPanic(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if rec := recover(); rec == nil {
			t.Error("guard swallowed the panic; recovery is the door's job, not the engine's")
		}
	}()
	fn()
}

func (h *segmentGuardHarness) assertSegmentReleased(t *testing.T, entryTxID string) {
	t.Helper()
	openTxID := h.lastSegmentTxID(t)
	if openTxID == "" || openTxID == entryTxID {
		t.Fatal("test did not segment; it proves nothing")
	}
	if !h.txMgr.sawRollbackOf(openTxID) {
		t.Fatalf("engine leaked segment %s through a panic", openTxID)
	}
	if h.txMgr.sawRollbackOf(entryTxID) {
		t.Fatalf("engine rolled back the caller's entry transaction %s", entryTxID)
	}
}

// TestExecuteCommitBeforeDispatch_EveryFailurePathRollsBack is coverage row 4b.
// Every failure return in executeCommitBeforeDispatch is `return nil, "", err`,
// which is exactly why the guard must read dedicated locals rather than the
// named returns.
func TestExecuteCommitBeforeDispatch_EveryFailurePathRollsBack(t *testing.T) {
	cases := []struct {
		name string
		fail func(*segmentGuardHarness)
	}{
		{"dispatch error, startNewTxOnDispatch=true", (*segmentGuardHarness).failDispatchNewTx},
		{"apply processor data", (*segmentGuardHarness).failApplyProcessorData},
		{"entity store for CAS", (*segmentGuardHarness).failEntityStoreLookup},
		{"CompareAndSave conflict", (*segmentGuardHarness).failCompareAndSave},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newSegmentGuardHarness(t, "memory")
			h.registerCBDProcessor("segmenter")
			tc.fail(h)

			entryTxID, entryCtx := h.begin(t)
			if _, err := h.engine.Execute(entryCtx, h.entity, ""); err == nil {
				t.Fatal("expected a failure")
			}
			openTxID := h.lastSegmentTxID(t)
			if openTxID != "" && !h.txMgr.sawRollbackOf(openTxID) {
				t.Fatalf("TX_post %s leaked on the %q path", openTxID, tc.name)
			}
			_ = entryTxID
		})
	}
}

// --- harness ---

// segmentGuardHarness builds an Engine over a real backend with a workflow
// whose first automated transition carries a COMMIT_BEFORE_DISPATCH processor,
// so every test in this file exercises a genuine segment boundary rather than a
// simulated one. The external-processing stub is configured per test via the
// register*/fail* helpers.
type segmentGuardHarness struct {
	factory  spi.StoreFactory
	txMgr    *countingTxMgr
	engine   *Engine
	entity   *spi.Entity
	modelRef spi.ModelRef
	ctx      context.Context
	wf       spi.WorkflowDefinition

	dispatchProcessor func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition, workflow, transition, txID string) (*spi.Entity, error)
	dispatchCriteria  func(ctx context.Context, entity *spi.Entity, criterion json.RawMessage, target, workflow, transition, processor, txID string) (bool, string, error)

	// segmentTxIDs records every txID the CBD processor stub was dispatched
	// with. The harness always sets startNewTxOnDispatch=true, so that value is
	// TX_post — the segment the engine opened and owes a rollback on.
	segmentTxIDs []string

	// Armed from inside the dispatch stub so only the POST-dispatch store calls
	// fail; the pre-dispatch flush goes through the same two entry points.
	entityStoreErr error
	casErr         error

	// panicOnAuditEvent, when set, makes the audit store panic on that event
	// type — the cheapest way to blow up in a specific engine frame.
	panicOnAuditEvent spi.StateMachineEventType
}

// segmentGuardProc is the harness's contract.ExternalProcessingService stub.
type segmentGuardProc struct{ h *segmentGuardHarness }

func (p *segmentGuardProc) DispatchProcessor(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition, workflow, transition, txID string) (*spi.Entity, error) {
	if p.h.dispatchProcessor != nil {
		return p.h.dispatchProcessor(ctx, entity, proc, workflow, transition, txID)
	}
	return nil, nil
}

func (p *segmentGuardProc) DispatchCriteria(ctx context.Context, entity *spi.Entity, criterion json.RawMessage, target, workflow, transition, processor, txID string) (bool, string, error) {
	if p.h.dispatchCriteria != nil {
		return p.h.dispatchCriteria(ctx, entity, criterion, target, workflow, transition, processor, txID)
	}
	return true, "", nil
}

func (p *segmentGuardProc) DispatchFunction(context.Context, *spi.Entity, spi.ScheduleFunction, string, string, string) (contract.FunctionResult, error) {
	return contract.FunctionResult{}, nil
}

// hookedFactory lets a test fail the EntityStore lookup and the CompareAndSave
// that executeCommitBeforeDispatch performs after the dispatch returns. Every
// other capability delegates to the real backend factory.
type hookedFactory struct {
	spi.StoreFactory
	h *segmentGuardHarness
}

func (f *hookedFactory) EntityStore(ctx context.Context) (spi.EntityStore, error) {
	if f.h.entityStoreErr != nil {
		return nil, f.h.entityStoreErr
	}
	es, err := f.StoreFactory.EntityStore(ctx)
	if err != nil {
		return nil, err
	}
	return &hookedEntityStore{EntityStore: es, h: f.h}, nil
}

func (f *hookedFactory) StateMachineAuditStore(ctx context.Context) (spi.StateMachineAuditStore, error) {
	as, err := f.StoreFactory.StateMachineAuditStore(ctx)
	if err != nil {
		return nil, err
	}
	return &hookedAuditStore{StateMachineAuditStore: as, h: f.h}, nil
}

type hookedAuditStore struct {
	spi.StateMachineAuditStore
	h *segmentGuardHarness
}

func (s *hookedAuditStore) Record(ctx context.Context, entityID string, event spi.StateMachineEvent) error {
	if s.h.panicOnAuditEvent != "" && event.EventType == s.h.panicOnAuditEvent {
		panic("audit store panicked on " + string(event.EventType))
	}
	return s.StateMachineAuditStore.Record(ctx, entityID, event)
}

type hookedEntityStore struct {
	spi.EntityStore
	h *segmentGuardHarness
}

func (s *hookedEntityStore) CompareAndSave(ctx context.Context, entity *spi.Entity, expectedTxID string) (int64, error) {
	if s.h.casErr != nil {
		return 0, s.h.casErr
	}
	return s.EntityStore.CompareAndSave(ctx, entity, expectedTxID)
}

func newSegmentGuardHarness(t *testing.T, backend string) *segmentGuardHarness {
	t.Helper()
	ctx := ctxWithTenant(testTenant)
	h := &segmentGuardHarness{
		ctx:      ctx,
		modelRef: spi.ModelRef{EntityName: "segment-guard", ModelVersion: "1.0"},
	}

	var inner spi.StoreFactory
	switch backend {
	case "memory":
		f := memory.NewStoreFactory()
		t.Cleanup(func() { f.Close() })
		inner = f
	case "sqlite":
		f, err := sqlite.NewStoreFactoryForTest(ctx, filepath.Join(t.TempDir(), "segment-guard.db"))
		if err != nil {
			t.Fatalf("sqlite.NewStoreFactoryForTest: %v", err)
		}
		t.Cleanup(func() { _ = f.Close() })
		inner = f
	default:
		t.Fatalf("unknown backend %q", backend)
	}

	tm, err := inner.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	h.txMgr = &countingTxMgr{TransactionManager: tm}
	h.factory = &hookedFactory{StoreFactory: inner, h: h}
	h.engine = NewEngine(h.factory, common.NewTestUUIDGenerator(), h.txMgr,
		WithExternalProcessing(&segmentGuardProc{h: h}))

	h.wf = spi.WorkflowDefinition{
		Version: "1.1", Name: "SegmentGuardWF", InitialState: "A", Active: true,
		States: map[string]spi.StateDefinition{
			"A": {Transitions: []spi.TransitionDefinition{{Name: "segment", Next: "B"}}},
			"B": {},
		},
	}
	return h
}

// registerCBDProcessor puts a COMMIT_BEFORE_DISPATCH processor on the first
// automated transition and records the txID the engine dispatches it with.
// startNewTxOnDispatch=true so TX_post exists — and is observable — before the
// callout runs.
func (h *segmentGuardHarness) registerCBDProcessor(name string) {
	startNewTx := true
	st := h.wf.States["A"]
	st.Transitions[0].Processors = []spi.ProcessorDefinition{{
		Type:          ProcessorTypeExternalized,
		Name:          name,
		ExecutionMode: ExecutionModeCommitBeforeDispatch,
		Config:        spi.ProcessorConfig{StartNewTxOnDispatch: &startNewTx},
	}}
	h.wf.States["A"] = st
	h.dispatchProcessor = func(_ context.Context, _ *spi.Entity, _ spi.ProcessorDefinition, _, _, txID string) (*spi.Entity, error) {
		h.segmentTxIDs = append(h.segmentTxIDs, txID)
		return nil, nil
	}
}

// registerPanickingProcessorAfterCBD appends a SYNC processor that panics to the
// same pipeline, so the blow-up happens inside executeProcessors while TX_post is
// live but before cascadeAutomated has seen it.
func (h *segmentGuardHarness) registerPanickingProcessorAfterCBD(name string) {
	st := h.wf.States["A"]
	st.Transitions[0].Processors = append(st.Transitions[0].Processors, spi.ProcessorDefinition{
		Type: ProcessorTypeExternalized, Name: name, ExecutionMode: ExecutionModeSync,
	})
	h.wf.States["A"] = st
	prev := h.dispatchProcessor
	h.dispatchProcessor = func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition, workflow, transition, txID string) (*spi.Entity, error) {
		if proc.Name == name {
			panic("processor callout panicked: " + name)
		}
		return prev(ctx, entity, proc, workflow, transition, txID)
	}
}

// makeSegmentTransitionManual routes the segmenting transition through
// attemptTransition/fireTransition instead of the cascade loop.
func (h *segmentGuardHarness) makeSegmentTransitionManual() {
	st := h.wf.States["A"]
	st.Transitions[0].Manual = true
	h.wf.States["A"] = st
}

// addGateTransition gives state B an automated transition guarded by a FUNCTION
// criterion, so the criterion runs after the CBD segment boundary.
func (h *segmentGuardHarness) addGateTransition(name string) {
	h.wf.States["B"] = spi.StateDefinition{Transitions: []spi.TransitionDefinition{{
		Name: "gate", Next: "C",
		Criterion: json.RawMessage(fmt.Sprintf(`{"type":"function","function":{"name":%q}}`, name)),
	}}}
	h.wf.States["C"] = spi.StateDefinition{}
}

func (h *segmentGuardHarness) registerFailingCriterion(name string, failure error) {
	h.addGateTransition(name)
	h.dispatchCriteria = func(context.Context, *spi.Entity, json.RawMessage, string, string, string, string, string) (bool, string, error) {
		return false, "", failure
	}
}

func (h *segmentGuardHarness) registerPanickingCriterion(name string) {
	h.addGateTransition(name)
	h.dispatchCriteria = func(context.Context, *spi.Entity, json.RawMessage, string, string, string, string, string) (bool, string, error) {
		panic("criterion callout panicked: " + name)
	}
}

func (h *segmentGuardHarness) failDispatchNewTx() {
	h.dispatchProcessor = func(_ context.Context, _ *spi.Entity, _ spi.ProcessorDefinition, _, _, txID string) (*spi.Entity, error) {
		h.segmentTxIDs = append(h.segmentTxIDs, txID)
		return nil, errors.New("compute node refused the dispatch")
	}
}

func (h *segmentGuardHarness) failApplyProcessorData() {
	h.dispatchProcessor = func(_ context.Context, _ *spi.Entity, _ spi.ProcessorDefinition, _, _, txID string) (*spi.Entity, error) {
		h.segmentTxIDs = append(h.segmentTxIDs, txID)
		return &spi.Entity{Data: []byte(`{not json`)}, nil
	}
}

func (h *segmentGuardHarness) failEntityStoreLookup() {
	h.dispatchProcessor = func(_ context.Context, _ *spi.Entity, _ spi.ProcessorDefinition, _, _, txID string) (*spi.Entity, error) {
		h.segmentTxIDs = append(h.segmentTxIDs, txID)
		h.entityStoreErr = errors.New("entity store unavailable")
		return nil, nil
	}
}

func (h *segmentGuardHarness) failCompareAndSave() {
	h.dispatchProcessor = func(_ context.Context, _ *spi.Entity, _ spi.ProcessorDefinition, _, _, txID string) (*spi.Entity, error) {
		h.segmentTxIDs = append(h.segmentTxIDs, txID)
		h.casErr = spi.ErrConflict
		return nil, nil
	}
}

// begin persists the configured workflow and model, opens the caller's entry
// transaction and stages the entity on it — the shape a handler hands the
// engine.
func (h *segmentGuardHarness) begin(t *testing.T) (string, context.Context) {
	t.Helper()
	saveWorkflow(t, h.factory, h.ctx, h.modelRef, []spi.WorkflowDefinition{h.wf})
	ms, err := h.factory.ModelStore(h.ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	if err := ms.Save(h.ctx, &spi.ModelDescriptor{Ref: h.modelRef}); err != nil {
		t.Fatalf("Save model: %v", err)
	}

	txID, txCtx, err := h.txMgr.Begin(h.ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	// Released through the wrapped manager so the cleanup never pollutes the
	// rolledBack ledger the assertions read.
	t.Cleanup(func() { _ = h.txMgr.TransactionManager.Rollback(h.ctx, txID) })

	h.entity = &spi.Entity{
		Meta: spi.EntityMeta{
			ID: "segment-guard-1", TenantID: testTenant,
			ModelRef: h.modelRef, TransactionID: txID,
		},
		Data: []byte(`{"x":1}`),
	}
	return txID, txCtx
}

// lastSegmentTxID returns the txID the CBD stub was dispatched with — TX_post,
// the segment the engine owns from that point on. Empty when the flow never
// reached the dispatch.
func (h *segmentGuardHarness) lastSegmentTxID(t *testing.T) string {
	t.Helper()
	if len(h.segmentTxIDs) == 0 {
		return ""
	}
	return h.segmentTxIDs[len(h.segmentTxIDs)-1]
}
