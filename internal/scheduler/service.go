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
	keep   bool // never given back: the store rejected its outcome
}

// Service is one pnode's scheduler: a claim loop, a heartbeat goroutine, a
// watchdog goroutine and one goroutine per run.
type Service struct {
	cfg         Config
	deps        Deps
	incarnation uuid.UUID
	window      time.Duration // W
	sweepEvery  time.Duration
	m           *metrics
	store       spi.ScheduledTaskStore

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

	slotFreed  chan struct{}
	drainingCh chan struct{} // closed at shutdown step 1, before the drain reads any run's Unsafe record
	stopLoop   chan struct{}
	loopDone   chan struct{}
	stopHB     chan struct{}
	hbDone     chan struct{}
	wdDone     chan struct{}
	wdArm      chan time.Time
	stopBooks  chan struct{} // closed when shutdown stops waiting for outcomes to be recorded
	runsWG     sync.WaitGroup

	startOnce sync.Once
	startErr  error // the first Start's error, returned by every later call
	stopOnce  sync.Once
}

// New builds a service. Start begins claiming.
func New(cfg Config, deps Deps) *Service {
	return &Service{
		cfg:         cfg,
		deps:        deps,
		incarnation: uuid.New(),
		window:      watchdogWindow(cfg.StaleAfter),
		sweepEvery:  sweepInterval,
		runs:        make(map[uuid.UUID]*liveRun),
		perTenant:   make(map[spi.TenantID]int),
		slotFreed:   make(chan struct{}, 1),
		drainingCh:  make(chan struct{}),
		stopLoop:    make(chan struct{}),
		loopDone:    make(chan struct{}),
		stopHB:      make(chan struct{}),
		hbDone:      make(chan struct{}),
		wdDone:      make(chan struct{}),
		wdArm:       make(chan time.Time, 1),
		stopBooks:   make(chan struct{}),
	}
}

// Start starts the heartbeat, the watchdog and the claim loop. It returns
// once the first heartbeat is scheduled. A disabled service starts nothing.
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
	func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.m, s.store, s.started = m, store, true
	}()
	go s.watchdogLoop()
	go s.heartbeatLoop()
	go s.loop()
	// The multi-node scenarios map a claim's owner to its pnode by this
	// line. The incarnation is an identifier, not a secret.
	slog.Info("scheduler started", "pkg", "scheduler", "incarnation", s.incarnation.String())
	return nil
}

// Stop stops the claim loop, the heartbeat and the watchdog.
func (s *Service) Stop() {
	s.stopOnce.Do(func() {
		if !s.isStarted() {
			return
		}
		close(s.stopBooks)
		close(s.stopLoop)
		<-s.loopDone
		close(s.stopHB)
		<-s.hbDone
		<-s.wdDone
	})
}

func (s *Service) isStarted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started
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
		case <-s.stopHB:
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

func (s *Service) tick() {
	if !s.isHealthy() {
		return
	}
	s.giveBackIdle()
	s.claim()
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
	for _, r := range s.register(tasks, free) {
		s.m.claimed(claimReason(r.task))
		go s.run(r)
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
func (s *Service) register(tasks []spi.ScheduledTask, free int) []*liveRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.filled = len(tasks) >= free
	runs := make([]*liveRun, 0, len(tasks))
	for _, t := range tasks {
		runs = append(runs, s.registerLocked(t))
	}
	return runs
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
	s.runsWG.Add(1)
	s.m.runStarted()
	switch {
	case s.latched:
		r.reason = latchCancelled
		cancel()
	case !time.Now().Before(s.wdDeadline):
		// The watchdog's deadline passed while the claim was in flight.
		// A heartbeat that merely failed meanwhile does not cancel: one
		// failed heartbeat never self-cancels.
		r.reason = selfCancelled
		cancel()
	}
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

func (s *Service) run(r *liveRun) {
	defer s.runsWG.Done()
	start := time.Now()
	ctx, span := observability.Tracer().Start(r.ctx, "scheduler.run")
	defer span.End()

	rep := s.fire(ctx, r)
	nowMs := s.deps.Clock.Now().UnixMilli()

	reason, draining := s.markEnded(r)

	cut := rep.Outcome == workflow.OutcomeFailed && errors.Is(rep.Err, context.Canceled) &&
		(reason == shutdownCancelled || (reason == notCancelled && draining))
	var errText string
	errTicket := uuid.Nil
	if rep.Err != nil {
		text, ticket, warnOnly := recordedError(rep.Err)
		errText, errTicket = sanitiseErrorText(text), ticket
		if !warnOnly {
			slog.Error("scheduled run failed with an internal error", "pkg", "scheduler",
				"taskId", r.task.ID, "tenant", string(r.task.TenantID), "ticket", ticket.String(), "err", rep.Err)
		}
	}
	bk := decideBookkeeping(rep, r.task, cut, false, nowMs, s.cfg, errText)
	outcome := runOutcome(rep, bk, false, reason, cut)
	err := s.book(r, bk)
	s.logOutcome(r, bk, err, errTicket, rep.Err)
	if errors.Is(err, spi.ErrStaleClaim) {
		outcome = outcomeSuperseded
	}
	s.finish(r, bk, err)
	span.SetAttributes(attribute.String("outcome", outcome))
	s.m.runEnded(outcome, time.Since(start))
}

// markEnded records that r's fire has returned, and reads why it was
// cancelled, if it was, and whether the service is draining.
func (s *Service) markEnded(r *liveRun) (cancelReason, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.ended = true
	return r.reason, s.draining
}

func (s *Service) fire(ctx context.Context, r *liveRun) workflow.RunReport {
	ctx = spi.WithUserContext(ctx, common.SystemUserContextValue(r.task.TenantID))
	ctx = workflow.WithRunGuard(ctx, &workflow.RunGuard{
		Ref: r.ref, Store: s.store, Done: r.ctx.Done(), NoNewUnsafe: s.drainingCh, Unsafe: r.unsafe,
	})
	return s.deps.Firer.FireScheduledTransition(ctx, r.task, s.cfg.MaxLostOwners, s.cfg.RetryDelay)
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
// store rejected it, stays in the set.
func (s *Service) finish(r *liveRun, bk Bookkeeping, bookErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if bk.Kind != NoneKind && bookErr != nil && !errors.Is(bookErr, spi.ErrStaleClaim) {
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
	s.latch()
}

// latch marks the node unhealthy for good; a latched node claims nothing.
func (s *Service) latch() {
	if s.deps.HealthFlag != nil {
		s.deps.HealthFlag.Store(false)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.latched = true
}
