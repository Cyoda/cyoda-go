package audit_test

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

	"github.com/cyoda-platform/cyoda-go/app"

	_ "github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// --- helpers ---

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

func createEntityAndGetID(t *testing.T, base, entityName string, version int, body string) string {
	t.Helper()
	url := base + "/entity/JSON/" + entityName + "/" + strconv.Itoa(version)
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("create entity request failed: %v", err)
	}
	expectStatus(t, resp, http.StatusOK)
	b := readBody(t, resp)
	var arr []map[string]any
	json.Unmarshal(b, &arr)
	cr := arr[0]
	ids := cr["entityIds"].([]any)
	return ids[0].(string)
}

func createEntityAndGetIDAndTxID(t *testing.T, base, entityName string, version int, body string) (string, string) {
	t.Helper()
	url := base + "/entity/JSON/" + entityName + "/" + strconv.Itoa(version)
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("create entity request failed: %v", err)
	}
	expectStatus(t, resp, http.StatusOK)
	b := readBody(t, resp)
	var arr []map[string]any
	json.Unmarshal(b, &arr)
	cr := arr[0]
	ids := cr["entityIds"].([]any)
	txID := cr["transactionId"].(string)
	return ids[0].(string), txID
}

func updateEntity(t *testing.T, base, entityID, body string) string {
	t.Helper()
	url := fmt.Sprintf("%s/entity/JSON/%s/UPDATE", base, entityID)
	req, err := http.NewRequest(http.MethodPut, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("failed to create update request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("update entity request failed: %v", err)
	}
	expectStatus(t, resp, http.StatusOK)
	b := readBody(t, resp)
	var result map[string]any
	json.Unmarshal(b, &result)
	return result["transactionId"].(string)
}

func deleteEntity(t *testing.T, base, entityID string) {
	t.Helper()
	url := fmt.Sprintf("%s/entity/%s", base, entityID)
	req, _ := http.NewRequest(http.MethodDelete, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete entity request failed: %v", err)
	}
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()
}

func getAuditEvents(t *testing.T, base, entityID string, queryParams ...string) ([]map[string]any, map[string]any) {
	t.Helper()
	url := fmt.Sprintf("%s/audit/entity/%s", base, entityID)
	if len(queryParams) > 0 {
		url += "?" + strings.Join(queryParams, "&")
	}
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("audit request failed: %v", err)
	}
	expectStatus(t, resp, http.StatusOK)
	b := readBody(t, resp)
	var result map[string]any
	if err := json.Unmarshal(b, &result); err != nil {
		t.Fatalf("failed to parse audit response: %v (body: %s)", err, string(b))
	}
	items := result["items"].([]any)
	events := make([]map[string]any, len(items))
	for i, item := range items {
		events[i] = item.(map[string]any)
	}
	pagination := result["pagination"].(map[string]any)
	return events, pagination
}

func getAuditEventsRaw(t *testing.T, base, entityID string, queryParams ...string) *http.Response {
	t.Helper()
	url := fmt.Sprintf("%s/audit/entity/%s", base, entityID)
	if len(queryParams) > 0 {
		url += "?" + strings.Join(queryParams, "&")
	}
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("audit request failed: %v", err)
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

// --- tests ---

func TestAuditAfterCreate(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "AuditCreate", 1, `{"name":"Alice"}`)

	entityID := createEntityAndGetID(t, srv.URL, "AuditCreate", 1, `{"name":"Bob"}`)

	events, pagination := getAuditEvents(t, srv.URL, entityID, "eventType=EntityChange")

	if len(events) != 1 {
		t.Fatalf("expected 1 audit event, got %d", len(events))
	}

	ev := events[0]
	if ev["auditEventType"] != "EntityChange" {
		t.Errorf("expected auditEventType=EntityChange, got %v", ev["auditEventType"])
	}
	if ev["changeType"] != "CREATE" {
		t.Errorf("expected changeType=CREATE, got %v", ev["changeType"])
	}
	if ev["severity"] != "INFO" {
		t.Errorf("expected severity=INFO, got %v", ev["severity"])
	}

	// Actor should be present
	actor, ok := ev["actor"].(map[string]any)
	if !ok {
		t.Fatal("expected actor to be present")
	}
	if actor["id"] == nil || actor["id"] == "" {
		t.Error("expected non-empty actor.id")
	}
	if actor["legalId"] == nil || actor["legalId"] == "" {
		t.Error("expected non-empty actor.legalId")
	}

	if pagination["hasNext"] != false {
		t.Errorf("expected hasNext=false, got %v", pagination["hasNext"])
	}
}

func TestAuditAfterUpdate(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "AuditUpdate", 1, `{"name":"Alice"}`)

	entityID := createEntityAndGetID(t, srv.URL, "AuditUpdate", 1, `{"name":"Bob"}`)
	updateEntity(t, srv.URL, entityID, `{"name":"Carol"}`)

	events, _ := getAuditEvents(t, srv.URL, entityID, "eventType=EntityChange")

	if len(events) != 2 {
		t.Fatalf("expected 2 audit events, got %d", len(events))
	}

	// Newest first (canonical wire spelling)
	if events[0]["changeType"] != "UPDATE" {
		t.Errorf("expected first event changeType=UPDATE, got %v", events[0]["changeType"])
	}
	if events[1]["changeType"] != "CREATE" {
		t.Errorf("expected second event changeType=CREATE, got %v", events[1]["changeType"])
	}
}

func TestAuditAfterDelete(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "AuditDelete", 1, `{"name":"Alice"}`)

	entityID := createEntityAndGetID(t, srv.URL, "AuditDelete", 1, `{"name":"Bob"}`)
	deleteEntity(t, srv.URL, entityID)

	events, _ := getAuditEvents(t, srv.URL, entityID, "eventType=EntityChange")

	if len(events) < 2 {
		t.Fatalf("expected at least 2 events, got %d", len(events))
	}

	// Newest first — DELETE should be first (canonical wire spelling)
	if events[0]["changeType"] != "DELETE" {
		t.Errorf("expected first event changeType=DELETE, got %v", events[0]["changeType"])
	}
}

func TestAuditFullLifecycle(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "AuditLC", 1, `{"name":"Alice"}`)

	entityID := createEntityAndGetID(t, srv.URL, "AuditLC", 1, `{"name":"Bob"}`)
	updateEntity(t, srv.URL, entityID, `{"name":"Carol"}`)
	updateEntity(t, srv.URL, entityID, `{"name":"Dave"}`)
	deleteEntity(t, srv.URL, entityID)

	events, _ := getAuditEvents(t, srv.URL, entityID, "eventType=EntityChange")

	if len(events) != 4 {
		t.Fatalf("expected 4 audit events, got %d", len(events))
	}

	expectedOrder := []string{"DELETE", "UPDATE", "UPDATE", "CREATE"}
	for i, want := range expectedOrder {
		got := events[i]["changeType"]
		if got != want {
			t.Errorf("event[%d]: expected changeType=%s, got %v", i, want, got)
		}
	}
}

func TestAuditPagination(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "AuditPage", 1, `{"name":"Alice"}`)

	entityID := createEntityAndGetID(t, srv.URL, "AuditPage", 1, `{"name":"Bob"}`)
	updateEntity(t, srv.URL, entityID, `{"name":"Carol"}`)
	updateEntity(t, srv.URL, entityID, `{"name":"Dave"}`)
	updateEntity(t, srv.URL, entityID, `{"name":"Eve"}`)
	// 4 EntityChange events total: 1 create + 3 updates

	// First page: limit=2, filter to EntityChange only
	events, pagination := getAuditEvents(t, srv.URL, entityID, "limit=2", "eventType=EntityChange")
	if len(events) != 2 {
		t.Fatalf("expected 2 events on first page, got %d", len(events))
	}
	if pagination["hasNext"] != true {
		t.Fatalf("expected hasNext=true on first page, got %v", pagination["hasNext"])
	}
	nextCursor, ok := pagination["nextCursor"].(string)
	if !ok || nextCursor == "" {
		t.Fatalf("expected non-empty nextCursor, got %v", pagination["nextCursor"])
	}

	// Second page
	events2, pagination2 := getAuditEvents(t, srv.URL, entityID, "limit=2", "cursor="+nextCursor, "eventType=EntityChange")
	if len(events2) != 2 {
		t.Fatalf("expected 2 events on second page, got %d", len(events2))
	}
	if pagination2["hasNext"] != false {
		t.Errorf("expected hasNext=false on second page, got %v", pagination2["hasNext"])
	}
}

func TestAuditTimeRangeFilter(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "AuditTime", 1, `{"name":"Alice"}`)

	entityID := createEntityAndGetID(t, srv.URL, "AuditTime", 1, `{"name":"Bob"}`)

	// Small sleep so update has a different timestamp
	time.Sleep(50 * time.Millisecond)
	midpoint := time.Now().UTC()
	time.Sleep(50 * time.Millisecond)

	updateEntity(t, srv.URL, entityID, `{"name":"Carol"}`)

	// Filter from midpoint — should get only the update EntityChange event
	fromParam := "fromUtcTime=" + midpoint.Format(time.RFC3339Nano)
	events, _ := getAuditEvents(t, srv.URL, entityID, fromParam, "eventType=EntityChange")

	if len(events) != 1 {
		t.Fatalf("expected 1 event after time filter, got %d", len(events))
	}
	if events[0]["changeType"] != "UPDATE" {
		t.Errorf("expected changeType=UPDATE, got %v", events[0]["changeType"])
	}
}

func TestAuditTransactionIdFilter(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "AuditTx", 1, `{"name":"Alice"}`)

	entityID, createTxID := createEntityAndGetIDAndTxID(t, srv.URL, "AuditTx", 1, `{"name":"Bob"}`)
	_ = updateEntity(t, srv.URL, entityID, `{"name":"Carol"}`)

	// Filter by the create transaction ID — returns both EntityChange and
	// StateMachine events that share the same txID (issue #20: the workflow
	// engine now uses the entity-write txID for SM audit events).
	events, _ := getAuditEvents(t, srv.URL, entityID, "transactionId="+createTxID)

	if len(events) < 1 {
		t.Fatal("expected at least 1 event with txId filter, got 0")
	}

	// The EntityChange CREATE event must be present (canonical wire spelling).
	var foundCreated bool
	for _, ev := range events {
		if ev["auditEventType"] == "EntityChange" && ev["changeType"] == "CREATE" {
			foundCreated = true
		}
	}
	if !foundCreated {
		t.Error("expected an EntityChange event with changeType=CREATE in filtered results")
	}

	// The update's EntityChange event (different txID) must NOT be present.
	for _, ev := range events {
		if ev["auditEventType"] == "EntityChange" && ev["changeType"] == "UPDATE" {
			t.Error("update event should not appear when filtering by create txID")
		}
	}
}

func TestAuditTenantIsolation(t *testing.T) {
	// Create entity in tenant A (default)
	srvA := newTestServer(t)
	importAndLockModel(t, srvA.URL, "AuditIso", 1, `{"name":"Alice"}`)
	entityID := createEntityAndGetID(t, srvA.URL, "AuditIso", 1, `{"name":"Bob"}`)

	// Create server with tenant B
	cfgB := app.DefaultConfig()
	cfgB.ContextPath = ""
	cfgB.IAM.MockTenantID = "tenant-b"
	cfgB.IAM.MockTenantName = "Tenant B"
	srvB := newTestServerWithConfig(t, cfgB)

	// Query audit from tenant B for entity from tenant A → 404
	resp := getAuditEventsRaw(t, srvB.URL, entityID)
	if resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 404, got %d; body: %s", resp.StatusCode, string(body))
	}
	resp.Body.Close()
}

func TestAuditEntityNotFound(t *testing.T) {
	srv := newTestServer(t)

	resp := getAuditEventsRaw(t, srv.URL, "00000000-0000-0000-0000-000000000099")
	if resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 404, got %d; body: %s", resp.StatusCode, string(body))
	}
	resp.Body.Close()
}

// --- helpers for workflow-based tests ---

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

// --- StateMachine audit event tests ---

func TestAuditWithStateMachineEvents(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "AuditSM", 1, `{"name":"Alice","age":30}`)

	// Import workflow: INITIAL --(auto)--> STABLE
	wfBody := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1",
			"name": "audit-sm-flow",
			"initialState": "INITIAL",
			"active": true,
			"states": {
				"INITIAL": {
					"transitions": [{
						"name": "auto-validate",
						"next": "STABLE",
						"manual": false
					}]
				},
				"STABLE": {}
			}
		}]
	}`
	importWorkflow(t, srv.URL, "AuditSM", 1, wfBody)

	entityID := createEntityAndGetID(t, srv.URL, "AuditSM", 1, `{"name":"Bob","age":25}`)

	// Query audit events (default: both EntityChange and StateMachine)
	events, _ := getAuditEvents(t, srv.URL, entityID)

	// Expect at least one EntityChange (CREATED) and at least one StateMachine event
	hasEntityChange := false
	hasStateMachine := false
	for _, ev := range events {
		switch ev["auditEventType"] {
		case "EntityChange":
			hasEntityChange = true
		case "StateMachine":
			hasStateMachine = true
		}
	}
	if !hasEntityChange {
		t.Error("expected at least one EntityChange audit event")
	}
	if !hasStateMachine {
		t.Error("expected at least one StateMachine audit event")
	}

	// Verify SM events include expected event types
	smEventTypes := make(map[string]bool)
	for _, ev := range events {
		if ev["auditEventType"] == "StateMachine" {
			if et, ok := ev["eventType"].(string); ok {
				smEventTypes[et] = true
			}
		}
	}
	for _, expected := range []string{"STATE_MACHINE_START", "STATE_MACHINE_FINISH", "WORKFLOW_FOUND", "TRANSITION_MAKE"} {
		if !smEventTypes[expected] {
			t.Errorf("expected StateMachine event type %q in audit events, found types: %v", expected, smEventTypes)
		}
	}
}

func TestAuditFilterStateMachineOnly(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "AuditSMOnly", 1, `{"name":"Alice","age":30}`)

	wfBody := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1",
			"name": "sm-only-flow",
			"initialState": "INITIAL",
			"active": true,
			"states": {
				"INITIAL": {
					"transitions": [{
						"name": "auto-go",
						"next": "DONE",
						"manual": false
					}]
				},
				"DONE": {}
			}
		}]
	}`
	importWorkflow(t, srv.URL, "AuditSMOnly", 1, wfBody)

	entityID := createEntityAndGetID(t, srv.URL, "AuditSMOnly", 1, `{"name":"Bob","age":25}`)

	// Filter: StateMachine only
	events, _ := getAuditEvents(t, srv.URL, entityID, "eventType=StateMachine")
	if len(events) == 0 {
		t.Fatal("expected at least one StateMachine event")
	}
	for _, ev := range events {
		if ev["auditEventType"] != "StateMachine" {
			t.Errorf("expected all events to be StateMachine, got %v", ev["auditEventType"])
		}
	}
}

func TestAuditFilterEntityChangeOnly(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "AuditECOnly", 1, `{"name":"Alice","age":30}`)

	wfBody := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1",
			"name": "ec-only-flow",
			"initialState": "INITIAL",
			"active": true,
			"states": {
				"INITIAL": {
					"transitions": [{
						"name": "auto-go",
						"next": "DONE",
						"manual": false
					}]
				},
				"DONE": {}
			}
		}]
	}`
	importWorkflow(t, srv.URL, "AuditECOnly", 1, wfBody)

	entityID := createEntityAndGetID(t, srv.URL, "AuditECOnly", 1, `{"name":"Bob","age":25}`)

	// Filter: EntityChange only
	events, _ := getAuditEvents(t, srv.URL, entityID, "eventType=EntityChange")
	if len(events) == 0 {
		t.Fatal("expected at least one EntityChange event")
	}
	for _, ev := range events {
		if ev["auditEventType"] != "EntityChange" {
			t.Errorf("expected all events to be EntityChange, got %v", ev["auditEventType"])
		}
	}
}

func newTestServerNoContextPath(t *testing.T) *httptest.Server {
	t.Helper()
	cfg := app.DefaultConfig()
	cfg.ContextPath = ""
	return newTestServerWithConfig(t, cfg)
}

// getSmTransactionID queries audit events for the given entity and returns the
// transactionId from the first StateMachine event. Since issue #20, this is the
// same as the entity-write transaction ID (the workflow engine uses
// entity.Meta.TransactionID for SM audit events).
func getSmTransactionID(t *testing.T, base, entityID string) string {
	t.Helper()
	events, _ := getAuditEvents(t, base, entityID, "eventType=StateMachine")
	if len(events) == 0 {
		t.Fatal("expected at least one StateMachine event to extract transactionId")
	}
	txID, ok := events[0]["transactionId"].(string)
	if !ok || txID == "" {
		t.Fatal("expected non-empty transactionId on SM event")
	}
	return txID
}

func TestGetStateMachineFinishedEvent_Found(t *testing.T) {
	srv := newTestServerNoContextPath(t)
	importAndLockModel(t, srv.URL, "SMFinish", 1, `{"name":"Alice","age":30}`)

	// Import workflow: INITIAL --(auto)--> FINAL
	wfBody := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1",
			"name": "finish-flow",
			"initialState": "INITIAL",
			"active": true,
			"states": {
				"INITIAL": {
					"transitions": [{
						"name": "auto-finish",
						"next": "FINAL",
						"manual": false
					}]
				},
				"FINAL": {}
			}
		}]
	}`
	importWorkflow(t, srv.URL, "SMFinish", 1, wfBody)

	entityID := createEntityAndGetID(t, srv.URL, "SMFinish", 1, `{"name":"Bob","age":25}`)

	// Since issue #20, the SM txID matches the entity-write txID.
	smTxID := getSmTransactionID(t, srv.URL, entityID)

	url := fmt.Sprintf("%s/audit/entity/%s/workflow/%s/finished", srv.URL, entityID, smTxID)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d; body: %s", resp.StatusCode, string(body))
	}

	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if result["auditEventType"] != "StateMachine" {
		t.Errorf("expected auditEventType=StateMachine, got %v", result["auditEventType"])
	}
	if result["eventType"] != "STATE_MACHINE_FINISH" {
		t.Errorf("expected eventType=STATE_MACHINE_FINISH, got %v", result["eventType"])
	}
	if result["entityId"] != entityID {
		t.Errorf("expected entityId=%s, got %v", entityID, result["entityId"])
	}
	if result["transactionId"] != smTxID {
		t.Errorf("expected transactionId=%s, got %v", smTxID, result["transactionId"])
	}
}

func TestGetStateMachineFinishedEvent_NoFinishedEvent(t *testing.T) {
	// Test with a workflow that has events but no finished event for a different transaction.
	srv := newTestServerNoContextPath(t)
	importAndLockModel(t, srv.URL, "SMNoFinish", 1, `{"name":"Alice","age":30}`)

	wfBody := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1",
			"name": "no-finish-flow",
			"initialState": "INITIAL",
			"active": true,
			"states": {
				"INITIAL": {
					"transitions": [{
						"name": "auto-go",
						"next": "DONE",
						"manual": false
					}]
				},
				"DONE": {}
			}
		}]
	}`
	importWorkflow(t, srv.URL, "SMNoFinish", 1, wfBody)

	entityID, _ := createEntityAndGetIDAndTxID(t, srv.URL, "SMNoFinish", 1, `{"name":"Bob","age":25}`)

	// Use a different (random) transaction ID — no finished event will match
	url := fmt.Sprintf("%s/audit/entity/%s/workflow/%s/finished",
		srv.URL, entityID, "00000000-0000-0000-0000-ffffffffffff")
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 404, got %d; body: %s", resp.StatusCode, string(body))
	}
}

func TestGetStateMachineFinishedEvent_NonExistentEntity(t *testing.T) {
	srv := newTestServerNoContextPath(t)

	url := fmt.Sprintf("%s/audit/entity/%s/workflow/%s/finished",
		srv.URL,
		"00000000-0000-0000-0000-000000000001",
		"00000000-0000-0000-0000-000000000002",
	)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 404, got %d; body: %s", resp.StatusCode, string(body))
	}
}
