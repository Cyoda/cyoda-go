package parity

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// schema_extension_contract.go — what a request that extends a LOCKED model
// leaves behind, the same on every backend:
//   - the field stays when the write that brought it fails afterwards;
//   - a criterion in the same request can read the field just added;
//   - an extension of a model unlocked mid-request is refused 409
//     MODEL_NOT_LOCKED and the model is unchanged.
// Races are isolated e2e; every scenario here is sequential.

func init() {
	Register(
		NamedTest{Name: "SchemaExtension_KeptAfterFailedWrite", Fn: RunSchemaExtension_KeptAfterFailedWrite},
		NamedTest{Name: "SchemaExtension_CriterionSameRequest", Fn: RunSchemaExtension_CriterionSameRequest},
		NamedTest{Name: "SchemaExtension_NotLocked", Fn: RunSchemaExtension_NotLocked},
	)
}

const schemaExtSample = `{"name":"seed"}`

// schemaExtLockedModel imports schemaExtSample, locks the model, sets
// STRUCTURAL and imports the workflow.
func schemaExtLockedModel(t *testing.T, c *client.Client, model, workflow string) {
	t.Helper()
	setupModelWithWorkflow(t, c, model, 1, schemaExtSample, workflow)
	if err := c.SetChangeLevel(t, model, 1, "STRUCTURAL"); err != nil {
		t.Fatalf("SetChangeLevel STRUCTURAL: %v", err)
	}
}

// requireSimpleView fails unless the model exports as the fold of bodies.
func requireSimpleView(t *testing.T, c *client.Client, model, state string, bodies ...map[string]string) {
	t.Helper()
	got, err := c.ExportModel(t, "SIMPLE_VIEW", model, 1)
	if err != nil {
		t.Fatalf("ExportModel: %v", err)
	}
	want, err := expectedSimpleViewFromBodies(bodies, state)
	if err != nil {
		t.Fatalf("oracle: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("model export\n  got:  %s\n  want: %s", got, want)
	}
}

// RunSchemaExtension_KeptAfterFailedWrite: a processor returns a field the
// model does not hold and a later processor of the same transition fails the
// create. The create is refused, and the model keeps the field.
func RunSchemaExtension_KeptAfterFailedWrite(t *testing.T, fixture BackendFixture) {
	tenant := fixture.ComputeTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)
	const model = "ext-kept-after-fail"

	schemaExtLockedModel(t, c, model, cbWorkflowDoc(model+"-wf", map[string]any{
		"NONE": map[string]any{"transitions": []any{map[string]any{
			"name": "init", "next": "DONE", "manual": false,
			"processors": []any{
				cbProc("tag-with-foo", "SYNC", "", nil),
				cbProc("inject-error", "SYNC", "", nil),
			},
		}}},
		"DONE": map[string]any{},
	}))

	requireSimpleView(t, c, model, "LOCKED", map[string]string{"name": "seed"})

	status, body, err := c.CreateEntityRaw(t, model, 1, `{"name":"a"}`)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if code := problemErrorCode(body); status != http.StatusBadRequest || code != "WORKFLOW_FAILED" {
		t.Fatalf("create answered %d %q; want the failing processor's 400 WORKFLOW_FAILED: %s", status, code, body)
	}

	requireSimpleView(t, c, model, "LOCKED", map[string]string{"name": "seed"}, map[string]string{"name": "a", "tag": "foo"})
}

// RunSchemaExtension_CriterionSameRequest: a criterion on the field a
// processor added earlier in the same request sees that field.
func RunSchemaExtension_CriterionSameRequest(t *testing.T, fixture BackendFixture) {
	tenant := fixture.ComputeTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)
	const model = "ext-criterion-same-request"

	schemaExtLockedModel(t, c, model, cbWorkflowDoc(model+"-wf", map[string]any{
		"NONE": map[string]any{"transitions": []any{map[string]any{
			"name": "init", "next": "TAGGED", "manual": false,
			"processors": []any{cbProc("tag-with-foo", "SYNC", "", nil)},
		}}},
		"TAGGED": map[string]any{"transitions": []any{map[string]any{
			"name": "check", "next": "CHECKED", "manual": false,
			"criterion": map[string]any{"type": "simple", "jsonPath": "$.tag", "operatorType": "EQUALS", "value": "foo"},
		}}},
		"CHECKED": map[string]any{},
	}))

	id, err := c.CreateEntity(t, model, 1, `{"name":"a"}`)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	e, err := c.GetEntity(t, id)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	if e.Meta.State != "CHECKED" || e.Data["tag"] != "foo" {
		t.Fatalf("entity is %s with tag %v; want CHECKED with tag foo", e.Meta.State, e.Data["tag"])
	}
	requireSimpleView(t, c, model, "LOCKED", map[string]string{"name": "seed"}, map[string]string{"name": "a", "tag": "foo"})
}

// RunSchemaExtension_NotLocked: the compute node's processor unlocks the
// model (an unjoined call; the owner's entity is uncommitted, so the model
// has no committed entity) and returns a field the model does not hold. The
// extension finds the model UNLOCKED: the create is 409 MODEL_NOT_LOCKED and
// the model is unchanged.
func RunSchemaExtension_NotLocked(t *testing.T, fixture BackendFixture) {
	tenant := fixture.ComputeTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)
	const model = "ext-not-locked"

	schemaExtLockedModel(t, c, model, cbWorkflowDoc(model+"-wf", map[string]any{
		"NONE": map[string]any{"transitions": []any{map[string]any{
			"name": "init", "next": "DONE", "manual": false,
			"processors": []any{cbProc("cb-unlock-then-tag", "SYNC",
				`{"secondaryModel":"`+model+`","secondaryVersion":1}`, nil)},
		}}},
		"DONE": map[string]any{},
	}))

	status, body, err := c.CreateEntityRaw(t, model, 1, `{"name":"a"}`)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if code := problemErrorCode(body); status != http.StatusConflict || code != "MODEL_NOT_LOCKED" {
		t.Fatalf("create answered %d %q; want 409 MODEL_NOT_LOCKED: %s", status, code, body)
	}

	requireSimpleView(t, c, model, "UNLOCKED", map[string]string{"name": "seed"})
}

// problemErrorCode returns the errorCode property of a problem-details body,
// or "" when it has none.
func problemErrorCode(body []byte) string {
	var pd struct {
		Properties map[string]any `json:"properties"`
	}
	_ = json.Unmarshal(body, &pd)
	code, _ := pd.Properties["errorCode"].(string)
	return code
}
