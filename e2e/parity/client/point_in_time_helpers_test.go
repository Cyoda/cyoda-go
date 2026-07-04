package client_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// TestGetEntityByTransactionID asserts the helper issues
// GET /api/entity/{id}?transactionId=<tx> and decodes the EntityResult
// envelope returned by the server.
func TestGetEntityByTransactionID(t *testing.T) {
	var gotMethod, gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"type": "ENTITY",
			"data": {"k": 7},
			"meta": {
				"id": "00000000-0000-0000-0000-000000000001",
				"state": "ACTIVE",
				"creationDate": "2025-01-01T00:00:00Z",
				"lastUpdateTime": "2025-01-01T00:00:00Z",
				"transactionId": "tx-abc-123"
			}
		}`))
	}))
	defer srv.Close()
	c := client.NewClient(srv.URL, "tok")
	id := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	ent, err := c.GetEntityByTransactionID(t, id, "tx-abc-123")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method: got %q want GET", gotMethod)
	}
	if gotPath != "/api/entity/"+id.String() {
		t.Errorf("path: got %q", gotPath)
	}
	if !strings.Contains(gotQuery, "transactionId=tx-abc-123") {
		t.Errorf("query: got %q want transactionId=tx-abc-123", gotQuery)
	}
	if ent.Type != "ENTITY" {
		t.Errorf("type: got %q want ENTITY", ent.Type)
	}
	if ent.Meta.TransactionID != "tx-abc-123" {
		t.Errorf("meta.transactionId: got %q want tx-abc-123", ent.Meta.TransactionID)
	}
	if v, ok := ent.Data["k"]; !ok || v != float64(7) {
		t.Errorf("data.k: got %v", ent.Data["k"])
	}
}

// TestGetEntityByTransactionIDRaw asserts the raw helper returns the
// HTTP status code + body bytes without erroring on non-2xx, mirroring
// the *Raw pattern of LockModelRaw.
func TestGetEntityByTransactionIDRaw(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		var gotMethod, gotPath, gotQuery string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod = r.Method
			gotPath = r.URL.Path
			gotQuery = r.URL.RawQuery
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"type":"ENTITY"}`))
		}))
		defer srv.Close()
		c := client.NewClient(srv.URL, "tok")
		id := uuid.New()
		status, body, err := c.GetEntityByTransactionIDRaw(t, id, "tx-1")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if gotMethod != http.MethodGet {
			t.Errorf("method: got %q", gotMethod)
		}
		if gotPath != "/api/entity/"+id.String() {
			t.Errorf("path: got %q", gotPath)
		}
		if !strings.Contains(gotQuery, "transactionId=tx-1") {
			t.Errorf("query: got %q", gotQuery)
		}
		if status != http.StatusOK {
			t.Errorf("status: got %d", status)
		}
		if len(body) == 0 {
			t.Error("expected non-empty body")
		}
	})

	t.Run("not_found", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"type":"about:blank","status":404,"properties":{"errorCode":"ENTITY_NOT_FOUND"}}`))
		}))
		defer srv.Close()
		c := client.NewClient(srv.URL, "tok")
		id := uuid.New()
		status, body, err := c.GetEntityByTransactionIDRaw(t, id, "bogus-tx")
		if err != nil {
			t.Fatalf("err on 404: %v (must return raw, not error)", err)
		}
		if status != http.StatusNotFound {
			t.Errorf("status: got %d want 404", status)
		}
		if !strings.Contains(string(body), "ENTITY_NOT_FOUND") {
			t.Errorf("body: got %q", string(body))
		}
	})
}

// TestGetEntityChangesAt asserts the helper issues
// GET /api/entity/{id}/changes?pointInTime=<ISO> and decodes the
// []EntityChangeMeta array returned by the server.
func TestGetEntityChangesAt(t *testing.T) {
	var gotMethod, gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[
			{"timeOfChange":"2025-01-01T00:00:00Z","user":"alice","changeType":"CREATE","transactionId":"tx-1"},
			{"timeOfChange":"2025-01-01T00:00:01Z","user":"alice","changeType":"UPDATE","transactionId":"tx-2"}
		]`))
	}))
	defer srv.Close()
	c := client.NewClient(srv.URL, "tok")
	id := uuid.New()
	pit := time.Date(2025, 1, 1, 0, 0, 1, 0, time.UTC)
	changes, err := c.GetEntityChangesAt(t, id, pit)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method: got %q want GET", gotMethod)
	}
	if gotPath != "/api/entity/"+id.String()+"/changes" {
		t.Errorf("path: got %q", gotPath)
	}
	// pointInTime is RFC3339Nano-formatted; we don't pin the exact encoded string,
	// just that the parameter is present.
	if !strings.Contains(gotQuery, "pointInTime=") {
		t.Errorf("query: got %q want pointInTime=...", gotQuery)
	}
	if len(changes) != 2 {
		t.Fatalf("changes: got %d want 2", len(changes))
	}
	if changes[0].ChangeType != "CREATE" {
		t.Errorf("changes[0].changeType: got %q", changes[0].ChangeType)
	}
	if changes[1].TransactionID != "tx-2" {
		t.Errorf("changes[1].transactionId: got %q", changes[1].TransactionID)
	}
}

// TestGetEntityAtRaw asserts the (refactored) raw helper returns
// HTTP status + body bytes for GET /api/entity/{id}?pointInTime=<ISO>
// without erroring on non-2xx — needed by 12_04 to assert the 404 body.
func TestGetEntityAtRaw(t *testing.T) {
	t.Run("not_found_returns_body", func(t *testing.T) {
		var gotMethod, gotPath, gotQuery string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod = r.Method
			gotPath = r.URL.Path
			gotQuery = r.URL.RawQuery
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"type":"about:blank","status":404,"properties":{"errorCode":"ENTITY_NOT_FOUND"}}`))
		}))
		defer srv.Close()
		c := client.NewClient(srv.URL, "tok")
		id := uuid.New()
		pit := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
		status, body, err := c.GetEntityAtRaw(t, id, pit)
		if err != nil {
			t.Fatalf("err on 404: %v (must return raw, not error)", err)
		}
		if gotMethod != http.MethodGet {
			t.Errorf("method: got %q", gotMethod)
		}
		if gotPath != "/api/entity/"+id.String() {
			t.Errorf("path: got %q", gotPath)
		}
		if !strings.Contains(gotQuery, "pointInTime=") {
			t.Errorf("query: got %q want pointInTime=...", gotQuery)
		}
		if status != http.StatusNotFound {
			t.Errorf("status: got %d want 404", status)
		}
		if !strings.Contains(string(body), "ENTITY_NOT_FOUND") {
			t.Errorf("body: got %q", string(body))
		}
	})

	t.Run("success_returns_body", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"type":"ENTITY"}`))
		}))
		defer srv.Close()
		c := client.NewClient(srv.URL, "tok")
		id := uuid.New()
		pit := time.Now().UTC()
		status, body, err := c.GetEntityAtRaw(t, id, pit)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if status != http.StatusOK {
			t.Errorf("status: got %d", status)
		}
		if !strings.Contains(string(body), "ENTITY") {
			t.Errorf("body: got %q", string(body))
		}
	})
}
