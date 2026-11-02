// Package health holds the process-wide health latch.
package health

import "sync"

// Latch is a one-way health latch. Trip marks it tripped for good and notifies
// every OnTrip callback. All methods are safe on a nil *Latch, which never
// reports tripped.
type Latch struct {
	mu        sync.Mutex
	tripped   bool
	callbacks []func()
}

// NewLatch returns an untripped latch.
func NewLatch() *Latch { return &Latch{} }

// Trip trips the latch. The first call starts each registered callback in its
// own goroutine, so Trip never blocks (it runs from panic-recovery paths).
// Later calls do nothing.
func (l *Latch) Trip() {
	if l == nil {
		return
	}
	cbs := func() []func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.tripped {
			return nil
		}
		l.tripped = true
		cbs := l.callbacks
		l.callbacks = nil
		return cbs
	}()
	for _, f := range cbs {
		go f()
	}
}

// Tripped reports whether the latch has tripped.
func (l *Latch) Tripped() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.tripped
}

// OnTrip registers f to run once, in its own goroutine, when the latch trips,
// or at once in a goroutine if it has already tripped.
func (l *Latch) OnTrip(f func()) {
	if l == nil {
		return
	}
	already := func() bool {
		l.mu.Lock()
		defer l.mu.Unlock()
		if !l.tripped {
			l.callbacks = append(l.callbacks, f)
		}
		return l.tripped
	}()
	if already {
		go f()
	}
}
