package grpc

import (
	"encoding/json"
	"fmt"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// CalloutKind is which of the three callouts a Callout is.
type CalloutKind int

const (
	ProcessorCallout CalloutKind = iota
	CriteriaCallout
	FunctionCallout
)

// String is the word used for the kind in diagnostics a client sees
// ("processor charge: …") and in the hand-over between pnodes.
func (k CalloutKind) String() string {
	switch k {
	case ProcessorCallout:
		return "processor"
	case CriteriaCallout:
		return "criteria"
	case FunctionCallout:
		return "function"
	default:
		return fmt.Sprintf("CalloutKind(%d)", int(k))
	}
}

// CalloutResult is what a cnode answered; the field for the callout's kind is
// set.
type CalloutResult struct {
	Entity   *spi.Entity             // processor
	Matches  bool                    // criterion
	Reason   string                  // criterion
	Function contract.FunctionResult // function
}

// CalloutSource is what a Callout was built from: what another pnode needs to
// build the same Callout with the same builder.
type CalloutSource struct {
	Entity                       *spi.Entity
	WorkflowName, TransitionName string
	Processor                    *spi.ProcessorDefinition // ProcessorCallout
	Criterion                    json.RawMessage          // CriteriaCallout
	Target, ProcessorName        string                   // CriteriaCallout
	Function                     *spi.ScheduleFunction    // FunctionCallout
}

// Callout is one processor, criterion or function request, in the form the
// local procedure tries on cnodes. Build it with NewProcessorCallout,
// NewCriteriaCallout or NewFunctionCallout; the caller then fills RequestID,
// AnswerLimit and OwnerNodeID.
type Callout struct {
	Kind     CalloutKind
	Name     string // the configured processor, criterion or function name
	TenantID spi.TenantID
	Tags     string // calculationNodesTags, comma separated
	// ResponseTimeoutMs is the value stored in the workflow; the owner turns it
	// into AnswerLimit with ResolveAnswerLimit.
	ResponseTimeoutMs int64
	TxID              string // empty when the callout runs outside a transaction
	EntityID          string

	// RequestID is sent on every try, as the request's id and requestId.
	RequestID string
	// AnswerLimit is how long one cnode is given to take the work and answer.
	AnswerLimit time.Duration
	// RepeatSafe: the work may be given to another cnode after a hand-off.
	RepeatSafe bool
	// OwnerNodeID is the pnode that holds the transaction; a cnode's callbacks
	// are routed there.
	OwnerNodeID string
	// Number gives the fencing number of each try. RunLocal calls Next once
	// before every try, before it mints that try's pass.
	Number TryNumberer
	// Outer names every enclosing callout, for a callout made from inside a
	// callback; it is copied into every pass.
	Outer []token.Pair
	// Source is what the callout was built from. A hand-over sends it, and the
	// pnode that receives it builds the same Callout with the same builder.
	Source CalloutSource

	eventType    string
	buildRequest func(requestID string) any
	mapResponse  func(resp *ProcessingResponse) (CalloutResult, error)
}

// NewProcessorCallout builds the callout for an externalized processor.
// RepeatSafe is left false: whether a processor may be repeated is its
// author's declaration, which the owner reads from the processor's config.
func NewProcessorCallout(tenantID spi.TenantID, entity *spi.Entity, processor spi.ProcessorDefinition, workflowName, transitionName, txID string) Callout {
	return Callout{
		Kind:              ProcessorCallout,
		Name:              processor.Name,
		TenantID:          tenantID,
		Tags:              processor.Config.CalculationNodesTags,
		ResponseTimeoutMs: processor.Config.ResponseTimeoutMs,
		TxID:              txID,
		EntityID:          entity.Meta.ID,
		Source:            CalloutSource{Entity: entity, WorkflowName: workflowName, TransitionName: transitionName, Processor: &processor},
		eventType:         EntityProcessorCalculationRequest,
		buildRequest: func(requestID string) any {
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
			// ProcessorConfig.Context is a pass-through string surfaced verbatim
			// in the request's parameters node. One processor implementation can
			// serve multiple workflow roles distinguished by Context.
			if processor.Config.Context != "" {
				req.Parameters = processor.Config.Context
			}
			if processor.Config.AttachEntity {
				req.Payload = buildEntityPayload(entity)
			}
			return req
		},
		mapResponse: func(resp *ProcessingResponse) (CalloutResult, error) {
			updated, err := applyProcessorResponse(entity, resp)
			if err != nil {
				return CalloutResult{}, err
			}
			return CalloutResult{Entity: updated}, nil
		},
	}
}

// NewCriteriaCallout builds the callout for a FUNCTION criterion. A criterion
// computes and does not write, so it is repeat-safe by rule. A criterion that
// does not parse would fail identically on any cnode: Terminal.
func NewCriteriaCallout(tenantID spi.TenantID, entity *spi.Entity, criterion json.RawMessage, target, workflowName, transitionName, processorName, txID string) (Callout, *contract.CalloutFailure) {
	// One parser serves import validation and dispatch alike.
	fn, err := contract.ParseCriterionFunction(criterion)
	if err != nil {
		wrapped := fmt.Errorf("invalid criterion JSON: %w", err)
		return Callout{}, &contract.CalloutFailure{Kind: contract.Terminal, Message: wrapped.Error(), Err: wrapped}
	}
	name := fn.Name
	config := fn.Config
	attachEntity := config.AttachEntity == nil || *config.AttachEntity

	return Callout{
		Kind:              CriteriaCallout,
		Name:              name,
		TenantID:          tenantID,
		Tags:              config.CalculationNodesTags,
		ResponseTimeoutMs: config.ResponseTimeoutMs,
		TxID:              txID,
		EntityID:          entity.Meta.ID,
		RepeatSafe:        true,
		Source: CalloutSource{Entity: entity, WorkflowName: workflowName, TransitionName: transitionName,
			Criterion: criterion, Target: target, ProcessorName: processorName},
		eventType: EntityCriteriaCalculationRequest,
		buildRequest: func(requestID string) any {
			req := events.EntityCriteriaCalculationRequestJson{
				ID:            requestID,
				RequestID:     requestID,
				EntityID:      entity.Meta.ID,
				CriteriaID:    name,
				CriteriaName:  name,
				Target:        events.EntityCriteriaCalculationRequestJsonTarget(target),
				Workflow:      &events.WorkflowInfoJson{ID: workflowName, Name: workflowName},
				Transition:    &events.TransitionInfoJson{ID: transitionName, Name: transitionName},
				TransactionID: &txID,
				Success:       true,
			}
			if processorName != "" {
				req.Processor = &events.ProcessorInfoJson{Name: processorName}
			}
			if config.Context != "" {
				req.Parameters = config.Context
			}
			if attachEntity {
				req.Payload = buildEntityPayload(entity)
			}
			return req
		},
		mapResponse: func(resp *ProcessingResponse) (CalloutResult, error) {
			return CalloutResult{Matches: resp.Matches != nil && *resp.Matches, Reason: resp.Reason}, nil
		},
	}, nil
}

// NewFunctionCallout builds the callout for a generic Function (e.g. a
// scheduled transition's timing computation). Repeat-safe by rule.
func NewFunctionCallout(tenantID spi.TenantID, entity *spi.Entity, fn spi.ScheduleFunction, workflowName, transitionName, txID string) Callout {
	return Callout{
		Kind:              FunctionCallout,
		Name:              fn.Name,
		TenantID:          tenantID,
		Tags:              fn.CalculationNodesTags,
		ResponseTimeoutMs: fn.ResponseTimeoutMs,
		TxID:              txID,
		EntityID:          entity.Meta.ID,
		RepeatSafe:        true,
		Source:            CalloutSource{Entity: entity, WorkflowName: workflowName, TransitionName: transitionName, Function: &fn},
		eventType:         EntityFunctionCalculationRequest,
		buildRequest: func(requestID string) any {
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
			return req
		},
		mapResponse: func(resp *ProcessingResponse) (CalloutResult, error) {
			return CalloutResult{Function: contract.FunctionResult{Kind: resp.ResultKind, Value: resp.Result}}, nil
		},
	}
}
