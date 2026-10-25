package fixtureutil

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

// A pnode's scheduler acts only while its heartbeats succeed (spec §6.1: no
// claim "while heartbeats are failing"), and makes a lost-owner claim only
// after its own heartbeats have succeeded without a gap for a whole
// STALE_AFTER. Both are per pnode: each failed heartbeat restarts that pnode's
// clean run, and the first pnode allowed acts. Under the tuned 1s heartbeat a
// heartbeat fails whenever the database stalls for a second, which a loaded
// test host does. A scenario waiting for such an action therefore cannot count
// from a fixed instant: HeartbeatGate reads each pnode's failures from its
// captured log and moves the deadline with the first allowed pnode. The
// moved deadline is within about one heartbeat interval of the scheduler's
// own gate (allowedAt says which way), and Slack, at least a few intervals,
// covers the difference. The action must still come promptly once that pnode
// is allowed.

// heartbeatFailureLine matches the two WARN lines with which a pnode's
// scheduler ends its clean run of heartbeats (internal/scheduler/service.go,
// heartbeatDone and selfCancel). Each is written after the run is cleared.
// The next heartbeat starts within one interval, and a clean run starts with
// the first one that succeeds.
var heartbeatFailureLine = regexp.MustCompile(`\btime=(\S+) level=WARN msg="scheduler (?:heartbeat failed|heartbeats failed for the whole watchdog window)`)

// heartbeatPostponeMax bounds how far failed heartbeats may move a gate's
// deadline. Pnodes whose heartbeats keep failing never act, and the scenario
// then fails at this bound, saying so, rather than at the package timeout.
const heartbeatPostponeMax = 2 * TunedStaleAfter

// LastHeartbeatFailure returns the latest failed heartbeat one pnode's log
// records, or the zero time when it records none.
func LastHeartbeatFailure(log string) time.Time {
	var last time.Time
	for _, m := range heartbeatFailureLine.FindAllStringSubmatch(log, -1) {
		at, err := time.Parse(time.RFC3339Nano, m[1])
		if err == nil && at.After(last) {
			last = at
		}
	}
	return last
}

// HeartbeatGate is a scheduler action that a pnode may take only with clean
// heartbeats, awaited across the pnodes that may take it.
type HeartbeatGate struct {
	// ReadyAt is when the action becomes possible on a pnode whose
	// heartbeats never failed: the owner going stale for a lost-owner claim.
	ReadyAt time.Time
	// Clean is how long a pnode's heartbeats must have succeeded without a
	// gap before it may act: TunedStaleAfter for a lost-owner claim, 0 for
	// any other claim or for a heartbeat landing.
	Clean time.Duration
	// Slack is how long after the first pnode is allowed the action must be
	// seen.
	Slack time.Duration
}

// allowedAt is when the pnode whose last failed heartbeat was lastFailure is
// taken to be allowed to act: ReadyAt, or lastFailure + one interval + Clean.
// The next heartbeat starts in (lastFailure, lastFailure + interval].
//   - Clean > 0: the scheduler's gate is that start + Clean (healthySince is
//     the start, internal/scheduler/service.go:431). allowedAt is never
//     earlier, and at most one interval later.
//   - Clean = 0: the pnode is healthy when that heartbeat ends, up to one
//     heartbeat budget after its start (service.go:430-431). allowedAt is at
//     most one interval later, or at most one budget earlier.
//
// Slack absorbs both.
func (g HeartbeatGate) allowedAt(lastFailure time.Time) time.Time {
	if lastFailure.IsZero() {
		return g.ReadyAt
	}
	if at := lastFailure.Add(TunedHeartbeatInterval + g.Clean); at.After(g.ReadyAt) {
		return at
	}
	return g.ReadyAt
}

// Deadline returns when the action is overdue, given the captured logs of the
// pnodes that may take it: Slack after the earliest moment any of them is
// allowed. lastFailures holds each pnode's last failed heartbeat, zero for
// none. With no pnodes it is Slack after ReadyAt.
func (g HeartbeatGate) Deadline(logs ...string) (deadline time.Time, lastFailures []time.Time) {
	lastFailures = make([]time.Time, len(logs))
	first := g.ReadyAt
	for i, log := range logs {
		lastFailures[i] = LastHeartbeatFailure(log)
		if at := g.allowedAt(lastFailures[i]); i == 0 || at.Before(first) {
			first = at
		}
	}
	return first.Add(g.Slack), lastFailures
}

// Await polls check every 100ms until it reports done. It fails t once
// Deadline has passed, re-reading logs whenever the deadline it holds passes,
// or once failed heartbeats have moved the deadline heartbeatPostponeMax
// beyond ReadyAt + Slack. what names the awaited action; check's last
// describes the state it last saw.
func (g HeartbeatGate) Await(t testing.TB, logs func() []string, what string, check func() (done bool, last string)) {
	t.Helper()
	limit := g.ReadyAt.Add(g.Slack + heartbeatPostponeMax)
	deadline := g.ReadyAt.Add(g.Slack)
	var failures []time.Time
	for {
		done, last := check()
		if done {
			return
		}
		if now := time.Now(); now.After(deadline) {
			deadline, failures = g.Deadline(logs()...)
			if deadline.After(limit) {
				if now.After(limit) {
					t.Fatalf("%s not seen: the pnodes' heartbeats kept failing (last failures: %s), and none may act until its heartbeats have run clean for %s; gave up at %s; last: %s",
						what, describeFailures(failures), g.Clean, limit.Format(time.RFC3339Nano), last)
				}
				deadline = limit
			} else if now.After(deadline) {
				t.Fatalf("%s not seen by %s, %s after the first pnode was allowed (ready at %s, clean run %s; last failures: %s); last: %s",
					what, deadline.Format(time.RFC3339Nano), g.Slack, g.ReadyAt.Format(time.RFC3339Nano), g.Clean, describeFailures(failures), last)
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// AwaitNodeScheduler implements parity.SchedulerWaitFixture for a fixture
// whose nodes' outputs are logs, every node running a scheduler: check is due
// slack after the later of now and the heartbeat that follows the last failed
// heartbeat of the first node allowed to act.
func AwaitNodeScheduler(t testing.TB, slack time.Duration, what string, check func() (done bool, last string), logs ...*SyncBuffer) {
	t.Helper()
	HeartbeatGate{ReadyAt: time.Now(), Slack: slack}.Await(t, func() []string {
		out := make([]string, len(logs))
		for i, l := range logs {
			out[i] = l.String()
		}
		return out
	}, what, check)
}

// describeFailures lists each pnode's last failed heartbeat.
func describeFailures(failures []time.Time) string {
	parts := make([]string, len(failures))
	for i, at := range failures {
		if at.IsZero() {
			parts[i] = "none"
		} else {
			parts[i] = at.Format(time.RFC3339Nano)
		}
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
