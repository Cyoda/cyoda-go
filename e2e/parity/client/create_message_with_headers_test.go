package client

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestCreateMessageWithHeaders_SetsXHeaders verifies that each non-empty
// field in MessageHeaderInput is sent as the matching HTTP header on the
// outbound POST /api/message/new/{subject} request.
func TestCreateMessageWithHeaders_SetsXHeaders(t *testing.T) {
	var gotMethod, gotPath string
	var gotHeaders http.Header
	var gotBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotHeaders = r.Header.Clone()
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[{"entityIds":["msg-abc"],"transactionId":"tx-1"}]`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	id, err := c.CreateMessageWithHeaders(t, "Publication", `{"k":1}`, MessageHeaderInput{
		ContentType:     "application/json",
		ContentEncoding: "utf-8",
		MessageID:       "msg-id-1",
		UserID:          "user-42",
		Recipient:       "rec-1",
		ReplyTo:         "reply-addr",
		CorrelationID:   "corr-xyz",
	})
	if err != nil {
		t.Fatalf("CreateMessageWithHeaders: %v", err)
	}

	// Method + path
	if gotMethod != http.MethodPost {
		t.Errorf("method: got %q, want POST", gotMethod)
	}
	if gotPath != "/api/message/new/Publication" {
		t.Errorf("path: got %q, want /api/message/new/Publication", gotPath)
	}

	// ID decoded correctly
	if id != "msg-abc" {
		t.Errorf("returned id: got %q, want msg-abc", id)
	}

	// X-* headers
	if got := gotHeaders.Get("X-Message-ID"); got != "msg-id-1" {
		t.Errorf("X-Message-ID: got %q, want msg-id-1", got)
	}
	if got := gotHeaders.Get("X-User-ID"); got != "user-42" {
		t.Errorf("X-User-ID: got %q, want user-42", got)
	}
	if got := gotHeaders.Get("X-Recipient"); got != "rec-1" {
		t.Errorf("X-Recipient: got %q, want rec-1", got)
	}
	if got := gotHeaders.Get("X-Reply-To"); got != "reply-addr" {
		t.Errorf("X-Reply-To: got %q, want reply-addr", got)
	}
	if got := gotHeaders.Get("X-Correlation-ID"); got != "corr-xyz" {
		t.Errorf("X-Correlation-ID: got %q, want corr-xyz", got)
	}
	if got := gotHeaders.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type: got %q, want application/json", got)
	}
	if got := gotHeaders.Get("Content-Encoding"); got != "utf-8" {
		t.Errorf("Content-Encoding: got %q, want utf-8", got)
	}

	// Body envelope: {"payload": ..., "metaData": {"source": "parity"}}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(gotBody, &envelope); err != nil {
		t.Fatalf("body not valid JSON: %v", err)
	}
	if _, ok := envelope["payload"]; !ok {
		t.Error("body missing 'payload' field")
	}
	if _, ok := envelope["metaData"]; !ok {
		t.Error("body missing 'metaData' field")
	}
}

// TestCreateMessageWithHeaders_EmptyFieldsOmitted verifies that fields
// left as zero value in MessageHeaderInput are NOT set as headers.
func TestCreateMessageWithHeaders_EmptyFieldsOmitted(t *testing.T) {
	var gotHeaders http.Header

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[{"entityIds":["msg-def"],"transactionId":"tx-2"}]`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	_, err := c.CreateMessageWithHeaders(t, "Pub", `{"k":2}`, MessageHeaderInput{
		// No optional fields set — only ContentType will default to application/json
	})
	if err != nil {
		t.Fatalf("CreateMessageWithHeaders: %v", err)
	}

	// Optional X-* headers must be absent
	for _, h := range []string{"X-Message-ID", "X-User-ID", "X-Recipient", "X-Reply-To", "X-Correlation-ID", "Content-Encoding"} {
		if v := gotHeaders.Get(h); v != "" {
			t.Errorf("header %q should be absent for zero-value input, got %q", h, v)
		}
	}

	// Content-Type must default to application/json
	if got := gotHeaders.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type default: got %q, want application/json", got)
	}
}
