package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// A message posted under a callback's transaction token never joins that
// transaction: the owner's rollback leaves the message in place.
func TestCallback_MessageUnderToken_OutsideTheTransaction(t *testing.T) {
	h := newCallbackHarness(t)

	const model = "cb-msg-outside-tx"
	posted := make(chan string, 1)
	h.RegisterProc("cb-post-message-then-fail", func(rc *reqCtx) (map[string]any, error) {
		res, err := rc.h.callback(http.MethodPost, "/api/message/new/cb-msg-outside-tx", `{"payload":{"x":1}}`, rc.token)
		if err != nil {
			return nil, fmt.Errorf("callback message failed: %w", err)
		}
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("callback message status=%d body=%s", res.StatusCode, res.Body)
		}
		var results []map[string]any
		if err := json.Unmarshal([]byte(res.Body), &results); err != nil || len(results) == 0 {
			return nil, fmt.Errorf("callback message body %q: %v", res.Body, err)
		}
		ids, _ := results[0]["entityIds"].([]any)
		if len(ids) == 0 {
			return nil, fmt.Errorf("callback message returned no id: %s", res.Body)
		}
		posted <- fmt.Sprint(ids[0])
		return nil, fmt.Errorf("processor deliberately fails after posting a message")
	})

	h.SetupModelWithWorkflow(t, model, `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "cb-msg-outside-tx-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE":   {"transitions": [{"name": "init", "next": "ACTIVE", "manual": false,
					"processors": [{"type": "externalized", "name": "cb-post-message-then-fail", "executionMode": "SYNC",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]
				}]},
				"ACTIVE": {}
			}
		}]
	}`)

	owner := h.CreateEntityRaw(model, 1, `{"name":"owner","amount":1,"status":"new"}`)
	if owner.status != http.StatusBadRequest || problemErrorCode(owner.body) != "WORKFLOW_FAILED" {
		t.Fatalf("owner create: status=%d body=%s; want 400 WORKFLOW_FAILED", owner.status, owner.body)
	}

	var msgID string
	select {
	case msgID = <-posted:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout: the callback did not post a message")
	}

	resp := h.DoAuth(t, http.MethodGet, "/api/message/"+msgID, "", "")
	if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET message after the owner rolled back: %d %s; want 200", resp.StatusCode, body)
	}
}
