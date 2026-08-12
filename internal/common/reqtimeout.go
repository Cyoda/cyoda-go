package common

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"time"
)

// reqTimeoutKey marks a context whose deadline was attached from a
// client-supplied transactionTimeoutMillis / timeoutMillis param. The marker is
// what lets ClassifyRequestTimeout distinguish OUR deadline from every other
// deadline source (postgres statement_timeout stays 500, dispatch time.After
// stays 503) — spec D8.
type reqTimeoutKey struct{}

const maxRequestTimeoutMillis = math.MaxInt64 / int64(time.Millisecond)

// ValidateRequestTimeoutMillis returns a 400 AppError unless 1 <= millis and
// time.Duration(millis)*time.Millisecond does not overflow.
func ValidateRequestTimeoutMillis(millis int64) *AppError {
	if millis < 1 || millis > maxRequestTimeoutMillis {
		return Operational(http.StatusBadRequest, ErrCodeBadRequest,
			fmt.Sprintf("timeout must be a positive number of milliseconds not exceeding %d", maxRequestTimeoutMillis))
	}
	return nil
}

// WithRequestTimeout attaches a feature-owned deadline. Caller must have validated millis.
func WithRequestTimeout(ctx context.Context, millis int64) (context.Context, context.CancelFunc) {
	ctx = context.WithValue(ctx, reqTimeoutKey{}, true)
	return context.WithTimeout(ctx, time.Duration(millis)*time.Millisecond)
}

// HasRequestTimeout reports whether ctx carries a feature-attached deadline
// marker. The stored value must be exactly true — CommitContext clears the
// marker by re-stamping the key with nil (context.WithValue provides no way
// to delete a key), so a present-but-nil value must read as absent.
func HasRequestTimeout(ctx context.Context) bool {
	v, _ := ctx.Value(reqTimeoutKey{}).(bool)
	return v
}

// ErrCommitInterrupted marks an error born from a commit ATTEMPT whose own
// shielded context (common.CommitContext) was itself interrupted — its
// commitBudget expired, or it was otherwise cancelled — before the commit
// call returned. The commit's true outcome is unknowable in that case: it
// may have landed, may not have. Spec D2 treats an interrupted commit as an
// in-doubt outcome, never a rollback-able one, so an error carrying this
// marker must NEVER be classified as the client's 408 — 408 promises
// "nothing was committed", a promise this error cannot back. Commit sites
// (txScope.commitOwned, the workflow engine's flushAndCommitSegment) wrap
// with this sentinel only when the commit's OWN context shows an error at
// return time; a clean commit failure on a still-live shielded ctx (e.g.
// spi.ErrConflict) is unaffected and keeps its existing classification.
var ErrCommitInterrupted = errors.New("commit attempt interrupted")

// WrapIfCommitInterrupted wraps err with ErrCommitInterrupted when commitCtx
// — the context returned by CommitContext, passed to the commit call — shows
// an error (commitCtx.Err() != nil) at the point the commit call returned:
// its own commitBudget expired, or it was otherwise interrupted, while the
// commit was genuinely in flight. That is an in-doubt outcome (spec D2), so
// the error must never be classified as the client's clean 408 downstream,
// even if the client's own deadline also happens to have expired around the
// same time (see ErrCommitInterrupted). A clean commit failure while
// commitCtx is still live (e.g. spi.ErrConflict) passes through unwrapped
// and keeps its existing classification (409, etc.) — errors.Is still finds
// it through the wrap on the interrupted path too, since Go's multi-%w
// preserves every branch of the chain.
//
// err == nil passes through unchanged (no-op). Shared by txScope.commitOwned
// and the workflow engine's flushAndCommitSegment — the two sites that
// invoke a commit on a CommitContext-derived ctx.
func WrapIfCommitInterrupted(commitCtx context.Context, err error) error {
	if err != nil && commitCtx.Err() != nil {
		return fmt.Errorf("%w: %w", ErrCommitInterrupted, err)
	}
	return err
}

// CommitBudget is the production budget ShieldedCommit (and CommitContext)
// bound a commit call by. Exported so a call site that wants to declare its
// own default in terms of this one (e.g. workflow.Engine's commitBudget
// field, overridable via WithCommitBudget for tests) doesn't have to
// duplicate the magic number.
const CommitBudget = commitBudget

// ShieldedCommit runs commit on a CommitContext-derived ctx — WithoutCancel
// plus CommitBudget — and wraps any resulting error per WrapIfCommitInterrupted's
// rule. This is THE single place the "run a commit shielded, then decide
// whether it was interrupted" logic lives; both commit call sites
// (txScope.commitOwned in internal/domain/entity, and the workflow engine's
// flushAndCommitSegment) call this instead of duplicating the CommitContext
// derivation + wrap-decision pair.
func ShieldedCommit(ctx context.Context, commit func(context.Context) error) error {
	return ShieldedCommitWithBudget(ctx, CommitBudget, commit)
}

// ShieldedCommitWithBudget is ShieldedCommit with an injectable budget. The
// production commitBudget (30s) makes ShieldedCommit's own wrap path
// impractical to exercise for real in a fast test — this lets a test inject
// a short budget instead and drive the actual mechanism (a commit that
// blocks until its ctx is genuinely Done) rather than simulating the
// outcome. Production call sites use ShieldedCommit; only tests should need
// this directly.
func ShieldedCommitWithBudget(ctx context.Context, budget time.Duration, commit func(context.Context) error) error {
	commitCtx, cancel := commitContextWithBudget(ctx, budget)
	defer cancel()
	err := commit(commitCtx)
	return WrapIfCommitInterrupted(commitCtx, err)
}

// ClassifyRequestTimeout maps err to Operational(408, code).AsRetryable()
// only when ALL of the following hold (spec D2 pinned rule, ANDed — never
// any one alone):
//   - the marker is on ctx (this is OUR feature deadline, not some other
//     deadline source — postgres statement_timeout stays 500, dispatch
//     time.After stays 503);
//   - the chain — unwrapping *AppError causes — contains
//     context.DeadlineExceeded (context.Canceled never matches);
//   - ctx itself is currently expired with context.DeadlineExceeded (the
//     ours-actually-expired conjunct: a DeadlineExceeded elsewhere in the
//     chain — e.g. a nested postgres pool-acquire deadline — must not
//     borrow the marker's 408 while the client's own deadline is still
//     live);
//   - err does not carry ErrCommitInterrupted (disqualified first: a
//     commit-attempt-interrupted error is in-doubt and must never present
//     as the client's clean "nothing was committed" 408, even when the
//     client's own deadline also happens to have expired by coincidence).
func ClassifyRequestTimeout(ctx context.Context, err error, code string) *AppError {
	if err == nil || !HasRequestTimeout(ctx) {
		return nil
	}
	if errors.Is(err, ErrCommitInterrupted) {
		return nil
	}
	if !chainHasDeadlineExceeded(err) {
		return nil
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil
	}
	return Operational(http.StatusRequestTimeout, code,
		"operation exceeded the client-requested timeout before completing; nothing was committed").
		AsRetryable()
}

// chainHasDeadlineExceeded reports whether context.DeadlineExceeded appears
// anywhere in err's chain. A single errors.Is call already traverses an
// *AppError cause — AppError.Unwrap returns e.Err — so no separate
// errors.As(*AppError) branch is needed.
func chainHasDeadlineExceeded(err error) bool {
	return errors.Is(err, context.DeadlineExceeded)
}
