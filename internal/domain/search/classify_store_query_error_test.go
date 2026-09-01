package search_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/internal/match"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// classify_store_query_error_test.go pins Task 5's classification of the two
// sentinel classes an unevaluable leaf can surface as, once Tasks 1-4 made
// both evaluators fail closed instead of silently never-matching:
//
//   - spi.ErrUnevaluableLeaf / spi.ErrInvalidPattern — the SPI kernel's own
//     Prepare (spi.Filter side), reached through a plugin's Searcher.Search /
//     Iterable.Iterate. Maps to 400 INVALID_CONDITION (brief's own contract).
//   - match.ErrUnevaluableLeaf / match.ErrUnsupportedOperator —
//     internal/match's OWN Prepare (predicate.Condition side), reached
//     through search.Service.Search's GetAll+match fallback, entity's
//     conditional-delete planner, and grouped-stats' streaming tally. Maps to
//     400 CONDITION_TYPE_MISMATCH / 400 INVALID_CONDITION respectively — see
//     ClassifyStoreQueryError's own doc for why those two codes (not
//     INVALID_CONDITION uniformly) were chosen for this class.
//
// Before this task both classes classified as nil — an unrecognised store
// error — which the transport layer defaults to 500 SERVER_ERROR plus a
// support ticket for what is, in every case here, a malformed CLIENT INPUT.

// TestClassifyStoreQueryError_UnrelatedError_ReturnsNil pins the "does not
// swallow everything" half of the classifier's contract: an error with no
// known sentinel in its chain must come back nil so the caller's own
// fallback (a plain 500) still applies.
func TestClassifyStoreQueryError_UnrelatedError_ReturnsNil(t *testing.T) {
	err := fmt.Errorf("some unrelated storage failure")
	if got := search.ClassifyStoreQueryError(err); got != nil {
		t.Fatalf("ClassifyStoreQueryError(unrelated) = %v, want nil", got)
	}
}

// TestClassifyStoreQueryError_Nil_ReturnsNil is the trivial input case every
// call site relies on (`if appErr := ClassifyStoreQueryError(err); appErr !=
// nil`) to fall through cleanly when there was no error at all.
func TestClassifyStoreQueryError_Nil_ReturnsNil(t *testing.T) {
	if got := search.ClassifyStoreQueryError(nil); got != nil {
		t.Fatalf("ClassifyStoreQueryError(nil) = %v, want nil", got)
	}
}

func TestClassifyStoreQueryError_SPIUnevaluableLeaf_MapsTo400InvalidCondition(t *testing.T) {
	err := fmt.Errorf("plugin detail: %w", spi.ErrUnevaluableLeaf)
	appErr := search.ClassifyStoreQueryError(err)
	if appErr == nil {
		t.Fatal("ClassifyStoreQueryError(spi.ErrUnevaluableLeaf) = nil, want a classified AppError")
	}
	if appErr.Status != http.StatusBadRequest || appErr.Code != common.ErrCodeInvalidCondition {
		t.Errorf("got %d/%q, want 400/%s", appErr.Status, appErr.Code, common.ErrCodeInvalidCondition)
	}
	if !errors.Is(appErr, spi.ErrUnevaluableLeaf) {
		t.Error("errors.Is(appErr, spi.ErrUnevaluableLeaf) = false; WithCause must preserve the sentinel")
	}
}

func TestClassifyStoreQueryError_SPIInvalidPattern_MapsTo400InvalidCondition(t *testing.T) {
	err := fmt.Errorf("plugin detail: %w", spi.ErrInvalidPattern)
	appErr := search.ClassifyStoreQueryError(err)
	if appErr == nil {
		t.Fatal("ClassifyStoreQueryError(spi.ErrInvalidPattern) = nil, want a classified AppError")
	}
	if appErr.Status != http.StatusBadRequest || appErr.Code != common.ErrCodeInvalidCondition {
		t.Errorf("got %d/%q, want 400/%s", appErr.Status, appErr.Code, common.ErrCodeInvalidCondition)
	}
	if !errors.Is(appErr, spi.ErrInvalidPattern) {
		t.Error("errors.Is(appErr, spi.ErrInvalidPattern) = false; WithCause must preserve the sentinel")
	}
}

func TestClassifyStoreQueryError_MatchUnevaluableLeaf_MapsTo400ConditionTypeMismatch(t *testing.T) {
	err := fmt.Errorf("predicate match failed: %w", match.ErrUnevaluableLeaf)
	appErr := search.ClassifyStoreQueryError(err)
	if appErr == nil {
		t.Fatal("ClassifyStoreQueryError(match.ErrUnevaluableLeaf) = nil, want a classified AppError")
	}
	if appErr.Status != http.StatusBadRequest || appErr.Code != common.ErrCodeConditionTypeMismatch {
		t.Errorf("got %d/%q, want 400/%s", appErr.Status, appErr.Code, common.ErrCodeConditionTypeMismatch)
	}
	if !errors.Is(appErr, match.ErrUnevaluableLeaf) {
		t.Error("errors.Is(appErr, match.ErrUnevaluableLeaf) = false; WithCause must preserve the sentinel")
	}
}

func TestClassifyStoreQueryError_MatchUnsupportedOperator_MapsTo400InvalidCondition(t *testing.T) {
	err := fmt.Errorf("predicate match failed: %w", match.ErrUnsupportedOperator)
	appErr := search.ClassifyStoreQueryError(err)
	if appErr == nil {
		t.Fatal("ClassifyStoreQueryError(match.ErrUnsupportedOperator) = nil, want a classified AppError")
	}
	if appErr.Status != http.StatusBadRequest || appErr.Code != common.ErrCodeInvalidCondition {
		t.Errorf("got %d/%q, want 400/%s", appErr.Status, appErr.Code, common.ErrCodeInvalidCondition)
	}
	if !errors.Is(appErr, match.ErrUnsupportedOperator) {
		t.Error("errors.Is(appErr, match.ErrUnsupportedOperator) = false; WithCause must preserve the sentinel")
	}
}

// ---------------------------------------------------------------------------
// Both store routes: bounded Searcher.Search and unbounded Iterable.Iterate.
// ---------------------------------------------------------------------------
//
// Mirrors invalid_filter_path_test.go's own two-routes pattern for
// spi.ErrInvalidFilterPath: the engine reaches a backend by two paths
// (opts.Limit > 0 -> Searcher.Search, opts.Limit <= 0 -> Iterable.Iterate via
// drainIterate), and classifying only one would make the client-visible
// status depend on whether the request happened to carry a positive limit —
// a defect this codebase has already had once (see TestSearch_Iterate* in
// invalid_filter_path_test.go).

func TestSearch_SearcherSPISentinels_MapTo400(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"UnevaluableLeaf", spi.ErrUnevaluableLeaf},
		{"InvalidPattern", spi.ErrInvalidPattern},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, ctx, ref := newStubSearcherService(t, func(_ context.Context, _ spi.Filter, _ spi.SearchOptions) ([]*spi.Entity, error) {
				return nil, fmt.Errorf("plugin detail: %w", tc.err)
			})
			cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: "Alice"}
			_, err := svc.Search(ctx, ref, cond, search.SearchOptions{Limit: 10})

			var appErr *common.AppError
			if !errors.As(err, &appErr) {
				t.Fatalf("want *common.AppError, got %T: %v", err, err)
			}
			if appErr.Status != http.StatusBadRequest || appErr.Code != common.ErrCodeInvalidCondition {
				t.Errorf("got %d/%q, want 400/%s", appErr.Status, appErr.Code, common.ErrCodeInvalidCondition)
			}
			if !errors.Is(err, tc.err) {
				t.Errorf("errors.Is(err, %v) = false; WithCause must preserve the sentinel", tc.err)
			}
		})
	}
}

func TestSearch_IterateSPISentinels_MapTo400(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"UnevaluableLeaf", spi.ErrUnevaluableLeaf},
		{"InvalidPattern", spi.ErrInvalidPattern},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := memory.NewStoreFactory()
			defer base.Close()
			ctx := tenantCtx("tenant-1")
			ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}
			saveMinimalModel(t, ctx, base, ref)

			realStore, _ := base.EntityStore(ctx)
			ses := &searcherEntityStore{
				EntityStore: realStore,
				searchFn: func(context.Context, spi.Filter, spi.SearchOptions) ([]*spi.Entity, error) {
					t.Fatal("Searcher.Search must not be called for an unbounded request")
					return nil, nil
				},
			}
			sies := &searcherIterableEntityStore{
				searcherEntityStore: ses,
				iterateFn: func(context.Context, spi.ModelRef, spi.Filter, spi.IterateOptions) (spi.Iterator, error) {
					return nil, fmt.Errorf("plugin detail: %w", tc.err)
				},
			}
			factory := &searcherIterableFactory{StoreFactory: base, entityStore: sies}
			searchStore, _ := base.AsyncSearchStore(context.Background())
			svc := search.NewSearchService(factory, common.NewTestUUIDGenerator(), searchStore)

			cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: "Alice"}
			_, err := svc.Search(ctx, ref, cond, search.SearchOptions{Limit: 0})

			var appErr *common.AppError
			if !errors.As(err, &appErr) {
				t.Fatalf("want *common.AppError, got %T: %v", err, err)
			}
			if appErr.Status != http.StatusBadRequest || appErr.Code != common.ErrCodeInvalidCondition {
				t.Errorf("got %d/%q, want 400/%s", appErr.Status, appErr.Code, common.ErrCodeInvalidCondition)
			}
			if !errors.Is(err, tc.err) {
				t.Errorf("errors.Is(err, %v) = false; WithCause must preserve the sentinel", tc.err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Real (non-stub) propagation: a bare, typeless leaf ({"kind":"LEAF"}, no
// "types") is a genuine, spec-anticipated schema shape (model_schema.go's
// own FieldsMap doc: "A node declaring ONLY an empty scalar branch still
// emits it") that the condition-type boundary explicitly treats as "no
// constraint; accept" (condition_type_validate.go), yet the type-directed
// leaf kernel cannot evaluate a comparison operator against zero declared
// types (spi.ErrUnevaluableLeaf's own doc: "including an empty/nil declared
// set"). This is not a contrived stub — it is the real disagreement between
// the boundary and the evaluator that ClassifyStoreQueryError exists to
// paper over with a 400 instead of a 500. The memory plugin calls
// spi.Prepare on the WHOLE filter unconditionally (plugins/memory/searcher.go),
// so this reproduces through the REAL plugin, no error-injection needed.
// ---------------------------------------------------------------------------

// saveBareLeafModel registers a model whose single field is a bare
// {"kind":"LEAF"} node — present (a known path) but carrying NO declared
// scalar type.
func saveBareLeafModel(t *testing.T, ctx context.Context, factory *memory.StoreFactory, ref spi.ModelRef, field string) {
	t.Helper()
	raw := []byte(fmt.Sprintf(`{"kind":"OBJECT","children":{%q:{"kind":"LEAF"}}}`, field))
	node, err := schema.Unmarshal(raw)
	if err != nil {
		t.Fatalf("schema.Unmarshal: %v", err)
	}
	ms, err := factory.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	marshalled, err := schema.Marshal(node)
	if err != nil {
		t.Fatalf("schema.Marshal: %v", err)
	}
	if err := ms.Save(ctx, &spi.ModelDescriptor{Ref: ref, Schema: marshalled}); err != nil {
		t.Fatalf("Save model: %v", err)
	}
}

func TestSearch_BareLeafField_RealMemoryPlugin_Bounded_MapsTo400(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()
	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "widget", ModelVersion: "1"}
	saveBareLeafModel(t, ctx, base, ref, "score")

	searchStore, _ := base.AsyncSearchStore(context.Background())
	svc := search.NewSearchService(base, common.NewTestUUIDGenerator(), searchStore)

	cond := &predicate.SimpleCondition{JsonPath: "$.score", OperatorType: "EQUALS", Value: float64(5)}
	_, err := svc.Search(ctx, ref, cond, search.SearchOptions{Limit: 10})

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

func TestSearch_BareLeafField_RealMemoryPlugin_Unbounded_MapsTo400(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()
	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "widget", ModelVersion: "1"}
	saveBareLeafModel(t, ctx, base, ref, "score")

	searchStore, _ := base.AsyncSearchStore(context.Background())
	svc := search.NewSearchService(base, common.NewTestUUIDGenerator(), searchStore)

	cond := &predicate.SimpleCondition{JsonPath: "$.score", OperatorType: "EQUALS", Value: float64(5)}
	_, err := svc.Search(ctx, ref, cond, search.SearchOptions{Limit: 0})

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

// TestSearch_BareLeafField_MatchFallback_MapsTo400ConditionTypeMismatch
// wires the SAME bare-leaf schema shape through search.Service.Search's
// OTHER evaluator: the GetAll+internal/match fallback, forced by a store
// that implements neither spi.Searcher nor spi.Iterable (nonSearcherEntityStore,
// already defined in service_test.go for exactly this purpose). This proves
// the fallback's match.Prepare failure (line ~787) is actually routed through
// ClassifyStoreQueryError now, not merely that the classifier function itself
// knows the mapping.
func TestSearch_BareLeafField_MatchFallback_MapsTo400ConditionTypeMismatch(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()
	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "widget", ModelVersion: "1"}
	saveBareLeafModel(t, ctx, base, ref, "score")

	realStore, _ := base.EntityStore(ctx)
	factory := &nonSearcherFactory{StoreFactory: base, entityStore: &nonSearcherEntityStore{EntityStore: realStore}}
	searchStore, _ := base.AsyncSearchStore(context.Background())
	svc := search.NewSearchService(factory, common.NewTestUUIDGenerator(), searchStore)

	cond := &predicate.SimpleCondition{JsonPath: "$.score", OperatorType: "EQUALS", Value: float64(5)}
	_, err := svc.Search(ctx, ref, cond, search.SearchOptions{Limit: 10})

	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("want *common.AppError, got %T: %v", err, err)
	}
	if appErr.Status != http.StatusBadRequest || appErr.Code != common.ErrCodeConditionTypeMismatch {
		t.Errorf("got %d/%q, want 400/%s", appErr.Status, appErr.Code, common.ErrCodeConditionTypeMismatch)
	}
	if !errors.Is(err, match.ErrUnevaluableLeaf) {
		t.Errorf("errors.Is(err, match.ErrUnevaluableLeaf) = false, err = %v", err)
	}
}
