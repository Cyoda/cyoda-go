package help

import (
	"strings"
	"testing"
)

// readCalloutErrorTopic reads a help topic by its content-relative path. The
// package already has a readTopic(t, root, name) helper (condition_docs_parity_test.go);
// this one fixes the root to repoRoot(t) so the tests below read as the brief
// wrote them.
func readCalloutErrorTopic(t *testing.T, rel string) string {
	t.Helper()
	return readTopic(t, repoRoot(t), rel)
}

func TestCalloutErrorTopics_SayWhatRetryableSpeaksFor(t *testing.T) {
	for _, rel := range []string{
		"errors/DISPATCH_TIMEOUT.md", "errors/COMPUTE_MEMBER_DISCONNECTED.md", "errors/WORKFLOW_FAILED.md",
		"errors/DISPATCH_FORWARD_FAILED.md", "errors/CALLOUT_FAILED.md",
	} {
		body := readCalloutErrorTopic(t, rel)
		for _, want := range []string{"speaks for Cyoda's state only", "the application's to reconcile"} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: missing %q", rel, want)
			}
		}
	}
}

func TestCalloutErrorTopics_StatementsThatBecameFalseAreGone(t *testing.T) {
	gone := map[string][]string{
		"errors.md":                             {"trailer metadata"},
		"errors/WORKFLOW_FAILED.md":             {"Retryable: `no`"},
		"errors/DISPATCH_FORWARD_FAILED.md":     {"The operation has not been executed on the target node"},
		"errors/COMPUTE_MEMBER_DISCONNECTED.md": {"The cluster re-routes to an available member"},
		"errors/DISPATCH_TIMEOUT.md":            {"default `30000` ms", "govern cross-node forwarding between cluster nodes, not this timeout"},
		"errors/NO_COMPUTE_MEMBER_FOR_TAG.md":   {"no live cluster node advertising"},
	}
	for rel, phrases := range gone {
		body := readCalloutErrorTopic(t, rel)
		for _, phrase := range phrases {
			if strings.Contains(body, phrase) {
				t.Errorf("%s still says %q", rel, phrase)
			}
		}
	}
}

func TestErrorsIndex_WorkflowFailedIsConditionallyRetryable(t *testing.T) {
	index := readCalloutErrorTopic(t, "errors.md")
	if !strings.Contains(index, "- `errors.WORKFLOW_FAILED` — `400` — retryable only when the compute member said so") {
		t.Error("errors.md: the WORKFLOW_FAILED row must say when it is retryable")
	}
	if !strings.Contains(index, `"code": "CLIENT_ERROR"`) {
		t.Error("errors.md: the gRPC example must show the envelope as it is — code CLIENT_ERROR, the error code as the message's prefix")
	}
}
