package scheduledtransition

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// joined_conflict.go — a scheduled run whose processor's joined callback loses
// a write race records the CONFLICT text on every backend (spec §5.8). The
// steps run one after another: the compute node holds the callout, the test
// commits a change to the callback's target, and only then is the callout
// answered.

func init() {
	parity.Register(
		parity.NamedTest{Name: "ScheduledTransition_JoinedCallbackConflictRecorded", Fn: RunScheduledTransition_JoinedCallbackConflictRecorded},
	)
}

// conflictText is what a failed attempt records for a transaction conflict.
const conflictText = "CONFLICT: a concurrent write changed the entity or its task"

// targetSample declares the fields of an entity that names a callback target.
const targetSample = `{"k":1,"targetId":"00000000-0000-0000-0000-000000000000"}`

// RunScheduledTransition_JoinedCallbackConflictRecorded: E's scheduled Fire
// runs cb-update-target, which writes F through a joined callback. F is
// changed and committed by a client after the run's transaction began, so the
// run's write of F loses first-committer-wins. Where the backend reports the
// loss — at the callback's write or at the run's commit — is its own; what the
// task records is not.
func RunScheduledTransition_JoinedCallbackConflictRecorded(t *testing.T, fixture parity.BackendFixture) {
	oc := newOwnCase(t, fixture)
	cc := oc.start(t, fixture, oc.tag, parity.ComputeBehaviourHold)

	setupModelWithWorkflow(t, oc.c, "st-jc-f", 1, ownSample, ownWorkflow("st-jc-f-wf", map[string]any{"Open": map[string]any{}}))
	fID, err := oc.c.CreateEntity(t, "st-jc-f", 1, ownSample)
	if err != nil {
		t.Fatalf("CreateEntity F: %v", err)
	}
	t.Cleanup(func() { _ = oc.c.DeleteEntity(t, fID) })

	setupModelWithWorkflow(t, oc.c, "st-jc-e", 1, targetSample,
		ownWorkflow("st-jc-e-wf", fireToDone(100, 0, ownProc("cb-update-target-strict", oc.tag, true))))
	eID, err := oc.c.CreateEntity(t, "st-jc-e", 1, fmt.Sprintf(`{"k":1,"targetId":%q}`, fID))
	if err != nil {
		t.Fatalf("CreateEntity E: %v", err)
	}
	t.Cleanup(func() { _ = oc.c.DeleteEntity(t, eID) })

	awaitCallout(t, cc, eID, fireTimeout)
	if err := oc.c.UpdateEntityData(t, fID, `{"k":2,"flavor":"two"}`); err != nil {
		t.Fatalf("client update of F: %v", err)
	}
	cc.Release(t)

	task := awaitTask(t, oc.c, eID, "Fire", fireTimeout, "a recorded error",
		func(tk *client.ScheduledTask) bool { return tk != nil && tk.LastError != "" })
	if task.LastError != conflictText {
		t.Fatalf("lastError = %q; want %q", task.LastError, conflictText)
	}
}

// awaitCallout polls until cc has received a callout for entityID.
func awaitCallout(t *testing.T, cc parity.ComputeClient, entityID uuid.UUID, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for receivedFor(t, cc, entityID) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("no callout for %s within %s", entityID, within)
		}
		time.Sleep(pollInterval)
	}
}
