package auth

import (
	"fmt"
	"testing"
	"time"
)

func TestClientBuckets_LimitPerClient(t *testing.T) {
	b := newClientBuckets(2)
	now := time.Now()
	for i := 0; i < 2; i++ {
		if ok, _ := b.allow("C1", now); !ok {
			t.Fatalf("request %d refused within the burst", i+1)
		}
	}
	ok, wait := b.allow("C1", now)
	if ok || wait <= 0 {
		t.Fatalf("third request in the same instant: ok=%v wait=%v, want refused with a wait", ok, wait)
	}
	if ok, _ := b.allow("C2", now); !ok {
		t.Fatal("another client refused: buckets are per client")
	}
	// The refusal consumed nothing: one token is back after 30 s at 2/min.
	if ok, _ := b.allow("C1", now.Add(30*time.Second)); !ok {
		t.Fatal("refused after the refill interval")
	}
}

func TestClientBuckets_ZeroIsUnlimited(t *testing.T) {
	b := newClientBuckets(0)
	now := time.Now()
	for i := 0; i < 10000; i++ {
		if ok, _ := b.allow("C1", now); !ok {
			t.Fatalf("request %d refused with no limit", i+1)
		}
	}
}

func TestClientBuckets_NegativeIsUnlimited(t *testing.T) {
	b := newClientBuckets(-1)
	now := time.Now()
	for i := 0; i < 1000; i++ {
		if ok, _ := b.allow("C1", now); !ok {
			t.Fatalf("request %d refused with a negative limit", i+1)
		}
	}
}

// A full bucket is the same as a fresh one, so the buckets of clients idle
// for a refill period are dropped: the map holds only recently active
// clients.
func TestClientBuckets_IdleBucketsAreDropped(t *testing.T) {
	b := newClientBuckets(60)
	now := time.Now()
	for i := 0; i < 5000; i++ {
		b.allow(fmt.Sprintf("C%d", i), now)
	}
	b.allow("LATE", now.Add(2*time.Minute))
	n := func() int {
		b.mu.Lock()
		defer b.mu.Unlock()
		return len(b.limiters)
	}()
	if n != 1 {
		t.Fatalf("%d buckets after every earlier client was idle for two minutes", n)
	}
}
