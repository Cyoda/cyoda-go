package fence

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// Begin, Admit, Check, Advance and end from many goroutines at once: clean
// under -race, and every callout ends shut.
func TestFence_ConcurrentUse(t *testing.T) {
	f := newFence()
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := string(rune('a' + i))
			for round := range 50 {
				outerCtx, endOuter := f.Begin(context.Background(), id, nil)
				f.Advance(id, 1)
				cb, err := f.Admit(outerCtx, []Pair{{Callout: id, Major: 1}})
				if err != nil {
					t.Errorf("Admit: %v", err)
					endOuter()
					return
				}
				innerID := id + "-inner"
				innerCtx, endInner := f.Begin(cb, innerID, Pairs(cb))
				f.Advance(innerID, 1)
				if round%2 == 0 {
					f.Advance(id, 2)
					<-innerCtx.Done()
					if !errors.Is(Check(cb), ErrSuperseded) {
						t.Error("callback still admitted after its cnode was replaced")
					}
				}
				endInner()
				endOuter()
				if _, err := f.Admit(context.Background(), []Pair{{Callout: id, Major: 2}}); !errors.Is(err, ErrSuperseded) {
					t.Errorf("pass admitted after its callout ended: %v", err)
				}
			}
		}()
	}
	wg.Wait()

	remaining := func() int {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.callouts)
	}()
	if remaining != 0 {
		t.Fatalf("%d callouts still registered after every end ran: nothing may be kept past a callout", remaining)
	}
}
