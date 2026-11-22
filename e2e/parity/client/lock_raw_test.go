package client_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cyoda/cyoda-go/e2e/parity/client"
)

func TestLockModelRaw_PUTs_NoBody(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{
			"type":"about:blank","title":"Conflict","status":409,
			"detail":"already locked","instance":"/api/model/x/1/lock",
			"properties":{"errorCode":"MODEL_ALREADY_LOCKED","retryable":false}
		}`))
	}))
	defer srv.Close()

	c := client.NewClient(srv.URL, "fake-token")
	status, body, err := c.LockModelRaw(t, "x", 1)
	if err != nil {
		t.Fatalf("LockModelRaw: %v", err)
	}
	if gotMethod != http.MethodPut {
		t.Errorf("method: got %q, want PUT", gotMethod)
	}
	if gotPath != "/api/model/x/1/lock" {
		t.Errorf("path: got %q, want /api/model/x/1/lock", gotPath)
	}
	if gotBody != "" {
		t.Errorf("body: got %q, want empty", gotBody)
	}
	if status != http.StatusConflict {
		t.Errorf("status: got %d, want 409", status)
	}
	if len(body) == 0 {
		t.Error("expected body to be returned for caller-side parsing")
	}
}
