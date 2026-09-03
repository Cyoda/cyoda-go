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

// --- Task 11: search timeoutMillis — spec param + HTTP handler enforcement ---

// TestSearchEntities_TimeoutMillisZero_Returns400 pins that a non-positive
// timeoutMillis is rejected before the request ever reaches the search
// service, mirroring the write-ops' transactionTimeoutMillis validation
// (common.ValidateRequestTimeoutMillis).
func TestSearchEntities_TimeoutMillisZero_Returns400(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()
	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}
	saveMinimalModel(t, ctx, base, ref)

	searchStore, _ := base.AsyncSearchStore(context.Background())
	h := search.NewHandler(search.NewSearchService(base, common.NewTestUUIDGenerator(), searchStore))

	body := `{"type":"simple","jsonPath":"$.name","operatorType":"EQUALS","value":"Alice"}`
	req := httptest.NewRequest(http.MethodPost, "/search/direct/person/1?timeoutMillis=0", strings.NewReader(body)).WithContext(ctx)
	rr := httptest.NewRecorder()

	millis := int64(0)
	h.SearchEntities(rr, req, "person", 1, genapi.SearchEntitiesParams{TimeoutMillis: &millis})

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if code := problemErrorCode(rr.Body.String()); code != common.ErrCodeBadRequest {
		t.Fatalf("errorCode = %q, want %s; body=%s", code, common.ErrCodeBadRequest, rr.Body.String())
	}
}

// TestSearchEntities_TimeoutMillis_JoinedTxRejected400 pins that timeoutMillis
// is rejected — not silently honored or silently ignored — on a request that
// joins an open transaction (spi.GetTransaction(ctx) != nil, how a routed
// compute-node callback presents at param-resolution time). Mirrors
// TestWriteOps_TransactionTimeoutMillis_JoinedRejected400 in
// internal/domain/entity/handler_reqtimeout_test.go, but the search param is
// named timeoutMillis (not transactionTimeoutMillis) and the response must
// name it accordingly.
func TestSearchEntities_TimeoutMillis_JoinedTxRejected400(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}
	saveMinimalModel(t, tenantCtx("tenant-1"), base, ref)

	searchStore, _ := base.AsyncSearchStore(context.Background())
	h := search.NewHandler(search.NewSearchService(base, common.NewTestUUIDGenerator(), searchStore))

	ctx := spi.WithTransaction(context.Background(), &spi.TransactionState{ID: "tx-1"})
	body := `{"type":"simple","jsonPath":"$.name","operatorType":"EQUALS","value":"Alice"}`
	req := httptest.NewRequest(http.MethodPost, "/search/direct/person/1?timeoutMillis=5000", strings.NewReader(body)).WithContext(ctx)
	rr := httptest.NewRecorder()

	millis := int64(5000)
	h.SearchEntities(rr, req, "person", 1, genapi.SearchEntitiesParams{TimeoutMillis: &millis})

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	var pd struct {
		Detail     string         `json:"detail"`
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &pd); err != nil {
		t.Fatalf("decode body: %v; body=%s", err, rr.Body.String())
	}
	if !strings.Contains(pd.Detail, "timeoutMillis") {
		t.Fatalf("body does not mention the param name: %q", pd.Detail)
	}
	if !strings.Contains(pd.Detail, "joins an open transaction") {
		t.Fatalf("body does not mention the joined transaction: %q", pd.Detail)
	}
	if pd.Properties["errorCode"] != common.ErrCodeBadRequest {
		t.Fatalf("errorCode = %v, want %v", pd.Properties["errorCode"], common.ErrCodeBadRequest)
	}
}

// TestSearchEntities_TimeoutExpires_Returns408BeforeAnyBody pins the D2/D8
// contract end to end through the HTTP handler: a Search call that blocks
// past the client-requested deadline surfaces as 408 SEARCH_TIMEOUT
// (application/problem+json), never a 200 with a truncated/empty ndjson
// stream — the header/status pair alone (never x-ndjson) is the decisive
// assertion that this fires BEFORE any streaming byte is written.
func TestSearchEntities_TimeoutExpires_Returns408BeforeAnyBody(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()
	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}
	saveMinimalModel(t, ctx, base, ref)

	realStore, _ := base.EntityStore(ctx)
	ses := &searcherEntityStore{
		EntityStore: realStore,
		searchFn: func(ctx context.Context, _ spi.Filter, _ spi.SearchOptions) ([]*spi.Entity, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	factory := &searcherFactory{StoreFactory: base, entityStore: ses}
	searchStore, _ := base.AsyncSearchStore(context.Background())
	h := search.NewHandler(search.NewSearchService(factory, common.NewTestUUIDGenerator(), searchStore))

	cond := `{"type":"lifecycle","field":"state","operatorType":"EQUALS","value":"CREATED"}`
	req := httptest.NewRequest(http.MethodPost, "/search/direct/person/1?timeoutMillis=1", strings.NewReader(cond)).WithContext(ctx)
	rr := httptest.NewRecorder()

	millis := int64(1)
	h.SearchEntities(rr, req, "person", 1, genapi.SearchEntitiesParams{TimeoutMillis: &millis})

	if rr.Code != http.StatusRequestTimeout {
		t.Fatalf("status = %d, want 408; body=%s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json (must not be application/x-ndjson)", ct)
	}
	var pd struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &pd); err != nil {
		t.Fatalf("decode body: %v; body=%s", err, rr.Body.String())
	}
	if pd.Properties["errorCode"] != common.ErrCodeSearchTimeout {
		t.Fatalf("errorCode = %v, want %v", pd.Properties["errorCode"], common.ErrCodeSearchTimeout)
	}
	if pd.Properties["retryable"] != true {
		t.Fatalf("retryable = %v, want true", pd.Properties["retryable"])
	}
	if ses.searchCalls != 1 {
		t.Fatalf("Search called %d times, want exactly 1", ses.searchCalls)
	}
}
