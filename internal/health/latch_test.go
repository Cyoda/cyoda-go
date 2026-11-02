package health

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestLatch_TripIsOneWay(t *testing.T) {
	l := NewLatch()
	if l.Tripped() {
		t.Fatal("a new latch is tripped")
	}
	l.Trip()
	if !l.Tripped() {
		t.Fatal("latch not tripped after Trip")
	}
	l.Trip()
	if !l.Tripped() {
		t.Fatal("latch reset by a second Trip")
	}
}

func TestLatch_OnTripNotifiesOnceWithoutBlocking(t *testing.T) {
	l := NewLatch()
	release := make(chan struct{})
	var runs atomic.Int32
	started := make(chan struct{}, 4)
	l.OnTrip(func() {
		runs.Add(1)
		started <- struct{}{}
		<-release
	})

	done := make(chan struct{})
	go func() {
		l.Trip()
		l.Trip()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Trip blocked on a callback")
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("callback never ran")
	}
	close(release)
	time.Sleep(50 * time.Millisecond)
	if got := runs.Load(); got != 1 {
		t.Fatalf("callback ran %d times, want 1", got)
	}
}

func TestLatch_OnTripAfterTrip(t *testing.T) {
	l := NewLatch()
	l.Trip()
	ran := make(chan struct{})
	l.OnTrip(func() { close(ran) })
	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("callback registered after the trip never ran")
	}
}

func TestLatch_NilIsSafe(t *testing.T) {
	var l *Latch
	l.Trip()
	if l.Tripped() {
		t.Fatal("nil latch reports tripped")
	}
}
