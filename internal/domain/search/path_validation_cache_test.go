package search_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// countingModelStore extends refreshingModelStore semantics but tracks
// concurrent counters for both Get and RefreshAndGet so the negative-
// cache test can assert the inner-store call shape under serial and
// concurrent flooding.
type countingModelStore struct {
	mu           sync.Mutex
	descriptor   *spi.ModelDescriptor
	getCount     atomic.Int64
	refreshCount atomic.Int64
}

func (s *countingModelStore) Get(_ context.Context, _ spi.ModelRef) (*spi.ModelDescriptor, error) {
	s.getCount.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.descriptor, nil
}

func (s *countingModelStore) RefreshAndGet(_ context.Context, _ spi.ModelRef) (*spi.ModelDescriptor, error) {
	s.refreshCount.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.descriptor, nil
}

func (s *countingModelStore) Save(context.Context, *spi.ModelDescriptor) error     { return nil }
func (s *countingModelStore) GetAll(context.Context) ([]spi.ModelRef, error)       { return nil, nil }
func (s *countingModelStore) Delete(context.Context, spi.ModelRef) error           { return nil }
func (s *countingModelStore) Lock(context.Context, spi.ModelRef) error             { return nil }
func (s *countingModelStore) Unlock(context.Context, spi.ModelRef) error           { return nil }
func (s *countingModelStore) IsLocked(context.Context, spi.ModelRef) (bool, error) { return true, nil }
func (s *countingModelStore) SetChangeLevel(context.Context, spi.ModelRef, spi.ChangeLevel) error {
	return nil
}
func (s *countingModelStore) ExtendSchema(context.Context, spi.ModelRef, spi.SchemaDelta) error {
	return nil
}

var _ spi.ModelStore = (*countingModelStore)(nil)

// TestSearch_NegativeCache_CollapsesSerialFloodForUnknownPath asserts that
// a serial flood of validation requests for the same unknown field path
// hits the path-validation inner store at most once for the initial Get and
// once for the bounded RefreshAndGet. EnsureModelRegistered fires one Get
// per Search call (it is not cached), so the total Get count is
// reqCount+1. Without the negative cache every request would fire its own
// Get+RefreshAndGet pair for path validation, amplifying client error into
// a denial-of-service vector against the schema cache.
func TestSearch_NegativeCache_CollapsesSerialFloodForUnknownPath(t *testing.T) {
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}
	desc := buildSearchDescriptor(t, ref, "a")
	ms := &countingModelStore{descriptor: desc}

	base := memory.NewStoreFactory()
	defer base.Close()
	factory := &modelStoreFactory{StoreFactory: base, modelStore: ms}

	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := base.AsyncSearchStore(context.Background())

	cache := search.NewPathValidationCache()
	svc := search.NewSearchService(factory, uuids, searchStore).
		WithPathValidationCache(cache)

	ctx := tenantCtx("tenant-1")
	cond := &predicate.SimpleCondition{
		JsonPath:     "$.unknown",
		OperatorType: "EQUALS",
		Value:        "x",
	}

	const reqCount = 100
	for i := 0; i < reqCount; i++ {
		_, err := svc.Search(ctx, ref, cond, search.SearchOptions{})
		if err == nil {
			t.Fatalf("iter %d: expected error for unknown path, got nil", i)
		}
	}

	// EnsureModelRegistered fires 1 Get per call; path validation fires 1 Get
	// only on the first call (then the negative cache short-circuits it).
	if got := ms.getCount.Load(); got > int64(reqCount)+1 {
		t.Errorf("inner Get count: want <=%d (EnsureModelRegistered per call + 1 path-validation load), got %d", reqCount+1, got)
	}
	if got := ms.refreshCount.Load(); got > 1 {
		t.Errorf("inner RefreshAndGet count: want <=1 (bounded), got %d", got)
	}
}

// TestSearch_NegativeCache_InvalidatedOnSchemaChange asserts that after
// InvalidateRef fires for a (tenant, ref), the previously-cached
// negative entry is dropped: the next validation re-consults the inner
// store. Required to preserve the issue #77 "fresh-after-extend"
// contract — a peer extending the model must not be hidden behind a
// stale negative cache.
//
// Issue #174 — pre-fix the cache subscribed to a gossip broadcaster
// directly, so this contract did not hold on single-node deployments.
// After the redesign, app.go wires modelcache.SubscribeLocal to
// pathValidationCache.InvalidateRef so every invalidation (local OR
// gossip) reaches the cache regardless of cluster topology. Tests
// drive InvalidateRef directly, decoupled from the wiring layer.
func TestSearch_NegativeCache_InvalidatedOnSchemaChange(t *testing.T) {
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}
	desc := buildSearchDescriptor(t, ref, "a")
	ms := &countingModelStore{descriptor: desc}

	base := memory.NewStoreFactory()
	defer base.Close()
	factory := &modelStoreFactory{StoreFactory: base, modelStore: ms}

	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := base.AsyncSearchStore(context.Background())

	cache := search.NewPathValidationCache()
	svc := search.NewSearchService(factory, uuids, searchStore).
		WithPathValidationCache(cache)

	ctx := tenantCtx("tenant-1")
	cond := &predicate.SimpleCondition{
		JsonPath:     "$.unknown",
		OperatorType: "EQUALS",
		Value:        "x",
	}

	// First request: warms the negative cache.
	if _, err := svc.Search(ctx, ref, cond, search.SearchOptions{}); err == nil {
		t.Fatalf("expected error on first call")
	}
	getsBefore := ms.getCount.Load()
	refreshesBefore := ms.refreshCount.Load()

	// Second request: EnsureModelRegistered fires one Get, but path validation
	// should hit the negative cache (no additional inner store calls).
	if _, err := svc.Search(ctx, ref, cond, search.SearchOptions{}); err == nil {
		t.Fatalf("expected error on second call")
	}
	if ms.getCount.Load() > getsBefore+1 || ms.refreshCount.Load() != refreshesBefore {
		t.Fatalf("expected only EnsureModelRegistered's Get (no path-validation calls) between warmup and invalidation, got Δgets=%d Δrefreshes=%d",
			ms.getCount.Load()-getsBefore, ms.refreshCount.Load()-refreshesBefore)
	}

	// Invalidate the (tenant, ref) — the negative cache must drop the
	// entry so the next request goes back to the inner store.
	cache.InvalidateRef("tenant-1", ref)

	// Third request: cache invalidated, inner Get must fire again.
	if _, err := svc.Search(ctx, ref, cond, search.SearchOptions{}); err == nil {
		t.Fatalf("expected error on third call")
	}
	if ms.getCount.Load() <= getsBefore {
		t.Errorf("expected inner Get to be re-issued after invalidation; getCount stayed at %d", ms.getCount.Load())
	}
}

// TestSearch_NegativeCache_ConcurrentMissAndInvalidation drives 100
// concurrent validation requests interleaved with a single InvalidateRef.
// The contract: no panics, no races, all requests return the expected
// 4xx error. Run with -race once before PR.
func TestSearch_NegativeCache_ConcurrentMissAndInvalidation(t *testing.T) {
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}
	desc := buildSearchDescriptor(t, ref, "a")
	ms := &countingModelStore{descriptor: desc}

	base := memory.NewStoreFactory()
	defer base.Close()
	factory := &modelStoreFactory{StoreFactory: base, modelStore: ms}

	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := base.AsyncSearchStore(context.Background())

	cache := search.NewPathValidationCache()
	svc := search.NewSearchService(factory, uuids, searchStore).
		WithPathValidationCache(cache)

	ctx := tenantCtx("tenant-1")
	cond := &predicate.SimpleCondition{
		JsonPath:     "$.unknown",
		OperatorType: "EQUALS",
		Value:        "x",
	}

	var wg sync.WaitGroup
	const concurrency = 100
	wg.Add(concurrency)
	errs := make(chan error, concurrency)
	for i := 0; i < concurrency; i++ {
		go func(i int) {
			defer wg.Done()
			if i == concurrency/2 {
				cache.InvalidateRef("tenant-1", ref)
			}
			_, err := svc.Search(ctx, ref, cond, search.SearchOptions{})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		if err == nil {
			t.Errorf("expected error for unknown path, got nil")
		}
	}
}
