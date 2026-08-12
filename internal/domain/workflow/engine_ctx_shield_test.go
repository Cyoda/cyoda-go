package workflow

import (
	"context"
	"errors"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// TestFlushAndCommitSegment_ExpiredCtxBeforeCommit_FailsClosed is coverage row
// (a): when the caller's ctx is already expired at the point
// flushAndCommitSegment would commit TX_pre, the commit must not be attempted
// at all — the segment stays uncommitted so a caller-side rollback produces
// the spec D2 "nothing committed" guarantee. Routed through
// ManualTransitionWithIfMatch (attemptTransition/fireTransition), NOT the
// cascade loop, so this isolates flushAndCommitSegment's own pre-commit check
// from the separate check added to cascadeAutomated's loop head.
func TestFlushAndCommitSegment_ExpiredCtxBeforeCommit_FailsClosed(t *testing.T) {
	h := newSegmentGuardHarness(t, "memory")
	h.registerCBDProcessor("segmenter")
	h.makeSegmentTransitionManual()

	_, entryCtx := h.begin(t)
	h.entity.Meta.State = "A"

	expiredCtx, cancel := common.WithRequestTimeout(entryCtx, 1)
	defer cancel()
	<-expiredCtx.Done() // deadline expires before the engine ever runs

	_, err := h.engine.ManualTransitionWithIfMatch(expiredCtx, h.entity, "segment", "")
	if err == nil {
		t.Fatal("expected the pre-commit ctx check to fail the segment commit")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want context.DeadlineExceeded in the error chain, got %v", err)
	}
	if errors.Is(err, ErrCommitBeforeDispatchInfra) {
		t.Fatalf("pre-commit check must reach the handler-seam classifier as a plain deadline chain (408), "+
			"not a ticketed 500: %v", err)
	}
	if len(h.segmentTxIDs) != 0 {
		t.Fatalf("commit-before-dispatch was dispatched despite the pre-commit check failing: %v", h.segmentTxIDs)
	}
}

// TestFlushAndCommitSegment_CommitCtxInterrupted_WrapsErrCommitInterrupted
// closes the Gate-6 gap flagged in review: nothing previously proved the CBD
// commit site (flushAndCommitSegment, via common.ShieldedCommitWithBudget)
// actually applies the common.ErrCommitInterrupted wrap when the commit's
// OWN shielded ctx is what failed it. Real, not simulated: WithCommitBudget
// injects a short budget so the commit's own shielded ctx genuinely expires,
// and the fake Commit blocks on that ctx's Done() channel (deterministic —
// no wall-clock race) before returning ctx.Err() — the same technique
// internal/common's TestShieldedCommitWithBudget_RealExpiry_WrapsErrCommitInterrupted
// uses, and blockingEntityStore uses on the entity-handler side (see
// internal/domain/entity/handler_reqtimeout_test.go).
func TestFlushAndCommitSegment_CommitCtxInterrupted_WrapsErrCommitInterrupted(t *testing.T) {
	h := newSegmentGuardHarness(t, "memory", WithCommitBudget(5*time.Millisecond))
	h.registerCBDProcessor("segmenter")
	h.txMgr.commit = func(ctx context.Context, txID string) error {
		<-ctx.Done()
		return ctx.Err()
	}

	err := h.fireSegment(t, "")
	if err == nil {
		t.Fatal("expected the interrupted commit to fail the segment transition")
	}
	if !errors.Is(err, common.ErrCommitInterrupted) {
		t.Fatalf("want common.ErrCommitInterrupted in the chain, got %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want the original commit error (context.DeadlineExceeded) still reachable, got %v", err)
	}
	if !errors.Is(err, ErrCommitBeforeDispatchInfra) {
		t.Fatalf("want the existing ErrCommitBeforeDispatchInfra classification preserved (spec D2: "+
			"\"a shielded-commit failure keeps its existing classification\" — still a ticketed 500, "+
			"ErrCommitInterrupted only disqualifies a would-be 408 reclassification downstream), got %v", err)
	}
}

// TestCascadeAutomated_ExpiredOriginalDeadline_PostCBD_ContinuesToFinalState
// is coverage row (b). The CBD segment commits while the client's deadline is
// still live. By the time cascadeAutomated's loop reaches the next automated
// transition, the SAME original deadline has genuinely elapsed — but the
// cascade is now running on the post-CBD ctx (context.WithoutCancel-derived,
// via commitAndBeginNextSegment), which carries no deadline of its own. The
// cascade must continue to the entity's final state regardless.
func TestCascadeAutomated_ExpiredOriginalDeadline_PostCBD_ContinuesToFinalState(t *testing.T) {
	h := newSegmentGuardHarness(t, "memory")
	h.registerCBDProcessor("segmenter") // A -[CBD, automated]-> B
	h.wf.States["B"] = spi.StateDefinition{Transitions: []spi.TransitionDefinition{{Name: "advance", Next: "C"}}}
	h.wf.States["C"] = spi.StateDefinition{}

	entryTxID, entryCtx := h.begin(t)
	_ = entryTxID

	// A short-lived client deadline. Still valid when Execute starts the
	// cascade (no I/O happens before the dispatch stub below).
	shortCtx, cancel := common.WithRequestTimeout(entryCtx, 20)
	defer cancel()

	h.dispatchProcessor = func(_ context.Context, _ *spi.Entity, _ spi.ProcessorDefinition, _, _, txID string) (*spi.Entity, error) {
		h.segmentTxIDs = append(h.segmentTxIDs, txID)
		// Block until the ORIGINAL client deadline has genuinely elapsed —
		// deterministic proof that real time passed the deadline before the
		// cascade's continuing iterations run, rather than a fixed sleep.
		<-shortCtx.Done()
		return nil, nil
	}

	res, err := h.engine.Execute(shortCtx, h.entity, "")
	if err != nil {
		t.Fatalf("cascade aborted despite running on the post-CBD (WithoutCancel) ctx: %v", err)
	}
	if h.entity.Meta.State != "C" {
		t.Fatalf("cascade did not continue past the CBD segment to the final state: got %q", h.entity.Meta.State)
	}
	if res == nil || !res.Segmented {
		t.Fatalf("test did not segment; it proves nothing: res=%+v", res)
	}
	if len(h.segmentTxIDs) == 0 || h.segmentTxIDs[len(h.segmentTxIDs)-1] == entryTxID {
		t.Fatal("test did not segment; it proves nothing")
	}
}

// TestCascadeAutomated_PreCancelledCtx_ReturnsError is coverage row (c): a
// cascade whose ctx is already cancelled before the loop starts must return
// an error rather than firing the first automated transition — memory-backend
// uniformity, spec D9. No COMMIT_BEFORE_DISPATCH processor is involved; this
// pins the check cascadeAutomated performs itself, independent of any segment
// boundary.
func TestCascadeAutomated_PreCancelledCtx_ReturnsError(t *testing.T) {
	h := newSegmentGuardHarness(t, "memory")
	// Base harness workflow: A --[automated "segment"]--> B, B has no
	// transitions. No CBD processor registered.

	_, entryCtx := h.begin(t)
	cancelledCtx, cancel := context.WithCancel(entryCtx)
	cancel()

	_, err := h.engine.Execute(cancelledCtx, h.entity, "")
	if err == nil {
		t.Fatal("expected cascadeAutomated to abort on a pre-cancelled ctx")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled in the error chain, got %v", err)
	}
	if h.entity.Meta.State != "A" {
		t.Fatalf("automated transition fired despite the pre-cancelled ctx: entity now in state %q", h.entity.Meta.State)
	}
}
