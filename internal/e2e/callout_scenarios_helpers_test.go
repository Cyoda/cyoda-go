package e2e_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	cyodapb "github.com/cyoda-platform/cyoda-go/api/grpc/cyoda"
	"github.com/cyoda-platform/cyoda-go/app"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
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

// assertRefusedOnAllDoors presents pass on every door a compute node can call
// back through and expects the same refusal from all six: the HTTP entity
// routes, write and read; the gRPC unary methods, EntityManage and
// EntitySearch; and the gRPC server-streaming methods, EntityManageCollection
// and EntitySearchCollection, whose join runs a path of its own — the request
// message is received before the transaction's lock is taken and every response
// frame is held until it has been released.
//
// Each door is a subtest, so a door that answers wrongly neither hides the
// others nor stops the scenario: what every door did is in the report.
// wantDetail "" skips the message check.
func assertRefusedOnAllDoors(t *testing.T, h *callbackHarness, pass, writeModel, readEntityID string, wantStatus int, wantCode, wantDetail string) {
	t.Helper()
	const child = `{"name":"late-child","amount":1,"status":"late"}`
	problem := func(door string, call func() (callbackResult, error)) {
		t.Run(door, func(t *testing.T) {
			res, err := call()
			if err != nil {
				t.Fatalf("%v", err)
			}
			pd := assertProblem(t, res.StatusCode, res.Body, wantStatus, wantCode, false)
			if wantDetail != "" && !strings.Contains(pd.Detail, wantDetail) {
				t.Errorf("detail = %q; want it to contain %q", pd.Detail, wantDetail)
			}
		})
	}
	envelope := func(door string, call func() (txEnvelope, error)) {
		t.Run(door, func(t *testing.T) {
			env, err := call()
			assertEnvelope(t, door, env, err, wantCode, false)
			if wantDetail != "" && env.Error != nil && !strings.Contains(env.Error.Message, wantDetail) {
				t.Errorf("%s: message = %q; want it to contain %q", door, env.Error.Message, wantDetail)
			}
		})
	}
	problem("http-write", func() (callbackResult, error) { return h.ReplayCreateHTTP(pass, writeModel, 1, child) })
	problem("http-read", func() (callbackResult, error) { return h.ReplayGetHTTP(pass, readEntityID) })
	envelope("grpc-write", func() (txEnvelope, error) { return h.ReplayCreateGRPC(pass, writeModel, 1, child) })
	envelope("grpc-read", func() (txEnvelope, error) { return h.ReplayGetGRPC(pass, readEntityID) })
	envelope("grpc-stream-write", func() (txEnvelope, error) {
		return h.ReplayCreateCollectionGRPC(pass, writeModel, 1, child)
	})
	envelope("grpc-stream-read", func() (txEnvelope, error) { return h.ReplaySearchCollectionGRPC(pass, writeModel, 1) })
}

// ReplayCreateCollectionGRPC presents a recorded pass on the gRPC
// server-streaming write door (EntityManageCollection) with a one-item create
// collection, and returns the first frame's envelope.
func (h *callbackHarness) ReplayCreateCollectionGRPC(pass, model string, version int, payload string) (txEnvelope, error) {
	if pass == "" {
		return txEnvelope{}, errReplayNeedsPass
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(payload), &data); err != nil {
		return txEnvelope{}, fmt.Errorf("failed to read the payload: %w", err)
	}
	reqCE, err := internalgrpc.NewCloudEvent(internalgrpc.EntityCreateCollectionRequest, map[string]any{
		"id":         "replay-grpc-create-collection",
		"dataFormat": "JSON",
		"payloads": []any{map[string]any{
			"model": map[string]any{"name": model, "version": version},
			"data":  data,
		}},
	})
	if err != nil {
		return txEnvelope{}, fmt.Errorf("failed to build create-collection request: %w", err)
	}
	stream, err := cyodapb.NewCloudEventsServiceClient(h.apiConn).EntityManageCollection(h.grpcCtx(pass), reqCE)
	if err != nil {
		return txEnvelope{}, fmt.Errorf("failed to call EntityManageCollection: %w", err)
	}
	return firstStreamEnvelope(stream)
}

// ReplaySearchCollectionGRPC presents a recorded pass on the gRPC
// server-streaming read door (EntitySearchCollection) and returns the first
// frame's envelope.
func (h *callbackHarness) ReplaySearchCollectionGRPC(pass, model string, version int) (txEnvelope, error) {
	if pass == "" {
		return txEnvelope{}, errReplayNeedsPass
	}
	reqCE, err := internalgrpc.NewCloudEvent(internalgrpc.EntitySearchRequest, map[string]any{
		"id":    "replay-grpc-search-collection",
		"model": map[string]any{"name": model, "version": version},
		"condition": map[string]any{
			"type":         "simple",
			"jsonPath":     "$.status",
			"operatorType": "EQUALS",
			"value":        "late",
		},
	})
	if err != nil {
		return txEnvelope{}, fmt.Errorf("failed to build search request: %w", err)
	}
	stream, err := cyodapb.NewCloudEventsServiceClient(h.apiConn).EntitySearchCollection(h.grpcCtx(pass), reqCE)
	if err != nil {
		return txEnvelope{}, fmt.Errorf("failed to call EntitySearchCollection: %w", err)
	}
	return firstStreamEnvelope(stream)
}

// firstStreamEnvelope reads a server-streaming reply's first frame as an
// envelope. A refusal is that one frame; a stream that ends without any frame
// is reported as a success carrying no error, which is what a refused door must
// never answer.
func firstStreamEnvelope(stream interface {
	Recv() (*cepb.CloudEvent, error)
}) (txEnvelope, error) {
	frame, err := stream.Recv()
	if errors.Is(err, io.EOF) {
		return txEnvelope{Success: true}, nil
	}
	if err != nil {
		return txEnvelope{}, fmt.Errorf("failed to read the stream's first frame: %w", err)
	}
	return parseTxEnvelope(frame)
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
