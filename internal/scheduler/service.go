// Package scheduler runs scheduled transitions. Every pnode claims due tasks
// from the store and runs them itself, and the claim token fences every write
// a run makes. A heartbeat proves the pnode alive; a watchdog cancels the
// pnode's runs before another pnode could consider it stale.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
	"github.com/cyoda-platform/cyoda-go/internal/observability"
)

// Firer runs one claimed task. *workflow.Engine satisfies it.
type Firer interface {
	FireScheduledTransition(ctx context.Context, task spi.ScheduledTask, maxLostOwners int, retryDelay time.Duration) workflow.RunReport
}

// Deps are the service's collaborators. HealthFlag and Meter may be nil.
type Deps struct {
	Store     spi.StoreFactory
	TxManager spi.TransactionManager
	Firer     Firer
	// Clock is the pnode clock: the one that set scheduledTime at arm.
	Clock Clock
	// HealthFlag is the process-wide flag a recovered panic latches false.
	HealthFlag *atomic.Bool
	Meter      metric.Meter
	// CalloutDeadlineMax is the longest one callout can take. Shutdown step 4
	// waits for it, counted from the start of each unsafe callout in flight.
	CalloutDeadlineMax time.Duration
}

// storeCallBudget bounds one store call the scheduler makes outside a run: a
// claim, a give-back, a sweep, one attempt at recording an outcome.
const storeCallBudget = 10 * time.Second

// sweepInterval is how often the claim loop sweeps dead owners and the marks
// of ended lives.
const sweepInterval = time.Minute

// ownerSweepFactor × STALE_AFTER is how long a dead owner's liveness record
// is kept.
const ownerSweepFactor = 10

// shutdownTail is the part of the step-4 bound after the commit budget.
const shutdownTail = 15 * time.Second

// warnEvery rate-limits the WARN line of a retried outcome write, per run.
const warnEvery = time.Minute

var (
	errHeartbeatLate      = errors.New("the heartbeat succeeded after its watchdog window")
	errBookkeepingStopped = errors.New("the scheduler stopped before the outcome was recorded")
)

type cancelReason int

const (
	notCancelled cancelReason = iota
	selfCancelled
	latchCancelled
	shutdownCancelled
)

// liveRun is one claim this incarnation holds.
type liveRun struct {
	task   spi.ScheduledTask
	ref    spi.TaskRef
	ctx    context.Context
	cancel context.CancelFunc
	unsafe *workflow.UnsafeFlight

	// Guarded by Service.mu.
	reason cancelReason
	ended  bool // the fire has returned
	exited bool // the run goroutine has returned, bookkeeping included
	keep   bool // never given back: the run panicked, or the store rejected its outcome
}

// Service is one pnode's scheduler: a claim loop, a heartbeat goroutine, a
// watchdog goroutine and one goroutine per run.
type Service struct {
	cfg         Config
	deps        Deps
	incarnation uuid.UUID
	window      time.Duration // W
	sweepEvery  time.Duration
	// stepFourMargin is the part of the step-4 bound past the longest
	// callout in flight: CommitBudget + 15s.
	stepFourMargin time.Duration
	// bookWait bounds the wait for outcome writes under way that follows
	// step 4: one store-call budget. It is taken out of stepFourMargin, so
	// step 4 and the wait together stay within the step-4 bound that the
	// grace period is sized for. A RecordAttempt ends within it. A Fail
	// commits through ShieldedCommit with its own CommitBudget and can
	// outlive it; that run's claim is then kept and the owner is not
	// retired, so the task is reclaimed after STALE_AFTER.
	bookWait time.Duration
	m        *metrics
	store    spi.ScheduledTaskStore

	mu           sync.Mutex
	started      bool
	runs         map[uuid.UUID]*liveRun // by claim token
	perTenant    map[spi.TenantID]int
	healthy      bool      // the last heartbeat succeeded in time and the watchdog has not fired since
	healthySince time.Time // start of the current run of clean heartbeats
	wdDeadline   time.Time // the last in-time heartbeat's recorded start + W
	latched      bool
	draining     bool
	filled       bool // the last claim took every free slot
	active       int  // run goroutines that have not exited
	booking      int  // run goroutines whose fire has returned and that have not exited

	slotFreed  chan struct{}
	drainingCh chan struct{} // closed at shutdown step 1, before the drain reads any run's Unsafe record
	stopLoop   chan struct{}
	loopDone   chan struct{}
	stopHB     chan struct{}
	hbDone     chan struct{}
	wdDone     chan struct{}
	wdArm      chan time.Time
	stopBooks  chan struct{} // closed when shutdown stops waiting for outcomes to be recorded
	runsIdle   chan struct{} // closed once the service drains and no run goroutine is left
	bookLeft   chan struct{} // poked when a run goroutine whose fire has returned exits

	startOnce sync.Once
	startErr  error // the first Start's error, returned by every later call
	drainOnce sync.Once
}

// New builds a service. Start begins claiming.
func New(cfg Config, deps Deps) *Service {
	return &Service{
		cfg:            cfg,
		deps:           deps,
		incarnation:    uuid.New(),
		window:         watchdogWindow(cfg.StaleAfter),
		sweepEvery:     sweepInterval,
		stepFourMargin: common.CommitBudget + shutdownTail,
		bookWait:       storeCallBudget,
		runs:           make(map[uuid.UUID]*liveRun),
		perTenant:      make(map[spi.TenantID]int),
		slotFreed:      make(chan struct{}, 1),
		drainingCh:     make(chan struct{}),
		stopLoop:       make(chan struct{}),
		loopDone:       make(chan struct{}),
		stopHB:         make(chan struct{}),
		hbDone:         make(chan struct{}),
		wdDone:         make(chan struct{}),
		wdArm:          make(chan time.Time, 1),
		stopBooks:      make(chan struct{}),
		runsIdle:       make(chan struct{}),
		bookLeft:       make(chan struct{}, 1),
	}
}

// Start starts the heartbeat, the watchdog and the claim loop. It returns
// once the first heartbeat is scheduled. A disabled service, or one drained
// before it started, starts nothing.
// Only the first call does anything; every later call returns its error.
func (s *Service) Start(ctx context.Context) error {
	if !s.cfg.Enabled {
		return nil
	}
	s.startOnce.Do(func() {
		s.startErr = s.start(ctx)
	})
	return s.startErr
}

func (s *Service) start(ctx context.Context) error {
	m, err := newMetrics(s.deps.Meter)
	if err != nil {
		return err
	}
	store, err := s.deps.Store.ScheduledTaskStore(ctx)
	if err != nil {
		return fmt.Errorf("failed to get the scheduled task store: %w", err)
	}
	started := func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		// A Drain that ran first leaves nothing to stop later: start nothing.
		if s.draining {
			return false
		}
		s.m, s.store, s.started = m, store, true
		return true
	}()
	if !started {
		return nil
	}
	go s.watchdogLoop()
	go s.heartbeatLoop()
	go s.loop()
	// The multi-node scenarios map a claim's owner to its pnode by this
	// line. The incarnation is an identifier, not a secret.
	slog.Info("scheduler started", "pkg", "scheduler", "incarnation", s.incarnation.String())
	return nil
}

// Stop drains with no outside deadline. It is the entry point when a server
// failed: App.Shutdown calls it after the servers stop. After a Drain it does
// nothing.
func (s *Service) Stop() { s.Drain(context.Background()) }

// Drain runs shutdown steps 1-5 (spec §6.4). ctx ends the waits of steps 2
// and 4, and the wait for outcome writes, early. Only the first call does anything. A Drain before Start makes
// every later Start do nothing.
func (s *Service) Drain(ctx context.Context) {
	s.drainOnce.Do(func() { s.drain(ctx) })
}

func (s *Service) drain(ctx context.Context) {
	started := func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		if !s.started {
			// A Start after this does nothing.
			s.draining = true
			return false
		}
		// Step 1: stop claiming. From here no run starts a new unsafe
		// dispatch. NoNewUnsafe closes here, before steps 3 and 4 read any
		// run's Unsafe record, in program order on this goroutine: the engine
		// counts an unsafe dispatch before it checks NoNewUnsafe, and only
		// this order makes those reads miss no dispatch that goes ahead.
		s.draining = true
		close(s.drainingCh)
		if s.active == 0 {
			close(s.runsIdle)
		}
		return true
	}()
	if !started {
		return
	}
	// A loop that panicked has already exited; nothing below needs it.
	close(s.stopLoop)
	<-s.loopDone

	// Step 2: wait for the runs while the streams and callback routes are open.
	if !s.waitRuns(ctx, s.cfg.ShutdownDrain) {
		// Step 3: cancel every run without an unsafe callout in flight.
		s.cutRuns()
		// Step 4: wait for every run to record its outcome. The last
		// bookWait of the bound is left for the wait below.
		s.waitRuns(ctx, s.stepFourBound()-s.bookWait)
	}
	close(s.stopBooks)
	// A run still writing its outcome holds its task row, and GiveBackIdle
	// skips a held row. Wait for those writes to end, so that no task is
	// left RUNNING under an owner that step 5 retires.
	s.waitBooks(ctx, s.bookWait)

	// Step 5: give back the claims whose run ended without a recorded outcome,
	// stop the heartbeat, and retire the owner if nothing still holds a claim.
	keep := s.finalKeep()
	gctx, cancel := context.WithTimeout(context.Background(), storeCallBudget)
	if n, err := s.store.GiveBackIdle(gctx, s.incarnation, keep); err != nil {
		slog.Warn("scheduler could not give back its claims at shutdown", "pkg", "scheduler", "err", err)
	} else if n > 0 {
		slog.Info("scheduler gave back claims at shutdown", "pkg", "scheduler", "count", n)
	}
	cancel()
	// The heartbeat stops even with a run still live: that run's task is
	// reclaimed after STALE_AFTER. The watchdog goes on until the last run
	// goroutine exits, so it cancels a live run before another pnode could
	// consider this owner stale (§6.3).
	close(s.stopHB)
	<-s.hbDone
	select {
	case <-s.runsIdle:
		<-s.wdDone
	default:
	}
	if len(keep) > 0 {
		slog.Warn("scheduler stopped with runs still holding their tasks; another node takes them over after CYODA_SCHEDULER_STALE_AFTER",
			"pkg", "scheduler", "runs", len(keep))
		return
	}
	rctx, cancel := context.WithTimeout(context.Background(), storeCallBudget)
	defer cancel()
	if err := s.store.RetireOwner(rctx, s.incarnation); err != nil {
		slog.Warn("scheduler could not retire its liveness record", "pkg", "scheduler", "err", err)
	}
}

// waitRuns waits up to d for every run goroutine to end, bookkeeping included.
// No run is registered after step 1, so the count only goes down.
func (s *Service) waitRuns(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-s.runsIdle:
		return true
	case <-timer.C:
		return false
	case <-ctx.Done():
		return false
	}
}

// cutRuns is step 3: a run whose unsafe callout is in flight is left alone
// (spec §6.4). A run already cancelled keeps the reason it was cancelled for.
// Cancelling a run whose fire has returned changes nothing: its bookkeeping
// runs without the run's cancellation.
func (s *Service) cutRuns() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.runs {
		if _, inFlight := r.unsafe.Since(); inFlight {
			continue
		}
		if r.reason == notCancelled {
			r.reason = shutdownCancelled
		}
		r.cancel()
	}
}

// stepFourBound is the longest remaining callout deadline + CommitBudget + 15s.
func (s *Service) stepFourBound() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	var longest time.Duration
	for _, r := range s.runs {
		if since, inFlight := r.unsafe.Since(); inFlight {
			longest = max(longest, s.deps.CalloutDeadlineMax-time.Since(since))
		}
	}
	return longest + s.stepFourMargin
}

// waitBooks waits up to d for every run whose fire has returned to leave its
// bookkeeping. Bookkeeping has been told to stop, so each such run makes at
// most the write attempt already under way. ctx ends the wait early; a run
// still writing then keeps its claim.
func (s *Service) waitBooks(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for s.inBookkeeping() {
		select {
		case <-s.bookLeft:
		case <-timer.C:
			return
		case <-ctx.Done():
			return
		}
	}
}

func (s *Service) inBookkeeping() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.booking > 0
}

// finalKeep releases every run that ended without a recorded outcome and
// returns the claims that must stay: runs still live or still writing their
// outcome, and kept runs.
func (s *Service) finalKeep() []uuid.UUID {
	s.mu.Lock()
	defer s.mu.Unlock()
	keep := make([]uuid.UUID, 0, len(s.runs))
	for token, r := range s.runs {
		if !r.exited || r.keep {
			keep = append(keep, token)
			continue
		}
		s.releaseLocked(r)
	}
	return keep
}

// --- liveness ---------------------------------------------------------------

func (s *Service) heartbeatLoop() {
	defer close(s.hbDone)
	defer s.recoverLatch("heartbeat")
	ticker := time.NewTicker(s.cfg.HeartbeatInterval)
	defer ticker.Stop()
	for {
		s.heartbeat()
		select {
		case <-s.stopHB:
			return
		case <-ticker.C:
		}
	}
}

// heartbeat records the moment before the call acquires its connection; the
// store's stamp is never earlier.
func (s *Service) heartbeat() {
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), heartbeatBudget(s.cfg.HeartbeatInterval))
	err := s.store.Heartbeat(ctx, s.incarnation)
	cancel()
	s.heartbeatDone(started, err)
}

func (s *Service) heartbeatDone(started time.Time, err error) {
	if err == nil && time.Since(started) >= s.window {
		err = errHeartbeatLate
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.healthy, s.healthySince = false, time.Time{}
		s.m.heartbeatFailed()
		slog.Warn("scheduler heartbeat failed; no claims until one succeeds", "pkg", "scheduler", "err", err)
		return
	}
	if !s.healthy {
		s.healthy, s.healthySince = true, started
	}
	s.wdDeadline = started.Add(s.window)
	select {
	case <-s.wdArm:
	default:
	}
	s.wdArm <- s.wdDeadline
}

func (s *Service) watchdogLoop() {
	defer close(s.wdDone)
	defer s.recoverLatch("watchdog")
	timer := time.NewTimer(0)
	timer.Stop()
	for {
		select {
		case <-s.runsIdle:
			timer.Stop()
			return
		case at := <-s.wdArm:
			timer.Reset(time.Until(at))
		case <-timer.C:
			s.selfCancel()
		}
	}
}

// selfCancel runs when W has passed since the last successful heartbeat began:
// every run in progress is cancelled, and nothing is claimed until a heartbeat
// succeeds. A timer that fires after a later heartbeat moved the deadline on
// is stale and does nothing; the watchdog re-arms from that heartbeat.
//
// It reads no run's Unsafe record. A path that does (the shutdown drain) must
// close NoNewUnsafe before it reads Unsafe.Since or UnsafeInFlight, in program
// order on its own goroutine: the engine counts an unsafe dispatch before it
// checks NoNewUnsafe, and only that order makes the read miss no dispatch that
// goes ahead.
func (s *Service) selfCancel() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Now().Before(s.wdDeadline) {
		return
	}
	s.healthy, s.healthySince = false, time.Time{}
	n := 0
	for _, r := range s.runs {
		if r.ended {
			continue
		}
		if r.reason == notCancelled {
			r.reason = selfCancelled
		}
		r.cancel()
		n++
	}
	slog.Warn("scheduler heartbeats failed for the whole watchdog window; cancelled every run in progress",
		"pkg", "scheduler", "runs", n)
}

func (s *Service) isHealthy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.healthy
}

// --- claiming ---------------------------------------------------------------

// loop is the only goroutine that calls ClaimDue and GiveBackIdle.
func (s *Service) loop() {
	defer close(s.loopDone)
	defer s.recoverLatch("claim loop")
	scan := time.NewTicker(s.cfg.ScanInterval)
	defer scan.Stop()
	sweep := time.NewTicker(s.sweepEvery)
	defer sweep.Stop()
	for {
		select {
		case <-s.stopLoop:
			return
		case <-scan.C:
			s.tick()
		case <-s.slotFreed:
			s.claim()
		case <-sweep.C:
			if s.isHealthy() {
				s.sweep()
			}
		}
	}
}

// tick gives back idle claims and claims. It does nothing once the service
// drains: the loop may still take a tick that raced shutdown step 1, and step
// 5 makes the last give-back.
func (s *Service) tick() {
	if !s.tickable() {
		return
	}
	s.giveBackIdle()
	s.claim()
}

func (s *Service) tickable() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.healthy && !s.draining
}

// giveBackIdle returns to WAITING, uncounted, every task this incarnation holds
// RUNNING without a live run: a claim whose reply was lost.
func (s *Service) giveBackIdle() {
	keep := s.liveTokens()
	ctx, cancel := context.WithTimeout(context.Background(), storeCallBudget)
	defer cancel()
	n, err := s.store.GiveBackIdle(ctx, s.incarnation, keep)
	if err != nil {
		slog.Warn("scheduler could not give back idle claims", "pkg", "scheduler", "err", err)
		return
	}
	if n > 0 {
		slog.Info("scheduler gave back claims that had no run", "pkg", "scheduler", "count", n)
	}
}

func (s *Service) liveTokens() []uuid.UUID {
	s.mu.Lock()
	defer s.mu.Unlock()
	keep := make([]uuid.UUID, 0, len(s.runs))
	for token := range s.runs {
		keep = append(keep, token)
	}
	return keep
}

func (s *Service) claim() {
	req, free, ok := s.claimRequest()
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), storeCallBudget)
	tasks, err := s.store.ClaimDue(ctx, req)
	cancel()
	if err != nil {
		slog.Warn("scheduler claim failed", "pkg", "scheduler", "err", err)
		return
	}
	runs, giveBack := s.register(tasks, free)
	for _, r := range runs {
		s.m.claimed(claimReason(r.task))
		go s.run(r)
	}
	if giveBack {
		s.giveBackIdle()
	}
}

// claimRequest builds the next claim, or reports that none may be made: the
// pnode is unhealthy, latched or draining, or every run slot is taken.
func (s *Service) claimRequest() (req spi.ClaimRequest, free int, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.healthy || s.latched || s.draining {
		return spi.ClaimRequest{}, 0, false
	}
	free = s.cfg.MaxRuns - len(s.runs)
	if free <= 0 {
		s.filled = true
		return spi.ClaimRequest{}, 0, false
	}
	inProgress := make(map[spi.TenantID]int, len(s.perTenant))
	for tenant, n := range s.perTenant {
		inProgress[tenant] = n
	}
	return spi.ClaimRequest{
		Owner:            s.incarnation,
		NowMs:            s.deps.Clock.Now().UnixMilli(),
		StaleAfter:       s.cfg.StaleAfter,
		Limit:            free,
		PerTenantLimit:   s.cfg.MaxRunsPerTenant,
		TenantInProgress: inProgress,
		AllowLostOwner:   time.Since(s.healthySince) >= s.cfg.StaleAfter,
	}, free, true
}

// register adds every claimed task to the set of live runs. The loop calls it
// before anything else runs on its goroutine, the next give-back included.
// A claim that returns after the node latched, began to drain, or passed its
// watchdog deadline is not started: it never ran, so it is refused and goes
// back uncounted. After a latch or the watchdog deadline, giveBack asks the
// loop to return it at once. During a drain, shutdown step 5 returns it, so
// no run is registered once step 1 has closed. A heartbeat that merely failed
// while the claim was in flight refuses nothing: one failed heartbeat never
// self-cancels.
func (s *Service) register(tasks []spi.ScheduledTask, free int) (runs []*liveRun, giveBack bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.draining {
		return nil, false
	}
	if s.latched || !time.Now().Before(s.wdDeadline) {
		return nil, len(tasks) > 0
	}
	s.filled = len(tasks) >= free
	runs = make([]*liveRun, 0, len(tasks))
	for _, t := range tasks {
		runs = append(runs, s.registerLocked(t))
	}
	return runs, false
}

func (s *Service) registerLocked(t spi.ScheduledTask) *liveRun {
	ctx, cancel := context.WithCancel(context.Background())
	r := &liveRun{
		task:   t,
		ref:    spi.TaskRef{TenantID: t.TenantID, ID: t.ID, ArmToken: t.ArmToken, ClaimToken: t.Claim.Token},
		ctx:    ctx,
		cancel: cancel,
		unsafe: &workflow.UnsafeFlight{},
	}
	s.runs[r.ref.ClaimToken] = r
	s.perTenant[t.TenantID]++
	s.active++
	s.m.runStarted()
	return r
}

func claimReason(t spi.ScheduledTask) string {
	if t.ClaimedFromLostOwner {
		return claimOwnerLost
	}
	return claimDue
}

// sweep removes dead owners' liveness records and the marks of ended lives.
func (s *Service) sweep() {
	ctx, cancel := context.WithTimeout(context.Background(), storeCallBudget)
	defer cancel()
	if err := s.store.SweepOwners(ctx, ownerSweepFactor*s.cfg.StaleAfter); err != nil {
		slog.Warn("scheduler could not sweep dead owners", "pkg", "scheduler", "err", err)
	}
	if err := s.store.SweepMarks(ctx); err != nil {
		slog.Warn("scheduler could not sweep the marks of ended lives", "pkg", "scheduler", "err", err)
	}
}

// --- runs -------------------------------------------------------------------

// run fires one claimed task and records its outcome. A panic in the fire is
// recovered by fire; a panic in the bookkeeping is recovered here. Either way
// the claim is kept and the node latches (§6.5).
func (s *Service) run(r *liveRun) {
	defer s.runExited(r)
	start := time.Now()
	defer s.recoverBookkeeping(r, start)
	ctx, span := observability.Tracer().Start(r.ctx, "scheduler.run")
	defer span.End()

	rep, panicked, panicTicket := s.fire(ctx, r)
	nowMs := s.deps.Clock.Now().UnixMilli()

	reason, draining := s.markEnded(r)

	// A panicked run has the zero report, which is never cut; decideBookkeeping
	// and runOutcome match their panic rows first.
	cut := rep.Outcome == workflow.OutcomeFailed && errors.Is(rep.Err, context.Canceled) &&
		(reason == shutdownCancelled || (reason == notCancelled && draining))
	var errText string
	errTicket := uuid.Nil
	switch {
	case panicked:
		errText, errTicket = internalErrorText(panicTicket), panicTicket
	case rep.Err != nil:
		text, ticket, warnOnly := recordedError(rep.Err)
		errText, errTicket = sanitiseErrorText(text), ticket
		if !warnOnly {
			slog.Error("scheduled run failed with an internal error", "pkg", "scheduler",
				"taskId", r.task.ID, "tenant", string(r.task.TenantID), "ticket", ticket.String(), "err", rep.Err)
		}
	}
	bk := decideBookkeeping(rep, r.task, cut, panicked, nowMs, s.cfg, errText)
	outcome := runOutcome(rep, bk, panicked, reason, cut)
	err := s.book(r, bk)
	s.logOutcome(r, bk, err, errTicket, rep.Err)
	if errors.Is(err, spi.ErrStaleClaim) {
		outcome = outcomeSuperseded
	}
	s.finish(r, bk, err)
	span.SetAttributes(attribute.String("outcome", outcome))
	s.m.runEnded(outcome, time.Since(start))
}

// runExited counts a run goroutine out, and wakes a drain waiting for runs to
// leave their bookkeeping. The last one to exit once the service drains
// closes runsIdle.
func (s *Service) runExited(r *liveRun) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.exited = true
	if r.ended {
		s.booking--
		select {
		case s.bookLeft <- struct{}{}:
		default:
		}
	}
	s.active--
	if s.draining && s.active == 0 {
		close(s.runsIdle)
	}
}

// recoverBookkeeping recovers a panic in a run's bookkeeping, and latches the
// node. The outcome is not known to be recorded, so the claim is kept for
// good: it stays in the live set, because finish did not run, and it is
// marked kept, so shutdown step 5 does not give it back either (§6.5). A
// transaction the panic interrupted is rolled back by its own deferred
// rollback. Deferred directly, so recover sees the panic.
func (s *Service) recoverBookkeeping(r *liveRun, start time.Time) {
	v := recover()
	if v == nil {
		return
	}
	ticket := uuid.New()
	slog.Error("scheduled run bookkeeping panicked; node latched", "pkg", "scheduler",
		"taskId", r.task.ID, "tenant", string(r.task.TenantID), "ticket", ticket.String(),
		"err", fmt.Errorf("panic: %v", v), "stack", string(debug.Stack()))
	s.keepRun(r)
	s.latchAndCancel()
	s.m.runEnded(outcomePanicked, time.Since(start))
}

// markEnded records that r's fire has returned, and reads why it was
// cancelled, if it was, and whether the service is draining.
func (s *Service) markEnded(r *liveRun) (cancelReason, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.ended = true
	s.booking++
	return r.reason, s.draining
}

// fire runs the task. A panic is recovered here: it is logged at ERROR with
// the ticket lastError will carry, the claim is kept for good, and the node
// latches. The panicked run returns no report; its run guard's unsafe record
// was closed by the engine's own deferred calls as the panic unwound.
func (s *Service) fire(ctx context.Context, r *liveRun) (rep workflow.RunReport, panicked bool, ticket uuid.UUID) {
	defer func() {
		v := recover()
		if v == nil {
			return
		}
		rep, panicked, ticket = workflow.RunReport{}, true, uuid.New()
		slog.Error("scheduled run panicked; node latched", "pkg", "scheduler",
			"taskId", r.task.ID, "tenant", string(r.task.TenantID), "ticket", ticket.String(),
			"err", fmt.Errorf("panic: %v", v), "stack", string(debug.Stack()))
		s.keepRun(r)
		s.latchAndCancel()
	}()
	ctx = spi.WithUserContext(ctx, common.SystemUserContextValue(r.task.TenantID))
	ctx = workflow.WithRunGuard(ctx, &workflow.RunGuard{
		Ref: r.ref, Store: s.store, Done: r.ctx.Done(), NoNewUnsafe: s.drainingCh, Unsafe: r.unsafe,
	})
	return s.deps.Firer.FireScheduledTransition(ctx, r.task, s.cfg.MaxLostOwners, s.cfg.RetryDelay), false, uuid.Nil
}

// logOutcome writes the §9 line once the outcome is accepted. ticket is the
// one lastError carries, or nil. runErr is the run's error: when its recorded
// text is an Operational AppError that keeps its cause out of lastError (a
// storage outage), the line carries that cause, as the HTTP door's log does.
func (s *Service) logOutcome(r *liveRun, bk Bookkeeping, err error, ticket uuid.UUID, runErr error) {
	attrs := []any{"pkg", "scheduler", "taskId", r.task.ID, "tenant", string(r.task.TenantID),
		"entityId", r.task.EntityID, "transition", r.task.Transition}
	if cause := operationalCause(runErr); cause != nil {
		attrs = append(attrs, "cause", cause.Error())
	}
	switch {
	case errors.Is(err, spi.ErrStaleClaim):
		slog.Debug("scheduled run superseded", "pkg", "scheduler", "taskId", r.task.ID)
	case err != nil, bk.Kind == NoneKind:
	case bk.Kind == RecordAttemptKind:
		slog.Warn("scheduled run failed; the task waits for its next attempt", append(attrs,
			"counted", !bk.Attempt.NotCounted, "nextAttemptTime", bk.Attempt.NextAttemptTime,
			"lastError", bk.Attempt.Error)...)
	case bk.Kind == FailKind:
		if ticket == uuid.Nil {
			ticket = uuid.New()
		}
		slog.Error("scheduled task FAILED", append(attrs,
			"reason", string(bk.Failure.Reason), "ticket", ticket.String(), "lastError", bk.Failure.Error)...)
	}
}

// operationalCause is the cause an Operational AppError in err's chain keeps
// out of its client-facing message, or nil.
func operationalCause(err error) error {
	var appErr *common.AppError
	if errors.As(err, &appErr) && appErr.Level == common.LevelOperational {
		return appErr.Err
	}
	return nil
}

// book records the outcome with a fenced write that never inherits the run's
// cancellation. Every error is retried, after 1s and then doubling up to the
// heartbeat interval, until the write is accepted or refused, or until
// shutdown stops waiting for outcomes. Only a deterministic rejection by the
// store latches the node; its claim is kept.
func (s *Service) book(r *liveRun, bk Bookkeeping) error {
	if bk.Kind == NoneKind {
		return nil
	}
	delay := min(time.Second, s.cfg.HeartbeatInterval)
	var warned time.Time
	for {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.ctx), storeCallBudget)
		err := s.writeOutcome(ctx, r, bk)
		cancel()
		switch {
		case err == nil, errors.Is(err, spi.ErrStaleClaim):
			return err
		case errors.Is(err, spi.ErrStoreRejected):
			ticket := uuid.New()
			slog.Error("the store rejected a scheduled run's outcome; node latched", "pkg", "scheduler",
				"taskId", r.task.ID, "tenant", string(r.task.TenantID), "ticket", ticket.String(), "err", err)
			s.keepRun(r)
			s.latch()
			return err
		}
		s.m.bookkeepingRetried()
		if time.Since(warned) >= warnEvery {
			warned = time.Now()
			slog.Warn("scheduled run outcome not recorded; retrying", "pkg", "scheduler",
				"taskId", r.task.ID, "tenant", string(r.task.TenantID), "err", err)
		}
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-s.stopBooks:
			timer.Stop()
			return errBookkeepingStopped
		}
		delay = min(delay*2, s.cfg.HeartbeatInterval)
	}
}

func (s *Service) keepRun(r *liveRun) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.keep = true
}

func (s *Service) writeOutcome(ctx context.Context, r *liveRun, bk Bookkeeping) error {
	if bk.Kind == FailKind {
		return s.failWithAudit(ctx, r, bk.Failure)
	}
	return s.store.RecordAttempt(ctx, r.ref, bk.Attempt)
}

// failWithAudit writes Fail and the SCHEDULED_TRANSITION_FAIL event in one
// transaction (§5.7). The event's state is the entity's state in that
// transaction, or the task's source state when the entity is gone.
func (s *Service) failWithAudit(ctx context.Context, r *liveRun, f spi.Failure) error {
	ctx = spi.WithUserContext(ctx, common.SystemUserContextValue(r.task.TenantID))
	txID, txCtx, err := s.deps.TxManager.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin the transaction that fails a scheduled task: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			rbCtx, cancel := common.RollbackContext(txCtx)
			defer cancel()
			_ = s.deps.TxManager.Rollback(rbCtx, txID)
		}
	}()
	if err := s.store.Fail(txCtx, r.ref, f); err != nil {
		return fmt.Errorf("failed to fail the scheduled task: %w", err)
	}
	state := r.task.SourceState
	entities, err := s.deps.Store.EntityStore(txCtx)
	if err != nil {
		return fmt.Errorf("failed to get the entity store: %w", err)
	}
	switch entity, err := entities.Get(txCtx, r.task.EntityID); {
	case err == nil:
		state = entity.Meta.State
	case !errors.Is(err, spi.ErrNotFound):
		return fmt.Errorf("failed to read the entity of a failed scheduled task: %w", err)
	}
	audit, err := s.deps.Store.StateMachineAuditStore(txCtx)
	if err != nil {
		return fmt.Errorf("failed to get the audit store: %w", err)
	}
	if err := audit.Record(txCtx, r.task.EntityID, spi.StateMachineEvent{
		EventType:     spi.SMEventScheduledTransitionFailed,
		EntityID:      r.task.EntityID,
		State:         state,
		TransactionID: txID,
		Details:       fmt.Sprintf("Scheduled transition %q failed: %s", r.task.Transition, f.Reason),
		Data: map[string]any{
			"transition":  r.task.Transition,
			"sourceState": r.task.SourceState,
			"reason":      string(f.Reason),
			"attempts":    r.task.Attempts,
			"lostOwners":  r.task.LostOwners,
		},
		Timestamp: time.UnixMilli(f.AtMs),
	}); err != nil {
		return fmt.Errorf("failed to record the scheduled task's failure: %w", err)
	}
	if err := common.ShieldedCommit(txCtx, func(c context.Context) error {
		return s.deps.TxManager.Commit(c, txID)
	}); err != nil {
		return fmt.Errorf("failed to commit the scheduled task's failure: %w", err)
	}
	committed = true
	return nil
}

// finish releases the claim once its outcome is accepted or refused. A claim
// whose outcome is not recorded, because shutdown stopped the retry or the
// store rejected it, stays in the set, and so does a panicked run's claim
// even once its FAILED write is accepted: it is never given back (§6.5).
func (s *Service) finish(r *liveRun, bk Bookkeeping, bookErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.keep || (bk.Kind != NoneKind && bookErr != nil && !errors.Is(bookErr, spi.ErrStaleClaim)) {
		return
	}
	s.releaseLocked(r)
	if s.filled {
		s.filled = false
		select {
		case s.slotFreed <- struct{}{}:
		default:
		}
	}
}

func (s *Service) releaseLocked(r *liveRun) {
	if _, ok := s.runs[r.ref.ClaimToken]; !ok {
		return
	}
	delete(s.runs, r.ref.ClaimToken)
	s.perTenant[r.task.TenantID]--
	if s.perTenant[r.task.TenantID] <= 0 {
		delete(s.perTenant, r.task.TenantID)
	}
	r.cancel()
}

func runOutcome(rep workflow.RunReport, bk Bookkeeping, panicked bool, reason cancelReason, cut bool) string {
	switch {
	case panicked:
		return outcomePanicked
	case rep.Outcome != workflow.OutcomeFailed:
		return string(rep.Outcome)
	case bk.Kind == FailKind:
		return outcomeFailed
	case cut:
		return outcomeShutdownCancelled
	case reason == selfCancelled:
		return outcomeSelfCancelled
	default:
		return outcomeAttemptFailed
	}
}

// --- panics -----------------------------------------------------------------

// recoverLatch recovers a panic in a scheduler goroutine and latches the node.
// Deferred directly, so recover sees the panic.
func (s *Service) recoverLatch(site string) {
	v := recover()
	if v == nil {
		return
	}
	ticket := uuid.New()
	slog.Error("panic recovered in the scheduler "+site+"; node latched", "pkg", "scheduler",
		"ticket", ticket.String(), "err", fmt.Errorf("panic: %v", v), "stack", string(debug.Stack()))
	s.latchAndCancel()
}

// latch marks the node unhealthy for good. A latched node claims nothing, and
// its runs in progress go on: only a panic recovery cancels them
// (latchAndCancel). It keeps heartbeating unless the heartbeat itself
// panicked, so its runs are not taken over while they finish.
func (s *Service) latch() {
	if s.deps.HealthFlag != nil {
		s.deps.HealthFlag.Store(false)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.latched = true
}

// latchAndCancel is the latch of a recovered panic: it latches the node and
// cancels every run in progress (§6.5).
func (s *Service) latchAndCancel() {
	s.latch()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.runs {
		if r.ended {
			continue
		}
		if r.reason == notCancelled {
			r.reason = latchCancelled
		}
		r.cancel()
	}
}
