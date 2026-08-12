package entity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// afterNthCommitTxMgr wraps a real spi.TransactionManager and runs `after`
// synchronously immediately once the Nth Commit call has genuinely succeeded
// against the real backend — i.e. strictly after that transaction's writes
// are durable, never mid-commit. Lets a test inject a deterministic
// out-of-band mutation between two phases of a multi-tx flow (e.g. "after the
// resolution tx commits, before any batch begins") without racing a
// wall-clock sleep.
type afterNthCommitTxMgr struct {
	spi.TransactionManager
	mu      sync.Mutex
	commits int
	n       int
	after   func()
}

func (m *afterNthCommitTxMgr) Commit(ctx context.Context, txID string) error {
	if err := m.TransactionManager.Commit(ctx, txID); err != nil {
		return err
	}
	m.mu.Lock()
	m.commits++
	fire := m.commits == m.n
	m.mu.Unlock()
	if fire && m.after != nil {
		m.after()
	}
	return nil
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
// IDToError, and the batch granularity is externally observable as more than
// one Begin/Commit pair beyond the resolution tx (1 resolution + 3 batches of
// 2,2,1 = >=4 Begin calls total).
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

	if begins := rtm.beginCount() - beginsBefore; begins < 4 {
		t.Errorf("Begin calls during delete = %d, want >=4 (1 resolution + >=3 batches: batch granularity must be observable)", begins)
	}
}

// TestDeleteEntitiesConditional_Batched_VersionGuard pins spec D4's version
// guard: entity #3 (0-based index 2 of 4) is mutated by a completely separate
// operation immediately after the resolution tx commits (and therefore
// before any batch begins) — deterministic via afterNthCommitTxMgr, never a
// wall-clock race. The mutated entity must survive the delete (its baseline
// version no longer matches when the batch containing it re-reads it), land
// in IDToError, and still carry its NEW payload afterward — proof the delete
// was skipped, not merely reported as skipped while actually racing through.
func TestDeleteEntitiesConditional_Batched_VersionGuard(t *testing.T) {
	realFactory := memory.NewStoreFactory()
	t.Cleanup(func() { realFactory.Close() })
	realTxMgr := mustTxMgr(t, realFactory)

	ctx := newDeleteBatchedCtx(t, realFactory)
	hSeed := buildDeleteBatchedHandler(t, realFactory, realTxMgr)
	ids := seedPersons(t, hSeed, ctx, 4)

	afterMgr := &afterNthCommitTxMgr{TransactionManager: realTxMgr, n: 1}
	afterMgr.after = func() {
		if _, err := hSeed.UpdateEntity(ctx, UpdateEntityInput{
			EntityID: ids[2],
			Format:   "JSON",
			Data:     json.RawMessage(`{"name":"P","age":999}`),
		}); err != nil {
			t.Errorf("mid-flight mutation of ids[2] failed: %v", err)
		}
	}

	hDelete := buildDeleteBatchedHandler(t, realFactory, afterMgr)

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
// (recorded-commit index 3: 1 resolution + batch1 + THIS batch) is forced to
// fail. Its ids all land in IDToError with no RemovedCount credit, while the
// first and third batches commit normally — proving later batches are still
// attempted after a batch failure, not abandoned.
func TestDeleteEntitiesConditional_Batched_FailedBatchContinues(t *testing.T) {
	realFactory := memory.NewStoreFactory()
	t.Cleanup(func() { realFactory.Close() })
	realTxMgr := mustTxMgr(t, realFactory)

	ctx := newDeleteBatchedCtx(t, realFactory)
	hSeed := buildDeleteBatchedHandler(t, realFactory, realTxMgr)
	ids := seedPersons(t, hSeed, ctx, 6)

	failMgr := &failNthCommitTxMgr{TransactionManager: realTxMgr, failOn: map[int]error{3: errors.New("commit boom")}}
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
