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
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

func newContractFixture(t *testing.T) (*search.SearchService, context.Context, spi.ModelRef) {
	t.Helper()
	base := memory.NewStoreFactory()
	t.Cleanup(func() { base.Close() })
	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "item", ModelVersion: "1"}
	saveModelWithValAndItemsArray(t, ctx, base, ref)
	saveEntity(t, ctx, base, ref, "e0", []byte(`{"val":0}`))
	searchStore, err := base.AsyncSearchStore(context.Background())
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	return search.NewSearchService(base, common.NewTestUUIDGenerator(), searchStore), ctx, ref
}

// Limit <= 0 is a caller contract violation, not a client-visible status:
// both transports resolve a positive limit before reaching the service.
func TestSearch_NonPositiveLimit_IsContractError(t *testing.T) {
	svc, ctx, ref := newContractFixture(t)
	cond := &predicate.SimpleCondition{JsonPath: "$.val", OperatorType: "EQUALS", Value: 0}
	for _, limit := range []int{0, -1} {
		_, err := svc.Search(ctx, ref, cond, search.SearchOptions{Limit: limit})
		if err == nil {
			t.Fatalf("limit %d: expected an error", limit)
		}
		var appErr *common.AppError
		if errors.As(err, &appErr) {
			t.Fatalf("limit %d: got an AppError (%d %s); a contract violation is a plain error", limit, appErr.Status, appErr.Code)
		}
	}
}

// A nil condition is rejected at the validation boundary.
func TestSearch_NilCondition_Is400InvalidCondition(t *testing.T) {
	svc, ctx, ref := newContractFixture(t)
	_, err := svc.Search(ctx, ref, nil, search.SearchOptions{Limit: 10})
	var appErr *common.AppError
	if !errors.As(err, &appErr) || appErr.Status != http.StatusBadRequest || appErr.Code != common.ErrCodeInvalidCondition {
		t.Fatalf("got %v, want 400 %s", err, common.ErrCodeInvalidCondition)
	}
}

// unknownCondition is a predicate.Condition outside the five wire types:
// validation's type switch accepts what it does not recognise, translation
// rejects it. This is the only way a translation failure is reachable.
type unknownCondition struct{}

func (unknownCondition) Type() string { return "caller-built" }

func TestSearch_TranslationFailure_Is400InvalidCondition(t *testing.T) {
	svc, ctx, ref := newContractFixture(t)
	_, err := svc.Search(ctx, ref, unknownCondition{}, search.SearchOptions{Limit: 10})
	var appErr *common.AppError
	if !errors.As(err, &appErr) || appErr.Status != http.StatusBadRequest || appErr.Code != common.ErrCodeInvalidCondition {
		t.Fatalf("got %v, want 400 %s", err, common.ErrCodeInvalidCondition)
	}
}

// The path-shaped leg of translation failure cannot be reached through
// Search (validation and translation share the grammar); the classifier's
// mapping of the wrapped sentinel is pinned directly.
func TestClassifyStoreQueryError_WrappedInvalidFilterPath_IsInvalidFieldPath(t *testing.T) {
	err := fmt.Errorf("translate: %w", spi.ErrInvalidFilterPath)
	appErr := search.ClassifyStoreQueryError(err)
	if appErr == nil || appErr.Status != http.StatusBadRequest || appErr.Code != common.ErrCodeInvalidFieldPath {
		t.Fatalf("got %v, want 400 %s", appErr, common.ErrCodeInvalidFieldPath)
	}
}
