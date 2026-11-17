package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/internal/platformclock"
	"github.com/cyoda-platform/cyoda-go/internal/txcoord"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

func syncProc(name string) spi.ProcessorDefinition {
	return spi.ProcessorDefinition{Type: ProcessorTypeExternalized, Name: name, ExecutionMode: ExecutionModeSync}
}

// readUnderToken reads the anchor as a callback that joined the transaction
// on ctx reads it.
func readUnderToken(ctx context.Context, factory spi.StoreFactory, id string) (*spi.Entity, error) {
	es, err := factory.EntityStore(ctx)
	if err != nil {
		return nil, err
	}
	return es.Get(ctx, id)
}

// saveUnderToken saves entity as a callback that joined the transaction on
// ctx saves it.
func saveUnderToken(ctx context.Context, factory spi.StoreFactory, entity *spi.Entity) error {
	es, err := factory.EntityStore(ctx)
	if err != nil {
		return err
	}
	_, err = es.Save(ctx, entity)
	return err
}

// manualWithData seeds entity id committed in S_pre with {"x":0}, then runs
// the manual transition CALLOUT on it in a new transaction with data as the
// request's payload, as the entity service runs an update with a transition.
// It returns the seed's (committed) transaction id, the run's transaction id,
// the engine's entity, result and error.
func manualWithData(t *testing.T, env *cbdCallbackEnv, id string, data []byte) (string, string, *spi.Entity, *EngineResult, error) {
	t.Helper()
	seeded := seedFireEntity(t, env.factory, env.ctx, id, env.model, "S_pre", true, map[string]any{"x": 0})
	txID, txCtx, err := env.txMgr.Begin(env.ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	t.Cleanup(func() { _ = env.txMgr.Rollback(env.ctx, txID) })
	es, err := env.factory.EntityStore(txCtx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	entity, err := es.Get(txCtx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if data != nil {
		entity.Data = data
	}
	entity.Meta.TransactionID = txID
	result, err := env.engine.ManualTransition(txCtx, entity, "CALLOUT", IfMatch{})
	return seeded.Meta.TransactionID, txID, entity, result, err
}

// newCreateEnv is newCallbackEnv for a create: the workflow starts in S_new
// and its automated transition AUTO runs procs on the way to S_done.
func newCreateEnv(t *testing.T, procs []spi.ProcessorDefinition, dispatch func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition, txID string) (*spi.Entity, error)) *cbdCallbackEnv {
	t.Helper()
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)
	mock := &mockExternalProcessing{dispatchFunc: func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition, _, _, txID string) (*spi.Entity, error) {
		return dispatch(ctx, entity, proc, txID)
	}}
	engine := NewEngine(cachedFactory(factory), uuids, txMgr, platformclock.Real(), WithExternalProcessing(mock), WithCoordination(txcoord.New(txMgr)))
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "presave-create", ModelVersion: "1.0"}
	registerModelFields(t, ctx, factory, modelRef, map[string]schema.DataType{
		"x": schema.Integer, "by": schema.String,
	})
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{{
		Version: "1.1", Name: "PresaveCreateWF", InitialState: "S_new", Active: true,
		States: map[string]spi.StateDefinition{
			"S_new":  {Transitions: []spi.TransitionDefinition{{Name: "AUTO", Next: "S_done", Processors: procs}}},
			"S_done": {},
		},
	}})
	return &cbdCallbackEnv{factory: factory, txMgr: txMgr, engine: engine, ctx: ctx, model: modelRef}
}

// create runs the engine's create on a new entity id with {"x":3,"by":"request"}
// in a new transaction, as the entity service does. It returns the
// transaction's id, the engine's entity, result and error.
func (env *cbdCallbackEnv) create(t *testing.T, id string) (string, *spi.Entity, *EngineResult, error) {
	t.Helper()
	txID, txCtx, err := env.txMgr.Begin(env.ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	t.Cleanup(func() { _ = env.txMgr.Rollback(env.ctx, txID) })
	entity := makeEntity(id, env.model, map[string]any{"x": 3, "by": "request"})
	entity.Meta.TransactionID = txID
	result, err := env.engine.Execute(txCtx, entity)
	return txID, entity, result, err
}

// A callback's read on an update returns the data the request sent, in the
// transition's source state, stamped with the transaction; the payload's
// transactionId is the one that read returns.
func TestPreCalloutSave_Update_CallbackSeesRequestData(t *testing.T) {
	var env *cbdCallbackEnv
	var seen *spi.Entity
	var payloadTxID string
	env = newCallbackEnv(t, []spi.ProcessorDefinition{syncProc("p1")},
		func(ctx context.Context, entity *spi.Entity, _ spi.ProcessorDefinition, _ string) (*spi.Entity, error) {
			payloadTxID = entity.Meta.TransactionID
			var err error
			seen, err = readUnderToken(ctx, env.factory, entity.Meta.ID)
			return nil, err
		})
	_, txID, _, _, err := manualWithData(t, env, "pcu-1", []byte(`{"x":5,"by":"request"}`))
	if err != nil {
		t.Fatalf("ManualTransition: %v", err)
	}
	if seen == nil {
		t.Fatal("the callback read nothing")
	}
	assertData(t, seen.Data, map[string]any{"x": float64(5), "by": "request"})
	if seen.Meta.State != "S_pre" {
		t.Errorf("state = %q, want S_pre", seen.Meta.State)
	}
	if seen.Meta.TransactionID != txID {
		t.Errorf("read transactionId = %q, want the transaction's %q", seen.Meta.TransactionID, txID)
	}
	if payloadTxID != seen.Meta.TransactionID {
		t.Errorf("payload transactionId = %q, want the read's %q", payloadTxID, seen.Meta.TransactionID)
	}
}

// On a create the callback finds the anchor, with the request's data and the
// state the cascade has reached.
func TestPreCalloutSave_Create_CallbackFindsAnchor(t *testing.T) {
	var env *cbdCallbackEnv
	var seen *spi.Entity
	var payloadTxID string
	env = newCreateEnv(t, []spi.ProcessorDefinition{syncProc("p1")},
		func(ctx context.Context, entity *spi.Entity, _ spi.ProcessorDefinition, _ string) (*spi.Entity, error) {
			payloadTxID = entity.Meta.TransactionID
			var err error
			seen, err = readUnderToken(ctx, env.factory, entity.Meta.ID)
			return nil, err
		})
	txID, _, _, err := env.create(t, "pcc-1")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if seen == nil {
		t.Fatal("the callback did not find the anchor")
	}
	assertData(t, seen.Data, map[string]any{"x": float64(3), "by": "request"})
	if seen.Meta.State != "S_new" {
		t.Errorf("state = %q, want S_new", seen.Meta.State)
	}
	if seen.Meta.TransactionID != txID || payloadTxID != txID {
		t.Errorf("read transactionId = %q, payload transactionId = %q, want both %q", seen.Meta.TransactionID, payloadTxID, txID)
	}
}

// A later processor's callback sees the data an earlier processor returned.
func TestPreCalloutSave_SecondProcessorSeesFirstProcessorsData(t *testing.T) {
	var env *cbdCallbackEnv
	var seen *spi.Entity
	env = newCallbackEnv(t, []spi.ProcessorDefinition{syncProc("p1"), syncProc("p2")},
		func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition, _ string) (*spi.Entity, error) {
			if proc.Name == "p1" {
				return &spi.Entity{Data: []byte(`{"x":42,"by":"engine"}`)}, nil
			}
			var err error
			seen, err = readUnderToken(ctx, env.factory, entity.Meta.ID)
			return nil, err
		})
	if _, _, _, _, err := manualWithData(t, env, "pcs-1", nil); err != nil {
		t.Fatalf("ManualTransition: %v", err)
	}
	if seen == nil {
		t.Fatal("the callback read nothing")
	}
	assertData(t, seen.Data, engineApplied)
}

// An anchor the transaction already holds as it is gets no save: the read
// still returns the committed version. The payload, and the entity the
// workflow goes on with, carry the running transaction's id: they are the
// version the operation makes.
func TestPreCalloutSave_NoChange_NoSave(t *testing.T) {
	var env *cbdCallbackEnv
	var seen *spi.Entity
	var payloadTxID string
	env = newCallbackEnv(t, []spi.ProcessorDefinition{syncProc("p1")},
		func(ctx context.Context, entity *spi.Entity, _ spi.ProcessorDefinition, _ string) (*spi.Entity, error) {
			payloadTxID = entity.Meta.TransactionID
			var err error
			seen, err = readUnderToken(ctx, env.factory, entity.Meta.ID)
			return nil, err
		})
	committedTxID, runningTxID, entity, _, err := manualWithData(t, env, "pcn-1", nil)
	if err != nil {
		t.Fatalf("ManualTransition: %v", err)
	}
	if seen == nil {
		t.Fatal("the callback read nothing")
	}
	if seen.Meta.TransactionID != committedTxID {
		t.Errorf("read transactionId = %q, want the committed %q: nothing was to be saved", seen.Meta.TransactionID, committedTxID)
	}
	if payloadTxID != runningTxID {
		t.Errorf("payload transactionId = %q, want the running transaction's %q", payloadTxID, runningTxID)
	}
	if entity.Meta.TransactionID != runningTxID {
		t.Errorf("entity transactionId after the callout = %q, want the running transaction's %q", entity.Meta.TransactionID, runningTxID)
	}
}

// A dispatch with no transaction id saves nothing, even in a transaction.
func TestPreCalloutSave_NoTxID_NoSave(t *testing.T) {
	env := newCallbackEnv(t, nil, nil)
	seeded := seedFireEntity(t, env.factory, env.ctx, "pct-1", env.model, "S_pre", true, map[string]any{"x": 0})
	txID, txCtx, err := env.txMgr.Begin(env.ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = env.txMgr.Rollback(env.ctx, txID) }()
	entity := makeEntity("pct-1", env.model, map[string]any{"x": 9})
	entity.Meta.State = "S_pre"
	entity.Meta.TransactionID = "caller-id"

	if err := env.engine.saveAnchorBeforeCallout(txCtx, entity, ""); err != nil {
		t.Fatalf("saveAnchorBeforeCallout: %v", err)
	}
	got, err := readUnderToken(txCtx, env.factory, "pct-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Meta.TransactionID != seeded.Meta.TransactionID {
		t.Errorf("read transactionId = %q, want the committed %q", got.Meta.TransactionID, seeded.Meta.TransactionID)
	}
	if entity.Meta.TransactionID != "caller-id" {
		t.Errorf("entity transactionId = %q, want it unchanged", entity.Meta.TransactionID)
	}
}

// A dispatch with no transaction in the context saves nothing: a save there
// would be a write of its own, committed at once.
func TestPreCalloutSave_NoTxInCtx_NoSave(t *testing.T) {
	env := newCallbackEnv(t, nil, nil)
	seeded := seedFireEntity(t, env.factory, env.ctx, "pcx-1", env.model, "S_pre", true, map[string]any{"x": 0})

	changed := makeEntity("pcx-1", env.model, map[string]any{"x": 9})
	changed.Meta.State = "S_pre"
	changed.Meta.TransactionID = "caller-id"
	if err := env.engine.saveAnchorBeforeCallout(env.ctx, changed, "minted-id"); err != nil {
		t.Fatalf("saveAnchorBeforeCallout: %v", err)
	}
	got, found := env.committed(t, "pcx-1")
	if !found || got.Meta.TransactionID != seeded.Meta.TransactionID {
		t.Errorf("committed = %+v, want the seed, transactionId %q", got, seeded.Meta.TransactionID)
	}
	if changed.Meta.TransactionID != "caller-id" {
		t.Errorf("entity transactionId = %q, want it unchanged", changed.Meta.TransactionID)
	}

	created := makeEntity("pcx-2", env.model, map[string]any{"x": 1})
	if err := env.engine.saveAnchorBeforeCallout(env.ctx, created, "minted-id"); err != nil {
		t.Fatalf("saveAnchorBeforeCallout: %v", err)
	}
	if got, found := env.committed(t, "pcx-2"); found {
		t.Errorf("committed = %+v, want none", got)
	}
}

// failingSaveFactory fails EntityStore.Save once *armed is set.
type failingSaveFactory struct {
	spi.StoreFactory
	armed *bool
	err   error
}

type failingSaveEntityStore struct {
	spi.EntityStore
	armed *bool
	err   error
}

func (f failingSaveFactory) EntityStore(ctx context.Context) (spi.EntityStore, error) {
	es, err := f.StoreFactory.EntityStore(ctx)
	if err != nil {
		return nil, err
	}
	return failingSaveEntityStore{EntityStore: es, armed: f.armed, err: f.err}, nil
}

func (s failingSaveEntityStore) Save(ctx context.Context, e *spi.Entity) (int64, error) {
	if *s.armed {
		return 0, s.err
	}
	return s.EntityStore.Save(ctx, e)
}

// A failed read or save before a callout is an infrastructure failure. It
// dispatched nothing, so a scheduled run neither marks its task nor records
// unsafe work as having reached a compute node.
func TestPreCalloutSave_FailureIsInfra_NotMarkedUnsafe(t *testing.T) {
	t.Run("read fails", func(t *testing.T) {
		stores, gets := 0, 0
		storeErr := errors.New("store unavailable")
		ext := &scriptedExtProc{}
		env := newRunEnvWith(t, ext, func(f spi.StoreFactory) spi.StoreFactory {
			return nthFailFactory{StoreFactory: f, stores: &stores, gets: &gets, err: storeErr}
		}, nil)
		claimed := env.claimed(t, "pcf-r1", oneHopWF("CLOSED", []spi.ProcessorDefinition{unsafeProc("p1", ExecutionModeSync)}, nil))
		// The first Get is the fire's own read; the second is the read
		// before the save.
		stores, gets = 0, 0
		env.engine.factory = cachedFactory(nthFailFactory{StoreFactory: env.factory.(nthFailFactory).StoreFactory,
			failGetAt: 2, stores: &stores, gets: &gets, err: storeErr})

		r := runWithin(t, env, claimed)
		if r.Outcome != OutcomeFailed || !errors.Is(r.Err, storeErr) || !errors.Is(r.Err, ErrAnchorSaveInfra) {
			t.Fatalf("report = %+v, want failed on the store error, marked ErrAnchorSaveInfra", r)
		}
		if r.MarkHeld || r.UnsafeReached {
			t.Errorf("report = %+v, want no mark and no unsafe work reached", r)
		}
		if n := ext.count("p1"); n != 0 {
			t.Errorf("p1 dispatched %d times, want 0", n)
		}
	})
	t.Run("save fails", func(t *testing.T) {
		armed := false
		storeErr := errors.New("store unavailable")
		ext := &scriptedExtProc{processor: func(_ context.Context, proc spi.ProcessorDefinition, _ string) (*spi.Entity, error) {
			if proc.Name == "p1" {
				armed = true
				return &spi.Entity{Data: []byte(`{"x":1}`)}, nil
			}
			return nil, nil
		}}
		env := newRunEnvWith(t, ext, func(f spi.StoreFactory) spi.StoreFactory {
			return failingSaveFactory{StoreFactory: f, armed: &armed, err: storeErr}
		}, nil)
		registerModelFields(t, env.ctx, env.factory, spi.ModelRef{EntityName: "run-pcf-s1", ModelVersion: "1.0"},
			map[string]schema.DataType{"x": schema.Integer})
		// p1 is idempotent and returns data; the save before p2 fails.
		claimed := env.claimed(t, "pcf-s1", oneHopWF("CLOSED", []spi.ProcessorDefinition{
			safeProc("p1", ExecutionModeSync), unsafeProc("p2", ExecutionModeSync),
		}, nil))

		r := runWithin(t, env, claimed)
		armed = false
		if r.Outcome != OutcomeFailed || !errors.Is(r.Err, storeErr) || !errors.Is(r.Err, ErrAnchorSaveInfra) {
			t.Fatalf("report = %+v, want failed on the store error, marked ErrAnchorSaveInfra", r)
		}
		if r.MarkHeld || r.UnsafeReached {
			t.Errorf("report = %+v, want no mark and no unsafe work reached", r)
		}
		if n := ext.count("p2"); n != 0 {
			t.Errorf("p2 dispatched %d times, want 0", n)
		}
	})
}

// The save fails on a client update: the operation fails as an
// infrastructure failure and the processor is not dispatched.
func TestPreCalloutSave_SaveFails_IsInfra(t *testing.T) {
	storeErr := errors.New("store unavailable")
	dispatched := 0
	env := newCallbackEnv(t, []spi.ProcessorDefinition{syncProc("p1")},
		func(context.Context, *spi.Entity, spi.ProcessorDefinition, string) (*spi.Entity, error) {
			dispatched++
			return nil, nil
		})
	armed := true
	env.engine.factory = cachedFactory(failingSaveFactory{StoreFactory: env.factory, armed: &armed, err: storeErr})

	_, _, _, _, err := manualWithData(t, env, "pcsf-1", []byte(`{"x":5}`))
	if !errors.Is(err, storeErr) || !errors.Is(err, ErrAnchorSaveInfra) {
		t.Fatalf("err = %v, want the store error marked ErrAnchorSaveInfra", err)
	}
	if dispatched != 0 {
		t.Errorf("p1 dispatched %d times, want 0", dispatched)
	}
}

// The step stamps the entity with the transaction's id, whatever id the
// dispatch carries, whether or not it saved. It saves only when the anchor
// differs from the one the transaction holds; an anchor the transaction does
// not hold is a create's that is not stored yet, and is saved.
func TestPreCalloutSave_StampsTransactionID(t *testing.T) {
	env := newCallbackEnv(t, nil, nil)
	seeded := seedFireEntity(t, env.factory, env.ctx, "pcv-1", env.model, "S_pre", true, map[string]any{"x": 0})
	txID, txCtx, err := env.txMgr.Begin(env.ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = env.txMgr.Rollback(env.ctx, txID) }()

	t.Run("unchanged: nothing saved", func(t *testing.T) {
		entity, err := readUnderToken(txCtx, env.factory, "pcv-1")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if err := env.engine.saveAnchorBeforeCallout(txCtx, entity, "minted-id"); err != nil {
			t.Fatalf("saveAnchorBeforeCallout: %v", err)
		}
		if entity.Meta.TransactionID != txID {
			t.Errorf("entity transactionId = %q, want the transaction's %q", entity.Meta.TransactionID, txID)
		}
		stored, err := readUnderToken(txCtx, env.factory, "pcv-1")
		if err != nil {
			t.Fatalf("Get after: %v", err)
		}
		if stored.Meta.TransactionID != seeded.Meta.TransactionID {
			t.Errorf("stored transactionId = %q, want the committed %q: nothing was to be saved", stored.Meta.TransactionID, seeded.Meta.TransactionID)
		}
	})
	t.Run("changed: saved", func(t *testing.T) {
		entity, err := readUnderToken(txCtx, env.factory, "pcv-1")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		entity.Data = []byte(`{"x":9}`)
		if err := env.engine.saveAnchorBeforeCallout(txCtx, entity, "minted-id"); err != nil {
			t.Fatalf("saveAnchorBeforeCallout: %v", err)
		}
		if entity.Meta.TransactionID != txID {
			t.Errorf("entity transactionId = %q, want the transaction's %q", entity.Meta.TransactionID, txID)
		}
		stored, err := readUnderToken(txCtx, env.factory, "pcv-1")
		if err != nil {
			t.Fatalf("Get after: %v", err)
		}
		if stored.Meta.TransactionID != txID {
			t.Errorf("stored transactionId = %q, want the transaction's %q", stored.Meta.TransactionID, txID)
		}
		assertData(t, stored.Data, map[string]any{"x": float64(9)})
	})
	t.Run("not stored: saved", func(t *testing.T) {
		entity := makeEntity("pcv-new", env.model, map[string]any{"x": 1})
		entity.Meta.TransactionID = "caller-id"
		if err := env.engine.saveAnchorBeforeCallout(txCtx, entity, "minted-id"); err != nil {
			t.Fatalf("saveAnchorBeforeCallout: %v", err)
		}
		if entity.Meta.TransactionID != txID {
			t.Errorf("entity transactionId = %q, want the transaction's %q", entity.Meta.TransactionID, txID)
		}
		stored, err := readUnderToken(txCtx, env.factory, "pcv-new")
		if err != nil {
			t.Fatalf("Get after: %v", err)
		}
		if stored.Meta.TransactionID != txID {
			t.Errorf("stored transactionId = %q, want the transaction's %q", stored.Meta.TransactionID, txID)
		}
	})
}

// A change of state alone is a change: after T1 moved the anchor to S_mid
// with no change to its data, the callback of T2's processor sees S_mid,
// stamped with the run's transaction.
func TestPreCalloutSave_StateOnlyChange_Saves(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)
	var seen *spi.Entity
	mock := &mockExternalProcessing{dispatchFunc: func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition, _, _, _ string) (*spi.Entity, error) {
		if proc.Name != "p1" {
			return nil, nil
		}
		var err error
		seen, err = readUnderToken(ctx, factory, entity.Meta.ID)
		return nil, err
	}}
	engine := NewEngine(cachedFactory(factory), uuids, txMgr, platformclock.Real(), WithExternalProcessing(mock), WithCoordination(txcoord.New(txMgr)))
	ctx := ctxWithTenant(testTenant)
	model := spi.ModelRef{EntityName: "presave-state", ModelVersion: "1.0"}
	registerModelFields(t, ctx, factory, model, map[string]schema.DataType{"x": schema.Integer})
	saveWorkflow(t, factory, ctx, model, []spi.WorkflowDefinition{{
		Version: "1.1", Name: "PresaveStateWF", InitialState: "S_pre", Active: true,
		States: map[string]spi.StateDefinition{
			"S_pre":  {Transitions: []spi.TransitionDefinition{{Name: "T1", Next: "S_mid", Manual: true, Processors: []spi.ProcessorDefinition{syncProc("p0")}}}},
			"S_mid":  {Transitions: []spi.TransitionDefinition{{Name: "T2", Next: "S_done", Processors: []spi.ProcessorDefinition{syncProc("p1")}}}},
			"S_done": {},
		},
	}})
	seedFireEntity(t, factory, ctx, "pcst-1", model, "S_pre", true, map[string]any{"x": 0})
	txID, txCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	t.Cleanup(func() { _ = txMgr.Rollback(ctx, txID) })
	entity, err := readUnderToken(txCtx, factory, "pcst-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	entity.Meta.TransactionID = txID
	if _, err := engine.ManualTransition(txCtx, entity, "T1", IfMatch{}); err != nil {
		t.Fatalf("ManualTransition: %v", err)
	}
	if seen == nil {
		t.Fatal("p1's callback read nothing")
	}
	if seen.Meta.State != "S_mid" || seen.Meta.TransactionID != txID {
		t.Errorf("read state %q, transactionId %q; want S_mid and the run's %q", seen.Meta.State, seen.Meta.TransactionID, txID)
	}
}

// calloutProbe is an external processing service whose processor, criterion
// and schedule-function dispatches each run a test's function.
type calloutProbe struct {
	processor func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition, txID string) (*spi.Entity, error)
	criterion func(ctx context.Context, entity *spi.Entity, target, transition, txID string) (bool, string, error)
	function  func(ctx context.Context, entity *spi.Entity, txID string) (contract.FunctionResult, error)
}

func (p *calloutProbe) DispatchProcessor(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition, _, _, txID string) (*spi.Entity, error) {
	if p.processor == nil {
		return nil, nil
	}
	return p.processor(ctx, entity, proc, txID)
}

func (p *calloutProbe) DispatchCriteria(ctx context.Context, entity *spi.Entity, _ json.RawMessage, target, _, transition, _, txID string) (bool, string, error) {
	if p.criterion == nil {
		return true, "", nil
	}
	return p.criterion(ctx, entity, target, transition, txID)
}

func (p *calloutProbe) DispatchFunction(ctx context.Context, entity *spi.Entity, _ spi.ScheduleFunction, _, _, txID string) (contract.FunctionResult, error) {
	if p.function == nil {
		return contract.FunctionResult{}, errors.New("no schedule function")
	}
	return p.function(ctx, entity, txID)
}

// newProbeEnv is an engine on a memory backend with probe as its external
// processing service and wfs as the model's workflows.
func newProbeEnv(t *testing.T, probe *calloutProbe, wfs []spi.WorkflowDefinition) *cbdCallbackEnv {
	t.Helper()
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)
	engine := NewEngine(cachedFactory(factory), uuids, txMgr, platformclock.Real(), WithExternalProcessing(probe), WithCoordination(txcoord.New(txMgr)))
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "presave-probe", ModelVersion: "1.0"}
	registerModelFields(t, ctx, factory, modelRef, map[string]schema.DataType{
		"x": schema.Integer, "by": schema.String,
	})
	saveWorkflow(t, factory, ctx, modelRef, wfs)
	return &cbdCallbackEnv{factory: factory, txMgr: txMgr, engine: engine, ctx: ctx, model: modelRef}
}

// begin opens a transaction rolled back at the end of the test.
func (env *cbdCallbackEnv) begin(t *testing.T) (string, context.Context) {
	t.Helper()
	txID, txCtx, err := env.txMgr.Begin(env.ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	t.Cleanup(func() { _ = env.txMgr.Rollback(env.ctx, txID) })
	return txID, txCtx
}

// fnCriterion is a criterion that is one function condition.
func fnCriterion(name string) json.RawMessage {
	return json.RawMessage(`{"type":"function","function":{"name":"` + name + `"}}`)
}

// calloutSight is what one callout's callback saw: the anchor read under the
// token, and the payload's transaction id.
type calloutSight struct {
	read      *spi.Entity
	readErr   error
	payloadTx string
}

func lookUnderToken(ctx context.Context, factory spi.StoreFactory, entity *spi.Entity) calloutSight {
	read, err := readUnderToken(ctx, factory, entity.Meta.ID)
	return calloutSight{read: read, readErr: err, payloadTx: entity.Meta.TransactionID}
}

// assertSight checks that the callback found the anchor in state, holding
// data, stamped with txID, and that the payload carried that id.
func assertSight(t *testing.T, what string, s *calloutSight, state string, data map[string]any, txID string) {
	t.Helper()
	if s == nil {
		t.Fatalf("%s: no callout was made", what)
	}
	if s.readErr != nil {
		t.Fatalf("%s: read under the token: %v", what, s.readErr)
	}
	assertData(t, s.read.Data, data)
	if s.read.Meta.State != state {
		t.Errorf("%s: state = %q, want %q", what, s.read.Meta.State, state)
	}
	if s.read.Meta.TransactionID != txID {
		t.Errorf("%s: read transactionId = %q, want %q", what, s.read.Meta.TransactionID, txID)
	}
	if s.payloadTx != s.read.Meta.TransactionID {
		t.Errorf("%s: payload transactionId = %q, want the read's %q", what, s.payloadTx, s.read.Meta.TransactionID)
	}
}

// An ASYNC_NEW_TX processor's callback sees the anchor the engine saved
// before the dispatch. The save is in the chain's scope, not the processor's:
// a processor failure discards the processor's scope and keeps the save.
func TestPreCalloutSave_AsyncNewTx_CallbackSeesAnchor_FailureKeepsSave(t *testing.T) {
	var env *cbdCallbackEnv
	var seen *calloutSight
	probe := &calloutProbe{processor: func(ctx context.Context, entity *spi.Entity, _ spi.ProcessorDefinition, _ string) (*spi.Entity, error) {
		s := lookUnderToken(ctx, env.factory, entity)
		seen = &s
		return nil, errors.New("processor failed")
	}}
	env = newProbeEnv(t, probe, []spi.WorkflowDefinition{{
		Version: "1.1", Name: "PresaveAsyncWF", InitialState: "S_pre", Active: true,
		States: map[string]spi.StateDefinition{
			"S_pre": {Transitions: []spi.TransitionDefinition{{Name: "CALLOUT", Next: "S_post", Manual: true, Processors: []spi.ProcessorDefinition{
				{Type: ProcessorTypeExternalized, Name: "a1", ExecutionMode: ExecutionModeAsyncNewTx},
			}}}},
			"S_post": {},
		},
	}})
	seedFireEntity(t, env.factory, env.ctx, "pca-1", env.model, "S_pre", true, map[string]any{"x": 0})
	txID, txCtx := env.begin(t)
	entity, err := readUnderToken(txCtx, env.factory, "pca-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	entity.Data = []byte(`{"x":5,"by":"request"}`)
	entity.Meta.TransactionID = txID
	if _, err := env.engine.ManualTransition(txCtx, entity, "CALLOUT", IfMatch{}); err != nil {
		t.Fatalf("ManualTransition: %v", err)
	}
	assertSight(t, "a1", seen, "S_pre", map[string]any{"x": float64(5), "by": "request"}, txID)

	after, err := readUnderToken(txCtx, env.factory, "pca-1")
	if err != nil {
		t.Fatalf("Get after the run: %v", err)
	}
	if after.Meta.TransactionID != txID {
		t.Errorf("after the failed processor: transactionId = %q, want the save of %q kept", after.Meta.TransactionID, txID)
	}
	assertData(t, after.Data, map[string]any{"x": float64(5), "by": "request"})
}

// A failed save before an ASYNC_NEW_TX processor fails the operation: it is
// not the processor's failure, which the pipeline logs and goes past.
func TestPreCalloutSave_AsyncNewTx_SaveFails_FailsOperation(t *testing.T) {
	storeErr := errors.New("store unavailable")
	dispatched := 0
	probe := &calloutProbe{processor: func(context.Context, *spi.Entity, spi.ProcessorDefinition, string) (*spi.Entity, error) {
		dispatched++
		return nil, nil
	}}
	env := newProbeEnv(t, probe, []spi.WorkflowDefinition{{
		Version: "1.1", Name: "PresaveAsyncFailWF", InitialState: "S_pre", Active: true,
		States: map[string]spi.StateDefinition{
			"S_pre": {Transitions: []spi.TransitionDefinition{{Name: "CALLOUT", Next: "S_post", Manual: true, Processors: []spi.ProcessorDefinition{
				{Type: ProcessorTypeExternalized, Name: "a1", ExecutionMode: ExecutionModeAsyncNewTx},
			}}}},
			"S_post": {},
		},
	}})
	seedFireEntity(t, env.factory, env.ctx, "pcaf-1", env.model, "S_pre", true, map[string]any{"x": 0})
	txID, txCtx := env.begin(t)
	entity, err := readUnderToken(txCtx, env.factory, "pcaf-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	entity.Data = []byte(`{"x":5}`)
	entity.Meta.TransactionID = txID
	armed := true
	env.engine.factory = cachedFactory(failingSaveFactory{StoreFactory: env.factory, armed: &armed, err: storeErr})

	_, err = env.engine.ManualTransition(txCtx, entity, "CALLOUT", IfMatch{})
	if !errors.Is(err, storeErr) || !errors.Is(err, ErrAnchorSaveInfra) {
		t.Fatalf("err = %v, want the store error marked ErrAnchorSaveInfra", err)
	}
	if dispatched != 0 {
		t.Errorf("a1 dispatched %d times, want 0", dispatched)
	}
}

// A function criterion's callback sees the anchor as it is at the callout:
// on a manual transition the request's data in the source state, and on the
// automated cascade after it the data a processor returned, in the state the
// cascade reached.
func TestPreCalloutSave_CriterionCascade_SeesCurrentStateAndData(t *testing.T) {
	var env *cbdCallbackEnv
	seen := map[string]*calloutSight{}
	probe := &calloutProbe{
		processor: func(context.Context, *spi.Entity, spi.ProcessorDefinition, string) (*spi.Entity, error) {
			return &spi.Entity{Data: []byte(`{"x":42,"by":"engine"}`)}, nil
		},
		criterion: func(ctx context.Context, entity *spi.Entity, _, transition, _ string) (bool, string, error) {
			s := lookUnderToken(ctx, env.factory, entity)
			seen[transition] = &s
			return true, "", nil
		},
	}
	env = newProbeEnv(t, probe, []spi.WorkflowDefinition{{
		Version: "1.1", Name: "PresaveCritWF", InitialState: "S_pre", Active: true,
		States: map[string]spi.StateDefinition{
			"S_pre": {Transitions: []spi.TransitionDefinition{{Name: "T1", Next: "S_mid", Manual: true,
				Criterion: fnCriterion("fm"), Processors: []spi.ProcessorDefinition{syncProc("p1")}}}},
			"S_mid":  {Transitions: []spi.TransitionDefinition{{Name: "AUTO", Next: "S_done", Criterion: fnCriterion("fa")}}},
			"S_done": {},
		},
	}})
	seedFireEntity(t, env.factory, env.ctx, "pcc-crit", env.model, "S_pre", true, map[string]any{"x": 0})
	txID, txCtx := env.begin(t)
	entity, err := readUnderToken(txCtx, env.factory, "pcc-crit")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	entity.Data = []byte(`{"x":5,"by":"request"}`)
	entity.Meta.TransactionID = txID
	if _, err := env.engine.ManualTransition(txCtx, entity, "T1", IfMatch{}); err != nil {
		t.Fatalf("ManualTransition: %v", err)
	}
	assertSight(t, "manual transition criterion", seen["T1"], "S_pre", map[string]any{"x": float64(5), "by": "request"}, txID)
	assertSight(t, "cascade criterion", seen["AUTO"], "S_mid", engineApplied, txID)
}

// On a create, workflow selection runs before the anchor has a state: the
// selection criterion's callback finds the anchor with the request's data
// and an empty state.
func TestPreCalloutSave_WorkflowSelectionOnCreate_EmptyState(t *testing.T) {
	var env *cbdCallbackEnv
	var seen *calloutSight
	probe := &calloutProbe{criterion: func(ctx context.Context, entity *spi.Entity, target, _, _ string) (bool, string, error) {
		if target == "WORKFLOW" {
			s := lookUnderToken(ctx, env.factory, entity)
			seen = &s
		}
		return true, "", nil
	}}
	env = newProbeEnv(t, probe, []spi.WorkflowDefinition{{
		Version: "1.1", Name: "PresaveSelectWF", InitialState: "S_new", Active: true, Criterion: fnCriterion("sel"),
		States: map[string]spi.StateDefinition{"S_new": {}},
	}})
	txID, txCtx := env.begin(t)
	entity := makeEntity("pcw-1", env.model, map[string]any{"x": 3, "by": "request"})
	entity.Meta.TransactionID = txID
	if _, err := env.engine.Execute(txCtx, entity); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	assertSight(t, "workflow selection", seen, "", map[string]any{"x": float64(3), "by": "request"}, txID)
}

// A schedule function on a client door runs before the door's final save:
// its callback sees the anchor in the state the cascade settled in, with the
// request's data.
func TestPreCalloutSave_ScheduleFunction_ClientDoor(t *testing.T) {
	var env *cbdCallbackEnv
	var seen *calloutSight
	probe := &calloutProbe{function: func(ctx context.Context, entity *spi.Entity, _ string) (contract.FunctionResult, error) {
		s := lookUnderToken(ctx, env.factory, entity)
		seen = &s
		return contract.FunctionResult{Kind: "Schedule", Value: json.RawMessage(`{"fireAfterMs":60000}`)}, nil
	}}
	env = newProbeEnv(t, probe, []spi.WorkflowDefinition{{
		Version: "1.1", Name: "PresaveSchedWF", InitialState: "S_pre", Active: true,
		States: map[string]spi.StateDefinition{
			"S_pre": {Transitions: []spi.TransitionDefinition{{Name: "WAIT", Next: "S_wait", Manual: true}}},
			"S_wait": {Transitions: []spi.TransitionDefinition{{Name: "TIMEOUT", Next: "S_done", Schedule: &spi.TransitionSchedule{
				Function: &spi.ScheduleFunction{Name: "calc", ResultKind: "Schedule", CalculationNodesTags: "sched"},
			}}}},
			"S_done": {},
		},
	}})
	seedFireEntity(t, env.factory, env.ctx, "pcsch-1", env.model, "S_pre", true, map[string]any{"x": 0})
	txID, txCtx := env.begin(t)
	entity, err := readUnderToken(txCtx, env.factory, "pcsch-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	entity.Data = []byte(`{"x":5,"by":"request"}`)
	entity.Meta.TransactionID = txID
	if _, err := env.engine.ManualTransition(txCtx, entity, "WAIT", IfMatch{}); err != nil {
		t.Fatalf("ManualTransition: %v", err)
	}
	assertSight(t, "schedule function", seen, "S_wait", map[string]any{"x": float64(5), "by": "request"}, txID)
}

// The available-transitions read runs the workflow-selection criterion with
// no transaction id. It writes nothing, even inside a transaction and with
// an entity that differs from the stored one.
func TestAvailableTransitionsRead_RunsCriterion_NoWrite(t *testing.T) {
	var env *cbdCallbackEnv
	var seen *calloutSight
	var dispatchTxID string
	probe := &calloutProbe{criterion: func(ctx context.Context, entity *spi.Entity, _, _, txID string) (bool, string, error) {
		dispatchTxID = txID
		s := lookUnderToken(ctx, env.factory, entity)
		seen = &s
		return true, "", nil
	}}
	env = newProbeEnv(t, probe, []spi.WorkflowDefinition{{
		Version: "1.1", Name: "PresaveQueryWF", InitialState: "S_pre", Active: true, Criterion: fnCriterion("sel"),
		States: map[string]spi.StateDefinition{
			"S_pre":  {Transitions: []spi.TransitionDefinition{{Name: "T1", Next: "S_done", Manual: true}}},
			"S_done": {},
		},
	}})
	seeded := seedFireEntity(t, env.factory, env.ctx, "pcq-1", env.model, "S_pre", true, map[string]any{"x": 0})
	_, txCtx := env.begin(t)
	entity, err := readUnderToken(txCtx, env.factory, "pcq-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	entity.Data = []byte(`{"x":9}`)
	names, err := env.engine.GetAvailableTransitionsForEntity(txCtx, entity)
	if err != nil {
		t.Fatalf("GetAvailableTransitionsForEntity: %v", err)
	}
	if len(names) != 1 || names[0] != "T1" {
		t.Errorf("transitions = %v, want [T1]", names)
	}
	if seen == nil {
		t.Fatal("the selection criterion was not called")
	}
	if dispatchTxID != "" {
		t.Errorf("dispatch transaction id = %q, want none", dispatchTxID)
	}
	after, err := readUnderToken(txCtx, env.factory, "pcq-1")
	if err != nil {
		t.Fatalf("Get after the read: %v", err)
	}
	if after.Meta.TransactionID != seeded.Meta.TransactionID {
		t.Errorf("transactionId = %q, want the committed %q: a read must not write", after.Meta.TransactionID, seeded.Meta.TransactionID)
	}
	assertData(t, after.Data, map[string]any{"x": float64(0)})
}

// A scheduled fire whose target state arms a function schedule fires: the
// save before the schedule function writes the anchor in the fire's
// transaction, and the final persist names that transaction, not the
// committed version it read first.
func TestPreCalloutSave_ScheduledFire_TargetArmsFunctionSchedule_Fires(t *testing.T) {
	ext := &scriptedExtProc{function: func(context.Context) (contract.FunctionResult, error) {
		return contract.FunctionResult{Kind: "Schedule", Value: json.RawMessage(`{"fireAfterMs":60000}`)}, nil
	}}
	env := newRunEnv(t, ext)
	claimed := env.claimed(t, "pcfire-1", oneHopWF("DONE", nil, map[string]spi.StateDefinition{
		"DONE": {Transitions: []spi.TransitionDefinition{{Name: "Tick", Next: "OPEN",
			Schedule: &spi.TransitionSchedule{Function: &spi.ScheduleFunction{Name: "tick", ResultKind: "Schedule", CalculationNodesTags: "sched"}}}}},
	}))
	r, _ := env.run(claimed)
	if r.Outcome != OutcomeFired {
		t.Fatalf("report = %+v, want fired", r)
	}
	if got := env.state(t, "pcfire-1"); got != "DONE" {
		t.Errorf("entity state = %q, want DONE", got)
	}
}

func cbdSameCtxProc(name string) spi.ProcessorDefinition {
	return spi.ProcessorDefinition{Type: ProcessorTypeExternalized, Name: name, ExecutionMode: ExecutionModeCommitBeforeDispatch}
}

// wantPayloadTxID is the payload transactionId rule: the dispatch's
// transaction id when the dispatch carries one, the id of the entity's last
// committed version (what a callback read without a token returns) when it
// does not.
func wantPayloadTxID(dispatchTxID string, s *calloutSight) string {
	if dispatchTxID != "" {
		return dispatchTxID
	}
	return s.read.Meta.TransactionID
}

// On the update path every processor's payload carries the transaction id of
// its dispatch, and a dispatch without one (COMMIT_BEFORE_DISPATCH without
// startNewTxOnDispatch) carries the id of the version the segment boundary
// committed.
func TestPayloadTxID_IsTheDispatchTransaction_UpdatePath(t *testing.T) {
	cases := []struct {
		name  string
		procs []spi.ProcessorDefinition
	}{
		{"sync", []spi.ProcessorDefinition{syncProc("p1")}},
		{"after a segment boundary", []spi.ProcessorDefinition{cbdSameCtxProc("c1"), cbdNewTxProc("c2"), syncProc("p3")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var env *cbdCallbackEnv
			seen := map[string]*calloutSight{}
			dispatchTx := map[string]string{}
			env = newCallbackEnv(t, tc.procs,
				func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition, txID string) (*spi.Entity, error) {
					s := lookUnderToken(ctx, env.factory, entity)
					seen[proc.Name] = &s
					dispatchTx[proc.Name] = txID
					return nil, nil
				})
			_, _, entity, result, err := manualWithData(t, env, "ptx-"+tc.name, []byte(`{"x":5,"by":"request"}`))
			if err != nil {
				t.Fatalf("ManualTransition: %v", err)
			}
			for _, p := range tc.procs {
				s := seen[p.Name]
				if s == nil {
					t.Fatalf("%s: no dispatch", p.Name)
				}
				if s.readErr != nil {
					t.Fatalf("%s: callback read: %v", p.Name, s.readErr)
				}
				if want := wantPayloadTxID(dispatchTx[p.Name], s); s.payloadTx != want {
					t.Errorf("%s: payload transactionId = %q, want %q", p.Name, s.payloadTx, want)
				}
			}
			stored, err := readUnderToken(result.FinalCtx, env.factory, entity.Meta.ID)
			if err != nil {
				t.Fatalf("read after the run: %v", err)
			}
			if entity.Meta.TransactionID != stored.Meta.TransactionID {
				t.Errorf("after the run: entity transactionId = %q, want the stored anchor's %q", entity.Meta.TransactionID, stored.Meta.TransactionID)
			}
		})
	}
}

// A COMMIT_BEFORE_DISPATCH processor with startNewTxOnDispatch is dispatched
// with TX_post's token, and its payload carries TX_post's id: TX_post makes
// the next version. On a scheduled fire too, though the entity arrived with
// the committed id of an earlier transaction. After the post-dispatch apply,
// the engine's entity carries TX_post's id.
func TestPayloadTxID_IsTheDispatchTransaction_CommitBeforeDispatch(t *testing.T) {
	t.Run("scheduled fire", func(t *testing.T) {
		var env *runEnv
		var seen *calloutSight
		var dispatchTx string
		probe := &calloutProbe{processor: func(ctx context.Context, entity *spi.Entity, _ spi.ProcessorDefinition, txID string) (*spi.Entity, error) {
			s := lookUnderToken(ctx, env.factory, entity)
			seen = &s
			dispatchTx = txID
			return nil, nil
		}}
		env = newRunEnv(t, probe)
		claimed := env.claimed(t, "ptx-cbd-fire", oneHopWF("DONE", []spi.ProcessorDefinition{cbdNewTxProc("c1")}, nil))
		r, _ := env.run(claimed)
		if seen == nil {
			t.Fatal("no dispatch")
		}
		if seen.readErr != nil {
			t.Fatalf("callback read: %v", seen.readErr)
		}
		if dispatchTx == "" {
			t.Fatal("dispatched without a transaction id")
		}
		if seen.payloadTx != dispatchTx {
			t.Errorf("payload transactionId = %q, want TX_post's %q", seen.payloadTx, dispatchTx)
		}
		if r.Outcome != OutcomeFired {
			t.Fatalf("report = %+v, want fired", r)
		}
		if got := env.state(t, "ptx-cbd-fire"); got != "DONE" {
			t.Errorf("entity state = %q, want DONE", got)
		}
	})

	t.Run("two segments", func(t *testing.T) {
		var env *cbdCallbackEnv
		seen := map[string]*calloutSight{}
		dispatchTx := map[string]string{}
		env = newCallbackEnv(t, []spi.ProcessorDefinition{cbdNewTxProc("c1"), cbdNewTxProc("c2")},
			func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition, txID string) (*spi.Entity, error) {
				s := lookUnderToken(ctx, env.factory, entity)
				seen[proc.Name] = &s
				dispatchTx[proc.Name] = txID
				return &spi.Entity{Data: []byte(`{"x":42,"by":"engine"}`)}, nil
			})
		_, entity, result, err := env.transition(t, "ptx-cbd-two")
		if err != nil {
			t.Fatalf("ManualTransition: %v", err)
		}
		t.Cleanup(func() { _ = env.txMgr.Rollback(env.ctx, result.FinalTxID) })
		for _, name := range []string{"c1", "c2"} {
			s := seen[name]
			if s == nil {
				t.Fatalf("%s: no dispatch", name)
			}
			if s.readErr != nil {
				t.Fatalf("%s: callback read: %v", name, s.readErr)
			}
			if dispatchTx[name] == "" {
				t.Fatalf("%s: dispatched without a transaction id", name)
			}
			if s.payloadTx != dispatchTx[name] {
				t.Errorf("%s: payload transactionId = %q, want TX_post's %q", name, s.payloadTx, dispatchTx[name])
			}
		}
		if entity.Meta.TransactionID != result.FinalTxID {
			t.Errorf("after the apply: entity transactionId = %q, want TX_post's %q", entity.Meta.TransactionID, result.FinalTxID)
		}
	})
}
