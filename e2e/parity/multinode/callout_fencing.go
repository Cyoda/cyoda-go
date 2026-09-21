package multinode

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// callout_fencing.go — passes minted by another pnode, and the numbers they
// carry. The pnode that makes the hand-off mints each try's pass in the OWNER's
// name, so a cnode attached to the peer has its callbacks routed to the owner
// and judged by the owner's fence: admitted while its try is the current one,
// refused with 410 once the callout has moved on.
//
// Nothing here stops a pnode or a cnode. The scenarios never assert a precise
// interleave: they hold a transaction open with a second callout and assert the
// two doors' answers the moment they are given.

func init() {
	Register(
		NamedTest{Name: "Callout_PassFromAnotherPnode", Fn: RunCallout_PassFromAnotherPnode},
		NamedTest{Name: "Callout_MinorAbsorbedAcrossHandOvers", Fn: RunCallout_MinorAbsorbedAcrossHandOvers},
	)
}

type mnCreate struct {
	status int
	body   []byte
	err    error
}

func mnGoCreate(t *testing.T, c *client.Client, model, sample string) <-chan mnCreate {
	out := make(chan mnCreate, 1)
	go func() {
		status, body, err := c.CreateEntityRaw(t, model, 1, sample)
		out <- mnCreate{status, body, err}
	}()
	return out
}

func mnAwaitCreate(t *testing.T, ch <-chan mnCreate, within time.Duration) mnCreate {
	t.Helper()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("create: %v", r.err)
		}
		return r
	case <-time.After(within):
		t.Fatalf("the create did not complete within %s", within)
		return mnCreate{}
	}
}

// mnLastCallback returns the late-callback outcome of the most recent request
// in a released client's record.
func mnLastCallback(t *testing.T, recs []parity.ReceivedCallout) parity.LateCallbackOutcome {
	t.Helper()
	if len(recs) == 0 || recs[len(recs)-1].Callback == nil {
		t.Fatalf("Release recorded no callback outcome: %+v", recs)
	}
	cb := *recs[len(recs)-1].Callback
	if cb.Error != "" {
		t.Fatalf("the late callback could not be made: %s", cb.Error)
	}
	return cb
}

func mnAssertRefused(t *testing.T, who string, cb parity.LateCallbackOutcome) {
	t.Helper()
	if cb.HTTPStatus != http.StatusGone || cb.HTTPErrorCode != "CALLOUT_SUPERSEDED" {
		t.Errorf("%s, HTTP door: %d %s; want 410 CALLOUT_SUPERSEDED", who, cb.HTTPStatus, cb.HTTPErrorCode)
	}
	if cb.GRPCAttempted && (cb.GRPCSuccess || !strings.HasPrefix(cb.GRPCErrorMessage, "CALLOUT_SUPERSEDED:")) {
		t.Errorf("%s, gRPC door: success=%t message=%q; want a refusal with CALLOUT_SUPERSEDED", who, cb.GRPCSuccess, cb.GRPCErrorMessage)
	}
}

// RunCallout_PassFromAnotherPnode: the pnode that makes the hand-off mints the
// pass, in the owner's name. A callback bearing it joins the owner's
// transaction; once that callout has ended the same kind of pass is refused
// while the transaction is still open.
func RunCallout_PassFromAnotherPnode(t *testing.T, fixture MultiNodeFixture) {
	urls := mnRequire(t, fixture, 2)

	t.Run("joins-the-owners-transaction", func(t *testing.T) {
		tenant := fixture.NewTenant(t)
		owner := client.NewClient(urls[0], tenant.Token)
		const primary, secondary, tag = "mn-pass-join", "mn-pass-join-sec", "mn-pass-join"
		cbRouteSetupModel(t, owner, secondary, cbRouteSampleSecondary, cbRouteSecondaryWorkflow)
		mnWarmUp(t, fixture, owner, tenant, 1, "mn-pass-join-w", tag)
		cbRouteSetupModel(t, owner, primary, cbRouteSampleCreateSecondary,
			mnWorkflow("mn-pass-join-wf", mnProc("cb-create-secondary", "SYNC", tag, cbRouteContext(secondary, "mn-pass-join"), nil)))

		id, err := owner.CreateEntity(t, primary, 1, cbRouteSampleCreateSecondary)
		if err != nil {
			t.Fatalf("create through the owner: %v", err)
		}
		ent, err := owner.GetEntity(t, id)
		if err != nil {
			t.Fatalf("GetEntity: %v", err)
		}
		secID, _ := ent.Data["secondaryId"].(string)
		if secID == "" || ent.Data["tokenWasEmpty"] != false {
			t.Fatalf("primary data = %v; want a secondaryId and tokenWasEmpty=false", ent.Data)
		}
		cbRouteSameTxID(t, owner, id, uuid.MustParse(secID))
	})

	t.Run("refused-once-its-callout-ended", func(t *testing.T) {
		tenant := fixture.NewTenant(t)
		owner := client.NewClient(urls[0], tenant.Token)
		const primary, secondary, tagLate, tagHold = "mn-pass-ended", "mn-pass-ended-sec", "mn-pass-late", "mn-pass-hold"
		cbRouteSetupModel(t, owner, secondary, cbRouteSampleSecondary, cbRouteSecondaryWorkflow)

		late := StartComputeClientOrSkip(t, fixture, 1, parity.ComputeClientSpec{TenantID: tenant.ID, Tags: []string{tagLate}, Behaviour: parity.ComputeBehaviourLateCallback})
		hold := StartComputeClientOrSkip(t, fixture, 1, parity.ComputeClientSpec{TenantID: tenant.ID, Tags: []string{tagHold}, Behaviour: parity.ComputeBehaviourStall})
		mnWarmUp(t, fixture, owner, tenant, 1, "mn-pass-ended-w")
		cbRouteSetupModel(t, owner, primary, cbRouteSampleNoWriteback, mnWorkflow("mn-pass-ended-wf",
			mnProc("late", "ASYNC_NEW_TX", tagLate, cbRouteContext(secondary, "mn-pass-ended"), map[string]any{"responseTimeoutMs": 500}),
			mnProc("hold", "ASYNC_NEW_TX", tagHold, "", map[string]any{"responseTimeoutMs": 5000})))

		done := mnGoCreate(t, owner, primary, mnSample)
		parity.AwaitReceived(t, hold, 1, 15*time.Second) // the first callout has ended; the transaction is open
		mnAssertRefused(t, "the late compute node", mnLastCallback(t, late.Release(t)))

		if r := mnAwaitCreate(t, done, 30*time.Second); r.status != http.StatusOK {
			t.Fatalf("create: %d %s; want 200 — both processors are ASYNC_NEW_TX", r.status, r.body)
		}
		if list, err := owner.ListEntitiesByModel(t, secondary, 1); err != nil || len(list) != 0 {
			t.Errorf("secondary entities: %d err=%v; want 0 — the refused callback wrote nothing", len(list), err)
		}
	})
}

// RunCallout_MinorAbsorbedAcrossHandOvers: pnode 1 makes two tries under one
// hand-over. The second try's callback is admitted and, from then on, the
// first try's is refused. The callout is then handed over to pnode 2, whose
// first try (minor 1 under the next major) is admitted although minor 2 was
// seen under the previous major.
//
// Two peers advertise the tag and the owner's peer order is random, so the
// scenario drives creates until one is handed to pnode 1 first — seen from
// pnode 1's held client receiving work — and asserts only on that one. A create
// handed to pnode 2 first is served there at once, costs milliseconds and is
// discarded. Ten creates all going to pnode 2 first has probability 2^-10, and
// the scenario then fails loudly rather than passing vacuously.
func RunCallout_MinorAbsorbedAcrossHandOvers(t *testing.T, fixture MultiNodeFixture) {
	urls := mnRequire(t, fixture, 3)
	tenant := fixture.NewTenant(t)
	owner := client.NewClient(urls[0], tenant.Token)
	const primary, secondary, tag = "mn-minor", "mn-minor-sec", "mn-minor"
	cbRouteSetupModel(t, owner, secondary, cbRouteSampleSecondary, cbRouteSecondaryWorkflow)

	try1 := StartComputeClientOrSkip(t, fixture, 1, parity.ComputeClientSpec{TenantID: tenant.ID, Tags: []string{tag}, Behaviour: parity.ComputeBehaviourLateCallback})
	try2 := StartComputeClientOrSkip(t, fixture, 1, parity.ComputeClientSpec{TenantID: tenant.ID, Tags: []string{tag}, Behaviour: parity.ComputeBehaviourLateCallback})
	mnWarmUp(t, fixture, owner, tenant, 1, "mn-minor-w1")
	mnWarmUp(t, fixture, owner, tenant, 2, "mn-minor-w2", tag) // the healthy one, on pnode 2
	cbRouteSetupModel(t, owner, primary, cbRouteSampleCreateSecondary, mnWorkflow("mn-minor-wf",
		mnProc("cb-create-secondary", "SYNC", tag, cbRouteContext(secondary, "mn-minor"), map[string]any{"idempotent": true, "responseTimeoutMs": 4000})))

	for attempt := 1; attempt <= 10; attempt++ {
		done := mnGoCreate(t, owner, primary, cbRouteSampleCreateSecondary)
		exercised := false
		for waiting := true; waiting; {
			select {
			case r := <-done:
				if r.err != nil || r.status != http.StatusOK {
					t.Fatalf("attempt %d, served by pnode 2 first: status=%d err=%v body=%s", attempt, r.status, r.err, r.body)
				}
				waiting = false
			case <-time.After(50 * time.Millisecond):
				if len(try1.Received(t)) > 0 {
					exercised, waiting = true, false
				}
			}
		}
		if !exercised {
			continue // pnode 2 was asked first; nothing to assert on this create
		}

		// pnode 1 was asked first: try (M,1) went to try1, which holds it.
		parity.AwaitReceived(t, try2, 1, 15*time.Second) // after 4s: try (M,2) went to try2
		cb2 := mnLastCallback(t, try2.Release(t))
		if cb2.HTTPStatus != http.StatusOK || (cb2.GRPCAttempted && !cb2.GRPCSuccess) {
			t.Fatalf("the second try's callback: HTTP %d %s, gRPC success=%t; want admitted — a higher minor is absorbed",
				cb2.HTTPStatus, cb2.HTTPErrorCode, cb2.GRPCSuccess)
		}
		mnAssertRefused(t, "the first try, after the second try's callback was seen", mnLastCallback(t, try1.Release(t)))

		// try2 never answers; pnode 1 reports no_answer; the callout is handed
		// over to pnode 2, whose cb-create-secondary calls back under (M+1, 1).
		r := mnAwaitCreate(t, done, 60*time.Second)
		if r.status != http.StatusOK {
			t.Fatalf("create: %d %s; want 200 — the second hand-over's minor 1 must be admitted", r.status, r.body)
		}
		return
	}
	t.Fatal("pnode 1 was never asked first in 10 creates (probability 2^-10); the scenario asserted nothing")
}
