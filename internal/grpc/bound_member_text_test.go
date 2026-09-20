package grpc

import (
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// A 600-rune message with a multi-byte rune sitting exactly astride the
// bound: 511 ASCII 'a's, then 'é' (2 bytes) as the 512th rune, then 88 more
// 'a's. A byte-based cut at 512 bytes would land inside 'é' (its second
// byte); a rune-safe cut must not.
func straddlingMessage() (full string, want string) {
	full = strings.Repeat("a", 511) + "é" + strings.Repeat("a", 88)
	want = strings.Repeat("a", 511) + "é" + "…"
	return full, want
}

func TestBoundMemberText_RuneSafeAtTheBoundary(t *testing.T) {
	full, want := straddlingMessage()
	if n := utf8.RuneCountInString(full); n != 600 {
		t.Fatalf("test setup: full has %d runes, want 600", n)
	}

	got := boundMemberText(full)

	if !utf8.ValidString(got) {
		t.Fatalf("result is not valid UTF-8: %q", got)
	}
	if got != want {
		t.Errorf("boundMemberText = %q, want %q", got, want)
	}
	if n := utf8.RuneCountInString(strings.TrimSuffix(got, "…")); n != maxMemberMessageRunes {
		t.Errorf("kept %d runes before the ellipsis, want %d", n, maxMemberMessageRunes)
	}
}

func TestBoundMemberText_ShortMessageUnchanged(t *testing.T) {
	short := "card declined"
	if got := boundMemberText(short); got != short {
		t.Errorf("boundMemberText(%q) = %q, want unchanged", short, got)
	}
}

// The compute member's own failure message is bounded exactly where it
// becomes client text: the MemberFailed builder in dispatchCalloutToMember.
// Unbounded, it flows straight into a 400 body and, once several tries are
// exhausted, into the concatenated CALLOUT_FAILED list.
func TestTryKind_MemberAnsweredFailure_MessageIsBounded(t *testing.T) {
	full, want := straddlingMessage()
	d, registry, memberID, sentCh := setupTestDispatcher(t)
	member := registry.Get(memberID)
	go func() {
		<-sentCh
		member.CompleteRequest("r1", &ProcessingResponse{Success: false, Error: full})
	}()
	failure, ctxErr := kindOf(d, testContext(), member, rawCall(5*time.Second))
	if ctxErr != nil {
		t.Fatalf("unexpected ctx error: %v", ctxErr)
	}
	if failure == nil || failure.Kind != contract.MemberFailed {
		t.Fatalf("failure = %+v, want MemberFailed", failure)
	}
	if failure.Message != want || failure.Error() != want {
		t.Errorf("Message/Error = %q/%q, want %q", failure.Message, failure.Error(), want)
	}
	var appErr *common.AppError
	if errors.As(failure, &appErr) {
		t.Errorf("MemberFailed must carry no AppError, got %v", appErr)
	}
}
