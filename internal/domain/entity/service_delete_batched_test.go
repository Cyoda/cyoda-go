package entity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	wfengine "github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// --- Task 10: batched, version-guarded DeleteEntitiesConditional ---

// batchDeleteAgeGEZero matches every seeded entity (age is always >= 0), so
// it exercises the search-selection branch of deleteBatched (as opposed to
// the empty-condition GetAll branch).
var batchDeleteAgeGEZero = []byte(`{"type":"simple","jsonPath":"$.age","operatorType":"GREATER_OR_EQUAL","value":0}`)

// deleteBatchedUserCtx builds a bare UserContext-scoped ctx (no model
// registration) — factored out of newDeleteBatchedCtx so a test that needs a
// tenant-resolvable ctx BEFORE the model exists (e.g. to construct a spy
// EntityStore straight off the raw factory) can get one without duplicating
// the UserContext literal.
func deleteBatchedUserCtx() context.Context {
	return spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID:   "delete-batched-user",
		UserName: "Delete Batched",
		Tenant:   spi.Tenant{ID: "delete-batched-tenant", Name: "DeleteBatched"},
		Roles:    []string{"user"},
	})
}

// newDeleteBatchedCtx builds a UserContext-scoped ctx and registers the
// "Person" model ({name: String, age: Integer}) locked, against factory. Call
// once per test; buildDeleteBatchedHandler may then build multiple Handlers
// against the same factory/ctx with different txMgrs (e.g. one to seed data
// with a plain txMgr, another to drive the delete with an instrumented one).
func newDeleteBatchedCtx(t *testing.T, factory spi.StoreFactory) context.Context {
	t.Helper()
	ctx := deleteBatchedUserCtx()

	node := schema.NewObjectNode()
	node.SetChild("name", schema.NewLeafNode(schema.String))
	node.SetChild("age", schema.NewLeafNode(schema.Integer))
	raw, err := schema.Marshal(node)
	if err != nil {
		t.Fatalf("schema.Marshal: %v", err)
	}

	modelStore, err := factory.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	if err := modelStore.Save(ctx, &spi.ModelDescriptor{
		Ref:    spi.ModelRef{EntityName: "Person", ModelVersion: "1"},
		State:  spi.ModelLocked,
		Schema: raw,
	}); err != nil {
		t.Fatalf("ModelStore.Save: %v", err)
	}
	return ctx
}

// buildDeleteBatchedHandler wires a Handler to factory/txMgr with a real
// search.SearchService, so condition-based deletes exercise the production
// selection path rather than a stub.
func buildDeleteBatchedHandler(t *testing.T, factory spi.StoreFactory, txMgr spi.TransactionManager) *Handler {
	t.Helper()
	bg := context.Background()
	searchStore, err := factory.AsyncSearchStore(bg)
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	searchSvc := search.NewSearchService(factory, common.NewDefaultUUIDGenerator(), searchStore)
	engine := wfengine.NewEngine(factory, common.NewDefaultUUIDGenerator(), txMgr)
	return New(factory, txMgr, common.NewDefaultUUIDGenerator(), engine, txgate.New(), searchSvc)
}

// seedPersons creates n Person entities (ages 0..n-1) via h and returns their
// ids in creation order.
func seedPersons(t *testing.T, h *Handler, ctx context.Context, n int) []string {
	t.Helper()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		res, err := h.CreateEntity(ctx, CreateEntityInput{
			EntityName:   "Person",
			ModelVersion: "1",
			Format:       "JSON",
			Data:         json.RawMessage(fmt.Sprintf(`{"name":"P","age":%d}`, i)),
		})
		if err != nil {
			t.Fatalf("CreateEntity[%d]: %v", i, err)
		}
		ids = append(ids, res.EntityIDs[0])
	}
	return ids
}

// failNthCommitTxMgr wraps a real spi.TransactionManager and fails the Nth
// Commit call (1-based, across the wrapped manager's whole lifetime) with the
// configured error instead of delegating — simulating a commit failure (e.g.
// a conflict at the storage layer) on exactly one transaction in a
// multi-batch sequence, so the surrounding batches' real behavior can be
// pinned deterministically.
type failNthCommitTxMgr struct {
	spi.TransactionManager
	mu      sync.Mutex
	commits int
	failOn  map[int]error
}

func (m *failNthCommitTxMgr) Commit(ctx context.Context, txID string) error {
	m.mu.Lock()
	m.commits++
	idx := m.commits
	m.mu.Unlock()
	if err, ok := m.failOn[idx]; ok {
		return err
	}
	return m.TransactionManager.Commit(ctx, txID)
}

// deleteAllSpyStore wraps a real spi.EntityStore and records whether DeleteAll
// was ever called, so a test can assert the batched delete-all path (spec D4)
// enumerates and deletes individually rather than falling back to the
// single-tx DeleteAll fast path.
type deleteAllSpyStore struct {
	spi.EntityStore
	mu     sync.Mutex
	called bool
}

func (s *deleteAllSpyStore) DeleteAll(ctx context.Context, ref spi.ModelRef) error {
	s.mu.Lock()
	s.called = true
	s.mu.Unlock()
	return s.EntityStore.DeleteAll(ctx, ref)
}

func (s *deleteAllSpyStore) wasCalled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.called
}

// Iterate passes through to the embedded real store. spi.EntityStore is
// embedded by interface value, which does NOT promote spi.Iterable's
// Iterate method onto *deleteAllSpyStore even though the underlying
// concrete store implements it — an explicit passthrough is required so
// deleteBatched's own entityStore.(spi.Iterable) capability check succeeds
// against this spy the same way it does against the real store.
func (s *deleteAllSpyStore) Iterate(ctx context.Context, model spi.ModelRef, filter spi.Filter, opts spi.IterateOptions) (spi.Iterator, error) {
	return s.EntityStore.(spi.Iterable).Iterate(ctx, model, filter, opts)
}

// mutateAfterSelectionStore wraps a real spi.EntityStore and, on the
// Close() of whichever streamed selection cycle's Iterate() yielded
// targetID, synchronously runs mutate — AFTER that cycle's iterator (and so
// its baseline-version capture, see deleteBatched's streamed-branch doc
// comment) has closed, but BEFORE deleteBatched hands the chunk to
// deleteOneBatch, which begins its OWN fresh transaction and only then
// re-reads targetID. That ordering matters: firing the mutation any later —
// e.g. from inside deleteOneBatch's own Get(targetID) call, as a naive hook
// would — lands the write while deleteOneBatch's chunk transaction is
// already open and has already read targetID, which trips first-committer-
// wins and CONFLICTs the whole chunk's commit (rolling back every id in it,
// not just targetID) instead of cleanly exercising the per-id version
// guard this test exists to pin. Tracking yielded ids per iterator instance
// (rather than hooking Get by id) also sidesteps depending on which
// streamed cycle happens to contain targetID — unspecified per the
// spi.Iterable contract.
type mutateAfterSelectionStore struct {
	spi.EntityStore
	mu       sync.Mutex
	fired    bool
	targetID string
	mutate   func()
}

func (s *mutateAfterSelectionStore) Iterate(ctx context.Context, model spi.ModelRef, filter spi.Filter, opts spi.IterateOptions) (spi.Iterator, error) {
	it, err := s.EntityStore.(spi.Iterable).Iterate(ctx, model, filter, opts)
	if err != nil {
		return nil, err
	}
	return &mutateAfterSelectionIterator{Iterator: it, store: s}, nil
}

type mutateAfterSelectionIterator struct {
	spi.Iterator
	store       *mutateAfterSelectionStore
	sawTargetID bool
}

func (it *mutateAfterSelectionIterator) Next() bool {
	ok := it.Iterator.Next()
	if ok && it.Iterator.Entity().Meta.ID == it.store.targetID {
		it.sawTargetID = true
	}
	return ok
}

func (it *mutateAfterSelectionIterator) Close() error {
	err := it.Iterator.Close()
	if it.sawTargetID {
		it.store.mu.Lock()
		fire := !it.store.fired
		it.store.fired = true
		it.store.mu.Unlock()
		if fire {
			it.store.mutate()
		}
	}
	return err
}

// mutateAfterSelectionFactory wraps a real spi.StoreFactory but always
// hands out the given mutateAfterSelectionStore from EntityStore(); every
// other accessor delegates to the wrapped factory unchanged.
type mutateAfterSelectionFactory struct {
	spi.StoreFactory
	store *mutateAfterSelectionStore
}

func (f *mutateAfterSelectionFactory) EntityStore(_ context.Context) (spi.EntityStore, error) {
	return f.store, nil
}

// deleteAllSpyFactory wraps a real spi.StoreFactory but always hands out the
// given deleteAllSpyStore from EntityStore(); every other accessor delegates
// to the wrapped factory unchanged.
type deleteAllSpyFactory struct {
	spi.StoreFactory
	store *deleteAllSpyStore
}

func (f *deleteAllSpyFactory) EntityStore(_ context.Context) (spi.EntityStore, error) {
	return f.store, nil
}

// TestDeleteEntitiesConditional_Batched_HappyPath pins the batched path's
// basic shape: 5 matching entities, batchSize=2 removes all 5 with an empty
// IDToError. Since #472 the selection streams via a fresh spi.Iterable
// iterator per cycle instead of one upfront resolution transaction — a live
// re-scan naturally shrinks as each cycle's deletes land, so there is no
// separate resolution Begin/Commit any more, just one Begin per batch (3
// batches of 2,2,1 = exactly 3 Begin calls).
func TestDeleteEntitiesConditional_Batched_HappyPath(t *testing.T) {
	realFactory := memory.NewStoreFactory()
	t.Cleanup(func() { realFactory.Close() })
	realTxMgr := mustTxMgr(t, realFactory)
	rtm := &recordingTxMgr{TransactionManager: realTxMgr}

	ctx := newDeleteBatchedCtx(t, realFactory)
	h := buildDeleteBatchedHandler(t, realFactory, rtm)
	seedPersons(t, h, ctx, 5)

	beginsBefore := rtm.beginCount()

	result, err := h.DeleteEntitiesConditional(ctx, "Person", "1", batchDeleteAgeGEZero, nil, false, 2)
	if err != nil {
		t.Fatalf("DeleteEntitiesConditional: %v", err)
	}
	if result.MatchedCount != 5 {
		t.Errorf("MatchedCount = %d, want 5", result.MatchedCount)
	}
	if result.RemovedCount != 5 {
		t.Errorf("RemovedCount = %d, want 5", result.RemovedCount)
	}
	if len(result.IDToError) != 0 {
		t.Errorf("IDToError = %v, want empty", result.IDToError)
	}

	if begins := rtm.beginCount() - beginsBefore; begins < 3 {
		t.Errorf("Begin calls during delete = %d, want >=3 (one per batch: batch granularity must be observable)", begins)
	}
}

// TestDeleteEntitiesConditional_Batched_VersionGuard pins spec D4's version
// guard: the entity a completely separate operation mutates concurrently
// must survive the delete (its baseline version no longer matches when
// deleteOneBatch re-reads it), land in IDToError, and still carry its NEW
// payload afterward — proof the delete was skipped, not merely reported as
// skipped while actually racing through.
//
// mutateAfterSelectionStore (see its own doc comment) hooks the mutation to
// fire right after the streamed cycle that selected the target id closes
// its iterator — after that cycle's baseline-version capture, before
// deleteOneBatch opens its own transaction and re-reads it — rather than a
// transaction-count-based hook like the pre-#472 afterNthCommitTxMgr: the
// streamed selection design (#472) no longer has a distinct "resolution"
// transaction to count from.
func TestDeleteEntitiesConditional_Batched_VersionGuard(t *testing.T) {
	realFactory := memory.NewStoreFactory()
	t.Cleanup(func() { realFactory.Close() })
	realTxMgr := mustTxMgr(t, realFactory)

	ctx := newDeleteBatchedCtx(t, realFactory)
	hSeed := buildDeleteBatchedHandler(t, realFactory, realTxMgr)
	ids := seedPersons(t, hSeed, ctx, 4)

	realStoreForMutate, err := realFactory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	spyStore := &mutateAfterSelectionStore{EntityStore: realStoreForMutate, targetID: ids[2]}
	spyStore.mutate = func() {
		if _, err := hSeed.UpdateEntity(ctx, UpdateEntityInput{
			EntityID: ids[2],
			Format:   "JSON",
			Data:     json.RawMessage(`{"name":"P","age":999}`),
		}); err != nil {
			t.Errorf("mid-flight mutation of ids[2] failed: %v", err)
		}
	}
	spyFactory := &mutateAfterSelectionFactory{StoreFactory: realFactory, store: spyStore}

	hDelete := buildDeleteBatchedHandler(t, spyFactory, realTxMgr)

	result, err := hDelete.DeleteEntitiesConditional(ctx, "Person", "1", batchDeleteAgeGEZero, nil, false, 2)
	if err != nil {
		t.Fatalf("DeleteEntitiesConditional: %v", err)
	}

	if result.MatchedCount != 4 {
		t.Errorf("MatchedCount = %d, want 4", result.MatchedCount)
	}
	if result.RemovedCount != 3 {
		t.Errorf("RemovedCount = %d, want 3 (the mutated id must not be counted removed)", result.RemovedCount)
	}
	if _, ok := result.IDToError[ids[2]]; !ok {
		t.Errorf("IDToError missing entry for the mutated id %s: %v", ids[2], result.IDToError)
	}

	realStore, err := realFactory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	e, err := realStore.Get(ctx, ids[2])
	if err != nil {
		t.Fatalf("mutated entity must still exist after the guarded delete: %v", err)
	}
	if !bytes.Contains(e.Data, []byte("999")) {
		t.Errorf("mutated entity payload = %s, want it to still carry the mid-flight mutation (age:999)", e.Data)
	}
}

// TestDeleteEntitiesConditional_Batched_FailedBatchContinues pins that a
// single batch's commit failure isolates to that batch's ids: 6 matching
// entities, batchSize=2 means 3 batches; the middle batch's commit
// (recorded-commit index 2 — the streamed selection design (#472) has no
// separate resolution transaction any more, so commit indices run
// batch1, batch2, batch3) is forced to fail. Its ids all land in IDToError
// with no RemovedCount credit, while the first and third batches commit
// normally — proving later batches are still attempted after a batch
// failure, not abandoned. The failed batch's ids stay live and would
// otherwise resurface on a later cycle's re-scan; deleteBatched's seen set
// is what keeps them from being double-counted (see its own doc comment).
func TestDeleteEntitiesConditional_Batched_FailedBatchContinues(t *testing.T) {
	realFactory := memory.NewStoreFactory()
	t.Cleanup(func() { realFactory.Close() })
	realTxMgr := mustTxMgr(t, realFactory)

	ctx := newDeleteBatchedCtx(t, realFactory)
	hSeed := buildDeleteBatchedHandler(t, realFactory, realTxMgr)
	ids := seedPersons(t, hSeed, ctx, 6)

	failMgr := &failNthCommitTxMgr{TransactionManager: realTxMgr, failOn: map[int]error{2: errors.New("commit boom")}}
	hDelete := buildDeleteBatchedHandler(t, realFactory, failMgr)

	result, err := hDelete.DeleteEntitiesConditional(ctx, "Person", "1", batchDeleteAgeGEZero, nil, false, 2)
	if err != nil {
		t.Fatalf("DeleteEntitiesConditional: %v", err)
	}

	if result.MatchedCount != 6 {
		t.Errorf("MatchedCount = %d, want 6", result.MatchedCount)
	}
	if result.RemovedCount != 4 {
		t.Errorf("RemovedCount = %d, want 4 (batches 1 and 3, 2 ids each; batch 2's commit failed)", result.RemovedCount)
	}
	if len(result.IDToError) != 2 {
		t.Errorf("IDToError = %v, want exactly 2 entries (the failed batch's ids)", result.IDToError)
	}

	realStore, err := realFactory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	removed := 0
	for _, id := range ids {
		if _, gErr := realStore.Get(ctx, id); errors.Is(gErr, spi.ErrNotFound) {
			removed++
		}
	}
	if removed != 4 {
		t.Errorf("durably removed count = %d, want 4 (the failed batch's ids must still exist)", removed)
	}
}

// TestDeleteEntitiesConditional_Batched_FailedBatchIsTicketed pins that a
// batch commit failure classified as an Internal-level AppError (a bare
// commit error carries no sentinel, so common.Internal routes it to a 500,
// not the retryable-409 branch) is folded into IDToError as a ticketed,
// client-safe message — never the raw cause — and the cause is logged at
// ERROR under that same ticket so it is recoverable server-side. Mirrors
// TestDeleteEntitiesConditional_Batched_FailedBatchContinues's batch shape;
// that test's own assertions (count only, no message-content checks) must
// keep passing unchanged since this fix does not change IDToError's size or
// which ids land in it, only what the message text looks like.
func TestDeleteEntitiesConditional_Batched_FailedBatchIsTicketed(t *testing.T) {
	buf := captureEntitySlog(t)

	realFactory := memory.NewStoreFactory()
	t.Cleanup(func() { realFactory.Close() })
	realTxMgr := mustTxMgr(t, realFactory)

	ctx := newDeleteBatchedCtx(t, realFactory)
	hSeed := buildDeleteBatchedHandler(t, realFactory, realTxMgr)
	ids := seedPersons(t, hSeed, ctx, 6)

	const cause = "commit boom: ERROR: canceling statement due to statement timeout (SQLSTATE 57014)"
	failMgr := &failNthCommitTxMgr{TransactionManager: realTxMgr, failOn: map[int]error{2: errors.New(cause)}}
	hDelete := buildDeleteBatchedHandler(t, realFactory, failMgr)

	result, err := hDelete.DeleteEntitiesConditional(ctx, "Person", "1", batchDeleteAgeGEZero, nil, false, 2)
	if err != nil {
		t.Fatalf("DeleteEntitiesConditional: %v", err)
	}
	if result.RemovedCount != 4 {
		t.Errorf("RemovedCount = %d, want 4 (batches 1 and 3, 2 ids each; batch 2's commit failed)", result.RemovedCount)
	}
	if len(result.IDToError) != 2 {
		t.Fatalf("IDToError = %v, want exactly 2 entries (the failed batch's ids)", result.IDToError)
	}

	for id, msg := range result.IDToError {
		if !strings.Contains(msg, "[ticket: ") {
			t.Errorf("IDToError[%s] = %q, want a ticketed message like perIDDeleteError's internal branch", id, msg)
		}
		if strings.Contains(msg, "commit boom") || strings.Contains(msg, "SQLSTATE") {
			t.Errorf("IDToError[%s] = %q, leaks the raw commit cause onto the wire", id, msg)
		}
		if !strings.Contains(msg, common.ErrCodeServerError) {
			t.Errorf("IDToError[%s] = %q, want the SERVER_ERROR code", id, msg)
		}
	}

	// Every id in the failed batch shares the SAME ticket — one ticket per
	// batch failure, not one per id (they all share one underlying cause).
	var tickets []string
	for _, msg := range result.IDToError {
		if i := strings.Index(msg, "[ticket: "); i >= 0 {
			tickets = append(tickets, msg[i:])
		}
	}
	if len(tickets) == 2 && tickets[0] != tickets[1] {
		t.Errorf("failed batch's two ids carry different tickets, want one ticket shared across the batch: %v", tickets)
	}

	logged := buf.String()
	if !strings.Contains(logged, "commit boom") || !strings.Contains(logged, "57014") {
		t.Errorf("the commit cause was sanitized out of the response but never logged, so it is lost: %s", logged)
	}

	// Confirm the durable state matches TestDeleteEntitiesConditional_Batched_FailedBatchContinues:
	// the failed batch's ids must still exist.
	realStore, err := realFactory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	removed := 0
	for _, id := range ids {
		if _, gErr := realStore.Get(ctx, id); errors.Is(gErr, spi.ErrNotFound) {
			removed++
		}
	}
	if removed != 4 {
		t.Errorf("durably removed count = %d, want 4 (the failed batch's ids must still exist)", removed)
	}
}

// TestDeleteEntitiesConditional_Batched_DeleteAllPath pins that an empty
// condition with batchSize>0 still goes through the batched
// enumerate-then-delete path (spec D4), NOT the single-tx DeleteAll fast
// path: the spy store asserts EntityStore.DeleteAll is never invoked, yet all
// 5 entities are individually removed.
func TestDeleteEntitiesConditional_Batched_DeleteAllPath(t *testing.T) {
	realFactory := memory.NewStoreFactory()
	t.Cleanup(func() { realFactory.Close() })

	realStore, err := realFactory.EntityStore(deleteBatchedUserCtx())
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	spyStore := &deleteAllSpyStore{EntityStore: realStore}
	spyFactory := &deleteAllSpyFactory{StoreFactory: realFactory, store: spyStore}

	txMgr := mustTxMgr(t, realFactory)
	ctx := newDeleteBatchedCtx(t, spyFactory)
	h := buildDeleteBatchedHandler(t, spyFactory, txMgr)
	ids := seedPersons(t, h, ctx, 5)

	result, err := h.DeleteEntitiesConditional(ctx, "Person", "1", nil, nil, false, 2)
	if err != nil {
		t.Fatalf("DeleteEntitiesConditional: %v", err)
	}
	if result.MatchedCount != 5 {
		t.Errorf("MatchedCount = %d, want 5", result.MatchedCount)
	}
	if result.RemovedCount != 5 {
		t.Errorf("RemovedCount = %d, want 5", result.RemovedCount)
	}
	if len(result.IDToError) != 0 {
		t.Errorf("IDToError = %v, want empty", result.IDToError)
	}
	if spyStore.wasCalled() {
		t.Error("EntityStore.DeleteAll must NOT be called on the batched (transactionSize>0) delete-all path")
	}

	for _, id := range ids {
		if _, gErr := realStore.Get(ctx, id); !errors.Is(gErr, spi.ErrNotFound) {
			t.Errorf("entity %s still exists after the batched delete-all", id)
		}
	}
}

// TestDeleteEntitiesConditional_NoBatchSize_Unchanged pins that batchSize<=0
// keeps today's single-tx behavior byte-for-byte: exactly one Begin/Commit
// pair, and the empty-condition fast path (EntityStore.DeleteAll) is used —
// the mirror image of TestDeleteEntitiesConditional_Batched_DeleteAllPath.
func TestDeleteEntitiesConditional_NoBatchSize_Unchanged(t *testing.T) {
	realFactory := memory.NewStoreFactory()
	t.Cleanup(func() { realFactory.Close() })

	realStore, err := realFactory.EntityStore(deleteBatchedUserCtx())
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	spyStore := &deleteAllSpyStore{EntityStore: realStore}
	spyFactory := &deleteAllSpyFactory{StoreFactory: realFactory, store: spyStore}

	realTxMgr := mustTxMgr(t, realFactory)
	rtm := &recordingTxMgr{TransactionManager: realTxMgr}

	ctx := newDeleteBatchedCtx(t, spyFactory)
	h := buildDeleteBatchedHandler(t, spyFactory, rtm)
	ids := seedPersons(t, h, ctx, 3)

	beginsBefore := rtm.beginCount()

	result, err := h.DeleteEntitiesConditional(ctx, "Person", "1", nil, nil, false, 0)
	if err != nil {
		t.Fatalf("DeleteEntitiesConditional: %v", err)
	}
	if result.MatchedCount != 3 {
		t.Errorf("MatchedCount = %d, want 3", result.MatchedCount)
	}
	if result.RemovedCount != 3 {
		t.Errorf("RemovedCount = %d, want 3", result.RemovedCount)
	}

	if begins := rtm.beginCount() - beginsBefore; begins != 1 {
		t.Errorf("Begin calls = %d, want exactly 1 (single-tx path, unchanged)", begins)
	}
	if !spyStore.wasCalled() {
		t.Error("EntityStore.DeleteAll must be called on the batchSize<=0 empty-condition fast path")
	}

	for _, id := range ids {
		if _, gErr := realStore.Get(ctx, id); !errors.Is(gErr, spi.ErrNotFound) {
			t.Errorf("entity %s still exists after delete-all", id)
		}
	}
}
