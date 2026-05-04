package client_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

func TestDeleteEntitiesByModel_DELETE_NoBody(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"deleteResult":{"numberOfEntitites":0,"numberOfEntititesRemoved":0,"idToError":{}},"entityModelClassId":"abc"}`))
	}))
	defer srv.Close()

	c := client.NewClient(srv.URL, "fake-token")
	if err := c.DeleteEntitiesByModel(t, "family", 1); err != nil {
		t.Fatalf("DeleteEntitiesByModel: %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method: got %q, want DELETE", gotMethod)
	}
	if gotPath != "/api/entity/family/1" {
		t.Errorf("path: got %q, want /api/entity/family/1", gotPath)
	}
	if gotBody != "" {
		t.Errorf("body: got %q, want empty", gotBody)
	}
}

func TestDeleteEntitiesByModelAt_DELETE_PointInTimeQuery(t *testing.T) {
	var gotMethod, gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"deleteResult":{"numberOfEntitites":0,"numberOfEntititesRemoved":0,"idToError":{}},"entityModelClassId":"abc"}`))
	}))
	defer srv.Close()

	c := client.NewClient(srv.URL, "fake-token")
	pit := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	if err := c.DeleteEntitiesByModelAt(t, "family", 1, pit); err != nil {
		t.Fatalf("DeleteEntitiesByModelAt: %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method: got %q, want DELETE", gotMethod)
	}
	if gotPath != "/api/entity/family/1" {
		t.Errorf("path: got %q, want /api/entity/family/1", gotPath)
	}
	// Expect pointInTime=2026-04-24T12:00:00Z in the query string.
	if !strings.Contains(gotQuery, "pointInTime=") {
		t.Errorf("query missing pointInTime: %q", gotQuery)
	}
	if !strings.Contains(gotQuery, "2026-04-24T12") {
		t.Errorf("query missing expected ISO timestamp: %q", gotQuery)
	}
}
