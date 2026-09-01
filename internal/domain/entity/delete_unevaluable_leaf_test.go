package entity_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/entity"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// delete_unevaluable_leaf_test.go closes the review's Important-1 finding on
// Task 5's fix round 1: DeleteEntitiesConditional's own REACHABLE selection
// path (planDeleteSelection's successfully-translated Filter, driven through
// a real store's Iterate — not the structurally-unreachable match.Prepare
// residual branch) answered 500 SERVER_ERROR for the identical bare-leaf
// input that /search/direct already classifies 400, because three
// consuming sites (drainDeleteSelection's own two error returns, and
// deleteBatched's separate inline Iterate loop) never routed the store's
// error through search.ClassifyStoreQueryError. Delete-by-condition has the
// worst blast radius of any surface in this plan — a wrongly-answered
// condition here removes rows — so this gets its own dedicated coverage
// rather than resting on /search's.
//
// Both DeleteEntitiesConditional shapes are covered, per the review's own
// repro: batchSize=0 (single-tx, selectDeleteIDs -> drainDeleteSelection) and
// batchSize=2 (deleteBatched's own separate inline Iterate loop).

// saveBareLeafModelEntity registers a model, via the memory plugin's
// ModelStore directly, whose single field is a bare {"kind":"LEAF"} node —
// present (a known path) but carrying NO declared scalar type. Same
// reproduction as internal/domain/search's classify_store_query_error_test.go
// — see that file's header for the full rationale (a real, spec-anticipated
// schema shape the condition-type boundary explicitly accepts as "no
// constraint" but the leaf kernel cannot evaluate a comparison against).
func saveBareLeafModelEntity(t *testing.T, ctx context.Context, factory *memory.StoreFactory, ref spi.ModelRef, field string) {
	t.Helper()
	raw := []byte(fmt.Sprintf(`{"kind":"OBJECT","children":{%q:{"kind":"LEAF"}}}`, field))
	node, err := schema.Unmarshal(raw)
	if err != nil {
		t.Fatalf("schema.Unmarshal: %v", err)
	}
	marshalled, err := schema.Marshal(node)
	if err != nil {
		t.Fatalf("schema.Marshal: %v", err)
	}
	ms, err := factory.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	if err := ms.Save(ctx, &spi.ModelDescriptor{Ref: ref, State: spi.ModelLocked, Schema: marshalled}); err != nil {
		t.Fatalf("Save model: %v", err)
	}
}

func newBareLeafDeleteFixture(t *testing.T) (h *entity.Handler, ctx context.Context, entityName, modelVersion string) {
	t.Helper()
	base := memory.NewStoreFactory()
	t.Cleanup(func() { base.Close() })

	ctx = statsTestCtx("tenant-delete-unevaluable-leaf")
	ref := spi.ModelRef{EntityName: "DeleteBareLeafModel", ModelVersion: "1"}
	saveBareLeafModelEntity(t, ctx, base, ref, "score")

	txMgr, err := base.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	h = entity.New(base, txMgr, common.NewDefaultUUIDGenerator(), nil, txgate.New())

	return h, ctx, ref.EntityName, ref.ModelVersion
}

// TestDeleteEntitiesConditional_UnevaluableLeaf_SingleTx_MapsTo400 is the
// batchSize=0 (single-tx, selectDeleteIDs) shape.
func TestDeleteEntitiesConditional_UnevaluableLeaf_SingleTx_MapsTo400(t *testing.T) {
	h, ctx, entityName, modelVersion := newBareLeafDeleteFixture(t)

	cond := []byte(`{"type":"simple","jsonPath":"$.score","operatorType":"EQUALS","value":5}`)
	_, err := h.DeleteEntitiesConditional(ctx, entityName, modelVersion, cond, nil, false, 0)

	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("got err %v (%T), want *common.AppError", err, err)
	}
	if appErr.Status != http.StatusBadRequest {
		t.Fatalf("got status %d, want %d (a malformed condition must not answer 500 + ticket)", appErr.Status, http.StatusBadRequest)
	}
	if appErr.Code != common.ErrCodeInvalidCondition {
		t.Fatalf("got code %s, want %s", appErr.Code, common.ErrCodeInvalidCondition)
	}
	if !errors.Is(err, spi.ErrUnevaluableLeaf) {
		t.Errorf("errors.Is(err, spi.ErrUnevaluableLeaf) = false, err = %v", err)
	}
}

// TestDeleteEntitiesConditional_UnevaluableLeaf_Batched_MapsTo400 is the
// batchSize=2 (deleteBatched's own separate inline Iterate loop) shape —
// deleteBatched does not route through drainDeleteSelection at all, so this
// exercises a genuinely different call site than the single-tx test above.
func TestDeleteEntitiesConditional_UnevaluableLeaf_Batched_MapsTo400(t *testing.T) {
	h, ctx, entityName, modelVersion := newBareLeafDeleteFixture(t)

	cond := []byte(`{"type":"simple","jsonPath":"$.score","operatorType":"EQUALS","value":5}`)
	_, err := h.DeleteEntitiesConditional(ctx, entityName, modelVersion, cond, nil, false, 2)

	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("got err %v (%T), want *common.AppError", err, err)
	}
	if appErr.Status != http.StatusBadRequest {
		t.Fatalf("got status %d, want %d (a malformed condition must not answer 500 + ticket)", appErr.Status, http.StatusBadRequest)
	}
	if appErr.Code != common.ErrCodeInvalidCondition {
		t.Fatalf("got code %s, want %s", appErr.Code, common.ErrCodeInvalidCondition)
	}
	if !errors.Is(err, spi.ErrUnevaluableLeaf) {
		t.Errorf("errors.Is(err, spi.ErrUnevaluableLeaf) = false, err = %v", err)
	}
}
