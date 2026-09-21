package e2e_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// callout_patience_test.go covers the patience a callout owes a cnode that
// has not attached yet (spec §5 "Patience") and the two ways a caller can end
// the wait itself: its own deadline (408) and going away outright.

// TestCalloutPatience_WaitsForACnode: with no cnode attached the callout
// waits; one attaches; the operation succeeds. One try is all that is
// allowed, so the wait itself used none. The same with retryPolicy NONE.
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
// during a wait or during a try, and a caller that goes away ends the
// callout: no cnode is given the work afterwards.
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
