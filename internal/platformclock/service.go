package platformclock

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// progressEvery bounds how often a move logs its progress.
const progressEvery = 10 * time.Second

// startupTolerance is how far the store's latest recorded time may lie ahead
// of real time when a node starts on real time. It is coupled to sqlite, which
// persists its consistency floor up to 1 s ahead (consistencyHighStep in
// plugins/sqlite/txmanager.go); a host wall clock that steps back across a
// restart can therefore fail startup, which fails closed.
const startupTolerance = time.Second

// Change is one request to change the test clock. At and AdvanceByMs are
// exclusive; either may be combined with RatePPM. The API layer validates At
// and RatePPM; the service resolves an advance and checks its target.
type Change struct {
	At          *time.Time
	AdvanceByMs *int64
	RatePPM     *int64
}

// State is the clock as a caller sees it.
type State struct {
	Now          time.Time
	RatePPM      int64
	Controllable bool
}

// Rate is RatePPM as the multiplier both doors answer (1 is real time).
func (s State) Rate() float64 { return float64(s.RatePPM) / 1e6 }

// BeforeRecordedTimeError refuses a set-back: the store has recorded a time
// later than the requested start. Latest is that time.
type BeforeRecordedTimeError struct {
	Latest time.Time
}

func (e *BeforeRecordedTimeError) Error() string {
	return fmt.Sprintf("the store holds a time later than the requested start: latest recorded time %s",
		stamp(e.Latest))
}

// OutOfRangeError refuses an advance whose target lies past the year 9000.
type OutOfRangeError struct{}

func (*OutOfRangeError) Error() string {
	return "advanceByMs moves the clock beyond the year 9000"
}

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
// store's cross-tenant SchedulerActivity get the request's cancellation without
// its user context.
// publish broadcasts each new setting; poll is the interval between
// scheduler polls during a move. It panics on a nil clock, storeTime, tasks or
// publish.
func NewService(c *Test, storeTime func(ctx context.Context) (time.Time, error),
	tasks func(ctx context.Context) (spi.ScheduledTaskStore, error),
	publish func(Setting), poll time.Duration) *Service {
	switch {
	case c == nil:
		panic("platformclock.NewService: nil clock")
	case storeTime == nil:
		panic("platformclock.NewService: nil store time")
	case tasks == nil:
		panic("platformclock.NewService: nil scheduled-task store")
	case publish == nil:
		panic("platformclock.NewService: nil publish")
	}
	return &Service{c: c, storeTime: storeTime, tasks: tasks, publish: publish, poll: poll}
}

// State returns the current platform time and rate.
func (s *Service) State() State {
	now, setting := s.c.reading()
	return State{
		Now:          now,
		RatePPM:      setting.RatePPM,
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
	case ch.AdvanceByMs != nil:
		// Checked in milliseconds before the target is formed, so no advance
		// can overflow on the way to the year-9000 refusal.
		if *ch.AdvanceByMs > (maxSetTime.UnixMicro()-now.UnixMicro())/1000 {
			return s.State(), &OutOfRangeError{}
		}
		t := time.UnixMicro(now.UnixMicro() + *ch.AdvanceByMs*1000)
		target = &t
	}

	var err error
	switch {
	case target == nil:
		s.commit(s.c.set(nil, rate))
	case target.After(now):
		err = s.move(ctx, *target, rate)
	default:
		err = s.setBack(ctx, *target, rate)
	}
	st := s.State()
	if err != nil {
		return st, err
	}
	att, exe := spi.AttributionFor(ctx)
	slog.Info("platform clock changed", "pkg", "platformclock",
		"now", stamp(st.Now), "ratePPM", st.RatePPM,
		"attributedId", att.ID, "attributedKind", string(att.Kind), "executorId", exe.ID, "executorKind", string(exe.Kind))
	return st, nil
}

// setBack applies at (and rate) locally, then asks the store for its
// consistency time. On a test clock the store answers from this clock, so an
// answer later than at plus the call's duration at the new rate means the
// store has recorded a later time: the previous setting is restored and
// nothing is published.
func (s *Service) setBack(ctx context.Context, at time.Time, rate *int64) error {
	prev, next := s.c.setState(&at, rate)
	latest, err := s.storeTime(ctx)
	if err != nil {
		s.c.restore(next, prev)
		return clientGoneOr(ctx, fmt.Errorf("failed to read the store's consistency time: %w", err))
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
func (s *Service) move(reqCtx context.Context, target time.Time, rate *int64) error {
	att, exe := spi.AttributionFor(reqCtx)
	// The store calls span tenants, so they carry no user context. A request
	// context has no deadline, and its cancellation still stops the move.
	ctx := spi.WithUserContext(reqCtx, nil)
	store, err := s.tasks(ctx)
	if err != nil {
		return clientGoneOr(ctx, fmt.Errorf("failed to resolve the scheduled-task store: %w", err))
	}
	ticker := time.NewTicker(s.poll)
	defer ticker.Stop()
	p := &progress{src: s.c.src, last: s.c.src(), target: target}
	for {
		rep, err := s.awaitIdle(ctx, store, ticker, p)
		if err != nil {
			// Where the move stopped is this line's event. The cause rides
			// along only for a departed client, whose error the door logs at
			// DEBUG; any other failure reaches the door's ERROR line.
			attrs := []any{"pkg", "platformclock", "target", stamp(target), "now", stamp(s.c.Now()),
				"attributedId", att.ID, "attributedKind", string(att.Kind), "executorId", exe.ID, "executorKind", string(exe.Kind)}
			if errors.Is(err, common.ErrClientGone) {
				attrs = append(attrs, "cause", err.Error())
			}
			slog.Info("platform clock move stopped", attrs...)
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
			return spi.SchedulerActivityReport{}, common.ClientGone(fmt.Errorf("platform clock move stopped: %w", err))
		}
		rep, err := store.SchedulerActivity(ctx)
		if err != nil {
			return spi.SchedulerActivityReport{}, clientGoneOr(ctx, fmt.Errorf("failed to read scheduler activity: %w", err))
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
			return spi.SchedulerActivityReport{}, common.ClientGone(fmt.Errorf("platform clock move stopped: %w", ctx.Err()))
		case <-ticker.C:
		}
	}
}

// clientGoneOr marks err as the client's departure when the move's context
// has ended: a store call the cancellation interrupted returns its own error,
// and the client going away is what stopped it.
func clientGoneOr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return common.ClientGone(err)
	}
	return err
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

// StartupCheck fails when the latest time the store has recorded is more
// than one second later than now: a node starting on real time over a store
// that holds later times would record history out of order. latestRecorded
// reads the store's floor without moving it (spi.TransactionManager's
// LatestRecordedTime); a read that raised the floor to now would refuse every
// later start in the past.
func StartupCheck(ctx context.Context, latestRecorded func(ctx context.Context) (time.Time, error), now time.Time) error {
	latest, err := latestRecorded(ctx)
	if err != nil {
		return fmt.Errorf("failed to read the store's latest recorded time: %w", err)
	}
	if latest.After(now.Add(startupTolerance)) {
		return fmt.Errorf("the store holds times later than real time; start over with a fresh store (latest recorded time %s, real time %s)",
			stamp(latest), stamp(now))
	}
	return nil
}
