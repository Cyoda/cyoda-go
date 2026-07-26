package search_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// doSearch issues a direct search against a fresh in-memory model/store,
// applying the given raw query suffix (e.g. "?limit=0") to build the
// request params. Only the "limit" query parameter is threaded through —
// that is the only knob these tests need.
func doSearch(t *testing.T, query string) *httptest.ResponseRecorder {
	t.Helper()
	base := memory.NewStoreFactory()
	defer base.Close()
	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}
	saveMinimalModel(t, ctx, base, ref)

	searchStore, _ := base.AsyncSearchStore(context.Background())
	h := search.NewHandler(search.NewSearchService(base, common.NewTestUUIDGenerator(), searchStore))

	body := `{"type":"simple","jsonPath":"$.name","operatorType":"EQUALS","value":"Alice"}`
	req := httptest.NewRequest(http.MethodPost, "/search/direct/person/1"+query, strings.NewReader(body)).WithContext(ctx)
	rr := httptest.NewRecorder()

	params := genapi.SearchEntitiesParams{}
	if q, err := url.ParseQuery(strings.TrimPrefix(query, "?")); err == nil {
		if lim := q.Get("limit"); lim != "" {
			params.Limit = &lim
		}
	}

	h.SearchEntities(rr, req, "person", 1, params)
	return rr
}

// problemErrorCode extracts properties.errorCode from a ProblemDetail
// response body, mirroring the inline parsing already used by
// TestSearchEntities_ResultLimitSentinel_Returns400.
func problemErrorCode(body string) string {
	var obj map[string]any
	_ = json.Unmarshal([]byte(body), &obj)
	props, _ := obj["properties"].(map[string]any)
	if props == nil {
		return ""
	}
	code, _ := props["errorCode"].(string)
	return code
}

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

// limit=0 previously reached the SPI as Limit 0, which means UNBOUNDED — an
// unbounded synchronous NDJSON search, and a way around the cap this endpoint
// exists to enforce. Reject it, consistent with how limit > MaxPageSize is
// rejected rather than clamped.
func TestSearchEntities_LimitZeroRejected(t *testing.T) {
	rr := doSearch(t, "?limit=0")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("limit=0: got status %d, want 400", rr.Code)
	}
	if code := problemErrorCode(rr.Body.String()); code != common.ErrCodeBadRequest {
		t.Fatalf("limit=0: got errorCode %q, want %s", code, common.ErrCodeBadRequest)
	}
}

func TestSearchEntities_LimitNegativeRejected(t *testing.T) {
	rr := doSearch(t, "?limit=-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("limit=-1: got status %d, want 400", rr.Code)
	}
}
