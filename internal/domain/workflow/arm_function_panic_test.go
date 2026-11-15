package workflow

// arm_function_panic_test.go — the arm-time Function callout must leave the
// per-tx gate exactly as it found it, panic or not.
//
// A joined (routed compute-node callback) write runs in a callback scope that
// holds the transaction's gate. armViaFunction pauses that scope — releasing
// the gate — across the blocking dispatch. If the dispatch panics and the
// resume never runs, the scope ends holding no slot, and the gate's queue
// would be granted to a sibling while the panicking request's scope is still
// being discarded: mutual exclusion for that transaction silently gone.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/txcoord"
)

// panickingFunctionProc panics inside the Function callout — standing in for any
// compute-node dispatch that faults (nil map write, index out of range, a panic
// raised by a codec below the dispatcher).
type panickingFunctionProc struct{}

func (panickingFunctionProc) DispatchProcessor(_ context.Context, _ *spi.Entity, _ spi.ProcessorDefinition, _, _, _ string) (*spi.Entity, error) {
	return nil, nil
}

func (panickingFunctionProc) DispatchCriteria(_ context.Context, _ *spi.Entity, _ json.RawMessage, _, _, _, _, _ string) (bool, string, error) {
	return true, "", nil
}

func (panickingFunctionProc) DispatchFunction(_ context.Context, _ *spi.Entity, _ spi.ScheduleFunction, _, _, _ string) (contract.FunctionResult, error) {
	panic("Function callout panicked")
}

func TestArmViaFunction_PanickingCalloutLeavesGateHeld(t *testing.T) {
	const nowMs = int64(1_700_000_000_000)
	engine, _ := setupEngineWithClockAndExtProc(t, nowMs, panickingFunctionProc{})

	coord := txcoord.New(engine.txMgr)
	const txID = "tx-arm-panic"
	_, end := coord.BeginCallout(context.Background(), txID, "c-1")
	defer end()
	coord.Advance("c-1", 1)
	pairs := []txcoord.Pair{{Callout: "c-1", Major: 1}}

	// Stand in for the joined callback path: a callback scope holding gate(T),
	// opened exactly as the join layer opens it.
	scope, ctx, err := coord.Enter(ctxWithTenant(testTenant), txID, pairs, txcoord.Outside, 0)
	if err != nil {
		t.Fatalf("Enter: %v", err)
	}

	wf := scheduleFunctionWorkflow("PanicWF", "boom")
	tr := wf.States["OPEN"].Transitions[0]
	entity := makeEntity("panic-e1", spi.ModelRef{EntityName: "panic-order", ModelVersion: "1.0"}, map[string]any{})

	panicked := false
	func() {
		defer func() { panicked = recover() != nil }()
		_, _, _ = engine.armViaFunction(ctx, entity, &wf, &tr, "OPEN", "task-1", nowMs, 1, txID)
	}()
	if !panicked {
		t.Fatal("the callout panic did not propagate — recovery is the door's job, not the engine's")
	}

	// The scope is about to end: it MUST hold the gate again, so that no other
	// request of the transaction runs before it has been discarded.
	sibling := func(wait time.Duration) error {
		wctx, cancel := context.WithTimeout(ctxWithTenant(testTenant), wait)
		defer cancel()
		s, _, err := coord.Enter(wctx, txID, pairs, txcoord.Outside, 0)
		if err == nil {
			s.End(nil)
		}
		return err
	}
	if err := sibling(50 * time.Millisecond); err == nil {
		t.Fatal("gate(T) was free after the panic unwound: the deferred resume never retook it")
	} else if !errors.Is(err, common.ErrClientGone) {
		t.Fatalf("the sibling was refused (%v); want it queued behind the panicking scope", err)
	}

	// Ending the scope once frees it.
	scope.End(&common.Outcome{Panicked: true})
	if err := sibling(time.Second); err != nil {
		t.Fatalf("gate(T) still held after the scope ended: %v", err)
	}
}
