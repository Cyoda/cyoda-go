package workflow

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

func proofOf(cause error) error { return &contract.NoHandOffProof{Err: cause} }

func TestUnsafeReached_Fact(t *testing.T) {
	for _, tc := range []struct {
		name        string
		procs       []spi.ProcessorDefinition
		script      func(name string) (*spi.Entity, error)
		wantReached bool
	}{
		{"no_member_proof_resets", []spi.ProcessorDefinition{unsafeProc("p1", ExecutionModeSync)},
			func(string) (*spi.Entity, error) { return nil, proofOf(errors.New("no compute member for tag")) }, false},
		{"error_without_proof_stays", []spi.ProcessorDefinition{unsafeProc("p1", ExecutionModeSync)},
			func(string) (*spi.Entity, error) { return nil, errors.New("member failed") }, true},
		{"later_proof_does_not_reset", []spi.ProcessorDefinition{unsafeProc("p1", ExecutionModeSync), unsafeProc("p2", ExecutionModeSync)},
			func(name string) (*spi.Entity, error) {
				if name == "p1" {
					return nil, nil
				}
				return nil, proofOf(errors.New("no compute member for tag"))
			}, true},
		{"cancelled_before_send", []spi.ProcessorDefinition{unsafeProc("p1", ExecutionModeSync)},
			func(string) (*spi.Entity, error) { return nil, proofOf(context.Canceled) }, false},
		{"cancelled_after_send", []spi.ProcessorDefinition{unsafeProc("p1", ExecutionModeSync)},
			func(string) (*spi.Entity, error) { return nil, fmt.Errorf("callout: %w", context.Canceled) }, true},
		{"apply_fails_after_successful_dispatch", []spi.ProcessorDefinition{unsafeProc("p1", ExecutionModeSync)},
			func(string) (*spi.Entity, error) { return &spi.Entity{Data: []byte("{not json")}, nil }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ext := &scriptedExtProc{processor: func(_ context.Context, proc spi.ProcessorDefinition, _ string) (*spi.Entity, error) {
				return tc.script(proc.Name)
			}}
			env := newRunEnv(t, ext)
			claimed := env.claimed(t, "fact-"+tc.name, oneHopWF("CLOSED", tc.procs, nil))

			r, _ := env.run(claimed)
			if r.Outcome != OutcomeFailed || !r.MarkHeld || r.UnsafeReached != tc.wantReached {
				t.Fatalf("report = %+v, want failed, markHeld, unsafeReached=%v", r, tc.wantReached)
			}
		})
	}
}

// failingRollbackToSavepointTxMgr cannot undo a savepoint.
type failingRollbackToSavepointTxMgr struct{ spi.TransactionManager }

func (failingRollbackToSavepointTxMgr) RollbackToSavepoint(context.Context, string, string) error {
	return errors.New("savepoint gone")
}

func TestUnsafeReached_SavepointErrorReplacesProof(t *testing.T) {
	ext := &scriptedExtProc{processor: func(context.Context, spi.ProcessorDefinition, string) (*spi.Entity, error) {
		return nil, proofOf(errors.New("no compute member for tag"))
	}}
	env := newRunEnvWith(t, ext, nil, func(tm spi.TransactionManager) spi.TransactionManager {
		return failingRollbackToSavepointTxMgr{TransactionManager: tm}
	})
	claimed := env.claimed(t, "sp-e1", oneHopWF("CLOSED", []spi.ProcessorDefinition{unsafeProc("p1", ExecutionModeAsyncNewTx)}, nil))

	r, _ := env.run(claimed)
	if r.Outcome != OutcomeFailed || !errors.Is(r.Err, ErrSavepointInfra) || !r.UnsafeReached {
		t.Fatalf("report = %+v, want failed on the savepoint with unsafeReached (the proof was replaced)", r)
	}
}

func TestUnsafeReached_NoComputeNode_ThenFires(t *testing.T) {
	down := true
	ext := &scriptedExtProc{processor: func(context.Context, spi.ProcessorDefinition, string) (*spi.Entity, error) {
		if down {
			return nil, proofOf(errors.New("no compute member for tag"))
		}
		return nil, nil
	}}
	env := newRunEnv(t, ext)
	first := env.claimed(t, "retry-e1", oneHopWF("CLOSED", []spi.ProcessorDefinition{unsafeProc("p1", ExecutionModeSync)}, nil))

	r, _ := env.run(first)
	if r.Outcome != OutcomeFailed || !r.MarkHeld || r.UnsafeReached {
		t.Fatalf("first run = %+v, want a safe failure with the mark held and nothing reached", r)
	}
	// The scheduler's bookkeeping for that row (spec §5.6).
	ref := spi.TaskRef{TenantID: testTenant, ID: first.ID, ArmToken: first.ArmToken, ClaimToken: first.Claim.Token}
	if err := env.sts.RecordAttempt(env.ctx, ref, spi.Attempt{Error: "NO_COMPUTE_MEMBER_FOR_TAG", AtMs: env.nowMs(), NextAttemptTime: env.nowMs(), ClearOwnMark: true}); err != nil {
		t.Fatalf("RecordAttempt: %v", err)
	}
	down = false
	second := env.claimOne(t, testOwner, false)
	if second.UnsafeMarked || second.Attempts != 1 {
		t.Fatalf("second claim = %+v, want no mark and attempts 1", second)
	}

	r, _ = env.run(second)
	if r.Outcome != OutcomeFired {
		t.Fatalf("second run = %+v, want fired", r)
	}
	if got := env.state(t, "retry-e1"); got != "CLOSED" {
		t.Errorf("entity state = %q, want CLOSED", got)
	}
}

func TestUnsafeMark_MarkedTaskIsNeverRerun(t *testing.T) {
	ext := &scriptedExtProc{processor: func(context.Context, spi.ProcessorDefinition, string) (*spi.Entity, error) {
		return nil, errors.New("member failed")
	}}
	env := newRunEnv(t, ext)
	first := env.claimed(t, "never-e1", oneHopWF("CLOSED", []spi.ProcessorDefinition{unsafeProc("p1", ExecutionModeSync)}, nil))
	if r, _ := env.run(first); !r.UnsafeReached {
		t.Fatalf("first run = %+v, want unsafeReached", r)
	}
	// The owner dies before its bookkeeping: another pnode reclaims the task.
	second := env.claimOne(t, otherOwner, true)
	if !second.UnsafeMarked {
		t.Fatalf("reclaimed record = %+v, want UnsafeMarked", second)
	}

	r, _ := env.run(second)
	if r.Outcome != OutcomeFailed || r.FailReason != spi.FailureUnsafeWorkNotCompleted {
		t.Fatalf("second run = %+v, want FAILED UNSAFE_WORK_NOT_COMPLETED", r)
	}
	if n := ext.count("p1"); n != 1 {
		t.Errorf("p1 dispatched %d times, want 1", n)
	}
}

func TestUnsafeMark_NoNewUnsafe_CountsAsCut(t *testing.T) {
	ext := &scriptedExtProc{}
	env := newRunEnv(t, ext)
	claimed := env.claimed(t, "drain-e1", oneHopWF("CLOSED", []spi.ProcessorDefinition{
		safeProc("p0", ExecutionModeSync), unsafeProc("p1", ExecutionModeSync),
	}, nil))
	store := &scriptedMarkStore{ScheduledTaskStore: env.sts}
	run := newTestRun(store, claimed)
	signalled := make(chan struct{})
	close(signalled)
	run.guard.NoNewUnsafe = signalled

	r := run.fire(env.engine, env.ctx, claimed)
	assertCancelled(t, r)
	if ext.count("p0") != 1 || ext.count("p1") != 0 || store.count() != 0 {
		t.Errorf("dispatches p0=%d p1=%d marks=%d, want 1, 0, 0", ext.count("p0"), ext.count("p1"), store.count())
	}
	if r.UnsafeReached {
		t.Error("nothing unsafe was sent")
	}
}

func TestUnsafeReached_InFlightIsVisible(t *testing.T) {
	var duringUnsafe, duringSafe bool
	ext := &scriptedExtProc{processor: func(ctx context.Context, proc spi.ProcessorDefinition, _ string) (*spi.Entity, error) {
		switch proc.Name {
		case "p1":
			duringUnsafe = RunGuardFrom(ctx).UnsafeInFlight()
		case "p2":
			duringSafe = RunGuardFrom(ctx).UnsafeInFlight()
		}
		return nil, nil
	}}
	env := newRunEnv(t, ext)
	claimed := env.claimed(t, "inflight-e1", oneHopWF("CLOSED", []spi.ProcessorDefinition{
		unsafeProc("p1", ExecutionModeSync), safeProc("p2", ExecutionModeSync),
	}, nil))

	r, run := env.run(claimed)
	if r.Outcome != OutcomeFired {
		t.Fatalf("report = %+v, want fired", r)
	}
	if !duringUnsafe || duringSafe || run.guard.UnsafeInFlight() {
		t.Errorf("in flight: during unsafe=%v, during safe=%v, after=%v; want true, false, false",
			duringUnsafe, duringSafe, run.guard.UnsafeInFlight())
	}
}

// Every dispatch site reports the step's error to dispatched: the proof
// resets the fact, a success or an error without the proof leaves it set, and
// the in-flight count covers the dispatch and falls to zero after it. A site
// that stops reporting fails the proof row and the "after" check.
func TestUnsafeReached_EverySite(t *testing.T) {
	for _, site := range unsafeAtEverySite() {
		for _, tc := range []struct {
			name        string
			err         error
			wantReached bool
		}{
			{"success", nil, true},
			{"error_without_proof", errors.New("member failed"), true},
			{"proof", proofOf(errors.New("no compute member for tag")), false},
		} {
			t.Run(site.name+"/"+tc.name, func(t *testing.T) {
				var during bool
				ext := &scriptedExtProc{processor: func(ctx context.Context, _ spi.ProcessorDefinition, _ string) (*spi.Entity, error) {
					during = RunGuardFrom(ctx).UnsafeInFlight()
					return nil, tc.err
				}}
				env := newRunEnv(t, ext)
				claimed := env.claimed(t, "every-"+site.name+"-"+tc.name, oneHopWF("CLOSED", []spi.ProcessorDefinition{site.proc}, nil))

				r, run := env.run(claimed)
				if !r.MarkHeld || r.UnsafeReached != tc.wantReached {
					t.Fatalf("report = %+v, want markHeld and unsafeReached=%v", r, tc.wantReached)
				}
				if !during || run.guard.UnsafeInFlight() {
					t.Errorf("in flight: during=%v, after=%v; want true, false", during, run.guard.UnsafeInFlight())
				}
			})
		}
	}
}

// A dispatch that panics reaches dispatched with a nil error, the value a
// success gives. It is not a proof: the fact stays set, and the in-flight
// count still falls. The engine does not recover the panic; the scheduler's
// run goroutine does, and the recover below stands in for it.
func TestUnsafeReached_EverySite_PanicLeavesFactSet(t *testing.T) {
	for _, site := range unsafeAtEverySite() {
		t.Run(site.name, func(t *testing.T) {
			ext := &scriptedExtProc{processor: func(context.Context, spi.ProcessorDefinition, string) (*spi.Entity, error) {
				panic("processor dispatch panicked")
			}}
			env := newRunEnv(t, ext)
			claimed := env.claimed(t, "panic-"+site.name, oneHopWF("CLOSED", []spi.ProcessorDefinition{site.proc}, nil))
			run := newTestRun(env.sts, claimed)

			if recovered := firePanics(env, run, claimed); recovered == nil {
				t.Fatal("the dispatch panic did not propagate")
			}
			if !run.guard.markHeld || !run.guard.unsafeReached || run.guard.UnsafeInFlight() {
				t.Errorf("after the panic: markHeld=%v unsafeReached=%v inFlight=%v; want true, true, false",
					run.guard.markHeld, run.guard.unsafeReached, run.guard.UnsafeInFlight())
			}
		})
	}
}

// firePanics runs the fire and returns what a recover of it returns.
func firePanics(env *runEnv, run *testRun, task spi.ScheduledTask) (recovered any) {
	defer func() { recovered = recover() }()
	run.fire(env.engine, env.ctx, task)
	return nil
}

// panickingMarkStore panics inside MarkUnsafe, after recording whether the
// run counted the dispatch as in flight.
type panickingMarkStore struct {
	spi.ScheduledTaskStore
	inFlight bool
}

func (s *panickingMarkStore) MarkUnsafe(ctx context.Context, _ spi.TaskRef) error {
	s.inFlight = RunGuardFrom(ctx).UnsafeInFlight()
	panic("MarkUnsafe panicked")
}

// The dispatch counts as in flight from before its mark, and a panic in the
// mark does not leave the count raised.
func TestUnsafeInFlight_EverySite_CountsFromBeforeTheMark_PanicInMark(t *testing.T) {
	for _, site := range unsafeAtEverySite() {
		t.Run(site.name, func(t *testing.T) {
			ext := &scriptedExtProc{}
			env := newRunEnv(t, ext)
			claimed := env.claimed(t, "markpanic-"+site.name, oneHopWF("CLOSED", []spi.ProcessorDefinition{site.proc}, nil))
			store := &panickingMarkStore{ScheduledTaskStore: env.sts}
			run := newTestRun(store, claimed)

			if recovered := firePanics(env, run, claimed); recovered == nil {
				t.Fatal("the MarkUnsafe panic did not propagate")
			}
			if !store.inFlight {
				t.Error("the dispatch was not in flight during its mark")
			}
			if run.guard.UnsafeInFlight() || run.guard.unsafeReached {
				t.Errorf("after the panic: inFlight=%v unsafeReached=%v; want false, false",
					run.guard.UnsafeInFlight(), run.guard.unsafeReached)
			}
			if n := ext.count("p1"); n != 0 {
				t.Errorf("p1 dispatched %d times, want 0", n)
			}
		})
	}
}

// After the signal no site starts unsafe work: the run is cut, nothing is
// marked or sent, and the count it raised falls again.
func TestUnsafeMark_EverySite_NoNewUnsafe_CountsAsCut(t *testing.T) {
	for _, site := range unsafeAtEverySite() {
		t.Run(site.name, func(t *testing.T) {
			ext := &scriptedExtProc{}
			env := newRunEnv(t, ext)
			claimed := env.claimed(t, "drain-"+site.name, oneHopWF("CLOSED", []spi.ProcessorDefinition{site.proc}, nil))
			store := &scriptedMarkStore{ScheduledTaskStore: env.sts}
			run := newTestRun(store, claimed)
			signalled := make(chan struct{})
			close(signalled)
			run.guard.NoNewUnsafe = signalled

			r := run.fire(env.engine, env.ctx, claimed)
			assertCancelled(t, r)
			if r.UnsafeReached || r.MarkHeld || run.guard.UnsafeInFlight() {
				t.Errorf("report = %+v, inFlight=%v; want nothing reached, no mark, nothing in flight", r, run.guard.UnsafeInFlight())
			}
			if ext.count("p1") != 0 || store.count() != 0 {
				t.Errorf("dispatches=%d marks=%d, want 0, 0", ext.count("p1"), store.count())
			}
		})
	}
}

// A refused mark sends nothing, so it leaves nothing in flight at any site.
func TestUnsafeInFlight_EverySite_RefusedMarkLeavesNothingInFlight(t *testing.T) {
	for _, site := range unsafeAtEverySite() {
		t.Run(site.name, func(t *testing.T) {
			env := newRunEnv(t, &scriptedExtProc{})
			claimed := env.claimed(t, "refinf-"+site.name, oneHopWF("CLOSED", []spi.ProcessorDefinition{site.proc}, nil))
			store := &scriptedMarkStore{ScheduledTaskStore: env.sts, markErr: errors.New("connection refused")}
			run := newTestRun(store, claimed)

			r := run.fire(env.engine, env.ctx, claimed)
			if r.Outcome != OutcomeFailed || r.UnsafeReached || run.guard.UnsafeInFlight() {
				t.Errorf("report = %+v, inFlight=%v; want failed, nothing reached, nothing in flight", r, run.guard.UnsafeInFlight())
			}
		})
	}
}

// A nil record, as a guard without one has, records nothing and never
// reports a dispatch in flight.
func TestUnsafeFlight_NilRecordsNothing(t *testing.T) {
	var f *UnsafeFlight
	f.Begin()
	if _, ok := f.Since(); ok {
		t.Error("a nil record reported a dispatch in flight")
	}
	f.End()
}

func TestUnsafeFlight_SinceIsTheOldestStart(t *testing.T) {
	f := &UnsafeFlight{}
	if _, ok := f.Since(); ok {
		t.Fatal("an empty record reported a dispatch in flight")
	}
	f.Begin()
	first, ok := f.Since()
	if !ok {
		t.Fatal("Begin did not count")
	}
	time.Sleep(2 * time.Millisecond)
	f.Begin()
	if second, _ := f.Since(); !second.Equal(first) {
		t.Errorf("Since = %v after a second Begin, want the oldest start %v", second, first)
	}
	f.End()
	if _, ok := f.Since(); !ok {
		t.Error("one End of two Begins cleared the record")
	}
	f.End()
	if _, ok := f.Since(); ok {
		t.Error("two Ends of two Begins left a dispatch in flight")
	}
	f.End()
	f.Begin()
	if _, ok := f.Since(); !ok {
		t.Error("an extra End drove the count below zero")
	}
}

func TestUnsafeFlight_ConcurrentBeginEnd(t *testing.T) {
	f := &UnsafeFlight{}
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				f.Begin()
				f.Since()
				f.End()
			}
		}()
	}
	wg.Wait()
	if _, ok := f.Since(); ok {
		t.Error("balanced Begins and Ends left a dispatch in flight")
	}
}
