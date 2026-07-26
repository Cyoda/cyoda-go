package search_test

import (
	"context"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// saveTypedModel registers a model whose schema declares `age` as Integer, so
// loadFieldsMap returns a declared numeric type for `$.age`. This is what makes
// the in-memory fallback's comparison type-directed rather than degrading to
// non-match on an untyped leaf.
func saveTypedModel(t *testing.T, ctx context.Context, factory *memory.StoreFactory, ref spi.ModelRef) {
	t.Helper()
	node := schema.NewObjectNode()
	node.SetChild("age", schema.NewLeafNode(schema.Integer))
	raw, err := schema.Marshal(node)
	if err != nil {
		t.Fatalf("schema.Marshal: %v", err)
	}
	ms, err := factory.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	if err := ms.Save(ctx, &spi.ModelDescriptor{Ref: ref, State: spi.ModelLocked, Schema: raw}); err != nil {
		t.Fatalf("Save model: %v", err)
	}
}

// corruptSchemaModelStore returns a descriptor whose Schema bytes are not a
// valid schema, so EnsureModelRegistered (which only checks existence) and the
// tolerant validators pass, but loadFieldsMap's schema.Unmarshal fails — a
// GENUINE load error, distinct from the (nil,nil) no-schema case.
type corruptSchemaModelStore struct {
	spi.ModelStore
	ref spi.ModelRef
}

func (m *corruptSchemaModelStore) Get(context.Context, spi.ModelRef) (*spi.ModelDescriptor, error) {
	return &spi.ModelDescriptor{Ref: m.ref, State: spi.ModelLocked, Schema: []byte("not-a-schema")}, nil
}

type combinedFallbackFactory struct {
	spi.StoreFactory
	entityStore spi.EntityStore
	modelStore  spi.ModelStore
}

func (f *combinedFallbackFactory) EntityStore(context.Context) (spi.EntityStore, error) {
	return f.entityStore, nil
}

func (f *combinedFallbackFactory) ModelStore(context.Context) (spi.ModelStore, error) {
	return f.modelStore, nil
}

// TestSearchFallback_TypeDirectedWithRegisteredModel proves the Task-8 wiring on
// the in-memory fallback path: a non-Searcher store forces the GetAll+match
// fallback, and with a registered model declaring `$.age` Integer, a
// GREATER_THAN data-field condition evaluates type-directed (numeric) — only the
// entity whose age exceeds the operand matches. An untyped leaf would degrade
// every comparison to non-match, so a non-empty, correct result set is the
// evidence the declared types were loaded and threaded.
func TestSearchFallback_TypeDirectedWithRegisteredModel(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()

	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}

	saveTypedModel(t, ctx, base, ref)
	saveEntity(t, ctx, base, ref, "e1", []byte(`{"age":30}`))
	saveEntity(t, ctx, base, ref, "e2", []byte(`{"age":3}`))

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

	// Parse from JSON so the numeric operand arrives as float64 (matching
	// production request parsing), which the type classifier recognizes.
	cond, err := predicate.ParseCondition([]byte(`{"type":"simple","jsonPath":"$.age","operatorType":"GREATER_THAN","value":5}`))
	if err != nil {
		t.Fatalf("ParseCondition: %v", err)
	}

	results, err := svc.Search(ctx, ref, cond, search.SearchOptions{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 || results[0].Meta.ID != "e1" {
		t.Fatalf("expected 1 type-directed result (e1 age=30 > 5), got %d results: %+v", len(results), results)
	}
}

// TestSearchFallback_FailsClosedOnGenuineModelLoadError proves the
// correctness-over-availability fix on the fallback path: when the model store
// yields a genuine schema-load error (here, corrupt schema bytes), the in-memory
// fallback surfaces the error rather than silently under-matching with untyped
// leaves. A naive `fallbackFields, _ := loadFieldsMap(...)` (the pre-fix
// swallow) would instead return an empty result set with nil error.
func TestSearchFallback_FailsClosedOnGenuineModelLoadError(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()

	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}

	// Save an entity so GetAll (which runs before the fail-closed load) succeeds.
	saveEntity(t, ctx, base, ref, "e1", []byte(`{"age":30}`))

	realStore, _ := base.EntityStore(ctx)
	nonSearcher := &nonSearcherEntityStore{EntityStore: realStore}
	factory := &combinedFallbackFactory{
		StoreFactory: base,
		entityStore:  nonSearcher,
		modelStore:   &corruptSchemaModelStore{ref: ref},
	}

	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := base.AsyncSearchStore(context.Background())
	svc := search.NewSearchService(factory, uuids, searchStore)

	cond, err := predicate.ParseCondition([]byte(`{"type":"simple","jsonPath":"$.age","operatorType":"GREATER_THAN","value":5}`))
	if err != nil {
		t.Fatalf("ParseCondition: %v", err)
	}

	_, err = svc.Search(ctx, ref, cond, search.SearchOptions{})
	if err == nil {
		t.Fatal("a genuine model-load error on the in-memory fallback must fail closed (surface the error)")
	}
}
