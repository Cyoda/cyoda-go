package fixtureutil_test

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/fixtureutil"
)

// slogTime is the time format of the server's slog text handler.
const slogTime = "2006-01-02T15:04:05.000Z07:00"

func warnLine(at time.Time, msg string) string {
	return fmt.Sprintf("time=%s level=WARN msg=%q pkg=scheduler err=\"timeout: context deadline exceeded\"\n", at.Format(slogTime), msg)
}

const (
	hbFailed   = "scheduler heartbeat failed; no claims until one succeeds"
	hbWatchdog = "scheduler heartbeats failed for the whole watchdog window; cancelled every run in progress"
)

// TestLastHeartbeatFailure: in one pnode's log, both lines that end its clean
// run of heartbeats count, the latest wins, and nothing else does.
func TestLastHeartbeatFailure(t *testing.T) {
	base := time.Date(2026, 10, 11, 10, 39, 0, 0, time.FixedZone("", -7*3600))
	if got := fixtureutil.LastHeartbeatFailure(""); !got.IsZero() {
		t.Errorf("an empty log = %s; want zero", got)
	}
	quiet := `time=2026-10-11T10:39:00.000-07:00 level=INFO msg="scheduler started" pkg=scheduler incarnation=0b6f4a2e-3c1d-4e5f-8a9b-0c1d2e3f4a5b` + "\n" +
		warnLine(base.Add(50*time.Second), "scheduler claim failed") +
		warnLine(base.Add(51*time.Second), "scheduler could not sweep dead owners")
	if got := fixtureutil.LastHeartbeatFailure(quiet); !got.IsZero() {
		t.Errorf("a log without a failed heartbeat = %s; want zero", got)
	}
	log := quiet + warnLine(base.Add(10*time.Second), hbFailed) + warnLine(base.Add(12*time.Second+345*time.Millisecond), hbFailed) +
		warnLine(base.Add(11*time.Second), hbWatchdog)
	if got, want := fixtureutil.LastHeartbeatFailure(log), base.Add(12*time.Second+345*time.Millisecond); !got.Equal(want) {
		t.Errorf("LastHeartbeatFailure = %s; want %s, the latest", got, want)
	}
	if got, want := fixtureutil.LastHeartbeatFailure(log+warnLine(base.Add(20*time.Second), hbWatchdog)), base.Add(20*time.Second); !got.Equal(want) {
		t.Errorf("LastHeartbeatFailure = %s; want %s, the watchdog line", got, want)
	}
}

// TestHeartbeatGateDeadline: each pnode may act once ReadyAt has come and its
// own heartbeats have run Clean after its last failure (spec §6.1 is per
// pnode); the first pnode allowed acts, so the deadline is Slack after the
// minimum over the pnodes.
func TestHeartbeatGateDeadline(t *testing.T) {
	staleAt := time.Date(2026, 10, 11, 10, 40, 7, 0, time.UTC)
	const slack = 30 * time.Second
	hb := fixtureutil.TunedHeartbeatInterval
	g := fixtureutil.HeartbeatGate{ReadyAt: staleAt, Clean: fixtureutil.TunedStaleAfter, Slack: slack}
	early := staleAt.Add(-fixtureutil.TunedStaleAfter - 2*time.Second)
	late := staleAt.Add(-18 * time.Second)
	later := staleAt.Add(-10 * time.Second)

	for _, tc := range []struct {
		name string
		logs []string
		want time.Time
	}{
		{"no pnode", nil, staleAt.Add(slack)},
		{"a clean pnode", []string{""}, staleAt.Add(slack)},
		{"a failure a stale period before", []string{warnLine(early, hbFailed)}, staleAt.Add(slack)},
		{"a late failure on the only pnode", []string{warnLine(early, hbFailed) + warnLine(late, hbFailed)},
			late.Add(hb + fixtureutil.TunedStaleAfter + slack)},
		{"one pnode failed late, another stayed clean", []string{warnLine(late, hbFailed), ""}, staleAt.Add(slack)},
		{"one pnode failed late, another early", []string{warnLine(late, hbFailed), warnLine(early, hbFailed)}, staleAt.Add(slack)},
		{"both failed late: the earlier one acts first", []string{warnLine(later, hbFailed), warnLine(late, hbWatchdog)},
			late.Add(hb + fixtureutil.TunedStaleAfter + slack)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, failures := g.Deadline(tc.logs...)
			if !got.Equal(tc.want) {
				t.Errorf("Deadline = %s; want %s", got, tc.want)
			}
			if len(failures) != len(tc.logs) {
				t.Errorf("%d failures reported; want one per pnode, %d", len(failures), len(tc.logs))
			}
		})
	}

	// Clean 0: an action any healthy pnode takes (a claim, a heartbeat
	// landing) is due once the heartbeat after its last failure has run.
	g0 := fixtureutil.HeartbeatGate{ReadyAt: staleAt, Slack: slack}
	fail := staleAt.Add(5 * time.Second)
	if got, _ := g0.Deadline(warnLine(fail, hbFailed)); !got.Equal(fail.Add(hb + slack)) {
		t.Errorf("Clean 0 after a failure = %s; want %s", got, fail.Add(hb+slack))
	}
}

// fatalTB records the first Fatalf and ends the goroutine, as testing.T does.
type fatalTB struct {
	testing.TB
	msg string
}

func (f *fatalTB) Helper() {}

func (f *fatalTB) Fatalf(format string, args ...any) {
	f.msg = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

// runAwait runs HeartbeatGate.Await on a goroutine of its own and returns the
// fatal message ("" when it returned) and how long it took.
func runAwait(g fixtureutil.HeartbeatGate, logs func() []string, check func() (bool, string)) (string, time.Duration) {
	tb := &fatalTB{}
	done := make(chan struct{})
	start := time.Now()
	go func() {
		defer close(done)
		g.Await(tb, logs, "the claim", check)
	}()
	<-done
	return tb.msg, time.Since(start)
}

// TestHeartbeatGateAwait: a failed heartbeat moves the deadline as far as it
// moves the first allowed pnode; past it the wait fails and says why; and
// pnodes whose heartbeats keep failing cannot hold the test open forever.
func TestHeartbeatGateAwait(t *testing.T) {
	const slack = 200 * time.Millisecond
	stale := fixtureutil.TunedStaleAfter
	never := func() (bool, string) { return false, "RUNNING lostOwners 0" }

	t.Run("seen before the deadline", func(t *testing.T) {
		n := 0
		msg, _ := runAwait(fixtureutil.HeartbeatGate{ReadyAt: time.Now(), Clean: stale, Slack: time.Minute}, func() []string { return nil }, func() (bool, string) {
			n++
			return n == 3, ""
		})
		if msg != "" || n != 3 {
			t.Errorf("fatal %q after %d checks; want a return on the third", msg, n)
		}
	})

	t.Run("no failure: due slack after ReadyAt", func(t *testing.T) {
		msg, took := runAwait(fixtureutil.HeartbeatGate{ReadyAt: time.Now(), Clean: stale, Slack: slack}, func() []string { return []string{""} }, never)
		if !strings.Contains(msg, "the claim not seen") || !strings.Contains(msg, "RUNNING lostOwners 0") {
			t.Errorf("fatal = %q; want what was awaited and the last state", msg)
		}
		if took < slack || took > slack+2*time.Second {
			t.Errorf("failed after %s; want about %s", took, slack)
		}
	})

	t.Run("a late failure on every pnode moves the deadline", func(t *testing.T) {
		// ReadyAt is long past; both pnodes' heartbeats failed so recently
		// that §6.1 allows the first of them 700ms from now.
		fail := time.Now().Add(-fixtureutil.TunedHeartbeatInterval - stale + 700*time.Millisecond)
		other := fail.Add(5 * time.Second)
		logs := func() []string { return []string{warnLine(other, hbFailed), warnLine(fail, hbFailed)} }
		msg, took := runAwait(fixtureutil.HeartbeatGate{ReadyAt: time.Now().Add(-time.Minute), Clean: stale, Slack: slack}, logs, never)
		if took < 700*time.Millisecond || took > 3*time.Second {
			t.Errorf("failed after %s; the earlier failure moved the deadline to about 900ms", took)
		}
		for _, at := range []time.Time{fail, other} {
			if !strings.Contains(msg, at.Format(slogTime)[:19]) {
				t.Errorf("fatal = %q; want it to name each pnode's last failure, %s among them", msg, at.Format(slogTime))
			}
		}
	})

	t.Run("one clean pnode keeps the deadline", func(t *testing.T) {
		fail := time.Now().Add(-stale + 5*time.Second)
		logs := func() []string { return []string{warnLine(fail, hbFailed), ""} }
		_, took := runAwait(fixtureutil.HeartbeatGate{ReadyAt: time.Now(), Clean: stale, Slack: slack}, logs, never)
		if took > 2*time.Second {
			t.Errorf("failed after %s; the clean pnode was allowed at once, so want about %s", took, slack)
		}
	})

	t.Run("failures cannot postpone it without bound", func(t *testing.T) {
		// Failures far in the future stand for heartbeats that keep failing:
		// the wait ends at the cap, 2 x STALE_AFTER past the unmoved deadline.
		readyAt := time.Now().Add(-slack - 2*stale + 500*time.Millisecond)
		logs := func() []string { return []string{warnLine(time.Now().Add(time.Hour), hbFailed)} }
		msg, took := runAwait(fixtureutil.HeartbeatGate{ReadyAt: readyAt, Clean: stale, Slack: slack}, logs, never)
		if took < 400*time.Millisecond || took > 3*time.Second {
			t.Errorf("failed after %s; want about 500ms, at the cap", took)
		}
		if !strings.Contains(msg, "kept failing") {
			t.Errorf("fatal = %q; want it to say the heartbeats kept failing", msg)
		}
	})
}
