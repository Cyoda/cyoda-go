package entity_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/app"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
	"github.com/cyoda-platform/cyoda-go/internal/domain/entity"
	_ "github.com/cyoda-platform/cyoda-go/plugins/memory"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	cfg := app.DefaultConfig()
	cfg.ContextPath = ""
	return newTestServerWithConfig(t, cfg)
}

func newTestServerWithConfig(t *testing.T, cfg app.Config) *httptest.Server {
	t.Helper()
	a := app.New(cfg)
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func importAndLockModel(t *testing.T, base, entityName string, version int, sampleData string) {
	t.Helper()
	url := base + "/model/import/JSON/SAMPLE_DATA/" + entityName + "/" + strconv.Itoa(version)
	resp, err := http.Post(url, "application/json", strings.NewReader(sampleData))
	if err != nil {
		t.Fatalf("import request failed: %v", err)
	}
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	lockURL := base + "/model/" + entityName + "/" + strconv.Itoa(version) + "/lock"
	req, _ := http.NewRequest(http.MethodPut, lockURL, nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("lock request failed: %v", err)
	}
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()
}

func doCreateEntity(t *testing.T, base, format, entityName string, version int, body string) *http.Response {
	t.Helper()
	url := base + "/entity/" + format + "/" + entityName + "/" + strconv.Itoa(version)
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("create entity request failed: %v", err)
	}
	return resp
}

func doGetEntity(t *testing.T, base, entityID string) *http.Response {
	t.Helper()
	url := base + "/entity/" + entityID
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get entity request failed: %v", err)
	}
	return resp
}

func doGetEntityWithPointInTime(t *testing.T, base, entityID string, pit time.Time) *http.Response {
	t.Helper()
	url := fmt.Sprintf("%s/entity/%s?pointInTime=%s", base, entityID, pit.UTC().Format(time.RFC3339Nano))
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get entity with pointInTime request failed: %v", err)
	}
	return resp
}

func doGetEntityWithTransactionID(t *testing.T, base, entityID, txID string) *http.Response {
	t.Helper()
	url := fmt.Sprintf("%s/entity/%s?transactionId=%s", base, entityID, txID)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get entity with transactionId request failed: %v", err)
	}
	return resp
}

func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read response body: %v", err)
	}
	return b
}

func expectStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected status %d, got %d; body: %s", want, resp.StatusCode, string(body))
	}
}

func TestCreateEntity_JSONArrayCreatesBatch(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "BatchTest", 1, `{"name":"test"}`)

	resp := doCreateEntity(t, srv.URL, "JSON", "BatchTest", 1, `[{"name":"a"},{"name":"b"},{"name":"c"}]`)
	expectStatus(t, resp, http.StatusOK)

	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var results []map[string]any
	if err := json.Unmarshal(body, &results); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result envelope, got %d", len(results))
	}
	entityIDs, ok := results[0]["entityIds"].([]any)
	if !ok {
		t.Fatalf("expected entityIds array, got %T", results[0]["entityIds"])
	}
	if len(entityIDs) != 3 {
		t.Fatalf("expected 3 entity IDs, got %d", len(entityIDs))
	}
}

func TestNewHandler(t *testing.T) {
	h := entity.New(nil, nil, common.NewDefaultUUIDGenerator(), nil)
	if h == nil {
		t.Fatal("expected non-nil handler")
	}
}

func TestCreateAndGetEntity(t *testing.T) {
	srv := newTestServer(t)

	importAndLockModel(t, srv.URL, "Person", 1, `{"name":"Alice","age":30}`)

	// Create entity
	resp := doCreateEntity(t, srv.URL, "JSON", "Person", 1, `{"name":"Bob","age":25}`)
	expectStatus(t, resp, http.StatusOK)

	body := readBody(t, resp)
	var createArr []map[string]any
	if err := json.Unmarshal(body, &createArr); err != nil {
		t.Fatalf("failed to parse create response: %v", err)
	}
	if len(createArr) != 1 {
		t.Fatalf("expected array of length 1, got %d", len(createArr))
	}
	createResp := createArr[0]

	// Verify transactionId present
	txID, ok := createResp["transactionId"].(string)
	if !ok || txID == "" {
		t.Fatalf("expected non-empty transactionId, got %v", createResp["transactionId"])
	}

	// Verify entityIds present
	entityIDs, ok := createResp["entityIds"].([]any)
	if !ok || len(entityIDs) == 0 {
		t.Fatalf("expected non-empty entityIds, got %v", createResp["entityIds"])
	}

	entityID, ok := entityIDs[0].(string)
	if !ok || entityID == "" {
		t.Fatalf("expected non-empty entity id, got %v", entityIDs[0])
	}

	// Get entity back
	resp = doGetEntity(t, srv.URL, entityID)
	expectStatus(t, resp, http.StatusOK)

	body = readBody(t, resp)
	var envelope map[string]any
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("failed to parse entity envelope: %v", err)
	}

	// Verify envelope shape
	if envelope["type"] != "ENTITY" {
		t.Errorf("expected type=ENTITY, got %v", envelope["type"])
	}

	data, ok := envelope["data"].(map[string]any)
	if !ok {
		t.Fatal("expected data to be an object")
	}
	if data["name"] != "Bob" {
		t.Errorf("expected data.name=Bob, got %v", data["name"])
	}

	meta, ok := envelope["meta"].(map[string]any)
	if !ok {
		t.Fatal("expected meta to be an object")
	}
	if meta["state"] != "CREATED" {
		t.Errorf("expected meta.state=CREATED, got %v", meta["state"])
	}
	if meta["id"] != entityID {
		t.Errorf("expected meta.id=%s, got %v", entityID, meta["id"])
	}
	if meta["transactionId"] == nil || meta["transactionId"] == "" {
		t.Error("expected non-empty transactionId in meta")
	}

	// Check modelKey in meta
	mk, ok := meta["modelKey"].(map[string]any)
	if !ok {
		t.Fatal("expected modelKey in meta")
	}
	if mk["name"] != "Person" {
		t.Errorf("expected modelKey.name=Person, got %v", mk["name"])
	}
	if mk["version"] != float64(1) {
		t.Errorf("expected modelKey.version=1, got %v", mk["version"])
	}
}

func TestCreateEntityUnlockedModel(t *testing.T) {
	srv := newTestServer(t)

	// Import model but do NOT lock
	url := srv.URL + "/model/import/JSON/SAMPLE_DATA/UnlockedTest/1"
	resp, err := http.Post(url, "application/json", strings.NewReader(`{"name":"Alice"}`))
	if err != nil {
		t.Fatalf("import request failed: %v", err)
	}
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	// Create entity against unlocked model → 409
	resp = doCreateEntity(t, srv.URL, "JSON", "UnlockedTest", 1, `{"name":"Bob"}`)
	expectStatus(t, resp, http.StatusConflict)
	resp.Body.Close()
}

func TestCreateEntityModelNotFound(t *testing.T) {
	srv := newTestServer(t)

	resp := doCreateEntity(t, srv.URL, "JSON", "NonExistent", 1, `{"foo":"bar"}`)
	expectStatus(t, resp, http.StatusNotFound)
	resp.Body.Close()
}

func TestCreateEntityNonConforming(t *testing.T) {
	srv := newTestServer(t)

	importAndLockModel(t, srv.URL, "Strict", 1, `{"name":"Alice"}`)

	// name should be string, sending number → 400
	resp := doCreateEntity(t, srv.URL, "JSON", "Strict", 1, `{"name": 123}`)
	expectStatus(t, resp, http.StatusBadRequest)
	resp.Body.Close()
}

func TestCreateEntityXML(t *testing.T) {
	srv := newTestServer(t)

	importAndLockModel(t, srv.URL, "XMLTest", 1, `{"name":"Alice","age":30}`)

	// Create entity with XML format
	xmlBody := `<root><name>Bob</name><age>25</age></root>`
	resp := doCreateEntity(t, srv.URL, "XML", "XMLTest", 1, xmlBody)
	expectStatus(t, resp, http.StatusOK)

	body := readBody(t, resp)
	var createArr []map[string]any
	if err := json.Unmarshal(body, &createArr); err != nil {
		t.Fatalf("failed to parse create response: %v", err)
	}
	createResp := createArr[0]

	entityIDs, ok := createResp["entityIds"].([]any)
	if !ok || len(entityIDs) == 0 {
		t.Fatalf("expected non-empty entityIds, got %v", createResp["entityIds"])
	}

	entityID := entityIDs[0].(string)

	// Get entity back and verify data
	resp = doGetEntity(t, srv.URL, entityID)
	expectStatus(t, resp, http.StatusOK)

	body = readBody(t, resp)
	var envelope map[string]any
	json.Unmarshal(body, &envelope)
	data, ok := envelope["data"].(map[string]any)
	if !ok {
		t.Fatal("expected data to be an object")
	}
	if data["name"] != "Bob" {
		t.Errorf("expected data.name=Bob, got %v", data["name"])
	}
}

func TestGetEntityNotFound(t *testing.T) {
	srv := newTestServer(t)

	resp := doGetEntity(t, srv.URL, "00000000-0000-0000-0000-000000000099")
	expectStatus(t, resp, http.StatusNotFound)
	resp.Body.Close()
}

func TestGetEntityPointInTime(t *testing.T) {
	srv := newTestServer(t)

	importAndLockModel(t, srv.URL, "PITTest", 1, `{"name":"Alice","age":30}`)

	// Create entity
	resp := doCreateEntity(t, srv.URL, "JSON", "PITTest", 1, `{"name":"Original","age":1}`)
	expectStatus(t, resp, http.StatusOK)
	body := readBody(t, resp)
	var createArr []map[string]any
	json.Unmarshal(body, &createArr)
	createResp := createArr[0]

	entityIDs := createResp["entityIds"].([]any)
	entityID := entityIDs[0].(string)

	// Wait a moment so the timestamps differ
	time.Sleep(50 * time.Millisecond)
	pointInTime := time.Now()
	time.Sleep(50 * time.Millisecond)

	// Update entity (create a new version by saving via update endpoint — for now use direct store)
	// Since UpdateSingle is not implemented yet, we'll just verify get with pointInTime works
	// by getting the entity at the pointInTime (which should return the original)
	resp = doGetEntityWithPointInTime(t, srv.URL, entityID, pointInTime)
	expectStatus(t, resp, http.StatusOK)

	body = readBody(t, resp)
	var envelope map[string]any
	json.Unmarshal(body, &envelope)
	data := envelope["data"].(map[string]any)
	if data["name"] != "Original" {
		t.Errorf("expected data.name=Original at pointInTime, got %v", data["name"])
	}
}

// --- Helper functions for GetAll, Update, Delete ---

func doGetAllEntities(t *testing.T, base, entityName string, version int, pageSize, pageNumber int) *http.Response {
	t.Helper()
	url := fmt.Sprintf("%s/entity/%s/%d?pageSize=%d&pageNumber=%d", base, entityName, version, pageSize, pageNumber)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get all entities request failed: %v", err)
	}
	return resp
}

func doUpdateEntity(t *testing.T, base, format string, entityId, transition, body string) *http.Response {
	t.Helper()
	url := fmt.Sprintf("%s/entity/%s/%s/%s", base, format, entityId, transition)
	req, err := http.NewRequest(http.MethodPut, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("failed to create update request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("update entity request failed: %v", err)
	}
	return resp
}

func doDeleteEntity(t *testing.T, base, entityId string) *http.Response {
	t.Helper()
	url := fmt.Sprintf("%s/entity/%s", base, entityId)
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		t.Fatalf("failed to create delete request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete entity request failed: %v", err)
	}
	return resp
}

func createEntityAndGetID(t *testing.T, base, entityName string, version int, body string) string {
	t.Helper()
	resp := doCreateEntity(t, base, "JSON", entityName, version, body)
	expectStatus(t, resp, http.StatusOK)
	b := readBody(t, resp)
	var arr []map[string]any
	json.Unmarshal(b, &arr)
	cr := arr[0]
	ids := cr["entityIds"].([]any)
	return ids[0].(string)
}

// --- GetAllEntities tests ---

func TestGetAllEntities(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "GetAllTest", 1, `{"name":"Alice","age":30}`)

	// Create 3 entities
	for i := 0; i < 3; i++ {
		body := fmt.Sprintf(`{"name":"Person%d","age":%d}`, i, 20+i)
		createEntityAndGetID(t, srv.URL, "GetAllTest", 1, body)
	}

	// GET all entities
	resp := doGetAllEntities(t, srv.URL, "GetAllTest", 1, 20, 0)
	expectStatus(t, resp, http.StatusOK)

	body := readBody(t, resp)
	var result []map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	if len(result) != 3 {
		t.Fatalf("expected 3 entities, got %d", len(result))
	}

	// Verify envelope shape: type=ENTITY, data present, meta present without modelKey
	for i, ent := range result {
		if ent["type"] != "ENTITY" {
			t.Errorf("entity %d: expected type=ENTITY, got %v", i, ent["type"])
		}
		if ent["data"] == nil {
			t.Errorf("entity %d: expected data to be present", i)
		}
		meta, ok := ent["meta"].(map[string]any)
		if !ok {
			t.Errorf("entity %d: expected meta to be an object", i)
			continue
		}
		if meta["id"] == nil || meta["id"] == "" {
			t.Errorf("entity %d: expected non-empty id in meta", i)
		}
		if meta["modelKey"] != nil {
			t.Errorf("entity %d: expected no modelKey in meta for GetAll, got %v", i, meta["modelKey"])
		}
	}
}

func TestGetAllEntitiesPagination(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "PageTest", 1, `{"name":"Alice","age":30}`)

	// Create 5 entities
	for i := 0; i < 5; i++ {
		body := fmt.Sprintf(`{"name":"Person%d","age":%d}`, i, 20+i)
		createEntityAndGetID(t, srv.URL, "PageTest", 1, body)
	}

	// Page 0, size 2 → 2 results
	resp := doGetAllEntities(t, srv.URL, "PageTest", 1, 2, 0)
	expectStatus(t, resp, http.StatusOK)
	body := readBody(t, resp)
	var page0 []map[string]any
	json.Unmarshal(body, &page0)
	if len(page0) != 2 {
		t.Fatalf("page 0: expected 2 entities, got %d", len(page0))
	}

	// Page 1, size 2 → 2 results
	resp = doGetAllEntities(t, srv.URL, "PageTest", 1, 2, 1)
	expectStatus(t, resp, http.StatusOK)
	body = readBody(t, resp)
	var page1 []map[string]any
	json.Unmarshal(body, &page1)
	if len(page1) != 2 {
		t.Fatalf("page 1: expected 2 entities, got %d", len(page1))
	}

	// Page 2, size 2 → 1 result
	resp = doGetAllEntities(t, srv.URL, "PageTest", 1, 2, 2)
	expectStatus(t, resp, http.StatusOK)
	body = readBody(t, resp)
	var page2 []map[string]any
	json.Unmarshal(body, &page2)
	if len(page2) != 1 {
		t.Fatalf("page 2: expected 1 entity, got %d", len(page2))
	}
}

func TestGetAllEntitiesEmpty(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "EmptyTest", 1, `{"name":"Alice"}`)

	resp := doGetAllEntities(t, srv.URL, "EmptyTest", 1, 20, 0)
	expectStatus(t, resp, http.StatusOK)

	body := readBody(t, resp)
	// Should be empty JSON array, not null
	if strings.TrimSpace(string(body)) != "[]" {
		t.Fatalf("expected empty array [], got %s", string(body))
	}
}

// --- UpdateSingle tests ---

func TestUpdateSingleEntity(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "UpdateTest", 1, `{"name":"Alice","age":30}`)

	// Create entity
	entityID := createEntityAndGetID(t, srv.URL, "UpdateTest", 1, `{"name":"Original","age":1}`)

	// Wait so timestamps differ
	time.Sleep(50 * time.Millisecond)
	beforeUpdate := time.Now()
	time.Sleep(50 * time.Millisecond)

	// Update entity (uses default workflow UPDATE transition)
	resp := doUpdateEntity(t, srv.URL, "JSON", entityID, "UPDATE", `{"name":"Updated","age":99}`)
	expectStatus(t, resp, http.StatusOK)

	updateBody := readBody(t, resp)
	var updateResp map[string]any
	if err := json.Unmarshal(updateBody, &updateResp); err != nil {
		t.Fatalf("failed to parse update response: %v", err)
	}
	if updateResp["transactionId"] == nil || updateResp["transactionId"] == "" {
		t.Fatal("expected non-empty transactionId in update response")
	}
	entityIDs, ok := updateResp["entityIds"].([]any)
	if !ok || len(entityIDs) == 0 {
		t.Fatal("expected entityIds in update response")
	}

	// GET current version → should have updated data
	resp = doGetEntity(t, srv.URL, entityID)
	expectStatus(t, resp, http.StatusOK)
	body := readBody(t, resp)
	var envelope map[string]any
	json.Unmarshal(body, &envelope)
	data := envelope["data"].(map[string]any)
	if data["name"] != "Updated" {
		t.Errorf("expected data.name=Updated, got %v", data["name"])
	}
	if data["age"] != float64(99) {
		t.Errorf("expected data.age=99, got %v", data["age"])
	}

	meta := envelope["meta"].(map[string]any)
	if meta["transitionForLatestSave"] != "UPDATE" {
		t.Errorf("expected transitionForLatestSave=UPDATE, got %v", meta["transitionForLatestSave"])
	}

	// GET with pointInTime before update → should return original data
	resp = doGetEntityWithPointInTime(t, srv.URL, entityID, beforeUpdate)
	expectStatus(t, resp, http.StatusOK)
	body = readBody(t, resp)
	json.Unmarshal(body, &envelope)
	data = envelope["data"].(map[string]any)
	if data["name"] != "Original" {
		t.Errorf("expected data.name=Original at pointInTime, got %v", data["name"])
	}
}

// --- DeleteSingleEntity tests ---

func TestDeleteSingleEntity(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "DeleteTest", 1, `{"name":"Alice","age":30}`)

	// Create entity
	entityID := createEntityAndGetID(t, srv.URL, "DeleteTest", 1, `{"name":"ToDelete","age":1}`)

	// Wait so timestamps differ
	time.Sleep(50 * time.Millisecond)
	beforeDelete := time.Now()
	time.Sleep(50 * time.Millisecond)

	// Delete entity
	resp := doDeleteEntity(t, srv.URL, entityID)
	expectStatus(t, resp, http.StatusOK)

	deleteBody := readBody(t, resp)
	var deleteResp map[string]any
	json.Unmarshal(deleteBody, &deleteResp)
	if deleteResp["id"] == nil || deleteResp["id"] == "" {
		t.Fatal("expected non-empty id in delete response")
	}
	mk, ok := deleteResp["modelKey"].(map[string]any)
	if !ok {
		t.Fatal("expected modelKey object in delete response")
	}
	if mk["name"] != "DeleteTest" {
		t.Errorf("expected modelKey.name=DeleteTest, got %v", mk["name"])
	}

	// GET after delete → 404
	resp = doGetEntity(t, srv.URL, entityID)
	expectStatus(t, resp, http.StatusNotFound)
	resp.Body.Close()

	// GET with pointInTime before delete → should return entity
	resp = doGetEntityWithPointInTime(t, srv.URL, entityID, beforeDelete)
	expectStatus(t, resp, http.StatusOK)
	body := readBody(t, resp)
	var envelope map[string]any
	json.Unmarshal(body, &envelope)
	data := envelope["data"].(map[string]any)
	if data["name"] != "ToDelete" {
		t.Errorf("expected data.name=ToDelete at pointInTime, got %v", data["name"])
	}
}

func TestDeleteEntityNotFound(t *testing.T) {
	srv := newTestServer(t)

	resp := doDeleteEntity(t, srv.URL, "00000000-0000-0000-0000-000000000099")
	expectStatus(t, resp, http.StatusNotFound)
	resp.Body.Close()
}

// --- Batch operations helpers ---

func doCreateCollection(t *testing.T, base, format, body string) *http.Response {
	t.Helper()
	url := base + "/entity/" + format
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("create collection request failed: %v", err)
	}
	return resp
}

func doDeleteEntities(t *testing.T, base, entityName string, version int) *http.Response {
	t.Helper()
	url := fmt.Sprintf("%s/entity/%s/%d", base, entityName, version)
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		t.Fatalf("failed to create delete entities request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete entities request failed: %v", err)
	}
	return resp
}

// --- Task 8: Batch operations tests ---

func TestCreateCollection(t *testing.T) {
	srv := newTestServer(t)

	importAndLockModel(t, srv.URL, "BatchPerson", 1, `{"name":"Alice","age":30}`)

	// Batch create 3 entities
	body := `[
		{"model":{"name":"BatchPerson","version":1},"payload":"{\"name\":\"Alice\",\"age\":30}"},
		{"model":{"name":"BatchPerson","version":1},"payload":"{\"name\":\"Bob\",\"age\":25}"},
		{"model":{"name":"BatchPerson","version":1},"payload":"{\"name\":\"Charlie\",\"age\":35}"}
	]`

	resp := doCreateCollection(t, srv.URL, "JSON", body)
	expectStatus(t, resp, http.StatusOK)

	respBody := readBody(t, resp)
	var createArr []map[string]any
	if err := json.Unmarshal(respBody, &createArr); err != nil {
		t.Fatalf("failed to parse create collection response: %v", err)
	}
	if len(createArr) != 1 {
		t.Fatalf("expected array of length 1, got %d", len(createArr))
	}
	createResp := createArr[0]

	// Verify transactionId present
	txID, ok := createResp["transactionId"].(string)
	if !ok || txID == "" {
		t.Fatalf("expected non-empty transactionId, got %v", createResp["transactionId"])
	}

	// Verify 3 entity IDs
	entityIDs, ok := createResp["entityIds"].([]any)
	if !ok || len(entityIDs) != 3 {
		t.Fatalf("expected 3 entityIds, got %v", createResp["entityIds"])
	}

	// Verify each entity can be fetched
	for i, idObj := range entityIDs {
		eid, ok := idObj.(string)
		if !ok || eid == "" {
			t.Fatalf("entityIds[%d]: expected non-empty string, got %T", i, idObj)
		}

		getResp := doGetEntity(t, srv.URL, eid)
		expectStatus(t, getResp, http.StatusOK)
		getResp.Body.Close()
	}
}

// --- Statistics helpers ---

func doGetStats(t *testing.T, base string) *http.Response {
	t.Helper()
	url := base + "/entity/stats"
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get stats request failed: %v", err)
	}
	return resp
}

func doGetStatsForModel(t *testing.T, base, entityName string, version int) *http.Response {
	t.Helper()
	url := fmt.Sprintf("%s/entity/stats/%s/%d", base, entityName, version)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get stats for model request failed: %v", err)
	}
	return resp
}

func doGetStatsByState(t *testing.T, base string) *http.Response {
	t.Helper()
	url := base + "/entity/stats/states"
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get stats by state request failed: %v", err)
	}
	return resp
}

func doGetStatsByStateForModel(t *testing.T, base, entityName string, version int) *http.Response {
	t.Helper()
	url := fmt.Sprintf("%s/entity/stats/states/%s/%d", base, entityName, version)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get stats by state for model request failed: %v", err)
	}
	return resp
}

// --- Task 9: Entity statistics tests ---

func TestGetEntityStatistics(t *testing.T) {
	srv := newTestServer(t)

	importAndLockModel(t, srv.URL, "StatsA", 1, `{"name":"Alice"}`)
	importAndLockModel(t, srv.URL, "StatsB", 1, `{"value":42}`)

	// Create 2 entities for StatsA, 1 for StatsB
	createEntityAndGetID(t, srv.URL, "StatsA", 1, `{"name":"Alice"}`)
	createEntityAndGetID(t, srv.URL, "StatsA", 1, `{"name":"Bob"}`)
	createEntityAndGetID(t, srv.URL, "StatsB", 1, `{"value":99}`)

	resp := doGetStats(t, srv.URL)
	expectStatus(t, resp, http.StatusOK)

	body := readBody(t, resp)
	var stats []map[string]any
	if err := json.Unmarshal(body, &stats); err != nil {
		t.Fatalf("failed to parse stats response: %v", err)
	}

	// Build a map of modelName -> count for verification
	countMap := make(map[string]float64)
	for _, s := range stats {
		name, _ := s["modelName"].(string)
		count, _ := s["count"].(float64)
		countMap[name] += count
	}

	if countMap["StatsA"] != 2 {
		t.Errorf("expected StatsA count=2, got %v", countMap["StatsA"])
	}
	if countMap["StatsB"] != 1 {
		t.Errorf("expected StatsB count=1, got %v", countMap["StatsB"])
	}
}

func TestGetEntityStatisticsForModel(t *testing.T) {
	srv := newTestServer(t)

	importAndLockModel(t, srv.URL, "StatsModel", 1, `{"name":"Alice"}`)
	createEntityAndGetID(t, srv.URL, "StatsModel", 1, `{"name":"Alice"}`)
	createEntityAndGetID(t, srv.URL, "StatsModel", 1, `{"name":"Bob"}`)

	resp := doGetStatsForModel(t, srv.URL, "StatsModel", 1)
	expectStatus(t, resp, http.StatusOK)

	body := readBody(t, resp)
	var stats map[string]any
	if err := json.Unmarshal(body, &stats); err != nil {
		t.Fatalf("failed to parse stats response: %v", err)
	}

	if stats["modelName"] != "StatsModel" {
		t.Errorf("expected modelName=StatsModel, got %v", stats["modelName"])
	}
	if stats["count"] != float64(2) {
		t.Errorf("expected count=2, got %v", stats["count"])
	}
}

func TestGetEntityStatisticsByState(t *testing.T) {
	srv := newTestServer(t)

	importAndLockModel(t, srv.URL, "StateStatsA", 1, `{"name":"Alice"}`)
	createEntityAndGetID(t, srv.URL, "StateStatsA", 1, `{"name":"Alice"}`)
	createEntityAndGetID(t, srv.URL, "StateStatsA", 1, `{"name":"Bob"}`)

	resp := doGetStatsByState(t, srv.URL)
	expectStatus(t, resp, http.StatusOK)

	body := readBody(t, resp)
	var stats []map[string]any
	if err := json.Unmarshal(body, &stats); err != nil {
		t.Fatalf("failed to parse stats by state response: %v", err)
	}

	// All entities should be in NEW state
	found := false
	for _, s := range stats {
		name, _ := s["modelName"].(string)
		state, _ := s["state"].(string)
		count, _ := s["count"].(float64)
		if name == "StateStatsA" && state == "CREATED" && count == 2 {
			found = true
		}
	}
	if !found {
		t.Errorf("expected StateStatsA with state=CREATED count=2, stats=%v", stats)
	}
}

func TestGetEntityStatisticsByStateForModel(t *testing.T) {
	srv := newTestServer(t)

	importAndLockModel(t, srv.URL, "StateStatsM", 1, `{"name":"Alice"}`)
	createEntityAndGetID(t, srv.URL, "StateStatsM", 1, `{"name":"Alice"}`)
	createEntityAndGetID(t, srv.URL, "StateStatsM", 1, `{"name":"Bob"}`)
	createEntityAndGetID(t, srv.URL, "StateStatsM", 1, `{"name":"Charlie"}`)

	resp := doGetStatsByStateForModel(t, srv.URL, "StateStatsM", 1)
	expectStatus(t, resp, http.StatusOK)

	body := readBody(t, resp)
	var stats []map[string]any
	if err := json.Unmarshal(body, &stats); err != nil {
		t.Fatalf("failed to parse stats by state for model response: %v", err)
	}

	if len(stats) != 1 {
		t.Fatalf("expected 1 state group, got %d: %v", len(stats), stats)
	}
	if stats[0]["modelName"] != "StateStatsM" {
		t.Errorf("expected modelName=StateStatsM, got %v", stats[0]["modelName"])
	}
	if stats[0]["state"] != "CREATED" {
		t.Errorf("expected state=CREATED, got %v", stats[0]["state"])
	}
	if stats[0]["count"] != float64(3) {
		t.Errorf("expected count=3, got %v", stats[0]["count"])
	}
}

func TestDeleteEntities(t *testing.T) {
	srv := newTestServer(t)

	importAndLockModel(t, srv.URL, "BatchDel", 1, `{"name":"Alice","age":30}`)

	// Create 3 entities
	for i := 0; i < 3; i++ {
		body := fmt.Sprintf(`{"name":"Person%d","age":%d}`, i, 20+i)
		createEntityAndGetID(t, srv.URL, "BatchDel", 1, body)
	}

	// Verify 3 entities exist
	resp := doGetAllEntities(t, srv.URL, "BatchDel", 1, 20, 0)
	expectStatus(t, resp, http.StatusOK)
	body := readBody(t, resp)
	var entities []map[string]any
	json.Unmarshal(body, &entities)
	if len(entities) != 3 {
		t.Fatalf("expected 3 entities before delete, got %d", len(entities))
	}

	// Batch delete all
	resp = doDeleteEntities(t, srv.URL, "BatchDel", 1)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	// Verify GetAll returns empty
	resp = doGetAllEntities(t, srv.URL, "BatchDel", 1, 20, 0)
	expectStatus(t, resp, http.StatusOK)
	body = readBody(t, resp)
	if strings.TrimSpace(string(body)) != "[]" {
		t.Fatalf("expected empty array after batch delete, got %s", string(body))
	}
}

func TestDeleteEntities_NonexistentModel_Returns404(t *testing.T) {
	srv := newTestServer(t)

	// DELETE entities for a model:version that was never imported.
	resp := doDeleteEntities(t, srv.URL, "NoSuchModel", 1)
	expectStatus(t, resp, http.StatusNotFound)
	resp.Body.Close()
}

// --- Task 11: Tenant isolation and API fidelity tests ---

func TestTenantIsolation(t *testing.T) {
	// App A: default tenant
	cfgA := app.DefaultConfig()
	cfgA.ContextPath = ""
	cfgA.IAM.MockTenantID = "tenant-A"
	cfgA.IAM.MockTenantName = "Tenant A"
	srvA := newTestServerWithConfig(t, cfgA)

	// App B: different tenant (separate in-memory stores)
	cfgB := app.DefaultConfig()
	cfgB.ContextPath = ""
	cfgB.IAM.MockTenantID = "tenant-B"
	cfgB.IAM.MockTenantName = "Tenant B"
	srvB := newTestServerWithConfig(t, cfgB)

	// Import + lock model and create entity in app A
	importAndLockModel(t, srvA.URL, "IsoTest", 1, `{"name":"Alice","age":30}`)
	entityID := createEntityAndGetID(t, srvA.URL, "IsoTest", 1, `{"name":"TenantA","age":1}`)

	// Verify entity exists in app A
	resp := doGetEntity(t, srvA.URL, entityID)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	// Import + lock same model in app B so the route is valid
	importAndLockModel(t, srvB.URL, "IsoTest", 1, `{"name":"Alice","age":30}`)

	// GetAll from app B should return empty array (separate store)
	resp = doGetAllEntities(t, srvB.URL, "IsoTest", 1, 20, 0)
	expectStatus(t, resp, http.StatusOK)
	body := readBody(t, resp)
	if strings.TrimSpace(string(body)) != "[]" {
		t.Fatalf("expected empty array from tenant B, got %s", string(body))
	}

	// GetEntity by ID from app B should return 404
	resp = doGetEntity(t, srvB.URL, entityID)
	expectStatus(t, resp, http.StatusNotFound)
	resp.Body.Close()
}

func TestEntityResponseEnvelope(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "EnvTest", 1, `{"name":"Alice","age":30}`)

	entityID := createEntityAndGetID(t, srv.URL, "EnvTest", 1, `{"name":"Bob","age":25}`)

	resp := doGetEntity(t, srv.URL, entityID)
	expectStatus(t, resp, http.StatusOK)
	body := readBody(t, resp)

	var envelope map[string]any
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	// Verify top-level keys: type, data, meta
	typ, ok := envelope["type"].(string)
	if !ok || typ != "ENTITY" {
		t.Errorf("expected type=ENTITY (string), got %v", envelope["type"])
	}

	data, ok := envelope["data"].(map[string]any)
	if !ok {
		t.Fatal("expected data to be an object")
	}
	if data["name"] != "Bob" {
		t.Errorf("expected data.name=Bob, got %v", data["name"])
	}

	meta, ok := envelope["meta"].(map[string]any)
	if !ok {
		t.Fatal("expected meta to be an object")
	}

	// Verify all required meta fields
	requiredMetaKeys := []string{
		"id", "modelKey", "state", "creationDate",
		"lastUpdateTime", "transactionId",
	}
	for _, key := range requiredMetaKeys {
		if _, exists := meta[key]; !exists {
			t.Errorf("expected meta to have key %q, but it is missing", key)
		}
	}

	// After creation without an explicit transition, transitionForLatestSave
	// must be "loopback" — not the literal "workflow", which is not a valid
	// value (issue #94).
	if v, exists := meta["transitionForLatestSave"]; !exists || v != "loopback" {
		t.Errorf("expected transitionForLatestSave=loopback after creation, got %v", meta["transitionForLatestSave"])
	}

	// Verify meta.id is a string matching the entity ID
	if meta["id"] != entityID {
		t.Errorf("expected meta.id=%s, got %v", entityID, meta["id"])
	}

	// Verify modelKey is an object with name and version
	mk, ok := meta["modelKey"].(map[string]any)
	if !ok {
		t.Fatal("expected meta.modelKey to be an object")
	}
	if mk["name"] != "EnvTest" {
		t.Errorf("expected modelKey.name=EnvTest, got %v", mk["name"])
	}
	if mk["version"] != float64(1) {
		t.Errorf("expected modelKey.version=1, got %v", mk["version"])
	}

	// Verify state is a string
	if _, ok := meta["state"].(string); !ok {
		t.Errorf("expected meta.state to be a string, got %T", meta["state"])
	}

	// Verify creationDate and lastUpdateTime are non-empty strings
	if cd, ok := meta["creationDate"].(string); !ok || cd == "" {
		t.Errorf("expected non-empty creationDate string, got %v", meta["creationDate"])
	}
	if lut, ok := meta["lastUpdateTime"].(string); !ok || lut == "" {
		t.Errorf("expected non-empty lastUpdateTime string, got %v", meta["lastUpdateTime"])
	}

	// Verify transactionId is a non-empty string
	if txID, ok := meta["transactionId"].(string); !ok || txID == "" {
		t.Errorf("expected non-empty transactionId string, got %v", meta["transactionId"])
	}

}

func TestTransactionIdIsUUID(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "TxUUIDTest", 1, `{"name":"Alice"}`)

	// Create entity and check transactionId in the create response
	resp := doCreateEntity(t, srv.URL, "JSON", "TxUUIDTest", 1, `{"name":"Bob"}`)
	expectStatus(t, resp, http.StatusOK)
	body := readBody(t, resp)

	var createArr []map[string]any
	if err := json.Unmarshal(body, &createArr); err != nil {
		t.Fatalf("failed to parse create response: %v", err)
	}
	createResp := createArr[0]

	txIDStr, ok := createResp["transactionId"].(string)
	if !ok || txIDStr == "" {
		t.Fatalf("expected non-empty transactionId string, got %v", createResp["transactionId"])
	}

	if _, err := uuid.Parse(txIDStr); err != nil {
		t.Errorf("transactionId %q is not a valid UUID: %v", txIDStr, err)
	}

	// Also check transactionId in the entity envelope (via GET)
	entityIDs := createResp["entityIds"].([]any)
	entityID := entityIDs[0].(string)

	resp = doGetEntity(t, srv.URL, entityID)
	expectStatus(t, resp, http.StatusOK)
	body = readBody(t, resp)

	var envelope map[string]any
	json.Unmarshal(body, &envelope)
	meta := envelope["meta"].(map[string]any)

	metaTxID, ok := meta["transactionId"].(string)
	if !ok || metaTxID == "" {
		t.Fatalf("expected non-empty transactionId in meta, got %v", meta["transactionId"])
	}
	if _, err := uuid.Parse(metaTxID); err != nil {
		t.Errorf("meta transactionId %q is not a valid UUID: %v", metaTxID, err)
	}
}

// --- changeLevel helpers ---

func doChangeLevel(t *testing.T, base, entityName string, version int, level string) *http.Response {
	t.Helper()
	url := base + "/model/" + entityName + "/" + strconv.Itoa(version) + "/changeLevel/" + level
	req, _ := http.NewRequest(http.MethodPost, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("changeLevel request failed: %v", err)
	}
	return resp
}

func doExportModel(t *testing.T, base, converter, entityName string, version int) *http.Response {
	t.Helper()
	url := base + "/model/export/" + converter + "/" + entityName + "/" + strconv.Itoa(version)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("export request failed: %v", err)
	}
	return resp
}

// TestCreateEntityWithChangeLevel verifies that when changeLevel is STRUCTURAL,
// creating an entity with extra fields extends the model rather than rejecting.
func TestCreateEntityWithChangeLevel(t *testing.T) {
	srv := newTestServer(t)

	// Import a model with only "name" field, lock it, then set changeLevel=STRUCTURAL
	importAndLockModel(t, srv.URL, "CLEntity", 1, `{"name":"Alice"}`)

	resp := doChangeLevel(t, srv.URL, "CLEntity", 1, "STRUCTURAL")
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	// Create entity with extra fields not in the original model
	resp = doCreateEntity(t, srv.URL, "JSON", "CLEntity", 1, `{"name":"Bob","age":25,"email":"bob@test.com"}`)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	// Export the model and verify it was extended with the new fields
	resp = doExportModel(t, srv.URL, "JSON_SCHEMA", "CLEntity", 1)
	expectStatus(t, resp, http.StatusOK)

	body := readBody(t, resp)
	var schema map[string]any
	if err := json.Unmarshal(body, &schema); err != nil {
		t.Fatalf("failed to parse exported schema: %v", err)
	}

	// The JSON schema wraps the model; navigate to the model's properties.
	// Structure: { "model": { "type": "object", "properties": { ... } } }
	modelObj, ok := schema["model"].(map[string]any)
	if !ok {
		// Fall back to top-level properties if model key is absent.
		modelObj = schema
	}
	props, ok := modelObj["properties"].(map[string]any)
	if !ok {
		t.Fatalf("expected properties in schema model, got %v", schema)
	}
	for _, field := range []string{"name", "age", "email"} {
		if _, exists := props[field]; !exists {
			t.Errorf("expected property %q in exported schema after extension, properties: %v", field, props)
		}
	}
}

// TestCreateEntityStrictValidationRejectsExtraFields verifies that without
// changeLevel, creating an entity with extra fields is rejected (400).
func TestCreateEntityStrictValidationRejectsExtraFields(t *testing.T) {
	srv := newTestServer(t)

	// Import a model with only "name" field and lock it (no changeLevel set)
	importAndLockModel(t, srv.URL, "StrictCL", 1, `{"name":"Alice"}`)

	// Create entity with extra fields → should be rejected
	resp := doCreateEntity(t, srv.URL, "JSON", "StrictCL", 1, `{"name":"Bob","extraField":"unexpected"}`)
	expectStatus(t, resp, http.StatusBadRequest)
	resp.Body.Close()
}

func TestUpdateSingleEntityNotFound(t *testing.T) {
	srv := newTestServer(t)
	resp := doUpdateEntity(t, srv.URL, "JSON", "00000000-0000-0000-0000-000000000099", "t", `{"x":1}`)
	expectStatus(t, resp, http.StatusNotFound)
	resp.Body.Close()
}

func TestUpdateSingleEntityXML(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "UpdXML", 1, `{"name":"Alice","age":30}`)
	entityID := createEntityAndGetID(t, srv.URL, "UpdXML", 1, `{"name":"Original","age":1}`)

	xmlBody := `<root><name>XMLUpdated</name><age>42</age></root>`
	resp := doUpdateEntity(t, srv.URL, "XML", entityID, "UPDATE", xmlBody)
	expectStatus(t, resp, http.StatusOK)

	body := readBody(t, resp)
	var updateResp map[string]any
	if err := json.Unmarshal(body, &updateResp); err != nil {
		t.Fatalf("failed to parse update response: %v", err)
	}
	if updateResp["transactionId"] == nil {
		t.Fatal("expected transactionId")
	}

	// Verify data was updated
	resp = doGetEntity(t, srv.URL, entityID)
	expectStatus(t, resp, http.StatusOK)
	body = readBody(t, resp)
	var envelope map[string]any
	json.Unmarshal(body, &envelope)
	data := envelope["data"].(map[string]any)
	if data["name"] != "XMLUpdated" {
		t.Errorf("expected name=XMLUpdated, got %v", data["name"])
	}
}

func TestUpdateSingleValidationRejectsExtraFields(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "UpdStrict", 1, `{"name":"Alice"}`)
	entityID := createEntityAndGetID(t, srv.URL, "UpdStrict", 1, `{"name":"Original"}`)

	resp := doUpdateEntity(t, srv.URL, "JSON", entityID, "t", `{"name":"Bob","extra":"bad"}`)
	expectStatus(t, resp, http.StatusBadRequest)
	resp.Body.Close()
}

func TestCreateCollectionValidation(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "BatchVal", 1, `{"name":"Alice"}`)

	// One item with extra field → 400
	body := `[{"model":{"name":"BatchVal","version":1},"payload":"{\"name\":\"Bob\",\"extra\":\"bad\"}"}]`
	resp := doCreateCollection(t, srv.URL, "JSON", body)
	expectStatus(t, resp, http.StatusBadRequest)
	resp.Body.Close()
}

func TestCreateCollectionModelNotLocked(t *testing.T) {
	srv := newTestServer(t)
	// Import but do NOT lock
	url := srv.URL + "/model/import/JSON/SAMPLE_DATA/BatchUnlocked/1"
	resp, _ := http.Post(url, "application/json", strings.NewReader(`{"name":"Alice"}`))
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	body := `[{"model":{"name":"BatchUnlocked","version":1},"payload":"{\"name\":\"Bob\"}"}]`
	resp = doCreateCollection(t, srv.URL, "JSON", body)
	expectStatus(t, resp, http.StatusConflict)
	resp.Body.Close()
}

func TestDeleteEntitiesVerbose(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "VerbDel", 1, `{"name":"Alice"}`)
	createEntityAndGetID(t, srv.URL, "VerbDel", 1, `{"name":"Alice"}`)
	createEntityAndGetID(t, srv.URL, "VerbDel", 1, `{"name":"Bob"}`)

	url := fmt.Sprintf("%s/entity/%s/%d?verbose=true", srv.URL, "VerbDel", 1)
	req, _ := http.NewRequest(http.MethodDelete, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete request failed: %v", err)
	}
	expectStatus(t, resp, http.StatusOK)
	body := readBody(t, resp)

	// Response is a single StreamDeleteResult object (not an array).
	// Spec: deleteEntities → 200 → $ref StreamDeleteResult
	// {entityModelClassId, deleteResult: {numberOfEntitites, numberOfEntititesRemoved, idToError}}
	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("expected JSON object, got error: %v\nbody: %s", err, body)
	}
	dr, ok := result["deleteResult"].(map[string]any)
	if !ok {
		t.Fatal("expected deleteResult object in response")
	}
	if dr["numberOfEntitites"] != float64(2) {
		t.Errorf("expected numberOfEntitites=2, got %v", dr["numberOfEntitites"])
	}
	if dr["numberOfEntititesRemoved"] != float64(2) {
		t.Errorf("expected numberOfEntititesRemoved=2, got %v", dr["numberOfEntititesRemoved"])
	}
	if _, ok := result["entityModelClassId"]; !ok {
		t.Errorf("expected entityModelClassId in response")
	}
}

func TestCreateEntityInvalidJSON(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "BadJSON", 1, `{"name":"Alice"}`)
	resp := doCreateEntity(t, srv.URL, "JSON", "BadJSON", 1, `{invalid}`)
	expectStatus(t, resp, http.StatusBadRequest)
	resp.Body.Close()
}

func TestCreateEntityUnsupportedFormat(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "FmtTest", 1, `{"name":"Alice"}`)
	// Use an unsupported format string in the URL
	url := srv.URL + "/entity/YAML/FmtTest/1"
	resp, err := http.Post(url, "application/json", strings.NewReader(`{"name":"Alice"}`))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	expectStatus(t, resp, http.StatusBadRequest)
	resp.Body.Close()
}

func TestUpdateSingleUnsupportedFormat(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "UpdFmt", 1, `{"name":"Alice"}`)
	entityID := createEntityAndGetID(t, srv.URL, "UpdFmt", 1, `{"name":"Alice"}`)
	resp := doUpdateEntity(t, srv.URL, "YAML", entityID, "t", `{"name":"Bob"}`)
	expectStatus(t, resp, http.StatusBadRequest)
	resp.Body.Close()
}

func TestUpdateSingleInvalidJSON(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "UpdBad", 1, `{"name":"Alice"}`)
	entityID := createEntityAndGetID(t, srv.URL, "UpdBad", 1, `{"name":"Alice"}`)
	resp := doUpdateEntity(t, srv.URL, "JSON", entityID, "t", `{bad json}`)
	expectStatus(t, resp, http.StatusBadRequest)
	resp.Body.Close()
}

func TestCreateCollectionInvalidJSON(t *testing.T) {
	srv := newTestServer(t)
	resp := doCreateCollection(t, srv.URL, "JSON", `{not an array}`)
	expectStatus(t, resp, http.StatusBadRequest)
	resp.Body.Close()
}

func TestCreateCollectionModelNotFound(t *testing.T) {
	srv := newTestServer(t)
	body := `[{"model":{"name":"NoSuchModel","version":1},"payload":"{\"name\":\"Bob\"}"}]`
	resp := doCreateCollection(t, srv.URL, "JSON", body)
	expectStatus(t, resp, http.StatusNotFound)
	resp.Body.Close()
}

func TestCreateCollectionInvalidPayload(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "BatchBadPay", 1, `{"name":"Alice"}`)
	body := `[{"model":{"name":"BatchBadPay","version":1},"payload":"{bad json}"}]`
	resp := doCreateCollection(t, srv.URL, "JSON", body)
	expectStatus(t, resp, http.StatusBadRequest)
	resp.Body.Close()
}

func TestGetEntityChangesMetadata(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "ChangesMeta", 1, `{"name":"Alice","age":30}`)

	// Create entity
	entityID := createEntityAndGetID(t, srv.URL, "ChangesMeta", 1, `{"name":"Alice","age":30}`)

	// Update twice
	resp := doUpdateEntity(t, srv.URL, "JSON", entityID, "UPDATE", `{"name":"Alice","age":31}`)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	resp = doUpdateEntity(t, srv.URL, "JSON", entityID, "UPDATE", `{"name":"Alice","age":32}`)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	// Query changes metadata
	url := fmt.Sprintf("%s/entity/%s/changes", srv.URL, entityID)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get changes metadata request failed: %v", err)
	}
	expectStatus(t, resp, http.StatusOK)
	body := readBody(t, resp)

	var changes []map[string]any
	if err := json.Unmarshal(body, &changes); err != nil {
		t.Fatalf("failed to parse changes response: %v", err)
	}

	if len(changes) != 3 {
		t.Fatalf("expected 3 change entries, got %d", len(changes))
	}

	// Verify reverse chronological order: UPDATED, UPDATED, CREATED
	expectedTypes := []string{"UPDATED", "UPDATED", "CREATED"}
	for i, expected := range expectedTypes {
		ct, ok := changes[i]["changeType"].(string)
		if !ok || ct != expected {
			t.Errorf("entry %d: expected changeType=%s, got %v", i, expected, changes[i]["changeType"])
		}
		if changes[i]["timeOfChange"] == nil || changes[i]["timeOfChange"] == "" {
			t.Errorf("entry %d: expected non-empty timeOfChange", i)
		}
		if changes[i]["transactionId"] == nil || changes[i]["transactionId"] == "" {
			t.Errorf("entry %d: expected non-empty transactionId", i)
		}
	}

	// Verify 404 for non-existent entity
	badID := uuid.New().String()
	url = fmt.Sprintf("%s/entity/%s/changes", srv.URL, badID)
	resp, err = http.Get(url)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	expectStatus(t, resp, http.StatusNotFound)
	resp.Body.Close()
}

// TestGetEntityChangesMetadata_PointInTime asserts that the pointInTime
// query parameter constrains the returned change history to entries whose
// timeOfChange is at or before the supplied timestamp. Regression test
// for issue #152: handler previously dropped the parameter silently.
func TestGetEntityChangesMetadata_PointInTime(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "ChangesMetaPIT", 1, `{"k":1}`)

	// Create entity (k=1).
	entityID := createEntityAndGetID(t, srv.URL, "ChangesMetaPIT", 1, `{"k":1}`)

	// Update (k=2).
	resp := doUpdateEntity(t, srv.URL, "JSON", entityID, "UPDATE", `{"k":2}`)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	// Capture cutoff between updates.
	time.Sleep(50 * time.Millisecond)
	cutoff := time.Now().UTC()
	time.Sleep(50 * time.Millisecond)

	// Update (k=3) — after cutoff.
	resp = doUpdateEntity(t, srv.URL, "JSON", entityID, "UPDATE", `{"k":3}`)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	// Without pointInTime — full history (3 entries).
	url := fmt.Sprintf("%s/entity/%s/changes", srv.URL, entityID)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get changes (full): %v", err)
	}
	expectStatus(t, resp, http.StatusOK)
	body := readBody(t, resp)
	var full []map[string]any
	if err := json.Unmarshal(body, &full); err != nil {
		t.Fatalf("parse full: %v", err)
	}
	if len(full) != 3 {
		t.Fatalf("full history: expected 3 entries, got %d", len(full))
	}

	// With pointInTime=cutoff — truncated history (2 entries: CREATED + first UPDATED).
	pitURL := fmt.Sprintf("%s/entity/%s/changes?pointInTime=%s",
		srv.URL, entityID, cutoff.Format(time.RFC3339Nano))
	resp, err = http.Get(pitURL)
	if err != nil {
		t.Fatalf("get changes (pit): %v", err)
	}
	expectStatus(t, resp, http.StatusOK)
	body = readBody(t, resp)
	var truncated []map[string]any
	if err := json.Unmarshal(body, &truncated); err != nil {
		t.Fatalf("parse truncated: %v", err)
	}
	if len(truncated) != 2 {
		t.Fatalf("truncated history: expected 2 entries (CREATED + UPDATED), got %d: %v", len(truncated), truncated)
	}
	// Newest-first order: [UPDATED, CREATED].
	if ct, _ := truncated[0]["changeType"].(string); ct != "UPDATED" {
		t.Errorf("truncated[0].changeType: got %v, want UPDATED", truncated[0]["changeType"])
	}
	if ct, _ := truncated[1]["changeType"].(string); ct != "CREATED" {
		t.Errorf("truncated[1].changeType: got %v, want CREATED", truncated[1]["changeType"])
	}

	// All returned entries must have timeOfChange <= cutoff.
	for i, entry := range truncated {
		ts, _ := entry["timeOfChange"].(string)
		got, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			t.Fatalf("entry %d: bad timeOfChange %q: %v", i, ts, err)
		}
		if got.After(cutoff) {
			t.Errorf("entry %d: timeOfChange %s is after cutoff %s", i, got, cutoff)
		}
	}
}

// TestGetEntityChangesMetadata_PointInTimeFuture asserts that a pointInTime
// strictly after the latest change returns the full history — equivalent to
// omitting the parameter. Boundary case for issue #152.
func TestGetEntityChangesMetadata_PointInTimeFuture(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "ChangesMetaPITFuture", 1, `{"k":1}`)

	entityID := createEntityAndGetID(t, srv.URL, "ChangesMetaPITFuture", 1, `{"k":1}`)
	resp := doUpdateEntity(t, srv.URL, "JSON", entityID, "UPDATE", `{"k":2}`)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()
	resp = doUpdateEntity(t, srv.URL, "JSON", entityID, "UPDATE", `{"k":3}`)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	// pointInTime strictly after the latest change.
	future := time.Now().UTC().Add(1 * time.Hour)
	pitURL := fmt.Sprintf("%s/entity/%s/changes?pointInTime=%s",
		srv.URL, entityID, future.Format(time.RFC3339Nano))
	resp, err := http.Get(pitURL)
	if err != nil {
		t.Fatalf("get changes (future pit): %v", err)
	}
	expectStatus(t, resp, http.StatusOK)
	body := readBody(t, resp)
	var futureResult []map[string]any
	if err := json.Unmarshal(body, &futureResult); err != nil {
		t.Fatalf("parse future result: %v", err)
	}
	if len(futureResult) != 3 {
		t.Fatalf("future pointInTime: expected full history (3 entries), got %d", len(futureResult))
	}

	// Cross-check: omitting the parameter yields the same result set.
	url := fmt.Sprintf("%s/entity/%s/changes", srv.URL, entityID)
	resp, err = http.Get(url)
	if err != nil {
		t.Fatalf("get changes (no pit): %v", err)
	}
	expectStatus(t, resp, http.StatusOK)
	body = readBody(t, resp)
	var fullResult []map[string]any
	if err := json.Unmarshal(body, &fullResult); err != nil {
		t.Fatalf("parse full result: %v", err)
	}
	if len(fullResult) != len(futureResult) {
		t.Fatalf("future pointInTime length %d != full history length %d", len(futureResult), len(fullResult))
	}
	for i := range fullResult {
		if fullResult[i]["timeOfChange"] != futureResult[i]["timeOfChange"] {
			t.Errorf("entry %d: full timeOfChange=%v, future timeOfChange=%v",
				i, fullResult[i]["timeOfChange"], futureResult[i]["timeOfChange"])
		}
		if fullResult[i]["changeType"] != futureResult[i]["changeType"] {
			t.Errorf("entry %d: full changeType=%v, future changeType=%v",
				i, fullResult[i]["changeType"], futureResult[i]["changeType"])
		}
	}
}

// TestGetEntityChangesMetadata_PointInTimeExactBoundary asserts that a
// pointInTime exactly equal to a change's timestamp INCLUDES that change —
// the filter is at-or-before (<=), not strictly-before. Boundary case for
// issue #152.
func TestGetEntityChangesMetadata_PointInTimeExactBoundary(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "ChangesMetaPITExact", 1, `{"k":1}`)

	entityID := createEntityAndGetID(t, srv.URL, "ChangesMetaPITExact", 1, `{"k":1}`)
	resp := doUpdateEntity(t, srv.URL, "JSON", entityID, "UPDATE", `{"k":2}`)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()
	resp = doUpdateEntity(t, srv.URL, "JSON", entityID, "UPDATE", `{"k":3}`)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	// Read the full history to discover an actual change timestamp.
	url := fmt.Sprintf("%s/entity/%s/changes", srv.URL, entityID)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get changes (full): %v", err)
	}
	expectStatus(t, resp, http.StatusOK)
	body := readBody(t, resp)
	var full []map[string]any
	if err := json.Unmarshal(body, &full); err != nil {
		t.Fatalf("parse full: %v", err)
	}
	if len(full) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(full))
	}

	// History is newest-first: pick the middle change (first UPDATED).
	// Using its exact timestamp as pointInTime must include that change
	// (the boundary is inclusive) and exclude the later one.
	boundaryStr, _ := full[1]["timeOfChange"].(string)
	boundary, err := time.Parse(time.RFC3339Nano, boundaryStr)
	if err != nil {
		t.Fatalf("parse boundary timestamp %q: %v", boundaryStr, err)
	}

	pitURL := fmt.Sprintf("%s/entity/%s/changes?pointInTime=%s",
		srv.URL, entityID, boundary.Format(time.RFC3339Nano))
	resp, err = http.Get(pitURL)
	if err != nil {
		t.Fatalf("get changes (exact pit): %v", err)
	}
	expectStatus(t, resp, http.StatusOK)
	body = readBody(t, resp)
	var atBoundary []map[string]any
	if err := json.Unmarshal(body, &atBoundary); err != nil {
		t.Fatalf("parse boundary result: %v", err)
	}

	// Inclusive semantics: the boundary entry (full[1]) AND the older
	// CREATED entry (full[2]) must be present; the newer entry (full[0])
	// must be absent.
	if len(atBoundary) != 2 {
		t.Fatalf("exact-boundary pointInTime: expected 2 entries (boundary + older), got %d: %v",
			len(atBoundary), atBoundary)
	}
	// Newest-first: [boundary UPDATED, CREATED].
	if got := atBoundary[0]["timeOfChange"]; got != boundaryStr {
		t.Errorf("atBoundary[0].timeOfChange: got %v, want %s (boundary entry must be included)", got, boundaryStr)
	}
	if ct, _ := atBoundary[0]["changeType"].(string); ct != "UPDATED" {
		t.Errorf("atBoundary[0].changeType: got %v, want UPDATED", atBoundary[0]["changeType"])
	}
	if ct, _ := atBoundary[1]["changeType"].(string); ct != "CREATED" {
		t.Errorf("atBoundary[1].changeType: got %v, want CREATED", atBoundary[1]["changeType"])
	}
}

// --- Workflow integration tests ---

// importWorkflow posts a workflow definition for the given model.
func importWorkflow(t *testing.T, base, entityName string, version int, body string) {
	t.Helper()
	url := base + "/model/" + entityName + "/" + strconv.Itoa(version) + "/workflow/import"
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("workflow import request failed: %v", err)
	}
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()
}

// doUpdateEntityWithLoopback PUTs entity data without a named transition (loopback).
func doUpdateEntityWithLoopback(t *testing.T, base, format, entityID, body string) *http.Response {
	t.Helper()
	url := fmt.Sprintf("%s/entity/%s/%s", base, format, entityID)
	req, err := http.NewRequest(http.MethodPut, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("failed to create loopback update request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("loopback update request failed: %v", err)
	}
	return resp
}

func getEntityState(t *testing.T, base, entityID string) string {
	t.Helper()
	resp := doGetEntity(t, base, entityID)
	expectStatus(t, resp, http.StatusOK)
	body := readBody(t, resp)
	var envelope map[string]any
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("failed to parse entity envelope: %v", err)
	}
	meta := envelope["meta"].(map[string]any)
	return meta["state"].(string)
}

func TestCreateWithWorkflow(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "WfCreate", 1, `{"name":"Alice","age":30}`)

	// Import workflow: INITIAL --(auto)--> VALIDATED (stable, no further auto transitions)
	wfBody := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1",
			"name": "create-flow",
			"initialState": "INITIAL",
			"active": true,
			"states": {
				"INITIAL": {
					"transitions": [{
						"name": "auto-validate",
						"next": "VALIDATED",
						"manual": false
					}]
				},
				"VALIDATED": {}
			}
		}]
	}`
	importWorkflow(t, srv.URL, "WfCreate", 1, wfBody)

	entityID := createEntityAndGetID(t, srv.URL, "WfCreate", 1, `{"name":"Bob","age":25}`)
	state := getEntityState(t, srv.URL, entityID)
	if state != "VALIDATED" {
		t.Errorf("expected entity state VALIDATED after workflow, got %q", state)
	}
}

func TestCreateNoWorkflow(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "NoWf", 1, `{"name":"Alice"}`)

	entityID := createEntityAndGetID(t, srv.URL, "NoWf", 1, `{"name":"Bob"}`)
	state := getEntityState(t, srv.URL, entityID)
	if state != "CREATED" {
		t.Errorf("expected entity state CREATED (no workflow), got %q", state)
	}
}

func TestManualTransition(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "WfManual", 1, `{"name":"Alice","age":30}`)

	// Workflow: INITIAL --(auto)--> PENDING --(manual "approve")--> APPROVED
	wfBody := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1",
			"name": "approval-flow",
			"initialState": "INITIAL",
			"active": true,
			"states": {
				"INITIAL": {
					"transitions": [{
						"name": "auto-pending",
						"next": "PENDING",
						"manual": false
					}]
				},
				"PENDING": {
					"transitions": [{
						"name": "approve",
						"next": "APPROVED",
						"manual": true
					}]
				},
				"APPROVED": {}
			}
		}]
	}`
	importWorkflow(t, srv.URL, "WfManual", 1, wfBody)

	entityID := createEntityAndGetID(t, srv.URL, "WfManual", 1, `{"name":"Bob","age":25}`)
	state := getEntityState(t, srv.URL, entityID)
	if state != "PENDING" {
		t.Fatalf("expected PENDING after create, got %q", state)
	}

	// Fire manual transition "approve"
	resp := doUpdateEntity(t, srv.URL, "JSON", entityID, "approve", `{"name":"Bob","age":25}`)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	state = getEntityState(t, srv.URL, entityID)
	if state != "APPROVED" {
		t.Errorf("expected APPROVED after manual transition, got %q", state)
	}
}

func TestLoopbackTransition(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "WfLoop", 1, `{"name":"Alice","age":30}`)

	// Workflow: INITIAL --(auto)--> PENDING (stable, manual only)
	// After loopback with updated data matching criterion, auto transition fires:
	// PENDING --(auto, criterion: age>=50)--> SENIOR
	wfBody := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1",
			"name": "loopback-flow",
			"initialState": "INITIAL",
			"active": true,
			"states": {
				"INITIAL": {
					"transitions": [{
						"name": "auto-pending",
						"next": "PENDING",
						"manual": false
					}]
				},
				"PENDING": {
					"transitions": [{
						"name": "auto-senior",
						"next": "SENIOR",
						"manual": false,
						"criterion": {"type":"simple","jsonPath":"$.age","operatorType":"GREATER_THAN","value":49}
					}]
				},
				"SENIOR": {}
			}
		}]
	}`
	importWorkflow(t, srv.URL, "WfLoop", 1, wfBody)

	entityID := createEntityAndGetID(t, srv.URL, "WfLoop", 1, `{"name":"Bob","age":25}`)
	state := getEntityState(t, srv.URL, entityID)
	if state != "PENDING" {
		t.Fatalf("expected PENDING after create (criterion not met), got %q", state)
	}

	// Loopback with age >= 50 → should trigger auto-senior
	resp := doUpdateEntityWithLoopback(t, srv.URL, "JSON", entityID, `{"name":"Bob","age":55}`)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	state = getEntityState(t, srv.URL, entityID)
	if state != "SENIOR" {
		t.Errorf("expected SENIOR after loopback with age>=50, got %q", state)
	}
}

func TestTransitionNotFound(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "WfBadTx", 1, `{"name":"Alice"}`)

	// Simple workflow with one state, no manual transitions
	wfBody := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1",
			"name": "simple-flow",
			"initialState": "STABLE",
			"active": true,
			"states": {
				"STABLE": {}
			}
		}]
	}`
	importWorkflow(t, srv.URL, "WfBadTx", 1, wfBody)

	entityID := createEntityAndGetID(t, srv.URL, "WfBadTx", 1, `{"name":"Bob"}`)

	// Try a non-existent transition → 400
	resp := doUpdateEntity(t, srv.URL, "JSON", entityID, "nonExistent", `{"name":"Bob"}`)
	expectStatus(t, resp, http.StatusBadRequest)
	resp.Body.Close()
}

func TestWaitForConsistencyFalse(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "WfAsync", 1, `{"name":"Alice"}`)

	entityID := createEntityAndGetID(t, srv.URL, "WfAsync", 1, `{"name":"Bob"}`)

	// UpdateSingle with waitForConsistencyAfter=false → succeeds (200) after SSI commit.
	url := fmt.Sprintf("%s/entity/JSON/%s/UPDATE?waitForConsistencyAfter=false", srv.URL, entityID)
	req, err := http.NewRequest(http.MethodPut, url, strings.NewReader(`{"name":"Bob"}`))
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	// UpdateSingleWithLoopback with waitForConsistencyAfter=false → succeeds (200).
	url = fmt.Sprintf("%s/entity/JSON/%s?waitForConsistencyAfter=false", srv.URL, entityID)
	req, err = http.NewRequest(http.MethodPut, url, strings.NewReader(`{"name":"Bob"}`))
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()
}

// --- If-Match optimistic-concurrency tests ---

func getEntityTransactionID(t *testing.T, base, entityID string) string {
	t.Helper()
	resp := doGetEntity(t, base, entityID)
	expectStatus(t, resp, http.StatusOK)
	body := readBody(t, resp)
	var envelope map[string]any
	json.Unmarshal(body, &envelope)
	meta := envelope["meta"].(map[string]any)
	return meta["transactionId"].(string)
}

func TestMVCCMatchSucceeds(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "MVCCMatch", 1, `{"name":"Alice","age":30}`)

	entityID := createEntityAndGetID(t, srv.URL, "MVCCMatch", 1, `{"name":"Bob","age":25}`)
	txID := getEntityTransactionID(t, srv.URL, entityID)

	// Update with matching If-Match → should succeed (200)
	url := fmt.Sprintf("%s/entity/JSON/%s/UPDATE", srv.URL, entityID)
	req, err := http.NewRequest(http.MethodPut, url, strings.NewReader(`{"name":"Carol","age":26}`))
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", txID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()
}

func TestMVCCMismatchFails(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "MVCCMismatch", 1, `{"name":"Alice","age":30}`)

	entityID := createEntityAndGetID(t, srv.URL, "MVCCMismatch", 1, `{"name":"Bob","age":25}`)

	// Update with a stale/fake If-Match → should fail (412)
	staleTransactionID := uuid.New().String()
	url := fmt.Sprintf("%s/entity/JSON/%s/UPDATE", srv.URL, entityID)
	req, err := http.NewRequest(http.MethodPut, url, strings.NewReader(`{"name":"Carol","age":26}`))
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", staleTransactionID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	expectStatus(t, resp, http.StatusPreconditionFailed)
	commontest.ExpectErrorCode(t, resp, "ENTITY_MODIFIED")
	resp.Body.Close()
}

func TestMVCCAbsentSkips(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "MVCCAbsent", 1, `{"name":"Alice","age":30}`)

	entityID := createEntityAndGetID(t, srv.URL, "MVCCAbsent", 1, `{"name":"Bob","age":25}`)

	// Update without If-Match header → should succeed (200)
	resp := doUpdateEntity(t, srv.URL, "JSON", entityID, "UPDATE", `{"name":"Carol","age":26}`)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()
}

func TestGetEntityPointInTimeBothParamsRejected(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "BothParams", 1, `{"name":"Alice"}`)
	entityID := createEntityAndGetID(t, srv.URL, "BothParams", 1, `{"name":"Alice"}`)

	url := fmt.Sprintf("%s/entity/%s?pointInTime=%s&transactionId=00000000-0000-0000-0000-000000000001",
		srv.URL, entityID, time.Now().UTC().Format(time.RFC3339Nano))
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	expectStatus(t, resp, http.StatusBadRequest)
	resp.Body.Close()
}

// TestGetEntityByTransactionID verifies that GET /entity/{id}?transactionId=<tx>
// returns the entity envelope as it stood at that transaction — not the
// latest version. Issue #150: the handler previously parsed
// params.TransactionId but never propagated it, so the query parameter was
// silently dropped and the latest version was returned regardless.
func TestGetEntityByTransactionID(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "TxGet", 1, `{"k":1}`)

	// Create — capture the create transactionId.
	entityID := createEntityAndGetID(t, srv.URL, "TxGet", 1, `{"k":1}`)
	resp := doGetEntity(t, srv.URL, entityID)
	expectStatus(t, resp, http.StatusOK)
	body := readBody(t, resp)
	var envelope map[string]any
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode current envelope: %v", err)
	}
	meta := envelope["meta"].(map[string]any)
	createTxID, _ := meta["transactionId"].(string)
	if createTxID == "" {
		t.Fatalf("expected non-empty transactionId on freshly created entity")
	}

	// Update twice — k=1 → 2 → 3.
	resp = doUpdateEntity(t, srv.URL, "JSON", entityID, "UPDATE", `{"k":2}`)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()
	resp = doUpdateEntity(t, srv.URL, "JSON", entityID, "UPDATE", `{"k":3}`)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	// Sanity check: latest is k=3.
	resp = doGetEntity(t, srv.URL, entityID)
	expectStatus(t, resp, http.StatusOK)
	body = readBody(t, resp)
	var latestEnv map[string]any
	json.Unmarshal(body, &latestEnv)
	latestData := latestEnv["data"].(map[string]any)
	if v, _ := latestData["k"].(float64); v != 3 {
		t.Fatalf("sanity: latest k=%v, want 3", latestData["k"])
	}

	// GET with createTxID — must return the create-time snapshot k=1.
	resp = doGetEntityWithTransactionID(t, srv.URL, entityID, createTxID)
	expectStatus(t, resp, http.StatusOK)
	body = readBody(t, resp)
	var atTxEnv map[string]any
	if err := json.Unmarshal(body, &atTxEnv); err != nil {
		t.Fatalf("decode at-tx envelope: %v", err)
	}
	atTxData := atTxEnv["data"].(map[string]any)
	if v, _ := atTxData["k"].(float64); v != 1 {
		t.Errorf("GET ?transactionId=%s: k=%v, want 1 (create-time snapshot)", createTxID, atTxData["k"])
	}
	atTxMeta := atTxEnv["meta"].(map[string]any)
	if got, _ := atTxMeta["transactionId"].(string); got != createTxID {
		t.Errorf("at-tx envelope meta.transactionId: got %q, want %q", got, createTxID)
	}
}

// TestGetEntityByTransactionID_BogusReturns404 verifies that a transactionId
// that doesn't appear in the entity's version history yields 404
// ENTITY_NOT_FOUND. Issue #150 (dictionary 12/neg/05): cyoda-go previously
// returned HTTP 200 with the latest entity because the query parameter was
// dropped silently.
func TestGetEntityByTransactionID_BogusReturns404(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "TxGetBogus", 1, `{"k":1}`)
	entityID := createEntityAndGetID(t, srv.URL, "TxGetBogus", 1, `{"k":1}`)

	resp := doGetEntityWithTransactionID(t, srv.URL, entityID, "00000000-0000-0000-0000-000000000000")
	expectStatus(t, resp, http.StatusNotFound)
	body := readBody(t, resp)
	var apiErr struct {
		Properties struct {
			ErrorCode string `json:"errorCode"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(body, &apiErr); err != nil {
		t.Fatalf("decode error response: %v\nbody: %s", err, body)
	}
	if apiErr.Properties.ErrorCode != common.ErrCodeEntityNotFound {
		t.Errorf("expected errorCode %q, got %q (body: %s)",
			common.ErrCodeEntityNotFound, apiErr.Properties.ErrorCode, body)
	}
}

// TestGetEntityByTransactionID_NonExistentEntityReturns404 verifies that a
// transactionId-scoped GET on a non-existent entity yields 404
// ENTITY_NOT_FOUND (mirrors the latest-GET path).
func TestGetEntityByTransactionID_NonExistentEntityReturns404(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "TxGetMissing", 1, `{"k":1}`)
	bogusEntityID := uuid.NewString()

	resp := doGetEntityWithTransactionID(t, srv.URL, bogusEntityID, "00000000-0000-0000-0000-000000000001")
	expectStatus(t, resp, http.StatusNotFound)
	resp.Body.Close()
}

// --- SSI Transaction tests ---

func TestCreateTransaction(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "TxCreate", 1, `{"name":"Alice","age":30}`)

	// Create entity — should succeed with transaction committed.
	entityID := createEntityAndGetID(t, srv.URL, "TxCreate", 1, `{"name":"TxBob","age":25}`)

	// Entity should be visible (committed).
	resp := doGetEntity(t, srv.URL, entityID)
	expectStatus(t, resp, http.StatusOK)
	body := readBody(t, resp)

	var envelope map[string]any
	json.Unmarshal(body, &envelope)
	data := envelope["data"].(map[string]any)
	if data["name"] != "TxBob" {
		t.Errorf("expected data.name=TxBob, got %v", data["name"])
	}

	meta := envelope["meta"].(map[string]any)
	txID, ok := meta["transactionId"].(string)
	if !ok || txID == "" {
		t.Fatal("expected non-empty transactionId in committed entity")
	}

	// Verify transactionId is a valid UUID.
	if _, err := uuid.Parse(txID); err != nil {
		t.Errorf("transactionId %q is not a valid UUID: %v", txID, err)
	}
}

func TestDeleteSingleEntityTransaction(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "TxDelSingle", 1, `{"name":"Alice","age":30}`)

	entityID := createEntityAndGetID(t, srv.URL, "TxDelSingle", 1, `{"name":"Alice","age":30}`)

	// Delete entity — wrapped in SSI transaction.
	resp := doDeleteEntity(t, srv.URL, entityID)
	expectStatus(t, resp, http.StatusOK)

	deleteBody := readBody(t, resp)
	var deleteResp map[string]any
	json.Unmarshal(deleteBody, &deleteResp)
	txID, ok := deleteResp["transactionId"].(string)
	if !ok || txID == "" {
		t.Fatal("expected non-empty transactionId in delete response")
	}
	if _, err := uuid.Parse(txID); err != nil {
		t.Errorf("transactionId %q is not a valid UUID: %v", txID, err)
	}

	// Verify entity is gone.
	resp = doGetEntity(t, srv.URL, entityID)
	expectStatus(t, resp, http.StatusNotFound)
	resp.Body.Close()
}

func TestUpdateSingleEntityTransaction(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "TxUpdate", 1, `{"name":"Alice","age":30}`)

	entityID := createEntityAndGetID(t, srv.URL, "TxUpdate", 1, `{"name":"Original","age":1}`)

	// Update entity — wrapped in SSI transaction (uses default workflow UPDATE transition).
	resp := doUpdateEntity(t, srv.URL, "JSON", entityID, "UPDATE", `{"name":"Updated","age":99}`)
	expectStatus(t, resp, http.StatusOK)

	updateBody := readBody(t, resp)
	var updateResp map[string]any
	json.Unmarshal(updateBody, &updateResp)
	txID, ok := updateResp["transactionId"].(string)
	if !ok || txID == "" {
		t.Fatal("expected non-empty transactionId in update response")
	}

	// Verify entity was updated.
	resp = doGetEntity(t, srv.URL, entityID)
	expectStatus(t, resp, http.StatusOK)
	body := readBody(t, resp)
	var envelope map[string]any
	json.Unmarshal(body, &envelope)
	data := envelope["data"].(map[string]any)
	if data["name"] != "Updated" {
		t.Errorf("expected data.name=Updated, got %v", data["name"])
	}
}

func TestBatchDeleteTransaction(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "TxBatchDel", 1, `{"name":"Alice","age":30}`)

	for i := 0; i < 3; i++ {
		body := fmt.Sprintf(`{"name":"Person%d","age":%d}`, i, 20+i)
		createEntityAndGetID(t, srv.URL, "TxBatchDel", 1, body)
	}

	// Batch delete all — wrapped in SSI transaction.
	resp := doDeleteEntities(t, srv.URL, "TxBatchDel", 1)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	// Verify GetAll returns empty.
	resp = doGetAllEntities(t, srv.URL, "TxBatchDel", 1, 20, 0)
	expectStatus(t, resp, http.StatusOK)
	body := readBody(t, resp)
	if strings.TrimSpace(string(body)) != "[]" {
		t.Fatalf("expected empty array after batch delete, got %s", string(body))
	}
}

// TestCreateEntity_IncompatibleType_ReturnsSpecificCode asserts the full
// HTTP stack (handler → AppError → RFC 9457 problem-detail body) emits
// HTTP 400 with `errorCode: "INCOMPATIBLE_TYPE"` and the structured Props
// (`fieldPath`, `expectedType`, `actualType`, `entityName`,
// `entityVersion`) when an entity payload's leaf value type is not
// assignable to the schema's declared DataType.
//
// Closes #129. Cloud equivalent:
// FoundIncompatibleTypeWithEntityModelException.
func TestCreateEntity_IncompatibleType_ReturnsSpecificCode(t *testing.T) {
	srv := newTestServer(t)
	// Sample model infers price as INTEGER.
	importAndLockModel(t, srv.URL, "IncompatibleTypeTest", 1, `{"price":13}`)

	// Submit a DOUBLE — incompatible with the locked INTEGER schema (no
	// changeLevel, no widening).
	resp := doCreateEntity(t, srv.URL, "JSON", "IncompatibleTypeTest", 1, `{"price":13.111}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: got %d, want 400; body: %s", resp.StatusCode, string(body))
	}
	commontest.ExpectErrorCode(t, resp, common.ErrCodeIncompatibleType)

	// Decode and assert structured Props.
	body, _ := io.ReadAll(resp.Body)
	var pd struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(body, &pd); err != nil {
		t.Fatalf("decode problem detail: %v; body: %s", err, string(body))
	}
	if got := pd.Properties["fieldPath"]; got != "price" {
		t.Errorf("properties.fieldPath: got %v, want %q", got, "price")
	}
	if got := pd.Properties["actualType"]; got != "DOUBLE" {
		t.Errorf("properties.actualType: got %v, want %q", got, "DOUBLE")
	}
	expectedAny, ok := pd.Properties["expectedType"].([]any)
	if !ok || len(expectedAny) != 1 || expectedAny[0] != "INTEGER" {
		t.Errorf("properties.expectedType: got %v, want [\"INTEGER\"]", pd.Properties["expectedType"])
	}
	if got := pd.Properties["entityName"]; got != "IncompatibleTypeTest" {
		t.Errorf("properties.entityName: got %v, want %q", got, "IncompatibleTypeTest")
	}
	if got := pd.Properties["entityVersion"]; got != "1" {
		t.Errorf("properties.entityVersion: got %v, want %q", got, "1")
	}
}
