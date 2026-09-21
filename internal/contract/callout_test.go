package contract_test

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// The engine wraps a processor's error as "processor <name> failed: %w"; the
// classifier must still find both the failure and the AppError it carries.
func TestCalloutFailure_ErrorsAsFindsFailureAndItsAppError(t *testing.T) {
	appErr := common.Operational(http.StatusServiceUnavailable, common.ErrCodeDispatchTimeout,
		"processor dispatch timed out after 50ms: no response").AsRetryable()
	failure := &contract.CalloutFailure{Kind: contract.NoAnswer, Code: appErr.Code, Message: appErr.Message, Err: appErr}
	wrapped := fmt.Errorf("processor %s failed: %w", "charge", failure)

	var gotFailure *contract.CalloutFailure
	if !errors.As(wrapped, &gotFailure) {
		t.Fatalf("errors.As did not find *CalloutFailure in %v", wrapped)
	}
	if gotFailure.Kind != contract.NoAnswer {
		t.Errorf("Kind = %v, want NoAnswer", gotFailure.Kind)
	}
	var gotApp *common.AppError
	if !errors.As(wrapped, &gotApp) {
		t.Fatalf("errors.As did not find the carried *AppError in %v", wrapped)
	}
	if gotApp != appErr {
		t.Errorf("found a different *AppError than the one carried")
	}
	if got, want := failure.Error(), appErr.Error(); got != want {
		t.Errorf("Error() = %q, want the carried error's text %q", got, want)
	}
}

func TestCalloutFailure_MemberFailedCarriesNoAppError(t *testing.T) {
	yes := true
	failure := &contract.CalloutFailure{Kind: contract.MemberFailed, Message: "card declined", Retryable: &yes}
	wrapped := fmt.Errorf("processor %s failed: %w", "charge", failure)

	var gotApp *common.AppError
	if errors.As(wrapped, &gotApp) {
		t.Fatalf("a MemberFailed failure must not carry an *AppError, found %v", gotApp)
	}
	if got, want := wrapped.Error(), "processor charge failed: card declined"; got != want {
		t.Errorf("message = %q, want %q", got, want)
	}
	if failure.Unwrap() != nil {
		t.Errorf("Unwrap() = %v, want nil", failure.Unwrap())
	}
}

func TestCalloutFailure_SentinelsSurvive(t *testing.T) {
	for _, sentinel := range []error{contract.ErrNoMatchingMember, contract.ErrAuthContextUnavailable} {
		failure := &contract.CalloutFailure{Kind: contract.NoHandOff, Err: fmt.Errorf("%w: tags %q", sentinel, "python")}
		if !errors.Is(fmt.Errorf("outer: %w", failure), sentinel) {
			t.Errorf("errors.Is lost %v through the failure", sentinel)
		}
	}
}

func TestCalloutFailureKind_MayTryAnother(t *testing.T) {
	tests := []struct {
		kind             contract.CalloutFailureKind
		repeatSafe, want bool
	}{
		{contract.NoHandOff, true, true},
		{contract.NoHandOff, false, true},
		{contract.NoAnswer, true, true},
		{contract.NoAnswer, false, false},
		{contract.MemberFailed, true, false},
		{contract.MemberFailed, false, false},
		{contract.Terminal, true, false},
		{contract.Terminal, false, false},
	}
	for _, tt := range tests {
		if got := tt.kind.MayTryAnother(tt.repeatSafe); got != tt.want {
			t.Errorf("%v.MayTryAnother(%v) = %v, want %v", tt.kind, tt.repeatSafe, got, tt.want)
		}
	}
}

func TestCalloutFailureKind_String(t *testing.T) {
	want := map[contract.CalloutFailureKind]string{
		contract.NoHandOff:    "no_handoff",
		contract.NoAnswer:     "no_answer",
		contract.MemberFailed: "member_failed",
		contract.Terminal:     "terminal",
	}
	for kind, s := range want {
		if kind.String() != s {
			t.Errorf("String() = %q, want %q", kind.String(), s)
		}
	}
}
