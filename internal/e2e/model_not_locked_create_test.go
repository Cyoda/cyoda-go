package e2e_test

// model_not_locked_create_test.go — an entity is created only in a LOCKED
// model. A create into a model that was imported and never locked is
// refused with 409 MODEL_NOT_LOCKED, on the single and the collection door,
// and writes nothing.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// importUnlockedModel imports model from sample data and leaves it unlocked.
func importUnlockedModel(t *testing.T, model string) {
	t.Helper()
	resp := doAuth(t, http.MethodPost, fmt.Sprintf("/api/model/import/JSON/SAMPLE_DATA/%s/1", model), `{"x":1}`)
	if body := readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("import model %s: %d %s", model, resp.StatusCode, body)
	}
}

// requireNoEntities fails the test unless model has no entities.
func requireNoEntities(t *testing.T, model string) {
	t.Helper()
	resp := doAuth(t, http.MethodGet, fmt.Sprintf("/api/entity/%s/1", model), "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list entities of %s: %d %s", model, resp.StatusCode, body)
	}
	var list []any
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("list entities of %s: %v: %s", model, err, body)
	}
	if len(list) != 0 {
		t.Fatalf("model %s has %d entities, want 0: %s", model, len(list), body)
	}
}

// TestCreate_ModelNeverLocked_409 — POST /api/entity/{format}/{name}/{version}
// into a model that was never locked.
func TestCreate_ModelNeverLocked_409(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	model := "never-locked-" + uuid.NewString()
	importUnlockedModel(t, model)

	resp := doAuth(t, http.MethodPost, fmt.Sprintf("/api/entity/JSON/%s/1", model), `{"x":2}`)
	assertProblemJSON(t, resp, http.StatusConflict, "MODEL_NOT_LOCKED")
	requireNoEntities(t, model)
}

// TestCreateCollection_ModelNeverLocked_409 — POST /api/entity/{format} with
// a first item into a LOCKED model and a second into a model that was never
// locked: the whole request is refused and neither item is written.
func TestCreateCollection_ModelNeverLocked_409(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	locked := "locked-" + uuid.NewString()
	importModel(t, locked, 1)
	unlocked := "never-locked-" + uuid.NewString()
	importUnlockedModel(t, unlocked)

	body := fmt.Sprintf(`[{"model":{"name":%q,"version":1},"payload":"{\"x\":2}"},`+
		`{"model":{"name":%q,"version":1},"payload":"{\"x\":3}"}]`, locked, unlocked)
	resp := doAuth(t, http.MethodPost, "/api/entity/JSON", body)
	assertProblemJSON(t, resp, http.StatusConflict, "MODEL_NOT_LOCKED")
	requireNoEntities(t, locked)
	requireNoEntities(t, unlocked)
}
