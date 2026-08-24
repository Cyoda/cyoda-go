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

// spi.ErrInvalidFilterPath is the cross-backend sentinel a plugin returns when
// a Filter.Path (or an OrderSpec.Path) is not the model's path syntax. It is
// the plugins' backstop for a path the engine boundary should already have
// rejected — and COMPATIBILITY.md documents the engine as using it to tell
// "invalid input, 400" from "valid but unpushdownable, fall back".
//
// No such mapping existed. Search's sentinel switch classified two OTHER
// sentinels and returned everything else verbatim, so a plugin-side rejection
// of malformed client input surfaced as a 500 plus a support ticket. These
// tests pin the documented behaviour on every classification site that reaches
// a store with a translated Filter.
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

// TestSearch_IterateInvalidFilterPathSentinel_MapsTo400 covers the unbounded
// (Limit <= 0) sibling branch, which reaches the store through Iterate rather
// than Search and had no sentinel classification at all.
func TestSearch_IterateInvalidFilterPathSentinel_MapsTo400(t *testing.T) {
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
			return nil, fmt.Errorf("plugin detail: %w", spi.ErrInvalidFilterPath)
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
	if appErr.Status != http.StatusBadRequest || appErr.Code != common.ErrCodeInvalidFieldPath {
		t.Errorf("got %d/%q, want 400/%s", appErr.Status, appErr.Code, common.ErrCodeInvalidFieldPath)
	}
	if !errors.Is(err, spi.ErrInvalidFilterPath) {
		t.Errorf("errors.Is(err, ErrInvalidFilterPath) = false; WithCause must preserve the sentinel")
	}
}
