package workflow

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/domain/txjoin"
	"github.com/cyoda-platform/cyoda-go/internal/fence"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

type lockProbingCriteria struct {
	free       func() bool
	duringFree bool
}

func (p *lockProbingCriteria) DispatchProcessor(_ context.Context, e *spi.Entity, _ spi.ProcessorDefinition, _, _, _ string) (*spi.Entity, error) {
	return e, nil
}
func (p *lockProbingCriteria) DispatchCriteria(context.Context, *spi.Entity, json.RawMessage, string, string, string, string, string) (bool, string, error) {
	p.duringFree = p.free()
	return true, "", nil
}
func (p *lockProbingCriteria) DispatchFunction(context.Context, *spi.Entity, spi.ScheduleFunction, string, string, string) (contract.FunctionResult, error) {
	return contract.FunctionResult{}, nil
}

func TestJoinedGetTransitions_HoldsTheLock_AndGivesItUpForTheCallout(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)
	gate := txgate.New()
	f := fence.New(gate)
	signer, _ := token.NewSigner([]byte("test-secret-key-at-least-32-bytes!"))
	base := ctxWithTenant(testTenant)
	txID, _, err := txMgr.Begin(base)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	free := func() bool {
		ch := make(chan struct{})
		go func() { gate.Acquire(txID)(); close(ch) }()
		select {
		case <-ch:
			return true
		case <-time.After(50 * time.Millisecond):
			return false
		}
	}
	ext := &lockProbingCriteria{free: free}
	engine := NewEngine(factory, uuids, txMgr, WithExternalProcessing(ext))

	modelRef := spi.ModelRef{EntityName: "joined-transitions", ModelVersion: "1.0"}
	crit, _ := json.Marshal(map[string]any{"type": "function", "function": map[string]any{"name": "c", "config": map[string]any{"calculationNodesTags": "t"}}})
	saveWorkflow(t, factory, base, modelRef, []spi.WorkflowDefinition{{
		Version: "1.1", Name: "WF", InitialState: "INITIAL", Active: true, Criterion: crit,
		States: map[string]spi.StateDefinition{"INITIAL": {Transitions: []spi.TransitionDefinition{{Name: "GO", Next: "DONE", Manual: true}}}, "DONE": {}},
	}})

	_, end := f.Begin(base, "req-1", txID, nil)
	defer end()
	f.Advance("req-1", 1)
	pass, _ := signer.Issue(token.Claims{NodeID: "local", TxRef: txID, ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-1", Major: 1})

	joiner, err := txjoin.NewJoiner(signer, txMgr, f, gate, nil)
	if err != nil {
		t.Fatalf("NewJoiner: %v", err)
	}

	heldBefore, heldAfter := false, false
	err = joiner.Run(base, pass, func(ctx context.Context) {
		heldBefore = !free()
		entity := makeEntity("jt-1", modelRef, map[string]any{"x": 1})
		entity.Meta.State = "INITIAL"
		if _, err := engine.GetAvailableTransitionsForEntity(ctx, entity); err != nil {
			t.Errorf("GetAvailableTransitionsForEntity: %v", err)
		}
		heldAfter = !free()
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !heldBefore || !heldAfter {
		t.Fatalf("the joined read must hold the lock before (%v) and after (%v) the callout", heldBefore, heldAfter)
	}
	if !ext.duringFree {
		t.Fatal("the lock was kept across the function criterion's callout: a callback of that callout would deadlock on it")
	}
}
