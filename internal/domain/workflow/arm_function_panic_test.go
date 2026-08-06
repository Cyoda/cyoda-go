package workflow

// arm_function_panic_test.go — the arm-time Function callout must leave the
// per-tx gate exactly as it found it, panic or not.
//
// A joined (routed compute-node callback) write installs a held gate handle on
// ctx and registers `defer releaseGate()`. armViaFunction suspends that gate
// across the blocking dispatch. If the dispatch panics and the re-acquire never
// runs, the unwinding deferred release unlocks a mutex nobody holds — a runtime
// fatal (`sync: unlock of unlocked mutex`) that NO recover() can catch, so the
// HTTP/gRPC recovery interceptors and the health latch are all bypassed and the
// node dies. Worse, if another goroutine won the gate in between, the second
// unlock frees THEIR gate and refs double-decrements, deleting a held registry
// entry: mutual exclusion for that txID silently gone.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
)

// panickingFunctionProc panics inside the Function callout — standing in for any
// compute-node dispatch that faults (nil map write, index out of range, a panic
// raised by a codec below the dispatcher).
type panickingFunctionProc struct{}

func (panickingFunctionProc) DispatchProcessor(_ context.Context, entity *spi.Entity, _ spi.ProcessorDefinition, _, _, _ string) (*spi.Entity, error) {
	return entity, nil
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

	reg := txgate.New()
	const txID = "tx-arm-panic"

	// Stand in for the joined callback path: hold gate(T) and record it on ctx
	// exactly as Handler.acquireJoinedGate does.
	release := reg.Acquire(txID)
	ctx, _ := txgate.WithHeld(ctxWithTenant(testTenant), reg, txID, &release)

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

	// The caller's `defer releaseGate()` is about to run. It MUST find the gate
	// held, or it unlocks an unlocked mutex.
	acquired := make(chan struct{})
	go func() {
		rel := reg.Acquire(txID)
		close(acquired)
		rel()
	}()
	select {
	case <-acquired:
		t.Fatal("gate(T) was free after the panic unwound: resume() never ran, so the caller's deferred release will unlock an unlocked mutex (runtime fatal, uncatchable)")
	case <-time.After(50 * time.Millisecond):
	}

	// And releasing it once must free it — the deferred resume re-acquires
	// through the caller's own release variable, so one release is enough.
	release()
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("gate(T) still held after the caller's release() — resume did not rebind the release func")
	}
}
