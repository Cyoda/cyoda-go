package platformclock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"sort"
	"sync"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	"github.com/cyoda-platform/cyoda-go/internal/common"
)

const testPoll = time.Millisecond

// fakeScheduler is a ScheduledTaskStore whose only live method is
// SchedulerActivity. Each poll reports the tasks as they stand, then "fires"
// every task due on the platform clock by removing it and running onFire.
type fakeScheduler struct {
	spi.ScheduledTaskStore

	mu       sync.Mutex
	clock    *Test
	due      []time.Time
	running  int                           // polls still reporting Running
	fire     bool                          // whether due tasks fire at all
	onFire   func(e time.Time) []time.Time // returns tasks armed by the fire
	lastSeen time.Time                     // platform time at the latest poll
	polls    int
	failAt   int // from this poll on, SchedulerActivity fails with failErr
	failErr  error
	onPoll   func() // runs at the start of each poll, before any failure
}

func (f *fakeScheduler) SchedulerActivity(ctx context.Context) (spi.SchedulerActivityReport, error) {
	if spi.GetUserContext(ctx) != nil {
		return spi.SchedulerActivityReport{}, errors.New("cross-tenant call given a tenant context")
	}
	if f.onPoll != nil {
		f.onPoll()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.polls++
	if f.failAt > 0 && f.polls >= f.failAt {
		return spi.SchedulerActivityReport{}, f.failErr
	}
	now := f.clock.Now()
	f.lastSeen = now
	var rep spi.SchedulerActivityReport
	if f.running > 0 {
		f.running--
		rep.Running = true
	}
	sort.Slice(f.due, func(i, j int) bool { return f.due[i].Before(f.due[j]) })
	if len(f.due) > 0 {
		rep.HasWaiting = true
		rep.EarliestWaitingMs = f.due[0].UnixMilli()
	}
	if f.fire && !rep.Running {
		var keep []time.Time
		var armed []time.Time
		for _, e := range f.due {
			if e.After(now) {
				keep = append(keep, e)
				continue
			}
			if f.onFire != nil {
				armed = append(armed, f.onFire(e)...)
			}
		}
		f.due = append(keep, armed...)
	}
	return rep, nil
}

func (f *fakeScheduler) seen() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastSeen
}

// fakeStoreTime answers the store's consistency time.
type fakeStoreTime struct {
	answer func() time.Time
	err    error
	calls  int
}

func (f *fakeStoreTime) storeTime(context.Context) (time.Time, error) {
	f.calls++
	if f.err != nil {
		return time.Time{}, f.err
	}
	return f.answer(), nil
}

// confirmLog is a Confirm that records every setting it is given.
type confirmLog struct {
	mu      sync.Mutex
	sets    []Setting
	hook    func(Setting)
	failAt  int // the 1-based call that returns failErr; 0 never fails
	failErr error
	nodes   []string // the answer; ["n1"] when nil
}

func (p *confirmLog) confirm(_ context.Context, s Setting) ([]string, error) {
	n := func() int {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.sets = append(p.sets, s)
		return len(p.sets)
	}()
	if p.hook != nil {
		p.hook(s)
	}
	if p.failAt > 0 && n == p.failAt {
		return nil, p.failErr
	}
	if p.nodes == nil {
		return []string{"n1"}, nil
	}
	return p.nodes, nil
}

func (p *confirmLog) confirmed() []Setting {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Setting(nil), p.sets...)
}

func (p *confirmLog) starts() []time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]time.Time, len(p.sets))
	for i, s := range p.sets {
		out[i] = time.UnixMicro(s.StartUnixMicro)
	}
	return out
}

type fixture struct {
	src   *fakeSrc
	clock *Test
	sched *fakeScheduler
	tm    *fakeStoreTime
	conf  *confirmLog
	svc   *Service
	t0    time.Time // platform time at fixture creation
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	src := newFakeSrc()
	c := NewTest("n1", src.now)
	f := &fixture{
		src:   src,
		clock: c,
		sched: &fakeScheduler{clock: c, fire: true},
		tm:    &fakeStoreTime{answer: func() time.Time { return time.UnixMicro(0) }},
		conf:  &confirmLog{},
		t0:    c.Now(),
	}
	f.svc = NewService(c, f.tm.storeTime,
		func(ctx context.Context) (spi.ScheduledTaskStore, error) {
			if spi.GetUserContext(ctx) != nil {
				return nil, errors.New("cross-tenant lookup given a tenant context")
			}
			return f.sched, nil
		},
		f.conf.confirm, testPoll)
	return f
}

func ptr[T any](v T) *T { return &v }

func sameTimes(a, b []time.Time) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !a[i].Equal(b[i]) {
			return false
		}
	}
	return true
}

// A Service missing a collaborator is refused when it is built, not at the
// first change that needs it.
func TestNewService_NilCollaboratorPanics(t *testing.T) {
	c := NewTest("n1", newFakeSrc().now)
	storeTime := (&fakeStoreTime{}).storeTime
	tasks := func(context.Context) (spi.ScheduledTaskStore, error) { return nil, nil }
	confirm := (&confirmLog{}).confirm
	tests := map[string]func(){
		"a nil clock":      func() { NewService(nil, storeTime, tasks, confirm, testPoll) },
		"a nil store time": func() { NewService(c, nil, tasks, confirm, testPoll) },
		"nil tasks":        func() { NewService(c, storeTime, nil, confirm, testPoll) },
		"a nil confirm":    func() { NewService(c, storeTime, tasks, nil, testPoll) },
		"a zero poll":      func() { NewService(c, storeTime, tasks, confirm, 0) },
	}
	for name, build := range tests {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("NewService with %s did not panic", name)
				}
			}()
			build()
		})
	}
}

func TestMove_NoTasks_SetsTargetImmediately(t *testing.T) {
	f := newFixture(t)
	target := f.t0.Add(4 * time.Hour).Truncate(time.Millisecond)
	res, err := f.svc.Change(context.Background(), Change{At: &target})
	if err != nil {
		t.Fatalf("Change: %v", err)
	}
	if got := f.conf.starts(); !sameTimes(got, []time.Time{target}) {
		t.Fatalf("confirmed starts = %v, want [%v]", got, target)
	}
	if !res.State.Now.Equal(target) || !res.State.Controllable || res.State.RatePPM != 1_000_000 {
		t.Fatalf("state = %+v", res.State)
	}
}

func TestMove_AdvanceBy_TargetsNowPlusDuration(t *testing.T) {
	f := newFixture(t)
	_, err := f.svc.Change(context.Background(), Change{AdvanceByMs: ptr(int64(2 * time.Hour / time.Millisecond))})
	if err != nil {
		t.Fatalf("Change: %v", err)
	}
	if got := f.conf.starts(); !sameTimes(got, []time.Time{f.t0.Add(2 * time.Hour)}) {
		t.Fatalf("confirmed starts = %v", got)
	}
}

func TestMove_StepsThroughDueTimesInOrder(t *testing.T) {
	f := newFixture(t)
	base := f.t0.Truncate(time.Millisecond).Add(time.Millisecond)
	h1, h2, h3 := base.Add(time.Hour), base.Add(2*time.Hour), base.Add(3*time.Hour)
	target := base.Add(4 * time.Hour)
	f.sched.due = []time.Time{h1, h3}
	f.sched.onFire = func(e time.Time) []time.Time {
		if e.Equal(h1) {
			return []time.Time{h2}
		}
		return nil
	}
	if _, err := f.svc.Change(context.Background(), Change{At: &target}); err != nil {
		t.Fatalf("Change: %v", err)
	}
	want := []time.Time{h1, h2, h3, target}
	if got := f.conf.starts(); !sameTimes(got, want) {
		t.Fatalf("confirmed starts = %v, want %v", got, want)
	}
}

func TestMove_RateAppliedAtEnd(t *testing.T) {
	f := newFixture(t)
	base := f.t0.Truncate(time.Millisecond).Add(time.Millisecond)
	h1, target := base.Add(time.Hour), base.Add(2*time.Hour)
	f.sched.due = []time.Time{h1}
	res, err := f.svc.Change(context.Background(), Change{At: &target, RatePPM: ptr(int64(3_000_000))})
	if err != nil {
		t.Fatalf("Change: %v", err)
	}
	sets := f.conf.confirmed()
	if len(sets) != 2 || sets[0].RatePPM != 1_000_000 || sets[1].RatePPM != 3_000_000 ||
		sets[1].StartUnixMicro != target.UnixMicro() {
		t.Fatalf("confirmed = %+v", sets)
	}
	if res.State.RatePPM != 3_000_000 {
		t.Fatalf("state rate = %d", res.State.RatePPM)
	}
}

func TestMove_NeverSetsBackward(t *testing.T) {
	f := newFixture(t)
	base := f.t0.Truncate(time.Millisecond).Add(time.Millisecond)
	h1, h3 := base.Add(time.Hour), base.Add(3*time.Hour)
	target := base.Add(4 * time.Hour)
	f.sched.due = []time.Time{h1, h3}
	f.sched.onFire = func(e time.Time) []time.Time {
		if e.Equal(h3) {
			f.src.advance(2 * time.Hour) // the last fire runs past the target
		}
		return nil
	}
	f.conf.hook = func(s Setting) {
		if seen := f.sched.seen(); s.StartUnixMicro < seen.UnixMicro() {
			t.Errorf("confirmed start %v before the clock's time %v", time.UnixMicro(s.StartUnixMicro), seen)
		}
	}
	if _, err := f.svc.Change(context.Background(), Change{At: &target}); err != nil {
		t.Fatalf("Change: %v", err)
	}
	// The steps, then the fresh final setting where the last fire left the
	// clock: past the target.
	got := f.conf.starts()
	if len(got) != 3 || !sameTimes(got[:2], []time.Time{h1, h3}) || !got[2].After(target) {
		t.Fatalf("confirmed starts = %v, want [%v %v] then one past %v", got, h1, h3, target)
	}
	if !f.clock.Now().After(target) {
		t.Fatalf("clock %v not past target %v", f.clock.Now(), target)
	}
}

func TestMove_WaitsWhileRunning(t *testing.T) {
	f := newFixture(t)
	f.sched.running = 5
	target := f.t0.Add(time.Hour)
	f.conf.hook = func(Setting) {
		f.sched.mu.Lock()
		defer f.sched.mu.Unlock()
		if f.sched.running > 0 {
			t.Errorf("confirmed while %d running polls remain", f.sched.running)
		}
	}
	if _, err := f.svc.Change(context.Background(), Change{At: &target}); err != nil {
		t.Fatalf("Change: %v", err)
	}
	if got := f.conf.starts(); len(got) != 1 {
		t.Fatalf("confirmed starts = %v", got)
	}
}

func TestMove_CancelIsClientGone(t *testing.T) {
	f := newFixture(t)
	base := f.t0.Truncate(time.Millisecond).Add(time.Millisecond)
	f.sched.due = []time.Time{base.Add(time.Hour)}
	f.sched.fire = false
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.conf.hook = func(Setting) { cancel() }
	target := base.Add(2 * time.Hour)
	_, err := f.svc.Change(ctx, Change{At: &target})
	if !errors.Is(err, common.ErrClientGone) || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want ErrClientGone wrapping context.Canceled", err)
	}
}

// A cancellation that lands while the store is answering comes back as the
// store's own error; the move still reports it as the client's departure.
func TestMove_CancelDuringPollIsClientGone(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.sched.onPoll = cancel
	f.sched.failAt = 1
	f.sched.failErr = fmt.Errorf("query: %w", context.Canceled)
	target := f.t0.Add(time.Hour)
	_, err := f.svc.Change(ctx, Change{At: &target})
	if !errors.Is(err, common.ErrClientGone) || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want ErrClientGone wrapping context.Canceled", err)
	}
}

func TestMove_CancelDuringStoreLookupIsClientGone(t *testing.T) {
	src := newFakeSrc()
	c := NewTest("n1", src.now)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc := NewService(c, (&fakeStoreTime{}).storeTime,
		func(context.Context) (spi.ScheduledTaskStore, error) {
			cancel()
			return nil, fmt.Errorf("resolve: %w", context.Canceled)
		},
		(&confirmLog{}).confirm, testPoll)
	target := c.Now().Add(time.Hour)
	_, err := svc.Change(ctx, Change{At: &target})
	if !errors.Is(err, common.ErrClientGone) || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want ErrClientGone wrapping context.Canceled", err)
	}
}

func TestMove_CancelLeavesClock(t *testing.T) {
	f := newFixture(t)
	base := f.t0.Truncate(time.Millisecond).Add(time.Millisecond)
	h1, target := base.Add(time.Hour), base.Add(2*time.Hour)
	f.sched.due = []time.Time{h1}
	f.sched.fire = false // no scheduler runs: the due task stays pending
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.conf.hook = func(Setting) { cancel() }
	_, err := f.svc.Change(ctx, Change{At: &target})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got := f.conf.starts(); !sameTimes(got, []time.Time{h1}) {
		t.Fatalf("confirmed starts = %v", got)
	}
	if !f.clock.Now().Equal(h1) {
		t.Fatalf("clock = %v, want %v", f.clock.Now(), h1)
	}
}

func TestMove_SchedulerErrorFails(t *testing.T) {
	src := newFakeSrc()
	c := NewTest("n1", src.now)
	conf := &confirmLog{}
	boom := errors.New("store down")
	svc := NewService(c, (&fakeStoreTime{}).storeTime,
		func(context.Context) (spi.ScheduledTaskStore, error) { return nil, boom },
		conf.confirm, testPoll)
	target := c.Now().Add(time.Hour)
	if _, err := svc.Change(context.Background(), Change{At: &target}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if len(conf.starts()) != 0 {
		t.Fatal("confirmed despite the error")
	}
}

func TestSetBack_RefusedWhenRecordedLater(t *testing.T) {
	f := newFixture(t)
	f.src.advance(time.Second)
	recorded := f.clock.Now()
	f.tm.answer = func() time.Time { return recorded }
	prev := f.clock.Setting()
	at := f.t0.Add(-24 * time.Hour)
	_, err := f.svc.Change(context.Background(), Change{At: &at})
	if before := (*BeforeRecordedTimeError)(nil); !errors.As(err, &before) {
		t.Fatalf("err = %v, want *BeforeRecordedTimeError", err)
	}
	var be *BeforeRecordedTimeError
	if !errors.As(err, &be) || !be.Latest.Equal(recorded) {
		t.Fatalf("err = %#v, want Latest %v", err, recorded)
	}
	if got := f.clock.Setting(); got != prev {
		t.Fatalf("setting = %+v, want restored %+v", got, prev)
	}
	if !f.clock.Now().Equal(recorded) {
		t.Fatalf("clock = %v, want %v", f.clock.Now(), recorded)
	}
	if len(f.conf.starts()) != 0 {
		t.Fatal("confirmed a refused setting")
	}
}

func TestSetBack_ConsistencyTimeErrorRestores(t *testing.T) {
	f := newFixture(t)
	boom := errors.New("unavailable")
	f.tm.err = boom
	prev := f.clock.Setting()
	at := f.t0.Add(-time.Hour)
	if _, err := f.svc.Change(context.Background(), Change{At: &at}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if got := f.clock.Setting(); got != prev {
		t.Fatalf("setting = %+v, want restored %+v", got, prev)
	}
	if len(f.conf.starts()) != 0 {
		t.Fatal("confirmed despite the error")
	}
}

func TestSetBack_FreshStoreAccepted(t *testing.T) {
	f := newFixture(t)
	// A fresh store answers max(clock, floor 0) = the clock, after the call
	// took 50 ms at rate 2: within the tolerance.
	f.tm.answer = func() time.Time {
		f.src.advance(50 * time.Millisecond)
		return f.clock.Now()
	}
	at := time.Date(1970, 1, 1, 0, 0, 1, 0, time.UTC)
	res, err := f.svc.Change(context.Background(), Change{At: &at, RatePPM: ptr(int64(2_000_000))})
	if err != nil {
		t.Fatalf("Change: %v", err)
	}
	if got := f.conf.starts(); !sameTimes(got, []time.Time{at}) {
		t.Fatalf("confirmed starts = %v", got)
	}
	if want := at.Add(100 * time.Millisecond); !res.State.Now.Equal(want) {
		t.Fatalf("state now = %v, want %v", res.State.Now, want)
	}
}

func TestSetBack_ToleranceIsCallDurationTimesRate(t *testing.T) {
	f := newFixture(t)
	at := f.t0.Add(-time.Hour)
	// The store reports a time 1 µs beyond what the call's duration covers.
	f.tm.answer = func() time.Time {
		f.src.advance(50 * time.Millisecond)
		return at.Add(50*time.Millisecond + time.Microsecond)
	}
	if _, err := f.svc.Change(context.Background(), Change{At: &at}); !errors.As(err, new(*BeforeRecordedTimeError)) {
		t.Fatalf("err = %v, want *BeforeRecordedTimeError", err)
	}
}

func TestChange_TimeEqualNowWithRate_AppliesBoth(t *testing.T) {
	f := newFixture(t)
	f.tm.answer = func() time.Time { return f.clock.Now() }
	at := f.clock.Now()
	if _, err := f.svc.Change(context.Background(), Change{At: &at, RatePPM: ptr(int64(5_000_000))}); err != nil {
		t.Fatalf("Change: %v", err)
	}
	sets := f.conf.confirmed()
	if len(sets) != 1 || sets[0].StartUnixMicro != at.UnixMicro() || sets[0].RatePPM != 5_000_000 {
		t.Fatalf("confirmed = %+v", sets)
	}
	if f.tm.calls != 1 {
		t.Fatalf("ConsistencyTime calls = %d, want 1", f.tm.calls)
	}
}

func TestRateOnly_NoJump(t *testing.T) {
	f := newFixture(t)
	f.src.advance(10 * time.Second)
	before := f.clock.Now()
	res, err := f.svc.Change(context.Background(), Change{RatePPM: ptr(int64(2_000_000))})
	if err != nil {
		t.Fatalf("Change: %v", err)
	}
	if !res.State.Now.Equal(before) || res.State.RatePPM != 2_000_000 {
		t.Fatalf("state = %+v, want now %v rate 2x", res.State, before)
	}
	if got := f.conf.starts(); !sameTimes(got, []time.Time{before}) {
		t.Fatalf("confirmed starts = %v", got)
	}
	f.src.advance(time.Second)
	if want := before.Add(2 * time.Second); !f.clock.Now().Equal(want) {
		t.Fatalf("clock = %v, want %v", f.clock.Now(), want)
	}
	if f.tm.calls != 0 {
		t.Fatalf("rate-only change asked the store")
	}
}

func TestStartupCheck_AheadFails(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	tm := &fakeStoreTime{answer: func() time.Time { return now.Add(time.Second + time.Microsecond) }}
	if err := StartupCheck(context.Background(), tm.storeTime, now); err == nil {
		t.Fatal("expected a startup error")
	}
}

func TestStartupCheck_BehindPasses(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	for _, c := range []time.Time{now.Add(-48 * time.Hour), now.Add(time.Second)} {
		tm := &fakeStoreTime{answer: func() time.Time { return c }}
		if err := StartupCheck(context.Background(), tm.storeTime, now); err != nil {
			t.Fatalf("latest recorded %v: %v", c, err)
		}
	}
}

// A fresh store has recorded nothing: the zero time.
func TestStartupCheck_FreshStorePasses(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	tm := &fakeStoreTime{answer: func() time.Time { return time.Time{} }}
	if err := StartupCheck(context.Background(), tm.storeTime, now); err != nil {
		t.Fatalf("fresh store: %v", err)
	}
}

func TestStartupCheck_StoreErrorFails(t *testing.T) {
	boom := errors.New("down")
	if err := StartupCheck(context.Background(), (&fakeStoreTime{err: boom}).storeTime, time.Unix(0, 0)); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
}

// steppingSrc advances by step on every read, so the clock moves between any
// two reads the service makes.
type steppingSrc struct {
	mu   sync.Mutex
	base time.Time
	off  time.Duration
	step time.Duration
}

func (s *steppingSrc) now() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.off += s.step
	return s.base.Add(s.off)
}

// TestMove_StepSkippedWhenClockPassedDueTime: the clock runs past e between
// the activity read and the set; setting it to e then would move it back.
// Each k places the due time at a different read, so some k lands e between
// the read that found it not yet due and the step.
func TestMove_StepSkippedWhenClockPassedDueTime(t *testing.T) {
	const step = time.Minute
	for k := 1; k <= 8; k++ {
		src := &steppingSrc{base: time.Now(), step: step} // realtime: monotonic base for the fake source
		c := NewTest("n1", src.now)
		t0 := c.Now()
		sched := &fakeScheduler{clock: c, fire: true,
			due: []time.Time{t0.Truncate(time.Millisecond).Add(time.Duration(k) * step)}}
		prev := c.Setting()
		var backward, stale []string
		confirm := func(_ context.Context, s Setting) ([]string, error) {
			// The previous setting's reading at the new anchor instant.
			was := prev.StartUnixMicro + (s.AnchorUnixMicro-prev.AnchorUnixMicro)*prev.RatePPM/1_000_000
			if s.StartUnixMicro < was {
				backward = append(backward, time.UnixMicro(s.StartUnixMicro).String())
			}
			if s.Version <= prev.Version {
				stale = append(stale, fmt.Sprintf("version %d after %d", s.Version, prev.Version))
			}
			prev = s
			return []string{"n1"}, nil
		}
		svc := NewService(c, (&fakeStoreTime{}).storeTime,
			func(context.Context) (spi.ScheduledTaskStore, error) { return sched, nil }, confirm, testPoll)
		target := t0.Add(10 * time.Hour)
		if _, err := svc.Change(context.Background(), Change{At: &target}); err != nil {
			t.Fatalf("k=%d: Change: %v", k, err)
		}
		if len(backward) > 0 {
			t.Errorf("k=%d: confirmed backward starts %v", k, backward)
		}
		if len(stale) > 0 {
			t.Errorf("k=%d: confirmed versions not strictly increasing: %v", k, stale)
		}
		if got := c.Setting(); got != prev {
			t.Errorf("k=%d: clock holds %+v, want the last confirmed %+v", k, got, prev)
		}
	}
}

func TestMove_SchedulerErrorMidMove(t *testing.T) {
	f := newFixture(t)
	base := f.t0.Truncate(time.Millisecond).Add(time.Millisecond)
	h1, h2, target := base.Add(time.Hour), base.Add(2*time.Hour), base.Add(3*time.Hour)
	f.sched.due = []time.Time{h1, h2}
	boom := errors.New("store down")
	f.sched.failAt = 3 // poll 1: step to h1; poll 2: h1 due, fired; poll 3: fails
	f.sched.failErr = boom
	_, err := f.svc.Change(context.Background(), Change{At: &target, RatePPM: ptr(int64(2_000_000))})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want wrapped %v", err, boom)
	}
	sets := f.conf.confirmed()
	if len(sets) != 1 || sets[0].StartUnixMicro != h1.UnixMicro() || sets[0].RatePPM != 1_000_000 {
		t.Fatalf("confirmed = %+v, want one complete step to %v at the old rate", sets, h1)
	}
	if got := f.clock.Setting(); got != sets[0] {
		t.Fatalf("clock setting = %+v, want the last confirmed %+v", got, sets[0])
	}
}

type capturedExchange struct{ local func() []byte }

func (e *capturedExchange) SetStateExchange(l func() []byte, _ func([]byte)) { e.local = l }

func servedSetting(t *testing.T, ex *capturedExchange) *Setting {
	t.Helper()
	raw := ex.local()
	if raw == nil {
		return nil
	}
	var s Setting
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("served state undecodable: %v", err)
	}
	return &s
}

// TestSetBack_PendingSettingNeverServed: while a set-back waits for the
// store, and after it is refused, the state exchange serves only the setting
// last confirmed, never the pending one.
func TestSetBack_PendingSettingNeverServed(t *testing.T) {
	f := newFixture(t)
	ex := &capturedExchange{}
	Attach(f.clock, ex)

	if _, err := f.svc.Change(context.Background(), Change{RatePPM: ptr(int64(2_000_000))}); err != nil {
		t.Fatalf("rate change: %v", err)
	}
	confirmed := f.clock.Setting()
	if got := servedSetting(t, ex); got == nil || *got != confirmed {
		t.Fatalf("served %+v, want the confirmed %+v", got, confirmed)
	}

	called := make(chan struct{})
	release := make(chan time.Time)
	f.tm.answer = func() time.Time {
		close(called)
		return <-release
	}
	done := make(chan error, 1)
	at := f.t0.Add(-time.Hour)
	go func() {
		_, err := f.svc.Change(context.Background(), Change{At: &at})
		done <- err
	}()
	<-called
	if pending := f.clock.Setting(); pending == confirmed {
		t.Fatal("set-back not applied locally before the store call")
	}
	if got := servedSetting(t, ex); got == nil || *got != confirmed {
		t.Fatalf("served %+v while the set-back is pending, want %+v", got, confirmed)
	}
	release <- f.clock.Now().Add(time.Hour) // the store holds a later time
	if err := <-done; !errors.As(err, new(*BeforeRecordedTimeError)) {
		t.Fatalf("err = %v, want *BeforeRecordedTimeError", err)
	}
	if got := servedSetting(t, ex); got == nil || *got != confirmed {
		t.Fatalf("served %+v after the refusal, want %+v", got, confirmed)
	}
}

// TestSetBack_PendingOnFreshClockServesNothing: before anything is
// confirmed the exchange serves nothing, even while a set-back is pending;
// once accepted and confirmed, it serves that setting.
func TestSetBack_PendingOnFreshClockServesNothing(t *testing.T) {
	f := newFixture(t)
	ex := &capturedExchange{}
	Attach(f.clock, ex)
	f.tm.answer = func() time.Time {
		if raw := ex.local(); raw != nil {
			t.Errorf("served %q while the first set-back is pending", raw)
		}
		return f.clock.Now()
	}
	at := f.t0.Add(-time.Hour)
	if _, err := f.svc.Change(context.Background(), Change{At: &at}); err != nil {
		t.Fatalf("Change: %v", err)
	}
	if got := servedSetting(t, ex); got == nil || *got != f.clock.Setting() {
		t.Fatalf("served %+v after the confirm, want %+v", got, f.clock.Setting())
	}
}

func TestClassify_ClientGoneKeepsMarker(t *testing.T) {
	e := classify(common.ClientGone(context.Canceled))
	if !errors.Is(e.Err, common.ErrClientGone) {
		t.Fatalf("classified error lost the client-gone marker: %v", e.Err)
	}
}

// A move runs with the user context of its request stripped: the scheduled-task
// store is read across tenants (the fakes fail on any user context), and
// cancelling the request still stops the move.
func TestMove_StripsUserContext(t *testing.T) {
	f := newFixture(t)
	base := f.t0.Truncate(time.Millisecond).Add(time.Millisecond)
	f.sched.due = []time.Time{base.Add(time.Hour)}
	ctx := spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID: "u", Tenant: spi.Tenant{ID: "t"},
	})
	target := base.Add(2 * time.Hour)
	if _, err := f.svc.Change(ctx, Change{At: &target}); err != nil {
		t.Fatalf("Change with a user context: %v", err)
	}
}

func TestMove_CancelStopsWithUserContext(t *testing.T) {
	f := newFixture(t)
	base := f.t0.Truncate(time.Millisecond).Add(time.Millisecond)
	f.sched.due = []time.Time{base.Add(time.Hour)}
	f.sched.fire = false
	ctx, cancel := context.WithCancel(spi.WithUserContext(context.Background(),
		&spi.UserContext{UserID: "u", Tenant: spi.Tenant{ID: "t"}}))
	f.conf.hook = func(Setting) { cancel() }
	target := base.Add(2 * time.Hour)
	_, err := f.svc.Change(ctx, Change{At: &target})
	if !errors.Is(err, common.ErrClientGone) || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want ErrClientGone wrapping context.Canceled", err)
	}
}

// A task due exactly at the target fires at the target, and the clock runs on
// while it fires; the final step then finds the clock past the target and must
// not move it back.
func TestMove_DueAtTarget_DoesNotMoveBack(t *testing.T) {
	f := newFixture(t)
	target := f.t0.Truncate(time.Millisecond).Add(time.Hour)
	f.sched.due = []time.Time{target}
	f.sched.onFire = func(time.Time) []time.Time { f.src.advance(time.Millisecond); return nil }
	if _, err := f.svc.Change(context.Background(), Change{At: &target}); err != nil {
		t.Fatalf("Change: %v", err)
	}
	// The step to the target, then a fresh final setting where the fire left
	// the clock, never back at the target.
	sets := f.conf.confirmed()
	if len(sets) < 2 || sets[0].StartUnixMicro != target.UnixMicro() {
		t.Fatalf("confirmed = %+v, want them to begin at %v", sets, target)
	}
	last := sets[len(sets)-1]
	if last.StartUnixMicro < target.Add(time.Millisecond).UnixMicro() || last.Version <= sets[0].Version {
		t.Fatalf("last confirmed = %+v, want a newer setting no earlier than %v", last, target.Add(time.Millisecond))
	}
	if f.clock.Now().Before(target.Add(time.Millisecond)) {
		t.Fatalf("clock = %v, behind the fire at %v", f.clock.Now(), target.Add(time.Millisecond))
	}
}

func TestMove_DueAtTarget_WithRate_RateAppliesAtEnd(t *testing.T) {
	f := newFixture(t)
	target := f.t0.Truncate(time.Millisecond).Add(time.Hour)
	f.sched.due = []time.Time{target}
	f.sched.onFire = func(time.Time) []time.Time { f.src.advance(time.Millisecond); return nil }
	res, err := f.svc.Change(context.Background(), Change{At: &target, RatePPM: ptr(int64(3_000_000))})
	if err != nil {
		t.Fatalf("Change: %v", err)
	}
	sets := f.conf.confirmed()
	if len(sets) < 2 {
		t.Fatalf("confirmed = %+v, want the step and the rate", sets)
	}
	if sets[0].StartUnixMicro != target.UnixMicro() || sets[0].RatePPM != 1_000_000 {
		t.Fatalf("first confirmed = %+v, want the step at the target", sets[0])
	}
	last := sets[len(sets)-1]
	if last.RatePPM != 3_000_000 || last.StartUnixMicro < target.Add(time.Millisecond).UnixMicro() {
		t.Fatalf("last confirmed = %+v, want rate 3000000 starting no earlier than the fire", last)
	}
	if res.State.RatePPM != 3_000_000 || res.State.Now.Before(target.Add(time.Millisecond)) {
		t.Fatalf("state = %+v", res.State)
	}
}

// Repeating a cancelled move continues from where the last step put the clock.
func TestMove_RepeatAfterCancelContinues(t *testing.T) {
	f := newFixture(t)
	base := f.t0.Truncate(time.Millisecond).Add(time.Millisecond)
	h1, h2, h3 := base.Add(time.Hour), base.Add(2*time.Hour), base.Add(3*time.Hour)
	target := base.Add(4 * time.Hour)
	f.sched.due = []time.Time{h1, h2, h3}
	f.sched.fire = false
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.conf.hook = func(Setting) { cancel() }
	if _, err := f.svc.Change(ctx, Change{At: &target}); !errors.Is(err, context.Canceled) {
		t.Fatalf("first move err = %v, want context.Canceled", err)
	}
	if !f.clock.Now().Equal(h1) {
		t.Fatalf("clock after cancel = %v, want %v", f.clock.Now(), h1)
	}
	f.conf.hook = nil
	func() {
		f.sched.mu.Lock()
		defer f.sched.mu.Unlock()
		f.sched.fire = true
	}()
	if _, err := f.svc.Change(context.Background(), Change{At: &target}); err != nil {
		t.Fatalf("repeated move: %v", err)
	}
	want := []time.Time{h1, h2, h3, target}
	if got := f.conf.starts(); !sameTimes(got, want) {
		t.Fatalf("confirmed starts = %v, want %v", got, want)
	}
	if !f.clock.Now().Equal(target) {
		t.Fatalf("clock = %v, want %v", f.clock.Now(), target)
	}
}

func TestSetBack_ClientLeavingDuringStoreCallIsClientGone(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.svc.storeTime = func(ctx context.Context) (time.Time, error) {
		cancel()
		return time.Time{}, fmt.Errorf("store call: %w", context.Canceled)
	}
	prev := f.clock.Setting()
	at := f.t0.Add(-time.Hour)
	_, err := f.svc.Change(ctx, Change{At: &at})
	if !errors.Is(err, common.ErrClientGone) {
		t.Fatalf("err = %v, want ErrClientGone", err)
	}
	if got := f.clock.Setting(); got != prev {
		t.Fatalf("setting = %+v, want restored %+v", got, prev)
	}
	if len(f.conf.starts()) != 0 {
		t.Fatal("confirmed despite the departure")
	}
}

// The service resolves advanceByMs against the clock it moves, so it is the
// one that refuses a target past the year 9000: nothing changes and nothing
// is confirmed.
func TestChange_AdvancePastYear9000_Refused(t *testing.T) {
	f := newFixture(t)
	near := time.Date(9000, 12, 31, 23, 59, 0, 0, time.UTC)
	if _, err := f.svc.Change(context.Background(), Change{At: &near}); err != nil {
		t.Fatalf("set near the end: %v", err)
	}
	confirmed := len(f.conf.starts())
	ms := int64(2 * time.Minute / time.Millisecond)
	_, err := f.svc.Change(context.Background(), Change{AdvanceByMs: &ms})
	if !errors.As(err, new(*OutOfRangeError)) {
		t.Fatalf("err = %v, want *OutOfRangeError", err)
	}
	if got := len(f.conf.starts()); got != confirmed {
		t.Fatalf("confirmed %d settings after the refusal, want %d", got, confirmed)
	}
	if f.clock.Now().After(maxSetTime) {
		t.Fatalf("clock = %v, beyond the year 9000", f.clock.Now())
	}
}

// The year-9000 limit applies to what a change sets or moves to; a rate alone
// re-anchors at now, which may lie past it once the clock has run on.
func TestChange_RateOnlyPastSetLimit_Applies(t *testing.T) {
	f := newFixture(t)
	end := maxSetTime
	if _, err := f.svc.Change(context.Background(), Change{At: &end}); err != nil {
		t.Fatalf("set the limit: %v", err)
	}
	f.src.advance(2 * time.Second)
	confirmed := len(f.conf.starts())
	rate := int64(2_000_000)
	if _, err := f.svc.Change(context.Background(), Change{RatePPM: &rate}); err != nil {
		t.Fatalf("rate change: %v", err)
	}
	if got := len(f.conf.starts()); got != confirmed+1 {
		t.Fatalf("confirmed %d settings, want %d", got, confirmed+1)
	}
}

// The largest advance a body can carry is refused as past the year 9000; the
// check runs before any arithmetic that could overflow.
func TestChange_HugeAdvance_Refused(t *testing.T) {
	f := newFixture(t)
	confirmed := len(f.conf.starts())
	ms := int64(math.MaxInt64)
	_, err := f.svc.Change(context.Background(), Change{AdvanceByMs: &ms})
	if !errors.As(err, new(*OutOfRangeError)) {
		t.Fatalf("err = %v, want *OutOfRangeError", err)
	}
	if got := len(f.conf.starts()); got != confirmed {
		t.Fatalf("confirmed %d settings after the refusal, want %d", got, confirmed)
	}
}

// An advance longer than a Go duration (about 292 years) is accepted when its
// target lies before the end of the year 9000.
func TestChange_AdvanceLongerThanDuration_Applies(t *testing.T) {
	f := newFixture(t)
	ms := int64(10_000_000_000_000) // about 317 years
	if _, err := f.svc.Change(context.Background(), Change{AdvanceByMs: &ms}); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if years := f.clock.Now().Year() - f.t0.Year(); years < 316 || years > 318 {
		t.Fatalf("clock moved %d years, to %v from %v; want about 317", years, f.clock.Now(), f.t0)
	}
}

// A change answers the nodes the confirmation names.
func TestChange_ReturnsConfirmedNodes(t *testing.T) {
	f := newFixture(t)
	f.conf.nodes = []string{"a", "n1"}
	res, err := f.svc.Change(context.Background(), Change{RatePPM: ptr(int64(2_000_000))})
	if err != nil {
		t.Fatalf("Change: %v", err)
	}
	if len(res.Nodes) != 2 || res.Nodes[0] != "a" || res.Nodes[1] != "n1" {
		t.Fatalf("nodes = %v, want [a n1]", res.Nodes)
	}
}

// A change some node does not confirm fails, and nothing is rolled back: the
// handling node keeps the setting it installed.
func TestChange_NotConfirmed_KeepsSetting(t *testing.T) {
	f := newFixture(t)
	f.conf.failAt = 1
	f.conf.failErr = &NotConfirmedError{Nodes: []string{"a"}}
	res, err := f.svc.Change(context.Background(), Change{RatePPM: ptr(int64(2_000_000))})
	var nc *NotConfirmedError
	if !errors.As(err, &nc) {
		t.Fatalf("err = %v, want *NotConfirmedError", err)
	}
	if got := f.clock.Setting().RatePPM; got != 2_000_000 {
		t.Fatalf("rate = %d, want the installed 2000000 kept", got)
	}
	if res.Nodes != nil {
		t.Fatalf("nodes = %v on an error, want nil", res.Nodes)
	}
}

// A step some node does not confirm stops the move there: the clock stays at
// the step, and the move does not wait for the step's tasks.
func TestMove_StepNotConfirmed_StopsThere(t *testing.T) {
	f := newFixture(t)
	base := f.t0.Truncate(time.Millisecond).Add(time.Millisecond)
	h1, h2, h3 := base.Add(time.Hour), base.Add(2*time.Hour), base.Add(3*time.Hour)
	f.sched.due = []time.Time{h1, h2}
	f.conf.failAt = 1
	f.conf.failErr = &NotConfirmedError{Nodes: []string{"a"}}
	polls := -1
	f.conf.hook = func(Setting) {
		f.sched.mu.Lock()
		defer f.sched.mu.Unlock()
		polls = f.sched.polls
	}
	_, err := f.svc.Change(context.Background(), Change{At: &h3})
	if !errors.As(err, new(*NotConfirmedError)) {
		t.Fatalf("err = %v, want *NotConfirmedError", err)
	}
	if !f.clock.Now().Equal(h1) {
		t.Fatalf("clock = %v, want %v", f.clock.Now(), h1)
	}
	if n := len(f.conf.confirmed()); n != 1 {
		t.Fatalf("confirm calls = %d, want 1", n)
	}
	f.sched.mu.Lock()
	defer f.sched.mu.Unlock()
	if f.sched.polls != polls {
		t.Fatalf("scheduler polled %d times after the failed confirm (%d before)", f.sched.polls-polls, polls)
	}
}

// msgLog records the messages of the log records it handles.
type msgLog struct {
	slog.Handler
	mu   sync.Mutex
	msgs []string
}

func (m *msgLog) Enabled(context.Context, slog.Level) bool { return true }
func (m *msgLog) Handle(_ context.Context, r slog.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.msgs = append(m.msgs, r.Message)
	return nil
}

func (m *msgLog) count(msg string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, s := range m.msgs {
		if s == msg {
			n++
		}
	}
	return n
}

// A move that fails to confirm logs where it stopped, whether the failed
// confirmation is a step's or the move's final setting.
func TestMove_NotConfirmed_LogsStopped(t *testing.T) {
	cases := map[string]bool{"step": true, "final": false}
	for name, withStep := range cases {
		t.Run(name, func(t *testing.T) {
			ml := &msgLog{Handler: slog.NewTextHandler(io.Discard, nil)}
			prev := slog.Default()
			slog.SetDefault(slog.New(ml))
			defer slog.SetDefault(prev)

			f := newFixture(t)
			base := f.t0.Truncate(time.Millisecond).Add(time.Millisecond)
			h1, target := base.Add(time.Hour), base.Add(2*time.Hour)
			if withStep {
				f.sched.due = []time.Time{h1}
			}
			f.conf.failAt = 1
			f.conf.failErr = &NotConfirmedError{Nodes: []string{"a"}}
			_, err := f.svc.Change(context.Background(), Change{At: &target})
			if !errors.As(err, new(*NotConfirmedError)) {
				t.Fatalf("err = %v, want *NotConfirmedError", err)
			}
			if n := ml.count("platform clock move stopped"); n != 1 {
				t.Fatalf("%d \"platform clock move stopped\" lines, want 1", n)
			}
		})
	}
}

// A move confirms each step before it waits for that step's tasks: every poll
// after the first confirm sees the clock on the setting last confirmed.
func TestMove_ConfirmsEachStepBeforeWaiting(t *testing.T) {
	f := newFixture(t)
	base := f.t0.Truncate(time.Millisecond).Add(time.Millisecond)
	h1, h2 := base.Add(time.Hour), base.Add(2*time.Hour)
	f.sched.due = []time.Time{h1, h2}
	f.sched.onPoll = func() {
		sets := f.conf.confirmed()
		if len(sets) == 0 {
			return
		}
		if got, last := f.clock.Setting(), sets[len(sets)-1]; got != last {
			t.Errorf("polled on %+v, not the last confirmed %+v", got, last)
		}
	}
	target := base.Add(3 * time.Hour)
	if _, err := f.svc.Change(context.Background(), Change{At: &target}); err != nil {
		t.Fatalf("Change: %v", err)
	}
	if got := f.conf.starts(); !sameTimes(got, []time.Time{h1, h2, target}) {
		t.Fatalf("confirmed starts = %v", got)
	}
}

// A move whose target the clock has already passed when it ends installs and
// confirms a fresh setting of its own at the current time.
func TestMove_TargetPassed_InstallsFreshFinalSetting(t *testing.T) {
	f := newFixture(t)
	f.sched.onPoll = func() { f.src.advance(time.Second) }
	res, err := f.svc.Change(context.Background(), Change{AdvanceByMs: ptr(int64(1))})
	if err != nil {
		t.Fatalf("Change: %v", err)
	}
	now := f.clock.Now()
	sets := f.conf.confirmed()
	if len(sets) != 1 {
		t.Fatalf("confirmed = %+v, want one fresh setting", sets)
	}
	s := sets[0]
	if s.Version != 1 || s.Writer != "n1" {
		t.Fatalf("confirmed = %+v, want version 1 by n1", s)
	}
	if d := now.Sub(time.UnixMicro(s.StartUnixMicro)); d < 0 || d > time.Millisecond {
		t.Fatalf("confirmed start %v, want within 1 ms of %v", time.UnixMicro(s.StartUnixMicro), now)
	}
	if len(res.Nodes) != 1 || res.Nodes[0] != "n1" {
		t.Fatalf("nodes = %v, want [n1]", res.Nodes)
	}
}

// Every confirmed setting is one this node installed in the change: written by
// it, with a version above every earlier one, never the starting version 0.
func TestChange_EveryConfirmedSettingIsFresh(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.svc.Change(ctx, Change{RatePPM: ptr(int64(2_000_000))}); err != nil {
		t.Fatalf("rate change: %v", err)
	}
	back := f.t0.Add(-time.Hour)
	if _, err := f.svc.Change(ctx, Change{At: &back}); err != nil {
		t.Fatalf("set-back: %v", err)
	}
	h1 := f.clock.Now().Truncate(time.Millisecond).Add(time.Hour)
	func() {
		f.sched.mu.Lock()
		defer f.sched.mu.Unlock()
		f.sched.due = []time.Time{h1}
	}()
	target := h1.Add(time.Hour)
	if _, err := f.svc.Change(ctx, Change{At: &target}); err != nil {
		t.Fatalf("move: %v", err)
	}
	f.sched.onPoll = func() { f.src.advance(time.Second) }
	if _, err := f.svc.Change(ctx, Change{AdvanceByMs: ptr(int64(1))}); err != nil {
		t.Fatalf("advance past its target: %v", err)
	}
	sets := f.conf.confirmed()
	if len(sets) < 5 {
		t.Fatalf("confirmed %d settings, want at least 5: %+v", len(sets), sets)
	}
	for i, s := range sets {
		if s.Writer != "n1" || s.Version != uint64(i+1) {
			t.Fatalf("confirmed[%d] = %+v, want version %d by n1", i, s, i+1)
		}
	}
}
