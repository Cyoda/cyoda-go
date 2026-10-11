package e2e_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
)

// Conditional delete reads at most 10 MiB of condition, as every other JSON
// body on the entity endpoints does. A larger body is 400 BAD_REQUEST and
// deletes nothing, even when what it carries is a valid condition.
func TestDeleteConditional_BodyOverLimit_IsBadRequest(t *testing.T) {
	const model = "e2e-delete-body-limit"
	idA, idB := setupScoresModel(t, model)
	cond := `{"type":"simple","jsonPath":"$.name","operatorType":"EQUALS","value":"A"}`
	body := strings.Repeat(" ", 10*1024*1024) + cond
	resp := doAuth(t, http.MethodDelete, fmt.Sprintf("/api/entity/%s/1", model), body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body: %s", resp.StatusCode, readBody(t, resp))
	}
	commontest.ExpectErrorCode(t, resp, "BAD_REQUEST")
	resp.Body.Close()
	for _, id := range []string{idA, idB} {
		if r := doAuth(t, http.MethodGet, "/api/entity/"+id, ""); r.StatusCode != http.StatusOK {
			t.Errorf("entity %s must survive a refused delete, got %d", id, r.StatusCode)
		}
	}
}
