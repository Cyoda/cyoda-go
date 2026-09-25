package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// sawCancel reports whether ctx is cancelled within two seconds.
func sawCancel(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	case <-time.After(2 * time.Second):
		return false
	}
}

func assertCancelled(t *testing.T, r RunReport) {
	t.Helper()
	if r.Outcome != OutcomeFailed || !errors.Is(r.Err, context.Canceled) {
		t.Fatalf("report = %+v, want failed with a cancellation", r)
	}
}

func TestRunCancel_BeforeProcessorDispatch(t *testing.T) {
	var run *testRun
	ext := &scriptedExtProc{processor: func(_ context.Context, proc spi.ProcessorDefinition, _ string) (*spi.Entity, error) {
		if proc.Name == "p1" {
			run.cancel()
		}
		return nil, nil
	}}
	env := newRunEnv(t, ext)
	claimed := env.claimed(t, "cp-e1", oneHopWF("CLOSED", []spi.ProcessorDefinition{
		safeProc("p1", ExecutionModeSync), safeProc("p2", ExecutionModeSync),
	}, nil))
	run = newTestRun(env.sts, claimed)

	assertCancelled(t, run.fire(env.engine, env.ctx, claimed))
	if n := ext.count("p2"); n != 0 {
		t.Errorf("p2 dispatched %d times after the cancellation, want 0", n)
	}
	if got := env.state(t, "cp-e1"); got != "OPEN" {
		t.Errorf("entity state = %q, want OPEN", got)
	}
}

func TestRunCancel_AfterTXPre_StopsAtNextStep(t *testing.T) {
	var run *testRun
	ext := &scriptedExtProc{processor: func(_ context.Context, proc spi.ProcessorDefinition, _ string) (*spi.Entity, error) {
		if proc.Name == "p1" {
			run.cancel()
		}
		return nil, nil
	}}
	env := newRunEnv(t, ext)
	claimed := env.claimed(t, "txpre-e1", oneHopWF("MID",
		[]spi.ProcessorDefinition{safeProc("p1", ExecutionModeCommitBeforeDispatch)},
		map[string]spi.StateDefinition{"MID": autoStep("DONE", safeProc("p2", ExecutionModeSync))}))
	run = newTestRun(env.sts, claimed)

	r := run.fire(env.engine, env.ctx, claimed)
	assertCancelled(t, r)
	if n := ext.count("p2"); n != 0 {
		t.Errorf("p2 dispatched %d times, want 0 (the cascade step checks the guard)", n)
	}
	if got := env.state(t, "txpre-e1"); got != "OPEN" {
		t.Errorf("entity state = %q, want OPEN (TX_pre only)", got)
	}
	if r.UnsafeReached {
		t.Error("UnsafeReached must be false: only idempotent processors ran")
	}
}

func TestRunCancel_BeforeFinalCommit_NoCommit(t *testing.T) {
	var run *testRun
	ext := &scriptedExtProc{function: func(context.Context) (contract.FunctionResult, error) {
		run.cancel()
		return contract.FunctionResult{Kind: "Schedule", Value: json.RawMessage(`{"fireAfterMs":60000}`)}, nil
	}}
	env := newRunEnv(t, ext)
	claimed := env.claimed(t, "final-e1", oneHopWF("DONE", nil, map[string]spi.StateDefinition{
		"DONE": {Transitions: []spi.TransitionDefinition{{Name: "Tick", Next: "OPEN",
			Schedule: &spi.TransitionSchedule{Function: &spi.ScheduleFunction{Name: "tick", ResultKind: "Schedule", CalculationNodesTags: "sched"}}}}},
	}))
	run = newTestRun(env.sts, claimed)

	assertCancelled(t, run.fire(env.engine, env.ctx, claimed))
	if got := env.state(t, "final-e1"); got != "OPEN" {
		t.Errorf("entity state = %q, want OPEN (no commit after the cancellation)", got)
	}
	if got, _ := env.task(t, claimed.ID); got == nil || got.Claim == nil || got.Claim.Token != claimed.Claim.Token {
		t.Errorf("task = %+v, want untouched under this claim", got)
	}
}

// saveHookFactory calls onSave before every EntityStore.Save.
type saveHookFactory struct {
	spi.StoreFactory
	onSave *func()
}

type saveHookEntityStore struct {
	spi.EntityStore
	onSave *func()
}

func (f saveHookFactory) EntityStore(ctx context.Context) (spi.EntityStore, error) {
	es, err := f.StoreFactory.EntityStore(ctx)
	if err != nil {
		return nil, err
	}
	return saveHookEntityStore{EntityStore: es, onSave: f.onSave}, nil
}

func (s saveHookEntityStore) Save(ctx context.Context, e *spi.Entity) (int64, error) {
	if *s.onSave != nil {
		(*s.onSave)()
	}
	return s.EntityStore.Save(ctx, e)
}

func TestRunCancel_BeforeSegmentCommit_NoCommit(t *testing.T) {
	var onSave func()
	ext := &scriptedExtProc{}
	env := newRunEnvWith(t, ext, func(f spi.StoreFactory) spi.StoreFactory {
		return saveHookFactory{StoreFactory: f, onSave: &onSave}
	}, nil)
	// p1's flush is a CompareAndSave (the first segment applies the
	// precondition); p2's flush is the first Save.
	claimed := env.claimed(t, "segc-e1", oneHopWF("MID",
		[]spi.ProcessorDefinition{safeProc("p1", ExecutionModeCommitBeforeDispatch)},
		map[string]spi.StateDefinition{"MID": autoStep("DONE", safeProc("p2", ExecutionModeCommitBeforeDispatch))}))
	run := newTestRun(env.sts, claimed)
	onSave = run.cancel

	assertCancelled(t, run.fire(env.engine, env.ctx, claimed))
	if n := ext.count("p2"); n != 0 {
		t.Errorf("p2 dispatched %d times, want 0 (its segment must not commit)", n)
	}
	if got := env.state(t, "segc-e1"); got != "OPEN" {
		t.Errorf("entity state = %q, want OPEN", got)
	}
}

func TestRunCancel_CalloutsAfterCBDSeeTheCancellation(t *testing.T) {
	type probe struct{ saw bool }
	for _, tc := range []struct {
		name  string
		wire  func(ext *scriptedExtProc, run **testRun, p *probe)
		after spi.StateDefinition
	}{
		{"processor", func(ext *scriptedExtProc, run **testRun, p *probe) {
			ext.processor = func(ctx context.Context, proc spi.ProcessorDefinition, _ string) (*spi.Entity, error) {
				if proc.Name != "p2" {
					return nil, nil
				}
				(*run).cancel()
				p.saw = sawCancel(ctx)
				return nil, ctx.Err()
			}
		}, autoStep("DONE", safeProc("p2", ExecutionModeSync))},
		{"criterion", func(ext *scriptedExtProc, run **testRun, p *probe) {
			ext.criterion = func(ctx context.Context) (bool, string, error) {
				(*run).cancel()
				p.saw = sawCancel(ctx)
				return false, "", ctx.Err()
			}
		}, spi.StateDefinition{Transitions: []spi.TransitionDefinition{{Name: "Step", Next: "DONE", Criterion: functionCriterion()}}}},
		{"schedule_function", func(ext *scriptedExtProc, run **testRun, p *probe) {
			ext.function = func(ctx context.Context) (contract.FunctionResult, error) {
				(*run).cancel()
				p.saw = sawCancel(ctx)
				return contract.FunctionResult{}, ctx.Err()
			}
		}, spi.StateDefinition{Transitions: []spi.TransitionDefinition{{Name: "Tick", Next: "OPEN",
			Schedule: &spi.TransitionSchedule{Function: &spi.ScheduleFunction{Name: "tick", ResultKind: "Schedule", CalculationNodesTags: "sched"}}}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var run *testRun
			p := &probe{}
			ext := &scriptedExtProc{}
			tc.wire(ext, &run, p)
			env := newRunEnv(t, ext)
			claimed := env.claimed(t, "call-"+tc.name, oneHopWF("MID",
				[]spi.ProcessorDefinition{safeProc("p1", ExecutionModeCommitBeforeDispatch)},
				map[string]spi.StateDefinition{"MID": tc.after}))
			run = newTestRun(env.sts, claimed)

			assertCancelled(t, run.fire(env.engine, env.ctx, claimed))
			if !p.saw {
				t.Error("a callout after a COMMIT_BEFORE_DISPATCH commit did not see the run's cancellation")
			}
		})
	}
}
