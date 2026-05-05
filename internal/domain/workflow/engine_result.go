package workflow

import (
	"context"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// EngineResult is the cyoda-go-internal cascade result. It embeds the SPI's
// ExecutionResult (returned to public consumers via the engine's public API)
// and adds FinalCtx / FinalTxID / Segmented — the transaction context, txID,
// and segmentation flag at the moment Execute / ManualTransition / Loopback
// returns.
//
// For cascades that segment via COMMIT_BEFORE_DISPATCH, FinalTxID is TX_post
// (or the most recent segment's TX) and Segmented is true. For non-segmenting
// cascades, FinalTxID is the caller's input txID and Segmented is false. The
// handler is responsible for committing FinalTxID.
type EngineResult struct {
	*spi.ExecutionResult
	// FinalCtx is the transaction context open at the moment the engine
	// returns; the handler must use it (not the caller's original ctx) for
	// any follow-on EntityStore writes and for the final Commit.
	//
	// For CBD segments where the segmenter elected `startNewTxOnDispatch=false`,
	// FinalCtx is wrapped via `context.WithoutCancel` — callers issuing
	// follow-on operations are decoupled from the original context's
	// cancellation. This is intentional: the engine has committed the TX
	// and downstream cleanup (CompareAndSave + Commit) must complete even
	// if the caller's request was cancelled mid-dispatch.
	FinalCtx context.Context
	// FinalTxID is the still-open TX at engine return — the caller's input
	// txID for non-segmenting cascades, or TX_post (the latest segment's
	// txID) for segmenting cascades.
	FinalTxID string
	// Segmented reports whether this cascade segment-committed mid-flight
	// (typically via COMMIT_BEFORE_DISPATCH). When true, FinalTxID differs
	// from the cascade-entry TX — the engine has already committed TX_pre
	// and the handler should NOT re-apply the caller's IfMatch precondition
	// (the engine consumed it at first-segment flush). When false, FinalTxID
	// equals the cascade-entry TX and the handler still owns IfMatch.
	Segmented bool
}
