package entity

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	wfengine "github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// --- Task E4.1: RED tests for the streamed delete selection ---
//
// These tests pin that DeleteEntitiesConditional (single-tx and batched) and
// DeleteAllEntities select entities via an Iterate drain instead of
// search.SearchService.Search, and never hold the matched set as
// materialised *spi.Entity values.

// searchForbiddenStore wraps a real spi.EntityStore and fails the test
// outright if Search is ever called on it — proving a delete path selects
// via Iterate, never search.SearchService's pushdown. Also counts every
// Iterate() call and every
// spi.Iterator.Entity() call it hands out across its lifetime, so a test can
// assert (a) how many times selection re-opened an iterator (streamed
// batching re-opens one per cycle) and (b) that the number of entities
// actually streamed through equals the expected match count — the closest
// external evidence available that selection streamed rather than
// materialising a slice of entities (an internal implementation detail no
// assertion outside the package can observe directly).
type searchForbiddenStore struct {
	spi.EntityStore
	t            *testing.T
	iterateOpens int32
	entityCalls  int32
}

func (s *searchForbiddenStore) Search(context.Context, spi.Filter, spi.SearchOptions) ([]*spi.Entity, error) {
	s.t.Helper()
	s.t.Fatal("Search must not be called by a delete path: selection streams via Iterate")
	return nil, fmt.Errorf("unreachable")
}

func (s *searchForbiddenStore) Iterate(ctx context.Context, model spi.ModelRef, filter spi.Filter, opts spi.IterateOptions) (spi.Iterator, error) {
	atomic.AddInt32(&s.iterateOpens, 1)
	it, err := s.EntityStore.Iterate(ctx, model, filter, opts)
	if err != nil {
		return nil, err
	}
	return &countingIterator{Iterator: it, store: s}, nil
}

func (s *searchForbiddenStore) iterateOpenCount() int {
	return int(atomic.LoadInt32(&s.iterateOpens))
}

func (s *searchForbiddenStore) entityCallCount() int {
	return int(atomic.LoadInt32(&s.entityCalls))
}

// countingIterator wraps a real spi.Iterator, counting Entity() calls into
// the owning searchForbiddenStore.
type countingIterator struct {
	spi.Iterator
	store *searchForbiddenStore
}

func (it *countingIterator) Entity() *spi.Entity {
	atomic.AddInt32(&it.store.entityCalls, 1)
	return it.Iterator.Entity()
}

// searchForbiddenFactory wraps a real spi.StoreFactory but always hands out
// the given searchForbiddenStore from EntityStore(); every other accessor
// delegates to the wrapped factory unchanged.
type searchForbiddenFactory struct {
	spi.StoreFactory
	store *searchForbiddenStore
}

func (f *searchForbiddenFactory) EntityStore(_ context.Context) (spi.EntityStore, error) {
	return f.store, nil
}

// newDeleteStreamCtx builds a UserContext-scoped ctx and registers ref
// locked with a schema declaring "kind" (String) and an "items" array of
// {name: String} objects, against factory. The items[*].name leaf exists so
// TestDeleteEntitiesConditional_SingleTx_UntranslatableConditionStreams's
// wildcard-array condition clears path validation — mirrors search's own
// matchAllFixtureCondition fixture (saveModelWithValAndItemsArray,
// internal/domain/search/service_test.go). spi.ConditionToFilter now pushes a
// wildcard array path down like any other (path-grammar.md §2/§8), so this
// no longer forces the untranslatable route specifically — see the test's
// own doc comment for what property this still pins.
func newDeleteStreamCtx(t *testing.T, factory spi.StoreFactory, ref spi.ModelRef) context.Context {
	t.Helper()
	ctx := spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID:   "delete-stream-user",
		UserName: "Delete Stream",
		Tenant:   spi.Tenant{ID: "delete-stream-tenant", Name: "DeleteStream"},
		Roles:    []string{"user"},
	})

	elem := schema.NewObjectNode()
	elem.SetChild("name", schema.NewLeafNode(schema.String))
	node := schema.NewObjectNode()
	node.SetChild("kind", schema.NewLeafNode(schema.String))
	node.SetChild("items", schema.NewArrayNode(elem))
	raw, err := schema.Marshal(node)
	if err != nil {
		t.Fatalf("schema.Marshal: %v", err)
	}
	modelStore, err := factory.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	if err := modelStore.Save(ctx, &spi.ModelDescriptor{Ref: ref, State: spi.ModelLocked, Schema: raw}); err != nil {
		t.Fatalf("ModelStore.Save: %v", err)
	}
	return ctx
}

// buildDeleteStreamHandler wires a Handler to factory/txMgr, mirroring
// buildDeleteBatchedHandler (service_delete_batched_test.go).
func buildDeleteStreamHandler(t *testing.T, factory spi.StoreFactory, txMgr spi.TransactionManager) *Handler {
	t.Helper()
	engine := wfengine.NewEngine(factory, common.NewDefaultUUIDGenerator(), txMgr)
	return New(factory, txMgr, common.NewDefaultUUIDGenerator(), engine, txgate.New())
}

// seedKind creates n entities of ref's model with {"kind": kind} and returns
// their ids in creation order.
func seedKind(t *testing.T, h *Handler, ctx context.Context, ref spi.ModelRef, n int, kind string) []string {
	t.Helper()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		res, err := h.CreateEntity(ctx, CreateEntityInput{
			EntityName:   ref.EntityName,
			ModelVersion: ref.ModelVersion,
			Format:       "JSON",
			Data:         json.RawMessage(fmt.Sprintf(`{"kind":%q}`, kind)),
		})
		if err != nil {
			t.Fatalf("CreateEntity[%d]: %v", i, err)
		}
		ids = append(ids, res.EntityIDs[0])
	}
	return ids
}

// TestDeleteEntitiesConditional_SingleTx_StreamsSelection is E4.1(a): a
// single-tx conditional delete of 5k matched / 5k decoys removes exactly the
// matched set (correct MatchedCount/RemovedCount, decoys survive), and the
// spy proves the selection never called Search or GetAll and streamed
// exactly the matched count of entities through the iterator (never more —
// no slice materialising every row up front for the caller to re-filter).
func TestDeleteEntitiesConditional_SingleTx_StreamsSelection(t *testing.T) {
	realFactory := memory.NewStoreFactory()
	t.Cleanup(func() { realFactory.Close() })
	ref := spi.ModelRef{EntityName: "StreamModel", ModelVersion: "1"}
	ctx := newDeleteStreamCtx(t, realFactory, ref)

	realStore, err := realFactory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	spy := &searchForbiddenStore{EntityStore: realStore, t: t}
	spyFactory := &searchForbiddenFactory{StoreFactory: realFactory, store: spy}

	txMgr := mustTxMgr(t, realFactory)
	h := buildDeleteStreamHandler(t, spyFactory, txMgr)

	const matched, decoys = 5000, 5000
	seedKind(t, h, ctx, ref, matched, "drop")
	decoyIDs := seedKind(t, h, ctx, ref, decoys, "keep")

	cond := []byte(`{"type":"simple","jsonPath":"$.kind","operatorType":"EQUALS","value":"drop"}`)
	result, err := h.DeleteEntitiesConditional(ctx, ref.EntityName, ref.ModelVersion, cond, nil, false, 0)
	if err != nil {
		t.Fatalf("DeleteEntitiesConditional: %v", err)
	}
	if result.MatchedCount != matched {
		t.Errorf("MatchedCount = %d, want %d", result.MatchedCount, matched)
	}
	if result.RemovedCount != matched {
		t.Errorf("RemovedCount = %d, want %d", result.RemovedCount, matched)
	}
	if got := spy.entityCallCount(); got != matched {
		t.Errorf("Entity() calls = %d, want exactly %d (pushdown filter must yield only matches, streamed one at a time)", got, matched)
	}

	for _, id := range decoyIDs {
		if _, gErr := realStore.Get(ctx, id); gErr != nil {
			t.Errorf("decoy %s should survive, got err %v", id, gErr)
		}
	}
}

// TestDeleteBatched_StreamsSelectionInCycles is E4.1(b): deleteBatched with
// batchSize 100 over 1k matches opens at most (matches/batch)+1 = 11
// iterators (one per streamed cycle, plus at most one confirming empty
// cycle — see deleteBatched's own doc comment), and commits are visible at
// batch granularity (more than one Begin/Commit pair, proving the work
// wasn't done as a single all-or-nothing transaction).
func TestDeleteBatched_StreamsSelectionInCycles(t *testing.T) {
	realFactory := memory.NewStoreFactory()
	t.Cleanup(func() { realFactory.Close() })
	ref := spi.ModelRef{EntityName: "StreamBatchModel", ModelVersion: "1"}
	ctx := newDeleteStreamCtx(t, realFactory, ref)

	realStore, err := realFactory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	spy := &searchForbiddenStore{EntityStore: realStore, t: t}
	spyFactory := &searchForbiddenFactory{StoreFactory: realFactory, store: spy}

	realTxMgr := mustTxMgr(t, realFactory)
	rtm := &recordingTxMgr{TransactionManager: realTxMgr}
	h := buildDeleteStreamHandler(t, spyFactory, rtm)

	const matched, batchSize = 1000, 100
	seedKind(t, h, ctx, ref, matched, "drop")

	beginsBefore := rtm.beginCount()

	cond := []byte(`{"type":"simple","jsonPath":"$.kind","operatorType":"EQUALS","value":"drop"}`)
	result, err := h.DeleteEntitiesConditional(ctx, ref.EntityName, ref.ModelVersion, cond, nil, false, batchSize)
	if err != nil {
		t.Fatalf("DeleteEntitiesConditional: %v", err)
	}
	if result.MatchedCount != matched {
		t.Errorf("MatchedCount = %d, want %d", result.MatchedCount, matched)
	}
	if result.RemovedCount != matched {
		t.Errorf("RemovedCount = %d, want %d", result.RemovedCount, matched)
	}

	wantMaxOpens := matched/batchSize + 1
	if got := spy.iterateOpenCount(); got > wantMaxOpens {
		t.Errorf("Iterate() opens = %d, want <= %d (matches/batch + 1)", got, wantMaxOpens)
	}
	if got := spy.iterateOpenCount(); got < matched/batchSize {
		t.Errorf("Iterate() opens = %d, want >= %d (at least one cycle per full batch)", got, matched/batchSize)
	}

	if begins := rtm.beginCount() - beginsBefore; begins < 2 {
		t.Errorf("Begin calls during delete = %d, want >=2 (batch granularity must be observable, not one all-or-nothing tx)", begins)
	}
}

// TestDeleteAllEntities_StreamsSelection is E4.1(c): DeleteAllEntities
// returns the correct TotalCount without ever calling GetAll — the spy
// asserts Iterate is used instead (its Search-forbidding branch is inert
// here since DeleteAllEntities has no condition to select by, but the
// GetAll-forbidding branch is exactly what this test drives).
func TestDeleteAllEntities_StreamsSelection(t *testing.T) {
	realFactory := memory.NewStoreFactory()
	t.Cleanup(func() { realFactory.Close() })
	ref := spi.ModelRef{EntityName: "StreamAllModel", ModelVersion: "1"}
	ctx := newDeleteStreamCtx(t, realFactory, ref)

	realStore, err := realFactory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	spy := &searchForbiddenStore{EntityStore: realStore, t: t}
	spyFactory := &searchForbiddenFactory{StoreFactory: realFactory, store: spy}

	txMgr := mustTxMgr(t, realFactory)
	h := buildDeleteStreamHandler(t, spyFactory, txMgr)

	const n = 250
	ids := seedKind(t, h, ctx, ref, n, "whatever")

	result, err := h.DeleteAllEntities(ctx, ref.EntityName, ref.ModelVersion)
	if err != nil {
		t.Fatalf("DeleteAllEntities: %v", err)
	}
	if result.TotalCount != n {
		t.Errorf("TotalCount = %d, want %d", result.TotalCount, n)
	}
	if spy.iterateOpenCount() < 1 {
		t.Error("Iterate() was never called; DeleteAllEntities must count via Iterate")
	}
	if got := spy.entityCallCount(); got != n {
		t.Errorf("Entity() calls = %d, want exactly %d", got, n)
	}

	for _, id := range ids {
		if _, gErr := realStore.Get(ctx, id); gErr == nil {
			t.Errorf("entity %s should be gone after DeleteAllEntities", id)
		}
	}
}

// TestDeleteEntitiesConditional_SingleTx_UntranslatableConditionStreams is
// E4.1(d): a condition carrying an array-wildcard leaf (mirroring search's
// own matchAllFixtureCondition fixture) still deletes correctly — decoys
// survive, only the matching entity is removed — via Iterate, never
// falling back to Search or GetAll.
//
// The condition is a wildcard array path, which once did not translate and
// drove the residual branch planDeleteSelection used to carry. It has
// translated since spi.ConditionToFilter learned to carry a subscript through
// (path-grammar.md §2/§8), and the residual branch is gone with the rest of
// the whole-model-scan family — so this hands Iterate a real pushdown filter.
// The property the test pins is unchanged and was never about which branch
// ran: streamed selection via Iterate, never Search, decoys survive, matches
// are removed.
func TestDeleteEntitiesConditional_SingleTx_WildcardPathConditionStreams(t *testing.T) {
	realFactory := memory.NewStoreFactory()
	t.Cleanup(func() { realFactory.Close() })
	ref := spi.ModelRef{EntityName: "StreamUntranslatableModel", ModelVersion: "1"}
	ctx := newDeleteStreamCtx(t, realFactory, ref)

	realStore, err := realFactory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	spy := &searchForbiddenStore{EntityStore: realStore, t: t}
	spyFactory := &searchForbiddenFactory{StoreFactory: realFactory, store: spy}

	txMgr := mustTxMgr(t, realFactory)
	h := buildDeleteStreamHandler(t, spyFactory, txMgr)

	dropIDs := seedKind(t, h, ctx, ref, 3, "drop")
	keepIDs := seedKind(t, h, ctx, ref, 3, "keep")

	// An array-wildcard leaf inside an OR, same shape as search's own
	// matchAllFixtureCondition test fixture — it now translates and pushes
	// down like any other path (path-grammar.md §2/§8), rather than forcing
	// planDeleteSelection's untranslatable branch. The wildcard disjunct
	// never matches these entities (no "items" field at all) either way, so
	// only the "kind" disjunct decides the match.
	cond := []byte(`{
		"type": "group",
		"operator": "OR",
		"conditions": [
			{"type":"simple","jsonPath":"$.kind","operatorType":"EQUALS","value":"drop"},
			{"type":"simple","jsonPath":"$.items[*].name","operatorType":"EQUALS","value":"never-present"}
		]
	}`)

	result, err := h.DeleteEntitiesConditional(ctx, ref.EntityName, ref.ModelVersion, cond, nil, false, 0)
	if err != nil {
		t.Fatalf("DeleteEntitiesConditional: %v", err)
	}
	if result.MatchedCount != 3 {
		t.Errorf("MatchedCount = %d, want 3", result.MatchedCount)
	}
	if result.RemovedCount != 3 {
		t.Errorf("RemovedCount = %d, want 3", result.RemovedCount)
	}

	for _, id := range dropIDs {
		if _, gErr := realStore.Get(ctx, id); gErr == nil {
			t.Errorf("matched entity %s should be gone", id)
		}
	}
	for _, id := range keepIDs {
		if _, gErr := realStore.Get(ctx, id); gErr != nil {
			t.Errorf("decoy %s should survive, got err %v", id, gErr)
		}
	}
	if spy.iterateOpenCount() < 1 {
		t.Error("Iterate() was never called; the untranslatable-condition path must still stream via Iterate")
	}
}
