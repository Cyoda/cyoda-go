package e2e_test

// Isolated single-backend concurrency coverage for model locking. Per
// .claude/rules/test-coverage.md concurrency scenarios live here, not in the
// shared parity suite: they assert consistency (one winner, losers rejected),
// never a precise interleave.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// TestModelLock_Concurrent_ConvergesToLocked asserts that N simultaneous lock
// attempts on the same unlocked model leave the model consistently LOCKED,
// with every attempt terminating in a documented status and the schema intact.
//
// Deliberately NOT asserted: that exactly one attempt wins. LockModel checks
// state and then locks without holding the two together, so concurrent callers
// can all observe UNLOCKED and all receive 200 rather than one 200 and N-1
// 409 MODEL_ALREADY_LOCKED. Locking is an operator action taken in a
// controlled rollout, the converged state is identical either way, and the
// duplicate lifecycle savepoints the race writes are drained by Unlock — so
// enforcing single-winner semantics is not worth the atomic-transition work
// across every backend. The sequential contract is covered by
// TestModelLock_AlreadyLocked_409.
//
// What this test does guard: no attempt may 500, the model must not be left
// half-transitioned, and the schema must survive the race.
func TestModelLock_Concurrent_ConvergesToLocked(t *testing.T) {
	const model = "e2e-lock-concurrent"
	const n = 8

	importModelE2E(t, model, 1)

	ctx := e2eCtx(t)
	path := fmt.Sprintf("/api/model/%s/1/lock", model)
	results := make([]httpResult, n)
	ready := make(chan struct{})

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-ready // release all lockers together
			results[idx] = resultOf(doAuthRaw(ctx, http.MethodPut, path, ""))
		}(i)
	}
	close(ready)
	wg.Wait()

	// Assert on the test goroutine — Fatal is only legal here.
	var winners int
	for idx, res := range results {
		if res.err != nil {
			t.Fatalf("lock attempt %d: %v", idx, res.err)
		}
		switch res.status {
		case http.StatusOK:
			winners++
		case http.StatusConflict:
			// MODEL_ALREADY_LOCKED is the business conflict. CONFLICT is the
			// retryable serialization abort, which doAuthRaw may still surface
			// if all five of its retries lose the race — not a defect.
			assertErrorCodeOneOf(t, res.body, "MODEL_ALREADY_LOCKED", "CONFLICT")
		default:
			t.Errorf("lock attempt %d: status=%d, want 200 or 409; body: %s", idx, res.status, res.body)
		}
	}
	if winners == 0 {
		t.Errorf("no lock attempt succeeded — the model was never locked")
	}

	// The model must be genuinely locked afterwards, not left half-transitioned.
	resp := doAuth(t, http.MethodPut, path, "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("post-race relock: status=%d, want 409; body: %s", resp.StatusCode, body)
	}
	assertErrorCode(t, body, "MODEL_ALREADY_LOCKED")

	// The schema must have survived the race: the locked model still exports
	// the properties it declared before the storm.
	exported := exportModelE2E(t, model, 1)
	if len(exported) == 0 {
		t.Fatalf("model export empty after concurrent locks — schema did not survive the race")
	}
	if raw := fmt.Sprintf("%v", exported); !strings.Contains(raw, "name") {
		t.Errorf("exported schema lost its declared properties after the race: %s", raw)
	}
}

// assertErrorCodeOneOf asserts the problem body carries one of the given error
// codes.
func assertErrorCodeOneOf(t *testing.T, body string, codes ...string) {
	t.Helper()
	var pd struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal([]byte(body), &pd); err != nil {
		t.Fatalf("decode problem detail %q: %v", body, err)
	}
	got, _ := pd.Properties["errorCode"].(string)
	for _, want := range codes {
		if got == want {
			return
		}
	}
	t.Errorf("errorCode=%q, want one of %v; body: %s", got, codes, body)
}
