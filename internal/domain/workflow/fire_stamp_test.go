package workflow

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func failing(name string) func(context.Context, spi.ProcessorDefinition, string) (*spi.Entity, error) {
	return func(_ context.Context, proc spi.ProcessorDefinition, _ string) (*spi.Entity, error) {
		if proc.Name == name {
			return nil, errors.New(name + " failed")
		}
		return nil, nil
	}
}

func TestStamp_FiredTransitionSegment_NotPartial_RetriedFromTXPre(t *testing.T) {
	ext := &scriptedExtProc{processor: failing("p2")}
	env := newRunEnv(t, ext)
	first := env.claimed(t, "txpre-e1", oneHopWF("CLOSED", []spi.ProcessorDefinition{
		safeProc("p1", ExecutionModeCommitBeforeDispatch), safeProc("p2", ExecutionModeSync),
	}, nil))

	r, _ := env.run(first)
	if r.Outcome != OutcomeFailed || r.PartialCommit {
		t.Fatalf("first run = %+v, want a safe failure without PartialCommit", r)
	}
	if got, _ := env.task(t, first.ID); got.PartialCommit {
		t.Errorf("stored task = %+v, want PartialCommit false", got)
	}
	if got := env.state(t, "txpre-e1"); got != "OPEN" {
		t.Errorf("entity state = %q, want OPEN (TX_pre)", got)
	}
	ref := spi.TaskRef{TenantID: testTenant, ID: first.ID, ArmToken: first.ArmToken, ClaimToken: first.Claim.Token}
	if err := env.sts.RecordAttempt(env.ctx, ref, spi.Attempt{Error: "p2 failed", AtMs: env.nowMs(), NextAttemptTime: env.nowMs()}); err != nil {
		t.Fatalf("RecordAttempt: %v", err)
	}
	ext.processor = nil
	r, _ = env.run(env.claimOne(t, testOwner, false))
	if r.Outcome != OutcomeFired {
		t.Fatalf("retry = %+v, want fired", r)
	}
}

func cascadePartialWF() spi.WorkflowDefinition {
	return oneHopWF("MID", nil, map[string]spi.StateDefinition{
		"MID": autoStep("DONE", safeProc("p1", ExecutionModeCommitBeforeDispatch), safeProc("p2", ExecutionModeSync)),
	})
}

func TestStamp_CascadeStepSegment_SetsPartialCommit(t *testing.T) {
	ext := &scriptedExtProc{processor: failing("p2")}
	env := newRunEnv(t, ext)
	claimed := env.claimed(t, "cascade-e1", cascadePartialWF())

	r, _ := env.run(claimed)
	if r.Outcome != OutcomeFailed || !r.PartialCommit {
		t.Fatalf("report = %+v, want failed with PartialCommit", r)
	}
	if got, _ := env.task(t, claimed.ID); !got.PartialCommit {
		t.Errorf("stored task = %+v, want PartialCommit true", got)
	}
	if got := env.state(t, "cascade-e1"); got != "MID" {
		t.Errorf("entity state = %q, want MID (committed by the cascade step's segment)", got)
	}
}

// failFirstCommitTxMgr fails the engine's first Commit and commits nothing.
type failFirstCommitTxMgr struct {
	spi.TransactionManager
	failed bool
}

func (m *failFirstCommitTxMgr) Commit(ctx context.Context, txID string) error {
	if !m.failed {
		m.failed = true
		return errors.New("commit failed")
	}
	return m.TransactionManager.Commit(ctx, txID)
}

// PartialCommit records a segment that committed, not one that was stamped:
// a cascade-step segment whose commit fails leaves it false, in the report
// and in the store.
func TestStamp_SegmentCommitFails_NoPartialCommit(t *testing.T) {
	ext := &scriptedExtProc{}
	env := newRunEnvWith(t, ext, nil, func(tm spi.TransactionManager) spi.TransactionManager {
		return &failFirstCommitTxMgr{TransactionManager: tm}
	})
	claimed := env.claimed(t, "commitfail-e1", cascadePartialWF())

	r, _ := env.run(claimed)
	if r.Outcome != OutcomeFailed || r.PartialCommit {
		t.Fatalf("report = %+v, want failed without PartialCommit: the segment did not commit", r)
	}
	if got, _ := env.task(t, claimed.ID); got.PartialCommit {
		t.Errorf("stored task = %+v, want PartialCommit false", got)
	}
	if got := env.state(t, "commitfail-e1"); got != "OPEN" {
		t.Errorf("entity state = %q, want OPEN: nothing committed", got)
	}
	if n := ext.count("p1"); n != 0 {
		t.Errorf("p1 dispatched %d times, want 0", n)
	}
}

func TestStamp_NextClaimAfterPartialCommit_Failed(t *testing.T) {
	ext := &scriptedExtProc{processor: failing("p2")}
	env := newRunEnv(t, ext)
	first := env.claimed(t, "killed-e1", cascadePartialWF())
	env.run(first) // the owner then dies without bookkeeping

	second := env.claimOne(t, otherOwner, true)
	r, _ := env.run(second)
	if r.Outcome != OutcomeFailed || r.FailReason != spi.FailureStoppedAfterPartialCommit {
		t.Fatalf("report = %+v, want FAILED STOPPED_AFTER_PARTIAL_COMMIT", r)
	}
	if n := ext.count("p1"); n != 1 {
		t.Errorf("p1 dispatched %d times, want 1", n)
	}
}

func TestStamp_CascadeBackInSourceState_SetsPartialCommit(t *testing.T) {
	ext := &scriptedExtProc{processor: failing("p2")}
	env := newRunEnv(t, ext)
	wf := oneHopWF("MID", nil, map[string]spi.StateDefinition{"MID": autoStep("OPEN")})
	open := wf.States["OPEN"]
	open.Transitions = append(open.Transitions, spi.TransitionDefinition{Name: "Leave", Next: "GONE",
		Processors: []spi.ProcessorDefinition{safeProc("p1", ExecutionModeCommitBeforeDispatch), safeProc("p2", ExecutionModeSync)}})
	wf.States["OPEN"] = open
	wf.States["GONE"] = spi.StateDefinition{}
	claimed := env.claimed(t, "back-e1", wf)

	r, _ := env.run(claimed)
	if r.Outcome != OutcomeFailed || !r.PartialCommit {
		t.Fatalf("report = %+v, want PartialCommit: the segment was committed by the cascade, back in the source state", r)
	}
}

func TestStamp_ReplacedOwnerSegmentRefused(t *testing.T) {
	var env *runEnv
	ext := &scriptedExtProc{processor: func(_ context.Context, proc spi.ProcessorDefinition, _ string) (*spi.Entity, error) {
		if proc.Name != "p1" {
			return nil, nil
		}
		got, err := env.sts.ClaimDue(env.ctx, claimRequest(otherOwner, env.nowMs(), true))
		if err == nil && len(got) != 1 {
			err = fmt.Errorf("reclaim claimed %d tasks", len(got))
		}
		return nil, err
	}}
	env = newRunEnv(t, ext)
	startNewTx := true
	p1 := safeProc("p1", ExecutionModeCommitBeforeDispatch)
	p1.Config.StartNewTxOnDispatch = &startNewTx
	claimed := env.claimed(t, "replaced-e1", oneHopWF("MID", nil, map[string]spi.StateDefinition{
		"MID": autoStep("DONE", p1, safeProc("p2", ExecutionModeCommitBeforeDispatch)),
	}))

	r, _ := env.run(claimed)
	if r.Outcome != OutcomeSuperseded {
		t.Fatalf("report = %+v, want superseded (p2's stamp is refused)", r)
	}
	if n := ext.count("p2"); n != 0 {
		t.Errorf("p2 dispatched %d times, want 0: its segment must not commit", n)
	}
	if got, _ := env.task(t, claimed.ID); got.Claim == nil || got.Claim.Owner != otherOwner {
		t.Errorf("task = %+v, want RUNNING under the other owner", got)
	}
}

func TestStamp_CallbackReArmedInRunTx_StampRefused(t *testing.T) {
	var env *runEnv
	var claimed spi.ScheduledTask
	ext := &scriptedExtProc{processor: func(_ context.Context, proc spi.ProcessorDefinition, txID string) (*spi.Entity, error) {
		if proc.Name != "p0" {
			return nil, nil
		}
		jctx, err := env.txMgr.Join(env.ctx, txID)
		if err != nil {
			return nil, err
		}
		_, err = env.sts.ReconcileForEntity(jctx, spi.ReconcileRequest{
			TenantID: testTenant, EntityID: claimed.EntityID, CurrentState: "OPEN", Arm: []spi.ScheduledTask{armable(claimed)},
		})
		return nil, err
	}}
	env = newRunEnv(t, ext)
	claimed = env.claimed(t, "cbstamp-e1", oneHopWF("CLOSED", []spi.ProcessorDefinition{
		safeProc("p0", ExecutionModeSync), safeProc("p1", ExecutionModeCommitBeforeDispatch),
	}, nil))

	r, _ := env.run(claimed)
	if r.Outcome != OutcomeFailed || !errors.Is(r.Err, spi.ErrStaleClaim) {
		t.Fatalf("report = %+v, want an ordinary failure: the refusal came from the run's own transaction", r)
	}
	if n := ext.count("p1"); n != 0 {
		t.Errorf("p1 dispatched %d times, want 0", n)
	}
	if got, _ := env.task(t, claimed.ID); got.ArmToken != claimed.ArmToken || got.Claim == nil || got.Claim.Token != claimed.Claim.Token {
		t.Errorf("task = %+v, want the claimed life, still under this claim", got)
	}
}

func TestStamp_CommitFindsGuardByTransaction(t *testing.T) {
	cases := []struct {
		name          string
		guard         func(claimed spi.ScheduledTask) *RunGuard
		registerOther bool // register the guard to a different transaction id
		wantErr       func(error) bool
		wantState     string
	}{
		{name: "stale claim token → stamp refused",
			guard: func(c spi.ScheduledTask) *RunGuard {
				return &RunGuard{Ref: spi.TaskRef{TenantID: testTenant, ID: c.ID, ArmToken: c.ArmToken, ClaimToken: uuid.New()}}
			},
			wantErr: func(err error) bool { return errors.Is(err, spi.ErrStaleClaim) }, wantState: "OPEN"},
		{name: "cancelled run → not committed",
			guard: func(c spi.ScheduledTask) *RunGuard {
				done := make(chan struct{})
				close(done)
				return &RunGuard{Ref: spi.TaskRef{TenantID: testTenant, ID: c.ID, ArmToken: c.ArmToken, ClaimToken: c.Claim.Token}, Done: done}
			},
			wantErr: func(err error) bool { return errors.Is(err, context.Canceled) }, wantState: "OPEN"},
		{name: "guard in another tenant → stamp refused, not committed",
			guard: func(c spi.ScheduledTask) *RunGuard {
				return &RunGuard{Ref: spi.TaskRef{TenantID: "other-tenant", ID: c.ID, ArmToken: c.ArmToken, ClaimToken: c.Claim.Token}}
			},
			wantErr: func(err error) bool {
				return errors.Is(err, spi.ErrTxTenantMismatch) && errors.Is(err, ErrCommitBeforeDispatchInfra)
			}, wantState: "OPEN"},
		{name: "guard of another transaction → this commit is not guarded",
			guard: func(c spi.ScheduledTask) *RunGuard {
				done := make(chan struct{})
				close(done)
				return &RunGuard{Ref: spi.TaskRef{TenantID: testTenant, ID: c.ID, ArmToken: c.ArmToken, ClaimToken: uuid.New()}, Done: done}
			},
			registerOther: true,
			wantErr:       func(err error) bool { return err == nil }, wantState: "CLOSED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newRunEnv(t, &scriptedExtProc{})
			claimed := env.claimed(t, "bytx-e1", oneHopWF("CLOSED", nil, nil))
			g := tc.guard(claimed)
			g.Store = env.sts

			txID, txCtx, err := env.txMgr.Begin(env.ctx) // env.ctx carries no guard
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			defer env.txMgr.Rollback(env.ctx, txID)
			regID := txID
			if tc.registerOther {
				regID = uuid.NewString()
			}
			env.engine.runTxs.register(regID, g)
			defer env.engine.runTxs.release(g)
			es, _ := env.factory.EntityStore(txCtx)
			ent, err := es.Get(txCtx, "bytx-e1")
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			ent.Meta.State = "CLOSED"

			err = env.engine.flushAndCommitSegment(txCtx, ent, txID, "", false)
			if !tc.wantErr(err) {
				t.Fatalf("flushAndCommitSegment = %v", err)
			}
			if got := env.state(t, "bytx-e1"); got != tc.wantState {
				t.Errorf("entity state = %q, want %q", got, tc.wantState)
			}
		})
	}
}

// stampErrStore answers StampSegment with err instead of the real store.
type stampErrStore struct {
	spi.ScheduledTaskStore
	err error
}

func (s stampErrStore) StampSegment(context.Context, spi.TaskRef, bool) error { return s.err }

// A refused stamp is the run's refusal, not an infrastructure failure; any
// other stamp error is infrastructure. Neither lets the segment commit, and
// the processor behind it is not dispatched.
func TestStamp_ErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		wantInfra bool
	}{
		{"conflict", fmt.Errorf("task row: %w", spi.ErrConflict), false},
		{"stale claim", fmt.Errorf("task row: %w", spi.ErrStaleClaim), false},
		{"store failure", errors.New("connection reset"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ext := &scriptedExtProc{}
			env := newRunEnv(t, ext)
			claimed := env.claimed(t, "stamperr-e1", oneHopWF("CLOSED", []spi.ProcessorDefinition{
				safeProc("p1", ExecutionModeCommitBeforeDispatch),
			}, nil))
			run := newTestRun(stampErrStore{ScheduledTaskStore: env.sts, err: tc.err}, claimed)

			r := run.fire(env.engine, env.ctx, claimed)
			if r.Outcome != OutcomeFailed || !errors.Is(r.Err, tc.err) {
				t.Fatalf("report = %+v, want failed with the stamp error", r)
			}
			if got := errors.Is(r.Err, ErrCommitBeforeDispatchInfra); got != tc.wantInfra {
				t.Errorf("infra = %v, want %v (err %v)", got, tc.wantInfra, r.Err)
			}
			if n := ext.count("p1"); n != 0 {
				t.Errorf("p1 dispatched %d times, want 0", n)
			}
			if got := env.state(t, "stamperr-e1"); got != "OPEN" {
				t.Errorf("entity state = %q, want OPEN: the segment must not commit", got)
			}
		})
	}
}

// txProbe records, for each processor call, the transaction it saw and
// whether that transaction was registered to the run's guard.
type txProbe struct {
	txIDs      []string
	registered []bool
}

func (p *txProbe) see(env *runEnv, run *testRun, txID string) {
	p.txIDs = append(p.txIDs, txID)
	p.registered = append(p.registered, env.engine.runTxs.forTx(txID) == run.guard)
}

// Every transaction a run begins is registered to its guard while the run is
// open, and released on every ending, a panic included: a leaked entry would
// hand a stale guard to a later transaction that reuses the id.
func TestRunTxs_ReleasedOnEveryEnding(t *testing.T) {
	for _, tc := range []struct {
		name  string
		p2    func(env *runEnv) error
		panic bool
		want  ScheduledOutcome
	}{
		{name: "fired", p2: func(*runEnv) error { return nil }, want: OutcomeFired},
		{name: "failed", p2: func(*runEnv) error { return errors.New("p2 failed") }, want: OutcomeFailed},
		{name: "superseded", p2: func(env *runEnv) error {
			_, err := env.sts.ClaimDue(env.ctx, claimRequest(otherOwner, env.nowMs(), true))
			return err
		}, want: OutcomeSuperseded},
		{name: "panic", panic: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var env *runEnv
			var run *testRun
			probe := &txProbe{}
			ext := &scriptedExtProc{processor: func(_ context.Context, proc spi.ProcessorDefinition, txID string) (*spi.Entity, error) {
				probe.see(env, run, txID)
				if proc.Name != "p2" {
					return nil, nil
				}
				if tc.panic {
					panic("p2 panicked")
				}
				return nil, tc.p2(env)
			}}
			env = newRunEnv(t, ext)
			startNewTx := true
			p1 := safeProc("p1", ExecutionModeCommitBeforeDispatch)
			p1.Config.StartNewTxOnDispatch = &startNewTx
			claimed := env.claimed(t, "release-"+tc.name, oneHopWF("MID", nil, map[string]spi.StateDefinition{
				"MID": autoStep("DONE", safeProc("p0", ExecutionModeSync), p1, safeProc("p2", ExecutionModeSync)),
			}))
			run = newTestRun(env.sts, claimed)

			if tc.panic {
				if recovered := firePanics(env, run, claimed); recovered == nil {
					t.Fatal("the dispatch panic did not propagate")
				}
			} else if r := run.fire(env.engine, env.ctx, claimed); r.Outcome != tc.want {
				t.Fatalf("report = %+v, want %s", r, tc.want)
			}
			if len(probe.txIDs) != 3 || probe.txIDs[0] == probe.txIDs[1] {
				t.Fatalf("processors saw transactions %v, want the entry transaction then TX_post twice", probe.txIDs)
			}
			for i, id := range probe.txIDs {
				if !probe.registered[i] {
					t.Errorf("transaction %s was not registered to the run's guard during the run", id)
				}
				if g := env.engine.runTxs.forTx(id); g != nil {
					t.Errorf("transaction %s still registered after the run ended", id)
				}
			}
		})
	}
}

// A TX_post begun after a COMMIT_BEFORE_DISPATCH commit inherits the guard of
// the transaction just committed, at both begin sites. Its own segment commit
// is therefore stamped: after the owner is replaced inside TX_post, p2's
// segment is refused and p2 is never dispatched.
func TestRunTxs_TXPostInheritsGuard(t *testing.T) {
	for _, startNewTx := range []bool{true, false} {
		t.Run(fmt.Sprintf("startNewTxOnDispatch=%v", startNewTx), func(t *testing.T) {
			var env *runEnv
			var run *testRun
			inherited := false
			ext := &scriptedExtProc{processor: func(_ context.Context, proc spi.ProcessorDefinition, txID string) (*spi.Entity, error) {
				if proc.Name != "pmid" {
					return nil, nil
				}
				inherited = env.engine.runTxs.forTx(txID) == run.guard
				got, err := env.sts.ClaimDue(env.ctx, claimRequest(otherOwner, env.nowMs(), true))
				if err == nil && len(got) != 1 {
					err = fmt.Errorf("reclaim claimed %d tasks", len(got))
				}
				return nil, err
			}}
			env = newRunEnv(t, ext)
			p1 := safeProc("p1", ExecutionModeCommitBeforeDispatch)
			p1.Config.StartNewTxOnDispatch = &startNewTx
			claimed := env.claimed(t, "inherit-e1", oneHopWF("CLOSED", []spi.ProcessorDefinition{
				p1, safeProc("pmid", ExecutionModeSync), safeProc("p2", ExecutionModeCommitBeforeDispatch),
			}, nil))
			run = newTestRun(env.sts, claimed)

			r := run.fire(env.engine, env.ctx, claimed)
			if !inherited {
				t.Error("TX_post was not registered to the run's guard")
			}
			if r.Outcome != OutcomeSuperseded {
				t.Fatalf("report = %+v, want superseded (p2's stamp is refused)", r)
			}
			if n := ext.count("p2"); n != 0 {
				t.Errorf("p2 dispatched %d times, want 0: its segment must not commit", n)
			}
		})
	}
}

// The run's final commit finds its guard by the transaction it commits, as
// the segment commits do: the guard on the context does not decide.
func TestCommitRun_FindsGuardByTransaction(t *testing.T) {
	cases := []struct {
		name          string
		ctxCancelled  *bool // nil: no guard on the context
		regCancelled  bool
		wantCancelled bool
		wantState     string
	}{
		{name: "no guard on the context, registered guard cancelled", regCancelled: true, wantCancelled: true, wantState: "OPEN"},
		{name: "live guard on the context, registered guard cancelled", ctxCancelled: new(bool), regCancelled: true, wantCancelled: true, wantState: "OPEN"},
		{name: "cancelled guard on the context, registered guard live", ctxCancelled: func() *bool { b := true; return &b }(), wantState: "CLOSED"},
	}
	guard := func(cancelled bool) *RunGuard {
		done := make(chan struct{})
		if cancelled {
			close(done)
		}
		return &RunGuard{Done: done}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newRunEnv(t, &scriptedExtProc{})
			env.setup(t, "commitrun-e1", oneHopWF("CLOSED", nil, nil))
			ctx := env.ctx
			if tc.ctxCancelled != nil {
				ctx = WithRunGuard(ctx, guard(*tc.ctxCancelled))
			}
			txID, txCtx, err := env.txMgr.Begin(ctx)
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			defer env.txMgr.Rollback(env.ctx, txID)
			g := guard(tc.regCancelled)
			env.engine.runTxs.register(txID, g)
			defer env.engine.runTxs.release(g)
			es, _ := env.factory.EntityStore(txCtx)
			ent, err := es.Get(txCtx, "commitrun-e1")
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			ent.Meta.State = "CLOSED"
			if _, err := es.Save(txCtx, ent); err != nil {
				t.Fatalf("Save: %v", err)
			}

			err = env.engine.commitRun(txCtx, txID)
			if got := errors.Is(err, context.Canceled); got != tc.wantCancelled {
				t.Fatalf("commitRun = %v, want cancelled=%v", err, tc.wantCancelled)
			}
			if got := env.state(t, "commitrun-e1"); got != tc.wantState {
				t.Errorf("entity state = %q, want %q", got, tc.wantState)
			}
		})
	}
}

// The run state the segment commit reads and writes is safe on its own: two
// segment commits of one run on different goroutines do not race, whether or
// not something refused one of them earlier (spec §5.2). Run with -race.
func TestStamp_RunStateSafeAcrossGoroutines(t *testing.T) {
	env := newRunEnv(t, &scriptedExtProc{})
	claimed := env.claimed(t, "race-e1", oneHopWF("CLOSED", nil, nil))
	run := newTestRun(env.sts, claimed)
	g := run.guard
	g.firedTransitionDone.Store(true)
	defer env.engine.runTxs.release(g)

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		txID, txCtx, err := env.txMgr.Begin(env.ctx)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		defer env.txMgr.Rollback(env.ctx, txID)
		env.engine.runTxs.register(txID, g)
		es, _ := env.factory.EntityStore(txCtx)
		ent, err := es.Get(txCtx, "race-e1")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			// The two stamps conflict with each other; either outcome is fine.
			_ = env.engine.flushAndCommitSegment(txCtx, ent, txID, "", false)
			_ = g.partialCommitted.Load()
		}()
	}
	wg.Wait()
}
