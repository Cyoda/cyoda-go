package callout

import (
	"errors"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

func TestVerdict_MemberTextSurvivesEarlierNoAnswer(t *testing.T) {
	p := &progress{
		triesLeft: 2,
		attempts:  []contract.CalloutAttempt{{MemberID: "m-1", Kind: contract.NoAnswer, Cause: "DISPATCH_TIMEOUT: no response"}},
		lastTried: &contract.CalloutFailure{Kind: contract.MemberFailed, Message: "card declined", MemberText: "card declined"},
	}
	done, err := p.verdict(true)
	var failure *contract.CalloutFailure
	if !done || !errors.As(err, &failure) || failure.MemberText != "card declined" || len(failure.Attempts) != 1 {
		t.Fatalf("done=%v failure=%+v, want the member's text kept beside the earlier attempt", done, failure)
	}
}
