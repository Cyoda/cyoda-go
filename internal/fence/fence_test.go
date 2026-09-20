package fence

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
)

func newFence() *Fence { return New(txgate.New()) }

// begin registers a callout and ends it when the test finishes.
func begin(t *testing.T, f *Fence, ctx context.Context, calloutID, txID string, outer []Pair) (context.Context, func()) {
	t.Helper()
	cctx, end := f.Begin(ctx, calloutID, txID, outer)
	t.Cleanup(end)
	return cctx, end
}

func assertRefused(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a refusal, got nil")
	}
	if !errors.Is(err, ErrSuperseded) {
		t.Fatalf("refusal must carry ErrSuperseded as its cause, got %v", err)
	}
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("refusal must be an *common.AppError, got %T", err)
	}
	if appErr.Status != http.StatusGone || appErr.Code != common.ErrCodeCalloutSuperseded || appErr.Retryable {
		t.Fatalf("refusal = %d %s retryable=%v; want 410 CALLOUT_SUPERSEDED not retryable", appErr.Status, appErr.Code, appErr.Retryable)
	}
	const want = "CALLOUT_SUPERSEDED: this compute node was replaced, or its callout has ended"
	if appErr.Message != want {
		t.Fatalf("message = %q; want %q", appErr.Message, want)
	}
}

func TestAdmit_Currentness(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(f *Fence, t *testing.T)
		pairs   []Pair
		refused bool
	}{
		{
			name:    "unknown callout",
			prepare: func(*Fence, *testing.T) {},
			pairs:   []Pair{{Callout: "c", Major: 1}},
			refused: true,
		},
		{
			name: "begun but never advanced: no pass is current yet",
			prepare: func(f *Fence, t *testing.T) {
				begin(t, f, context.Background(), "c", "tx", nil)
			},
			pairs:   []Pair{{Callout: "c", Major: 0}},
			refused: true,
		},
		{
			name: "current major",
			prepare: func(f *Fence, t *testing.T) {
				begin(t, f, context.Background(), "c", "tx", nil)
				f.Advance("c", 1)
			},
			pairs: []Pair{{Callout: "c", Major: 1}},
		},
		{
			name: "lower major after the work went to another cnode",
			prepare: func(f *Fence, t *testing.T) {
				begin(t, f, context.Background(), "c", "tx", nil)
				f.Advance("c", 1)
				f.Advance("c", 2)
			},
			pairs:   []Pair{{Callout: "c", Major: 1}},
			refused: true,
		},
		{
			name: "a major that was never issued",
			prepare: func(f *Fence, t *testing.T) {
				begin(t, f, context.Background(), "c", "tx", nil)
				f.Advance("c", 1)
			},
			pairs:   []Pair{{Callout: "c", Major: 2}},
			refused: true,
		},
		{
			name: "the number only goes up: a lower Advance changes nothing",
			prepare: func(f *Fence, t *testing.T) {
				begin(t, f, context.Background(), "c", "tx", nil)
				f.Advance("c", 2)
				f.Advance("c", 1)
			},
			pairs:   []Pair{{Callout: "c", Major: 1}},
			refused: true,
		},
		{
			name: "callout ended",
			prepare: func(f *Fence, t *testing.T) {
				_, end := f.Begin(context.Background(), "c", "tx", nil)
				f.Advance("c", 1)
				end()
			},
			pairs:   []Pair{{Callout: "c", Major: 1}},
			refused: true,
		},
		{
			name:    "no pairs at all",
			prepare: func(*Fence, *testing.T) {},
			pairs:   nil,
			refused: true,
		},
		{
			name: "enclosing callout no longer current",
			prepare: func(f *Fence, t *testing.T) {
				begin(t, f, context.Background(), "outer", "tx", nil)
				f.Advance("outer", 1)
				begin(t, f, context.Background(), "inner", "tx", []Pair{{Callout: "outer", Major: 1}})
				f.Advance("inner", 1)
				f.Advance("outer", 2)
			},
			pairs:   []Pair{{Callout: "inner", Major: 1}, {Callout: "outer", Major: 1}},
			refused: true,
		},
		{
			name: "own and enclosing callout both current",
			prepare: func(f *Fence, t *testing.T) {
				begin(t, f, context.Background(), "outer", "tx", nil)
				f.Advance("outer", 1)
				begin(t, f, context.Background(), "inner", "tx", []Pair{{Callout: "outer", Major: 1}})
				f.Advance("inner", 1)
			},
			pairs: []Pair{{Callout: "inner", Major: 1}, {Callout: "outer", Major: 1}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFence()
			tc.prepare(f, t)
			base := context.Background()
			ctx, err := f.Admit(base, tc.pairs)
			if tc.refused {
				assertRefused(t, err)
				if ctx != base {
					t.Fatal("a refused Admit must hand back the caller's context unchanged")
				}
				return
			}
			if err != nil {
				t.Fatalf("expected admission, got %v", err)
			}
			got := Pairs(ctx)
			if len(got) != len(tc.pairs) {
				t.Fatalf("Pairs = %v; want %v", got, tc.pairs)
			}
			for i := range got {
				if got[i] != tc.pairs[i] {
					t.Fatalf("Pairs = %v; want %v", got, tc.pairs)
				}
			}
		})
	}
}

func TestCheck_NeverAdmittedAlwaysPasses(t *testing.T) {
	if err := Check(context.Background()); err != nil {
		t.Fatalf("the owner's own chain carries no pairs and is never refused, got %v", err)
	}
	if got := Pairs(context.Background()); got != nil {
		t.Fatalf("Pairs of a never-admitted context = %v; want nil", got)
	}
}

func TestCheck_FollowsTheNumber(t *testing.T) {
	f := newFence()
	_, end := f.Begin(context.Background(), "c", "tx", nil)
	f.Advance("c", 1)
	ctx, err := f.Admit(context.Background(), []Pair{{Callout: "c", Major: 1}})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if err := Check(ctx); err != nil {
		t.Fatalf("Check under the current number: %v", err)
	}
	// The pairs are a context VALUE: detaching cancellation keeps them.
	if err := Check(context.WithoutCancel(ctx)); err != nil {
		t.Fatalf("Check on a detached context: %v", err)
	}

	f.Advance("c", 2)
	assertRefused(t, Check(ctx))
	assertRefused(t, Check(context.WithoutCancel(ctx)))
	if ctx.Err() != nil {
		t.Fatal("the fence must never cancel a callback's context")
	}

	ctx2, err := f.Admit(context.Background(), []Pair{{Callout: "c", Major: 2}})
	if err != nil {
		t.Fatalf("Admit under the new number: %v", err)
	}
	end()
	assertRefused(t, Check(ctx2))
	if ctx2.Err() != nil {
		t.Fatal("the fence must never cancel a callback's context")
	}
}

func TestPairs_ReturnsACopy(t *testing.T) {
	f := newFence()
	begin(t, f, context.Background(), "c", "tx", nil)
	f.Advance("c", 1)
	in := []Pair{{Callout: "c", Major: 1}}
	ctx, err := f.Admit(context.Background(), in)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	in[0].Major = 9
	Pairs(ctx)[0].Major = 9
	if got := Pairs(ctx); got[0].Major != 1 {
		t.Fatalf("admitted pairs were mutated through an alias: %v", got)
	}
	if err := Check(ctx); err != nil {
		t.Fatalf("Check after alias mutation: %v", err)
	}
}

// Tries made by another pnode are numbered minor = 1, 2, … under the major of
// the hand-over. The owner learns of the next try from the first callback that
// carries the higher minor.
func TestAdmit_AbsorbsAHigherMinor(t *testing.T) {
	f := newFence()
	begin(t, f, context.Background(), "c", "tx", nil)
	f.Advance("c", 3) // a hand-over under major 3

	first, err := f.Admit(context.Background(), []Pair{{Callout: "c", Major: 3, Minor: 1}})
	if err != nil {
		t.Fatalf("minor 1: %v", err)
	}
	// Until a callback with a higher minor arrives, the earlier cnode of a
	// hand-over is still admitted.
	if _, err := f.Admit(context.Background(), []Pair{{Callout: "c", Major: 3, Minor: 1}}); err != nil {
		t.Fatalf("minor 1 again: %v", err)
	}

	if _, err := f.Admit(context.Background(), []Pair{{Callout: "c", Major: 3, Minor: 2}}); err != nil {
		t.Fatalf("minor 2: %v", err)
	}
	_, err = f.Admit(context.Background(), []Pair{{Callout: "c", Major: 3, Minor: 1}})
	assertRefused(t, err)
	assertRefused(t, Check(first))

	// Advance resets the highest minor seen, so the first callback of a SECOND
	// hand-over (minor 1) is not measured against the last try of the first.
	f.Advance("c", 4)
	if _, err := f.Admit(context.Background(), []Pair{{Callout: "c", Major: 4, Minor: 1}}); err != nil {
		t.Fatalf("minor 1 of the second hand-over: %v", err)
	}
}

// A higher minor is absorbed only after EVERY pair on the pass was verified.
func TestAdmit_RefusedPassAbsorbsNothing(t *testing.T) {
	f := newFence()
	begin(t, f, context.Background(), "outer", "tx", nil)
	f.Advance("outer", 1)
	begin(t, f, context.Background(), "inner", "tx", []Pair{{Callout: "outer", Major: 1}})
	f.Advance("inner", 1)

	lower, err := f.Admit(context.Background(), []Pair{{Callout: "inner", Major: 1, Minor: 1}, {Callout: "outer", Major: 1}})
	if err != nil {
		t.Fatalf("minor 1: %v", err)
	}

	// Minor 5 of "inner", but under an enclosing pair that is not current.
	_, err = f.Admit(context.Background(), []Pair{{Callout: "inner", Major: 1, Minor: 5}, {Callout: "outer", Major: 7}})
	assertRefused(t, err)

	if err := Check(lower); err != nil {
		t.Fatalf("a refused pass must change nothing, but minor 1 is now refused: %v", err)
	}
}

// context.Cause tells the fence's release from a caller that went away.
func TestBegin_CallerCancellationIsNotASupersede(t *testing.T) {
	f := newFence()
	parent, cancel := context.WithCancel(context.Background())
	cctx, _ := begin(t, f, parent, "c", "tx", nil)
	cancel()
	<-cctx.Done()
	if cause := context.Cause(cctx); errors.Is(cause, ErrSuperseded) || !errors.Is(cause, context.Canceled) {
		t.Fatalf("cause = %v; want context.Canceled and not ErrSuperseded", cause)
	}
}

func TestEnd_Idempotent_AndLeavesALaterCalloutAlone(t *testing.T) {
	f := newFence()
	_, end := f.Begin(context.Background(), "c", "tx", nil)
	f.Advance("c", 1)
	end()
	end()

	begin(t, f, context.Background(), "d", "tx", nil)
	f.Advance("d", 1)
	end() // a stale end must not touch "d"
	if _, err := f.Admit(context.Background(), []Pair{{Callout: "d", Major: 1}}); err != nil {
		t.Fatalf("callout d: %v", err)
	}
}

// The Coordinator defers end, so a callout that ends in a panic still shuts its
// passes out.
func TestEnd_RunsOnPanic(t *testing.T) {
	f := newFence()
	func() {
		defer func() { _ = recover() }()
		_, end := f.Begin(context.Background(), "c", "tx", nil)
		defer end()
		f.Advance("c", 1)
		panic("coordinator blew up")
	}()
	_, err := f.Admit(context.Background(), []Pair{{Callout: "c", Major: 1}})
	assertRefused(t, err)
}
