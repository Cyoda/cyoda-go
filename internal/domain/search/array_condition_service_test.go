package search_test

// Search-service-level coverage for ArrayCondition against a real model
// FieldsMap. Nothing exercised that combination: ArrayCondition was covered at
// the translator and the validators, and the service was covered with
// SimpleCondition, so the one shape that names an array CONTAINER — the
// natural spelling for "is any element of $.tags one of these values" — never
// reached pre-execution path validation in a test.

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

// TestSearch_ArrayConditionOnContainerPath_IsRejected is the service-level
// proof of path-grammar.md §8: an `array` clause tests elements by position,
// so its jsonPath must carry a trailing wildcard. "$.tags" addresses the
// array itself, not its elements, and cannot carry a positional test — it is
// rejected 400 INVALID_FIELD_PATH even though the model declares the field
// (as "$.tags[*]").
//
// This inverts what used to be
// TestSearch_ArrayConditionOnContainerPath_IsAccepted: that test predated the
// wildcard requirement and pinned the opposite (wrong) behavior. See the
// companion below — TestSearch_SimpleConditionOnContainerPath_NotNull_IsAccepted
// — for why the bare path itself must stay valid; only the `array` clause's
// requirement changed.
func TestSearch_ArrayConditionOnContainerPath_IsRejected(t *testing.T) {
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
	_, err = svc.Search(ctx, ref, cond, search.SearchOptions{Limit: 10})
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("Search on an array clause's bare container path: got err %v, want *common.AppError", err)
	}
	if appErr.Status != http.StatusBadRequest || appErr.Code != common.ErrCodeInvalidFieldPath {
		t.Errorf("got %d/%s, want 400/%s", appErr.Status, appErr.Code, common.ErrCodeInvalidFieldPath)
	}
}

// TestSearch_SimpleConditionOnContainerPath_NotNull_IsAccepted guards the
// container acceptance path_validate.go's pathOrContainerKnown implements:
// only the `array` clause gained the trailing-wildcard requirement. A
// `simple` clause's bare path still addresses the array itself, and
// "$.tags NOT_NULL" — one of the two presence tests section 5 requires to
// stay answerable on a container path — must keep working. Without this
// companion, the container acceptance in path_validate.go could be removed
// by mistake in a future change to satisfy the array clause's new
// requirement, silently breaking "$.tags IS_NULL"/"NOT_NULL".
func TestSearch_SimpleConditionOnContainerPath_NotNull_IsAccepted(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()
	ctx := tenantCtx("tenant-array-simple-notnull")
	ref := spi.ModelRef{EntityName: "tagged3", ModelVersion: "1"}
	saveScalarArrayModel(t, ctx, base, ref)

	saveEntity(t, ctx, base, ref, "e1", []byte(`{"tags":["red","blue"]}`))

	searchStore, err := base.AsyncSearchStore(context.Background())
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	svc := search.NewSearchService(base, common.NewTestUUIDGenerator(), searchStore)

	cond := &predicate.SimpleCondition{JsonPath: "$.tags", OperatorType: "NOT_NULL"}
	results, err := svc.Search(ctx, ref, cond, search.SearchOptions{Limit: 10})
	if err != nil {
		t.Fatalf("Search on \"$.tags NOT_NULL\": %v — the bare path must stay valid for a simple clause", err)
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
