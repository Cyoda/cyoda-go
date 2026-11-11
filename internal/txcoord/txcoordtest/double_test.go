package txcoordtest_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	"github.com/cyoda-platform/cyoda-go/internal/txcoord/txcoordtest"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// The two spitest Coordination cases the conformance run skips, with the
// entity committed before the case's transaction begins.

type fixture struct {
	t      *testing.T
	f      spi.StoreFactory
	co     spi.TransactionCoordinator
	tm     spi.TransactionManager
	ctx    context.Context
	txID   string
	txCtx  context.Context
	entity string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := txcoordtest.NewFactory(memory.NewStoreFactory())
	t.Cleanup(func() { _ = f.Close() })
	co, err := f.(spi.TransactionCoordinationCapable).TransactionCoordinator(context.Background())
	if err != nil || co == nil {
		t.Fatalf("TransactionCoordinator = (%v, %v)", co, err)
	}
	tenant := spi.TenantID("double-" + uuid.NewString())
	ctx := spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID: "double-test", Tenant: spi.Tenant{ID: tenant, Name: string(tenant)},
	})
	tm, err := f.TransactionManager(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fx := &fixture{t: t, f: f, co: co, tm: tm, ctx: ctx}

	// The seed, committed before the case's transaction begins.
	fx.entity = uuid.NewString()
	seedID, seedCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fx.save(seedCtx, fx.entity, "seed")
	if err := tm.Commit(seedCtx, seedID); err != nil {
		t.Fatal(err)
	}

	fx.txID, fx.txCtx, err = tm.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tm.Rollback(fx.txCtx, fx.txID) })
	return fx
}

func (fx *fixture) save(ctx context.Context, id, by string) {
	fx.t.Helper()
	es, err := fx.f.EntityStore(ctx)
	if err != nil {
		fx.t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]string{"by": by})
	e := &spi.Entity{Meta: spi.EntityMeta{ID: id, ModelRef: spi.ModelRef{EntityName: "m-double", ModelVersion: "1"}}, Data: data}
	if _, err := es.Save(ctx, e); err != nil {
		fx.t.Fatal(err)
	}
}

func (fx *fixture) by(ctx context.Context) string {
	fx.t.Helper()
	es, err := fx.f.EntityStore(ctx)
	if err != nil {
		fx.t.Fatal(err)
	}
	e, err := es.Get(ctx, fx.entity)
	if err != nil {
		fx.t.Fatal(err)
	}
	var v struct{ By string }
	if err := json.Unmarshal(e.Data, &v); err != nil {
		fx.t.Fatal(err)
	}
	return v.By
}

// begin begins a callout from the scope on ctx, under outer, at major 1.
func (fx *fixture) begin(ctx context.Context, outer ...spi.CalloutPair) string {
	fx.t.Helper()
	id := "callout-" + uuid.NewString()
	if err := fx.co.BeginCallout(ctx, fx.txID, id, outer); err != nil {
		fx.t.Fatal(err)
	}
	if err := fx.co.AdvanceCallout(ctx, fx.txID, id, 1); err != nil {
		fx.t.Fatal(err)
	}
	return id
}

func (fx *fixture) open(pairs ...spi.CalloutPair) context.Context {
	fx.t.Helper()
	jctx, err := fx.tm.Join(fx.ctx, fx.txID)
	if err != nil {
		fx.t.Fatal(err)
	}
	sctx, err := fx.co.OpenScope(jctx, fx.txID, pairs)
	if err != nil {
		fx.t.Fatal(err)
	}
	return sctx
}

func at(callout string) spi.CalloutPair { return spi.CalloutPair{Callout: callout, Major: 1} }

// TestDouble_ScopeConflict: two scopes read the entity at the same version and
// each writes it; the first to complete wins, the second is refused with
// ErrScopeConflict and discarded.
func TestDouble_ScopeConflict(t *testing.T) {
	fx := newFixture(t)
	c := fx.begin(fx.txCtx)
	s1, s2 := fx.open(at(c)), fx.open(at(c))
	if fx.by(s1) != "seed" || fx.by(s2) != "seed" {
		t.Fatal("both scopes must read the seeded version")
	}
	fx.save(s1, fx.entity, "s1")
	fx.save(s2, fx.entity, "s2")
	if got := fx.by(s2); got != "s2" {
		t.Fatalf("s2 reads %q; want its own write", got)
	}
	if got := fx.by(fx.txCtx); got != "seed" {
		t.Fatalf("the root reads %q before any scope completed; want seed", got)
	}

	if err := fx.co.CompleteScope(s1); err != nil {
		t.Fatalf("CompleteScope(s1) = %v", err)
	}
	if err := fx.co.CompleteScope(s2); !errors.Is(err, spi.ErrScopeConflict) {
		t.Fatalf("CompleteScope(s2) = %v; want ErrScopeConflict", err)
	}
	if err := fx.co.EndCallout(fx.txCtx, fx.txID, c); err != nil {
		t.Fatal(err)
	}
	if err := fx.co.Seal(fx.txCtx, fx.txID); err != nil {
		t.Fatal(err)
	}
	if err := fx.tm.Commit(fx.txCtx, fx.txID); err != nil {
		t.Fatal(err)
	}
	if got := fx.by(fx.ctx); got != "s1" {
		t.Fatalf("committed %q; want s1", got)
	}
}

// TestDouble_RollbackOnlyBeforeScopeConflict: a marked scope that also
// conflicts is refused as rollback-only first.
func TestDouble_RollbackOnlyBeforeScopeConflict(t *testing.T) {
	fx := newFixture(t)
	c := fx.begin(fx.txCtx)
	s1, s2 := fx.open(at(c)), fx.open(at(c))
	fx.save(s1, fx.entity, "s1")
	fx.save(s2, fx.entity, "s2")
	if err := fx.co.CompleteScope(s1); err != nil {
		t.Fatal(err)
	}
	n := fx.begin(s2, at(c))
	s3 := fx.open(at(n), at(c))
	if err := fx.co.DiscardScope(s3, true); err != nil {
		t.Fatal(err)
	}
	if err := fx.co.EndCallout(s2, fx.txID, n); err != nil {
		t.Fatal(err)
	}
	if err := fx.co.CompleteScope(s2); !errors.Is(err, spi.ErrTxRollbackOnly) {
		t.Fatalf("CompleteScope(s2) = %v; want ErrTxRollbackOnly", err)
	}
}

// TestDouble_SupersededBeforeRollbackOnly: a superseded scope under a marked
// parent is reported, and refused, as superseded first.
func TestDouble_SupersededBeforeRollbackOnly(t *testing.T) {
	fx := newFixture(t)
	c := fx.begin(fx.txCtx)
	s1 := fx.open(at(c))
	n := fx.begin(s1, at(c))
	s2, s3 := fx.open(at(n), at(c)), fx.open(at(n), at(c))

	if err := fx.co.DiscardScope(s3, true); err != nil {
		t.Fatal(err)
	}
	if err := fx.co.CheckScope(s1); !errors.Is(err, spi.ErrTxRollbackOnly) {
		t.Fatalf("CheckScope(s1) = %v; want ErrTxRollbackOnly", err)
	}
	if err := fx.co.AdvanceCallout(s1, fx.txID, n, 2); err != nil {
		t.Fatal(err)
	}
	if err := fx.co.CheckScope(s2); !errors.Is(err, spi.ErrCalloutSuperseded) {
		t.Fatalf("CheckScope(s2) = %v; want ErrCalloutSuperseded", err)
	}
	if err := fx.co.CompleteScope(s2); !errors.Is(err, spi.ErrCalloutSuperseded) {
		t.Fatalf("CompleteScope(s2) = %v; want ErrCalloutSuperseded", err)
	}
}
