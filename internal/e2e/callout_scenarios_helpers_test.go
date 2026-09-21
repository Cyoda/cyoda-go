package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/app"
)

// callout_scenarios_helpers_test.go holds what the callout scenario files
// share. A callout harness stack is not behind the OpenAPI validator, so the
// assertions here are the only check of a response's shape.

// supersededDetail is the client-visible text of CALLOUT_SUPERSEDED.
const supersededDetail = "this compute node was replaced, or its callout has ended"

// problemDoc is the part of an RFC 9457 body the scenarios assert on.
type problemDoc struct {
	Status     int            `json:"status"`
	Detail     string         `json:"detail"`
	Properties map[string]any `json:"properties"`
}

// assertProblem checks status, errorCode and the retryable property of a
// problem response and returns it for message assertions.
func assertProblem(t *testing.T, status int, body string, wantStatus int, wantCode string, wantRetryable bool) problemDoc {
	t.Helper()
	if status != wantStatus {
		t.Fatalf("status = %d; want %d %s (body: %s)", status, wantStatus, wantCode, body)
	}
	var pd problemDoc
	if err := json.Unmarshal([]byte(body), &pd); err != nil {
		t.Fatalf("response is not a problem document: %v (body: %s)", err, body)
	}
	if got, _ := pd.Properties["errorCode"].(string); got != wantCode {
		t.Errorf("errorCode = %q; want %q (body: %s)", got, wantCode, body)
	}
	if got, _ := pd.Properties["retryable"].(bool); got != wantRetryable {
		t.Errorf("retryable = %t; want %t (body: %s)", got, wantRetryable, body)
	}
	return pd
}

// assertEnvelope checks a refused gRPC call: the envelope class is
// CLIENT_ERROR, the message starts with the domain code, and retryable is set
// only when true.
func assertEnvelope(t *testing.T, door string, env txEnvelope, err error, wantDomainCode string, wantRetryable bool) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: transport error: %v", door, err)
	}
	if env.Success || env.Error == nil {
		t.Fatalf("%s: success=%t error=%v; want a refusal with %s", door, env.Success, env.Error, wantDomainCode)
	}
	if env.Error.Code != "CLIENT_ERROR" {
		t.Errorf("%s: Error.Code = %q; want CLIENT_ERROR", door, env.Error.Code)
	}
	if !strings.HasPrefix(env.Error.Message, wantDomainCode+":") {
		t.Errorf("%s: Error.Message = %q; want the prefix %q", door, env.Error.Message, wantDomainCode+":")
	}
	if got := env.Error.Retryable != nil && *env.Error.Retryable; got != wantRetryable {
		t.Errorf("%s: retryable = %t; want %t", door, got, wantRetryable)
	}
}

// assertRefusedOnAllDoors presents pass as a write and as a read on the HTTP
// door and on the gRPC door and expects the same refusal from all four.
// wantDetail "" skips the message check.
func assertRefusedOnAllDoors(t *testing.T, h *callbackHarness, pass, writeModel, readEntityID string, wantStatus int, wantCode, wantDetail string) {
	t.Helper()
	const child = `{"name":"late-child","amount":1,"status":"late"}`
	check := func(door string, res callbackResult, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", door, err)
		}
		pd := assertProblem(t, res.StatusCode, res.Body, wantStatus, wantCode, false)
		if wantDetail != "" && !strings.Contains(pd.Detail, wantDetail) {
			t.Errorf("%s: detail = %q; want it to contain %q", door, pd.Detail, wantDetail)
		}
	}
	res, err := h.ReplayCreateHTTP(pass, writeModel, 1, child)
	check("HTTP write", res, err)
	res, err = h.ReplayGetHTTP(pass, readEntityID)
	check("HTTP read", res, err)

	env, err := h.ReplayCreateGRPC(pass, writeModel, 1, child)
	assertEnvelope(t, "gRPC write", env, err, wantCode, false)
	if wantDetail != "" && env.Error != nil && !strings.Contains(env.Error.Message, wantDetail) {
		t.Errorf("gRPC write: message = %q; want it to contain %q", env.Error.Message, wantDetail)
	}
	env, err = h.ReplayGetGRPC(pass, readEntityID)
	assertEnvelope(t, "gRPC read", env, err, wantCode, false)
}

// procSpec is one processor of a chain workflow. config is merged over
// {"attachEntity": true}; every caller names calculationNodesTags.
type procSpec struct {
	name, mode string
	config     map[string]any
}

// chainWorkflowJSON builds a NONE -> DONE workflow whose one automated
// transition runs procs in order.
func chainWorkflowJSON(wfName string, procs ...procSpec) string {
	list := make([]any, 0, len(procs))
	for _, p := range procs {
		cfg := map[string]any{"attachEntity": true}
		for k, v := range p.config {
			cfg[k] = v
		}
		list = append(list, map[string]any{"type": "calculator", "name": p.name, "executionMode": p.mode, "config": cfg})
	}
	return workflowDocJSON(wfName, map[string]any{
		"NONE": map[string]any{"transitions": []any{map[string]any{
			"name": "init", "next": "DONE", "manual": false, "processors": list,
		}}},
		"DONE": map[string]any{},
	})
}

// criterionWorkflowJSON builds a NONE -> DONE workflow whose automated
// transition is guarded by one function criterion.
func criterionWorkflowJSON(wfName, critName string, config map[string]any) string {
	return workflowDocJSON(wfName, map[string]any{
		"NONE": map[string]any{"transitions": []any{map[string]any{
			"name": "init", "next": "DONE", "manual": false,
			"criterion": map[string]any{"type": "function", "function": map[string]any{"name": critName, "config": config}},
		}}},
		"DONE": map[string]any{},
	})
}

// workflowDocJSON wraps states in a schema 1.5 import document — the minor
// that accepts a processor's idempotent and a criterion's retryPolicy.
func workflowDocJSON(wfName string, states map[string]any) string {
	b, _ := json.Marshal(map[string]any{
		"importMode": "REPLACE",
		"workflows": []any{map[string]any{
			"version": "1.5", "name": wfName, "initialState": "NONE", "active": true, "states": states,
		}},
	})
	return string(b)
}

// scriptHold keeps a callout until release is closed (or the cnode ends), then
// gives then. It is scriptLateCallback with nothing to call back.
func scriptHold(release <-chan struct{}, then cnodeReply) cnodeScript {
	return scriptLateCallback(release, func(*reqCtx) {}, then)
}

// calloutTuning sets the retries after the first try and the patience.
func calloutTuning(retries int, patience time.Duration) func(*app.Config) {
	return func(cfg *app.Config) {
		cfg.Callout.FixedNumRetries = retries
		cfg.Cluster.DispatchWaitTimeout = patience
	}
}

// countEntities returns how many committed entities the model has.
func (h *callbackHarness) countEntities(t *testing.T, model string) int {
	t.Helper()
	resp := h.DoAuth(t, http.MethodGet, fmt.Sprintf("/api/entity/%s/1", model), "", "")
	body := h.readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list %s: %d %s", model, resp.StatusCode, body)
	}
	var list []map[string]any
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("decode list of %s: %v (body: %s)", model, err, body)
	}
	return len(list)
}

// joinedRequest is h.callback with a context of the caller's, so a scenario
// can walk away from a callback in progress.
func (h *callbackHarness) joinedRequest(ctx context.Context, method, path, body, pass string) (callbackResult, error) {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.baseURL+path, r)
	if err != nil {
		return callbackResult{}, err
	}
	tok, _ := h.bearerVal.Load().(string)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	if pass != "" {
		req.Header.Set("X-Tx-Token", pass)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return callbackResult{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return callbackResult{StatusCode: resp.StatusCode, Body: string(raw)}, nil
}
