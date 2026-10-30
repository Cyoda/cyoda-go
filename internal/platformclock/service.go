package platformclock

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/common"
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
		ErrBeforeRecordedTime.Error(), e.Latest.UTC().Format(time.RFC3339Nano))
}

func (e *BeforeRecordedTimeError) Unwrap() error { return ErrBeforeRecordedTime }

// Service changes the test clock: it sets it (into the past only when the
// store allows), moves it forward through every scheduled due time on the
// way, and changes its rate. Every change is applied locally and published.
// One change at a time is the caller's restriction; the service does not
// detect concurrent changes.
type Service struct {
	c       *Test
	tm      spi.TransactionManager
	tasks   func(ctx context.Context) (spi.ScheduledTaskStore, error)
	publish func(Setting)
	poll    time.Duration
}

// NewService returns a Service changing c. tasks resolves the scheduled-task
// store a move polls, publish broadcasts each new setting, and poll is the
// interval between scheduler polls during a move.
func NewService(c *Test, tm spi.TransactionManager,
	tasks func(ctx context.Context) (spi.ScheduledTaskStore, error),
	publish func(Setting), poll time.Duration) *Service {
	return &Service{c: c, tm: tm, tasks: tasks, publish: publish, poll: poll}
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

	ctx = platformContext(ctx)
	var err error
	switch {
	case target == nil && rate == nil:
		return s.State(), nil
	case target == nil:
		s.publish(s.c.Set(nil, rate))
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
		"now", st.Now.UTC().Format(time.RFC3339Nano), "ratePPM", st.RatePPM)
	return st, nil
}

// setBack applies at (and rate) locally, then asks the store for its
// consistency time. On a test clock the store answers from this clock, so an
// answer later than at plus the call's duration at the new rate means the
// store has recorded a later time: the previous setting is restored and
// nothing is published.
func (s *Service) setBack(ctx context.Context, at time.Time, rate *int64) error {
	prev, next := s.c.set(&at, rate)
	latest, err := s.tm.ConsistencyTime(ctx)
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
	s.publish(next.setting)
	return nil
}

// move runs the step-through loop towards target. The clock keeps running
// throughout, so each step sets it only forward.
func (s *Service) move(ctx context.Context, target time.Time, rate *int64) error {
	store, err := s.tasks(ctx)
	if err != nil {
		return fmt.Errorf("failed to resolve the scheduled-task store: %w", err)
	}
	ticker := time.NewTicker(s.poll)
	defer ticker.Stop()
	lastLog := s.c.src()
	for {
		rep, err := s.awaitIdle(ctx, store, ticker)
		if err != nil {
			return err
		}
		now := s.c.Now()
		e := time.UnixMilli(rep.EarliestWaitingMs)
		if !rep.HasWaiting || e.After(target) {
			switch {
			case now.Before(target):
				s.publish(s.c.Set(&target, rate))
			case rate != nil:
				s.publish(s.c.Set(nil, rate))
			}
			return nil
		}
		if now.Before(e) {
			s.publish(s.c.Set(&e, nil))
		}
		if src := s.c.src(); src.Sub(lastLog) >= progressEvery {
			lastLog = src
			slog.Info("platform clock move in progress", "pkg", "platformclock",
				"now", s.c.Now().UTC().Format(time.RFC3339Nano),
				"target", target.UTC().Format(time.RFC3339Nano))
		}
	}
}

// awaitIdle polls store until no scheduled task is RUNNING and none WAITING
// is due on the clock, and returns that report.
func (s *Service) awaitIdle(ctx context.Context, store spi.ScheduledTaskStore,
	ticker *time.Ticker) (spi.SchedulerActivityReport, error) {
	for {
		if err := ctx.Err(); err != nil {
			return spi.SchedulerActivityReport{}, fmt.Errorf("platform clock move stopped: %w", err)
		}
		rep, err := store.SchedulerActivity(ctx)
		if err != nil {
			return spi.SchedulerActivityReport{}, fmt.Errorf("failed to read scheduler activity: %w", err)
		}
		due := rep.HasWaiting && !time.UnixMilli(rep.EarliestWaitingMs).After(s.c.Now())
		if !rep.Running && !due {
			return rep, nil
		}
		select {
		case <-ctx.Done():
			return spi.SchedulerActivityReport{}, fmt.Errorf("platform clock move stopped: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// StartupCheck fails when the store's consistency time is more than one
// second later than now: a node starting on real time over a store that
// holds later times would record history out of order.
func StartupCheck(ctx context.Context, tm spi.TransactionManager, now time.Time) error {
	latest, err := tm.ConsistencyTime(platformContext(ctx))
	if err != nil {
		return fmt.Errorf("failed to read the store's consistency time: %w", err)
	}
	if latest.After(now.Add(startupTolerance)) {
		return fmt.Errorf("the store holds times later than real time; start over with a fresh store (latest recorded time %s, real time %s)",
			latest.UTC().Format(time.RFC3339Nano), now.UTC().Format(time.RFC3339Nano))
	}
	return nil
}

// platformContext puts the system principal of the platform tenant on ctx:
// ConsistencyTime requires a tenant, and the clock belongs to the platform.
func platformContext(ctx context.Context) context.Context {
	return spi.WithUserContext(ctx, common.SystemUserContextValue(auth.PlatformTenantID))
}
