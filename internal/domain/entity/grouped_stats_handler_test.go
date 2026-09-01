package entity_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/entity"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
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
	resolver := func(_ *http.Request, _, _ string) (any, spi.ModelRef, map[string]schema.FieldDescriptor, spi.ModelStore, bool, error) {
		return nil, spi.ModelRef{}, nil, nil, false, nil
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
	resolver := func(_ *http.Request, _, _ string) (any, spi.ModelRef, map[string]schema.FieldDescriptor, spi.ModelStore, bool, error) {
		return empty{}, spi.ModelRef{EntityName: "X", ModelVersion: "1"}, nil, nil, true, nil
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
	resolver := func(_ *http.Request, _, _ string) (any, spi.ModelRef, map[string]schema.FieldDescriptor, spi.ModelStore, bool, error) {
		return store, spi.ModelRef{EntityName: "X", ModelVersion: "1"}, nil, nil, true, nil
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
	resolver := func(_ *http.Request, _, _ string) (any, spi.ModelRef, map[string]schema.FieldDescriptor, spi.ModelStore, bool, error) {
		return store, spi.ModelRef{EntityName: "X", ModelVersion: "1"}, nil, nil, true, nil
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
	resolver := func(_ *http.Request, _, _ string) (any, spi.ModelRef, map[string]schema.FieldDescriptor, spi.ModelStore, bool, error) {
		return store, spi.ModelRef{EntityName: "X", ModelVersion: "1"}, nil, nil, true, nil
	}
	h := entity.NewGroupedStatsHandler(resolver, 10000)
	// Parse-based (spec §6): a comparison operand that parses into no temporal
	// type against the temporal creationDate meta field is a
	// CONDITION_TYPE_MISMATCH — parity with /search.
	body := strings.NewReader(`{"groupBy":["state"],"condition":{"type":"lifecycle","field":"creationDate","operatorType":"GREATER_THAN","value":"not-a-date"}}`)
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
	resolver := func(_ *http.Request, _, _ string) (any, spi.ModelRef, map[string]schema.FieldDescriptor, spi.ModelStore, bool, error) {
		return store, spi.ModelRef{EntityName: "X", ModelVersion: "1"}, nil, nil, true, nil
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
	resolver := func(_ *http.Request, _, _ string) (any, spi.ModelRef, map[string]schema.FieldDescriptor, spi.ModelStore, bool, error) {
		return store, spi.ModelRef{EntityName: "X", ModelVersion: "1"}, nil, nil, true, nil
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
	resolver := func(_ *http.Request, _, _ string) (any, spi.ModelRef, map[string]schema.FieldDescriptor, spi.ModelStore, bool, error) {
		return store, spi.ModelRef{EntityName: "X", ModelVersion: "1"}, nil, nil, true, nil
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
	resolver := func(_ *http.Request, _, _ string) (any, spi.ModelRef, map[string]schema.FieldDescriptor, spi.ModelStore, bool, error) {
		return nil, spi.ModelRef{}, nil, nil, false, errors.New("boom")
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
var _ entity.StoreResolver = func(_ *http.Request, _, _ string) (any, spi.ModelRef, map[string]schema.FieldDescriptor, spi.ModelStore, bool, error) {
	return nil, spi.ModelRef{}, nil, nil, false, nil
}

// --- Schema-membership validation (grouped stats) ---
//
// /search/direct and conditional DELETE both reject a condition naming a field
// the model does not declare, with 400 INVALID_FIELD_PATH. Grouped stats did
// not check at all — not the condition, not the groupBy paths, not the
// aggregate fields — so such a request was answered from a filter whose
// comparison leaves annihilate while its string leaves keep matching. The
// numbers came back looking like a real answer.

// declaredFieldsModelStore serves a descriptor declaring the given String
// leaves. It implements no RefreshAndGet: the cached miss is authoritative,
// which is the same shape validateConditionPaths treats as final.
type declaredFieldsModelStore struct {
	ref    spi.ModelRef
	fields []string
}

func (s *declaredFieldsModelStore) Get(context.Context, spi.ModelRef) (*spi.ModelDescriptor, error) {
	node := schema.NewObjectNode()
	for _, f := range s.fields {
		dt := schema.String
		if f == "age" {
			dt = schema.Integer
		}
		node.SetChild(f, schema.NewLeafNode(dt))
	}
	raw, err := schema.Marshal(node)
	if err != nil {
		return nil, err
	}
	return &spi.ModelDescriptor{Ref: s.ref, State: spi.ModelLocked, Schema: raw}, nil
}

func (s *declaredFieldsModelStore) Save(context.Context, *spi.ModelDescriptor) error { return nil }
func (s *declaredFieldsModelStore) GetAll(context.Context) ([]spi.ModelRef, error)   { return nil, nil }
func (s *declaredFieldsModelStore) Delete(context.Context, spi.ModelRef) error       { return nil }
func (s *declaredFieldsModelStore) Lock(context.Context, spi.ModelRef) error         { return nil }
func (s *declaredFieldsModelStore) Unlock(context.Context, spi.ModelRef) error       { return nil }
func (s *declaredFieldsModelStore) IsLocked(context.Context, spi.ModelRef) (bool, error) {
	return true, nil
}
func (s *declaredFieldsModelStore) SetChangeLevel(context.Context, spi.ModelRef, spi.ChangeLevel) error {
	return nil
}
func (s *declaredFieldsModelStore) ExtendSchema(context.Context, spi.ModelRef, spi.SchemaDelta) error {
	return nil
}

var _ spi.ModelStore = (*declaredFieldsModelStore)(nil)

// groupedStatsPathReq drives one request against a model declaring "name",
// returning the recorder.
func groupedStatsPathReq(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	ref := spi.ModelRef{EntityName: "X", ModelVersion: "1"}
	ms := &declaredFieldsModelStore{ref: ref, fields: []string{"name", "age"}}
	fields := map[string]schema.FieldDescriptor{
		"$.name": {Path: "$.name", Types: []spi.DataType{spi.String}},
		"$.age":  {Path: "$.age", Types: []spi.DataType{spi.Integer}},
	}
	resolver := func(_ *http.Request, _, _ string) (any, spi.ModelRef, map[string]schema.FieldDescriptor, spi.ModelStore, bool, error) {
		return &fakeIterable{}, ref, fields, ms, true, nil
	}
	h := entity.NewGroupedStatsHandler(resolver, 10000)
	req := httptest.NewRequest(http.MethodPost, "/api/entity/stats/X/1/query", strings.NewReader(body))
	req.SetPathValue("entityName", "X")
	req.SetPathValue("modelVersion", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestGroupedStatsHandler_UnknownConditionPath_Rejected(t *testing.T) {
	rec := groupedStatsPathReq(t, `{"groupBy":["state"],"condition":{"type":"simple","jsonPath":"$.nope","operatorType":"EQUALS","value":"x"}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 — /search/direct rejects this condition (body: %s)", rec.Code, rec.Body.String())
	}
	if got := decodeProblemErrorCode(t, rec.Body.Bytes()); got != "INVALID_FIELD_PATH" {
		t.Fatalf("errorCode=%s, want INVALID_FIELD_PATH", got)
	}
}

func TestGroupedStatsHandler_UnknownGroupByPath_Rejected(t *testing.T) {
	rec := groupedStatsPathReq(t, `{"groupBy":["$.nope"]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: grouping by a field the model does not declare "+
			"buckets every entity under \"absent\" and reports it as a result (body: %s)",
			rec.Code, rec.Body.String())
	}
	if got := decodeProblemErrorCode(t, rec.Body.Bytes()); got != "INVALID_FIELD_PATH" {
		t.Fatalf("errorCode=%s, want INVALID_FIELD_PATH", got)
	}
}

func TestGroupedStatsHandler_UnknownAggregateField_Rejected(t *testing.T) {
	rec := groupedStatsPathReq(t, `{"groupBy":["state"],"aggregations":[{"op":"sum","field":"$.nope","as":"s"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: summing a field the model does not declare "+
			"reports 0 as though it were the total (body: %s)", rec.Code, rec.Body.String())
	}
	if got := decodeProblemErrorCode(t, rec.Body.Bytes()); got != "INVALID_FIELD_PATH" {
		t.Fatalf("errorCode=%s, want INVALID_FIELD_PATH", got)
	}
}

// The accept side: a declared path on every surface still runs.
func TestGroupedStatsHandler_DeclaredPaths_StillAccepted(t *testing.T) {
	rec := groupedStatsPathReq(t, `{"groupBy":["$.name"],"condition":{"type":"simple","jsonPath":"$.name","operatorType":"EQUALS","value":"x"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 for paths the model declares (body: %s)", rec.Code, rec.Body.String())
	}
}

// TestGroupedStatsHandler_ConditionTypeMismatch_Rejected closes the other half
// of grouped stats' schema blindness. The service deliberately passed a nil
// model to ValidateConditionValueTypes — commented as "grouped-stats has no
// model-schema plumbing" — so the schema-DEPENDENT arm of that check was
// skipped and an operand that parses into none of a declared field's types was
// accepted. /search/direct answers 400 CONDITION_TYPE_MISMATCH for the same
// condition. The plumbing exists now.
func TestGroupedStatsHandler_ConditionTypeMismatch_Rejected(t *testing.T) {
	rec := groupedStatsPathReq(t, `{"groupBy":["state"],"condition":{"type":"simple","jsonPath":"$.age","operatorType":"GREATER_THAN","value":"not-a-number"}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := decodeProblemErrorCode(t, rec.Body.Bytes()); got != "CONDITION_TYPE_MISMATCH" {
		t.Fatalf("errorCode=%s, want CONDITION_TYPE_MISMATCH", got)
	}
}

// refreshFailingModelStore serves a warm-but-stale cached descriptor (Get
// always succeeds, declaring only "name") and always FAILS its
// RefreshAndGet — modelling a genuine model-store outage discovered while
// confirming a possibly-stale field-path miss.
type refreshFailingModelStore struct {
	ref spi.ModelRef
}

func (s *refreshFailingModelStore) Get(context.Context, spi.ModelRef) (*spi.ModelDescriptor, error) {
	node := schema.NewObjectNode()
	node.SetChild("name", schema.NewLeafNode(schema.String))
	raw, err := schema.Marshal(node)
	if err != nil {
		return nil, err
	}
	return &spi.ModelDescriptor{Ref: s.ref, State: spi.ModelLocked, Schema: raw}, nil
}

func (s *refreshFailingModelStore) RefreshAndGet(context.Context, spi.ModelRef) (*spi.ModelDescriptor, error) {
	return nil, errors.New("connection refused")
}

func (s *refreshFailingModelStore) Save(context.Context, *spi.ModelDescriptor) error { return nil }
func (s *refreshFailingModelStore) GetAll(context.Context) ([]spi.ModelRef, error)   { return nil, nil }
func (s *refreshFailingModelStore) Delete(context.Context, spi.ModelRef) error       { return nil }
func (s *refreshFailingModelStore) Lock(context.Context, spi.ModelRef) error         { return nil }
func (s *refreshFailingModelStore) Unlock(context.Context, spi.ModelRef) error       { return nil }
func (s *refreshFailingModelStore) IsLocked(context.Context, spi.ModelRef) (bool, error) {
	return true, nil
}
func (s *refreshFailingModelStore) SetChangeLevel(context.Context, spi.ModelRef, spi.ChangeLevel) error {
	return nil
}
func (s *refreshFailingModelStore) ExtendSchema(context.Context, spi.ModelRef, spi.SchemaDelta) error {
	return nil
}

var _ spi.ModelStore = (*refreshFailingModelStore)(nil)

// TestGroupedStatsHandler_SchemaRefreshFailure_Returns5xxNot400 pins the
// review-round finding that grouped stats shares the same 400→5xx flip as
// /search/direct and conditional DELETE: a condition naming a field absent
// from the cached schema, whose confirming RefreshAndGet call itself FAILS,
// must be reported as infrastructure — not folded into the same 400
// INVALID_FIELD_PATH TestGroupedStatsHandler_UnknownConditionPath_Rejected
// pins for a genuinely-unknown path.
func TestGroupedStatsHandler_SchemaRefreshFailure_Returns5xxNot400(t *testing.T) {
	ref := spi.ModelRef{EntityName: "X", ModelVersion: "1"}
	ms := &refreshFailingModelStore{ref: ref}
	fields := map[string]schema.FieldDescriptor{
		"$.name": {Path: "$.name", Types: []spi.DataType{spi.String}},
	}
	resolver := func(_ *http.Request, _, _ string) (any, spi.ModelRef, map[string]schema.FieldDescriptor, spi.ModelStore, bool, error) {
		return &fakeIterable{}, ref, fields, ms, true, nil
	}
	h := entity.NewGroupedStatsHandler(resolver, 10000)
	body := `{"groupBy":["state"],"condition":{"type":"simple","jsonPath":"$.zz","operatorType":"EQUALS","value":"x"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/entity/stats/X/1/query", strings.NewReader(body))
	req.SetPathValue("entityName", "X")
	req.SetPathValue("modelVersion", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code < http.StatusInternalServerError {
		t.Fatalf("status %d, want 5xx: a failed schema refresh is infrastructure, not a client fault (body: %s)",
			rec.Code, rec.Body.String())
	}
	if got := decodeProblemErrorCode(t, rec.Body.Bytes()); got == "INVALID_FIELD_PATH" {
		t.Errorf("errorCode=%s, must NOT be INVALID_FIELD_PATH — that is the genuine-unknown-path classification", got)
	}
}
