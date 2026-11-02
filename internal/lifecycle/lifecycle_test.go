package lifecycle

import (
	"sync"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/health"
)

func rank(s State) int {
	for i, x := range States() {
		if x == s {
			return i
		}
	}
	return -1
}

func TestLifecycle_Precedence(t *testing.T) {
	cases := []struct {
		tripped, draining, started bool
		want                       State
	}{
		{false, false, false, Starting},
		{false, false, true, Normal},
		{false, true, false, Draining},
		{false, true, true, Draining},
		{true, false, false, Unhealthy},
		{true, false, true, Unhealthy},
		{true, true, false, Unhealthy},
		{true, true, true, Unhealthy},
	}
	for _, c := range cases {
		latch := health.NewLatch()
		l := New(latch)
		if c.started {
			l.MarkStarted()
		}
		if c.draining {
			l.BeginDraining()
		}
		if c.tripped {
			latch.Trip()
		}
		if got := l.State(); got != c.want {
			t.Errorf("tripped=%v draining=%v started=%v: got %s, want %s", c.tripped, c.draining, c.started, got, c.want)
		}
	}
}

func TestLifecycle_NothingUndone(t *testing.T) {
	l := New(health.NewLatch())
	l.BeginDraining()
	l.MarkStarted()
	if l.State() != Draining || !l.Draining() {
		t.Fatalf("got %s, want DRAINING", l.State())
	}

	latch := health.NewLatch()
	u := New(latch)
	latch.Trip()
	u.MarkStarted()
	u.BeginDraining()
	if u.State() != Unhealthy {
		t.Fatalf("got %s, want UNHEALTHY", u.State())
	}
}

func TestLifecycle_ValidAndStates(t *testing.T) {
	for _, s := range States() {
		if !s.Valid() {
			t.Errorf("%s not valid", s)
		}
	}
	if State("BOGUS").Valid() {
		t.Error("BOGUS valid")
	}
}

func TestLifecycle_OnChangeDeliversCurrentThenChanges(t *testing.T) {
	latch := health.NewLatch()
	l := New(latch)
	var mu sync.Mutex
	var got []State
	l.OnChange(func(s State) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, s)
	})
	snap := func() []State {
		mu.Lock()
		defer mu.Unlock()
		return append([]State(nil), got...)
	}
	eq := func(want ...State) bool {
		g := snap()
		if len(g) != len(want) {
			return false
		}
		for i := range g {
			if g[i] != want[i] {
				return false
			}
		}
		return true
	}
	if !eq(Starting) {
		t.Fatalf("after register: %v", snap())
	}
	l.MarkStarted()
	if !eq(Starting, Normal) {
		t.Fatalf("after MarkStarted: %v", snap())
	}
	l.MarkStarted()
	if !eq(Starting, Normal) {
		t.Fatalf("second MarkStarted delivered: %v", snap())
	}
	l.BeginDraining()
	if !eq(Starting, Normal, Draining) {
		t.Fatalf("after BeginDraining: %v", snap())
	}
	latch.Trip()
	deadline := time.Now().Add(2 * time.Second)
	for !eq(Starting, Normal, Draining, Unhealthy) {
		if time.Now().After(deadline) {
			t.Fatalf("trip not delivered: %v", snap())
		}
		time.Sleep(time.Millisecond)
	}
}

func TestLifecycle_ConcurrentTripAndDrain_MonotoneDelivery(t *testing.T) {
	for i := 0; i < 100; i++ {
		latch := health.NewLatch()
		l := New(latch)
		var mu sync.Mutex
		var got []State
		l.OnChange(func(s State) {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, s)
		})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); l.BeginDraining() }()
		go func() { defer wg.Done(); latch.Trip() }()
		wg.Wait()

		deadline := time.Now().Add(2 * time.Second)
		for {
			mu.Lock()
			last := got[len(got)-1]
			mu.Unlock()
			if last == Unhealthy {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("iteration %d: last state %s, want UNHEALTHY", i, last)
			}
			time.Sleep(time.Millisecond)
		}
		mu.Lock()
		for j := 1; j < len(got); j++ {
			if rank(got[j]) < rank(got[j-1]) {
				t.Fatalf("iteration %d: rank decreased: %v", i, got)
			}
		}
		mu.Unlock()
	}
}

func TestLifecycle_OnChangeAfterTrip_NoDuplicateUnhealthy(t *testing.T) {
	for i := 0; i < 200; i++ {
		latch := health.NewLatch()
		l := New(latch)
		var mu sync.Mutex
		counts := map[int]int{}
		listen := func(id int) func(State) {
			return func(s State) {
				if s != Unhealthy {
					return
				}
				mu.Lock()
				defer mu.Unlock()
				counts[id]++
			}
		}
		l.OnChange(listen(0))
		latch.Trip()
		l.OnChange(listen(1))
		deadline := time.Now().Add(2 * time.Second)
		for {
			mu.Lock()
			done := counts[0] >= 1 && counts[1] >= 1
			mu.Unlock()
			if done {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("iteration %d: UNHEALTHY not delivered: %v", i, counts)
			}
			time.Sleep(time.Millisecond)
		}
		time.Sleep(time.Millisecond)
		mu.Lock()
		if counts[0] != 1 || counts[1] != 1 {
			t.Fatalf("iteration %d: UNHEALTHY deliveries %v, want one each", i, counts)
		}
		mu.Unlock()
	}
}
