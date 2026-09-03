package search_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
)

// SubmitAsync translates the condition before persisting the job, so a
// condition that cannot be translated is refused at submission.
func TestSubmitAsync_TranslationFailure_Is400(t *testing.T) {
	svc, ctx, ref := newContractFixture(t)
	_, err := svc.SubmitAsync(ctx, ref, unknownCondition{}, search.SearchOptions{Limit: 10})
	var appErr *common.AppError
	if !errors.As(err, &appErr) || appErr.Status != http.StatusBadRequest || appErr.Code != common.ErrCodeInvalidCondition {
		t.Fatalf("got %v, want 400 %s", err, common.ErrCodeInvalidCondition)
	}
}

// As in TestSearch_NilCondition_Is400InvalidCondition, the status is not what
// the guard buys — the submit-time translation would answer the same 400. The
// message is, and so is the ordering: the refusal lands before the model
// store is touched at all.
func TestSubmitAsync_NilCondition_Is400(t *testing.T) {
	svc, ctx, ref := newContractFixture(t)
	_, err := svc.SubmitAsync(ctx, ref, nil, search.SearchOptions{Limit: 10})
	var appErr *common.AppError
	if !errors.As(err, &appErr) || appErr.Status != http.StatusBadRequest || appErr.Code != common.ErrCodeInvalidCondition {
		t.Fatalf("got %v, want 400 %s", err, common.ErrCodeInvalidCondition)
	}
	if !strings.Contains(appErr.Message, "condition is required") {
		t.Errorf("message = %q, want it to name the missing condition", appErr.Message)
	}
}
