package search_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/internal/platformclock"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// countingModelStore serves one fixed descriptor and counts Get and
// RefreshAndGet calls, so a test can assert the store-call shape of a request.
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

// TestResolveSortKeys_UnknownField_OneRefreshPerRequest pins that a DATA sort
// key absent from the cached and the authoritative schema is refused each time
// and costs exactly one bounded RefreshAndGet per request: a refusal is not
// remembered between requests.
func TestResolveSortKeys_UnknownField_OneRefreshPerRequest(t *testing.T) {
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}
	desc := buildSearchDescriptor(t, ref, "a")
	ms := &countingModelStore{descriptor: desc}

	base := memory.NewStoreFactory()
	defer base.Close()
	factory := &modelStoreFactory{StoreFactory: base, modelStore: ms}

	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := base.AsyncSearchStore(context.Background())

	svc := search.NewSearchService(factory, uuids, searchStore, newTestConsistency(t, factory), platformclock.Real())

	ctx := tenantCtx("tenant-1")
	keys := []search.OrderKey{{Path: "$.reallyMissing", Source: spi.SourceData}}

	const reqCount = 20
	for i := 0; i < reqCount; i++ {
		if _, err := svc.ResolveSortKeysForTest(ctx, ref, keys); err == nil {
			t.Fatalf("iter %d: expected error for unknown sort field, got nil", i)
		}
	}

	// One bounded refresh per request, and no more: a refusal is not
	// remembered, so each repeat reads again and pays exactly one refresh.
	if got := ms.refreshCount.Load(); got != reqCount {
		t.Errorf("inner RefreshAndGet count: want %d (one per request), got %d", reqCount, got)
	}
}

// TestResolveSortKeys_RefreshesOnceBeforeRefusing verifies that a sort key
// naming a field absent from the cached schema but present in the
// authoritative (post-RefreshAndGet) schema triggers exactly one bounded
// refresh and then resolves — the same refresh-once contract
// TestSearch_StaleSchema_RefreshesOnceAndSucceeds (path_validate_test.go)
// pins for condition paths. Before this fix, resolveSortKeys read the
// cached schema once and refused, so the same field sorted on one node
// (whose cache had already been refreshed) and 400'd on another purely
// because of read-side cache staleness.
func TestResolveSortKeys_RefreshesOnceBeforeRefusing(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()

	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}

	stale := buildSearchDescriptor(t, ref, "a")
	fresh := buildSearchDescriptor(t, ref, "a", "newField")
	ms := &refreshingModelStore{
		getQueue:     []*spi.ModelDescriptor{stale},
		refreshQueue: []*spi.ModelDescriptor{fresh},
	}
	factory := &modelStoreFactory{StoreFactory: base, modelStore: ms}

	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := base.AsyncSearchStore(context.Background())
	svc := search.NewSearchService(factory, uuids, searchStore, newTestConsistency(t, factory), platformclock.Real())

	keys := []search.OrderKey{{Path: "$.newField", Source: spi.SourceData}}
	specs, err := svc.ResolveSortKeysForTest(ctx, ref, keys)
	if err != nil {
		t.Fatalf("sort key on a freshly added field must resolve: %v", err)
	}
	if len(specs) != 1 {
		t.Fatalf("expected 1 resolved order spec, got %d", len(specs))
	}
	if got := ms.RefreshCount(); got != 1 {
		t.Errorf("want exactly one bounded refresh, got %d", got)
	}
}

// TestResolveSortKeys_TrulyMissingField_FourxxAfterOneRefresh is the
// bound's other half: a sort key that is genuinely unknown to both the
// cached and the authoritative schema is refused with a 4xx and triggers
// refresh at most once — no unbounded refresh loop from a misconfigured
// client.
func TestResolveSortKeys_TrulyMissingField_FourxxAfterOneRefresh(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()

	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}

	stale := buildSearchDescriptor(t, ref, "a")
	stillStale := buildSearchDescriptor(t, ref, "a")
	ms := &refreshingModelStore{
		getQueue:     []*spi.ModelDescriptor{stale},
		refreshQueue: []*spi.ModelDescriptor{stillStale},
	}
	factory := &modelStoreFactory{StoreFactory: base, modelStore: ms}

	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := base.AsyncSearchStore(context.Background())
	svc := search.NewSearchService(factory, uuids, searchStore, newTestConsistency(t, factory), platformclock.Real())

	keys := []search.OrderKey{{Path: "$.reallyMissing", Source: spi.SourceData}}
	_, err := svc.ResolveSortKeysForTest(ctx, ref, keys)
	if err == nil {
		t.Fatalf("expected a validation error for a truly-missing sort field, got nil")
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

// TestResolveSortKeys_MetaFieldUnknown_NoRefresh pins that an unknown META
// sort key does not trigger a schema refresh at all: the meta allowlist is
// a static vocabulary, not model schema, so no refresh could ever change
// the outcome.
func TestResolveSortKeys_MetaFieldUnknown_NoRefresh(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()

	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}

	stale := buildSearchDescriptor(t, ref, "a")
	ms := &refreshingModelStore{
		getQueue: []*spi.ModelDescriptor{stale},
	}
	factory := &modelStoreFactory{StoreFactory: base, modelStore: ms}

	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := base.AsyncSearchStore(context.Background())
	svc := search.NewSearchService(factory, uuids, searchStore, newTestConsistency(t, factory), platformclock.Real())

	keys := []search.OrderKey{{Path: "notAMetaField", Source: spi.SourceMeta}}
	_, err := svc.ResolveSortKeysForTest(ctx, ref, keys)
	if err == nil {
		t.Fatalf("expected a validation error for an unknown meta sort field, got nil")
	}
	if got := ms.RefreshCount(); got != 0 {
		t.Errorf("an unknown meta field must not trigger a schema refresh, got %d", got)
	}
}

// TestResolveSortKeys_ContainerPath_OneRefreshPerRequest is the container
// counterpart. The schema declares only "$.address.street"; a sort key naming
// the interior node "$.address" is refused (it is not a scalar leaf) after one
// bounded refresh, on every request.
func TestResolveSortKeys_ContainerPath_OneRefreshPerRequest(t *testing.T) {
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}
	root := schema.NewObjectNode()
	addr := schema.NewObjectNode()
	addr.SetChild("street", schema.NewLeafNode(schema.String))
	root.SetChild("address", addr)
	raw, err := schema.Marshal(root)
	if err != nil {
		t.Fatalf("schema.Marshal: %v", err)
	}
	desc := &spi.ModelDescriptor{Ref: ref, State: spi.ModelLocked, Schema: raw}
	ms := &countingModelStore{descriptor: desc}

	base := memory.NewStoreFactory()
	defer base.Close()
	factory := &modelStoreFactory{StoreFactory: base, modelStore: ms}

	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := base.AsyncSearchStore(context.Background())

	svc := search.NewSearchService(factory, uuids, searchStore, newTestConsistency(t, factory), platformclock.Real())

	ctx := tenantCtx("tenant-1")
	keys := []search.OrderKey{{Path: "$.address", Source: spi.SourceData}}

	const reqCount = 20
	for i := 0; i < reqCount; i++ {
		if _, err := svc.ResolveSortKeysForTest(ctx, ref, keys); err == nil {
			t.Fatalf("iter %d: expected error sorting by a non-scalar container path, got nil", i)
		}
	}

	// One bounded refresh per request, and no more: a refusal is not
	// remembered, so each repeat reads again and pays exactly one refresh.
	if got := ms.refreshCount.Load(); got != reqCount {
		t.Errorf("inner RefreshAndGet count: want %d (one per request), got %d", reqCount, got)
	}
}

// TestResolveSortKeys_ArrayPath_OneRefreshPerRequest is the array-path sibling:
// the schema records the element under "$.tags[*]" and never the bare
// container "$.tags", which is refused as a sort key after one bounded refresh,
// on every request.
func TestResolveSortKeys_ArrayPath_OneRefreshPerRequest(t *testing.T) {
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}
	root := schema.NewObjectNode()
	root.SetChild("tags", schema.NewArrayNode(schema.NewLeafNode(schema.String)))
	raw, err := schema.Marshal(root)
	if err != nil {
		t.Fatalf("schema.Marshal: %v", err)
	}
	desc := &spi.ModelDescriptor{Ref: ref, State: spi.ModelLocked, Schema: raw}
	ms := &countingModelStore{descriptor: desc}

	base := memory.NewStoreFactory()
	defer base.Close()
	factory := &modelStoreFactory{StoreFactory: base, modelStore: ms}

	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := base.AsyncSearchStore(context.Background())

	svc := search.NewSearchService(factory, uuids, searchStore, newTestConsistency(t, factory), platformclock.Real())

	ctx := tenantCtx("tenant-1")
	keys := []search.OrderKey{{Path: "$.tags", Source: spi.SourceData}}

	const reqCount = 20
	for i := 0; i < reqCount; i++ {
		if _, err := svc.ResolveSortKeysForTest(ctx, ref, keys); err == nil {
			t.Fatalf("iter %d: expected error sorting by an array container path, got nil", i)
		}
	}

	// One bounded refresh per request, and no more: a refusal is not
	// remembered, so each repeat reads again and pays exactly one refresh.
	if got := ms.refreshCount.Load(); got != reqCount {
		t.Errorf("inner RefreshAndGet count: want %d (one per request), got %d", reqCount, got)
	}
}
