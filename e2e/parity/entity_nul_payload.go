package parity

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// nulEscapePayload is valid JSON whose "name" string carries a NUL (U+0000)
// escape. PostgreSQL text/jsonb cannot represent U+0000, while the memory and
// sqlite stores would accept it — so without a boundary rejection the same
// request succeeds on one backend and fails on another.
const nulEscapePayload = "{\"name\":\"a\\u0000b\",\"amount\":1,\"status\":\"active\"}"

// RunEntityNulPayloadRejected asserts that every backend rejects a payload
// containing U+0000 identically: 400 BAD_REQUEST at the boundary, with nothing
// persisted. This is the cross-backend half of the contract — the per-write-path
// coverage lives in internal/e2e/entity_nul_payload_test.go.
//
// The failure this guards against is not "postgres errors": it is postgres
// erroring while memory and sqlite quietly accept, which makes the storable
// value set backend-dependent.
func RunEntityNulPayloadRejected(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "entity-nul-payload-test"
	const modelVersion = 1

	setupSimpleWorkflow(t, c, modelName, modelVersion)

	status, raw, err := c.CreateEntityRaw(t, modelName, modelVersion, nulEscapePayload)
	if err != nil {
		t.Fatalf("CreateEntityRaw: %v", err)
	}
	if status != http.StatusBadRequest {
		t.Fatalf("NUL payload: status=%d, want 400 — a value no backend can store must be rejected at the boundary, not deep in the store; body: %s",
			status, raw)
	}

	var pd struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(raw, &pd); err != nil {
		t.Fatalf("decode problem detail %q: %v", raw, err)
	}
	if got, _ := pd.Properties["errorCode"].(string); got != "BAD_REQUEST" {
		t.Errorf("errorCode=%q, want BAD_REQUEST; body: %s", got, raw)
	}
	// The offending path must be named, and the raw NUL must not be echoed back.
	if !strings.Contains(string(raw), "name") {
		t.Errorf("rejection does not name the offending field; body: %s", raw)
	}
	if strings.ContainsRune(string(raw), 0) {
		t.Errorf("response body echoes a raw NUL byte; body: %q", raw)
	}

	// A clean payload on the same model must still be accepted, so the check
	// is not rejecting the model outright.
	okStatus, okRaw, err := c.CreateEntityRaw(t, modelName, modelVersion,
		`{"name":"Alice","amount":1,"status":"active"}`)
	if err != nil {
		t.Fatalf("CreateEntityRaw (clean): %v", err)
	}
	if okStatus != http.StatusOK {
		t.Fatalf("clean payload after rejection: status=%d, want 200; body: %s", okStatus, okRaw)
	}
}
