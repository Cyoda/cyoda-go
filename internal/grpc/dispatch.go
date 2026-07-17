package grpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/logging"
)

// ErrNoMatchingMember is returned when no calculation member is registered for
// the requested tags. Callers (e.g. ClusterDispatcher) test for this sentinel
// via errors.Is rather than string matching.
var ErrNoMatchingMember = errors.New("no matching calculation member")

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

	ce, err := NewCloudEvent(EntityProcessorCalculationRequest, req)
	if err != nil {
		return nil, fmt.Errorf("failed to build processor cloud event: %w", err)
	}
	AttachAuthContext(ctx, ce)
	AttachTxToken(ce, d.resolveTxToken(ctx, txID))

	ceData := ce.GetTextData()
	slog.Debug("dispatch request", "pkg", "grpc", "requestId", requestID, "payload", logging.PayloadPreview([]byte(ceData), 200))

	ch := member.TrackRequest(requestID)

	if err := member.Send(ce); err != nil {
		slog.Error("failed to send to member", "pkg", "grpc", "memberId", member.ID, "error", err)
		return nil, fmt.Errorf("failed to send processor request: %w", err)
	}

	timeoutMs := processor.Config.ResponseTimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = defaultResponseTimeoutMs
	}

	timeout := time.Duration(timeoutMs) * time.Millisecond

	select {
	case resp := <-ch:
		// Propagate warnings from processor to request diagnostics.
		if resp != nil {
			for _, w := range resp.Warnings {
				common.AddWarning(ctx, fmt.Sprintf("processor %s: %s", processor.Name, w))
			}
		}
		if resp == nil || !resp.Success {
			errMsg := "processor returned failure"
			if resp != nil && resp.Error != "" {
				errMsg = resp.Error
				common.AddError(ctx, fmt.Sprintf("processor %s: %s", processor.Name, errMsg))
			}
			return nil, fmt.Errorf("processor dispatch failed: %s", errMsg)
		}
		slog.Info("processor completed", "pkg", "grpc", "memberId", member.ID, "processor", processor.Name, "requestId", requestID, "success", true)
		return d.applyProcessorResponse(entity, resp)
	case <-time.After(timeout):
		slog.Error("dispatch timeout", "pkg", "grpc", "processor", processor.Name, "requestId", requestID, "timeout", timeout)
		return nil, fmt.Errorf("processor dispatch timed out after %dms", timeoutMs)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
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

	ce, err := NewCloudEvent(EntityCriteriaCalculationRequest, req)
	if err != nil {
		return false, "", fmt.Errorf("failed to build criteria cloud event: %w", err)
	}
	AttachAuthContext(ctx, ce)
	AttachTxToken(ce, d.resolveTxToken(ctx, txID))

	ceData := ce.GetTextData()
	slog.Debug("dispatch request", "pkg", "grpc", "requestId", requestID, "payload", logging.PayloadPreview([]byte(ceData), 200))

	ch := member.TrackRequest(requestID)

	if err := member.Send(ce); err != nil {
		slog.Error("failed to send to member", "pkg", "grpc", "memberId", member.ID, "error", err)
		return false, "", fmt.Errorf("failed to send criteria request: %w", err)
	}

	timeoutMs := parsed.Function.Config.ResponseTimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = defaultResponseTimeoutMs
	}

	timeout := time.Duration(timeoutMs) * time.Millisecond

	select {
	case resp := <-ch:
		// Propagate warnings from criteria to request diagnostics.
		if resp != nil {
			for _, w := range resp.Warnings {
				common.AddWarning(ctx, fmt.Sprintf("criteria %s: %s", parsed.Function.Name, w))
			}
		}
		if resp == nil || !resp.Success {
			errMsg := "criteria returned failure"
			if resp != nil && resp.Error != "" {
				errMsg = resp.Error
				common.AddError(ctx, fmt.Sprintf("criteria %s: %s", parsed.Function.Name, errMsg))
			}
			return false, "", fmt.Errorf("criteria dispatch failed: %s", errMsg)
		}
		slog.Info("criteria completed", "pkg", "grpc", "memberId", member.ID, "criteria", parsed.Function.Name, "requestId", requestID, "success", true)
		if resp.Matches != nil {
			return *resp.Matches, resp.Reason, nil
		}
		return false, resp.Reason, nil
	case <-time.After(timeout):
		slog.Error("dispatch timeout", "pkg", "grpc", "criteria", parsed.Function.Name, "requestId", requestID, "timeout", timeout)
		return false, "", fmt.Errorf("criteria dispatch timed out after %dms", timeoutMs)
	case <-ctx.Done():
		return false, "", ctx.Err()
	}
}
