package help

import (
	"strings"
	"testing"
)

// TestJoinedBounds_TheWaiterCapIsNotPresentedAsAMemoryBound — a reader asking
// "how much memory can one transaction's queued callbacks hold?" must not get
// the waiter cap as the answer. It bounds how many callbacks may queue, not
// how many bytes they hold: the reading is taken without reserving anything, so
// more than the cap may pass it at once, and each callback buffers its whole
// request — up to a fixed 10 MiB that no setting moves — before it queues.
func TestJoinedBounds_TheWaiterCapIsNotPresentedAsAMemoryBound(t *testing.T) {
	for _, rel := range []string{"config/grpc.md", "errors/TOO_MANY_JOINED_REQUESTS.md"} {
		body := readCalloutErrorTopic(t, rel)
		for _, want := range []string{
			"bounds how many callbacks may queue, not how many bytes they hold",
			"10 MiB",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: missing %q", rel, want)
			}
		}
	}
}

// TestJoinedBounds_TheRequestSideCapIsNamedAsFixed — the request side has no
// setting at all, and a reader who is told to raise a limit must not go looking
// for one that does not exist.
func TestJoinedBounds_TheRequestSideCapIsNamedAsFixed(t *testing.T) {
	for _, rel := range []string{"config/grpc.md", "errors/JOINED_RESPONSE_TOO_LARGE.md"} {
		body := readCalloutErrorTopic(t, rel)
		if !strings.Contains(body, "fixed 10 MiB, which no setting moves") {
			t.Errorf("%s: the request-body cap must be named as fixed and settingless", rel)
		}
	}
}

// TestJoinedBounds_OneRuleForNamingTheConfiguredFigure — the 413 names its
// ceiling to the caller and the 503 withholds its cap, and that is one rule,
// not two habits: a refusal names a figure the caller can work within, and
// withholds one that only reads how loaded the node is. Both topics say so, so
// a reader of either knows what to expect of the other.
func TestJoinedBounds_OneRuleForNamingTheConfiguredFigure(t *testing.T) {
	const rule = "a refusal names the figure the caller can work within, and withholds one that only reads how loaded the node is"
	tooLarge := readCalloutErrorTopic(t, "errors/JOINED_RESPONSE_TOO_LARGE.md")
	tooMany := readCalloutErrorTopic(t, "errors/TOO_MANY_JOINED_REQUESTS.md")

	if !strings.Contains(tooLarge, "The message carries the configured figure") {
		t.Error("JOINED_RESPONSE_TOO_LARGE: the message names the ceiling the caller must page under — say so")
	}
	if !strings.Contains(tooMany, "The message does not carry the figure") {
		t.Error("TOO_MANY_JOINED_REQUESTS: the message withholds the cap — say so")
	}
	for rel, body := range map[string]string{
		"errors/JOINED_RESPONSE_TOO_LARGE.md": tooLarge,
		"errors/TOO_MANY_JOINED_REQUESTS.md":  tooMany,
	} {
		if !strings.Contains(body, rule) {
			t.Errorf("%s: missing the rule both refusals follow", rel)
		}
	}
}
