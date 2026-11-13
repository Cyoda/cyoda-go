package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/modelcache"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/internal/platformclock"
	"github.com/cyoda-platform/cyoda-go/internal/txcoord"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// extendingMemoryFactory is a memory factory whose ExtendSchema really applies
// deltas (the production ApplyFunc).
func extendingMemoryFactory(t *testing.T) *memory.StoreFactory {
	t.Helper()
	f := memory.NewStoreFactory(memory.WithApplyFunc(func(base []byte, delta spi.SchemaDelta) ([]byte, error) {
		node, err := schema.Unmarshal(base)
		if err != nil {
			return nil, err
		}
		ext, err := schema.Apply(node, delta)
		if err != nil {
			return nil, err
		}
		return schema.Marshal(ext)
	}))
	t.Cleanup(func() { f.Close() })
	return f
}

// registerExtensibleModel saves a STRUCTURAL-change-level model declaring x.
func registerExtensibleModel(t *testing.T, ctx context.Context, factory spi.StoreFactory, ref spi.ModelRef) {
	t.Helper()
	node := schema.NewObjectNode()
	node.SetChild("x", schema.NewLeafNode(schema.Integer))
	raw, err := schema.Marshal(node)
	if err != nil {
		t.Fatal(err)
	}
	ms, err := factory.ModelStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := ms.Save(ctx, &spi.ModelDescriptor{Ref: ref, Schema: raw, ChangeLevel: spi.ChangeLevelStructural}); err != nil {
		t.Fatal(err)
	}
}

// extendDoubleFactory counts ExtendSchema calls and can fail them.
type extendDoubleFactory struct {
	spi.StoreFactory
	extends   int
	extendErr error
}

func (f *extendDoubleFactory) ModelStore(ctx context.Context) (spi.ModelStore, error) {
	inner, err := f.StoreFactory.ModelStore(ctx)
	if err != nil {
		return nil, err
	}
	return &extendDoubleStore{ModelStore: inner, f: f}, nil
}

type extendDoubleStore struct {
	spi.ModelStore
	f *extendDoubleFactory
}

func (s *extendDoubleStore) ExtendSchema(ctx context.Context, ref spi.ModelRef, d spi.SchemaDelta) error {
	s.f.extends++
	if s.f.extendErr != nil {
		return s.f.extendErr
	}
	return s.ModelStore.ExtendSchema(ctx, ref, d)
}

type unavailableErr struct{}

func (unavailableErr) Error() string            { return "store unavailable" }
func (unavailableErr) StorageUnavailable() bool { return true }

// extendEngine builds an engine over an extensible model whose pipeline has one
// externalized sync processor per entry of procData.
func extendEngine(t *testing.T, procData []string) (*Engine, *extendDoubleFactory, spi.TransactionManager, context.Context, spi.ModelRef) {
	t.Helper()
	inner := extendingMemoryFactory(t)
	double := &extendDoubleFactory{StoreFactory: inner}
	uuids := common.NewTestUUIDGenerator()
	txMgr := inner.NewTransactionManager(uuids)

	call := 0
	mock := &mockExternalProcessing{
		dispatchFunc: func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition, _, _, txID string) (*spi.Entity, error) {
			d := procData[call]
			call++
			return &spi.Entity{Data: []byte(d)}, nil
		},
	}
	engine := NewEngine(double, uuids, txMgr, platformclock.Real(), WithExternalProcessing(mock), WithCoordination(txcoord.New(txMgr)))

	ctx := ctxWithTenant(testTenant)
	ref := spi.ModelRef{EntityName: "procout-extend", ModelVersion: "1.0"}
	registerExtensibleModel(t, ctx, inner, ref)

	var procs []spi.ProcessorDefinition
	for i := range procData {
		procs = append(procs, spi.ProcessorDefinition{Type: ProcessorTypeExternalized, Name: string(rune('a' + i)), ExecutionMode: ExecutionModeSync})
	}
	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "ExtWF", InitialState: "S_pre", Active: true,
		States: map[string]spi.StateDefinition{
			"S_pre":  {Transitions: []spi.TransitionDefinition{{Name: "CALLOUT", Next: "S_post", Processors: procs}}},
			"S_post": {},
		},
	}
	saveWorkflow(t, inner, ctx, ref, []spi.WorkflowDefinition{wf})
	return engine, double, txMgr, ctx, ref
}

func TestProcessorOutput_SecondProcessorSameField_NoSecondExtend(t *testing.T) {
	engine, double, txMgr, ctx, ref := extendEngine(t, []string{`{"x":1,"p":"a"}`, `{"x":1,"p":"b"}`})
	if _, err := runProcOutput(t, engine, txMgr, ctx, ref); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if double.extends != 1 {
		t.Errorf("ExtendSchema called %d times; want 1 — the second processor's field is already in the descriptor the first extension returned", double.extends)
	}
}

func TestProcessorOutput_StoreUnavailable_Is503(t *testing.T) {
	engine, double, txMgr, ctx, ref := extendEngine(t, []string{`{"x":1,"p":"a"}`})
	double.extendErr = unavailableErr{}
	_, err := runProcOutput(t, engine, txMgr, ctx, ref)
	if err == nil {
		t.Fatal("want an error")
	}
	if !errors.Is(err, ErrProcessorOutputInfra) {
		t.Errorf("err = %v; want ErrProcessorOutputInfra", err)
	}
	appErr := common.StorageUnavailable(err)
	if appErr == nil || appErr.Status != http.StatusServiceUnavailable {
		t.Errorf("storage-unavailable cause lost: StorageUnavailable(err) = %v", appErr)
	}
}

func TestProcessorOutput_ModelNotLocked_Is409(t *testing.T) {
	engine, double, txMgr, ctx, ref := extendEngine(t, []string{`{"x":1,"p":"a"}`})
	double.extendErr = spi.ErrModelNotLocked
	_, err := runProcOutput(t, engine, txMgr, ctx, ref)
	if err == nil {
		t.Fatal("want an error")
	}
	appErr := common.Internal("processor output check failed", err)
	if appErr.Status != http.StatusConflict || appErr.Code != common.ErrCodeModelNotLocked {
		t.Errorf("classified %d %s; want 409 MODEL_NOT_LOCKED", appErr.Status, appErr.Code)
	}
}

// TestEngine_CriterionOnFieldAddedBySameRequest: a processor adds field p; the
// next automated transition's criterion on p evaluates against the extended
// schema (through the model cache) and matches. Regression guard.
func TestEngine_CriterionOnFieldAddedBySameRequest(t *testing.T) {
	inner := extendingMemoryFactory(t)
	cached := modelcache.NewCachingStoreFactory(inner, nil, nil, 3600*1e9)
	uuids := common.NewTestUUIDGenerator()
	txMgr := inner.NewTransactionManager(uuids)
	mock := &mockExternalProcessing{
		dispatchFunc: func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition, _, _, txID string) (*spi.Entity, error) {
			return &spi.Entity{Data: []byte(`{"x":1,"p":"v"}`)}, nil
		},
	}
	engine := NewEngine(cached, uuids, txMgr, platformclock.Real(), WithExternalProcessing(mock), WithCoordination(txcoord.New(txMgr)))
	ctx := ctxWithTenant(testTenant)
	ref := spi.ModelRef{EntityName: "crit-extend", ModelVersion: "1.0"}
	registerExtensibleModel(t, ctx, cached, ref)

	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "CritWF", InitialState: "S_pre", Active: true,
		States: map[string]spi.StateDefinition{
			"S_pre": {Transitions: []spi.TransitionDefinition{{Name: "ENRICH", Next: "S_mid",
				Processors: []spi.ProcessorDefinition{{Type: ProcessorTypeExternalized, Name: "enrich", ExecutionMode: ExecutionModeSync}}}}},
			"S_mid": {Transitions: []spi.TransitionDefinition{{Name: "ON_P", Next: "S_done",
				Criterion: simpleCriterion("$.p", "EQUALS", "v")}}},
			"S_done": {},
		},
	}
	saveWorkflow(t, inner, ctx, ref, []spi.WorkflowDefinition{wf})

	entity, err := runProcOutput(t, engine, txMgr, ctx, ref)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if entity.Meta.State != "S_done" {
		var d map[string]any
		_ = json.Unmarshal(entity.Data, &d)
		t.Errorf("state = %q; want S_done (data %v)", entity.Meta.State, d)
	}
}
