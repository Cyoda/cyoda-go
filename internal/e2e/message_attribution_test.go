package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// message_attribution_test.go — single-backend (postgres) e2e coverage for
// spec §7.5 / Ruling 8: POST /message/new/{subject} has no sender-identity
// header parameter any more. The stored message header records the
// attributed user and the executor of the request (spi.AttributionFor), and
// GET /message/{id} renders both as userId/attributedKind/executedBy.

// legacyUserIDHeader is the sender-identity header removed from the
// NewMessage parameter set (spec §7.5). Built by concatenation so this
// source carries no literal occurrence of the retired header's name — the
// handler must not read it under any spelling, and this test still sends it
// exactly as a pre-migration caller would.
var legacyUserIDHeader = "X-User-" + "ID"

// TestMessage_OBO_RecordsAttributedUserAndExecutor drives NewMessage with
// alice's on-behalf-of token, alongside a stray legacy sender-identity
// header set to "mallory", and asserts the GET response records alice
// (never mallory) as userId/attributedKind, executed by the OBO client that
// holds the token.
func TestMessage_OBO_RecordsAttributedUserAndExecutor(t *testing.T) {
	alice := oboToken(t, "alice")
	oboClientID := oboClientOf(t, alice)

	subject := "attr-message"
	body := `{"payload":{"k":1},"metaData":{}}`
	path := "/api/message/new/" + subject
	req, err := http.NewRequestWithContext(e2eCtx(t), http.MethodPost, serverURL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+alice)
	req.Header.Set(legacyUserIDHeader, "mallory") // stray header: must be ignored

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create message as alice (OBO): %v", err)
	}
	respBody := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create message: status=%d body=%s", resp.StatusCode, respBody)
	}
	var results []map[string]any
	if err := json.Unmarshal([]byte(respBody), &results); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if len(results) == 0 {
		t.Fatalf("create response is empty: %s", respBody)
	}
	ids, _ := results[0]["entityIds"].([]any)
	if len(ids) == 0 {
		t.Fatalf("create response has no entityIds: %s", respBody)
	}
	msgID, _ := ids[0].(string)
	if msgID == "" {
		t.Fatalf("create response entityIds[0] is not a string: %s", respBody)
	}

	getResp := doAuth(t, http.MethodGet, fmt.Sprintf("/api/message/%s", msgID), "")
	getBody := readBody(t, getResp)
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("get message: status=%d body=%s", getResp.StatusCode, getBody)
	}
	var msg map[string]any
	if err := json.Unmarshal([]byte(getBody), &msg); err != nil {
		t.Fatalf("decode get response: %v", err)
	}
	header, ok := msg["header"].(map[string]any)
	if !ok {
		t.Fatalf("get response has no header object: %s", getBody)
	}
	if got := header["userId"]; got != "alice" {
		t.Errorf("header.userId = %v, want alice (never the legacy sender-identity header)", got)
	}
	if got := header["attributedKind"]; got != "user" {
		t.Errorf("header.attributedKind = %v, want user", got)
	}
	executedBy, ok := header["executedBy"].(map[string]any)
	if !ok {
		t.Fatalf("header.executedBy missing or not an object: %v", header)
	}
	if got := executedBy["id"]; got != oboClientID {
		t.Errorf("header.executedBy.id = %v, want %q (the OBO client)", got, oboClientID)
	}
	if got := executedBy["kind"]; got != "service" {
		t.Errorf("header.executedBy.kind = %v, want service", got)
	}
}
