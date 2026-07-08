package search_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// refreshingModelStore is a ModelStore fake used to drive the search
// service's pre-execution path-validation logic. Get returns the head
// of getQueue; RefreshAndGet returns the head of refreshQueue. Both
// counters are observable so tests can assert refresh behaviour.
type refreshingModelStore struct {
	mu           sync.Mutex
	getQueue     []*spi.ModelDescriptor
	refreshQueue []*spi.ModelDescriptor
	getCount     int
	refreshCount int
}

func (s *refreshingModelStore) Get(_ context.Context, _ spi.ModelRef) (*spi.ModelDescriptor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.getCount++
	if len(s.getQueue) == 0 {
		// Drain to last refresh result so post-validation lookups still resolve.
		if len(s.refreshQueue) > 0 {
			return s.refreshQueue[0], nil
		}
		return nil, nil
	}
	d := s.getQueue[0]
	s.getQueue = s.getQueue[1:]
	return d, nil
}

func (s *refreshingModelStore) RefreshAndGet(_ context.Context, _ spi.ModelRef) (*spi.ModelDescriptor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshCount++
	if len(s.refreshQueue) == 0 {
		return nil, nil
	}
	d := s.refreshQueue[0]
	s.refreshQueue = s.refreshQueue[1:]
	return d, nil
}

func (s *refreshingModelStore) Save(context.Context, *spi.ModelDescriptor) error { return nil }
func (s *refreshingModelStore) GetAll(context.Context) ([]spi.ModelRef, error)   { return nil, nil }
func (s *refreshingModelStore) Delete(context.Context, spi.ModelRef) error       { return nil }
func (s *refreshingModelStore) Lock(context.Context, spi.ModelRef) error         { return nil }
func (s *refreshingModelStore) Unlock(context.Context, spi.ModelRef) error       { return nil }
func (s *refreshingModelStore) IsLocked(context.Context, spi.ModelRef) (bool, error) {
	return true, nil
}
func (s *refreshingModelStore) SetChangeLevel(context.Context, spi.ModelRef, spi.ChangeLevel) error {
	return nil
}
func (s *refreshingModelStore) ExtendSchema(context.Context, spi.ModelRef, spi.SchemaDelta) error {
	return nil
}

var _ spi.ModelStore = (*refreshingModelStore)(nil)

func (s *refreshingModelStore) RefreshCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshCount
}

// modelStoreFactory wraps an inner StoreFactory and returns a fixed
// ModelStore (the refreshingModelStore under test). EntityStore and other
// methods delegate to the inner factory so fallback search still works.
type modelStoreFactory struct {
	spi.StoreFactory
	modelStore spi.ModelStore
}

func (f *modelStoreFactory) ModelStore(_ context.Context) (spi.ModelStore, error) {
	return f.modelStore, nil
}

// buildSearchDescriptor constructs a LOCKED descriptor whose schema declares
// the given top-level string fields. Mirrors the helper in
// internal/domain/entity/handler_validate_refresh_test.go.
func buildSearchDescriptor(t *testing.T, ref spi.ModelRef, fields ...string) *spi.ModelDescriptor {
	t.Helper()
	node := schema.NewObjectNode()
	for _, f := range fields {
		node.SetChild(f, schema.NewLeafNode(schema.String))
	}
	raw, err := schema.Marshal(node)
	if err != nil {
		t.Fatalf("schema.Marshal: %v", err)
	}
	return &spi.ModelDescriptor{
		Ref:    ref,
		State:  spi.ModelLocked,
		Schema: raw,
	}
}

// TestSearch_StaleSchema_RefreshesOnceAndSucceeds verifies that a search
// referencing a field absent from the cached schema but present in the
// authoritative (post-RefreshAndGet) schema triggers exactly one refresh
// and then succeeds. This is the issue-#77 contract.
func TestSearch_StaleSchema_RefreshesOnceAndSucceeds(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()

	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}

	// Save a single entity carrying field "z". The fallback search will
	// match it once validation accepts the path.
	saveEntity(t, ctx, base, ref, "e1", []byte(`{"z":"hello"}`))

	stale := buildSearchDescriptor(t, ref, "a")
	fresh := buildSearchDescriptor(t, ref, "a", "z")
	ms := &refreshingModelStore{
		// EnsureModelRegistered consumes the first Get; loadFieldsMap gets the second.
		getQueue:     []*spi.ModelDescriptor{stale, stale},
		refreshQueue: []*spi.ModelDescriptor{fresh},
	}
	factory := &modelStoreFactory{StoreFactory: base, modelStore: ms}

	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := base.AsyncSearchStore(context.Background())
	svc := search.NewSearchService(factory, uuids, searchStore)

	cond := &predicate.SimpleCondition{
		JsonPath:     "$.z",
		OperatorType: "EQUALS",
		Value:        "hello",
	}

	results, err := svc.Search(ctx, ref, cond, search.SearchOptions{})
	if err != nil {
		t.Fatalf("expected search to succeed after one refresh, got error: %v", err)
	}
	if len(results) != 1 || results[0].Meta.ID != "e1" {
		t.Fatalf("expected 1 result (e1), got %d results", len(results))
	}
	if got := ms.RefreshCount(); got != 1 {
		t.Errorf("expected exactly 1 RefreshAndGet call, got %d", got)
	}
}

// TestSearch_TrulyMissingPath_FourxxAfterOneRefresh verifies that when the
// referenced field is absent from BOTH the cached and the authoritative
// schemas, Search rejects the request with a 4xx and triggers refresh at
// most once (no unbounded refresh loop).
func TestSearch_TrulyMissingPath_FourxxAfterOneRefresh(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()

	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}

	stale := buildSearchDescriptor(t, ref, "a")
	stillStale := buildSearchDescriptor(t, ref, "a")
	ms := &refreshingModelStore{
		// EnsureModelRegistered consumes the first Get; loadFieldsMap gets the second.
		getQueue:     []*spi.ModelDescriptor{stale, stale},
		refreshQueue: []*spi.ModelDescriptor{stillStale},
	}
	factory := &modelStoreFactory{StoreFactory: base, modelStore: ms}

	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := base.AsyncSearchStore(context.Background())
	svc := search.NewSearchService(factory, uuids, searchStore)

	cond := &predicate.SimpleCondition{
		JsonPath:     "$.z",
		OperatorType: "EQUALS",
		Value:        "hello",
	}

	_, err := svc.Search(ctx, ref, cond, search.SearchOptions{})
	if err == nil {
		t.Fatalf("expected validation error for truly-missing path, got nil")
	}
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *common.AppError, got %T: %v", err, err)
	}
	if appErr.Status < 400 || appErr.Status >= 500 {
		t.Errorf("expected 4xx status, got %d", appErr.Status)
	}
	if appErr.Code != common.ErrCodeInvalidFieldPath {
		t.Errorf("expected errorCode %q, got %q", common.ErrCodeInvalidFieldPath, appErr.Code)
	}
	if got := ms.RefreshCount(); got != 1 {
		t.Errorf("expected exactly 1 RefreshAndGet call (bounded), got %d", got)
	}
}
