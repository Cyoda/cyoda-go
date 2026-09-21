package workflow

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestCapReason covers the ASCII length-bounding behaviour of the criterion
// reason cap — the security control that bounds compute-node-supplied text
// before it is persisted to the audit trail or reflected into a 400 body.
func TestCapReason(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantLen int
	}{
		{"empty", "", 0},
		{"short ascii", "credit score too low", len("credit score too low")},
		{"exactly at cap", strings.Repeat("a", maxCriterionReasonLen), maxCriterionReasonLen},
		{"one over cap", strings.Repeat("a", maxCriterionReasonLen+1), maxCriterionReasonLen},
		{"far over cap", strings.Repeat("a", maxCriterionReasonLen*4), maxCriterionReasonLen},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := capReason(tc.in)
			if len(got) != tc.wantLen {
				t.Errorf("len = %d, want %d", len(got), tc.wantLen)
			}
			if !strings.HasPrefix(tc.in, got) {
				t.Errorf("result %q is not a prefix of the input", got)
			}
		})
	}
}

// TestMaxCriterionReasonLen_NeverRecutsTheMemberBound enforces the invariant
// behind maxCriterionReasonLen's comment: the member-side bound
// (internal/grpc's MaxCriterionReasonRunes — 2048 runes, marked with "…") is
// meant to be the one that bites. capReason's byte cap must be at least that
// bound's worst case in bytes — 2048 runes at up to 4 bytes each (the most a
// single UTF-8 rune can encode to) plus the 3-byte ellipsis — or a reason
// already at its rune limit would be cut a second time here, unmarked. This
// tree does not import internal/grpc from the domain layer, so the bound is
// restated rather than referenced; TestDispatchCriteria_ReasonIsBounded
// (internal/grpc) pins the source value.
func TestMaxCriterionReasonLen_NeverRecutsTheMemberBound(t *testing.T) {
	const memberBoundRunes = 2048 // internal/grpc's MaxCriterionReasonRunes
	const maxBytesPerRune = 4     // the most a single UTF-8 rune can encode to
	const ellipsisBytes = 3       // "…" (U+2026) in UTF-8
	worstCase := memberBoundRunes*maxBytesPerRune + ellipsisBytes
	if maxCriterionReasonLen < worstCase {
		t.Fatalf("maxCriterionReasonLen = %d bytes, want >= %d: capReason would re-cut a reason already bounded to %d runes",
			maxCriterionReasonLen, worstCase, memberBoundRunes)
	}
}

// TestCapReason_NeverSplitsRune verifies that truncation backs off to a UTF-8
// rune boundary rather than splitting a multibyte rune. '世' is 3 bytes and
// maxCriterionReasonLen (2048) is not a multiple of 3, so a naive byte-slice
// at 2048 would split a rune and produce invalid UTF-8.
func TestCapReason_NeverSplitsRune(t *testing.T) {
	in := strings.Repeat("世", (maxCriterionReasonLen/3)+10) // > 2048 bytes
	got := capReason(in)

	if len(got) > maxCriterionReasonLen {
		t.Fatalf("result %d bytes exceeds cap %d", len(got), maxCriterionReasonLen)
	}
	if !utf8.ValidString(got) {
		t.Errorf("result is not valid UTF-8 — truncation split a multibyte rune")
	}
	if len(got)%3 != 0 {
		t.Errorf("expected only whole 3-byte runes, got %d bytes", len(got))
	}
	if !strings.HasPrefix(in, got) {
		t.Errorf("result is not a prefix of the input")
	}
}
