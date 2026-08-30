package search_test

import (
	"context"
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// TestResolveSortKeys_NegativeCache_CollapsesRepeatedRequests is the sort-key
// counterpart of TestSearch_NegativeCache_CollapsesSerialFloodForUnknownPath
// (path_validation_cache_test.go). A DATA sort key absent from BOTH the
// cached and the authoritative schema costs one Get + one bounded
// RefreshAndGet on the FIRST request; every repeat must hit
// PathValidationCache.IsAbsent and short-circuit before touching the model
// store at all.
//
// Before this fix, resolveSortKeys never consulted the negative cache and
// never called markPathsAbsent: a repeated bogus sort key paid a full
// RefreshAndGet — an authoritative model-store read plus a full schema
// re-parse — on every single request, indefinitely, and RefreshAndGet
// repopulates the shared model-descriptor cache, pushing the cost onto
// legitimate concurrent readers of the same model.
func TestResolveSortKeys_NegativeCache_CollapsesRepeatedRequests(t *testing.T) {
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
	keys := []search.OrderKey{{Path: "$.reallyMissing", Source: spi.SourceData}}

	const reqCount = 20
	for i := 0; i < reqCount; i++ {
		if _, err := svc.ResolveSortKeysForTest(ctx, ref, keys); err == nil {
			t.Fatalf("iter %d: expected error for unknown sort field, got nil", i)
		}
	}

	if got := ms.getCount.Load(); got > 1 {
		t.Errorf("inner Get count: want <=1 (negative cache short-circuits every repeat), got %d", got)
	}
	if got := ms.refreshCount.Load(); got > 1 {
		t.Errorf("inner RefreshAndGet count: want exactly 1 (bounded, then negative-cached), got %d", got)
	}
}

// TestResolveSortKeys_RefreshesOnceBeforeRefusing verifies that a sort key
// naming a field absent from the cached schema but present in the
// authoritative (post-RefreshAndGet) schema triggers exactly one bounded
// refresh and then resolves — the same issue-#77 contract
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
	svc := search.NewSearchService(factory, uuids, searchStore)

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
	svc := search.NewSearchService(factory, uuids, searchStore)

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
	svc := search.NewSearchService(factory, uuids, searchStore)

	keys := []search.OrderKey{{Path: "notAMetaField", Source: spi.SourceMeta}}
	_, err := svc.ResolveSortKeysForTest(ctx, ref, keys)
	if err == nil {
		t.Fatalf("expected a validation error for an unknown meta sort field, got nil")
	}
	if got := ms.RefreshCount(); got != 0 {
		t.Errorf("an unknown meta field must not trigger a schema refresh, got %d", got)
	}
}

// TestResolveSortKeys_NegativeCache_EngagesForContainerPath is
// TestResolveSortKeys_NegativeCache_CollapsesRepeatedRequests' container
// counterpart. The schema declares only "$.address.street" — a sort key
// naming the interior node "$.address" itself is rejected by resolveOrderBy
// (it is not a scalar leaf), but before this fix the negative-cache
// membership check (isPathKnown) is the CONDITION-path predicate, which
// treats "$.address" as known because a leaf descends from it. That mismatch
// made missing (and therefore markPathsAbsent) a no-op, so the negative
// cache never engaged and every repeat request paid a full authoritative
// model-store read plus a schema re-parse.
func TestResolveSortKeys_NegativeCache_EngagesForContainerPath(t *testing.T) {
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

	cache := search.NewPathValidationCache()
	svc := search.NewSearchService(factory, uuids, searchStore).
		WithPathValidationCache(cache)

	ctx := tenantCtx("tenant-1")
	keys := []search.OrderKey{{Path: "$.address", Source: spi.SourceData}}

	const reqCount = 20
	for i := 0; i < reqCount; i++ {
		if _, err := svc.ResolveSortKeysForTest(ctx, ref, keys); err == nil {
			t.Fatalf("iter %d: expected error sorting by a non-scalar container path, got nil", i)
		}
	}

	if got := ms.getCount.Load(); got > 1 {
		t.Errorf("inner Get count: want <=1 (negative cache short-circuits every repeat), got %d", got)
	}
	if got := ms.refreshCount.Load(); got > 1 {
		t.Errorf("inner RefreshAndGet count: want exactly 1 (bounded, then negative-cached), got %d", got)
	}
}

// TestResolveSortKeys_NegativeCache_EngagesForArrayPath is the array-path
// sibling of TestResolveSortKeys_NegativeCache_EngagesForContainerPath: the
// schema records the array's element once, under the wildcard key
// "$.tags[*]", and never the bare container "$.tags". resolveOrderBy
// rejects "$.tags" as an unknown sort field (it is not a scalar leaf key),
// but isPathKnown's container-prefix matching considers "$.tags" known
// because "$.tags[*]" descends from it, so the negative cache never engaged
// for this shape either.
func TestResolveSortKeys_NegativeCache_EngagesForArrayPath(t *testing.T) {
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

	cache := search.NewPathValidationCache()
	svc := search.NewSearchService(factory, uuids, searchStore).
		WithPathValidationCache(cache)

	ctx := tenantCtx("tenant-1")
	keys := []search.OrderKey{{Path: "$.tags", Source: spi.SourceData}}

	const reqCount = 20
	for i := 0; i < reqCount; i++ {
		if _, err := svc.ResolveSortKeysForTest(ctx, ref, keys); err == nil {
			t.Fatalf("iter %d: expected error sorting by an array container path, got nil", i)
		}
	}

	if got := ms.getCount.Load(); got > 1 {
		t.Errorf("inner Get count: want <=1 (negative cache short-circuits every repeat), got %d", got)
	}
	if got := ms.refreshCount.Load(); got > 1 {
		t.Errorf("inner RefreshAndGet count: want exactly 1 (bounded, then negative-cached), got %d", got)
	}
}
