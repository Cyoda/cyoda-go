package auth

import "sync"

// coalescingRunner runs at most one execution of a job at a time and
// coalesces triggers that arrive mid-run into exactly one trailing rerun.
// Unlike a drop-style singleflight, a trigger is never lost: state observed
// after the triggering event is always re-read by the trailing run. Used by
// the trusted-key gossip ping handler, where a dropped trigger would
// silently downgrade revocation propagation to backstop latency.
type coalescingRunner struct {
	mu      sync.Mutex
	running bool
	dirty   bool
	pending func() // latest run passed to a Trigger that arrived mid-flight
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
				return nil, false
			}()
			if !again {
				return
			}
		}
	}()
}

// busy is a test-only inspector.
func (c *coalescingRunner) busy() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.running
}
