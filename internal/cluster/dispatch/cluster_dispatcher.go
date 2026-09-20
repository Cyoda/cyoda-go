package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

const gossipPollInterval = 200 * time.Millisecond

// AnswerLimitResolver resolves a callout's stored responseTimeoutMs to its
// answer limit; in production (*grpc.ProcessorDispatcher).ResolveAnswerLimit.
type AnswerLimitResolver func(responseTimeoutMs int64) (time.Duration, *contract.CalloutFailure)

// ClusterDispatcher implements contract.ExternalProcessingService with cluster-aware
// dispatch. It tries the local node first, and if no local calculation member
// matches the required tags, it looks up peers via gossip and hands the callout
// over to a peer that advertises the tag.
type ClusterDispatcher struct {
	local             contract.ExternalProcessingService
	router            *PeerRouter
	answerLimit       AnswerLimitResolver
	waitTimeout       time.Duration
	handoverAllowance time.Duration
}

// NewClusterDispatcher constructs a ClusterDispatcher.
func NewClusterDispatcher(local contract.ExternalProcessingService, router *PeerRouter, answerLimit AnswerLimitResolver, waitTimeout, handoverAllowance time.Duration) *ClusterDispatcher {
	return &ClusterDispatcher{local: local, router: router, answerLimit: answerLimit, waitTimeout: waitTimeout, handoverAllowance: handoverAllowance}
}

// DispatchProcessor tries the local node first. If the local node has no matching
// calculation member, it looks up peers via gossip and hands the callout over.
func (d *ClusterDispatcher) DispatchProcessor(ctx context.Context, entity *spi.Entity, processor spi.ProcessorDefinition, workflowName string, transitionName string, txID string) (*spi.Entity, error) {
	result, err := d.local.DispatchProcessor(ctx, entity, processor, workflowName, transitionName, txID)
	if err == nil || !isNoMatchingMember(err) {
		return result, err
	}
	uc := spi.MustGetUserContext(ctx)
	res, err := d.forwardWithFailover(ctx, internalgrpc.NewProcessorCallout(uc.Tenant.ID, entity, processor, workflowName, transitionName, txID))
	if err != nil {
		return nil, err
	}
	return res.Entity, nil
}

// DispatchCriteria tries the local node first. If the local node has no matching
// calculation member, it looks up peers via gossip and hands the callout over.
func (d *ClusterDispatcher) DispatchCriteria(ctx context.Context, entity *spi.Entity, criterion json.RawMessage, target string, workflowName string, transitionName string, processorName string, txID string) (bool, string, error) {
	matches, reason, err := d.local.DispatchCriteria(ctx, entity, criterion, target, workflowName, transitionName, processorName, txID)
	if err == nil || !isNoMatchingMember(err) {
		return matches, reason, err
	}
	uc := spi.MustGetUserContext(ctx)
	call, failure := internalgrpc.NewCriteriaCallout(uc.Tenant.ID, entity, criterion, target, workflowName, transitionName, processorName, txID)
	if failure != nil {
		return false, "", failure
	}
	res, err := d.forwardWithFailover(ctx, call)
	if err != nil {
		return false, "", err
	}
	return res.Matches, res.Reason, nil
}

// DispatchFunction tries the local node first. If the local node has no matching
// calculation member, it looks up peers via gossip and hands the callout over.
func (d *ClusterDispatcher) DispatchFunction(ctx context.Context, entity *spi.Entity, fn spi.ScheduleFunction, workflowName string, transitionName string, txID string) (contract.FunctionResult, error) {
	result, err := d.local.DispatchFunction(ctx, entity, fn, workflowName, transitionName, txID)
	if err == nil || !isNoMatchingMember(err) {
		return result, err
	}
	uc := spi.MustGetUserContext(ctx)
	res, err := d.forwardWithFailover(ctx, internalgrpc.NewFunctionCallout(uc.Tenant.ID, entity, fn, workflowName, transitionName, txID))
	if err != nil {
		return contract.FunctionResult{}, err
	}
	return res.Function, nil
}

// forwardWithFailover hands the callout over with one try, to one peer after
// another for as long as the failure allows another cnode to be tried: always
// when nothing was handed off, and after a hand-off only if the callout is
// repeat-safe. Each peer is asked at most once; the last failure surfaces.
func (d *ClusterDispatcher) forwardWithFailover(ctx context.Context, call internalgrpc.Callout) (internalgrpc.CalloutResult, error) {
	limit, failure := d.answerLimit(call.ResponseTimeoutMs)
	if failure != nil {
		return internalgrpc.CalloutResult{}, failure
	}
	call.RequestID = uuid.NewString()
	call.AnswerLimit = limit

	tenantID := string(call.TenantID)
	tried := make(map[string]bool)
	peer, err := d.findPeerWithPolling(ctx, tenantID, call.Tags, tried)
	if err != nil {
		return internalgrpc.CalloutResult{}, err
	}
	for {
		ans := func() HandOverAnswer {
			hctx, cancel := context.WithTimeout(ctx, limit+d.handoverAllowance)
			defer cancel()
			return d.router.HandOver(hctx, peer, call, 1, 1)
		}()
		for _, w := range ans.Warnings {
			common.AddWarning(ctx, w)
		}
		if ans.Failure == nil {
			return *ans.Result, nil
		}
		tried[peer.NodeID] = true
		if !ans.Failure.Kind.MayTryAnother(call.RepeatSafe) || ctx.Err() != nil {
			return internalgrpc.CalloutResult{}, ans.Failure
		}
		next, found := d.findPeer(tenantID, call.Tags, tried)
		if !found {
			return internalgrpc.CalloutResult{}, ans.Failure
		}
		peer = next
	}
}

// findPeerWithPolling polls the gossip registry for a peer with matching tags,
// retrying every gossipPollInterval up to waitTimeout. Peers in exclude are
// skipped.
func (d *ClusterDispatcher) findPeerWithPolling(ctx context.Context, tenantID string, tags string, exclude map[string]bool) (contract.NodeInfo, error) {
	deadline := time.After(d.waitTimeout)
	ticker := time.NewTicker(gossipPollInterval)
	defer ticker.Stop()

	// Try immediately first, then poll.
	for {
		peer, found := d.findPeer(tenantID, tags, exclude)
		if found {
			return peer, nil
		}

		select {
		case <-deadline:
			return contract.NodeInfo{}, common.Operational(http.StatusServiceUnavailable, common.ErrCodeNoComputeMemberForTag,
				fmt.Sprintf("no peer with tags %q for tenant %s after %v", tags, tenantID, d.waitTimeout)).AsRetryable()
		case <-ctx.Done():
			return contract.NodeInfo{}, ctx.Err()
		case <-ticker.C:
			// Continue polling.
		}
	}
}

// findPeer returns the first peer advertising the tags, in selector order, that
// is not in exclude.
func (d *ClusterDispatcher) findPeer(tenantID, tags string, exclude map[string]bool) (contract.NodeInfo, bool) {
	for _, n := range d.router.Peers(tenantID, tags) {
		if !exclude[n.NodeID] {
			return n, true
		}
	}
	return contract.NodeInfo{}, false
}

// isNoMatchingMember returns true if the error indicates no local calculation
// member was found (tests against the sentinel from ProcessorDispatcher).
func isNoMatchingMember(err error) bool {
	return errors.Is(err, internalgrpc.ErrNoMatchingMember)
}
