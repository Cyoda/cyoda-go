package messaging_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/app"

	_ "github.com/cyoda-platform/cyoda-go/plugins/memory"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	cfg := app.DefaultConfig()
	cfg.ContextPath = ""
	a := app.New(cfg)
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	return srv
}

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

func postMessage(t *testing.T, base, subject, body string) *http.Response {
	t.Helper()
	url := base + "/message/new/" + subject
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST request failed: %v", err)
	}
	return resp
}

func getMessage(t *testing.T, base, messageID string) *http.Response {
	t.Helper()
	url := base + "/message/" + messageID
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET request failed: %v", err)
	}
	return resp
}

func deleteMessage(t *testing.T, base, messageID string) *http.Response {
	t.Helper()
	url := base + "/message/" + messageID
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE request failed: %v", err)
	}
	return resp
}

func deleteMessages(t *testing.T, base string, ids []string) *http.Response {
	t.Helper()
	body, _ := json.Marshal(ids)
	url := base + "/message"
	req, err := http.NewRequest(http.MethodDelete, url, strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE batch request failed: %v", err)
	}
	return resp
}

func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read body: %v", err)
	}
	return data
}

func expectStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected status %d, got %d; body: %s", want, resp.StatusCode, string(body))
	}
}

// extractMessageID extracts the first entity ID from the create-message response.
// Response shape: [{"entityIds": ["<uuid>"], "transactionId": "<uuid>"}]
func extractMessageID(t *testing.T, resp *http.Response) string {
	t.Helper()
	body := readBody(t, resp)
	var result []map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("failed to parse create response: %v\nbody: %s", err, string(body))
	}
	if len(result) == 0 {
		t.Fatal("empty create response array")
	}
	ids, ok := result[0]["entityIds"].([]any)
	if !ok || len(ids) == 0 {
		t.Fatalf("no entityIds in response: %s", string(body))
	}
	return ids[0].(string)
}

func TestNewMessageAndGet(t *testing.T) {
	srv := newTestServer(t)

	// POST a new message
	body := `{"payload": {"name": "Alice", "age": 30}, "meta-data": {"eventType": "test.event"}}`
	resp := postMessage(t, srv.URL, "test.subject", body)
	expectStatus(t, resp, http.StatusOK)
	msgID := extractMessageID(t, resp)

	if msgID == "" {
		t.Fatal("expected non-empty message ID")
	}

	// GET the message back
	resp = getMessage(t, srv.URL, msgID)
	expectStatus(t, resp, http.StatusOK)
	data := readBody(t, resp)

	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("failed to parse GET response: %v", err)
	}

	// Verify header fields
	header, ok := msg["header"].(map[string]any)
	if !ok {
		t.Fatalf("expected header object, got %v", msg["header"])
	}
	if got := header["subject"]; got != "test.subject" {
		t.Errorf("expected subject=test.subject, got %v", got)
	}
	if got := header["contentType"]; got != "application/json" {
		t.Errorf("expected contentType=application/json, got %v", got)
	}
	if got := header["contentEncoding"]; got != "UTF-8" {
		t.Errorf("expected contentEncoding=UTF-8, got %v", got)
	}

	// Verify content is an embedded JSON object (not a string — see #21 JSON-in-string defect).
	content, ok := msg["content"].(map[string]any)
	if !ok {
		t.Fatalf("expected content to be a JSON object, got %T: %v", msg["content"], msg["content"])
	}
	if content["name"] != "Alice" {
		t.Errorf("expected content.name=Alice, got %v", content["name"])
	}
}

func TestNewMessageWithIndexedMetadata(t *testing.T) {
	srv := newTestServer(t)

	// POST message with indexed metadata
	body := `{"payload": {"data": "test"}, "meta-data": {"eventType": "nobel.prize.announced", "timestamp": "2024-10-09T12:00:00Z", "category": "physics"}}`
	resp := postMessage(t, srv.URL, "events.nobel", body)
	expectStatus(t, resp, http.StatusOK)
	msgID := extractMessageID(t, resp)

	// GET the message back
	resp = getMessage(t, srv.URL, msgID)
	expectStatus(t, resp, http.StatusOK)
	data := readBody(t, resp)

	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("failed to parse GET response: %v", err)
	}

	// Verify metaData contains indexed values
	metaData, ok := msg["metaData"].(map[string]any)
	if !ok {
		t.Fatalf("expected metaData object, got %v", msg["metaData"])
	}

	indexedValues, ok := metaData["indexedValues"].(map[string]any)
	if !ok {
		t.Fatalf("expected indexedValues object, got %v", metaData["indexedValues"])
	}

	if indexedValues["eventType"] != "nobel.prize.announced" {
		t.Errorf("expected eventType=nobel.prize.announced, got %v", indexedValues["eventType"])
	}
	if indexedValues["timestamp"] != "2024-10-09T12:00:00Z" {
		t.Errorf("expected timestamp=2024-10-09T12:00:00Z, got %v", indexedValues["timestamp"])
	}
	if indexedValues["category"] != "physics" {
		t.Errorf("expected category=physics, got %v", indexedValues["category"])
	}

	// Verify values contains only typeReferences (meta-data goes to indexedValues)
	values, ok := metaData["values"].(map[string]any)
	if !ok {
		t.Fatalf("expected values object, got %v", metaData["values"])
	}
	if _, hasTypeRefs := values["typeReferences"]; !hasTypeRefs {
		t.Errorf("expected typeReferences in values, got %v", values)
	}
	if len(values) != 1 {
		t.Errorf("expected values to contain only typeReferences, got %v", values)
	}
}

func TestMetadataPreservesJsonTypes(t *testing.T) {
	srv := newTestServer(t)

	// POST message with mixed-type metadata: string, number, boolean, null
	body := `{"payload": {"x": 1}, "meta-data": {"name": "Alice", "age": 30, "active": true, "score": 99.5, "tags": ["a","b"]}}`
	resp := postMessage(t, srv.URL, "typed.meta", body)
	expectStatus(t, resp, http.StatusOK)
	msgID := extractMessageID(t, resp)

	// GET the message back
	resp = getMessage(t, srv.URL, msgID)
	expectStatus(t, resp, http.StatusOK)
	data := readBody(t, resp)

	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("failed to parse GET response: %v", err)
	}

	metaData := msg["metaData"].(map[string]any)
	indexed := metaData["indexedValues"].(map[string]any)

	// String stays string
	if name, ok := indexed["name"].(string); !ok || name != "Alice" {
		t.Errorf("expected name=\"Alice\" (string), got %v (%T)", indexed["name"], indexed["name"])
	}

	// Number stays number (JSON numbers are float64 in Go)
	if age, ok := indexed["age"].(float64); !ok || age != 30 {
		t.Errorf("expected age=30 (float64), got %v (%T)", indexed["age"], indexed["age"])
	}

	// Boolean stays boolean
	if active, ok := indexed["active"].(bool); !ok || active != true {
		t.Errorf("expected active=true (bool), got %v (%T)", indexed["active"], indexed["active"])
	}

	// Float stays float
	if score, ok := indexed["score"].(float64); !ok || score != 99.5 {
		t.Errorf("expected score=99.5 (float64), got %v (%T)", indexed["score"], indexed["score"])
	}

	// Array stays array
	if tags, ok := indexed["tags"].([]any); !ok || len(tags) != 2 {
		t.Errorf("expected tags=[\"a\",\"b\"] ([]any), got %v (%T)", indexed["tags"], indexed["tags"])
	}
}

func TestNewMessageWithoutMetadata(t *testing.T) {
	srv := newTestServer(t)

	body := `{"payload": {"key": "value"}}`
	resp := postMessage(t, srv.URL, "no.meta", body)
	expectStatus(t, resp, http.StatusOK)
	msgID := extractMessageID(t, resp)

	resp = getMessage(t, srv.URL, msgID)
	expectStatus(t, resp, http.StatusOK)
	data := readBody(t, resp)

	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("failed to parse GET response: %v", err)
	}

	// Content should be an embedded JSON object (not a string — see #21 JSON-in-string defect).
	content, ok := msg["content"].(map[string]any)
	if !ok {
		t.Fatalf("expected content to be a JSON object, got %T: %v", msg["content"], msg["content"])
	}
	if content["key"] != "value" {
		t.Errorf("expected content.key=value, got %v", content["key"])
	}
}

func TestDeleteMessage(t *testing.T) {
	srv := newTestServer(t)

	body := `{"payload": {"x": 1}}`
	resp := postMessage(t, srv.URL, "del.test", body)
	expectStatus(t, resp, http.StatusOK)
	msgID := extractMessageID(t, resp)

	// Delete the message
	resp = deleteMessage(t, srv.URL, msgID)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	// GET should now return 404
	resp = getMessage(t, srv.URL, msgID)
	expectStatus(t, resp, http.StatusNotFound)
	resp.Body.Close()
}

func TestDeleteMessages(t *testing.T) {
	srv := newTestServer(t)

	// Create 3 messages
	var ids []string
	for i := 0; i < 3; i++ {
		body := `{"payload": {"index": ` + string(rune('0'+i)) + `}}`
		resp := postMessage(t, srv.URL, "batch.del", body)
		expectStatus(t, resp, http.StatusOK)
		ids = append(ids, extractMessageID(t, resp))
	}

	// Batch delete first two
	resp := deleteMessages(t, srv.URL, ids[:2])
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	// First two should be gone
	resp = getMessage(t, srv.URL, ids[0])
	expectStatus(t, resp, http.StatusNotFound)
	resp.Body.Close()

	resp = getMessage(t, srv.URL, ids[1])
	expectStatus(t, resp, http.StatusNotFound)
	resp.Body.Close()

	// Third should still exist
	resp = getMessage(t, srv.URL, ids[2])
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()
}

func TestGetMessageNotFound(t *testing.T) {
	srv := newTestServer(t)

	resp := getMessage(t, srv.URL, "00000000-0000-0000-0000-000000000000")
	expectStatus(t, resp, http.StatusNotFound)
	resp.Body.Close()
}

func TestTenantIsolation(t *testing.T) {
	srvA := newTestServerWithTenant(t, "tenant-A", "Tenant A")
	srvB := newTestServerWithTenant(t, "tenant-B", "Tenant B")

	// Create message in tenant A
	body := `{"payload": {"secret": "A-only"}}`
	resp := postMessage(t, srvA.URL, "isolated", body)
	expectStatus(t, resp, http.StatusOK)
	msgID := extractMessageID(t, resp)

	// Tenant A can read it
	resp = getMessage(t, srvA.URL, msgID)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	// Tenant B cannot see it
	resp = getMessage(t, srvB.URL, msgID)
	expectStatus(t, resp, http.StatusNotFound)
	resp.Body.Close()
}

func TestNewMessageInvalidJSON(t *testing.T) {
	srv := newTestServer(t)

	resp := postMessage(t, srv.URL, "bad.json", `not valid json`)
	expectStatus(t, resp, http.StatusBadRequest)
	resp.Body.Close()
}

func TestNewMessageMissingPayload(t *testing.T) {
	srv := newTestServer(t)

	resp := postMessage(t, srv.URL, "no.payload", `{"other": "field"}`)
	expectStatus(t, resp, http.StatusBadRequest)
	resp.Body.Close()
}

func TestDeleteMessageNotFound(t *testing.T) {
	srv := newTestServer(t)

	resp := deleteMessage(t, srv.URL, "00000000-0000-0000-0000-000000000000")
	expectStatus(t, resp, http.StatusNotFound)
	resp.Body.Close()
}

func TestDeleteMessagesInvalidBody(t *testing.T) {
	srv := newTestServer(t)

	url := srv.URL + "/message"
	req, err := http.NewRequest(http.MethodDelete, url, strings.NewReader(`not json`))
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE batch request failed: %v", err)
	}
	expectStatus(t, resp, http.StatusBadRequest)
	resp.Body.Close()
}

func TestNewMessageWithOptionalHeaders(t *testing.T) {
	srv := newTestServer(t)

	body := `{"payload": {"data": "value"}, "meta-data": {"key1": "val1"}}`
	resp := postMessage(t, srv.URL, "headers.test", body)
	expectStatus(t, resp, http.StatusOK)
	msgID := extractMessageID(t, resp)

	// Verify all header optional fields are handled
	resp = getMessage(t, srv.URL, msgID)
	expectStatus(t, resp, http.StatusOK)
	data := readBody(t, resp)

	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("failed to parse GET response: %v", err)
	}

	// Verify metaData contains our key in indexedValues
	md, ok := msg["metaData"].(map[string]any)
	if !ok {
		t.Fatalf("expected metaData object, got %v", msg["metaData"])
	}
	indexedValues, ok := md["indexedValues"].(map[string]any)
	if !ok {
		t.Fatalf("expected indexedValues map, got %v", md["indexedValues"])
	}
	if indexedValues["key1"] != "val1" {
		t.Errorf("expected metadata key1=val1, got %v", indexedValues["key1"])
	}
}

func TestResponseShape(t *testing.T) {
	srv := newTestServer(t)

	// Verify create response shape: [{"entityIds": [...], "transactionId": "..."}]
	body := `{"payload": {"shape": "test"}}`
	resp := postMessage(t, srv.URL, "shape.test", body)
	expectStatus(t, resp, http.StatusOK)
	data := readBody(t, resp)

	var createResult []map[string]any
	if err := json.Unmarshal(data, &createResult); err != nil {
		t.Fatalf("create response is not JSON array: %v", err)
	}
	if len(createResult) != 1 {
		t.Fatalf("expected 1 element in create response, got %d", len(createResult))
	}
	entry := createResult[0]
	txID, ok := entry["transactionId"].(string)
	if !ok || txID == "" {
		t.Errorf("expected non-empty transactionId string, got %v", entry["transactionId"])
	}
	entityIds, ok := entry["entityIds"].([]any)
	if !ok || len(entityIds) != 1 {
		t.Errorf("expected entityIds array with 1 element, got %v", entry["entityIds"])
	}

	msgID := entityIds[0].(string)

	// Verify get response shape: {"header": {...}, "metaData": {...}, "content": <json>}
	resp = getMessage(t, srv.URL, msgID)
	expectStatus(t, resp, http.StatusOK)
	data = readBody(t, resp)

	var getResult map[string]any
	if err := json.Unmarshal(data, &getResult); err != nil {
		t.Fatalf("get response is not JSON object: %v", err)
	}

	// Must have header, metaData, and content as an embedded JSON object
	// (not a string — see #21 JSON-in-string defect).
	if _, ok := getResult["header"].(map[string]any); !ok {
		t.Error("expected header object in get response")
	}
	if _, ok := getResult["metaData"].(map[string]any); !ok {
		t.Error("expected metaData object in get response")
	}
	if _, ok := getResult["content"].(map[string]any); !ok {
		t.Errorf("expected content to be a JSON object in get response, got %T: %v", getResult["content"], getResult["content"])
	}
}
