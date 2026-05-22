package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// TestManualTransitionWithIfMatch_NoSegmentingCascade verifies that without
// any COMMIT_BEFORE_DISPATCH processor in the cascade, the engine never
// consumes the If-Match expected-txID — the slot remains for the handler to
// apply post-engine via its own CompareAndSave path.
//
// Behaviour mirrors ManualTransition exactly: returns FinalTxID == input txID,
// FinalCtx carries the same TX, no engine-side flush happens.
func TestManualTransitionWithIfMatch_NoSegmentingCascade(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)
	engine := NewEngine(factory, uuids, txMgr)

	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "ifmatch-no-cbd", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "IfMatchNoCbdWF", InitialState: "PENDING", Active: true,
		States: map[string]spi.StateDefinition{
			"PENDING":  {Transitions: []spi.TransitionDefinition{{Name: "approve", Next: "APPROVED", Manual: true}}},
			"APPROVED": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	// Seed the entity in PENDING via TX1.
	seedTxID, seedCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("seed Begin: %v", err)
	}
	es, _ := factory.EntityStore(seedCtx)
	entity := &spi.Entity{
		Meta: spi.EntityMeta{
			ID: "ifmatch-no-cbd-1", TenantID: testTenant,
			ModelRef: modelRef, State: "PENDING", TransactionID: seedTxID,
		},
		Data: []byte(`{"x":1}`),
	}
	if _, err := es.Save(seedCtx, entity); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	if err := txMgr.Commit(seedCtx, seedTxID); err != nil {
		t.Fatalf("seed Commit: %v", err)
	}

	// Cascade-entry transaction (TX2) — handler-owned in the real flow.
	txID, txCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = txMgr.Rollback(txCtx, txID) }()

	entity.Meta.TransactionID = txID

	// Pass an arbitrary IfMatch value — the engine should NOT consume it
	// because the cascade has no CBD segments.
	res, err := engine.ManualTransitionWithIfMatch(txCtx, entity, "approve", "should-not-be-consumed")
	if err != nil {
		t.Fatalf("ManualTransitionWithIfMatch: %v", err)
	}
	if res.FinalTxID != txID {
		t.Errorf("FinalTxID = %q; want input txID %q (engine should not segment)", res.FinalTxID, txID)
	}
	if entity.Meta.State != "APPROVED" {
		t.Errorf("state = %q; want APPROVED", entity.Meta.State)
	}

	// The slot must still hold the expected — i.e. NOT consumed.
	got, ok := consumeIfMatch(res.FinalCtx)
	if !ok || got != "should-not-be-consumed" {
		t.Errorf("expected IfMatch slot to be untouched on non-CBD cascade, got (%q,%v)", got, ok)
	}
}

// TestManualTransitionWithIfMatch_CBDCascadeMatching verifies that with a
// CBD cascade and a matching IfMatch (the entity's current TransactionID),
// the engine commits TX_pre via CompareAndSave (not Save) and the cascade
// completes successfully. The IfMatch slot is consumed by the engine.
func TestManualTransitionWithIfMatch_CBDCascadeMatching(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)

	mock := &mockExternalProcessing{
		dispatchFunc: func(ctx context.Context, entity *spi.Entity, _ spi.ProcessorDefinition, _, _, _ string) (*spi.Entity, error) {
			modified, _ := json.Marshal(map[string]any{"enriched": true})
			return &spi.Entity{Data: modified}, nil
		},
	}
	engine := NewEngine(factory, uuids, txMgr, WithExternalProcessing(mock))

	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "ifmatch-cbd-match", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "IfMatchCbdWF", InitialState: "S_pre", Active: true,
		States: map[string]spi.StateDefinition{
			"S_pre": {Transitions: []spi.TransitionDefinition{
				{Name: "go", Next: "S_post", Manual: true,
					Processors: []spi.ProcessorDefinition{
						{Type: ProcessorTypeExternalized, Name: "cbd-proc", ExecutionMode: ExecutionModeCommitBeforeDispatch},
					}},
			}},
			"S_post": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	// Seed in S_pre via TX0; capture its committed txID as the matching IfMatch.
	seedTxID, seedCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("seed Begin: %v", err)
	}
	es, _ := factory.EntityStore(seedCtx)
	if _, err := es.Save(seedCtx, &spi.Entity{
		Meta: spi.EntityMeta{
			ID: "ifmatch-cbd-1", TenantID: testTenant,
			ModelRef: modelRef, State: "S_pre", TransactionID: seedTxID,
		},
		Data: []byte(`{"x":1}`),
	}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	if err := txMgr.Commit(seedCtx, seedTxID); err != nil {
		t.Fatalf("seed Commit: %v", err)
	}

	// Cascade-entry transaction (TX_pre).
	cTxID, cCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("cascade Begin: %v", err)
	}

	entity := &spi.Entity{
		Meta: spi.EntityMeta{
			ID: "ifmatch-cbd-1", TenantID: testTenant,
			ModelRef: modelRef, State: "S_pre", TransactionID: cTxID,
		},
		Data: []byte(`{"x":1}`),
	}

	// IfMatch = seed's committed txID. The engine's first-segment flush is
	// CompareAndSave(entity, expected=seedTxID) — must succeed.
	res, err := engine.ManualTransitionWithIfMatch(cCtx, entity, "go", seedTxID)
	if err != nil {
		t.Fatalf("ManualTransitionWithIfMatch (matching): %v", err)
	}
	if res.FinalTxID == cTxID {
		t.Errorf("FinalTxID == cascade-entry %q; expected post-segment txID", cTxID)
	}
	if entity.Meta.State != "S_post" {
		t.Errorf("state = %q; want S_post", entity.Meta.State)
	}

	// Slot consumed.
	if got, ok := consumeIfMatch(res.FinalCtx); ok {
		t.Errorf("expected IfMatch slot to be consumed, found %q", got)
	}

	// Commit final TX (Task-13 handler responsibility).
	if err := txMgr.Commit(res.FinalCtx, res.FinalTxID); err != nil {
		t.Fatalf("commit FinalTxID: %v", err)
	}
}

// TestManualTransitionWithIfMatch_CBDCascadeStaleAbortsBeforeDispatch
// verifies the spec §4.1 "strictly-earlier-enforcement" guarantee for
// cascades containing COMMIT_BEFORE_DISPATCH processors: a stale If-Match
// must short-circuit the cascade BEFORE the external processor is dispatched.
//
// The fake dispatch records whether it was called; the test asserts it was
// NOT called when the IfMatch is stale.
func TestManualTransitionWithIfMatch_CBDCascadeStaleAbortsBeforeDispatch(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)

	dispatched := false
	mock := &mockExternalProcessing{
		dispatchFunc: func(ctx context.Context, entity *spi.Entity, _ spi.ProcessorDefinition, _, _, _ string) (*spi.Entity, error) {
			dispatched = true
			return entity, nil
		},
	}
	engine := NewEngine(factory, uuids, txMgr, WithExternalProcessing(mock))

	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "ifmatch-cbd-stale", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "IfMatchStaleWF", InitialState: "S_pre", Active: true,
		States: map[string]spi.StateDefinition{
			"S_pre": {Transitions: []spi.TransitionDefinition{
				{Name: "go", Next: "S_post", Manual: true,
					Processors: []spi.ProcessorDefinition{
						{Type: ProcessorTypeExternalized, Name: "cbd-proc", ExecutionMode: ExecutionModeCommitBeforeDispatch},
					}},
			}},
			"S_post": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	seedTxID, seedCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("seed Begin: %v", err)
	}
	es, _ := factory.EntityStore(seedCtx)
	if _, err := es.Save(seedCtx, &spi.Entity{
		Meta: spi.EntityMeta{
			ID: "ifmatch-cbd-stale-1", TenantID: testTenant,
			ModelRef: modelRef, State: "S_pre", TransactionID: seedTxID,
		},
		Data: []byte(`{"x":1}`),
	}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	if err := txMgr.Commit(seedCtx, seedTxID); err != nil {
		t.Fatalf("seed Commit: %v", err)
	}

	cTxID, cCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("cascade Begin: %v", err)
	}

	entity := &spi.Entity{
		Meta: spi.EntityMeta{
			ID: "ifmatch-cbd-stale-1", TenantID: testTenant,
			ModelRef: modelRef, State: "S_pre", TransactionID: cTxID,
		},
		Data: []byte(`{"x":1}`),
	}

	// Stale IfMatch — must surface ErrConflict and NOT dispatch.
	_, err = engine.ManualTransitionWithIfMatch(cCtx, entity, "go", "tx-that-never-existed")
	if err == nil {
		t.Fatalf("expected error on stale IfMatch, got nil")
	}
	if !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("expected errors.Is(err, spi.ErrConflict); got %v", err)
	}
	if dispatched {
		t.Errorf("dispatch fired despite stale IfMatch — spec §4.1 strictly-earlier-enforcement violated")
	}

	_ = txMgr.Rollback(cCtx, cTxID)
}
