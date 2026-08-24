package search_test

// Search-service-level coverage for ArrayCondition against a real model
// FieldsMap. Nothing exercised that combination: ArrayCondition was covered at
// the translator and the validators, and the service was covered with
// SimpleCondition, so the one shape that names an array CONTAINER — the
// natural spelling for "is any element of $.tags one of these values" — never
// reached pre-execution path validation in a test.

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

// saveScalarArrayModel registers a model whose only field is a string array,
// and asserts the FieldsMap it produces holds exactly the ELEMENT key
// ("$.tags[*]") — the schema never records the container as a leaf, which is
// the whole reason the container probe matters.
func saveScalarArrayModel(t *testing.T, ctx context.Context, factory *memory.StoreFactory, ref spi.ModelRef) {
	t.Helper()
	node := schema.NewObjectNode()
	node.SetChild("tags", schema.NewArrayNode(schema.NewLeafNode(schema.String)))
	fields := node.FieldsMap()
	if _, ok := fields["$.tags[*]"]; !ok || len(fields) != 1 {
		t.Fatalf("FieldsMap = %v, want exactly the \"$.tags[*]\" element key", fields)
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

// TestSearch_ArrayConditionOnContainerPath_IsAccepted is the service-level
// regression proof: "$.tags" names a field the model declares (as
// "$.tags[*]"), so the request must be served, not rejected 400
// INVALID_FIELD_PATH.
func TestSearch_ArrayConditionOnContainerPath_IsAccepted(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()
	ctx := tenantCtx("tenant-array")
	ref := spi.ModelRef{EntityName: "tagged", ModelVersion: "1"}
	saveScalarArrayModel(t, ctx, base, ref)

	saveEntity(t, ctx, base, ref, "e1", []byte(`{"tags":["red","blue"]}`))
	saveEntity(t, ctx, base, ref, "e2", []byte(`{"tags":["green"]}`))

	searchStore, err := base.AsyncSearchStore(context.Background())
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	svc := search.NewSearchService(base, common.NewTestUUIDGenerator(), searchStore)

	cond := &predicate.ArrayCondition{JsonPath: "$.tags", Values: []any{"red"}}
	results, err := svc.Search(ctx, ref, cond, search.SearchOptions{Limit: 10})
	if err != nil {
		t.Fatalf("Search on the array container path: %v — the model declares $.tags[*], so $.tags names a field it knows", err)
	}
	if len(results) != 1 || results[0].Meta.ID != "e1" {
		t.Fatalf("got %d results, want exactly e1", len(results))
	}
}

// TestSearch_ArrayConditionOnUnknownPath_IsRejected is the negative control:
// widening the existence probe must not stop it rejecting a path the model
// genuinely does not declare.
func TestSearch_ArrayConditionOnUnknownPath_IsRejected(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()
	ctx := tenantCtx("tenant-array-neg")
	ref := spi.ModelRef{EntityName: "tagged2", ModelVersion: "1"}
	saveScalarArrayModel(t, ctx, base, ref)

	searchStore, err := base.AsyncSearchStore(context.Background())
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	svc := search.NewSearchService(base, common.NewTestUUIDGenerator(), searchStore)

	cond := &predicate.ArrayCondition{JsonPath: "$.tag", Values: []any{"red"}}
	if _, err := svc.Search(ctx, ref, cond, search.SearchOptions{Limit: 10}); err == nil {
		t.Fatal("Search on an undeclared array path = nil error, want rejection")
	}
}
