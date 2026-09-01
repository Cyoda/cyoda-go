package search_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

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
//     400 INVALID_CONDITION — the SAME code as the SPI-side sentinels above,
//     not a dedicated CONDITION_TYPE_MISMATCH: match.ErrUnevaluableLeaf
//     itself wraps three distinct causes and only one is ever a type
//     mismatch, and a NOT condition can route the identical leaf through
//     either evaluator depending on translatability alone — the client-
//     visible status must not depend on which one ran. See
//     ClassifyStoreQueryError's own doc for the full rationale.
//
// Before this task both classes classified as nil — an unrecognised store
// error — which the transport layer defaults to 500 SERVER_ERROR plus a
// support ticket for what is, in every case here, a malformed CLIENT INPUT.
//
// This classifier is a BACKSTOP, exactly like the pre-existing
// spi.ErrInvalidFilterPath case it sits beside — not a client-reachable
// path through this repo's own API. Every trigger in this file goes through
// the ModelStore backdoor (a hand-built bare {"kind":"LEAF"} schema node), not
// through ImportModel: ImportModel accepts only the SAMPLE_DATA converter,
// and a null-only sample field yields declared types ["NULL"] (model_schema.go's
// own doc: "a node observed only as null declares NULL at its own path"), not
// an empty set — a comparison against a NULL-typed field is rejected 400 by
// the condition-type boundary before any store is ever reached. The bare-leaf
// shape this file constructs directly is nonetheless a real, spec-anticipated
// one (model_schema.go: "A node declaring ONLY an empty scalar branch still
// emits it, which is what a bare {"kind":"LEAF"} has always meant") that a
// backend genuinely disagrees with the boundary about — precisely the
// commercial-backend obligation (spec §14.4) this classifier exists for.
//
// PARITY WAIVER (test-coverage.md: "a missing cell blocks merge unless
// waived with a one-line reason"): there is no cross-backend e2e/parity
// scenario for this class. e2e/parity.BackendFixture exposes only
// BaseURL/GRPCEndpoint/NewTenant/ComputeTenant — no storage handle
// ("verification is API-only", fixture.go's own doc) — so a parity scenario
// cannot reach the ModelStore backdoor this file uses, and the trigger
// cannot be produced over HTTP at all (the paragraph above). Coverage here
// is instead: the real memory plugin (this file), the real postgres plugin
// (internal/e2e), and internal/match's own evaluator (this file's
// MatchFallback test) — three of the backends the parity suite would have
// covered, exercised individually because the harness that runs them
// together cannot construct the input.

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

func TestClassifyStoreQueryError_MatchUnevaluableLeaf_MapsTo400InvalidCondition(t *testing.T) {
	err := fmt.Errorf("predicate match failed: %w", match.ErrUnevaluableLeaf)
	appErr := search.ClassifyStoreQueryError(err)
	if appErr == nil {
		t.Fatal("ClassifyStoreQueryError(match.ErrUnevaluableLeaf) = nil, want a classified AppError")
	}
	// Same code as the SPI-side spi.ErrUnevaluableLeaf, not a dedicated
	// CONDITION_TYPE_MISMATCH — see ClassifyStoreQueryError's own doc: this
	// sentinel bundles three causes (only one a type mismatch), and a NOT
	// condition can route the identical leaf through either evaluator
	// depending on translatability, so the status must not depend on which
	// evaluator ran.
	if appErr.Status != http.StatusBadRequest || appErr.Code != common.ErrCodeInvalidCondition {
		t.Errorf("got %d/%q, want 400/%s", appErr.Status, appErr.Code, common.ErrCodeInvalidCondition)
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

// TestSearch_BareLeafField_MatchFallback_MapsTo400InvalidCondition wires the
// SAME bare-leaf schema shape through search.Service.Search's OTHER
// evaluator: the GetAll+internal/match fallback, forced by a store that
// implements neither spi.Searcher nor spi.Iterable (nonSearcherEntityStore,
// already defined in service_test.go for exactly this purpose). This proves
// the fallback's match.Prepare failure (line ~787) is actually routed through
// ClassifyStoreQueryError now, not merely that the classifier function itself
// knows the mapping. Asserts 400 INVALID_CONDITION — the same code the SPI
// sentinel above maps to, not CONDITION_TYPE_MISMATCH; see
// ClassifyStoreQueryError's doc for why the two classes are not split.
func TestSearch_BareLeafField_MatchFallback_MapsTo400InvalidCondition(t *testing.T) {
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
	if appErr.Status != http.StatusBadRequest || appErr.Code != common.ErrCodeInvalidCondition {
		t.Errorf("got %d/%q, want 400/%s", appErr.Status, appErr.Code, common.ErrCodeInvalidCondition)
	}
	if !errors.Is(err, match.ErrUnevaluableLeaf) {
		t.Errorf("errors.Is(err, match.ErrUnevaluableLeaf) = false, err = %v", err)
	}
}

// ---------------------------------------------------------------------------
// The async search endpoint: an HTTP-reachable unbounded route.
// ---------------------------------------------------------------------------
//
// POST /search/async leaves SearchOptions.Limit at zero (search/handler.go
// never resolves a default for the async submit path the way /search/direct
// does), so it is the ONE genuinely client-reachable way to drive an
// unbounded (Iterate) search over real HTTP — unlike the service-layer-only
// Limit<=0 branch the other "both routes" tests in this file exercise
// directly. It inherits this task's classifier fix for free (runAsyncJob's
// own Iterate call already routed through ClassifyStoreQueryError before this
// task, exactly like TestAsyncSearchJob_StoreSentinelIsClassified above pins
// for spi.ErrInvalidFilterPath), but nothing exercised it end to end for the
// new sentinels — including how the domain code renders the classified error
// into the job record, which is the only report an async caller ever gets
// (jobFailureMessage's own doc: "AppError.Error() returns the client-safe
// Message alone").

// TestAsyncSearchJob_BareLeafField_RendersInvalidConditionMessage mirrors
// TestAsyncSearchJob_StoreSentinelIsClassified's shape exactly, but drives
// the REAL memory plugin (no stub) with the same bare-leaf-no-types schema
// this file's other tests use, through the full SubmitAsync -> runAsyncJob
// -> writeAsyncFailure -> searchStore.GetJob round trip.
func TestAsyncSearchJob_BareLeafField_RendersInvalidConditionMessage(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()
	ctx := tenantCtx("tenant-async-bare-leaf")
	ref := spi.ModelRef{EntityName: "widget", ModelVersion: "1"}
	saveBareLeafModel(t, ctx, base, ref, "score")

	searchStore, _ := base.AsyncSearchStore(context.Background())
	svc := search.NewSearchService(base, common.NewTestUUIDGenerator(), searchStore)

	cond := &predicate.SimpleCondition{JsonPath: "$.score", OperatorType: "EQUALS", Value: float64(5)}
	jobID, err := svc.SubmitAsync(ctx, ref, cond, search.SearchOptions{})
	if err != nil {
		t.Fatalf("SubmitAsync: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	var status search.SearchJobStatus
	for time.Now().Before(deadline) {
		status, err = svc.GetAsyncStatus(ctx, jobID)
		if err != nil {
			t.Fatalf("GetAsyncStatus: %v", err)
		}
		if status.Status == "FAILED" || status.Status == "SUCCESSFUL" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if status.Status != "FAILED" {
		t.Fatalf("status = %q, want FAILED", status.Status)
	}

	job, err := searchStore.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if !strings.Contains(job.Error, "INVALID_CONDITION") {
		t.Errorf("job error = %q, want it to carry INVALID_CONDITION — an async caller's ONLY report of why the job failed", job.Error)
	}
	// Gate 3 / jobFailureMessage's own contract: the classified message is the
	// client-safe one (AppError.Error()), never the store's internal detail
	// or the generic fallback a bare sentinel used to collapse into.
	if strings.Contains(job.Error, "ExpandLeaf") || strings.Contains(job.Error, "parses into no declared type") {
		t.Errorf("job error leaks internal detail: %q", job.Error)
	}
	if job.Error == "search failed unexpectedly" {
		t.Error("job error is the generic unclassified fallback — the fix did not reach the async path")
	}
}

// captureSlog redirects the default logger into a buffer at DEBUG threshold
// (so a demoted-from-WARN log line is still captured) for the duration of a
// single call, and restores the previous default afterward.
func captureSlog(t *testing.T, f func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)
	f()
	return buf.String()
}

// TestClassifyStoreQueryError_UnevaluableLeafLogSeverity pins the
// security-review fix: spi.ErrUnevaluableLeaf's and match.ErrUnevaluableLeaf's
// single most common cause, by far, is a field with NO declared type —
// condition_type_validate.go's boundary deliberately treats that as "no
// constraint; accept" rather than rejecting it, so the SPI/match kernels
// refusing to evaluate it is the DESIGNED interaction between an
// intentionally permissive boundary and a kernel that must have a type to
// compare against — not a boundary/backend disagreement worth an
// investigate-me WARN. TestSearch_BareLeafField_Postgres_DirectSearch_
// 400InvalidCondition (internal/e2e) pins this exact case as a documented,
// ordinary 400. spi.ErrInvalidPattern is different: ValidatePatterns
// pre-validates with the SAME derivation before a query ever reaches a
// backend, so reaching it there IS a genuine inconsistency and must stay a
// WARN.
func TestClassifyStoreQueryError_UnevaluableLeafLogSeverity(t *testing.T) {
	t.Run("spi.ErrUnevaluableLeaf is not logged as an alarm", func(t *testing.T) {
		err := fmt.Errorf("%w: operand %q for op %q: %v", spi.ErrUnevaluableLeaf, "abc", "GT",
			errors.New(`ExpandLeaf: operand "abc" parses into no declared type`))
		out := captureSlog(t, func() { search.ClassifyStoreQueryError(err) })
		if strings.Contains(out, `"level":"WARN"`) {
			t.Errorf("expected the documented-normal no-declared-type case NOT to log at WARN, got: %s", out)
		}
	})

	t.Run("match.ErrUnevaluableLeaf is not logged as an alarm", func(t *testing.T) {
		err := fmt.Errorf("predicate match failed: %w", match.ErrUnevaluableLeaf)
		out := captureSlog(t, func() { search.ClassifyStoreQueryError(err) })
		if strings.Contains(out, `"level":"WARN"`) {
			t.Errorf("expected the documented-normal no-declared-type case NOT to log at WARN, got: %s", out)
		}
	})

	t.Run("spi.ErrInvalidPattern still logs at WARN: a genuine boundary/backend disagreement", func(t *testing.T) {
		err := fmt.Errorf("plugin detail: %w", spi.ErrInvalidPattern)
		out := captureSlog(t, func() { search.ClassifyStoreQueryError(err) })
		if !strings.Contains(out, `"level":"WARN"`) {
			t.Errorf("expected spi.ErrInvalidPattern to keep its WARN — it means the pre-validated pattern and the kernel disagree, got: %s", out)
		}
	})

	t.Run("spi.ErrInvalidFilterPath still logs at WARN: unrelated to this fix", func(t *testing.T) {
		err := fmt.Errorf("plugin detail: %w", spi.ErrInvalidFilterPath)
		out := captureSlog(t, func() { search.ClassifyStoreQueryError(err) })
		if !strings.Contains(out, `"level":"WARN"`) {
			t.Errorf("expected spi.ErrInvalidFilterPath's WARN to be untouched, got: %s", out)
		}
	})
}
