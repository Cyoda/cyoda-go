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

// newPatternTestService wires a fresh in-memory SearchService plus a
// registered model, mirroring the setup used throughout service_test.go.
func newPatternTestService(t *testing.T, tenant string, ref spi.ModelRef) (*search.SearchService, context.Context) {
	t.Helper()
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { _ = factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := factory.AsyncSearchStore(context.Background())
	svc := search.NewSearchService(factory, uuids, searchStore)

	ctx := tenantCtx(tenant)
	// These tests address $.name (string operators) and $.age (a numeric
	// comparison), so both must be declared with the types they are compared
	// against — an undeclared path now fails the request before any pattern
	// is examined, and a String "age" would fail on the operand's type.
	saveModelWithFields(t, ctx, factory, ref, map[string]schema.DataType{
		"name": schema.String,
		"age":  schema.Integer,
	})
	return svc, ctx
}

func assertInvalidCondition(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error for malformed pattern, got nil")
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
// error-free spi.PreparedFilter.Match kernel.
func TestSearch_MalformedRegex_Rejected(t *testing.T) {
	ref := spi.ModelRef{EntityName: "regex-model", ModelVersion: "1"}
	svc, ctx := newPatternTestService(t, "tenant-regex", ref)

	cond := &predicate.SimpleCondition{
		JsonPath:     "$.name",
		OperatorType: "MATCHES_PATTERN",
		Value:        "(",
	}

	_, err := svc.Search(ctx, ref, cond, search.SearchOptions{Limit: 10})
	assertInvalidCondition(t, err)
}

// TestSearch_ValidRegex_Accepted verifies a well-formed pattern passes
// validation (and the search executes without error).
func TestSearch_ValidRegex_Accepted(t *testing.T) {
	ref := spi.ModelRef{EntityName: "regex-model-valid", ModelVersion: "1"}
	svc, ctx := newPatternTestService(t, "tenant-regex-valid", ref)

	cond := &predicate.SimpleCondition{
		JsonPath:     "$.name",
		OperatorType: "MATCHES_PATTERN",
		Value:        "^a.*z$",
	}

	_, err := svc.Search(ctx, ref, cond, search.SearchOptions{Limit: 10})
	if err != nil {
		t.Fatalf("expected success for valid pattern, got: %v", err)
	}
}

// TestSearch_MalformedRegex_Nested_Rejected verifies the condition tree is
// walked fully: a malformed pattern nested inside an AND/OR group must be
// found and rejected, not just top-level SimpleConditions.
func TestSearch_MalformedRegex_Nested_Rejected(t *testing.T) {
	ref := spi.ModelRef{EntityName: "regex-model-nested", ModelVersion: "1"}
	svc, ctx := newPatternTestService(t, "tenant-regex-nested", ref)

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

	_, err := svc.Search(ctx, ref, cond, search.SearchOptions{Limit: 10})
	assertInvalidCondition(t, err)
}

// TestSubmitAsync_MalformedRegex_Rejected mirrors the sync-search case for
// the async submit path: no job should ever be created for a malformed
// pattern (issue #77's synchronous-validation contract extended to regex).
func TestSubmitAsync_MalformedRegex_Rejected(t *testing.T) {
	ref := spi.ModelRef{EntityName: "regex-model-async", ModelVersion: "1"}
	svc, ctx := newPatternTestService(t, "tenant-regex-async", ref)

	cond := &predicate.SimpleCondition{
		JsonPath:     "$.name",
		OperatorType: "MATCHES_PATTERN",
		Value:        "(",
	}

	jobID, err := svc.SubmitAsync(ctx, ref, cond, search.SearchOptions{Limit: 10})
	assertInvalidCondition(t, err)
	if jobID != "" {
		t.Errorf("expected no job ID to be created, got %q", jobID)
	}
}

// TestSubmitAsync_ValidRegex_Accepted mirrors the accept case for SubmitAsync.
func TestSubmitAsync_ValidRegex_Accepted(t *testing.T) {
	ref := spi.ModelRef{EntityName: "regex-model-async-valid", ModelVersion: "1"}
	svc, ctx := newPatternTestService(t, "tenant-regex-async-valid", ref)

	cond := &predicate.SimpleCondition{
		JsonPath:     "$.name",
		OperatorType: "MATCHES_PATTERN",
		Value:        "^a.*z$",
	}

	jobID, err := svc.SubmitAsync(ctx, ref, cond, search.SearchOptions{Limit: 10})
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
	svc, ctx := newPatternTestService(t, "tenant-regex-lifecycle", ref)

	cond := &predicate.LifecycleCondition{
		Field:        "state",
		OperatorType: "MATCHES_PATTERN",
		Value:        "(",
	}

	_, err := svc.Search(ctx, ref, cond, search.SearchOptions{Limit: 10})
	assertInvalidCondition(t, err)
}

// TestSearch_AnchorSkewPattern_Rejected closes the accept-then-fail direction
// of the validator/kernel skew. An unterminated \Q swallows whatever follows
// it, so "\Q" compiles fine on its own but not once the kernel wraps it as
// `\A(?:\Q)\z` — the appended `)\z` is quoted away and the group never
// closes. Validating bare therefore returned 200 with a job id for an operand
// the evaluator cannot compile.
func TestSearch_AnchorSkewPattern_Rejected(t *testing.T) {
	ref := spi.ModelRef{EntityName: "pattern-anchor-skew", ModelVersion: "1"}
	svc, ctx := newPatternTestService(t, "tenant-pattern-anchor-skew", ref)

	cond := &predicate.SimpleCondition{
		JsonPath:     "$.name",
		OperatorType: "MATCHES_PATTERN",
		Value:        `\Q`,
	}

	_, err := svc.Search(ctx, ref, cond, search.SearchOptions{Limit: 10})
	assertInvalidCondition(t, err)
}

// TestSubmitAsync_AnchorSkewPattern_Rejected mirrors the sync case: the
// accept-then-fail skew was worst on the async path, where the client got a
// 200 and a job id and only discovered the problem when the job went FAILED.
func TestSubmitAsync_AnchorSkewPattern_Rejected(t *testing.T) {
	ref := spi.ModelRef{EntityName: "pattern-anchor-skew-async", ModelVersion: "1"}
	svc, ctx := newPatternTestService(t, "tenant-pattern-anchor-skew-async", ref)

	cond := &predicate.SimpleCondition{
		JsonPath:     "$.name",
		OperatorType: "MATCHES_PATTERN",
		Value:        `\Q`,
	}

	jobID, err := svc.SubmitAsync(ctx, ref, cond, search.SearchOptions{Limit: 10})
	assertInvalidCondition(t, err)
	if jobID != "" {
		t.Errorf("expected no job ID to be created, got %q", jobID)
	}
}

// TestSearch_UnbalancedParenPattern_Rejected pins the other side of the same
// rule. ")|(" does not parse standalone, but anchoring is concatenation, so
// `\A(?:)|()\z` compiles — an alternation whose first branch matches the empty
// string at position 0, i.e. every stored value. Widening the validator to the
// anchored accept-set would have published a match-everything operand as
// contract; requiring a standalone parse keeps that family unrepresentable.
func TestSearch_UnbalancedParenPattern_Rejected(t *testing.T) {
	for _, pattern := range []string{`)|(`, `)x(`, `)$|(`} {
		t.Run(pattern, func(t *testing.T) {
			ref := spi.ModelRef{EntityName: "pattern-unbalanced", ModelVersion: "1"}
			svc, ctx := newPatternTestService(t, "tenant-pattern-unbalanced-"+pattern, ref)

			cond := &predicate.SimpleCondition{
				JsonPath:     "$.name",
				OperatorType: "MATCHES_PATTERN",
				Value:        pattern,
			}

			_, err := svc.Search(ctx, ref, cond, search.SearchOptions{Limit: 10})
			assertInvalidCondition(t, err)
		})
	}
}

// TestSearch_ValidPattern_StillAccepted guards the accept side against
// over-tightening: patterns that compile both standalone and anchored are
// unaffected by adopting the kernel's derivation.
func TestSearch_ValidPattern_StillAccepted(t *testing.T) {
	for _, pattern := range []string{`a|b`, `^foo`, `A.*e`, ``} {
		t.Run(pattern, func(t *testing.T) {
			ref := spi.ModelRef{EntityName: "pattern-valid", ModelVersion: "1"}
			svc, ctx := newPatternTestService(t, "tenant-pattern-valid-"+pattern, ref)

			cond := &predicate.SimpleCondition{
				JsonPath:     "$.name",
				OperatorType: "MATCHES_PATTERN",
				Value:        pattern,
			}

			if _, err := svc.Search(ctx, ref, cond, search.SearchOptions{Limit: 10}); err != nil {
				t.Fatalf("expected success for valid pattern %q, got: %v", pattern, err)
			}
		})
	}
}

// TestSearch_MalformedLike_Rejected extends boundary validation to LIKE.
// A trailing unpaired backslash is the one malformed operand the glob grammar
// admits. Unvalidated it reaches the evaluator, where Prepare's contract turns
// it into a leaf that never matches — an empty page where a 400 belongs, and a
// cross-backend divergence: the in-tree evaluators return empty while the
// commercial async evaluator fails the whole job.
func TestSearch_MalformedLike_Rejected(t *testing.T) {
	ref := spi.ModelRef{EntityName: "pattern-like-malformed", ModelVersion: "1"}
	svc, ctx := newPatternTestService(t, "tenant-pattern-like-malformed", ref)

	cond := &predicate.SimpleCondition{
		JsonPath:     "$.name",
		OperatorType: "LIKE",
		Value:        `abc\`,
	}

	_, err := svc.Search(ctx, ref, cond, search.SearchOptions{Limit: 10})
	assertInvalidCondition(t, err)
}

// TestSubmitAsync_MalformedLike_Rejected mirrors the sync case: no job is
// created for a LIKE operand the kernel cannot expand.
func TestSubmitAsync_MalformedLike_Rejected(t *testing.T) {
	ref := spi.ModelRef{EntityName: "pattern-like-malformed-async", ModelVersion: "1"}
	svc, ctx := newPatternTestService(t, "tenant-pattern-like-malformed-async", ref)

	cond := &predicate.SimpleCondition{
		JsonPath:     "$.name",
		OperatorType: "LIKE",
		Value:        `abc\`,
	}

	jobID, err := svc.SubmitAsync(ctx, ref, cond, search.SearchOptions{Limit: 10})
	assertInvalidCondition(t, err)
	if jobID != "" {
		t.Errorf("expected no job ID to be created, got %q", jobID)
	}
}

// TestSearch_MalformedLike_LifecycleCondition_Rejected covers the lifecycle
// leaf, which lifecycleToFilter pushes down through the identical FilterLike
// path and so carries the identical exposure.
func TestSearch_MalformedLike_LifecycleCondition_Rejected(t *testing.T) {
	ref := spi.ModelRef{EntityName: "pattern-like-lifecycle", ModelVersion: "1"}
	svc, ctx := newPatternTestService(t, "tenant-pattern-like-lifecycle", ref)

	cond := &predicate.LifecycleCondition{
		Field:        "state",
		OperatorType: "LIKE",
		Value:        `abc\`,
	}

	_, err := svc.Search(ctx, ref, cond, search.SearchOptions{Limit: 10})
	assertInvalidCondition(t, err)
}

// TestSearch_MalformedLike_NestedInGroup_Rejected verifies the whole tree is
// walked for LIKE, not only its top-level clause.
func TestSearch_MalformedLike_NestedInGroup_Rejected(t *testing.T) {
	ref := spi.ModelRef{EntityName: "pattern-like-nested", ModelVersion: "1"}
	svc, ctx := newPatternTestService(t, "tenant-pattern-like-nested", ref)

	cond := &predicate.GroupCondition{
		Operator: "AND",
		Conditions: []predicate.Condition{
			&predicate.SimpleCondition{
				JsonPath:     "$.age",
				OperatorType: "GREATER_THAN",
				Value:        float64(10),
			},
			&predicate.SimpleCondition{
				JsonPath:     "$.name",
				OperatorType: "LIKE",
				Value:        `abc\`,
			},
		},
	}

	_, err := svc.Search(ctx, ref, cond, search.SearchOptions{Limit: 10})
	assertInvalidCondition(t, err)
}

// TestSearch_ValidLike_Accepted guards the accept side of the glob grammar:
// a paired escape and the two wildcards must still pass the boundary.
func TestSearch_ValidLike_Accepted(t *testing.T) {
	for name, pattern := range map[string]string{
		"wildcards":    `a_c%`,
		"pairedEscape": `50\%`,
		"literalOnly":  `plain`,
	} {
		t.Run(name, func(t *testing.T) {
			ref := spi.ModelRef{EntityName: "pattern-like-valid", ModelVersion: "1"}
			svc, ctx := newPatternTestService(t, "tenant-pattern-like-valid-"+name, ref)

			cond := &predicate.SimpleCondition{
				JsonPath:     "$.name",
				OperatorType: "LIKE",
				Value:        pattern,
			}

			if _, err := svc.Search(ctx, ref, cond, search.SearchOptions{Limit: 10}); err != nil {
				t.Fatalf("expected success for valid LIKE %q, got: %v", pattern, err)
			}
		})
	}
}
