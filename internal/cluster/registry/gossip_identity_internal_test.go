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
