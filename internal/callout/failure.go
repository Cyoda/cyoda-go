package callout

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// attemptsFailure is the error for a callout that recorded attempts and can do
// nothing more: every try is used, or the patience or the callout's deadline
// ran out with tries still left.
//
// Exactly one attempt is reported as itself — last is that attempt's failure,
// returned with its own code and not wrapped. More than one becomes
// CALLOUT_FAILED, a retryable 503 that lists them.
func attemptsFailure(attempts []contract.CalloutAttempt, last *contract.CalloutFailure) *contract.CalloutFailure {
	if len(attempts) == 1 {
		return withAttempts(last, attempts)
	}
	appErr := common.Operational(http.StatusServiceUnavailable, common.ErrCodeCalloutFailed, attemptsMessage(attempts)).AsRetryable()
	return &contract.CalloutFailure{
		Kind:     last.Kind,
		Code:     appErr.Code,
		Message:  appErr.Message,
		Attempts: attempts,
		Err:      appErr,
	}
}

// withAttempts returns a copy of failure that carries the callout's attempts.
func withAttempts(failure *contract.CalloutFailure, attempts []contract.CalloutAttempt) *contract.CalloutFailure {
	out := *failure
	out.Attempts = attempts
	return &out
}

// attemptsMessage renders the attempts the way Cyoda Cloud renders an
// exhausted retry: the count is taken before identical entries are collapsed,
// an entry that occurs k > 1 times is written once with "(k times)", and
// entries keep the order in which they first occurred. A member id of "-" is a
// hand-over whose answer was lost.
//
// Member ids identify the tenant's own connections and are already known to
// the cnode from its greet. Node ids and peer addresses never appear: an
// attempt's Cause is client-safe text by contract.
func attemptsMessage(attempts []contract.CalloutAttempt) string {
	counts := make(map[string]int, len(attempts))
	var order []string
	for _, a := range attempts {
		entry := fmt.Sprintf("member<%s>: %s", a.MemberID, a.Cause)
		if counts[entry] == 0 {
			order = append(order, entry)
		}
		counts[entry]++
	}
	parts := make([]string, 0, len(order))
	for _, entry := range order {
		if n := counts[entry]; n > 1 {
			entry = fmt.Sprintf("%s (%d times)", entry, n)
		}
		parts = append(parts, "["+entry+"]")
	}
	return fmt.Sprintf("the callout could not be completed, got %d failures: %s", len(attempts), strings.Join(parts, ", "))
}
