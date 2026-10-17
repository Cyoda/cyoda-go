package workflow

import (
	"context"
	"errors"
	"net/http"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// A caller's transition name and If-Match reach the run's audit events, and
// the audit trail cannot hold U+0000 (PostgreSQL refuses it in jsonb). Every
// other text an event carries is checked where it enters — a workflow at
// import, a compute answer at dispatch, a collection body at the handler — so
// the engine refuses these two before it records anything: 400 BAD_REQUEST and
// no event, on every door, a joined write included.
func TestEngine_CallerTextHoldingNUL_RefusedBeforeAnyEvent(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)
	engine := NewEngine(factory, uuids, txMgr)

	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "caller-nul", ModelVersion: "1.0"}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{{
		Version: "1.1", Name: "CallerNulWF", InitialState: "S_pre", Active: true,
		States: map[string]spi.StateDefinition{
			"S_pre":  {Transitions: []spi.TransitionDefinition{{Name: "go", Next: "S_post", Manual: true}}},
			"S_post": {},
		},
	}})

	seedTxID, seedCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("seed Begin: %v", err)
	}
	es, _ := factory.EntityStore(seedCtx)
	if _, err := es.Save(seedCtx, &spi.Entity{
		Meta: spi.EntityMeta{ID: "caller-nul-1", TenantID: testTenant, ModelRef: modelRef, State: "S_pre", TransactionID: seedTxID},
		Data: []byte(`{"x":1}`),
	}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	if err := txMgr.Commit(seedCtx, seedTxID); err != nil {
		t.Fatalf("seed Commit: %v", err)
	}

	for name, run := range map[string]func(ctx context.Context, entity *spi.Entity) error{
		"transition name": func(ctx context.Context, entity *spi.Entity) error {
			_, err := engine.ManualTransition(ctx, entity, "g\x00o")
			return err
		},
		"If-Match on a transition": func(ctx context.Context, entity *spi.Entity) error {
			_, err := engine.ManualTransitionWithIfMatch(ctx, entity, "go", IfMatch{Expected: "t\x00x", Current: seedTxID})
			return err
		},
		"If-Match on a loopback": func(ctx context.Context, entity *spi.Entity) error {
			_, err := engine.LoopbackWithIfMatch(ctx, entity, IfMatch{Expected: "t\x00x", Current: seedTxID})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			txID, txCtx, err := txMgr.Begin(ctx)
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			defer func() { _ = txMgr.Rollback(txCtx, txID) }()
			entity := &spi.Entity{
				Meta: spi.EntityMeta{ID: "caller-nul-1", TenantID: testTenant, ModelRef: modelRef, State: "S_pre", TransactionID: txID},
				Data: []byte(`{"x":1}`),
			}

			err = run(txCtx, entity)
			var appErr *common.AppError
			if !errors.As(err, &appErr) || appErr.Status != http.StatusBadRequest || appErr.Code != common.ErrCodeBadRequest {
				t.Fatalf("error = %v; want a 400 BAD_REQUEST *common.AppError", err)
			}
			auditStore, err := factory.StateMachineAuditStore(ctx)
			if err != nil {
				t.Fatalf("StateMachineAuditStore: %v", err)
			}
			events, err := auditStore.GetEvents(txCtx, "caller-nul-1")
			if err != nil {
				t.Fatalf("GetEvents: %v", err)
			}
			if len(events) != 0 {
				t.Errorf("events = %v; want none: the text is refused before anything is recorded", events)
			}
		})
	}
}
