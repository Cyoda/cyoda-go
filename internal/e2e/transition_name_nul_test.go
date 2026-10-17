package e2e_test

import (
	"net/http"
	"strings"
	"testing"
)

// TestUpdateSingle_TransitionNameHoldingNUL_400: a transition name holding
// U+0000 — sent percent-encoded in the path — is refused with 400 BAD_REQUEST
// before the run records anything, and the entity is unchanged. The name
// reaches the run's audit events, which cannot hold U+0000, and a joined write
// that fails leaves the owner's transaction to commit, so the engine refuses
// the name where it enters rather than letting its event be lost.
func TestUpdateSingle_TransitionNameHoldingNUL_400(t *testing.T) {
	model := "e2e-transition-nul-" + randSuffix(t)
	setupModelWithWorkflow(t, model, trivialModelWF())
	id := createEntityE2E(t, model, 1, `{"name":"A","amount":1,"status":"new"}`)
	before := getEntityTxID(t, id)

	resp := doAuth(t, http.MethodPut, "/api/entity/JSON/"+id+"/g%00o", `{"name":"A","amount":2,"status":"new"}`)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "BAD_REQUEST") {
		t.Fatalf("expected 400 BAD_REQUEST for a transition name holding U+0000, got %d: %s", resp.StatusCode, body)
	}
	if strings.ContainsRune(body, 0) {
		t.Errorf("the response echoes the raw U+0000: %q", body)
	}
	if after := getEntityTxID(t, id); after != before {
		t.Errorf("the entity was written (transactionId %s -> %s); want nothing committed", before, after)
	}
}
