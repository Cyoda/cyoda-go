package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

// callout_harness_selftest_test.go holds the self-tests of the callout test
// harness itself: each proves one harness capability against the running
// stack, so a scenario built on the harness cannot pass or fail because of
// the harness.

// TestCalloutHarness_StartsWithNoCnode proves newCalloutHarness attaches no
// cnode and that the gRPC API is reachable without one.
func TestCalloutHarness_StartsWithNoCnode(t *testing.T) {
	h := newCalloutHarness(t, nil)

	if h.member != nil {
		t.Fatal("newCalloutHarness attached a default cnode; it must attach none")
	}
	if n := len(h.app.MemberRegistry().List()); n != 0 {
		t.Fatalf("member registry holds %d cnodes on a fresh callout harness; want 0", n)
	}

	const model = "h1-no-cnode"
	h.SetupModelWithWorkflow(t, model, secondaryWorkflow)
	env, err := h.createEntityGRPC(model, 1, workflowSampleModel)
	if err != nil {
		t.Fatalf("gRPC create over the harness API connection: %v", err)
	}
	if !env.Success {
		code := ""
		if env.Error != nil {
			code = env.Error.Code + ": " + env.Error.Message
		}
		t.Fatalf("gRPC create over the harness API connection failed: %s", code)
	}
}

// TestComputeMember_JoinsWithGivenTagsAndBearer proves a cnode joins under the
// tags and the tenant it was given, and learns its member id from the greet.
func TestComputeMember_JoinsWithGivenTagsAndBearer(t *testing.T) {
	h := newCalloutHarness(t, nil)

	own := newComputeMember(t, h, memberSpec{tags: []string{"h2-a", "h2-b"}, handle: func(*computeMember, func(*cepb.CloudEvent) error, calcRequest) {}})
	t.Cleanup(own.stop)
	if own.id == "" {
		t.Fatal("member id from the greet is empty")
	}
	got := h.app.MemberRegistry().Get(own.id)
	if got == nil {
		t.Fatalf("registry has no member %s", own.id)
	}
	if !slices.Equal(got.Tags, []string{"h2-a", "h2-b"}) {
		t.Errorf("tags = %v; want [h2-a h2-b]", got.Tags)
	}
	if string(got.TenantID) != "test-tenant" {
		t.Errorf("tenant = %q; want test-tenant", got.TenantID)
	}

	clientID, secret := h.provisionTenant(t, "h2-other-tenant", "h2-user")
	other := newComputeMember(t, h, memberSpec{
		bearer: h.fetchTokenFor(t, clientID, secret),
		tags:   []string{"h2-a"},
		handle: func(*computeMember, func(*cepb.CloudEvent) error, calcRequest) {},
	})
	t.Cleanup(other.stop)
	if m := h.app.MemberRegistry().Get(other.id); m == nil || string(m.TenantID) != "h2-other-tenant" {
		t.Errorf("second cnode did not join under the bearer's tenant: %+v", m)
	}
}

// TestCnodeReply_CloudEvent pins the wire shape of every reply a cnode can give.
func TestCnodeReply_CloudEvent(t *testing.T) {
	cases := []struct {
		name     string
		kind     string
		reply    cnodeReply
		wantType string
		want     string // the reply payload, as JSON; "" = nothing is sent
	}{
		{"processor ok, unchanged", calloutProcessor, answerOK(), internalgrpc.EntityProcessorCalculationResponse,
			`{"requestId":"r-1","success":true}`},
		{"processor ok, data", calloutProcessor, answerData(map[string]any{"k": "v"}), internalgrpc.EntityProcessorCalculationResponse,
			`{"requestId":"r-1","success":true,"payload":{"data":{"k":"v"}}}`},
		{"criterion ok", calloutCriterion, answerMatches(true), internalgrpc.EntityCriteriaCalculationResponse,
			`{"requestId":"r-1","success":true,"matches":true}`},
		{"function ok", calloutFunction, answerResult("Schedule", map[string]any{"fireAfterMs": 5}), internalgrpc.EntityFunctionCalculationResponse,
			`{"requestId":"r-1","success":true,"resultKind":"Schedule","result":{"fireAfterMs":5}}`},
		{"failure, no verdict", calloutProcessor, answerFail("boom"), internalgrpc.EntityProcessorCalculationResponse,
			`{"requestId":"r-1","success":false,"error":{"message":"boom"}}`},
		{"failure, retryable", calloutCriterion, answerFailVerdict("boom", true), internalgrpc.EntityCriteriaCalculationResponse,
			`{"requestId":"r-1","success":false,"error":{"message":"boom","retryable":true}}`},
		{"failure, not retryable", calloutFunction, answerFailVerdict("boom", false), internalgrpc.EntityFunctionCalculationResponse,
			`{"requestId":"r-1","success":false,"error":{"message":"boom","retryable":false}}`},
		{"malformed payload", calloutProcessor, answerMalformedPayload(), internalgrpc.EntityProcessorCalculationResponse,
			`{"requestId":"r-1","success":true,"payload":"not-an-object"}`},
		{"never answer", calloutProcessor, neverAnswer(), "", ""},
		{"close stream", calloutProcessor, closeStream(), "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ce, err := tc.reply.cloudEvent(calcRequest{kind: tc.kind, replyID: "r-1"})
			if err != nil {
				t.Fatalf("cloudEvent: %v", err)
			}
			if tc.want == "" {
				if ce != nil {
					t.Fatalf("reply sends %s; want nothing sent", ce.GetType())
				}
				return
			}
			if ce.GetType() != tc.wantType {
				t.Errorf("type = %s; want %s", ce.GetType(), tc.wantType)
			}
			var got, want any
			if err := json.Unmarshal([]byte(ce.GetTextData()), &got); err != nil {
				t.Fatalf("reply payload is not JSON: %v", err)
			}
			_ = json.Unmarshal([]byte(tc.want), &want)
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(want)
			if string(gotJSON) != string(wantJSON) {
				t.Errorf("payload = %s; want %s", gotJSON, wantJSON)
			}
		})
	}
}

// TestScriptedCnodes_EachReceivesOnlyItsTag: two cnodes with different tags
// each receive only their tag's callout, and the harness records request id,
// CloudEvent id, pass and arrival order.
func TestScriptedCnodes_EachReceivesOnlyItsTag(t *testing.T) {
	h := newCalloutHarness(t, nil)
	a := h.AttachCnode(t, cnodeSpec{name: "a", tags: []string{"h3-tag-a"}})
	b := h.AttachCnode(t, cnodeSpec{name: "b", tags: []string{"h3-tag-b"}})

	h.SetupModelWithWorkflow(t, "h3-model-a", procWorkflowJSON("h3-wf-a", "h3-proc-a", "SYNC",
		map[string]any{"calculationNodesTags": "h3-tag-a"}))
	h.SetupModelWithWorkflow(t, "h3-model-b", procWorkflowJSON("h3-wf-b", "h3-proc-b", "SYNC",
		map[string]any{"calculationNodesTags": "h3-tag-b"}))

	for _, model := range []string{"h3-model-a", "h3-model-b"} {
		if _, status, body := h.CreateEntity(t, model, 1, workflowSampleModel); status != http.StatusOK {
			t.Fatalf("create %s: %d %s", model, status, body)
		}
	}

	recs := h.ReceivedCallouts()
	if len(recs) != 2 {
		t.Fatalf("recorded %d callouts; want 2: %v", len(recs), recs)
	}
	want := []struct {
		cnode    *scriptedCnode
		name     string
		procName string
	}{{a, "a", "h3-proc-a"}, {b, "b", "h3-proc-b"}}
	for i, w := range want {
		r := recs[i]
		if r.Seq != i+1 || r.Cnode != w.name || r.Name != w.procName || r.Kind != calloutProcessor {
			t.Errorf("record %d = %v; want seq %d on cnode %s for %s", i, r, i+1, w.name, w.procName)
		}
		if r.MemberID != w.cnode.MemberID() || r.MemberID == "" {
			t.Errorf("record %d member id = %q; want %q", i, r.MemberID, w.cnode.MemberID())
		}
		if r.RequestID == "" || r.EventID == "" || r.EntityID == "" {
			t.Errorf("record %d is missing an id: %v", i, r)
		}
		if r.Pass() == "" {
			t.Errorf("record %d carries no pass; a SYNC processor callout always has one", i)
		}
		if got := w.cnode.Received(); len(got) != 1 || got[0].Seq != r.Seq {
			t.Errorf("cnode %s Received() = %v; want exactly record %d", w.name, got, i)
		}
	}
	if s := fmt.Sprintf("%v %+v %#v", recs[0], recs[0], recs[0]); strings.Contains(s, recs[0].Pass()) {
		t.Error("formatting a receivedCallout prints the pass")
	}
}

// TestScriptedCnode_Replies: each scripted reply reaches the client as the
// status and code today's single-shot dispatch gives it.
func TestScriptedCnode_Replies(t *testing.T) {
	h := newCalloutHarness(t, nil)
	cases := []struct {
		name       string
		reply      cnodeReply
		timeoutMs  int
		wantStatus int
		wantCode   string
		wantInBody string
	}{
		{"ok", answerOK(), 0, http.StatusOK, "", ""},
		{"fail", answerFail("h3 boom"), 0, http.StatusBadRequest, "WORKFLOW_FAILED", "h3 boom"},
		{"fail-verdict", answerFailVerdict("h3 verdict boom", true), 0, http.StatusBadRequest, "WORKFLOW_FAILED", "h3 verdict boom"},
		{"never-answer", neverAnswer(), 300, http.StatusServiceUnavailable, "DISPATCH_TIMEOUT", ""},
		{"close-stream", closeStream(), 0, http.StatusServiceUnavailable, "COMPUTE_MEMBER_DISCONNECTED", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tag, model := "h3-r-"+tc.name, "h3-replies-"+tc.name
			c := h.AttachCnode(t, cnodeSpec{name: tc.name, tags: []string{tag}, script: scriptAlways(tc.reply)})
			defer c.Detach(t)

			cfg := map[string]any{"calculationNodesTags": tag}
			if tc.timeoutMs > 0 {
				cfg["responseTimeoutMs"] = tc.timeoutMs
			}
			h.SetupModelWithWorkflow(t, model, procWorkflowJSON("h3-replies-wf-"+tc.name, "h3-proc", "SYNC", cfg))

			_, status, body := h.CreateEntity(t, model, 1, workflowSampleModel)
			if status != tc.wantStatus {
				t.Fatalf("status = %d; want %d (body: %s)", status, tc.wantStatus, body)
			}
			if tc.wantCode != "" {
				if code := problemErrorCode(body); code != tc.wantCode {
					t.Errorf("errorCode = %q; want %q (body: %s)", code, tc.wantCode, body)
				}
			}
			if tc.wantInBody != "" && !strings.Contains(body, tc.wantInBody) {
				t.Errorf("body does not carry the cnode's message %q: %s", tc.wantInBody, body)
			}
			if got := c.Received(); len(got) != 1 {
				t.Errorf("cnode received %d callouts; want 1", len(got))
			}
		})
	}
}

// TestScriptedCnode_AttachAndDetachMidTest: no cnode -> refused; attach ->
// served; detach -> refused again. Detach returns only once the server has
// dropped the member, so the third step needs no polling.
func TestScriptedCnode_AttachAndDetachMidTest(t *testing.T) {
	h := newCalloutHarness(t, nil)
	const model, tag = "h3-attach-detach", "h3-ad"
	h.SetupModelWithWorkflow(t, model, procWorkflowJSON("h3-ad-wf", "h3-proc", "SYNC",
		map[string]any{"calculationNodesTags": tag}))

	expectNoCnode := func(step string) {
		t.Helper()
		_, status, body := h.CreateEntity(t, model, 1, workflowSampleModel)
		if status != http.StatusServiceUnavailable || problemErrorCode(body) != "NO_COMPUTE_MEMBER_FOR_TAG" {
			t.Fatalf("%s: got %d %s; want 503 NO_COMPUTE_MEMBER_FOR_TAG", step, status, body)
		}
	}

	expectNoCnode("before any cnode attached")

	c := h.AttachCnode(t, cnodeSpec{name: "late", tags: []string{tag}})
	if _, status, body := h.CreateEntity(t, model, 1, workflowSampleModel); status != http.StatusOK {
		t.Fatalf("with the cnode attached: %d %s", status, body)
	}

	c.Detach(t)
	if m := h.app.MemberRegistry().Get(c.MemberID()); m != nil {
		t.Fatal("Detach returned while the server still holds the member")
	}
	expectNoCnode("after Detach")
	c.Detach(t) // idempotent
}

// TestScriptedCnode_RecordsCriterionAndFunction: the recorder names the kind
// of every callout, not only processors, and scriptSequence hands out replies
// in order.
func TestScriptedCnode_RecordsCriterionAndFunction(t *testing.T) {
	h := newCalloutHarness(t, nil)
	const tag = "h3-kinds"
	h.AttachCnode(t, cnodeSpec{name: "kinds", tags: []string{tag}, script: scriptSequence(
		answerMatches(true),
		answerResult("Schedule", map[string]any{"fireAfterMs": int64(3600000)}),
	)})

	critWF := fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "h3-crit-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "DONE", "manual": false,
					"criterion": {"type": "function", "function": {"name": "h3-crit",
						"config": {"calculationNodesTags": %q}}}}]},
				"DONE": {}
			}
		}]
	}`, tag)
	h.SetupModelWithWorkflow(t, "h3-kinds-crit", critWF)
	h.SetupModelWithWorkflow(t, "h3-kinds-fn", scheduleFunctionWorkflowJSON("h3-fn-wf",
		fmt.Sprintf(`{"name":"h3-fn","resultKind":"Schedule","calculationNodesTags":%q}`, tag)))

	critID, status, body := h.CreateEntity(t, "h3-kinds-crit", 1, workflowSampleModel)
	if status != http.StatusOK {
		t.Fatalf("create behind a criterion: %d %s", status, body)
	}
	if st, _ := h.GetEntityState(t, critID); st != "DONE" {
		t.Errorf("state = %q; want DONE (the scripted criterion matched)", st)
	}
	if _, status, body := h.CreateEntity(t, "h3-kinds-fn", 1, workflowSampleModel); status != http.StatusOK {
		t.Fatalf("create arming a schedule function: %d %s", status, body)
	}

	recs := h.AwaitCallouts(t, 2, 5*time.Second)
	if recs[0].Kind != calloutCriterion || recs[0].Name != "h3-crit" {
		t.Errorf("first record = %v; want criterion h3-crit", recs[0])
	}
	if recs[1].Kind != calloutFunction || recs[1].Name != "h3-fn" {
		t.Errorf("second record = %v; want function h3-fn", recs[1])
	}
}

// TestScriptedCnode_LateCallbackAndReplay: a cnode makes a callback with the
// pass it was given only after the test says so, and the recorded pass can be
// presented again on the HTTP door and on the gRPC door.
func TestScriptedCnode_LateCallbackAndReplay(t *testing.T) {
	h := newCalloutHarness(t, nil)
	const primary, secondary, tag = "h4-primary", "h4-secondary", "h4-tag"
	const child = `{"name":"child","amount":1,"status":"h4"}`
	h.SetupModelWithWorkflow(t, secondary, secondaryWorkflow)
	h.SetupModelWithWorkflow(t, primary, procWorkflowJSON("h4-wf", "h4-proc", "SYNC",
		map[string]any{"calculationNodesTags": tag}))

	release := make(chan struct{})
	inCallout := make(chan callbackResult, 1)
	h.AttachCnode(t, cnodeSpec{name: "late", tags: []string{tag}, script: scriptLateCallback(release,
		func(rc *reqCtx) {
			res, err := rc.CreateEntity(secondary, 1, child)
			if err != nil {
				res = callbackResult{StatusCode: -1, Body: err.Error()}
			}
			inCallout <- res
		}, answerOK())})

	created := make(chan createEntityResult, 1)
	go func() { created <- h.CreateEntityRaw(primary, 1, workflowSampleModel) }()

	recs := h.AwaitCallouts(t, 1, 10*time.Second)
	select {
	case res := <-inCallout:
		t.Fatalf("the cnode called back before the test released it: %d", res.StatusCode)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)

	if res := <-inCallout; res.StatusCode != http.StatusOK {
		t.Fatalf("callback made during the callout: %d %s; want 200 (it joins the open transaction)", res.StatusCode, res.Body)
	}
	if res := <-created; res.err != nil || res.status != http.StatusOK {
		t.Fatalf("primary create: status=%d err=%v body=%s", res.status, res.err, res.body)
	}

	// The callout has ended and its transaction is committed. The same pass,
	// presented again, is evaluated and refused on both doors.
	pass := recs[0].Pass()
	httpRes, err := h.ReplayCreateHTTP(pass, secondary, 1, child)
	if err != nil {
		t.Fatalf("ReplayCreateHTTP: %v", err)
	}
	if httpRes.StatusCode == http.StatusOK {
		t.Error("HTTP door accepted a pass whose callout has ended")
	}
	if got, err := h.ReplayGetHTTP(pass, recs[0].EntityID); err != nil || got.StatusCode == http.StatusOK {
		t.Errorf("HTTP read with the ended pass: status=%d err=%v; want a refusal", got.StatusCode, err)
	}
	grpcRes, err := h.ReplayCreateGRPC(pass, secondary, 1, child)
	if err != nil {
		t.Fatalf("ReplayCreateGRPC: %v", err)
	}
	if grpcRes.Success {
		t.Error("gRPC door accepted a pass whose callout has ended")
	}
	if got, err := h.ReplayGetGRPC(pass, recs[0].EntityID); err != nil || got.Success {
		t.Errorf("gRPC read with the ended pass: success=%t err=%v; want a refusal", got.Success, err)
	}

	// Control: the same requests without a pass succeed, so the refusals above
	// are about the pass and nothing else.
	if res, err := h.callback(http.MethodPost, "/api/entity/JSON/"+secondary+"/1", child, ""); err != nil || res.StatusCode != http.StatusOK {
		t.Errorf("unjoined HTTP create: status=%d err=%v", res.StatusCode, err)
	}
	if env, err := h.createEntityGRPC(secondary, 1, child); err != nil || !env.Success {
		t.Errorf("unjoined gRPC create: success=%t err=%v", env.Success, err)
	}

	if _, err := h.ReplayCreateHTTP("", secondary, 1, child); err == nil {
		t.Error("ReplayCreateHTTP accepted an empty pass; it must refuse, or a test could pass unjoined")
	}
}
