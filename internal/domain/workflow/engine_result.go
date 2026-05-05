package workflow

import (
	"context"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// EngineResult is the cyoda-go-internal cascade result. It embeds the SPI's
// ExecutionResult (returned to public consumers via the engine's public API)
// and adds FinalCtx / FinalTxID — the transaction context and txID open at
// the moment Execute / ManualTransition / Loopback returns.
//
// For cascades that segment via COMMIT_BEFORE_DISPATCH, FinalTxID is TX_post
// (or the most recent segment's TX). For non-segmenting cascades, it is the
// caller's input txID. The handler is responsible for committing FinalTxID.
type EngineResult struct {
	*spi.ExecutionResult
	FinalCtx  context.Context
	FinalTxID string
}
