package auth

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A trigger during an in-flight run coalesces to exactly one trailing run.
func TestCoalescingRunner_TrailingRun(t *testing.T) {
	var runs atomic.Int32
	inFirst := make(chan struct{})
	release := make(chan struct{})
	var c coalescingRunner

	c.Trigger(func() {
		runs.Add(1)
		close(inFirst)
		<-release
	})
	<-inFirst
	c.Trigger(func() { runs.Add(1) }) // arrives mid-flight → dirty flag
	c.Trigger(func() { runs.Add(1) }) // second mid-flight → still one trailing
	close(release)

	deadline := time.After(2 * time.Second)
	for runs.Load() != 2 {
		select {
		case <-deadline:
			t.Fatalf("want exactly 2 runs (1 + 1 trailing), got %d", runs.Load())
		case <-time.After(5 * time.Millisecond):
		}
	}
	time.Sleep(50 * time.Millisecond)
	if got := runs.Load(); got != 2 {
		t.Fatalf("extra trailing runs: got %d", got)
	}
}

// Concurrent triggers never lose the final state and never deadlock.
func TestCoalescingRunner_ConcurrentTriggers(t *testing.T) {
	var runs atomic.Int32
	var c coalescingRunner
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Trigger(func() { runs.Add(1) })
		}()
	}
	wg.Wait()
	deadline := time.After(2 * time.Second)
	for {
		if !c.busy() {
			break
		}
		select {
		case <-deadline:
			t.Fatal("runner still busy after all triggers")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if runs.Load() < 1 {
		t.Fatal("no run executed")
	}
}
