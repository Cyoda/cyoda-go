package callout

import (
	"context"
	"encoding/json"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

// DispatchProcessor runs an externalized processor. It may be given to another
// cnode after a hand-off only if its author declared it idempotent.
func (c *Coordinator) DispatchProcessor(ctx context.Context, entity *spi.Entity, processor spi.ProcessorDefinition, workflowName string, transitionName string, txID string) (*spi.Entity, error) {
	uc := spi.MustGetUserContext(ctx)
	call := internalgrpc.NewProcessorCallout(uc.Tenant.ID, entity, processor, workflowName, transitionName, txID)
	call.RepeatSafe = processor.Config.Idempotent
	res, err := c.run(ctx, call)
	if err != nil {
		return nil, err
	}
	return res.Entity, nil
}

// DispatchCriteria evaluates a FUNCTION criterion. A criterion computes and
// does not write, so it is repeat-safe by rule.
func (c *Coordinator) DispatchCriteria(ctx context.Context, entity *spi.Entity, criterion json.RawMessage, target string, workflowName string, transitionName string, processorName string, txID string) (bool, string, error) {
	uc := spi.MustGetUserContext(ctx)
	call, failure := internalgrpc.NewCriteriaCallout(uc.Tenant.ID, entity, criterion, target, workflowName, transitionName, processorName, txID)
	if failure != nil {
		return false, "", failure
	}
	res, err := c.run(ctx, call)
	if err != nil {
		return false, "", err
	}
	return res.Matches, res.Reason, nil
}

// DispatchFunction runs a generic Function callout (a scheduled transition's
// timing computation). Repeat-safe by rule.
func (c *Coordinator) DispatchFunction(ctx context.Context, entity *spi.Entity, fn spi.ScheduleFunction, workflowName string, transitionName string, txID string) (contract.FunctionResult, error) {
	uc := spi.MustGetUserContext(ctx)
	call := internalgrpc.NewFunctionCallout(uc.Tenant.ID, entity, fn, workflowName, transitionName, txID)
	res, err := c.run(ctx, call)
	if err != nil {
		return contract.FunctionResult{}, err
	}
	return res.Function, nil
}
