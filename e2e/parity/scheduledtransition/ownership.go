package scheduledtransition

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/fixtureutil"
)

// ownership.go — the endings of a scheduled run on every backend (spec §4):
// retried safe failures, FAILED tasks and why, the audit event of a failure,
// and the fire-time cancel. Each scenario owns a tenant and a tag; nothing
// here runs two things at once (concurrency lives in internal/e2e and the
// multi-node tests).

func init() {
	parity.Register(
		parity.NamedTest{Name: "ScheduledTransition_NoComputeNodeThenFires", Fn: RunScheduledTransition_NoComputeNodeThenFires},
		parity.NamedTest{Name: "ScheduledTransition_SelfLoopReArmsNewLife", Fn: RunScheduledTransition_SelfLoopReArmsNewLife},
		parity.NamedTest{Name: "ScheduledTransition_CriterionErrorRetried", Fn: RunScheduledTransition_CriterionErrorRetried},
		parity.NamedTest{Name: "ScheduledTransition_IdempotentFailureRetried", Fn: RunScheduledTransition_IdempotentFailureRetried},
		parity.NamedTest{Name: "ScheduledTransition_LateAfterFailedAttemptsFails", Fn: RunScheduledTransition_LateAfterFailedAttemptsFails},
		parity.NamedTest{Name: "ScheduledTransition_UnsafeFailureFails", Fn: RunScheduledTransition_UnsafeFailureFails},
		parity.NamedTest{Name: "ScheduledTransition_LaterStepFailsAfterUnsafeHandOff", Fn: RunScheduledTransition_LaterStepFailsAfterUnsafeHandOff},
		parity.NamedTest{Name: "ScheduledTransition_FireTimeCancelNotScheduled", Fn: RunScheduledTransition_FireTimeCancelNotScheduled},
		parity.NamedTest{Name: "ScheduledTransition_FailedTaskReArmedByUpdate", Fn: RunScheduledTransition_FailedTaskReArmedByUpdate},
		parity.NamedTest{Name: "ScheduledTransition_FailedTaskCancelledWhenEntityLeaves", Fn: RunScheduledTransition_FailedTaskCancelledWhenEntityLeaves},
		parity.NamedTest{Name: "ScheduledTransition_NoLongerScheduledRemovedAtNextWrite", Fn: RunScheduledTransition_NoLongerScheduledRemovedAtNextWrite},
	)
}

const eventFailed = "SCHEDULED_TRANSITION_FAIL"

// ownSample declares every field the scenarios' entities carry.
const ownSample = `{"k":1,"flavor":"one"}`

// taskOf returns the entity's task for transition as GET /scheduled-tasks
// shows it, or nil when there is none.
func taskOf(t *testing.T, c *client.Client, entityID uuid.UUID, transition string) *client.ScheduledTask {
	t.Helper()
	page, err := c.ListScheduledTasks(t, url.Values{"entityId": {entityID.String()}, "limit": {"1000"}})
	if err != nil {
		t.Fatalf("ListScheduledTasks: %v", err)
	}
	for i := range page.Items {
		if page.Items[i].Transition == transition {
			return &page.Items[i]
		}
	}
	return nil
}

// awaitTask polls the entity's task every 50ms and returns the first view for
// which cond holds.
func awaitTask(t *testing.T, c *client.Client, entityID uuid.UUID, transition string, within time.Duration, what string, cond func(*client.ScheduledTask) bool) *client.ScheduledTask {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		task := taskOf(t, c, entityID, transition)
		if cond(task) {
			return task
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s/%s: %s not seen within %s; last: %+v", entityID, transition, what, within, task)
		}
		time.Sleep(pollInterval)
	}
}

// ownWorkflow wraps states in a schema 1.5 import document (the minor that
// accepts a processor's idempotent and retryPolicy) with initial state Open.
func ownWorkflow(wfName string, states map[string]any) string {
	b, _ := json.Marshal(map[string]any{
		"importMode": "REPLACE",
		"workflows": []any{map[string]any{
			"version": "1.5", "name": wfName, "initialState": "Open", "active": true, "states": states,
		}},
	})
	return string(b)
}

// ownProc is one SYNC catalog processor routed to tag, with one try.
func ownProc(name, tag string, idempotent bool) map[string]any {
	return map[string]any{"type": "calculator", "name": name, "executionMode": "SYNC",
		"config": map[string]any{"attachEntity": true, "calculationNodesTags": tag,
			"idempotent": idempotent, "retryPolicy": "NONE", "responseTimeoutMs": 10000}}
}

// fireToDone is Open -[Fire, scheduled]-> Done. timeoutMs 0 leaves it off.
func fireToDone(delayMs, timeoutMs int64, procs ...map[string]any) map[string]any {
	sched := map[string]any{"delayMs": delayMs}
	if timeoutMs > 0 {
		sched["timeoutMs"] = timeoutMs
	}
	fire := map[string]any{"name": "Fire", "next": "Done", "manual": false, "schedule": sched}
	if len(procs) > 0 {
		list := make([]any, 0, len(procs))
		for _, p := range procs {
			list = append(list, p)
		}
		fire["processors"] = list
	}
	return map[string]any{
		"Open": map[string]any{"transitions": []any{fire}},
		"Done": map[string]any{},
	}
}

// receivedFor counts the requests cc received for entityID.
func receivedFor(t *testing.T, cc parity.ComputeClient, entityID uuid.UUID) int {
	t.Helper()
	n := 0
	for _, r := range cc.Received(t) {
		if r.EntityID == entityID.String() {
			n++
		}
	}
	return n
}

// countEvents counts the entity's StateMachine events of eventType.
func countEvents(t *testing.T, c *client.Client, id uuid.UUID, eventType string) int {
	t.Helper()
	n := 0
	for _, ev := range stateMachineEvents(t, c, id) {
		if ev.EventType == eventType {
			n++
		}
	}
	return n
}

// failEventData decodes the data of the entity's SCHEDULED_TRANSITION_FAIL.
func failEventData(t *testing.T, c *client.Client, id uuid.UUID) map[string]any {
	t.Helper()
	ev := awaitStateMachineEvent(t, c, id, eventFailed, "", fireTimeout)
	var data map[string]any
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatalf("decode %s data: %v (%s)", eventFailed, err, ev.Data)
	}
	return data
}

// ownCase is one scenario's tenant, client and tag.
type ownCase struct {
	tenant parity.Tenant
	c      *client.Client
	tag    string
}

func newOwnCase(t *testing.T, fixture parity.BackendFixture) ownCase {
	t.Helper()
	tenant := fixture.NewTenant(t)
	return ownCase{tenant: tenant, c: client.NewClient(fixture.BaseURL(), tenant.Token), tag: "st-" + uuid.NewString()[:6]}
}

func (oc ownCase) start(t *testing.T, fixture parity.BackendFixture, tag, behaviour string) parity.ComputeClient {
	t.Helper()
	return parity.StartComputeClientOrSkip(t, fixture, parity.ComputeClientSpec{TenantID: oc.tenant.ID, Tags: []string{tag}, Behaviour: behaviour})
}

func (oc ownCase) create(t *testing.T, model, wf string) uuid.UUID {
	t.Helper()
	setupModelWithWorkflow(t, oc.c, model, 1, ownSample, wf)
	id, err := oc.c.CreateEntity(t, model, 1, ownSample)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	// Delete the entity when the scenario ends, however it ends (a skip or a
	// failure included), so a task that keeps retrying does not run for the
	// rest of the shared server's life.
	oc.c.DeleteEntityOnCleanup(t, id)
	return id
}

// RunScheduledTransition_NoComputeNodeThenFires: an unsafe processor whose tag
// has no compute node. The coordinator proves nothing was handed off
// (NotHandedOff), so the run clears the mark it wrote and the task goes back
// to WAITING, attempts 1 (spec §5.5, §5.6). When a compute node appears the
// retry fires. Had the mark stayed, the next claim would have ended the task
// FAILED UNSAFE_WORK_NOT_COMPLETED (§5.1 step 1) — the fire is the proof it
// was cleared.
func RunScheduledTransition_NoComputeNodeThenFires(t *testing.T, fixture parity.BackendFixture) {
	if _, ok := fixture.(parity.ComputeClientFixture); !ok {
		t.Skip("fixture cannot start further compute clients; scenario pending on this backend")
	}
	oc := newOwnCase(t, fixture)
	id := oc.create(t, "st-own-nocn", ownWorkflow("st-own-nocn-wf", fireToDone(100, 0, ownProc("noop", oc.tag, false))))

	first := awaitTask(t, oc.c, id, "Fire", fireTimeout, "a recorded attempt",
		func(tk *client.ScheduledTask) bool { return tk != nil && tk.Attempts >= 1 })
	if first.Status != "WAITING" || first.Attempts != 1 || first.FailureReason != "" {
		t.Fatalf("after the first attempt: %+v; want WAITING, attempts 1, no failure reason", *first)
	}
	if !strings.HasPrefix(first.LastError, "NO_COMPUTE_MEMBER_FOR_TAG: ") {
		t.Errorf("lastError = %q; want the NO_COMPUTE_MEMBER_FOR_TAG text", first.LastError)
	}
	if first.NextAttemptTime == nil || first.LastAttemptTime == nil {
		t.Errorf("a WAITING task after a failed attempt shows nextAttemptTime and lastAttemptTime: %+v", *first)
	}

	cc := oc.start(t, fixture, oc.tag, parity.ComputeBehaviourCatalog)
	awaitEntityState(t, oc.c, id, "Done", fireTimeout)
	if n := countEvents(t, oc.c, id, eventFired); n != 1 {
		t.Errorf("%d %s events; want 1", n, eventFired)
	}
	if n := countEvents(t, oc.c, id, eventFailed); n != 0 {
		t.Errorf("%d %s events; want 0", n, eventFailed)
	}
	if n := receivedFor(t, cc, id); n != 1 {
		t.Errorf("the compute node received %d requests; want 1 (the first attempt reached none)", n)
	}
	if task := taskOf(t, oc.c, id, "Fire"); task != nil {
		t.Errorf("the fired task is still listed: %+v", *task)
	}
}

// RunScheduledTransition_SelfLoopReArmsNewLife: Open -[Tick]-> Open. Each fire
// lands in the source state again and re-arms the same task id as a new life
// (spec §5.2 "a self-loop re-arms the same id"): same taskId, a later
// armedTime, attempts back to 0. Tick carries a criterion that always holds:
// workflow import refuses an unguarded automated self-loop, and counts a
// scheduled transition as automated.
func RunScheduledTransition_SelfLoopReArmsNewLife(t *testing.T, fixture parity.BackendFixture) {
	oc := newOwnCase(t, fixture)
	id := oc.create(t, "st-own-loop", ownWorkflow("st-own-loop-wf", map[string]any{
		"Open": map[string]any{"transitions": []any{map[string]any{
			"name": "Tick", "next": "Open", "manual": false, "schedule": map[string]any{"delayMs": 300},
			"criterion": map[string]any{"type": "simple", "jsonPath": "$.k", "operatorType": "EQUALS", "value": 1},
		}}},
	}))

	armed := taskOf(t, oc.c, id, "Tick")
	if armed == nil {
		t.Fatal("no task after the create")
	}
	deadline := time.Now().Add(fireTimeout)
	for countEvents(t, oc.c, id, eventFired) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("fewer than two fires within %s", fireTimeout)
		}
		time.Sleep(pollInterval)
	}
	later := awaitTask(t, oc.c, id, "Tick", fireTimeout, "the re-armed task",
		func(tk *client.ScheduledTask) bool { return tk != nil && tk.ArmedTime.After(armed.ArmedTime) })
	if later.TaskID != armed.TaskID {
		t.Errorf("taskId changed across lives: %s then %s; a self-loop re-arms the same id", armed.TaskID, later.TaskID)
	}
	if later.Attempts != 0 || later.LostOwners != 0 || later.LastError != "" {
		t.Errorf("re-armed task = %+v; a new life starts at attempts 0, lostOwners 0, no error", *later)
	}
	if got, _ := oc.c.GetEntity(t, id); got.Meta.State != "Open" {
		t.Errorf("state = %q; want Open", got.Meta.State)
	}
}

// RunScheduledTransition_CriterionErrorRetried: a criterion callout fails.
// Criteria are repeat-safe (spec §3), so this is a safe failure: WAITING,
// attempts 1, the compute node's message recorded; never FAILED.
func RunScheduledTransition_CriterionErrorRetried(t *testing.T, fixture parity.BackendFixture) {
	oc := newOwnCase(t, fixture)
	oc.start(t, fixture, oc.tag, parity.ComputeBehaviourCatalog)
	states := map[string]any{
		"Open": map[string]any{"transitions": []any{map[string]any{
			"name": "Fire", "next": "Done", "manual": false, "schedule": map[string]any{"delayMs": 100},
			"criterion": map[string]any{"type": "function", "function": map[string]any{
				"name":   "inject-criterion-error-retryable",
				"config": map[string]any{"calculationNodesTags": oc.tag, "attachEntity": true},
			}},
		}}},
		"Done": map[string]any{},
	}
	id := oc.create(t, "st-own-crit", ownWorkflow("st-own-crit-wf", states))

	first := awaitTask(t, oc.c, id, "Fire", fireTimeout, "a recorded attempt",
		func(tk *client.ScheduledTask) bool { return tk != nil && tk.Attempts >= 1 })
	if first.Status != "WAITING" || first.Attempts != 1 || first.FailureReason != "" {
		t.Fatalf("after the failed criterion: %+v; want WAITING, attempts 1", *first)
	}
	if !strings.Contains(first.LastError, "inject-criterion-error-retryable: deliberate failure") {
		t.Errorf("lastError = %q; want the compute node's own message", first.LastError)
	}
	if got, _ := oc.c.GetEntity(t, id); got.Meta.State != "Open" {
		t.Errorf("state = %q; want Open", got.Meta.State)
	}
}

// RunScheduledTransition_IdempotentFailureRetried: a processor declared
// idempotent fails on its compute node. That is a safe failure: WAITING,
// attempts 1, and the platform may send it again — it does.
func RunScheduledTransition_IdempotentFailureRetried(t *testing.T, fixture parity.BackendFixture) {
	oc := newOwnCase(t, fixture)
	cc := oc.start(t, fixture, oc.tag, parity.ComputeBehaviourFail)
	id := oc.create(t, "st-own-idem", ownWorkflow("st-own-idem-wf", fireToDone(100, 0, ownProc("noop", oc.tag, true))))

	first := awaitTask(t, oc.c, id, "Fire", fireTimeout, "a recorded attempt",
		func(tk *client.ScheduledTask) bool { return tk != nil && tk.Attempts >= 1 })
	if first.Status != "WAITING" || first.Attempts != 1 || first.FailureReason != "" {
		t.Fatalf("after the failed idempotent processor: %+v; want WAITING, attempts 1", *first)
	}
	if !strings.Contains(first.LastError, "scripted failure: fail") {
		t.Errorf("lastError = %q; want the compute node's own message", first.LastError)
	}
	deadline := time.Now().Add(fireTimeout)
	for receivedFor(t, cc, id) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("the idempotent processor was not sent again within %s", fireTimeout)
		}
		time.Sleep(pollInterval)
	}
	if n := countEvents(t, oc.c, id, eventFailed); n != 0 {
		t.Errorf("%d %s events; a safe failure never fails the task", n, eventFailed)
	}
}

// RunScheduledTransition_LateAfterFailedAttemptsFails: timeoutMs 1500 and a
// processor that always fails. The retries are clamped to the deadline, the
// last attempt runs up to RETRY_DELAY past it, and the counted attempt after
// the deadline ends the task FAILED EXPIRED_AFTER_FAILED_ATTEMPTS (spec §5.1,
// §5.6). The entity never moves.
func RunScheduledTransition_LateAfterFailedAttemptsFails(t *testing.T, fixture parity.BackendFixture) {
	oc := newOwnCase(t, fixture)
	oc.start(t, fixture, oc.tag, parity.ComputeBehaviourFail)
	id := oc.create(t, "st-own-late", ownWorkflow("st-own-late-wf", fireToDone(100, 1500, ownProc("noop", oc.tag, true))))

	failed := awaitTask(t, oc.c, id, "Fire", 20*time.Second, "FAILED",
		func(tk *client.ScheduledTask) bool { return tk != nil && tk.Status == "FAILED" })
	if failed.FailureReason != "EXPIRED_AFTER_FAILED_ATTEMPTS" || failed.Attempts < 1 || failed.FailedTime == nil {
		t.Errorf("failed task = %+v; want EXPIRED_AFTER_FAILED_ATTEMPTS, attempts >= 1, failedTime set", *failed)
	}
	if failed.ExpiresTime == nil {
		t.Errorf("expiresTime missing; a task with timeoutMs shows it")
	}
	// The failure's text is the last run's error. The counted attempts before
	// it set lastAttemptTime, and Fail leaves it as it was.
	if !strings.Contains(failed.LastError, "scripted failure: fail") {
		t.Errorf("lastError = %q; want the compute node's own message", failed.LastError)
	}
	if failed.LastAttemptTime == nil {
		t.Errorf("lastAttemptTime missing; the counted attempts set it")
	}
	if data := failEventData(t, oc.c, id); data["reason"] != "EXPIRED_AFTER_FAILED_ATTEMPTS" {
		t.Errorf("%s data = %v; want reason EXPIRED_AFTER_FAILED_ATTEMPTS", eventFailed, data)
	}
	if got, _ := oc.c.GetEntity(t, id); got.Meta.State != "Open" {
		t.Errorf("state = %q; a FAILED task never moves the entity", got.Meta.State)
	}
}

// RunScheduledTransition_UnsafeFailureFails: a processor not declared
// idempotent reached its compute node and the run did not commit. The task
// ends FAILED UNSAFE_WORK_NOT_COMPLETED, with a SCHEDULED_TRANSITION_FAIL
// event carrying {transition, sourceState, reason, attempts, lostOwners}, and
// the processor is never sent again (spec §5.5, §5.7).
func RunScheduledTransition_UnsafeFailureFails(t *testing.T, fixture parity.BackendFixture) {
	oc := newOwnCase(t, fixture)
	cc := oc.start(t, fixture, oc.tag, parity.ComputeBehaviourFail)
	id := oc.create(t, "st-own-unsafe", ownWorkflow("st-own-unsafe-wf", fireToDone(100, 0, ownProc("noop", oc.tag, false))))

	// The FAILED item as GET /scheduled-tasks shows it (§8): status, reason,
	// the failure's text paired with failedTime, and attempts. Fail does not count an attempt (§10.1: only
	// RecordAttempt adds 1), and this was the first run: attempts 0.
	failed := awaitTask(t, oc.c, id, "Fire", fireTimeout, "FAILED",
		func(tk *client.ScheduledTask) bool { return tk != nil && tk.Status == "FAILED" })
	if failed.FailureReason != "UNSAFE_WORK_NOT_COMPLETED" {
		t.Errorf("failureReason = %q; want UNSAFE_WORK_NOT_COMPLETED", failed.FailureReason)
	}
	if failed.Attempts != 0 || failed.LostOwners != 0 {
		t.Errorf("attempts %d, lostOwners %d; want 0 and 0", failed.Attempts, failed.LostOwners)
	}
	if failed.FailedTime == nil || failed.FailedTime.Before(failed.ScheduledTime) {
		t.Errorf("failedTime = %v; want a time at or after scheduledTime %v", failed.FailedTime, failed.ScheduledTime)
	}
	if failed.NextAttemptTime != nil {
		t.Errorf("nextAttemptTime = %v; a FAILED item has none (§8: WAITING only)", failed.NextAttemptTime)
	}
	// For a FAILED task lastError is the failure's own text and pairs with
	// failedTime (§8). Fail counts no attempt and leaves lastAttemptTime as it
	// was, so this first-run failure shows none.
	if !strings.Contains(failed.LastError, "scripted failure: fail") {
		t.Errorf("lastError = %q; want the compute node's own message", failed.LastError)
	}
	if failed.LastAttemptTime != nil {
		t.Errorf("lastAttemptTime = %v; a first-run failure counted no attempt", failed.LastAttemptTime)
	}
	data := failEventData(t, oc.c, id)
	if data["transition"] != "Fire" || data["sourceState"] != "Open" || data["reason"] != "UNSAFE_WORK_NOT_COMPLETED" {
		t.Errorf("%s data = %v; want transition Fire, sourceState Open, reason UNSAFE_WORK_NOT_COMPLETED", eventFailed, data)
	}
	if data["attempts"] != float64(0) || data["lostOwners"] != float64(0) {
		t.Errorf("%s data attempts/lostOwners = %v/%v; want 0/0, as the item", eventFailed, data["attempts"], data["lostOwners"])
	}

	// Never re-run: three retry delays later the compute node still has one
	// request, and the task is still FAILED.
	time.Sleep(3 * fixtureutil.TunedRetryDelay)
	if n := receivedFor(t, cc, id); n != 1 {
		t.Errorf("the unsafe processor was sent %d times; want exactly 1", n)
	}
	if task := taskOf(t, oc.c, id, "Fire"); task == nil || task.Status != "FAILED" {
		t.Errorf("task = %+v; a FAILED task is kept", task)
	}
	if got, _ := oc.c.GetEntity(t, id); got.Meta.State != "Open" {
		t.Errorf("state = %q; want Open", got.Meta.State)
	}
}

// RunScheduledTransition_LaterStepFailsAfterUnsafeHandOff: the unsafe first
// processor succeeds on its compute node; the idempotent second one fails, so
// the run does not commit. Unsafe work reached a compute node: FAILED
// UNSAFE_WORK_NOT_COMPLETED, and neither processor is sent again.
func RunScheduledTransition_LaterStepFailsAfterUnsafeHandOff(t *testing.T, fixture parity.BackendFixture) {
	oc := newOwnCase(t, fixture)
	tagB := oc.tag + "b"
	unsafe := oc.start(t, fixture, oc.tag, parity.ComputeBehaviourCatalog)
	failing := oc.start(t, fixture, tagB, parity.ComputeBehaviourFail)
	id := oc.create(t, "st-own-later", ownWorkflow("st-own-later-wf",
		fireToDone(100, 0, ownProc("noop", oc.tag, false), ownProc("noop", tagB, true))))

	failed := awaitTask(t, oc.c, id, "Fire", fireTimeout, "FAILED",
		func(tk *client.ScheduledTask) bool { return tk != nil && tk.Status == "FAILED" })
	if failed.FailureReason != "UNSAFE_WORK_NOT_COMPLETED" {
		t.Errorf("failureReason = %q; want UNSAFE_WORK_NOT_COMPLETED", failed.FailureReason)
	}
	time.Sleep(3 * fixtureutil.TunedRetryDelay)
	if a, b := receivedFor(t, unsafe, id), receivedFor(t, failing, id); a != 1 || b != 1 {
		t.Errorf("requests: unsafe %d, failing %d; want 1 and 1", a, b)
	}
	if got, _ := oc.c.GetEntity(t, id); got.Meta.State != "Open" {
		t.Errorf("state = %q; want Open", got.Meta.State)
	}
}

// RunScheduledTransition_FireTimeCancelNotScheduled: the model has two
// workflows selected by $.flavor. The entity (flavor one) arms Fire under W1.
// A re-import makes Fire manual in W1 and scheduled in W2, so the import keeps
// the task (it is scheduled in some workflow of the model, spec §7), but at
// fire time the selected workflow W1 no longer schedules it: the task is
// removed with SCHEDULED_TRANSITION_CANCEL and the entity stays (spec §4).
func RunScheduledTransition_FireTimeCancelNotScheduled(t *testing.T, fixture parity.BackendFixture) {
	oc := newOwnCase(t, fixture)
	const model = "st-own-cancel"
	doc := func(scheduledIn string) string {
		wf := func(name, flavor string, scheduled bool) map[string]any {
			fire := map[string]any{"name": "Fire", "next": "Done", "manual": !scheduled}
			if scheduled {
				fire["schedule"] = map[string]any{"delayMs": 5000}
			}
			return map[string]any{
				"version": "1.5", "name": name, "initialState": "Open", "active": true,
				"criterion": map[string]any{"type": "simple", "jsonPath": "$.flavor", "operatorType": "EQUALS", "value": flavor},
				"states": map[string]any{
					"Open": map[string]any{"transitions": []any{fire}},
					"Done": map[string]any{},
				},
			}
		}
		b, _ := json.Marshal(map[string]any{"importMode": "REPLACE", "workflows": []any{
			wf("st-own-cancel-w1", "one", scheduledIn == "w1"),
			wf("st-own-cancel-w2", "two", scheduledIn == "w2"),
		}})
		return string(b)
	}
	id := oc.create(t, model, doc("w1"))
	if taskOf(t, oc.c, id, "Fire") == nil {
		t.Fatal("no task after the create under W1")
	}
	if err := oc.c.ImportWorkflow(t, model, 1, doc("w2")); err != nil {
		t.Fatalf("re-import: %v", err)
	}
	if taskOf(t, oc.c, id, "Fire") == nil {
		t.Fatal("the re-import removed the task; it is still scheduled in W2, so the import keeps it")
	}

	awaitStateMachineEvent(t, oc.c, id, eventCancelled, "Open", fireTimeout)
	awaitTask(t, oc.c, id, "Fire", fireTimeout, "removal", func(tk *client.ScheduledTask) bool { return tk == nil })
	if n := countEvents(t, oc.c, id, eventFired); n != 0 {
		t.Errorf("%d %s events; want 0", n, eventFired)
	}
	if got, _ := oc.c.GetEntity(t, id); got.Meta.State != "Open" {
		t.Errorf("state = %q; want Open", got.Meta.State)
	}
}

// failOnce builds the shared start of the two FAILED-task scenarios: Open
// carries a scheduled Fire whose unsafe processor always fails, and a manual
// Leave. It waits for the task to end FAILED UNSAFE_WORK_NOT_COMPLETED.
func failOnce(t *testing.T, fixture parity.BackendFixture, model string) (ownCase, parity.ComputeClient, uuid.UUID, *client.ScheduledTask) {
	t.Helper()
	oc := newOwnCase(t, fixture)
	cc := oc.start(t, fixture, oc.tag, parity.ComputeBehaviourFail)
	states := fireToDone(1500, 0, ownProc("noop", oc.tag, false))
	open := states["Open"].(map[string]any)
	open["transitions"] = append(open["transitions"].([]any),
		map[string]any{"name": "Leave", "next": "Left", "manual": true})
	states["Left"] = map[string]any{}
	id := oc.create(t, model, ownWorkflow(model+"-wf", states))
	failed := awaitTask(t, oc.c, id, "Fire", fireTimeout, "FAILED",
		func(tk *client.ScheduledTask) bool { return tk != nil && tk.Status == "FAILED" })
	if failed.FailureReason != "UNSAFE_WORK_NOT_COMPLETED" {
		t.Fatalf("failureReason = %q; want UNSAFE_WORK_NOT_COMPLETED", failed.FailureReason)
	}
	return oc, cc, id, failed
}

// RunScheduledTransition_FailedTaskReArmedByUpdate: an entity write in the
// source state re-arms a FAILED task as a new life (spec §4, §7): WAITING,
// attempts 0, no reason, no error, a later armedTime — and no mark, so the new
// life sends its processor again instead of failing on the old life's mark.
func RunScheduledTransition_FailedTaskReArmedByUpdate(t *testing.T, fixture parity.BackendFixture) {
	oc, cc, id, failed := failOnce(t, fixture, "st-own-rearm")

	if err := oc.c.UpdateEntityData(t, id, `{"k":2,"flavor":"one"}`); err != nil {
		t.Fatalf("update in the source state: %v", err)
	}
	fresh := taskOf(t, oc.c, id, "Fire")
	if fresh == nil {
		t.Fatal("the update removed the task; it re-arms it")
	}
	if fresh.Status != "WAITING" || fresh.Attempts != 0 || fresh.LostOwners != 0 ||
		fresh.FailureReason != "" || fresh.LastError != "" || fresh.FailedTime != nil {
		t.Errorf("re-armed task = %+v; want a new life: WAITING, attempts 0, no reason, no error", *fresh)
	}
	if !fresh.ArmedTime.After(failed.ArmedTime) || fresh.TaskID != failed.TaskID {
		t.Errorf("armedTime %v (was %v), taskId %s (was %s); want a later arm of the same id",
			fresh.ArmedTime, failed.ArmedTime, fresh.TaskID, failed.TaskID)
	}

	// The new life runs: its processor is sent (a second request), and it ends
	// FAILED again, for its own reason.
	deadline := time.Now().Add(fireTimeout)
	for receivedFor(t, cc, id) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("the new life never sent its processor; an old-life mark would end it FAILED unsent")
		}
		time.Sleep(pollInterval)
	}
	again := awaitTask(t, oc.c, id, "Fire", fireTimeout, "FAILED again",
		func(tk *client.ScheduledTask) bool { return tk != nil && tk.Status == "FAILED" })
	if again.FailureReason != "UNSAFE_WORK_NOT_COMPLETED" || !again.ArmedTime.Equal(fresh.ArmedTime) {
		t.Errorf("second ending = %+v; want the new life FAILED UNSAFE_WORK_NOT_COMPLETED", *again)
	}
}

// RunScheduledTransition_FailedTaskCancelledWhenEntityLeaves: the entity
// leaves the source state; the FAILED task is removed with
// SCHEDULED_TRANSITION_CANCEL (spec §4, "A FAILED task is never claimed. One
// of these ends it").
func RunScheduledTransition_FailedTaskCancelledWhenEntityLeaves(t *testing.T, fixture parity.BackendFixture) {
	oc, _, id, _ := failOnce(t, fixture, "st-own-leave")
	if err := oc.c.UpdateEntity(t, id, "Leave", `{"k":1,"flavor":"one"}`); err != nil {
		t.Fatalf("manual Leave: %v", err)
	}
	if task := taskOf(t, oc.c, id, "Fire"); task != nil {
		t.Errorf("the FAILED task outlived the state exit: %+v", *task)
	}
	if !hasStateMachineEvent(stateMachineEvents(t, oc.c, id), eventCancelled, "Open") {
		t.Errorf("no %s event for the FAILED task of state Open", eventCancelled)
	}
	if got, _ := oc.c.GetEntity(t, id); got.Meta.State != "Left" {
		t.Errorf("state = %q; want Left", got.Meta.State)
	}
}

// RunScheduledTransition_NoLongerScheduledRemovedAtNextWrite: as
// FireTimeCancelNotScheduled, the re-import keeps a task its entity's
// workflow no longer schedules. The next write of the entity removes it with
// SCHEDULED_TRANSITION_CANCEL — the reconcile removes every task not in the
// new arm set (spec §7) — long before it is due.
func RunScheduledTransition_NoLongerScheduledRemovedAtNextWrite(t *testing.T, fixture parity.BackendFixture) {
	oc := newOwnCase(t, fixture)
	const model = "st-own-nextwrite"
	doc := func(scheduledIn string) string {
		wf := func(name, flavor string, scheduled bool) map[string]any {
			fire := map[string]any{"name": "Fire", "next": "Done", "manual": !scheduled}
			if scheduled {
				fire["schedule"] = map[string]any{"delayMs": 3600000}
			}
			return map[string]any{
				"version": "1.5", "name": name, "initialState": "Open", "active": true,
				"criterion": map[string]any{"type": "simple", "jsonPath": "$.flavor", "operatorType": "EQUALS", "value": flavor},
				"states":    map[string]any{"Open": map[string]any{"transitions": []any{fire}}, "Done": map[string]any{}},
			}
		}
		b, _ := json.Marshal(map[string]any{"importMode": "REPLACE", "workflows": []any{
			wf("st-own-nw-w1", "one", scheduledIn == "w1"),
			wf("st-own-nw-w2", "two", scheduledIn == "w2"),
		}})
		return string(b)
	}
	id := oc.create(t, model, doc("w1"))
	if err := oc.c.ImportWorkflow(t, model, 1, doc("w2")); err != nil {
		t.Fatalf("re-import: %v", err)
	}
	if taskOf(t, oc.c, id, "Fire") == nil {
		t.Fatal("the re-import removed the task; it is still scheduled in W2")
	}
	if err := oc.c.UpdateEntityData(t, id, `{"k":2,"flavor":"one"}`); err != nil {
		t.Fatalf("update: %v", err)
	}
	if task := taskOf(t, oc.c, id, "Fire"); task != nil {
		t.Errorf("the write kept a task its workflow no longer schedules: %+v", *task)
	}
	if !hasStateMachineEvent(stateMachineEvents(t, oc.c, id), eventCancelled, "Open") {
		t.Errorf("no %s event for the removed task", eventCancelled)
	}
}
