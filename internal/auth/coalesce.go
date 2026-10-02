package auth

import "sync"

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
	go func() {
		next := run
		for {
			next()
			var again bool
			next, again = func() (func(), bool) {
				c.mu.Lock()
				defer c.mu.Unlock()
				if c.dirty {
					c.dirty = false
					p := c.pending
					c.pending = nil
					return p, true
				}
				c.running = false
				close(c.done)
				return nil, false
			}()
			if !again {
				return
			}
		}
	}()
}

// Wait blocks until any run in flight at the moment it is called — including
// a trailing run already queued by a Trigger that arrived mid-flight — has
// finished. Returns immediately if nothing is running. Production shutdown
// behaviour (kvReplica.Wait calls it), not a test hook: a caller that has
// already stopped triggering new runs (cancelled the ctx reconcileOnce
// checks) uses it to be sure a run already under way — one that could still
// be mid-store-call — has actually returned before anything it was reading
// closes. It does not wait for a Trigger that arrives after Wait has already
// observed the runner idle; making that safe is the caller's job (see
// kvReplica's ctx-done guard), not this runner's.
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
