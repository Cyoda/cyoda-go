# Stream S — scenario tests (the E, P and M layers of spec §13)

These tasks are **tests only**, written through the public doors (HTTP, gRPC,
the cnode stream). They run **last**, when the feature is wired: the
`Coordinator` has replaced the dispatchers, the join layer holds the
transaction's lock, the fence is enforced, the hand-over carries tries. No task
here has a production "Step 3"; where a scenario needs a harness capability H
does not give, the step adds it to the **test** harness and says so.

**What RED means in this stream.** The production code already exists when
these tasks run, so a scenario cannot be red against the branch head. Each task
therefore states (a) what the scenario does against the merge-base
(`git merge-base HEAD origin/release/v0.9.0`, with stream H's harness commits
cherry-picked — the scenarios do not compile without them), and (b) **one named
temporary revert** of a production line that must turn the scenario red, which
the implementer applies, runs, records the failure text in the commit body, and
undoes (`git checkout -- <file>`; never committed). A scenario that stays green
under its revert has no teeth and is rewritten, not committed. Where a row is
"as today" the scenario is green on the merge-base by design — it is a pin — and
only (b) applies.

**Where the scenarios run.** Every E scenario uses a stack of its own
(`newCalloutHarness`), because patience, tries and cnodes are per test.
Such a stack is **not** behind the enforce-mode OpenAPI validator: only the
shared `testApp` is (`internal/e2e/e2e_test.go:199-203`;
`transaction_control_test.go:203-205` says the same of the callback harness).
So each scenario asserts status, `errorCode`, `retryable` and (where §8.2 fixes
it) the message by hand, through `assertProblem` / `assertEnvelope` (S-1). See
Open point 1 for what that means for `api/openapi.yaml`.

**Commands.** E: `go test ./internal/e2e/ -run '<TestName>'`. P, one scenario
on one backend (the fixture builds the server in a subprocess the test cache
cannot see, so this is the one place `-count=1` is right — CLAUDE.md, "the one
exception"; the Makefile's parity recipe does the same, `Makefile:127`):
`go test -count=1 ./e2e/parity/memory/ -run 'TestParity/<Scenario>'` (then
`sqlite`, `postgres`). M: `go test -count=1 ./e2e/parity/postgres/ -run 'TestMultiNode/<Scenario>'`.
Docker must be running for all three. Never `-v`.

**Every commit message ends with**
`Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`.

---

### Task S-1: e2e — scenario helpers; a processor's failed try, and whether the next cnode is asked

**Spec:** §3 (both tables), D1, D3, §8.2 rows 2, 3, 6, 7, 8; §4 "Every try sends the same `RequestID`"; §13 rows 1007, 1008, 1010, 1011, 1012, 1013, 1014 (processor), 1018 — layer **E**.

**Files:**
- Create: `internal/e2e/callout_scenarios_helpers_test.go`
- Create: `internal/e2e/callout_failover_test.go`
- Modify: `internal/e2e/callback_harness_test.go` (H-2's `cnodeReplyKind` constants and `cnodeReply.cloudEvent`: one more reply — a success whose payload is not an object; H's reply model cannot produce the one `Terminal` a cnode can cause)

**Interfaces:**
- Consumes (H): `newCalloutHarness`, `AttachCnode`, `cnodeSpec`, `scriptAlways`, `answerOK`, `answerFail`, `answerFailVerdict`, `neverAnswer`, `closeStream`, `(c) Received`, `(c) Detach`, `(c) AwaitGone`, `receivedCallout{RequestID, EventID}`, `Replay*`, `txEnvelope`, `callbackResult`, `h.callback`, `h.SetupModelWithWorkflow`, `h.CreateEntity`, `workflowSampleModel`.
- Consumes (C): `cfg.Callout.FixedNumRetries`, `cfg.Cluster.DispatchWaitTimeout`; import accepts `idempotent` and `retryPolicy`.
- Consumes (O, L): the behaviour of §3/§5 — nothing by name.
- Produces (for S-2 … S-10):
  - `type problemDoc struct{ Status int; Detail string; Properties map[string]any }`
  - `func assertProblem(t *testing.T, status int, body string, wantStatus int, wantCode string, wantRetryable bool) problemDoc`
  - `func assertEnvelope(t *testing.T, door string, env txEnvelope, err error, wantDomainCode string, wantRetryable bool)`
  - `const supersededDetail = "this compute node was replaced, or its callout has ended"`
  - `func assertRefusedOnAllDoors(t *testing.T, h *callbackHarness, pass, writeModel, readEntityID string, wantStatus int, wantCode, wantDetail string)`
  - `type procSpec struct{ name, mode string; config map[string]any }`, `func chainWorkflowJSON(wfName string, procs ...procSpec) string`
  - `func criterionWorkflowJSON(wfName, critName string, config map[string]any) string`
  - `func scriptHold(release <-chan struct{}, then cnodeReply) cnodeScript`
  - `func calloutTuning(retries int, patience time.Duration) func(*app.Config)`
  - `func (h *callbackHarness) countEntities(t *testing.T, model string) int`
  - `func (h *callbackHarness) joinedRequest(ctx context.Context, method, path, body, pass string) (callbackResult, error)`
  - `func answerMalformedPayload() cnodeReply`

Existing tests: none changes. `TestScriptedCnode_Replies` (H-3) keeps pinning the single-cnode outcomes; with one matching cnode and a non-idempotent processor they are unchanged by the feature.

- [ ] **Step 1: Write the helpers and the failing scenario**

`internal/e2e/callback_harness_test.go` — in H-2's reply model add the kind and its constructor, and one arm in `cloudEvent`:

```go
const (
	replyOK cnodeReplyKind = iota
	replyFail
	replySilent
	replyCloseStream
	replyMalformed
)

// answerMalformedPayload answers success=true with a payload that is a JSON
// string, not an object: the one failure a cnode can cause that would fail
// identically on any other cnode.
func answerMalformedPayload() cnodeReply { return cnodeReply{kind: replyMalformed} }
```

```go
	// in (r cnodeReply) cloudEvent, directly after the replyFail block:
	if r.kind == replyMalformed {
		body["success"] = true
		body["payload"] = "not-an-object"
		return internalgrpc.NewCloudEvent(respType, body)
	}
```

`internal/e2e/callout_scenarios_helpers_test.go`:

```go
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
// gives then.
func scriptHold(release <-chan struct{}, then cnodeReply) cnodeScript {
	return func(ctx context.Context, _ receivedCallout, _ *reqCtx) cnodeReply {
		select {
		case <-release:
			return then
		case <-ctx.Done():
			return neverAnswer()
		}
	}
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
```

`internal/e2e/callout_failover_test.go`:

```go
package e2e_test

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestCalloutFailover_Processor: two cnodes serve one tag; the one attached
// first is tried first. What the first does with the work, and whether the
// processor is declared idempotent, decides whether the second is asked.
func TestCalloutFailover_Processor(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(3, 100*time.Millisecond))

	cases := []struct {
		name          string
		first         cnodeReply
		config        map[string]any
		wantStatus    int
		wantCode      string
		wantRetryable bool
		wantDetail    string
		wantSecond    int // callouts the second cnode receives
	}{
		{"no-answer-not-idempotent", neverAnswer(), nil,
			http.StatusServiceUnavailable, "DISPATCH_TIMEOUT", true, "", 0},
		{"no-answer-idempotent", neverAnswer(), map[string]any{"idempotent": true},
			http.StatusOK, "", false, "", 1},
		{"drop-not-idempotent", closeStream(), nil,
			http.StatusServiceUnavailable, "COMPUTE_MEMBER_DISCONNECTED", true, "", 0},
		{"drop-idempotent", closeStream(), map[string]any{"idempotent": true},
			http.StatusOK, "", false, "", 1},
		{"failed-verdict-true", answerFailVerdict("s1 boom", true), map[string]any{"idempotent": true},
			http.StatusBadRequest, "WORKFLOW_FAILED", true, "processor s1-proc failed: s1 boom", 0},
		{"failed-verdict-false", answerFailVerdict("s1 boom", false), map[string]any{"idempotent": true},
			http.StatusBadRequest, "WORKFLOW_FAILED", false, "processor s1-proc failed: s1 boom", 0},
		{"failed-no-verdict", answerFail("s1 boom"), map[string]any{"idempotent": true},
			http.StatusBadRequest, "WORKFLOW_FAILED", false, "processor s1-proc failed: s1 boom", 0},
		{"terminal", answerMalformedPayload(), map[string]any{"idempotent": true},
			http.StatusBadRequest, "WORKFLOW_FAILED", false, "", 0},
		{"retry-policy-none", neverAnswer(), map[string]any{"idempotent": true, "retryPolicy": "NONE"},
			http.StatusServiceUnavailable, "DISPATCH_TIMEOUT", true, "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tag, model := "s1-"+tc.name, "s1-model-"+tc.name
			first := h.AttachCnode(t, cnodeSpec{name: "first", tags: []string{tag}, script: scriptAlways(tc.first)})
			second := h.AttachCnode(t, cnodeSpec{name: "second", tags: []string{tag}})
			defer second.Detach(t)
			defer first.Detach(t)

			cfg := map[string]any{"calculationNodesTags": tag, "responseTimeoutMs": 300}
			for k, v := range tc.config {
				cfg[k] = v
			}
			h.SetupModelWithWorkflow(t, model, chainWorkflowJSON("s1-wf-"+tc.name, procSpec{"s1-proc", "SYNC", cfg}))

			_, status, body := h.CreateEntity(t, model, 1, workflowSampleModel)
			if tc.wantStatus == http.StatusOK {
				if status != http.StatusOK {
					t.Fatalf("create: %d %s; want 200 (the second cnode answers)", status, body)
				}
			} else {
				pd := assertProblem(t, status, body, tc.wantStatus, tc.wantCode, tc.wantRetryable)
				if tc.wantDetail != "" && !strings.Contains(pd.Detail, tc.wantDetail) {
					t.Errorf("detail = %q; want it to contain %q", pd.Detail, tc.wantDetail)
				}
				if strings.Contains(pd.Detail, "processor dispatch failed:") {
					t.Errorf("detail = %q; the inner \"processor dispatch failed:\" segment is gone", pd.Detail)
				}
				if n := h.countEntities(t, model); n != 0 {
					t.Errorf("%d entities committed by a failed SYNC create; want 0", n)
				}
			}

			got1, got2 := first.Received(), second.Received()
			if len(got1) != 1 {
				t.Fatalf("first cnode received %d callouts; want 1 (it was attached first): %v", len(got1), got1)
			}
			if len(got2) != tc.wantSecond {
				t.Fatalf("second cnode received %d callouts; want %d: %v", len(got2), tc.wantSecond, got2)
			}
			if tc.wantSecond == 1 {
				if got1[0].RequestID == "" || got1[0].RequestID != got2[0].RequestID {
					t.Errorf("request ids differ across tries: %q then %q", got1[0].RequestID, got2[0].RequestID)
				}
				if got1[0].EventID == got2[0].EventID {
					t.Errorf("both tries carry CloudEvent id %q; the envelope id is unique per event", got1[0].EventID)
				}
				if got1[0].Pass() == got2[0].Pass() {
					t.Error("both tries carry the same pass; every try mints its own")
				}
			}
		})
	}
}
```

- [ ] **Step 2: Prove the scenario has teeth**

Run: `go test ./internal/e2e/ -run 'TestCalloutFailover_Processor'` — GREEN on the branch head.

Merge-base (+H): `no-answer-idempotent`, `drop-idempotent` and `retry-policy-none` fail at `import workflow … 400` (`idempotent` is not a known field) — after C alone they fail `create: 503 …; want 200`.

Temporary revert (one at a time, never committed):
1. `internal/contract` `CalloutFailureKind.MayTryAnother`: return `false` for `NoAnswer` → `no-answer-idempotent` and `drop-idempotent` FAIL `create: 503`.
2. The same function: return `true` for `MemberFailed` and `Terminal` → the three `failed-*` cases and `terminal` FAIL `second cnode received 1 callouts; want 0`.
3. In the Coordinator's tries resolution (D3), treat `NONE` as `FIXED` → `retry-policy-none` FAILS `second cnode received 1`.

- [ ] **Step 3: (no production code)**

- [ ] **Step 4: Run the package**

Run: `go test ./internal/e2e/...`

- [ ] **Step 5: Commit**

```
git add internal/e2e/callout_scenarios_helpers_test.go internal/e2e/callout_failover_test.go internal/e2e/callback_harness_test.go
git commit -m "test(e2e): a processor's failed try and whether the next cnode is asked (#254)"
```

---

### Task S-2: e2e — criteria and functions are repeat-safe by rule; `retryPolicy: NONE` on each

**Spec:** D1, D2 ("Criteria and functions are repeat-safe by rule"), D3; §13 rows 1009, 1014 (criterion, function) — layer **E**.

**Files:**
- Modify: `internal/e2e/callout_failover_test.go` (append)

**Interfaces:**
- Consumes: S-1 helpers; H `answerMatches`, `answerResult`, `scheduleFunctionWorkflowJSON(wfName, fnJSON string)` (`scheduled_function_test.go:57`), `h.GetEntityState`.

- [ ] **Step 1: Write the scenario** (append; add import `encoding/json`)

```go
// TestCalloutFailover_CriterionAndFunction: a criterion and a schedule function
// go to the next cnode after a try that got no answer, with no declaration by
// the author; retryPolicy NONE keeps each to one try.
func TestCalloutFailover_CriterionAndFunction(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(3, 100*time.Millisecond))

	fnJSON := func(name, tag, policy string) string {
		fn := map[string]any{"name": name, "resultKind": "Schedule", "calculationNodesTags": tag, "responseTimeoutMs": 300}
		if policy != "" {
			fn["retryPolicy"] = policy
		}
		b, _ := json.Marshal(fn)
		return string(b)
	}
	critCfg := func(tag, policy string) map[string]any {
		cfg := map[string]any{"calculationNodesTags": tag, "responseTimeoutMs": 300}
		if policy != "" {
			cfg["retryPolicy"] = policy
		}
		return cfg
	}
	schedule := answerResult("Schedule", map[string]any{"fireAfterMs": int64(3600000)})

	cases := []struct {
		name       string
		workflow   func(tag string) string
		second     cnodeReply
		wantOK     bool
		wantState  string
		wantSecond int
	}{
		{"criterion", func(tag string) string { return criterionWorkflowJSON("s2-crit-wf", "s2-crit", critCfg(tag, "")) },
			answerMatches(true), true, "DONE", 1},
		{"criterion-none", func(tag string) string { return criterionWorkflowJSON("s2-crit-none-wf", "s2-crit", critCfg(tag, "NONE")) },
			answerMatches(true), false, "", 0},
		{"function", func(tag string) string { return scheduleFunctionWorkflowJSON("s2-fn-wf", fnJSON("s2-fn", tag, "")) },
			schedule, true, "Open", 1},
		{"function-none", func(tag string) string { return scheduleFunctionWorkflowJSON("s2-fn-none-wf", fnJSON("s2-fn", tag, "NONE")) },
			schedule, false, "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tag, model := "s2-"+tc.name, "s2-model-"+tc.name
			first := h.AttachCnode(t, cnodeSpec{name: "first", tags: []string{tag}, script: scriptAlways(neverAnswer())})
			second := h.AttachCnode(t, cnodeSpec{name: "second", tags: []string{tag}, script: scriptAlways(tc.second)})
			defer second.Detach(t)
			defer first.Detach(t)
			h.SetupModelWithWorkflow(t, model, tc.workflow(tag))

			id, status, body := h.CreateEntity(t, model, 1, workflowSampleModel)
			if tc.wantOK {
				if status != http.StatusOK {
					t.Fatalf("create: %d %s; want 200", status, body)
				}
				if st, _ := h.GetEntityState(t, id); st != tc.wantState {
					t.Errorf("state = %q; want %q", st, tc.wantState)
				}
			} else {
				assertProblem(t, status, body, http.StatusServiceUnavailable, "DISPATCH_TIMEOUT", true)
			}
			got1, got2 := first.Received(), second.Received()
			if len(got1) != 1 || len(got2) != tc.wantSecond {
				t.Fatalf("first received %d, second %d; want 1 and %d", len(got1), len(got2), tc.wantSecond)
			}
			if tc.wantSecond == 1 && got1[0].RequestID != got2[0].RequestID {
				t.Errorf("request ids differ across tries: %q then %q", got1[0].RequestID, got2[0].RequestID)
			}
		})
	}
}
```

- [ ] **Step 2: Prove the scenario has teeth**

Run: `go test ./internal/e2e/ -run 'TestCalloutFailover_CriterionAndFunction'`.
Merge-base (+H, +C): `criterion` and `function` FAIL `create: 503 DISPATCH_TIMEOUT; want 200`.
Temporary revert: in `grpc.NewCriteriaCallout` and `grpc.NewFunctionCallout` set `RepeatSafe: false` → `criterion` and `function` FAIL `create: 503`. Separately, make the Coordinator ignore `Config.RetryPolicy` for criteria/functions → the two `-none` cases FAIL `second 1; want 0` (and `create: 200`).

- [ ] **Step 4: Run the package** — `go test ./internal/e2e/...`

- [ ] **Step 5: Commit**

```
git add internal/e2e/callout_failover_test.go
git commit -m "test(e2e): criteria and functions fail over by rule; retryPolicy NONE is one try (#254)"
```

---

### Task S-3: e2e — what the client is told when tries run out

**Spec:** §5 "Precedence when nothing more can be done"; §8.2 rows 4, 5 (`CALLOUT_FAILED`, "exactly one attempt → not wrapped") and "Member ids appear in client-visible text … node ids and peer addresses do not"; §13 rows 1015, 1016, 1017 — layer **E**.

**Files:**
- Create: `internal/e2e/callout_errors_test.go`

**Interfaces:**
- Consumes: S-1 helpers; H `(c) MemberID()`.
- Produces (for S-10): `type calloutFailedMsg struct{ n int; perMember map[string]int }`, `func parseCalloutFailed(t *testing.T, detail string) calloutFailedMsg`.

The literal rendering of one entry — `[member<id>: cause]` in §8.2 — is the owner's-loop stream's to fix; R§5 quotes Cloud's as `member<id>` with `<id>` a placeholder. The parser below accepts `member<id>`, `member <id>` and `memberID` so that the scenario pins the shape §8.2 states (the sentence, the count before collapsing, one bracketed entry per distinct member, `(k times)`) and not a punctuation choice. See Open point 3.

- [ ] **Step 1: Write the scenarios**

```go
package e2e_test

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/app"
)

var (
	calloutFailedRe      = regexp.MustCompile(`the callout could not be completed, got (\d+) failures: (.*)$`)
	calloutFailedEntryRe = regexp.MustCompile(`\[member ?<?([^>:\]\s]+)>?: [^\]]*?(?: \((\d+) times\))?\]`)
)

// calloutFailedMsg is a parsed CALLOUT_FAILED detail.
type calloutFailedMsg struct {
	n         int            // the count the message states
	perMember map[string]int // member id -> failures attributed to it
}

func parseCalloutFailed(t *testing.T, detail string) calloutFailedMsg {
	t.Helper()
	m := calloutFailedRe.FindStringSubmatch(detail)
	if m == nil {
		t.Fatalf("detail = %q; want \"the callout could not be completed, got N failures: [...]\"", detail)
	}
	out := calloutFailedMsg{perMember: map[string]int{}}
	out.n, _ = strconv.Atoi(m[1])
	sum := 0
	for _, e := range calloutFailedEntryRe.FindAllStringSubmatch(m[2], -1) {
		k := 1
		if e[2] != "" {
			k, _ = strconv.Atoi(e[2])
		}
		out.perMember[e[1]] += k
		sum += k
	}
	if sum != out.n {
		t.Errorf("detail = %q; it states %d failures but its entries add up to %d", detail, out.n, sum)
	}
	return out
}

// TestCalloutErrors_EveryTryUsed: two tries, two cnodes, both silent on an
// idempotent processor -> 503 CALLOUT_FAILED naming both members, and nothing
// that identifies a pnode.
func TestCalloutErrors_EveryTryUsed(t *testing.T) {
	const nodeID = "s3-owner-pnode"
	h := newCalloutHarness(t, func(cfg *app.Config) {
		calloutTuning(1, 0)(cfg)
		cfg.Cluster.NodeID = nodeID
	})
	const tag, model = "s3-all-used", "s3-model-all-used"
	a := h.AttachCnode(t, cnodeSpec{name: "a", tags: []string{tag}, script: scriptAlways(neverAnswer())})
	b := h.AttachCnode(t, cnodeSpec{name: "b", tags: []string{tag}, script: scriptAlways(neverAnswer())})
	h.SetupModelWithWorkflow(t, model, chainWorkflowJSON("s3-all-used-wf", procSpec{"s3-proc", "SYNC",
		map[string]any{"calculationNodesTags": tag, "responseTimeoutMs": 300, "idempotent": true}}))

	_, status, body := h.CreateEntity(t, model, 1, workflowSampleModel)
	pd := assertProblem(t, status, body, http.StatusServiceUnavailable, "CALLOUT_FAILED", true)
	msg := parseCalloutFailed(t, pd.Detail)
	if msg.n != 2 || msg.perMember[a.MemberID()] != 1 || msg.perMember[b.MemberID()] != 1 {
		t.Errorf("detail = %q; want 2 failures, one for each of %s and %s", pd.Detail, a.MemberID(), b.MemberID())
	}
	for _, leak := range []string{"127.0.0.1", "localhost", nodeID} {
		if strings.Contains(pd.Detail, leak) {
			t.Errorf("detail = %q; it names a pnode (%q)", pd.Detail, leak)
		}
	}
	if n := h.countEntities(t, model); n != 0 {
		t.Errorf("%d entities committed; want 0", n)
	}
}

// TestCalloutErrors_OneAttemptIsNotWrapped: one try allowed, a second cnode is
// available but never asked -> the try's own code, unwrapped.
func TestCalloutErrors_OneAttemptIsNotWrapped(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(0, 0))
	const tag, model = "s3-one", "s3-model-one"
	h.AttachCnode(t, cnodeSpec{name: "silent", tags: []string{tag}, script: scriptAlways(neverAnswer())})
	spare := h.AttachCnode(t, cnodeSpec{name: "spare", tags: []string{tag}})
	h.SetupModelWithWorkflow(t, model, chainWorkflowJSON("s3-one-wf", procSpec{"s3-proc", "SYNC",
		map[string]any{"calculationNodesTags": tag, "responseTimeoutMs": 300, "idempotent": true}}))

	_, status, body := h.CreateEntity(t, model, 1, workflowSampleModel)
	pd := assertProblem(t, status, body, http.StatusServiceUnavailable, "DISPATCH_TIMEOUT", true)
	if strings.Contains(pd.Detail, "the callout could not be completed") {
		t.Errorf("detail = %q; a single attempt is reported as itself, not wrapped", pd.Detail)
	}
	if got := spare.Received(); len(got) != 0 {
		t.Errorf("the spare cnode received %d callouts; the one try was used", len(got))
	}
}

// TestCalloutErrors_AttemptsBeatNoCnode: every cnode drops on receiving the
// work, so when the patience runs out there is no cnode left — but tries were
// made, and the error reports them, not NO_COMPUTE_MEMBER_FOR_TAG.
func TestCalloutErrors_AttemptsBeatNoCnode(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(3, 200*time.Millisecond))

	t.Run("one-attempt", func(t *testing.T) {
		const tag, model = "s3-beat-1", "s3-model-beat-1"
		c := h.AttachCnode(t, cnodeSpec{name: "drop", tags: []string{tag}, script: scriptAlways(closeStream())})
		h.SetupModelWithWorkflow(t, model, chainWorkflowJSON("s3-beat-1-wf", procSpec{"s3-proc", "SYNC",
			map[string]any{"calculationNodesTags": tag, "idempotent": true}}))
		_, status, body := h.CreateEntity(t, model, 1, workflowSampleModel)
		assertProblem(t, status, body, http.StatusServiceUnavailable, "COMPUTE_MEMBER_DISCONNECTED", true)
		c.AwaitGone(t)
	})

	t.Run("two-attempts-tries-left", func(t *testing.T) {
		const tag, model = "s3-beat-2", "s3-model-beat-2"
		a := h.AttachCnode(t, cnodeSpec{name: "drop-a", tags: []string{tag}, script: scriptAlways(closeStream())})
		b := h.AttachCnode(t, cnodeSpec{name: "drop-b", tags: []string{tag}, script: scriptAlways(closeStream())})
		h.SetupModelWithWorkflow(t, model, chainWorkflowJSON("s3-beat-2-wf", procSpec{"s3-proc", "SYNC",
			map[string]any{"calculationNodesTags": tag, "idempotent": true}}))
		_, status, body := h.CreateEntity(t, model, 1, workflowSampleModel)
		pd := assertProblem(t, status, body, http.StatusServiceUnavailable, "CALLOUT_FAILED", true)
		msg := parseCalloutFailed(t, pd.Detail)
		if msg.n != 2 || msg.perMember[a.MemberID()] != 1 || msg.perMember[b.MemberID()] != 1 {
			t.Errorf("detail = %q; want 2 failures, one per dropped cnode", pd.Detail)
		}
	})
}
```

- [ ] **Step 2: Prove the scenarios have teeth**

Run: `go test ./internal/e2e/ -run 'TestCalloutErrors_'`.
Merge-base (+H, +C): `EveryTryUsed` FAILS `errorCode = "DISPATCH_TIMEOUT"; want "CALLOUT_FAILED"`; `AttemptsBeatNoCnode/two-attempts` likewise with `COMPUTE_MEMBER_DISCONNECTED`. `OneAttemptIsNotWrapped` and `one-attempt` are green there (single-shot) — pins.
Temporary revert: in the Coordinator's exhaustion path, wrap whenever `len(attempts) >= 1` → `OneAttemptIsNotWrapped` and `one-attempt` FAIL on the code. In the precedence branch, return `NO_COMPUTE_MEMBER_FOR_TAG` whenever the last pass found no cnode → both `AttemptsBeatNoCnode` subtests FAIL on the code.

- [ ] **Step 4:** `go test ./internal/e2e/...`

- [ ] **Step 5: Commit**

```
git add internal/e2e/callout_errors_test.go
git commit -m "test(e2e): CALLOUT_FAILED shape, the unwrapped single attempt, attempts beat no-cnode (#254)"
```

---

### Task S-4: e2e — patience, and a caller that stops waiting

**Spec:** D7; §5 "Patience", "A cancelled caller ends the loop at once", "only the caller's context ending … returns `ctx.Err()` unchanged (408 …)"; §8.2 rows 1 and 11-as-today; §13 rows 1020, 1021, 1023, 1024 — layer **E**. (Rows 1019 and 1022 are U only.)

**Files:**
- Create: `internal/e2e/callout_patience_test.go`

**Interfaces:**
- Consumes: S-1 helpers; H `h.CreateEntityRaw`, `createEntityResult`; existing `txctlAssert408` (`transaction_control_test.go:206`).

- [ ] **Step 1: Write the scenarios**

```go
package e2e_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// TestCalloutPatience_WaitsForACnode: with no cnode attached the callout waits;
// one attaches; the operation succeeds. One try is all that is allowed, so the
// wait itself used none. The same with retryPolicy NONE.
func TestCalloutPatience_WaitsForACnode(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(0, 15*time.Second))
	_ = h.token(t)

	for _, policy := range []string{"FIXED", "NONE"} {
		t.Run(policy, func(t *testing.T) {
			tag, model := "s4-wait-"+policy, "s4-model-wait-"+policy
			h.SetupModelWithWorkflow(t, model, chainWorkflowJSON("s4-wait-wf-"+policy, procSpec{"s4-proc", "SYNC",
				map[string]any{"calculationNodesTags": tag, "retryPolicy": policy}}))

			done := make(chan createEntityResult, 1)
			go func() { done <- h.CreateEntityRaw(model, 1, workflowSampleModel) }()

			select {
			case res := <-done:
				t.Fatalf("answered %d before any cnode attached: %s", res.status, res.body)
			case <-time.After(500 * time.Millisecond):
			}
			c := h.AttachCnode(t, cnodeSpec{name: "late-" + policy, tags: []string{tag}})
			defer c.Detach(t)

			select {
			case res := <-done:
				if res.err != nil || res.status != http.StatusOK {
					t.Fatalf("create: status=%d err=%v body=%s; want 200 once a cnode attached", res.status, res.err, res.body)
				}
			case <-time.After(15 * time.Second):
				t.Fatal("the callout did not notice the cnode that attached")
			}
			if got := c.Received(); len(got) != 1 {
				t.Errorf("the cnode received %d callouts; want 1", len(got))
			}
		})
	}
}

// TestCalloutPatience_NoCnode: the patience is what a callout with no cnode
// costs — and nothing when it is 0.
func TestCalloutPatience_NoCnode(t *testing.T) {
	cases := []struct {
		name     string
		patience time.Duration
		atLeast  time.Duration
		atMost   time.Duration
	}{
		{"patience-700ms", 700 * time.Millisecond, 650 * time.Millisecond, 4 * time.Second},
		{"patience-0", 0, 0, 3 * time.Second}, // "at once": well under the 5s default
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newCalloutHarness(t, calloutTuning(3, tc.patience))
			model := "s4-model-" + tc.name
			h.SetupModelWithWorkflow(t, model, chainWorkflowJSON("s4-wf-"+tc.name, procSpec{"s4-proc", "SYNC",
				map[string]any{"calculationNodesTags": "s4-nobody"}}))

			start := time.Now()
			_, status, body := h.CreateEntity(t, model, 1, workflowSampleModel)
			elapsed := time.Since(start)
			assertProblem(t, status, body, http.StatusServiceUnavailable, "NO_COMPUTE_MEMBER_FOR_TAG", true)
			if elapsed < tc.atLeast || elapsed > tc.atMost {
				t.Errorf("answered after %v; want between %v and %v", elapsed, tc.atLeast, tc.atMost)
			}
		})
	}
}

// TestCalloutCallerEnds: the caller's own deadline is a 408 whether it fires
// during a wait or during a try, and a caller that goes away ends the callout:
// no cnode is given the work afterwards.
func TestCalloutCallerEnds(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(3, 30*time.Second))
	_ = h.token(t)

	setup := func(t *testing.T, name string, cfg map[string]any) (model, tag string) {
		t.Helper()
		model, tag = "s4-model-"+name, "s4-"+name
		cfg["calculationNodesTags"] = tag
		h.SetupModelWithWorkflow(t, model, chainWorkflowJSON("s4-wf-"+name, procSpec{"s4-proc", "SYNC", cfg}))
		return model, tag
	}

	t.Run("408-during-a-wait", func(t *testing.T) {
		model, _ := setup(t, "408-wait", map[string]any{})
		start := time.Now()
		resp := h.DoAuth(t, http.MethodPost, fmt.Sprintf("/api/entity/JSON/%s/1?transactionTimeoutMillis=700", model), workflowSampleModel, "")
		txctlAssert408(t, h, resp)
		if el := time.Since(start); el > 10*time.Second {
			t.Errorf("408 after %v; the wait must end with the caller's deadline", el)
		}
	})

	t.Run("408-during-a-try", func(t *testing.T) {
		model, tag := setup(t, "408-try", map[string]any{"responseTimeoutMs": 30000})
		c := h.AttachCnode(t, cnodeSpec{name: "silent-408", tags: []string{tag}, script: scriptAlways(neverAnswer())})
		defer c.Detach(t)
		resp := h.DoAuth(t, http.MethodPost, fmt.Sprintf("/api/entity/JSON/%s/1?transactionTimeoutMillis=700", model), workflowSampleModel, "")
		txctlAssert408(t, h, resp)
	})

	t.Run("cancelled-during-a-wait", func(t *testing.T) {
		model, tag := setup(t, "cancel-wait", map[string]any{})
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		if _, err := h.joinedRequest(ctx, http.MethodPost, fmt.Sprintf("/api/entity/JSON/%s/1", model), workflowSampleModel, ""); err == nil {
			t.Fatal("the request finished; it was meant to be abandoned during the wait")
		}
		c := h.AttachCnode(t, cnodeSpec{name: "after-cancel", tags: []string{tag}})
		defer c.Detach(t)
		time.Sleep(700 * time.Millisecond)
		if got := c.Received(); len(got) != 0 {
			t.Errorf("a cnode attached after the caller went away received %d callouts; the callout had ended", len(got))
		}
	})

	t.Run("cancelled-during-a-try", func(t *testing.T) {
		model, tag := setup(t, "cancel-try", map[string]any{"responseTimeoutMs": 1500, "idempotent": true})
		first := h.AttachCnode(t, cnodeSpec{name: "silent-cancel", tags: []string{tag}, script: scriptAlways(neverAnswer())})
		second := h.AttachCnode(t, cnodeSpec{name: "spare-cancel", tags: []string{tag}})
		defer second.Detach(t)
		defer first.Detach(t)
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		if _, err := h.joinedRequest(ctx, http.MethodPost, fmt.Sprintf("/api/entity/JSON/%s/1", model), workflowSampleModel, ""); err == nil {
			t.Fatal("the request finished; it was meant to be abandoned during the try")
		}
		time.Sleep(2200 * time.Millisecond) // past the first try's answer limit
		if got := second.Received(); len(got) != 0 {
			t.Errorf("the spare cnode received %d callouts after the caller went away", len(got))
		}
		if n := h.countEntities(t, model); n != 0 {
			t.Errorf("%d entities committed for an abandoned create; want 0", n)
		}
	})
}
```

- [ ] **Step 2: Prove the scenarios have teeth**

Run: `go test ./internal/e2e/ -run 'TestCalloutPatience_|TestCalloutCallerEnds'`.
Merge-base (+H, +C): `WaitsForACnode` FAILS `answered 503 before any cnode attached` (a single pnode does not wait today); `NoCnode/patience-700ms` FAILS on `answered after …`; `408-during-a-wait` FAILS with 503. `patience-0`, `408-during-a-try` are pins.
Temporary revert: in the Coordinator's wait, replace `case <-ctx.Done()` by nothing (wait only on the change channels and the patience) → `408-during-a-wait` FAILS (no 408 within the client's 700 ms; the test sees the 408 only when the patience ends, `el > 10s`) and `cancelled-during-a-wait` FAILS `received 1 callouts`. Drop the `ctx.Err()` check between tries in `RunLocal` → `cancelled-during-a-try` FAILS.

- [ ] **Step 4:** `go test ./internal/e2e/...`

- [ ] **Step 5: Commit**

```
git add internal/e2e/callout_patience_test.go
git commit -m "test(e2e): patience on a single pnode, and a caller that stops waiting (#254)"
```

---

### Task S-5: e2e — the processor modes and a scheduled fire

**Spec:** §8.1 (all four rows); §17 ("changes nothing in `internal/scheduler`" — the callouts of a fire still go through the Coordinator); §13 rows 1025, 1026, 1027 — layer **E**.

**Files:**
- Create: `internal/e2e/callout_modes_test.go`

**Interfaces:**
- Consumes: S-1 helpers; H as before; `h.GetEntityState`.

- [ ] **Step 1: Write the scenarios**

```go
package e2e_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestCalloutModes_AsyncNewTx: a failed callout does not fail the operation and
// nothing of it reaches the client; the rules for asking another cnode are the
// same as in every other mode.
func TestCalloutModes_AsyncNewTx(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(3, 100*time.Millisecond))
	cases := []struct {
		name       string
		first      cnodeReply
		idempotent bool
		wantSecond int
	}{
		{"failed-verdict-true", answerFailVerdict("s5 async boom", true), true, 0},
		{"no-answer-not-idempotent", neverAnswer(), false, 0},
		{"no-answer-idempotent", neverAnswer(), true, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tag, model := "s5-async-"+tc.name, "s5-model-async-"+tc.name
			first := h.AttachCnode(t, cnodeSpec{name: "first", tags: []string{tag}, script: scriptAlways(tc.first)})
			second := h.AttachCnode(t, cnodeSpec{name: "second", tags: []string{tag}})
			defer second.Detach(t)
			defer first.Detach(t)
			h.SetupModelWithWorkflow(t, model, chainWorkflowJSON("s5-async-wf-"+tc.name, procSpec{"s5-proc", "ASYNC_NEW_TX",
				map[string]any{"calculationNodesTags": tag, "responseTimeoutMs": 300, "idempotent": tc.idempotent}}))

			id, status, body := h.CreateEntity(t, model, 1, workflowSampleModel)
			if status != http.StatusOK {
				t.Fatalf("create: %d %s; want 200 — an ASYNC_NEW_TX failure does not fail the operation", status, body)
			}
			if strings.Contains(body, "s5 async boom") || strings.Contains(body, "retryable") {
				t.Errorf("the response carries the cnode's failure: %s", body)
			}
			if st, _ := h.GetEntityState(t, id); st != "DONE" {
				t.Errorf("state = %q; want DONE", st)
			}
			if got := second.Received(); len(got) != tc.wantSecond {
				t.Errorf("second cnode received %d callouts; want %d", len(got), tc.wantSecond)
			}
		})
	}
}

// TestCalloutModes_CommitBeforeDispatch: in both variants a failed callout
// fails the operation and leaves TX_pre committed; an idempotent processor is
// tried on the next cnode.
func TestCalloutModes_CommitBeforeDispatch(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(3, 100*time.Millisecond))
	for _, newTx := range []bool{true, false} {
		for _, idempotent := range []bool{false, true} {
			name := map[bool]string{true: "newtx", false: "notx"}[newTx] + map[bool]string{true: "-idempotent", false: "-not-idempotent"}[idempotent]
			t.Run(name, func(t *testing.T) {
				tag, model := "s5-cbd-"+name, "s5-model-cbd-"+name
				first := h.AttachCnode(t, cnodeSpec{name: "first", tags: []string{tag}, script: scriptAlways(neverAnswer())})
				second := h.AttachCnode(t, cnodeSpec{name: "second", tags: []string{tag}})
				defer second.Detach(t)
				defer first.Detach(t)
				h.SetupModelWithWorkflow(t, model, chainWorkflowJSON("s5-cbd-wf-"+name, procSpec{"s5-proc", "COMMIT_BEFORE_DISPATCH",
					map[string]any{"calculationNodesTags": tag, "responseTimeoutMs": 300,
						"idempotent": idempotent, "startNewTxOnDispatch": newTx}}))

				_, status, body := h.CreateEntity(t, model, 1, workflowSampleModel)
				if idempotent {
					if status != http.StatusOK {
						t.Fatalf("create: %d %s; want 200 (the second cnode answers)", status, body)
					}
					if got := second.Received(); len(got) != 1 {
						t.Errorf("second cnode received %d callouts; want 1", len(got))
					}
				} else {
					assertProblem(t, status, body, http.StatusServiceUnavailable, "DISPATCH_TIMEOUT", true)
					if got := second.Received(); len(got) != 0 {
						t.Errorf("second cnode received %d callouts; want 0", len(got))
					}
				}
				if n := h.countEntities(t, model); n != 1 {
					t.Errorf("%d entities committed; want 1 — TX_pre stays committed whatever the callout does", n)
				}
			})
		}
	}
}

// TestCalloutModes_ScheduledFire: the processor of a scheduled transition is
// tried on the next cnode like any other, under one request id.
func TestCalloutModes_ScheduledFire(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(3, 100*time.Millisecond))
	const tag, model = "s5-sched", "s5-model-sched"
	first := h.AttachCnode(t, cnodeSpec{name: "first", tags: []string{tag}, script: scriptAlways(neverAnswer())})
	second := h.AttachCnode(t, cnodeSpec{name: "second", tags: []string{tag}})

	wf, _ := json.Marshal(map[string]any{
		"importMode": "REPLACE",
		"workflows": []any{map[string]any{
			"version": "1.5", "name": "s5-sched-wf", "initialState": "Open", "active": true,
			"states": map[string]any{
				"Open": map[string]any{"transitions": []any{map[string]any{
					"name": "AutoClose", "next": "Closed", "manual": false,
					"schedule": map[string]any{"delayMs": 200},
					"processors": []any{map[string]any{"type": "calculator", "name": "s5-sched-proc", "executionMode": "SYNC",
						"config": map[string]any{"attachEntity": true, "calculationNodesTags": tag,
							"responseTimeoutMs": 300, "idempotent": true}}},
				}}},
				"Closed": map[string]any{},
			},
		}},
	})
	h.SetupModelWithWorkflow(t, model, string(wf))

	id, status, body := h.CreateEntity(t, model, 1, workflowSampleModel)
	if status != http.StatusOK {
		t.Fatalf("create: %d %s", status, body)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		if st, _ := h.GetEntityState(t, id); st == "Closed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the scheduled transition did not complete; callouts: %v", h.ReceivedCallouts())
		}
		time.Sleep(100 * time.Millisecond)
	}
	got1, got2 := first.Received(), second.Received()
	if len(got1) < 1 || len(got2) < 1 {
		t.Fatalf("first received %d, second %d; want the silent cnode tried and the second to answer", len(got1), len(got2))
	}
	if got1[0].RequestID != got2[0].RequestID {
		t.Errorf("request ids differ across the tries of one fire: %q then %q", got1[0].RequestID, got2[0].RequestID)
	}
}
```

- [ ] **Step 2: Prove the scenarios have teeth**

Run: `go test ./internal/e2e/ -run 'TestCalloutModes_'`.
Merge-base (+H, +C): the three `…-idempotent` cases and `ScheduledFire` FAIL (no second try). The non-idempotent cases are green — pins of §8.1's "as today".
Temporary revert: revert #1 of S-1 (`MayTryAnother` false for `NoAnswer`) turns the same four red; additionally, in `executeAsyncNewTx` return the callout's error instead of logging it → the three `AsyncNewTx` cases FAIL `create: 400/503; want 200`.

- [ ] **Step 4:** `go test ./internal/e2e/...`

- [ ] **Step 5: Commit**

```
git add internal/e2e/callout_modes_test.go
git commit -m "test(e2e): failover in ASYNC_NEW_TX, both COMMIT_BEFORE_DISPATCH variants and a scheduled fire (#254)"
```

---

## The workflow shapes the fencing scenarios are built from

A late callback is only answered 410 while its transaction is **open** (§7
point 1: `Join` comes before `Admit`). Four shapes keep it open
deterministically; none depends on a sleep to create the condition — sleeps
appear only to observe that something has *not* happened before the test lets
it happen.

| Shape | Workflow | cnodes | The transaction is open because |
|---|---|---|---|
| **F-ended** | one transition, processor A (tag a) then processor B (tag b) | `a` answers (or fails, for `ASYNC_NEW_TX`); `b` = `scriptHold(release, answerOK())` | B's callout is in progress until the test closes `release`. A's recorded pass belongs to a callout that has *ended*. |
| **F-replaced** | one processor, `idempotent`, short `responseTimeoutMs` | `first` (attached first) never answers; `second` = `scriptHold` | the callout is in progress on `second`. `first`'s pass is *superseded* (`Advance`). |
| **F-blocked** | as F-replaced (or F-ended), plus a committed *victim* entity whose row the test holds `FOR UPDATE` from its own connection (`holdRowLock`, `storage_ceilings_e2e_test.go:345`) — or, for a read, the `messages` table held `ACCESS EXCLUSIVE` | the cnode's script starts a joined update of the victim (or a joined `GET /message/{id}`) on a goroutine | that joined request sits **inside PostgreSQL, holding the transaction's lock**, until the test releases its database lock. `awaitBlockedStatement` (S-8) proves it got there. |
| **F-nested** | outer model: processor OUT (`idempotent`, short limit). Inner model: processor IN. | `out1` calls back `CreateEntity(innerModel)` — whose workflow makes the inner callout to `in1`, which holds; `out2` = `scriptHold` | `out1`'s callback is waiting on a callout of its own when OUT's limit passes and the work goes to `out2`. |

A recorded pass is replayed by the test goroutine (`Replay*`): a callback is
nothing but a request bearing the pass, so who sends it does not matter, and
assertions stay on the test goroutine.

---

### Task S-6: e2e — a late callback: 410 while the transaction is open, 404 once it has ended

**Spec:** §7 "Where it is enforced" point 1, "`end` unregisters the callout"; §8.2 rows 10, 11; §13 rows 1028, 1029, 1030 — layer **E**.

**Files:**
- Create: `internal/e2e/callout_fencing_test.go`

**Interfaces:**
- Consumes: S-1 helpers; H `Replay*`, `(c) Received`, `secondaryWorkflow` (`callback_txjoin_test.go:27`).
- Produces (for S-7 … S-10): `func awaitCnodeReceived(t *testing.T, c *scriptedCnode, n int, within time.Duration) []receivedCallout`; `func closeOnce(ch chan struct{}) func()`; `func awaitCreate(t *testing.T, done <-chan createEntityResult, within time.Duration) createEntityResult`.

- [ ] **Step 1: Write the scenario**

```go
package e2e_test

import (
	"net/http"
	"sync"
	"testing"
	"time"
)

// awaitCnodeReceived waits until c has received at least n callouts.
func awaitCnodeReceived(t *testing.T, c *scriptedCnode, n int, within time.Duration) []receivedCallout {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if got := c.Received(); len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("cnode %s received %d callouts within %s; want %d (all: %v)", c.name, len(c.Received()), within, n, c.h.ReceivedCallouts())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// closeOnce returns a func that closes ch the first time it is called, so a
// test can release a held script early and still register it as a cleanup.
func closeOnce(ch chan struct{}) func() {
	var once sync.Once
	return func() { once.Do(func() { close(ch) }) }
}

// awaitCreate waits for a create driven from a goroutine.
func awaitCreate(t *testing.T, done <-chan createEntityResult, within time.Duration) createEntityResult {
	t.Helper()
	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("create: %v", res.err)
		}
		return res
	case <-time.After(within):
		t.Fatalf("the create did not complete within %s", within)
		return createEntityResult{}
	}
}

// TestCalloutFence_LateCallback (shape F-ended): processor A's callout has
// ended — answered, or failed under ASYNC_NEW_TX — while processor B keeps the
// transaction open. A's pass is refused 410 on both doors, write and read, and
// what it tried to write is not in the committed result. Once the transaction
// has ended the same pass is 404, as it always was.
func TestCalloutFence_LateCallback(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(3, 100*time.Millisecond))
	_ = h.token(t)

	cases := []struct {
		name   string
		mode   string
		aReply cnodeReply
	}{
		{"answered-sync", "SYNC", answerOK()},
		{"failed-async-new-tx", "ASYNC_NEW_TX", answerFail("s6 failed")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model, secondary := "s6-model-"+tc.name, "s6-secondary-"+tc.name
			tagA, tagB := "s6-a-"+tc.name, "s6-b-"+tc.name
			h.SetupModelWithWorkflow(t, secondary, secondaryWorkflow)
			h.SetupModelWithWorkflow(t, model, chainWorkflowJSON("s6-wf-"+tc.name,
				procSpec{"s6-proc-a", tc.mode, map[string]any{"calculationNodesTags": tagA}},
				procSpec{"s6-proc-b", "SYNC", map[string]any{"calculationNodesTags": tagB}}))

			release := make(chan struct{})
			releaseNow := closeOnce(release)
			t.Cleanup(releaseNow)
			a := h.AttachCnode(t, cnodeSpec{name: "a", tags: []string{tagA}, script: scriptAlways(tc.aReply)})
			b := h.AttachCnode(t, cnodeSpec{name: "b", tags: []string{tagB}, script: scriptHold(release, answerOK())})
			defer b.Detach(t)
			defer a.Detach(t)

			done := make(chan createEntityResult, 1)
			go func() { done <- h.CreateEntityRaw(model, 1, workflowSampleModel) }()

			awaitCnodeReceived(t, b, 1, 15*time.Second) // A's callout has ended; B's is in progress
			ended := a.Received()[0]

			assertRefusedOnAllDoors(t, h, ended.Pass(), secondary, ended.EntityID,
				http.StatusGone, "CALLOUT_SUPERSEDED", supersededDetail)

			releaseNow()
			if res := awaitCreate(t, done, 15*time.Second); res.status != http.StatusOK {
				t.Fatalf("create: %d %s; want 200 — a refused late callback does not touch the operation", res.status, res.body)
			}
			if n := h.countEntities(t, secondary); n != 0 {
				t.Errorf("%d secondary entities committed; the late callback's write must not be in the result", n)
			}

			// The transaction has ended: looked up first, so 404 as today.
			assertRefusedOnAllDoors(t, h, ended.Pass(), secondary, ended.EntityID,
				http.StatusNotFound, "TRANSACTION_NOT_FOUND", "")
		})
	}
}
```

- [ ] **Step 2: Prove the scenario has teeth**

Run: `go test ./internal/e2e/ -run 'TestCalloutFence_LateCallback'`.
Merge-base (+H): both cases FAIL at the first door, `status = 200; want 410 CALLOUT_SUPERSEDED` — the hole §7 describes; in `failed-async-new-tx` the late write would also be committed.
Temporary revert: in `txjoin.JoinFromToken`, skip the `fence.Admit` call (pass the joined context on unchanged) → the same failure. Second revert, for the 404 half: call `Admit` **before** `txMgr.Join` → the last block FAILS `status = 410; want 404`.

- [ ] **Step 4:** `go test ./internal/e2e/...`

- [ ] **Step 5: Commit**

```
git add internal/e2e/callout_fencing_test.go
git commit -m "test(e2e): a late callback is 410 while its transaction is open and 404 after (#254)"
```

---

### Task S-7: e2e — a cnode that was replaced, and a callback waiting on a callout of its own

**Spec:** §7 "The fencing number" (`Advance` before each local try), point 4 ("A callback waiting on a callout of its own is released"), point 3, "A refused chain performs no store operation"; §8.2 row 10; §13 rows 1031, 1035, 1049 — layer **E**. Row 1036 (the savepoint is neither undone nor released) is U only; here the `ASYNC_NEW_TX` case asserts what a client can see: 410, and nothing of the chain committed.

**Files:**
- Modify: `internal/e2e/callout_fencing_test.go` (append)

**Interfaces:**
- Consumes: S-1, S-6 helpers; H `rc.CreateEntity`.

`COMMIT_BEFORE_DISPATCH` is left out of "every processor mode" for the inner processor: a workflow that runs inside a callback and contains such a processor commits the *outer* transaction — the defect §7 names and files separately. A scenario built on it would pin the defect.

- [ ] **Step 1: Write the scenarios** (append; the file's imports become `context`, `net/http`, `strings`, `sync`, `testing`, `time`)

```go
// TestCalloutFence_FirstCnodeRefusedOnceReplaced (shape F-replaced): the owner
// gave the work to a second cnode of its own; while that callout is still in
// progress the first cnode's pass is refused at once on every door, and the
// second's is admitted.
func TestCalloutFence_FirstCnodeRefusedOnceReplaced(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(3, 100*time.Millisecond))
	_ = h.token(t)
	const model, secondary, tag = "s7-replaced", "s7-replaced-secondary", "s7-replaced"
	h.SetupModelWithWorkflow(t, secondary, secondaryWorkflow)
	h.SetupModelWithWorkflow(t, model, chainWorkflowJSON("s7-replaced-wf", procSpec{"s7-proc", "SYNC",
		map[string]any{"calculationNodesTags": tag, "responseTimeoutMs": 400, "idempotent": true}}))

	release := make(chan struct{})
	releaseNow := closeOnce(release)
	t.Cleanup(releaseNow)
	first := h.AttachCnode(t, cnodeSpec{name: "first", tags: []string{tag}, script: scriptAlways(neverAnswer())})
	second := h.AttachCnode(t, cnodeSpec{name: "second", tags: []string{tag}, script: scriptHold(release, answerOK())})

	done := make(chan createEntityResult, 1)
	go func() { done <- h.CreateEntityRaw(model, 1, workflowSampleModel) }()

	current := awaitCnodeReceived(t, second, 1, 15*time.Second)[0]
	replaced := first.Received()[0]

	assertRefusedOnAllDoors(t, h, replaced.Pass(), secondary, replaced.EntityID,
		http.StatusGone, "CALLOUT_SUPERSEDED", supersededDetail)

	// Control: the cnode that holds the work now is admitted, read and write.
	if res, err := h.ReplayGetHTTP(current.Pass(), current.EntityID); err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("the current cnode's joined read: status=%d err=%v body=%s; want 200", res.StatusCode, err, res.Body)
	}
	if res, err := h.ReplayCreateHTTP(current.Pass(), secondary, 1, `{"name":"current-child","amount":1,"status":"ok"}`); err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("the current cnode's joined write: status=%d err=%v body=%s; want 200", res.StatusCode, err, res.Body)
	}

	releaseNow()
	if res := awaitCreate(t, done, 15*time.Second); res.status != http.StatusOK {
		t.Fatalf("create: %d %s; want 200", res.status, res.body)
	}
	if n := h.countEntities(t, secondary); n != 1 {
		t.Errorf("%d secondary entities committed; want exactly the current cnode's one", n)
	}
}

// TestCalloutFence_NestedCalloutReleased (shape F-nested): out1's callback is
// waiting on a callout of its own when out1 is replaced. The inner callout
// ends, the callback is answered 410 — not 200 — in every processor mode of the
// inner processor, ASYNC_NEW_TX as the last processor included; the inner
// cnode's pass, which names the replaced callout as enclosing, is refused; and
// nothing of the chain is committed.
func TestCalloutFence_NestedCalloutReleased(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(3, 100*time.Millisecond))
	_ = h.token(t)

	for _, innerMode := range []string{"SYNC", "ASYNC_SAME_TX", "ASYNC_NEW_TX"} {
		t.Run(innerMode, func(t *testing.T) {
			sfx := strings.ToLower(strings.ReplaceAll(innerMode, "_", "-"))
			outer, inner, third := "s7-outer-"+sfx, "s7-inner-"+sfx, "s7-third-"+sfx
			tagOut, tagIn := "s7-out-"+sfx, "s7-in-"+sfx
			h.SetupModelWithWorkflow(t, third, secondaryWorkflow)
			h.SetupModelWithWorkflow(t, inner, chainWorkflowJSON("s7-inner-wf-"+sfx,
				procSpec{"s7-in", innerMode, map[string]any{"calculationNodesTags": tagIn}}))
			h.SetupModelWithWorkflow(t, outer, chainWorkflowJSON("s7-outer-wf-"+sfx,
				procSpec{"s7-out", "SYNC", map[string]any{"calculationNodesTags": tagOut, "responseTimeoutMs": 700, "idempotent": true}}))

			holdIn, holdOut2 := make(chan struct{}), make(chan struct{})
			releaseIn, releaseOut2 := closeOnce(holdIn), closeOnce(holdOut2)
			t.Cleanup(releaseIn)
			t.Cleanup(releaseOut2)

			out1Res := make(chan callbackResult, 1)
			out1 := h.AttachCnode(t, cnodeSpec{name: "out1", tags: []string{tagOut},
				script: func(_ context.Context, _ receivedCallout, rc *reqCtx) cnodeReply {
					res, err := rc.CreateEntity(inner, 1, workflowSampleModel)
					if err != nil {
						res = callbackResult{StatusCode: -1, Body: err.Error()}
					}
					out1Res <- res
					return neverAnswer()
				}})
			in1 := h.AttachCnode(t, cnodeSpec{name: "in1", tags: []string{tagIn}, script: scriptHold(holdIn, answerOK())})
			out2 := h.AttachCnode(t, cnodeSpec{name: "out2", tags: []string{tagOut}, script: scriptHold(holdOut2, answerOK())})
			defer out2.Detach(t)
			defer in1.Detach(t)
			defer out1.Detach(t)

			done := make(chan createEntityResult, 1)
			go func() { done <- h.CreateEntityRaw(outer, 1, workflowSampleModel) }()

			innerCall := awaitCnodeReceived(t, in1, 1, 15*time.Second)[0] // out1's callback is now waiting on IN
			awaitCnodeReceived(t, out2, 1, 15*time.Second)               // OUT was given to out2: the wait is over

			select {
			case res := <-out1Res:
				pd := assertProblem(t, res.StatusCode, res.Body, http.StatusGone, "CALLOUT_SUPERSEDED", false)
				if !strings.Contains(pd.Detail, supersededDetail) {
					t.Errorf("detail = %q; want %q", pd.Detail, supersededDetail)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("out1's callback was not released when out1 was replaced")
			}

			// in1 still holds the inner work; its pass names the replaced callout
			// as enclosing, and its own callout has been ended.
			assertRefusedOnAllDoors(t, h, innerCall.Pass(), third, innerCall.EntityID,
				http.StatusGone, "CALLOUT_SUPERSEDED", supersededDetail)

			releaseIn()
			releaseOut2()
			if res := awaitCreate(t, done, 15*time.Second); res.status != http.StatusOK {
				t.Fatalf("outer create: %d %s; want 200", res.status, res.body)
			}
			if n := h.countEntities(t, inner); n != 0 {
				t.Errorf("%d inner entities committed; the superseded chain must write nothing further", n)
			}
			if n := h.countEntities(t, third); n != 0 {
				t.Errorf("%d third-model entities committed; want 0", n)
			}
			if n := len(out1.Received()); n != 1 {
				t.Errorf("out1 received %d callouts; want 1", n)
			}
		})
	}
}
```

- [ ] **Step 2: Prove the scenarios have teeth**

Run: `go test ./internal/e2e/ -run 'TestCalloutFence_FirstCnodeRefusedOnceReplaced|TestCalloutFence_NestedCalloutReleased'`.
Merge-base (+H, +C): both FAIL in `awaitCnodeReceived(second|out2)` — there is no second try.
Temporary reverts: (1) in the Coordinator's `TryNumberer.Next`, skip `Fence.Advance` → `FirstCnodeRefused…` FAILS `status = 200; want 410`. (2) In `Fence.Begin`, return the caller's context instead of the cancellable one → `NestedCalloutReleased` FAILS `out1's callback was not released` (out2 is never reached: the wait blocks on nothing, but the inner callout runs its full 30 s limit — the test's 15 s wait on `out2` fails first). (3) Remove the `fence.Check` after the mode switch in `executeProcessors` (§7 point 3) → the `ASYNC_NEW_TX` subtest FAILS `status = 200; want 410` and `1 inner entities committed`.

- [ ] **Step 4:** `go test ./internal/e2e/...`

- [ ] **Step 5: Commit**

```
git add internal/e2e/callout_fencing_test.go
git commit -m "test(e2e): a replaced cnode is refused at once; a callback waiting on its own callout is released (#254)"
```

---

### Task S-8: e2e — the wait: a joined request in progress is never overtaken, never interrupted

**Spec:** §7 "The wait", point 2 (the check under the lock), "There is deliberately no check before a joined operation's final write", "A joined request is not interrupted by its cnode going away", "The fence never cancels a callback's context"; §13 rows 1032, 1033, 1034, 1037, 1038, 1045, 1051 — layer **E** (PostgreSQL).

**Files:**
- Create: `internal/e2e/callout_fencing_wait_test.go`

**Interfaces:**
- Consumes: S-1, S-6 helpers; existing `holdRowLock` (`storage_ceilings_e2e_test.go:345`), `withAppName`, `pgURLFromEnv` (`tx_lifecycle_e2e_test.go:94, 149`), the package's `dbPool` (`e2e_test.go:37`), `workflowV1` (`workflow_test.go:27`: NONE → CREATED, manual `approve` → APPROVED), `h.GetEntityData`, `h.GetEntityState`.
- Produces: `func awaitBlockedStatement(t *testing.T, table string)`, `func holdMessagesTableLock(t *testing.T) func()`, `func (h *callbackHarness) seedVictim(t *testing.T, model string) string`.

How "in progress" is made deterministic (shape F-blocked). A joined **write** to a committed row the test holds `FOR UPDATE` waits inside PostgreSQL, on the operation's own connection, while the join layer holds the transaction's lock for it. A joined **read** cannot be blocked by a row lock, so the read scenarios use `GET /message/{id}` with the `messages` table held `ACCESS EXCLUSIVE` by the test. `storage_ceilings_e2e_test.go:340-344` forbids a table lock on `entities` because the shared stack's background loops write there; nothing in this package's background loops touches `messages`, no test runs in parallel (`grep -c 't.Parallel' internal/e2e/*_test.go` → 0), and the lock is held for about two seconds. `awaitBlockedStatement` polls `pg_stat_activity` until a backend is waiting on a lock with the table in its statement, so the test never guesses whether the request has reached the database.

- [ ] **Step 1: Write the helpers and the scenarios**

```go
package e2e_test

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	victimUpdate = `{"name": "Test Order", "amount": 100, "status": "held"}`
	lateChild    = `{"name":"late-child","amount":1,"status":"late"}`
)

// awaitBlockedStatement returns once some backend is waiting on a lock while
// running a statement that names table.
func awaitBlockedStatement(t *testing.T, table string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		err := dbPool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_stat_activity
			  WHERE wait_event_type = 'Lock' AND state = 'active' AND query ILIKE '%' || $1 || '%'`, table).Scan(&n)
		if err != nil {
			t.Fatalf("pg_stat_activity: %v", err)
		}
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no statement on %s is waiting on a lock after 10s; the joined request never reached the database", table)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// holdMessagesTableLock holds ACCESS EXCLUSIVE on messages from a connection of
// the test's own until the returned func is called, so a joined message read
// waits inside PostgreSQL. See the task text for why this table, and only it.
func holdMessagesTableLock(t *testing.T) func() {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	pool, err := pgxpool.New(ctx, withAppName(t, pgURLFromEnv(t), "callout-fence-locker"))
	if err != nil {
		cancel()
		t.Fatalf("open locker pool: %v", err)
	}
	tx, err := pool.Begin(ctx)
	if err == nil {
		_, err = tx.Exec(ctx, `LOCK TABLE messages IN ACCESS EXCLUSIVE MODE`)
	}
	if err != nil {
		pool.Close()
		cancel()
		t.Fatalf("lock messages: %v", err)
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			_ = tx.Rollback(ctx)
			pool.Close()
			cancel()
		})
	}
	t.Cleanup(release)
	return release
}

// seedVictim commits one entity on a processor-free workflow and returns its id.
func (h *callbackHarness) seedVictim(t *testing.T, model string) string {
	t.Helper()
	h.SetupModelWithWorkflow(t, model, workflowV1)
	id, status, body := h.CreateEntity(t, model, 1, workflowSampleModel)
	if status != http.StatusOK {
		t.Fatalf("seed victim: %d %s", status, body)
	}
	return id
}

// goJoined runs one joined HTTP request off the script's goroutine and delivers
// its outcome.
func goJoined(h *callbackHarness, method, path, body, pass string) <-chan callbackResult {
	out := make(chan callbackResult, 1)
	go func() {
		res, err := h.callback(method, path, body, pass)
		if err != nil {
			res = callbackResult{StatusCode: -1, Body: err.Error()}
		}
		out <- res
	}()
	return out
}

func awaitResult(t *testing.T, what string, ch <-chan callbackResult) callbackResult {
	t.Helper()
	select {
	case res := <-ch:
		return res
	case <-time.After(15 * time.Second):
		t.Fatalf("%s did not complete within 15s", what)
		return callbackResult{}
	}
}

// TestCalloutFence_OwnerWaitsForARequestInProgress (F-replaced + F-blocked):
// the first cnode has one joined write inside PostgreSQL and a second queued
// for the transaction's lock when its answer limit passes. The second cnode is
// not given the work until the first write has finished; that write lands and
// is answered 200; the queued one is refused on taking the lock and writes
// nothing.
func TestCalloutFence_OwnerWaitsForARequestInProgress(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(3, 100*time.Millisecond))
	_ = h.token(t)
	const model, secondary, tag = "s8-wait", "s8-wait-secondary", "s8-wait"
	victim := h.seedVictim(t, "s8-wait-victim")
	h.SetupModelWithWorkflow(t, secondary, secondaryWorkflow)
	h.SetupModelWithWorkflow(t, model, chainWorkflowJSON("s8-wait-wf", procSpec{"s8-proc", "SYNC",
		map[string]any{"calculationNodesTags": tag, "responseTimeoutMs": 1500, "idempotent": true}}))

	unlock := holdRowLock(t, victim)
	defer unlock()

	queueNow := make(chan struct{})
	t.Cleanup(closeOnce(queueNow))
	inProgress := make(chan (<-chan callbackResult), 1)
	queued := make(chan callbackResult, 1)
	first := h.AttachCnode(t, cnodeSpec{name: "first", tags: []string{tag},
		script: func(ctx context.Context, call receivedCallout, rc *reqCtx) cnodeReply {
			inProgress <- goJoined(h, http.MethodPut, "/api/entity/JSON/"+victim, victimUpdate, call.Pass())
			select {
			case <-queueNow:
			case <-ctx.Done():
				return neverAnswer()
			}
			res, err := rc.CreateEntity(secondary, 1, lateChild) // queues behind the write in progress
			if err != nil {
				res = callbackResult{StatusCode: -1, Body: err.Error()}
			}
			queued <- res
			return neverAnswer()
		}})
	second := h.AttachCnode(t, cnodeSpec{name: "second", tags: []string{tag}})
	_ = first

	done := make(chan createEntityResult, 1)
	go func() { done <- h.CreateEntityRaw(model, 1, workflowSampleModel) }()

	writeRes := <-inProgress
	awaitBlockedStatement(t, "entities")
	close(queueNow)

	time.Sleep(2 * time.Second) // the first try's 1.5s answer limit has passed
	if got := second.Received(); len(got) != 0 {
		t.Fatalf("the second cnode was given the work while a request of the first is in progress: %v", got)
	}

	unlock()
	if res := awaitResult(t, "the write in progress", writeRes); res.StatusCode != http.StatusOK {
		t.Errorf("the write in progress: %d %s; want 200 — it made its check before the number rose, and it lands", res.StatusCode, res.Body)
	}
	res := awaitResult(t, "the queued write", queued)
	assertProblem(t, res.StatusCode, res.Body, http.StatusGone, "CALLOUT_SUPERSEDED", false)

	awaitCnodeReceived(t, second, 1, 15*time.Second)
	if res := awaitCreate(t, done, 15*time.Second); res.status != http.StatusOK {
		t.Fatalf("create: %d %s; want 200", res.status, res.body)
	}
	if got := h.GetEntityData(t, victim)["status"]; got != "held" {
		t.Errorf("victim.status = %v; want \"held\" — the write in progress landed in the transaction that committed", got)
	}
	if n := h.countEntities(t, secondary); n != 0 {
		t.Errorf("%d secondary entities committed; the queued write must have written nothing", n)
	}
}

// TestCalloutFence_AsyncNewTxFailedWriteIsNotCommitted (F-blocked): an
// ASYNC_NEW_TX processor's cnode has a write inside PostgreSQL when its try
// times out. The engine does not carry on until that write has finished, and
// the write is then undone with the processor's savepoint.
func TestCalloutFence_AsyncNewTxFailedWriteIsNotCommitted(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(3, 100*time.Millisecond))
	_ = h.token(t)
	const model, tagA, tagB = "s8-async", "s8-async-a", "s8-async-b"
	victim := h.seedVictim(t, "s8-async-victim")
	h.SetupModelWithWorkflow(t, model, chainWorkflowJSON("s8-async-wf",
		procSpec{"s8-proc-a", "ASYNC_NEW_TX", map[string]any{"calculationNodesTags": tagA, "responseTimeoutMs": 1000}},
		procSpec{"s8-proc-b", "SYNC", map[string]any{"calculationNodesTags": tagB}}))

	unlock := holdRowLock(t, victim)
	defer unlock()

	inProgress := make(chan (<-chan callbackResult), 1)
	h.AttachCnode(t, cnodeSpec{name: "a", tags: []string{tagA},
		script: func(_ context.Context, call receivedCallout, _ *reqCtx) cnodeReply {
			inProgress <- goJoined(h, http.MethodPut, "/api/entity/JSON/"+victim, victimUpdate, call.Pass())
			return neverAnswer()
		}})
	b := h.AttachCnode(t, cnodeSpec{name: "b", tags: []string{tagB}})

	done := make(chan createEntityResult, 1)
	go func() { done <- h.CreateEntityRaw(model, 1, workflowSampleModel) }()

	writeRes := <-inProgress
	awaitBlockedStatement(t, "entities")
	time.Sleep(1500 * time.Millisecond) // A's answer limit has passed; its callout has failed
	if got := b.Received(); len(got) != 0 {
		t.Fatalf("the engine carried on to processor B while A's write is in progress: %v", got)
	}

	unlock()
	if res := awaitResult(t, "A's write in progress", writeRes); res.StatusCode != http.StatusOK {
		t.Errorf("A's write in progress: %d %s; want 200", res.StatusCode, res.Body)
	}
	if res := awaitCreate(t, done, 15*time.Second); res.status != http.StatusOK {
		t.Fatalf("create: %d %s; want 200 — an ASYNC_NEW_TX failure does not fail the operation", res.status, res.body)
	}
	if got := h.GetEntityData(t, victim)["status"]; got != "draft" {
		t.Errorf("victim.status = %v; want \"draft\" — the failed processor's write is undone with its savepoint", got)
	}
	if n := len(b.Received()); n != 1 {
		t.Errorf("processor B ran %d times; want 1", n)
	}
}

// TestCalloutFence_CallbackPastItsLastCheckLandsWhole (F-blocked): a cnode
// answers while its own joined collection update is still inside PostgreSQL.
// The engine does not carry on until the collection has finished; the
// collection lands whole and is answered 200.
func TestCalloutFence_CallbackPastItsLastCheckLandsWhole(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(3, 100*time.Millisecond))
	_ = h.token(t)
	const model, tagA, tagB = "s8-whole", "s8-whole-a", "s8-whole-b"
	v1 := h.seedVictim(t, "s8-whole-victim")
	_, status, body := h.CreateEntity(t, "s8-whole-victim", 1, workflowSampleModel)
	if status != http.StatusOK {
		t.Fatalf("seed second victim: %d %s", status, body)
	}
	var v2 string
	{
		var arr []map[string]any
		_ = json.Unmarshal([]byte(body), &arr)
		ids, _ := arr[0]["entityIds"].([]any)
		v2, _ = ids[0].(string)
	}
	h.SetupModelWithWorkflow(t, model, chainWorkflowJSON("s8-whole-wf",
		procSpec{"s8-proc-a", "SYNC", map[string]any{"calculationNodesTags": tagA}},
		procSpec{"s8-proc-b", "SYNC", map[string]any{"calculationNodesTags": tagB}}))

	items, _ := json.Marshal([]map[string]any{ // the locked row FIRST: v2 is written after the callout ended
		{"id": v1, "payload": victimUpdate, "transition": "approve"},
		{"id": v2, "payload": victimUpdate, "transition": "approve"},
	})

	unlock := holdRowLock(t, v1)
	defer unlock()

	answerNow := make(chan struct{})
	t.Cleanup(closeOnce(answerNow))
	inProgress := make(chan (<-chan callbackResult), 1)
	h.AttachCnode(t, cnodeSpec{name: "a", tags: []string{tagA},
		script: func(ctx context.Context, call receivedCallout, _ *reqCtx) cnodeReply {
			inProgress <- goJoined(h, http.MethodPut, "/api/entity/JSON", string(items), call.Pass())
			select {
			case <-answerNow:
				return answerOK() // answers before its own callback has finished
			case <-ctx.Done():
				return neverAnswer()
			}
		}})
	b := h.AttachCnode(t, cnodeSpec{name: "b", tags: []string{tagB}})

	done := make(chan createEntityResult, 1)
	go func() { done <- h.CreateEntityRaw(model, 1, workflowSampleModel) }()

	collRes := <-inProgress
	awaitBlockedStatement(t, "entities")
	close(answerNow)
	time.Sleep(700 * time.Millisecond)
	if got := b.Received(); len(got) != 0 {
		t.Fatalf("the engine carried on to processor B while A's collection is in progress: %v", got)
	}

	unlock()
	if res := awaitResult(t, "the collection in progress", collRes); res.StatusCode != http.StatusOK {
		t.Fatalf("the collection: %d %s; want 200 — what it wrote is in the transaction", res.StatusCode, res.Body)
	}
	if res := awaitCreate(t, done, 15*time.Second); res.status != http.StatusOK {
		t.Fatalf("create: %d %s; want 200", res.status, res.body)
	}
	for _, id := range []string{v1, v2} {
		if st, _ := h.GetEntityState(t, id); st != "APPROVED" {
			t.Errorf("victim %s state = %q; want APPROVED — the collection lands whole", id, st)
		}
	}
}

// TestCalloutFence_JoinedReadInProgress (F-replaced + F-blocked, a read): the
// first cnode has a joined read inside PostgreSQL when it is replaced — because
// its answer limit passed, or because it disconnected and abandoned the read
// mid-statement. Either way the statement is not interrupted, the owner waits
// for it, and the owner's operation succeeds.
func TestCalloutFence_JoinedReadInProgress(t *testing.T) {
	for _, disconnect := range []bool{false, true} {
		name := map[bool]string{false: "answer-limit-passes", true: "cnode-disconnects-mid-callback"}[disconnect]
		t.Run(name, func(t *testing.T) {
			h := newCalloutHarness(t, calloutTuning(3, 100*time.Millisecond))
			_ = h.token(t)
			model, tag := "s8-read-"+name, "s8-read-"+name
			h.SetupModelWithWorkflow(t, model, chainWorkflowJSON("s8-read-wf", procSpec{"s8-proc", "SYNC",
				map[string]any{"calculationNodesTags": tag, "responseTimeoutMs": 1000, "idempotent": true}}))

			resp := h.DoAuth(t, http.MethodPost, "/api/message/new/s8-read", `{"payload":{"x":1}}`, "")
			msgBody := h.readBody(t, resp)
			var created []map[string]any
			if err := json.Unmarshal([]byte(msgBody), &created); err != nil || resp.StatusCode != http.StatusOK || len(created) == 0 {
				t.Fatalf("seed message: %d %s", resp.StatusCode, msgBody)
			}
			ids, _ := created[0]["entityIds"].([]any)
			msgID, _ := ids[0].(string)

			unlock := holdMessagesTableLock(t)

			readCtx, abandonRead := context.WithCancel(context.Background())
			defer abandonRead()
			dropNow := make(chan struct{})
			t.Cleanup(closeOnce(dropNow))
			readRes := make(chan callbackResult, 1)
			h.AttachCnode(t, cnodeSpec{name: "first", tags: []string{tag},
				script: func(ctx context.Context, call receivedCallout, _ *reqCtx) cnodeReply {
					go func() {
						res, err := h.joinedRequest(readCtx, http.MethodGet, "/api/message/"+msgID, "", call.Pass())
						if err != nil {
							res = callbackResult{StatusCode: -1, Body: err.Error()}
						}
						readRes <- res
					}()
					if !disconnect {
						return neverAnswer()
					}
					select {
					case <-dropNow:
						return closeStream()
					case <-ctx.Done():
						return neverAnswer()
					}
				}})
			second := h.AttachCnode(t, cnodeSpec{name: "second", tags: []string{tag}})

			done := make(chan createEntityResult, 1)
			go func() { done <- h.CreateEntityRaw(model, 1, workflowSampleModel) }()

			awaitBlockedStatement(t, "messages")
			if disconnect {
				abandonRead()  // the cnode walks away from its callback mid-statement …
				close(dropNow) // … and drops its stream
				time.Sleep(700 * time.Millisecond)
			} else {
				time.Sleep(1500 * time.Millisecond) // the answer limit has passed
			}
			if got := second.Received(); len(got) != 0 {
				t.Fatalf("the second cnode was given the work while the first's read is in progress: %v", got)
			}

			unlock()
			res := awaitResult(t, "the read in progress", readRes)
			if !disconnect && res.StatusCode != http.StatusOK {
				t.Errorf("the read in progress: %d %s; want 200 — it completes", res.StatusCode, res.Body)
			}
			awaitCnodeReceived(t, second, 1, 15*time.Second)
			if cr := awaitCreate(t, done, 15*time.Second); cr.status != http.StatusOK {
				t.Fatalf("create: %d %s; want 200 — no statement of the operation's connection was interrupted", cr.status, cr.body)
			}
		})
	}
}

// TestCalloutFence_WaitDoesNotDeadlock (F-nested + F-blocked): out1's callback
// made a callout of its own, and that inner cnode's callback holds the
// transaction's lock inside PostgreSQL when out1 is replaced. Everything
// unwinds once the database lets the write through.
func TestCalloutFence_WaitDoesNotDeadlock(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(3, 100*time.Millisecond))
	_ = h.token(t)
	const outer, inner, tagOut, tagIn = "s8-dl-outer", "s8-dl-inner", "s8-dl-out", "s8-dl-in"
	victim := h.seedVictim(t, "s8-dl-victim")
	h.SetupModelWithWorkflow(t, inner, chainWorkflowJSON("s8-dl-inner-wf",
		procSpec{"s8-in", "SYNC", map[string]any{"calculationNodesTags": tagIn}}))
	h.SetupModelWithWorkflow(t, outer, chainWorkflowJSON("s8-dl-outer-wf",
		procSpec{"s8-out", "SYNC", map[string]any{"calculationNodesTags": tagOut, "responseTimeoutMs": 1500, "idempotent": true}}))

	unlock := holdRowLock(t, victim)
	defer unlock()

	holdIn := make(chan struct{})
	t.Cleanup(closeOnce(holdIn))
	out1Res := make(chan callbackResult, 1)
	inWrite := make(chan (<-chan callbackResult), 1)
	h.AttachCnode(t, cnodeSpec{name: "out1", tags: []string{tagOut},
		script: func(_ context.Context, _ receivedCallout, rc *reqCtx) cnodeReply {
			res, err := rc.CreateEntity(inner, 1, workflowSampleModel)
			if err != nil {
				res = callbackResult{StatusCode: -1, Body: err.Error()}
			}
			out1Res <- res
			return neverAnswer()
		}})
	h.AttachCnode(t, cnodeSpec{name: "in1", tags: []string{tagIn},
		script: func(ctx context.Context, call receivedCallout, _ *reqCtx) cnodeReply {
			inWrite <- goJoined(h, http.MethodPut, "/api/entity/JSON/"+victim, victimUpdate, call.Pass())
			select {
			case <-holdIn:
			case <-ctx.Done():
			}
			return neverAnswer()
		}})
	out2 := h.AttachCnode(t, cnodeSpec{name: "out2", tags: []string{tagOut}})

	done := make(chan createEntityResult, 1)
	go func() { done <- h.CreateEntityRaw(outer, 1, workflowSampleModel) }()

	writeRes := <-inWrite
	awaitBlockedStatement(t, "entities")
	time.Sleep(2 * time.Second) // OUT's 1.5s answer limit has passed
	if got := out2.Received(); len(got) != 0 {
		t.Fatalf("out2 was given the work while the inner cnode's write holds the transaction: %v", got)
	}

	unlock()
	if res := awaitResult(t, "the inner cnode's write", writeRes); res.StatusCode != http.StatusOK {
		t.Errorf("the inner cnode's write: %d %s; want 200 — it was in progress and lands", res.StatusCode, res.Body)
	}
	res := awaitResult(t, "out1's callback", out1Res)
	assertProblem(t, res.StatusCode, res.Body, http.StatusGone, "CALLOUT_SUPERSEDED", false)
	awaitCnodeReceived(t, out2, 1, 15*time.Second)
	if cr := awaitCreate(t, done, 15*time.Second); cr.status != http.StatusOK {
		t.Fatalf("outer create: %d %s; want 200", cr.status, cr.body)
	}
	if n := h.countEntities(t, inner); n != 0 {
		t.Errorf("%d inner entities committed; want 0", n)
	}
}
```

- [ ] **Step 2: Prove the scenarios have teeth**

Run: `go test ./internal/e2e/ -run 'TestCalloutFence_OwnerWaits|TestCalloutFence_AsyncNewTxFailedWrite|TestCalloutFence_CallbackPastItsLastCheck|TestCalloutFence_JoinedReadInProgress|TestCalloutFence_WaitDoesNotDeadlock'`.

Merge-base (+H, +C): `AsyncNewTxFailedWrite…` FAILS `the engine carried on to processor B while A's write is in progress` (or the create fails on a busy connection — either is the defect); `CallbackPastItsLastCheck…` FAILS the same way; `JoinedReadInProgress/cnode-disconnects…` FAILS `create: 5xx` (the cancelled statement closes the operation's connection, `pgconn.go:344`); the two that need a second try fail for want of one.

Temporary reverts: (1) in `Fence.Advance` and in `end`, remove the acquire-and-release of the transaction's lock → `OwnerWaits…`, `AsyncNewTx…`, `CallbackPast…`, `JoinedReadInProgress/answer-limit-passes` and `WaitDoesNotDeadlock` each FAIL their "was given the work while … in progress" assertion. (2) In the join helper, skip `fence.Check` after taking the lock → `OwnerWaits…` FAILS on the queued write (`status = 200; want 410`, and `1 secondary entities committed`). (3) In the join helper, run the handler under the request's own context instead of `context.WithoutCancel` → `cnode-disconnects-mid-callback` FAILS `create: 5xx`.

- [ ] **Step 4:** `go test ./internal/e2e/...`

- [ ] **Step 5: Commit**

```
git add internal/e2e/callout_fencing_wait_test.go
git commit -m "test(e2e): the owner waits for a joined request in progress; nothing is interrupted (#254)"
```

---

### Task S-9: e2e — one transaction, one user at a time: parallel joined requests, non-entity requests, a stalled body

**Spec:** §7 "One transaction, one user at a time" and its first bullet ("The lock is held only while the request is wholly in memory"), point 2 ("every non-entity handler behind the join middleware"); §13 rows 1039, 1040, 1042 — layer **E**.

**Files:**
- Create: `internal/e2e/callout_fencing_join_test.go`

**Interfaces:**
- Consumes: S-1, S-6 helpers; H `h.apiConn`, `h.grpcCtx`; `cyodapb.CloudEventsService_EntityManageCollection_FullMethodName` (`api/grpc/cyoda/*_grpc.pb.go:28`); `txctlSimpleCond` (`transaction_control_test.go:67`).

Row 1039's E cell is a consistency assertion (every request answered 200, the operation succeeds, the writes are all there) — not an interleaving. Its `-race` half on the memory backend is the unit layer's.

- [ ] **Step 1: Write the scenarios**

```go
package e2e_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	cyodapb "github.com/cyoda-platform/cyoda-go/api/grpc/cyoda"
)

// TestCalloutFence_ParallelJoinedRequests: one cnode fires reads and writes at
// its transaction in parallel. Each is answered 200, the operation succeeds
// and every write is in the result — on PostgreSQL, no "conn busy".
func TestCalloutFence_ParallelJoinedRequests(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(3, 100*time.Millisecond))
	_ = h.token(t)
	const model, secondary, tag = "s9-parallel", "s9-parallel-secondary", "s9-parallel"
	const reads, writes, rounds = 8, 4, 3
	h.SetupModelWithWorkflow(t, secondary, secondaryWorkflow)
	h.SetupModelWithWorkflow(t, model, chainWorkflowJSON("s9-parallel-wf",
		procSpec{"s9-proc", "SYNC", map[string]any{"calculationNodesTags": tag}}))

	var mu sync.Mutex
	var bad []string
	h.AttachCnode(t, cnodeSpec{name: "parallel", tags: []string{tag},
		script: func(_ context.Context, _ receivedCallout, rc *reqCtx) cnodeReply {
			var wg sync.WaitGroup
			note := func(what string, res callbackResult, err error) {
				if err == nil && res.StatusCode == http.StatusOK {
					return
				}
				mu.Lock()
				defer mu.Unlock()
				bad = append(bad, fmt.Sprintf("%s: status=%d err=%v body=%s", what, res.StatusCode, err, res.Body))
			}
			for i := 0; i < reads; i++ {
				wg.Add(1)
				go func() { defer wg.Done(); res, err := rc.GetEntity(rc.entityID); note("read", res, err) }()
			}
			for i := 0; i < writes; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					res, err := rc.CreateEntity(secondary, 1, `{"name":"parallel-child","amount":1,"status":"new"}`)
					note("write", res, err)
				}()
			}
			wg.Wait()
			return answerOK()
		}})

	for round := 0; round < rounds; round++ {
		if _, status, body := h.CreateEntity(t, model, 1, workflowSampleModel); status != http.StatusOK {
			t.Fatalf("round %d create: %d %s", round, status, body)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bad) != 0 {
		t.Fatalf("%d joined requests were not answered 200: %v", len(bad), bad)
	}
	if n := h.countEntities(t, secondary); n != writes*rounds {
		t.Errorf("%d secondary entities committed; want %d", n, writes*rounds)
	}
}

// TestCalloutFence_NonEntityJoinedRequests (shape F-ended): model, message,
// search, statistics and audit requests pass the same join layer as entity
// requests — admitted with the pass of the callout in progress, refused 410
// with the pass of one that has ended.
func TestCalloutFence_NonEntityJoinedRequests(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(3, 100*time.Millisecond))
	_ = h.token(t)
	const model, tagA, tagB = "s9-nonentity", "s9-nonentity-a", "s9-nonentity-b"
	h.SetupModelWithWorkflow(t, model, chainWorkflowJSON("s9-nonentity-wf",
		procSpec{"s9-proc-a", "SYNC", map[string]any{"calculationNodesTags": tagA}},
		procSpec{"s9-proc-b", "SYNC", map[string]any{"calculationNodesTags": tagB}}))
	committed := h.seedVictim(t, "s9-nonentity-committed")

	resp := h.DoAuth(t, http.MethodPost, "/api/message/new/s9-seed", `{"payload":{"x":1}}`, "")
	if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("seed message: %d %s", resp.StatusCode, body)
	}

	release := make(chan struct{})
	releaseNow := closeOnce(release)
	t.Cleanup(releaseNow)
	a := h.AttachCnode(t, cnodeSpec{name: "a", tags: []string{tagA}})
	b := h.AttachCnode(t, cnodeSpec{name: "b", tags: []string{tagB}, script: scriptHold(release, answerOK())})

	done := make(chan createEntityResult, 1)
	go func() { done <- h.CreateEntityRaw(model, 1, workflowSampleModel) }()
	current := awaitCnodeReceived(t, b, 1, 15*time.Second)[0].Pass()
	ended := a.Received()[0].Pass()

	requests := []struct{ name, method, path, body string }{
		{"model list", http.MethodGet, "/api/model/", ""},
		{"message create", http.MethodPost, "/api/message/new/s9-joined", `{"payload":{"x":2}}`},
		{"search", http.MethodPost, "/api/search/direct/s9-nonentity-committed/1", txctlSimpleCond},
		{"statistics", http.MethodGet, "/api/entity/stats", ""},
		{"audit", http.MethodGet, "/api/audit/entity/" + committed, ""},
	}
	for _, rq := range requests {
		res, err := h.callback(rq.method, rq.path, rq.body, ended)
		if err != nil {
			t.Fatalf("%s with the ended pass: %v", rq.name, err)
		}
		pd := assertProblem(t, res.StatusCode, res.Body, http.StatusGone, "CALLOUT_SUPERSEDED", false)
		if pd.Detail == "" {
			t.Errorf("%s: empty detail", rq.name)
		}
		res, err = h.callback(rq.method, rq.path, rq.body, current)
		if err != nil || res.StatusCode != http.StatusOK {
			t.Errorf("%s with the current pass: status=%d err=%v body=%s; want 200", rq.name, res.StatusCode, err, res.Body)
		}
	}

	releaseNow()
	if res := awaitCreate(t, done, 15*time.Second); res.status != http.StatusOK {
		t.Fatalf("create: %d %s; want 200", res.status, res.body)
	}
}

// TestCalloutFence_StalledBodyHoldsNothing: a cnode opens a joined HTTP request
// and sends only part of its body, and opens a joined gRPC server-streaming
// call and never sends its message. Neither holds the transaction: the cnode's
// next joined request is served, and the operation completes.
func TestCalloutFence_StalledBodyHoldsNothing(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(3, 100*time.Millisecond))
	_ = h.token(t)
	const model, secondary, tag = "s9-stall", "s9-stall-secondary", "s9-stall"
	h.SetupModelWithWorkflow(t, secondary, secondaryWorkflow)
	h.SetupModelWithWorkflow(t, model, chainWorkflowJSON("s9-stall-wf",
		procSpec{"s9-proc", "SYNC", map[string]any{"calculationNodesTags": tag}}))

	u, err := url.Parse(h.baseURL)
	if err != nil {
		t.Fatalf("parse base url: %v", err)
	}
	bearer, _ := h.bearerVal.Load().(string)
	stallCtx, stopStalling := context.WithCancel(context.Background())
	t.Cleanup(stopStalling)

	problems := make(chan string, 4)
	h.AttachCnode(t, cnodeSpec{name: "staller", tags: []string{tag},
		script: func(_ context.Context, call receivedCallout, rc *reqCtx) cnodeReply {
			// 1. HTTP: headers and 10 of 64 promised body bytes, then silence.
			conn, err := net.Dial("tcp", u.Host)
			if err != nil {
				problems <- "dial: " + err.Error()
				return answerFail("harness")
			}
			go func() { <-stallCtx.Done(); _ = conn.Close() }()
			fmt.Fprintf(conn, "POST /api/entity/JSON/%s/1 HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\n"+
				"X-Tx-Token: %s\r\nContent-Type: application/json\r\nContent-Length: 64\r\n\r\n{\"name\":\"", secondary, u.Host, bearer, call.Pass())

			// 2. gRPC server-streaming: the call is opened, its one message never sent.
			md := h.grpcCtx(call.Pass())
			streamCtx, cancel := context.WithCancel(md)
			go func() { <-stallCtx.Done(); cancel() }()
			if _, err := h.apiConn.NewStream(streamCtx, &grpc.StreamDesc{ServerStreams: true},
				cyodapb.CloudEventsService_EntityManageCollection_FullMethodName); err != nil {
				problems <- "open stream: " + err.Error()
			}

			// 3. An ordinary joined read and write must not wait for either.
			if res, err := rc.GetEntity(rc.entityID); err != nil || res.StatusCode != http.StatusOK {
				problems <- fmt.Sprintf("joined read behind the stalled requests: status=%d err=%v", res.StatusCode, err)
			}
			if res, err := rc.CreateEntity(secondary, 1, `{"name":"after-stall","amount":1,"status":"new"}`); err != nil || res.StatusCode != http.StatusOK {
				problems <- fmt.Sprintf("joined write behind the stalled requests: status=%d err=%v", res.StatusCode, err)
			}
			return answerOK()
		}})

	done := make(chan createEntityResult, 1)
	go func() { done <- h.CreateEntityRaw(model, 1, workflowSampleModel) }()
	if res := awaitCreate(t, done, 10*time.Second); res.status != http.StatusOK {
		t.Fatalf("create: %d %s; want 200 while both stalled requests are still open", res.status, res.body)
	}
	close(problems)
	for p := range problems {
		t.Error(p)
	}
	if n := h.countEntities(t, secondary); n != 1 {
		t.Errorf("%d secondary entities committed; want 1 (the stalled request wrote nothing)", n)
	}
}
```

`close(problems)` is safe: the script has returned (its reply is what let the create finish).

- [ ] **Step 2: Prove the scenarios have teeth**

Run: `go test ./internal/e2e/ -run 'TestCalloutFence_ParallelJoinedRequests|TestCalloutFence_NonEntityJoinedRequests|TestCalloutFence_StalledBodyHoldsNothing'`.

Merge-base (+H): `ParallelJoinedRequests` is expected to FAIL with 5xx answers (`conn busy`) on most runs — joined reads take no lock today; it is a consistency check, so record the failure text when seen and rely on the revert for certainty. `NonEntityJoinedRequests` FAILS `status = 200; want 410`. `StalledBodyHoldsNothing` is green on the merge-base: today no lock is held while a body is read — the hazard arrives with this feature, so this scenario guards the new code only.
Temporary reverts: (1) in the join helper, take the lock only when the route is an entity write (today's scope) → `ParallelJoinedRequests` fails as on the merge-base. (2) Remove `Admit` from the HTTP join middleware only → `NonEntity…` FAILS. (3) In the join helper take the transaction's lock **before** reading the body (HTTP) / receiving the message (gRPC) → `StalledBody…` FAILS `the create did not complete within 10s`; run it once per door by commenting out the other stall.

- [ ] **Step 4:** `go test ./internal/e2e/...`

- [ ] **Step 5: Commit**

```
git add internal/e2e/callout_fencing_join_test.go
git commit -m "test(e2e): joined requests are serialised, non-entity ones fenced, a stalled body holds nothing (#254)"
```

---

### Task S-10: e2e — a pass that is malformed, expired, or presented by another tenant

**Spec:** §7 "Claims" and point 1 ("The tenant check comes first so that a stolen pass tells another tenant nothing about which callouts exist"); §8.2 rows 10, 13; §13 rows 1053, 1054 — layer **E**.

**Files:**
- Create: `internal/e2e/callout_pass_test.go`

**Interfaces:**
- Consumes: S-1, S-6 helpers; F `token.Claims{NodeID, TxRef, ExpiresAt, Callout, Major, Minor, Outer}` and `(*token.Signer).Issue(token.Claims) (string, error)`; `h.app.TokenSigner()` (`app/app.go:935`); `h.provisionTenant`, `h.fetchTokenFor`, `h.doAuthBearer` (`callback_txjoin_errors_test.go:62, 83, 113`); `internalgrpcTxTokenKey` (`callback_txjoin_grpc_search_test.go:66`).

Existing tests: `TestCallbackErr_LoudFailCodes` (`callback_txjoin_errors_test.go:138`) stays; F-4 rewrites its three `Issue(...)` calls and F-6 adds `NoCalloutAndNumber_401` beside `Forged_401`. Those subtests present, on the HTTP door only, passes with no live callout behind them. The scenario below does not repeat them: it presents each pass against a **live** callout on an open transaction, on both doors, write and read, and adds the case only fencing creates — another tenant presenting a pass that is already *superseded* must still be told 403, never 410. `lateChild` is S-8's constant. F's Open point 7 confirms `Issue` validates nothing, which is what lets the 401 pass be minted from the stack's own signer.

- [ ] **Step 1: Write the scenario**

```go
package e2e_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	cyodapb "github.com/cyoda-platform/cyoda-go/api/grpc/cyoda"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

// passClaims decodes the claims of a recorded pass. The claims are not secret;
// the signature is what protects them.
func passClaims(t *testing.T, pass string) token.Claims {
	t.Helper()
	head, _, ok := strings.Cut(pass, ".")
	if !ok {
		t.Fatal("recorded pass has no signature part")
	}
	raw, err := base64.RawURLEncoding.DecodeString(head)
	if err != nil {
		t.Fatalf("decode pass claims: %v", err)
	}
	var c token.Claims
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("unmarshal pass claims: %v", err)
	}
	return c
}

// TestCalloutPass (shape F-ended): with processor B's callout in progress and
// processor A's ended, on one open transaction.
func TestCalloutPass(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(3, 100*time.Millisecond))
	_ = h.token(t)
	const model, secondary, tagA, tagB = "s10-pass", "s10-pass-secondary", "s10-pass-a", "s10-pass-b"
	h.SetupModelWithWorkflow(t, secondary, secondaryWorkflow)
	h.SetupModelWithWorkflow(t, model, chainWorkflowJSON("s10-pass-wf",
		procSpec{"s10-proc-a", "SYNC", map[string]any{"calculationNodesTags": tagA}},
		procSpec{"s10-proc-b", "SYNC", map[string]any{"calculationNodesTags": tagB}}))

	release := make(chan struct{})
	releaseNow := closeOnce(release)
	t.Cleanup(releaseNow)
	a := h.AttachCnode(t, cnodeSpec{name: "a", tags: []string{tagA}})
	b := h.AttachCnode(t, cnodeSpec{name: "b", tags: []string{tagB}, script: scriptHold(release, answerOK())})

	done := make(chan createEntityResult, 1)
	go func() { done <- h.CreateEntityRaw(model, 1, workflowSampleModel) }()
	live := awaitCnodeReceived(t, b, 1, 15*time.Second)[0]
	ended := a.Received()[0]
	claims := passClaims(t, live.Pass())
	if claims.Callout == "" || claims.Major == 0 {
		t.Fatalf("a minted pass carries callout=%q major=%d; want the callout's request id and a number", claims.Callout, claims.Major)
	}
	if claims.Callout != live.RequestID {
		t.Errorf("pass callout = %q; want the callout's request id %q", claims.Callout, live.RequestID)
	}

	t.Run("no-callout-and-number-401", func(t *testing.T) {
		bare := token.Claims{NodeID: claims.NodeID, TxRef: claims.TxRef, ExpiresAt: claims.ExpiresAt}
		pass, err := h.app.TokenSigner().Issue(bare)
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}
		assertRefusedOnAllDoors(t, h, pass, secondary, live.EntityID, http.StatusUnauthorized, "UNAUTHORIZED", "invalid transaction token")
	})

	t.Run("expired-410", func(t *testing.T) {
		old := claims
		old.ExpiresAt = time.Now().Add(-time.Minute).Unix()
		pass, err := h.app.TokenSigner().Issue(old)
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}
		assertRefusedOnAllDoors(t, h, pass, secondary, live.EntityID, http.StatusGone, "TRANSACTION_EXPIRED", "")
	})

	t.Run("another-tenant-403", func(t *testing.T) {
		clientB, secretB := h.provisionTenant(t, "s10-tenant-b", "s10-user-b")
		bearerB := h.fetchTokenFor(t, clientB, secretB)
		// A current pass and a superseded one: another tenant learns the same
		// nothing from both — never CALLOUT_SUPERSEDED.
		for name, pass := range map[string]string{"current pass": live.Pass(), "ended pass": ended.Pass()} {
			resp := h.doAuthBearer(t, bearerB, http.MethodGet, "/api/entity/"+live.EntityID, "", pass)
			assertProblem(t, resp.StatusCode, h.readBody(t, resp), http.StatusForbidden, "FORBIDDEN", false)
			resp = h.doAuthBearer(t, bearerB, http.MethodPost, "/api/entity/JSON/"+secondary+"/1", lateChild, pass)
			assertProblem(t, resp.StatusCode, h.readBody(t, resp), http.StatusForbidden, "FORBIDDEN", false)

			ctx := metadata.AppendToOutgoingContext(context.Background(),
				"authorization", "Bearer "+bearerB, internalgrpcTxTokenKey, pass)
			reqCE, err := internalgrpc.NewCloudEvent(internalgrpc.EntityGetRequest, map[string]any{"id": "s10-stolen", "entityId": live.EntityID})
			if err != nil {
				t.Fatalf("build get request: %v", err)
			}
			respCE, err := cyodapb.NewCloudEventsServiceClient(h.apiConn).EntitySearch(ctx, reqCE)
			if err != nil {
				t.Fatalf("%s, gRPC: %v", name, err)
			}
			env, err := parseTxEnvelope(respCE)
			assertEnvelope(t, name+", gRPC read", env, err, "FORBIDDEN", false)
		}
	})

	releaseNow()
	if res := awaitCreate(t, done, 15*time.Second); res.status != http.StatusOK {
		t.Fatalf("create: %d %s; want 200 — none of the refused passes touched the operation", res.status, res.body)
	}
	if n := h.countEntities(t, secondary); n != 0 {
		t.Errorf("%d secondary entities committed; want 0", n)
	}
}
```

- [ ] **Step 2: Prove the scenario has teeth**

Run: `go test ./internal/e2e/ -run 'TestCalloutPass'`.
Merge-base: does not compile (`token.Claims` has no `Callout`; `Issue` takes three arguments) — with F's token task alone: `no-callout-and-number-401` FAILS `status = 200; want 401`.
Temporary reverts: (1) in the pass verification, accept an empty `Callout` → the 401 subtest FAILS with 200 (or 410). (2) In `txjoin.JoinFromToken`, call `Admit` before `txMgr.Join` → `another-tenant-403` FAILS on the ended pass: `status = 410; want 403`.

- [ ] **Step 4:** `go test ./internal/e2e/...`

- [ ] **Step 5: Commit**

```
git add internal/e2e/callout_pass_test.go
git commit -m "test(e2e): a pass without a callout is 401, an expired one 410, another tenant's 403 before any fencing answer (#254)"
```

---

### Task S-11: e2e — round robin, and two tenants on one tag

**Spec:** §4 "Selection" (`RoundRobinSelector`: "a new one has never been picked and goes first"); §8.2 "Member ids … identify the tenant's own connections"; §13 rows 1056, 1057 — layer **E**.

**Files:**
- Create: `internal/e2e/callout_selection_test.go`

**Interfaces:**
- Consumes: S-1 helpers; S-3 `parseCalloutFailed`; H `cnodeSpec.bearer`, `h.provisionTenant`, `h.fetchTokenFor`, `h.doAuthBearer`.

- [ ] **Step 1: Write the scenarios**

```go
package e2e_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// TestCalloutSelection_RoundRobin: two cnodes of one tag take turns, and a
// cnode that attaches later goes first.
func TestCalloutSelection_RoundRobin(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(3, 0))
	const model, tag = "s11-rr", "s11-rr"
	h.SetupModelWithWorkflow(t, model, chainWorkflowJSON("s11-rr-wf",
		procSpec{"s11-proc", "SYNC", map[string]any{"calculationNodesTags": tag}}))
	h.AttachCnode(t, cnodeSpec{name: "x", tags: []string{tag}})
	h.AttachCnode(t, cnodeSpec{name: "y", tags: []string{tag}})

	create := func() {
		t.Helper()
		if _, status, body := h.CreateEntity(t, model, 1, workflowSampleModel); status != http.StatusOK {
			t.Fatalf("create: %d %s", status, body)
		}
	}
	for i := 0; i < 4; i++ {
		create()
	}
	h.AttachCnode(t, cnodeSpec{name: "z", tags: []string{tag}})
	create()

	var order []string
	for _, r := range h.ReceivedCallouts() {
		order = append(order, r.Cnode)
	}
	if got, want := strings.Join(order, ","), "x,y,x,y,z"; got != want {
		t.Errorf("callouts went to %s; want %s", got, want)
	}
}

// TestCalloutSelection_TwoTenantsOneTag: cnodes of two tenants join under the
// same tag. Each tenant's callouts go only to its own cnodes, and a failure
// names only its own.
func TestCalloutSelection_TwoTenantsOneTag(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(1, 0))
	_ = h.token(t)
	clientB, secretB := h.provisionTenant(t, "s11-tenant-b", "s11-user-b")
	bearerB := h.fetchTokenFor(t, clientB, secretB)

	asB := func(method, path, body string) (int, string) {
		resp := h.doAuthBearer(t, bearerB, method, path, body, "")
		return resp.StatusCode, h.readBody(t, resp)
	}
	setupAsB := func(model, wf string) {
		t.Helper()
		for _, step := range []struct{ method, path, body string }{
			{http.MethodPost, fmt.Sprintf("/api/model/import/JSON/SAMPLE_DATA/%s/1", model), workflowSampleModel},
			{http.MethodPut, fmt.Sprintf("/api/model/%s/1/lock", model), ""},
			{http.MethodPost, fmt.Sprintf("/api/model/%s/1/workflow/import", model), wf},
		} {
			if status, body := asB(step.method, step.path, step.body); status != http.StatusOK {
				t.Fatalf("tenant B %s %s: %d %s", step.method, step.path, status, body)
			}
		}
	}

	t.Run("routing", func(t *testing.T) {
		const tag = "s11-shared"
		wf := chainWorkflowJSON("s11-shared-wf", procSpec{"s11-proc", "SYNC", map[string]any{"calculationNodesTags": tag}})
		h.SetupModelWithWorkflow(t, "s11-shared-a", wf)
		setupAsB("s11-shared-b", wf)
		a := h.AttachCnode(t, cnodeSpec{name: "tenant-a", tags: []string{tag}})
		b := h.AttachCnode(t, cnodeSpec{name: "tenant-b", tags: []string{tag}, bearer: bearerB})

		for i := 0; i < 2; i++ { // twice: round robin would alternate if the tenants shared a pool
			if _, status, body := h.CreateEntity(t, "s11-shared-a", 1, workflowSampleModel); status != http.StatusOK {
				t.Fatalf("tenant A create: %d %s", status, body)
			}
		}
		if na, nb := len(a.Received()), len(b.Received()); na != 2 || nb != 0 {
			t.Fatalf("after tenant A's two creates: A's cnode received %d, B's %d; want 2 and 0", na, nb)
		}
		for i := 0; i < 2; i++ {
			if status, body := asB(http.MethodPost, "/api/entity/JSON/s11-shared-b/1", workflowSampleModel); status != http.StatusOK {
				t.Fatalf("tenant B create: %d %s", status, body)
			}
		}
		if na, nb := len(a.Received()), len(b.Received()); na != 2 || nb != 2 {
			t.Errorf("after tenant B's two creates: A's cnode received %d, B's %d; want 2 and 2", na, nb)
		}
	})

	t.Run("a-failure-names-only-the-tenants-own-cnodes", func(t *testing.T) {
		const tag = "s11-shared-fail"
		h.SetupModelWithWorkflow(t, "s11-shared-fail-a", chainWorkflowJSON("s11-shared-fail-wf", procSpec{"s11-proc", "SYNC",
			map[string]any{"calculationNodesTags": tag, "responseTimeoutMs": 300, "idempotent": true}}))
		a1 := h.AttachCnode(t, cnodeSpec{name: "a1", tags: []string{tag}, script: scriptAlways(neverAnswer())})
		a2 := h.AttachCnode(t, cnodeSpec{name: "a2", tags: []string{tag}, script: scriptAlways(neverAnswer())})
		b := h.AttachCnode(t, cnodeSpec{name: "b-healthy", tags: []string{tag}, bearer: bearerB})

		_, status, body := h.CreateEntity(t, "s11-shared-fail-a", 1, workflowSampleModel)
		pd := assertProblem(t, status, body, http.StatusServiceUnavailable, "CALLOUT_FAILED", true)
		msg := parseCalloutFailed(t, pd.Detail)
		if msg.n != 2 || msg.perMember[a1.MemberID()] != 1 || msg.perMember[a2.MemberID()] != 1 {
			t.Errorf("detail = %q; want one failure for each of tenant A's two cnodes", pd.Detail)
		}
		if strings.Contains(pd.Detail, b.MemberID()) {
			t.Errorf("detail = %q; it names tenant B's cnode %s", pd.Detail, b.MemberID())
		}
		if got := b.Received(); len(got) != 0 {
			t.Errorf("tenant B's healthy cnode received %d of tenant A's callouts", len(got))
		}
	})
}
```

- [ ] **Step 2: Prove the scenarios have teeth**

Run: `go test ./internal/e2e/ -run 'TestCalloutSelection_'`.
Merge-base (+H): `RoundRobin` FAILS on the order (today's choice is map-iteration order, `members.go:470-481`) on most runs; `a-failure-names…` FAILS on the code. `routing` is green — tenant scoping of `FindByTags` exists today; it pins that `Candidates` keeps it.
Temporary reverts: (1) make `RoundRobinSelector.Select` return `candidates[0]` → `RoundRobin` FAILS `x,x,x,x,x`. (2) In `MemberRegistry.Candidates`, drop the tenant comparison → `routing` FAILS `A's cnode received 1, B's 1` and the second subtest FAILS on `b.Received()`.

- [ ] **Step 4:** `go test ./internal/e2e/...`

- [ ] **Step 5: Commit**

```
git add internal/e2e/callout_selection_test.go
git commit -m "test(e2e): round robin with a newcomer first; two tenants on one tag stay apart (#254)"
```

---

### Task S-12: parity — the three externalization scenarios that waited for a second compute client (09/09, 09/10, 09/11)

**Spec:** §13 rows 1008, 1010 — layer **P**; §13 "What each harness can do" (second bullet).

**Files:**
- Modify: `e2e/parity/externalapi/workflow_externalization.go` (`RunExternalAPI_09_09_…`, `_09_10_…`, `_09_11_…` at :298-314 — the three `t.Skip` bodies are replaced; the file's header comment loses nothing)

**Interfaces:**
- Consumes (H): `parity.ComputeClientSpec`, `parity.ComputeBehaviourDrop`, `parity.ComputeBehaviourStall`, `parity.StartComputeClientOrSkip`, `parity.AwaitReceived`, `parity.ComputeClientWorkflow`, `parity.ReceivedCallout`; existing `setupExternalModel` (:93), `errorcontract.Match`.
- Produces (file-local): `failoverPair`, `assertRetryableProblem`.

No registry or count change: the three names are registered already (`workflow_externalization.go:57-59`) and scenarios registered from `externalapi` are outside `wantParityScenarioCount`.

- [ ] **Step 1: Replace the three skipped bodies** (add imports `net/http`, `time`)

```go
// failoverPair starts, under a FRESH tenant, a compute client with the given
// behaviour and then a healthy one, both on tag — so the misbehaving one is
// tried first — and imports a model whose one SYNC processor is routed to tag.
// A fixture that cannot start compute clients skips here, before the server is
// touched.
func failoverPair(t *testing.T, fixture parity.BackendFixture, name, behaviour string, extraConfig map[string]any) (c *parityclient.Client, model string, bad, good parity.ComputeClient) {
	t.Helper()
	tenant := fixture.NewTenant(t)
	tag := "ext-" + name
	bad = parity.StartComputeClientOrSkip(t, fixture, parity.ComputeClientSpec{TenantID: tenant.ID, Tags: []string{tag}, Behaviour: behaviour})
	good = parity.StartComputeClientOrSkip(t, fixture, parity.ComputeClientSpec{TenantID: tenant.ID, Tags: []string{tag}})
	c = parityclient.NewClient(fixture.BaseURL(), tenant.Token)
	model = "ext-" + name
	setupExternalModel(t, c, model, 1, `{"k":1}`, parity.ComputeClientWorkflow("ext-"+name+"-wf", "noop", tag, "", extraConfig))
	return c, model, bad, good
}

// assertRetryableProblem matches status and errorCode and requires
// properties.retryable=true.
func assertRetryableProblem(t *testing.T, status int, body []byte, wantStatus int, wantCode string) {
	t.Helper()
	errorcontract.Match(t, status, body, errorcontract.ExpectedError{HTTPStatus: wantStatus, ErrorCode: wantCode})
	var problem struct {
		Properties struct {
			Retryable bool `json:"retryable"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(body, &problem); err != nil {
		t.Fatalf("failed to parse response body as JSON: %v\nbody: %s", err, body)
	}
	if !problem.Properties.Retryable {
		t.Errorf("expected properties.retryable=true (body: %s)", body)
	}
}

// assertTriedThenServed: bad received the work first, good served the same
// request afterwards.
func assertTriedThenServed(t *testing.T, bad, good parity.ComputeClient) {
	t.Helper()
	b := parity.AwaitReceived(t, bad, 1, 5*time.Second)
	g := parity.AwaitReceived(t, good, 1, 5*time.Second)
	if len(b) != 1 || len(g) != 1 {
		t.Fatalf("first client received %d requests, second %d; want 1 and 1", len(b), len(g))
	}
	if b[0].RequestID == "" || b[0].RequestID != g[0].RequestID {
		t.Errorf("request ids differ across tries: %q then %q", b[0].RequestID, g[0].RequestID)
	}
}

// RunExternalAPI_09_09_ExternalDisconnectSucceedsOnRetry — dictionary 09/09.
// The first compute node drops its connection on receiving the work; the
// processor is declared idempotent, so the second is asked and the entity is
// created.
func RunExternalAPI_09_09_ExternalDisconnectSucceedsOnRetry(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	c, model, bad, good := failoverPair(t, fixture, "0909", parity.ComputeBehaviourDrop, map[string]any{"idempotent": true})
	id, err := c.CreateEntity(t, model, 1, `{"k":1}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	if got, err := c.GetEntity(t, id); err != nil || got.Meta.State != "ACTIVE" {
		t.Errorf("state = %q err=%v; want ACTIVE", got.Meta.State, err)
	}
	assertTriedThenServed(t, bad, good)
}

// RunExternalAPI_09_10_ExternalTimeoutFailover — dictionary 09/10.
// The first compute node never answers; after the processor's own
// responseTimeoutMs the second is asked.
func RunExternalAPI_09_10_ExternalTimeoutFailover(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	c, model, bad, good := failoverPair(t, fixture, "0910", parity.ComputeBehaviourStall,
		map[string]any{"idempotent": true, "responseTimeoutMs": 300})
	if _, err := c.CreateEntity(t, model, 1, `{"k":1}`); err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	assertTriedThenServed(t, bad, good)
}

// RunExternalAPI_09_11_ProcessingNodeDisconnectsMidRequest — dictionary 09/11.
// A compute node disconnects with the request in its hands while another
// remains. What happens next is the workflow author's declaration: without
// `idempotent` the operation fails with the try's own code and the other node
// is never asked; with it, the other node serves the request.
func RunExternalAPI_09_11_ProcessingNodeDisconnectsMidRequest(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	t.Run("not-idempotent", func(t *testing.T) {
		c, model, bad, good := failoverPair(t, fixture, "0911n", parity.ComputeBehaviourDrop, nil)
		status, body, err := c.CreateEntityRaw(t, model, 1, `{"k":1}`)
		if err != nil {
			t.Fatalf("CreateEntityRaw: %v", err)
		}
		assertRetryableProblem(t, status, body, http.StatusServiceUnavailable, "COMPUTE_MEMBER_DISCONNECTED")
		parity.AwaitReceived(t, bad, 1, 5*time.Second)
		if got := good.Received(t); len(got) != 0 {
			t.Errorf("the remaining compute node received %d requests; a processor not declared idempotent is not repeated", len(got))
		}
		if list, err := c.ListEntitiesByModel(t, model, 1); err != nil || len(list) != 0 {
			t.Errorf("entities after the failed create: %d err=%v; want 0", len(list), err)
		}
	})
	t.Run("idempotent", func(t *testing.T) {
		c, model, bad, good := failoverPair(t, fixture, "0911i", parity.ComputeBehaviourDrop, map[string]any{"idempotent": true})
		if _, err := c.CreateEntity(t, model, 1, `{"k":1}`); err != nil {
			t.Fatalf("CreateEntity: %v", err)
		}
		assertTriedThenServed(t, bad, good)
	})
}
```

- [ ] **Step 2: Prove the scenarios have teeth**

Run: `go test -count=1 ./e2e/parity/memory/ -run 'TestParity/ExternalAPI_09_(09|10|11)'`, then `sqlite`, `postgres`. Expected on the branch head: PASS, **not** SKIP — check `… -json | grep -c '"Action":"skip"'` → `0`.
Merge-base (+H, +C): 09_09, 09_10 and 09_11/idempotent FAIL `CreateEntity: … 503`. 09_11/not-idempotent is green (single-shot is today's behaviour) — a pin.
Temporary revert: S-1's revert #1 (`MayTryAnother` false for `NoAnswer`) → the three FAIL; its opposite (true for a non-repeat-safe `NoAnswer`) → 09_11/not-idempotent FAILS `the remaining compute node received 1 requests`.

- [ ] **Step 4:** `go test ./e2e/parity/...` (unit level), then the three backend runs above.

- [ ] **Step 5: Commit**

```
git add e2e/parity/externalapi/workflow_externalization.go
git commit -m "test(parity): un-skip externalization 09/09, 09/10, 09/11 on a second compute client (#254)"
```

---

### Task S-13: parity — what stops a callout, and what the client is told, on every backend

**Spec:** §13 rows 1007, 1009, 1011, 1012, 1014, 1015 — layer **P**. (Row 1024's P cell is the existing `ExternalAPI_09_08_NoExternalRegisteredFails`, which stays as it is and asserts 503 `NO_COMPUTE_MEMBER_FOR_TAG`, `retryable`; H-11's 200 ms patience keeps it fast.)

**Files:**
- Create: `e2e/parity/callout_failover.go`
- Create: `e2e/parity/callout_failover_test.go`
- Modify: `e2e/parity/registry.go` (six entries; header count), `e2e/parity/registry_count_test.go` (`wantParityScenarioCount` **+6** — 280 → 286 once H's two and C's three are in; read the constant, do not assume it)

**Interfaces:**
- Consumes (H): as S-12, plus `ComputeBehaviourFail`, `ComputeBehaviourFailRetryable`, `requireComputeClients`, `stubNoComputeClients` (`compute_client_test.go`), catalog name `inject-error-not-retryable` (H-5); existing `cbSetupModel`, `cbProc`, `containsErrorCode`; catalog `always-true`, `sched-fn-resolve`.
- Every scenario: a **fresh tenant** per case and a tag of its own; clients started one after the other; no goroutines, no interleaving.

The default tries on the parity fixtures are 1 + 3 and the patience 200 ms (H-11). With two silent clients the owner's loop makes two tries, waits out the patience, and — depending on how the owner's-loop stream reads "start a new pass" once the patience is spent — may visit both again. `CalloutEveryTryUsed` therefore asserts what §8.2 fixes whichever way that goes: the code, `retryable`, the sentence, a stated count that equals the sum of its entries and lies in 2…4, and entries for exactly the tenant's two members. See Open point 3.

- [ ] **Step 1: Write the failing unit test** — `e2e/parity/callout_failover_test.go`

```go
package parity

import "testing"

var calloutScenarios = map[string]func(*testing.T, BackendFixture){
	"CalloutNoAnswerNotIdempotentStops": RunCalloutNoAnswerNotIdempotentStops,
	"CalloutCriterionFailsOver":         RunCalloutCriterionFailsOver,
	"CalloutFunctionFailsOver":          RunCalloutFunctionFailsOver,
	"CalloutMemberFailedStops":          RunCalloutMemberFailedStops,
	"CalloutRetryPolicyNoneOneTry":      RunCalloutRetryPolicyNoneOneTry,
	"CalloutEveryTryUsed":               RunCalloutEveryTryUsed,
}

// TestCalloutScenariosSkipWithoutCapability: on a backend whose fixture cannot
// start compute clients these scenarios skip, before they touch the server.
func TestCalloutScenariosSkipWithoutCapability(t *testing.T) {
	for name, fn := range calloutScenarios {
		skipped := t.Run(name, func(t *testing.T) {
			fn(t, stubNoComputeClients{})
			t.Fatalf("scenario %s did not skip on a fixture without the capability", name)
		})
		if !skipped {
			t.Errorf("scenario %s must t.Skip when the fixture cannot start compute clients", name)
		}
	}
}

func TestCalloutScenariosRegistered(t *testing.T) {
	registered := map[string]bool{}
	for _, nt := range allTests {
		registered[nt.Name] = true
	}
	for name := range calloutScenarios {
		if !registered[name] {
			t.Errorf("scenario %s is not in the registry", name)
		}
	}
}
```

(`t.Run` returns false for a subtest that called `t.Fatalf`, true for one that skipped — the idiom H-9 uses.)

- [ ] **Step 2: Run to verify RED**

Run: `go test ./e2e/parity/ -run 'TestCalloutScenarios|TestParityScenarioCount'`
Expected: FAIL (build) `undefined: RunCalloutNoAnswerNotIdempotentStops`; with the scenarios written but unregistered, `TestCalloutScenariosRegistered` FAILS; registered but the constant not bumped, `TestParityScenarioCount` FAILS `parity scenario count = 286, want 280`.

- [ ] **Step 3: Write the scenarios and register them** — `e2e/parity/callout_failover.go`

```go
package parity

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// callout_failover.go — what ends a callout and what the client is told, on
// every backend. Each case runs under a fresh tenant with a tag of its own and
// starts its compute clients one after the other, so "the client started first
// is tried first" holds and nothing carries over between scenarios. Anything
// that depends on timing or order between requests lives in internal/e2e.

const (
	calloutSample      = `{"name":"Test","amount":10,"status":"new"}`
	calloutFnSample    = `{"k":1,"schedMode":"relative","offsetMs":3600000,"expireOffsetMs":0}`
	calloutShortLimit  = 300 // responseTimeoutMs for a client that never answers
	calloutAwaitWithin = 5 * time.Second
)

// calloutDoc marshals a workflow-import document at the current schema version.
func calloutDoc(name, initial string, states map[string]any) string {
	b, _ := json.Marshal(map[string]any{
		"importMode": "REPLACE",
		"workflows": []any{map[string]any{
			"version": "1.5", "name": name, "initialState": initial, "active": true, "states": states,
		}},
	})
	return string(b)
}

func calloutProcessorWF(name, proc, tag string, extra map[string]any) string {
	return ComputeClientWorkflow(name, proc, tag, "", extra)
}

func calloutCriterionWF(name, tag string, extra map[string]any) string {
	cfg := map[string]any{"calculationNodesTags": tag, "attachEntity": true}
	for k, v := range extra {
		cfg[k] = v
	}
	return calloutDoc(name, "NONE", map[string]any{
		"NONE": map[string]any{"transitions": []any{map[string]any{
			"name": "init", "next": "ACTIVE", "manual": false,
			"criterion": map[string]any{"type": "function", "function": map[string]any{"name": "always-true", "config": cfg}},
		}}},
		"ACTIVE": map[string]any{},
	})
}

func calloutFunctionWF(name, tag string, extra map[string]any) string {
	fn := map[string]any{"name": "sched-fn-resolve", "resultKind": "Schedule", "calculationNodesTags": tag, "attachEntity": true}
	for k, v := range extra {
		fn[k] = v
	}
	return calloutDoc(name, "Open", map[string]any{
		"Open": map[string]any{"transitions": []any{map[string]any{
			"name": "AutoClose", "next": "Closed", "manual": false, "schedule": map[string]any{"function": fn},
		}}},
		"Closed": map[string]any{},
	})
}

// calloutCase is one tenant, one tag, one model, and the compute clients
// started for it in order.
type calloutCase struct {
	c       *client.Client
	model   string
	clients []ComputeClient
}

func newCalloutCase(t *testing.T, fixture BackendFixture, name, sample string, workflow func(tag string) string, behaviours ...string) calloutCase {
	t.Helper()
	requireComputeClients(t, fixture)
	tenant := fixture.NewTenant(t)
	tag := "co-" + name
	cc := calloutCase{c: client.NewClient(fixture.BaseURL(), tenant.Token), model: "co-" + name}
	for _, b := range behaviours {
		cc.clients = append(cc.clients, StartComputeClientOrSkip(t, fixture, ComputeClientSpec{TenantID: tenant.ID, Tags: []string{tag}, Behaviour: b}))
	}
	cbSetupModel(t, cc.c, cc.model, sample, workflow(tag))
	return cc
}

// calloutProblem asserts status, errorCode and retryable, and returns detail.
func calloutProblem(t *testing.T, status int, body []byte, wantStatus int, wantCode string, wantRetryable bool) string {
	t.Helper()
	var pd struct {
		Detail     string         `json:"detail"`
		Properties map[string]any `json:"properties"`
	}
	if status != wantStatus {
		t.Fatalf("status = %d; want %d %s (body: %s)", status, wantStatus, wantCode, body)
	}
	if err := json.Unmarshal(body, &pd); err != nil {
		t.Fatalf("response is not a problem document: %v (body: %s)", err, body)
	}
	if got, _ := pd.Properties["errorCode"].(string); got != wantCode {
		t.Errorf("errorCode = %q; want %q (body: %s)", got, wantCode, body)
	}
	if got, _ := pd.Properties["retryable"].(bool); got != wantRetryable {
		t.Errorf("retryable = %t; want %t (body: %s)", got, wantRetryable, body)
	}
	return pd.Detail
}

// failAndExpect drives one create that must fail, and requires that the spare
// client (the last one started) was never asked and nothing was committed.
func (cc calloutCase) failAndExpect(t *testing.T, sample string, wantStatus int, wantCode string, wantRetryable bool) string {
	t.Helper()
	status, body, err := cc.c.CreateEntityRaw(t, cc.model, 1, sample)
	if err != nil {
		t.Fatalf("CreateEntityRaw: %v", err)
	}
	detail := calloutProblem(t, status, body, wantStatus, wantCode, wantRetryable)
	AwaitReceived(t, cc.clients[0], 1, calloutAwaitWithin)
	spare := cc.clients[len(cc.clients)-1]
	if got := spare.Received(t); len(got) != 0 {
		t.Errorf("the spare compute client received %d requests; want 0: %+v", len(got), got)
	}
	if list, err := cc.c.ListEntitiesByModel(t, cc.model, 1); err != nil || len(list) != 0 {
		t.Errorf("entities after the failed create: %d err=%v; want 0", len(list), err)
	}
	return detail
}

// succeedOnSecond drives one create that must succeed on the second client
// under the first try's request id, and returns the new entity.
func (cc calloutCase) succeedOnSecond(t *testing.T, sample, wantKind string) client.EntityResult {
	t.Helper()
	id, err := cc.c.CreateEntity(t, cc.model, 1, sample)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	first := AwaitReceived(t, cc.clients[0], 1, calloutAwaitWithin)
	second := AwaitReceived(t, cc.clients[1], 1, calloutAwaitWithin)
	if first[0].Kind != wantKind || second[0].Kind != wantKind {
		t.Errorf("kinds = %q then %q; want %q", first[0].Kind, second[0].Kind, wantKind)
	}
	if first[0].RequestID == "" || first[0].RequestID != second[0].RequestID {
		t.Errorf("request ids differ across tries: %q then %q", first[0].RequestID, second[0].RequestID)
	}
	ent, err := cc.c.GetEntity(t, id)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	return ent
}

// RunCalloutNoAnswerNotIdempotentStops: a processor not declared idempotent is
// not repeated after a try that got no answer: 503 with the try's own code.
func RunCalloutNoAnswerNotIdempotentStops(t *testing.T, fixture BackendFixture) {
	cc := newCalloutCase(t, fixture, "noanswer-stop", calloutSample, func(tag string) string {
		return calloutProcessorWF("co-noanswer-stop-wf", "noop", tag, map[string]any{"responseTimeoutMs": calloutShortLimit})
	}, ComputeBehaviourStall, ComputeBehaviourCatalog)
	cc.failAndExpect(t, calloutSample, http.StatusServiceUnavailable, "DISPATCH_TIMEOUT", true)
}

// RunCalloutCriterionFailsOver: a criterion is repeat-safe by rule.
func RunCalloutCriterionFailsOver(t *testing.T, fixture BackendFixture) {
	cc := newCalloutCase(t, fixture, "crit-failover", calloutSample, func(tag string) string {
		return calloutCriterionWF("co-crit-failover-wf", tag, map[string]any{"responseTimeoutMs": calloutShortLimit})
	}, ComputeBehaviourStall, ComputeBehaviourCatalog)
	if ent := cc.succeedOnSecond(t, calloutSample, "criterion"); ent.Meta.State != "ACTIVE" {
		t.Errorf("state = %q; want ACTIVE (the criterion matched on the second compute client)", ent.Meta.State)
	}
}

// RunCalloutFunctionFailsOver: a schedule function is repeat-safe by rule.
func RunCalloutFunctionFailsOver(t *testing.T, fixture BackendFixture) {
	cc := newCalloutCase(t, fixture, "fn-failover", calloutFnSample, func(tag string) string {
		return calloutFunctionWF("co-fn-failover-wf", tag, map[string]any{"responseTimeoutMs": calloutShortLimit})
	}, ComputeBehaviourStall, ComputeBehaviourCatalog)
	if ent := cc.succeedOnSecond(t, calloutFnSample, "function"); ent.Meta.State != "Open" {
		t.Errorf("state = %q; want Open (armed an hour out, not fired)", ent.Meta.State)
	}
}

// RunCalloutMemberFailedStops: a compute node that answers "failed" ends the
// callout — declared idempotent or not — and its message and verdict reach the
// client.
func RunCalloutMemberFailedStops(t *testing.T, fixture BackendFixture) {
	requireComputeClients(t, fixture)
	cases := []struct {
		name          string
		first         string
		proc          string
		wantRetryable bool
		wantDetail    string
	}{
		{"verdict-true", ComputeBehaviourFailRetryable, "noop", true, "processor noop failed: scripted failure: fail-retryable"},
		{"no-verdict", ComputeBehaviourFail, "noop", false, "processor noop failed: scripted failure: fail"},
		{"verdict-false", ComputeBehaviourCatalog, "inject-error-not-retryable", false,
			"processor inject-error-not-retryable failed: inject-error-not-retryable: deliberate failure"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cc := newCalloutCase(t, fixture, "failed-"+tc.name, calloutSample, func(tag string) string {
				return calloutProcessorWF("co-failed-wf", tc.proc, tag, map[string]any{"idempotent": true})
			}, tc.first, ComputeBehaviourCatalog)
			detail := cc.failAndExpect(t, calloutSample, http.StatusBadRequest, "WORKFLOW_FAILED", tc.wantRetryable)
			if !strings.Contains(detail, tc.wantDetail) {
				t.Errorf("detail = %q; want it to contain %q", detail, tc.wantDetail)
			}
			if strings.Contains(detail, "processor dispatch failed:") {
				t.Errorf("detail = %q; the inner \"processor dispatch failed:\" segment is gone", detail)
			}
		})
	}
}

// RunCalloutRetryPolicyNoneOneTry: retryPolicy NONE is one try on a processor,
// a criterion and a function, repeat-safe or not.
func RunCalloutRetryPolicyNoneOneTry(t *testing.T, fixture BackendFixture) {
	requireComputeClients(t, fixture)
	none := func(extra map[string]any) map[string]any {
		extra["retryPolicy"] = "NONE"
		extra["responseTimeoutMs"] = calloutShortLimit
		return extra
	}
	cases := []struct {
		name, sample string
		workflow     func(tag string) string
	}{
		{"processor", calloutSample, func(tag string) string {
			return calloutProcessorWF("co-none-proc-wf", "noop", tag, none(map[string]any{"idempotent": true}))
		}},
		{"criterion", calloutSample, func(tag string) string { return calloutCriterionWF("co-none-crit-wf", tag, none(map[string]any{})) }},
		{"function", calloutFnSample, func(tag string) string { return calloutFunctionWF("co-none-fn-wf", tag, none(map[string]any{})) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cc := newCalloutCase(t, fixture, "none-"+tc.name, tc.sample, tc.workflow, ComputeBehaviourStall, ComputeBehaviourCatalog)
			cc.failAndExpect(t, tc.sample, http.StatusServiceUnavailable, "DISPATCH_TIMEOUT", true)
		})
	}
}

var (
	calloutFailedRe      = regexp.MustCompile(`the callout could not be completed, got (\d+) failures: (.*)$`)
	calloutFailedEntryRe = regexp.MustCompile(`\[member ?<?([^>:\]\s]+)>?: [^\]]*?(?: \((\d+) times\))?\]`)
)

// RunCalloutEveryTryUsed: every compute node of the tag stays silent on an
// idempotent processor -> 503 CALLOUT_FAILED, retryable, in the shape the
// contract fixes, naming only this tenant's compute nodes.
func RunCalloutEveryTryUsed(t *testing.T, fixture BackendFixture) {
	cc := newCalloutCase(t, fixture, "all-used", calloutSample, func(tag string) string {
		return calloutProcessorWF("co-all-used-wf", "noop", tag, map[string]any{"idempotent": true, "responseTimeoutMs": calloutShortLimit})
	}, ComputeBehaviourStall, ComputeBehaviourStall)

	status, body, err := cc.c.CreateEntityRaw(t, cc.model, 1, calloutSample)
	if err != nil {
		t.Fatalf("CreateEntityRaw: %v", err)
	}
	detail := calloutProblem(t, status, body, http.StatusServiceUnavailable, "CALLOUT_FAILED", true)
	m := calloutFailedRe.FindStringSubmatch(detail)
	if m == nil {
		t.Fatalf("detail = %q; want \"the callout could not be completed, got N failures: [...]\"", detail)
	}
	stated, _ := strconv.Atoi(m[1])
	perMember, sum := map[string]int{}, 0
	for _, e := range calloutFailedEntryRe.FindAllStringSubmatch(m[2], -1) {
		k := 1
		if e[2] != "" {
			k, _ = strconv.Atoi(e[2])
		}
		perMember[e[1]] += k
		sum += k
	}
	if stated != sum || stated < 2 || stated > 4 {
		t.Errorf("detail = %q; states %d failures, entries add up to %d; want them equal and between 2 and 4", detail, stated, sum)
	}
	a, b := cc.clients[0].MemberID(), cc.clients[1].MemberID()
	if len(perMember) != 2 || perMember[a] == 0 || perMember[b] == 0 {
		t.Errorf("detail = %q; want entries for exactly members %s and %s", detail, a, b)
	}
	if list, err := cc.c.ListEntitiesByModel(t, cc.model, 1); err != nil || len(list) != 0 {
		t.Errorf("entities after the failed create: %d err=%v; want 0", len(list), err)
	}
}
```

`registry.go` — after H's `ComputeClient…` block of `allTests`:

```go
	// Callout failover: what ends a callout and what the client is told. Each
	// case starts its own compute clients under a fresh tenant; a fixture that
	// cannot start them skips.
	{"CalloutNoAnswerNotIdempotentStops", RunCalloutNoAnswerNotIdempotentStops},
	{"CalloutCriterionFailsOver", RunCalloutCriterionFailsOver},
	{"CalloutFunctionFailsOver", RunCalloutFunctionFailsOver},
	{"CalloutMemberFailedStops", RunCalloutMemberFailedStops},
	{"CalloutRetryPolicyNoneOneTry", RunCalloutRetryPolicyNoneOneTry},
	{"CalloutEveryTryUsed", RunCalloutEveryTryUsed},
```

Header count and `wantParityScenarioCount`: +6.

`ComputeBehaviourCatalog` is H-8's constant for "serve the catalog" (the empty string). `RunCalloutEveryTryUsed` reaches `requireComputeClients` through `newCalloutCase`, as do the first three.

- [ ] **Step 4: Run to verify GREEN, and prove the teeth**

Run: `go test ./e2e/parity/`, then `go test -count=1 ./e2e/parity/memory/ -run 'TestParity/Callout'` and the same for `sqlite` and `postgres`; all six PASS, none SKIP (`-json | grep -c '"Action":"skip"'` → `0`).
Merge-base (+H, +C): `CalloutCriterionFailsOver`, `CalloutFunctionFailsOver` FAIL `CreateEntity: … 503`; `CalloutEveryTryUsed` FAILS on the code; `CalloutMemberFailedStops` FAILS on `detail` (today's text carries `processor dispatch failed:`), and `verdict-true` on `retryable` if the verdict is not yet carried. `CalloutNoAnswerNotIdempotentStops` and `CalloutRetryPolicyNoneOneTry/processor` are green there — pins.
Temporary reverts: S-1's #1, #2, #3 turn, respectively, the two `FailsOver` scenarios, `MemberFailedStops`, and `RetryPolicyNoneOneTry` red on the memory backend.

- [ ] **Step 5: Commit**

```
git add e2e/parity/callout_failover.go e2e/parity/callout_failover_test.go e2e/parity/registry.go e2e/parity/registry_count_test.go
git commit -m "test(parity): callout failover rules and error shapes on every backend (#254)"
```

---

### Task S-14: multi-pnode parity — the hand-over: success, two tries in one exchange, the cnode's words, two tenants

**Spec:** D5, D9, D12; §5 (the loop's hand-over step); §6 (`triesLeft`, `answerLimitMs`, `memberError`, `memberRetryable`, `attempts`); §13 rows 1057, 1062, 1063, 1070 — layer **M**.

**Files:**
- Create: `e2e/parity/multinode/callout_handover.go`

**Interfaces:**
- Consumes (H): `multinode.StartComputeClientOrSkip(t, fixture, node, spec)`, `multinode.ComputeClientCapable`, `parity.ComputeClientSpec`, `parity.ComputeBehaviour*`, `parity.AwaitReceived`, `parity.ReceivedCallout`; existing `cbRouteSetupModel`, `cbRouteSampleNoWriteback` (`callback_route.go:133, 123`), `Register`, `NamedTest`.
- Consumes (M stream): a pnode's list of tenants and tags reaches the other pnodes whole and in order — which is what makes the warm-up below a proof that the scenario's own tag is known too.
- Produces (for S-15): `mnProc`, `mnWorkflow`, `mnWarmUp`, `mnProblem`, `mnHasRequest`.

**Knowing that the owner knows.** A tag attached on pnode 1 reaches pnode 0 by the membership protocol, some time later. A scenario that drove its create at once would measure that delay against the fixture's 2 s patience (H-11). So each scenario starts, **last** on every peer it uses, a healthy client that also carries a *warm-up tag*, and creates warm-up entities through the owner until one succeeds. Lists travel whole, so once the warm-up tag is routable every tag attached before it on that pnode is known to the owner as well. No scenario stops a pnode or a client mid-flight; nothing here depends on which peer the random `PeerSelector` picks (every scenario in this task has exactly one peer advertising the tag).

What "honours the owner's answer limit" can mean from outside: both pnodes read the same workflow and the same configuration, so they would resolve the same limit on their own. The M scenario asserts that the peer's first try was cut off at the processor's 500 ms and not at the 30 s default (elapsed well under 10 s, and at least 500 ms); that the value *travels* rather than being re-resolved is the hand-over stream's unit test.

- [ ] **Step 1: Write the failing test** — append to `e2e/parity/multinode/compute_client_skip_test.go` (H-10)

```go
func TestCalloutHandOverScenariosSkipAndAreRegistered(t *testing.T) {
	scenarios := map[string]func(*testing.T, MultiNodeFixture){
		"Callout_HandOverSucceeds":                 RunCallout_HandOverSucceeds,
		"Callout_HandOverTwoTriesInOneExchange":    RunCallout_HandOverTwoTriesInOneExchange,
		"Callout_HandOverCarriesMessageAndVerdict": RunCallout_HandOverCarriesMessageAndVerdict,
		"Callout_TwoTenantsShareATag":              RunCallout_TwoTenantsShareATag,
	}
	registered := map[string]bool{}
	for _, nt := range AllTests() {
		registered[nt.Name] = true
	}
	for name, fn := range scenarios {
		if !registered[name] {
			t.Errorf("%s is not in the multinode registry", name)
		}
		if skipped := t.Run(name, func(t *testing.T) {
			fn(t, stubNoComputeClients{})
			t.Fatalf("%s did not skip on a fixture without ComputeClientCapable", name)
		}); !skipped {
			t.Errorf("%s must t.Skip when the fixture cannot start compute clients", name)
		}
	}
}
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./e2e/parity/multinode/ -run 'TestCalloutHandOverScenarios'` — FAIL (build) `undefined: RunCallout_HandOverSucceeds`.

- [ ] **Step 3: Write the scenarios** — `e2e/parity/multinode/callout_handover.go`

```go
package multinode

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

func init() {
	Register(
		NamedTest{Name: "Callout_HandOverSucceeds", Fn: RunCallout_HandOverSucceeds},
		NamedTest{Name: "Callout_HandOverTwoTriesInOneExchange", Fn: RunCallout_HandOverTwoTriesInOneExchange},
		NamedTest{Name: "Callout_HandOverCarriesMessageAndVerdict", Fn: RunCallout_HandOverCarriesMessageAndVerdict},
		NamedTest{Name: "Callout_TwoTenantsShareATag", Fn: RunCallout_TwoTenantsShareATag},
	)
}

const mnSample = `{"name":"Test","amount":10,"status":"new"}`

// mnProc builds one processor entry routed to tag.
func mnProc(name, mode, tag, contextValue string, extra map[string]any) map[string]any {
	cfg := map[string]any{"attachEntity": true, "calculationNodesTags": tag, "context": contextValue}
	for k, v := range extra {
		cfg[k] = v
	}
	return map[string]any{"type": "calculator", "name": name, "executionMode": mode, "config": cfg}
}

// mnWorkflow builds a NONE -> ACTIVE workflow whose automated transition runs
// procs in order.
func mnWorkflow(wfName string, procs ...map[string]any) string {
	list := make([]any, len(procs))
	for i, p := range procs {
		list[i] = p
	}
	b, _ := json.Marshal(map[string]any{
		"importMode": "REPLACE",
		"workflows": []any{map[string]any{
			"version": "1.5", "name": wfName, "initialState": "NONE", "active": true,
			"states": map[string]any{
				"NONE":   map[string]any{"transitions": []any{map[string]any{"name": "init", "next": "ACTIVE", "manual": false, "processors": list}}},
				"ACTIVE": map[string]any{},
			},
		}},
	})
	return string(b)
}

// mnRequire skips unless the fixture can start compute clients and has the
// pnodes the scenario needs.
func mnRequire(t *testing.T, fixture MultiNodeFixture, pnodes int) []string {
	t.Helper()
	if _, ok := fixture.(ComputeClientCapable); !ok {
		t.Skip("cluster fixture cannot start further compute clients; scenario pending on this backend")
	}
	urls := fixture.BaseURLs()
	if len(urls) < pnodes {
		t.Fatalf("needs at least %d pnodes, got %d", pnodes, len(urls))
	}
	return urls
}

// mnWarmUp starts a healthy compute client on pnode node carrying warmTag plus
// extraTags, and returns once a create routed to warmTag succeeds through
// owner — so the owner knows that pnode's list, with every tag attached to it
// before this call. Call it LAST for each peer.
func mnWarmUp(t *testing.T, fixture MultiNodeFixture, owner *client.Client, tenant parity.Tenant, node int, warmTag string, extraTags ...string) parity.ComputeClient {
	t.Helper()
	model := "mn-warm-" + warmTag
	cbRouteSetupModel(t, owner, model, cbRouteSampleNoWriteback, mnWorkflow(model+"-wf", mnProc("noop", "SYNC", warmTag, "", nil)))
	cc := StartComputeClientOrSkip(t, fixture, node, parity.ComputeClientSpec{TenantID: tenant.ID, Tags: append([]string{warmTag}, extraTags...)})
	deadline := time.Now().Add(30 * time.Second)
	for {
		status, body, err := owner.CreateEntityRaw(t, model, 1, mnSample)
		if err != nil {
			t.Fatalf("warm-up create: %v", err)
		}
		if status == http.StatusOK {
			return cc
		}
		if time.Now().After(deadline) {
			t.Fatalf("30s after it attached to pnode %d the owner still cannot route to tag %s: %d %s", node, warmTag, status, body)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// mnProblem asserts status, errorCode and retryable, and returns detail.
func mnProblem(t *testing.T, status int, body []byte, wantStatus int, wantCode string, wantRetryable bool) string {
	t.Helper()
	var pd struct {
		Detail     string         `json:"detail"`
		Properties map[string]any `json:"properties"`
	}
	if status != wantStatus {
		t.Fatalf("status = %d; want %d %s (body: %s)", status, wantStatus, wantCode, body)
	}
	if err := json.Unmarshal(body, &pd); err != nil {
		t.Fatalf("response is not a problem document: %v (body: %s)", err, body)
	}
	if got, _ := pd.Properties["errorCode"].(string); got != wantCode {
		t.Errorf("errorCode = %q; want %q (body: %s)", got, wantCode, body)
	}
	if got, _ := pd.Properties["retryable"].(bool); got != wantRetryable {
		t.Errorf("retryable = %t; want %t (body: %s)", got, wantRetryable, body)
	}
	return pd.Detail
}

// mnHasRequest reports whether cc received a calculation request with requestID.
func mnHasRequest(t *testing.T, cc parity.ComputeClient, requestID string) bool {
	t.Helper()
	for _, r := range cc.Received(t) {
		if r.RequestID == requestID {
			return true
		}
	}
	return false
}

// RunCallout_HandOverSucceeds: the owner's own compute node drops with the work
// in its hands; the processor is idempotent; the callout is handed over to the
// pnode that has another, under the same request id.
func RunCallout_HandOverSucceeds(t *testing.T, fixture MultiNodeFixture) {
	urls := mnRequire(t, fixture, 2)
	tenant := fixture.NewTenant(t)
	owner := client.NewClient(urls[0], tenant.Token)
	const model, tag = "mn-ho-ok", "mn-ho-ok"

	local := StartComputeClientOrSkip(t, fixture, 0, parity.ComputeClientSpec{TenantID: tenant.ID, Tags: []string{tag}, Behaviour: parity.ComputeBehaviourDrop})
	remote := mnWarmUp(t, fixture, owner, tenant, 1, "mn-ho-ok-w", tag)
	cbRouteSetupModel(t, owner, model, cbRouteSampleNoWriteback,
		mnWorkflow("mn-ho-ok-wf", mnProc("noop", "SYNC", tag, "", map[string]any{"idempotent": true})))

	if _, err := owner.CreateEntity(t, model, 1, mnSample); err != nil {
		t.Fatalf("create through the owner: %v", err)
	}
	first := parity.AwaitReceived(t, local, 1, 5*time.Second)
	if !mnHasRequest(t, remote, first[0].RequestID) {
		t.Errorf("the other pnode's compute client did not receive request %s; received: %+v", first[0].RequestID, remote.Received(t))
	}
}

// RunCallout_HandOverTwoTriesInOneExchange: the owner has no compute node of
// its own; the other pnode has a silent one and a healthy one. One hand-over
// covers both tries, and the silent one is given up after the processor's own
// limit.
func RunCallout_HandOverTwoTriesInOneExchange(t *testing.T, fixture MultiNodeFixture) {
	urls := mnRequire(t, fixture, 2)
	tenant := fixture.NewTenant(t)
	owner := client.NewClient(urls[0], tenant.Token)
	const model, tag = "mn-ho-two", "mn-ho-two"

	silent := StartComputeClientOrSkip(t, fixture, 1, parity.ComputeClientSpec{TenantID: tenant.ID, Tags: []string{tag}, Behaviour: parity.ComputeBehaviourStall})
	healthy := mnWarmUp(t, fixture, owner, tenant, 1, "mn-ho-two-w", tag)
	cbRouteSetupModel(t, owner, model, cbRouteSampleNoWriteback,
		mnWorkflow("mn-ho-two-wf", mnProc("noop", "SYNC", tag, "", map[string]any{"idempotent": true, "responseTimeoutMs": 500})))

	start := time.Now()
	if _, err := owner.CreateEntity(t, model, 1, mnSample); err != nil {
		t.Fatalf("create through the owner: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 500*time.Millisecond || elapsed > 10*time.Second {
		t.Errorf("create took %v; want at least the 500ms limit of the silent try and far less than the 30s default", elapsed)
	}
	first := parity.AwaitReceived(t, silent, 1, 5*time.Second)
	if len(first) != 1 {
		t.Errorf("the silent compute client received %d requests; want 1", len(first))
	}
	if !mnHasRequest(t, healthy, first[0].RequestID) {
		t.Errorf("the healthy compute client did not receive request %s", first[0].RequestID)
	}
}

// RunCallout_HandOverCarriesMessageAndVerdict: the compute node that fails the
// processor is on another pnode; its own message and its verdict reach the
// client of the owner.
func RunCallout_HandOverCarriesMessageAndVerdict(t *testing.T, fixture MultiNodeFixture) {
	urls := mnRequire(t, fixture, 2)
	tenant := fixture.NewTenant(t)
	owner := client.NewClient(urls[0], tenant.Token)
	const model, tag = "mn-ho-verdict", "mn-ho-verdict"

	failing := StartComputeClientOrSkip(t, fixture, 1, parity.ComputeClientSpec{TenantID: tenant.ID, Tags: []string{tag}, Behaviour: parity.ComputeBehaviourFailRetryable})
	mnWarmUp(t, fixture, owner, tenant, 1, "mn-ho-verdict-w")
	cbRouteSetupModel(t, owner, model, cbRouteSampleNoWriteback,
		mnWorkflow("mn-ho-verdict-wf", mnProc("noop", "SYNC", tag, "", map[string]any{"idempotent": true})))

	status, body, err := owner.CreateEntityRaw(t, model, 1, mnSample)
	if err != nil {
		t.Fatalf("CreateEntityRaw: %v", err)
	}
	detail := mnProblem(t, status, body, http.StatusBadRequest, "WORKFLOW_FAILED", true)
	if want := "processor noop failed: scripted failure: fail-retryable"; !strings.Contains(detail, want) {
		t.Errorf("detail = %q; want it to contain %q", detail, want)
	}
	if got := parity.AwaitReceived(t, failing, 1, 5*time.Second); len(got) != 1 {
		t.Errorf("the failing compute client received %d requests; want 1 — a \"failed\" answer ends the callout", len(got))
	}
}

var mnMemberRe = regexp.MustCompile(`\[member ?<?([^>:\]\s]+)>?:`)

// RunCallout_TwoTenantsShareATag: two tenants attach compute nodes under one
// tag to one pnode. Each tenant's callout, handed over from another pnode,
// goes only to its own; a failure names only its own.
func RunCallout_TwoTenantsShareATag(t *testing.T, fixture MultiNodeFixture) {
	urls := mnRequire(t, fixture, 2)
	x, y := fixture.NewTenant(t), fixture.NewTenant(t)
	ownerX, ownerY := client.NewClient(urls[0], x.Token), client.NewClient(urls[0], y.Token)
	const model, tag = "mn-two-tenants", "mn-shared"

	x1 := StartComputeClientOrSkip(t, fixture, 1, parity.ComputeClientSpec{TenantID: x.ID, Tags: []string{tag}, Behaviour: parity.ComputeBehaviourStall})
	x2 := StartComputeClientOrSkip(t, fixture, 1, parity.ComputeClientSpec{TenantID: x.ID, Tags: []string{tag}, Behaviour: parity.ComputeBehaviourStall})
	mnWarmUp(t, fixture, ownerX, x, 1, "mn-shared-wx")
	yHealthy := mnWarmUp(t, fixture, ownerY, y, 1, "mn-shared-wy", tag)

	wf := mnWorkflow("mn-two-tenants-wf", mnProc("noop", "SYNC", tag, "", map[string]any{"idempotent": true, "responseTimeoutMs": 300}))
	cbRouteSetupModel(t, ownerX, model, cbRouteSampleNoWriteback, wf)
	cbRouteSetupModel(t, ownerY, model, cbRouteSampleNoWriteback, wf)

	yBefore := len(yHealthy.Received(t))
	status, body, err := ownerX.CreateEntityRaw(t, model, 1, mnSample)
	if err != nil {
		t.Fatalf("tenant X CreateEntityRaw: %v", err)
	}
	detail := mnProblem(t, status, body, http.StatusServiceUnavailable, "CALLOUT_FAILED", true)
	named := map[string]bool{}
	for _, m := range mnMemberRe.FindAllStringSubmatch(detail, -1) {
		named[m[1]] = true
	}
	if !named[x1.MemberID()] || !named[x2.MemberID()] || len(named) != 2 {
		t.Errorf("detail = %q; want it to name exactly tenant X's members %s and %s", detail, x1.MemberID(), x2.MemberID())
	}
	if strings.Contains(detail, yHealthy.MemberID()) {
		t.Errorf("detail = %q; it names tenant Y's compute node", detail)
	}
	if got := len(yHealthy.Received(t)); got != yBefore {
		t.Errorf("tenant Y's healthy compute node received %d of tenant X's requests", got-yBefore)
	}

	if _, err := ownerY.CreateEntity(t, model, 1, mnSample); err != nil {
		t.Fatalf("tenant Y create: %v", err)
	}
	if got := len(yHealthy.Received(t)); got != yBefore+1 {
		t.Errorf("tenant Y's compute node received %d requests for tenant Y's create; want 1", got-yBefore)
	}
	if len(x1.Received(t))+len(x2.Received(t)) > 4 {
		t.Errorf("tenant X's compute nodes received more requests than tenant X's tries allow")
	}
}
```

Keep tags short, as H-10 asks: if this task runs before the membership stream's fix is merged, four tenants' worth of tags still ride in the 512-byte memberlist metadata.

- [ ] **Step 4: Run to verify GREEN, and prove the teeth**

Run: `go test ./e2e/parity/multinode/`, then `go test -count=1 ./e2e/parity/postgres/ -run 'TestMultiNode/Callout_(HandOver|TwoTenants)'` — four PASS, none SKIP.
Merge-base (+H, +C): `HandOverSucceeds` FAILS `create through the owner: … 503` (today a local member ends the matter, `cluster_dispatcher.go`); `TwoTriesInOneExchange` FAILS the same way (the peer makes one try); `TwoTenantsShareATag` FAILS on the code. `CarriesMessageAndVerdict` FAILS on `retryable` and on the detail (the verdict and the cnode's text are flattened across the hop today, R§4.3).
Temporary reverts: (1) in the peer's hand-over handler call `RunLocal(ctx, call, 1)` instead of `triesLeft` → the peer answers `no_answer` after the silent try and the owner has no other peer to ask; `TwoTriesInOneExchange` FAILS `503` — unless the owner's loop starts another pass once its patience has run out (Open point 3), in which case the create succeeds about 2 s later and this scenario cannot tell; "one exchange" is then pinned by the hand-over stream's unit test alone, and the implementer records which of the two he saw. (2) In the owner's reading of a `member_failed` answer, drop `memberRetryable` → `CarriesMessageAndVerdict` FAILS on `retryable`. (3) In `MemberRegistry.Candidates`, drop the tenant comparison → `TwoTenantsShareATag` FAILS.

- [ ] **Step 5: Commit**

```
git add e2e/parity/multinode/callout_handover.go e2e/parity/multinode/compute_client_skip_test.go
git commit -m "test(multinode): hand-over success, two tries in one exchange, the cnode's words, two tenants on one tag (#254)"
```

---

### Task S-15: multi-pnode parity — passes minted by another pnode, and the numbers they carry

**Spec:** §7 "Claims" ("A pass is minted per try by the pnode that makes the hand-off … with `NodeID` = the owner's id"), "The fencing number" (a higher `minor` is absorbed; `Advance` resets the highest `minor` seen); §13 rows 1047, 1072 — layer **M**.

**Files:**
- Create: `e2e/parity/multinode/callout_fencing.go`
- Modify: `e2e/parity/multinode/compute_client_skip_test.go` (the scenario map of S-14's test gains the two names below)

**Interfaces:**
- Consumes: S-14 helpers; H `parity.ComputeBehaviourLateCallback`, `(cc) Release(t)`, `parity.LateCallbackOutcome{HTTPStatus, HTTPErrorCode, GRPCAttempted, GRPCSuccess, GRPCErrorMessage, Error}`; catalog processor `cb-create-secondary` (`cmd/compute-test-client/callback.go:294`); existing `cbRouteContext`, `cbRouteSecondaryWorkflow`, `cbRouteSampleSecondary`, `cbRouteSampleCreateSecondary`, `cbRouteSameTxID` (`callback_route.go:57, 72, 126, 128, 434`).

**Keeping the transaction open across pnodes.** The compute-test-client cannot be told to answer slowly (`slow-configurable` reads `sleep_ms` from `parameters`, which is a JSON *string*, so its sleep is always 0 — Open point 5), so the open transaction is made from a second `ASYNC_NEW_TX` processor routed to a `stall` client with a 5 s limit: its failure does not fail the operation, and for those 5 s the transaction is open while the first processor's callout has ended. The late callback is released as soon as the stalled client has the work, so the 5 s are slack, not a race: the refusal is asserted the moment it is made.

**The one place a random choice matters.** Row 1047 needs two hand-overs of one callout, so two peers advertise the tag, and `PeerSelector` is random. The scenario does not assert by luck: it drives creates until one is handed to pnode 1 first (seen from pnode 1's client receiving work), and asserts only on that one. A create handed to pnode 2 first is served at once by the healthy client there, costs milliseconds, and is discarded. Ten creates all going to pnode 2 first has probability 2⁻¹⁰; the scenario fails loudly in that case rather than passing vacuously. Footnote ³ of §13 waived a *different* row for randomness plus damage to the shared fixture; nothing here stops a pnode. See Open point 4.

- [ ] **Step 1: Write the failing test** — add to S-14's scenario map

```go
		"Callout_PassFromAnotherPnode":         RunCallout_PassFromAnotherPnode,
		"Callout_MinorAbsorbedAcrossHandOvers": RunCallout_MinorAbsorbedAcrossHandOvers,
```

- [ ] **Step 2: Run to verify RED** — `go test ./e2e/parity/multinode/ -run 'TestCalloutHandOverScenarios'` → FAIL (build) `undefined: RunCallout_PassFromAnotherPnode`.

- [ ] **Step 3: Write the scenarios** — `e2e/parity/multinode/callout_fencing.go`

```go
package multinode

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

func init() {
	Register(
		NamedTest{Name: "Callout_PassFromAnotherPnode", Fn: RunCallout_PassFromAnotherPnode},
		NamedTest{Name: "Callout_MinorAbsorbedAcrossHandOvers", Fn: RunCallout_MinorAbsorbedAcrossHandOvers},
	)
}

type mnCreate struct {
	status int
	body   []byte
	err    error
}

func mnGoCreate(t *testing.T, c *client.Client, model, sample string) <-chan mnCreate {
	out := make(chan mnCreate, 1)
	go func() {
		status, body, err := c.CreateEntityRaw(t, model, 1, sample)
		out <- mnCreate{status, body, err}
	}()
	return out
}

func mnAwaitCreate(t *testing.T, ch <-chan mnCreate, within time.Duration) mnCreate {
	t.Helper()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("create: %v", r.err)
		}
		return r
	case <-time.After(within):
		t.Fatalf("the create did not complete within %s", within)
		return mnCreate{}
	}
}

// mnLastCallback returns the late-callback outcome of the most recent request
// in a released client's record.
func mnLastCallback(t *testing.T, recs []parity.ReceivedCallout) parity.LateCallbackOutcome {
	t.Helper()
	if len(recs) == 0 || recs[len(recs)-1].Callback == nil {
		t.Fatalf("Release recorded no callback outcome: %+v", recs)
	}
	cb := *recs[len(recs)-1].Callback
	if cb.Error != "" {
		t.Fatalf("the late callback could not be made: %s", cb.Error)
	}
	return cb
}

func mnAssertRefused(t *testing.T, who string, cb parity.LateCallbackOutcome) {
	t.Helper()
	if cb.HTTPStatus != http.StatusGone || cb.HTTPErrorCode != "CALLOUT_SUPERSEDED" {
		t.Errorf("%s, HTTP door: %d %s; want 410 CALLOUT_SUPERSEDED", who, cb.HTTPStatus, cb.HTTPErrorCode)
	}
	if cb.GRPCAttempted && (cb.GRPCSuccess || !strings.HasPrefix(cb.GRPCErrorMessage, "CALLOUT_SUPERSEDED:")) {
		t.Errorf("%s, gRPC door: success=%t message=%q; want a refusal with CALLOUT_SUPERSEDED", who, cb.GRPCSuccess, cb.GRPCErrorMessage)
	}
}

// RunCallout_PassFromAnotherPnode: the pnode that makes the hand-off mints the
// pass, in the owner's name. A callback bearing it joins the owner's
// transaction; once that callout has ended the same kind of pass is refused
// while the transaction is still open.
func RunCallout_PassFromAnotherPnode(t *testing.T, fixture MultiNodeFixture) {
	urls := mnRequire(t, fixture, 2)

	t.Run("joins-the-owners-transaction", func(t *testing.T) {
		tenant := fixture.NewTenant(t)
		owner := client.NewClient(urls[0], tenant.Token)
		const primary, secondary, tag = "mn-pass-join", "mn-pass-join-sec", "mn-pass-join"
		cbRouteSetupModel(t, owner, secondary, cbRouteSampleSecondary, cbRouteSecondaryWorkflow)
		mnWarmUp(t, fixture, owner, tenant, 1, "mn-pass-join-w", tag)
		cbRouteSetupModel(t, owner, primary, cbRouteSampleCreateSecondary,
			mnWorkflow("mn-pass-join-wf", mnProc("cb-create-secondary", "SYNC", tag, cbRouteContext(secondary, "mn-pass-join"), nil)))

		id, err := owner.CreateEntity(t, primary, 1, cbRouteSampleCreateSecondary)
		if err != nil {
			t.Fatalf("create through the owner: %v", err)
		}
		ent, err := owner.GetEntity(t, id)
		if err != nil {
			t.Fatalf("GetEntity: %v", err)
		}
		secID, _ := ent.Data["secondaryId"].(string)
		if secID == "" || ent.Data["tokenWasEmpty"] != false {
			t.Fatalf("primary data = %v; want a secondaryId and tokenWasEmpty=false", ent.Data)
		}
		cbRouteSameTxID(t, owner, id, uuid.MustParse(secID))
	})

	t.Run("refused-once-its-callout-ended", func(t *testing.T) {
		tenant := fixture.NewTenant(t)
		owner := client.NewClient(urls[0], tenant.Token)
		const primary, secondary, tagLate, tagHold = "mn-pass-ended", "mn-pass-ended-sec", "mn-pass-late", "mn-pass-hold"
		cbRouteSetupModel(t, owner, secondary, cbRouteSampleSecondary, cbRouteSecondaryWorkflow)

		late := StartComputeClientOrSkip(t, fixture, 1, parity.ComputeClientSpec{TenantID: tenant.ID, Tags: []string{tagLate}, Behaviour: parity.ComputeBehaviourLateCallback})
		hold := StartComputeClientOrSkip(t, fixture, 1, parity.ComputeClientSpec{TenantID: tenant.ID, Tags: []string{tagHold}, Behaviour: parity.ComputeBehaviourStall})
		mnWarmUp(t, fixture, owner, tenant, 1, "mn-pass-ended-w")
		cbRouteSetupModel(t, owner, primary, cbRouteSampleNoWriteback, mnWorkflow("mn-pass-ended-wf",
			mnProc("late", "ASYNC_NEW_TX", tagLate, cbRouteContext(secondary, "mn-pass-ended"), map[string]any{"responseTimeoutMs": 500}),
			mnProc("hold", "ASYNC_NEW_TX", tagHold, "", map[string]any{"responseTimeoutMs": 5000})))

		done := mnGoCreate(t, owner, primary, mnSample)
		parity.AwaitReceived(t, hold, 1, 15*time.Second) // the first callout has ended; the transaction is open
		mnAssertRefused(t, "the late compute node", mnLastCallback(t, late.Release(t)))

		if r := mnAwaitCreate(t, done, 30*time.Second); r.status != http.StatusOK {
			t.Fatalf("create: %d %s; want 200 — both processors are ASYNC_NEW_TX", r.status, r.body)
		}
		if list, err := owner.ListEntitiesByModel(t, secondary, 1); err != nil || len(list) != 0 {
			t.Errorf("secondary entities: %d err=%v; want 0 — the refused callback wrote nothing", len(list), err)
		}
	})
}

// RunCallout_MinorAbsorbedAcrossHandOvers: pnode 1 makes two tries under one
// hand-over. The second try's callback is admitted and, from then on, the
// first try's is refused. The callout is then handed over to pnode 2, whose
// first try (minor 1 under the next major) is admitted although minor 2 was
// seen under the previous major.
func RunCallout_MinorAbsorbedAcrossHandOvers(t *testing.T, fixture MultiNodeFixture) {
	urls := mnRequire(t, fixture, 3)
	tenant := fixture.NewTenant(t)
	owner := client.NewClient(urls[0], tenant.Token)
	const primary, secondary, tag = "mn-minor", "mn-minor-sec", "mn-minor"
	cbRouteSetupModel(t, owner, secondary, cbRouteSampleSecondary, cbRouteSecondaryWorkflow)

	try1 := StartComputeClientOrSkip(t, fixture, 1, parity.ComputeClientSpec{TenantID: tenant.ID, Tags: []string{tag}, Behaviour: parity.ComputeBehaviourLateCallback})
	try2 := StartComputeClientOrSkip(t, fixture, 1, parity.ComputeClientSpec{TenantID: tenant.ID, Tags: []string{tag}, Behaviour: parity.ComputeBehaviourLateCallback})
	mnWarmUp(t, fixture, owner, tenant, 1, "mn-minor-w1")
	mnWarmUp(t, fixture, owner, tenant, 2, "mn-minor-w2", tag) // the healthy one, on pnode 2
	cbRouteSetupModel(t, owner, primary, cbRouteSampleCreateSecondary, mnWorkflow("mn-minor-wf",
		mnProc("cb-create-secondary", "SYNC", tag, cbRouteContext(secondary, "mn-minor"), map[string]any{"idempotent": true, "responseTimeoutMs": 4000})))

	for attempt := 1; attempt <= 10; attempt++ {
		done := mnGoCreate(t, owner, primary, cbRouteSampleCreateSecondary)
		exercised := false
		for waiting := true; waiting; {
			select {
			case r := <-done:
				if r.err != nil || r.status != http.StatusOK {
					t.Fatalf("attempt %d, served by pnode 2 first: status=%d err=%v body=%s", attempt, r.status, r.err, r.body)
				}
				waiting = false
			case <-time.After(50 * time.Millisecond):
				if len(try1.Received(t)) > 0 {
					exercised, waiting = true, false
				}
			}
		}
		if !exercised {
			continue // pnode 2 was asked first; nothing to assert on this create
		}

		// pnode 1 was asked first: try (M,1) went to try1, which holds it.
		parity.AwaitReceived(t, try2, 1, 15*time.Second) // after 4s: try (M,2) went to try2
		cb2 := mnLastCallback(t, try2.Release(t))
		if cb2.HTTPStatus != http.StatusOK || (cb2.GRPCAttempted && !cb2.GRPCSuccess) {
			t.Fatalf("the second try's callback: HTTP %d %s, gRPC success=%t; want admitted — a higher minor is absorbed",
				cb2.HTTPStatus, cb2.HTTPErrorCode, cb2.GRPCSuccess)
		}
		mnAssertRefused(t, "the first try, after the second try's callback was seen", mnLastCallback(t, try1.Release(t)))

		// try2 never answers; pnode 1 reports no_answer; the callout is handed
		// over to pnode 2, whose cb-create-secondary calls back under (M+1, 1).
		r := mnAwaitCreate(t, done, 60*time.Second)
		if r.status != http.StatusOK {
			t.Fatalf("create: %d %s; want 200 — the second hand-over's minor 1 must be admitted", r.status, r.body)
		}
		return
	}
	t.Fatal("pnode 1 was never asked first in 10 creates (probability 2^-10); the scenario asserted nothing")
}
```

- [ ] **Step 4: Run to verify GREEN, and prove the teeth**

Run: `go test ./e2e/parity/multinode/`, then `go test -count=1 ./e2e/parity/postgres/ -run 'TestMultiNode/Callout_(PassFromAnotherPnode|MinorAbsorbed)'`. Both PASS, neither SKIP. `MinorAbsorbed…` costs about 9 s when exercised (two 4 s limits), `refused-once…` about 5.5 s.
Merge-base (+H, +C): `joins-the-owners-transaction` is green (it is today's `Callback_ForwardedDispatch_HTTP` with the pass now minted by the peer) — a pin; `refused-once-its-callout-ended` FAILS `HTTP door: 200; want 410`; `MinorAbsorbed…` FAILS `create: 503`.
Temporary reverts: (1) on the peer, mint the pass with the peer's own node id as `NodeID` → `joins-…` FAILS (the callback is not routed to the owner; `tokenWasEmpty`/`secondaryId` assertions or a 404). (2) In `Fence.Admit`, do not absorb a higher `minor` → `the first try … was seen` FAILS `HTTP door: 200; want 410`. (3) In `Fence.Advance`, do not reset the highest `minor` seen → the final create FAILS `400 WORKFLOW_FAILED` (pnode 2's callback under minor 1 is refused, so `cb-create-secondary` fails).

- [ ] **Step 5: Commit**

```
git add e2e/parity/multinode/callout_fencing.go e2e/parity/multinode/compute_client_skip_test.go
git commit -m "test(multinode): passes minted by another pnode join and are fenced; minor absorbed, reset by the next hand-over (#254)"
```

---

## Gate 4 for this stream

Nothing user-facing changes. No env var, error code, help topic, README section
or OpenAPI entry is added here. `registry.go`'s header count and
`wantParityScenarioCount` move together (S-13). Courtesy note for the commercial
backend's suite, to be passed on by whoever lands the plan: the new P and M
scenarios skip there until its fixtures implement `parity.ComputeClientFixture`
/ `multinode.ComputeClientCapable` (H's Gate-4 note says how); `TestMultiNode`
there needs three pnodes for `Callout_MinorAbsorbedAcrossHandOvers`.

## Order and cost

S-1 … S-11 depend on H-1…H-4, C (import and settings), L, O, F, and run in
order within one worktree (S-1 and S-6 produce helpers the later ones use;
otherwise S-2…S-5, S-6…S-10 and S-11 are independent and may be written
concurrently once S-1 and S-6 exist). S-12, S-13 depend on H-5…H-9, H-11 and O.
S-14, S-15 depend on H-10, H-11, P (hand-over), M (membership) and F.
Added wall-clock, measured in answer limits the scenarios wait out: E ≈ 40 s
across eleven files (each stack start is the larger cost: one PostgreSQL-backed
app per test function, ~25 of them); P ≈ 6 s per backend plus ~20 compute-client
subprocess starts; M ≈ 25 s plus ~16 subprocess starts and the warm-up polls.

## §13, row by row — the E, P and M cells

"—" = the matrix has no mark in that cell. Rows are in the matrix's order.

| §13 row | E | P | M |
|---|---|---|---|
| Every dispatcher error site → its kind | — | — | — |
| `NoHandOff` → next local cnode answers | waived (footnote 1) | waived (footnote 1) | — |
| `NoHandOff`, tries = 1 → the try's own code | waived (footnote 1) | — | — |
| `NoAnswer`, processor not idempotent → stop, 503 own code | S-1 | S-13 (`CalloutNoAnswerNotIdempotentStops`); S-12 (09_11/not-idempotent) | — |
| `NoAnswer`, `idempotent` → next cnode answers (09_10) | S-1 | S-12 (09_10) | — |
| `NoAnswer`, criterion; function → next cnode answers | S-2 | S-13 (`CalloutCriterionFailsOver`, `CalloutFunctionFailsOver`) | — |
| cnode drops after hand-off (09_09, 09_11) — both settings | S-1 | S-12 (09_09; 09_11 both subtests) | — |
| `MemberFailed` verdict true → 400 retryable, one try | S-1 | S-13 (`CalloutMemberFailedStops/verdict-true`) | — |
| `MemberFailed` verdict false / absent → 400, one try | S-1 | S-13 (`…/verdict-false`, `…/no-verdict`) | — |
| `Terminal` → stop, as today | S-1 (`terminal`) | — | — |
| `retryPolicy: NONE` on a processor, a criterion, a function → one try | S-1 (processor), S-2 (criterion, function) | S-13 (`CalloutRetryPolicyNoneOneTry`) | — |
| Every try used → 503 `CALLOUT_FAILED`, message shape | S-3 | S-13 (`CalloutEveryTryUsed`) | — |
| Exactly one attempt recorded → not wrapped | S-3 | — | — |
| Attempts on record beat "no cnode" | S-3 | — | — |
| Same request id on every try | S-1 (also asserted in S-2, S-5, S-12, S-13, S-14) | — | — |
| Callout deadline cuts off a try in progress | — | — | — |
| Client timeout (408) / cancellation during a wait and during a try | S-4 | — | — |
| No cnode → waits, one attaches → succeeds, no try used | S-4 | waived (footnote 2) | waived (footnote 2) |
| Patience is one allowance across several waits | — | — | — |
| Patience applies with `retryPolicy: NONE` | S-4 | — | — |
| No cnode within patience → 503; patience 0 → at once | S-4 | covered by the existing `ExternalAPI_09_08_NoExternalRegisteredFails` (kept unchanged; H-11 sets the patience it waits out) | — |
| `ASYNC_NEW_TX`: callout fails → operation succeeds, nothing reported | S-5 | — | — |
| `COMMIT_BEFORE_DISPATCH`, both variants: failure leaves TX_pre committed | S-5 | — | — |
| Callouts made from a scheduled fire follow the same rules | S-5 | — | — |
| Late callback after the callout ended, transaction still open → 410, both doors, write and read | S-6 | — | — |
| Late callback after the transaction ended → 404 | S-6 | — | — |
| Late callback in `ASYNC_NEW_TX` after failure → 410 | S-6 (`failed-async-new-tx`) | — | — |
| Second cnode of the owner's → first cnode's callback refused at once | S-7 | — | — |
| A joined request queued for the lock → refused on taking it | S-8 (`OwnerWaitsForARequestInProgress`, the queued write) | — | — |
| A joined request in progress → the wait | S-8 (same test, the write in progress) | — | — |
| `ASYNC_NEW_TX`: failed processor's write in progress is not in the committed result | S-8 | waived (footnote 4) | — |
| A callback waiting on a callout of its own → released … 410, not 200 | S-7 (`NestedCalloutReleased`, three modes) | — | — |
| The same in `ASYNC_NEW_TX`: savepoint neither undone nor released | — | — | — |
| A callback past its last check → lands, 200; a joined collection lands whole | S-8 | — | — |
| A joined read in progress → completes; owner's operation succeeds (PostgreSQL) | S-8 (`JoinedReadInProgress/answer-limit-passes`) | — | — |
| Parallel joined reads / read against write serialised | S-9 | — | — |
| A non-entity joined request … refused once its cnode is replaced | S-9 | — | — |
| A joined request's response is sent after the lock is released | — | — | — |
| A joined request's body is read before the lock is taken | S-9 (`StalledBodyHoldsNothing`, HTTP and gRPC server-streaming) | — | — |
| A joined handler that panics gives the lock back | — | — | — |
| A joined `GetTransitions` … holds the lock | — | — | — |
| A cnode disconnects in the middle of its callback (PostgreSQL) | S-8 (`JoinedReadInProgress/cnode-disconnects-mid-callback`) | — | — |
| `ASYNC_NEW_TX`: a savepoint that cannot be created, undone or released | — | — | — |
| Tries made by another pnode: higher `minor` absorbed; second hand-over's `minor = 1` admitted | — | — | S-15 (`Callout_MinorAbsorbedAcrossHandOvers`) |
| A pass refused for an enclosing pair absorbs nothing | — | — | — |
| A pass naming an enclosing callout that is no longer current → refused | S-7 (`NestedCalloutReleased`, the inner cnode's pass) | — | — |
| A Coordinator released by the fence reports `CALLOUT_SUPERSEDED` … | — | — | — |
| The wait cannot deadlock | S-8 (`WaitDoesNotDeadlock`) | — | — |
| Callout ended by a panic → its passes are refused | — | — | — |
| Pass without callout and number → 401; expired pass → 410 | S-10 (live callout, both doors); also F-6 (`NoCalloutAndNumber_401`, `Expired_410`, HTTP door) | — | — |
| Stolen pass presented by another tenant → 403 before any fencing answer | S-10 (current and superseded pass, both doors); also F-6 (`CrossTenant_403` kept) | — | — |
| Pass lifetime follows the answer limit | — | — | — |
| Round robin across two cnodes; a new cnode goes first | S-11 | — | — |
| Two tenants share a tag on one pnode | S-11 | — | S-14 (`Callout_TwoTenantsShareATag`) |
| Import: criterion / function `retryPolicy` invalid → 400 | covered by C-6 | covered by C-9 | — |
| Import: `responseTimeoutMs` over the bound; negative → 400 | covered by C-7 | covered by C-9 | — |
| Stored `responseTimeoutMs` over a lowered bound → `Terminal` | — | — | — |
| Import / export round-trip of `idempotent`, function `retryPolicy`; schema 1.5 | covered by C-8 | covered by C-9 | — |
| Owner's cnode fails → hand-over succeeds | — | — | S-14 (`Callout_HandOverSucceeds`) |
| Hand-over: peer makes two tries in one exchange; honours the owner's answer limit | — | — | S-14 (`Callout_HandOverTwoTriesInOneExchange`) |
| A pnode that receives a hand-over never hands on | — | — | — |
| Hand-over answer lost → one try counted … | — | — | — |
| `triesUsed` out of range → `no_answer` | — | — | — |
| Peer cannot be connected to → no try used, next peer | — | — | waived (footnote 3) |
| No peer can be connected to, no local cnode → 503 | — | — | — |
| Non-2xx / truncated / unauthenticated answer → `no_answer` | — | — | — |
| cnode message and verdict survive the hand-over | — | — | S-14 (`Callout_HandOverCarriesMessageAndVerdict`) |
| Response protection: forged, reflected or replayed answer refused | — | — | — |
| Pass minted by another pnode joins the owner's transaction; refused once the callout ended | — | — | S-15 (`Callout_PassFromAnotherPnode`, both subtests) |
| Many tenants on one pnode stay visible | — | — | covered by M-8 |
| Restart under the same id, with an earlier clock | — | — | — |
| Late joiner / lost list message → fetched | — | — | — |
| Leaving pnode's list dropped | — | — | — |
| No call into memberlist from inside a callback | — | — | — |
| Identity over 512 bytes → refuses to start | — | — | — |
| Unparseable metadata → not alive; routing 503, not 500 | — | — | — |
| Each new or newly validated setting | — | — | — |

The §13 paragraph on concurrency ("two callouts racing on one cnode; attach and
detach during a callout; a callback racing the end of its callout") names no
matrix row. Its third case is S-8's `CallbackPastItsLastCheckLandsWhole`; its
second is S-4's `WaitsForACnode` (attach) and S-3's `AttemptsBeatNoCnode`
(detach by drop); its first — two creates at once against one cnode — is
exercised by S-9's rounds only sequentially, and is left to the unit layer under
`-race` as the paragraph allows ("isolated single-backend e2e **and** unit
tests"). Every such scenario here asserts one outcome and no torn write, never
an order.

## Stream interface summary

**Produced** — test code only; nothing another stream's production code consumes.
- `internal/e2e` (package `e2e_test`): `assertProblem`, `assertEnvelope`, `assertRefusedOnAllDoors`, `supersededDetail`, `procSpec`, `chainWorkflowJSON`, `criterionWorkflowJSON`, `scriptHold`, `calloutTuning`, `(h) countEntities`, `(h) joinedRequest`, `answerMalformedPayload` (S-1); `parseCalloutFailed` (S-3); `awaitCnodeReceived`, `closeOnce`, `awaitCreate` (S-6); `awaitBlockedStatement`, `holdMessagesTableLock`, `(h) seedVictim`, `goJoined`, `awaitResult` (S-8); `passClaims` (S-10).
- `e2e/parity`: six registered scenarios `Callout*` (S-13); `wantParityScenarioCount` +6; the three un-skipped `ExternalAPI_09_09/10/11` (S-12).
- `e2e/parity/multinode`: six registered scenarios `Callout_*`; helpers `mnProc`, `mnWorkflow`, `mnWarmUp`, `mnProblem`, `mnHasRequest` (S-14, S-15).
- One addition to H's e2e harness: the reply `answerMalformedPayload()` (S-1), because no H reply can make a cnode cause a `Terminal`.

**Consumed**
- H: everything in H's interface summary, by the names given there (`newCalloutHarness`, `AttachCnode`, `cnodeSpec`, scripts, replies, `Received`/`ReceivedCallouts`/`AwaitCallouts`, `Replay*`, `createEntityGRPCJoined`, `parity.ComputeClientSpec`, `parity.ComputeBehaviour*`, `parity.StartComputeClientOrSkip`, `parity.AwaitReceived`, `parity.ComputeClientWorkflow`, `parity.ComputeClient{MemberID, Received, Release, Stop}`, `parity.LateCallbackOutcome`, `requireComputeClients`, `stubNoComputeClients`, `multinode.StartComputeClientOrSkip`, `multinode.ComputeClientCapable`, the 200 ms / 2 s patience of H-11, catalog `inject-error-not-retryable`). S uses its own `chainWorkflowJSON` rather than `procWorkflowJSON` wherever a transition needs more than one processor.
- C: `cfg.Callout.FixedNumRetries`, `cfg.Cluster.DispatchWaitTimeout`, `cfg.Cluster.NodeID`; import of `idempotent`, `retryPolicy` on all three kinds, schema `"1.5"`.
- F: `token.Claims{NodeID, TxRef, ExpiresAt int64, Callout, Major, Minor, Outer}`, `(*token.Signer).Issue(token.Claims)`, "`Issue` validates nothing" (F Open point 7); the client-visible text of `CALLOUT_SUPERSEDED` exactly as §8.2 gives it. F's coverage table assigns the E and M cells of its rows to "stream O": they are **this** stream's (S-6 … S-10, S-15).
- L, O, P, M: behaviour only, through the doors. The temporary reverts name `CalloutFailureKind.MayTryAnother`, `grpc.NewCriteriaCallout`/`NewFunctionCallout` (`RepeatSafe`), `RoundRobinSelector.Select`, `MemberRegistry.Candidates`, `RunLocal`'s `ctx.Err()` check (L); the Coordinator's tries resolution, wait, exhaustion/precedence branch and `TryNumberer.Next` (O); `txjoin.JoinFromToken`, `(*Joiner).Run`, `Fence.Begin/Advance/Admit`, the engine's check after the mode switch (F); the peer handler's `RunLocal(…, triesLeft)`, the owner's reading of `member_failed`, the pass's `NodeID` on the peer (P).

## Open points

1. **The scenarios' stacks are not behind the OpenAPI validator, and `api/openapi.yaml` does not describe the callback contract at all.** The assignment says the e2e server records every response through the enforce-mode validator; that is true of the shared `testApp` only (`internal/e2e/e2e_test.go:199-203`). Every callout scenario needs a stack of its own (`newCalloutHarness`), whose handler is `a.Handler()` unwrapped (`callback_harness_test.go:228`; `transaction_control_test.go:203-205` records the same). So no scenario here fails on an undeclared status, and none is needed in `EntityErrorCodeMatrix` (`zzz_errorcode_matrix_test.go`), which is fed by the validated server only. What the check would have found, had it run: `api/openapi.yaml` has **no `410` response anywhere**, no mention of `X-Tx-Token`, `TRANSACTION_EXPIRED`, `TRANSACTION_NOT_FOUND` or `TRANSACTION_NODE_UNAVAILABLE` (`grep -n '"410"\|X-Tx-Token\|TRANSACTION_' api/openapi.yaml` → no hits) — the joined-request surface is undocumented there today, before this feature. With it, every operation behind the join middleware can answer `410 CALLOUT_SUPERSEDED`, and the workflow-running entity operations (`create`, `createCollection`, `updateSingle`, `updateSingleWithLoopback`, `updateCollection`, `patchSingle*`) gain `503 CALLOUT_FAILED` beside the 503 they already declare (the status exists; the code list in its description does not name it). No finished section plans an OpenAPI edit for either (C-4 covers the import fields only; F-1 adds the code and its help topic; `grep -c openapi` over the plan: F 1, D 1, both unrelated). Owner, in my reading: F for `410 CALLOUT_SUPERSEDED` (it defines the code) and O for `503 CALLOUT_FAILED`; describing `X-Tx-Token` and its four pre-existing answers as a shared header/response component is a Gate-6 item that needs a decision on scope — it touches ~60 operations. I have not planned it.
2. **F's Open point 1 and S-9.** F-7 reads the gRPC request message before the lock and notes the pre-lock `RecvMsg` has no bound. `StalledBodyHoldsNothing` leaves one such stream open for the length of the test and cancels it in cleanup; it asserts only that the transaction is not held.
3. **What the owner's loop does when the patience runs out after a pass that made tries.** §5's pseudocode reads "wait for either channel, or the rest of the patience, or ctx / start a new pass", with "if patience used up … → return" at the top of the *next* pass's tail. Read literally, a wait that ends by the patience running out still starts one more pass, which revisits the same cnodes (a new `RunLocal` has an empty tried set). With two silent cnodes and four tries that gives 4 attempts, `[member A: … (2 times)], [member B: … (2 times)]`; read the other way it gives 2. The E scenario avoids the question (tries = 2, S-3); the P scenario cannot set tries, so `CalloutEveryTryUsed` accepts 2…4 and checks the count against the entries. It also weakens the teeth of S-14's two-tries scenario (said there). The O section should state which it is; if it is "one more pass", say so in the help for `CYODA_DISPATCH_WAIT_TIMEOUT`, because a callout then costs a second round of answer limits. Related: the literal rendering of an entry — §8.2 writes `[member<id>: cause]`, R§5 quotes Cloud's with `<id>` as a placeholder — is not fixed anywhere I could find; the parsers accept `member<id>`, `member <id>` and `memberID`. Once O fixes it, tighten the three regexps (S-3, S-13, S-14) to the one form.
4. **Row 1047 (M) uses a retry-until-exercised loop around the random `PeerSelector`.** It never asserts by luck and fails loudly at 2⁻¹⁰, but it is a probabilistic construction in a suite with a known flaky entry (`TestMultiNode`). The alternative is to waive the M cell like footnote 3 and rely on F-2's unit tests. My recommendation is to keep it: it is the only place the absorb-and-reset rule is exercised with two real pnodes minting passes.
5. **`slow-configurable` never sleeps.** `cmd/compute-test-client/catalog.go:107-120` unmarshals `parameters` into a struct, but `parameters` is a JSON *string* holding the context (`callback.go:78-94` decodes it in two steps). `sleep_ms` is therefore always 0, and `e2e/parity/contracts.go:162` carries a TODO that depends on it. S-15 works around it with a stalled `ASYNC_NEW_TX` processor. The fix is two lines (decode the string first) plus a catalog test; it belongs to H's compute-test-client tasks (H-5/H-6) or a one-commit chore — Gate 6 says fix, not list; I did not plan it because the assignment limits this stream to scenario tests.
6. **`holdMessagesTableLock` takes a table lock in the shared container.** `storage_ceilings_e2e_test.go:340-344` rules that out for `entities`. I found no background loop of any stack that touches `messages`, no e2e test runs in parallel, and the lock is held ~2 s; but it is a judgement. If it is unwelcome, the read scenarios (rows 1038, 1045) need another way to hold a joined *read* inside PostgreSQL, and I know of none that is deterministic (a row lock does not block a plain `SELECT`).
7. **Row 1033's E scenario observes an absence over a fixed interval** ("the second cnode has not been given the work 500 ms after the first's answer limit passed, while the database lock is still held"). It cannot fail spuriously — the lock is released only afterwards — but on a very slow runner it could pass without having waited long enough to catch a regression. The same holds for the four other `time.Sleep` observations in S-8. The revert in each task's Step 2 is what shows they bite on the machine that runs them.
8. **`COMMIT_BEFORE_DISPATCH` as the inner processor of a nested callout is not covered** (S-7 says why): it would pin the separately filed defect. Row 1035 says "every processor mode"; S-7 covers SYNC, ASYNC_SAME_TX and ASYNC_NEW_TX. When the defect is fixed, add the fourth case there.
9. **Scheduled fire (row 1027) asserts the positive path only** — silent cnode, then the second answers, one request id. A failing fire has no client to assert on, and what the scheduler does next (§17, the 30 s throttle) is deliberately not this design's. `first.Received()` may exceed 1 if the fire is re-dispatched; the scenario asserts `>= 1`.

