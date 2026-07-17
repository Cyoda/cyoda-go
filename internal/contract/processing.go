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

// ExternalProcessingService dispatches processor execution and criteria evaluation
// to external calculation nodes.
type ExternalProcessingService interface {
	DispatchProcessor(ctx context.Context, entity *spi.Entity, processor spi.ProcessorDefinition, workflowName string, transitionName string, txID string) (*spi.Entity, error)
	// DispatchCriteria evaluates a FUNCTION criterion on an external node.
	// reason carries the compute node's explanation for the result (empty
	// when none was supplied); it is only consumed on a matches=false
	// rejection.
	DispatchCriteria(ctx context.Context, entity *spi.Entity, criterion json.RawMessage, target string, workflowName string, transitionName string, processorName string, txID string) (matches bool, reason string, err error)
}
