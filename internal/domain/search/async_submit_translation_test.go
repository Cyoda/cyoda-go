package search_test

import (
	"errors"
	"net/http"
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

func TestSubmitAsync_NilCondition_Is400(t *testing.T) {
	svc, ctx, ref := newContractFixture(t)
	_, err := svc.SubmitAsync(ctx, ref, nil, search.SearchOptions{Limit: 10})
	var appErr *common.AppError
	if !errors.As(err, &appErr) || appErr.Status != http.StatusBadRequest || appErr.Code != common.ErrCodeInvalidCondition {
		t.Fatalf("got %v, want 400 %s", err, common.ErrCodeInvalidCondition)
	}
}
