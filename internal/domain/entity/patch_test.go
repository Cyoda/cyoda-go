package entity

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	wfengine "github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

func TestPatchEntity_MergeAndStrictValidate(t *testing.T) {
	h, ctx := newPatchTestHandler(t)
	id := createPersonEntity(t, h, ctx, map[string]any{"name": "Alice", "age": json.Number("30")})
	txid := getTxID(t, h, ctx, id)

	// Patch only "age"; "name" must be preserved.
	res, err := h.PatchEntity(ctx, PatchEntityInput{
		EntityID:    id,
		Patch:       []byte(`{"age":31}`),
		PatchFormat: "MERGE_PATCH",
		IfMatch:     txid,
	})
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	if len(res.EntityIDs) != 1 {
		t.Fatalf("expected 1 entity id")
	}
	got := getEntityData(t, h, ctx, id)
	if got["name"] != "Alice" || got["age"].(json.Number).String() != "31" {
		t.Errorf("merge wrong: %#v", got)
	}
}

func TestPatchEntity_JSONPatchNotImplemented(t *testing.T) {
	h, ctx := newPatchTestHandler(t)
	id := createPersonEntity(t, h, ctx, map[string]any{"name": "A"})
	_, err := h.PatchEntity(ctx, PatchEntityInput{
		EntityID: id, Patch: []byte(`[]`), PatchFormat: "JSON_PATCH", IfMatch: "*",
	})
	assertStatus(t, err, http.StatusNotImplemented)
}

func TestPatchEntity_StrictRejectsNewField(t *testing.T) {
	h, ctx := newPatchTestHandler(t) // model is locked & strict (ChangeLevel "")
	id := createPersonEntity(t, h, ctx, map[string]any{"name": "A"})
	_, err := h.PatchEntity(ctx, PatchEntityInput{
		EntityID: id, Patch: []byte(`{"newfield":"x"}`), PatchFormat: "MERGE_PATCH", IfMatch: "*",
	})
	assertStatus(t, err, http.StatusBadRequest)
}

func TestPatchEntity_StaleTokenIs412(t *testing.T) {
	h, ctx := newPatchTestHandler(t)
	id := createPersonEntity(t, h, ctx, map[string]any{"name": "A", "age": json.Number("1")})
	stale := getTxID(t, h, ctx, id)
	// Advance the entity so the token goes stale.
	if _, err := h.PatchEntity(ctx, PatchEntityInput{EntityID: id, Patch: []byte(`{"age":2}`), PatchFormat: "MERGE_PATCH", IfMatch: stale}); err != nil {
		t.Fatalf("first patch: %v", err)
	}
	_, err := h.PatchEntity(ctx, PatchEntityInput{EntityID: id, Patch: []byte(`{"age":3}`), PatchFormat: "MERGE_PATCH", IfMatch: stale})
	assertStatus(t, err, http.StatusPreconditionFailed)
}

func TestPatchEntity_StarIsUnconditional(t *testing.T) {
	h, ctx := newPatchTestHandler(t)
	id := createPersonEntity(t, h, ctx, map[string]any{"name": "A", "age": json.Number("1")})
	if _, err := h.PatchEntity(ctx, PatchEntityInput{EntityID: id, Patch: []byte(`{"age":2}`), PatchFormat: "MERGE_PATCH", IfMatch: "*"}); err != nil {
		t.Fatalf("star patch should succeed unconditionally: %v", err)
	}
}

// --- helpers ---

// newPatchTestHandler builds a Handler wired to a fresh in-memory store with a
// real workflow engine, and a context carrying the test user. The model "Person/1"
// has schema {name: String, age: Integer}, is locked, and has ChangeLevel == ""
// (strict — never extends).
func newPatchTestHandler(t *testing.T) (*Handler, context.Context) {
	t.Helper()

	factory := memory.NewStoreFactory()
	ctx := spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID:   "patch-test-user",
		UserName: "Patch Test",
		Tenant:   spi.Tenant{ID: "patch-tenant", Name: "Patch"},
		Roles:    []string{"user"},
	})

	txMgr, err := factory.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}

	engine := wfengine.NewEngine(factory, common.NewDefaultUUIDGenerator(), txMgr)
	h := New(factory, txMgr, common.NewDefaultUUIDGenerator(), engine, txgate.New())

	// Build a strict (ChangeLevel == "") schema for Person: {name: String, age: Integer}.
	node := schema.NewObjectNode()
	node.SetChild("name", schema.NewLeafNode(schema.String))
	node.SetChild("age", schema.NewLeafNode(schema.Integer))
	raw, mErr := schema.Marshal(node)
	if mErr != nil {
		t.Fatalf("schema.Marshal: %v", mErr)
	}

	modelStore, err := factory.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	if err := modelStore.Save(ctx, &spi.ModelDescriptor{
		Ref:    spi.ModelRef{EntityName: "Person", ModelVersion: "1"},
		State:  spi.ModelLocked,
		Schema: raw,
		// ChangeLevel == "" means strict-validate-only (never extends the schema).
	}); err != nil {
		t.Fatalf("ModelStore.Save: %v", err)
	}

	return h, ctx
}

// createPersonEntity creates a Person entity with the given data fields and
// returns its entity ID.
func createPersonEntity(t *testing.T, h *Handler, ctx context.Context, data map[string]any) string {
	t.Helper()
	b, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal entity data: %v", err)
	}
	res, err := h.CreateEntity(ctx, CreateEntityInput{
		EntityName:   "Person",
		ModelVersion: "1",
		Format:       "JSON",
		Data:         json.RawMessage(b),
	})
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	if len(res.EntityIDs) != 1 {
		t.Fatalf("expected 1 entity ID from CreateEntity, got %d", len(res.EntityIDs))
	}
	return res.EntityIDs[0]
}

// getTxID retrieves the current transactionId from the entity's meta.
func getTxID(t *testing.T, h *Handler, ctx context.Context, entityID string) string {
	t.Helper()
	env, err := h.GetEntity(ctx, GetOneEntityInput{EntityID: entityID})
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	meta, ok := env.Meta["transactionId"]
	if !ok {
		t.Fatalf("entity meta has no transactionId: %#v", env.Meta)
	}
	txid, ok := meta.(string)
	if !ok || txid == "" {
		t.Fatalf("transactionId is not a non-empty string: %v", meta)
	}
	return txid
}

// getEntityData returns the data map from the entity, with json.Number for numerics.
func getEntityData(t *testing.T, h *Handler, ctx context.Context, entityID string) map[string]any {
	t.Helper()
	env, err := h.GetEntity(ctx, GetOneEntityInput{EntityID: entityID})
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	data, ok := env.Data.(map[string]any)
	if !ok {
		t.Fatalf("entity data is not map[string]any: %T %#v", env.Data, env.Data)
	}
	return data
}

// assertStatus checks that err is a *common.AppError with the given HTTP status.
func assertStatus(t *testing.T, err error, want int) {
	t.Helper()
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected AppError, got %v", err)
	}
	if appErr.Status != want {
		t.Fatalf("status: got %d want %d (%s)", appErr.Status, want, appErr.Message)
	}
}
