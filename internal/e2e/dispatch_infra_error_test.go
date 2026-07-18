package e2e_test

// dispatch_infra_error_test.go — e2e coverage for Task 2.1: a compute-infra
// dispatch failure (no calculation member registered for the processor's
// tags) must surface as a retryable 503 NO_COMPUTE_MEMBER_FOR_TAG, not a
// client-attributable 400 WORKFLOW_FAILED.
//
// This test stands up its OWN in-process stack (memory backend, mock IAM,
// cluster disabled) rather than reusing the package's shared TestMain
// server. The shared server wires cfg.ExternalProcessing = procSvc (see
// e2e_test.go), a stub *localproc.LocalProcessingService that resolves
// processors by name only — it never consults calculationNodesTags or a
// MemberRegistry, so it cannot reproduce grpc.ErrNoMatchingMember. Leaving
// cfg.ExternalProcessing nil here selects the REAL
// internal/grpc.ProcessorDispatcher wired over an empty MemberRegistry (no
// compute member ever connects), so FindByTags deterministically returns no
// match for any tag and DispatchProcessor returns the genuine sentinel.
//
// Mock IAM mode (the default) auto-authenticates every request as the
// default user (ROLE_ADMIN, ROLE_M2M) with no Authorization header needed
// (see iam_gated_fixtures_test.go for the same pattern).

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/app"
)

// newDispatchInfraErrorServer builds a standalone in-process stack with the
// real gRPC-backed processor dispatcher and zero connected compute members.
func newDispatchInfraErrorServer(t *testing.T) (string, func()) {
	t.Helper()
	cfg := app.DefaultConfig()
	cfg.StorageBackend = "memory"
	cfg.Cluster.Enabled = false
	// cfg.IAM.Mode defaults to "mock" — no bearer token required.
	// cfg.ExternalProcessing left nil — real ProcessorDispatcher, empty
	// MemberRegistry (no compute member connects in this test).

	a := app.New(cfg)
	srv := httptest.NewServer(a.Handler())
	return srv.URL, func() {
		srv.Close()
		a.Shutdown()
		_ = a.Close()
	}
}

// doInfraErrRequest issues an unauthenticated (mock-IAM-auto-authed) HTTP request
// against the dispatch-infra-error test server and returns (status, body).
func doInfraErrRequest(t *testing.T, base, method, path, body string) (int, string) {
	t.Helper()
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	} else {
		reader = strings.NewReader("")
	}
	req, err := http.NewRequest(method, base+path, reader)
	if err != nil {
		t.Fatalf("build request %s %s: %v", method, path, err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body for %s %s: %v", method, path, err)
	}
	return resp.StatusCode, string(buf)
}

// TestProcessorNoMember_Returns503 imports a workflow whose sole automated
// transition carries a SYNC processor with a calculationNodesTags value that
// no connected compute member can ever satisfy (no member connects at all in
// this harness). Firing the transition via entity creation must surface the
// dispatcher's grpc.ErrNoMatchingMember as HTTP 503, errorCode
// NO_COMPUTE_MEMBER_FOR_TAG, properties.retryable=true — not the pre-fix 400
// WORKFLOW_FAILED default.
func TestProcessorNoMember_Returns503(t *testing.T) {
	base, closeFn := newDispatchInfraErrorServer(t)
	defer closeFn()

	const model = "e2e-dispatch-no-member"
	const modelVersion = 1

	// Import model from sample data.
	status, body := doInfraErrRequest(t, base, http.MethodPost,
		fmt.Sprintf("/api/model/import/JSON/SAMPLE_DATA/%s/%d", model, modelVersion),
		`{"name":"Test","amount":1}`)
	if status != http.StatusOK {
		t.Fatalf("import model: expected 200, got %d: %s", status, body)
	}

	// Lock the model.
	status, body = doInfraErrRequest(t, base, http.MethodPut,
		fmt.Sprintf("/api/model/%s/%d/lock", model, modelVersion), "")
	if status != http.StatusOK {
		t.Fatalf("lock model: expected 200, got %d: %s", status, body)
	}

	// Import a workflow: NONE -init(SYNC processor, unmatched tag)-> DONE.
	wf := fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "%s-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "DONE", "manual": false,
					"processors": [{"type": "calculator", "name": "noop-proc", "executionMode": "SYNC",
						"config": {"attachEntity": true, "calculationNodesTags": "no-such-tag"}}]
				}]},
				"DONE": {}
			}
		}]
	}`, model)
	status, body = doInfraErrRequest(t, base, http.MethodPost,
		fmt.Sprintf("/api/model/%s/%d/workflow/import", model, modelVersion), wf)
	if status != http.StatusOK {
		t.Fatalf("import workflow: expected 200, got %d: %s", status, body)
	}

	// Create an entity — fires the "init" automated transition, which
	// dispatches to a calculation member matching tag "no-such-tag". No
	// member is connected in this harness, so the dispatcher returns
	// grpc.ErrNoMatchingMember.
	status, body = doInfraErrRequest(t, base, http.MethodPost,
		fmt.Sprintf("/api/entity/JSON/%s/%d", model, modelVersion),
		`{"name":"Test","amount":1}`)

	if status != http.StatusServiceUnavailable {
		t.Fatalf("create entity firing no-member dispatch: expected 503, got %d: %s", status, body)
	}

	var problem struct {
		Properties struct {
			ErrorCode string `json:"errorCode"`
			Retryable bool   `json:"retryable"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(body), &problem); err != nil {
		t.Fatalf("failed to parse response body as JSON: %v\nbody: %s", err, body)
	}
	if problem.Properties.ErrorCode != "NO_COMPUTE_MEMBER_FOR_TAG" {
		t.Errorf("expected errorCode NO_COMPUTE_MEMBER_FOR_TAG, got %q (body: %s)", problem.Properties.ErrorCode, body)
	}
	if !problem.Properties.Retryable {
		t.Errorf("expected properties.retryable=true, got false (body: %s)", body)
	}
}

// TestScheduledFunctionNoMember_Returns503 is TestProcessorNoMember_Returns503's
// counterpart for a schedule.function Function callout (Task 9.2, issue
// #419): a workflow whose sole automated transition carries a schedule.function
// with a calculationNodesTags value no connected compute member can ever
// satisfy (no member connects at all in this harness — same empty
// MemberRegistry as the processor case). Arming the transition at entity
// creation dispatches via internal/grpc.ProcessorDispatcher.DispatchFunction,
// which resolves the same grpc.ErrNoMatchingMember sentinel
// classifyWorkflowError maps to a retryable 503 NO_COMPUTE_MEMBER_FOR_TAG —
// proving the Function dispatch path shares the processor path's
// compute-infra-error classification, not the pre-fix 400 WORKFLOW_FAILED
// default.
func TestScheduledFunctionNoMember_Returns503(t *testing.T) {
	base, closeFn := newDispatchInfraErrorServer(t)
	defer closeFn()

	const model = "e2e-dispatch-no-member-schedfn"
	const modelVersion = 1

	status, body := doInfraErrRequest(t, base, http.MethodPost,
		fmt.Sprintf("/api/model/import/JSON/SAMPLE_DATA/%s/%d", model, modelVersion),
		`{"name":"Test","amount":1}`)
	if status != http.StatusOK {
		t.Fatalf("import model: expected 200, got %d: %s", status, body)
	}

	status, body = doInfraErrRequest(t, base, http.MethodPut,
		fmt.Sprintf("/api/model/%s/%d/lock", model, modelVersion), "")
	if status != http.StatusOK {
		t.Fatalf("lock model: expected 200, got %d: %s", status, body)
	}

	// schedule.function requires schema version 1.3 (docs/workflow-schema-versioning.md).
	// calculationNodesTags "no-such-tag" matches no connected member (none
	// connects in this harness at all).
	wf := fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.3", "name": "%s-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "DONE", "manual": false,
					"schedule": {"function": {"name": "calcFire", "resultKind": "Schedule", "calculationNodesTags": "no-such-tag"}}
				}]},
				"DONE": {}
			}
		}]
	}`, model)
	status, body = doInfraErrRequest(t, base, http.MethodPost,
		fmt.Sprintf("/api/model/%s/%d/workflow/import", model, modelVersion), wf)
	if status != http.StatusOK {
		t.Fatalf("import workflow: expected 200, got %d: %s", status, body)
	}

	// Create an entity — arming "init"'s schedule.function dispatches to a
	// calculation member matching tag "no-such-tag". No member is connected
	// in this harness, so DispatchFunction returns grpc.ErrNoMatchingMember.
	status, body = doInfraErrRequest(t, base, http.MethodPost,
		fmt.Sprintf("/api/entity/JSON/%s/%d", model, modelVersion),
		`{"name":"Test","amount":1}`)

	if status != http.StatusServiceUnavailable {
		t.Fatalf("create entity firing no-member Function dispatch: expected 503, got %d: %s", status, body)
	}

	var problem struct {
		Properties struct {
			ErrorCode string `json:"errorCode"`
			Retryable bool   `json:"retryable"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(body), &problem); err != nil {
		t.Fatalf("failed to parse response body as JSON: %v\nbody: %s", err, body)
	}
	if problem.Properties.ErrorCode != "NO_COMPUTE_MEMBER_FOR_TAG" {
		t.Errorf("expected errorCode NO_COMPUTE_MEMBER_FOR_TAG, got %q (body: %s)", problem.Properties.ErrorCode, body)
	}
	if !problem.Properties.Retryable {
		t.Errorf("expected properties.retryable=true, got false (body: %s)", body)
	}
}
