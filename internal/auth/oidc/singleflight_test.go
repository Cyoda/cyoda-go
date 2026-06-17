package oidc

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSingleflightDebouncer_DropsConcurrentSameKey(t *testing.T) {
	sf := newSingleflightDebouncer()
	var count int32
	block := make(chan struct{})
	var firstStarted sync.WaitGroup
	firstStarted.Add(1)

	fn := func() {
		atomic.AddInt32(&count, 1)
		firstStarted.Done()
		<-block
	}

	if ok := sf.Dispatch("k1", fn); !ok {
		t.Fatal("first dispatch dropped unexpectedly")
	}
	firstStarted.Wait()

	// Second dispatch under same key should be dropped.
	if ok := sf.Dispatch("k1", fn); ok {
		t.Fatal("second dispatch should drop")
	}
	close(block)
	time.Sleep(20 * time.Millisecond)

	if got := atomic.LoadInt32(&count); got != 1 {
		t.Errorf("count = %d, want 1", got)
	}
}

func TestSingleflightDebouncer_AllowsDifferentKeys(t *testing.T) {
	sf := newSingleflightDebouncer()
	var count int32
	done := make(chan struct{}, 2)
	fn := func() { atomic.AddInt32(&count, 1); done <- struct{}{} }

	_ = sf.Dispatch("k1", fn)
	_ = sf.Dispatch("k2", fn)
	<-done
	<-done
	if got := atomic.LoadInt32(&count); got != 2 {
		t.Errorf("count = %d, want 2", got)
	}
}

func TestSingleflightDebouncer_ReleasesKeyAfterCompletion(t *testing.T) {
	sf := newSingleflightDebouncer()
	done := make(chan struct{}, 1)
	fn := func() { done <- struct{}{} }

	_ = sf.Dispatch("k1", fn)
	<-done
	time.Sleep(5 * time.Millisecond) // give defer time to run

	// After completion, the same key should dispatch again.
	if ok := sf.Dispatch("k1", fn); !ok {
		t.Error("expected key to be free after first completion")
	}
	<-done
}
