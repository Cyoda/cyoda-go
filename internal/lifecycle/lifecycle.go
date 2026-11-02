// Package lifecycle derives a node's lifecycle state from three one-way facts:
// the node finished starting, it began draining, and its health latch tripped.
package lifecycle

import (
	"sync"

	"github.com/cyoda-platform/cyoda-go/internal/health"
)

// State is a node's lifecycle state.
type State string

const (
	Starting  State = "STARTING"
	Normal    State = "NORMAL"
	Draining  State = "DRAINING"
	Unhealthy State = "UNHEALTHY"
)

// States returns every state in rank order, lowest first. Counts, the metric
// and the size check use this order.
func States() []State { return []State{Starting, Normal, Draining, Unhealthy} }

// Valid reports whether s is one of the defined states.
func (s State) Valid() bool {
	for _, x := range States() {
		if x == s {
			return true
		}
	}
	return false
}

// Lifecycle holds the facts and computes the state by precedence
// UNHEALTHY > DRAINING > STARTING > NORMAL. Every fact is one-way; nothing is
// undone.
type Lifecycle struct {
	latch *health.Latch

	mu        sync.Mutex
	started   bool
	draining  bool
	last      State
	listeners []func(State)
}

// New returns a lifecycle in STARTING and registers for the latch's trip.
func New(latch *health.Latch) *Lifecycle {
	l := &Lifecycle{latch: latch, last: Starting}
	latch.OnTrip(l.refresh)
	return l
}

func (l *Lifecycle) compute() State {
	switch {
	case l.latch.Tripped():
		return Unhealthy
	case l.draining:
		return Draining
	case !l.started:
		return Starting
	default:
		return Normal
	}
}

// State returns the current state.
func (l *Lifecycle) State() State {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.compute()
}

// refresh recomputes the state and, if it changed, delivers it to every
// listener, all under the lock so deliveries stay in order.
func (l *Lifecycle) refresh() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refreshLocked()
}

func (l *Lifecycle) refreshLocked() {
	s := l.compute()
	if s == l.last {
		return
	}
	l.last = s
	for _, f := range l.listeners {
		f(s)
	}
}

// MarkStarted records that startup finished.
func (l *Lifecycle) MarkStarted() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.started = true
	l.refreshLocked()
}

// BeginDraining records that the node began draining.
func (l *Lifecycle) BeginDraining() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.draining = true
	l.refreshLocked()
}

// Draining reports whether the node began draining.
func (l *Lifecycle) Draining() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.draining
}

// OnChange registers f. It is called once now with the current state, then
// once per change of the computed state, under the lifecycle's lock and in
// order, so a later call never carries a state that ranks lower in States()
// order. f must not block or call back into the lifecycle.
func (l *Lifecycle) OnChange(f func(State)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// Bring last up to date first, so a trip whose handler has not run yet is
	// delivered to the earlier listeners once, not to f twice.
	l.refreshLocked()
	l.listeners = append(l.listeners, f)
	f(l.last)
}
