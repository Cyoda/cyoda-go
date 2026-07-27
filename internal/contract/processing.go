package contract

import (
	"context"
	"encoding/json"
	"errors"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// ErrNoMatchingMember is returned by ExternalProcessingService implementations
// when no calculation member is registered for the requested tags. Callers
// test for this sentinel via errors.Is rather than string matching.
//
// Defined here (rather than in internal/grpc, where the dispatcher that
// mints it lives) so error-classification code in internal/domain/entity can
// reference it without an import cycle: internal/grpc already imports
// internal/domain/entity (server.go, entity.go, search.go wire the gRPC
// handlers over the entity service), so internal/domain/entity cannot import
// internal/grpc back. internal/contract is a leaf package with no cyoda-go
// dependencies, safe for both sides to import.
var ErrNoMatchingMember = errors.New("no matching calculation member")

// ErrAuthContextUnavailable is joined into the error AttachAuthContext
// (internal/grpc) returns when it cannot faithfully populate a dispatch
// CloudEvent's Auth Context extension: no UserContext on ctx, an unset or
// unrecognized principal Kind, or a nil CloudEvent. None of these can ever
// originate from client-supplied input — the client does not control
// dispatch-path UserContext construction — so they are server-side
// conditions (missed constructor / missed cross-node context forwarding /
// misconfiguration), not a bad request.
//
// error-classification code in internal/domain/entity matches this sentinel
// via errors.Is to map the failure to a sanitized 5xx (ticket UUID, no
// principal id or internal detail in the client response) instead of the
// classifyWorkflowError GENERIC fallback, which would otherwise surface it
// as 400 WORKFLOW_FAILED echoing the raw message.
//
// Defined here (rather than in internal/grpc, where AttachAuthContext lives)
// for the same import-cycle reason as ErrNoMatchingMember above:
// internal/grpc already imports internal/domain/entity, so
// internal/domain/entity cannot import internal/grpc back. internal/contract
// is a leaf package safe for both sides to import.
var ErrAuthContextUnavailable = errors.New("auth context unavailable for dispatch")

// FunctionResult holds the outcome of a generic Function callout dispatch —
// used, e.g., by scheduled-transition timing where a compute node computes a
// fire time (kind "Schedule") rather than mutating entity data or evaluating
// a boolean criterion.
type FunctionResult struct {
	// Kind is the function-response discriminator string
	// (EntityFunctionCalculationResponse.resultKind), e.g. "Schedule".
	Kind string
	// Value is the raw JSON result payload
	// (EntityFunctionCalculationResponse.result).
	Value json.RawMessage
}

// ExternalProcessingService dispatches processor execution and criteria evaluation
// to external calculation nodes.
type ExternalProcessingService interface {
	DispatchProcessor(ctx context.Context, entity *spi.Entity, processor spi.ProcessorDefinition, workflowName string, transitionName string, txID string) (*spi.Entity, error)
	// DispatchCriteria evaluates a FUNCTION criterion on an external node.
	// reason carries the compute node's explanation for the result (empty
	// when none was supplied); it is only consumed on a matches=false
	// rejection.
	DispatchCriteria(ctx context.Context, entity *spi.Entity, criterion json.RawMessage, target string, workflowName string, transitionName string, processorName string, txID string) (matches bool, reason string, err error)
	// DispatchFunction sends a generic Function callout (e.g. a scheduled-
	// transition timing computation) to an external node and returns its
	// typed result.
	DispatchFunction(ctx context.Context, entity *spi.Entity, fn spi.ScheduleFunction, workflowName string, transitionName string, txID string) (FunctionResult, error)
}
