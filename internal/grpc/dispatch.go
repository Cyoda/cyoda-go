package grpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/logging"
)

// ErrNoMatchingMember is returned when no calculation member is registered for
// the requested tags. Callers (e.g. ClusterDispatcher) test for this sentinel
// via errors.Is rather than string matching.
//
// Aliased from contract.ErrNoMatchingMember (the canonical definition lives
// in the leaf internal/contract package) so error-classification code in
// internal/domain/entity, which internal/grpc already depends on, can match
// this sentinel without an import cycle. See contract.ErrNoMatchingMember's
// doc comment for the full rationale.
var ErrNoMatchingMember = contract.ErrNoMatchingMember

// ProcessorDispatcher dispatches processor and criteria calculations to external
// calculation members via the MemberRegistry.
type ProcessorDispatcher struct {
	registry           *MemberRegistry
	selector           MemberSelector
	uuids              spi.UUIDGenerator
	signer             *token.Signer
	selfNodeID         string
	tokenTTL           time.Duration
	answerLimitDefault time.Duration
	answerLimitMax     time.Duration
}

// NewProcessorDispatcher creates a new ProcessorDispatcher.
func NewProcessorDispatcher(registry *MemberRegistry, selector MemberSelector, uuids spi.UUIDGenerator, signer *token.Signer, selfNodeID string, tokenTTL, answerLimitDefault, answerLimitMax time.Duration) *ProcessorDispatcher {
	return &ProcessorDispatcher{
		registry:           registry,
		selector:           selector,
		uuids:              uuids,
		signer:             signer,
		selfNodeID:         selfNodeID,
		tokenTTL:           tokenTTL,
		answerLimitDefault: answerLimitDefault,
		answerLimitMax:     answerLimitMax,
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
		err := fmt.Errorf("responseTimeoutMs %d exceeds the upper bound of %d ms set by CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS",
			responseTimeoutMs, d.answerLimitMax.Milliseconds())
		return 0, &contract.CalloutFailure{Kind: contract.Terminal, Message: err.Error(), Err: err}
	}
	return time.Duration(responseTimeoutMs) * time.Millisecond, nil
}

// resolveTxToken returns the tx-token to attach to a calc request. A token
// pre-minted by an upstream ClusterDispatcher (carried on ctx, NodeID = owner)
// wins so a forwarded dispatch routes callbacks to the owner, not this node.
// Otherwise self-mint {selfNodeID, txID}. Empty txID → no token (standalone).
func (d *ProcessorDispatcher) resolveTxToken(ctx context.Context, txID string) string {
	if tok := TxTokenFromContext(ctx); tok != "" {
		return tok
	}
	if txID == "" || d.signer == nil {
		return ""
	}
	tok, err := d.signer.Issue(d.selfNodeID, txID, time.Now().Add(d.tokenTTL))
	if err != nil {
		slog.Error("failed to mint tx-token", "pkg", "grpc", "err", err)
		return ""
	}
	return tok
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
//   - ctx.Err(), unchanged, when the caller's own context ended.
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
		return CalloutResult{}, terminalFailure(fmt.Errorf("failed to build %s cloud event: %w", label, err)), nil
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
			return CalloutResult{}, nil, ctx.Err()
		default:
			slog.Error("dispatch timeout", "pkg", "grpc", "phase", "enqueue", "memberId", member.ID, "label", label, "name", name, "requestId", requestID, "timeout", call.AnswerLimit)
			return CalloutResult{}, appFailure(contract.NoHandOff, common.Operational(http.StatusServiceUnavailable, common.ErrCodeDispatchTimeout,
				fmt.Sprintf("%s dispatch timed out after %dms: member not draining", label, limitMs)).AsRetryable()), nil
		}
	}

	// The hand-off happened: from here on the work may have reached the cnode.
	select {
	case resp := <-ch:
		// Warnings first, keyed by callout name, so that a failed try still
		// surfaces them and the client sees which callout warned.
		if resp != nil {
			for _, w := range resp.Warnings {
				common.AddWarning(ctx, fmt.Sprintf("%s %s: %s", label, name, w))
			}
		}
		if resp != nil && resp.Disconnected {
			slog.Error("member disconnected mid-dispatch", "pkg", "grpc", "memberId", member.ID, "label", label, "name", name, "requestId", requestID)
			return CalloutResult{}, appFailure(contract.NoAnswer, disconnectedErr(label)), nil
		}
		if resp == nil || !resp.Success {
			failure := &contract.CalloutFailure{Kind: contract.MemberFailed, Message: label + " returned failure"}
			if resp != nil {
				failure.Retryable = resp.Retryable
				if resp.Error != "" {
					failure.Message = resp.Error
					common.AddError(ctx, fmt.Sprintf("%s %s: %s", label, name, resp.Error))
				}
			}
			return CalloutResult{}, failure, nil
		}
		result, err := call.mapResponse(resp)
		if err != nil {
			return CalloutResult{}, terminalFailure(err), nil
		}
		slog.Debug("dispatch completed", "pkg", "grpc", "memberId", member.ID, "label", label, "name", name, "requestId", requestID)
		return result, nil, nil
	case <-callCtx.Done():
		if ctx.Err() != nil {
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

// terminalFailure is a failure that would repeat identically on any cnode.
func terminalFailure(err error) *contract.CalloutFailure {
	return &contract.CalloutFailure{Kind: contract.Terminal, Message: err.Error(), Err: err}
}

// disconnectedErr is the retryable 503 for a cnode that is gone — before the
// hand-off (NoHandOff) or after it (NoAnswer); the kind beside it tells which.
func disconnectedErr(label string) *common.AppError {
	return common.Operational(http.StatusServiceUnavailable, common.ErrCodeComputeMemberDisconnected,
		fmt.Sprintf("compute member disconnected during %s dispatch", label)).AsRetryable()
}

// singleTryNumberer numbers the one try an entry point below makes: the
// owner's first, (1, 0).
type singleTryNumberer struct{}

func (singleTryNumberer) Next() (uint32, uint32) { return 1, 0 }

// runSingleTry is the local procedure with one try, as the owner of the
// callout: what DispatchProcessor, DispatchCriteria and DispatchFunction do
// until the owner's loop takes their place.
func (d *ProcessorDispatcher) runSingleTry(ctx context.Context, call Callout) (CalloutResult, error) {
	limit, failure := d.ResolveAnswerLimit(call.ResponseTimeoutMs)
	if failure != nil {
		return CalloutResult{}, failure
	}
	call.RequestID = uuid.UUID(d.uuids.NewTimeUUID()).String()
	call.AnswerLimit = limit
	call.OwnerNodeID = d.selfNodeID
	call.Number = singleTryNumberer{}
	res := d.RunLocal(ctx, call, 1)
	return res.Result, res.Err()
}

// DispatchProcessor sends an entity processor calculation request to a matching
// calculation member and waits for the response.
func (d *ProcessorDispatcher) DispatchProcessor(ctx context.Context, entity *spi.Entity, processor spi.ProcessorDefinition, workflowName string, transitionName string, txID string) (*spi.Entity, error) {
	uc := spi.MustGetUserContext(ctx)
	res, err := d.runSingleTry(ctx, NewProcessorCallout(uc.Tenant.ID, entity, processor, workflowName, transitionName, txID))
	if err != nil {
		return nil, err
	}
	return res.Entity, nil
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

// DispatchCriteria sends an entity criteria calculation request to a matching
// calculation member and waits for the boolean result.
func (d *ProcessorDispatcher) DispatchCriteria(ctx context.Context, entity *spi.Entity, criterion json.RawMessage, target string, workflowName string, transitionName string, processorName string, txID string) (bool, string, error) {
	uc := spi.MustGetUserContext(ctx)
	call, failure := NewCriteriaCallout(uc.Tenant.ID, entity, criterion, target, workflowName, transitionName, processorName, txID)
	if failure != nil {
		return false, "", failure
	}
	res, err := d.runSingleTry(ctx, call)
	if err != nil {
		return false, "", err
	}
	return res.Matches, res.Reason, nil
}

// DispatchFunction sends a generic Function calculation request (e.g. a
// scheduled-transition timing computation) to a matching calculation member
// and returns its typed result.
func (d *ProcessorDispatcher) DispatchFunction(ctx context.Context, entity *spi.Entity, fn spi.ScheduleFunction, workflowName string, transitionName string, txID string) (contract.FunctionResult, error) {
	uc := spi.MustGetUserContext(ctx)
	res, err := d.runSingleTry(ctx, NewFunctionCallout(uc.Tenant.ID, entity, fn, workflowName, transitionName, txID))
	if err != nil {
		return contract.FunctionResult{}, err
	}
	return res.Function, nil
}
