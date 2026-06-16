package workflow

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestTruncateForLog_RuneBoundary fences the contract documented in the
// function comment: "clipped to maxLen runes with a '...' suffix when
// truncation occurred". The function previously measured byte length
// (len(s)) and sliced at the byte boundary (s[:maxLen]), which split
// multi-byte UTF-8 sequences mid-character and produced invalid UTF-8
// in the audit-log preview. Rune-aware truncation is the documented
// behaviour and the only way to keep audit logs valid UTF-8.
func TestTruncateForLog_RuneBoundary(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		maxLen  int
		want    string
		wantLen int // rune length of the returned string
	}{
		{
			name:    "ascii_under_limit_no_truncation",
			in:      "hello",
			maxLen:  10,
			want:    "hello",
			wantLen: 5,
		},
		{
			name:    "ascii_exact_limit_no_truncation",
			in:      "hello",
			maxLen:  5,
			want:    "hello",
			wantLen: 5,
		},
		{
			name:    "ascii_over_limit_truncates_at_rune_boundary",
			in:      "abcdefgh",
			maxLen:  4,
			want:    "abcd...",
			wantLen: 7,
		},
		{
			// Mixed multi-byte content: "a" (1 byte) + "🌍" (4 bytes,
			// 1 rune) + "b" (1 byte) = 6 bytes / 3 runes. Limit=2 must
			// keep "a🌍" intact, not split the emoji.
			name:    "emoji_truncation_preserves_codepoint",
			in:      "a🌍bcdef",
			maxLen:  2,
			want:    "a🌍...",
			wantLen: 5,
		},
		{
			// CJK three-byte runes — Chinese characters are each
			// 3 bytes / 1 rune in UTF-8.
			name:    "cjk_truncation_preserves_codepoint",
			in:      "你好世界abc",
			maxLen:  3,
			want:    "你好世...",
			wantLen: 6,
		},
		{
			// All-emoji string at exactly the rune limit — must NOT
			// truncate just because byte length exceeds maxLen.
			name:    "all_emoji_under_rune_limit_no_truncation",
			in:      "🌍🌎🌏",
			maxLen:  5,
			want:    "🌍🌎🌏",
			wantLen: 3,
		},
		{
			// Empty string: always returned as-is.
			name:    "empty_no_truncation",
			in:      "",
			maxLen:  5,
			want:    "",
			wantLen: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateForLog(tc.in, tc.maxLen)
			if got != tc.want {
				t.Errorf("truncateForLog(%q, %d) = %q, want %q", tc.in, tc.maxLen, got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("truncateForLog produced invalid UTF-8: % x", got)
			}
			if gotRunes := utf8.RuneCountInString(got); gotRunes != tc.wantLen {
				t.Errorf("truncateForLog(%q, %d) returned %d runes, want %d", tc.in, tc.maxLen, gotRunes, tc.wantLen)
			}
		})
	}
}

// TestTruncateForLog_ProducesValidUTF8AtBoundary is a property-style
// fence: for any maxLen value and any string built from multi-byte
// runes, the returned string must always be valid UTF-8. The byte-based
// implementation fails this property in every multi-byte case where the
// byte cap lands inside a rune; the rune-based implementation passes
// unconditionally.
func TestTruncateForLog_ProducesValidUTF8AtBoundary(t *testing.T) {
	// 50 copies of 🌍 (4 bytes each) = 200 bytes / 50 runes.
	in := strings.Repeat("🌍", 50)

	// Sweep maxLen across every value from 1 to 49 runes. Each one
	// must produce a valid UTF-8 string that ends with "..." and has
	// exactly maxLen + 3 runes.
	for max := 1; max < 50; max++ {
		got := truncateForLog(in, max)
		if !utf8.ValidString(got) {
			t.Errorf("maxLen=%d: produced invalid UTF-8: % x", max, got)
		}
		if !strings.HasSuffix(got, "...") {
			t.Errorf("maxLen=%d: missing '...' suffix: %q", max, got)
		}
		gotRunes := utf8.RuneCountInString(got)
		if gotRunes != max+3 {
			t.Errorf("maxLen=%d: got %d runes, want %d", max, gotRunes, max+3)
		}
	}
}
