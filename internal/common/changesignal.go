package common

import "sync"

// ChangeSignal tells any number of waiters that something changed, without a
// timer loop. Changed returns a channel; the next Fire closes it and installs
// a fresh one.
//
// A waiter takes the channel BEFORE it looks at the state it cares about, and
// waits on it only if the state was not what it wanted. A change between the
// look and the wait has then already closed the channel it holds, so no
// wake-up is lost.
type ChangeSignal struct {
	mu sync.Mutex
	ch chan struct{}
}

// NewChangeSignal returns a signal whose first channel is open.
func NewChangeSignal() *ChangeSignal {
	return &ChangeSignal{ch: make(chan struct{})}
}

// Changed returns the channel the next Fire will close.
func (s *ChangeSignal) Changed() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ch
}

// Fire wakes every waiter holding the current channel and replaces it.
func (s *ChangeSignal) Fire() {
	s.mu.Lock()
	defer s.mu.Unlock()
	close(s.ch)
	s.ch = make(chan struct{})
}
