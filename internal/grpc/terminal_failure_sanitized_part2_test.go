package grpc

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// A criterion FUNCTION definition that does not parse is a broken workflow
// configuration — authored ahead of time, not this node's fault and not the
// compute member's (no member is ever contacted; NewCriteriaCallout fails
// before a Callout even exists). Per spec §8.2's Terminal row ("500 ticketed
// for auth-context; 400 WORKFLOW_FAILED otherwise") it is not the
// auth-context special case, so it must surface as the "otherwise" row: a
// domain 400 WORKFLOW_FAILED — but with a fixed, sanitized message, never the
// raw parse error, which can quote a byte of the criterion's own JSON.
func TestNewCriteriaCallout_InvalidJSON_NoMarkerLeak(t *testing.T) {
	const marker = "SECRET-MARKER-123"
	criterion := json.RawMessage(marker + " this is not valid json")

	_, failure := NewCriteriaCallout(testTenantID, testEntity(), criterion, "transition", "wf1", "t1", "", "tx-1")

	if failure == nil || failure.Kind != contract.Terminal {
		t.Fatalf("failure = %+v, want Terminal", failure)
	}
	const wantMsg = "the workflow's criterion function could not be parsed"
	if failure.Message != wantMsg || failure.Error() != wantMsg {
		t.Errorf("Message/Error = %q/%q, want %q", failure.Message, failure.Error(), wantMsg)
	}
	if strings.Contains(failure.Error(), marker) {
		t.Errorf("leaked the marker: %q", failure.Error())
	}
}

// ResolveAnswerLimit's failure text is built from configuration integers, not
// from an underlying error's own text — but it must still not be constructed
// by handing a raw error's .Error() to the client: the field must carry only
// the deliberately authored, safe message this function composes itself.
func TestResolveAnswerLimit_TooLarge_NoRawError(t *testing.T) {
	d, _, _, _ := setupTestDispatcher(t)

	_, failure := d.ResolveAnswerLimit(d.answerLimitMax.Milliseconds() + 1)

	if failure == nil || failure.Kind != contract.Terminal {
		t.Fatalf("failure = %+v, want Terminal", failure)
	}
	if failure.Err != nil {
		t.Errorf("Err = %v, want nil: nothing here needs a wrapped cause beyond the message itself", failure.Err)
	}
	if !strings.Contains(failure.Message, "exceeds the upper bound") {
		t.Errorf("Message = %q, want the bound explanation", failure.Message)
	}
}
