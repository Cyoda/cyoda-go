package parity

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// RunUnknownModel404 asserts that a model-scoped operation against a model
// that has never been registered returns 404 MODEL_NOT_FOUND on every
// backend. Uses GET /api/entity/stats/{name}/{version} as the representative
// operation because it requires no request body and is gated by
// common.EnsureModelRegistered — guaranteeing the model check runs before
// any query work. Every backend (memory / sqlite / postgres / commercial)
// must return 404 MODEL_NOT_FOUND for a model name that was never imported.
func RunUnknownModel404(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	// UUID suffix guarantees the model name has never been imported in any
	// prior test on this backend, even on a shared long-running instance.
	unknownModel := "never-registered-" + uuid.NewString()
	path := fmt.Sprintf("/api/entity/stats/%s/1", unknownModel)

	status, body, err := c.DoJSONBodyRaw(t, http.MethodGet, path, nil)
	if err != nil && status == 0 {
		t.Fatalf("transport error: %v", err)
	}
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", status, string(body))
	}
	assertErrCode(t, body, "MODEL_NOT_FOUND")
}

// assertErrCode asserts that raw decodes as an RFC-9457 ProblemDetail
// envelope carrying wantCode in properties.errorCode.
func assertErrCode(t *testing.T, raw []byte, wantCode string) {
	t.Helper()
	var envelope struct {
		Properties struct {
			ErrorCode string `json:"errorCode"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Errorf("assertErrCode: failed to decode error body %q: %v", string(raw), err)
		return
	}
	if envelope.Properties.ErrorCode != wantCode {
		t.Errorf("properties.errorCode: got %q, want %q (body: %s)", envelope.Properties.ErrorCode, wantCode, string(raw))
	}
}
