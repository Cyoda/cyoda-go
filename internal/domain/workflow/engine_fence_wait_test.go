package workflow

import (
	"context"
	"errors"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/fence"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// A failed ASYNC_NEW_TX processor's write in progress lands BEFORE its
// savepoint is undone, so it is not in the committed result.
func TestAsyncNewTx_FailedProcessorsWriteLandsBeforeSavepointIsUndone(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)
	gate := txgate.New()
	f := fence.New(gate)

	base := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "fence-wait", ModelVersion: "1.0"}
	const lateID = "late-write"

	writeStarted := make(chan struct{})
	letWriteLand := make(chan struct{})
	writeLanded := make(chan struct{})

	ext := &mockExternalProcessing{
		dispatchFunc: func(ctx context.Context, _ *spi.Entity, _ spi.ProcessorDefinition, _, _, txID string) (*spi.Entity, error) {
			_, end := f.Begin(ctx, "callout-1", txID, fence.Pairs(ctx))
			defer end() // the wait happens here, before the engine undoes the savepoint
			f.Advance("callout-1", 1)

			// The cnode's callback: joined, admitted, under the lock, checked —
			// and then slow.
			joined, err := txMgr.Join(base, txID)
			if err != nil {
				t.Errorf("Join: %v", err)
				return nil, err
			}
			admitted, err := f.Admit(joined, []fence.Pair{{Callout: "callout-1", Major: 1}})
			if err != nil {
				t.Errorf("Admit: %v", err)
				return nil, err
			}
			go func() {
				release := gate.Acquire(txID)
				defer release()
				// Closed before the lock is released (defers run last in, first
				// out), so end's wait cannot return until writeLanded is closed.
				defer close(writeLanded)
				if err := fence.Check(admitted); err != nil {
					t.Errorf("Check: %v", err)
					return
				}
				close(writeStarted)
				<-letWriteLand
				es, err := factory.EntityStore(admitted)
				if err != nil {
					t.Errorf("EntityStore: %v", err)
					return
				}
				late := makeEntity(lateID, modelRef, map[string]any{"late": true})
				late.Meta.TransactionID = txID
				if _, err := es.Save(admitted, late); err != nil {
					t.Errorf("late Save: %v", err)
				}
			}()
			<-writeStarted
			go func() {
				time.Sleep(50 * time.Millisecond) // end() is blocked in the wait meanwhile
				close(letWriteLand)
			}()
			return nil, errors.New("the cnode answered: failed")
		},
	}
	engine := NewEngine(factory, uuids, txMgr, WithExternalProcessing(ext))

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
	if _, err := engine.Execute(txCtx, entity, ""); err != nil {
		t.Fatalf("an ASYNC_NEW_TX processor failure must not fail the operation: %v", err)
	}
	select {
	case <-writeLanded:
	default:
		t.Fatal("the engine carried on while the failed processor's write was still in progress")
	}
	if err := txMgr.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	es, err := factory.EntityStore(base)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	if _, err := es.Get(base, lateID); !errors.Is(err, spi.ErrNotFound) {
		t.Fatalf("the failed processor's write is in the committed result (Get err = %v): it landed after its savepoint was undone", err)
	}
}
