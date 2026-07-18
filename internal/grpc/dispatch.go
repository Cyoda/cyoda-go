package grpc

import (
	"context"
	"encoding/json"
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

const defaultResponseTimeoutMs = 30000

// ProcessorDispatcher dispatches processor and criteria calculations to external
// calculation members via the MemberRegistry.
type ProcessorDispatcher struct {
	registry   *MemberRegistry
	uuids      spi.UUIDGenerator
	signer     *token.Signer
	selfNodeID string
	tokenTTL   time.Duration
}

// NewProcessorDispatcher creates a new ProcessorDispatcher.
func NewProcessorDispatcher(registry *MemberRegistry, uuids spi.UUIDGenerator, signer *token.Signer, selfNodeID string, tokenTTL time.Duration) *ProcessorDispatcher {
	return &ProcessorDispatcher{
		registry:   registry,
		uuids:      uuids,
		signer:     signer,
		selfNodeID: selfNodeID,
		tokenTTL:   tokenTTL,
	}
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

// dispatchCalloutToMember carries out the transport sequence shared by every
// calculation callout: attach auth/tx-token to a CloudEvent wrapping req,
// track the request, send it, and wait for the tracked response or a
// timeout. Member resolution (FindByTags/ErrNoMatchingMember), request-struct
// construction, and response parsing stay with the caller — this handles only
// the wire protocol common to processor and criteria dispatch.
//
// label is the callout kind ("processor"/"criteria") and name is the
// configured processor/criteria name; both flow into client-facing
// diagnostics (warnings/errors surface in the gRPC warnings array and HTTP
// body — see .claude/rules/error-handling.md) and server logs, so operators
// and clients keep name-based correlation.
func (d *ProcessorDispatcher) dispatchCalloutToMember(ctx context.Context, member *Member, ceType string, req any, requestID, txID string, timeoutMs int64, label, name string) (*ProcessingResponse, error) {
	ce, err := NewCloudEvent(ceType, req)
	if err != nil {
		return nil, fmt.Errorf("failed to build %s cloud event: %w", label, err)
	}
	AttachAuthContext(ctx, ce)
	AttachTxToken(ce, d.resolveTxToken(ctx, txID))

	ceData := ce.GetTextData()
	slog.Debug("dispatch request", "pkg", "grpc", "requestId", requestID, "payload", logging.PayloadPreview([]byte(ceData), 200))

	ch := member.TrackRequest(requestID)

	if err := member.Send(ce); err != nil {
		slog.Error("failed to send to member", "pkg", "grpc", "memberId", member.ID, "error", err)
		return nil, fmt.Errorf("failed to send %s request: %w", label, err)
	}

	if timeoutMs <= 0 {
		timeoutMs = defaultResponseTimeoutMs
	}
	timeout := time.Duration(timeoutMs) * time.Millisecond

	select {
	case resp := <-ch:
		// Propagate warnings to request diagnostics, keyed by callout name
		// so the client sees which processor/criteria warned.
		if resp != nil {
			for _, w := range resp.Warnings {
				common.AddWarning(ctx, fmt.Sprintf("%s %s: %s", label, name, w))
			}
		}
		if resp != nil && resp.Disconnected {
			slog.Error("member disconnected mid-dispatch", "pkg", "grpc", "memberId", member.ID, "label", label, "name", name, "requestId", requestID)
			return nil, common.Operational(http.StatusServiceUnavailable, common.ErrCodeComputeMemberDisconnected,
				fmt.Sprintf("compute member disconnected during %s dispatch", label)).AsRetryable()
		}
		if resp == nil || !resp.Success {
			errMsg := label + " returned failure"
			if resp != nil && resp.Error != "" {
				errMsg = resp.Error
				common.AddError(ctx, fmt.Sprintf("%s %s: %s", label, name, errMsg))
			}
			return nil, fmt.Errorf("%s dispatch failed: %s", label, errMsg)
		}
		slog.Info("dispatch completed", "pkg", "grpc", "memberId", member.ID, "label", label, "name", name, "requestId", requestID, "success", true)
		return resp, nil
	case <-time.After(timeout):
		slog.Error("dispatch timeout", "pkg", "grpc", "label", label, "name", name, "requestId", requestID, "timeout", timeout)
		return nil, common.Operational(http.StatusServiceUnavailable, common.ErrCodeDispatchTimeout,
			fmt.Sprintf("%s dispatch timed out after %dms", label, timeoutMs)).AsRetryable()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// DispatchProcessor sends an entity processor calculation request to a matching
// calculation member and waits for the response.
func (d *ProcessorDispatcher) DispatchProcessor(ctx context.Context, entity *spi.Entity, processor spi.ProcessorDefinition, workflowName string, transitionName string, txID string) (*spi.Entity, error) {
	uc := spi.MustGetUserContext(ctx)
	tenantID := uc.Tenant.ID

	member := d.registry.FindByTags(tenantID, processor.Config.CalculationNodesTags)
	if member == nil {
		slog.Warn("no matching calculation member", "pkg", "grpc", "tags", processor.Config.CalculationNodesTags, "entityId", entity.Meta.ID)
		return nil, fmt.Errorf("%w: tags %q", ErrNoMatchingMember, processor.Config.CalculationNodesTags)
	}

	slog.Info("dispatching processor", "pkg", "grpc", "memberId", member.ID, "processor", processor.Name, "entityId", entity.Meta.ID)

	requestID := uuid.UUID(d.uuids.NewTimeUUID()).String()

	req := events.EntityProcessorCalculationRequestJson{
		ID:            requestID,
		RequestID:     requestID,
		EntityID:      entity.Meta.ID,
		ProcessorID:   processor.Name,
		ProcessorName: processor.Name,
		Workflow:      events.WorkflowInfoJson{ID: workflowName, Name: workflowName},
		Transition:    &events.TransitionInfoJson{ID: transitionName, Name: transitionName},
		TransactionID: &txID,
		Success:       true,
	}
	// ProcessorConfig.Context is a pass-through string surfaced verbatim in
	// the request's parameters node. One processor implementation can serve
	// multiple workflow roles distinguished by Context.
	if processor.Config.Context != "" {
		req.Parameters = processor.Config.Context
	}
	if processor.Config.AttachEntity {
		req.Payload = buildEntityPayload(entity)
	}

	resp, err := d.dispatchCalloutToMember(ctx, member, EntityProcessorCalculationRequest, req, requestID, txID, processor.Config.ResponseTimeoutMs, "processor", processor.Name)
	if err != nil {
		return nil, err
	}
	return d.applyProcessorResponse(entity, resp)
}

// applyProcessorResponse extracts updated entity data from the response payload.
func (d *ProcessorDispatcher) applyProcessorResponse(entity *spi.Entity, resp *ProcessingResponse) (*spi.Entity, error) {
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
	tenantID := uc.Tenant.ID

	// FunctionCondition schema: {"type":"function","function":{"name":"...","config":{...}}}
	var parsed struct {
		Function struct {
			Name   string `json:"name"`
			Config struct {
				CalculationNodesTags string `json:"calculationNodesTags"`
				AttachEntity         *bool  `json:"attachEntity"` // nil = default true
				ResponseTimeoutMs    int64  `json:"responseTimeoutMs"`
				// Context — pass-through string surfaced verbatim in the
				// request's parameters node.
				Context string `json:"context"`
			} `json:"config"`
		} `json:"function"`
	}
	if err := json.Unmarshal(criterion, &parsed); err != nil {
		return false, "", fmt.Errorf("invalid criterion JSON: %w", err)
	}

	// attachEntity defaults to true when not explicitly set.
	attachEntity := true
	if parsed.Function.Config.AttachEntity != nil {
		attachEntity = *parsed.Function.Config.AttachEntity
	}

	member := d.registry.FindByTags(tenantID, parsed.Function.Config.CalculationNodesTags)
	if member == nil {
		slog.Warn("no matching calculation member", "pkg", "grpc", "tags", parsed.Function.Config.CalculationNodesTags, "entityId", entity.Meta.ID)
		return false, "", fmt.Errorf("%w: tags %q", ErrNoMatchingMember, parsed.Function.Config.CalculationNodesTags)
	}

	slog.Info("dispatching criteria", "pkg", "grpc", "memberId", member.ID, "criteria", parsed.Function.Name, "entityId", entity.Meta.ID)

	requestID := uuid.UUID(d.uuids.NewTimeUUID()).String()

	req := events.EntityCriteriaCalculationRequestJson{
		ID:            requestID,
		RequestID:     requestID,
		EntityID:      entity.Meta.ID,
		CriteriaID:    parsed.Function.Name,
		CriteriaName:  parsed.Function.Name,
		Target:        events.EntityCriteriaCalculationRequestJsonTarget(target),
		Workflow:      &events.WorkflowInfoJson{ID: workflowName, Name: workflowName},
		Transition:    &events.TransitionInfoJson{ID: transitionName, Name: transitionName},
		TransactionID: &txID,
		Success:       true,
	}
	if processorName != "" {
		req.Processor = &events.ProcessorInfoJson{Name: processorName}
	}
	if parsed.Function.Config.Context != "" {
		req.Parameters = parsed.Function.Config.Context
	}
	if attachEntity {
		req.Payload = buildEntityPayload(entity)
	}

	resp, err := d.dispatchCalloutToMember(ctx, member, EntityCriteriaCalculationRequest, req, requestID, txID, parsed.Function.Config.ResponseTimeoutMs, "criteria", parsed.Function.Name)
	if err != nil {
		return false, "", err
	}
	if resp.Matches != nil {
		return *resp.Matches, resp.Reason, nil
	}
	return false, resp.Reason, nil
}

// DispatchFunction sends a generic Function calculation request (e.g. a
// scheduled-transition timing computation) to a matching calculation member
// and returns its typed result.
func (d *ProcessorDispatcher) DispatchFunction(ctx context.Context, entity *spi.Entity, fn spi.ScheduleFunction, workflowName string, transitionName string, txID string) (contract.FunctionResult, error) {
	uc := spi.MustGetUserContext(ctx)
	tenantID := uc.Tenant.ID

	member := d.registry.FindByTags(tenantID, fn.CalculationNodesTags)
	if member == nil {
		slog.Warn("no matching calculation member", "pkg", "grpc", "tags", fn.CalculationNodesTags, "entityId", entity.Meta.ID)
		return contract.FunctionResult{}, fmt.Errorf("%w: tags %q", ErrNoMatchingMember, fn.CalculationNodesTags)
	}

	slog.Info("dispatching function", "pkg", "grpc", "memberId", member.ID, "function", fn.Name, "entityId", entity.Meta.ID)

	requestID := uuid.UUID(d.uuids.NewTimeUUID()).String()

	req := events.EntityFunctionCalculationRequestJson{
		ID:            requestID,
		RequestID:     requestID,
		EntityID:      entity.Meta.ID,
		FunctionID:    fn.Name,
		FunctionName:  fn.Name,
		Workflow:      events.WorkflowInfoJson{ID: workflowName, Name: workflowName},
		Transition:    &events.TransitionInfoJson{ID: transitionName, Name: transitionName},
		TransactionID: &txID,
		Success:       true,
	}
	if fn.Context != "" {
		req.Parameters = fn.Context
	}
	if fn.AttachEntity {
		req.Payload = buildEntityPayload(entity)
	}

	resp, err := d.dispatchCalloutToMember(ctx, member, EntityFunctionCalculationRequest, req, requestID, txID, fn.ResponseTimeoutMs, "function", fn.Name)
	if err != nil {
		return contract.FunctionResult{}, err
	}
	return contract.FunctionResult{Kind: resp.ResultKind, Value: resp.Result}, nil
}
