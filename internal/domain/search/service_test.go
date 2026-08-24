package search_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
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

// helper: register a minimal model descriptor so EnsureModelRegistered passes.
func saveMinimalModel(t *testing.T, ctx context.Context, factory *memory.StoreFactory, ref spi.ModelRef) {
	t.Helper()
	ms, err := factory.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	if err := ms.Save(ctx, &spi.ModelDescriptor{Ref: ref}); err != nil {
		t.Fatalf("Save model: %v", err)
	}
}

// helper: register a model whose schema declares the given top-level leaf
// fields with their declared types. Search evaluation is type-directed: both
// the plugin Searcher and the in-memory fallback resolve a data leaf's declared
// subtype from the model's FieldsMap. A comparison/equality leaf over a path
// with no declared type degrades to non-match — so a search test that expects
// matches must register the schema the same way production does.
func saveModelWithFields(t *testing.T, ctx context.Context, factory *memory.StoreFactory, ref spi.ModelRef, fields map[string]schema.DataType) {
	t.Helper()
	node := schema.NewObjectNode()
	for name, dt := range fields {
		node.SetChild(name, schema.NewLeafNode(dt))
	}
	raw, err := schema.Marshal(node)
	if err != nil {
		t.Fatalf("schema.Marshal: %v", err)
	}
	ms, err := factory.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	if err := ms.Save(ctx, &spi.ModelDescriptor{Ref: ref, Schema: raw}); err != nil {
		t.Fatalf("Save model: %v", err)
	}
}

// helper: register a model whose schema declares `arrayField` as an array of
// objects each carrying a single String leaf `leafField`. The resulting
// FieldsMap key is "$.<arrayField>[*].<leafField>", which is what a wildcard
// array condition ("$.items[*].name") resolves its element type against.
func saveModelWithArrayOfStringField(t *testing.T, ctx context.Context, factory *memory.StoreFactory, ref spi.ModelRef, arrayField, leafField string) {
	t.Helper()
	elem := schema.NewObjectNode()
	elem.SetChild(leafField, schema.NewLeafNode(schema.String))
	node := schema.NewObjectNode()
	node.SetChild(arrayField, schema.NewArrayNode(elem))
	raw, err := schema.Marshal(node)
	if err != nil {
		t.Fatalf("schema.Marshal: %v", err)
	}
	ms, err := factory.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	if err := ms.Save(ctx, &spi.ModelDescriptor{Ref: ref, Schema: raw}); err != nil {
		t.Fatalf("Save model: %v", err)
	}
}

// helper: register a model declaring an Integer leaf `val` alongside an
// `items` array of objects carrying a String leaf `name`. The array member
// exists purely so untranslatableCondition's wildcard path resolves against a
// declared field and survives pre-execution path validation.
func saveModelWithValAndItemsArray(t *testing.T, ctx context.Context, factory *memory.StoreFactory, ref spi.ModelRef) {
	t.Helper()
	elem := schema.NewObjectNode()
	elem.SetChild("name", schema.NewLeafNode(schema.String))
	node := schema.NewObjectNode()
	node.SetChild("val", schema.NewLeafNode(schema.Integer))
	node.SetChild("items", schema.NewArrayNode(elem))
	raw, err := schema.Marshal(node)
	if err != nil {
		t.Fatalf("schema.Marshal: %v", err)
	}
	ms, err := factory.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	if err := ms.Save(ctx, &spi.ModelDescriptor{Ref: ref, Schema: raw}); err != nil {
		t.Fatalf("Save model: %v", err)
	}
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

	saveModelWithFields(t, ctx, factory, ref, map[string]schema.DataType{"name": schema.String, "age": schema.Integer})
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

	saveMinimalModel(t, ctx, factory, ref)
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

	saveModelWithFields(t, ctx, factory, ref, map[string]schema.DataType{"name": schema.String})
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

// TestDirectSearch_UnboundedReturnsAllMatches verifies that an omitted
// (zero-value) Limit is genuinely unbounded on the Searcher pushdown path:
// all 5 matches come back, none truncated. (Renamed from
// TestDirectSearchPagination — this exercises no offset/pagination
// parameter, only the unbounded-limit case; see also
// TestSearch_FallbackBranchUnboundedReturnsAll for the same guarantee on
// the GetAll in-memory fallback branch.)
func TestDirectSearch_UnboundedReturnsAllMatches(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := factory.AsyncSearchStore(context.Background())
	svc := search.NewSearchService(factory, uuids, searchStore)

	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "item", ModelVersion: "1"}

	saveModelWithFields(t, ctx, factory, ref, map[string]schema.DataType{"val": schema.Integer})
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
}

func TestAsyncLifecycle(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := factory.AsyncSearchStore(context.Background())
	svc := search.NewSearchService(factory, uuids, searchStore)

	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}

	saveModelWithFields(t, ctx, factory, ref, map[string]schema.DataType{"name": schema.String})
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
	saveMinimalModel(t, ctx, factory, ref)

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
	saveMinimalModel(t, ctxA, factory, ref)

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
	saveMinimalModel(t, ctx, factory, ref)

	saveEntity(t, ctx, factory, ref, "e1", []byte(`{"name":"Alice"}`))

	cond := &predicate.SimpleCondition{
		JsonPath:     "$.name",
		OperatorType: "EQUALS",
		Value:        "Alice",
	}

	pit := time.Now().Add(-1 * time.Hour)
	opts := search.SearchOptions{
		Limit:       50,
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
		Limit int `json:"limit"`
	}
	if err := json.Unmarshal(job.SearchOpts, &decoded); err != nil {
		t.Fatalf("failed to unmarshal SearchOpts: %v", err)
	}
	if decoded.Limit != 50 {
		t.Errorf("SearchOpts.Limit = %d, want 50", decoded.Limit)
	}

	// The persisted job-opts JSON must no longer carry an "offset" key —
	// SearchOptions.Offset was removed, and a SelfExecutingSearchStore that
	// decodes this blob must not resurrect a phantom pagination field.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(job.SearchOpts, &raw); err != nil {
		t.Fatalf("failed to unmarshal SearchOpts as raw map: %v", err)
	}
	if _, present := raw["offset"]; present {
		t.Error(`persisted SearchOpts JSON must not contain an "offset" key`)
	}
}

// I-3: Cancel-then-complete must not overwrite CANCELLED with SUCCESSFUL.
// We use a blocking search store wrapper to control timing deterministically.

// blockingSearchStore wraps spi.AsyncSearchStore and blocks SaveResults until released.
type blockingSearchStore struct {
	spi.AsyncSearchStore
	saveResultsGate chan struct{} // close to unblock SaveResults
}

func (b *blockingSearchStore) SaveResults(ctx context.Context, jobID string, epoch int64, entityIDs iter.Seq[string]) error {
	<-b.saveResultsGate // block until gate is opened
	return b.AsyncSearchStore.SaveResults(ctx, jobID, epoch, entityIDs)
}

func TestCancelRaceDoesNotOverwriteCancelled(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	uuids := common.NewTestUUIDGenerator()
	realStore, _ := factory.AsyncSearchStore(context.Background())

	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}
	saveMinimalModel(t, ctx, factory, ref)

	gate := make(chan struct{})
	blockedStore := &blockingSearchStore{
		AsyncSearchStore: realStore,
		saveResultsGate:  gate,
	}

	svc := search.NewSearchService(factory, uuids, blockedStore)

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

// cancelDispatchCaptureStore wraps spi.AsyncSearchStore, blocks SaveResults
// until released (so the job is deterministically still RUNNING when the
// test calls CancelAsync), and records Cancel/UpdateJobStatus calls so the
// test can assert which method CancelAsync dispatches through.
type cancelDispatchCaptureStore struct {
	spi.AsyncSearchStore
	saveResultsGate chan struct{} // close to unblock SaveResults

	mu                sync.Mutex
	cancelCalls       int
	cancelFinishTime  time.Time
	updateStatusCalls int
}

func (c *cancelDispatchCaptureStore) SaveResults(ctx context.Context, jobID string, epoch int64, entityIDs iter.Seq[string]) error {
	<-c.saveResultsGate // block until gate is opened
	return c.AsyncSearchStore.SaveResults(ctx, jobID, epoch, entityIDs)
}

func (c *cancelDispatchCaptureStore) Cancel(ctx context.Context, jobID string, finishTime time.Time) error {
	c.mu.Lock()
	c.cancelCalls++
	c.cancelFinishTime = finishTime
	c.mu.Unlock()
	return c.AsyncSearchStore.Cancel(ctx, jobID, finishTime)
}

func (c *cancelDispatchCaptureStore) UpdateJobStatus(ctx context.Context, jobID string, epoch int64, status string, resultCount int, errMsg string, finishTime time.Time, calcTimeMs int64) error {
	c.mu.Lock()
	c.updateStatusCalls++
	c.mu.Unlock()
	return c.AsyncSearchStore.UpdateJobStatus(ctx, jobID, epoch, status, resultCount, errMsg, finishTime, calcTimeMs)
}

// TestCancelAsync_DispatchesStoreCancel verifies that CancelAsync on a
// RUNNING job calls the store's Cancel with a non-zero finishTime, and does
// NOT call UpdateJobStatus — CancelAsync must dispatch through Cancel, not
// reimplement the transition via the generic status-update path (which
// leaves cancelled jobs unreapable if the store's UpdateJobStatus path
// diverges from Cancel's terminal-state handling).
func TestCancelAsync_DispatchesStoreCancel(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	uuids := common.NewTestUUIDGenerator()
	realStore, _ := factory.AsyncSearchStore(context.Background())

	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}
	saveMinimalModel(t, ctx, factory, ref)

	gate := make(chan struct{})
	capture := &cancelDispatchCaptureStore{
		AsyncSearchStore: realStore,
		saveResultsGate:  gate,
	}

	svc := search.NewSearchService(factory, uuids, capture)

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

	// Wait for the goroutine to reach SaveResults (it will block on the
	// gate), so the job is guaranteed still RUNNING when CancelAsync runs.
	time.Sleep(50 * time.Millisecond)

	result, err := svc.CancelAsync(ctx, jobID)
	if err != nil {
		t.Fatalf("CancelAsync: %v", err)
	}
	if !result.Cancelled {
		t.Fatal("expected cancel to succeed while goroutine is blocked")
	}

	capture.mu.Lock()
	cancelCalls := capture.cancelCalls
	cancelFinishTime := capture.cancelFinishTime
	updateStatusCalls := capture.updateStatusCalls
	capture.mu.Unlock()

	if cancelCalls != 1 {
		t.Errorf("Cancel calls = %d, want 1", cancelCalls)
	}
	if cancelFinishTime.IsZero() {
		t.Error("Cancel must be called with a non-zero finishTime")
	}
	if updateStatusCalls != 0 {
		t.Errorf("CancelAsync must not call UpdateJobStatus, got %d calls", updateStatusCalls)
	}

	// Release the blocked goroutine so it doesn't leak past the test.
	close(gate)
	time.Sleep(100 * time.Millisecond)
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

func (c *captureSearchStore) SaveResults(ctx context.Context, jobID string, epoch int64, ids iter.Seq[string]) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.saveResultsCalls++
	return c.AsyncSearchStore.SaveResults(ctx, jobID, epoch, ids)
}

func (c *captureSearchStore) UpdateJobStatus(ctx context.Context, jobID string, epoch int64, status string, resultCount int, errMsg string, finishTime time.Time, calcTimeMs int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.updateStatusCalls++
	return c.AsyncSearchStore.UpdateJobStatus(ctx, jobID, epoch, status, resultCount, errMsg, finishTime, calcTimeMs)
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
	saveMinimalModel(t, ctx, factory, ref)
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
// It records Search calls and delegates to a provided function. It also
// counts GetAll calls so tests can assert the fallback path was (or was
// not) reached.
type searcherEntityStore struct {
	spi.EntityStore
	searchFn     func(ctx context.Context, filter spi.Filter, opts spi.SearchOptions) ([]*spi.Entity, error)
	searchCalls  int
	getAllCalls  int
	capturedOpts spi.SearchOptions
}

func (s *searcherEntityStore) Search(ctx context.Context, filter spi.Filter, opts spi.SearchOptions) ([]*spi.Entity, error) {
	s.searchCalls++
	s.capturedOpts = opts
	return s.searchFn(ctx, filter, opts)
}

func (s *searcherEntityStore) GetAll(ctx context.Context, modelRef spi.ModelRef) ([]*spi.Entity, error) {
	s.getAllCalls++
	return s.EntityStore.GetAll(ctx, modelRef)
}

// searcherFactory wraps a StoreFactory and returns a Searcher-implementing EntityStore.
type searcherFactory struct {
	spi.StoreFactory
	entityStore *searcherEntityStore
}

func (f *searcherFactory) EntityStore(ctx context.Context) (spi.EntityStore, error) {
	return f.entityStore, nil
}

// nonSearcherEntityStore embeds the spi.EntityStore INTERFACE (not a concrete
// type), so no Search method is promoted and the wrapper does NOT satisfy
// spi.Searcher. The memory plugin now implements spi.Searcher itself, so a
// dedicated non-Searcher store is required to exercise the search service's
// in-memory GetAll+match fallback path.
type nonSearcherEntityStore struct {
	spi.EntityStore
}

// nonSearcherFactory returns a non-Searcher EntityStore, delegating everything
// else to the wrapped StoreFactory.
type nonSearcherFactory struct {
	spi.StoreFactory
	entityStore spi.EntityStore
}

func (f *nonSearcherFactory) EntityStore(ctx context.Context) (spi.EntityStore, error) {
	return f.entityStore, nil
}

func TestSearchDelegatesToSearcher(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()

	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}

	saveMinimalModel(t, ctx, base, ref)
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

	// Limit must be > 0: opts.Limit <= 0 now skips Searcher pushdown
	// entirely (see TestSearch_LimitZeroPassesUnboundedToSearcher) — real
	// callers always resolve a positive limit before reaching Search, and a
	// bounded request is what this test means to exercise delegation with.
	results, err := svc.Search(ctx, ref, cond, search.SearchOptions{Limit: 10})
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

// TestSearch_TrackingReadPushedToSearcher verifies that Search with
// opts.TrackingRead set threads the flag through to the spi.SearchOptions
// passed to the plugin Searcher's Search call (pushdown branch).
func TestSearch_TrackingReadPushedToSearcher(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()

	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}

	saveMinimalModel(t, ctx, base, ref)

	realStore, _ := base.EntityStore(ctx)

	var capturedOpts spi.SearchOptions
	ses := &searcherEntityStore{
		EntityStore: realStore,
		searchFn: func(_ context.Context, _ spi.Filter, opts spi.SearchOptions) ([]*spi.Entity, error) {
			capturedOpts = opts
			return nil, nil
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

	// Limit: 10 — see TestSearchDelegatesToSearcher's comment: opts.Limit <=
	// 0 skips Searcher pushdown entirely now.
	_, err := svc.Search(ctx, ref, cond, search.SearchOptions{Limit: 10, TrackingRead: true})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if ses.searchCalls != 1 {
		t.Fatalf("expected searcher to be called once, got %d", ses.searchCalls)
	}
	if !capturedOpts.TrackingRead {
		t.Errorf("capturedOpts.TrackingRead = false, want true")
	}
}

func TestSearchFallsBackWhenNotSearcher(t *testing.T) {
	// Wrap the memory store so it does NOT implement spi.Searcher (the memory
	// plugin implements it directly now), forcing the GetAll+match fallback.
	base := memory.NewStoreFactory()
	defer base.Close()

	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}

	saveModelWithFields(t, ctx, base, ref, map[string]schema.DataType{"name": schema.String})
	saveEntity(t, ctx, base, ref, "e1", []byte(`{"name":"Alice"}`))

	realStore, _ := base.EntityStore(ctx)
	if _, ok := realStore.(spi.Searcher); !ok {
		t.Fatal("precondition: memory store expected to implement spi.Searcher")
	}
	nonSearcher := &nonSearcherEntityStore{EntityStore: realStore}
	if _, ok := any(nonSearcher).(spi.Searcher); ok {
		t.Fatal("wrapper must NOT implement spi.Searcher")
	}
	factory := &nonSearcherFactory{StoreFactory: base, entityStore: nonSearcher}

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
	if len(results) != 1 || results[0].Meta.ID != "e1" {
		t.Fatalf("expected 1 result (e1), got %d", len(results))
	}
}

// TestSearchFallsBackWhenNotSearcher_NilConditionMatchesAll pins the
// fallback's explicit nil-condition guard: a nil predicate.Condition means
// "no filter" and must return every entity, the same answer the pre-split
// per-row evaluator gave (it never faulted on a nil condition — it only ever
// ran once a row reached it, and a nil condition never produced a per-row
// fault). Without the guard, match.Prepare(nil, ...) has no case for a nil
// predicate.Condition (every concrete variant of the sum type is a non-nil
// struct) and reports "unknown condition type: <nil>", turning this into a
// 500 instead of the pre-split 200-with-everything.
func TestSearchFallsBackWhenNotSearcher_NilConditionMatchesAll(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()

	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}

	saveModelWithFields(t, ctx, base, ref, map[string]schema.DataType{"name": schema.String})
	saveEntity(t, ctx, base, ref, "e1", []byte(`{"name":"Alice"}`))
	saveEntity(t, ctx, base, ref, "e2", []byte(`{"name":"Bob"}`))

	realStore, _ := base.EntityStore(ctx)
	nonSearcher := &nonSearcherEntityStore{EntityStore: realStore}
	factory := &nonSearcherFactory{StoreFactory: base, entityStore: nonSearcher}

	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := base.AsyncSearchStore(context.Background())
	svc := search.NewSearchService(factory, uuids, searchStore)

	results, err := svc.Search(ctx, ref, nil, search.SearchOptions{})
	if err != nil {
		t.Fatalf("Search with nil condition: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected both entities back for a nil (match-all) condition, got %d: %v", len(results), results)
	}
}

// TestSearchDelegatesToSearcherInTransaction verifies the de-guarded
// contract (Task 13): a plugin Searcher is now tx-aware (read-your-own-writes)
// on every OSS backend, so Search delegates to it even with an active
// transaction in ctx — it must NOT fall back to GetAll+match just because a
// tx is present. This replaces the pre-Task-13 expectation (formerly
// TestSearchFallsBackWhenInTransaction) that in-tx searches always bypassed
// pushdown; that expectation was correct for the old tx==nil gate but is now
// the wrong contract now that all backends implement a tx-aware Searcher.
func TestSearchDelegatesToSearcherInTransaction(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()

	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}

	saveMinimalModel(t, ctx, base, ref)
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

	// Limit: 10 — see TestSearchDelegatesToSearcher's comment: opts.Limit <=
	// 0 skips Searcher pushdown entirely now.
	results, err := svc.Search(txCtx, ref, cond, search.SearchOptions{Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	// Should delegate to the plugin Searcher, NOT the GetAll fallback.
	if ses.searchCalls != 1 {
		t.Errorf("searchCalls = %d, want 1 (in-tx search must delegate to the tx-aware Searcher)", ses.searchCalls)
	}
	if ses.getAllCalls != 0 {
		t.Errorf("getAllCalls = %d, want 0 (must not use the GetAll fallback when a Searcher is available)", ses.getAllCalls)
	}
	if len(results) != 1 || results[0].Meta.ID != "from-searcher" {
		t.Fatalf("expected 1 result from the searcher, got %d results", len(results))
	}
}

// TestSearch_TranslateFailure_FallsBackEvenInTransaction verifies the other
// half of the Task 13 contract: a condition ConditionToFilter cannot
// translate (a wildcard JsonPath, which is not pushdownable) still falls
// back to GetAll+in-memory match, even with an active transaction — the
// de-guard only removes the "in-tx ⇒ never pushdown" rule, it does not
// change the translate-failure fallback.
func TestSearch_TranslateFailure_FallsBackEvenInTransaction(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()

	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}

	// Register a schema declaring items[*].name as String so the type-directed
	// in-memory fallback resolves the array-element leaf and matches the
	// wildcard equality (FieldsMap key "$.items[*].name").
	saveModelWithArrayOfStringField(t, ctx, base, ref, "items", "name")
	saveEntity(t, ctx, base, ref, "e1", []byte(`{"items":[{"name":"gadget"},{"name":"widget"}]}`))
	saveEntity(t, ctx, base, ref, "e2", []byte(`{"items":[{"name":"gadget"},{"name":"other"}]}`))

	realStore, _ := base.EntityStore(ctx)

	// Searcher is available (so the "no Searcher" fallback branch isn't what's
	// exercised here) but must NOT be called: the wildcard path fails
	// ConditionToFilter translation before the searcher is ever invoked.
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

	// Active transaction — should not change the translate-failure fallback.
	tx := &spi.TransactionState{
		ID:           "test-tx-2",
		TenantID:     "tenant-1",
		SnapshotTime: time.Now(),
		ReadSet:      make(map[string]bool),
		WriteSet:     make(map[string]bool),
		Buffer:       make(map[string]*spi.Entity),
		Deletes:      make(map[string]bool),
	}
	txCtx := spi.WithTransaction(ctx, tx)

	// Wildcard JsonPath: ConditionToFilter rejects "[*]" as non-pushdownable
	// syntax, forcing the in-memory fallback; match.Prepare/(Prepared).Match
	// evaluates the wildcard against each element of "items" and matches e1
	// only.
	cond := &predicate.SimpleCondition{
		JsonPath:     "$.items[*].name",
		OperatorType: "EQUALS",
		Value:        "widget",
	}

	results, err := svc.Search(txCtx, ref, cond, search.SearchOptions{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if ses.searchCalls != 0 {
		t.Errorf("searchCalls = %d, want 0 (translate failure must not reach the Searcher)", ses.searchCalls)
	}
	if ses.getAllCalls != 1 {
		t.Errorf("getAllCalls = %d, want 1 (translate failure must use the GetAll fallback)", ses.getAllCalls)
	}
	if len(results) != 1 || results[0].Meta.ID != "e1" {
		t.Fatalf("expected 1 result (e1) from the in-memory fallback, got %d results", len(results))
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
		// EnsureModelRegistered + validateConditionPaths (for $.surname) +
		// validateConditionTypes + resolveSortKeys each call Get once.
		getQueue: []*spi.ModelDescriptor{desc, desc, desc, desc},
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

	// Limit: 10 — see TestSearchDelegatesToSearcher's comment: opts.Limit <=
	// 0 skips Searcher pushdown entirely now.
	_, err := svc.Search(ctx, ref, cond, search.SearchOptions{
		Limit:   10,
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
// configured cap. The cap check fires inside resolveSortKeys, before the
// model schema is consulted for sort-key typing.
func TestSearch_SortKeyCap_ReturnsError(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := factory.AsyncSearchStore(context.Background())
	// Cap set to 2 — sending 3 keys must be rejected.
	svc := search.NewSearchService(factory, uuids, searchStore).WithMaxSortKeys(2)

	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "item", ModelVersion: "1"}
	saveMinimalModel(t, ctx, factory, ref)

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
	saveMinimalModel(t, ctx, factory, ref)

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

	saveMinimalModel(t, ctx, factory, ref)
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

// iterableEntityStore wraps a real EntityStore and overrides Iterate. The
// streaming async executor calls spi.Iterable.Iterate directly for a
// translatable condition — it never reaches the plugin's Searcher.Search at
// all (see searcherEntityStore, used by the *synchronous* Search() tests
// above) — so this is the injection point for async-executor failure
// scenarios (panics, sentinel/classified errors) that the pre-streaming
// architecture used to drive through searchFn.
type iterableEntityStore struct {
	spi.EntityStore
	iterateFn func(ctx context.Context, model spi.ModelRef, filter spi.Filter, opts spi.IterateOptions) (spi.Iterator, error)
}

func (s *iterableEntityStore) Iterate(ctx context.Context, model spi.ModelRef, filter spi.Filter, opts spi.IterateOptions) (spi.Iterator, error) {
	return s.iterateFn(ctx, model, filter, opts)
}

// iterableFactory wraps a StoreFactory and returns an iterableEntityStore,
// delegating everything else (notably ModelStore, so schema/path validation
// runs against the real registered model) to the wrapped StoreFactory.
type iterableFactory struct {
	spi.StoreFactory
	entityStore *iterableEntityStore
}

func (f *iterableFactory) EntityStore(context.Context) (spi.EntityStore, error) {
	return f.entityStore, nil
}

// TestAsyncSearchJob_PanicIsRecovered is coverage for Task 7 (tx-lifecycle
// safety): the async search job goroutine runs on context.Background() with
// no HTTP handler above it to recover a panic — net/http's per-connection
// recover has nothing to do with a background goroutine, so an unrecovered
// panic here takes the whole process down (the search analogue of the
// scheduler's own dispatch goroutine, which already recovers). iterateFn
// panics to simulate a store-layer panic reaching the job; if the
// goroutine's own recover did not exist or did not fire, this test binary
// would already be gone rather than reaching the FAILED assertion below.
func TestAsyncSearchJob_PanicIsRecovered(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()

	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}
	saveModelWithFields(t, ctx, base, ref, map[string]schema.DataType{"name": schema.String})
	saveEntity(t, ctx, base, ref, "e1", []byte(`{"name":"Alice"}`))

	realStore, _ := base.EntityStore(ctx)
	ies := &iterableEntityStore{
		EntityStore: realStore,
		iterateFn: func(context.Context, spi.ModelRef, spi.Filter, spi.IterateOptions) (spi.Iterator, error) {
			panic("injected panic in async search execution")
		},
	}
	factory := &iterableFactory{StoreFactory: base, entityStore: ies}

	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := base.AsyncSearchStore(context.Background())
	svc := search.NewSearchService(factory, uuids, searchStore)

	cond := &predicate.SimpleCondition{
		JsonPath:     "$.name",
		OperatorType: "EQUALS",
		Value:        "Alice",
	}
	jobID, err := svc.SubmitAsync(ctx, ref, cond, search.SearchOptions{})
	if err != nil {
		t.Fatalf("SubmitAsync: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	var status search.SearchJobStatus
	for time.Now().Before(deadline) {
		status, err = svc.GetAsyncStatus(ctx, jobID)
		if err != nil {
			t.Fatalf("GetAsyncStatus: %v", err)
		}
		if status.Status == "FAILED" || status.Status == "SUCCESSFUL" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if status.Status != "FAILED" {
		t.Fatalf("expected FAILED after a panicking search, got %q (a job stuck RUNNING means the goroutine died without recording the failure)", status.Status)
	}

	// Gate 3 (output sanitization): the persisted failure record must not
	// leak the panic value or stack — only the generic message the recover
	// handler writes. Full detail belongs in the log, not the job record.
	job, err := searchStore.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.Error == "" {
		t.Error("expected a non-empty job error message")
	}
	if strings.Contains(job.Error, "injected panic") || strings.Contains(job.Error, "goroutine") {
		t.Errorf("job error message leaks panic/internal detail: %q", job.Error)
	}
}

// TestAsyncSearchJob_PanicMarksNodeUnhealthy holds the async-search goroutine
// to the same contract as the two request doors: a recovered panic latches the
// node's health flag false, so the node reports 503 on /health and /readyz and
// stops taking client traffic. A panic here is exactly as much evidence of
// unverified state as a panic in an HTTP or gRPC handler — it runs the same
// engine and store code — so recording the job FAILED and carrying on would
// leave the node serving from state nothing has checked.
func TestAsyncSearchJob_PanicMarksNodeUnhealthy(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()

	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}
	saveModelWithFields(t, ctx, base, ref, map[string]schema.DataType{"name": schema.String})
	saveEntity(t, ctx, base, ref, "e1", []byte(`{"name":"Alice"}`))

	realStore, _ := base.EntityStore(ctx)
	ies := &iterableEntityStore{
		EntityStore: realStore,
		iterateFn: func(context.Context, spi.ModelRef, spi.Filter, spi.IterateOptions) (spi.Iterator, error) {
			panic("injected panic in async search execution")
		},
	}
	factory := &iterableFactory{StoreFactory: base, entityStore: ies}

	healthFlag := &atomic.Bool{}
	healthFlag.Store(true)

	searchStore, _ := base.AsyncSearchStore(context.Background())
	svc := search.NewSearchService(factory, common.NewTestUUIDGenerator(), searchStore).
		WithHealthFlag(healthFlag)

	jobID, err := svc.SubmitAsync(ctx, ref, &predicate.SimpleCondition{
		JsonPath: "$.name", OperatorType: "EQUALS", Value: "Alice",
	}, search.SearchOptions{})
	if err != nil {
		t.Fatalf("SubmitAsync: %v", err)
	}
	awaitJobSettled(t, svc, ctx, jobID, "FAILED")

	if healthFlag.Load() {
		t.Fatal("health flag still true after a recovered panic in the async search goroutine — the node keeps taking traffic with unverified state")
	}
}

// TestAsyncSearchJob_SuccessLeavesNodeHealthy is the other direction: an async
// job that completes normally must not touch the flag. Without it, a recover
// handler that latched the flag unconditionally would still pass the test above.
func TestAsyncSearchJob_SuccessLeavesNodeHealthy(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()

	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}
	saveMinimalModel(t, ctx, factory, ref)
	saveEntity(t, ctx, factory, ref, "e1", []byte(`{"name":"Alice"}`))

	healthFlag := &atomic.Bool{}
	healthFlag.Store(true)

	searchStore, _ := factory.AsyncSearchStore(context.Background())
	svc := search.NewSearchService(factory, common.NewTestUUIDGenerator(), searchStore).
		WithHealthFlag(healthFlag)

	jobID, err := svc.SubmitAsync(ctx, ref, &predicate.SimpleCondition{
		JsonPath: "$.name", OperatorType: "EQUALS", Value: "Alice",
	}, search.SearchOptions{})
	if err != nil {
		t.Fatalf("SubmitAsync: %v", err)
	}
	awaitJobSettled(t, svc, ctx, jobID, "SUCCESSFUL")

	if !healthFlag.Load() {
		t.Fatal("health flag went false after a successful async search — the node took itself out of service for nothing")
	}
}

// awaitJobSettled polls until the job leaves RUNNING and asserts the terminal
// status, so the health-flag assertions above run against a finished goroutine.
func awaitJobSettled(t *testing.T, svc *search.SearchService, ctx context.Context, jobID string, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status, err := svc.GetAsyncStatus(ctx, jobID)
		if err != nil {
			t.Fatalf("GetAsyncStatus: %v", err)
		}
		if status.Status == "FAILED" || status.Status == "SUCCESSFUL" {
			if status.Status != want {
				t.Fatalf("job status = %q, want %q", status.Status, want)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job did not settle within the deadline; want %q", want)
}

// TestSearch_LimitExceedsMax verifies the service-layer defense-in-depth cap:
// limit > MaxPageSize is rejected with a 400 BAD_REQUEST AppError before any
// store access, and the unbounded case (limit < 0) is NOT rejected.
func TestSearch_LimitExceedsMax(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := factory.AsyncSearchStore(context.Background())
	svc := search.NewSearchService(factory, uuids, searchStore)

	ctx := tenantCtx("tenant-cap")
	ref := spi.ModelRef{EntityName: "cap-model", ModelVersion: "1"}
	saveMinimalModel(t, ctx, factory, ref)

	cond := &predicate.GroupCondition{Operator: "AND", Conditions: []predicate.Condition{}}

	t.Run("limit above max rejected", func(t *testing.T) {
		_, err := svc.Search(ctx, ref, cond, search.SearchOptions{Limit: 10001})
		if err == nil {
			t.Fatal("expected error for limit=10001, got nil")
		}
		var appErr *common.AppError
		if !errors.As(err, &appErr) {
			t.Fatalf("expected *common.AppError, got %T: %v", err, err)
		}
		if appErr.Status != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", appErr.Status)
		}
		if appErr.Code != common.ErrCodeBadRequest {
			t.Errorf("code = %q, want %q", appErr.Code, common.ErrCodeBadRequest)
		}
	})

	t.Run("limit at max accepted", func(t *testing.T) {
		_, err := svc.Search(ctx, ref, cond, search.SearchOptions{Limit: 10000})
		if err != nil {
			t.Fatalf("expected success for limit=10000, got: %v", err)
		}
	})

	t.Run("unbounded limit (negative) accepted", func(t *testing.T) {
		_, err := svc.Search(ctx, ref, cond, search.SearchOptions{Limit: -1})
		if err != nil {
			t.Fatalf("expected success for unbounded limit=-1, got: %v", err)
		}
	})
}

// TestSubmitAsync_LimitExceedsMax mirrors TestSearch_LimitExceedsMax for the
// async submit path: limit > MaxPageSize must be rejected synchronously (before
// any job is created), unbounded (limit<0) and boundary (limit==MaxPageSize)
// must be allowed.
func TestSubmitAsync_LimitExceedsMax(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := factory.AsyncSearchStore(context.Background())
	svc := search.NewSearchService(factory, uuids, searchStore)

	ctx := tenantCtx("tenant-async-cap")
	ref := spi.ModelRef{EntityName: "async-cap-model", ModelVersion: "1"}
	saveMinimalModel(t, ctx, factory, ref)

	cond := &predicate.GroupCondition{Operator: "AND", Conditions: []predicate.Condition{}}

	t.Run("limit above max rejected synchronously", func(t *testing.T) {
		jobID, err := svc.SubmitAsync(ctx, ref, cond, search.SearchOptions{Limit: 10001})
		if err == nil {
			t.Fatalf("expected error for limit=10001, got nil (jobID=%s)", jobID)
		}
		if jobID != "" {
			t.Errorf("expected empty job ID on rejection, got %q", jobID)
		}
		var appErr *common.AppError
		if !errors.As(err, &appErr) {
			t.Fatalf("expected *common.AppError, got %T: %v", err, err)
		}
		if appErr.Status != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", appErr.Status)
		}
		if appErr.Code != common.ErrCodeBadRequest {
			t.Errorf("code = %q, want %q", appErr.Code, common.ErrCodeBadRequest)
		}
	})

	t.Run("limit at max accepted", func(t *testing.T) {
		_, err := svc.SubmitAsync(ctx, ref, cond, search.SearchOptions{Limit: 10000})
		if err != nil {
			t.Fatalf("expected success for limit=10000, got: %v", err)
		}
	})

	t.Run("unbounded limit (negative) accepted", func(t *testing.T) {
		_, err := svc.SubmitAsync(ctx, ref, cond, search.SearchOptions{Limit: -1})
		if err != nil {
			t.Fatalf("expected success for unbounded limit=-1, got: %v", err)
		}
	})
}

// --- FieldsMap threading (Task 6b) ---

// wrapModelStoreCounter wraps a real spi.ModelStore and counts Get calls, so
// tests can observe whether a code path actually consults the schema.
type wrapModelStoreCounter struct {
	spi.ModelStore
	getCalls int
}

func (m *wrapModelStoreCounter) Get(ctx context.Context, ref spi.ModelRef) (*spi.ModelDescriptor, error) {
	m.getCalls++
	return m.ModelStore.Get(ctx, ref)
}

// wrapModelStoreCounterFactory wraps a StoreFactory and returns a
// wrapModelStoreCounter, delegating everything else (notably EntityStore, so
// the real memory Searcher is still exercised) to the wrapped StoreFactory.
type wrapModelStoreCounterFactory struct {
	spi.StoreFactory
	modelStore *wrapModelStoreCounter
}

func (f *wrapModelStoreCounterFactory) ModelStore(ctx context.Context) (spi.ModelStore, error) {
	return f.modelStore, nil
}

// TestSearch_ThreadsFieldsMapIntoConditionToFilter verifies that the
// Searcher pushdown branch of Search loads the model's FieldsMap and
// threads it into ConditionToFilter, rather than hardcoding nil.
//
// A LifecycleCondition addresses no data-field paths, so
// validateConditionPaths short-circuits without touching the ModelStore
// (extractFieldPaths returns empty — see path_validate.go), and an empty
// OrderBy makes resolveSortKeys return before touching it too. That isolates
// the count: with no OrderBy and a lifecycle-only condition, the only
// ModelStore.Get call before this task's change is EnsureModelRegistered's
// single lookup. Once Search's Searcher branch calls loadFieldsMap to build
// the fields argument for ConditionToFilter, a second Get call appears.
//
// (dataCoercion's routing effect is not separately observable via the
// result set today — classifyType never classifies a *data* field as
// spi.OrderTemporal until a future polymorphic-temporal-typing follow-up
// flips scalarClass; meta-field temporal stamping, exercised in Task 6,
// already works unconditionally of this fields argument. This test proves
// the wiring itself: the schema is loaded and handed to ConditionToFilter
// on the pushdown path, which is the forward-compatible behavior that
// follow-up needs.)
func TestSearch_ThreadsFieldsMapIntoConditionToFilter(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()

	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}

	saveMinimalModel(t, ctx, base, ref)
	saveEntity(t, ctx, base, ref, "e1", []byte(`{"name":"Alice"}`))

	baseMS, err := base.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	cms := &wrapModelStoreCounter{ModelStore: baseMS}
	factory := &wrapModelStoreCounterFactory{StoreFactory: base, modelStore: cms}

	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := base.AsyncSearchStore(context.Background())
	svc := search.NewSearchService(factory, uuids, searchStore)

	cond := &predicate.LifecycleCondition{
		Field:        "state",
		OperatorType: "EQUALS",
		Value:        "NEW",
	}

	// Precondition: EnsureModelRegistered is the only ModelStore.Get call a
	// lifecycle-only, no-OrderBy search makes before Search's pushdown
	// branch is threaded with a real FieldsMap.
	realStore, err := base.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	if _, ok := realStore.(spi.Searcher); !ok {
		t.Fatal("precondition: memory store expected to implement spi.Searcher")
	}

	_, err = svc.Search(ctx, ref, cond, search.SearchOptions{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if cms.getCalls < 2 {
		t.Errorf("ModelStore.Get calls = %d, want >= 2 (EnsureModelRegistered + Search's FieldsMap load for ConditionToFilter)", cms.getCalls)
	}
}

// TestSearch_LimitZeroSkipsSearcherPushdown pins the corrected contract:
// spi.Searcher.Search requires Limit >= 1 (Limit <= 0 is a documented
// contract violation every backend now enforces — see the Searcher doc
// comment: "the engine resolves the direct-search default before calling,
// so Search itself never needs to guess a bound"). opts.Limit <= 0 must
// never be forwarded to the Searcher. This fixture's store (searcherEntityStore)
// implements only spi.Searcher, not spi.Iterable, so Search's Limit<=0 branch
// has no pushdown capability to use and falls all the way through to the
// GetAll + in-memory-match fallback — see TestSearch_LimitZeroUsesIterablePushdown
// below for the (now more common) case where the store also implements
// spi.Iterable and Limit<=0 pushes down via Iterate instead of GetAll.
// Renamed from TestSearch_LimitZeroPassesUnboundedToSearcher, whose
// assertion (0 forwarded to the Searcher) was the pre-tightening contract.
func TestSearch_LimitZeroSkipsSearcherPushdown(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()
	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}
	saveModelWithFields(t, ctx, base, ref, map[string]schema.DataType{"name": schema.String})
	saveEntity(t, ctx, base, ref, "e1", []byte(`{"name":"Alice"}`))

	realStore, _ := base.EntityStore(ctx)
	ses := &searcherEntityStore{
		EntityStore: realStore,
		searchFn: func(_ context.Context, _ spi.Filter, opts spi.SearchOptions) ([]*spi.Entity, error) {
			t.Fatal("Searcher.Search must not be called for an unbounded (Limit<=0) request")
			return nil, nil
		},
	}
	factory := &searcherFactory{StoreFactory: base, entityStore: ses}
	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := base.AsyncSearchStore(context.Background())
	svc := search.NewSearchService(factory, uuids, searchStore)

	cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: "Alice"}
	results, err := svc.Search(ctx, ref, cond, search.SearchOptions{Limit: 0})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if ses.searchCalls != 0 {
		t.Errorf("searchCalls = %d, want 0 (Limit<=0 must skip Searcher pushdown)", ses.searchCalls)
	}
	if ses.getAllCalls != 1 {
		t.Errorf("getAllCalls = %d, want 1 (Limit<=0 must use the GetAll fallback)", ses.getAllCalls)
	}
	if len(results) != 1 || results[0].Meta.ID != "e1" {
		t.Fatalf("expected 1 result (e1) from the unbounded fallback, got %d", len(results))
	}
}

// searcherIterableEntityStore wraps a searcherEntityStore and additionally
// implements spi.Iterable, so a test can assert which pushdown capability
// Search's Limit<=0 branch reaches for when a store offers both — the
// question at the heart of the over-recording regression this file's
// TestSearch_LimitZeroUsesIterablePushdown pins: does Limit<=0 correctly
// prefer Iterate (per-yielded-entity TrackingRead recording), or does it
// (as the bug did) fall through to GetAll (unconditional whole-model
// recording, regardless of TrackingRead)?
type searcherIterableEntityStore struct {
	*searcherEntityStore
	iterateFn           func(ctx context.Context, model spi.ModelRef, filter spi.Filter, opts spi.IterateOptions) (spi.Iterator, error)
	iterateCalls        int
	capturedIterateOpts spi.IterateOptions
}

func (s *searcherIterableEntityStore) Iterate(ctx context.Context, model spi.ModelRef, filter spi.Filter, opts spi.IterateOptions) (spi.Iterator, error) {
	s.iterateCalls++
	s.capturedIterateOpts = opts
	return s.iterateFn(ctx, model, filter, opts)
}

// searcherIterableFactory wraps a StoreFactory and returns a
// searcherIterableEntityStore, delegating everything else (notably
// ModelStore, so schema/path validation runs against the real registered
// model) to the wrapped StoreFactory.
type searcherIterableFactory struct {
	spi.StoreFactory
	entityStore *searcherIterableEntityStore
}

func (f *searcherIterableFactory) EntityStore(context.Context) (spi.EntityStore, error) {
	return f.entityStore, nil
}

// TestSearch_LimitZeroUsesIterablePushdown is the regression test for the
// over-recording bug: Search's Limit<=0 branch used to skip ALL pushdown
// capability and fall through unconditionally to GetAll + in-memory-match,
// which records every entity in the model into the transaction's read-set
// regardless of TrackingRead (see the GetAll fallback's own doc comment).
// Once a store also implements spi.Iterable, Limit<=0 must prefer Iterate
// over that fallback: Iterate's TrackingRead gate records only the entities
// it actually yields, per entity, as it streams — matching Search's
// contract ("records only entities it returns, only when tracking is on")
// even though the request can't be bounded. This is asserted here purely as
// a call-routing/opts-threading question (no store implements both
// Searcher and Iterable in production without also honouring both
// contracts correctly — that per-entity correctness is covered where it's
// implemented, e.g. plugins/postgres's TestIterateTx_TrackingReadRecordsOnlyYieldedIds).
func TestSearch_LimitZeroUsesIterablePushdown(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()
	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}
	saveModelWithFields(t, ctx, base, ref, map[string]schema.DataType{"name": schema.String})
	saveEntity(t, ctx, base, ref, "e1", []byte(`{"name":"Alice"}`))
	saveEntity(t, ctx, base, ref, "e2", []byte(`{"name":"Bob"}`))

	realStore, _ := base.EntityStore(ctx)
	realIterable, ok := realStore.(spi.Iterable)
	if !ok {
		t.Fatal("memory EntityStore must implement spi.Iterable for this test to be meaningful")
	}
	ses := &searcherEntityStore{
		EntityStore: realStore,
		searchFn: func(_ context.Context, _ spi.Filter, opts spi.SearchOptions) ([]*spi.Entity, error) {
			t.Fatal("Searcher.Search must not be called for an unbounded (Limit<=0) request")
			return nil, nil
		},
	}
	sies := &searcherIterableEntityStore{
		searcherEntityStore: ses,
		iterateFn:           realIterable.Iterate,
	}
	factory := &searcherIterableFactory{StoreFactory: base, entityStore: sies}
	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := base.AsyncSearchStore(context.Background())
	svc := search.NewSearchService(factory, uuids, searchStore)

	cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: "Alice"}
	results, err := svc.Search(ctx, ref, cond, search.SearchOptions{Limit: 0, TrackingRead: true})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if ses.searchCalls != 0 {
		t.Errorf("searchCalls = %d, want 0 (Limit<=0 must not use Searcher)", ses.searchCalls)
	}
	if ses.getAllCalls != 0 {
		t.Errorf("getAllCalls = %d, want 0 (Limit<=0 must prefer Iterate over the GetAll fallback when the store implements spi.Iterable)", ses.getAllCalls)
	}
	if sies.iterateCalls != 1 {
		t.Fatalf("iterateCalls = %d, want 1", sies.iterateCalls)
	}
	if !sies.capturedIterateOpts.TrackingRead {
		t.Error("IterateOptions.TrackingRead = false, want true (opts.TrackingRead must thread through)")
	}
	if sies.capturedIterateOpts.PointInTime != nil {
		t.Errorf("IterateOptions.PointInTime = %v, want nil", sies.capturedIterateOpts.PointInTime)
	}
	if len(results) != 1 || results[0].Meta.ID != "e1" {
		t.Fatalf("expected exactly 1 result (e1), got %d (%v)", len(results), results)
	}
}

// newStubSearcherService builds a SearchService backed by a memory
// StoreFactory whose EntityStore.Search is replaced by fn, with a minimal
// "person" model already registered. Shared by the sentinel-mapping tests
// below (and reusable by future Searcher-stub tests).
func newStubSearcherService(t *testing.T, fn func(context.Context, spi.Filter, spi.SearchOptions) ([]*spi.Entity, error)) (*search.SearchService, context.Context, spi.ModelRef) {
	t.Helper()
	base := memory.NewStoreFactory()
	t.Cleanup(func() { base.Close() })
	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}
	saveMinimalModel(t, ctx, base, ref)
	realStore, _ := base.EntityStore(ctx)
	ses := &searcherEntityStore{EntityStore: realStore, searchFn: fn}
	factory := &searcherFactory{StoreFactory: base, entityStore: ses}
	searchStore, _ := base.AsyncSearchStore(context.Background())
	return search.NewSearchService(factory, common.NewTestUUIDGenerator(), searchStore), ctx, ref
}

func TestSearch_SearcherResultLimitSentinel_MapsTo400(t *testing.T) {
	svc, ctx, ref := newStubSearcherService(t, func(_ context.Context, _ spi.Filter, _ spi.SearchOptions) ([]*spi.Entity, error) {
		return nil, fmt.Errorf("plugin detail: %w", spi.ErrSearchResultLimitExceeded)
	})
	cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: "Alice"}
	_, err := svc.Search(ctx, ref, cond, search.SearchOptions{Limit: 10})

	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("want *common.AppError, got %T: %v", err, err)
	}
	if appErr.Status != http.StatusBadRequest || appErr.Code != common.ErrCodeSearchResultLimit {
		t.Errorf("got %d/%q, want 400/%s", appErr.Status, appErr.Code, common.ErrCodeSearchResultLimit)
	}
	if !errors.Is(err, spi.ErrSearchResultLimitExceeded) {
		t.Errorf("errors.Is(err, ErrSearchResultLimitExceeded) = false; WithCause must preserve the sentinel")
	}
}

func TestSearch_SearcherScanBudgetSentinel_MapsTo400(t *testing.T) {
	svc, ctx, ref := newStubSearcherService(t, func(_ context.Context, _ spi.Filter, _ spi.SearchOptions) ([]*spi.Entity, error) {
		return nil, fmt.Errorf("examined N rows: %w", spi.ErrScanBudgetExhausted)
	})
	cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: "Alice"}
	_, err := svc.Search(ctx, ref, cond, search.SearchOptions{Limit: 10})

	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("want *common.AppError, got %T: %v", err, err)
	}
	if appErr.Status != http.StatusBadRequest || appErr.Code != common.ErrCodeScanBudgetExhausted {
		t.Errorf("got %d/%q, want 400/%s", appErr.Status, appErr.Code, common.ErrCodeScanBudgetExhausted)
	}
}

// --- GetAll + in-memory fallback bounded-or-fail tests ---

// newFallbackFixture builds a SearchService whose EntityStore is wrapped so
// it does NOT implement spi.Searcher (the nonSearcherEntityStore/
// nonSearcherFactory pair defined above, also used by
// TestSearchFallsBackWhenNotSearcher), then registers n entities that all
// satisfy untranslatableCondition(t)'s always-true first branch. Every Search call
// against this fixture is forced through the GetAll + in-memory match
// branch — there is no Searcher to even attempt pushdown against.
func newFallbackFixture(t *testing.T, n int) (*search.SearchService, context.Context, spi.ModelRef) {
	t.Helper()
	base := memory.NewStoreFactory()
	t.Cleanup(func() { base.Close() })

	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "item", ModelVersion: "1"}

	saveModelWithValAndItemsArray(t, ctx, base, ref)
	for i := 0; i < n; i++ {
		saveEntity(t, ctx, base, ref, fmt.Sprintf("e%d", i), []byte(fmt.Sprintf(`{"val":%d}`, i)))
	}

	realStore, _ := base.EntityStore(ctx)
	nonSearcher := &nonSearcherEntityStore{EntityStore: realStore}
	factory := &nonSearcherFactory{StoreFactory: base, entityStore: nonSearcher}

	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := base.AsyncSearchStore(context.Background())
	svc := search.NewSearchService(factory, uuids, searchStore)

	return svc, ctx, ref
}

// untranslatableCondition returns a condition tree that ConditionToFilter
// cannot push down, so any request carrying it always travels through the
// GetAll + in-memory fallback — independent of whether the store also happens
// to lack a Searcher, as in newFallbackFixture.
// TestSearch_FallbackBranchIsBounded_TranslateFailureRoute pins that
// translate-failure property directly, against a real Searcher-implementing
// store, rather than relying on newFallbackFixture's belt-and-braces (no
// Searcher AND untranslatable) setup to demonstrate it.
//
// The untranslatable member is a wildcard array path: stripDollarDot rejects
// the "[" as non-pushdownable syntax, and groupToFilter propagates that
// failure, so the whole tree fails translation. It is a declared field
// ($.items[*].name in saveModelWithValAndItemsArray) so the tree still clears
// pre-execution path validation, and it is OR'd with an always-true
// SimpleCondition ($.val > -1, true for every entity the fixtures save) so the
// fallback's match loop still yields every entity and reaches the bound check
// these tests exist to exercise.
//
// This used to be a FunctionCondition, which is untranslatable for the same
// reason. It no longer can be: a function clause is a criterion shape and
// ValidateCondition now rejects it at the search boundary, so it never reaches
// translation at all (see function_condition_reject_test.go).
func untranslatableCondition(t *testing.T) predicate.Condition {
	t.Helper()
	return &predicate.GroupCondition{
		Operator: "OR",
		Conditions: []predicate.Condition{
			&predicate.SimpleCondition{
				JsonPath:     "$.val",
				OperatorType: "GREATER_THAN",
				Value:        float64(-1),
			},
			&predicate.SimpleCondition{
				JsonPath:     "$.items[*].name",
				OperatorType: "EQUALS",
				Value:        "never-present",
			},
		},
	}
}

// The GetAll + in-memory match fallback (reached when a condition is not
// translatable to a pushdown filter — a function condition never is) must be
// bounded-or-fail too. Otherwise a translate-failure request silently
// truncates while the pushdown path 400s, which is the same divergence inside
// one backend that this change removes across backends.
func TestSearch_FallbackBranchIsBounded(t *testing.T) {
	svc, ctx, ref := newFallbackFixture(t, 3) // 3 matching entities, no Searcher
	_, err := svc.Search(ctx, ref, untranslatableCondition(t), search.SearchOptions{Limit: 2})

	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("got err %v, want *common.AppError", err)
	}
	if appErr.Status != http.StatusBadRequest || appErr.Code != common.ErrCodeSearchResultLimit {
		t.Fatalf("got %d/%s, want 400/%s", appErr.Status, appErr.Code, common.ErrCodeSearchResultLimit)
	}
	// Same sentinel identity as the Searcher-pushdown branch
	// (TestSearch_SearcherResultLimitSentinel_MapsTo400): the two
	// bounded-or-fail paths must be indistinguishable to errors.Is callers.
	if !errors.Is(err, spi.ErrSearchResultLimitExceeded) {
		t.Errorf("errors.Is(err, ErrSearchResultLimitExceeded) = false; WithCause must preserve the sentinel")
	}
}

// TestSearch_FallbackBranchUnboundedReturnsAll is table-driven over both
// unbounded sentinels: 0 (the "client omitted"/async-submit case) and -1
// (the sentinel a scoped conditional delete relies on at
// internal/domain/entity/service.go:947 to select the complete match set —
// silently truncating it would be data loss). Every -1 call site elsewhere
// in the test suite travels the Searcher pushdown path, never this
// fallback branch, so without -1 here a refactor of `opts.Limit > 0` into
// `opts.Limit != 0` would keep every existing test green while silently
// truncating -1 on this branch specifically.
func TestSearch_FallbackBranchUnboundedReturnsAll(t *testing.T) {
	for _, limit := range []int{0, -1} {
		t.Run(fmt.Sprintf("limit=%d", limit), func(t *testing.T) {
			svc, ctx, ref := newFallbackFixture(t, 3)
			got, err := svc.Search(ctx, ref, untranslatableCondition(t), search.SearchOptions{Limit: limit})
			if err != nil {
				t.Fatalf("limit %d must be unbounded: unexpected err %v", limit, err)
			}
			if len(got) != 3 {
				t.Fatalf("got %d, want 3", len(got))
			}
		})
	}
}

// TestSearch_FallbackBranchIsBounded_TranslateFailureRoute pins the second,
// independent route into the GetAll fallback that newFallbackFixture does
// not isolate: the Search doc comment states the fallback is reached either
// when the store has no Searcher, or when a condition simply fails
// ConditionToFilter translation — but newFallbackFixture combines both (a
// non-Searcher store AND an untranslatable condition), so neither mechanism
// is individually pinned there. This test uses the real memory
// StoreFactory, whose EntityStore DOES implement spi.Searcher, wrapped only
// to observe calls (not to suppress the interface) — so a passing result
// here proves translate failure alone, independent of Searcher
// availability, routes to the bounded fallback and is bounded there too.
func TestSearch_FallbackBranchIsBounded_TranslateFailureRoute(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()

	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "item", ModelVersion: "1"}

	saveModelWithValAndItemsArray(t, ctx, base, ref)
	for i := 0; i < 3; i++ {
		saveEntity(t, ctx, base, ref, fmt.Sprintf("e%d", i), []byte(fmt.Sprintf(`{"val":%d}`, i)))
	}

	realStore, _ := base.EntityStore(ctx)
	// searcherEntityStore DOES implement spi.Searcher (it wraps the real,
	// Searcher-capable memory store) — its searchFn returns an
	// obviously-wrong sentinel result, so getAllCalls==1/searchCalls==0
	// below proves translate failure, not a missing Searcher, drove the
	// fallback.
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

	_, err := svc.Search(ctx, ref, untranslatableCondition(t), search.SearchOptions{Limit: 2})

	if ses.searchCalls != 0 {
		t.Errorf("searchCalls = %d, want 0 (translate failure must not reach the Searcher)", ses.searchCalls)
	}
	if ses.getAllCalls != 1 {
		t.Errorf("getAllCalls = %d, want 1 (translate failure must use the GetAll fallback)", ses.getAllCalls)
	}

	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("got err %v, want *common.AppError", err)
	}
	if appErr.Status != http.StatusBadRequest || appErr.Code != common.ErrCodeSearchResultLimit {
		t.Fatalf("got %d/%s, want 400/%s", appErr.Status, appErr.Code, common.ErrCodeSearchResultLimit)
	}
}

// ---------------------------------------------------------------------------
// What the async job record says when the search failed
// ---------------------------------------------------------------------------

// ceilingErr is a storage-plugin error carrying the marker a backend sets when
// the async-search scan exceeded that backend's own search ceiling. Declared
// here as an ordinary error type because the marker is matched with errors.As
// on an interface — no plugin import, and any backend can opt in.
type ceilingErr struct{ cause error }

func (e *ceilingErr) Error() string               { return "search query: " + e.cause.Error() }
func (e *ceilingErr) Unwrap() error               { return e.cause }
func (e *ceilingErr) SearchCeilingExceeded() bool { return true }

// runFailingAsyncJob submits an async search whose execution fails with
// searchErr and returns the persisted job record once the job is terminal.
func runFailingAsyncJob(t *testing.T, searchErr error) *spi.SearchJob {
	t.Helper()
	base := memory.NewStoreFactory()
	t.Cleanup(func() { _ = base.Close() })

	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}
	saveModelWithFields(t, ctx, base, ref, map[string]schema.DataType{"name": schema.String})
	saveEntity(t, ctx, base, ref, "e1", []byte(`{"name":"Alice"}`))

	realStore, _ := base.EntityStore(ctx)
	// The streaming async executor calls Iterate directly for a
	// translatable condition (see iterableEntityStore's doc comment) — the
	// injection point for an async-job-failure scenario is Iterate
	// returning the error, not Searcher.Search.
	ies := &iterableEntityStore{
		EntityStore: realStore,
		iterateFn: func(context.Context, spi.ModelRef, spi.Filter, spi.IterateOptions) (spi.Iterator, error) {
			return nil, searchErr
		},
	}
	factory := &iterableFactory{StoreFactory: base, entityStore: ies}

	searchStore, _ := base.AsyncSearchStore(context.Background())
	svc := search.NewSearchService(factory, common.NewTestUUIDGenerator(), searchStore)

	cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: "Alice"}
	jobID, err := svc.SubmitAsync(ctx, ref, cond, search.SearchOptions{})
	if err != nil {
		t.Fatalf("SubmitAsync: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, err := svc.GetAsyncStatus(ctx, jobID)
		if err != nil {
			t.Fatalf("GetAsyncStatus: %v", err)
		}
		if st.Status != "RUNNING" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	job, err := searchStore.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.Status != "FAILED" {
		t.Fatalf("job status = %q, want FAILED", job.Status)
	}
	return job
}

// TestAsyncSearchJob_SearchCeiling_RecordsFixedMessage — the job record is the
// one place a failure message is persisted and served back, so the backend's
// search ceiling firing must land there as a fixed, non-revealing string naming
// the ceiling and nothing else. The driver's own text stays in the log.
func TestAsyncSearchJob_SearchCeiling_RecordsFixedMessage(t *testing.T) {
	job := runFailingAsyncJob(t, &ceilingErr{
		cause: errors.New("ERROR: canceling statement due to statement timeout (SQLSTATE 57014)"),
	})

	if !strings.Contains(job.Error, "search statement ceiling") {
		t.Fatalf("job error %q does not name the ceiling the caller hit", job.Error)
	}
	for _, leak := range []string{"pgx", "SELECT", "SQLSTATE", "57014", "statement timeout", "host=", "password"} {
		if strings.Contains(job.Error, leak) {
			t.Fatalf("job error leaked internals (%q): %s", leak, job.Error)
		}
	}
}

// TestAsyncSearchJob_StorageError_IsNotPersistedVerbatim — Gate 3. Anything the
// storage layer says about ITSELF (SQL, SQLSTATEs, connection detail) is
// operator information; the job record is caller-facing.
func TestAsyncSearchJob_StorageError_IsNotPersistedVerbatim(t *testing.T) {
	job := runFailingAsyncJob(t, fmt.Errorf("search query: %w",
		errors.New(`ERROR: relation "entity_versions" does not exist (SQLSTATE 42P01), host=db.internal user=cyoda`)))

	if job.Error == "" {
		t.Fatal("a failed job recorded no message at all")
	}
	for _, leak := range []string{"SQLSTATE", "42P01", "entity_versions", "host=", "user="} {
		if strings.Contains(job.Error, leak) {
			t.Fatalf("job error leaked internals (%q): %s", leak, job.Error)
		}
	}
}

// TestAsyncSearchJob_ClientErrorIsPreservedVerbatim — the counterweight. A
// classified 4xx is the caller's own mistake and its text is already
// client-safe, so sanitizing must not flatten it into a generic message the
// caller cannot act on.
func TestAsyncSearchJob_ClientErrorIsPreservedVerbatim(t *testing.T) {
	appErr := common.Operational(http.StatusBadRequest, common.ErrCodeInvalidCondition,
		"invalid regex pattern in condition")
	job := runFailingAsyncJob(t, appErr)

	if job.Error != appErr.Error() {
		t.Fatalf("job error = %q, want the classified client error %q", job.Error, appErr.Error())
	}
}
