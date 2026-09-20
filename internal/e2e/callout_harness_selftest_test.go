package e2e_test

import (
	"encoding/json"
	"slices"
	"testing"

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

	own := newComputeMember(t, h, memberSpec{tags: []string{"h2-a", "h2-b"}, handle: h.handleRegistered})
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
		handle: h.handleRegistered,
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
