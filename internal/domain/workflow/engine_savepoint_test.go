package workflow

import (
	"context"
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// failingSavepoints fails one of the three savepoint operations.
type failingSavepoints struct {
	spi.TransactionManager
	failCreate, failUndo, failRelease error
}

func (m failingSavepoints) Savepoint(ctx context.Context, txID string) (string, error) {
	if m.failCreate != nil {
		return "", m.failCreate
	}
	return m.TransactionManager.Savepoint(ctx, txID)
}
func (m failingSavepoints) RollbackToSavepoint(ctx context.Context, txID, sp string) error {
	if m.failUndo != nil {
		return m.failUndo
	}
	return m.TransactionManager.RollbackToSavepoint(ctx, txID, sp)
}
func (m failingSavepoints) ReleaseSavepoint(ctx context.Context, txID, sp string) error {
	if m.failRelease != nil {
		return m.failRelease
	}
	return m.TransactionManager.ReleaseSavepoint(ctx, txID, sp)
}

// TestAsyncNewTx_SavepointFailureFailsTheOperation — a savepoint that cannot be
// created, undone or released means the transaction is unusable. It is never the
// ASYNC_NEW_TX processor's own failure, so it must not take that mode's
// "log and continue" path: carrying on commits the writes of a processor that
// failed, or silently skips one.
func TestAsyncNewTx_SavepointFailureFailsTheOperation(t *testing.T) {
	boom := errors.New(`ERROR: current transaction is aborted (SQLSTATE 25P02) host=db-1`)
	tests := []struct {
		name         string
		mgr          failingSavepoints
		processorErr error
		wantDispatch int
	}{
		{name: "cannot be created", mgr: failingSavepoints{failCreate: boom}, wantDispatch: 0},
		{name: "cannot be undone", mgr: failingSavepoints{failUndo: boom}, processorErr: errors.New("cnode failed"), wantDispatch: 1},
		{name: "cannot be released", mgr: failingSavepoints{failRelease: boom}, wantDispatch: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			factory := memory.NewStoreFactory()
			t.Cleanup(func() { factory.Close() })
			uuids := common.NewTestUUIDGenerator()
			tc.mgr.TransactionManager = factory.NewTransactionManager(uuids)
			dispatched := 0
			ext := &mockExternalProcessing{dispatchFunc: func(_ context.Context, e *spi.Entity, p spi.ProcessorDefinition, _, _, _ string) (*spi.Entity, error) {
				dispatched++
				if p.Name == "first" {
					return e, tc.processorErr
				}
				return e, nil
			}}
			engine := NewEngine(factory, uuids, tc.mgr, WithExternalProcessing(ext))
			base := ctxWithTenant(testTenant)
			modelRef := spi.ModelRef{EntityName: "sp-fatal", ModelVersion: "1.0"}
			saveWorkflow(t, factory, base, modelRef, []spi.WorkflowDefinition{{
				Version: "1.1", Name: "WF", InitialState: "INITIAL", Active: true,
				States: map[string]spi.StateDefinition{
					"INITIAL": {Transitions: []spi.TransitionDefinition{{Name: "T", Next: "DONE", Processors: []spi.ProcessorDefinition{
						{Type: ProcessorTypeExternalized, Name: "first", ExecutionMode: ExecutionModeAsyncNewTx},
						{Type: ProcessorTypeExternalized, Name: "second", ExecutionMode: ExecutionModeAsyncNewTx},
					}}}},
					"DONE": {},
				},
			}})
			txID, txCtx, err := tc.mgr.Begin(base)
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			entity := makeEntity("sp-1", modelRef, map[string]any{"x": 1})
			entity.Meta.TransactionID = txID

			_, err = engine.Execute(txCtx, entity, "")
			if !errors.Is(err, ErrSavepointInfra) {
				t.Fatalf("err = %v; want ErrSavepointInfra — a savepoint failure is not the processor's failure", err)
			}
			if !errors.Is(err, boom) {
				t.Fatalf("the cause must stay in the chain for the log: %v", err)
			}
			if entity.Meta.State == "DONE" {
				t.Fatal("the pipeline carried on past an unusable transaction")
			}
			if dispatched != tc.wantDispatch {
				t.Fatalf("dispatched %d processors; want %d (the second must never run)", dispatched, tc.wantDispatch)
			}
		})
	}
}
