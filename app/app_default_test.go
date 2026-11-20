package app_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cyoda/cyoda-go/app"

	_ "github.com/cyoda/cyoda-go/plugins/memory"
)

func TestApp_DefaultMemoryBackend(t *testing.T) {
	cfg := app.DefaultConfig()
	cfg.ContextPath = ""
	a := app.New(cfg)

	if a.StoreFactory() == nil {
		t.Fatal("expected non-nil StoreFactory")
	}
	if a.TransactionManager() == nil {
		t.Fatal("expected non-nil TransactionManager")
	}

	srv := httptest.NewServer(a.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("health check failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}
