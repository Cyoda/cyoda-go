package e2e_test

// unique_keys_commit_time_test.go — unique keys are checked when the
// transaction commits, on its final entities, with the stored model's keys.
// Through the full HTTP and gRPC stack on postgres, a joined callback's write
// whose entity duplicates a key, or whose processors' returned data leaves a
// partial key, is answered 2xx; the owner's commit is refused (409
// UNIQUE_VIOLATION / 422 INVALID_UNIQUE_KEY).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

// ukctKeyedModel imports model from workflowSampleModel, declares keysJSON on
// it, locks it and imports workflow wf.
func ukctKeyedModel(t *testing.T, h *callbackHarness, model, keysJSON, wf string) {
	t.Helper()
	resp := h.DoAuth(t, http.MethodPost, fmt.Sprintf("/api/model/import/JSON/SAMPLE_DATA/%s/1", model), workflowSampleModel, "")
	if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("import model %s: %d %s", model, resp.StatusCode, body)
	}
	resp = h.DoAuth(t, http.MethodPut, fmt.Sprintf("/api/model/%s/1/unique-keys", model), keysJSON, "")
	if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("unique keys of %s: %d %s", model, resp.StatusCode, body)
	}
	resp = h.DoAuth(t, http.MethodPut, fmt.Sprintf("/api/model/%s/1/lock", model), "", "")
	if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("lock model %s: %d %s", model, resp.StatusCode, body)
	}
	resp = h.DoAuth(t, http.MethodPost, fmt.Sprintf("/api/model/%s/1/workflow/import", model), wf, "")
	if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("import workflow %s: %d %s", model, resp.StatusCode, body)
	}
}

// ukctNameKey is a unique key on $.name.
const ukctNameKey = `{"uniqueKeys":[{"id":"name-key","fields":["$.name"]}]}`

// ukctDup is the key value the duplicate tests collide on.
const ukctDup = `{"name":"dup","amount":1,"status":"draft"}`

// requireModelState fails unless model's state is want.
func (h *callbackHarness) requireModelState(t *testing.T, model, want string) {
	t.Helper()
	var list []struct {
		ModelName    string `json:"modelName"`
		CurrentState string `json:"currentState"`
	}
	resp := h.DoAuth(t, http.MethodGet, "/api/model/", "", "")
	body := h.readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/model/: %d %s", resp.StatusCode, body)
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("decode model list: %v (body: %s)", err, body)
	}
	for _, m := range list {
		if m.ModelName == model {
			if m.CurrentState != want {
				t.Errorf("model %s is %s; want %s", model, m.CurrentState, want)
			}
			return
		}
	}
	t.Fatalf("model %s not listed: %s", model, body)
}

// joinedDuplicate runs the joined-callback duplicate scenario: entity X of
// target K holds name dup; an owner create's SYNC processor writes, joined
// through write, a second K entity named dup and records what write answered.
// It returns the owner's answer and the write's record.
func joinedDuplicate[R any](t *testing.T, prefix string, write func(rc *reqCtx, model string) (R, error)) (h *callbackHarness, target, owner string, status int, body string, res R) {
	t.Helper()
	h = newCallbackHarness(t)
	proc := prefix + "-proc"
	target, owner = uniq(prefix+"-target"), uniq(prefix+"-owner")
	type record struct {
		res R
		err error
	}
	done := make(chan record, 1)
	h.RegisterProc(proc, func(rc *reqCtx) (map[string]any, error) {
		var r record
		r.res, r.err = write(rc, target)
		done <- r
		return nil, nil
	})
	ukctKeyedModel(t, h, target, ukctNameKey, chainWorkflowJSON(prefix+"-target-wf"))
	h.SetupModelWithWorkflow(t, owner, syncProcWorkflow(proc))

	if _, st, b := h.CreateEntity(t, target, 1, ukctDup); st != http.StatusOK {
		t.Fatalf("create X: %d %s", st, b)
	}
	_, status, body = h.CreateEntity(t, owner, 1, workflowSampleModel)
	r := awaitOutcome(t, done)
	if r.err != nil {
		t.Fatalf("joined write: transport error: %v", r.err)
	}
	return h, target, owner, status, body, r.res
}

// TestUniqueKeys_JoinedCallbackDuplicate_OwnerCommit409: a joined HTTP create
// duplicates a committed key value. The callback is answered 200; the owner's
// commit is refused 409 UNIQUE_VIOLATION, and nothing it wrote is stored.
func TestUniqueKeys_JoinedCallbackDuplicate_OwnerCommit409(t *testing.T) {
	h, target, owner, status, body, cb := joinedDuplicate(t, "ukct-dup", func(rc *reqCtx, model string) (callbackResult, error) {
		return rc.CreateEntity(model, 1, ukctDup)
	})
	if cb.StatusCode != http.StatusOK {
		t.Errorf("joined create of a duplicate: %d %s; want 200 (keys are checked at the owner's commit)", cb.StatusCode, cb.Body)
	}
	assertProblem(t, status, body, http.StatusConflict, "UNIQUE_VIOLATION", false)
	if n := h.countEntities(t, target); n != 1 {
		t.Errorf("target has %d entities; want 1 (only X)", n)
	}
	if n := h.countEntities(t, owner); n != 0 {
		t.Errorf("owner model has %d entities; want 0", n)
	}
}

// TestUniqueKeys_JoinedCallbackDuplicate_GRPCTxToken_OwnerCommit409 is the
// same scenario with the joined create sent over gRPC with the tx-token: the
// envelope reports success; the owner's commit is refused 409
// UNIQUE_VIOLATION.
func TestUniqueKeys_JoinedCallbackDuplicate_GRPCTxToken_OwnerCommit409(t *testing.T) {
	h, target, owner, status, body, env := joinedDuplicate(t, "ukct-dup-grpc", func(rc *reqCtx, model string) (txEnvelope, error) {
		return rc.h.createEntityGRPCJoined(model, 1, ukctDup, rc.token)
	})
	if !env.Success {
		t.Errorf("joined gRPC create of a duplicate: %s; want success (keys are checked at the owner's commit)", grpcJoinedOutcome(env, nil).detail)
	}
	assertProblem(t, status, body, http.StatusConflict, "UNIQUE_VIOLATION", false)
	if n := h.countEntities(t, target); n != 1 {
		t.Errorf("target has %d entities; want 1 (only X)", n)
	}
	if n := h.countEntities(t, owner); n != 0 {
		t.Errorf("owner model has %d entities; want 0", n)
	}
}

// TestUniqueKeys_CallbackProcessorPartialKey_OwnerCommit422: a joined create
// whose body states the whole composite key runs a processor that returns
// data without one of the key's fields. The callback is answered 200; the
// owner's commit is refused 422 INVALID_UNIQUE_KEY, and nothing is stored.
func TestUniqueKeys_CallbackProcessorPartialKey_OwnerCommit422(t *testing.T) {
	h := newCallbackHarness(t)
	const driver, partial = "ukct-partial-driver", "ukct-partial-proc"
	target, owner := uniq("ukct-partial-target"), uniq("ukct-partial-owner")

	h.RegisterProc(partial, func(rc *reqCtx) (map[string]any, error) {
		return map[string]any{"name": "p", "status": "draft"}, nil
	})
	type record struct {
		res callbackResult
		err error
	}
	done := make(chan record, 1)
	h.RegisterProc(driver, func(rc *reqCtx) (map[string]any, error) {
		res, err := rc.CreateEntity(target, 1, `{"name":"p","amount":1,"status":"draft"}`)
		done <- record{res, err}
		return nil, nil
	})
	ukctKeyedModel(t, h, target, `{"uniqueKeys":[{"id":"name-amount","fields":["$.name","$.amount"]}]}`, syncProcWorkflow(partial))
	h.SetupModelWithWorkflow(t, owner, syncProcWorkflow(driver))

	_, status, body := h.CreateEntity(t, owner, 1, workflowSampleModel)
	r := awaitOutcome(t, done)
	if r.err != nil || r.res.StatusCode != http.StatusOK {
		t.Errorf("joined create: err=%v %d %s; want 200 (keys are checked at the owner's commit)", r.err, r.res.StatusCode, r.res.Body)
	}
	assertProblem(t, status, body, http.StatusUnprocessableEntity, "INVALID_UNIQUE_KEY", false)
	if n := h.countEntities(t, target); n != 0 {
		t.Errorf("target has %d entities; want 0", n)
	}
	if n := h.countEntities(t, owner); n != 0 {
		t.Errorf("owner model has %d entities; want 0", n)
	}
}
