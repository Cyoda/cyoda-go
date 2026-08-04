package workflow

import (
	"context"
	"errors"
	"fmt"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/ingest"
)

// ErrProcessorOutputRejected marks data returned by a processor that the model
// rejects — unstorable content, or a shape the model's ChangeLevel does not
// permit.
//
// The cause's text is folded in with %s, deliberately NOT %w. ingest returns
// *common.AppError values carrying 400 BAD_REQUEST, and the handler's
// classifier unwraps any embedded AppError first — so wrapping would surface
// the checker's verdict verbatim and blame the caller for bytes a processor
// produced. Breaking the chain routes this to the WORKFLOW_FAILED branch,
// which is the truthful answer: the transition failed.
var ErrProcessorOutputRejected = errors.New("processor output rejected")

// ErrProcessorOutputInfra marks a server-side failure while checking a
// processor's output — the model store being unreachable, a codec failure, or
// a schema-extension write failing. Handlers map it to a sanitized 5xx.
var ErrProcessorOutputInfra = errors.New("processor output check failed")

// applyProcessorData runs a processor's returned bytes through the same checks
// a client write passes, and only then adopts them onto the entity.
//
// The project's rule is that the engine gets no special rights: a processor may
// extend an entity beyond its model exactly when a client could, i.e. when the
// model's ChangeLevel allows it, and never otherwise. Before this existed a
// processor could write anything at all — content no backend could store
// (surfacing as a 500), or fields the model does not declare (leaving an entity
// the API could read but not accept back on a PUT, and absent from the model
// export).
//
// desc resolves the model descriptor lazily and at most once per transition;
// see modelDescMemo.
//
// Coverage note: this rule deliberately has no cross-backend parity scenario.
// Every check here runs above the SPI, before any store is called, so no
// backend can answer differently — unlike the storability guard, where memory
// and postgres genuinely disagreed about what could be persisted. The contract
// is pinned by internal/e2e/workflow_proc_output_test.go instead.
func (e *Engine) applyProcessorData(ctx context.Context, entity *spi.Entity, desc *modelDescMemo, data []byte) error {
	if err := ingest.RejectUnstorable(data); err != nil {
		return fmt.Errorf("%w: %s", ErrProcessorOutputRejected, err)
	}

	var parsed any
	if err := ingest.DecodeJSONPreservingNumbers(data, &parsed); err != nil {
		return fmt.Errorf("%w: invalid JSON: %s", ErrProcessorOutputRejected, err)
	}

	modelStore, err := e.factory.ModelStore(ctx)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrProcessorOutputInfra, err)
	}

	resolved, err := desc.get(ctx, modelStore, entity)
	if err != nil {
		return err
	}

	if err := ingest.ValidateOrExtend(ctx, modelStore, resolved, parsed); err != nil {
		if errors.Is(err, ingest.ErrInternalSchema) {
			return fmt.Errorf("%w: %s", ErrProcessorOutputInfra, err)
		}
		return fmt.Errorf("%w: %s", ErrProcessorOutputRejected, err)
	}

	entity.Data = data
	return nil
}

// modelDescMemo resolves the entity's model descriptor lazily, at most once per
// transition.
//
// Lazily, because a pipeline whose processors never return data needs no
// descriptor at all — requiring one would impose a new precondition the rule
// does not ask for.
//
// At most once, because that is correctness rather than an optimisation. The
// model cache is not transaction-aware and ExtendSchema invalidates it before
// the surrounding transaction commits, so re-reading between processors could
// repopulate the cache from the pre-extension schema and then reject the next
// processor's legitimate field. One descriptor carried across the pipeline
// cannot see that flap, and a stale-narrow descriptor is harmless because
// extension deltas are additive.
type modelDescMemo struct {
	desc   *spi.ModelDescriptor
	err    error
	loaded bool
}

// get returns the descriptor, loading it on first call.
//
// Fails closed when the model cannot be read, matching how criterion typing
// already treats a missing schema: a required input for judging correctness is
// absent, so reject rather than guess.
func (m *modelDescMemo) get(ctx context.Context, modelStore spi.ModelStore, entity *spi.Entity) (*spi.ModelDescriptor, error) {
	if m.loaded {
		return m.desc, m.err
	}
	m.loaded = true
	desc, err := modelStore.Get(ctx, entity.Meta.ModelRef)
	switch {
	case err != nil:
		m.err = fmt.Errorf("%w: model %s: %s", ErrProcessorOutputInfra, entity.Meta.ModelRef, err)
	case desc == nil:
		m.err = fmt.Errorf("%w: model %s not found", ErrProcessorOutputInfra, entity.Meta.ModelRef)
	default:
		m.desc = desc
	}
	return m.desc, m.err
}
