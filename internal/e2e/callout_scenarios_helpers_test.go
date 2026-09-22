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

	"google.golang.org/grpc/metadata"

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
// wantDetail "" skips the message check. forbidden, when given, is a list of
// secret substrings (a pass, a node id) that must not appear anywhere in any
// door's raw response; a leak is reported by position, never by printing the
// value.
func assertRefusedOnAllDoors(t *testing.T, h *callbackHarness, pass, writeModel, readEntityID string, wantStatus int, wantCode, wantDetail string, forbidden ...string) {
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
			assertNoLeak(t, door, res.Body, forbidden)
		})
	}
	envelope := func(door string, call func() (txEnvelope, error)) {
		t.Run(door, func(t *testing.T) {
			env, err := call()
			assertEnvelope(t, door, env, err, wantCode, false)
			var msg string
			if env.Error != nil {
				msg = env.Error.Message
			}
			if wantDetail != "" && !strings.Contains(msg, wantDetail) {
				t.Errorf("%s: message = %q; want it to contain %q", door, msg, wantDetail)
			}
			assertNoLeak(t, door, msg, forbidden)
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

// assertNoLeak fails t if raw contains any of forbidden's non-empty entries —
// a pass, a node id, or other internal text that must never reach a client.
// The failure names the door and the entry's position only; it never prints
// the forbidden value itself, so the assertion cannot become the leak.
func assertNoLeak(t *testing.T, door, raw string, forbidden []string) {
	t.Helper()
	for i, f := range forbidden {
		if f != "" && strings.Contains(raw, f) {
			t.Errorf("%s: response contains forbidden value #%d (a pass, a node id, or internal text) — value withheld from this failure message", door, i)
		}
	}
}

// doorResult is the door-agnostic shape of a refusal: an HTTP problem
// document and a gRPC CLIENT_ERROR envelope both reduce to it, because both
// carry an AppError's "<code>: <message>" as the one string the client sees
// (WriteError puts appErr.Message straight into the problem document's
// detail; the gRPC envelope's Error.Message is the same string). Two
// doorResults compare with ==, which is what a stolen-pass scenario needs
// when it presents more than one pass and must show the door answered them
// identically, not just that each matched a hardcoded expectation.
type doorResult struct {
	Status    int
	Code      string
	Retryable bool
	Detail    string
}

// assertRefusedOnAllDoorsAs is assertRefusedOnAllDoors for a caller identity
// other than the harness's own cached bearer. Every Replay* call and
// assertRefusedOnAllDoors itself authenticate as the harness's own tenant, so
// neither can express "tenant B presents tenant A's pass" — the shape a
// stolen-pass scenario needs. wantDetail, when non-empty, is matched for
// EQUALITY against the literal text the code gives, not a substring: a
// stolen-pass scenario needs the exact wording the client sees, not a
// superset of it. It returns each door's actual result keyed by name, so a
// caller can compare what two different passes produced on the very same
// door.
// forbidden, when given, is a list of secret substrings (a pass, a node id)
// that must not appear anywhere in any door's raw response; a leak is
// reported by position, never by printing the value (see assertNoLeak).
func assertRefusedOnAllDoorsAs(t *testing.T, h *callbackHarness, bearer, pass, writeModel, readEntityID string, wantStatus int, wantCode, wantDetail string, forbidden ...string) map[string]doorResult {
	t.Helper()
	const child = `{"name":"late-child","amount":1,"status":"late"}`
	out := make(map[string]doorResult, 6)
	httpDoor := func(door, method, path, body string) {
		t.Run(door, func(t *testing.T) {
			resp := h.doAuthBearer(t, bearer, method, path, body, pass)
			raw := h.readBody(t, resp)
			pd := assertProblem(t, resp.StatusCode, raw, wantStatus, wantCode, false)
			if wantDetail != "" && pd.Detail != wantDetail {
				t.Errorf("detail = %q; want exactly %q", pd.Detail, wantDetail)
			}
			assertNoLeak(t, door, raw, forbidden)
			code, _ := pd.Properties["errorCode"].(string)
			retryable, _ := pd.Properties["retryable"].(bool)
			out[door] = doorResult{Status: resp.StatusCode, Code: code, Retryable: retryable, Detail: pd.Detail}
		})
	}
	grpcDoor := func(door string, call func() (txEnvelope, error)) {
		t.Run(door, func(t *testing.T) {
			env, err := call()
			assertEnvelope(t, door, env, err, wantCode, false)
			var msg string
			var retryable bool
			if env.Error != nil {
				msg = env.Error.Message
				retryable = env.Error.Retryable != nil && *env.Error.Retryable
			}
			if wantDetail != "" && msg != wantDetail {
				t.Errorf("%s: message = %q; want exactly %q", door, msg, wantDetail)
			}
			assertNoLeak(t, door, msg, forbidden)
			code, _, _ := strings.Cut(msg, ": ")
			out[door] = doorResult{Status: 0, Code: code, Retryable: retryable, Detail: msg}
		})
	}
	httpDoor("http-write", http.MethodPost, "/api/entity/JSON/"+writeModel+"/1", child)
	httpDoor("http-read", http.MethodGet, "/api/entity/"+readEntityID, "")
	grpcDoor("grpc-write", func() (txEnvelope, error) { return h.replayCreateGRPCAs(bearer, pass, writeModel, 1, child) })
	grpcDoor("grpc-read", func() (txEnvelope, error) { return h.replayGetGRPCAs(bearer, pass, readEntityID) })
	grpcDoor("grpc-stream-write", func() (txEnvelope, error) {
		return h.replayCreateCollectionGRPCAs(bearer, pass, writeModel, 1, child)
	})
	grpcDoor("grpc-stream-read", func() (txEnvelope, error) { return h.replaySearchCollectionGRPCAs(bearer, pass, writeModel, 1) })
	return out
}

// grpcCtxAs is grpcCtx for an explicit bearer instead of the harness's own
// cached one.
func (h *callbackHarness) grpcCtxAs(bearer, joinTok string) context.Context {
	ctx := context.Background()
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+bearer)
	if joinTok != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, internalgrpcTxTokenKey, joinTok)
	}
	return ctx
}

// replayCreateGRPCAs is ReplayCreateGRPC under an explicit bearer.
func (h *callbackHarness) replayCreateGRPCAs(bearer, pass, model string, version int, payload string) (txEnvelope, error) {
	var data map[string]any
	if err := json.Unmarshal([]byte(payload), &data); err != nil {
		return txEnvelope{}, fmt.Errorf("failed to read the payload: %w", err)
	}
	reqCE, err := internalgrpc.NewCloudEvent(internalgrpc.EntityCreateRequest, map[string]any{
		"id":         "replay-grpc-create-as",
		"dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": model, "version": version},
			"data":  data,
		},
	})
	if err != nil {
		return txEnvelope{}, fmt.Errorf("failed to build create request: %w", err)
	}
	respCE, err := cyodapb.NewCloudEventsServiceClient(h.apiConn).EntityManage(h.grpcCtxAs(bearer, pass), reqCE)
	if err != nil {
		return txEnvelope{}, fmt.Errorf("failed to call EntityManage: %w", err)
	}
	return parseTxEnvelope(respCE)
}

// replayGetGRPCAs is ReplayGetGRPC under an explicit bearer.
func (h *callbackHarness) replayGetGRPCAs(bearer, pass, entityID string) (txEnvelope, error) {
	reqCE, err := internalgrpc.NewCloudEvent(internalgrpc.EntityGetRequest, map[string]any{
		"id":       "replay-grpc-get-as",
		"entityId": entityID,
	})
	if err != nil {
		return txEnvelope{}, fmt.Errorf("failed to build get request: %w", err)
	}
	respCE, err := cyodapb.NewCloudEventsServiceClient(h.apiConn).EntitySearch(h.grpcCtxAs(bearer, pass), reqCE)
	if err != nil {
		return txEnvelope{}, fmt.Errorf("failed to call EntitySearch: %w", err)
	}
	return parseTxEnvelope(respCE)
}

// replayCreateCollectionGRPCAs is ReplayCreateCollectionGRPC under an
// explicit bearer.
func (h *callbackHarness) replayCreateCollectionGRPCAs(bearer, pass, model string, version int, payload string) (txEnvelope, error) {
	var data map[string]any
	if err := json.Unmarshal([]byte(payload), &data); err != nil {
		return txEnvelope{}, fmt.Errorf("failed to read the payload: %w", err)
	}
	reqCE, err := internalgrpc.NewCloudEvent(internalgrpc.EntityCreateCollectionRequest, map[string]any{
		"id":         "replay-grpc-create-collection-as",
		"dataFormat": "JSON",
		"payloads": []any{map[string]any{
			"model": map[string]any{"name": model, "version": version},
			"data":  data,
		}},
	})
	if err != nil {
		return txEnvelope{}, fmt.Errorf("failed to build create-collection request: %w", err)
	}
	stream, err := cyodapb.NewCloudEventsServiceClient(h.apiConn).EntityManageCollection(h.grpcCtxAs(bearer, pass), reqCE)
	if err != nil {
		return txEnvelope{}, fmt.Errorf("failed to call EntityManageCollection: %w", err)
	}
	return firstStreamEnvelope(stream)
}

// replaySearchCollectionGRPCAs is ReplaySearchCollectionGRPC under an
// explicit bearer.
func (h *callbackHarness) replaySearchCollectionGRPCAs(bearer, pass, model string, version int) (txEnvelope, error) {
	reqCE, err := internalgrpc.NewCloudEvent(internalgrpc.EntitySearchRequest, map[string]any{
		"id":    "replay-grpc-search-collection-as",
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
	stream, err := cyodapb.NewCloudEventsServiceClient(h.apiConn).EntitySearchCollection(h.grpcCtxAs(bearer, pass), reqCE)
	if err != nil {
		return txEnvelope{}, fmt.Errorf("failed to call EntitySearchCollection: %w", err)
	}
	return firstStreamEnvelope(stream)
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
// that accepts a processor's idempotent and a criterion's retryPolicy — with
// no workflow-level criterion.
func workflowDocJSON(wfName string, states map[string]any) string {
	return workflowDocWithCriterionJSON(wfName, nil, states)
}

// workflowDocWithCriterionJSON is workflowDocJSON with the workflow's own
// criterion — the one a transitions query evaluates, since listing a state's
// transitions evaluates none of theirs. A nil criterion is left out.
func workflowDocWithCriterionJSON(wfName string, criterion any, states map[string]any) string {
	wf := map[string]any{
		"version": "1.5", "name": wfName, "initialState": "NONE", "active": true, "states": states,
	}
	if criterion != nil {
		wf["criterion"] = criterion
	}
	b, _ := json.Marshal(map[string]any{
		"importMode": "REPLACE",
		"workflows":  []any{wf},
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

// countEntitiesAs is countEntities under an explicit bearer, for asserting
// that a refused write did not land in ANOTHER tenant's space either — the
// space the pass's own tenant does not own and countEntities cannot see.
func (h *callbackHarness) countEntitiesAs(t *testing.T, bearer, model string) int {
	t.Helper()
	resp := h.doAuthBearer(t, bearer, http.MethodGet, fmt.Sprintf("/api/entity/%s/1", model), "", "")
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

// setupModelWithWorkflowAs is SetupModelWithWorkflow under an explicit
// bearer, so a scenario can give a second tenant its own copy of a model —
// needed to make an entity count in that tenant's space meaningful rather
// than an artifact of the model never having existed there.
func (h *callbackHarness) setupModelWithWorkflowAs(t *testing.T, bearer, entityName, workflowJSON string) {
	t.Helper()
	resp := h.doAuthBearer(t, bearer, http.MethodPost, fmt.Sprintf("/api/model/import/JSON/SAMPLE_DATA/%s/1", entityName), workflowSampleModel, "")
	if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("import model %s: %d %s", entityName, resp.StatusCode, body)
	}
	resp = h.doAuthBearer(t, bearer, http.MethodPut, fmt.Sprintf("/api/model/%s/1/lock", entityName), "", "")
	if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("lock model %s: %d %s", entityName, resp.StatusCode, body)
	}
	resp = h.doAuthBearer(t, bearer, http.MethodPost, fmt.Sprintf("/api/model/%s/1/workflow/import", entityName), workflowJSON, "")
	if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("import workflow %s: %d %s", entityName, resp.StatusCode, body)
	}
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
