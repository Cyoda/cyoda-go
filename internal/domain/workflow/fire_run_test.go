package workflow

import (
	"context"
	"errors"
	"fmt"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

func TestPreRunDecision(t *testing.T) {
	timeout := int64(1_000)
	retry := testRetryDelay
	base := spi.ScheduledTask{ScheduledTime: 10_000, TimeoutMs: &timeout}
	deadline := base.ScheduledTime + timeout
	with := func(f func(*spi.ScheduledTask)) spi.ScheduledTask { tk := base; f(&tk); return tk }

	cases := []struct {
		name       string
		task       spi.ScheduledTask
		nowMs      int64
		wantReason spi.ScheduledTaskFailureReason
		wantExpire bool
	}{
		{"mark first, before partial and lateness", with(func(tk *spi.ScheduledTask) { tk.UnsafeMarked, tk.PartialCommit, tk.Attempts = true, true, 5 }), deadline + 999_999, spi.FailureUnsafeWorkNotCompleted, false},
		{"partial commit", with(func(tk *spi.ScheduledTask) { tk.PartialCommit = true }), 0, spi.FailureStoppedAfterPartialCommit, false},
		{"owner lost max times", with(func(tk *spi.ScheduledTask) { tk.LostOwners = 3 }), 0, spi.FailureOwnerLostRepeatedly, false},
		{"owner lost once less", with(func(tk *spi.ScheduledTask) { tk.LostOwners = 2 }), deadline, "", false},
		{"no timeout, many attempts", spi.ScheduledTask{ScheduledTime: 10_000, Attempts: 40}, 9_999_999, "", false},
		{"first attempt at the deadline", base, deadline, "", false},
		{"first attempt 1ms late", base, deadline + 1, "", true},
		{"after a failed attempt, at deadline+retry", with(func(tk *spi.ScheduledTask) { tk.Attempts = 1 }), deadline + retry.Milliseconds(), "", false},
		{"after a failed attempt, 1ms beyond", with(func(tk *spi.ScheduledTask) { tk.Attempts = 1 }), deadline + retry.Milliseconds() + 1, spi.FailureExpiredAfterFailedAttempts, false},
		{"after a lost owner, late but within retry", with(func(tk *spi.ScheduledTask) { tk.LostOwners = 1 }), deadline + 1, "", false},
		{"after a lost owner, beyond retry", with(func(tk *spi.ScheduledTask) { tk.LostOwners = 1 }), deadline + retry.Milliseconds() + 1, spi.FailureExpiredAfterFailedAttempts, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, expire := preRunDecision(tc.task, tc.nowMs, testMaxLostOwners, retry)
			if reason != tc.wantReason || expire != tc.wantExpire {
				t.Fatalf("preRunDecision = (%q, %v), want (%q, %v)", reason, expire, tc.wantReason, tc.wantExpire)
			}
		})
	}
}

func TestFireScheduled_PreRunFailureTouchesNothing(t *testing.T) {
	ext := &scriptedExtProc{}
	env := newRunEnv(t, ext)
	claimed := env.claimed(t, "prerun-e1", oneHopWF("CLOSED", []spi.ProcessorDefinition{unsafeProc("p1", ExecutionModeSync)}, nil))
	claimed.LostOwners = testMaxLostOwners

	r, _ := env.run(claimed)
	if r.Outcome != OutcomeFailed || r.FailReason != spi.FailureOwnerLostRepeatedly || r.Err != nil {
		t.Fatalf("report = %+v, want failed OWNER_LOST_REPEATEDLY without an error", r)
	}
	if n := ext.count("p1"); n != 0 {
		t.Errorf("p1 dispatched %d times, want 0", n)
	}
	if got, _ := env.task(t, claimed.ID); got.Status != spi.ScheduledTaskRunning || got.Claim == nil || got.Claim.Token != claimed.Claim.Token {
		t.Errorf("task = %+v, want still RUNNING under this claim (the scheduler writes Fail)", got)
	}
}

func TestFireScheduled_ExpiredOnFirstAttempt(t *testing.T) {
	env := newRunEnv(t, nil)
	wf := oneHopWF("CLOSED", nil, nil)
	armed := env.setup(t, "exp-e1", wf)
	timeout := int64(1_000)
	armed.TimeoutMs = &timeout
	armTask(t, env.factory, env.ctx, armed)
	env.advance(timeout + 1)
	claimed := env.claimOne(t, testOwner, false)

	r, _ := env.run(claimed)
	if r.Outcome != OutcomeExpired || r.Err != nil {
		t.Fatalf("report = %+v, want expired", r)
	}
	if _, found := env.task(t, claimed.ID); found {
		t.Error("expired task must be removed")
	}
	if got := env.state(t, "exp-e1"); got != "OPEN" {
		t.Errorf("entity state = %q, want OPEN", got)
	}
	if n := countAuditEvents(t, env.factory, env.ctx, "exp-e1", spi.SMEventScheduledTransitionExpired); n != 1 {
		t.Errorf("SCHEDULED_TRANSITION_EXPIRE events = %d, want 1", n)
	}
}

func TestFireScheduled_LateAfterFailedAttempt(t *testing.T) {
	timeout := int64(1_000)
	for _, tc := range []struct {
		name    string
		lateMs  int64
		want    ScheduledOutcome
		wantFor spi.ScheduledTaskFailureReason
	}{
		{"within_retry_delay_runs", timeout + testRetryDelay.Milliseconds(), OutcomeFired, ""},
		{"beyond_retry_delay_failed", timeout + testRetryDelay.Milliseconds() + 1, OutcomeFailed, spi.FailureExpiredAfterFailedAttempts},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newRunEnv(t, nil)
			armed := env.setup(t, "late-e1", oneHopWF("CLOSED", nil, nil))
			armed.TimeoutMs = &timeout
			armTask(t, env.factory, env.ctx, armed)
			first := env.claimOne(t, testOwner, false)
			ref := spi.TaskRef{TenantID: testTenant, ID: first.ID, ArmToken: first.ArmToken, ClaimToken: first.Claim.Token}
			if err := env.sts.RecordAttempt(env.ctx, ref, spi.Attempt{Error: "boom", AtMs: env.nowMs(), NextAttemptTime: env.nowMs()}); err != nil {
				t.Fatalf("RecordAttempt: %v", err)
			}
			env.advance(tc.lateMs)
			second := env.claimOne(t, testOwner, false)

			r, _ := env.run(second)
			if r.Outcome != tc.want || r.FailReason != tc.wantFor {
				t.Fatalf("report = %+v, want %s %q", r, tc.want, tc.wantFor)
			}
		})
	}
}

func TestFireScheduled_SelfLoopReArmsSameIDAsNewLife(t *testing.T) {
	env := newRunEnv(t, nil)
	claimed := env.claimed(t, "loop-e1", oneHopWF("OPEN", nil, nil))

	r, _ := env.run(claimed)
	if r.Outcome != OutcomeFired || r.Err != nil {
		t.Fatalf("report = %+v, want fired", r)
	}
	got, found := env.task(t, claimed.ID)
	if !found {
		t.Fatal("self-loop must re-arm the same task id")
	}
	if got.ArmToken == claimed.ArmToken || got.Status != spi.ScheduledTaskWaiting || got.Claim != nil {
		t.Errorf("task = %+v, want a new WAITING life without a claim", got)
	}
	if got.ScheduledTime != env.nowMs()+60_000 {
		t.Errorf("ScheduledTime = %d, want %d", got.ScheduledTime, env.nowMs()+60_000)
	}
	if n := countAuditEvents(t, env.factory, env.ctx, "loop-e1", spi.SMEventScheduledTransitionCancelled); n != 0 {
		t.Errorf("SCHEDULED_TRANSITION_CANCEL events = %d, want 0", n)
	}
}

// TestFireScheduled_SelfLoopReArmsWithSameArmedBy covers §7.4: the WAITING
// life a self-loop re-arms (the SAME fire that consumed the claimed task)
// must carry the SAME ArmedBy as the task that fired. The fire seeds its
// transaction's origin from the claimed task's durable ArmedBy
// (fire_scheduled.go's WithAmbientOrigin, before Begin), and the fire's
// system-kind executor only inherits that origin because spi.AttributionFor
// inherits a transaction's origin for a service/system executor — exactly
// the branch arm.go's switch to AttributionFor (from spi.ResolveOrigin) now
// relies on at the re-arm site too. The ctx carries the system UserContext
// the real scheduler puts on it before firing (internal/scheduler/service.go's
// fire, common.SystemUserContextValue) — env.ctx's own test-user UserContext
// would otherwise mask the inheritance this test exists to prove. A
// regression that lost the ambient-origin seeding, or the inheritance, would
// silently re-arm with the zero Principal instead of carrying ArmedBy
// forward.
func TestFireScheduled_SelfLoopReArmsWithSameArmedBy(t *testing.T) {
	env := newRunEnv(t, nil)
	const entityID = "loop-armedby-e1"
	model := spi.ModelRef{EntityName: "run-" + entityID, ModelVersion: "1.0"}
	saveWorkflow(t, env.factory, env.ctx, model, []spi.WorkflowDefinition{oneHopWF("OPEN", nil, nil)})
	seedFireEntity(t, env.factory, env.ctx, entityID, model, "OPEN", "seed-tx-1", map[string]any{})
	now := env.nowMs()
	wantArmedBy := spi.Principal{ID: "alice", Kind: spi.PrincipalUser}
	armTask(t, env.factory, env.ctx, spi.ScheduledTask{
		ID: taskID(testTenant, entityID, "OPEN", "AutoClose"), TenantID: testTenant,
		Type: spi.ScheduledTaskFireTransition, ScheduledTime: now, EntityID: entityID,
		ModelName: model.EntityName, ModelVersion: 1, Transition: "AutoClose", SourceState: "OPEN",
		ArmedAt: now, ArmedBy: wantArmedBy,
	})
	claimed := env.claimOne(t, testOwner, false)

	fireCtx := spi.WithUserContext(env.ctx, common.SystemUserContextValue(testTenant))
	run := newTestRun(env.sts, claimed)
	r := run.fire(env.engine, fireCtx, claimed)
	if r.Outcome != OutcomeFired || r.Err != nil {
		t.Fatalf("report = %+v, want fired", r)
	}
	got, found := env.task(t, claimed.ID)
	if !found {
		t.Fatal("self-loop must re-arm the same task id")
	}
	if got.ArmedBy != wantArmedBy {
		t.Errorf("re-armed task ArmedBy = %+v, want %+v (the same as the fired task's ArmedBy)", got.ArmedBy, wantArmedBy)
	}
}

func TestFireScheduled_CriterionErrorIsASafeFailure(t *testing.T) {
	ext := &scriptedExtProc{criterion: func(context.Context) (bool, string, error) {
		return false, "", errors.New("criterion member failed")
	}}
	env := newRunEnv(t, ext)
	wf := oneHopWF("CLOSED", nil, nil)
	st := wf.States["OPEN"]
	st.Transitions[0].Criterion = functionCriterion()
	wf.States["OPEN"] = st
	claimed := env.claimed(t, "crit-e1", wf)

	r, _ := env.run(claimed)
	if r.Outcome != OutcomeFailed || r.Err == nil || r.FailReason != "" {
		t.Fatalf("report = %+v, want a safe failure with its error", r)
	}
	if got, _ := env.task(t, claimed.ID); got.Claim == nil || got.Claim.Token != claimed.Claim.Token {
		t.Errorf("task = %+v, want untouched under this claim", got)
	}
	if got := env.state(t, "crit-e1"); got != "OPEN" {
		t.Errorf("entity state = %q, want OPEN", got)
	}
}

func TestFireScheduled_ReArmedWhileClaimed_Superseded(t *testing.T) {
	env := newRunEnv(t, nil)
	claimed := env.claimed(t, "rearm-e1", oneHopWF("CLOSED", nil, nil))
	if err := env.rearm(claimed); err != nil {
		t.Fatalf("rearm: %v", err)
	}

	r, _ := env.run(claimed)
	if r.Outcome != OutcomeSuperseded || r.Err != nil {
		t.Fatalf("report = %+v, want superseded", r)
	}
	got, _ := env.task(t, claimed.ID)
	if got.ArmToken == claimed.ArmToken || got.Status != spi.ScheduledTaskWaiting {
		t.Errorf("task = %+v, want the new WAITING life", got)
	}
	if got := env.state(t, "rearm-e1"); got != "OPEN" {
		t.Errorf("entity state = %q, want OPEN", got)
	}
}

func TestFireScheduled_ABA_OldClaimAfterReArmAndNewClaim_Superseded(t *testing.T) {
	env := newRunEnv(t, nil)
	old := env.claimed(t, "aba-e1", oneHopWF("CLOSED", nil, nil))
	if err := env.rearm(old); err != nil {
		t.Fatalf("rearm: %v", err)
	}
	current := env.claimOne(t, otherOwner, false)

	r, _ := env.run(old)
	if r.Outcome != OutcomeSuperseded {
		t.Fatalf("old run outcome = %s, want superseded", r.Outcome)
	}
	got, _ := env.task(t, old.ID)
	if got.Claim == nil || got.Claim.Token != current.Claim.Token || got.ArmToken != current.ArmToken {
		t.Errorf("task = %+v, want the new claim of the new life untouched", got)
	}
}

func TestFireScheduled_ChangeCommittedDuringRun_Superseded(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(env *runEnv, claimed spi.ScheduledTask) error
		check  func(t *testing.T, got *spi.ScheduledTask, claimed spi.ScheduledTask)
	}{
		{"rearm", func(env *runEnv, claimed spi.ScheduledTask) error { return env.rearm(claimed) },
			func(t *testing.T, got *spi.ScheduledTask, claimed spi.ScheduledTask) {
				if got.ArmToken == claimed.ArmToken || got.Status != spi.ScheduledTaskWaiting {
					t.Errorf("task = %+v, want the new WAITING life", got)
				}
			}},
		{"reclaim", func(env *runEnv, _ spi.ScheduledTask) error {
			got, err := env.sts.ClaimDue(env.ctx, claimRequest(otherOwner, env.nowMs(), true))
			if err == nil && len(got) != 1 {
				err = fmt.Errorf("reclaim claimed %d tasks", len(got))
			}
			return err
		},
			func(t *testing.T, got *spi.ScheduledTask, _ spi.ScheduledTask) {
				if got.Claim == nil || got.Claim.Owner != otherOwner {
					t.Errorf("task = %+v, want RUNNING under the other owner", got)
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var env *runEnv
			var claimed spi.ScheduledTask
			ext := &scriptedExtProc{processor: func(context.Context, spi.ProcessorDefinition, string) (*spi.Entity, error) {
				return nil, tc.change(env, claimed)
			}}
			env = newRunEnv(t, ext)
			claimed = env.claimed(t, "c1-e1", oneHopWF("CLOSED", []spi.ProcessorDefinition{safeProc("p1", ExecutionModeSync)}, nil))

			r, _ := env.run(claimed)
			if r.Outcome != OutcomeSuperseded || r.Err != nil {
				t.Fatalf("report = %+v, want superseded (the commit conflicts on the task row)", r)
			}
			if got := env.state(t, "c1-e1"); got != "OPEN" {
				t.Errorf("entity state = %q, want OPEN", got)
			}
			got, _ := env.task(t, claimed.ID)
			tc.check(t, got, claimed)
		})
	}
}

func TestFireScheduled_GuardOfAnotherTenant_SeesNoTask(t *testing.T) {
	env := newRunEnv(t, nil)
	claimed := env.claimed(t, "tenant-e1", oneHopWF("CLOSED", nil, nil))
	forged := claimed
	forged.TenantID = testTenantB
	run := newTestRun(env.sts, forged)

	r := run.fire(env.engine, ctxWithTenant(testTenantB), forged)
	if r.Outcome != OutcomeSuperseded {
		t.Fatalf("outcome = %s, want superseded (another tenant's re-read finds nothing)", r.Outcome)
	}
	if got, _ := env.task(t, claimed.ID); got.Claim == nil || got.Claim.Token != claimed.Claim.Token {
		t.Errorf("tenant A's task = %+v, want untouched", got)
	}
	if got := env.state(t, "tenant-e1"); got != "OPEN" {
		t.Errorf("tenant A's entity state = %q, want OPEN", got)
	}
}

// cancelOnCommitTxMgr cancels the run inside Commit, before the commit reads
// its context: the moment a watchdog could fire while a commit is in flight.
type cancelOnCommitTxMgr struct {
	spi.TransactionManager
	cancel context.CancelFunc
}

func (m *cancelOnCommitTxMgr) Commit(ctx context.Context, txID string) error {
	if m.cancel != nil {
		m.cancel()
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("commit ran on a cancelled context: %w", err)
	}
	return m.TransactionManager.Commit(ctx, txID)
}

func TestFireScheduled_CommitInFlightNotCutByCancellation(t *testing.T) {
	var wrap *cancelOnCommitTxMgr
	env := newRunEnvWith(t, nil, nil, func(tm spi.TransactionManager) spi.TransactionManager {
		wrap = &cancelOnCommitTxMgr{TransactionManager: tm}
		return wrap
	})
	claimed := env.claimed(t, "shield-e1", oneHopWF("CLOSED", nil, nil))
	runCtx, cancel := context.WithCancel(env.ctx)
	defer cancel()
	wrap.cancel = cancel
	run := newTestRun(env.sts, claimed)
	run.guard.Done = runCtx.Done()

	r := run.fire(env.engine, runCtx, claimed)
	if r.Outcome != OutcomeFired || r.Err != nil {
		t.Fatalf("report = %+v, want fired: a commit in flight is shielded", r)
	}
	if got := env.state(t, "shield-e1"); got != "CLOSED" {
		t.Errorf("entity state = %q, want CLOSED", got)
	}
}

func TestScheduledRun_ReArmedDuringCBDDispatch_Superseded(t *testing.T) {
	var env *runEnv
	var claimed spi.ScheduledTask
	ext := &scriptedExtProc{processor: func(_ context.Context, proc spi.ProcessorDefinition, _ string) (*spi.Entity, error) {
		if proc.Name == "p1" {
			return nil, env.rearm(claimed)
		}
		return nil, nil
	}}
	env = newRunEnv(t, ext)
	claimed = env.claimed(t, "seg-e1", oneHopWF("CLOSED", []spi.ProcessorDefinition{
		safeProc("p1", ExecutionModeCommitBeforeDispatch),
		safeProc("p2", ExecutionModeSync),
	}, nil))

	r, _ := env.run(claimed)
	if r.Outcome != OutcomeSuperseded {
		t.Fatalf("report = %+v, want superseded at the re-read of TX_post", r)
	}
	if n := ext.count("p2"); n != 0 {
		t.Errorf("p2 dispatched %d times, want 0", n)
	}
	if got := env.state(t, "seg-e1"); got != "OPEN" {
		t.Errorf("entity state = %q, want OPEN (TX_pre only)", got)
	}
}

// beforeNthBeginTxMgr runs hook before the nth Begin of the engine's manager.
type beforeNthBeginTxMgr struct {
	spi.TransactionManager
	n     int
	hook  func() error
	count int
	err   error
}

func (m *beforeNthBeginTxMgr) Begin(ctx context.Context) (string, context.Context, error) {
	m.count++
	if m.count == m.n && m.hook != nil {
		m.err = m.hook()
	}
	return m.TransactionManager.Begin(ctx)
}

func TestScheduledRun_ReArmedBeforeTXPostBegin_StartNewTx_Superseded(t *testing.T) {
	var wrap *beforeNthBeginTxMgr
	env := newRunEnvWith(t, nil, nil, func(tm spi.TransactionManager) spi.TransactionManager {
		// The second Begin is TX_post's, after TX_pre committed.
		wrap = &beforeNthBeginTxMgr{TransactionManager: tm, n: 2}
		return wrap
	})
	startNewTx := true
	p1 := safeProc("p1", ExecutionModeCommitBeforeDispatch)
	p1.Config.StartNewTxOnDispatch = &startNewTx
	claimed := env.claimed(t, "seg-new-e1", oneHopWF("CLOSED", []spi.ProcessorDefinition{p1}, nil))
	wrap.hook = func() error { return env.rearm(claimed) }

	r, _ := env.run(claimed)
	if wrap.err != nil {
		t.Fatalf("rearm: %v", wrap.err)
	}
	if wrap.count < 2 {
		t.Fatalf("Begin called %d times, want a TX_post Begin", wrap.count)
	}
	if r.Outcome != OutcomeSuperseded {
		t.Fatalf("report = %+v, want superseded at the re-read of TX_post", r)
	}
	if got := env.state(t, "seg-new-e1"); got != "OPEN" {
		t.Errorf("entity state = %q, want OPEN (TX_pre only)", got)
	}
	got, found := env.task(t, claimed.ID)
	if !found || got.ArmToken == claimed.ArmToken || got.Status != spi.ScheduledTaskWaiting || got.Claim != nil {
		t.Errorf("task = %+v (found=%v), want the new WAITING life", got, found)
	}
}
