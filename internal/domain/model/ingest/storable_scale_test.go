package ingest

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestRejectUnstorable_ScalesWithSizeNotSizeTimesDepth pins the cost of the
// guard against a deeply nested payload.
//
// An earlier version re-parsed each subtree once per nesting level, making the
// walk O(size × depth) in time and allocation. A 21 KB body nested 9999 deep
// took 1.1s and allocated 1.76 GiB — roughly 85,000x amplification on a single
// authenticated request, on every entity write path. Nothing bounds that
// product: the 10 MB body limit bounds size only, and nesting is capped at
// 10000 by encoding/json.
//
// The scan is now in place, so cost is proportional to the payload, not to the
// payload times its depth. The bounds below are deliberately loose — they are
// there to catch a return to quadratic behaviour, not to police micro-timings.
func TestRejectUnstorable_ScalesWithSizeNotSizeTimesDepth(t *testing.T) {
	const depth = 9999
	leaf := `"` + strings.Repeat("x", 1024) + `"`
	payload := []byte(strings.Repeat("[", depth) + leaf + strings.Repeat("]", depth))

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	start := time.Now()

	if err := RejectUnstorable(payload); err != nil {
		t.Fatalf("payload is storable, guard rejected it: %v", err)
	}

	elapsed := time.Since(start)
	runtime.ReadMemStats(&after)
	allocMiB := float64(after.TotalAlloc-before.TotalAlloc) / (1 << 20)

	t.Logf("%d bytes at depth %d: %s, %.1f MiB allocated", len(payload), depth, elapsed, allocMiB)

	if elapsed > 200*time.Millisecond {
		t.Errorf("took %s for a %d-byte payload — cost is scaling with depth again", elapsed, len(payload))
	}
	if allocMiB > 50 {
		t.Errorf("allocated %.1f MiB for a %d-byte payload — the scan is copying subtrees again",
			allocMiB, len(payload))
	}
}

// TestRejectUnstorable_DeepNestingStillFindsContent guards the depth cap from
// being used as a bypass: content inside a deeply nested payload must still be
// found, up to the point the decoder itself refuses to parse.
func TestRejectUnstorable_DeepNestingStillFindsContent(t *testing.T) {
	for _, depth := range []int{1, 10, 500, 5000} {
		payload := []byte(strings.Repeat("[", depth) + `"a\u0000b"` + strings.Repeat("]", depth))
		if err := RejectUnstorable(payload); err == nil {
			t.Errorf("depth %d: NUL nested this deep was not found", depth)
		}
	}
}
