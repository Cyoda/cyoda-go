package callout

import (
	"errors"
	"net/http"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

const timeoutCause = "DISPATCH_TIMEOUT: processor dispatch timed out after 100ms: no response"

func timedOut(memberID string) (contract.CalloutAttempt, *contract.CalloutFailure) {
	appErr := common.Operational(http.StatusServiceUnavailable, common.ErrCodeDispatchTimeout,
		"processor dispatch timed out after 100ms: no response").AsRetryable()
	failure := &contract.CalloutFailure{Kind: contract.NoAnswer, Code: appErr.Code, Message: appErr.Message, Err: appErr}
	return contract.CalloutAttempt{MemberID: memberID, Kind: contract.NoAnswer, Cause: failure.Message}, failure
}

func TestAttemptsMessage(t *testing.T) {
	gone := "COMPUTE_MEMBER_DISCONNECTED: compute member disconnected during processor dispatch"
	lost := "DISPATCH_FORWARD_FAILED: forwarding the callout to a peer node failed"
	tests := []struct {
		name     string
		attempts []contract.CalloutAttempt
		want     string
	}{
		{
			name: "two different entries",
			attempts: []contract.CalloutAttempt{
				{MemberID: "m-1", Kind: contract.NoAnswer, Cause: timeoutCause},
				{MemberID: "m-2", Kind: contract.NoAnswer, Cause: gone},
			},
			want: "the callout could not be completed, got 2 failures: [member<m-1>: " + timeoutCause + "], [member<m-2>: " + gone + "]",
		},
		{
			name: "identical entries collapse, the count is taken before collapsing, first occurrence keeps its place",
			attempts: []contract.CalloutAttempt{
				{MemberID: "m-1", Kind: contract.NoAnswer, Cause: timeoutCause},
				{MemberID: "m-2", Kind: contract.NoAnswer, Cause: gone},
				{MemberID: "m-1", Kind: contract.NoAnswer, Cause: timeoutCause},
				{MemberID: "m-1", Kind: contract.NoAnswer, Cause: timeoutCause},
			},
			want: "the callout could not be completed, got 4 failures: [member<m-1>: " + timeoutCause + " (3 times)], [member<m-2>: " + gone + "]",
		},
		{
			name: "the same member with a different cause is a different entry",
			attempts: []contract.CalloutAttempt{
				{MemberID: "m-1", Kind: contract.NoAnswer, Cause: timeoutCause},
				{MemberID: "m-1", Kind: contract.NoHandOff, Cause: gone},
			},
			want: "the callout could not be completed, got 2 failures: [member<m-1>: " + timeoutCause + "], [member<m-1>: " + gone + "]",
		},
		{
			name: "a lost hand-over answer has no member",
			attempts: []contract.CalloutAttempt{
				{MemberID: "m-1", Kind: contract.NoAnswer, Cause: timeoutCause},
				{MemberID: "-", Kind: contract.NoAnswer, Cause: lost},
				{MemberID: "-", Kind: contract.NoAnswer, Cause: lost},
			},
			want: "the callout could not be completed, got 3 failures: [member<m-1>: " + timeoutCause + "], [member<->: " + lost + " (2 times)]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := attemptsMessage(tt.attempts); got != tt.want {
				t.Errorf("attemptsMessage\n got %s\nwant %s", got, tt.want)
			}
		})
	}
}

func TestAttemptsFailure_MoreThanOneAttempt_IsCalloutFailed(t *testing.T) {
	a1, _ := timedOut("m-1")
	a2, last := timedOut("m-2")
	attempts := []contract.CalloutAttempt{a1, a2}

	failure := attemptsFailure(attempts, last)

	var appErr *common.AppError
	if !errors.As(failure, &appErr) {
		t.Fatalf("no *AppError behind %v", failure)
	}
	if appErr.Status != http.StatusServiceUnavailable || appErr.Code != common.ErrCodeCalloutFailed || !appErr.Retryable {
		t.Errorf("got %d %s retryable=%v, want a retryable 503 CALLOUT_FAILED", appErr.Status, appErr.Code, appErr.Retryable)
	}
	if want := common.ErrCodeCalloutFailed + ": " + attemptsMessage(attempts); appErr.Message != want {
		t.Errorf("message = %q, want %q", appErr.Message, want)
	}
	if failure.Kind != contract.NoAnswer || failure.Code != common.ErrCodeCalloutFailed || len(failure.Attempts) != 2 {
		t.Errorf("failure = %+v, want the last try's kind, the new code and both attempts", failure)
	}
}

func TestAttemptsFailure_ExactlyOneAttempt_IsThatAttemptsOwnError(t *testing.T) {
	attempt, last := timedOut("m-1")

	failure := attemptsFailure([]contract.CalloutAttempt{attempt}, last)

	var appErr *common.AppError
	if !errors.As(failure, &appErr) || appErr.Code != common.ErrCodeDispatchTimeout {
		t.Fatalf("got %v, want the attempt's own DISPATCH_TIMEOUT, not wrapped", failure)
	}
	if appErr.Message != timeoutCause {
		t.Errorf("message = %q, want it unchanged: %q", appErr.Message, timeoutCause)
	}
	if len(failure.Attempts) != 1 || failure.Attempts[0].MemberID != "m-1" {
		t.Errorf("attempts = %+v, want the one attempt on the returned failure", failure.Attempts)
	}
	if last.Attempts != nil {
		t.Error("the try's own failure value must not be written to")
	}
}
