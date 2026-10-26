package search_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// undecodableData is stored entity data that is not JSON. GetEntity fails on it
// with a 5xx; a search writer must give the same answer, not a null or a gap.
var undecodableData = []byte(`{"name":`)

// expectInternalNothingPartial asserts a 500 whose body is a problem detail
// only: no entity of the result reached the caller.
func expectInternalNothingPartial(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "good-id") || strings.Contains(w.Body.String(), "Alice") {
		t.Errorf("part of the result was sent: %s", w.Body.String())
	}
}

func TestSearchEntities_UndecodableStoredData_Fails(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()
	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}
	saveMinimalModel(t, ctx, base, ref)

	realStore, _ := base.EntityStore(ctx)
	ses := &searcherEntityStore{EntityStore: realStore,
		searchFn: func(_ context.Context, _ spi.Filter, _ spi.SearchOptions) ([]*spi.Entity, error) {
			return []*spi.Entity{
				{Meta: spi.EntityMeta{ID: "good-id", ModelRef: ref}, Data: []byte(`{"name":"Alice"}`)},
				{Meta: spi.EntityMeta{ID: "bad-id", ModelRef: ref}, Data: undecodableData},
			}, nil
		}}
	factory := &searcherFactory{StoreFactory: base, entityStore: ses}
	searchStore, _ := base.AsyncSearchStore(context.Background())
	h := search.NewHandler(search.NewSearchService(factory, common.NewTestUUIDGenerator(), searchStore, newTestConsistency(t, factory)))

	body := `{"type":"simple","jsonPath":"$.name","operatorType":"EQUALS","value":"Alice"}`
	req := httptest.NewRequest(http.MethodPost, "/search/direct/person/1", strings.NewReader(body)).WithContext(ctx)
	w := httptest.NewRecorder()
	h.SearchEntities(w, req, "person", 1, genapi.SearchEntitiesParams{})

	expectInternalNothingPartial(t, w)
}

func TestGetAsyncSearchResults_UndecodableStoredData_Fails(t *testing.T) {
	jobID := uuid.New()
	svc := search.NewSearchService(
		&hydrateFactory{store: &hydrateEntityStore{data: map[string][]byte{
			"good-id": []byte(`{"name":"Alice"}`),
			"bad-id":  undecodableData,
		}}},
		nil, &resultIDsStore{ids: []string{"good-id", "bad-id"}}, newFixedConsistency())

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/search/async/"+jobID.String(), nil).WithContext(tenantCtx("tenant-1"))
	search.NewHandler(svc).GetAsyncSearchResults(w, r, jobID, genapi.GetAsyncSearchResultsParams{})

	expectInternalNothingPartial(t, w)
}
