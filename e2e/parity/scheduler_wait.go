package parity

import (
	"testing"
	"time"
)

// SchedulerWaitFixture is an OPTIONAL capability: a fixture that implements it
// awaits an outcome of its nodes' schedulers with a deadline that follows
// their heartbeats. A scheduler claims and fires only while its heartbeats
// succeed (spec §6.1), and under the tuned 1s heartbeat a database stall on a
// loaded host fails one, so such a wait cannot count from a fixed instant. In-
// tree fixtures implement it with fixtureutil.AwaitNodeScheduler. Like
// ComputeClientFixture it is not part of BackendFixture: a fixture without it
// keeps compiling, and its waits keep a fixed deadline.
type SchedulerWaitFixture interface {
	// AwaitScheduler polls check until it reports done, and fails t slack
	// after the later of now and the heartbeat that follows the last failed
	// one of the first node allowed to act. what names the awaited outcome;
	// check's last describes the state it last saw.
	AwaitScheduler(t *testing.T, slack time.Duration, what string, check func() (done bool, last string))
}

// SchedulerFixture is what AwaitScheduler takes: a BackendFixture or a
// cluster fixture (multinode.MultiNodeFixture), which both mint tenants.
type SchedulerFixture interface {
	NewTenant(t *testing.T) Tenant
}

// AwaitScheduler awaits an outcome of fixture's scheduler: through
// SchedulerWaitFixture when fixture has it, otherwise slack from now.
func AwaitScheduler(t *testing.T, fixture SchedulerFixture, slack time.Duration, what string, check func() (done bool, last string)) {
	t.Helper()
	if w, ok := fixture.(SchedulerWaitFixture); ok {
		w.AwaitScheduler(t, slack, what, check)
		return
	}
	deadline := time.Now().Add(slack) // realtime: test poll deadline
	for {
		done, last := check()
		if done {
			return
		}
		if time.Now().After(deadline) { // realtime: test poll deadline
			t.Fatalf("%s not seen within %s; last: %s", what, slack, last)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
