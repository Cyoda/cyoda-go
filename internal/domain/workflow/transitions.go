package workflow

import (
	"context"
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
//
// Resolution goes through the engine's single selection path, so the answer
// names the transitions of the definition the entity is actually bound to —
// the same one a subsequent ManualTransition will run.
//
// Two properties matter here and are pinned by tests:
//   - The query records no audit events. Selection events belong to the
//     transaction executing the entity, and a read has none to key them to.
//   - A criterion that cannot be evaluated fails the request. Answering with
//     some other workflow's transitions would be a wrong-but-available
//     result (.claude/rules/correctness-over-availability.md); a criterion
//     that merely does not MATCH still resolves to the default workflow,
//     which is selection working as documented, not a degradation.
func (e *Engine) GetAvailableTransitionsForEntity(ctx context.Context, entity *spi.Entity) ([]string, error) {
	selectedWF, err := e.resolveWorkflowForQuery(ctx, entity)
	if err != nil {
		return nil, err
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
