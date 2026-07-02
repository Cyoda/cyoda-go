package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

const (
	gossipPollInterval = 200 * time.Millisecond
)

// ClusterDispatcher implements contract.ExternalProcessingService with cluster-aware
// dispatch. It tries the local node first, and if no local calculation member
// matches the required tags, it looks up peers via gossip and forwards the
// request to a peer that advertises the tag.
type ClusterDispatcher struct {
	local       contract.ExternalProcessingService
	registry    contract.NodeRegistry
	selfNodeID  string
	selector    PeerSelector
	forwarder   DispatchForwarder
	waitTimeout time.Duration
	signer      *token.Signer
	tokenTTL    time.Duration
}

// NewClusterDispatcher constructs a ClusterDispatcher.
func NewClusterDispatcher(
	local contract.ExternalProcessingService,
	registry contract.NodeRegistry,
	selfNodeID string,
	selector PeerSelector,
	forwarder DispatchForwarder,
	waitTimeout time.Duration,
	signer *token.Signer,
	tokenTTL time.Duration,
) *ClusterDispatcher {
	return &ClusterDispatcher{
		local:       local,
		registry:    registry,
		selfNodeID:  selfNodeID,
		selector:    selector,
		forwarder:   forwarder,
		waitTimeout: waitTimeout,
		signer:      signer,
		tokenTTL:    tokenTTL,
	}
}

// DispatchProcessor tries the local node first. If the local node has no matching
// calculation member, it looks up peers via gossip and forwards the request.
func (d *ClusterDispatcher) DispatchProcessor(ctx context.Context, entity *spi.Entity, processor spi.ProcessorDefinition, workflowName string, transitionName string, txID string) (*spi.Entity, error) {
	// Mint the owner token once before the local-vs-forward split so that
	// a callback landing on a peer node routes back to this (owner) node.
	tok := ""
	if txID != "" && d.signer != nil {
		if t, err := d.signer.Issue(d.selfNodeID, txID, time.Now().Add(d.tokenTTL)); err == nil {
			tok = t
		} else {
			slog.Error("failed to mint tx-token", "pkg", "dispatch", "err", err)
		}
	}
	ctx = internalgrpc.WithTxToken(ctx, tok)

	// Try local first.
	result, err := d.local.DispatchProcessor(ctx, entity, processor, workflowName, transitionName, txID)
	if err == nil {
		return result, nil
	}
	if !isNoMatchingMember(err) {
		return nil, err
	}

	tags := processor.Config.CalculationNodesTags
	uc := spi.MustGetUserContext(ctx)
	tenantID := string(uc.Tenant.ID)

	slog.Debug("local dispatch found no member, looking up cluster peers",
		"pkg", "dispatch", "tenantID", tenantID, "tags", tags)

	req := d.buildProcessorRequest(entity, processor, workflowName, transitionName, txID, uc, tags, tok)

	peer, err := d.findPeerWithPolling(ctx, tenantID, tags)
	if err != nil {
		return nil, err
	}

	slog.Debug("forwarding processor to peer",
		"pkg", "dispatch", "peer", peer.NodeID, "addr", peer.Addr, "tags", tags)

	resp, err := d.forwarder.ForwardProcessor(ctx, peer.Addr, req)
	if err != nil {
		return nil, fmt.Errorf("%s: forward to %s: %w", common.ErrCodeDispatchForwardFailed, peer.NodeID, err)
	}
	if !resp.Success {
		return nil, fmt.Errorf("peer %s dispatch failed: %s", peer.NodeID, resp.Error)
	}
	for _, w := range resp.Warnings {
		common.AddWarning(ctx, w)
	}

	updated := &spi.Entity{
		Meta: entity.Meta,
		Data: resp.EntityData,
	}
	return updated, nil
}

// DispatchCriteria tries the local node first. If the local node has no matching
// calculation member, it looks up peers via gossip and forwards the request.
func (d *ClusterDispatcher) DispatchCriteria(ctx context.Context, entity *spi.Entity, criterion json.RawMessage, target string, workflowName string, transitionName string, processorName string, txID string) (bool, error) {
	// Mint the owner token once before the local-vs-forward split so that
	// a callback landing on a peer node routes back to this (owner) node.
	tok := ""
	if txID != "" && d.signer != nil {
		if t, err := d.signer.Issue(d.selfNodeID, txID, time.Now().Add(d.tokenTTL)); err == nil {
			tok = t
		} else {
			slog.Error("failed to mint tx-token", "pkg", "dispatch", "err", err)
		}
	}
	ctx = internalgrpc.WithTxToken(ctx, tok)

	// Try local first.
	matches, err := d.local.DispatchCriteria(ctx, entity, criterion, target, workflowName, transitionName, processorName, txID)
	if err == nil {
		return matches, nil
	}
	if !isNoMatchingMember(err) {
		return false, err
	}

	tags := extractCriteriaTags(criterion)
	uc := spi.MustGetUserContext(ctx)
	tenantID := string(uc.Tenant.ID)

	slog.Debug("local criteria dispatch found no member, looking up cluster peers",
		"pkg", "dispatch", "tenantID", tenantID, "tags", tags)

	req := d.buildCriteriaRequest(entity, criterion, target, workflowName, transitionName, processorName, txID, uc, tags, tok)

	peer, err := d.findPeerWithPolling(ctx, tenantID, tags)
	if err != nil {
		return false, err
	}

	slog.Debug("forwarding criteria to peer",
		"pkg", "dispatch", "peer", peer.NodeID, "addr", peer.Addr, "tags", tags)

	resp, err := d.forwarder.ForwardCriteria(ctx, peer.Addr, req)
	if err != nil {
		return false, fmt.Errorf("%s: forward to %s: %w", common.ErrCodeDispatchForwardFailed, peer.NodeID, err)
	}
	if !resp.Success {
		return false, fmt.Errorf("peer %s criteria dispatch failed: %s", peer.NodeID, resp.Error)
	}
	for _, w := range resp.Warnings {
		common.AddWarning(ctx, w)
	}

	return resp.Matches, nil
}

// findPeerWithPolling polls the gossip registry for a peer with matching tags,
// retrying every gossipPollInterval up to waitTimeout.
func (d *ClusterDispatcher) findPeerWithPolling(ctx context.Context, tenantID string, tags string) (contract.NodeInfo, error) {
	deadline := time.After(d.waitTimeout)
	ticker := time.NewTicker(gossipPollInterval)
	defer ticker.Stop()

	// Try immediately first, then poll.
	for {
		peer, found := d.findPeer(ctx, tenantID, tags)
		if found {
			return peer, nil
		}

		select {
		case <-deadline:
			return contract.NodeInfo{}, fmt.Errorf("%s: no peer with tags %q for tenant %s after %v",
				common.ErrCodeNoComputeMemberForTag, tags, tenantID, d.waitTimeout)
		case <-ctx.Done():
			return contract.NodeInfo{}, ctx.Err()
		case <-ticker.C:
			// Continue polling.
		}
	}
}

// findPeer queries the registry and returns a peer (not self, alive) whose tags
// for the given tenant overlap with the required tags.
func (d *ClusterDispatcher) findPeer(ctx context.Context, tenantID string, tags string) (contract.NodeInfo, bool) {
	nodes, err := d.registry.List(ctx)
	if err != nil {
		slog.Debug("failed to list cluster nodes", "pkg", "dispatch", "err", err)
		return contract.NodeInfo{}, false
	}

	var candidates []contract.NodeInfo
	for _, n := range nodes {
		if n.NodeID == d.selfNodeID {
			continue
		}
		if !n.Alive {
			continue
		}
		if common.TagsOverlap(n.Tags[tenantID], tags) {
			candidates = append(candidates, n)
		}
	}

	if len(candidates) == 0 {
		return contract.NodeInfo{}, false
	}

	peer, err := d.selector.Select(candidates)
	if err != nil {
		slog.Debug("peer selection failed", "pkg", "dispatch", "err", err)
		return contract.NodeInfo{}, false
	}
	return peer, true
}

// buildProcessorRequest constructs the cross-node dispatch request for a processor.
func (d *ClusterDispatcher) buildProcessorRequest(entity *spi.Entity, processor spi.ProcessorDefinition, workflowName, transitionName, txID string, uc *spi.UserContext, tags string, tok string) *DispatchProcessorRequest {
	return &DispatchProcessorRequest{
		Entity:         json.RawMessage(entity.Data),
		EntityMeta:     entity.Meta,
		Processor:      processor,
		WorkflowName:   workflowName,
		TransitionName: transitionName,
		TxID:           txID,
		TenantID:       string(uc.Tenant.ID),
		Tags:           tags,
		UserID:         uc.UserID,
		Roles:          uc.Roles,
		TxToken:        tok,
	}
}

// buildCriteriaRequest constructs the cross-node dispatch request for criteria.
func (d *ClusterDispatcher) buildCriteriaRequest(entity *spi.Entity, criterion json.RawMessage, target, workflowName, transitionName, processorName, txID string, uc *spi.UserContext, tags string, tok string) *DispatchCriteriaRequest {
	return &DispatchCriteriaRequest{
		Entity:         json.RawMessage(entity.Data),
		EntityMeta:     entity.Meta,
		Criterion:      criterion,
		Target:         target,
		WorkflowName:   workflowName,
		TransitionName: transitionName,
		ProcessorName:  processorName,
		TxID:           txID,
		TenantID:       string(uc.Tenant.ID),
		Tags:           tags,
		UserID:         uc.UserID,
		Roles:          uc.Roles,
		TxToken:        tok,
	}
}

// isNoMatchingMember returns true if the error indicates no local calculation
// member was found (tests against the sentinel from ProcessorDispatcher).
func isNoMatchingMember(err error) bool {
	return errors.Is(err, internalgrpc.ErrNoMatchingMember)
}

// extractCriteriaTags extracts the calculationNodesTags from a criterion JSON.
// The expected structure is: {"type":"function","function":{"config":{"calculationNodesTags":"..."}}}
func extractCriteriaTags(criterion json.RawMessage) string {
	var parsed struct {
		Function struct {
			Config struct {
				CalculationNodesTags string `json:"calculationNodesTags"`
			} `json:"config"`
		} `json:"function"`
	}
	if err := json.Unmarshal(criterion, &parsed); err != nil {
		return ""
	}
	return parsed.Function.Config.CalculationNodesTags
}
