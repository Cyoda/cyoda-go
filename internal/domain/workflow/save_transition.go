package workflow

import (
	"context"

	spi "github.com/cyoda/cyoda-go-spi"
)

// LoopbackTransition is the transition name a request that names none
// records: a create, an update without a transition.
const LoopbackTransition = "loopback"

// saveTransitionKey carries the transition the request named through a run.
type saveTransitionKey struct{}

// withSaveTransition returns ctx carrying name, the transition the request
// named. Every save the engine makes for the request writes it. Every context
// the run derives keeps it, the transactions a COMMIT_BEFORE_DISPATCH boundary
// begins included: they are begun from the run's context.
func withSaveTransition(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, saveTransitionKey{}, name)
}

// saveTransitionFrom returns the transition name ctx carries, or "".
func saveTransitionFrom(ctx context.Context) string {
	name, _ := ctx.Value(saveTransitionKey{}).(string)
	return name
}

// stampedForSave returns the entity a save writes for the request: a shallow
// copy of e whose Meta.TransitionForLatestSave is the request's transition
// name. It never changes e: what the run evaluates (workflow selection,
// criteria) keeps the name of the version the request started from. The copy
// shares e's Data; a store copies what it saves.
func stampedForSave(ctx context.Context, e *spi.Entity) *spi.Entity {
	stamped := *e
	stamped.Meta.TransitionForLatestSave = saveTransitionFrom(ctx)
	return &stamped
}
