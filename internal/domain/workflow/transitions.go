package workflow

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// GetAvailableTransitions returns the names of transitions available from the
// entity's CURRENT state in the matching workflow. It fetches the entity by ID,
// then delegates to GetAvailableTransitionsForEntity.
//
// This deliberately reads the current version rather than issuing a
// point-in-time read at the caller's "now": version times are stamped by the
// backend (the database itself, on postgres), so a process-clock "now" compared
// against them is a two-clock comparison that can report an existing entity as
// missing. Callers wanting a genuine historical view pass their own point in
// time to the store directly.
func (e *Engine) GetAvailableTransitions(ctx context.Context, entityID string, modelRef spi.ModelRef) ([]string, error) {
	entityStore, err := e.factory.EntityStore(ctx)
	if err != nil {
		return nil, common.Internal("failed to access entity store", err)
	}

	entity, err := entityStore.Get(ctx, entityID)
	if err != nil {
		return nil, common.Operational(http.StatusNotFound, common.ErrCodeEntityNotFound,
			fmt.Sprintf("entity %s not found", entityID))
	}

	return e.GetAvailableTransitionsForEntity(ctx, entity)
}

// GetAvailableTransitionsForEntity returns transition names for a pre-fetched entity.
// Use this when the caller already has the entity to avoid a redundant store lookup.
func (e *Engine) GetAvailableTransitionsForEntity(ctx context.Context, entity *spi.Entity) ([]string, error) {
	wfStore, err := e.factory.WorkflowStore(ctx)
	if err != nil {
		return nil, common.Internal("failed to access workflow store", err)
	}

	workflows, err := wfStore.Get(ctx, entity.Meta.ModelRef)
	if err != nil && errors.Is(err, spi.ErrNotFound) {
		workflows = nil
	} else if err != nil {
		return nil, common.Internal("failed to load workflows", err)
	}

	if len(workflows) == 0 {
		common.AddWarning(ctx, "no imported workflow matched — using default workflow")
		workflows = e.defaultWorkflows
	}

	// Find matching workflow. Use a no-op audit approach since we're just querying.
	smStore, _ := e.factory.StateMachineAuditStore(ctx)
	selectedWF, err := e.selectWorkflow(ctx, workflows, entity, smStore, "")
	if err != nil {
		// No workflow matched — use default
		if len(e.defaultWorkflows) > 0 {
			selectedWF = &e.defaultWorkflows[0]
		} else {
			return []string{}, nil
		}
	}

	stateDef, ok := selectedWF.States[entity.Meta.State]
	if !ok {
		return []string{}, nil
	}

	names := make([]string, len(stateDef.Transitions))
	for i, t := range stateDef.Transitions {
		names[i] = t.Name
	}
	return names, nil
}
