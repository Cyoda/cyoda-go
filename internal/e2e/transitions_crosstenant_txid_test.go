package e2e_test

import (
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// TestTransitions_CrossTenantTransactionID_Rejected pins the HTTP surface of
// the GetSubmitTime tenant check: GET /entity/{id}/transitions?transactionId=
// resolves the txID to a submit time via the transaction manager BEFORE any
// entity lookup, so a tenant-B caller supplying tenant A's txID must be
// rejected there — 400, the same status a nonexistent txID yields. Without
// the tenant check the lookup resolves and the request proceeds to the
// (tenant-scoped) entity read, answering 404 — which both confirms the
// foreign txID exists and that it was committed.
func TestTransitions_CrossTenantTransactionID_Rejected(t *testing.T) {
	const model = "e2e-transitions-xtenant"

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "txt-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)

	// Tenant A (the bootstrap tenant) creates an entity, capturing the txID.
	entityID, txIDA := createEntityE2EWithTxID(t, model, 1, `{"name":"A","amount":1,"status":"new"}`)
	if txIDA == "" {
		t.Fatal("create returned empty transactionId")
	}

	// Sanity: tenant A resolves its own txID.
	respOwn := doAuth(t, http.MethodGet, fmt.Sprintf("/api/entity/%s/transitions?transactionId=%s", entityID, txIDA), "")
	bodyOwn := readBody(t, respOwn)
	if respOwn.StatusCode != http.StatusOK {
		t.Fatalf("own-tenant transitions?transactionId: got %d, want 200; body: %s", respOwn.StatusCode, bodyOwn)
	}

	// Tenant B: a second M2M client in a different tenant.
	clientBID, clientBSecret := createM2MClient(t, "tenant-b-transitions", "user-b", []string{"ROLE_ADMIN", "ROLE_M2M"})

	getTransitionsAsB := func(txID string) (int, string) {
		t.Helper()
		resp := adminRequestAs(t, clientBID, clientBSecret,
			http.MethodGet, fmt.Sprintf("/entity/%s/transitions?transactionId=%s", entityID, txID), nil)
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		return resp.StatusCode, string(raw)
	}

	// (1) Tenant A's real txID: the submit-time lookup must reject it.
	statusReal, bodyReal := getTransitionsAsB(txIDA)
	if statusReal != http.StatusBadRequest {
		t.Errorf("tenant B transitions?transactionId=<txID_A>: got %d, want 400; body: %s", statusReal, bodyReal)
	}

	// (2) A txID that exists in no tenant: same 400 status — the status
	// code must not distinguish "exists in another tenant" from "doesn't
	// exist".
	statusBogus, bodyBogus := getTransitionsAsB(uuid.New().String())
	if statusBogus != http.StatusBadRequest {
		t.Errorf("tenant B transitions?transactionId=<bogus>: got %d, want 400; body: %s", statusBogus, bodyBogus)
	}
}
