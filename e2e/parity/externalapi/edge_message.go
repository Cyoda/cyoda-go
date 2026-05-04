package externalapi

import (
	"encoding/json"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/externalapi/driver"
	"github.com/cyoda-platform/cyoda-go/e2e/parity"
	parityclient "github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

func init() {
	parity.Register(
		parity.NamedTest{Name: "ExternalAPI_11_01_SaveSingle", Fn: RunExternalAPI_11_01_SaveSingle},
		parity.NamedTest{Name: "ExternalAPI_11_02_DeleteSingle", Fn: RunExternalAPI_11_02_DeleteSingle},
		parity.NamedTest{Name: "ExternalAPI_11_03_DeleteCollection", Fn: RunExternalAPI_11_03_DeleteCollection},
	)
}

const edgeMessagePayload = `{"hello": "world"}`

// edgeMessageHeaderInput returns the header set for tranche-3 file 11.
// Cyoda-go reads these as HTTP headers; the dictionary embeds them in the body.
func edgeMessageHeaderInput() parityclient.MessageHeaderInput {
	return parityclient.MessageHeaderInput{
		ContentType:   "application/json",
		MessageID:     "test-msg-11-01",
		UserID:        "Larry",
		ReplyTo:       "Jimmy",
		Recipient:     "Bobby",
		CorrelationID: "00000000-0000-0000-0000-000000000001",
	}
}

// RunExternalAPI_11_01_SaveSingle — dictionary 11/01.
// cyoda-go uses POST /api/message/new/{subject}; the dictionary references
// /edge-message — different_naming_same_level URL drift.
// Verifies that header fields round-trip through CreateMessageWithHeaders
// and that the body payload is retrievable via GetMessage.
func RunExternalAPI_11_01_SaveSingle(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)
	id, err := d.CreateMessageWithHeaders("Publication", edgeMessagePayload, edgeMessageHeaderInput())
	if err != nil {
		t.Fatalf("CreateMessageWithHeaders: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty message id")
	}

	got, err := d.GetMessage(id)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}

	// Response shape: {header: {...}, metaData: {...}, content: <json-value>}
	// Per #21 fix: "content" is now an embedded JSON value (not a JSON-in-string).
	// Verify the "content" key is present and the payload round-trips.
	rawContent, ok := got["content"]
	if !ok {
		t.Fatalf("GetMessage response missing 'content' key; got keys: %v", mapKeys(got))
	}

	// Re-encode content to JSON for structural comparison (handles any JSON type).
	gotJSON, err := json.Marshal(rawContent)
	if err != nil {
		t.Fatalf("marshal content: %v", err)
	}
	var wantBody any
	if err := json.Unmarshal([]byte(edgeMessagePayload), &wantBody); err != nil {
		t.Fatalf("edgeMessagePayload is not valid JSON (test bug): %v", err)
	}
	wantJSON, _ := json.Marshal(wantBody)
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("content round-trip mismatch: got %q, want %q", string(gotJSON), string(wantJSON))
	}

	// Verify header fields round-trip through the "header" sub-object.
	rawHeader, ok := got["header"]
	if !ok {
		t.Fatalf("GetMessage response missing 'header' key; got keys: %v", mapKeys(got))
	}
	hdr, ok := rawHeader.(map[string]any)
	if !ok {
		t.Fatalf("GetMessage 'header' is not an object; got %T", rawHeader)
	}
	checkHeaderField(t, hdr, "userId", "Larry")
	checkHeaderField(t, hdr, "replyTo", "Jimmy")
	checkHeaderField(t, hdr, "recipient", "Bobby")
	checkHeaderField(t, hdr, "correlationId", "00000000-0000-0000-0000-000000000001")
	checkHeaderField(t, hdr, "messageId", "test-msg-11-01")
}

// RunExternalAPI_11_02_DeleteSingle — dictionary 11/02.
// Creates a message, deletes it, then verifies GetMessage returns an error.
func RunExternalAPI_11_02_DeleteSingle(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)
	id, err := d.CreateMessage("Publication", edgeMessagePayload)
	if err != nil {
		t.Fatalf("CreateMessage: %v", err)
	}
	if err := d.DeleteMessage(id); err != nil {
		t.Fatalf("DeleteMessage: %v", err)
	}
	if _, err := d.GetMessage(id); err == nil {
		t.Fatal("expected GetMessage to fail after delete")
	}
}

// RunExternalAPI_11_03_DeleteCollection — dictionary 11/03.
// Creates two messages, batch-deletes both via DELETE /api/message with a
// JSON-array body, then verifies both return 404 on subsequent GET.
//
// Phase 0.2 incorrectly skipped this scenario under #134, believing the
// endpoint was delete-all-paged-by-tx-size. The handler
// (internal/domain/messaging/handler.go:222) in fact reads the ID list
// from the request body and calls store.DeleteBatch. transactionSize is
// only a paging knob (default 1000) and irrelevant for handfuls of IDs.
// #134 should be closed — no server-side change was ever needed.
func RunExternalAPI_11_03_DeleteCollection(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)

	id1, err := d.CreateMessage("Publication", edgeMessagePayload)
	if err != nil {
		t.Fatalf("create m1: %v", err)
	}
	id2, err := d.CreateMessage("Publication", edgeMessagePayload)
	if err != nil {
		t.Fatalf("create m2: %v", err)
	}

	deleted, err := d.DeleteMessages([]string{id1, id2})
	if err != nil {
		t.Fatalf("DeleteMessages: %v", err)
	}

	// Verify deleted count.
	if len(deleted) != 2 {
		t.Errorf("deleted count: got %d, want 2", len(deleted))
	}

	// Verify the returned IDs match the requested IDs (order-insensitive).
	requested := map[string]struct{}{id1: {}, id2: {}}
	for _, del := range deleted {
		if _, ok := requested[del]; !ok {
			t.Errorf("DeleteMessages returned unexpected ID %q", del)
		}
	}

	// Verify both messages are gone.
	for _, id := range []string{id1, id2} {
		if _, err := d.GetMessage(id); err == nil {
			t.Errorf("GetMessage(%s) succeeded after batch delete; expected 404", id)
		}
	}
}

// checkHeaderField asserts that hdr[key] == want and reports a test failure
// with context if it does not.
func checkHeaderField(t *testing.T, hdr map[string]any, key, want string) {
	t.Helper()
	v, ok := hdr[key]
	if !ok {
		t.Errorf("header field %q missing; header keys: %v", key, mapKeys(hdr))
		return
	}
	got, ok := v.(string)
	if !ok {
		t.Errorf("header field %q: expected string, got %T (%v)", key, v, v)
		return
	}
	if got != want {
		t.Errorf("header field %q: got %q, want %q", key, got, want)
	}
}

// mapKeys returns the sorted keys of m as a slice for diagnostic messages.
func mapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
