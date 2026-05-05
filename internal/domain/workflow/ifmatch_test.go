package workflow

import (
	"context"
	"testing"
)

// TestIfMatch_RoundTrip verifies that withIfMatch + consumeIfMatch is a
// single-shot channel: the value is observable exactly once.
func TestIfMatch_RoundTrip(t *testing.T) {
	ctx := withIfMatch(context.Background(), "tx-pre-123")

	got, ok := consumeIfMatch(ctx)
	if !ok || got != "tx-pre-123" {
		t.Fatalf("first consumeIfMatch: got (%q,%v), want (%q,%v)", got, ok, "tx-pre-123", true)
	}

	got2, ok2 := consumeIfMatch(ctx)
	if ok2 || got2 != "" {
		t.Fatalf("second consumeIfMatch: got (%q,%v), want (\"\",false)", got2, ok2)
	}
}

// TestIfMatch_EmptyNoSlot verifies that withIfMatch("") does not allocate a
// slot — consumeIfMatch must return ("", false) on the unwrapped context.
func TestIfMatch_EmptyNoSlot(t *testing.T) {
	parent := context.Background()
	child := withIfMatch(parent, "")
	if child != parent {
		t.Errorf("withIfMatch(\"\") should return parent unchanged; got a new context")
	}
	if got, ok := consumeIfMatch(child); ok || got != "" {
		t.Errorf("consumeIfMatch on empty: got (%q,%v), want (\"\",false)", got, ok)
	}
}

// TestIfMatch_NoSlot verifies consumeIfMatch on a bare context returns the
// not-present sentinel.
func TestIfMatch_NoSlot(t *testing.T) {
	if got, ok := consumeIfMatch(context.Background()); ok || got != "" {
		t.Errorf("consumeIfMatch on bare ctx: got (%q,%v), want (\"\",false)", got, ok)
	}
}
