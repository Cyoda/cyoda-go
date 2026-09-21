package grpc

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// callout_answer_shape_test.go pins how a LOCAL compute member's answer is
// read: an answer that cannot be read is Terminal (spec §3's "response payload
// unmarshal" row), and every piece of the member's own free text is bounded
// where it becomes this node's text.

// answerCriterion is the criterion literal the tests here dispatch.
const answerCriterion = `{"name":"my-criteria","config":{"calculationNodesTags":"python","responseTimeoutMs":5000}}`

// replyOnce answers the one request the member is sent with resp.
func replyOnce(t *testing.T, registry *MemberRegistry, memberID string, sentCh chan *cepb.CloudEvent, resp *ProcessingResponse) {
	t.Helper()
	go func() {
		ce := <-sentCh
		reqID, err := extractRequestID(ce)
		if err != nil {
			t.Errorf("extractRequestID: %v", err)
			return
		}
		registry.Get(memberID).CompleteRequest(reqID, resp)
	}()
}

// A criterion answer with no `matches` is not a verdict: reading it as "does
// not match" would invent an answer that decides a transition. It is an
// unreadable answer — Terminal, with the fixed client-safe message, and none of
// the member's own text.
func TestDispatchCriteria_MissingMatchesIsUnreadable(t *testing.T) {
	dispatcher, registry, memberID, sentCh := setupTestDispatcher(t)
	replyOnce(t, registry, memberID, sentCh, &ProcessingResponse{Success: true, Reason: "no verdict here"})

	matches, reason, err := dispatchCriteria(dispatcher, testContext(), testEntity(),
		json.RawMessage(answerCriterion), "transition", "wf1", "t1", "", "tx-1")
	if err == nil {
		t.Fatalf("a criterion answer with no matches was accepted as matches=%t reason=%q", matches, reason)
	}
	var failure *contract.CalloutFailure
	if !errors.As(err, &failure) {
		t.Fatalf("error is not a *contract.CalloutFailure: %v", err)
	}
	if failure.Kind != contract.Terminal {
		t.Errorf("kind = %s; want %s: another compute member would answer no better", failure.Kind, contract.Terminal)
	}
	const want = "the compute member's response could not be read"
	if failure.Message != want {
		t.Errorf("message = %q; want %q", failure.Message, want)
	}
	if strings.Contains(failure.Message, "no verdict here") {
		t.Errorf("message = %q; the member's own text must not reach the client here", failure.Message)
	}
}

// A criterion's reason is the member's own free text and reaches a 400 body and
// the audit trail, so it is bounded where the member speaks it — the same bound
// a MemberFailed message and a warning get.
func TestDispatchCriteria_ReasonIsBounded(t *testing.T) {
	dispatcher, registry, memberID, sentCh := setupTestDispatcher(t)
	matchesFalse := false
	long := strings.Repeat("é", maxMemberMessageRunes+50)
	replyOnce(t, registry, memberID, sentCh, &ProcessingResponse{Success: true, Matches: &matchesFalse, Reason: long})

	_, reason, err := dispatchCriteria(dispatcher, testContext(), testEntity(),
		json.RawMessage(answerCriterion), "transition", "wf1", "t1", "", "tx-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := utf8.RuneCountInString(reason); n != maxMemberMessageRunes+1 {
		t.Errorf("reason is %d runes; want %d (%d kept plus the ellipsis)", n, maxMemberMessageRunes+1, maxMemberMessageRunes)
	}
	if !strings.HasSuffix(reason, "…") {
		t.Error("reason does not end in an ellipsis, so a reader cannot tell a shortened reason from a short one")
	}
}

// A function's resultKind is the member's own text too: the engine refuses one
// it does not know and names it in the refusal, so the member must not decide
// how long that text is.
func TestDispatchFunction_ResultKindIsBounded(t *testing.T) {
	dispatcher, registry, memberID, sentCh := setupTestDispatcher(t)
	long := strings.Repeat("ß", maxMemberMessageRunes+50)
	replyOnce(t, registry, memberID, sentCh, &ProcessingResponse{
		Success: true, ResultKind: long, Result: json.RawMessage(`{"fireAfterMs":1}`)})

	res, err := dispatchFunction(dispatcher, testContext(), testEntity(), spi.ScheduleFunction{
		Name:                 "my-schedule-fn",
		ResultKind:           "Schedule",
		CalculationNodesTags: "python",
		ResponseTimeoutMs:    5000,
	}, "wf1", "t1", "tx-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := utf8.RuneCountInString(res.Kind); n != maxMemberMessageRunes+1 {
		t.Errorf("resultKind is %d runes; want %d (%d kept plus the ellipsis)", n, maxMemberMessageRunes+1, maxMemberMessageRunes)
	}
}
