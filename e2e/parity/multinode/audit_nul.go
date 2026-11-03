package multinode

import (
	"net/http"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// audit_nul.go — a compute member's reason holding U+0000 is refused through a
// hand-over too. The owner has no compute node for the tag; the criterion member
// is on the peer and answers with a reason holding U+0000. The peer refuses the
// answer as unreadable, and the owner's create fails exactly as a local one does.

func init() {
	Register(
		NamedTest{Name: "AuditNUL_HandOverAnswerRefused", Fn: RunAuditNUL_HandOverAnswerRefused},
	)
}

func RunAuditNUL_HandOverAnswerRefused(t *testing.T, fixture MultiNodeFixture) {
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
	deadline := time.Now().Add(30 * time.Second) // realtime: test poll deadline
	for {
		status, body, err := owner.CreateEntityRaw(t, warmModel, 1, mnSample)
		if err != nil {
			t.Fatalf("warm-up create: %v", err)
		}
		if status == http.StatusOK {
			break
		}
		if time.Now().After(deadline) { // realtime: test poll deadline
			t.Fatalf("30s after it attached to pnode 1 the owner still cannot route to tag %s: %d %s", warmTag, status, body)
		}
		time.Sleep(200 * time.Millisecond)
	}

	cbRouteSetupModel(t, owner, model, cbRouteSampleNoWriteback, parity.AuditNULWorkflow("mn-nul-wf", tag))
	status, body, err := owner.CreateEntityRaw(t, model, 1, mnSample)
	if err != nil {
		t.Fatalf("create through the owner: %v", err)
	}
	parity.AssertNULAnswerRefused(t, owner, model, status, body)
}
