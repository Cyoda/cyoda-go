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
)

// spi.ErrInvalidFilterPath is the cross-backend sentinel a plugin returns when
// a Filter.Path (or an OrderSpec.Path) is not the model's path syntax. It is
// the plugins' backstop for a path the engine boundary should already have
// rejected — and COMPATIBILITY.md documents the engine as using it to tell
// "invalid input, 400" from a storage failure.
//
// No such mapping existed. Search's sentinel switch classified two OTHER
// sentinels and returned everything else verbatim, so a plugin-side rejection
// of malformed client input surfaced as a 500 plus a support ticket. These
// This pins the documented behaviour on the classification site that reaches
// the store with a translated Filter. The async executor's Iterate route is
// pinned by TestAsyncSearchJob_StoreSentinelIsClassified.
func TestSearch_SearcherInvalidFilterPathSentinel_MapsTo400(t *testing.T) {
	svc, ctx, ref := newStubSearcherService(t, func(_ context.Context, _ spi.Filter, _ spi.SearchOptions) ([]*spi.Entity, error) {
		return nil, fmt.Errorf("plugin detail: %w", spi.ErrInvalidFilterPath)
	})
	cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: "Alice"}
	_, err := svc.Search(ctx, ref, cond, search.SearchOptions{Limit: 10})

	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("want *common.AppError, got %T: %v", err, err)
	}
	if appErr.Status != http.StatusBadRequest || appErr.Code != common.ErrCodeInvalidFieldPath {
		t.Errorf("got %d/%q, want 400/%s", appErr.Status, appErr.Code, common.ErrCodeInvalidFieldPath)
	}
	if !errors.Is(err, spi.ErrInvalidFilterPath) {
		t.Errorf("errors.Is(err, ErrInvalidFilterPath) = false; WithCause must preserve the sentinel")
	}
}
