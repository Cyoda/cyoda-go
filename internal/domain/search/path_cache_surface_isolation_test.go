package search_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// TestPathValidationCache_RejectedSortOnArrayContainer_DoesNotPoisonCondition
// is the direct reproduction of the fix-round-2 regression: the condition
// surface (validateConditionPaths) and the sort-key surface
// (resolveSortKeys) share ONE *search.PathValidationCache instance — exactly
// how app.go wires it, one cache per SearchService — but they do not agree
// on what "absent" means for the same spelling. "$.tags" is a legitimate
// CONDITION path (TestSearch_SimpleConditionOnContainerPath_NotNull_IsAccepted
// pins that "$.tags NOT_NULL" must succeed even though the schema records
// only the element key "$.tags[*]"), but resolveOrderBy requires an exact
// scalar-leaf key and rejects "$.tags" as an unknown sort field.
//
// Before namespacing the cache key by surface, a rejected sort on "$.tags"
// called markPathsAbsent("$.tags") on the SHARED cache, and the very next,
// otherwise-legitimate condition search on "$.tags NOT_NULL" hit
// cachedAbsentPaths and 400'd — a valid search permanently broken by an
// unrelated sort request, on a stable schema, until a schema change fires
// InvalidateRef or otter evicts the entry.
func TestPathValidationCache_RejectedSortOnArrayContainer_DoesNotPoisonCondition(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()
	ctx := tenantCtx("tenant-surface-isolation-array")
	ref := spi.ModelRef{EntityName: "tagged-isolation", ModelVersion: "1"}
	saveScalarArrayModel(t, ctx, base, ref)
	saveEntity(t, ctx, base, ref, "e1", []byte(`{"tags":["red","blue"]}`))

	searchStore, err := base.AsyncSearchStore(context.Background())
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	cache := search.NewPathValidationCache()
	svc := search.NewSearchService(base, common.NewTestUUIDGenerator(), searchStore).
		WithPathValidationCache(cache)

	condition := &predicate.SimpleCondition{JsonPath: "$.tags", OperatorType: "NOT_NULL"}

	// 1. BEFORE any sort request: the condition search succeeds.
	before, err := svc.Search(ctx, ref, condition, search.SearchOptions{Limit: 10})
	if err != nil {
		t.Fatalf("Search($.tags NOT_NULL) before sort request: unexpected error %v", err)
	}
	if len(before) != 1 {
		t.Fatalf("Search($.tags NOT_NULL) before sort request: got %d results, want 1", len(before))
	}

	// 2. A sort request on the same path is correctly rejected — "$.tags" is
	// not a scalar leaf as far as resolveOrderBy is concerned.
	sortKeys := []search.OrderKey{{Path: "$.tags", Source: spi.SourceData}}
	if _, err := svc.ResolveSortKeysForTest(ctx, ref, sortKeys); err == nil {
		t.Fatalf("ResolveSortKeysForTest($.tags): expected rejection (not a scalar leaf), got nil")
	}

	// 3. AFTER the sort rejection: the IDENTICAL condition search must still
	// succeed. Before the fix this 400'd with "condition references unknown
	// field path(s): $.tags".
	after, err := svc.Search(ctx, ref, condition, search.SearchOptions{Limit: 10})
	if err != nil {
		t.Fatalf("Search($.tags NOT_NULL) after an unrelated sort rejection: unexpected error %v — "+
			"the sort surface's negative-cache entry leaked into the condition surface's lookup", err)
	}
	if len(after) != 1 {
		t.Fatalf("Search($.tags NOT_NULL) after an unrelated sort rejection: got %d results, want 1", len(after))
	}
}

// TestPathValidationCache_RejectedSortOnObjectContainer_DoesNotPoisonCondition
// is the object-container sibling: the schema declares only
// "$.address.street", "$.address" itself addresses a legitimate condition
// container, but resolveOrderBy rejects it as a sort key (not a scalar
// leaf).
func TestPathValidationCache_RejectedSortOnObjectContainer_DoesNotPoisonCondition(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()
	ctx := tenantCtx("tenant-surface-isolation-object")
	ref := spi.ModelRef{EntityName: "addressed-isolation", ModelVersion: "1"}

	root := schema.NewObjectNode()
	addr := schema.NewObjectNode()
	addr.SetChild("street", schema.NewLeafNode(schema.String))
	root.SetChild("address", addr)
	raw, err := schema.Marshal(root)
	if err != nil {
		t.Fatalf("schema.Marshal: %v", err)
	}
	ms, err := base.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	if err := ms.Save(ctx, &spi.ModelDescriptor{Ref: ref, Schema: raw}); err != nil {
		t.Fatalf("Save model: %v", err)
	}
	saveEntity(t, ctx, base, ref, "e1", []byte(`{"address":{"street":"Main St"}}`))

	searchStore, err := base.AsyncSearchStore(context.Background())
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	cache := search.NewPathValidationCache()
	svc := search.NewSearchService(base, common.NewTestUUIDGenerator(), searchStore).
		WithPathValidationCache(cache)

	condition := &predicate.SimpleCondition{JsonPath: "$.address", OperatorType: "NOT_NULL"}

	before, err := svc.Search(ctx, ref, condition, search.SearchOptions{Limit: 10})
	if err != nil {
		t.Fatalf("Search($.address NOT_NULL) before sort request: unexpected error %v", err)
	}
	if len(before) != 1 {
		t.Fatalf("Search($.address NOT_NULL) before sort request: got %d results, want 1", len(before))
	}

	sortKeys := []search.OrderKey{{Path: "$.address", Source: spi.SourceData}}
	if _, err := svc.ResolveSortKeysForTest(ctx, ref, sortKeys); err == nil {
		t.Fatalf("ResolveSortKeysForTest($.address): expected rejection (not a scalar leaf), got nil")
	}

	after, err := svc.Search(ctx, ref, condition, search.SearchOptions{Limit: 10})
	if err != nil {
		t.Fatalf("Search($.address NOT_NULL) after an unrelated sort rejection: unexpected error %v — "+
			"the sort surface's negative-cache entry leaked into the condition surface's lookup", err)
	}
	if len(after) != 1 {
		t.Fatalf("Search($.address NOT_NULL) after an unrelated sort rejection: got %d results, want 1", len(after))
	}
}

// TestPathValidationCache_RejectedCondition_StillRejectsSort is the other
// direction named in the fix-round-2 finding: a condition path the negative
// cache has recorded as genuinely absent from the schema (not merely a
// container/array mismatch — actually unknown) must still be rejected when
// the SAME path is later used as a sort key. This direction was already
// safe before namespacing (a path the condition surface's more permissive
// membership test calls absent is absent under the sort surface's stricter
// exact-leaf test too — condition-absence is a strict superset of
// sort-absence), and stays safe after: each surface now reads back only its
// own cache entries, so the sort key gets its own independent (and still
// correctly bounded) rejection rather than a shared one.
func TestPathValidationCache_RejectedCondition_StillRejectsSort(t *testing.T) {
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}
	desc := buildSearchDescriptor(t, ref, "a")

	base := memory.NewStoreFactory()
	defer base.Close()
	ms := &countingModelStore{descriptor: desc}
	factory := &modelStoreFactory{StoreFactory: base, modelStore: ms}

	ctx := tenantCtx("tenant-surface-isolation-reverse")
	searchStore, err := base.AsyncSearchStore(context.Background())
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	cache := search.NewPathValidationCache()
	svc := search.NewSearchService(factory, common.NewTestUUIDGenerator(), searchStore).
		WithPathValidationCache(cache)

	// 1. A condition on a genuinely-unknown path is rejected and negative-cached.
	condition := &predicate.SimpleCondition{JsonPath: "$.reallyMissing", OperatorType: "NOT_NULL"}
	_, err = svc.Search(ctx, ref, condition, search.SearchOptions{Limit: 10})
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("Search($.reallyMissing NOT_NULL): got err %v, want *common.AppError", err)
	}
	if appErr.Status != http.StatusBadRequest || appErr.Code != common.ErrCodeInvalidFieldPath {
		t.Fatalf("Search($.reallyMissing NOT_NULL): got %d/%s, want 400/%s",
			appErr.Status, appErr.Code, common.ErrCodeInvalidFieldPath)
	}

	// 2. A sort key on the same path must still be rejected — never silently
	// accepted because a different surface cached a related-but-not-identical
	// verdict.
	sortKeys := []search.OrderKey{{Path: "$.reallyMissing", Source: spi.SourceData}}
	if _, err := svc.ResolveSortKeysForTest(ctx, ref, sortKeys); err == nil {
		t.Fatalf("ResolveSortKeysForTest($.reallyMissing): expected rejection, got nil")
	}
}
