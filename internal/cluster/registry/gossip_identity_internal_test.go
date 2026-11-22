package registry

import (
	"errors"
	"net"
	"testing"

	"github.com/hashicorp/memberlist"
)

// aliveAt is a peer record as NotifyMerge receives it during a join exchange.
func aliveAt(name, ip string, port uint16) *memberlist.Node {
	return &memberlist.Node{Name: name, Addr: net.ParseIP(ip), Port: port, State: memberlist.StateAlive}
}

// TestIdentityGuard_OwnRecordIsTheAdvertisedAddress pins why the guard
// compares against what memberlist advertises rather than against the bind
// address: under the default CYODA_GOSSIP_ADDR (`:7946`) the bind host is
// empty, and `0.0.0.0` is the other spelling of the same thing. A peer holds
// neither — it holds the address memberlist derived — so a guard comparing
// bind addresses would read this node's own record as somebody else's and
// refuse every restart of a node that had crashed.
func TestIdentityGuard_OwnRecordIsTheAdvertisedAddress(t *testing.T) {
	const advertised = "10.1.2.3:7946"

	for _, bind := range []string{"", "0.0.0.0"} {
		guard := newIdentityGuard("node-a")
		guard.publish(advertised)

		// What a bind-address comparison would have produced, to keep the
		// test honest about what it is ruling out.
		composed := net.JoinHostPort(bind, "7946")
		if composed == advertised {
			t.Fatalf("bind %q composes to the advertised address; the test proves nothing", bind)
		}

		own := aliveAt("node-a", "10.1.2.3", 7946)
		if err := guard.NotifyMerge([]*memberlist.Node{own}); err != nil {
			t.Errorf("bind %q: this node's own record was refused: %v", bind, err)
		}
		if addr := guard.duplicate(); addr != "" {
			t.Errorf("bind %q: this node's own record was recorded as a duplicate at %s", bind, addr)
		}

		other := aliveAt("node-a", "10.1.2.9", 7946)
		if err := guard.NotifyMerge([]*memberlist.Node{other}); !errors.Is(err, errDuplicateNodeID) {
			t.Errorf("bind %q: a second node under this id was admitted: %v", bind, err)
		}
		if addr := guard.duplicate(); addr != "10.1.2.9:7946" {
			t.Errorf("bind %q: duplicate = %q, want the other node's address", bind, addr)
		}
	}
}

// TestIdentityGuard_DepartedRecordDoesNotHoldTheName covers the other
// exclusion at the unit level: a record left behind by a node that stopped is
// not a second holder of the id.
func TestIdentityGuard_DepartedRecordDoesNotHoldTheName(t *testing.T) {
	for _, state := range []memberlist.NodeStateType{memberlist.StateLeft, memberlist.StateDead} {
		guard := newIdentityGuard("node-a")
		guard.publish("10.1.2.3:7946")

		departed := aliveAt("node-a", "10.1.2.9", 7946)
		departed.State = state
		if err := guard.NotifyMerge([]*memberlist.Node{departed}); err != nil {
			t.Errorf("state %v: a restart under a departed node's id was refused: %v", state, err)
		}
		if addr := guard.duplicate(); addr != "" {
			t.Errorf("state %v: a departed record was recorded as a duplicate at %s", state, addr)
		}
	}
}

// TestIdentityGuard_ForgetDropsWhatAnEarlierAttemptFound is what lets a
// restarting node in once its peers have reaped the record of its previous
// life: every join attempt judges the cluster as it is then, not as the first
// attempt found it.
func TestIdentityGuard_ForgetDropsWhatAnEarlierAttemptFound(t *testing.T) {
	guard := newIdentityGuard("node-a")
	guard.publish("10.1.2.3:7946")

	if err := guard.NotifyMerge([]*memberlist.Node{aliveAt("node-a", "10.1.2.9", 7946)}); err == nil {
		t.Fatal("a second node under this id was admitted")
	}
	guard.forget()
	if addr := guard.duplicate(); addr != "" {
		t.Errorf("duplicate = %q after forget; the next attempt must judge the cluster as it is then", addr)
	}
}

// TestIdentityGuard_ConflictBeforeStartingRefusesTheNode covers the case no
// join exchange can carry, and so the merge delegate cannot reach: two nodes
// under one id starting at the same instant, each learning of the other from
// the gossip that follows. memberlist takes `existing` from this node's own
// node map, so a record that is this node's own — its id at its own address —
// is proof that this node is one of the two.
func TestIdentityGuard_ConflictBeforeStartingRefusesTheNode(t *testing.T) {
	guard := newIdentityGuard("node-a")
	guard.publish("10.1.2.3:7946")

	own := aliveAt("node-a", "10.1.2.3", 7946)
	other := aliveAt("node-a", "10.1.2.9", 7946)
	guard.NotifyConflict(own, other)

	if addr := guard.duplicate(); addr != "10.1.2.9:7946" {
		t.Errorf("duplicate = %q, want the address the id was claimed from", addr)
	}
}

// TestIdentityGuard_ConflictWhileServingDoesNotStopTheNode is the other half
// of the same proof. Taking down a node that is already serving loses
// availability and gains no correctness, so what a node does about its id
// being contested after it has started is one ERROR line and nothing else.
func TestIdentityGuard_ConflictWhileServingDoesNotStopTheNode(t *testing.T) {
	guard := newIdentityGuard("node-a")
	guard.publish("10.1.2.3:7946")
	guard.startServing()

	guard.NotifyConflict(aliveAt("node-a", "10.1.2.3", 7946), aliveAt("node-a", "10.1.2.9", 7946))

	if addr := guard.duplicate(); addr != "" {
		t.Errorf("duplicate = %q; a node that is already serving does not stop over a contested id", addr)
	}
}

// TestIdentityGuard_ConflictBetweenOtherNodesIsOnlyLogged pins the limit of
// the proof: a witness of somebody else's duplicate is not one of the two and
// has nothing to refuse.
func TestIdentityGuard_ConflictBetweenOtherNodesIsOnlyLogged(t *testing.T) {
	for _, existing := range []*memberlist.Node{
		aliveAt("node-b", "10.1.2.4", 7946),  // another node's id entirely
		aliveAt("node-a", "10.1.2.99", 7946), // this node's id, but not this node's record
	} {
		guard := newIdentityGuard("node-a")
		guard.publish("10.1.2.3:7946")

		guard.NotifyConflict(existing, aliveAt(existing.Name, "10.1.2.9", 7946))

		if addr := guard.duplicate(); addr != "" {
			t.Errorf("existing %s: duplicate = %q; a conflict this node is not part of is only witnessed", existing.Address(), addr)
		}
	}
}

// TestIdentityGuard_MergeWhileServingRefusesTheExchangeOnly says what an
// established node does with a join that would establish a duplicate: it
// refuses that one exchange, which keeps the second record out of its view,
// and records nothing — nothing reads the finding once Register has returned.
func TestIdentityGuard_MergeWhileServingRefusesTheExchangeOnly(t *testing.T) {
	guard := newIdentityGuard("node-a")
	guard.publish("10.1.2.3:7946")
	guard.startServing()

	err := guard.NotifyMerge([]*memberlist.Node{aliveAt("node-a", "10.1.2.9", 7946)})
	if !errors.Is(err, errDuplicateNodeID) {
		t.Errorf("the exchange that would establish a duplicate was admitted: %v", err)
	}
	if addr := guard.duplicate(); addr != "" {
		t.Errorf("duplicate = %q; nothing reads the finding after Register", addr)
	}
}
