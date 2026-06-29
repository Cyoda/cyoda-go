package model_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/app"
	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// newTestServer creates an App with default config and returns an httptest.Server.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	cfg := app.DefaultConfig()
	cfg.ContextPath = ""
	a := app.New(cfg)
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	return srv
}

// newTestServerWithTenant creates an App wired to a specific tenant.
func newTestServerWithTenant(t *testing.T, tenantID, tenantName string) *httptest.Server {
	t.Helper()
	cfg := app.DefaultConfig()
	cfg.ContextPath = ""
	cfg.IAM.MockTenantID = tenantID
	cfg.IAM.MockTenantName = tenantName
	a := app.New(cfg)
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	return srv
}

const sampleJSON = `{"name":"Alice","age":30,"active":true}`
const sampleJSON2 = `{"name":"Bob","score":99.5}`

func doImport(t *testing.T, base, entityName string, version int, body string) *http.Response {
	t.Helper()
	url := base + "/model/import/JSON/SAMPLE_DATA/" + entityName + "/" + strconv.Itoa(version)
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("import request failed: %v", err)
	}
	return resp
}

func doExport(t *testing.T, base, converter, entityName string, version int) *http.Response {
	t.Helper()
	url := base + "/model/export/" + converter + "/" + entityName + "/" + strconv.Itoa(version)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("export request failed: %v", err)
	}
	return resp
}

func doLock(t *testing.T, base, entityName string, version int) *http.Response {
	t.Helper()
	url := base + "/model/" + entityName + "/" + strconv.Itoa(version) + "/lock"
	req, _ := http.NewRequest(http.MethodPut, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("lock request failed: %v", err)
	}
	return resp
}

func doUnlock(t *testing.T, base, entityName string, version int) *http.Response {
	t.Helper()
	url := base + "/model/" + entityName + "/" + strconv.Itoa(version) + "/unlock"
	req, _ := http.NewRequest(http.MethodPut, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("unlock request failed: %v", err)
	}
	return resp
}

func doDelete(t *testing.T, base, entityName string, version int) *http.Response {
	t.Helper()
	url := base + "/model/" + entityName + "/" + strconv.Itoa(version)
	req, _ := http.NewRequest(http.MethodDelete, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete request failed: %v", err)
	}
	return resp
}

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

func doGetAll(t *testing.T, base string) *http.Response {
	t.Helper()
	resp, err := http.Get(base + "/model/")
	if err != nil {
		t.Fatalf("getAll request failed: %v", err)
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

func ctxWithTenant(tid spi.TenantID) context.Context {
	uc := &spi.UserContext{
		UserID: "test-user",
		Tenant: spi.Tenant{ID: tid, Name: string(tid)},
		Roles:  []string{"USER"},
	}
	return spi.WithUserContext(context.Background(), uc)
}

func TestNewHandler(t *testing.T) {
	h := model.New(nil)
	if h == nil {
		t.Fatal("expected non-nil handler")
	}
}

func TestImportAndExportJSONSchema(t *testing.T) {
	srv := newTestServer(t)

	resp := doImport(t, srv.URL, "Person", 1, sampleJSON)
	expectStatus(t, resp, http.StatusOK)

	var modelID string
	json.Unmarshal(readBody(t, resp), &modelID)
	if modelID == "" {
		t.Fatalf("expected non-empty model ID string, got empty")
	}

	// Export as JSON_SCHEMA
	resp = doExport(t, srv.URL, "JSON_SCHEMA", "Person", 1)
	expectStatus(t, resp, http.StatusOK)

	body := readBody(t, resp)
	var schema map[string]any
	if err := json.Unmarshal(body, &schema); err != nil {
		t.Fatalf("failed to parse export: %v", err)
	}
	if schema["currentState"] != "UNLOCKED" {
		t.Errorf("expected currentState=UNLOCKED, got %v", schema["currentState"])
	}
	model, ok := schema["model"].(map[string]any)
	if !ok {
		t.Fatal("expected 'model' envelope in JSON_SCHEMA export")
	}
	if model["type"] != "object" {
		t.Errorf("expected root type=object, got %v", model["type"])
	}
	props, ok := model["properties"].(map[string]any)
	if !ok {
		t.Fatal("expected properties map in schema")
	}
	for _, field := range []string{"name", "age", "active"} {
		if _, ok := props[field]; !ok {
			t.Errorf("expected '%s' in schema properties", field)
		}
	}
}

func TestImportAndExportSimpleView(t *testing.T) {
	srv := newTestServer(t)

	resp := doImport(t, srv.URL, "Person", 1, sampleJSON)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	resp = doExport(t, srv.URL, "SIMPLE_VIEW", "Person", 1)
	expectStatus(t, resp, http.StatusOK)

	body := readBody(t, resp)
	var sv map[string]any
	if err := json.Unmarshal(body, &sv); err != nil {
		t.Fatalf("failed to parse simple view: %v", err)
	}
	if sv["currentState"] != "UNLOCKED" {
		t.Errorf("expected currentState=UNLOCKED, got %v", sv["currentState"])
	}
	if _, ok := sv["model"]; !ok {
		t.Error("expected 'model' key in simple view")
	}
}

func TestSuccessiveImportsMerge(t *testing.T) {
	srv := newTestServer(t)

	resp := doImport(t, srv.URL, "MergeTest", 1, sampleJSON)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	resp = doImport(t, srv.URL, "MergeTest", 1, sampleJSON2)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	resp = doExport(t, srv.URL, "JSON_SCHEMA", "MergeTest", 1)
	expectStatus(t, resp, http.StatusOK)

	body := readBody(t, resp)
	var schema map[string]any
	json.Unmarshal(body, &schema)
	model := schema["model"].(map[string]any)
	props := model["properties"].(map[string]any)

	for _, field := range []string{"name", "age", "active", "score"} {
		if _, ok := props[field]; !ok {
			t.Errorf("expected '%s' in merged schema properties", field)
		}
	}
}

func TestLockModel(t *testing.T) {
	srv := newTestServer(t)

	resp := doImport(t, srv.URL, "LockTest", 1, sampleJSON)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	resp = doLock(t, srv.URL, "LockTest", 1)
	expectStatus(t, resp, http.StatusOK)

	var result map[string]any
	json.Unmarshal(readBody(t, resp), &result)
	if result["success"] != true {
		t.Fatalf("expected success=true on lock")
	}

	// Verify locked via GetAll
	resp = doGetAll(t, srv.URL)
	expectStatus(t, resp, http.StatusOK)
	body := readBody(t, resp)
	var models []map[string]any
	json.Unmarshal(body, &models)

	found := false
	for _, m := range models {
		if m["modelName"] == "LockTest" {
			found = true
			if m["currentState"] != "LOCKED" {
				t.Errorf("expected LOCKED state, got %v", m["currentState"])
			}
		}
	}
	if !found {
		t.Error("LockTest model not found in GetAll")
	}
}

func TestUnlockModel(t *testing.T) {
	srv := newTestServer(t)

	resp := doImport(t, srv.URL, "UnlockTest", 1, sampleJSON)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	resp = doLock(t, srv.URL, "UnlockTest", 1)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	resp = doUnlock(t, srv.URL, "UnlockTest", 1)
	expectStatus(t, resp, http.StatusOK)

	var result map[string]any
	json.Unmarshal(readBody(t, resp), &result)
	if result["success"] != true {
		t.Fatalf("expected success=true on unlock")
	}
}

func TestDeleteUnlockedEmptyModel(t *testing.T) {
	srv := newTestServer(t)

	resp := doImport(t, srv.URL, "DeleteTest", 1, sampleJSON)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	resp = doDelete(t, srv.URL, "DeleteTest", 1)
	expectStatus(t, resp, http.StatusOK)

	var result map[string]any
	json.Unmarshal(readBody(t, resp), &result)
	if result["success"] != true {
		t.Fatalf("expected success=true on delete")
	}

	// Verify 404 on subsequent export
	resp = doExport(t, srv.URL, "JSON_SCHEMA", "DeleteTest", 1)
	expectStatus(t, resp, http.StatusNotFound)
	resp.Body.Close()
}

func TestDeleteLockedEmptyModelSucceeds(t *testing.T) {
	srv := newTestServer(t)

	resp := doImport(t, srv.URL, "LockedDel", 1, sampleJSON)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	resp = doLock(t, srv.URL, "LockedDel", 1)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	// Locked model with no entities should be deletable.
	resp = doDelete(t, srv.URL, "LockedDel", 1)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()
}

func TestImportRejectedOnLockedModel(t *testing.T) {
	srv := newTestServer(t)

	resp := doImport(t, srv.URL, "LockedImport", 1, sampleJSON)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	resp = doLock(t, srv.URL, "LockedImport", 1)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	resp = doImport(t, srv.URL, "LockedImport", 1, sampleJSON2)
	expectStatus(t, resp, http.StatusConflict)
	resp.Body.Close()
}

func TestLockAlreadyLocked(t *testing.T) {
	srv := newTestServer(t)

	resp := doImport(t, srv.URL, "DoubleLock", 1, sampleJSON)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	resp = doLock(t, srv.URL, "DoubleLock", 1)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	resp = doLock(t, srv.URL, "DoubleLock", 1)
	expectStatus(t, resp, http.StatusConflict)
	resp.Body.Close()
}

func TestUnlockNotLocked(t *testing.T) {
	srv := newTestServer(t)

	resp := doImport(t, srv.URL, "NotLocked", 1, sampleJSON)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	resp = doUnlock(t, srv.URL, "NotLocked", 1)
	expectStatus(t, resp, http.StatusConflict)
	resp.Body.Close()
}

func TestLockNotFound(t *testing.T) {
	srv := newTestServer(t)
	resp := doLock(t, srv.URL, "NoSuchModel", 1)
	expectStatus(t, resp, http.StatusNotFound)
	resp.Body.Close()
}

func TestExportNotFound(t *testing.T) {
	srv := newTestServer(t)
	resp := doExport(t, srv.URL, "JSON_SCHEMA", "NoSuchModel", 1)
	expectStatus(t, resp, http.StatusNotFound)
	resp.Body.Close()
}

func TestChangeLevelStored(t *testing.T) {
	srv := newTestServer(t)

	resp := doImport(t, srv.URL, "CLTest", 1, sampleJSON)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	resp = doChangeLevel(t, srv.URL, "CLTest", 1, "STRUCTURAL")
	expectStatus(t, resp, http.StatusOK)

	var result map[string]any
	json.Unmarshal(readBody(t, resp), &result)
	if result["success"] != true {
		t.Fatalf("expected success=true on change level")
	}
}

func TestChangeLevelNotFound(t *testing.T) {
	srv := newTestServer(t)
	resp := doChangeLevel(t, srv.URL, "NoModel", 1, "STRUCTURAL")
	expectStatus(t, resp, http.StatusNotFound)
	resp.Body.Close()
}

func TestChangeLevelInvalidValue(t *testing.T) {
	srv := newTestServer(t)

	resp := doImport(t, srv.URL, "CLInvalid", 1, sampleJSON)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	resp = doChangeLevel(t, srv.URL, "CLInvalid", 1, "INVALID_LEVEL")
	expectStatus(t, resp, http.StatusBadRequest)
	commontest.ExpectErrorCode(t, resp, "INVALID_CHANGE_LEVEL")
	resp.Body.Close()
}

// TestChangeLevelInvalidValue_PropsCarryStructuredDetail verifies that the
// invalid-enum response carries entityName, entityVersion, suppliedValue, and
// validValues in the problem-detail properties, so callers can branch on the
// precondition without scraping the message string.
func TestChangeLevelInvalidValue_PropsCarryStructuredDetail(t *testing.T) {
	srv := newTestServer(t)

	resp := doImport(t, srv.URL, "CLProps", 1, sampleJSON)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	resp = doChangeLevel(t, srv.URL, "CLProps", 1, "wrong")
	defer resp.Body.Close()
	expectStatus(t, resp, http.StatusBadRequest)

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var pd struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(body, &pd); err != nil {
		t.Fatalf("decode problem detail: %v; body: %s", err, string(body))
	}
	if got, _ := pd.Properties["errorCode"].(string); got != "INVALID_CHANGE_LEVEL" {
		t.Errorf("errorCode: want INVALID_CHANGE_LEVEL, got %q", got)
	}
	if got, _ := pd.Properties["entityName"].(string); got != "CLProps" {
		t.Errorf("entityName: want CLProps, got %q", got)
	}
	// JSON numbers decode as float64.
	if got, _ := pd.Properties["entityVersion"].(float64); got != 1 {
		t.Errorf("entityVersion: want 1, got %v", got)
	}
	if got, _ := pd.Properties["suppliedValue"].(string); got != "wrong" {
		t.Errorf("suppliedValue: want %q, got %q", "wrong", got)
	}
	validValuesRaw, ok := pd.Properties["validValues"].([]any)
	if !ok {
		t.Fatalf("validValues: want []any, got %T (body: %s)", pd.Properties["validValues"], string(body))
	}
	got := make(map[string]bool, len(validValuesRaw))
	for _, v := range validValuesRaw {
		s, _ := v.(string)
		got[s] = true
	}
	for _, want := range []string{"ARRAY_LENGTH", "ARRAY_ELEMENTS", "TYPE", "STRUCTURAL"} {
		if !got[want] {
			t.Errorf("validValues missing %q; got %v", want, validValuesRaw)
		}
	}
}

func TestGetAvailableEntityModels(t *testing.T) {
	srv := newTestServer(t)

	resp := doImport(t, srv.URL, "ModelA", 1, sampleJSON)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	resp = doImport(t, srv.URL, "ModelB", 2, sampleJSON)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	resp = doGetAll(t, srv.URL)
	expectStatus(t, resp, http.StatusOK)
	body := readBody(t, resp)

	var models []map[string]any
	if err := json.Unmarshal(body, &models); err != nil {
		t.Fatalf("failed to parse models list: %v", err)
	}

	if len(models) < 2 {
		t.Fatalf("expected at least 2 models, got %d", len(models))
	}

	for _, m := range models {
		for _, field := range []string{"modelName", "currentState", "id"} {
			if _, ok := m[field]; !ok {
				t.Errorf("missing %s in model DTO", field)
			}
		}
	}
}

func TestResponseDTOShape(t *testing.T) {
	srv := newTestServer(t)

	// Import returns a bare UUID string.
	resp := doImport(t, srv.URL, "ShapeTest", 1, sampleJSON)
	expectStatus(t, resp, http.StatusOK)
	body := readBody(t, resp)

	var modelID string
	if err := json.Unmarshal(body, &modelID); err != nil {
		t.Fatalf("failed to parse import result as string: %v", err)
	}
	if modelID == "" {
		t.Error("expected non-empty model ID string")
	}

	// Lock returns an EntityModelActionResultDto with message containing model name:version.
	resp = doLock(t, srv.URL, "ShapeTest", 1)
	expectStatus(t, resp, http.StatusOK)
	body = readBody(t, resp)

	var lockResult map[string]any
	if err := json.Unmarshal(body, &lockResult); err != nil {
		t.Fatalf("failed to parse lock result: %v", err)
	}
	if lockResult["success"] != true {
		t.Error("expected success=true in lock result")
	}
	msg, _ := lockResult["message"].(string)
	if msg == "" {
		t.Error("expected non-empty message in lock result")
	}
	mk, ok := lockResult["modelKey"].(map[string]any)
	if !ok {
		t.Fatal("expected modelKey object in lock result")
	}
	if mk["name"] != "ShapeTest" {
		t.Errorf("expected modelKey.name=ShapeTest, got %v", mk["name"])
	}
	if mk["version"] != float64(1) {
		t.Errorf("expected modelKey.version=1, got %v", mk["version"])
	}
}

func TestUnsupportedConverter(t *testing.T) {
	srv := newTestServer(t)

	url := srv.URL + "/model/import/JSON/JSON_SCHEMA/TestEntity/1"
	resp, err := http.Post(url, "application/json", strings.NewReader(sampleJSON))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	expectStatus(t, resp, http.StatusBadRequest)
	resp.Body.Close()
}

// newTestApp creates an App with default config and returns it along with an httptest.Server.
func newTestApp(t *testing.T) (*app.App, *httptest.Server) {
	t.Helper()
	cfg := app.DefaultConfig()
	cfg.ContextPath = ""
	a := app.New(cfg)
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	return a, srv
}

func TestUnlockBlockedByEntities(t *testing.T) {
	a, srv := newTestApp(t)

	// Import and lock model.
	resp := doImport(t, srv.URL, "UnlockGuard", 1, sampleJSON)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	resp = doLock(t, srv.URL, "UnlockGuard", 1)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	// Save an entity directly via the store.
	ctx := ctxWithTenant("mock-tenant")
	entityStore, err := a.StoreFactory().EntityStore(ctx)
	if err != nil {
		t.Fatalf("failed to get entity store: %v", err)
	}

	entity := &spi.Entity{
		Meta: spi.EntityMeta{
			TenantID: "mock-tenant",
			ModelRef: spi.ModelRef{EntityName: "UnlockGuard", ModelVersion: "1"},
			State:    "NEW",
		},
		Data: []byte(`{"x":1}`),
	}
	if _, err := entityStore.Save(ctx, entity); err != nil {
		t.Fatalf("failed to save entity: %v", err)
	}

	// Attempt unlock — should be blocked with 409 + MODEL_HAS_ENTITIES code.
	resp = doUnlock(t, srv.URL, "UnlockGuard", 1)
	expectStatus(t, resp, http.StatusConflict)
	commontest.ExpectErrorCode(t, resp, "MODEL_HAS_ENTITIES")
	resp.Body.Close()
}

func TestDeleteBlockedByEntities(t *testing.T) {
	a, srv := newTestApp(t)

	// Import model (stays unlocked).
	resp := doImport(t, srv.URL, "DeleteGuard", 1, sampleJSON)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	// Save an entity directly via the store.
	ctx := ctxWithTenant("mock-tenant")
	entityStore, err := a.StoreFactory().EntityStore(ctx)
	if err != nil {
		t.Fatalf("failed to get entity store: %v", err)
	}

	entity := &spi.Entity{
		Meta: spi.EntityMeta{
			TenantID: "mock-tenant",
			ModelRef: spi.ModelRef{EntityName: "DeleteGuard", ModelVersion: "1"},
			State:    "NEW",
		},
		Data: []byte(`{"x":1}`),
	}
	if _, err := entityStore.Save(ctx, entity); err != nil {
		t.Fatalf("failed to save entity: %v", err)
	}

	// Attempt delete — should be blocked with 409 + MODEL_HAS_ENTITIES code.
	resp = doDelete(t, srv.URL, "DeleteGuard", 1)
	expectStatus(t, resp, http.StatusConflict)
	commontest.ExpectErrorCode(t, resp, "MODEL_HAS_ENTITIES")
	resp.Body.Close()
}

func TestDeleteSucceedsAfterEntitiesDeleted(t *testing.T) {
	a, srv := newTestApp(t)

	// Import and lock model.
	resp := doImport(t, srv.URL, "DeleteAfterPurge", 1, sampleJSON)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	resp = doLock(t, srv.URL, "DeleteAfterPurge", 1)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	// Save an entity directly via the store.
	ctx := ctxWithTenant("mock-tenant")
	entityStore, err := a.StoreFactory().EntityStore(ctx)
	if err != nil {
		t.Fatalf("failed to get entity store: %v", err)
	}

	entity := &spi.Entity{
		Meta: spi.EntityMeta{
			ID:       "delete-purge-entity-1",
			TenantID: "mock-tenant",
			ModelRef: spi.ModelRef{EntityName: "DeleteAfterPurge", ModelVersion: "1"},
			State:    "CREATED",
		},
		Data: []byte(`{"x":1}`),
	}
	if _, err := entityStore.Save(ctx, entity); err != nil {
		t.Fatalf("failed to save entity: %v", err)
	}

	// Delete should be blocked — entity exists.
	resp = doDelete(t, srv.URL, "DeleteAfterPurge", 1)
	expectStatus(t, resp, http.StatusConflict)
	resp.Body.Close()

	// Soft-delete the entity.
	if err := entityStore.Delete(ctx, "delete-purge-entity-1"); err != nil {
		t.Fatalf("failed to delete entity: %v", err)
	}

	// Now delete should succeed — only deleted entities remain.
	resp = doDelete(t, srv.URL, "DeleteAfterPurge", 1)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()
}

func TestExportUnsupportedConverter(t *testing.T) {
	srv := newTestServer(t)

	// Import a model first so it exists.
	resp := doImport(t, srv.URL, "ExpConv", 1, sampleJSON)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	// Export with unsupported converter.
	resp = doExport(t, srv.URL, "INVALID", "ExpConv", 1)
	expectStatus(t, resp, http.StatusBadRequest)
	resp.Body.Close()
}

func TestTenantIsolation(t *testing.T) {
	factory := memory.NewStoreFactory()

	ctxA := ctxWithTenant("tenant-A")
	ctxB := ctxWithTenant("tenant-B")

	modelStoreA, err := factory.ModelStore(ctxA)
	if err != nil {
		t.Fatalf("failed to get model store for tenant-A: %v", err)
	}
	modelStoreB, err := factory.ModelStore(ctxB)
	if err != nil {
		t.Fatalf("failed to get model store for tenant-B: %v", err)
	}

	ref := spi.ModelRef{EntityName: "Shared", ModelVersion: "1"}
	desc := &spi.ModelDescriptor{
		Ref:    ref,
		State:  spi.ModelUnlocked,
		Schema: []byte(`{"kind":"OBJECT"}`),
	}

	if err := modelStoreA.Save(ctxA, desc); err != nil {
		t.Fatalf("save failed: %v", err)
	}

	_, err = modelStoreB.Get(ctxB, ref)
	if err == nil {
		t.Error("tenant-B should not see tenant-A's model")
	}

	got, err := modelStoreA.Get(ctxA, ref)
	if err != nil {
		t.Fatalf("tenant-A should see its own model: %v", err)
	}
	if got.Ref.EntityName != "Shared" {
		t.Errorf("unexpected entity name: %s", got.Ref.EntityName)
	}
}

func doValidate(t *testing.T, base, entityName string, version int, body string) *http.Response {
	t.Helper()
	url := base + "/model/validate/" + entityName + "/" + strconv.Itoa(version)
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("validate request failed: %v", err)
	}
	return resp
}

func TestValidateConformingData(t *testing.T) {
	srv := newTestServer(t)

	// Import model from sample data.
	resp := doImport(t, srv.URL, "ValOK", 1, `{"name":"Alice","age":30}`)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	// Validate conforming data.
	resp = doValidate(t, srv.URL, "ValOK", 1, `{"name":"Bob","age":25}`)
	expectStatus(t, resp, http.StatusOK)

	var result map[string]any
	json.Unmarshal(readBody(t, resp), &result)
	if result["success"] != true {
		t.Fatalf("expected success=true, got %v; message: %v", result["success"], result["message"])
	}
}

func TestValidateNonConformingData(t *testing.T) {
	srv := newTestServer(t)

	// Import model from sample data.
	resp := doImport(t, srv.URL, "ValBad", 1, `{"name":"Alice","age":30}`)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	// Validate non-conforming data (name should be string, age should be number).
	resp = doValidate(t, srv.URL, "ValBad", 1, `{"name":123,"age":"wrong"}`)
	expectStatus(t, resp, http.StatusOK)

	var result map[string]any
	json.Unmarshal(readBody(t, resp), &result)
	if result["success"] != false {
		t.Fatalf("expected success=false, got %v", result["success"])
	}
	msg, _ := result["message"].(string)
	if msg == "" {
		t.Fatal("expected non-empty message describing validation errors")
	}
}

func TestValidateModelNotFound(t *testing.T) {
	srv := newTestServer(t)

	resp := doValidate(t, srv.URL, "NoSuchModel", 1, `{"foo":"bar"}`)
	expectStatus(t, resp, http.StatusNotFound)
	resp.Body.Close()
}

func TestImportBodySizeLimit(t *testing.T) {
	srv := newTestServer(t)

	// Create a body larger than the 10MB limit.
	bigBody := strings.Repeat("x", 11*1024*1024)
	url := srv.URL + "/model/import/JSON/SAMPLE_DATA/BigTest/1"
	resp, err := http.Post(url, "application/json", strings.NewReader(bigBody))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	// Should get a 400 (MaxBytesReader triggers a read error caught as bad request).
	if resp.StatusCode == 200 {
		t.Errorf("expected rejection for oversized body, got 200")
	}
}

func doSetUniqueKeys(t *testing.T, base, entityName string, version int, body string) *http.Response {
	t.Helper()
	url := base + "/model/" + entityName + "/" + strconv.Itoa(version) + "/unique-keys"
	req, _ := http.NewRequest(http.MethodPut, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("setUniqueKeys request failed: %v", err)
	}
	return resp
}

func TestSetUniqueKeys_200_Valid(t *testing.T) {
	srv := newTestServer(t)

	resp := doImport(t, srv.URL, "UKTest", 1, sampleJSON)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	body := `{"uniqueKeys": [{"id": "uk1", "fields": ["$.name"]}]}`
	resp = doSetUniqueKeys(t, srv.URL, "UKTest", 1, body)
	expectStatus(t, resp, http.StatusOK)

	var result map[string]any
	json.Unmarshal(readBody(t, resp), &result)
	if result["success"] != true {
		t.Fatalf("expected success=true, got %v", result["success"])
	}
}

func TestSetUniqueKeys_409_Locked(t *testing.T) {
	srv := newTestServer(t)

	resp := doImport(t, srv.URL, "UKLocked", 1, sampleJSON)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	resp = doLock(t, srv.URL, "UKLocked", 1)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	body := `{"uniqueKeys": [{"id": "uk1", "fields": ["$.name"]}]}`
	resp = doSetUniqueKeys(t, srv.URL, "UKLocked", 1, body)
	expectStatus(t, resp, http.StatusConflict)
	commontest.ExpectErrorCode(t, resp, "MODEL_ALREADY_LOCKED")
}

func TestSetUniqueKeys_422_BadField(t *testing.T) {
	srv := newTestServer(t)

	resp := doImport(t, srv.URL, "UKBadField", 1, sampleJSON)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	body := `{"uniqueKeys": [{"id": "uk1", "fields": ["$.nonexistent_field_xyz"]}]}`
	resp = doSetUniqueKeys(t, srv.URL, "UKBadField", 1, body)
	expectStatus(t, resp, http.StatusUnprocessableEntity)
	commontest.ExpectErrorCode(t, resp, "INVALID_UNIQUE_KEY_DEFINITION")
}

func TestSetUniqueKeys_422_Unsupported(t *testing.T) {
	// incapableFactory embeds spi.StoreFactory as an interface so the
	// concrete SupportsCompositeUniqueKeys method on the real factory is NOT
	// promoted — the type assertion in SetUniqueKeys returns ok=false.
	type incapableFactory struct{ spi.StoreFactory }
	h := model.New(incapableFactory{memory.NewStoreFactory()})

	body := `{"uniqueKeys": [{"id": "uk1", "fields": ["$.name"]}]}`
	r := httptest.NewRequest(http.MethodPut, "/model/UKUnsupported/1/unique-keys", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	h.SetEntityModelUniqueKeys(w, r, "UKUnsupported", 1)

	resp := w.Result()
	expectStatus(t, resp, http.StatusUnprocessableEntity)
	commontest.ExpectErrorCode(t, resp, "COMPOSITE_KEY_UNSUPPORTED")
}

func TestExportIncludesUniqueKeys(t *testing.T) {
	srv := newTestServer(t)

	resp := doImport(t, srv.URL, "UKExport", 1, sampleJSON)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	body := `{"uniqueKeys": [{"id": "uk1", "fields": ["$.name"]}]}`
	resp = doSetUniqueKeys(t, srv.URL, "UKExport", 1, body)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	resp = doExport(t, srv.URL, "JSON_SCHEMA", "UKExport", 1)
	expectStatus(t, resp, http.StatusOK)

	var exported map[string]any
	json.Unmarshal(readBody(t, resp), &exported)
	uks, ok := exported["uniqueKeys"]
	if !ok {
		t.Fatal("expected 'uniqueKeys' field in export output")
	}
	arr, ok := uks.([]any)
	if !ok || len(arr) != 1 {
		t.Fatalf("expected 1 unique key in export, got %v", uks)
	}
}

func TestTenantIsolationViaHTTP(t *testing.T) {
	srvA := newTestServerWithTenant(t, "tenant-A", "Tenant A")
	srvB := newTestServerWithTenant(t, "tenant-B", "Tenant B")

	resp := doImport(t, srvA.URL, "Isolated", 1, sampleJSON)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	resp = doGetAll(t, srvB.URL)
	expectStatus(t, resp, http.StatusOK)
	body := readBody(t, resp)
	var models []map[string]any
	json.Unmarshal(body, &models)
	if len(models) != 0 {
		t.Errorf("expected 0 models for tenant-B, got %d", len(models))
	}
}
