package multinode

import (
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// audit_fail_reason.go — a processor's audit record on a real cluster. The
// compute node that fails the processor is on another pnode; the owner records
// the member's own failure text as failReason, as it does for a local member,
// and records none when the member sent none.

func init() {
	Register(
		NamedTest{Name: "Audit_FailReasonThroughHandOver", Fn: RunAudit_FailReasonThroughHandOver},
	)
}

// RunAudit_FailReasonThroughHandOver: an ASYNC_NEW_TX processor whose failure
// does not stop the run, so its record is durable, answered by a failing member
// on a peer.
func RunAudit_FailReasonThroughHandOver(t *testing.T, fixture MultiNodeFixture) {
	urls := mnRequire(t, fixture, 2)
	for _, tc := range []struct {
		name, tag, behaviour string
		wantFailReason       string // "" = absent
	}{
		{"member text", "mn-fr-text", parity.ComputeBehaviourFail, "scripted failure: fail"},
		{"no member text", "mn-fr-none", parity.ComputeBehaviourFailSilent, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tenant := fixture.NewTenant(t)
			owner := client.NewClient(urls[0], tenant.Token)
			model := tc.tag

			failing := StartComputeClientOrSkip(t, fixture, 1, parity.ComputeClientSpec{TenantID: tenant.ID, Tags: []string{tc.tag}, Behaviour: tc.behaviour})
			mnWarmUp(t, fixture, owner, tenant, 1, tc.tag+"-w")
			cbRouteSetupModel(t, owner, model, cbRouteSampleNoWriteback,
				mnWorkflow(tc.tag+"-wf", mnProc("charge", "ASYNC_NEW_TX", tc.tag, "", map[string]any{"idempotent": true})))

			id, err := owner.CreateEntity(t, model, 1, mnSample)
			if err != nil {
				t.Fatalf("create through the owner: %v; want 200, an ASYNC_NEW_TX failure does not stop the run", err)
			}
			if got := parity.AwaitReceived(t, failing, 1, 5*time.Second); len(got) != 1 {
				t.Fatalf("the failing compute client on the peer received %d requests; want 1", len(got))
			}

			resp, err := owner.GetAuditEvents(t, id)
			if err != nil {
				t.Fatalf("GetAuditEvents: %v", err)
			}
			var records int
			for i := range resp.Items {
				if resp.Items[i].AuditEventType != "StateMachine" {
					continue
				}
				sm, err := resp.Items[i].AsStateMachine()
				if err != nil {
					t.Fatalf("decode state-machine audit event: %v", err)
				}
				if sm.EventType == "STATE_PROCESS_RESULT" {
					records++
				}
			}
			if records != 1 {
				t.Fatalf("%d STATE_PROCESS_RESULT events; want exactly 1, the processor's own record", records)
			}
			data := parity.StateMachineEventData(t, owner, id, "STATE_PROCESS_RESULT")
			if data["processorName"] != "charge" || data["executionMode"] != "ASYNC_NEW_TX" || data["success"] != false {
				t.Errorf("STATE_PROCESS_RESULT data = %v; want charge, ASYNC_NEW_TX, success false", data)
			}
			got, present := data["failReason"]
			switch {
			case tc.wantFailReason == "" && present:
				t.Errorf("failReason = %v; want none, the member sent no text", got)
			case tc.wantFailReason != "" && got != tc.wantFailReason:
				t.Errorf("failReason = %v (present=%t); want %q, the member's own text", got, present, tc.wantFailReason)
			}
		})
	}
}
