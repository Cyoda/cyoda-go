package platformclock

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
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
}

func (f *fakeScheduler) SchedulerActivity(ctx context.Context) (spi.SchedulerActivityReport, error) {
	if uc := spi.GetUserContext(ctx); uc == nil || uc.Tenant.ID != "PLATFORM" {
		return spi.SchedulerActivityReport{}, errors.New("no PLATFORM tenant")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.polls++
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

// fakeTM is a TransactionManager whose only live method is ConsistencyTime.
type fakeTM struct {
	spi.TransactionManager
	answer func() time.Time
	err    error
	calls  int
}

func (f *fakeTM) ConsistencyTime(ctx context.Context) (time.Time, error) {
	f.calls++
	if uc := spi.GetUserContext(ctx); uc == nil || uc.Tenant.ID != "PLATFORM" {
		return time.Time{}, errors.New("no PLATFORM tenant")
	}
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
	tm    *fakeTM
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
		tm:    &fakeTM{answer: func() time.Time { return time.UnixMicro(0) }},
		pub:   &publishLog{},
		t0:    c.Now(),
	}
	f.svc = NewService(c, f.tm,
		func(context.Context) (spi.ScheduledTaskStore, error) { return f.sched, nil },
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
	svc := NewService(c, &fakeTM{},
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
	if !errors.Is(err, ErrBeforeRecordedTime) {
		t.Fatalf("err = %v, want ErrBeforeRecordedTime", err)
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
	if _, err := f.svc.Change(context.Background(), Change{At: &at}); !errors.Is(err, ErrBeforeRecordedTime) {
		t.Fatalf("err = %v, want ErrBeforeRecordedTime", err)
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

func TestChange_AtAndAdvanceByRefused(t *testing.T) {
	f := newFixture(t)
	at := f.t0.Add(time.Hour)
	if _, err := f.svc.Change(context.Background(), Change{At: &at, AdvanceBy: ptr(time.Hour)}); err == nil {
		t.Fatal("expected an error for At with AdvanceBy")
	}
	if len(f.pub.starts()) != 0 {
		t.Fatal("published")
	}
}

func TestStartupCheck_AheadFails(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	tm := &fakeTM{answer: func() time.Time { return now.Add(time.Second + time.Microsecond) }}
	if err := StartupCheck(context.Background(), tm, now); err == nil {
		t.Fatal("expected a startup error")
	}
}

func TestStartupCheck_BehindPasses(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	for _, c := range []time.Time{now.Add(-48 * time.Hour), now.Add(time.Second)} {
		tm := &fakeTM{answer: func() time.Time { return c }}
		if err := StartupCheck(context.Background(), tm, now); err != nil {
			t.Fatalf("C=%v: %v", c, err)
		}
	}
}

func TestStartupCheck_StoreErrorFails(t *testing.T) {
	boom := errors.New("down")
	if err := StartupCheck(context.Background(), &fakeTM{err: boom}, time.Unix(0, 0)); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
}
