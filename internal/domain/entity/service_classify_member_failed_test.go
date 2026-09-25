package entity

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// A cnode that answered "I failed" is a 400 WORKFLOW_FAILED whose message is the
// engine's wrap plus the cnode's own text, and which is retryable exactly when
// the cnode said so.
func TestClassifyWorkflowError_MemberFailed_CarriesTheVerdict(t *testing.T) {
	yes, no := true, false
	verdicts := []struct {
		name      string
		verdict   *bool
		retryable bool
	}{
		{"verdict true", &yes, true},
		{"verdict false", &no, false},
		{"verdict absent", nil, false},
	}
	wraps := []struct {
		name string
		wrap func(error) error
		want string
	}{
		{"processor", func(err error) error { return fmt.Errorf("processor %s failed: %w", "charge", err) },
			"WORKFLOW_FAILED: processor charge failed: card declined"},
		{"transition criterion", func(err error) error { return fmt.Errorf("failed to evaluate transition criterion: %w", err) },
			"WORKFLOW_FAILED: failed to evaluate transition criterion: card declined"},
		{"workflow criterion", func(err error) error {
			return fmt.Errorf("failed to evaluate workflow criterion for %q: %w", "orders", err)
		}, `WORKFLOW_FAILED: failed to evaluate workflow criterion for "orders": card declined`},
		{"function", func(err error) error { return fmt.Errorf("schedule function %s failed: %w", "calcFire", err) },
			"WORKFLOW_FAILED: schedule function calcFire failed: card declined"},
	}
	for _, v := range verdicts {
		for _, w := range wraps {
			t.Run(v.name+"/"+w.name, func(t *testing.T) {
				failure := &contract.CalloutFailure{Kind: contract.MemberFailed, Message: "card declined", Retryable: v.verdict}
				appErr := classifyWorkflowError(w.wrap(failure))
				if appErr.Status != http.StatusBadRequest || appErr.Code != common.ErrCodeWorkflowFailed {
					t.Fatalf("got %d %s, want 400 WORKFLOW_FAILED", appErr.Status, appErr.Code)
				}
				if appErr.Retryable != v.retryable {
					t.Errorf("Retryable = %v, want %v", appErr.Retryable, v.retryable)
				}
				if appErr.Message != w.want {
					t.Errorf("Message = %q, want %q", appErr.Message, w.want)
				}
			})
		}
	}
}

// Every other kind already carries its classified error and passes through
// unchanged — code, status and retryable flag are the try's own.
func TestClassifyWorkflowError_OtherCalloutFailureKindsPassThrough(t *testing.T) {
	timeout := common.Operational(http.StatusServiceUnavailable, common.ErrCodeDispatchTimeout, "processor dispatch timed out after 100ms: no response").AsRetryable()
	failure := &contract.CalloutFailure{Kind: contract.NoAnswer, Code: timeout.Code, Message: timeout.Message, Err: timeout}

	got := classifyWorkflowError(fmt.Errorf("processor %s failed: %w", "charge", failure))
	if got != timeout {
		t.Fatalf("got %+v, want the try's own *AppError unchanged", got)
	}
}

// A Terminal failure with no code of its own stays what it was: a
// non-retryable 400 WORKFLOW_FAILED.
func TestClassifyWorkflowError_TerminalWithoutCodeStays400NotRetryable(t *testing.T) {
	cause := fmt.Errorf("failed to unmarshal processor response payload: unexpected end of JSON input")
	failure := &contract.CalloutFailure{Kind: contract.Terminal, Message: cause.Error(), Err: cause}

	got := classifyWorkflowError(fmt.Errorf("processor %s failed: %w", "charge", failure))
	if got.Status != http.StatusBadRequest || got.Code != common.ErrCodeWorkflowFailed || got.Retryable {
		t.Fatalf("got %d %s retryable=%v, want a non-retryable 400 WORKFLOW_FAILED", got.Status, got.Code, got.Retryable)
	}
}

// The owner's callout loop marks some failures with contract.NoHandOffProof.
// The mark is for the scheduler alone: the client sees exactly what it saw
// without it — status, code, message and retryable flag — for every kind of
// callout failure and for the caller's own departure.
func TestClassifyWorkflowError_NoHandOffProofChangesNothing(t *testing.T) {
	yes := true
	noCnode := common.Operational(http.StatusServiceUnavailable, common.ErrCodeNoComputeMemberForTag, "no compute member").AsRetryable()
	cases := map[string]error{
		"no compute member": &contract.CalloutFailure{Kind: contract.NoHandOff, Code: noCnode.Code, Message: noCnode.Message,
			Err: fmt.Errorf("%w: %w", contract.ErrNoMatchingMember, noCnode)},
		"member failed":           &contract.CalloutFailure{Kind: contract.MemberFailed, Message: "card declined", Retryable: &yes},
		"terminal without a code": &contract.CalloutFailure{Kind: contract.Terminal, Message: "the workflow's criterion function could not be parsed"},
		"auth context unavailable": &contract.CalloutFailure{Kind: contract.Terminal, Message: "auth context unavailable for dispatch",
			Err: fmt.Errorf("failed to attach auth context: %w", contract.ErrAuthContextUnavailable)},
		"caller gone": common.ClientGone(context.Canceled),
	}
	for name, err := range cases {
		t.Run(name, func(t *testing.T) {
			wrap := func(e error) error { return fmt.Errorf("processor %s failed: %w", "charge", e) }
			plain := classifyWorkflowError(wrap(err))
			proved := classifyWorkflowError(wrap(&contract.NoHandOffProof{Err: err}))
			if plain.Status != proved.Status || plain.Code != proved.Code || plain.Message != proved.Message || plain.Retryable != proved.Retryable {
				t.Errorf("with the proof: %d %s %q retryable=%v; without: %d %s %q retryable=%v",
					proved.Status, proved.Code, proved.Message, proved.Retryable,
					plain.Status, plain.Code, plain.Message, plain.Retryable)
			}
		})
	}
}
