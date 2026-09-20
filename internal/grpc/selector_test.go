package grpc

import (
	"sync"
	"testing"

	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
)

func TestRoundRobinSelector_AlternatesAndANewMemberGoesFirst(t *testing.T) {
	reg := NewMemberRegistry()
	sel := NewRoundRobinSelector(reg)
	register := func(id string) {
		m := reg.Register(id, "tenant-1", []string{"x"}, noopSend, nil)
		t.Cleanup(func() { reg.Unregister(m) })
	}
	pick := func() string { return sel.Select(reg.Candidates("tenant-1", "x")).ID }

	register("m-1")
	register("m-2")
	for i, want := range []string{"m-1", "m-2", "m-1"} {
		if got := pick(); got != want {
			t.Fatalf("pick %d = %s, want %s", i, got, want)
		}
	}
	register("m-3") // never picked: goes first, though it attached last
	for i, want := range []string{"m-3", "m-2", "m-1", "m-3"} {
		if got := pick(); got != want {
			t.Fatalf("pick %d after m-3 attached = %s, want %s", i, got, want)
		}
	}
}

// RunLocal hands the selector only the cnodes it has not tried; the selector
// must choose among exactly those.
func TestRoundRobinSelector_ChoosesOnlyAmongWhatItIsGiven(t *testing.T) {
	reg := NewMemberRegistry()
	sel := NewRoundRobinSelector(reg)
	a := reg.Register("m-1", "tenant-1", []string{"x"}, noopSend, nil)
	b := reg.Register("m-2", "tenant-1", []string{"x"}, noopSend, nil)
	t.Cleanup(func() { reg.Unregister(a); reg.Unregister(b) })

	for i := 0; i < 3; i++ {
		if got := sel.Select([]*Member{b}); got != b {
			t.Fatalf("Select([m-2]) = %s", got.ID)
		}
	}
	if got := sel.Select(reg.Candidates("tenant-1", "x")); got != a {
		t.Fatalf("m-1 was never picked and must go first, got %s", got.ID)
	}
}

// Picking and stamping are one step: concurrent callouts share the cnodes
// evenly rather than all landing on the one that looked least recent.
func TestRoundRobinSelector_ConcurrentPicksAreSpreadEvenly(t *testing.T) {
	reg := NewMemberRegistry()
	sel := NewRoundRobinSelector(reg)
	a := reg.Register("m-1", "tenant-1", []string{"x"}, noopSend, nil)
	b := reg.Register("m-2", "tenant-1", []string{"x"}, noopSend, nil)
	t.Cleanup(func() { reg.Unregister(a); reg.Unregister(b) })

	var mu sync.Mutex
	picks := map[string]int{}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := sel.Select(reg.Candidates("tenant-1", "x")).ID
			mu.Lock()
			defer mu.Unlock()
			picks[id]++
		}()
	}
	wg.Wait()
	if picks["m-1"] != 50 || picks["m-2"] != 50 {
		t.Fatalf("picks = %v, want 50 each", picks)
	}
}

func TestDispatchProcessor_RoundRobinAcrossTwoMembers(t *testing.T) {
	registry := NewMemberRegistry()
	var mu sync.Mutex
	got := map[string]int{}
	for _, id := range []string{"m-1", "m-2"} {
		m := registry.Register(id, testTenantID, []string{"python"}, func(ce *cepb.CloudEvent) error {
			reqID, err := extractRequestID(ce)
			if err != nil {
				t.Errorf("extractRequestID: %v", err)
				return nil
			}
			func() {
				mu.Lock()
				defer mu.Unlock()
				got[id]++
			}()
			registry.Get(id).CompleteRequest(reqID, &ProcessingResponse{Success: true})
			return nil
		}, nil)
		t.Cleanup(func() { registry.Unregister(m) })
	}
	dispatcher := newTestDispatcher(t, registry)
	processor := testProcessor("python", 5000)
	for i := 0; i < 4; i++ {
		if _, err := dispatcher.DispatchProcessor(testContext(), testEntity(), processor, "wf1", "t1", "tx-1"); err != nil {
			t.Fatalf("dispatch %d: %v", i, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if got["m-1"] != 2 || got["m-2"] != 2 {
		t.Fatalf("requests per member = %v, want 2 each", got)
	}
}
