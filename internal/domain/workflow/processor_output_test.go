package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// procOutputEngine builds an engine whose single processor returns procData,
// running in the given execution mode, over a model declaring modelFields.
func procOutputEngine(
	t *testing.T,
	mode string,
	modelFields map[string]schema.DataType,
	procData string,
	procErr error,
) (*Engine, spi.StoreFactory, spi.TransactionManager, context.Context, spi.ModelRef) {
	t.Helper()

	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)

	mock := &mockExternalProcessing{
		dispatchFunc: func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition, _, _, txID string) (*spi.Entity, error) {
			if procErr != nil {
				return nil, procErr
			}
			return &spi.Entity{Data: []byte(procData)}, nil
		},
	}
	engine := NewEngine(factory, uuids, txMgr, WithExternalProcessing(mock))

	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "procout-" + mode, ModelVersion: "1.0"}
	if modelFields != nil {
		registerModelFields(t, ctx, factory, modelRef, modelFields)
	}

	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "ProcOutWF", InitialState: "S_pre", Active: true,
		States: map[string]spi.StateDefinition{
			"S_pre": {Transitions: []spi.TransitionDefinition{
				{Name: "CALLOUT", Next: "S_post", Manual: false,
					Processors: []spi.ProcessorDefinition{
						{Type: ProcessorTypeExternalized, Name: "procout-proc", ExecutionMode: mode},
					}},
			}},
			"S_post": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})
	return engine, factory, txMgr, ctx, modelRef
}

func runProcOutput(t *testing.T, engine *Engine, txMgr spi.TransactionManager, ctx context.Context, modelRef spi.ModelRef) (*spi.Entity, error) {
	t.Helper()
	txID, txCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	entity := &spi.Entity{
		Meta: spi.EntityMeta{
			ID: "procout-1", TenantID: testTenant, ModelRef: modelRef, TransactionID: txID,
		},
		Data: []byte(`{"x":1}`),
	}
	_, err = engine.Execute(txCtx, entity, "")
	return entity, err
}

// TestProcessorOutput_OffModelFieldRejectedInEveryExecutionMode covers the
// rejection path in each mode a processor's data can reach the entity through —
// including both COMMIT_BEFORE_DISPATCH branches, where the check runs on
// TX_post rather than where the data arrives.
func TestProcessorOutput_OffModelFieldRejectedInEveryExecutionMode(t *testing.T) {
	modes := []string{
		ExecutionModeSync,
		ExecutionModeAsyncSameTx,
		ExecutionModeCommitBeforeDispatch,
	}
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			engine, _, txMgr, ctx, ref := procOutputEngine(t, mode,
				map[string]schema.DataType{"x": schema.Integer},
				`{"x":1,"undeclared":"v"}`, nil)

			entity, err := runProcOutput(t, engine, txMgr, ctx, ref)
			if err == nil {
				t.Fatalf("processor wrote a field the model does not declare; want an error")
			}
			if !errors.Is(err, ErrProcessorOutputRejected) {
				t.Errorf("err = %v; want ErrProcessorOutputRejected", err)
			}
			if string(entity.Data) != `{"x":1}` {
				t.Errorf("entity.Data = %s; the rejected data must not be adopted", entity.Data)
			}
		})
	}
}

// TestProcessorOutput_RejectionDoesNotLeakAnAppError is the regression guard for
// the %s-not-%w choice in applyProcessorData.
//
// ingest returns *common.AppError values carrying 400 BAD_REQUEST. The handler's
// classifier unwraps any embedded AppError first, so wrapping the cause with %w
// would surface the checker's verdict verbatim — blaming the caller for bytes a
// processor produced. The chain must stay broken.
func TestProcessorOutput_RejectionDoesNotLeakAnAppError(t *testing.T) {
	engine, _, txMgr, ctx, ref := procOutputEngine(t, ExecutionModeSync,
		map[string]schema.DataType{"x": schema.Integer},
		`{"x":1,"undeclared":"v"}`, nil)

	_, err := runProcOutput(t, engine, txMgr, ctx, ref)
	if err == nil {
		t.Fatal("expected an error")
	}
	var appErr *common.AppError
	if errors.As(err, &appErr) {
		t.Errorf("an *common.AppError is reachable through the chain (status %d, code %s) — "+
			"the handler would surface it and blame the caller for the processor's bytes",
			appErr.Status, appErr.Code)
	}
}

// TestProcessorOutput_UnstorableContentRejected covers the storability half.
func TestProcessorOutput_UnstorableContentRejected(t *testing.T) {
	engine, _, txMgr, ctx, ref := procOutputEngine(t, ExecutionModeSync,
		map[string]schema.DataType{"x": schema.Integer, "s": schema.String},
		"{\"x\":1,\"s\":\"a\\ud800b\"}", nil)

	_, err := runProcOutput(t, engine, txMgr, ctx, ref)
	if err == nil {
		t.Fatal("processor returned an unpaired surrogate; want an error, not a storage failure")
	}
	if !errors.Is(err, ErrProcessorOutputRejected) {
		t.Errorf("err = %v; want ErrProcessorOutputRejected", err)
	}
}

// TestProcessorOutput_StorableOnModelDataAccepted keeps the guard from being
// vacuously strict: data that matches the model must still be adopted.
func TestProcessorOutput_StorableOnModelDataAccepted(t *testing.T) {
	engine, _, txMgr, ctx, ref := procOutputEngine(t, ExecutionModeSync,
		map[string]schema.DataType{"x": schema.Integer, "enriched": schema.Boolean},
		`{"x":1,"enriched":true}`, nil)

	entity, err := runProcOutput(t, engine, txMgr, ctx, ref)
	if err != nil {
		t.Fatalf("on-model processor output was rejected: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(entity.Data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["enriched"] != true {
		t.Errorf("processor output was not adopted: %s", entity.Data)
	}
}

// TestProcessorOutput_FailsClosedWhenTheModelCannotBeRead pins the infra route.
//
// A missing or unreadable model means the input needed to judge the output is
// absent, so the transition is rejected rather than guessed at — and it routes
// to ErrProcessorOutputInfra, which the handler maps to a sanitized 5xx rather
// than blaming the caller.
func TestProcessorOutput_FailsClosedWhenTheModelCannotBeRead(t *testing.T) {
	t.Run("model store unavailable", func(t *testing.T) {
		engine, factory, txMgr, ctx, ref := procOutputEngine(t, ExecutionModeSync,
			map[string]schema.DataType{"x": schema.Integer}, `{"x":1}`, nil)
		engine.factory = &errModelStoreFactory{StoreFactory: factory, err: fmt.Errorf("store down")}

		if _, err := runProcOutput(t, engine, txMgr, ctx, ref); !errors.Is(err, ErrProcessorOutputInfra) {
			t.Errorf("err = %v; want ErrProcessorOutputInfra", err)
		}
	})

	t.Run("model not registered", func(t *testing.T) {
		// nil modelFields — no descriptor is saved for this model.
		engine, _, txMgr, ctx, ref := procOutputEngine(t, ExecutionModeSync, nil, `{"x":1}`, nil)
		if _, err := runProcOutput(t, engine, txMgr, ctx, ref); !errors.Is(err, ErrProcessorOutputInfra) {
			t.Errorf("err = %v; want ErrProcessorOutputInfra", err)
		}
	})
}

// TestProcessorOutput_DescriptorResolvedOnceAcrossThePipeline pins the reason
// modelDescMemo exists.
//
// The model cache is not transaction-aware and ExtendSchema invalidates it
// before the surrounding transaction commits. Re-reading the descriptor between
// processors could therefore repopulate it from the pre-extension schema and
// reject the next processor's legitimate field. One read across the pipeline
// cannot see that flap.
func TestProcessorOutput_DescriptorResolvedOnceAcrossThePipeline(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)

	calls := 0
	mock := &mockExternalProcessing{
		dispatchFunc: func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition, _, _, txID string) (*spi.Entity, error) {
			calls++
			return &spi.Entity{Data: []byte(fmt.Sprintf(`{"x":%d}`, calls))}, nil
		},
	}
	engine := NewEngine(factory, uuids, txMgr, WithExternalProcessing(mock))

	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "procout-memo", ModelVersion: "1.0"}
	registerModelFields(t, ctx, factory, modelRef, map[string]schema.DataType{"x": schema.Integer})

	counting := &countingModelStoreFactory{StoreFactory: factory}
	engine.factory = counting

	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "MemoWF", InitialState: "S_pre", Active: true,
		States: map[string]spi.StateDefinition{
			"S_pre": {Transitions: []spi.TransitionDefinition{
				{Name: "CALLOUT", Next: "S_post", Manual: false,
					Processors: []spi.ProcessorDefinition{
						{Type: ProcessorTypeExternalized, Name: "p1", ExecutionMode: ExecutionModeSync},
						{Type: ProcessorTypeExternalized, Name: "p2", ExecutionMode: ExecutionModeSync},
						{Type: ProcessorTypeExternalized, Name: "p3", ExecutionMode: ExecutionModeSync},
					}},
			}},
			"S_post": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	counting.gets = 0
	if _, err := runProcOutput(t, engine, txMgr, ctx, modelRef); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected 3 processor dispatches, got %d", calls)
	}
	if counting.gets != 1 {
		t.Errorf("model descriptor was read %d times for one transition; want 1 — "+
			"re-reading between processors can observe a cache invalidated by an "+
			"uncommitted extension", counting.gets)
	}
}

// countingModelStoreFactory counts ModelStore.Get calls.
type countingModelStoreFactory struct {
	spi.StoreFactory
	gets int
}

func (f *countingModelStoreFactory) ModelStore(ctx context.Context) (spi.ModelStore, error) {
	inner, err := f.StoreFactory.ModelStore(ctx)
	if err != nil {
		return nil, err
	}
	return &countingModelStore{ModelStore: inner, f: f}, nil
}

type countingModelStore struct {
	spi.ModelStore
	f *countingModelStoreFactory
}

func (s *countingModelStore) Get(ctx context.Context, ref spi.ModelRef) (*spi.ModelDescriptor, error) {
	s.f.gets++
	return s.ModelStore.Get(ctx, ref)
}
