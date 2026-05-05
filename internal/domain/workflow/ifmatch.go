package workflow

import (
	"context"
	"sync"
)

// ifMatchSlot is the mutable cell that ManualTransitionWithIfMatch attaches
// to its context so that the first COMMIT_BEFORE_DISPATCH segment-flush can
// consume the caller-supplied If-Match expected-txID before any external
// dispatch fires (spec §4.1).
//
// The slot is single-shot: once a flush consumes the value it stores the
// empty string so subsequent segment commits use the chained-CAS path
// (CompareAndSave against TX_pre's commit-stamped txID, already the default).
//
// We use a pointer-to-struct so the engine writes through the context value
// without re-binding ctx (consume-and-clear semantics). The mutex makes the
// read-and-clear atomic; today's cascade is single-goroutine and safe without
// it, but defense-in-depth is cheap before any future fan-out makes the slot
// concurrently accessible.
type ifMatchSlot struct {
	mu       sync.Mutex
	expected string
}

type ifMatchKey struct{}

// withIfMatch returns a child context carrying expected as a single-shot
// If-Match expected-txID. If expected is empty, ctx is returned unchanged.
//
// No locking on construction: the slot is freshly allocated and not yet
// shared with any reader.
func withIfMatch(ctx context.Context, expected string) context.Context {
	if expected == "" {
		return ctx
	}
	return context.WithValue(ctx, ifMatchKey{}, &ifMatchSlot{expected: expected})
}

// consumeIfMatch atomically reads and clears the If-Match expected-txID set by
// withIfMatch. Returns ("", false) if no slot is present or it has already
// been consumed; otherwise returns the expected value once and clears the slot
// so subsequent calls return ("", false).
func consumeIfMatch(ctx context.Context) (string, bool) {
	s, _ := ctx.Value(ifMatchKey{}).(*ifMatchSlot)
	if s == nil {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.expected == "" {
		return "", false
	}
	out := s.expected
	s.expected = ""
	return out, true
}
