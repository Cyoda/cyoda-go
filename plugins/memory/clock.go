package memory

import (
	"sync"
	"time"
)

// Clock abstracts time.Now so tests can advance it deterministically.
// Production uses wallClock; conformance tests use TestClock.
type Clock interface {
	Now() time.Time
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

// TestClock is a mutable clock for tests. Advance(d) moves it forward;
// Now returns the current virtual time. Safe for concurrent use.
type TestClock struct {
	mu  sync.Mutex
	now time.Time
}

// NewTestClock returns a TestClock starting at the current wall-clock time.
func NewTestClock() *TestClock {
	return &TestClock{now: time.Now()}
}

// NewTestClockAt returns a TestClock whose virtual time starts at t.
// Tests that need a millisecond-aligned base (so sub-millisecond Advance
// steps land in a known millisecond) use this instead of NewTestClock.
func NewTestClockAt(t time.Time) *TestClock {
	return &TestClock{now: t}
}

// Now returns the current virtual time.
func (c *TestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the virtual clock forward by d. d must be > 0; d <= 0 panics.
// Matches the spitest.Harness.AdvanceClock contract.
func (c *TestClock) Advance(d time.Duration) {
	if d <= 0 {
		panic("TestClock.Advance: d must be > 0")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}
