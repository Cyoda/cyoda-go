package multinode

import (
	"net/http"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// audit_nul.go — an audit event holding U+0000 is not recorded, through a
// hand-over. The owner has no compute node for the tag; the criterion member
// is on the peer and answers with a reason holding U+0000. The owner records
// the event after the hand-over, refuses it, and the entity is saved.

func init() {
	Register(
		NamedTest{Name: "AuditNUL_HandOverReasonNotRecorded", Fn: RunAuditNUL_HandOverReasonNotRecorded},
	)
}

func RunAuditNUL_HandOverReasonNotRecorded(t *testing.T, fixture MultiNodeFixture) {
	urls := mnRequire(t, fixture, 2)
	tenant := fixture.NewTenant(t)
	owner := client.NewClient(urls[0], tenant.Token)
	const model, tag, warmTag = "mn-nul", "mn-nul", "mn-nul-w"

	// The peer's client answers a criterion with the U+0000 reason and serves
	// the warm-up's processor from its catalog.
	StartComputeClientOrSkip(t, fixture, 1, parity.ComputeClientSpec{
		TenantID: tenant.ID, Tags: []string{tag, warmTag}, Behaviour: parity.ComputeBehaviourCriterionReasonNUL,
	})
	// Wait until the owner can route to the peer (see mnWarmUp).
	warmModel := "mn-warm-" + warmTag
	cbRouteSetupModel(t, owner, warmModel, cbRouteSampleNoWriteback, mnWorkflow(warmModel+"-wf", mnProc("noop", "SYNC", warmTag, "", nil)))
	deadline := time.Now().Add(30 * time.Second)
	for {
		status, body, err := owner.CreateEntityRaw(t, warmModel, 1, mnSample)
		if err != nil {
			t.Fatalf("warm-up create: %v", err)
		}
		if status == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("30s after it attached to pnode 1 the owner still cannot route to tag %s: %d %s", warmTag, status, body)
		}
		time.Sleep(200 * time.Millisecond)
	}

	cbRouteSetupModel(t, owner, model, cbRouteSampleNoWriteback, parity.AuditNULWorkflow("mn-nul-wf", tag))
	id, err := owner.CreateEntity(t, model, 1, mnSample)
	if err != nil {
		t.Fatalf("create through the owner: %v", err)
	}
	got, err := owner.GetEntity(t, id)
	if err != nil {
		t.Fatalf("read the entity: %v", err)
	}
	if got.Meta.State != "CREATED" {
		t.Errorf("state = %q; want CREATED (the peer's criterion refused the transition)", got.Meta.State)
	}
	parity.AssertNoNotMatchEvent(t, owner, id)
}
