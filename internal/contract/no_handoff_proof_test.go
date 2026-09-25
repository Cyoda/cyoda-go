package contract_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// The proof is read through every wrap the engine adds, and through a join.
func TestNoHandOffProof_FoundThroughWraps(t *testing.T) {
	proof := &contract.NoHandOffProof{Err: errors.New("no compute member")}
	for name, err := range map[string]error{
		"bare":    proof,
		"wrapped": fmt.Errorf("processor %s failed: %w", "charge", proof),
		"joined":  errors.Join(errors.New("other"), proof),
	} {
		if !contract.ProvesNoHandOff(err) {
			t.Errorf("%s: ProvesNoHandOff = false, want true", name)
		}
	}
}

func TestNoHandOffProof_AbsentIsNotProof(t *testing.T) {
	failure := &contract.CalloutFailure{Kind: contract.NoHandOff, Message: "no compute member"}
	for name, err := range map[string]error{
		"nil":                           nil,
		"plain error":                   errors.New("boom"),
		"a NoHandOff failure, unmarked": failure,
	} {
		if contract.ProvesNoHandOff(err) {
			t.Errorf("%s: ProvesNoHandOff = true, want false", name)
		}
	}
}

// Everything a caller reads behind the proof is what it read before: the text,
// the failure, its AppError, the sentinels, and a cancellation.
func TestNoHandOffProof_ChangesNothingACallerReads(t *testing.T) {
	appErr := common.Operational(http.StatusServiceUnavailable, common.ErrCodeNoComputeMemberForTag, "no compute member").AsRetryable()
	failure := &contract.CalloutFailure{Kind: contract.NoHandOff, Code: appErr.Code, Message: appErr.Message,
		Err: fmt.Errorf("%w: %w", contract.ErrNoMatchingMember, appErr)}
	proved := fmt.Errorf("processor charge failed: %w", &contract.NoHandOffProof{Err: failure})

	if got, want := proved.Error(), fmt.Errorf("processor charge failed: %w", failure).Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	var gotFailure *contract.CalloutFailure
	if !errors.As(proved, &gotFailure) || gotFailure != failure {
		t.Errorf("errors.As did not find the wrapped *CalloutFailure")
	}
	var gotApp *common.AppError
	if !errors.As(proved, &gotApp) || gotApp != appErr {
		t.Errorf("errors.As did not find the carried *AppError")
	}
	if !errors.Is(proved, contract.ErrNoMatchingMember) {
		t.Errorf("errors.Is lost ErrNoMatchingMember behind the proof")
	}

	gone := &contract.NoHandOffProof{Err: common.ClientGone(context.Canceled)}
	if !errors.Is(gone, context.Canceled) || !errors.Is(gone, common.ErrClientGone) {
		t.Errorf("a cancellation behind the proof is no longer context.Canceled marked as the caller's departure: %v", gone)
	}
}
