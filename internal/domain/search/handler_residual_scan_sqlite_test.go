package search_test

import (
	"context"
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

// seedResidualScanEntities saves n entities carrying a string "val" field, so a
// MATCHES_PATTERN condition on "$.val" forces a non-pushable residual scan over
// all n rows AND matches every one of them. The value must be a string:
// MATCHES_PATTERN is a string regex, so a numeric "val" would scan all n rows
// and match none — which passes a status-only assertion vacuously.
func seedResidualScanEntities(t *testing.T, ctx context.Context, factory *sqlite.StoreFactory, ref spi.ModelRef, n int) {
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
			Data: []byte(fmt.Sprintf(`{"val":"v%d"}`, i)),
		})
		if err != nil {
			t.Fatalf("Save e%d: %v", i, err)
		}
	}
}

// TestSearchEntities_ResidualScanIsUnmetered_RealSqlite proves, over the full
// search.Handler -> search.SearchService -> plugins/sqlite chain against a real
// (non-fake) sqlite backend, that a non-pushable MATCHES_PATTERN condition
// forcing a residual scan completes with 200 and every matching row.
//
// This is the acceptance guard for the removed residual-scan budget. sqlite
// used to meter examined rows against CYODA_SQLITE_SEARCH_SCAN_LIMIT and fail
// such a search with 400 SCAN_BUDGET_EXHAUSTED — the one place any backend
// imposed its own bound on search work. Bounding search time is the caller's,
// via the direct-search timeout or async job cancellation; the backend imposes
// none. The row count here comfortably exceeds the smallest budget the old
// machinery was ever configured with in test.
func TestSearchEntities_ResidualScanIsUnmetered_RealSqlite(t *testing.T) {
	const rowCount = 200

	dbPath := filepath.Join(t.TempDir(), "residual_scan.db")
	factory, err := sqlite.NewStoreFactoryForTest(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("sqlite factory: %v", err)
	}
	defer factory.Close()

	ctx := tenantCtx("tenant-residual-scan")
	ref := spi.ModelRef{EntityName: "residualitem", ModelVersion: "1"}
	saveMinimalModelSqlite(t, ctx, factory, ref)
	seedResidualScanEntities(t, ctx, factory, ref, rowCount)

	searchStore, err := factory.AsyncSearchStore(context.Background())
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	h := search.NewHandler(search.NewSearchService(factory, common.NewTestUUIDGenerator(), searchStore))

	// MATCHES_PATTERN maps to spi.FilterMatchesRegex, which sqlite never pushes
	// down (see filter_translate.go's mapOperator and the absence of
	// FilterMatchesRegex from the planner), so every row is examined in Go.
	body := `{"type":"simple","jsonPath":"$.val","operatorType":"MATCHES_PATTERN","value":".*"}`
	req := httptest.NewRequest(http.MethodPost, "/search/direct/residualitem/1", strings.NewReader(body)).WithContext(ctx)
	rr := httptest.NewRecorder()
	h.SearchEntities(rr, req, "residualitem", 1, genapi.SearchEntitiesParams{})

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: a residual scan carries no backend-imposed budget; body=%s",
			rr.Code, rr.Body.String())
	}
	got := parseNDJSON(t, rr.Body.Bytes())
	if len(got) != rowCount {
		t.Errorf("got %d rows, want %d", len(got), rowCount)
	}
}
