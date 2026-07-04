package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
	"github.com/google/uuid"
)

// TestDeleteEntities_Conditional_SubsetSurvives asserts a condition-scoped
// delete removes only matching entities and leaves the rest (E2 data-loss fix).
func TestDeleteEntities_Conditional_SubsetSurvives(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	const model = "e2e-delcond-1"
	importModelWithSample(t, model, 1, `{"status":"sample","n":0}`)
	lockModelE2E(t, model, 1)
	keepID := createEntityE2E(t, model, 1, `{"status":"keep","n":1}`)
	dropID := createEntityE2E(t, model, 1, `{"status":"drop","n":2}`)

	// Delete only status=drop. AbstractConditionDto simple-condition form.
	cond := `{"type":"simple","jsonPath":"$.status","operatorType":"EQUALS","value":"drop"}`
	resp := doAuth(t, http.MethodDelete, fmt.Sprintf("/api/entity/%s/1?verbose=true", model), cond)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("conditional delete: %d: %s", resp.StatusCode, body)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode: %v: %s", err, body)
	}
	dr := obj["deleteResult"].(map[string]any)
	if got := dr["numberOfEntititesRemoved"]; got != float64(1) {
		t.Errorf("removed = %v, want 1", got)
	}
	// verbose ids must contain the dropped id, not the kept one.
	ids, _ := obj["ids"].([]any)
	if len(ids) != 1 || ids[0] != dropID {
		t.Errorf("verbose ids = %v, want [%s]", ids, dropID)
	}

	// Kept entity still readable; dropped entity is gone.
	if r := doAuth(t, http.MethodGet, "/api/entity/"+keepID, ""); r.StatusCode != http.StatusOK {
		t.Errorf("kept entity should survive, got %d", r.StatusCode)
	}
	if r := doAuth(t, http.MethodGet, "/api/entity/"+dropID, ""); r.StatusCode != http.StatusNotFound {
		t.Errorf("dropped entity should be gone, got %d", r.StatusCode)
	}
}

// TestDeleteEntities_InvalidCondition asserts a malformed condition body → 400 INVALID_CONDITION.
func TestDeleteEntities_InvalidCondition(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	const model = "e2e-delcond-2"
	importModel(t, model, 1)
	resp := doAuth(t, http.MethodDelete, fmt.Sprintf("/api/entity/%s/1", model), `{"type":"NONSENSE"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed condition: expected 400, got %d: %s", resp.StatusCode, readBody(t, resp))
	}
	commontest.ExpectErrorCode(t, resp, "INVALID_CONDITION")
}

// TestDeleteEntities_UnknownModel asserts deleting an unregistered model → 404 MODEL_NOT_FOUND.
func TestDeleteEntities_UnknownModel(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	resp := doAuth(t, http.MethodDelete, "/api/entity/never-registered-"+uuid.NewString()+"/1", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown model delete: expected 404, got %d", resp.StatusCode)
	}
	commontest.ExpectErrorCode(t, resp, "MODEL_NOT_FOUND")
}

// TestDeleteEntities_Conditional_OverThousandMatches verifies that a
// condition-scoped delete removes ALL matching entities when the match set
// exceeds the in-memory search fallback's default 1000-entity cap. Prior to
// the fix, scoped deletes were silently capped at 1000 and returned a
// MatchedCount of 1000 even when more entities matched.
func TestDeleteEntities_Conditional_OverThousandMatches(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	const (
		model = "e2e-delcond-over1k"
		total = 1050
		chunk = 350 // three batches of 350 keep each transactionWindow safe
	)

	importModelWithSample(t, model, 1, `{"status":"sample","n":0}`)
	lockModelE2E(t, model, 1)

	// Build and POST three batches of 350 entities (1050 total) all with
	// status="drop". transactionWindow=350 keeps each batch within one TX.
	batchPath := fmt.Sprintf("/api/entity/JSON/%s/1?transactionWindow=%d", model, chunk)
	for batchStart := 0; batchStart < total; batchStart += chunk {
		batchEnd := batchStart + chunk
		if batchEnd > total {
			batchEnd = total
		}
		items := make([]string, 0, batchEnd-batchStart)
		for i := batchStart; i < batchEnd; i++ {
			items = append(items, fmt.Sprintf(`{"status":"drop","n":%d}`, i))
		}
		body := "[" + strings.Join(items, ",") + "]"
		resp := doAuth(t, http.MethodPost, batchPath, body)
		respBody := readBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("batch create [%d,%d): expected 200, got %d: %s", batchStart, batchEnd, resp.StatusCode, respBody)
		}
	}

	// Single conditional delete — must remove all 1050 entities.
	cond := `{"type":"simple","jsonPath":"$.status","operatorType":"EQUALS","value":"drop"}`
	resp := doAuth(t, http.MethodDelete, fmt.Sprintf("/api/entity/%s/1", model), cond)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("conditional delete: expected 200, got %d: %s", resp.StatusCode, body)
	}

	var obj map[string]any
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode delete response: %v: %s", err, body)
	}
	dr, ok := obj["deleteResult"].(map[string]any)
	if !ok {
		t.Fatalf("deleteResult missing from response: %s", body)
	}
	if got := dr["numberOfEntitites"]; got != float64(total) {
		t.Errorf("numberOfEntitites = %v, want %d (scoped delete capped at 1000?)", got, total)
	}
	if got := dr["numberOfEntititesRemoved"]; got != float64(total) {
		t.Errorf("numberOfEntititesRemoved = %v, want %d", got, total)
	}
}
