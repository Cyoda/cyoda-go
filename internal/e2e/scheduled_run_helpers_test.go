package e2e_test

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
)

// scheduled_run_helpers_test.go — helpers the scheduled-run scenarios share.
// The endings and segments scenarios, written on a parallel branch, define
// the same four with the same text; when both branches merge, one copy stays.

// createOpen creates one entity of model and asserts 200.
func createOpen(t *testing.T, h *callbackHarness, model, payload string) string {
	t.Helper()
	id, status, body := h.CreateEntity(t, model, 1, payload)
	if status != http.StatusOK {
		t.Fatalf("create: %d %s", status, body)
	}
	return id
}

// requireState asserts the entity's state.
func requireState(t *testing.T, h *callbackHarness, id, want string) {
	t.Helper()
	if st, _ := h.GetEntityState(t, id); st != want {
		t.Errorf("state = %q; want %q", st, want)
	}
}

// holdScript signals gotWork on the first callout and holds it until release
// is closed; later callouts are answered at once.
func holdScript(gotWork chan<- struct{}, release <-chan struct{}) cnodeScript {
	var calls atomic.Int32
	return func(ctx context.Context, _ receivedCallout, _ *reqCtx) cnodeReply {
		if calls.Add(1) > 1 {
			return answerOK()
		}
		gotWork <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return neverAnswer()
		}
		return answerOK()
	}
}

// mustTask reads the task and fails the test if there is none.
func mustTask(t *testing.T, s *schedDB, id string) taskRow {
	t.Helper()
	r, ok := s.task(t, id, "Fire")
	if !ok {
		t.Fatalf("no task for %s", id)
	}
	return r
}
