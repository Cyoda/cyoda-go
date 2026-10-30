package platformclock

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// progressEvery bounds how often a move logs its progress.
const progressEvery = 10 * time.Second

// startupTolerance is how far the store's consistency time may lie ahead of
// real time when a node starts on real time.
const startupTolerance = time.Second

// Change is one request to change the test clock. At and AdvanceBy are
// exclusive; either may be combined with RatePPM. The service does not check
// ranges: the API layer validates them.
type Change struct {
	At        *time.Time
	AdvanceBy *time.Duration
	RatePPM   *int64
}

// State is the clock as a caller sees it.
type State struct {
	Now          time.Time
	RatePPM      int64
	Controllable bool
}

// ErrBeforeRecordedTime is matched (errors.Is) by a refused set-back: the
// store has recorded a time later than the requested start. errors.As into
// *BeforeRecordedTimeError gives that time.
var ErrBeforeRecordedTime = errors.New("the store holds a time later than the requested start")

// BeforeRecordedTimeError names the latest time the store has recorded.
type BeforeRecordedTimeError struct {
	Latest time.Time
}

func (e *BeforeRecordedTimeError) Error() string {
	return fmt.Sprintf("%s: latest recorded time %s",
		ErrBeforeRecordedTime.Error(), stamp(e.Latest))
}

func (e *BeforeRecordedTimeError) Unwrap() error { return ErrBeforeRecordedTime }

// Service changes the test clock: it sets it (into the past only when the
// store allows), moves it forward through every scheduled due time on the
// way, and changes its rate. Every change is applied locally and published.
// One change at a time is the caller's restriction; the service does not
// detect concurrent changes.
type Service struct {
	c         *Test
	storeTime func(ctx context.Context) (time.Time, error)
	tasks     func(ctx context.Context) (spi.ScheduledTaskStore, error)
	publish   func(Setting)
	poll      time.Duration
}

// NewService returns a Service changing c. storeTime returns the store's
// consistency time; the caller builds it with the tenant ConsistencyTime
// requires. tasks resolves the scheduled-task store a move polls; it and the
// store's cross-tenant SchedulerActivity get the caller's context as is.
// publish broadcasts each new setting; poll is the interval between
// scheduler polls during a move.
func NewService(c *Test, storeTime func(ctx context.Context) (time.Time, error),
	tasks func(ctx context.Context) (spi.ScheduledTaskStore, error),
	publish func(Setting), poll time.Duration) *Service {
	return &Service{c: c, storeTime: storeTime, tasks: tasks, publish: publish, poll: poll}
}

// State returns the current platform time and rate.
func (s *Service) State() State {
	st := s.c.st.Load()
	return State{
		Now:          time.UnixMicro(st.platformMicro(s.c.src())),
		RatePPM:      st.setting.RatePPM,
		Controllable: true,
	}
}

// Change applies ch:
//   - a target later than now moves the clock forward, stepping through
//     every scheduled due time up to the target; a rate applies at the end;
//   - a target not later than now sets the clock there, together with any
//     rate, if the store has recorded no later time;
//   - a rate alone re-anchors at now, so the time does not jump.
//
// A move stopped by ctx leaves the clock where its last step put it.
func (s *Service) Change(ctx context.Context, ch Change) (State, error) {
	if ch.At != nil && ch.AdvanceBy != nil {
		return State{}, errors.New("a clock change takes a time or an advance, not both")
	}
	var rate *int64
	if ch.RatePPM != nil {
		r := *ch.RatePPM
		rate = &r
	}
	now := s.c.Now()
	var target *time.Time
	switch {
	case ch.At != nil:
		t := *ch.At
		target = &t
	case ch.AdvanceBy != nil:
		t := now.Add(*ch.AdvanceBy)
		target = &t
	}

	var err error
	switch {
	case target == nil && rate == nil:
		return s.State(), nil
	case target == nil:
		s.commit(s.c.Set(nil, rate))
	case target.After(now):
		err = s.move(ctx, *target, rate)
	default:
		err = s.setBack(ctx, *target, rate)
	}
	st := s.State()
	if err != nil {
		return st, err
	}
	slog.Info("platform clock changed", "pkg", "platformclock",
		"now", stamp(st.Now), "ratePPM", st.RatePPM)
	return st, nil
}

// setBack applies at (and rate) locally, then asks the store for its
// consistency time. On a test clock the store answers from this clock, so an
// answer later than at plus the call's duration at the new rate means the
// store has recorded a later time: the previous setting is restored and
// nothing is published.
func (s *Service) setBack(ctx context.Context, at time.Time, rate *int64) error {
	prev, next := s.c.set(&at, rate)
	latest, err := s.storeTime(ctx)
	if err != nil {
		s.c.restore(next, prev)
		return fmt.Errorf("failed to read the store's consistency time: %w", err)
	}
	// The new setting's own reading is at plus the elapsed source time
	// times its rate: the tolerance for the time the call took.
	if latest.UnixMicro() > next.platformMicro(s.c.src()) {
		s.c.restore(next, prev)
		return &BeforeRecordedTimeError{Latest: latest}
	}
	s.commit(next.setting)
	return nil
}

// commit makes a setting already applied locally shareable and broadcasts it.
func (s *Service) commit(st Setting) {
	s.c.share(st)
	s.publish(st)
}

// move runs the step-through loop towards target. The clock keeps running
// throughout, so each step sets it only forward (setForward compares and
// sets atomically). A step failing to read the store leaves the clock at
// the last step, which was published whole.
func (s *Service) move(ctx context.Context, target time.Time, rate *int64) error {
	store, err := s.tasks(ctx)
	if err != nil {
		return fmt.Errorf("failed to resolve the scheduled-task store: %w", err)
	}
	ticker := time.NewTicker(s.poll)
	defer ticker.Stop()
	p := &progress{src: s.c.src, last: s.c.src(), target: target}
	for {
		rep, err := s.awaitIdle(ctx, store, ticker, p)
		if err != nil {
			return err
		}
		e := time.UnixMilli(rep.EarliestWaitingMs)
		if !rep.HasWaiting || e.After(target) {
			if st, ok := s.c.setForward(target, rate); ok {
				s.commit(st)
			}
			return nil
		}
		if st, ok := s.c.setForward(e, nil); ok {
			s.commit(st)
		}
		p.log("platform clock move in progress", "now", stamp(s.c.Now()))
	}
}

// awaitIdle polls store until no scheduled task is RUNNING and none WAITING
// is due on the clock, and returns that report.
func (s *Service) awaitIdle(ctx context.Context, store spi.ScheduledTaskStore,
	ticker *time.Ticker, p *progress) (spi.SchedulerActivityReport, error) {
	for {
		if err := ctx.Err(); err != nil {
			return spi.SchedulerActivityReport{}, fmt.Errorf("platform clock move stopped: %w", err)
		}
		rep, err := store.SchedulerActivity(ctx)
		if err != nil {
			return spi.SchedulerActivityReport{}, fmt.Errorf("failed to read scheduler activity: %w", err)
		}
		earliest := time.UnixMilli(rep.EarliestWaitingMs)
		due := rep.HasWaiting && !earliest.After(s.c.Now())
		switch {
		case rep.Running:
			p.log("platform clock move waiting for a running scheduled task", "now", stamp(s.c.Now()))
		case due:
			p.log("platform clock move waiting for a due scheduled task to fire",
				"due", stamp(earliest), "now", stamp(s.c.Now()))
		default:
			return rep, nil
		}
		select {
		case <-ctx.Done():
			return spi.SchedulerActivityReport{}, fmt.Errorf("platform clock move stopped: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// progress throttles a move's INFO lines to one per progressEvery, measured
// on the clock's source (real time).
type progress struct {
	src    func() time.Time
	last   time.Time
	target time.Time
}

func (p *progress) log(msg string, args ...any) {
	now := p.src()
	if now.Sub(p.last) < progressEvery {
		return
	}
	p.last = now
	slog.Info(msg, append([]any{"pkg", "platformclock", "target", stamp(p.target)}, args...)...)
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// StartupCheck fails when the store's consistency time is more than one
// second later than now: a node starting on real time over a store that
// holds later times would record history out of order.
func StartupCheck(ctx context.Context, storeTime func(ctx context.Context) (time.Time, error), now time.Time) error {
	latest, err := storeTime(ctx)
	if err != nil {
		return fmt.Errorf("failed to read the store's consistency time: %w", err)
	}
	if latest.After(now.Add(startupTolerance)) {
		return fmt.Errorf("the store holds times later than real time; start over with a fresh store (latest recorded time %s, real time %s)",
			stamp(latest), stamp(now))
	}
	return nil
}
