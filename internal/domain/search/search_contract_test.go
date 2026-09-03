package search_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
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

// limitProbeStore records whether the store's Search was reached at all.
// The backends reject a non-positive limit themselves, in the same shape the
// service does, so "an error came back" cannot tell the service's guard from
// the plugin's — and the invariant the guard exists for is the stronger one:
// a non-positive limit never reaches a backend
// (docs/cloud-parity/direct-search-bounded-or-fail.md, rule 2).
type limitProbeStore struct {
	spi.EntityStore
	searchCalls int
}

func (s *limitProbeStore) Search(ctx context.Context, filter spi.Filter, opts spi.SearchOptions) ([]*spi.Entity, error) {
	s.searchCalls++
	return s.EntityStore.Search(ctx, filter, opts)
}

type limitProbeFactory struct {
	spi.StoreFactory
	store *limitProbeStore
}

func (f *limitProbeFactory) EntityStore(ctx context.Context) (spi.EntityStore, error) {
	real, err := f.StoreFactory.EntityStore(ctx)
	if err != nil {
		return nil, err
	}
	f.store.EntityStore = real
	return f.store, nil
}

// Limit <= 0 is a caller contract violation, not a client-visible status:
// both transports resolve a positive limit before reaching the service.
func TestSearch_NonPositiveLimit_IsContractError(t *testing.T) {
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
	probe := &limitProbeStore{}
	svc := search.NewSearchService(&limitProbeFactory{StoreFactory: base, store: probe},
		common.NewTestUUIDGenerator(), searchStore)

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
		if probe.searchCalls != 0 {
			t.Fatalf("limit %d: the store's Search was reached %d time(s); a non-positive limit must never reach a backend", limit, probe.searchCalls)
		}
	}

	// The same fixture answers a positive limit through the store, so the
	// assertion above is about the limit and not about the probe never being
	// wired in.
	if _, err := svc.Search(ctx, ref, cond, search.SearchOptions{Limit: 10}); err != nil {
		t.Fatalf("positive limit: %v", err)
	}
	if probe.searchCalls != 1 {
		t.Fatalf("searchCalls = %d after a positive limit, want 1", probe.searchCalls)
	}
}

// A nil condition is rejected at the validation boundary, with a message that
// says what is missing.
//
// Reaching the translator instead would answer the same 400 — spi
// .ConditionToFilter rejects a nil condition too — so the guard is not what
// makes the status right. What it makes right is the diagnostic: "condition is
// required" tells the caller what to send, where the translate arm's
// "cannot be translated to a backend predicate" describes an engine step the
// caller cannot act on. The message is therefore the assertion.
func TestSearch_NilCondition_Is400InvalidCondition(t *testing.T) {
	svc, ctx, ref := newContractFixture(t)
	_, err := svc.Search(ctx, ref, nil, search.SearchOptions{Limit: 10})
	var appErr *common.AppError
	if !errors.As(err, &appErr) || appErr.Status != http.StatusBadRequest || appErr.Code != common.ErrCodeInvalidCondition {
		t.Fatalf("got %v, want 400 %s", err, common.ErrCodeInvalidCondition)
	}
	if !strings.Contains(appErr.Message, "condition is required") {
		t.Errorf("message = %q, want it to name the missing condition", appErr.Message)
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
	// The translator's own error is preserved for errors.Is and for the
	// server-side log, but is NOT interpolated into the client-visible
	// message: it renders Go type names (`unsupported condition type: %T`)
	// that describe the engine's internals rather than the request.
	if strings.Contains(appErr.Message, "search_test.unknownCondition") {
		t.Errorf("client message names an internal Go type: %q", appErr.Message)
	}
	// It survives as the cause, so a server-side log or an errors.Is check
	// downstream still reaches it.
	cause := appErr.Unwrap()
	if cause == nil {
		t.Fatal("the translator's error must be preserved as the cause")
	}
	if !strings.Contains(cause.Error(), "search_test.unknownCondition") {
		t.Errorf("cause lost the translator's detail: %q", cause)
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
