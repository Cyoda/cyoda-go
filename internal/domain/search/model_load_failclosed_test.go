package search_test

import (
	"context"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

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

// corruptSchemaFactory pairs a real memory EntityStore with the corrupt
// model store above.
type corruptSchemaFactory struct {
	spi.StoreFactory
	modelStore spi.ModelStore
}

func (f *corruptSchemaFactory) ModelStore(context.Context) (spi.ModelStore, error) {
	return f.modelStore, nil
}

// TestSearch_FailsClosedOnGenuineModelLoadError proves the
// correctness-over-availability stance on the schema load: when the model
// store yields a genuine schema-load error (here, corrupt schema bytes), the
// search surfaces the error rather than answering with untyped leaves. A
// swallowed load error would instead return an empty result set with nil
// error — an answer that looks like "nothing matched".
func TestSearch_FailsClosedOnGenuineModelLoadError(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()

	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}
	saveEntity(t, ctx, base, ref, "e1", []byte(`{"age":30}`))

	factory := &corruptSchemaFactory{
		StoreFactory: base,
		modelStore:   &corruptSchemaModelStore{ref: ref},
	}

	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := base.AsyncSearchStore(context.Background())
	svc := search.NewSearchService(factory, uuids, searchStore)

	cond, err := predicate.ParseCondition([]byte(`{"type":"simple","jsonPath":"$.age","operatorType":"GREATER_THAN","value":5}`))
	if err != nil {
		t.Fatalf("ParseCondition: %v", err)
	}

	if _, err := svc.Search(ctx, ref, cond, search.SearchOptions{Limit: 10}); err == nil {
		t.Fatal("a genuine model-load error must fail closed (surface the error)")
	}
}
