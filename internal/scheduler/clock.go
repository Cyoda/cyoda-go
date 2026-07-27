package scheduler

import "time"

// Clock abstracts wall-clock time so the scan loop (and its tests) can
// inject a deterministic source instead of depending on time.Now directly.
type Clock interface {
	Now() time.Time
}

// realClock is the production Clock backed by time.Now.
type realClock struct{}

// NewRealClock returns the production Clock implementation.
func NewRealClock() Clock {
	return realClock{}
}

func (realClock) Now() time.Time {
	return time.Now()
}
