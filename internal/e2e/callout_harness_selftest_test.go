package e2e_test

import (
	"testing"
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
