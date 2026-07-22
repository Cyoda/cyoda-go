package entity_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/entity"
)

// decodeProblemErrorCode pulls properties.errorCode out of the RFC 9457
// problem+json response that common.WriteError produces.
func decodeProblemErrorCode(t *testing.T, body []byte) string {
	t.Helper()
	var pd struct {
		Properties struct {
			ErrorCode string `json:"errorCode"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(body, &pd); err != nil {
		t.Fatalf("decode problem detail: %v\nbody: %s", err, body)
	}
	return pd.Properties.ErrorCode
}

// newHandlerWithoutResolver builds a handler with a nil resolver, used for
// the early-rejection tests (body-size, malformed JSON, validation).
func newHandlerWithoutResolver() *entity.GroupedStatsHandler {
	return entity.NewGroupedStatsHandler(nil, 10000)
}

func TestGroupedStatsHandler_Returns400OnMissingGroupBy(t *testing.T) {
	h := newHandlerWithoutResolver()
	body := bytes.NewBufferString(`{}`)
	req := httptest.NewRequest(http.MethodPost, "/api/entity/stats/X/1/query", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := decodeProblemErrorCode(t, rec.Body.Bytes()); got != "MISSING_GROUP_BY" {
		t.Fatalf("errorCode=%s, want MISSING_GROUP_BY (body: %s)", got, rec.Body.String())
	}
}

func TestGroupedStatsHandler_Returns413OnLargeBody(t *testing.T) {
	h := newHandlerWithoutResolver()
	body := bytes.NewBuffer(make([]byte, 11*1024*1024)) // 11 MiB > 10 MiB cap
	req := httptest.NewRequest(http.MethodPost, "/api/entity/stats/X/1/query", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d, want 413 (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestGroupedStatsHandler_RejectsMalformedJSON(t *testing.T) {
	h := newHandlerWithoutResolver()
	body := strings.NewReader(`{not json}`)
	req := httptest.NewRequest(http.MethodPost, "/api/entity/stats/X/1/query", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := decodeProblemErrorCode(t, rec.Body.Bytes()); got != "MALFORMED_REQUEST" {
		t.Fatalf("errorCode=%s, want MALFORMED_REQUEST (body: %s)", got, rec.Body.String())
	}
}

func TestGroupedStatsHandler_RejectsUnknownTopLevelField(t *testing.T) {
	// DisallowUnknownFields contract: garbage top-level field => 400 MALFORMED_REQUEST.
	h := newHandlerWithoutResolver()
	body := strings.NewReader(`{"groupBy":["state"],"nope":1}`)
	req := httptest.NewRequest(http.MethodPost, "/api/entity/stats/X/1/query", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := decodeProblemErrorCode(t, rec.Body.Bytes()); got != "MALFORMED_REQUEST" {
		t.Fatalf("errorCode=%s, want MALFORMED_REQUEST (body: %s)", got, rec.Body.String())
	}
}

func TestGroupedStatsHandler_Returns404OnUnknownModel(t *testing.T) {
	resolver := func(_ *http.Request, _, _ string) (any, spi.ModelRef, bool, error) {
		return nil, spi.ModelRef{}, false, nil
	}
	h := entity.NewGroupedStatsHandler(resolver, 10000)
	body := strings.NewReader(`{"groupBy":["state"]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/entity/stats/X/1/query", body)
	req.SetPathValue("entityName", "X")
	req.SetPathValue("modelVersion", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := decodeProblemErrorCode(t, rec.Body.Bytes()); got != "MODEL_NOT_FOUND" {
		t.Fatalf("errorCode=%s, want MODEL_NOT_FOUND (body: %s)", got, rec.Body.String())
	}
}

func TestGroupedStatsHandler_BackendNotSupportedReturns501(t *testing.T) {
	// "store" satisfies neither Iterable nor GroupedAggregator.
	type empty struct{}
	resolver := func(_ *http.Request, _, _ string) (any, spi.ModelRef, bool, error) {
		return empty{}, spi.ModelRef{EntityName: "X", ModelVersion: "1"}, true, nil
	}
	h := entity.NewGroupedStatsHandler(resolver, 10000)
	body := strings.NewReader(`{"groupBy":["state"]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/entity/stats/X/1/query", body)
	req.SetPathValue("entityName", "X")
	req.SetPathValue("modelVersion", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status %d, want 501 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := decodeProblemErrorCode(t, rec.Body.Bytes()); got != "NOT_IMPLEMENTED_BY_BACKEND" {
		t.Fatalf("errorCode=%s, want NOT_IMPLEMENTED_BY_BACKEND (body: %s)", got, rec.Body.String())
	}
}

func TestGroupedStatsHandler_GroupCardinalityExceededReturns422(t *testing.T) {
	// Stream three rows with a maxBuckets=1 ceiling — second distinct state trips the SPI sentinel.
	rows := []*spi.Entity{
		{Meta: spi.EntityMeta{State: "available"}, Data: []byte(`{}`)},
		{Meta: spi.EntityMeta{State: "allocated"}, Data: []byte(`{}`)},
	}
	store := &fakeIterable{entities: rows}
	resolver := func(_ *http.Request, _, _ string) (any, spi.ModelRef, bool, error) {
		return store, spi.ModelRef{EntityName: "X", ModelVersion: "1"}, true, nil
	}
	h := entity.NewGroupedStatsHandler(resolver, 1)
	body := strings.NewReader(`{"groupBy":["state"]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/entity/stats/X/1/query", body)
	req.SetPathValue("entityName", "X")
	req.SetPathValue("modelVersion", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := decodeProblemErrorCode(t, rec.Body.Bytes()); got != "GROUP_CARDINALITY_EXCEEDED" {
		t.Fatalf("errorCode=%s, want GROUP_CARDINALITY_EXCEEDED (body: %s)", got, rec.Body.String())
	}
}

func TestGroupedStatsHandler_InvalidConditionReturns400(t *testing.T) {
	rows := []*spi.Entity{
		{Meta: spi.EntityMeta{State: "available"}, Data: []byte(`{}`)},
	}
	store := &fakeIterable{entities: rows}
	resolver := func(_ *http.Request, _, _ string) (any, spi.ModelRef, bool, error) {
		return store, spi.ModelRef{EntityName: "X", ModelVersion: "1"}, true, nil
	}
	h := entity.NewGroupedStatsHandler(resolver, 10000)
	// Condition with bogus "type" — predicate.ParseCondition rejects it.
	body := strings.NewReader(`{"groupBy":["state"],"condition":{"type":"bogus"}}`)
	req := httptest.NewRequest(http.MethodPost, "/api/entity/stats/X/1/query", body)
	req.SetPathValue("entityName", "X")
	req.SetPathValue("modelVersion", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := decodeProblemErrorCode(t, rec.Body.Bytes()); got != "INVALID_CONDITION" {
		t.Fatalf("errorCode=%s, want INVALID_CONDITION (body: %s)", got, rec.Body.String())
	}
}

func TestGroupedStatsHandler_LifecycleTemporalTypeMismatchReturns400(t *testing.T) {
	rows := []*spi.Entity{
		{Meta: spi.EntityMeta{State: "available"}, Data: []byte(`{}`)},
	}
	store := &fakeIterable{entities: rows}
	resolver := func(_ *http.Request, _, _ string) (any, spi.ModelRef, bool, error) {
		return store, spi.ModelRef{EntityName: "X", ModelVersion: "1"}, true, nil
	}
	h := entity.NewGroupedStatsHandler(resolver, 10000)
	// CONTAINS is not a valid comparison operator against the temporal
	// creationDate meta field — parity with /search's CONDITION_TYPE_MISMATCH.
	body := strings.NewReader(`{"groupBy":["state"],"condition":{"type":"lifecycle","field":"creationDate","operatorType":"CONTAINS","value":"2021"}}`)
	req := httptest.NewRequest(http.MethodPost, "/api/entity/stats/X/1/query", body)
	req.SetPathValue("entityName", "X")
	req.SetPathValue("modelVersion", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := decodeProblemErrorCode(t, rec.Body.Bytes()); got != "CONDITION_TYPE_MISMATCH" {
		t.Fatalf("errorCode=%s, want CONDITION_TYPE_MISMATCH (body: %s)", got, rec.Body.String())
	}
}

func TestGroupedStatsHandler_UnknownMetaFieldReturns400(t *testing.T) {
	rows := []*spi.Entity{
		{Meta: spi.EntityMeta{State: "available"}, Data: []byte(`{}`)},
	}
	store := &fakeIterable{entities: rows}
	resolver := func(_ *http.Request, _, _ string) (any, spi.ModelRef, bool, error) {
		return store, spi.ModelRef{EntityName: "X", ModelVersion: "1"}, true, nil
	}
	h := entity.NewGroupedStatsHandler(resolver, 10000)
	// "bogus" is not a recognized meta filter field — parity with /search's
	// INVALID_FIELD_PATH.
	body := strings.NewReader(`{"groupBy":["state"],"condition":{"type":"lifecycle","field":"bogus","operatorType":"EQUALS","value":"x"}}`)
	req := httptest.NewRequest(http.MethodPost, "/api/entity/stats/X/1/query", body)
	req.SetPathValue("entityName", "X")
	req.SetPathValue("modelVersion", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := decodeProblemErrorCode(t, rec.Body.Bytes()); got != "INVALID_FIELD_PATH" {
		t.Fatalf("errorCode=%s, want INVALID_FIELD_PATH (body: %s)", got, rec.Body.String())
	}
}

func TestGroupedStatsHandler_MalformedBetweenArityReturns400(t *testing.T) {
	rows := []*spi.Entity{
		{Meta: spi.EntityMeta{State: "available"}, Data: []byte(`{"price":10}`)},
	}
	store := &fakeIterable{entities: rows}
	resolver := func(_ *http.Request, _, _ string) (any, spi.ModelRef, bool, error) {
		return store, spi.ModelRef{EntityName: "X", ModelVersion: "1"}, true, nil
	}
	h := entity.NewGroupedStatsHandler(resolver, 10000)
	body := strings.NewReader(`{"groupBy":["state"],"condition":{"type":"simple","jsonPath":"$.price","operatorType":"BETWEEN","value":[10]}}`)
	req := httptest.NewRequest(http.MethodPost, "/api/entity/stats/X/1/query", body)
	req.SetPathValue("entityName", "X")
	req.SetPathValue("modelVersion", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := decodeProblemErrorCode(t, rec.Body.Bytes()); got != "INVALID_CONDITION" {
		t.Fatalf("errorCode=%s, want INVALID_CONDITION (body: %s)", got, rec.Body.String())
	}
}

func TestGroupedStatsHandler_HappyPathReturns200(t *testing.T) {
	rows := []*spi.Entity{
		{Meta: spi.EntityMeta{State: "available"}, Data: []byte(`{}`)},
		{Meta: spi.EntityMeta{State: "available"}, Data: []byte(`{}`)},
		{Meta: spi.EntityMeta{State: "allocated"}, Data: []byte(`{}`)},
	}
	store := &fakeIterable{entities: rows}
	resolver := func(_ *http.Request, _, _ string) (any, spi.ModelRef, bool, error) {
		return store, spi.ModelRef{EntityName: "X", ModelVersion: "1"}, true, nil
	}
	h := entity.NewGroupedStatsHandler(resolver, 10000)
	body := strings.NewReader(`{"groupBy":["state"]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/entity/stats/X/1/query", body)
	req.SetPathValue("entityName", "X")
	req.SetPathValue("modelVersion", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type %q, want application/json", ct)
	}
	var buckets []entity.GroupedStatsBucket
	if err := json.Unmarshal(rec.Body.Bytes(), &buckets); err != nil {
		t.Fatalf("decode buckets: %v\nbody: %s", err, rec.Body.String())
	}
	if len(buckets) != 2 {
		t.Fatalf("buckets len=%d, want 2 (body: %s)", len(buckets), rec.Body.String())
	}
	// D12 total order: count desc.
	if buckets[0].Count < buckets[1].Count {
		t.Fatalf("count not sorted desc: %v", buckets)
	}
}

func TestGroupedStatsHandler_ResolverError_Returns500(t *testing.T) {
	resolver := func(_ *http.Request, _, _ string) (any, spi.ModelRef, bool, error) {
		return nil, spi.ModelRef{}, false, errors.New("boom")
	}
	h := entity.NewGroupedStatsHandler(resolver, 10000)
	body := strings.NewReader(`{"groupBy":["state"]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/entity/stats/X/1/query", body)
	req.SetPathValue("entityName", "X")
	req.SetPathValue("modelVersion", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500 (body: %s)", rec.Code, rec.Body.String())
	}
}

// Compile-time sanity: confirm the StoreResolver signature is what the
// router-wiring site expects.
var _ entity.StoreResolver = func(_ *http.Request, _, _ string) (any, spi.ModelRef, bool, error) {
	return nil, spi.ModelRef{}, false, nil
}
