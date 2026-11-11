package workflow

import (
	"context"
	"encoding/json"
	"github.com/cyoda-platform/cyoda-go/internal/platformclock"
	"strconv"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/domain/txjoin"
	"github.com/cyoda-platform/cyoda-go/internal/txcoord"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

type lockProbingCriteria struct {
	free       func(ctx context.Context) bool
	duringFree bool
}

func (p *lockProbingCriteria) DispatchProcessor(_ context.Context, _ *spi.Entity, _ spi.ProcessorDefinition, _, _, _ string) (*spi.Entity, error) {
	return nil, nil
}
func (p *lockProbingCriteria) DispatchCriteria(ctx context.Context, _ *spi.Entity, _ json.RawMessage, _, _, _, _, _ string) (bool, string, error) {
	p.duringFree = p.free(ctx)
	return true, "", nil
}
func (p *lockProbingCriteria) DispatchFunction(context.Context, *spi.Entity, spi.ScheduleFunction, string, string, string) (contract.FunctionResult, error) {
	return contract.FunctionResult{}, nil
}

func TestJoinedGetTransitions_HoldsTheGate_AndGivesItUpForTheCallout(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)
	coord := txcoord.New(txMgr)
	signer, _ := token.NewSigner([]byte("test-secret-key-at-least-32-bytes!"))
	base := ctxWithTenant(testTenant)
	txID, _, err := txMgr.Begin(base)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	// free reports whether a request may take the transaction's gate now: a
	// callback of a callout begun from ctx — of the chain's own callout while
	// it is out, so admitted under the stack rule — takes it within 50ms.
	probes := 0
	free := func(ctx context.Context) bool {
		probes++
		id := "probe-" + strconv.Itoa(probes)
		_, end := coord.BeginCallout(ctx, txID, id)
		defer end()
		coord.Advance(id, 1)
		pairs := append([]txcoord.Pair{{Callout: id, Major: 1}}, txcoord.Pairs(ctx)...)
		wctx, cancel := context.WithTimeout(base, 50*time.Millisecond)
		defer cancel()
		s, _, err := coord.Enter(wctx, txID, pairs, txcoord.Outside, 0)
		if err != nil {
			return false
		}
		s.End(nil)
		return true
	}
	ext := &lockProbingCriteria{free: free}
	engine := NewEngine(factory, uuids, txMgr, platformclock.Real(), WithExternalProcessing(ext), WithCoordination(coord))

	modelRef := spi.ModelRef{EntityName: "joined-transitions", ModelVersion: "1.0"}
	crit, _ := json.Marshal(map[string]any{"type": "function", "function": map[string]any{"name": "c", "config": map[string]any{"calculationNodesTags": "t"}}})
	saveWorkflow(t, factory, base, modelRef, []spi.WorkflowDefinition{{
		Version: "1.1", Name: "WF", InitialState: "INITIAL", Active: true, Criterion: crit,
		States: map[string]spi.StateDefinition{"INITIAL": {Transitions: []spi.TransitionDefinition{{Name: "GO", Next: "DONE", Manual: true}}}, "DONE": {}},
	}})

	_, end := coord.BeginCallout(base, txID, "req-1")
	defer end()
	coord.Advance("req-1", 1)
	pass, _ := signer.Issue(token.Claims{NodeID: "local", TxRef: txID, ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-1", Major: 1})

	joiner, err := txjoin.NewJoiner(signer, txMgr, coord, 10<<20, 128, nil)
	if err != nil {
		t.Fatalf("NewJoiner: %v", err)
	}

	heldBefore, heldAfter := false, false
	_, err = joiner.Run(base, pass, txcoord.Read, func(ctx context.Context) *common.Outcome {
		heldBefore = !free(ctx)
		entity := makeEntity("jt-1", modelRef, map[string]any{"x": 1})
		entity.Meta.State = "INITIAL"
		if _, err := engine.GetAvailableTransitionsForEntity(ctx, entity); err != nil {
			t.Errorf("GetAvailableTransitionsForEntity: %v", err)
		}
		heldAfter = !free(ctx)
		return nil
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !heldBefore || !heldAfter {
		t.Fatalf("the joined read must hold the gate before (%v) and after (%v) the callout", heldBefore, heldAfter)
	}
	if !ext.duringFree {
		t.Fatal("the gate was kept across the function criterion's callout: a callback of that callout would deadlock on it")
	}
}
