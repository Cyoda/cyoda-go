package entity

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	genapi "github.com/cyoda-platform/cyoda-go/api"
)

const sampleUUID = "1d1e1b10-1155-11f0-bcd5-ae468cd3ed16"

func TestPatch_XMLFormatIs415(t *testing.T) {
	h, _ := newPatchTestHandler(t)
	r := httptest.NewRequest(http.MethodPatch, "/entity/XML/"+sampleUUID, strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "application/merge-patch+json")
	r.Header.Set("If-Match", "*")
	w := httptest.NewRecorder()
	h.PatchSingleWithLoopback(w, r, "XML", openapi_types.UUID(mustUUID(sampleUUID)), genapiPatchLoopbackParams("*"))
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("got %d", w.Code)
	}
}

func TestPatch_BadContentTypeIs415(t *testing.T) {
	h, _ := newPatchTestHandler(t)
	r := httptest.NewRequest(http.MethodPatch, "/entity/JSON/"+sampleUUID, strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.PatchSingleWithLoopback(w, r, "JSON", openapi_types.UUID(mustUUID(sampleUUID)), genapiPatchLoopbackParams("*"))
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("got %d", w.Code)
	}
}

func TestPatch_MissingIfMatchIs428(t *testing.T) {
	h, _ := newPatchTestHandler(t)
	r := httptest.NewRequest(http.MethodPatch, "/entity/JSON/"+sampleUUID, strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "application/merge-patch+json")
	w := httptest.NewRecorder()
	h.PatchSingleWithLoopback(w, r, "JSON", openapi_types.UUID(mustUUID(sampleUUID)), genapiPatchLoopbackParams(""))
	if w.Code != http.StatusPreconditionRequired {
		t.Fatalf("got %d", w.Code)
	}
}

// --- helpers ---

// genapiPatchLoopbackParams returns PatchSingleWithLoopbackParams with IfMatch
// set to &ifMatch when ifMatch is non-empty, or nil when empty.
func genapiPatchLoopbackParams(ifMatch string) genapi.PatchSingleWithLoopbackParams {
	if ifMatch == "" {
		return genapi.PatchSingleWithLoopbackParams{IfMatch: nil}
	}
	return genapi.PatchSingleWithLoopbackParams{IfMatch: &ifMatch}
}

// mustUUID parses a UUID string and panics if invalid.
func mustUUID(s string) uuid.UUID {
	u, err := uuid.Parse(s)
	if err != nil {
		panic("invalid UUID: " + s)
	}
	return u
}
