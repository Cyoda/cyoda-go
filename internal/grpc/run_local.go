package grpc

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// TryNumberer gives the fencing number for the next try. RunLocal calls it
// once before each try, before it mints that try's pass.
//
// On the owner, Next raises the callout's major — which shuts the earlier
// cnode out and waits for its write in progress — and returns (major, 0). On a
// pnode that received a hand-over it counts minor = 1, 2, … under the major the
// hand-over carried. That is what lets one RunLocal make several tries while
// the number still rises before each.
type TryNumberer interface {
	Next() (major, minor uint32)
}

// MinorNumberer numbers the tries of a pnode that received a hand-over: (major,
// 1), (major, 2), … It touches no fence — the arbiter is the owner's. RunLocal
// calls it from one goroutine; it is not safe for concurrent use.
type MinorNumberer struct {
	major, minor uint32
}

func NewMinorNumberer(major uint32) *MinorNumberer { return &MinorNumberer{major: major} }

func (n *MinorNumberer) Next() (uint32, uint32) {
	n.minor++
	return n.major, n.minor
}

// LocalResult is what one run of the local procedure produced.
type LocalResult struct {
	// Result is the cnode's answer; meaningful when OK.
	Result CalloutResult
	// Failure is the last try's failure, or — when no matching cnode was there
	// to try — NoHandOff wrapping ErrNoMatchingMember. Whether another pnode
	// may be asked is Failure.Kind.MayTryAnother(call.RepeatSafe).
	Failure *contract.CalloutFailure
	// CtxErr is the caller's ctx.Err(), unchanged, when its context ended
	// during a try or between tries. Failure is then nil.
	CtxErr error
	// TriesUsed counts every cnode chosen, whether or not the hand-off
	// succeeded, a try in progress when the caller went away included.
	TriesUsed int
	// Attempts is one entry per failed try, in order.
	Attempts []contract.CalloutAttempt
}

// OK reports whether a cnode answered.
func (r LocalResult) OK() bool { return r.Failure == nil && r.CtxErr == nil }

// Err is the run's error: the caller's context error, else the failure, else nil.
func (r LocalResult) Err() error {
	if r.CtxErr != nil {
		return r.CtxErr
	}
	if r.Failure != nil {
		return r.Failure
	}
	return nil
}

// RunLocal tries the callout on this pnode's own matching cnodes, one after
// another, until one answers, a failure that forbids another try occurs, the
// matching cnodes are used up, or maxTries (at least 1) is reached.
//
// There is no pause between cnodes. A cnode is never tried twice within one
// run; the tried set lives in the call and is shared with nothing. The matching
// cnodes are looked up afresh before every try, so one that attaches during the
// run is seen. Every try sends call.RequestID. RunLocal never waits for a cnode
// to appear: when none is left it returns at once, and waiting is the owner's.
func (d *ProcessorDispatcher) RunLocal(ctx context.Context, call Callout, maxTries int) LocalResult {
	var res LocalResult
	var last *contract.CalloutFailure
	// Keyed by the Member, not its id: a cnode that re-registered under the
	// same id is a new connection and may be tried.
	tried := make(map[*Member]struct{})

	for res.TriesUsed < maxTries {
		if err := ctx.Err(); err != nil {
			if calloutDeadlinePassed(ctx) {
				break // no try starts after the callout's deadline
			}
			res.CtxErr = err
			return res
		}
		var untried []*Member
		for _, m := range d.registry.Candidates(call.TenantID, call.Tags) {
			if _, done := tried[m]; !done {
				untried = append(untried, m)
			}
		}
		if len(untried) == 0 {
			break
		}
		member := d.selector.Select(untried)
		tried[member] = struct{}{}
		res.TriesUsed++
		major, minor := call.Number.Next()

		slog.Debug("callout try", "pkg", "grpc", "kind", call.Kind.String(), "name", call.Name,
			"memberId", member.ID, "entityId", call.EntityID, "requestId", call.RequestID,
			"try", res.TriesUsed, "major", major, "minor", minor)

		result, failure, ctxErr := d.dispatchCalloutToMember(ctx, member, call, d.resolveTxToken(ctx, call.TxID, call.RequestID))
		if ctxErr != nil {
			res.CtxErr = ctxErr
			return res
		}
		if failure == nil {
			res.Result = result
			return res
		}
		res.Attempts = append(res.Attempts, contract.CalloutAttempt{MemberID: member.ID, Kind: failure.Kind, Cause: failure.Message})
		last = failure
		if !failure.Kind.MayTryAnother(call.RepeatSafe) {
			break
		}
	}

	if last == nil && calloutDeadlinePassed(ctx) {
		appErr := common.Operational(http.StatusServiceUnavailable, common.ErrCodeDispatchTimeout,
			"the callout deadline passed before a try could start").AsRetryable().WithCause(contract.ErrCalloutDeadline)
		last = appFailure(contract.NoHandOff, appErr)
	}
	if last == nil {
		slog.Debug("no matching calculation member", "pkg", "grpc", "tags", call.Tags, "entityId", call.EntityID)
		last = &contract.CalloutFailure{
			Kind:    contract.NoHandOff,
			Code:    common.ErrCodeNoComputeMemberForTag,
			Message: fmt.Sprintf("%s: no compute member for tags %q", common.ErrCodeNoComputeMemberForTag, call.Tags),
			Err:     fmt.Errorf("%w: tags %q", ErrNoMatchingMember, call.Tags),
		}
	}
	res.Failure = last
	return res
}
