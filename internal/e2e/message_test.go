package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// createMessageE2E creates a single message via the REST API and returns the message ID.
func createMessageE2E(t *testing.T, subject string, payload string) string {
	t.Helper()
	path := fmt.Sprintf("/api/message/new/%s", subject)
	body := fmt.Sprintf(`{"payload": %s, "meta-data": {"source": "e2e"}}`, payload)
	resp := doAuth(t, http.MethodPost, path, body)
	respBody := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("createMessage subject=%s: expected 200, got %d: %s", subject, resp.StatusCode, respBody)
	}

	// Response is an array: [{"entityIds": ["uuid"], "transactionId": "uuid"}]
	var results []map[string]any
	if err := json.Unmarshal([]byte(respBody), &results); err != nil {
		t.Fatalf("createMessage subject=%s: failed to parse JSON response: %v\nbody: %s", subject, err, respBody)
	}
	if len(results) == 0 {
		t.Fatalf("createMessage subject=%s: expected at least one result, got empty array\nbody: %s", subject, respBody)
	}

	entityIDs, ok := results[0]["entityIds"].([]any)
	if !ok || len(entityIDs) == 0 {
		t.Fatalf("createMessage subject=%s: expected entityIds array in response, got: %v", subject, results[0])
	}

	messageID, ok := entityIDs[0].(string)
	if !ok || messageID == "" {
		t.Fatalf("createMessage subject=%s: expected non-empty string messageId, got: %v", subject, entityIDs[0])
	}
	return messageID
}

// TestMessage_GetMessage_ContentIsEmbeddedJSON verifies that GetMessage returns the
// message payload as an embedded JSON object in the "content" field, not as a
// JSON-encoded string. This is the canonical #21 JSON-in-string defect for the
// messaging domain.
func TestMessage_GetMessage_ContentIsEmbeddedJSON(t *testing.T) {
	// Create a message with a JSON object payload.
	payload := `{"sample": "value", "n": 42}`
	msgID := createMessageE2E(t, "test-subject", payload)

	// Retrieve the message and assert content is an embedded object, not a string.
	path := fmt.Sprintf("/api/message/%s", msgID)
	req := authRequest(t, http.MethodGet, path, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get message: %v", err)
	}
	rawStr := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("getMessage status=%d body=%s", resp.StatusCode, rawStr)
	}

	var msg map[string]any
	if err := json.Unmarshal([]byte(rawStr), &msg); err != nil {
		t.Fatalf("decode message: %v\nbody: %s", err, rawStr)
	}
	content, ok := msg["content"].(map[string]any)
	if !ok {
		t.Fatalf("content field is not a JSON object; type=%T value=%v\n(expected embedded JSON, not a string — see #21 JSON-in-string defect)", msg["content"], msg["content"])
	}
	if content["sample"] != "value" {
		t.Errorf("content.sample = %v, want \"value\"", content["sample"])
	}
	if n, ok := content["n"].(float64); !ok || n != 42 {
		t.Errorf("content.n = %v (%T), want 42", content["n"], content["n"])
	}
}

// TestMessage_DeleteMessage_Shape verifies that DeleteMessage returns {entityIds:[]string}
// (no transactionId) — the actual server shape, not EntityTransactionResponse.
func TestMessage_DeleteMessage_Shape(t *testing.T) {
	msgID := createMessageE2E(t, "test-delete-shape", `{"x": 1}`)

	path := fmt.Sprintf("/api/message/%s", msgID)
	resp := doAuth(t, http.MethodDelete, path, "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("deleteMessage status=%d body=%s", resp.StatusCode, body)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatalf("decode delete response: %v\nbody: %s", err, body)
	}
	entityIDs, ok := result["entityIds"].([]any)
	if !ok {
		t.Fatalf("entityIds field is not an array; type=%T value=%v", result["entityIds"], result["entityIds"])
	}
	if len(entityIDs) == 0 {
		t.Fatalf("entityIds is empty")
	}
	if entityIDs[0].(string) != msgID {
		t.Errorf("entityIds[0] = %v, want %s", entityIDs[0], msgID)
	}
	// The server does NOT emit transactionId for delete (no transactionId field in handler output).
	if _, has := result["transactionId"]; has {
		t.Errorf("deleteMessage response unexpectedly contains transactionId field; server shape has no transactionId")
	}
}

// TestMessage_DeleteMessages_Shape verifies that DeleteMessages (batch) returns an array,
// not a string — the spec was wrong (type:string).
func TestMessage_DeleteMessages_Shape(t *testing.T) {
	id1 := createMessageE2E(t, "test-batch-shape", `{"seq": 1}`)
	id2 := createMessageE2E(t, "test-batch-shape", `{"seq": 2}`)

	batchBody := fmt.Sprintf("[%q, %q]", id1, id2)
	resp := doAuth(t, http.MethodDelete, "/api/message", batchBody)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("deleteMessages status=%d body=%s", resp.StatusCode, body)
	}

	// Response must be a JSON array, not a string.
	var result []any
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatalf("deleteMessages: response is not a JSON array: %v\nbody: %s", err, body)
	}
	if len(result) == 0 {
		t.Fatalf("deleteMessages: response array is empty")
	}
}

// TestMessage_NewMessage_Shape verifies that NewMessage returns an array with entityIds,
// not a bare string — the spec was wrong (type:string).
func TestMessage_NewMessage_Shape(t *testing.T) {
	body := `{"payload": {"k": "v"}, "meta-data": {}}`
	resp := doAuth(t, http.MethodPost, "/api/message/new/test-new-shape", body)
	respBody := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("newMessage status=%d body=%s", resp.StatusCode, respBody)
	}

	// Response must be a JSON array.
	var result []map[string]any
	if err := json.Unmarshal([]byte(respBody), &result); err != nil {
		t.Fatalf("newMessage: response is not a JSON array: %v\nbody: %s", err, respBody)
	}
	if len(result) == 0 {
		t.Fatalf("newMessage: response array is empty")
	}
	ids, ok := result[0]["entityIds"].([]any)
	if !ok || len(ids) == 0 {
		t.Fatalf("newMessage: entityIds not an array or empty: %v", result[0])
	}
}

// TestMessage_GetMessage_404_ContentType verifies that a 404 from getMessage uses
// application/problem+json per RFC 9457 (the original spec had application/json
// with ErrorResponse schema — both were wrong; spec now corrected to match server).
func TestMessage_GetMessage_404_ContentType(t *testing.T) {
	resp := doAuth(t, http.MethodGet, "/api/message/00000000-0000-1000-8000-000000000001", "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", resp.StatusCode, body)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("Content-Type = %q, want application/problem+json prefix (RFC 9457 — see #21)", ct)
	}
}

// TestMessage_DeleteBatch verifies that a batch delete removes the specified messages
// while leaving others intact.
func TestMessage_DeleteBatch(t *testing.T) {
	// 1. Create 3 messages.
	id1 := createMessageE2E(t, "e2e.batch", `{"seq": 1}`)
	id2 := createMessageE2E(t, "e2e.batch", `{"seq": 2}`)
	id3 := createMessageE2E(t, "e2e.batch", `{"seq": 3}`)

	// 2. Delete 2 by batch (id1 and id2).
	batchBody, err := json.Marshal([]string{id1, id2})
	if err != nil {
		t.Fatalf("failed to marshal batch IDs: %v", err)
	}
	delResp := doAuth(t, http.MethodDelete, "/api/message", string(batchBody))
	delBody := readBody(t, delResp)
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("deleteMessages batch: expected 200, got %d: %s", delResp.StatusCode, delBody)
	}

	// 3. GET remaining message (id3) — still exists.
	path3 := fmt.Sprintf("/api/message/%s", id3)
	getResp := doAuth(t, http.MethodGet, path3, "")
	getBody := readBody(t, getResp)
	if getResp.StatusCode != http.StatusOK {
		t.Errorf("getMessage %s (should exist): expected 200, got %d: %s", id3, getResp.StatusCode, getBody)
	}

	// 4. GET deleted messages — 404.
	for _, id := range []string{id1, id2} {
		p := fmt.Sprintf("/api/message/%s", id)
		r := doAuth(t, http.MethodGet, p, "")
		b := readBody(t, r)
		if r.StatusCode != http.StatusNotFound {
			t.Errorf("getMessage %s (should be deleted): expected 404, got %d: %s", id, r.StatusCode, b)
		}
	}
}
