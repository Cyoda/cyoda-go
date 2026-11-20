package driver_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cyoda/cyoda-go/e2e/externalapi/driver"
)

func TestDriver_SubmitAsyncSearch_POST(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`"job-x"`))
	}))
	defer srv.Close()

	d := driver.NewRemote(t, srv.URL, "tok")
	jobID, err := d.SubmitAsyncSearch("orders", 1, `{}`)
	if err != nil {
		t.Fatalf("SubmitAsyncSearch: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method: got %q want POST", gotMethod)
	}
	if gotPath != "/api/search/async/orders/1" {
		t.Errorf("path: got %q", gotPath)
	}
	if jobID != "job-x" {
		t.Errorf("jobID: got %q want job-x", jobID)
	}
}

func TestDriver_GetAsyncSearchStatus_GET(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"searchJobStatus":"RUNNING"}`))
	}))
	defer srv.Close()

	d := driver.NewRemote(t, srv.URL, "tok")
	status, err := d.GetAsyncSearchStatus("job-x")
	if err != nil {
		t.Fatalf("GetAsyncSearchStatus: %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method: got %q want GET", gotMethod)
	}
	if gotPath != "/api/search/async/job-x/status" {
		t.Errorf("path: got %q", gotPath)
	}
	if status != "RUNNING" {
		t.Errorf("status: got %q want RUNNING", status)
	}
}

func TestDriver_GetAsyncSearchResults_GET(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"content":[],"page":{"totalElements":0}}`))
	}))
	defer srv.Close()

	d := driver.NewRemote(t, srv.URL, "tok")
	page, err := d.GetAsyncSearchResults("job-x")
	if err != nil {
		t.Fatalf("GetAsyncSearchResults: %v", err)
	}
	if page.Page.TotalElements != 0 {
		t.Errorf("totalElements: got %d want 0", page.Page.TotalElements)
	}
}

func TestDriver_CancelAsyncSearch_PUT(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"isCancelled":true,"cancelled":true,"currentSearchJobStatus":"CANCELLED"}`))
	}))
	defer srv.Close()

	d := driver.NewRemote(t, srv.URL, "tok")
	if err := d.CancelAsyncSearch("job-x"); err != nil {
		t.Fatalf("CancelAsyncSearch: %v", err)
	}
	if gotMethod != http.MethodPut {
		t.Errorf("method: got %q want PUT", gotMethod)
	}
	if gotPath != "/api/search/async/job-x/cancel" {
		t.Errorf("path: got %q", gotPath)
	}
}

func TestDriver_AwaitAsyncSearchResults_Success(t *testing.T) {
	var statusCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		switch {
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/search/async/"):
			_, _ = w.Write([]byte(`"job-drv"`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/status"):
			n := statusCalls.Add(1)
			if n < 2 {
				_, _ = w.Write([]byte(`{"searchJobStatus":"RUNNING"}`))
			} else {
				_, _ = w.Write([]byte(`{"searchJobStatus":"SUCCESSFUL"}`))
			}
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"content":[],"page":{"totalElements":0}}`))
		}
	}))
	defer srv.Close()

	d := driver.NewRemote(t, srv.URL, "tok")
	page, err := d.AwaitAsyncSearchResults("orders", 1, `{}`, 5*time.Second)
	if err != nil {
		t.Fatalf("AwaitAsyncSearchResults: %v", err)
	}
	if page.Page.TotalElements != 0 {
		t.Errorf("totalElements: got %d want 0", page.Page.TotalElements)
	}
}
