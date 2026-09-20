package grpc

import (
	"context"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
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
	attachGone(t, reg, "m-1", testTenantID, "x")           // try 1: numbered (1,0), no hand-off
	_, a2 := attach(t, reg, "m-2", testTenantID, "x", nil) // try 2: (2,0), no answer
	_, a3 := attach(t, reg, "m-3", testTenantID, "x", answersAs("m-3"))
	d := newTestDispatcher(t, reg)
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

// Until the hand-over stops pre-minting (the last task of this stream), a pass
// already on the context still wins.
func TestMintPass_PassOnContextWins(t *testing.T) {
	d := newTestDispatcher(t, NewMemberRegistry())
	ctx := WithTxToken(context.Background(), "pre-minted-by-the-owner")
	pass, err := d.mintPass(ctx, processorCall("x", false, time.Second), 1, 0)
	if err != nil || pass != "pre-minted-by-the-owner" {
		t.Fatalf("mintPass = (%q, %v)", pass, err)
	}
}
