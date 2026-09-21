package grpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/logging"
)

// ProcessorDispatcher is the local procedure: it tries a callout on this node's
// own calculation members, one after another, for as many tries as the owner's
// loop gives it. The pnode that holds the transaction runs it through
// internal/callout.Coordinator; a pnode that received a hand-over runs it
// through the dispatch handler.
type ProcessorDispatcher struct {
	registry           *MemberRegistry
	selector           MemberSelector
	signer             *token.Signer
	answerLimitDefault time.Duration
	answerLimitMax     time.Duration
	passAllowance      time.Duration
}

// NewProcessorDispatcher creates a new ProcessorDispatcher. It holds no node
// identity of its own: the owner a pass names is the callout's, whichever
// pnode is running the try.
func NewProcessorDispatcher(registry *MemberRegistry, selector MemberSelector, signer *token.Signer, answerLimitDefault, answerLimitMax, passAllowance time.Duration) *ProcessorDispatcher {
	return &ProcessorDispatcher{
		registry:           registry,
		selector:           selector,
		signer:             signer,
		answerLimitDefault: answerLimitDefault,
		answerLimitMax:     answerLimitMax,
		passAllowance:      passAllowance,
	}
}

// ResolveAnswerLimit gives the answer limit of a callout: its stored
// responseTimeoutMs if positive, else the configured default. A stored value
// above the configured upper bound — possible when the bound was lowered after
// the workflow was imported — is not clamped: the callout fails as Terminal. A
// substituted limit would be a wrong-but-available answer.
func (d *ProcessorDispatcher) ResolveAnswerLimit(responseTimeoutMs int64) (time.Duration, *contract.CalloutFailure) {
	if responseTimeoutMs <= 0 {
		return d.answerLimitDefault, nil
	}
	// Compared in milliseconds: a huge stored value would overflow a Duration.
	if responseTimeoutMs > d.answerLimitMax.Milliseconds() {
		// Both numbers are configuration (the callout's own stored value, the
		// server's configured bound), never client-request or entity-payload
		// content, so composing this message directly is safe; it is written
		// as an authored Sprintf rather than an error's .Error() so nothing
		// here depends on an intermediate error's text ever being safe by
		// construction.
		msg := fmt.Sprintf("responseTimeoutMs %d exceeds the upper bound of %d ms set by CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS",
			responseTimeoutMs, d.answerLimitMax.Milliseconds())
		return 0, &contract.CalloutFailure{Kind: contract.Terminal, Message: msg}
	}
	return time.Duration(responseTimeoutMs) * time.Millisecond, nil
}

// mintPass issues the pass one try gives its cnode: the transaction token the
// cnode's callbacks carry. It names the owner — callbacks are routed there
// whichever pnode made the hand-off — the transaction, the callout and the
// try's fencing number, and it lives as long as the try may: the answer limit
// plus an allowance for routing and for clocks that differ between pnodes. A
// callout outside a transaction carries no pass.
//
// A callout inside a transaction that names no owner is refused here, before
// the hand-off, rather than minting a pass whose callbacks could never be
// routed.
func (d *ProcessorDispatcher) mintPass(call Callout, major, minor uint32) (string, error) {
	if call.TxID == "" {
		return "", nil
	}
	if call.OwnerNodeID == "" {
		return "", fmt.Errorf("failed to mint transaction pass: the callout names no owner")
	}
	pass, err := d.signer.Issue(token.Claims{
		NodeID:    call.OwnerNodeID,
		TxRef:     call.TxID,
		ExpiresAt: time.Now().Add(call.AnswerLimit + d.passAllowance).Unix(),
		Callout:   call.RequestID,
		Major:     major,
		Minor:     minor,
		Outer:     call.Outer,
	})
	if err != nil {
		return "", fmt.Errorf("failed to mint transaction pass: %w", err)
	}
	return pass, nil
}

// buildEntityPayload builds the DataPayloadJson attached to a calc request when
// AttachEntity is set. Shared by every callout shape.
func buildEntityPayload(entity *spi.Entity) *events.DataPayloadJson {
	versionInt := 0
	fmt.Sscanf(entity.Meta.ModelRef.ModelVersion, "%d", &versionInt)
	return &events.DataPayloadJson{
		Type: "JSON",
		Data: json.RawMessage(entity.Data),
		Meta: map[string]any{
			"id": entity.Meta.ID,
			"modelKey": map[string]any{
				"name":    entity.Meta.ModelRef.EntityName,
				"version": versionInt,
			},
			"state":          entity.Meta.State,
			"creationDate":   entity.Meta.CreationDate.Format(time.RFC3339Nano),
			"lastUpdateTime": entity.Meta.LastModifiedDate.Format(time.RFC3339Nano),
			"transactionId":  entity.Meta.TransactionID,
		},
	}
}

// dispatchCalloutToMember makes one try: it wraps the callout's request in a
// CloudEvent carrying the auth context and pass, tracks the request, hands it
// to the member's writer, and waits for the tracked response or the answer
// limit. Exactly one of its three results is meaningful:
//
//   - the mapped result, when the cnode answered success;
//   - a CalloutFailure, whose Kind says whether another cnode may be tried.
//     The line is the hand-off: until Member.Send returns nil the work provably
//     never left this pnode (NoHandOff); after it, silence or a dropped stream
//     is NoAnswer. Codes, statuses and messages are the ones a client has
//     always seen — the kind travels beside them;
//   - ctx.Err(), unchanged, when the caller's own context ended. When it ended
//     instead because the callout's own deadline passed (contract.ErrCalloutDeadline
//     as its cause), the try is classified as a CalloutFailure instead — NoAnswer
//     after the hand-off, NoHandOff before it — and ctx.Err() is not returned.
//
// One deadline — the answer limit — bounds the hand-off and the wait together,
// so a cnode that is attached but not taking data costs up to one answer limit
// before it is classified NoHandOff.
//
// The callout kind and name flow into client-facing diagnostics (warnings and
// errors surface in the gRPC warnings array and the HTTP body — see
// .claude/rules/error-handling.md) and into server logs.
func (d *ProcessorDispatcher) dispatchCalloutToMember(ctx context.Context, member *Member, call Callout, pass string) (CalloutResult, *contract.CalloutFailure, error) {
	label, name, requestID := call.Kind.String(), call.Name, call.RequestID
	limitMs := call.AnswerLimit.Milliseconds()

	ce, err := NewCloudEvent(call.eventType, call.buildRequest(requestID))
	if err != nil {
		return CalloutResult{}, terminalFailure(fmt.Errorf("failed to build %s cloud event: %w", label, err), member.ID, requestID), nil
	}
	if err := AttachAuthContext(ctx, ce); err != nil {
		// The cause names the principal; the client-safe Message does not.
		return CalloutResult{}, &contract.CalloutFailure{
			Kind:    contract.Terminal,
			Message: "auth context unavailable for dispatch",
			Err:     fmt.Errorf("failed to attach auth context to %s cloud event: %w", label, err),
		}, nil
	}
	AttachTxToken(ce, pass)

	slog.Debug("dispatch request", "pkg", "grpc", "requestId", requestID, "memberId", member.ID,
		"payload", logging.PayloadPreview([]byte(ce.GetTextData()), 200))

	callCtx, cancel := context.WithTimeout(ctx, call.AnswerLimit)
	defer cancel()

	// TrackRequest fails closed with ErrMemberEvicted the instant the member
	// is torn down, even in the window before Evict has finished closing the
	// evicted channel (see Member.closed): that is what keeps a try from
	// selecting on a never-tracked channel until its answer limit and
	// misreporting DISPATCH_TIMEOUT for a member that was already gone.
	ch, err := member.TrackRequest(requestID)
	if err != nil {
		slog.Warn("member gone before dispatch", "pkg", "grpc", "memberId", member.ID, "label", label, "name", name, "requestId", requestID)
		return CalloutResult{}, appFailure(contract.NoHandOff, disconnectedErr(label)), nil
	}
	// Every exit that does not consume the response must clear the tracking
	// entry, or a late reply finds a dangling channel and the map entry leaks.
	// The response arm's normal completion already cleared it; clearing again
	// is a no-op.
	defer member.AbandonRequest(requestID)

	if err := member.Send(callCtx, ce); err != nil {
		switch {
		case errors.Is(err, ErrMemberEvicted):
			slog.Error("member evicted while enqueueing dispatch", "pkg", "grpc", "memberId", member.ID, "label", label, "name", name, "requestId", requestID)
			return CalloutResult{}, appFailure(contract.NoHandOff, disconnectedErr(label)), nil
		case ctx.Err() != nil:
			if calloutDeadlinePassed(ctx) {
				slog.Error("dispatch cut off by the callout deadline", "pkg", "grpc", "phase", "enqueue", "memberId", member.ID, "label", label, "name", name, "requestId", requestID)
				return CalloutResult{}, appFailure(contract.NoHandOff, common.Operational(http.StatusServiceUnavailable, common.ErrCodeDispatchTimeout,
					fmt.Sprintf("%s dispatch cut off at the callout deadline: member not draining", label)).AsRetryable()), nil
			}
			return CalloutResult{}, nil, ctx.Err()
		default:
			slog.Error("dispatch timeout", "pkg", "grpc", "phase", "enqueue", "memberId", member.ID, "label", label, "name", name, "requestId", requestID, "timeout", call.AnswerLimit)
			return CalloutResult{}, appFailure(contract.NoHandOff, common.Operational(http.StatusServiceUnavailable, common.ErrCodeDispatchTimeout,
				fmt.Sprintf("%s dispatch timed out after %dms: member not draining", label, limitMs)).AsRetryable()), nil
		}
	}

	// The hand-off happened: from here on the work may have reached the cnode.
	// Every response on ch is non-nil — CompleteRequest and failAllPending are
	// its only producers and neither sends nil, and the channel is never
	// closed — so a nil one is not guarded for: doing so would give the
	// resulting failure a kind (MemberFailed says the cnode answered) that no
	// nil response could ever have earned.
	select {
	case resp := <-ch:
		// Warnings first, keyed by callout name, so that a failed try still
		// surfaces them and the client sees which callout warned. Bounded here,
		// where the member's own text becomes the client's, in the same way and
		// for the same reason as its failure message below.
		kept := resp.Warnings
		if len(kept) > maxMemberWarnings {
			kept = kept[:maxMemberWarnings]
		}
		for _, w := range kept {
			common.AddWarning(ctx, fmt.Sprintf("%s %s: %s", label, name, boundMemberText(w)))
		}
		if len(resp.Warnings) > maxMemberWarnings {
			common.AddWarning(ctx, fmt.Sprintf("%s %s: further warnings from the compute member were omitted", label, name))
		}
		if resp.Disconnected {
			slog.Error("member disconnected mid-dispatch", "pkg", "grpc", "memberId", member.ID, "label", label, "name", name, "requestId", requestID)
			return CalloutResult{}, appFailure(contract.NoAnswer, disconnectedErr(label)), nil
		}
		if !resp.Success {
			failure := &contract.CalloutFailure{Kind: contract.MemberFailed, Message: label + " returned failure", Retryable: resp.Retryable}
			if resp.Error != "" {
				failure.Message = boundMemberText(resp.Error)
				common.AddError(ctx, fmt.Sprintf("%s %s: %s", label, name, failure.Message))
			}
			return CalloutResult{}, failure, nil
		}
		result, err := call.mapResponse(resp)
		if err != nil {
			return CalloutResult{}, memberResponseUnreadable(err, label, name, member.ID, requestID), nil
		}
		slog.Debug("dispatch completed", "pkg", "grpc", "memberId", member.ID, "label", label, "name", name, "requestId", requestID)
		return result, nil, nil
	case <-callCtx.Done():
		if ctx.Err() != nil {
			if calloutDeadlinePassed(ctx) {
				slog.Error("dispatch cut off by the callout deadline", "pkg", "grpc", "phase", "response", "memberId", member.ID, "label", label, "name", name, "requestId", requestID)
				return CalloutResult{}, appFailure(contract.NoAnswer, common.Operational(http.StatusServiceUnavailable, common.ErrCodeDispatchTimeout,
					fmt.Sprintf("%s dispatch cut off at the callout deadline: no response", label)).AsRetryable()), nil
			}
			return CalloutResult{}, nil, ctx.Err()
		}
		slog.Error("dispatch timeout", "pkg", "grpc", "phase", "response", "memberId", member.ID, "label", label, "name", name, "requestId", requestID, "timeout", call.AnswerLimit)
		return CalloutResult{}, appFailure(contract.NoAnswer, common.Operational(http.StatusServiceUnavailable, common.ErrCodeDispatchTimeout,
			fmt.Sprintf("%s dispatch timed out after %dms: no response", label, limitMs)).AsRetryable()), nil
	}
}

// appFailure is a failure of the given kind carrying appErr, with its code and
// client-safe message repeated beside the kind.
func appFailure(kind contract.CalloutFailureKind, appErr *common.AppError) *contract.CalloutFailure {
	return &contract.CalloutFailure{Kind: kind, Code: appErr.Code, Message: appErr.Message, Err: appErr}
}

// terminalFailure is an internal failure that would repeat identically on any
// cnode: this pnode could not build its own request. The client sees the
// fixed literal every internal callout failure in this file carries; the real
// error — which can quote a byte of the entity payload being marshalled — is
// logged here by shape only, never by text, and kept solely behind the
// AppError's cause, a place common.WriteError never renders (it exposes only
// a ticket for a LevelInternal error), so errors.Is/As on it still reach the
// underlying cause.
func terminalFailure(err error, memberID, requestID string) *contract.CalloutFailure {
	slog.Error("dispatch could not build its own request", "pkg", "grpc", "memberId", memberID,
		"requestId", requestID, "error", jsonErrorShape(err))
	appErr := common.Internal("internal error", nil).WithCause(err)
	return &contract.CalloutFailure{Kind: contract.Terminal, Code: appErr.Code, Message: appErr.Message, Err: appErr}
}

// memberResponseUnreadable is Terminal: spec §3's site table assigns
// "response payload unmarshal" Terminal, not MemberFailed. MemberFailed means
// the cnode itself answered success=false, with the cnode's OWN message and
// its own retryable verdict (contract.CalloutFailure's doc); here the member
// answered success, and the message is ours, not the cnode's — reporting it
// as MemberFailed would also mislabel it over the wire, since fillFailure
// (internal/cluster/dispatch/handover.go) places a MemberFailed failure's
// Message into memberError, claiming it as the member's own words. The
// client sees a fixed, client-safe message; the decode error — which can
// quote a byte of the member's own response — is logged here by shape only.
// It is not attached to the failure at all (no Err, no Code — this Terminal
// failure has none of its own, like ResolveAnswerLimit's and
// NewCriteriaCallout's): CalloutFailure.Error() returns Err's text verbatim
// once Err is set, bypassing Message entirely, which would undo the
// sanitizing done here.
func memberResponseUnreadable(err error, label, name, memberID, requestID string) *contract.CalloutFailure {
	slog.Error("compute member response could not be read", "pkg", "grpc", "label", label, "name", name,
		"memberId", memberID, "requestId", requestID, "error", jsonErrorShape(err))
	return &contract.CalloutFailure{Kind: contract.Terminal, Message: "the compute member's response could not be read"}
}

// jsonErrorShape renders a decode/encode error for the server log without the
// value it failed on: json.SyntaxError and json.UnmarshalTypeError can quote a
// fragment of the payload — the tenant's entity data on the outbound side, the
// compute member's own response on the inbound side — that a log line must
// never carry. It logs the error's Go type and, where present, the byte
// offset (SyntaxError) or the struct field named (UnmarshalTypeError; the
// Struct and Field names come from this file's own fixed request/response
// shapes, never from payload content).
func jsonErrorShape(err error) string {
	var syn *json.SyntaxError
	if errors.As(err, &syn) {
		return fmt.Sprintf("%T at offset %d", syn, syn.Offset)
	}
	var ute *json.UnmarshalTypeError
	if errors.As(err, &ute) {
		return fmt.Sprintf("%T (struct %s field %s)", ute, ute.Struct, ute.Field)
	}
	return fmt.Sprintf("%T", err)
}

// calloutDeadlinePassed reports whether ctx ended because the callout's own
// deadline passed, as opposed to its caller going away.
func calloutDeadlinePassed(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), contract.ErrCalloutDeadline)
}

// disconnectedErr is the retryable 503 for a cnode that is gone — before the
// hand-off (NoHandOff) or after it (NoAnswer); the kind beside it tells which.
func disconnectedErr(label string) *common.AppError {
	return common.Operational(http.StatusServiceUnavailable, common.ErrCodeComputeMemberDisconnected,
		fmt.Sprintf("compute member disconnected during %s dispatch", label)).AsRetryable()
}

// maxMemberMessageRunes bounds each piece of a compute member's own free text
// before it becomes client text: a MemberFailed failure's Message, which flows
// into a 400 body directly and, once several tries are exhausted, is
// concatenated into a CALLOUT_FAILED list (internal/callout/failure.go's
// attemptsMessage) alongside every other try's cause; and each of the
// member's warnings, which are returned in the response. Unbounded, one
// talkative or malicious cnode could make either arbitrarily large.
const maxMemberMessageRunes = 512

// maxMemberWarnings bounds how many of a compute member's warnings become
// client text for one try. They are added to the request's diagnostics and
// returned in the response, so an unbounded list is a response a single cnode
// decides the size of. Past the bound one warning says the rest were left out.
const maxMemberWarnings = 32

// boundMemberText keeps the first maxMemberMessageRunes runes of s, appending
// "…" when anything was cut so a reader can tell a shortened message from a
// short one. Runes, not bytes: a cut must never split a multi-byte rune (see
// boundLine, internal/cluster/dispatch/peer_router.go, for the same shape
// applied to a hand-over answer's diagnostics).
func boundMemberText(s string) string {
	if len(s) <= maxMemberMessageRunes {
		return s // runes never outnumber bytes: nothing to cut
	}
	r := []rune(s)
	if len(r) <= maxMemberMessageRunes {
		return s
	}
	return string(r[:maxMemberMessageRunes]) + "…"
}

// applyProcessorResponse extracts updated entity data from the response payload.
func applyProcessorResponse(entity *spi.Entity, resp *ProcessingResponse) (*spi.Entity, error) {
	if resp.Payload == nil {
		return entity, nil
	}

	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(resp.Payload, &envelope); err != nil {
		return nil, fmt.Errorf("failed to unmarshal processor response payload: %w", err)
	}
	if envelope.Data == nil {
		return entity, nil
	}

	updated := &spi.Entity{
		Meta: entity.Meta,
		Data: []byte(envelope.Data),
	}
	return updated, nil
}
