package oidc

import "sync"

// singleflightDebouncer drops concurrent same-key dispatches. Unlike
// golang.org/x/sync/singleflight, we don't queue callers for the result —
// we just discard later calls while the in-flight one runs. This matches
// D18's intent: collapse a burst of broadcasts for the same (T, uri) into
// one reload, dropping the rest entirely.
type singleflightDebouncer struct {
	mu       sync.Mutex
	inFlight map[string]struct{}
}

func newSingleflightDebouncer() *singleflightDebouncer {
	return &singleflightDebouncer{inFlight: make(map[string]struct{})}
}

// inFlightCount returns the number of keys currently in the inFlight map.
// It is package-private and used only by tests.
func (s *singleflightDebouncer) inFlightCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.inFlight)
}

// Dispatch returns true if a goroutine was spawned to run fn, false if the
// call was dropped because another for the same key is in flight.
func (s *singleflightDebouncer) Dispatch(key string, fn func()) bool {
	busy := func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, b := s.inFlight[key]; b {
			return true
		}
		s.inFlight[key] = struct{}{}
		return false
	}()
	if busy {
		return false
	}

	go func() {
		defer func() {
			s.mu.Lock()
			delete(s.inFlight, key)
			s.mu.Unlock()
		}()
		fn()
	}()
	return true
}
