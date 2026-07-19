package search_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// TestHandlerDirectSearch_TrackingReadTrue_ReachesSearchOptions verifies that
// a sync (direct) search request carrying trackingRead=true in the parsed
// request params maps through to SearchOptions.TrackingRead == true at the
// SearchService.Search call — the same DTO-to-SearchOptions boundary the sync
// handler builds at its SearchOptions{...} construction site.
func TestHandlerDirectSearch_TrackingReadTrue_ReachesSearchOptions(t *testing.T) {
	capturedOpts, h := newTrackingReadTestHandler(t)

	body := `{"type":"simple","jsonPath":"$.name","operatorType":"EQUALS","value":"Alice"}`
	req := httptest.NewRequest(http.MethodPost, "/search/direct/person/1", strings.NewReader(body))
	req = req.WithContext(tenantCtx("tenant-1"))
	w := httptest.NewRecorder()

	tr := true
	params := genapi.SearchEntitiesParams{TrackingRead: &tr}
	h.SearchEntities(w, req, "person", 1, params)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if !capturedOpts().TrackingRead {
		t.Errorf("capturedOpts().TrackingRead = false, want true (trackingRead=true on request)")
	}
}

// TestHandlerDirectSearch_TrackingReadAbsent_DefaultsFalse verifies the
// default: when trackingRead is not present on the request, SearchOptions.TrackingRead
// must be false.
func TestHandlerDirectSearch_TrackingReadAbsent_DefaultsFalse(t *testing.T) {
	capturedOpts, h := newTrackingReadTestHandler(t)

	body := `{"type":"simple","jsonPath":"$.name","operatorType":"EQUALS","value":"Alice"}`
	req := httptest.NewRequest(http.MethodPost, "/search/direct/person/1", strings.NewReader(body))
	req = req.WithContext(tenantCtx("tenant-1"))
	w := httptest.NewRecorder()

	params := genapi.SearchEntitiesParams{}
	h.SearchEntities(w, req, "person", 1, params)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if capturedOpts().TrackingRead {
		t.Errorf("capturedOpts().TrackingRead = true, want false (trackingRead absent from request)")
	}
}

// newTrackingReadTestHandler builds a *search.Handler wired to a real
// SearchService backed by the memory plugin, with a spy Searcher that
// captures the spi.SearchOptions passed on each Search call. The returned
// closure reads the most recently captured options.
func newTrackingReadTestHandler(t *testing.T) (func() spi.SearchOptions, *search.Handler) {
	t.Helper()

	base := memory.NewStoreFactory()
	t.Cleanup(func() { _ = base.Close() })

	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}
	saveMinimalModel(t, ctx, base, ref)

	realStore, err := base.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}

	var captured spi.SearchOptions
	ses := &searcherEntityStore{
		EntityStore: realStore,
		searchFn: func(_ context.Context, _ spi.Filter, opts spi.SearchOptions) ([]*spi.Entity, error) {
			captured = opts
			return nil, nil
		},
	}
	factory := &searcherFactory{StoreFactory: base, entityStore: ses}

	uuids := common.NewTestUUIDGenerator()
	searchStore, err := base.AsyncSearchStore(context.Background())
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	svc := search.NewSearchService(factory, uuids, searchStore)

	return func() spi.SearchOptions { return captured }, search.NewHandler(svc)
}
