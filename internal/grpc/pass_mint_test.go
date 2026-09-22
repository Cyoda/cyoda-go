package grpc

import (
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

func verifyPass(t *testing.T, pass string) *token.Claims {
	t.Helper()
	signer, err := token.NewSigner(make32(t)) // the secret newTestDispatcher signs with
	if err != nil {
		t.Fatal(err)
	}
	claims, err := signer.Verify(pass)
	if err != nil {
		t.Fatalf("verify pass: %v", err)
	}
	return claims
}

func onlyPass(t *testing.T, a *asked) string {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.passes) != 1 {
		t.Fatalf("cnode saw %d requests, want 1", len(a.passes))
	}
	return a.passes[0]
}

func TestRunLocal_PassLifetimeFollowsTheAnswerLimit(t *testing.T) {
	for _, limit := range []time.Duration{2 * time.Second, 20 * time.Second} {
		t.Run(limit.String(), func(t *testing.T) {
			reg := NewMemberRegistry()
			_, a := attach(t, reg, "m-1", testTenantID, "x", answersAs("m-1"))
			d := newTestDispatcher(t, reg) // pass allowance 3s

			before := time.Now()
			if res := d.RunLocal(testContext(), processorCall("x", false, limit), 1); !res.OK() {
				t.Fatalf("res = %+v", res)
			}
			claims := verifyPass(t, onlyPass(t, a))
			want := before.Add(limit + 3*time.Second)
			got := time.Unix(claims.ExpiresAt, 0)
			// ExpiresAt is whole seconds: allow the truncation and the run's own time.
			if got.Before(want.Add(-time.Second)) || got.After(want.Add(2*time.Second)) {
				t.Errorf("ExpiresAt = %v, want about %v (now + answer limit + pass allowance)", got, want)
			}
		})
	}
}

func TestRunLocal_EveryTryMintsItsOwnPass(t *testing.T) {
	reg := NewMemberRegistry()
	attach(t, reg, "m-1", testTenantID, "x", nil)          // try 1: numbered (1,0), gone on pick, no hand-off
	_, a2 := attach(t, reg, "m-2", testTenantID, "x", nil) // try 2: (2,0), no answer
	_, a3 := attach(t, reg, "m-3", testTenantID, "x", answersAs("m-3"))
	d := dispatcherGoneOnPick(t, reg, "m-1")
	call := processorCall("x", true, 50*time.Millisecond)
	call.OwnerNodeID = "node-owner"
	call.Outer = []token.Pair{{Callout: "outer-req", Major: 4, Minor: 2}}

	if res := d.RunLocal(testContext(), call, 4); !res.OK() || res.TriesUsed != 3 {
		t.Fatalf("res = %+v", res)
	}
	second, third := verifyPass(t, onlyPass(t, a2)), verifyPass(t, onlyPass(t, a3))
	if onlyPass(t, a2) == onlyPass(t, a3) {
		t.Fatal("two tries were given the same pass")
	}
	for i, c := range []*token.Claims{second, third} {
		if c.NodeID != "node-owner" || c.TxRef != "tx-1" || c.Callout != "req-fixed" {
			t.Errorf("pass %d: NodeID/TxRef/Callout = %s/%s/%s", i, c.NodeID, c.TxRef, c.Callout)
		}
		if len(c.Outer) != 1 || c.Outer[0] != call.Outer[0] {
			t.Errorf("pass %d: Outer = %+v, want the callout's enclosing pairs", i, c.Outer)
		}
	}
	if second.Major != 2 || second.Minor != 0 || third.Major != 3 || third.Minor != 0 {
		t.Errorf("numbers = (%d,%d) then (%d,%d), want (2,0) then (3,0)", second.Major, second.Minor, third.Major, third.Minor)
	}
}

func TestRunLocal_HandOverNumbering_MinorRisesUnderTheGivenMajor(t *testing.T) {
	reg := NewMemberRegistry()
	_, a1 := attach(t, reg, "m-1", testTenantID, "x", nil)
	_, a2 := attach(t, reg, "m-2", testTenantID, "x", answersAs("m-2"))
	d := newTestDispatcher(t, reg)
	call := processorCall("x", true, 50*time.Millisecond)
	call.Number = NewMinorNumberer(5)

	if res := d.RunLocal(testContext(), call, 4); !res.OK() {
		t.Fatalf("res = %+v", res)
	}
	first, second := verifyPass(t, onlyPass(t, a1)), verifyPass(t, onlyPass(t, a2))
	if first.Major != 5 || first.Minor != 1 || second.Major != 5 || second.Minor != 2 {
		t.Errorf("numbers = (%d,%d) then (%d,%d), want (5,1) then (5,2)", first.Major, first.Minor, second.Major, second.Minor)
	}
}

func TestRunLocal_NoTransaction_NoPass_ButStillNumbered(t *testing.T) {
	reg := NewMemberRegistry()
	_, a := attach(t, reg, "m-1", testTenantID, "x", answersAs("m-1"))
	d := newTestDispatcher(t, reg)
	call := armed(NewProcessorCallout(testTenantID, testEntity(), testProcessor("x", 0), "wf1", "t1", ""), false, 5*time.Second)
	numberer := call.Number.(*countingNumberer)

	if res := d.RunLocal(testContext(), call, 1); !res.OK() {
		t.Fatalf("res = %+v", res)
	}
	if pass := onlyPass(t, a); pass != "" {
		t.Errorf("a callout outside a transaction carries no pass, got one")
	}
	if numberer.major != 1 {
		t.Errorf("Next was called %d times, want 1", numberer.major)
	}
}

// A callout inside a transaction that names no owner must be refused before
// the hand-off: minting a pass with an empty NodeID would let the work reach
// the cnode while every callback it makes is later refused as node-unavailable.
func TestRunLocal_NoOwnerMeansNothingIsSent(t *testing.T) {
	reg := NewMemberRegistry()
	_, a := attach(t, reg, "m-1", testTenantID, "x", answersAs("m-1"))
	d := newTestDispatcher(t, reg)
	call := processorCall("x", false, 5*time.Second)
	call.OwnerNodeID = ""

	res := d.RunLocal(testContext(), call, 4)
	if res.Failure == nil || res.Failure.Kind != contract.Terminal {
		t.Fatalf("failure = %+v, want Terminal", res.Failure)
	}
	if a.count() != 0 {
		t.Errorf("cnode was sent %d requests, want 0: no pass may go out for a callout with no owner", a.count())
	}
	if len(res.Attempts) != 1 || res.Attempts[0].Cause != "internal error" {
		t.Errorf("Attempts = %+v, want one attempt with Cause \"internal error\"", res.Attempts)
	}
}

// The owner guard must not reach a transaction-less callout: with no TxID,
// mintPass returns before the OwnerNodeID check, so an empty OwnerNodeID too
// still goes out with no pass. armed() always sets OwnerNodeID, so this is
// built by hand rather than reused from processorCall/armed.
func TestMintPass_NoTransaction_EmptyOwnerToo_StillNoPass(t *testing.T) {
	d := newTestDispatcher(t, NewMemberRegistry())
	call := NewProcessorCallout(testTenantID, testEntity(), testProcessor("x", 0), "wf1", "t1", "")
	if call.OwnerNodeID != "" {
		t.Fatalf("precondition: OwnerNodeID = %q, want empty", call.OwnerNodeID)
	}
	pass, err := d.mintPass(call, 1, 0)
	if err != nil || pass != "" {
		t.Fatalf("mintPass = (%q, %v), want (\"\", nil)", pass, err)
	}
}

// A pnode that received a hand-over mints the passes of its own tries, naming
// the owner — nothing reaches a cnode that the pnode making the hand-off did
// not mint for that try.
func TestRunLocal_OnAPnodeThatIsNotTheOwner_MintsPassesNamingTheOwner(t *testing.T) {
	reg := NewMemberRegistry()
	_, a := attach(t, reg, "m-1", testTenantID, "x", answersAs("m-1"))
	d := newTestDispatcher(t, reg) // this pnode is "node-test"
	call := processorCall("x", true, 5*time.Second)
	call.OwnerNodeID = "node-owner"
	call.Number = NewMinorNumberer(3)

	if res := d.RunLocal(testContext(), call, 1); !res.OK() {
		t.Fatalf("res = %+v", res)
	}
	claims := verifyPass(t, onlyPass(t, a))
	if claims.NodeID != "node-owner" || claims.Major != 3 || claims.Minor != 1 {
		t.Errorf("claims = %+v, want NodeID node-owner and number (3,1)", claims)
	}
}
