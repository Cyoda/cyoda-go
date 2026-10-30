package platformclock

import (
	"context"
	"encoding/json"
	"errors"
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
}

func (f *fakeScheduler) SchedulerActivity(ctx context.Context) (spi.SchedulerActivityReport, error) {
	if spi.GetUserContext(ctx) != nil {
		return spi.SchedulerActivityReport{}, errors.New("cross-tenant call given a tenant context")
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

type publishLog struct {
	mu   sync.Mutex
	sets []Setting
	hook func(Setting)
}

func (p *publishLog) publish(s Setting) {
	func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.sets = append(p.sets, s)
	}()
	if p.hook != nil {
		p.hook(s)
	}
}

func (p *publishLog) starts() []time.Time {
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
	pub   *publishLog
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
		pub:   &publishLog{},
		t0:    c.Now(),
	}
	f.svc = NewService(c, f.tm.storeTime,
		func(ctx context.Context) (spi.ScheduledTaskStore, error) {
			if spi.GetUserContext(ctx) != nil {
				return nil, errors.New("cross-tenant lookup given a tenant context")
			}
			return f.sched, nil
		},
		f.pub.publish, testPoll)
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

func TestMove_NoTasks_SetsTargetImmediately(t *testing.T) {
	f := newFixture(t)
	target := f.t0.Add(4 * time.Hour).Truncate(time.Millisecond)
	st, err := f.svc.Change(context.Background(), Change{At: &target})
	if err != nil {
		t.Fatalf("Change: %v", err)
	}
	if got := f.pub.starts(); !sameTimes(got, []time.Time{target}) {
		t.Fatalf("published starts = %v, want [%v]", got, target)
	}
	if !st.Now.Equal(target) || !st.Controllable || st.RatePPM != 1_000_000 {
		t.Fatalf("state = %+v", st)
	}
}

func TestMove_AdvanceBy_TargetsNowPlusDuration(t *testing.T) {
	f := newFixture(t)
	_, err := f.svc.Change(context.Background(), Change{AdvanceBy: ptr(2 * time.Hour)})
	if err != nil {
		t.Fatalf("Change: %v", err)
	}
	if got := f.pub.starts(); !sameTimes(got, []time.Time{f.t0.Add(2 * time.Hour)}) {
		t.Fatalf("published starts = %v", got)
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
	if got := f.pub.starts(); !sameTimes(got, want) {
		t.Fatalf("published starts = %v, want %v", got, want)
	}
}

func TestMove_RateAppliedAtEnd(t *testing.T) {
	f := newFixture(t)
	base := f.t0.Truncate(time.Millisecond).Add(time.Millisecond)
	h1, target := base.Add(time.Hour), base.Add(2*time.Hour)
	f.sched.due = []time.Time{h1}
	st, err := f.svc.Change(context.Background(), Change{At: &target, RatePPM: ptr(int64(3_000_000))})
	if err != nil {
		t.Fatalf("Change: %v", err)
	}
	f.pub.mu.Lock()
	sets := append([]Setting(nil), f.pub.sets...)
	f.pub.mu.Unlock()
	if len(sets) != 2 || sets[0].RatePPM != 1_000_000 || sets[1].RatePPM != 3_000_000 ||
		sets[1].StartUnixMicro != target.UnixMicro() {
		t.Fatalf("published = %+v", sets)
	}
	if st.RatePPM != 3_000_000 {
		t.Fatalf("state rate = %d", st.RatePPM)
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
	f.pub.hook = func(s Setting) {
		if seen := f.sched.seen(); s.StartUnixMicro < seen.UnixMicro() {
			t.Errorf("published start %v before the clock's time %v", time.UnixMicro(s.StartUnixMicro), seen)
		}
	}
	if _, err := f.svc.Change(context.Background(), Change{At: &target}); err != nil {
		t.Fatalf("Change: %v", err)
	}
	if got := f.pub.starts(); !sameTimes(got, []time.Time{h1, h3}) {
		t.Fatalf("published starts = %v, want [%v %v]", got, h1, h3)
	}
	if !f.clock.Now().After(target) {
		t.Fatalf("clock %v not past target %v", f.clock.Now(), target)
	}
}

func TestMove_WaitsWhileRunning(t *testing.T) {
	f := newFixture(t)
	f.sched.running = 5
	target := f.t0.Add(time.Hour)
	f.pub.hook = func(Setting) {
		f.sched.mu.Lock()
		defer f.sched.mu.Unlock()
		if f.sched.running > 0 {
			t.Errorf("published while %d running polls remain", f.sched.running)
		}
	}
	if _, err := f.svc.Change(context.Background(), Change{At: &target}); err != nil {
		t.Fatalf("Change: %v", err)
	}
	if got := f.pub.starts(); len(got) != 1 {
		t.Fatalf("published starts = %v", got)
	}
}

func TestMove_CancelIsClientGone(t *testing.T) {
	f := newFixture(t)
	base := f.t0.Truncate(time.Millisecond).Add(time.Millisecond)
	f.sched.due = []time.Time{base.Add(time.Hour)}
	f.sched.fire = false
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.pub.hook = func(Setting) { cancel() }
	target := base.Add(2 * time.Hour)
	_, err := f.svc.Change(ctx, Change{At: &target})
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
	f.pub.hook = func(Setting) { cancel() }
	_, err := f.svc.Change(ctx, Change{At: &target})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got := f.pub.starts(); !sameTimes(got, []time.Time{h1}) {
		t.Fatalf("published starts = %v", got)
	}
	if !f.clock.Now().Equal(h1) {
		t.Fatalf("clock = %v, want %v", f.clock.Now(), h1)
	}
}

func TestMove_SchedulerErrorFails(t *testing.T) {
	src := newFakeSrc()
	c := NewTest("n1", src.now)
	pub := &publishLog{}
	boom := errors.New("store down")
	svc := NewService(c, (&fakeStoreTime{}).storeTime,
		func(context.Context) (spi.ScheduledTaskStore, error) { return nil, boom },
		pub.publish, testPoll)
	target := c.Now().Add(time.Hour)
	if _, err := svc.Change(context.Background(), Change{At: &target}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if len(pub.starts()) != 0 {
		t.Fatal("published despite the error")
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
	if !errors.Is(err, errBeforeRecordedTime) {
		t.Fatalf("err = %v, want errBeforeRecordedTime", err)
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
	if len(f.pub.starts()) != 0 {
		t.Fatal("published a refused setting")
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
	if len(f.pub.starts()) != 0 {
		t.Fatal("published despite the error")
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
	st, err := f.svc.Change(context.Background(), Change{At: &at, RatePPM: ptr(int64(2_000_000))})
	if err != nil {
		t.Fatalf("Change: %v", err)
	}
	if got := f.pub.starts(); !sameTimes(got, []time.Time{at}) {
		t.Fatalf("published starts = %v", got)
	}
	if want := at.Add(100 * time.Millisecond); !st.Now.Equal(want) {
		t.Fatalf("state now = %v, want %v", st.Now, want)
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
	if _, err := f.svc.Change(context.Background(), Change{At: &at}); !errors.Is(err, errBeforeRecordedTime) {
		t.Fatalf("err = %v, want errBeforeRecordedTime", err)
	}
}

func TestChange_TimeEqualNowWithRate_AppliesBoth(t *testing.T) {
	f := newFixture(t)
	f.tm.answer = func() time.Time { return f.clock.Now() }
	at := f.clock.Now()
	if _, err := f.svc.Change(context.Background(), Change{At: &at, RatePPM: ptr(int64(5_000_000))}); err != nil {
		t.Fatalf("Change: %v", err)
	}
	f.pub.mu.Lock()
	sets := append([]Setting(nil), f.pub.sets...)
	f.pub.mu.Unlock()
	if len(sets) != 1 || sets[0].StartUnixMicro != at.UnixMicro() || sets[0].RatePPM != 5_000_000 {
		t.Fatalf("published = %+v", sets)
	}
	if f.tm.calls != 1 {
		t.Fatalf("ConsistencyTime calls = %d, want 1", f.tm.calls)
	}
}

func TestRateOnly_NoJump(t *testing.T) {
	f := newFixture(t)
	f.src.advance(10 * time.Second)
	before := f.clock.Now()
	st, err := f.svc.Change(context.Background(), Change{RatePPM: ptr(int64(2_000_000))})
	if err != nil {
		t.Fatalf("Change: %v", err)
	}
	if !st.Now.Equal(before) || st.RatePPM != 2_000_000 {
		t.Fatalf("state = %+v, want now %v rate 2x", st, before)
	}
	if got := f.pub.starts(); !sameTimes(got, []time.Time{before}) {
		t.Fatalf("published starts = %v", got)
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
		var backward []string
		publish := func(s Setting) {
			// The previous setting's reading at the new anchor instant.
			was := prev.StartUnixMicro + (s.AnchorUnixMicro-prev.AnchorUnixMicro)*prev.RatePPM/1_000_000
			if s.StartUnixMicro < was {
				backward = append(backward, time.UnixMicro(s.StartUnixMicro).String())
			}
			prev = s
		}
		svc := NewService(c, (&fakeStoreTime{}).storeTime,
			func(context.Context) (spi.ScheduledTaskStore, error) { return sched, nil }, publish, testPoll)
		target := t0.Add(10 * time.Hour)
		if _, err := svc.Change(context.Background(), Change{At: &target}); err != nil {
			t.Fatalf("k=%d: Change: %v", k, err)
		}
		if len(backward) > 0 {
			t.Errorf("k=%d: published backward starts %v", k, backward)
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
	f.pub.mu.Lock()
	sets := append([]Setting(nil), f.pub.sets...)
	f.pub.mu.Unlock()
	if len(sets) != 1 || sets[0].StartUnixMicro != h1.UnixMicro() || sets[0].RatePPM != 1_000_000 {
		t.Fatalf("published = %+v, want one complete step to %v at the old rate", sets, h1)
	}
	if got := f.clock.Setting(); got != sets[0] {
		t.Fatalf("clock setting = %+v, want the last published %+v", got, sets[0])
	}
}

type nopBroadcaster struct{}

func (nopBroadcaster) Broadcast(string, []byte)       {}
func (nopBroadcaster) Subscribe(string, func([]byte)) {}

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
// last published, never the pending one.
func TestSetBack_PendingSettingNeverServed(t *testing.T) {
	f := newFixture(t)
	ex := &capturedExchange{}
	Attach(f.clock, nopBroadcaster{}, ex)

	if _, err := f.svc.Change(context.Background(), Change{RatePPM: ptr(int64(2_000_000))}); err != nil {
		t.Fatalf("rate change: %v", err)
	}
	published := f.clock.Setting()
	if got := servedSetting(t, ex); got == nil || *got != published {
		t.Fatalf("served %+v, want the published %+v", got, published)
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
	if pending := f.clock.Setting(); pending == published {
		t.Fatal("set-back not applied locally before the store call")
	}
	if got := servedSetting(t, ex); got == nil || *got != published {
		t.Fatalf("served %+v while the set-back is pending, want %+v", got, published)
	}
	release <- f.clock.Now().Add(time.Hour) // the store holds a later time
	if err := <-done; !errors.Is(err, errBeforeRecordedTime) {
		t.Fatalf("err = %v, want errBeforeRecordedTime", err)
	}
	if got := servedSetting(t, ex); got == nil || *got != published {
		t.Fatalf("served %+v after the refusal, want %+v", got, published)
	}
}

// TestSetBack_PendingOnFreshClockServesNothing: before anything is
// published the exchange serves nothing, even while a set-back is pending;
// once accepted and published, it serves that setting.
func TestSetBack_PendingOnFreshClockServesNothing(t *testing.T) {
	f := newFixture(t)
	ex := &capturedExchange{}
	Attach(f.clock, nopBroadcaster{}, ex)
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
		t.Fatalf("served %+v after publish, want %+v", got, f.clock.Setting())
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
	f.pub.hook = func(Setting) { cancel() }
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
	starts := f.pub.starts()
	if len(starts) == 0 || !starts[len(starts)-1].Equal(target) {
		t.Fatalf("published starts = %v, want them to end at %v", starts, target)
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
	st, err := f.svc.Change(context.Background(), Change{At: &target, RatePPM: ptr(int64(3_000_000))})
	if err != nil {
		t.Fatalf("Change: %v", err)
	}
	f.pub.mu.Lock()
	sets := append([]Setting(nil), f.pub.sets...)
	f.pub.mu.Unlock()
	if len(sets) < 2 {
		t.Fatalf("published = %+v, want the step and the rate", sets)
	}
	if sets[0].StartUnixMicro != target.UnixMicro() || sets[0].RatePPM != 1_000_000 {
		t.Fatalf("first published = %+v, want the step at the target", sets[0])
	}
	last := sets[len(sets)-1]
	if last.RatePPM != 3_000_000 || last.StartUnixMicro < target.Add(time.Millisecond).UnixMicro() {
		t.Fatalf("last published = %+v, want rate 3000000 starting no earlier than the fire", last)
	}
	if st.RatePPM != 3_000_000 || st.Now.Before(target.Add(time.Millisecond)) {
		t.Fatalf("state = %+v", st)
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
	f.pub.hook = func(Setting) { cancel() }
	if _, err := f.svc.Change(ctx, Change{At: &target}); !errors.Is(err, context.Canceled) {
		t.Fatalf("first move err = %v, want context.Canceled", err)
	}
	if !f.clock.Now().Equal(h1) {
		t.Fatalf("clock after cancel = %v, want %v", f.clock.Now(), h1)
	}
	f.pub.hook = nil
	f.sched.mu.Lock()
	f.sched.fire = true
	f.sched.mu.Unlock()
	if _, err := f.svc.Change(context.Background(), Change{At: &target}); err != nil {
		t.Fatalf("repeated move: %v", err)
	}
	want := []time.Time{h1, h2, h3, target}
	if got := f.pub.starts(); !sameTimes(got, want) {
		t.Fatalf("published starts = %v, want %v", got, want)
	}
	if !f.clock.Now().Equal(target) {
		t.Fatalf("clock = %v, want %v", f.clock.Now(), target)
	}
}
