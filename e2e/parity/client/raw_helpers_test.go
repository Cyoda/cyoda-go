package client_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/cyoda/cyoda-go/e2e/parity/client"
)

func TestSetChangeLevelRaw(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"type":"about:blank","status":400,"properties":{"errorCode":"INVALID_ENUM"}}`))
	}))
	defer srv.Close()
	c := client.NewClient(srv.URL, "tok")
	status, body, err := c.SetChangeLevelRaw(t, "m", 1, "wrong")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method: got %q", gotMethod)
	}
	if gotPath != "/api/model/m/1/changeLevel/wrong" {
		t.Errorf("path: got %q", gotPath)
	}
	if status != http.StatusBadRequest {
		t.Errorf("status: got %d", status)
	}
	if len(body) == 0 {
		t.Error("expected body returned")
	}
}

func TestImportModelRaw(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := client.NewClient(srv.URL, "tok")
	status, _, err := c.ImportModelRaw(t, "m", 1, `{"a":1}`)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/model/import/JSON/SAMPLE_DATA/m/1" {
		t.Errorf("got %s %s", gotMethod, gotPath)
	}
	if gotBody != `{"a":1}` {
		t.Errorf("body: got %q", gotBody)
	}
	if status != http.StatusOK {
		t.Errorf("status: got %d", status)
	}
}

func TestUpdateEntityRaw(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := client.NewClient(srv.URL, "tok")
	id := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	status, _, err := c.UpdateEntityRaw(t, id, "BadTransition", `{"k":1}`)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if gotMethod != http.MethodPut {
		t.Errorf("method: got %q", gotMethod)
	}
	if gotPath != "/api/entity/JSON/00000000-0000-0000-0000-000000000001/BadTransition" {
		t.Errorf("path: got %q", gotPath)
	}
	if status != http.StatusBadRequest {
		t.Errorf("status: got %d", status)
	}
}

func TestGetEntityChangesRaw(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := client.NewClient(srv.URL, "tok")
	id := uuid.New()
	status, _, err := c.GetEntityChangesRaw(t, id)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method: got %q", gotMethod)
	}
	if gotPath != "/api/entity/"+id.String()+"/changes" {
		t.Errorf("path: got %q", gotPath)
	}
	if status != http.StatusNotFound {
		t.Errorf("status: got %d", status)
	}
}

func TestImportWorkflowRaw(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := client.NewClient(srv.URL, "tok")
	status, _, err := c.ImportWorkflowRaw(t, "m", 1, `{"workflows":[]}`)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method: got %q", gotMethod)
	}
	if gotPath != "/api/model/m/1/workflow/import" {
		t.Errorf("path: got %q", gotPath)
	}
	if gotBody != `{"workflows":[]}` {
		t.Errorf("body: got %q", gotBody)
	}
	if status != http.StatusNotFound {
		t.Errorf("status: got %d", status)
	}
}

func TestSyncSearchRaw(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"type":"about:blank","status":400,"properties":{"errorCode":"INVALID_CONDITION"}}`))
	}))
	defer srv.Close()
	c := client.NewClient(srv.URL, "tok")
	status, body, err := c.SyncSearchRaw(t, "orders", 2, `{"type":"group","conditions":[]}`)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method: got %q want POST", gotMethod)
	}
	if gotPath != "/api/search/direct/orders/2" {
		t.Errorf("path: got %q want /api/search/direct/orders/2", gotPath)
	}
	if gotBody != `{"type":"group","conditions":[]}` {
		t.Errorf("body: got %q", gotBody)
	}
	if status != http.StatusBadRequest {
		t.Errorf("status: got %d want 400", status)
	}
	if len(body) == 0 {
		t.Error("expected non-empty body")
	}
}

func TestSubmitAsyncSearchRaw(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"type":"about:blank","status":400,"properties":{"errorCode":"INVALID_CONDITION"}}`))
	}))
	defer srv.Close()
	c := client.NewClient(srv.URL, "tok")
	status, body, err := c.SubmitAsyncSearchRaw(t, "orders", 2, `{"type":"group","conditions":[]}`)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method: got %q want POST", gotMethod)
	}
	if gotPath != "/api/search/async/orders/2" {
		t.Errorf("path: got %q want /api/search/async/orders/2", gotPath)
	}
	if gotBody != `{"type":"group","conditions":[]}` {
		t.Errorf("body: got %q", gotBody)
	}
	if status != http.StatusBadRequest {
		t.Errorf("status: got %d want 400", status)
	}
	if len(body) == 0 {
		t.Error("expected non-empty body")
	}
}
