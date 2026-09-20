package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// TestSlowConfigurable_SleepsForSleepMS verifies the slow-configurable
// processor actually sleeps for sleep_ms. The server delivers the
// processor's context as a JSON string in parameters (dispatch.go passes
// req.Parameters through unchanged), so the config must be unwrapped from
// its string encoding before sleep_ms can be read.
func TestSlowConfigurable_SleepsForSleepMS(t *testing.T) {
	fn, ok := newCatalog(nil, nil).processor("slow-configurable")
	if !ok {
		t.Fatal("slow-configurable is not in the catalog")
	}
	cfg, _ := json.Marshal(`{"sleep_ms": 120}`)
	start := time.Now()
	if _, err := fn(context.Background(), &Entity{ID: "e"}, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d := time.Since(start); d < 100*time.Millisecond {
		t.Fatalf("slept %v, want at least 100ms", d)
	}
}
