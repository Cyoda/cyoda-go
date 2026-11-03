package clusterstatus

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/lifecycle"
)

// fakeDir is a Directory whose members and Changed() channel the test controls.
type fakeDir struct {
	mu      sync.Mutex
	members []Member
	changed chan struct{}
}

func newFakeDir(ms ...Member) *fakeDir {
	return &fakeDir{members: ms, changed: make(chan struct{})}
}

func (d *fakeDir) Members() []Member {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]Member(nil), d.members...)
}

func (d *fakeDir) Changed() <-chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.changed
}

// add appends a member and fires Changed(), as the gossip directory does on a join.
func (d *fakeDir) add(m Member) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.members = append(d.members, m)
	close(d.changed)
	d.changed = make(chan struct{})
}

// fakeAsker answers per node id through a function the test supplies.
type fakeAsker struct {
	mu     sync.Mutex
	calls  map[string]int
	answer func(ctx context.Context, id string, call int) (NodeStatus, error)
}

func newFakeAsker(answer func(ctx context.Context, id string, call int) (NodeStatus, error)) *fakeAsker {
	return &fakeAsker{calls: map[string]int{}, answer: answer}
}

func (a *fakeAsker) AskStatus(ctx context.Context, id, _ string) (NodeStatus, error) {
	call := func() int {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.calls[id]++
		return a.calls[id]
	}()
	return a.answer(ctx, id, call)
}

func (a *fakeAsker) called(id string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls[id]
}

func okStatus() NodeStatus {
	return NodeStatus{State: lifecycle.Normal, Database: true, Version: "v1", Clock: Clock{Controllable: true, Version: 3, Writer: "a"}}
}

func allOK(context.Context, string, int) (NodeStatus, error) { return okStatus(), nil }

func member(id string, st lifecycle.State) Member {
	return Member{ID: id, Addr: id + ".internal:9000", State: st}
}

func upEntry(id string, s NodeStatus) Entry {
	return Entry{ID: id, Reachability: Up, State: s.State, Status: &s}
}

func downEntry(id string, st lifecycle.State) Entry {
	return Entry{ID: id, Reachability: Down, State: st}
}

func read(t *testing.T, s *Service, waitFor int, timeout time.Duration) Report {
	t.Helper()
	r, err := s.Read(context.Background(), waitFor, timeout)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return r
}

// elapsed runs f and returns how long it took.
func elapsed(f func()) time.Duration {
	// realtime: the test measures how long a wait took
	start := time.Now()
	f()
	// realtime: the test measures how long a wait took
	return time.Since(start)
}

func TestRound_AllUpNormal_Operational(t *testing.T) {
	s := NewService(newFakeDir(member("b", lifecycle.Normal), member("a", lifecycle.Normal)), newFakeAsker(allOK), time.Second)
	r := read(t, s, 0, 0)
	want := Report{
		Operational: true,
		Counts:      []Count{{Reachability: Up, State: lifecycle.Normal, Count: 2}},
		Nodes:       []Entry{upEntry("a", okStatus()), upEntry("b", okStatus())},
	}
	if !reflect.DeepEqual(r, want) {
		t.Fatalf("got %+v, want %+v", r, want)
	}
}

func TestRound_DownKeepsMetadataState(t *testing.T) {
	failB := func(_ context.Context, id string, _ int) (NodeStatus, error) {
		if id == "b" {
			return NodeStatus{}, errors.New("connection refused")
		}
		return okStatus(), nil
	}

	t.Run("starting", func(t *testing.T) {
		s := NewService(newFakeDir(member("a", lifecycle.Normal), member("b", lifecycle.Starting)), newFakeAsker(failB), time.Second)
		r := read(t, s, 0, 0)
		if r.Operational {
			t.Fatal("Operational should be false with a DOWN member")
		}
		if got, want := r.Nodes[1], downEntry("b", lifecycle.Starting); !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("built, listeners not yet open", func(t *testing.T) {
		s := NewService(newFakeDir(member("a", lifecycle.Normal), member("b", lifecycle.Normal)), newFakeAsker(failB), time.Second)
		var r Report
		d := elapsed(func() { r = read(t, s, 2, 300*time.Millisecond) })
		if d < 300*time.Millisecond || d > 1500*time.Millisecond {
			t.Fatalf("Read took %s, want about 300ms", d)
		}
		if r.Operational {
			t.Fatal("Operational should be false")
		}
		if got, want := r.Nodes[1], downEntry("b", lifecycle.Normal); !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
		wantCounts := []Count{{Up, lifecycle.Normal, 1}, {Down, lifecycle.Normal, 1}}
		if !reflect.DeepEqual(r.Counts, wantCounts) {
			t.Fatalf("counts %+v, want %+v", r.Counts, wantCounts)
		}
	})
}

func TestRound_UnusableAddressIsDownWithoutAsk(t *testing.T) {
	a := newFakeAsker(allOK)
	s := NewService(newFakeDir(member("a", lifecycle.Normal), Member{ID: "b", State: lifecycle.Normal}), a, time.Second)
	r := read(t, s, 0, 0)
	if got, want := r.Nodes[1], downEntry("b", lifecycle.Normal); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if n := a.called("b"); n != 0 {
		t.Fatalf("asker called %d times for a member with no address", n)
	}
	if r.Operational {
		t.Fatal("Operational should be false")
	}
}

func TestRound_StatelessDownMember(t *testing.T) {
	failC := func(_ context.Context, id string, _ int) (NodeStatus, error) {
		if id == "c" {
			return NodeStatus{}, errors.New("no route")
		}
		return okStatus(), nil
	}
	s := NewService(newFakeDir(
		Member{ID: "z"}, // metadata did not parse: no address, no state
		member("c", lifecycle.Draining),
		member("a", lifecycle.Normal),
	), newFakeAsker(failC), time.Second)
	r := read(t, s, 0, 0)
	if got, want := r.Nodes[2], downEntry("z", ""); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	wantCounts := []Count{{Up, lifecycle.Normal, 1}, {Down, lifecycle.Draining, 1}, {Down, "", 1}}
	if !reflect.DeepEqual(r.Counts, wantCounts) {
		t.Fatalf("counts %+v, want %+v", r.Counts, wantCounts)
	}
}

func TestCounts_Order(t *testing.T) {
	st := func(s lifecycle.State) NodeStatus { n := okStatus(); n.State = s; return n }
	entries := []Entry{
		downEntry("a", ""),
		downEntry("b", lifecycle.Unhealthy),
		upEntry("c", st(lifecycle.Draining)),
		downEntry("d", lifecycle.Starting),
		upEntry("e", st(lifecycle.Normal)),
		upEntry("f", st(lifecycle.Starting)),
		downEntry("g", lifecycle.Starting),
		upEntry("h", st(lifecycle.Unhealthy)),
	}
	want := []Count{
		{Up, lifecycle.Starting, 1}, {Up, lifecycle.Normal, 1}, {Up, lifecycle.Draining, 1}, {Up, lifecycle.Unhealthy, 1},
		{Down, lifecycle.Starting, 2}, {Down, lifecycle.Unhealthy, 1}, {Down, "", 1},
	}
	if got := counts(entries); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestRound_InvalidAnswerIsDown(t *testing.T) {
	t.Run("asker refuses the answer", func(t *testing.T) {
		bad := NodeStatus{State: "BOGUS"}
		ask := func(_ context.Context, id string, _ int) (NodeStatus, error) {
			if id == "b" {
				return NodeStatus{}, fmt.Errorf("the peer's status is out of bounds: %w", bad.Validate())
			}
			return okStatus(), nil
		}
		s := NewService(newFakeDir(member("a", lifecycle.Normal), member("b", lifecycle.Normal)), newFakeAsker(ask), time.Second)
		r := read(t, s, 0, 0)
		if got, want := r.Nodes[1], downEntry("b", lifecycle.Normal); !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})
}

func TestRound_HangingPeerBoundedByAskTimeout(t *testing.T) {
	ask := func(ctx context.Context, id string, _ int) (NodeStatus, error) {
		if id == "b" {
			<-ctx.Done()
			return NodeStatus{}, ctx.Err()
		}
		return okStatus(), nil
	}
	s := NewService(newFakeDir(member("a", lifecycle.Normal), member("b", lifecycle.Normal), member("c", lifecycle.Normal)), newFakeAsker(ask), 200*time.Millisecond)
	var r Report
	d := elapsed(func() { r = read(t, s, 0, 0) })
	if d < 200*time.Millisecond || d > time.Second {
		t.Fatalf("round took %s, want the 200ms ask bound", d)
	}
	want := []Entry{upEntry("a", okStatus()), downEntry("b", lifecycle.Normal), upEntry("c", okStatus())}
	if !reflect.DeepEqual(r.Nodes, want) {
		t.Fatalf("got %+v, want %+v", r.Nodes, want)
	}
}

func TestRound_AskErrorLoggedNotReported(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(old)

	ask := func(context.Context, string, int) (NodeStatus, error) {
		return NodeStatus{}, errors.New("dial tcp 10.9.8.7:9000: secret-detail")
	}
	s := NewService(newFakeDir(Member{ID: "b", Addr: "10.9.8.7:9000", State: lifecycle.Normal}), newFakeAsker(ask), time.Second)
	r := read(t, s, 0, 0)
	if txt := fmt.Sprintf("%+v", r); strings.Contains(txt, "10.9.8.7") || strings.Contains(txt, "secret-detail") {
		t.Fatalf("report leaks address or error text: %s", txt)
	}
	log := buf.String()
	if !strings.Contains(log, "level=DEBUG") || !strings.Contains(log, "memberId=b") || !strings.Contains(log, "secret-detail") {
		t.Fatalf("DEBUG log should carry the member id and the error, got %q", log)
	}
}

func TestOperational_Table(t *testing.T) {
	with := func(f func(*NodeStatus)) NodeStatus { s := okStatus(); f(&s); return s }
	cases := []struct {
		name    string
		other   NodeStatus
		waitFor int
		want    bool
	}{
		{"all equal", okStatus(), 0, true},
		{"draining", with(func(s *NodeStatus) { s.State = lifecycle.Draining }), 0, false},
		{"unhealthy", with(func(s *NodeStatus) { s.State = lifecycle.Unhealthy }), 0, false},
		{"starting", with(func(s *NodeStatus) { s.State = lifecycle.Starting }), 0, false},
		{"database false", with(func(s *NodeStatus) { s.Database = false }), 0, false},
		{"version mismatch", with(func(s *NodeStatus) { s.Version = "v2" }), 0, false},
		{"clock controllable differs", with(func(s *NodeStatus) { s.Clock.Controllable = false }), 0, false},
		{"clock version differs", with(func(s *NodeStatus) { s.Clock.Version = 4 }), 0, false},
		{"clock writer differs", with(func(s *NodeStatus) { s.Clock.Writer = "b" }), 0, false},
		{"waitFor met", okStatus(), 2, true},
		{"waitFor not met", okStatus(), 3, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			entries := []Entry{upEntry("a", okStatus()), upEntry("b", c.other)}
			if got := operational(entries, c.waitFor); got != c.want {
				t.Fatalf("operational = %v, want %v", got, c.want)
			}
		})
	}
	t.Run("no entries", func(t *testing.T) {
		if operational(nil, 0) {
			t.Fatal("operational over no entries should be false")
		}
		if c := counts(nil); c == nil || len(c) != 0 {
			t.Fatalf("counts over no entries = %#v, want an empty slice, not nil", c)
		}
		r := read(t, NewService(newFakeDir(), newFakeAsker(allOK), time.Second), 0, 0)
		if r.Operational || r.Counts == nil || r.Nodes == nil || len(r.Counts) != 0 || len(r.Nodes) != 0 {
			t.Fatalf("report over no members = %#v, want not operational with empty, non-nil counts and nodes", r)
		}
	})
	t.Run("a DOWN entry", func(t *testing.T) {
		if operational([]Entry{upEntry("a", okStatus()), downEntry("b", lifecycle.Normal)}, 0) {
			t.Fatal("operational with a DOWN entry should be false")
		}
	})
}

func TestRead_WaitAnswersWhenSecondNodeJoins(t *testing.T) {
	d := newFakeDir(member("a", lifecycle.Normal))
	a := newFakeAsker(allOK)
	s := NewService(d, a, time.Second)
	go func() {
		for a.called("a") == 0 {
			time.Sleep(5 * time.Millisecond)
		}
		d.add(member("b", lifecycle.Normal))
	}()
	var r Report
	dur := elapsed(func() { r = read(t, s, 2, 10*time.Second) })
	// Under the 1s re-ask interval: Changed() woke the wait, not the timer.
	if dur >= 900*time.Millisecond {
		t.Fatalf("Read took %s, want an answer on Changed(), before the re-ask", dur)
	}
	if !r.Operational || len(r.Nodes) != 2 {
		t.Fatalf("got %+v, want two operational nodes", r)
	}
}

func TestRead_WaitMetWithExtraDown(t *testing.T) {
	failC := func(_ context.Context, id string, _ int) (NodeStatus, error) {
		if id == "c" {
			return NodeStatus{}, errors.New("refused")
		}
		return okStatus(), nil
	}
	s := NewService(newFakeDir(member("a", lifecycle.Normal), member("b", lifecycle.Normal), member("c", lifecycle.Normal)), newFakeAsker(failC), time.Second)
	var r Report
	dur := elapsed(func() { r = read(t, s, 2, 10*time.Second) })
	if dur > time.Second {
		t.Fatalf("Read took %s, want an answer at once", dur)
	}
	if r.Operational {
		t.Fatal("Operational should be false with an extra DOWN member")
	}
	if len(r.Nodes) != 3 {
		t.Fatalf("got %d nodes, want 3", len(r.Nodes))
	}
}

func TestRead_TimeoutAnswersLastCountedRound(t *testing.T) {
	s := NewService(newFakeDir(member("a", lifecycle.Normal), member("b", lifecycle.Normal)), newFakeAsker(allOK), time.Second)
	var r Report
	dur := elapsed(func() { r = read(t, s, 3, 300*time.Millisecond) })
	if dur < 300*time.Millisecond || dur > 1500*time.Millisecond {
		t.Fatalf("Read took %s, want about 300ms", dur)
	}
	if r.Operational {
		t.Fatal("Operational should be false: waitFor not met")
	}
	want := []Entry{upEntry("a", okStatus()), upEntry("b", okStatus())}
	if !reflect.DeepEqual(r.Nodes, want) {
		t.Fatalf("got %+v, want %+v", r.Nodes, want)
	}
}

// TestRead_DeadlineCancelsRunningRound: a round whose asks are still out at the
// deadline is called off, and the answer is the last counted round.
func TestRead_DeadlineCancelsRunningRound(t *testing.T) {
	var startedLive atomic.Bool
	ask := func(ctx context.Context, _ string, call int) (NodeStatus, error) {
		if call == 1 {
			return okStatus(), nil
		}
		startedLive.Store(ctx.Err() == nil)
		<-ctx.Done()
		return NodeStatus{}, ctx.Err()
	}
	d := newFakeDir(member("a", lifecycle.Normal))
	a := newFakeAsker(ask)
	s := NewService(d, a, 10*time.Second)
	go func() {
		for a.called("a") < 1 {
			time.Sleep(5 * time.Millisecond)
		}
		d.add(Member{ID: "b"}) // fires Changed(): a second round starts and hangs on "a"
	}()
	var r Report
	dur := elapsed(func() { r = read(t, s, 2, 500*time.Millisecond) })
	if dur < 500*time.Millisecond || dur > 2*time.Second {
		t.Fatalf("Read took %s, want about 500ms", dur)
	}
	want := []Entry{upEntry("a", okStatus())}
	if !reflect.DeepEqual(r.Nodes, want) {
		t.Fatalf("got %+v, want the first round %+v", r.Nodes, want)
	}
	if n := a.called("a"); n != 2 {
		t.Fatalf("a asked %d times, want 2: the second round must have run and been cut", n)
	}
	if !startedLive.Load() {
		t.Fatal("the second round started after the deadline; want it running when the deadline cut it")
	}
}

// slowRefusalLog is a slog handler that holds the log line of a member that
// did not answer for delay: the time a round spends after its last ask
// returned and before it ends.
type slowRefusalLog struct {
	delay   time.Duration
	delayed *atomic.Int32
}

func (slowRefusalLog) Enabled(context.Context, slog.Level) bool { return true }
func (h slowRefusalLog) Handle(_ context.Context, r slog.Record) error {
	if r.Message == "cluster status: member did not answer" {
		h.delayed.Add(1)
		time.Sleep(h.delay)
	}
	return nil
}
func (h slowRefusalLog) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h slowRefusalLog) WithGroup(string) slog.Handler      { return h }

// TestRead_RoundCompletedAtDeadlineCounts: a round whose asks all returned
// before the deadline counts, although the round ends after it.
func TestRead_RoundCompletedAtDeadlineCounts(t *testing.T) {
	old := slog.Default()
	var delayed atomic.Int32
	slog.SetDefault(slog.New(slowRefusalLog{delay: 300 * time.Millisecond, delayed: &delayed}))
	defer slog.SetDefault(old)

	ask := func(ctx context.Context, id string, call int) (NodeStatus, error) {
		if id == "b" {
			if call == 2 {
				dl, ok := ctx.Deadline()
				if !ok {
					return NodeStatus{}, errors.New("the second round's ask has no deadline")
				}
				// b refuses just before the deadline; its log line ends the
				// round after the deadline.
				time.Sleep(time.Until(dl) - 150*time.Millisecond)
			}
			return NodeStatus{}, errors.New("refused")
		}
		s := okStatus()
		if call == 1 {
			s.State = lifecycle.Draining
		}
		return s, nil
	}
	s := NewService(newFakeDir(member("a", lifecycle.Normal), member("b", lifecycle.Normal)), newFakeAsker(ask), 10*time.Second)
	r := read(t, s, 1, 1600*time.Millisecond)
	want := []Entry{upEntry("a", okStatus()), downEntry("b", lifecycle.Normal)}
	if !reflect.DeepEqual(r.Nodes, want) {
		t.Fatalf("got %+v, want the completed second round %+v", r.Nodes, want)
	}
	// Both rounds must have held their end on b's log line, or the second round
	// ended before the deadline and the test proves nothing.
	if got := delayed.Load(); got != 2 {
		t.Fatalf("delayed log lines = %d, want 2 (one per round)", got)
	}
}

// TestRead_AnswerAfterDeadlineIsCut: an ask that returns after the deadline
// has passed is cut, even with an answer: the deadline may have shaped it,
// as it shapes the database check of the in-process ask.
func TestRead_AnswerAfterDeadlineIsCut(t *testing.T) {
	ask := func(ctx context.Context, _ string, call int) (NodeStatus, error) {
		s := okStatus()
		if call == 1 {
			s.State = lifecycle.Draining
			return s, nil
		}
		<-ctx.Done()
		s.Database = false
		return s, nil
	}
	s := NewService(newFakeDir(member("a", lifecycle.Normal)), newFakeAsker(ask), 10*time.Second)
	r := read(t, s, 1, 1300*time.Millisecond)
	first := okStatus()
	first.State = lifecycle.Draining
	want := []Entry{upEntry("a", first)}
	if !reflect.DeepEqual(r.Nodes, want) {
		t.Fatalf("got %+v, want the first round %+v", r.Nodes, want)
	}
}

func TestRead_FirstRoundOutlastsTimeout(t *testing.T) {
	ask := func(ctx context.Context, _ string, _ int) (NodeStatus, error) {
		select {
		case <-time.After(200 * time.Millisecond):
			return okStatus(), nil
		case <-ctx.Done():
			return NodeStatus{}, ctx.Err()
		}
	}
	s := NewService(newFakeDir(member("a", lifecycle.Normal), member("b", lifecycle.Normal)), newFakeAsker(ask), 5*time.Second)
	var r Report
	dur := elapsed(func() { r = read(t, s, 3, time.Millisecond) })
	if dur < 200*time.Millisecond || dur > 1500*time.Millisecond {
		t.Fatalf("Read took %s, want the first round's 200ms", dur)
	}
	want := []Entry{upEntry("a", okStatus()), upEntry("b", okStatus())}
	if !reflect.DeepEqual(r.Nodes, want) {
		t.Fatalf("got %+v, want the completed first round %+v", r.Nodes, want)
	}
}

func TestRead_ReasksOnIntervalWithoutChanged(t *testing.T) {
	ask := func(_ context.Context, _ string, call int) (NodeStatus, error) {
		s := okStatus()
		if call == 1 {
			s.State = lifecycle.Draining
		}
		return s, nil
	}
	s := NewService(newFakeDir(member("a", lifecycle.Normal)), newFakeAsker(ask), time.Second)
	var r Report
	dur := elapsed(func() { r = read(t, s, 1, 5*time.Second) })
	if dur < time.Second || dur > 3*time.Second {
		t.Fatalf("Read took %s, want about the 1s re-ask interval", dur)
	}
	if !r.Operational {
		t.Fatalf("got %+v, want operational after the re-ask", r)
	}
}

func TestRead_ClientGone(t *testing.T) {
	s := NewService(newFakeDir(member("a", lifecycle.Normal)), newFakeAsker(allOK), time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	_, err := s.Read(ctx, 3, 10*time.Second)
	if !errors.Is(err, common.ErrClientGone) || !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want ClientGone over context.Canceled", err)
	}
}

func TestRead_ClientGoneDuringRound(t *testing.T) {
	ask := func(ctx context.Context, _ string, _ int) (NodeStatus, error) {
		<-ctx.Done()
		return NodeStatus{}, ctx.Err()
	}
	s := NewService(newFakeDir(member("a", lifecycle.Normal)), newFakeAsker(ask), 10*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err := s.Read(ctx, 0, 0)
	if !errors.Is(err, common.ErrClientGone) {
		t.Fatalf("got %v, want ClientGone", err)
	}
}

func TestInProcess_SingleNode(t *testing.T) {
	lc := normalLifecycle()
	clk := Clock{Controllable: true, Version: 2, Writer: "local"}
	self := NewSelf(lc, &fakePinger{}, "v1", func() Clock { return clk }, time.Second)
	s := NewService(LocalDirectory(lc), InProcess(self), time.Second)

	r := read(t, s, 0, 0)
	want := Report{
		Operational: true,
		Counts:      []Count{{Up, lifecycle.Normal, 1}},
		Nodes:       []Entry{upEntry("local", NodeStatus{State: lifecycle.Normal, Database: true, Version: "v1", Clock: clk})},
	}
	if !reflect.DeepEqual(r, want) {
		t.Fatalf("got %+v, want %+v", r, want)
	}

	dur := elapsed(func() { r = read(t, s, 2, 300*time.Millisecond) })
	if dur < 300*time.Millisecond || dur > 1500*time.Millisecond {
		t.Fatalf("Read took %s, want about 300ms", dur)
	}
	if r.Operational {
		t.Fatal("waitFor 2 can never be met by one node")
	}
}

// TestRead_TimedOutWaitStartsNoRound: a wait that runs to the deadline answers
// the last counted round; it does not start a round the deadline has already
// cut.
func TestRead_TimedOutWaitStartsNoRound(t *testing.T) {
	ask := func(context.Context, string, int) (NodeStatus, error) {
		s := okStatus()
		s.State = lifecycle.Draining
		return s, nil
	}
	a := newFakeAsker(ask)
	s := NewService(newFakeDir(member("a", lifecycle.Normal)), a, time.Second)
	r := read(t, s, 1, 300*time.Millisecond)
	if r.Operational {
		t.Fatal("Operational should be false: waitFor not met")
	}
	if n := a.called("a"); n != 1 {
		t.Fatalf("a asked %d times, want 1: the wait ran to the deadline", n)
	}
	draining := okStatus()
	draining.State = lifecycle.Draining
	if want := []Entry{upEntry("a", draining)}; !reflect.DeepEqual(r.Nodes, want) {
		t.Fatalf("nodes %+v, want the first round %+v", r.Nodes, want)
	}
}
