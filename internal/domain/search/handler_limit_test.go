package search_test

import (
	"context"
	"encoding/json"
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

func TestSearchEntities_OmittedLimitDefaultsTo1000(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()
	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}
	saveMinimalModel(t, ctx, base, ref)

	realStore, _ := base.EntityStore(ctx)
	ses := &searcherEntityStore{EntityStore: realStore,
		searchFn: func(_ context.Context, _ spi.Filter, _ spi.SearchOptions) ([]*spi.Entity, error) { return nil, nil }}
	factory := &searcherFactory{StoreFactory: base, entityStore: ses}
	searchStore, _ := base.AsyncSearchStore(context.Background())
	h := search.NewHandler(search.NewSearchService(factory, common.NewTestUUIDGenerator(), searchStore))

	body := `{"type":"simple","jsonPath":"$.name","operatorType":"EQUALS","value":"Alice"}`
	req := httptest.NewRequest(http.MethodPost, "/search/direct/person/1", strings.NewReader(body)).WithContext(ctx)
	rr := httptest.NewRecorder()
	h.SearchEntities(rr, req, "person", 1, genapi.SearchEntitiesParams{}) // params.Limit == nil

	if ses.capturedOpts.Limit != 1000 {
		t.Errorf("omitted limit → spiLimit %d, want 1000", ses.capturedOpts.Limit)
	}
}

func TestSearchEntities_ResultLimitSentinel_Returns400(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()
	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}
	saveMinimalModel(t, ctx, base, ref)
	realStore, _ := base.EntityStore(ctx)
	ses := &searcherEntityStore{EntityStore: realStore,
		searchFn: func(_ context.Context, _ spi.Filter, _ spi.SearchOptions) ([]*spi.Entity, error) {
			return nil, spi.ErrSearchResultLimitExceeded
		}}
	searchStore, _ := base.AsyncSearchStore(context.Background())
	h := search.NewHandler(search.NewSearchService(&searcherFactory{StoreFactory: base, entityStore: ses}, common.NewTestUUIDGenerator(), searchStore))

	body := `{"type":"simple","jsonPath":"$.name","operatorType":"EQUALS","value":"Alice"}`
	req := httptest.NewRequest(http.MethodPost, "/search/direct/person/1", strings.NewReader(body)).WithContext(ctx)
	rr := httptest.NewRecorder()
	h.SearchEntities(rr, req, "person", 1, genapi.SearchEntitiesParams{})

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	// error body is a ProblemDetail with properties.errorCode
	var obj map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &obj)
	props, _ := obj["properties"].(map[string]any)
	if props == nil || props["errorCode"] != common.ErrCodeSearchResultLimit {
		t.Errorf("errorCode = %v, want %s; body=%s", props, common.ErrCodeSearchResultLimit, rr.Body.String())
	}
}
