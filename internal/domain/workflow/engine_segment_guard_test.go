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
	// commit, when set, overrides Commit's delegation to the real manager —
	// lets a test control or observe commit behavior (e.g. block until ctx
	// is Done, so a caller-injected short budget can be observed expiring
	// for real) without a separate wrapper type. nil preserves the default
	// pass-through delegation every other test in this file relies on.
	commit func(ctx context.Context, txID string) error
}

func (c *countingTxMgr) Commit(ctx context.Context, txID string) error {
	if c.commit != nil {
		return c.commit(ctx, txID)
	}
	return c.TransactionManager.Commit(ctx, txID)
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

// TestCBD_ApplyResultConflict_CarriesPostSegmentMarker: the apply-result CAS runs
// after TX_pre committed and after the dispatch fired, so a conflict there is not
// something a caller can isolate and retry past. The marker is how the caller
// tells it apart from a first-segment-flush precondition failure, which is.
func TestCBD_ApplyResultConflict_CarriesPostSegmentMarker(t *testing.T) {
	h := newSegmentGuardHarness(t, "memory")
	h.registerCBDProcessor("segmenter")
	h.failCompareAndSave() // the apply-result CAS, post-commit

	err := h.fireSegment(t, "")
	if err == nil {
		t.Fatal("expected the apply-result CAS conflict to surface")
	}
	if got := h.casSites; len(got) != 1 || got[0] != casSiteApplyResult {
		t.Fatalf("injection did not land on the apply-result CAS: sites = %v", got)
	}
	if !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("lost the conflict sentinel: %v", err)
	}
	if !errors.Is(err, ErrPostSegmentConflict) {
		t.Fatalf("post-commit conflict not marked; a caller cannot tell it from an isolable one: %v", err)
	}
}

// TestCBD_FirstFlushConflict_HasNoPostSegmentMarker guards the other side. This
// one IS isolable and must stay that way.
func TestCBD_FirstFlushConflict_HasNoPostSegmentMarker(t *testing.T) {
	h := newSegmentGuardHarness(t, "memory")
	h.registerCBDProcessor("segmenter")
	h.failFirstFlushCAS(spi.ErrConflict) // flushAndCommitSegment, pre-commit

	err := h.fireSegment(t, staleIfMatchTxID)
	if err == nil {
		t.Fatal("expected the first-segment flush conflict to surface")
	}
	if got := h.casSites; len(got) != 1 || got[0] != casSiteFirstFlush {
		t.Fatalf("injection did not land on the first-segment flush: sites = %v", got)
	}
	if h.dispatched {
		t.Fatal("the flush conflict was raised after the dispatch fired; the case proves nothing")
	}
	if !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("lost the conflict sentinel: %v", err)
	}
	if errors.Is(err, ErrPostSegmentConflict) {
		t.Fatal("pre-commit precondition failure wrongly marked post-segment; the batch would abort instead of isolating the item")
	}
	// The infra marker is the OTHER thing UpdateEntityCollection excludes from
	// per-item isolation (service.go's engineErr branch). Marking a precondition
	// failure with it would turn an isolable ENTITY_MODIFIED item into a
	// request-wide failure that takes its successful siblings with it.
	if errors.Is(err, ErrCommitBeforeDispatchInfra) {
		t.Fatal("a caller's stale If-Match was marked as infrastructure; the batch would abort instead of isolating the item")
	}
}

// --- Segment-boundary CAS failures that are NOT the caller's business ---

// TestCBD_FirstFlushStoreFailure_IsMarkedInfra — the first-segment flush's
// CompareAndSave fails for two very different reasons. One is the caller's
// stale If-Match (above). The other is the store itself: a cancelled statement,
// a missing relation, a saturated pool. Those carry driver wording and a
// SQLSTATE, and an unmarked engine error lands on the entity service's
// catch-all, which mints a 400 WORKFLOW_FAILED whose detail is that text
// verbatim. Mark them so they take the sanitized-5xx-with-a-ticket path the
// engine's other infrastructure failures already take.
func TestCBD_FirstFlushStoreFailure_IsMarkedInfra(t *testing.T) {
	h := newSegmentGuardHarness(t, "memory")
	h.registerCBDProcessor("segmenter")
	storeErr := errors.New("ERROR: canceling statement due to statement timeout (SQLSTATE 57014)")
	h.failFirstFlushCAS(storeErr)

	err := h.fireSegment(t, staleIfMatchTxID)
	if err == nil {
		t.Fatal("expected the flush store failure to surface")
	}
	if got := h.casSites; len(got) != 1 || got[0] != casSiteFirstFlush {
		t.Fatalf("injection did not land on the first-segment flush: sites = %v", got)
	}
	if !errors.Is(err, ErrCommitBeforeDispatchInfra) {
		t.Errorf("store failure not marked infra; its text would reach a 400 body: %v", err)
	}
	if !errors.Is(err, storeErr) {
		t.Errorf("store cause dropped from the chain, so the server-side log loses it: %v", err)
	}
}

// TestCBD_FirstFlushUniqueViolation_StaysDomainAttributable — a composite
// unique-key clash raised by the same CompareAndSave IS the caller's business
// and has its own 409 mapping. Marking it as infrastructure would bury a
// precise, actionable answer under a generic ticket.
func TestCBD_FirstFlushUniqueViolation_StaysDomainAttributable(t *testing.T) {
	h := newSegmentGuardHarness(t, "memory")
	h.registerCBDProcessor("segmenter")
	h.failFirstFlushCAS(fmt.Errorf("claim key: %w", spi.ErrUniqueViolation))

	err := h.fireSegment(t, staleIfMatchTxID)
	if err == nil {
		t.Fatal("expected the unique-key violation to surface")
	}
	if !errors.Is(err, spi.ErrUniqueViolation) {
		t.Fatalf("lost the unique-violation sentinel: %v", err)
	}
	if errors.Is(err, ErrCommitBeforeDispatchInfra) {
		t.Errorf("a unique-key violation was marked as infrastructure: %v", err)
	}
}

// TestCBD_ApplyResultStoreFailure_IsMarkedInfra is the same split on the far
// side of the callout. The apply-result CAS chains ErrPostSegmentConflict, whose
// text reaches a 4xx body verbatim — so a raw store error there leaks exactly as
// the flush's does.
func TestCBD_ApplyResultStoreFailure_IsMarkedInfra(t *testing.T) {
	h := newSegmentGuardHarness(t, "memory")
	h.registerCBDProcessor("segmenter")
	storeErr := errors.New("ERROR: relation \"entities\" does not exist (SQLSTATE 42P01)")
	h.failCompareAndSaveWith(storeErr)

	err := h.fireSegment(t, "")
	if err == nil {
		t.Fatal("expected the apply-result store failure to surface")
	}
	if got := h.casSites; len(got) != 1 || got[0] != casSiteApplyResult {
		t.Fatalf("injection did not land on the apply-result CAS: sites = %v", got)
	}
	if !errors.Is(err, ErrCommitBeforeDispatchInfra) {
		t.Errorf("store failure not marked infra; its text would reach a 400 body: %v", err)
	}
	if !errors.Is(err, storeErr) {
		t.Errorf("store cause dropped from the chain, so the server-side log loses it: %v", err)
	}
}

// TestEngine_CriterionAfterSegment_CarriesCurrentSegmentTxID: the txID handed to
// a FUNCTION criterion is the compute node's join token. After a CBD segment the
// cascade-entry txID names a COMMITTED transaction, so a callback joining on it
// gets ErrTxNotFound.
func TestEngine_CriterionAfterSegment_CarriesCurrentSegmentTxID(t *testing.T) {
	h := newSegmentGuardHarness(t, "memory")
	h.registerCBDProcessor("segmenter")

	var criterionTxID string
	h.registerCriterion("gatekeeper", func(ctx context.Context, txID string) (bool, string, error) {
		criterionTxID = txID
		return true, "", nil
	})

	entryTxID, entryCtx := h.begin(t)
	if _, err := h.engine.Execute(entryCtx, h.entity, ""); err != nil {
		t.Fatalf("execute: %v", err)
	}

	segmentTxID := h.lastSegmentTxID(t)
	if segmentTxID == entryTxID {
		t.Fatal("test did not segment; it proves nothing")
	}
	if criterionTxID == entryTxID {
		t.Fatalf("criterion was handed the committed entry txID %s as its join token", entryTxID)
	}
	if criterionTxID != segmentTxID {
		t.Fatalf("criterion join token = %s, want the open segment %s", criterionTxID, segmentTxID)
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

	// flushCASErr fails the OTHER CompareAndSave a segmenting cascade makes —
	// flushAndCommitSegment's, which runs before TX_pre commits. Armed up front
	// rather than from the dispatch stub, because the flush happens first.
	flushCASErr error

	// dispatched records whether the CBD callout has fired. It is what separates
	// the two CAS sites: everything before it is the first-segment flush,
	// everything after it is the apply-result CAS.
	dispatched bool

	// casSites labels, in order, which site each CompareAndSave came from, so a
	// test can prove its injection landed where it meant it to.
	casSites []string

	// panicOnAuditEvent, when set, makes the audit store panic on that event
	// type — the cheapest way to blow up in a specific engine frame.
	panicOnAuditEvent spi.StateMachineEventType
}

// segmentGuardProc is the harness's contract.ExternalProcessingService stub.
type segmentGuardProc struct{ h *segmentGuardHarness }

func (p *segmentGuardProc) DispatchProcessor(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition, workflow, transition, txID string) (*spi.Entity, error) {
	p.h.dispatched = true
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

// The two CompareAndSave sites a COMMIT_BEFORE_DISPATCH cascade reaches. They
// sit on opposite sides of the callout, which is exactly why one is isolable and
// the other is not: the flush runs before TX_pre commits, the apply-result CAS
// after it committed and the dispatch fired.
const (
	casSiteFirstFlush  = "first-flush"
	casSiteApplyResult = "apply-result"
)

func (s *hookedEntityStore) CompareAndSave(ctx context.Context, entity *spi.Entity, expectedTxID string) (int64, error) {
	site := casSiteFirstFlush
	if s.h.dispatched {
		site = casSiteApplyResult
	}
	s.h.casSites = append(s.h.casSites, site)
	if site == casSiteFirstFlush && s.h.flushCASErr != nil {
		return 0, s.h.flushCASErr
	}
	if site == casSiteApplyResult && s.h.casErr != nil {
		return 0, s.h.casErr
	}
	return s.EntityStore.CompareAndSave(ctx, entity, expectedTxID)
}

// newSegmentGuardHarness builds the harness with the engine's default
// options plus any extras the caller supplies (e.g. WithCommitBudget for a
// test that needs to observe a real shielded-commit expiry). Every existing
// call site passes no extras and is unaffected.
func newSegmentGuardHarness(t *testing.T, backend string, extraOpts ...EngineOption) *segmentGuardHarness {
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
	opts := append([]EngineOption{WithExternalProcessing(&segmentGuardProc{h: h})}, extraOpts...)
	h.engine = NewEngine(h.factory, common.NewTestUUIDGenerator(), h.txMgr, opts...)

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

// registerCriterion wires the CBD workflow's gate transition to a FUNCTION
// criterion backed by fn, exposing exactly what DispatchCriteria receives
// (ctx, txID) so a test can capture the join token the engine handed it.
func (h *segmentGuardHarness) registerCriterion(name string, fn func(ctx context.Context, txID string) (bool, string, error)) {
	h.addGateTransition(name)
	h.dispatchCriteria = func(ctx context.Context, _ *spi.Entity, _ json.RawMessage, _, _, _, _, txID string) (bool, string, error) {
		return fn(ctx, txID)
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

// failCompareAndSave fails the apply-result CAS — the one
// executeCommitBeforeDispatch makes after TX_pre committed and after the callout
// returned. Armed from inside the dispatch stub so the pre-dispatch flush, which
// goes through the same hook, is untouched.
func (h *segmentGuardHarness) failCompareAndSave() {
	h.failCompareAndSaveWith(spi.ErrConflict)
}

// failCompareAndSaveWith is failCompareAndSave with the injected failure chosen
// by the caller, so a test can distinguish the conflict the caller owns from an
// infrastructure failure that only looks like one from the outside.
func (h *segmentGuardHarness) failCompareAndSaveWith(failure error) {
	h.dispatchProcessor = func(_ context.Context, _ *spi.Entity, _ spi.ProcessorDefinition, _, _, txID string) (*spi.Entity, error) {
		h.segmentTxIDs = append(h.segmentTxIDs, txID)
		h.casErr = failure
		return nil, nil
	}
}

// failFirstFlushCAS fails flushAndCommitSegment's CompareAndSave — the other
// site, reached before TX_pre commits and before anything is dispatched. That
// flush is only a CompareAndSave when the caller supplied an If-Match (spec
// §4.1), so fireSegment must be handed a non-empty one for this to land.
func (h *segmentGuardHarness) failFirstFlushCAS(failure error) {
	h.flushCASErr = failure
}

// staleIfMatchTxID is an expected-txID no entity in these tests carries.
const staleIfMatchTxID = "00000000-0000-4000-8000-000000000042"

// fireSegment drives the segmenting cascade through ManualTransitionWithIfMatch —
// the entry point UpdateEntityCollection uses — so the two conflict-shape tests
// differ only in WHERE the CAS fails. A non-empty ifMatch turns the first-segment
// flush into a CompareAndSave; with "" it stays a plain Save and the apply-result
// CAS is the cascade's only one.
func (h *segmentGuardHarness) fireSegment(t *testing.T, ifMatch string) error {
	t.Helper()
	h.makeSegmentTransitionManual()
	_, entryCtx := h.begin(t)
	h.entity.Meta.State = "A"
	_, err := h.engine.ManualTransitionWithIfMatch(entryCtx, h.entity, "segment", ifMatch)
	return err
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
