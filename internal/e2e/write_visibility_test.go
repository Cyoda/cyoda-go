package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// A successful write response means the write is visible to every
// subsequent read, unconditionally. The former consistency-wait flag is
// retired and, like any query parameter an operation does not declare, is
// refused: the write is not made. One case per write operation, each made
// once with the flag (refused, nothing changes) and once without (visible at
// once).
func TestWriteVisibility_RetiredFlagRefused_AllWriteOps(t *testing.T) {
	const model = "e2e-wvis-flag"
	setupModelWithWorkflow(t, model, `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "wvis-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE":    {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {"transitions": [{"name": "touch", "next": "CREATED", "manual": true}]}
			}
		}]
	}`)
	const flag = "?waitForConsistencyAfter=true"
	refused := func(t *testing.T, resp *http.Response) {
		t.Helper()
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "unknown query parameter") {
			t.Fatalf("with the retired flag: status = %d, want 400 unknown query parameter; body: %s", resp.StatusCode, body)
		}
	}

	t.Run("create", func(t *testing.T) {
		before := countEntitiesInModel(t, model, 1)
		refused(t, doAuth(t, http.MethodPost, fmt.Sprintf("/api/entity/JSON/%s/1%s", model, flag), `{"name":"A","amount":1,"status":"new"}`))
		if after := countEntitiesInModel(t, model, 1); after != before {
			t.Fatalf("a refused create wrote: %d entities, want %d", after, before)
		}
		resp := doAuth(t, http.MethodPost, fmt.Sprintf("/api/entity/JSON/%s/1", model), `{"name":"A","amount":1,"status":"new"}`)
		assertEntityAmount(t, firstEntityID(t, resp), 1)
	})
	t.Run("createCollection", func(t *testing.T) {
		body := fmt.Sprintf(`[{"model":{"name":%q,"version":1},"payload":"{\"name\":\"B\",\"amount\":2,\"status\":\"new\"}"}]`, model)
		before := countEntitiesInModel(t, model, 1)
		refused(t, doAuth(t, http.MethodPost, "/api/entity/JSON"+flag, body))
		if after := countEntitiesInModel(t, model, 1); after != before {
			t.Fatalf("a refused create wrote: %d entities, want %d", after, before)
		}
		assertEntityAmount(t, firstEntityID(t, doAuth(t, http.MethodPost, "/api/entity/JSON", body)), 2)
	})
	t.Run("updateSingle", func(t *testing.T) {
		id := createEntityE2E(t, model, 1, `{"name":"C","amount":3,"status":"new"}`)
		refused(t, doAuth(t, http.MethodPut, fmt.Sprintf("/api/entity/JSON/%s/touch%s", id, flag), `{"name":"C","amount":30,"status":"new"}`))
		assertEntityAmount(t, id, 3)
		expect200(t, doAuth(t, http.MethodPut, fmt.Sprintf("/api/entity/JSON/%s/touch", id), `{"name":"C","amount":30,"status":"new"}`))
		assertEntityAmount(t, id, 30)
	})
	t.Run("updateSingleWithLoopback", func(t *testing.T) {
		id := createEntityE2E(t, model, 1, `{"name":"D","amount":4,"status":"new"}`)
		refused(t, doAuth(t, http.MethodPut, fmt.Sprintf("/api/entity/JSON/%s%s", id, flag), `{"name":"D","amount":40,"status":"new"}`))
		assertEntityAmount(t, id, 4)
		expect200(t, doAuth(t, http.MethodPut, fmt.Sprintf("/api/entity/JSON/%s", id), `{"name":"D","amount":40,"status":"new"}`))
		assertEntityAmount(t, id, 40)
	})
	t.Run("updateCollection", func(t *testing.T) {
		id := createEntityE2E(t, model, 1, `{"name":"E","amount":5,"status":"new"}`)
		body := fmt.Sprintf(`[{"id":%q,"payload":"{\"name\":\"E\",\"amount\":50,\"status\":\"new\"}"}]`, id)
		refused(t, doAuth(t, http.MethodPut, "/api/entity/JSON"+flag, body))
		assertEntityAmount(t, id, 5)
		expect200(t, doAuth(t, http.MethodPut, "/api/entity/JSON", body))
		assertEntityAmount(t, id, 50)
	})
	t.Run("patchSingleWithLoopback", func(t *testing.T) {
		id, txID := createEntityE2EWithTxID(t, model, 1, `{"name":"F","amount":6,"status":"new"}`)
		refused(t, patchEntity(t, fmt.Sprintf("/api/entity/JSON/%s%s", id, flag), "application/merge-patch+json", txID, `{"amount":60}`))
		assertEntityAmount(t, id, 6)
		expect200(t, patchEntity(t, fmt.Sprintf("/api/entity/JSON/%s", id), "application/merge-patch+json", txID, `{"amount":60}`))
		assertEntityAmount(t, id, 60)
	})
	t.Run("patchSingle", func(t *testing.T) {
		id, txID := createEntityE2EWithTxID(t, model, 1, `{"name":"G","amount":7,"status":"new"}`)
		refused(t, patchEntity(t, fmt.Sprintf("/api/entity/JSON/%s/touch%s", id, flag), "application/merge-patch+json", txID, `{"amount":70}`))
		assertEntityAmount(t, id, 7)
		expect200(t, patchEntity(t, fmt.Sprintf("/api/entity/JSON/%s/touch", id), "application/merge-patch+json", txID, `{"amount":70}`))
		assertEntityAmount(t, id, 70)
	})
}

func expect200(t *testing.T, resp *http.Response) {
	t.Helper()
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, body)
	}
}

// firstEntityID reads a create response ([{transactionId, entityIds}]) and
// returns its first entity id, failing on any other status.
func firstEntityID(t *testing.T, resp *http.Response) string {
	t.Helper()
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, body)
	}
	var out []struct {
		EntityIDs []string `json:"entityIds"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || len(out) == 0 || len(out[0].EntityIDs) == 0 {
		t.Fatalf("unexpected create response: %v: %s", err, body)
	}
	return out[0].EntityIDs[0]
}

// assertEntityAmount is the read-your-write half of the contract: the GET
// issued immediately after the write's response sees the written value.
func assertEntityAmount(t *testing.T, id string, want float64) {
	t.Helper()
	data := getEntityData(t, id, "")
	if got := data["amount"]; got != want {
		t.Fatalf("GET after write: amount = %v, want %v", got, want)
	}
}
