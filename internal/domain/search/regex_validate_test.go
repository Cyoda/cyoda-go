package search_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// newRegexTestService wires a fresh in-memory SearchService plus a
// registered model, mirroring the setup used throughout service_test.go.
func newRegexTestService(t *testing.T, tenant string, ref spi.ModelRef) (*search.SearchService, context.Context) {
	t.Helper()
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { _ = factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := factory.AsyncSearchStore(context.Background())
	svc := search.NewSearchService(factory, uuids, searchStore)

	ctx := tenantCtx(tenant)
	saveMinimalModel(t, ctx, factory, ref)
	return svc, ctx
}

func assertInvalidCondition(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error for malformed regex pattern, got nil")
	}
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *common.AppError, got %T: %v", err, err)
	}
	if appErr.Status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", appErr.Status)
	}
	if appErr.Code != common.ErrCodeInvalidCondition {
		t.Errorf("code = %q, want %q", appErr.Code, common.ErrCodeInvalidCondition)
	}
}

// TestSearch_MalformedRegex_Rejected verifies that a MATCHES_PATTERN
// condition carrying an unparsable regex ("(" — unterminated group) is
// rejected with 400 INVALID_CONDITION before the filter tree is built,
// closing the fail-open regression left by Task 6's delegation to the
// error-free spi.MatchFilter.
func TestSearch_MalformedRegex_Rejected(t *testing.T) {
	ref := spi.ModelRef{EntityName: "regex-model", ModelVersion: "1"}
	svc, ctx := newRegexTestService(t, "tenant-regex", ref)

	cond := &predicate.SimpleCondition{
		JsonPath:     "$.name",
		OperatorType: "MATCHES_PATTERN",
		Value:        "(",
	}

	_, err := svc.Search(ctx, ref, cond, search.SearchOptions{})
	assertInvalidCondition(t, err)
}

// TestSearch_ValidRegex_Accepted verifies a well-formed pattern passes
// validation (and the search executes without error).
func TestSearch_ValidRegex_Accepted(t *testing.T) {
	ref := spi.ModelRef{EntityName: "regex-model-valid", ModelVersion: "1"}
	svc, ctx := newRegexTestService(t, "tenant-regex-valid", ref)

	cond := &predicate.SimpleCondition{
		JsonPath:     "$.name",
		OperatorType: "MATCHES_PATTERN",
		Value:        "^a.*z$",
	}

	_, err := svc.Search(ctx, ref, cond, search.SearchOptions{})
	if err != nil {
		t.Fatalf("expected success for valid pattern, got: %v", err)
	}
}

// TestSearch_MalformedRegex_Nested_Rejected verifies the condition tree is
// walked fully: a malformed pattern nested inside an AND/OR group must be
// found and rejected, not just top-level SimpleConditions.
func TestSearch_MalformedRegex_Nested_Rejected(t *testing.T) {
	ref := spi.ModelRef{EntityName: "regex-model-nested", ModelVersion: "1"}
	svc, ctx := newRegexTestService(t, "tenant-regex-nested", ref)

	cond := &predicate.GroupCondition{
		Operator: "AND",
		Conditions: []predicate.Condition{
			&predicate.SimpleCondition{
				JsonPath:     "$.age",
				OperatorType: "GREATER_THAN",
				Value:        float64(10),
			},
			&predicate.GroupCondition{
				Operator: "OR",
				Conditions: []predicate.Condition{
					&predicate.SimpleCondition{
						JsonPath:     "$.name",
						OperatorType: "MATCHES_PATTERN",
						Value:        "(",
					},
					&predicate.SimpleCondition{
						JsonPath:     "$.name",
						OperatorType: "EQUALS",
						Value:        "Alice",
					},
				},
			},
		},
	}

	_, err := svc.Search(ctx, ref, cond, search.SearchOptions{})
	assertInvalidCondition(t, err)
}

// TestSubmitAsync_MalformedRegex_Rejected mirrors the sync-search case for
// the async submit path: no job should ever be created for a malformed
// pattern (issue #77's synchronous-validation contract extended to regex).
func TestSubmitAsync_MalformedRegex_Rejected(t *testing.T) {
	ref := spi.ModelRef{EntityName: "regex-model-async", ModelVersion: "1"}
	svc, ctx := newRegexTestService(t, "tenant-regex-async", ref)

	cond := &predicate.SimpleCondition{
		JsonPath:     "$.name",
		OperatorType: "MATCHES_PATTERN",
		Value:        "(",
	}

	jobID, err := svc.SubmitAsync(ctx, ref, cond, search.SearchOptions{})
	assertInvalidCondition(t, err)
	if jobID != "" {
		t.Errorf("expected no job ID to be created, got %q", jobID)
	}
}

// TestSubmitAsync_ValidRegex_Accepted mirrors the accept case for SubmitAsync.
func TestSubmitAsync_ValidRegex_Accepted(t *testing.T) {
	ref := spi.ModelRef{EntityName: "regex-model-async-valid", ModelVersion: "1"}
	svc, ctx := newRegexTestService(t, "tenant-regex-async-valid", ref)

	cond := &predicate.SimpleCondition{
		JsonPath:     "$.name",
		OperatorType: "MATCHES_PATTERN",
		Value:        "^a.*z$",
	}

	jobID, err := svc.SubmitAsync(ctx, ref, cond, search.SearchOptions{})
	if err != nil {
		t.Fatalf("expected success for valid pattern, got: %v", err)
	}
	if jobID == "" {
		t.Error("expected a job ID to be created")
	}
}

// TestSearch_MalformedRegex_LifecycleCondition_Rejected verifies that
// MATCHES_PATTERN on a LifecycleCondition (e.g. state) is validated too —
// lifecycleToFilter (filter_translate.go) pushes it down via the same
// spi.FilterMatchesRegex path as SimpleCondition, so it carries the
// identical fail-open exposure.
func TestSearch_MalformedRegex_LifecycleCondition_Rejected(t *testing.T) {
	ref := spi.ModelRef{EntityName: "regex-model-lifecycle", ModelVersion: "1"}
	svc, ctx := newRegexTestService(t, "tenant-regex-lifecycle", ref)

	cond := &predicate.LifecycleCondition{
		Field:        "state",
		OperatorType: "MATCHES_PATTERN",
		Value:        "(",
	}

	_, err := svc.Search(ctx, ref, cond, search.SearchOptions{})
	assertInvalidCondition(t, err)
}
