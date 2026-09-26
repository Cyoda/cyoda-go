package grpc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/entity"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model"
	"github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
	"github.com/cyoda-platform/cyoda-go/internal/testing/localproc"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// txAbortedCASFactory, once armed, makes every CompareAndSave fail as a
// statement in a transaction an earlier conflict already aborted.
type txAbortedCASFactory struct {
	spi.StoreFactory
	armed bool
}

func (f *txAbortedCASFactory) EntityStore(ctx context.Context) (spi.EntityStore, error) {
	es, err := f.StoreFactory.EntityStore(ctx)
	if err != nil || !f.armed {
		return es, err
	}
	return &txAbortedCASStore{EntityStore: es}, nil
}

type txAbortedCASStore struct{ spi.EntityStore }

func (s *txAbortedCASStore) CompareAndSave(context.Context, *spi.Entity, string) (int64, error) {
	return 0, fmt.Errorf("compare-and-save: %w", spi.ErrTxAborted)
}

// TestRPC_EntityPatch_IfMatchInAbortedTxIsConflict: the gRPC If-Match door
// answers a compare that met an aborted transaction as the retryable CONFLICT,
// not ENTITY_MODIFIED — the precondition was never evaluated.
func TestRPC_EntityPatch_IfMatchInAbortedTxIsConflict(t *testing.T) {
	svc, ctx := newTestEnv(t)
	// The entity door rewired onto a store behind the arming wrapper; the
	// model door keeps the harness's own store, so both see one model.
	inner := memory.NewStoreFactory(memory.WithApplyFunc(testSchemaApply))
	inner.NewTransactionManager(common.NewDefaultUUIDGenerator())
	txMgr := inner.GetTransactionManager()
	factory := &txAbortedCASFactory{StoreFactory: inner}
	engine := workflow.NewEngine(factory, common.NewDefaultUUIDGenerator(), txMgr)
	svc.txMgr = txMgr
	svc.entityHandler = entity.New(factory, txMgr, common.NewDefaultUUIDGenerator(), engine, txgate.New())
	svc.modelHandler = model.New(inner)
	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"name": "A", "amount": 1})

	createResp, err := svc.EntityManage(ctx, makeCE(EntityCreateRequest, map[string]any{
		"id": "c1", "dataFormat": "JSON",
		"payload": map[string]any{"model": map[string]any{"name": "person", "version": 1}, "data": map[string]any{"name": "A", "amount": 1}},
	}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	txInfo := parseResponsePayload(t, createResp)["transactionInfo"].(map[string]any)
	entityID := txInfo["entityIds"].([]any)[0].(string)
	createTxID := txInfo["transactionId"].(string)

	factory.armed = true

	resp, err := svc.EntityManage(ctx, makeCE(EntityPatchRequest, map[string]any{
		"id": "p1", "patchFormat": "MERGE_PATCH",
		"payload": map[string]any{"entityId": entityID, "patch": map[string]any{"amount": 2}, "ifMatch": createTxID},
	}))
	if err != nil {
		t.Fatalf("patch transport error: %v", err)
	}
	var typed events.EntityTransactionResponseJson
	validateResponse(t, resp, &typed)
	if typed.Success || typed.Error == nil {
		t.Fatal("expected a failure envelope")
	}
	if typed.Error.Code != "CLIENT_ERROR" {
		t.Errorf("Error.Code = %q; want CLIENT_ERROR", typed.Error.Code)
	}
	if !strings.HasPrefix(typed.Error.Message, common.ErrCodeConflict+":") {
		t.Errorf("message = %q; want the CONFLICT domain code as its prefix", typed.Error.Message)
	}
	if typed.Error.Retryable == nil || !*typed.Error.Retryable {
		t.Error("a transaction conflict is retryable")
	}
}

// txAbortedGetFactory, once armed, makes every Get in a transaction fail as a
// statement in a transaction an earlier conflict already aborted — the state a
// joined callback that lost its write race leaves behind on a backend that
// aborts the whole transaction.
type txAbortedGetFactory struct {
	spi.StoreFactory
	armed atomic.Bool
}

func (f *txAbortedGetFactory) EntityStore(ctx context.Context) (spi.EntityStore, error) {
	es, err := f.StoreFactory.EntityStore(ctx)
	if err != nil {
		return es, err
	}
	return &txAbortedGetStore{EntityStore: es, armed: &f.armed}, nil
}

type txAbortedGetStore struct {
	spi.EntityStore
	armed *atomic.Bool
}

func (s *txAbortedGetStore) Get(ctx context.Context, id string) (*spi.Entity, error) {
	if s.armed.Load() && spi.GetTransaction(ctx) != nil {
		return nil, fmt.Errorf("get: %w", spi.ErrTxAborted)
	}
	return s.EntityStore.Get(ctx, id)
}

// TestRPC_EntityCreate_ProcessorFailedAfterLostRaceIsConflict: a processor
// whose joined write lost a race fails, and the transaction it ran in is
// aborted. The gRPC door answers the retryable CONFLICT the engine's probe of
// the transaction finds, not the processor's failure. The store turns aborted
// only inside the processor, so nothing before the dispatch sees it.
func TestRPC_EntityCreate_ProcessorFailedAfterLostRaceIsConflict(t *testing.T) {
	svc, ctx := newTestEnv(t)
	inner := memory.NewStoreFactory(memory.WithApplyFunc(testSchemaApply))
	inner.NewTransactionManager(common.NewDefaultUUIDGenerator())
	txMgr := inner.GetTransactionManager()
	factory := &txAbortedGetFactory{StoreFactory: inner}
	lp := localproc.New()
	lp.RegisterProcessor("loses-race", func(context.Context, *spi.Entity, spi.ProcessorDefinition) (*spi.Entity, error) {
		factory.armed.Store(true)
		return nil, errors.New("the joined write was refused")
	})
	engine := workflow.NewEngine(factory, common.NewDefaultUUIDGenerator(), txMgr, workflow.WithExternalProcessing(lp))
	svc.txMgr = txMgr
	svc.entityHandler = entity.New(factory, txMgr, common.NewDefaultUUIDGenerator(), engine, txgate.New())
	svc.modelHandler = model.New(inner)
	importAndLockModel(t, svc, ctx, "racer", "1", map[string]any{"name": "A", "amount": 1})

	ws, err := inner.WorkflowStore(ctx)
	if err != nil {
		t.Fatalf("WorkflowStore: %v", err)
	}
	if err := ws.Save(ctx, spi.ModelRef{EntityName: "racer", ModelVersion: "1"}, []spi.WorkflowDefinition{{
		Version: "1.1", Name: "race-wf", InitialState: "NONE", Active: true,
		States: map[string]spi.StateDefinition{
			"NONE": {Transitions: []spi.TransitionDefinition{{
				Name: "init", Next: "DONE",
				Processors: []spi.ProcessorDefinition{{Type: "calculator", Name: "loses-race", ExecutionMode: "SYNC"}},
			}}},
			"DONE": {},
		},
	}}); err != nil {
		t.Fatalf("WorkflowStore.Save: %v", err)
	}

	resp, err := svc.EntityManage(ctx, makeCE(EntityCreateRequest, map[string]any{
		"id": "c1", "dataFormat": "JSON",
		"payload": map[string]any{"model": map[string]any{"name": "racer", "version": 1}, "data": map[string]any{"name": "A", "amount": 1}},
	}))
	if err != nil {
		t.Fatalf("create transport error: %v", err)
	}
	if n := lp.ProcessorCallCount("loses-race"); n != 1 {
		t.Fatalf("processor dispatched %d times, want 1", n)
	}
	var typed events.EntityTransactionResponseJson
	validateResponse(t, resp, &typed)
	if typed.Success || typed.Error == nil {
		t.Fatal("expected a failure envelope")
	}
	if typed.Error.Code != "CLIENT_ERROR" {
		t.Errorf("Error.Code = %q; want CLIENT_ERROR", typed.Error.Code)
	}
	if !strings.HasPrefix(typed.Error.Message, common.ErrCodeConflict+":") {
		t.Errorf("message = %q; want the CONFLICT domain code as its prefix", typed.Error.Message)
	}
	if typed.Error.Retryable == nil || !*typed.Error.Retryable {
		t.Error("a transaction conflict is retryable")
	}
}
