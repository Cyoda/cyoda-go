package parity

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// RunTransitionNameNULRefused verifies that a transition name holding U+0000,
// sent percent-encoded in the path, is refused identically on every backend:
// 400 BAD_REQUEST, the raw U+0000 not echoed, and the entity unchanged. The
// name reaches the run's audit events; PostgreSQL refuses U+0000 in jsonb, so
// the operation's transaction would abort there, while memory and SQLite would
// store it. Refusing the name before the run records anything gives every
// backend the same result.
func RunTransitionNameNULRefused(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "transition-name-nul-test"
	const modelVersion = 1
	setupSimpleWorkflow(t, c, modelName, modelVersion)

	id, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"A","amount":1,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	before, err := c.GetEntity(t, id)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}

	status, raw, err := c.DoRaw(t, http.MethodPut, "/api/entity/JSON/"+id.String()+"/g%00o", `{"name":"A","amount":2,"status":"new"}`)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", status, raw)
	}
	var pd struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(raw, &pd); err != nil {
		t.Fatalf("decode problem detail %q: %v", raw, err)
	}
	if got, _ := pd.Properties["errorCode"].(string); got != "BAD_REQUEST" {
		t.Errorf("errorCode = %q, want BAD_REQUEST; body: %s", got, raw)
	}
	if strings.ContainsRune(string(raw), 0) {
		t.Errorf("response body echoes a raw U+0000; body: %q", raw)
	}

	after, err := c.GetEntity(t, id)
	if err != nil {
		t.Fatalf("GetEntity after: %v", err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Errorf("entity changed: %+v, want %+v", after, before)
	}
}
