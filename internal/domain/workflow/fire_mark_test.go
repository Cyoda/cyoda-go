package workflow

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// scriptedMarkStore counts MarkUnsafe calls and, when markErr is set,
// answers with it instead of the real store.
type scriptedMarkStore struct {
	spi.ScheduledTaskStore
	mu      sync.Mutex
	marks   int
	markErr error
}

func (s *scriptedMarkStore) MarkUnsafe(ctx context.Context, ref spi.TaskRef) error {
	func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.marks++
	}()
	if s.markErr != nil {
		return s.markErr
	}
	return s.ScheduledTaskStore.MarkUnsafe(ctx, ref)
}

func (s *scriptedMarkStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.marks
}

func TestUnsafeMark_Results(t *testing.T) {
	for _, tc := range []struct {
		name            string
		markErr         error
		want            ScheduledOutcome
		wantReason      spi.ScheduledTaskFailureReason
		wantMarkErrored bool
	}{
		{"stale_claim", fmt.Errorf("fenced: %w", spi.ErrStaleClaim), OutcomeSuperseded, "", false},
		{"marked_by_another_claim", fmt.Errorf("fenced: %w", spi.ErrMarkedByAnotherClaim), OutcomeFailed, spi.FailureUnsafeWorkNotCompleted, false},
		{"task_busy", fmt.Errorf("locked: %w", spi.ErrTaskBusy), OutcomeFailed, "", false},
		{"other_error", errors.New("connection refused"), OutcomeFailed, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ext := &scriptedExtProc{}
			env := newRunEnv(t, ext)
			claimed := env.claimed(t, "mark-"+tc.name, oneHopWF("CLOSED",
				[]spi.ProcessorDefinition{unsafeProc("p1", ExecutionModeSync)}, nil))
			store := &scriptedMarkStore{ScheduledTaskStore: env.sts, markErr: tc.markErr}

			r := newTestRun(store, claimed).fire(env.engine, env.ctx, claimed)
			if r.Outcome != tc.want || r.FailReason != tc.wantReason || r.MarkErrored != tc.wantMarkErrored || r.MarkHeld {
				t.Fatalf("report = %+v, want %s reason=%q markErrored=%v markHeld=false", r, tc.want, tc.wantReason, tc.wantMarkErrored)
			}
			if tc.want == OutcomeFailed && r.Err == nil {
				t.Error("a failed run must carry its error")
			}
			if n := ext.count("p1"); n != 0 {
				t.Errorf("p1 dispatched %d times, want 0", n)
			}
			if got := env.state(t, "mark-"+tc.name); got != "OPEN" {
				t.Errorf("entity state = %q, want OPEN", got)
			}
		})
	}
}

func TestUnsafeMark_BeforeEveryUnsafeDispatch(t *testing.T) {
	ext := &scriptedExtProc{}
	env := newRunEnv(t, ext)
	claimed := env.claimed(t, "every-e1", oneHopWF("CLOSED", []spi.ProcessorDefinition{
		unsafeProc("p1", ExecutionModeSync), safeProc("p2", ExecutionModeSync), unsafeProc("p3", ExecutionModeSync),
	}, nil))
	store := &scriptedMarkStore{ScheduledTaskStore: env.sts}

	r := newTestRun(store, claimed).fire(env.engine, env.ctx, claimed)
	if r.Outcome != OutcomeFired || !r.MarkHeld {
		t.Fatalf("report = %+v, want fired with the mark held", r)
	}
	if n := store.count(); n != 2 {
		t.Errorf("MarkUnsafe calls = %d, want 2 (one per unsafe dispatch, none for the idempotent one)", n)
	}
}

func TestUnsafeMark_IdempotentProcessorIsNotMarked(t *testing.T) {
	ext := &scriptedExtProc{processor: func(context.Context, spi.ProcessorDefinition, string) (*spi.Entity, error) {
		return nil, errors.New("member failed")
	}}
	env := newRunEnv(t, ext)
	claimed := env.claimed(t, "idem-e1", oneHopWF("CLOSED", []spi.ProcessorDefinition{safeProc("p1", ExecutionModeSync)}, nil))
	store := &scriptedMarkStore{ScheduledTaskStore: env.sts}

	r := newTestRun(store, claimed).fire(env.engine, env.ctx, claimed)
	if r.Outcome != OutcomeFailed || r.MarkHeld || r.UnsafeReached || r.FailReason != "" {
		t.Fatalf("report = %+v, want a plain safe failure", r)
	}
	if n := store.count(); n != 0 {
		t.Errorf("MarkUnsafe calls = %d, want 0", n)
	}
}

func TestUnsafeMark_SupersededOwnerSendsNoUnsafeProcessor(t *testing.T) {
	var env *runEnv
	var claimed spi.ScheduledTask
	ext := &scriptedExtProc{processor: func(_ context.Context, proc spi.ProcessorDefinition, _ string) (*spi.Entity, error) {
		if proc.Name == "p1" {
			return nil, env.rearm(claimed)
		}
		return nil, nil
	}}
	env = newRunEnv(t, ext)
	claimed = env.claimed(t, "sup-e1", oneHopWF("CLOSED", []spi.ProcessorDefinition{
		safeProc("p1", ExecutionModeSync), unsafeProc("p2", ExecutionModeSync),
	}, nil))

	r, _ := env.run(claimed)
	if r.Outcome != OutcomeSuperseded {
		t.Fatalf("report = %+v, want superseded (the old life's mark is refused)", r)
	}
	if n := ext.count("p2"); n != 0 {
		t.Errorf("p2 dispatched %d times, want 0", n)
	}
}

func TestUnsafeMark_AsyncNewTxFailure_RunCommits(t *testing.T) {
	ext := &scriptedExtProc{processor: func(context.Context, spi.ProcessorDefinition, string) (*spi.Entity, error) {
		return nil, errors.New("side effect failed")
	}}
	env := newRunEnv(t, ext)
	claimed := env.claimed(t, "async-e1", oneHopWF("CLOSED", []spi.ProcessorDefinition{unsafeProc("p1", ExecutionModeAsyncNewTx)}, nil))

	r, _ := env.run(claimed)
	if r.Outcome != OutcomeFired || !r.MarkHeld {
		t.Fatalf("report = %+v, want fired with the mark held", r)
	}
	if _, found := env.task(t, claimed.ID); found {
		t.Error("the committed run completes the task")
	}
}

func TestCallbackAntiPattern_WritesFiredEntity_ThenUnsafe_TaskBusy(t *testing.T) {
	var env *runEnv
	var claimed spi.ScheduledTask
	ext := &scriptedExtProc{processor: func(_ context.Context, proc spi.ProcessorDefinition, txID string) (*spi.Entity, error) {
		if proc.Name != "p1" {
			return nil, nil
		}
		// A joined callback updates the fired entity: the update re-arms its task in the run's transaction.
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
	claimed = env.claimed(t, "busy-e1", oneHopWF("CLOSED", []spi.ProcessorDefinition{
		safeProc("p1", ExecutionModeSync), unsafeProc("p2", ExecutionModeSync),
	}, nil))

	done := make(chan RunReport, 1)
	go func() { r, _ := env.run(claimed); done <- r }()
	var r RunReport
	select {
	case r = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the run hung")
	}
	if r.Outcome != OutcomeFailed || !errors.Is(r.Err, spi.ErrTaskBusy) || r.MarkHeld || r.FailReason != "" {
		t.Fatalf("report = %+v, want a safe failure on ErrTaskBusy", r)
	}
	if n := ext.count("p2"); n != 0 {
		t.Errorf("p2 dispatched %d times, want 0", n)
	}
}

// unsafeAtEverySite has one row per processor dispatch site: SYNC,
// ASYNC_NEW_TX, and COMMIT_BEFORE_DISPATCH with and without a new transaction.
func unsafeAtEverySite() []struct {
	name string
	proc spi.ProcessorDefinition
} {
	startNewTx := true
	cbdNewTx := unsafeProc("p1", ExecutionModeCommitBeforeDispatch)
	cbdNewTx.Config.StartNewTxOnDispatch = &startNewTx
	return []struct {
		name string
		proc spi.ProcessorDefinition
	}{
		{"sync", unsafeProc("p1", ExecutionModeSync)},
		{"async_new_tx", unsafeProc("p1", ExecutionModeAsyncNewTx)},
		{"cbd", unsafeProc("p1", ExecutionModeCommitBeforeDispatch)},
		{"cbd_start_new_tx", cbdNewTx},
	}
}

func TestUnsafeMark_EverySite_MarksBeforeDispatch(t *testing.T) {
	for _, site := range unsafeAtEverySite() {
		t.Run(site.name, func(t *testing.T) {
			var store *scriptedMarkStore
			marksAtDispatch := -1
			ext := &scriptedExtProc{processor: func(context.Context, spi.ProcessorDefinition, string) (*spi.Entity, error) {
				marksAtDispatch = store.count()
				return nil, nil
			}}
			env := newRunEnv(t, ext)
			claimed := env.claimed(t, "site-"+site.name, oneHopWF("CLOSED", []spi.ProcessorDefinition{site.proc}, nil))
			store = &scriptedMarkStore{ScheduledTaskStore: env.sts}

			r := newTestRun(store, claimed).fire(env.engine, env.ctx, claimed)
			if r.Outcome != OutcomeFired || !r.MarkHeld {
				t.Fatalf("report = %+v, want fired with the mark held", r)
			}
			if marksAtDispatch != 1 {
				t.Errorf("MarkUnsafe calls when p1 was dispatched = %d, want 1", marksAtDispatch)
			}
		})
	}
}

func TestUnsafeMark_EverySite_RefusedMarkDispatchesNothing(t *testing.T) {
	for _, site := range unsafeAtEverySite() {
		t.Run(site.name, func(t *testing.T) {
			ext := &scriptedExtProc{}
			env := newRunEnv(t, ext)
			claimed := env.claimed(t, "refused-"+site.name, oneHopWF("CLOSED", []spi.ProcessorDefinition{site.proc}, nil))
			store := &scriptedMarkStore{ScheduledTaskStore: env.sts, markErr: fmt.Errorf("fenced: %w", spi.ErrMarkedByAnotherClaim)}

			// ASYNC_NEW_TX included: a refused mark is fatal in every mode.
			r := newTestRun(store, claimed).fire(env.engine, env.ctx, claimed)
			if r.Outcome != OutcomeFailed || r.FailReason != spi.FailureUnsafeWorkNotCompleted || r.MarkHeld {
				t.Fatalf("report = %+v, want failed %s without a mark", r, spi.FailureUnsafeWorkNotCompleted)
			}
			if n := ext.count("p1"); n != 0 {
				t.Errorf("p1 dispatched %d times, want 0", n)
			}
		})
	}
}

// A run cancelled after the TX_pre commit of a COMMIT_BEFORE_DISPATCH
// processor writes no mark: the run is cut before any unsafe work, so the
// next claim may run it again.
func TestUnsafeMark_CancelledBeforeMark_NoMarkNoDispatch(t *testing.T) {
	var wrap *cancelOnCommitTxMgr
	ext := &scriptedExtProc{}
	env := newRunEnvWith(t, ext, nil, func(tm spi.TransactionManager) spi.TransactionManager {
		wrap = &cancelOnCommitTxMgr{TransactionManager: tm}
		return wrap
	})
	claimed := env.claimed(t, "cut-e1", oneHopWF("CLOSED",
		[]spi.ProcessorDefinition{unsafeProc("p1", ExecutionModeCommitBeforeDispatch)}, nil))
	store := &scriptedMarkStore{ScheduledTaskStore: env.sts}
	run := newTestRun(store, claimed)
	wrap.cancel = run.cancel

	assertCancelled(t, run.fire(env.engine, env.ctx, claimed))
	if n := store.count(); n != 0 {
		t.Errorf("MarkUnsafe calls = %d, want 0", n)
	}
	if n := ext.count("p1"); n != 0 {
		t.Errorf("p1 dispatched %d times, want 0", n)
	}
}
