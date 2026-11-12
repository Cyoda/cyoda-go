package workflow

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/platformclock"
	"github.com/cyoda-platform/cyoda-go/internal/txcoord"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// A callback of a failed ASYNC_NEW_TX processor still in progress when the
// processor's callout ends is waited for, then discarded and answered 410: its
// write is not in the committed result, and the engine carries on only after
// it has ended.
func TestAsyncNewTx_CallbackInProgressAtTheCalloutsEndIsDiscarded(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)
	coord := txcoord.New(txMgr)

	base := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "fence-wait", ModelVersion: "1.0"}
	const lateID = "late-write"

	writeStarted := make(chan struct{})
	var written atomic.Bool
	callbackEnded := make(chan *common.AppError, 1)

	ext := &mockExternalProcessing{
		dispatchFunc: func(ctx context.Context, _ *spi.Entity, _ spi.ProcessorDefinition, _, _, txID string) (*spi.Entity, error) {
			_, end := coord.BeginCallout(ctx, txID, "callout-1")
			defer end() // the wait happens here, before the engine ends the processor's scope
			coord.Advance("callout-1", 1)

			// The cnode's callback: joined, in its scope — and then slow.
			joined, err := txMgr.Join(base, txID)
			if err != nil {
				t.Errorf("Join: %v", err)
				return nil, err
			}
			go func() {
				scope, sctx, err := coord.Enter(joined, txID, []txcoord.Pair{{Callout: "callout-1", Major: 1}}, txcoord.Write, 0)
				if err != nil {
					t.Errorf("Enter: %v", err)
					close(writeStarted)
					callbackEnded <- nil
					return
				}
				close(writeStarted)
				// The write lands once the callout has ended under it: the
				// owner's end() has unregistered the callout and now waits.
				deadline := time.Now().Add(5 * time.Second)
				for !errors.Is(txcoord.Check(sctx), txcoord.ErrSuperseded) {
					if time.Now().After(deadline) {
						t.Error("the callout never ended under the callback")
						break
					}
					time.Sleep(time.Millisecond)
				}
				es, err := factory.EntityStore(sctx)
				if err != nil {
					t.Errorf("EntityStore: %v", err)
				} else {
					late := makeEntity(lateID, modelRef, map[string]any{"late": true})
					late.Meta.TransactionID = txID
					if _, err := es.Save(sctx, late); err != nil {
						t.Errorf("late Save: %v", err)
					}
				}
				written.Store(true)
				callbackEnded <- scope.End(nil)
			}()
			<-writeStarted
			return nil, errors.New("the cnode answered: failed")
		},
	}
	engine := NewEngine(factory, uuids, txMgr, platformclock.Real(), WithExternalProcessing(ext), WithCoordination(coord))

	saveWorkflow(t, factory, base, modelRef, []spi.WorkflowDefinition{{
		Version: "1.1", Name: "FenceWaitWF", InitialState: "INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"INITIAL": {Transitions: []spi.TransitionDefinition{{
				Name: "RUN", Next: "DONE",
				Processors: []spi.ProcessorDefinition{
					{Type: ProcessorTypeExternalized, Name: "fails", ExecutionMode: ExecutionModeAsyncNewTx},
				},
			}}},
			"DONE": {},
		},
	}})

	txID, txCtx, err := txMgr.Begin(base)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	entity := makeEntity("fence-wait-1", modelRef, map[string]any{"x": 1})
	entity.Meta.TransactionID = txID
	if _, err := engine.Execute(txCtx, entity); err != nil {
		t.Fatalf("an ASYNC_NEW_TX processor failure must not fail the operation: %v", err)
	}
	if !written.Load() {
		t.Fatal("the engine carried on while the failed processor's callback was still in progress")
	}
	select {
	case answer := <-callbackEnded:
		if answer == nil || answer.Code != common.ErrCodeCalloutSuperseded {
			t.Fatalf("the callback in progress at the callout's end was answered %v; want 410 CALLOUT_SUPERSEDED", answer)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the callback never ended")
	}
	if err := txMgr.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	es, err := factory.EntityStore(base)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	if _, err := es.Get(base, lateID); !errors.Is(err, spi.ErrNotFound) {
		t.Fatalf("the discarded callback's write is in the committed result (Get err = %v)", err)
	}
}
