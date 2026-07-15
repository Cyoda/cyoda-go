package contract

import (
	"context"
	"encoding/json"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

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
