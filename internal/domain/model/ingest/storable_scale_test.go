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

// TestRejectUnstorable_CostIsIndependentOfDepth pins the second half of the
// scaling property, which the depth-only test above cannot see.
//
// The first fix removed per-level subtree copying but left the path stack being
// derived per node via append. At any depth where len == cap, that re-runs
// growslice for EVERY sibling and copies the whole stack — reintroducing the
// same O(size x depth) blowup, relocated. It was invisible to every test here
// because they are all small, and it only bites at Go's slice-growth depths
// (1, 2, 4, 8, 16, 35, 71, ... 6912, 8960).
//
// Measured on the version this guards against: a 7.8 MB well-formed payload
// allocated 1.45 TB and took ~4 minutes — and was then ACCEPTED, so nothing
// downstream cut it short.
//
// The property is that the same number of bytes costs about the same whether it
// is flat or deeply nested. 8960 is chosen because it is one of the slice-growth
// depths, i.e. the worst case.
func TestRejectUnstorable_CostIsIndependentOfDepth(t *testing.T) {
	const depth = 8960
	const elems = 20000

	flat := []byte("[" + strings.Repeat("1,", elems) + "1]")
	nested := []byte(strings.Repeat("[", depth) + "[" + strings.Repeat("1,", elems) + "1]" +
		strings.Repeat("]", depth))

	measure := func(b []byte) (time.Duration, float64) {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		start := time.Now()
		if err := RejectUnstorable(b); err != nil {
			t.Fatalf("payload is storable, guard rejected it: %v", err)
		}
		d := time.Since(start)
		runtime.ReadMemStats(&after)
		return d, float64(after.TotalAlloc-before.TotalAlloc) / (1 << 20)
	}

	flatTime, flatMiB := measure(flat)
	nestedTime, nestedMiB := measure(nested)
	t.Logf("flat   %d bytes: %s, %.1f MiB", len(flat), flatTime, flatMiB)
	t.Logf("nested %d bytes at depth %d: %s, %.1f MiB", len(nested), depth, nestedTime, nestedMiB)

	// Generous factors: this catches a return to depth-multiplied cost, it does
	// not police micro-timings.
	if nestedMiB > flatMiB*8+16 {
		t.Errorf("nesting multiplied allocation: flat %.1f MiB vs nested %.1f MiB — "+
			"the path stack is being copied per sibling again", flatMiB, nestedMiB)
	}
	if nestedTime > flatTime*20+50*time.Millisecond {
		t.Errorf("nesting multiplied CPU: flat %s vs nested %s", flatTime, nestedTime)
	}
}
