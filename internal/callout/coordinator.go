// Package callout holds the owner's loop: the one place that decides how often,
// where and for how long a processor, criterion or function callout is tried.
// The pnode that holds the operation's transaction — the owner — runs it; a
// pnode that receives a hand-over runs the local procedure only and never this.
package callout

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/dispatch"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/fence"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

// retryPolicyNone is the retryPolicy value that selects a single try. Every
// other accepted value — FIXED, or none — selects 1 + Config.FixedNumRetries.
const retryPolicyNone = "NONE"

// PeerRouter is the owner's view of the other pnodes. *dispatch.PeerRouter
// satisfies it. A Coordinator on a single pnode has none.
type PeerRouter interface {
	// Peers returns the alive pnodes other than this one that advertise any of
	// tagsCSV for tenantID, in the order in which they should be asked.
	Peers(tenantID, tagsCSV string) []contract.NodeInfo
	// HandOver passes call to peer with triesLeft tries under fencing number
	// major. The deadline on ctx bounds the owner's wait for the answer.
	HandOver(ctx context.Context, peer contract.NodeInfo, call internalgrpc.Callout, triesLeft int, major uint32) dispatch.HandOverAnswer
	// Changed returns a channel that is closed when a pnode joins or leaves or
	// a pnode's list of tenants and tags arrives. Take it before looking.
	Changed() <-chan struct{}
}

// Config is the part of the server configuration the owner's loop reads.
type Config struct {
	// SelfNodeID is this pnode's id: the owner named in every pass.
	SelfNodeID string
	// FixedNumRetries is the number of tries after the first under retryPolicy
	// FIXED or none.
	FixedNumRetries int
	// Patience is how long one callout waits, in total, for a cnode to exist.
	// Zero disables waiting.
	Patience time.Duration
	// HandoverAllowance is what the owner allows a hand-over on top of
	// tries × answer limit.
	HandoverAllowance time.Duration
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
	// peers is the other pnodes, nil on a single pnode.
	peers PeerRouter
	fence *fence.Fence
	uuids spi.UUIDGenerator
	cfg   Config
}

// New builds the owner's loop. peers is nil on a single pnode — pass an untyped
// nil, not a nil *dispatch.PeerRouter, which would be a non-nil interface.
func New(local *internalgrpc.ProcessorDispatcher, members *internalgrpc.MemberRegistry, peers PeerRouter, f *fence.Fence, uuids spi.UUIDGenerator, cfg Config) *Coordinator {
	return &Coordinator{local: local, members: members, peers: peers, fence: f, uuids: uuids, cfg: cfg}
}

// triesFor is the number of tries retryPolicy selects.
func (c *Coordinator) triesFor(retryPolicy string) int {
	if retryPolicy == retryPolicyNone {
		return 1
	}
	return 1 + c.cfg.FixedNumRetries
}

// majorCounter is the one fencing counter of a callout. Both the local
// procedure (as its TryNumberer) and every hand-over draw from it, so the
// number rises — and the earlier cnode is shut out, and its joined request in
// progress waited for — before the work is given to anyone else. It is used
// from the goroutine that runs the callout only.
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
	// lastTried is the failure of the latest try that was actually made, here
	// or on another pnode. A pass that found no cnode does not overwrite it.
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

	// The hard limit on time, fixed now. A lost hand-over answer counts one try
	// while the peer may have made more, so the number of tries can exceed the
	// setting; the time cannot. It is a context of the callout's own, with a
	// cause of its own, never the caller's: that is how the local procedure
	// tells "this callout ran out of time" — the try in progress is NoAnswer —
	// from "the caller went away".
	deadline := time.Now().Add(time.Duration(tries)*limit + c.cfg.Patience + c.cfg.HandoverAllowance)
	cctx, cancel := context.WithDeadlineCause(cctx, deadline, contract.ErrCalloutDeadline)
	defer cancel()

	p := &progress{triesLeft: tries}
	result, err := c.loop(cctx, call, number, p)
	if stats := contract.CalloutStatsFrom(ctx); stats != nil {
		*stats = p.stats
	}
	return result, err
}

// loop makes passes — the local procedure, then one hand-over per peer — until
// a cnode answers, a failure forbids another try, the tries are used up, or
// nothing more can be done within the patience and the deadline.
func (c *Coordinator) loop(cctx context.Context, call internalgrpc.Callout, number *majorCounter, p *progress) (internalgrpc.CalloutResult, error) {
	none := internalgrpc.CalloutResult{}
	patienceLeft := c.cfg.Patience
	for {
		// Both channels are taken before looking, so a change that happens
		// while this pass looks is not lost to the wait that follows it.
		localChanged := c.members.Changed()
		var peersChanged <-chan struct{}
		if c.peers != nil {
			peersChanged = c.peers.Changed()
		}

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

		if c.peers != nil {
			result, done, err := c.askPeers(cctx, call, number, p)
			if done {
				return result, err
			}
		}

		// No cnode anywhere took the work in this pass. A pass that made tries
		// may still wait: a cnode that dropped and is coming back is the case
		// the patience exists for.
		if patienceLeft <= 0 || cctx.Err() != nil {
			return none, p.stop(cctx)
		}
		slog.Debug("callout waits for a cnode", "pkg", "callout", "kind", call.Kind.String(), "name", call.Name,
			"requestId", call.RequestID, "tags", call.Tags, "patienceLeftMs", patienceLeft.Milliseconds())
		waited, changed := waitForChange(cctx, localChanged, peersChanged, patienceLeft)
		patienceLeft -= waited
		p.stats.Waited += waited
		// A wait the patience or the context ended starts no further pass:
		// nothing changed, so the same cnodes would only be tried again.
		if !changed {
			return none, p.stop(cctx)
		}
		// A new pass: every peer may be asked again.
	}
}

// askPeers hands the callout over to one peer after another, each at most once
// in this pass. A peer that cannot be connected to, or that has no cnode for
// the work, uses no try. done is false when no peer took the work and the
// callout may go on.
func (c *Coordinator) askPeers(cctx context.Context, call internalgrpc.Callout, number *majorCounter, p *progress) (result internalgrpc.CalloutResult, done bool, err error) {
	none := internalgrpc.CalloutResult{}
	asked := make(map[string]struct{})
	for cctx.Err() == nil {
		peer, ok := nextPeer(c.peers.Peers(string(call.TenantID), call.Tags), asked)
		if !ok {
			return none, false, nil
		}
		asked[peer.NodeID] = struct{}{}

		// The same counter RunLocal draws from: the cnode that held the work
		// is shut out before the work goes to another pnode.
		major, _ := number.Next()
		wait := time.Duration(p.triesLeft)*call.AnswerLimit + c.cfg.HandoverAllowance
		hctx, cancel := context.WithDeadlineCause(cctx, time.Now().Add(wait), contract.ErrCalloutDeadline)
		slog.Debug("callout hand-over", "pkg", "callout", "kind", call.Kind.String(), "name", call.Name,
			"requestId", call.RequestID, "peer", peer.NodeID, "triesLeft", p.triesLeft, "major", major)
		a := c.peers.HandOver(hctx, peer, call, p.triesLeft, major)
		cancel()

		// The hand-over does not report the owner's own context ending, so the
		// owner reads it here: the callout's own deadline is not the end of the
		// callout's story — what it recorded is — but the caller going away and
		// the fence's release are.
		if err := cctx.Err(); err != nil && !calloutDeadlinePassed(cctx) {
			p.stats.HandOvers = append(p.stats.HandOvers, contract.CalloutOutcomeAbandoned)
			return none, true, ended(cctx, err)
		}
		for _, w := range a.Warnings {
			common.AddWarning(cctx, w)
		}
		if notAsked(a) {
			p.stats.HandOvers = append(p.stats.HandOvers, contract.CalloutOutcomeUnreachable)
			continue
		}
		p.triesLeft -= a.TriesUsed
		if a.Connected && a.Failure != nil && len(a.Attempts) == 0 {
			// The answer was lost: no cnode is known. It counts as a try and is
			// recorded as one, under the member id "-".
			a.Attempts = []contract.CalloutAttempt{{MemberID: "-", Kind: a.Failure.Kind, Cause: a.Failure.Message}}
		}
		p.attempts = append(p.attempts, a.Attempts...)
		for _, at := range a.Attempts {
			p.stats.Tries = append(p.stats.Tries, at.Kind.String())
		}
		if a.Failure == nil {
			p.stats.Tries = append(p.stats.Tries, contract.CalloutOutcomeOK)
			p.stats.HandOvers = append(p.stats.HandOvers, contract.CalloutOutcomeOK)
			return *a.Result, true, nil
		}
		p.stats.HandOvers = append(p.stats.HandOvers, a.Failure.Kind.String())
		p.lastTried = a.Failure
		if done, err := p.verdict(call.RepeatSafe); done {
			return none, true, err
		}
	}
	return none, false, nil
}

// notAsked reports whether an answer means the peer was never asked, so that
// the callout may be offered to the next one: the connection could not be
// opened, the peer's address failed validation, or the peer answered that it has
// no cnode for the work. A hand-over that was not connected is not enough on its
// own — one this pnode proved it cannot make at all is Terminal, and would fail
// identically for every peer.
func notAsked(a dispatch.HandOverAnswer) bool {
	return !a.Connected && (a.Failure == nil || a.Failure.Kind != contract.Terminal)
}

// nextPeer is the first of peers that was not asked yet in this pass.
func nextPeer(peers []contract.NodeInfo, asked map[string]struct{}) (contract.NodeInfo, bool) {
	for _, peer := range peers {
		if _, done := asked[peer.NodeID]; !done {
			return peer, true
		}
	}
	return contract.NodeInfo{}, false
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

// stop is the error when nothing more can be done: the patience is spent, the
// callout's deadline has passed, or its context ended for another reason.
// Attempts on record beat "no cnode": NO_COMPUTE_MEMBER_FOR_TAG is returned
// only when no try was ever made.
func (p *progress) stop(cctx context.Context) error {
	if err := cctx.Err(); err != nil && !calloutDeadlinePassed(cctx) {
		return ended(cctx, err)
	}
	if len(p.attempts) > 0 {
		return attemptsFailure(p.attempts, p.lastTried)
	}
	return p.noCnode
}

// waitForChange blocks until a cnode or a pnode came or went, the rest of the
// patience is spent, or ctx ends. It reports the time spent and whether a
// change ended the wait. The wait is on a signal, never a poll. A nil
// peersChanged — a Coordinator with no peers — never fires.
func waitForChange(ctx context.Context, localChanged, peersChanged <-chan struct{}, patienceLeft time.Duration) (time.Duration, bool) {
	start := time.Now()
	timer := time.NewTimer(patienceLeft)
	defer timer.Stop()
	select {
	case <-localChanged:
		return time.Since(start), true
	case <-peersChanged:
		return time.Since(start), true
	case <-timer.C:
		return patienceLeft, false
	case <-ctx.Done():
		return time.Since(start), false
	}
}

// calloutDeadlinePassed reports whether ctx ended because the callout's own
// deadline passed.
func calloutDeadlinePassed(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), contract.ErrCalloutDeadline)
}

// ended is the error for a callout whose context ended for a reason other than
// its own deadline. context.Cause tells the two apart: released by the fence —
// the callback this callout was made from belongs to a cnode that was replaced
// — is CALLOUT_SUPERSEDED; the caller going away, or its
// transactionTimeoutMillis, is ctxErr, unchanged.
func ended(cctx context.Context, ctxErr error) error {
	if errors.Is(context.Cause(cctx), fence.ErrSuperseded) {
		return fence.NewSupersededError()
	}
	return ctxErr
}
