package auth

import (
	"log/slog"
	"sync"
)

// coalescingRunner runs at most one execution of a job at a time and
// coalesces triggers that arrive mid-run into exactly one trailing rerun.
// Unlike a drop-style singleflight, a trigger is never lost: state observed
// after the triggering event is always re-read by the trailing run. Used by
// the signing-key gossip ping handler, where a dropped trigger would
// silently downgrade revocation propagation to backstop latency.
type coalescingRunner struct {
	mu      sync.Mutex
	running bool
	dirty   bool
	pending func() // latest run passed to a Trigger that arrived mid-flight
	// done is non-nil exactly while running is true: the run goroutine
	// closes it right before clearing running, so Wait can block on a
	// channel instead of polling. Trigger allocates a fresh one each time a
	// run starts.
	done chan struct{}
}

// Trigger schedules run. If an execution is in flight, it marks the runner
// dirty, records run as the pending trailing job, and returns — the
// in-flight goroutine will invoke the most recently triggered run once more
// when it finishes (not the run that is currently executing: a later
// Trigger's closure reflects state observed after the earlier one, so it is
// the one that must re-read that state). run executes on a fresh goroutine;
// Trigger never blocks.
func (c *coalescingRunner) Trigger(run func()) {
	start := func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.running {
			c.dirty = true
			c.pending = run
			return false
		}
		c.running = true
		c.done = make(chan struct{})
		return true
	}()
	if !start {
		return
	}
	go c.runLoop(run)
}

// runLoop executes run, then any trailing run a Trigger queued while it was
// in flight, until none remain.
func (c *coalescingRunner) runLoop(run func()) {
	for {
		next, again := c.runOnce(run)
		if !again {
			return
		}
		run = next
	}
}

// runOnce runs one job and reports what to do next. Its bookkeeping — check
// for a trailing run, or clear running and close done — happens in a
// deferred block, so a run that panics still clears running and closes
// done: a panicking run cannot hang a concurrent Wait or leave the runner
// permanently reporting itself busy.
func (c *coalescingRunner) runOnce(run func()) (next func(), again bool) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("coalescingRunner: a triggered run panicked", "pkg", "auth", "panic", rec)
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.dirty {
			c.dirty = false
			next = c.pending
			c.pending = nil
			again = true
			return
		}
		c.running = false
		close(c.done)
	}()
	run()
	return
}

// Wait blocks until any run in flight at the moment it is called — including
// a trailing run already queued by a Trigger that arrived mid-flight — has
// finished. Returns immediately if nothing is running. A caller that has
// already stopped triggering new runs (e.g. cancelled the ctx reconcileOnce
// checks) uses it to be sure a run already under way — one that could still
// be mid-store-call — has actually returned before anything it was reading
// closes. It does not wait for a Trigger that arrives after Wait has already
// observed the runner idle; the caller is responsible for making that safe
// (kvReplica's ctx-done guard does this).
func (c *coalescingRunner) Wait() {
	done := func() chan struct{} {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.done
	}()
	if done != nil {
		<-done
	}
}
