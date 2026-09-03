package entity

// TestDeleteEntitiesConditional_IteratorCloseErrorFailsDelete pins the
// final-review finding that two of the delete path's iterator-drain sites
// (drainDeleteSelection for single-tx delete, and deleteBatched's streamed
// loop) treated Close()'s error as advisory (log-and-continue) while
// trusting Err() as authoritative. For a database/sql-backed iterator (e.g.
// plugins/sqlite's sqliteIter), Close() returns rows.Close()'s error and
// database/sql does NOT fold that into Rows.Err() — so Err() can stay nil
// while Close() reports the scan was cut short. Before the fix, that
// combination let a delete report success (HTTP 200) for a partial
// selection, indistinguishable from a complete one. closeErrIterator
// reproduces exactly that shape.

import (
	"context"
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// entityCloseErrIterator wraps a real spi.Iterator, letting Next()/Entity()/
// Err() behave exactly as the wrapped iterator does but forcing Close() to
// return a non-nil error.
type entityCloseErrIterator struct {
	spi.Iterator
}

func (c *entityCloseErrIterator) Close() error {
	_ = c.Iterator.Close()
	return errEntityCloseInjected
}

var errEntityCloseInjected = errors.New("injected close error: driver error mid-scan")

// closeErrEntityStore wraps a real spi.EntityStore,
// handing back an entityCloseErrIterator from every Iterate call.
type closeErrEntityStore struct {
	spi.EntityStore
}

func (s *closeErrEntityStore) Iterate(ctx context.Context, model spi.ModelRef, filter spi.Filter, opts spi.IterateOptions) (spi.Iterator, error) {
	it, err := s.EntityStore.Iterate(ctx, model, filter, opts)
	if err != nil {
		return nil, err
	}
	return &entityCloseErrIterator{Iterator: it}, nil
}

type closeErrFactory struct {
	spi.StoreFactory
	store spi.EntityStore
}

func (f *closeErrFactory) EntityStore(context.Context) (spi.EntityStore, error) {
	return f.store, nil
}

func TestDeleteEntitiesConditional_SingleTx_IteratorCloseErrorFailsDelete(t *testing.T) {
	realFactory, ctx, ref := newDeleteStreamFixture(t)

	realStore, err := realFactory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	spyFactory := &closeErrFactory{StoreFactory: realFactory, store: &closeErrEntityStore{EntityStore: realStore}}

	txMgr := mustTxMgr(t, realFactory)
	h := buildDeleteStreamHandler(t, spyFactory, txMgr)
	seedKind(t, h, ctx, ref, 3, "drop")

	cond := []byte(`{"type":"simple","jsonPath":"$.kind","operatorType":"EQUALS","value":"drop"}`)
	_, err = h.DeleteEntitiesConditional(ctx, ref.EntityName, ref.ModelVersion, cond, nil, false, 0)
	if err == nil {
		t.Fatal("expected DeleteEntitiesConditional to fail when the selection iterator's Close() errors, even with Err()==nil — a silent Close() error must not report a partial selection as a complete success")
	}
}

func TestDeleteEntitiesConditional_Batched_IteratorCloseErrorFailsDelete(t *testing.T) {
	realFactory, ctx, ref := newDeleteStreamFixture(t)

	realStore, err := realFactory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	spyFactory := &closeErrFactory{StoreFactory: realFactory, store: &closeErrEntityStore{EntityStore: realStore}}

	txMgr := mustTxMgr(t, realFactory)
	h := buildDeleteStreamHandler(t, spyFactory, txMgr)
	seedKind(t, h, ctx, ref, 3, "drop")

	cond := []byte(`{"type":"simple","jsonPath":"$.kind","operatorType":"EQUALS","value":"drop"}`)
	_, err = h.DeleteEntitiesConditional(ctx, ref.EntityName, ref.ModelVersion, cond, nil, false, 1)
	if err == nil {
		t.Fatal("expected batched DeleteEntitiesConditional to fail when the streamed selection iterator's Close() errors, even with Err()==nil")
	}
}

// newDeleteStreamFixture is a thin wrapper around newDeleteStreamCtx that
// also returns the factory and ref, for tests that need to build their own
// store wrapper around the real factory (unlike newDeleteStreamCtx's callers
// in delete_stream_test.go, which build the ctx first and the factory
// separately).
func newDeleteStreamFixture(t *testing.T) (realFactory spi.StoreFactory, ctx context.Context, ref spi.ModelRef) {
	t.Helper()
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	ref = spi.ModelRef{EntityName: "CloseErrModel", ModelVersion: "1"}
	ctx = newDeleteStreamCtx(t, factory, ref)
	return factory, ctx, ref
}
