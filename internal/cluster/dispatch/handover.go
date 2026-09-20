package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

// forwardFailedClientMessage is the sanitized, client-facing message for a
// hand-over whose answer was lost. The transport error behind it embeds the
// peer's address, port and route — that detail must never reach the client; it
// is logged where the hand-over is made.
const forwardFailedClientMessage = "forwarding the callout to a peer node failed"

// peerFailedClientMessage stands in where the answering pnode gave no
// client-safe text of its own.
const peerFailedClientMessage = "peer node dispatch failed"

// HandOverAnswer is what the owner learns from one hand-over.
type HandOverAnswer struct {
	// Connected is false when the connection to the peer could not be opened
	// (a dial error, the connect timeout included), when the hand-over was
	// refused before connecting, or when the peer sent an authenticated
	// no_handoff having tried no cnode: nothing was handed to a cnode and no
	// try is used.
	Connected bool
	// Result is set when the outcome is ok.
	Result *internalgrpc.CalloutResult
	// Failure is nil when the outcome is ok. Its Kind is NoHandOff, NoAnswer,
	// MemberFailed or Terminal. A lost answer is NoAnswer with code
	// DISPATCH_FORWARD_FAILED and the sanitised message.
	Failure *contract.CalloutFailure
	// TriesUsed is 0 when Connected is false, and for a Terminal refusal made
	// before any try; otherwise 1..triesLeft. A lost or out-of-range answer
	// counts as exactly 1.
	TriesUsed int
	Attempts  []contract.CalloutAttempt
	Warnings  []string

	// peerErrors are the answering pnode's error diagnostics; HandOver adds
	// them to the owner's request diagnostics.
	peerErrors []string
}

// newHandOverRequest puts call on the wire for a peer that may make triesLeft
// tries under fencing number major. An error means the callout cannot be handed
// over at all — to any peer.
func newHandOverRequest(uc *spi.UserContext, ownerNodeID string, call internalgrpc.Callout, triesLeft int, major uint32) (DispatchCalloutRequest, error) {
	src := call.Source
	switch {
	case uc == nil:
		return DispatchCalloutRequest{}, errors.New("no user context")
	case src.Entity == nil:
		return DispatchCalloutRequest{}, errors.New("callout has no source")
	case call.RequestID == "":
		return DispatchCalloutRequest{}, errors.New("callout has no request id")
	case call.AnswerLimit < time.Millisecond:
		return DispatchCalloutRequest{}, fmt.Errorf("answer limit %s is below one millisecond", call.AnswerLimit)
	case triesLeft < 1:
		return DispatchCalloutRequest{}, fmt.Errorf("tries left is %d", triesLeft)
	case major < 1:
		return DispatchCalloutRequest{}, errors.New("hand-over has no fencing number")
	}
	req := DispatchCalloutRequest{
		Kind:           call.Kind.String(),
		Entity:         json.RawMessage(src.Entity.Data),
		EntityMeta:     src.Entity.Meta,
		WorkflowName:   src.WorkflowName,
		TransitionName: src.TransitionName,
		TxID:           call.TxID,
		TenantID:       string(call.TenantID),
		Tags:           call.Tags,
		UserID:         uc.UserID,
		PrincipalKind:  uc.Kind,
		Roles:          uc.Roles,
		RequestID:      call.RequestID,
		TriesLeft:      triesLeft,
		AnswerLimitMs:  call.AnswerLimit.Milliseconds(),
		OwnerNodeID:    ownerNodeID,
		Major:          major,
		RepeatSafe:     call.RepeatSafe,
		Processor:      src.Processor,
		Criterion:      src.Criterion,
		Target:         src.Target,
		ProcessorName:  src.ProcessorName,
		Function:       src.Function,
	}
	for _, p := range call.Outer {
		req.Outer = append(req.Outer, WirePair{Callout: p.Callout, Major: p.Major, Minor: p.Minor})
	}
	if err := req.validate(); err != nil {
		return DispatchCalloutRequest{}, err
	}
	return req, nil
}

// validate is the receiving pnode's check of a hand-over it has authenticated.
//
// A request carries two tenants: TenantID, which becomes the UserContext the
// callout runs as, and EntityMeta.TenantID, the entity's own. They must agree,
// or the callout runs as one tenant over another's entity. The equality is
// unconditional, an absent EntityMeta.TenantID included: every callout is built
// from a live stored entity whose tenant is always set, so an empty one can
// only come from a hand-crafted body. The error names neither value: both are
// peer-supplied.
func (req *DispatchCalloutRequest) validate() error {
	switch {
	case string(req.EntityMeta.TenantID) != req.TenantID:
		return errors.New("entity tenant does not match request tenant")
	case req.RequestID == "":
		return errors.New("requestID is empty")
	case req.TriesLeft < 1:
		return errors.New("triesLeft is below 1")
	case req.AnswerLimitMs < 1:
		return errors.New("answerLimitMs is below 1")
	case req.OwnerNodeID == "":
		return errors.New("ownerNodeID is empty")
	case req.Major < 1:
		return errors.New("major is below 1")
	}
	switch req.Kind {
	case internalgrpc.ProcessorCallout.String():
		if req.Processor == nil {
			return errors.New("processor callout without a processor")
		}
	case internalgrpc.CriteriaCallout.String():
		if len(req.Criterion) == 0 {
			return errors.New("criteria callout without a criterion")
		}
	case internalgrpc.FunctionCallout.String():
		if req.Function == nil {
			return errors.New("function callout without a function")
		}
	default:
		return errors.New("unknown callout kind")
	}
	return nil
}

// toCallout builds, on the receiving pnode, the Callout the owner built — with
// the same builder — and fills what the hand-over carried: the request id, the
// answer limit, whether it is repeat-safe, the owner's id for the passes, the
// enclosing pairs, and a numberer that counts minor = 1, 2, … under the
// hand-over's major. The request must have passed validate.
func (req *DispatchCalloutRequest) toCallout() (internalgrpc.Callout, *contract.CalloutFailure) {
	entity := &spi.Entity{Meta: req.EntityMeta, Data: []byte(req.Entity)}
	tenant := spi.TenantID(req.TenantID)

	var call internalgrpc.Callout
	switch req.Kind {
	case internalgrpc.ProcessorCallout.String():
		call = internalgrpc.NewProcessorCallout(tenant, entity, *req.Processor, req.WorkflowName, req.TransitionName, req.TxID)
	case internalgrpc.CriteriaCallout.String():
		var failure *contract.CalloutFailure
		call, failure = internalgrpc.NewCriteriaCallout(tenant, entity, req.Criterion, req.Target, req.WorkflowName, req.TransitionName, req.ProcessorName, req.TxID)
		if failure != nil {
			return internalgrpc.Callout{}, failure
		}
	default:
		call = internalgrpc.NewFunctionCallout(tenant, entity, *req.Function, req.WorkflowName, req.TransitionName, req.TxID)
	}
	call.RequestID = req.RequestID
	call.AnswerLimit = time.Duration(req.AnswerLimitMs) * time.Millisecond
	call.RepeatSafe = req.RepeatSafe
	call.OwnerNodeID = req.OwnerNodeID
	call.Number = internalgrpc.NewMinorNumberer(req.Major)
	for _, p := range req.Outer {
		call.Outer = append(call.Outer, token.Pair{Callout: p.Callout, Major: p.Major, Minor: p.Minor})
	}
	return call, nil
}

// responseFromLocal is the answer to a hand-over: what the local procedure
// produced, in the words of the wire.
func responseFromLocal(call internalgrpc.Callout, res internalgrpc.LocalResult, warnings, errs []string) DispatchCalloutResponse {
	used := res.TriesUsed
	resp := DispatchCalloutResponse{TriesUsed: &used, Warnings: warnings, Errors: errs}
	for _, a := range res.Attempts {
		resp.Attempts = append(resp.Attempts, WireAttempt{MemberID: a.MemberID, Kind: a.Kind.String(), Cause: a.Cause})
	}
	switch {
	case res.CtxErr != nil:
		// The owner hung up. Nobody reads this; it is well-formed all the same.
		resp.Outcome = contract.NoAnswer.String()
	case res.Failure != nil:
		fillFailure(&resp, res.Failure)
	default:
		resp.Outcome = OutcomeOK
		switch call.Kind {
		case internalgrpc.ProcessorCallout:
			if res.Result.Entity != nil {
				resp.EntityData = res.Result.Entity.Data
			}
		case internalgrpc.CriteriaCallout:
			matches := res.Result.Matches
			resp.Matches = &matches
			resp.Reason = res.Result.Reason
		case internalgrpc.FunctionCallout:
			resp.Result = res.Result.Function.Value
			resp.ResultKind = res.Result.Function.Kind
		}
	}
	return resp
}

// refusal is the answer of a pnode that tried no cnode.
func refusal(failure *contract.CalloutFailure) DispatchCalloutResponse {
	zero := 0
	resp := DispatchCalloutResponse{TriesUsed: &zero}
	fillFailure(&resp, failure)
	return resp
}

// fillFailure states a failure in the words of the wire. A classified error
// travels as its code, status and retryable flag; its client-safe text travels
// in the attempts. A cnode's own failure travels as its message and verdict.
func fillFailure(resp *DispatchCalloutResponse, failure *contract.CalloutFailure) {
	resp.Outcome = failure.Kind.String()
	if failure.Kind == contract.MemberFailed {
		resp.MemberError = failure.Message
		resp.MemberRetryable = failure.Retryable
		return
	}
	var appErr *common.AppError
	switch {
	case errors.As(failure, &appErr):
		resp.ErrorCode = appErr.Code
		resp.ErrorStatus = appErr.Status
		resp.ErrorRetryable = appErr.Retryable
	case errors.Is(failure, contract.ErrNoMatchingMember):
		// The same trio a single pnode answers for "no cnode".
		resp.ErrorCode = common.ErrCodeNoComputeMemberForTag
		resp.ErrorStatus = http.StatusServiceUnavailable
		resp.ErrorRetryable = true
	case errors.Is(failure, contract.ErrAuthContextUnavailable):
		// A ticketed 500 where it happens locally; the same on the owner.
		resp.ErrorCode = common.ErrCodeServerError
		resp.ErrorStatus = http.StatusInternalServerError
	}
}

// readAnswer reads a decoded, authenticated answer. Only an outcome of
// no_handoff says that nothing was handed to a cnode; whatever is not ok,
// member_failed or terminal either is no_answer. An answer without triesUsed
// counts as one try, never zero, so the owner's loop always makes progress; one
// whose triesUsed is outside 0..triesLeft is not believed at all.
func readAnswer(call internalgrpc.Callout, resp *DispatchCalloutResponse, triesLeft int) HandOverAnswer {
	if resp == nil {
		return lostAnswer()
	}
	used := 1
	if resp.TriesUsed != nil {
		used = *resp.TriesUsed
	}
	if used < 0 || used > triesLeft {
		return lostAnswer()
	}
	ans := HandOverAnswer{Connected: true, TriesUsed: used, Warnings: resp.Warnings, peerErrors: resp.Errors}
	for _, a := range resp.Attempts {
		ans.Attempts = append(ans.Attempts, contract.CalloutAttempt{MemberID: a.MemberID, Kind: kindFromWire(a.Kind), Cause: a.Cause})
	}

	switch resp.Outcome {
	case OutcomeOK:
		if used == 0 {
			return lostAnswer()
		}
		result := internalgrpc.CalloutResult{}
		switch call.Kind {
		case internalgrpc.ProcessorCallout:
			result.Entity = &spi.Entity{Meta: call.Source.Entity.Meta, Data: resp.EntityData}
		case internalgrpc.CriteriaCallout:
			result.Matches = resp.Matches != nil && *resp.Matches
			result.Reason = resp.Reason
		case internalgrpc.FunctionCallout:
			result.Function = contract.FunctionResult{Kind: resp.ResultKind, Value: resp.Result}
		}
		ans.Result = &result
	case contract.NoHandOff.String():
		ans.Connected = used > 0
		ans.Failure = classifiedFailure(contract.NoHandOff, resp, ans.Attempts)
	case contract.NoAnswer.String():
		if used == 0 {
			return lostAnswer()
		}
		ans.Failure = classifiedFailure(contract.NoAnswer, resp, ans.Attempts)
	case contract.MemberFailed.String():
		if used == 0 {
			return lostAnswer()
		}
		msg := resp.MemberError
		if msg == "" {
			msg = peerFailedClientMessage
		}
		ans.Failure = &contract.CalloutFailure{Kind: contract.MemberFailed, Message: msg, Retryable: resp.MemberRetryable}
	case contract.Terminal.String():
		ans.Failure = classifiedFailure(contract.Terminal, resp, ans.Attempts)
	default:
		return lostAnswer()
	}
	return ans
}

// classifiedFailure re-mints, on the owner, the error the answering pnode
// classified: the same code, status and retryable flag, with the client-safe
// text of the last try.
func classifiedFailure(kind contract.CalloutFailureKind, resp *DispatchCalloutResponse, attempts []contract.CalloutAttempt) *contract.CalloutFailure {
	text := peerFailedClientMessage
	if n := len(attempts); n > 0 && attempts[n-1].Cause != "" {
		text = attempts[n-1].Cause
	}
	if resp.ErrorCode == "" {
		switch kind {
		case contract.NoHandOff:
			return noCnodeFailure()
		case contract.NoAnswer:
			return lostAnswer().Failure
		default:
			err := errors.New(text)
			return &contract.CalloutFailure{Kind: kind, Message: text, Err: err}
		}
	}
	// A classified error's text already starts with its code.
	text = strings.TrimPrefix(text, resp.ErrorCode+": ")
	var appErr *common.AppError
	switch {
	case resp.ErrorStatus == http.StatusInternalServerError:
		appErr = common.InternalWithCode(resp.ErrorCode, text, nil)
	case resp.ErrorStatus >= 400 && resp.ErrorStatus <= 599:
		appErr = common.Operational(resp.ErrorStatus, resp.ErrorCode, text)
	default:
		appErr = common.Operational(http.StatusServiceUnavailable, resp.ErrorCode, text)
	}
	if resp.ErrorRetryable {
		appErr = appErr.AsRetryable()
	}
	return &contract.CalloutFailure{Kind: kind, Code: appErr.Code, Message: appErr.Message, Err: appErr}
}

func kindFromWire(s string) contract.CalloutFailureKind {
	for _, k := range []contract.CalloutFailureKind{contract.NoHandOff, contract.NoAnswer, contract.MemberFailed, contract.Terminal} {
		if k.String() == s {
			return k
		}
	}
	// An attempt of a kind this version does not know: the careful reading.
	return contract.NoAnswer
}

func noCnodeFailure() *contract.CalloutFailure {
	appErr := common.Operational(http.StatusServiceUnavailable, common.ErrCodeNoComputeMemberForTag,
		"no compute member took the callout on the peer node").AsRetryable()
	return &contract.CalloutFailure{Kind: contract.NoHandOff, Code: appErr.Code, Message: appErr.Message, Err: appErr}
}

// lostAnswer is a hand-over whose answer never arrived, did not authenticate,
// or cannot be believed. The peer may have handed the work to a cnode — to more
// than one — so it counts as one try and is NoAnswer.
func lostAnswer() HandOverAnswer {
	appErr := common.Operational(http.StatusServiceUnavailable, common.ErrCodeDispatchForwardFailed, forwardFailedClientMessage).AsRetryable()
	return HandOverAnswer{
		Connected: true,
		TriesUsed: 1,
		Failure:   &contract.CalloutFailure{Kind: contract.NoAnswer, Code: appErr.Code, Message: appErr.Message, Err: appErr},
		Attempts:  []contract.CalloutAttempt{{MemberID: "-", Kind: contract.NoAnswer, Cause: forwardFailedClientMessage}},
	}
}

// notConnected is a peer that was not asked: the connection to it could not be
// opened, or its address failed validation. Nothing left this pnode, and no try
// is used.
func notConnected() HandOverAnswer {
	return HandOverAnswer{Failure: noCnodeFailure()}
}

// provedBeforeConnecting is a hand-over that failed before any connection was
// attempted — a request that cannot be built, marshalled or signed. It would
// fail identically on every try: Terminal. Whatever the error says stays out of
// the client-safe message; it is logged where the hand-over is made.
func provedBeforeConnecting(err error) HandOverAnswer {
	appErr := common.Internal("the callout could not be handed over", err)
	return HandOverAnswer{Failure: &contract.CalloutFailure{Kind: contract.Terminal, Code: appErr.Code, Message: appErr.Message, Err: appErr}}
}
