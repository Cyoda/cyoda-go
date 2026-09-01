package e2e_test

// search_unevaluable_leaf_test.go — running-backend (postgres) e2e coverage
// for the fix in internal/domain/search/service.go's ClassifyStoreQueryError:
// a storage backend rejecting a condition leaf it cannot type-check
// (spi.ErrUnevaluableLeaf) or a pattern it cannot compile
// (spi.ErrInvalidPattern) must surface as 400 INVALID_CONDITION, not a 500
// plus a support ticket.
//
// Reproduction: a model field recorded as a bare {"kind":"LEAF"} node — no
// declared scalar type — is a real, spec-anticipated schema shape
// (cyoda-go-spi's model_schema.go FieldsMap doc: "A node declaring ONLY an
// empty scalar branch still emits it, which is what a bare {"kind":"LEAF"}
// has always meant"). The engine's own condition-type boundary
// (condition_type_validate.go) explicitly treats a field with zero declared
// types as "no constraint; accept" for every comparison operator — but the
// type-directed leaf kernel (spi.ExpandLeaf) cannot evaluate a comparison
// against zero declared types (spi.ErrUnevaluableLeaf's own doc: "including
// an empty/nil declared set"). That is the genuine disagreement between the
// boundary and the evaluator this classifier exists to paper over with a 400
// instead of a 500 — not a contrived fault injection.
//
// The postgres query planner (plugins/postgres/query_planner.go planQuery)
// calls spi.Prepare on the WHOLE filter UNCONDITIONALLY, before any
// pushed/residual dissection ("Evaluability is checked UNCONDITIONALLY... —
// never as a by-product of preparing the residual"), so a plain (non-NOT)
// comparison against a bare-leaf field already reaches it — no NOT node is
// needed to force residual evaluation for this specific reproduction.
//
// Model setup goes around the normal SAMPLE_DATA import (which infers a
// concrete scalar type from every sample value, and so can never produce a
// bare, typeless leaf) via testApp.StoreFactory() directly — the same
// established pattern search_intx_tracking_test.go and list_intx_readset_test.go
// use to reach store internals the HTTP surface doesn't expose. The actual
// SEARCH under test still goes through the full HTTP stack against the real
// postgres container TestMain started.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
)

// saveBareLeafModelE2E registers a model, under the shared e2e Postgres
// container/app, whose single field is a bare {"kind":"LEAF"} node: a known
// path with NO declared scalar type.
func saveBareLeafModelE2E(t *testing.T, ctx context.Context, ref spi.ModelRef, field string) {
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
	ms, err := testApp.StoreFactory().ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	if err := ms.Save(ctx, &spi.ModelDescriptor{Ref: ref, Schema: marshalled}); err != nil {
		t.Fatalf("Save model: %v", err)
	}
}

// TestSearch_BareLeafField_Postgres_DirectSearch_400InvalidCondition covers
// the BOUNDED store route: POST /search/direct resolves a positive limit
// before reaching SearchService.Search, so this exercises Searcher.Search
// against the real postgres plugin.
func TestSearch_BareLeafField_Postgres_DirectSearch_400InvalidCondition(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	const model = "e2e-search-unevaluable-leaf-direct"
	ctx := intxTenantCtx()
	ref := spi.ModelRef{EntityName: model, ModelVersion: "1"}
	saveBareLeafModelE2E(t, ctx, ref, "score")

	cond := `{"type":"simple","jsonPath":"$.score","operatorType":"EQUALS","value":5}`
	path := fmt.Sprintf("/api/search/direct/%s/1", model)
	resp := doAuth(t, http.MethodPost, path, cond)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, readBody(t, resp))
	}
	commontest.ExpectErrorCode(t, resp, "INVALID_CONDITION")
}

// TestSearch_BareLeafField_Postgres_DirectSearch_Unbounded_400InvalidCondition
// covers the UNBOUNDED store route: SearchService.Search's own Limit<=0
// branch drains spi.Iterable.Iterate instead of calling Searcher.Search
// (search/service.go's Search doc comment: "opts.Limit <= 0 ... streamed
// through spi.Iterable.Iterate instead of Searcher"). No transport resolves
// a non-positive limit before reaching the service (both HTTP and gRPC
// direct entry points resolve a default first), so this route is a
// service-layer contract, exercised here the same way the package's own
// unit tests exercise it (search.SearchService.Search called directly) — but
// against the REAL, running e2e Postgres app/store this file's TestMain
// stood up, not a stub.
func TestSearch_BareLeafField_Postgres_DirectSearch_Unbounded_400InvalidCondition(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	const model = "e2e-search-unevaluable-leaf-iterate"
	ctx := intxTenantCtx()
	ref := spi.ModelRef{EntityName: model, ModelVersion: "1"}
	saveBareLeafModelE2E(t, ctx, ref, "score")

	cond := &predicate.SimpleCondition{JsonPath: "$.score", OperatorType: "EQUALS", Value: float64(5)}
	_, err := testApp.SearchService().DirectSearch(ctx, ref, cond, search.SearchOptions{Limit: 0})

	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("want *common.AppError, got %T: %v", err, err)
	}
	if appErr.Status != http.StatusBadRequest || appErr.Code != common.ErrCodeInvalidCondition {
		t.Errorf("got %d/%q, want 400/%s", appErr.Status, appErr.Code, common.ErrCodeInvalidCondition)
	}
	if !errors.Is(err, spi.ErrUnevaluableLeaf) {
		t.Errorf("errors.Is(err, spi.ErrUnevaluableLeaf) = false, err = %v", err)
	}
}
