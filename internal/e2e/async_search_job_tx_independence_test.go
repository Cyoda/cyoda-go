package e2e_test

// async_search_job_tx_independence_test.go — an async-search job record does not
// belong to whatever transaction happened to submit it.
//
// The submit path runs on the caller's context, and that context can carry a
// joined transaction: the TxJoin middleware wraps the whole generated API mux
// (app.go's `mux.Handle("/", …txJoinMW(apiHandler))`), and the gRPC tx-route
// interceptor joins EntitySearch and EntitySearchCollection, which carry the
// snapshot RPCs. So a processor callback echoing its tx token can submit an
// async search inside T.
//
// The job record must stay outside T regardless. The goroutine that fills it in
// runs on a context of its own (`context.Background()` in SubmitAsync), so a job
// row written inside T is a row that goroutine cannot see — it would report the
// job missing on its first status update — and a row that vanishes if T rolls
// back, while the goroutine keeps writing to it. This test is the guard on that
// property: it fails if the job store is ever bound to the caller's transaction.

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestE2E_AsyncSearchSubmittedInsideTransaction_JobRecordIsIndependent(t *testing.T) {
	h := newCallbackHarness(t)

	const model = "asyncjob-txindep"
	const searchModel = "asyncjob-txindep-target"
	h.setupModelSampleWithWorkflow(t, searchModel, `{"name":"Alice","amount":1,"status":"new"}`, secondaryWorkflow)

	// The processor submits an async search over the joined HTTP door, echoing
	// the transaction token exactly as a cascading callback does.
	submitted := make(chan string, 1)
	h.RegisterProc("asyncjob-submit-in-tx", func(rc *reqCtx) (map[string]any, error) {
		res, err := rc.h.callback(http.MethodPost, "/api/search/async/"+searchModel+"/1",
			`{"type":"simple","jsonPath":"$.name","operatorType":"EQUALS","value":"Alice"}`, rc.token)
		if err != nil {
			return nil, fmt.Errorf("submit async search from inside T: %w", err)
		}
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("submit async search from inside T: status=%d body=%s", res.StatusCode, res.Body)
		}
		submitted <- strings.Trim(strings.TrimSpace(res.Body), `"`)
		return nil, nil
	})

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "asyncjob-txindep-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE":   {"transitions": [{"name": "init", "next": "ACTIVE", "manual": false,
					"processors": [{"type": "calculator", "name": "asyncjob-submit-in-tx", "executionMode": "SYNC",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]
				}]},
				"ACTIVE": {}
			}
		}]
	}`
	h.setupModelSampleWithWorkflow(t, model, `{"name":"parent","amount":100,"status":"new"}`, wf)

	if _, status, body := h.CreateEntity(t, model, 1, `{"name":"parent","amount":100,"status":"new"}`); status != http.StatusOK {
		t.Fatalf("create primary: status=%d body=%s", status, body)
	}

	var jobID string
	select {
	case jobID = <-submitted:
	case <-time.After(20 * time.Second):
		t.Fatal("the processor never submitted the async search")
	}

	// The background goroutine runs on its own context, so it reaches the job
	// record only if that record was written outside T. A job stuck RUNNING, or
	// one that settles FAILED, means the write joined the caller's transaction.
	deadline := time.Now().Add(30 * time.Second)
	for {
		resp := h.DoAuth(t, http.MethodGet, "/api/search/async/"+jobID+"/status", "", "")
		body := h.readBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("job status: %d %s", resp.StatusCode, body)
		}
		if strings.Contains(body, "SUCCESSFUL") {
			return
		}
		if strings.Contains(body, "FAILED") {
			t.Fatalf("the job the callback submitted settled FAILED — the record the goroutine updates is not the one the submit wrote: %s", body)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the job never settled: %s", body)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
