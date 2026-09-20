package fence

import (
	"context"
	"errors"
	"testing"
)

func TestBegin_ContextIsReleasedWhenAnEnclosingPairStopsBeingCurrent(t *testing.T) {
	tests := []struct {
		name string
		stop func(f *Fence, endOuter func())
	}{
		{"through Advance", func(f *Fence, _ func()) { f.Advance("outer", 2) }},
		{"through end", func(_ *Fence, endOuter func()) { endOuter() }},
		{"through a higher minor absorbed by Admit", func(f *Fence, _ func()) {
			if _, err := f.Admit(context.Background(), []Pair{{Callout: "outer", Major: 1, Minor: 2}}); err != nil {
				panic(err)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFence()
			_, endOuter := begin(t, f, context.Background(), "outer", "tx", nil)
			f.Advance("outer", 1)
			if _, err := f.Admit(context.Background(), []Pair{{Callout: "outer", Major: 1, Minor: 1}}); err != nil {
				t.Fatalf("Admit: %v", err)
			}

			cctx, _ := begin(t, f, context.Background(), "inner", "tx", []Pair{{Callout: "outer", Major: 1, Minor: 1}})
			f.Advance("inner", 1)
			if cctx.Err() != nil {
				t.Fatal("inner context cancelled while its enclosing pair is current")
			}

			tc.stop(f, endOuter)

			select {
			case <-cctx.Done():
			default:
				t.Fatal("inner context must be cancelled once the enclosing pair is not current")
			}
			if cause := context.Cause(cctx); !errors.Is(cause, ErrSuperseded) {
				t.Fatalf("cause = %v; want ErrSuperseded", cause)
			}
		})
	}
}

func TestBegin_UnderAnEnclosingPairThatIsAlreadyStale(t *testing.T) {
	f := newFence()
	begin(t, f, context.Background(), "outer", "tx", nil)
	f.Advance("outer", 2)

	cctx, _ := begin(t, f, context.Background(), "inner", "tx", []Pair{{Callout: "outer", Major: 1}})
	if cause := context.Cause(cctx); !errors.Is(cause, ErrSuperseded) {
		t.Fatalf("cause = %v; want ErrSuperseded at once", cause)
	}
}

// An enclosing pair can stop being current between the callback's Admit and the
// inner callout starting — the callout can also have ended, or never have been
// registered at all. Begin judges the outer pairs under the fence's mutex, so
// each of these hands back a context that is cancelled already.
func TestBegin_OuterAlreadyStale_ReleasesAtOnce(t *testing.T) {
	tests := []struct {
		name  string
		stale func(f *Fence, endOuter func())
		outer []Pair
	}{
		{"the enclosing callout has ended", func(_ *Fence, endOuter func()) { endOuter() }, []Pair{{Callout: "outer", Major: 1}}},
		{"the enclosing callout was never registered", func(*Fence, func()) {}, []Pair{{Callout: "nobody", Major: 1}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFence()
			_, endOuter := begin(t, f, context.Background(), "outer", "tx", nil)
			f.Advance("outer", 1)
			tc.stale(f, endOuter)

			cctx, _ := begin(t, f, context.Background(), "inner", "tx", tc.outer)
			select {
			case <-cctx.Done():
			default:
				t.Fatal("a callout begun under a pair that is no longer current must be released at once")
			}
			if cause := context.Cause(cctx); !errors.Is(cause, ErrSuperseded) {
				t.Fatalf("cause = %v; want ErrSuperseded", cause)
			}
		})
	}
}

// A callout with no transaction is registered like any other, so that it is
// released with an enclosing callback, and its end runs through without a
// transaction to wait on.
func TestBegin_NoTransaction(t *testing.T) {
	f := newFence()
	begin(t, f, context.Background(), "outer", "tx", nil)
	f.Advance("outer", 1)
	cctx, end := f.Begin(context.Background(), "inner", "", []Pair{{Callout: "outer", Major: 1}})
	f.Advance("inner", 1)
	f.Advance("outer", 2)
	if cause := context.Cause(cctx); !errors.Is(cause, ErrSuperseded) {
		t.Fatalf("cause = %v; want ErrSuperseded", cause)
	}
	end()
}
