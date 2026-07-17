package workflow

import (
	"errors"
	"sync"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TestFireScheduled_DualCoordinatorConcurrency_ExactlyOnceFire is an
// ISOLATED single-backend (memory) concurrency test — never part of the
// e2e/parity suite, per .claude/rules/test-coverage.md ("Concurrency/race:
// isolated single-backend e2e, never the shared parity suite. Assert
// consistency ... not a precise interleave").
//
// It exercises the guard-token CAS idempotency (design §6.3, §5.3 "re-read
// guard") under a REAL goroutine race, complementing
// TestFireScheduled_GuardCASRace_DropsWithoutTornWrite (which is a
// deterministic hook-injection SIMULATION of the race window, not a true
// concurrent race).
//
// Setup: one due scheduled task is armed for one entity. Two goroutines —
// standing in for two cluster members' scheduler.Service coordinators that
// both believe they own this task (design §9's dispatch-window: a
// peer-authenticated fire racing a stale/overlapping membership view) —
// call engine.FireScheduledTransition on the SAME task.ID concurrently.
// This is the "simpler" construction the task called out explicitly:
// racing two goroutines directly against FireScheduledTransition, rather
// than standing up two full scheduler.Service instances with a
// self-electing coordinator strategy, since FireScheduledTransition is
// already "the scheduler's ONLY door into the engine" (its own doc
// comment) — the coordinator-election layer above it is a routing
// concern, not part of the idempotency mechanism under test here.
//
// Known return-contract quirk (not a data bug — see below): the losing
// goroutine can observe outcome==OutcomeFired alongside a non-nil error.
// FireScheduledTransition's final line is
// `return OutcomeFired, e.txMgr.Commit(ctx, finalTxID)` — the outcome
// label is chosen before Commit is invoked, so if the loser's Commit
// itself fails the SI+FCW conflict check (plugins/memory/txmanager.go
// Commit), the loser still returns OutcomeFired paired with
// spi.ErrConflict. The only production caller
// (internal/scheduler.LocalExecutor.Execute) checks err first and never
// inspects outcome on a non-nil error, so this never surfaces as a
// mislabeled fire in practice — and, decisively, the loser's staged
// writes (entity buffer, scheduled-task delete, audit event) are all
// discarded unapplied when Commit aborts on conflict (see Commit's abort
// path, which deletes the tx's buffered state before returning
// spi.ErrConflict), so no torn or double write ever reaches the store.
// This test therefore treats "genuinely fired" as outcome==OutcomeFired
// AND err==nil, and additionally asserts on the settled STORE STATE
// (entity state, task existence) as the ground truth that actually
// matters — not on the raw outcome strings alone. Audit-event COUNT is
// deliberately excluded from that ground truth; see the comment at the
// audit-count check below and design doc §8 "Accepted edge (E3)".
func TestFireScheduled_DualCoordinatorConcurrency_ExactlyOnceFire(t *testing.T) {
	const nowMs = int64(1_700_000_000_000)
	engine, factory := setupEngineWithClock(t, nowMs)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "dual-coord-order", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "DualCoordWF", InitialState: "OPEN", Active: true,
		States: map[string]spi.StateDefinition{
			"OPEN": {Transitions: []spi.TransitionDefinition{
				{Name: "AutoClose", Next: "CLOSED", Schedule: &spi.TransitionSchedule{DelayMs: 0}},
			}},
			"CLOSED": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})
	seedFireEntity(t, factory, ctx, "dual-coord-e1", modelRef, "OPEN", "seed-tx-1", map[string]any{})

	id := taskID(testTenant, "dual-coord-e1", "OPEN", "AutoClose")
	armTask(t, factory, ctx, spi.ScheduledTask{
		ID: id, TenantID: testTenant, Type: spi.ScheduledTaskFireTransition,
		ScheduledTime: nowMs, EntityID: "dual-coord-e1", ModelName: modelRef.EntityName,
		Transition: "AutoClose", SourceState: "OPEN", ArmedAt: nowMs,
	})

	// Two competing "coordinators" race FireScheduledTransition on the
	// identical task.ID, concurrently, against the same engine/store.
	const coordinators = 2
	var wg sync.WaitGroup
	outcomes := make([]ScheduledOutcome, coordinators)
	errs := make([]error, coordinators)
	for i := 0; i < coordinators; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			outcomes[i], errs[i] = engine.FireScheduledTransition(ctx, spi.ScheduledTask{ID: id})
		}(i)
	}
	wg.Wait()

	// --- Consistency assertions on the returned (outcome, err) pairs ---
	// A goroutine only "genuinely fired" if it reports OutcomeFired with a
	// nil error (see the return-contract quirk explained above). Every
	// other combination must be a safe, non-corrupting loss: either a
	// silent Dropped/Declined (nil error) or an error that is a CAS/FCW
	// conflict — never an unexplained failure.
	genuineFires := 0
	for i := range outcomes {
		switch {
		case errs[i] == nil && outcomes[i] == OutcomeFired:
			genuineFires++
		case errs[i] == nil:
			// A nil-error non-Fired outcome (Dropped/Declined/Expired) is a
			// legitimate, silent loss — nothing further to assert.
		case errors.Is(errs[i], spi.ErrConflict):
			// Lost the CAS or the SI+FCW commit-time conflict check — the
			// expected shape of the losing attempt.
		default:
			t.Errorf("goroutine %d: unexpected non-conflict error: outcome=%v err=%v", i, outcomes[i], errs[i])
		}
	}
	if genuineFires != 1 {
		t.Fatalf("expected exactly one genuine fire (outcome=Fired, err=nil) across both coordinators, got %d (outcomes=%v, errs=%v)",
			genuineFires, outcomes, errs)
	}

	// --- Consistency assertions on the settled store state (ground truth) ---

	// The entity advances to Next EXACTLY once — no torn/duplicated write.
	if got := getEntityState(t, factory, ctx, "dual-coord-e1"); got != "CLOSED" {
		t.Errorf("entity state = %q, want CLOSED", got)
	}

	// Audit-event COUNT is deliberately not asserted here (design doc §8
	// "Accepted edge (E3)"). The memory backend's StateMachineAuditStore
	// writes non-transactionally (outside the SI+FCW buffer that entity and
	// scheduled-task writes correctly participate in), so a losing
	// coordinator's SCHEDULED_TRANSITION_FIRE audit event can land durably
	// even though its entity/task writes are correctly discarded on the CAS
	// conflict — a duplicate `Fired` audit line under transient
	// dual-coordinator races. This is accepted as a rare cosmetic dup: the
	// entity STATE is always exactly-once correct (CAS guarantees it; see
	// the assertion above), only the audit trail can duplicate. Per
	// .claude/rules/test-coverage.md, concurrency tests assert consistency,
	// not a precise interleave — so this test's ground truth is settled
	// STORE STATE (entity state, task deletion), not audit-event count.
	// The count is logged for visibility only, never fails the test.
	if n := countAuditEvents(t, factory, ctx, "dual-coord-e1", spi.SMEventScheduledTransitionFired); n != 1 {
		t.Logf("informational: SCHEDULED_TRANSITION_FIRE events = %d (accepted cosmetic dup under transient dual-coordinator on memory backend; see design doc §8 Accepted edge (E3))", n)
	}

	// The task is resolved (deleted) exactly once — not left double-armed,
	// and not resurrected by a losing attempt's discarded buffer.
	if _, found := getTask(t, factory, ctx, id); found {
		t.Error("expected task to be deleted after the winning fire")
	}
}
