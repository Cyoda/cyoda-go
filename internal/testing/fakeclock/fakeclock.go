// Package fakeclock provides a manually controlled clock for tests.
package fakeclock

import (
	"sync"
	"time"
)

// Clock is a settable clock, safe for concurrent use.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// New returns a Clock reading t.
func New(t time.Time) *Clock { return &Clock{now: t} }

// Now returns the clock's current time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Set moves the clock to t.
func (c *Clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// Advance moves the clock forward by d.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}
