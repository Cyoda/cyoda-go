package search_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// helper: create a context with a UserContext for the given tenant.
func tenantCtx(tenantID string) context.Context {
	return spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID:   "test-user",
		UserName: "Test User",
		Tenant: spi.Tenant{
			ID:   spi.TenantID(tenantID),
			Name: "Test Tenant",
		},
		Roles: []string{"ROLE_USER"},
	})
}

// helper: save an entity with JSON data, return its ID.
func saveEntity(t *testing.T, ctx context.Context, factory *memory.StoreFactory, modelRef spi.ModelRef, id string, data []byte) {
	t.Helper()
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	_, err = store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{
			ID:       id,
			ModelRef: modelRef,
			State:    "NEW",
		},
		Data: data,
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
}

func TestDirectSearchSimpleEquals(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := factory.AsyncSearchStore(context.Background())
	svc := search.NewSearchService(factory, uuids, searchStore)

	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}

	saveEntity(t, ctx, factory, ref, "e1", []byte(`{"name":"Alice","age":30}`))
	saveEntity(t, ctx, factory, ref, "e2", []byte(`{"name":"Bob","age":25}`))
	saveEntity(t, ctx, factory, ref, "e3", []byte(`{"name":"Alice","age":40}`))

	cond := &predicate.SimpleCondition{
		JsonPath:     "$.name",
		OperatorType: "EQUALS",
		Value:        "Alice",
	}

	results, err := svc.Search(ctx, ref, cond, search.SearchOptions{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	// Verify the matched entities are Alice
	for _, e := range results {
		if e.Meta.ID != "e1" && e.Meta.ID != "e3" {
			t.Errorf("unexpected entity ID: %s", e.Meta.ID)
		}
	}
}

func TestDirectSearchNoMatches(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := factory.AsyncSearchStore(context.Background())
	svc := search.NewSearchService(factory, uuids, searchStore)

	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}

	saveEntity(t, ctx, factory, ref, "e1", []byte(`{"name":"Alice"}`))

	cond := &predicate.SimpleCondition{
		JsonPath:     "$.name",
		OperatorType: "EQUALS",
		Value:        "Nobody",
	}

	results, err := svc.Search(ctx, ref, cond, search.SearchOptions{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results, got %d", len(results))
	}
}

func TestDirectSearchPointInTime(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := factory.AsyncSearchStore(context.Background())
	svc := search.NewSearchService(factory, uuids, searchStore)

	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}

	// Save original
	saveEntity(t, ctx, factory, ref, "e1", []byte(`{"name":"Alice"}`))

	snapshot := time.Now()
	time.Sleep(2 * time.Millisecond) // ensure time progresses

	// Update entity
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{
			ID:       "e1",
			ModelRef: ref,
			State:    "UPDATED",
		},
		Data: []byte(`{"name":"Bob"}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Search at old timestamp should find "Alice"
	cond := &predicate.SimpleCondition{
		JsonPath:     "$.name",
		OperatorType: "EQUALS",
		Value:        "Alice",
	}
	pit := snapshot
	results, err := svc.Search(ctx, ref, cond, search.SearchOptions{PointInTime: &pit})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result at point-in-time, got %d", len(results))
	}
	if results[0].Meta.ID != "e1" {
		t.Errorf("expected e1, got %s", results[0].Meta.ID)
	}

	// Search at current time for "Alice" should find nothing (entity is now "Bob")
	results, err = svc.Search(ctx, ref, cond, search.SearchOptions{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results for Alice at current time, got %d", len(results))
	}
}

func TestDirectSearchPagination(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := factory.AsyncSearchStore(context.Background())
	svc := search.NewSearchService(factory, uuids, searchStore)

	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "item", ModelVersion: "1"}

	for i := 0; i < 5; i++ {
		saveEntity(t, ctx, factory, ref,
			fmt.Sprintf("e%d", i),
			[]byte(fmt.Sprintf(`{"val":%d}`, i)),
		)
	}

	// Match all with a condition that always matches (val > -1)
	cond := &predicate.SimpleCondition{
		JsonPath:     "$.val",
		OperatorType: "GREATER_THAN",
		Value:        float64(-1),
	}

	// No pagination: should get all 5
	all, err := svc.Search(ctx, ref, cond, search.SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 5 {
		t.Fatalf("expected 5, got %d", len(all))
	}

	// Limit=2, Offset=2: should get 2 results
	page, err := svc.Search(ctx, ref, cond, search.SearchOptions{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 {
		t.Fatalf("expected 2 results with limit=2,offset=2, got %d", len(page))
	}

	// Offset=4, Limit=10: should get 1 result (only 5 total)
	tail, err := svc.Search(ctx, ref, cond, search.SearchOptions{Limit: 10, Offset: 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) != 1 {
		t.Fatalf("expected 1 result with offset=4, got %d", len(tail))
	}
}

func TestAsyncLifecycle(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := factory.AsyncSearchStore(context.Background())
	svc := search.NewSearchService(factory, uuids, searchStore)

	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}

	saveEntity(t, ctx, factory, ref, "e1", []byte(`{"name":"Alice"}`))
	saveEntity(t, ctx, factory, ref, "e2", []byte(`{"name":"Bob"}`))

	cond := &predicate.SimpleCondition{
		JsonPath:     "$.name",
		OperatorType: "EQUALS",
		Value:        "Alice",
	}

	jobID, err := svc.SubmitAsync(ctx, ref, cond, search.SearchOptions{})
	if err != nil {
		t.Fatalf("SubmitAsync: %v", err)
	}
	if jobID == "" {
		t.Fatal("expected non-empty job ID")
	}

	// Poll until SUCCESSFUL (with timeout)
	deadline := time.Now().Add(5 * time.Second)
	var status search.SearchJobStatus
	for time.Now().Before(deadline) {
		status, err = svc.GetAsyncStatus(ctx, jobID)
		if err != nil {
			t.Fatalf("GetAsyncStatus: %v", err)
		}
		if status.Status == "SUCCESSFUL" || status.Status == "FAILED" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if status.Status != "SUCCESSFUL" {
		t.Fatalf("expected SUCCESSFUL, got %s", status.Status)
	}
	if status.FinishTime == nil {
		t.Fatal("expected non-nil finish time")
	}

	page, err := svc.GetAsyncResults(ctx, jobID, search.ResultOptions{})
	if err != nil {
		t.Fatalf("GetAsyncResults: %v", err)
	}
	if len(page.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(page.Results))
	}
	if page.Results[0].Meta.ID != "e1" {
		t.Errorf("expected e1, got %s", page.Results[0].Meta.ID)
	}
	if page.Total != 1 {
		t.Errorf("expected total=1, got %d", page.Total)
	}
}

func TestAsyncCancel(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := factory.AsyncSearchStore(context.Background())
	svc := search.NewSearchService(factory, uuids, searchStore)

	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}

	// Create many entities to increase chance the goroutine is still running
	for i := 0; i < 100; i++ {
		saveEntity(t, ctx, factory, ref,
			fmt.Sprintf("e%d", i),
			[]byte(fmt.Sprintf(`{"name":"entity-%d"}`, i)),
		)
	}

	cond := &predicate.SimpleCondition{
		JsonPath:     "$.name",
		OperatorType: "EQUALS",
		Value:        "entity-0",
	}

	jobID, err := svc.SubmitAsync(ctx, ref, cond, search.SearchOptions{})
	if err != nil {
		t.Fatalf("SubmitAsync: %v", err)
	}

	// Cancel immediately
	result, err := svc.CancelAsync(ctx, jobID)
	if err != nil {
		t.Fatalf("CancelAsync: %v", err)
	}

	// The job might already be done (it's fast), so cancellation may or may not succeed
	// But we should at least be able to get the status without error
	status, err := svc.GetAsyncStatus(ctx, jobID)
	if err != nil {
		t.Fatalf("GetAsyncStatus after cancel: %v", err)
	}
	if result.Cancelled {
		if status.Status != "CANCELLED" {
			t.Errorf("expected CANCELLED status after successful cancel, got %s", status.Status)
		}
		if result.CurrentStatus != "CANCELLED" {
			t.Errorf("expected CancelResult.CurrentStatus=CANCELLED, got %s", result.CurrentStatus)
		}
	} else {
		// Job completed before cancel — CurrentStatus should reflect that
		if result.CurrentStatus != "SUCCESSFUL" && result.CurrentStatus != "FAILED" {
			t.Errorf("expected SUCCESSFUL or FAILED for non-cancelled job, got %s", result.CurrentStatus)
		}
	}
}

func TestAsyncTenantIsolation(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := factory.AsyncSearchStore(context.Background())
	svc := search.NewSearchService(factory, uuids, searchStore)

	ctxA := tenantCtx("tenant-A")
	ctxB := tenantCtx("tenant-B")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}

	saveEntity(t, ctxA, factory, ref, "e1", []byte(`{"name":"Alice"}`))

	cond := &predicate.SimpleCondition{
		JsonPath:     "$.name",
		OperatorType: "EQUALS",
		Value:        "Alice",
	}

	jobID, err := svc.SubmitAsync(ctxA, ref, cond, search.SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// Wait for completion
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, _ := svc.GetAsyncStatus(ctxA, jobID)
		if st.Status == "SUCCESSFUL" || st.Status == "FAILED" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Tenant B should not see tenant A's job
	_, err = svc.GetAsyncStatus(ctxB, jobID)
	if err == nil {
		t.Fatal("expected error when querying tenant A's job from tenant B context")
	}

	_, err = svc.GetAsyncResults(ctxB, jobID, search.ResultOptions{})
	if err == nil {
		t.Fatal("expected error when getting results of tenant A's job from tenant B context")
	}

	_, cancelErr := svc.CancelAsync(ctxB, jobID)
	if cancelErr == nil {
		t.Fatal("expected error when cancelling tenant A's job from tenant B context")
	}
}

// I-2: SubmitAsync must populate SearchOpts on the job.
func TestSubmitAsyncPopulatesSearchOpts(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := factory.AsyncSearchStore(context.Background())
	svc := search.NewSearchService(factory, uuids, searchStore)

	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}

	saveEntity(t, ctx, factory, ref, "e1", []byte(`{"name":"Alice"}`))

	cond := &predicate.SimpleCondition{
		JsonPath:     "$.name",
		OperatorType: "EQUALS",
		Value:        "Alice",
	}

	pit := time.Now().Add(-1 * time.Hour)
	opts := search.SearchOptions{
		Limit:       50,
		Offset:      10,
		PointInTime: &pit,
	}

	jobID, err := svc.SubmitAsync(ctx, ref, cond, opts)
	if err != nil {
		t.Fatalf("SubmitAsync: %v", err)
	}

	// Check the job in the store immediately (before goroutine finishes).
	job, err := searchStore.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}

	if len(job.SearchOpts) == 0 {
		t.Fatal("SearchOpts should be populated on the job, got empty")
	}

	// Verify it deserializes back correctly.
	var decoded struct {
		Limit  int `json:"limit"`
		Offset int `json:"offset"`
	}
	if err := json.Unmarshal(job.SearchOpts, &decoded); err != nil {
		t.Fatalf("failed to unmarshal SearchOpts: %v", err)
	}
	if decoded.Limit != 50 {
		t.Errorf("SearchOpts.Limit = %d, want 50", decoded.Limit)
	}
	if decoded.Offset != 10 {
		t.Errorf("SearchOpts.Offset = %d, want 10", decoded.Offset)
	}
}

// I-3: Cancel-then-complete must not overwrite CANCELLED with SUCCESSFUL.
// We use a blocking search store wrapper to control timing deterministically.

// blockingSearchStore wraps spi.AsyncSearchStore and blocks SaveResults until released.
type blockingSearchStore struct {
	spi.AsyncSearchStore
	saveResultsGate chan struct{} // close to unblock SaveResults
}

func (b *blockingSearchStore) SaveResults(ctx context.Context, jobID string, entityIDs []string) error {
	<-b.saveResultsGate // block until gate is opened
	return b.AsyncSearchStore.SaveResults(ctx, jobID, entityIDs)
}

func TestCancelRaceDoesNotOverwriteCancelled(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	uuids := common.NewTestUUIDGenerator()
	realStore, _ := factory.AsyncSearchStore(context.Background())

	gate := make(chan struct{})
	blockedStore := &blockingSearchStore{
		AsyncSearchStore: realStore,
		saveResultsGate:  gate,
	}

	svc := search.NewSearchService(factory, uuids, blockedStore)

	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}

	saveEntity(t, ctx, factory, ref, "e1", []byte(`{"name":"Alice"}`))

	cond := &predicate.SimpleCondition{
		JsonPath:     "$.name",
		OperatorType: "EQUALS",
		Value:        "Alice",
	}

	jobID, err := svc.SubmitAsync(ctx, ref, cond, search.SearchOptions{})
	if err != nil {
		t.Fatalf("SubmitAsync: %v", err)
	}

	// Wait for the goroutine to reach SaveResults (it will block on the gate).
	// Poll until the search goroutine has at least started (job is still RUNNING).
	time.Sleep(50 * time.Millisecond)

	// Cancel the job while the goroutine is blocked.
	result, err := svc.CancelAsync(ctx, jobID)
	if err != nil {
		t.Fatalf("CancelAsync: %v", err)
	}
	if !result.Cancelled {
		t.Fatal("expected cancel to succeed while goroutine is blocked")
	}

	// Now release the goroutine to proceed with SaveResults + UpdateJobStatus.
	close(gate)

	// Give the goroutine time to finish.
	time.Sleep(100 * time.Millisecond)

	// Final status must be CANCELLED, not SUCCESSFUL.
	status, err := svc.GetAsyncStatus(ctx, jobID)
	if err != nil {
		t.Fatalf("GetAsyncStatus: %v", err)
	}
	if status.Status != "CANCELLED" {
		t.Errorf("expected CANCELLED after cancel-then-complete race, got %s", status.Status)
	}
}

// captureSearchStore is an in-memory AsyncSearchStore that records which
// methods get called. Used by TestSubmitAsync_SelfExecutingStore_SkipsGoroutine.
type captureSearchStore struct {
	spi.AsyncSearchStore

	mu                sync.Mutex
	createJobCalls    int
	saveResultsCalls  int
	updateStatusCalls int
}

func newCaptureSearchStore(base spi.AsyncSearchStore) *captureSearchStore {
	return &captureSearchStore{AsyncSearchStore: base}
}

func (c *captureSearchStore) CreateJob(ctx context.Context, job *spi.SearchJob) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.createJobCalls++
	return c.AsyncSearchStore.CreateJob(ctx, job)
}

func (c *captureSearchStore) SaveResults(ctx context.Context, jobID string, ids []string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.saveResultsCalls++
	return c.AsyncSearchStore.SaveResults(ctx, jobID, ids)
}

func (c *captureSearchStore) UpdateJobStatus(ctx context.Context, jobID string, status string, resultCount int, errMsg string, finishTime time.Time, calcTimeMs int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.updateStatusCalls++
	return c.AsyncSearchStore.UpdateJobStatus(ctx, jobID, status, resultCount, errMsg, finishTime, calcTimeMs)
}

// selfExecutingCaptureStore wraps captureSearchStore and implements the
// spi.SelfExecutingSearchStore marker interface.
type selfExecutingCaptureStore struct {
	*captureSearchStore
}

func (s *selfExecutingCaptureStore) SelfExecuting() {}

// TestSubmitAsync_SelfExecutingStore_SkipsGoroutine verifies that a store
// implementing SelfExecutingSearchStore is not driven by the service's
// background goroutine — SaveResults and UpdateJobStatus must not be called.
func TestSubmitAsync_SelfExecutingStore_SkipsGoroutine(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	uuids := common.NewTestUUIDGenerator()
	baseStore, _ := factory.AsyncSearchStore(context.Background())

	capture := newCaptureSearchStore(baseStore)
	store := &selfExecutingCaptureStore{captureSearchStore: capture}

	svc := search.NewSearchService(factory, uuids, store)

	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}
	cond := &predicate.SimpleCondition{
		JsonPath:     "$.x",
		OperatorType: "EQUALS",
		Value:        "y",
	}

	jobID, err := svc.SubmitAsync(ctx, ref, cond, search.SearchOptions{})
	if err != nil {
		t.Fatalf("SubmitAsync: %v", err)
	}
	if jobID == "" {
		t.Error("expected non-empty jobID")
	}

	// Wait long enough that any (incorrect) goroutine would have finished.
	time.Sleep(100 * time.Millisecond)

	capture.mu.Lock()
	defer capture.mu.Unlock()

	if capture.createJobCalls != 1 {
		t.Errorf("CreateJob: want 1 call, got %d", capture.createJobCalls)
	}
	if capture.saveResultsCalls != 0 {
		t.Errorf("self-executing store should never have SaveResults called by the service; got %d calls", capture.saveResultsCalls)
	}
	if capture.updateStatusCalls != 0 {
		t.Errorf("self-executing store should never have UpdateJobStatus called by the service; got %d calls", capture.updateStatusCalls)
	}
}

// --- Searcher delegation tests ---

// searcherEntityStore wraps an EntityStore and implements spi.Searcher.
// It records Search calls and delegates to a provided function.
type searcherEntityStore struct {
	spi.EntityStore
	searchFn    func(ctx context.Context, filter spi.Filter, opts spi.SearchOptions) ([]*spi.Entity, error)
	searchCalls int
}

func (s *searcherEntityStore) Search(ctx context.Context, filter spi.Filter, opts spi.SearchOptions) ([]*spi.Entity, error) {
	s.searchCalls++
	return s.searchFn(ctx, filter, opts)
}

// searcherFactory wraps a StoreFactory and returns a Searcher-implementing EntityStore.
type searcherFactory struct {
	spi.StoreFactory
	entityStore *searcherEntityStore
}

func (f *searcherFactory) EntityStore(ctx context.Context) (spi.EntityStore, error) {
	return f.entityStore, nil
}

func TestSearchDelegatesToSearcher(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()

	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}

	// Save entities to the real store for fallback verification.
	saveEntity(t, ctx, base, ref, "e1", []byte(`{"name":"Alice"}`))

	realStore, _ := base.EntityStore(ctx)

	expected := []*spi.Entity{
		{Meta: spi.EntityMeta{ID: "from-searcher"}, Data: []byte(`{}`)},
	}
	ses := &searcherEntityStore{
		EntityStore: realStore,
		searchFn: func(_ context.Context, _ spi.Filter, _ spi.SearchOptions) ([]*spi.Entity, error) {
			return expected, nil
		},
	}

	factory := &searcherFactory{StoreFactory: base, entityStore: ses}

	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := base.AsyncSearchStore(context.Background())
	svc := search.NewSearchService(factory, uuids, searchStore)

	cond := &predicate.SimpleCondition{
		JsonPath:     "$.name",
		OperatorType: "EQUALS",
		Value:        "Alice",
	}

	results, err := svc.Search(ctx, ref, cond, search.SearchOptions{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	// The searcher was used, not the fallback.
	if ses.searchCalls != 1 {
		t.Errorf("searchCalls = %d, want 1", ses.searchCalls)
	}
	if len(results) != 1 || results[0].Meta.ID != "from-searcher" {
		t.Errorf("expected results from searcher, got %d results", len(results))
	}
}

func TestSearchFallsBackWhenNotSearcher(t *testing.T) {
	// The memory plugin doesn't implement Searcher, so this tests the fallback.
	factory := memory.NewStoreFactory()
	defer factory.Close()
	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := factory.AsyncSearchStore(context.Background())
	svc := search.NewSearchService(factory, uuids, searchStore)

	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}

	saveEntity(t, ctx, factory, ref, "e1", []byte(`{"name":"Alice"}`))

	cond := &predicate.SimpleCondition{
		JsonPath:     "$.name",
		OperatorType: "EQUALS",
		Value:        "Alice",
	}

	results, err := svc.Search(ctx, ref, cond, search.SearchOptions{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 || results[0].Meta.ID != "e1" {
		t.Fatalf("expected 1 result (e1), got %d", len(results))
	}
}

func TestSearchFallsBackWhenInTransaction(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()

	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}

	saveEntity(t, ctx, base, ref, "e1", []byte(`{"name":"Alice"}`))

	realStore, _ := base.EntityStore(ctx)

	ses := &searcherEntityStore{
		EntityStore: realStore,
		searchFn: func(_ context.Context, _ spi.Filter, _ spi.SearchOptions) ([]*spi.Entity, error) {
			return []*spi.Entity{{Meta: spi.EntityMeta{ID: "from-searcher"}}}, nil
		},
	}

	factory := &searcherFactory{StoreFactory: base, entityStore: ses}

	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := base.AsyncSearchStore(context.Background())
	svc := search.NewSearchService(factory, uuids, searchStore)

	// Create a context with an active transaction.
	tx := &spi.TransactionState{
		ID:           "test-tx",
		TenantID:     "tenant-1",
		SnapshotTime: time.Now(),
		ReadSet:      make(map[string]bool),
		WriteSet:     make(map[string]bool),
		Buffer:       make(map[string]*spi.Entity),
		Deletes:      make(map[string]bool),
	}
	txCtx := spi.WithTransaction(ctx, tx)

	cond := &predicate.SimpleCondition{
		JsonPath:     "$.name",
		OperatorType: "EQUALS",
		Value:        "Alice",
	}

	results, err := svc.Search(txCtx, ref, cond, search.SearchOptions{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	// Should NOT have used the searcher (in-tx fallback).
	if ses.searchCalls != 0 {
		t.Errorf("searchCalls = %d, want 0 (should fall back when in transaction)", ses.searchCalls)
	}
	// Should get result from the fallback path.
	if len(results) != 1 || results[0].Meta.ID != "e1" {
		t.Fatalf("expected 1 result (e1) from fallback, got %d results", len(results))
	}
}

// sortTestFactory returns a fixed Searcher entity store AND a fixed model store.
// Used by the sort-pushdown tests that need both dimensions controlled.
type sortTestFactory struct {
	spi.StoreFactory
	entityStore *searcherEntityStore
	modelStore  spi.ModelStore
}

func (f *sortTestFactory) EntityStore(_ context.Context) (spi.EntityStore, error) {
	return f.entityStore, nil
}

func (f *sortTestFactory) ModelStore(_ context.Context) (spi.ModelStore, error) {
	return f.modelStore, nil
}

// TestSearch_SortByDataField_PushesOrderSpecToSearcher verifies that Search
// with opts.OrderBy resolves the sort key against the model schema and passes
// the fully-typed spi.OrderSpec (including Kind) down to the spi.Searcher.
func TestSearch_SortByDataField_PushesOrderSpecToSearcher(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()

	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}

	// Model declares "surname" as a String field.
	desc := buildSearchDescriptor(t, ref, "surname")
	ms := &refreshingModelStore{
		// validateConditionPaths (for $.surname) + resolveSortKeys each call Get once.
		getQueue: []*spi.ModelDescriptor{desc, desc},
	}

	var capturedOpts spi.SearchOptions
	realStore, _ := base.EntityStore(ctx)
	ses := &searcherEntityStore{
		EntityStore: realStore,
		searchFn: func(_ context.Context, _ spi.Filter, opts spi.SearchOptions) ([]*spi.Entity, error) {
			capturedOpts = opts
			return nil, nil
		},
	}

	factory := &sortTestFactory{
		StoreFactory: base,
		entityStore:  ses,
		modelStore:   ms,
	}

	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := base.AsyncSearchStore(context.Background())
	svc := search.NewSearchService(factory, uuids, searchStore)

	cond := &predicate.SimpleCondition{
		JsonPath:     "$.surname",
		OperatorType: "EQUALS",
		Value:        "Smith",
	}

	_, err := svc.Search(ctx, ref, cond, search.SearchOptions{
		OrderBy: []search.OrderKey{{Path: "surname", Source: spi.SourceData, Desc: true}},
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if ses.searchCalls != 1 {
		t.Fatalf("expected searcher to be called once, got %d", ses.searchCalls)
	}
	if len(capturedOpts.OrderBy) != 1 {
		t.Fatalf("expected 1 OrderSpec pushed to searcher, got %d", len(capturedOpts.OrderBy))
	}
	spec := capturedOpts.OrderBy[0]
	if spec.Path != "surname" {
		t.Errorf("spec.Path = %q, want %q", spec.Path, "surname")
	}
	if spec.Source != spi.SourceData {
		t.Errorf("spec.Source = %q, want %q", spec.Source, spi.SourceData)
	}
	if !spec.Desc {
		t.Error("spec.Desc = false, want true")
	}
	if spec.Kind != spi.OrderText {
		t.Errorf("spec.Kind = %v, want spi.OrderText", spec.Kind)
	}
}

// TestSearch_UnknownSortField_ReturnsInvalidFieldPath verifies that Search
// with an OrderKey whose path is not in the model schema returns a
// 400-classified *common.AppError with code INVALID_FIELD_PATH.
func TestSearch_UnknownSortField_ReturnsInvalidFieldPath(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()

	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}

	// Model has "surname" but NOT "nonexistent".
	desc := buildSearchDescriptor(t, ref, "surname")
	ms := &refreshingModelStore{
		// validateConditionPaths is called but returns early with nil —
		// LifecycleCondition has no data paths, so it makes no model-store call.
		// resolveSortKeys needs exactly one Get call.
		getQueue: []*spi.ModelDescriptor{desc},
	}

	realStore, _ := base.EntityStore(ctx)
	ses := &searcherEntityStore{
		EntityStore: realStore,
		searchFn: func(_ context.Context, _ spi.Filter, _ spi.SearchOptions) ([]*spi.Entity, error) {
			return nil, nil
		},
	}

	factory := &sortTestFactory{
		StoreFactory: base,
		entityStore:  ses,
		modelStore:   ms,
	}

	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := base.AsyncSearchStore(context.Background())
	svc := search.NewSearchService(factory, uuids, searchStore)

	// LifecycleCondition: extractFieldPaths returns [] so validateConditionPaths
	// returns early without touching the model store.
	cond := &predicate.LifecycleCondition{
		Field:        "state",
		OperatorType: "EQUALS",
		Value:        "ACTIVE",
	}

	_, err := svc.Search(ctx, ref, cond, search.SearchOptions{
		OrderBy: []search.OrderKey{{Path: "nonexistent", Source: spi.SourceData, Desc: false}},
	})
	if err == nil {
		t.Fatal("expected error for unknown sort field, got nil")
	}
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *common.AppError, got %T: %v", err, err)
	}
	if appErr.Code != common.ErrCodeInvalidFieldPath {
		t.Errorf("appErr.Code = %q, want %q", appErr.Code, common.ErrCodeInvalidFieldPath)
	}
	if appErr.Status != http.StatusBadRequest {
		t.Errorf("appErr.Status = %d, want %d", appErr.Status, http.StatusBadRequest)
	}
}

// TestSubmitAsync_OrderBy_InvalidField verifies that SubmitAsync returns a
// 400 INVALID_FIELD_PATH error synchronously when a sort key is not known by
// the model schema — no job must be created before the error is returned.
func TestSubmitAsync_OrderBy_InvalidField(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()

	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}

	// Model declares "surname" but NOT "nonexistent".
	desc := buildSearchDescriptor(t, ref, "surname")
	ms := &refreshingModelStore{getQueue: []*spi.ModelDescriptor{desc}}

	realEntityStore, _ := base.EntityStore(ctx)
	ses := &searcherEntityStore{
		EntityStore: realEntityStore,
		searchFn: func(_ context.Context, _ spi.Filter, _ spi.SearchOptions) ([]*spi.Entity, error) {
			return nil, nil
		},
	}
	factory := &sortTestFactory{StoreFactory: base, entityStore: ses, modelStore: ms}

	uuids := common.NewTestUUIDGenerator()
	baseStore, _ := base.AsyncSearchStore(context.Background())
	capture := newCaptureSearchStore(baseStore)
	svc := search.NewSearchService(factory, uuids, capture)

	// LifecycleCondition has no data paths — validateConditionPaths exits
	// early without consuming from the model store queue.
	cond := &predicate.LifecycleCondition{
		Field:        "state",
		OperatorType: "EQUALS",
		Value:        "ACTIVE",
	}

	_, err := svc.SubmitAsync(ctx, ref, cond, search.SearchOptions{
		OrderBy: []search.OrderKey{{Path: "nonexistent", Source: spi.SourceData}},
	})
	if err == nil {
		t.Fatal("expected error for unknown sort field, got nil")
	}
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *common.AppError, got %T: %v", err, err)
	}
	if appErr.Code != common.ErrCodeInvalidFieldPath {
		t.Errorf("Code = %q, want %q", appErr.Code, common.ErrCodeInvalidFieldPath)
	}
	if appErr.Status != http.StatusBadRequest {
		t.Errorf("Status = %d, want %d", appErr.Status, http.StatusBadRequest)
	}

	// No job must have been created before the error was returned.
	capture.mu.Lock()
	defer capture.mu.Unlock()
	if capture.createJobCalls != 0 {
		t.Errorf("CreateJob called %d time(s), want 0 (error must precede job creation)", capture.createJobCalls)
	}
}

// TestSubmitAsync_OrderBy_PersistsTypedSpecs verifies that with valid sort keys
// the persisted SearchOpts JSON carries a typed []spi.OrderSpec with Kind set.
// Uses the self-executing store so no goroutine is launched and the job can be
// inspected synchronously right after SubmitAsync returns.
func TestSubmitAsync_OrderBy_PersistsTypedSpecs(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()

	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}

	// Any descriptor suffices — creationDate is a meta key resolved without
	// consulting the data-field map, but loadFieldsMap must still succeed.
	desc := buildSearchDescriptor(t, ref, "surname")
	ms := &refreshingModelStore{getQueue: []*spi.ModelDescriptor{desc}}

	realEntityStore, _ := base.EntityStore(ctx)
	ses := &searcherEntityStore{
		EntityStore: realEntityStore,
		searchFn: func(_ context.Context, _ spi.Filter, _ spi.SearchOptions) ([]*spi.Entity, error) {
			return nil, nil
		},
	}
	factory := &sortTestFactory{StoreFactory: base, entityStore: ses, modelStore: ms}

	uuids := common.NewTestUUIDGenerator()
	baseStore, _ := base.AsyncSearchStore(context.Background())
	capture := newCaptureSearchStore(baseStore)
	selfExec := &selfExecutingCaptureStore{captureSearchStore: capture}
	svc := search.NewSearchService(factory, uuids, selfExec)

	cond := &predicate.LifecycleCondition{
		Field:        "state",
		OperatorType: "EQUALS",
		Value:        "ACTIVE",
	}

	jobID, err := svc.SubmitAsync(ctx, ref, cond, search.SearchOptions{
		// creationDate is a canonical meta field → resolves to Kind=OrderTemporal.
		OrderBy: []search.OrderKey{{Path: "creationDate", Source: spi.SourceMeta}},
	})
	if err != nil {
		t.Fatalf("SubmitAsync: %v", err)
	}

	job, err := baseStore.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if len(job.SearchOpts) == 0 {
		t.Fatal("SearchOpts is empty")
	}

	var decoded struct {
		OrderBy []spi.OrderSpec `json:"orderBy"`
	}
	if err := json.Unmarshal(job.SearchOpts, &decoded); err != nil {
		t.Fatalf("Unmarshal SearchOpts: %v", err)
	}
	if len(decoded.OrderBy) != 1 {
		t.Fatalf("decoded.OrderBy len = %d, want 1", len(decoded.OrderBy))
	}
	spec := decoded.OrderBy[0]
	if spec.Path != "creationDate" {
		t.Errorf("spec.Path = %q, want %q", spec.Path, "creationDate")
	}
	if spec.Source != spi.SourceMeta {
		t.Errorf("spec.Source = %v, want SourceMeta", spec.Source)
	}
	if spec.Kind != spi.OrderTemporal {
		t.Errorf("spec.Kind = %v, want OrderTemporal (%v)", spec.Kind, spi.OrderTemporal)
	}
}

// TestSearch_SortKeyCap_ReturnsError verifies that Search returns a 400
// INVALID_FIELD_PATH AppError when the number of sort keys exceeds the
// configured cap. The cap check must fire before the model store is
// consulted so the model need not be registered.
func TestSearch_SortKeyCap_ReturnsError(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := factory.AsyncSearchStore(context.Background())
	// Cap set to 2 — sending 3 keys must be rejected.
	svc := search.NewSearchService(factory, uuids, searchStore).WithMaxSortKeys(2)

	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "item", ModelVersion: "1"}

	orderBy := []search.OrderKey{
		{Path: "a", Source: spi.SourceData},
		{Path: "b", Source: spi.SourceData},
		{Path: "c", Source: spi.SourceData},
	}
	cond := &predicate.LifecycleCondition{
		Field: "state", OperatorType: "EQUALS", Value: "ACTIVE",
	}

	_, err := svc.Search(ctx, ref, cond, search.SearchOptions{OrderBy: orderBy})
	if err == nil {
		t.Fatal("expected error for too many sort keys, got nil")
	}
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *common.AppError, got %T: %v", err, err)
	}
	if appErr.Code != common.ErrCodeInvalidFieldPath {
		t.Errorf("appErr.Code = %q, want %q", appErr.Code, common.ErrCodeInvalidFieldPath)
	}
	if appErr.Status != http.StatusBadRequest {
		t.Errorf("appErr.Status = %d, want 400", appErr.Status)
	}
}

// TestSubmitAsync_SortKeyCap_ReturnsError verifies that SubmitAsync returns a
// 400 INVALID_FIELD_PATH AppError synchronously when the number of sort keys
// exceeds the configured cap. The cap check fires before the job is created,
// so CreateJob must not be called.
func TestSubmitAsync_SortKeyCap_ReturnsError(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	uuids := common.NewTestUUIDGenerator()
	baseStore, _ := factory.AsyncSearchStore(context.Background())
	capture := newCaptureSearchStore(baseStore)
	// Cap set to 2 — sending 3 keys must be rejected.
	svc := search.NewSearchService(factory, uuids, capture).WithMaxSortKeys(2)

	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "item", ModelVersion: "1"}

	orderBy := []search.OrderKey{
		{Path: "a", Source: spi.SourceData},
		{Path: "b", Source: spi.SourceData},
		{Path: "c", Source: spi.SourceData},
	}
	cond := &predicate.LifecycleCondition{
		Field: "state", OperatorType: "EQUALS", Value: "ACTIVE",
	}

	_, err := svc.SubmitAsync(ctx, ref, cond, search.SearchOptions{OrderBy: orderBy})
	if err == nil {
		t.Fatal("expected error for too many sort keys, got nil")
	}
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *common.AppError, got %T: %v", err, err)
	}
	if appErr.Code != common.ErrCodeInvalidFieldPath {
		t.Errorf("appErr.Code = %q, want %q", appErr.Code, common.ErrCodeInvalidFieldPath)
	}
	if appErr.Status != http.StatusBadRequest {
		t.Errorf("appErr.Status = %d, want 400", appErr.Status)
	}

	// No job must have been created before the error was returned.
	capture.mu.Lock()
	defer capture.mu.Unlock()
	if capture.createJobCalls != 0 {
		t.Errorf("CreateJob called %d time(s), want 0 (cap error must precede job creation)", capture.createJobCalls)
	}
}

// TestSearch_DuplicateSortKeys_ReturnsError verifies that Search returns a
// 400 INVALID_FIELD_PATH AppError when two OrderKeys share the same
// source+path combination.
func TestSearch_DuplicateSortKeys_ReturnsError(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()

	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "item", ModelVersion: "1"}

	// Model declares "tag" as a scalar string field.
	desc := buildSearchDescriptor(t, ref, "tag")
	ms := &refreshingModelStore{
		// resolveSortKeys calls Get once.
		getQueue: []*spi.ModelDescriptor{desc},
	}

	realStore, _ := base.EntityStore(ctx)
	ses := &searcherEntityStore{
		EntityStore: realStore,
		searchFn: func(_ context.Context, _ spi.Filter, _ spi.SearchOptions) ([]*spi.Entity, error) {
			return nil, nil
		},
	}
	factory := &sortTestFactory{StoreFactory: base, entityStore: ses, modelStore: ms}

	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := base.AsyncSearchStore(context.Background())
	svc := search.NewSearchService(factory, uuids, searchStore)

	// Two identical keys — same source+path must be rejected.
	orderBy := []search.OrderKey{
		{Path: "tag", Source: spi.SourceData},
		{Path: "tag", Source: spi.SourceData},
	}
	cond := &predicate.LifecycleCondition{
		Field: "state", OperatorType: "EQUALS", Value: "ACTIVE",
	}

	_, err := svc.Search(ctx, ref, cond, search.SearchOptions{OrderBy: orderBy})
	if err == nil {
		t.Fatal("expected error for duplicate sort keys, got nil")
	}
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *common.AppError, got %T: %v", err, err)
	}
	if appErr.Code != common.ErrCodeInvalidFieldPath {
		t.Errorf("appErr.Code = %q, want %q", appErr.Code, common.ErrCodeInvalidFieldPath)
	}
	if appErr.Status != http.StatusBadRequest {
		t.Errorf("appErr.Status = %d, want 400", appErr.Status)
	}
}

// I-3 variant: ensure the fix doesn't break normal successful flow.
func TestAsyncSuccessfulWhenNotCancelled(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := factory.AsyncSearchStore(context.Background())
	svc := search.NewSearchService(factory, uuids, searchStore)

	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}

	saveEntity(t, ctx, factory, ref, "e1", []byte(`{"name":"Alice"}`))

	cond := &predicate.SimpleCondition{
		JsonPath:     "$.name",
		OperatorType: "EQUALS",
		Value:        "Alice",
	}

	jobID, err := svc.SubmitAsync(ctx, ref, cond, search.SearchOptions{})
	if err != nil {
		t.Fatalf("SubmitAsync: %v", err)
	}

	// Wait for completion.
	deadline := time.Now().Add(5 * time.Second)
	var status search.SearchJobStatus
	for time.Now().Before(deadline) {
		status, err = svc.GetAsyncStatus(ctx, jobID)
		if err != nil {
			t.Fatalf("GetAsyncStatus: %v", err)
		}
		if status.Status == "SUCCESSFUL" || status.Status == "FAILED" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if status.Status != "SUCCESSFUL" {
		t.Fatalf("expected SUCCESSFUL, got %s", status.Status)
	}
}
