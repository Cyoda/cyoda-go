package search_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

// saveMinimalModelSqlite registers a minimal model descriptor against a real
// sqlite factory so EnsureModelRegistered passes. Mirrors saveMinimalModel
// (service_test.go), which is typed to *memory.StoreFactory.
func saveMinimalModelSqlite(t *testing.T, ctx context.Context, factory *sqlite.StoreFactory, ref spi.ModelRef) {
	t.Helper()
	ms, err := factory.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	if err := ms.Save(ctx, &spi.ModelDescriptor{Ref: ref}); err != nil {
		t.Fatalf("Save model: %v", err)
	}
}

// seedScanBudgetEntities saves n entities carrying a "val" field, so a
// MATCHES_PATTERN condition on "$.val" forces a non-pushable residual scan
// over all n rows.
func seedScanBudgetEntities(t *testing.T, ctx context.Context, factory *sqlite.StoreFactory, ref spi.ModelRef, n int) {
	t.Helper()
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	for i := 0; i < n; i++ {
		_, err := store.Save(ctx, &spi.Entity{
			Meta: spi.EntityMeta{
				ID:       fmt.Sprintf("e%d", i),
				ModelRef: ref,
				State:    "NEW",
			},
			Data: []byte(fmt.Sprintf(`{"val":%d}`, i)),
		})
		if err != nil {
			t.Fatalf("Save e%d: %v", i, err)
		}
	}
}

// doScanBudgetSearch issues a direct HTTP search with a non-pushable
// MATCHES_PATTERN condition (mapped to spi.FilterMatchesRegex, which sqlite
// never pushes down — see filter_translate.go's mapOperator and the absence
// of FilterMatchesRegex from plugins/sqlite's planner) over a real sqlite
// backend built with the given scan limit, and returns the HTTP status and
// response body.
func doScanBudgetSearch(t *testing.T, scanLimit, rowCount int) (int, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "scan_budget.db")
	factory, err := sqlite.NewStoreFactoryForTestWithScanLimit(context.Background(), dbPath, scanLimit)
	if err != nil {
		t.Fatalf("sqlite factory: %v", err)
	}
	defer factory.Close()

	ctx := tenantCtx("tenant-scan-budget")
	ref := spi.ModelRef{EntityName: "budgetitem", ModelVersion: "1"}
	saveMinimalModelSqlite(t, ctx, factory, ref)
	seedScanBudgetEntities(t, ctx, factory, ref, rowCount)

	searchStore, err := factory.AsyncSearchStore(context.Background())
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	h := search.NewHandler(search.NewSearchService(factory, common.NewTestUUIDGenerator(), searchStore))

	body := `{"type":"simple","jsonPath":"$.val","operatorType":"MATCHES_PATTERN","value":".*"}`
	req := httptest.NewRequest(http.MethodPost, "/search/direct/budgetitem/1", strings.NewReader(body)).WithContext(ctx)
	rr := httptest.NewRecorder()
	h.SearchEntities(rr, req, "budgetitem", 1, genapi.SearchEntitiesParams{})
	return rr.Code, rr.Body.String()
}

// TestSearchEntities_ScanBudgetExhausted_RealSqlite proves, against a real
// (non-fake) sqlite backend, that exhausting the residual-scan budget
// (CYODA_SQLITE_SEARCH_SCAN_LIMIT / SearchScanLimit) on a non-pushable
// MATCHES_PATTERN condition surfaces as HTTP 400 with ProblemDetail
// properties.errorCode == SCAN_BUDGET_EXHAUSTED, not a 500. This exercises
// the full search.Handler -> search.SearchService -> plugins/sqlite chain
// wired via go.work to the local (in-worktree) sqlite plugin.
func TestSearchEntities_ScanBudgetExhausted_RealSqlite(t *testing.T) {
	const scanLimit = 3
	const rowCount = 10 // > scanLimit, so the residual scan exhausts the budget

	status, respBody := doScanBudgetSearch(t, scanLimit, rowCount)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", status, respBody)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(respBody), &obj); err != nil {
		t.Fatalf("unmarshal body: %v; body=%s", err, respBody)
	}
	props, _ := obj["properties"].(map[string]any)
	if props == nil || props["errorCode"] != common.ErrCodeScanBudgetExhausted {
		t.Errorf("errorCode = %v, want %s; body=%s", props, common.ErrCodeScanBudgetExhausted, respBody)
	}
}

// TestSearchEntities_ScanBudgetExhausted_RealSqlite_SanityHighLimit is the
// non-vacuousness check: the identical MATCHES_PATTERN search over the same
// row count, but with a scan limit comfortably above rowCount, must succeed
// (200) rather than 400/500 — proving the 400 above is caused by the low
// scan limit and not by some unrelated failure in the request shape.
func TestSearchEntities_ScanBudgetExhausted_RealSqlite_SanityHighLimit(t *testing.T) {
	const scanLimit = 1000
	const rowCount = 10

	status, respBody := doScanBudgetSearch(t, scanLimit, rowCount)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (sanity: high scan limit must not exhaust); body=%s", status, respBody)
	}
}
