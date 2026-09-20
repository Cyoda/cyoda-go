// Package callout holds the owner's loop: the one place that decides how often,
// where and for how long a processor, criterion or function callout is tried.
// The pnode that holds the operation's transaction — the owner — runs it; a
// pnode that receives a hand-over runs the local procedure only and never this.
package callout

import (
	"context"
	"errors"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/fence"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

// retryPolicyNone is the retryPolicy value that selects a single try. Every
// other accepted value — FIXED, or none — selects 1 + Config.FixedNumRetries.
const retryPolicyNone = "NONE"

// Config is the part of the server configuration the owner's loop reads.
type Config struct {
	// SelfNodeID is this pnode's id: the owner named in every pass.
	SelfNodeID string
	// FixedNumRetries is the number of tries after the first under retryPolicy
	// FIXED or none.
	FixedNumRetries int
}

// Coordinator implements contract.ExternalProcessingService on the owner.
//
// Every entry point must be called with the transaction's write lock suspended,
// never held: the fence's Advance before each try, and the callout's end, wait
// for that lock, so a Coordinator that held it would wait for itself. The
// engine suspends it around every dispatch site.
type Coordinator struct {
	local *internalgrpc.ProcessorDispatcher
	// members is this pnode's cnode registry: the change signal a callout that
	// found no cnode waits on.
	members *internalgrpc.MemberRegistry
	fence   *fence.Fence
	uuids   spi.UUIDGenerator
	cfg     Config
}

// New builds the owner's loop.
func New(local *internalgrpc.ProcessorDispatcher, members *internalgrpc.MemberRegistry, f *fence.Fence, uuids spi.UUIDGenerator, cfg Config) *Coordinator {
	return &Coordinator{local: local, members: members, fence: f, uuids: uuids, cfg: cfg}
}

// triesFor is the number of tries retryPolicy selects.
func (c *Coordinator) triesFor(retryPolicy string) int {
	if retryPolicy == retryPolicyNone {
		return 1
	}
	return 1 + c.cfg.FixedNumRetries
}

// majorCounter is the one fencing counter of a callout. Everything that gives
// the callout's work to a cnode draws from it, so the number rises — and the
// earlier cnode is shut out, and its joined request in progress waited for —
// before the work is given to anyone else. It is used from the goroutine that
// runs the callout only.
type majorCounter struct {
	fence     *fence.Fence
	calloutID string
	major     uint32
}

func (n *majorCounter) Next() (uint32, uint32) {
	n.major++
	n.fence.Advance(n.calloutID, n.major)
	return n.major, 0
}

// progress is the running account of one callout.
type progress struct {
	triesLeft int
	attempts  []contract.CalloutAttempt
	// lastTried is the failure of the latest try that was actually made. A
	// pass that found no cnode does not overwrite it.
	lastTried *contract.CalloutFailure
	// noCnode is what the latest pass that made no try reported.
	noCnode *contract.CalloutFailure
	stats   contract.CalloutStats
}

// run is the owner's loop. call comes from one of the three builders; run
// fills in what the owner decides.
func (c *Coordinator) run(ctx context.Context, call internalgrpc.Callout) (internalgrpc.CalloutResult, error) {
	limit, failure := c.local.ResolveAnswerLimit(call.ResponseTimeoutMs)
	if failure != nil {
		return internalgrpc.CalloutResult{}, failure
	}
	tries := c.triesFor(call.RetryPolicy)
	// The pairs this chain was admitted under, if it is a callback of an outer
	// callout: the callout is begun under them, and every pass it mints carries
	// them. The pass's type for a pair is the fence's own.
	outer := fence.Pairs(ctx)
	call.RequestID = uuid.UUID(c.uuids.NewTimeUUID()).String()
	call.AnswerLimit = limit
	call.OwnerNodeID = c.cfg.SelfNodeID
	call.Outer = outer
	number := &majorCounter{fence: c.fence, calloutID: call.RequestID}
	call.Number = number

	// The callout is ended on every exit path, a panic included: from then on
	// all its passes are refused, and end waits for a joined request of its
	// last cnode that is still in progress.
	cctx, end := c.fence.Begin(ctx, call.RequestID, call.TxID, outer)
	defer end()

	p := &progress{triesLeft: tries}
	result, err := c.loop(cctx, call, p)
	if stats := contract.CalloutStatsFrom(ctx); stats != nil {
		*stats = p.stats
	}
	return result, err
}

// loop runs the local procedure and decides what its outcome means for the
// callout.
func (c *Coordinator) loop(cctx context.Context, call internalgrpc.Callout, p *progress) (internalgrpc.CalloutResult, error) {
	none := internalgrpc.CalloutResult{}

	r := c.local.RunLocal(cctx, call, p.triesLeft)
	p.triesLeft -= r.TriesUsed
	p.attempts = append(p.attempts, r.Attempts...)
	for _, a := range r.Attempts {
		p.stats.Tries = append(p.stats.Tries, a.Kind.String())
	}
	switch {
	case r.CtxErr != nil:
		if r.TriesUsed > len(r.Attempts) {
			p.stats.Tries = append(p.stats.Tries, contract.CalloutOutcomeAbandoned)
		}
		return none, ended(cctx, r.CtxErr)
	case r.OK():
		p.stats.Tries = append(p.stats.Tries, contract.CalloutOutcomeOK)
		return r.Result, nil
	case r.TriesUsed == 0:
		p.noCnode = r.Failure
	default:
		p.lastTried = r.Failure
		if done, err := p.verdict(call.RepeatSafe); done {
			return none, err
		}
	}

	// No cnode took the work.
	return none, p.stop(cctx)
}

// verdict decides what a failed try means for the callout: done with the
// failure itself when its kind forbids another cnode, done with the attempts
// when no try is left, not done otherwise.
func (p *progress) verdict(repeatSafe bool) (bool, error) {
	if !p.lastTried.Kind.MayTryAnother(repeatSafe) {
		return true, withAttempts(p.lastTried, p.attempts)
	}
	if p.triesLeft <= 0 {
		return true, attemptsFailure(p.attempts, p.lastTried)
	}
	return false, nil
}

// stop is the error when nothing more can be done. Attempts on record beat
// "no cnode": NO_COMPUTE_MEMBER_FOR_TAG is returned only when no try was ever
// made.
func (p *progress) stop(cctx context.Context) error {
	if err := cctx.Err(); err != nil {
		return ended(cctx, err)
	}
	if len(p.attempts) > 0 {
		return attemptsFailure(p.attempts, p.lastTried)
	}
	return p.noCnode
}

// ended is the error for a callout whose context ended. context.Cause tells
// why: released by the fence — the callback this callout was made from belongs
// to a cnode that was replaced — is CALLOUT_SUPERSEDED; the caller going away,
// or its transactionTimeoutMillis, is ctxErr, unchanged.
func ended(cctx context.Context, ctxErr error) error {
	if errors.Is(context.Cause(cctx), fence.ErrSuperseded) {
		return fence.NewSupersededError()
	}
	return ctxErr
}
