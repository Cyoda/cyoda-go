package e2e_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/app"
)

// callout_modes_test.go covers spec §8.1's processor-mode table (whether a
// failed callout fails the operation, and what cyoda's state looks like
// afterward) and §17's "a scheduled fire follows the same callout rules".

// TestCalloutModes_AsyncNewTx: a failed callout does not fail the operation
// and nothing of it reaches the client; the rules for asking another cnode
// are the same as in every other mode.
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
			if got := first.Received(); len(got) != 1 {
				t.Errorf("first cnode received %d callouts; want exactly 1", len(got))
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
				if got := first.Received(); len(got) != 1 {
					t.Errorf("first cnode received %d callouts; want exactly 1", len(got))
				}
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
	// testApp's own scheduler is disabled (internal/e2e/e2e_test.go's
	// TestMain), so this harness's own scan of its own stack is the only
	// scanner that can ever see this fire's due row.
	h := newCalloutHarness(t, func(cfg *app.Config) {
		calloutTuning(3, 100*time.Millisecond)(cfg)
		cfg.Scheduler.ScanInterval = 50 * time.Millisecond
	})
	suffix := uuid.NewString()
	tag, model := "s5-sched-"+suffix, "s5-model-sched-"+suffix
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
	deadline := time.Now().Add(scheduledFireTimeout)
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
	if len(got1) != 1 || len(got2) != 1 {
		t.Fatalf("first received %d, second %d; want exactly 1 each (the silent cnode tried once, the second answers once)", len(got1), len(got2))
	}
	if got1[0].RequestID != got2[0].RequestID {
		t.Errorf("request ids differ across the tries of one fire: %q then %q", got1[0].RequestID, got2[0].RequestID)
	}
}
