package txgate

import (
	"sync"
	"testing"
	"time"
)

func TestRegistry_SerialisesSameTxID(t *testing.T) {
	r := New()
	var mu sync.Mutex
	var order []int
	var wg sync.WaitGroup
	rel1 := r.Acquire("tx-1")
	wg.Add(1)
	go func() {
		defer wg.Done()
		rel := r.Acquire("tx-1") // must block until rel1() runs
		mu.Lock()
		order = append(order, 2)
		mu.Unlock()
		rel()
	}()
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	order = append(order, 1)
	mu.Unlock()
	rel1()
	wg.Wait()
	if len(order) != 2 || order[0] != 1 || order[1] != 2 {
		t.Fatalf("expected serialized order [1 2], got %v", order)
	}
}

func TestRegistry_DifferentTxIDsDoNotBlock(t *testing.T) {
	r := New()
	rel1 := r.Acquire("tx-1")
	done := make(chan struct{})
	go func() { rel2 := r.Acquire("tx-2"); rel2(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Acquire on a different txID blocked")
	}
	rel1()
}

func TestRegistry_ReleasesMapEntry(t *testing.T) {
	r := New()
	r.Acquire("tx-1")() // acquire+release
	if n := r.len(); n != 0 {
		t.Fatalf("expected empty gate map after release, got %d", n)
	}
}
