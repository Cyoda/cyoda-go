package e2e_test

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"
)

// scheduled_run_race_test.go is a soak of client writes against a live
// scheduler. It runs on a stack and database of its own, never in the shared
// parity suite: a goroutine storm there would destabilise the other
// scenarios. It asserts consistency only — no 5xx, no lost timer, a loser gets
// a retryable 409 — and never a precise interleave.

// latestFire returns the recording time of the entity's newest
// SCHEDULED_TRANSITION_FIRE event, and false when it has none.
func latestFire(t *testing.T, h *callbackHarness, id string) (time.Time, bool) {
	t.Helper()
	fires := smEventsOfType(schedEvents(t, h, id), "SCHEDULED_TRANSITION_FIRE")
	if len(fires) == 0 {
		return time.Time{}, false
	}
	return smEventTime(t, fires[len(fires)-1]), true
}

// TestSchedRace_ClientWritesAgainstLiveScheduler: eight entities carry a
// self-loop timer that fires every 50ms, so the scheduler is always claiming,
// firing and re-arming their task rows. Four writers update the entities for
// five seconds, then every entity is deleted while its timer still runs.
// Every answer is 200 or a retryable 409 — never a 5xx, never a
// non-retryable 409. No timer is lost: after the writers stop, each entity
// has exactly one task and fires again. A delete that answers 200 leaves no
// task.
func TestSchedRace_ClientWritesAgainstLiveScheduler(t *testing.T) {
	h, s := newSchedulerHarness(t, nil)
	model := uniq("sx-race")
	h.SetupModelWithWorkflow(t, model, schedDoc("sx-race-wf", map[string]any{
		"Open": map[string]any{"transitions": []any{map[string]any{
			"name": "Tick", "next": "Open", "manual": false, "schedule": map[string]any{"delayMs": 50},
			// A guard the import requires of a self-loop; every write keeps it true.
			"criterion": map[string]any{"type": "simple", "jsonPath": "$.status", "operatorType": "EQUALS", "value": "draft"},
		}}},
	}))
	const n = 8
	ids := make([]string, n)
	for i := range ids {
		ids[i] = createOpen(t, h, model, workflowSampleModel)
	}
	// The timers are live before the writers start.
	for _, id := range ids {
		awaitDBCondition(t, scheduledFireTimeout, "a first fire of "+id, func() bool {
			_, ok := latestFire(t, h, id)
			return ok
		})
	}

	var mu sync.Mutex
	var bad []string
	conflicts, oks := 0, 0
	// record classifies one answer and reports whether it was a 200.
	record := func(what string, res callbackResult, err error) bool {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case err != nil:
			bad = append(bad, fmt.Sprintf("%s: %v", what, err))
		case res.StatusCode == http.StatusOK:
			oks++
			return true
		case res.StatusCode == http.StatusConflict && isRetryableConflict([]byte(res.Body)):
			conflicts++
		default:
			bad = append(bad, fmt.Sprintf("%s: %d %s", what, res.StatusCode, res.Body))
		}
		return false
	}

	deadline := time.Now().Add(5 * time.Second)
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; time.Now().Before(deadline); i++ {
				id := ids[(w+i)%n]
				res, err := h.callback(http.MethodPut, "/api/entity/JSON/"+id,
					fmt.Sprintf(`{"name":"Test Order","amount":%d,"status":"draft"}`, i), "")
				record("update "+id, res, err)
			}
		}(w)
	}
	wg.Wait()
	stopped := time.Now()
	if len(bad) > 0 {
		t.Fatalf("%d answers were neither 200 nor a retryable 409; first: %s", len(bad), bad[0])
	}
	if oks == 0 {
		t.Fatal("no update succeeded; the scenario exercised nothing")
	}
	t.Logf("updates: %d ok, %d retryable 409", oks, conflicts)

	// No lost timer: each entity still in Open has exactly one task, and it
	// fires again after the writers stopped.
	for _, id := range ids {
		requireState(t, h, id, "Open")
		if c := s.count(t, "SELECT count(*) FROM scheduled_tasks WHERE entity_id = $1", id); c != 1 {
			t.Errorf("entity %s: %d task rows after the writers stopped; want 1", id, c)
			continue
		}
		awaitDBCondition(t, scheduledFireTimeout, "a fire of "+id+" after the writers stopped", func() bool {
			at, ok := latestFire(t, h, id)
			return ok && at.After(stopped)
		})
	}

	// Deletes while the timers run. A retryable 409 is retried by the client.
	conflicts, oks = 0, 0
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			for try := 0; try < 5; try++ {
				res, err := h.callback(http.MethodDelete, "/api/entity/"+id, "", "")
				if record("delete "+id, res, err) {
					return
				}
				mu.Lock()
				failed := len(bad) > 0
				mu.Unlock()
				if failed {
					return
				}
			}
		}(id)
	}
	wg.Wait()
	if len(bad) > 0 {
		t.Fatalf("a delete answered neither 200 nor a retryable 409: %s", bad[0])
	}
	t.Logf("deletes: %d ok, %d retryable 409", oks, conflicts)
	for _, id := range ids {
		if _, status := h.GetEntityState(t, id); status != http.StatusNotFound {
			t.Errorf("entity %s: GET %d after its delete; want 404", id, status)
		}
		if c := s.count(t, "SELECT count(*) FROM scheduled_tasks WHERE entity_id = $1", id); c != 0 {
			t.Errorf("entity %s: %d task rows after its delete", id, c)
		}
	}
}
