package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	genapi "github.com/cyoda/cyoda-go/api"
	internalapi "github.com/cyoda/cyoda-go/internal/api"
	"github.com/cyoda/cyoda-go/internal/common"
)

// Compile-time check: Unimplemented must satisfy ServerInterface.
var _ genapi.ServerInterface = (*internalapi.Unimplemented)(nil)

func TestUnimplementedReturns501(t *testing.T) {
	u := &internalapi.Unimplemented{}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/entity/test-id", nil)
	u.GetOneEntity(w, r, [16]byte{}, genapi.GetOneEntityParams{})
	if w.Code != http.StatusNotImplemented {
		t.Errorf("expected 501, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("expected application/problem+json, got %s", ct)
	}
}

func TestUnimplementedUsesNotImplementedErrorCode(t *testing.T) {
	u := &internalapi.Unimplemented{}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/entity/test-id", nil)
	u.GetOneEntity(w, r, [16]byte{}, genapi.GetOneEntityParams{})

	var pd map[string]any
	if err := json.NewDecoder(w.Body).Decode(&pd); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	props, ok := pd["properties"].(map[string]any)
	if !ok {
		t.Fatal("expected properties in problem detail")
	}
	errorCode, ok := props["errorCode"].(string)
	if !ok {
		t.Fatal("expected errorCode in properties")
	}
	if errorCode != common.ErrCodeNotImplemented {
		t.Errorf("expected error code %q, got %q", common.ErrCodeNotImplemented, errorCode)
	}
}
