package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

const (
	gossipPollInterval = 200 * time.Millisecond

	// forwardFailedClientMessage is the sanitized, client-facing message for
	// a DISPATCH_FORWARD_FAILED error. The underlying transport error (from
	// HTTPForwarder.forward) embeds the peer's internal address, port, and
	// route — that detail must never reach the client (topology leak, see
	// .claude/rules/security.md and B2 in the final review); it is logged
	// server-side instead via slog at the call site.
	forwardFailedClientMessage = "forwarding the callout to a peer node failed"
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
	tok := d.mintTxToken(txID)
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

	resp, peerID, err := d.forwardWithFailover(ctx, tenantID, tags, "processor", req)
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		if resp.ErrorCode != "" {
			return nil, remintPeerError(*resp)
		}
		slog.Warn("peer processor dispatch failed", "pkg", "dispatch", "peer", peerID, "error", resp.Error)
		return nil, fmt.Errorf("peer dispatch failed")
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
func (d *ClusterDispatcher) DispatchCriteria(ctx context.Context, entity *spi.Entity, criterion json.RawMessage, target string, workflowName string, transitionName string, processorName string, txID string) (bool, string, error) {
	// Mint the owner token once before the local-vs-forward split so that
	// a callback landing on a peer node routes back to this (owner) node.
	tok := d.mintTxToken(txID)
	ctx = internalgrpc.WithTxToken(ctx, tok)

	// Try local first.
	matches, reason, err := d.local.DispatchCriteria(ctx, entity, criterion, target, workflowName, transitionName, processorName, txID)
	if err == nil {
		return matches, reason, nil
	}
	if !isNoMatchingMember(err) {
		return false, "", err
	}

	tags := extractCriteriaTags(criterion)
	uc := spi.MustGetUserContext(ctx)
	tenantID := string(uc.Tenant.ID)

	slog.Debug("local criteria dispatch found no member, looking up cluster peers",
		"pkg", "dispatch", "tenantID", tenantID, "tags", tags)

	req := d.buildCriteriaRequest(entity, criterion, target, workflowName, transitionName, processorName, txID, uc, tags, tok)

	resp, peerID, err := d.forwardWithFailover(ctx, tenantID, tags, "criteria", req)
	if err != nil {
		return false, "", err
	}
	if !resp.Success {
		if resp.ErrorCode != "" {
			return false, "", remintPeerError(*resp)
		}
		slog.Warn("peer criteria dispatch failed", "pkg", "dispatch", "peer", peerID, "error", resp.Error)
		return false, "", fmt.Errorf("peer dispatch failed")
	}
	for _, w := range resp.Warnings {
		common.AddWarning(ctx, w)
	}

	peerMatches := resp.Matches != nil && *resp.Matches
	return peerMatches, resp.Reason, nil
}

// DispatchFunction tries the local node first. If the local node has no matching
// calculation member, it looks up peers via gossip and forwards the request.
func (d *ClusterDispatcher) DispatchFunction(ctx context.Context, entity *spi.Entity, fn spi.ScheduleFunction, workflowName string, transitionName string, txID string) (contract.FunctionResult, error) {
	// Mint the owner token once before the local-vs-forward split so that
	// a callback landing on a peer node routes back to this (owner) node.
	tok := d.mintTxToken(txID)
	ctx = internalgrpc.WithTxToken(ctx, tok)

	// Try local first.
	result, err := d.local.DispatchFunction(ctx, entity, fn, workflowName, transitionName, txID)
	if err == nil {
		return result, nil
	}
	if !isNoMatchingMember(err) {
		return contract.FunctionResult{}, err
	}

	tags := fn.CalculationNodesTags
	uc := spi.MustGetUserContext(ctx)
	tenantID := string(uc.Tenant.ID)

	slog.Debug("local function dispatch found no member, looking up cluster peers",
		"pkg", "dispatch", "tenantID", tenantID, "tags", tags)

	req := d.buildFunctionRequest(entity, fn, workflowName, transitionName, txID, uc, tags, tok)

	resp, peerID, err := d.forwardWithFailover(ctx, tenantID, tags, "function", req)
	if err != nil {
		return contract.FunctionResult{}, err
	}
	if !resp.Success {
		if resp.ErrorCode != "" {
			return contract.FunctionResult{}, remintPeerError(*resp)
		}
		slog.Warn("peer function dispatch failed", "pkg", "dispatch", "peer", peerID, "error", resp.Error)
		return contract.FunctionResult{}, fmt.Errorf("peer dispatch failed")
	}
	for _, w := range resp.Warnings {
		common.AddWarning(ctx, w)
	}

	return contract.FunctionResult{Kind: resp.ResultKind, Value: resp.Result}, nil
}

// mintTxToken issues the signed tx-routing token for txID, or "" when there
// is no transaction or no signer. Mint failure is logged, not fatal: the
// dispatch proceeds without cross-node callback routing.
func (d *ClusterDispatcher) mintTxToken(txID string) string {
	if txID == "" || d.signer == nil {
		return ""
	}
	t, err := d.signer.Issue(d.selfNodeID, txID, time.Now().Add(d.tokenTTL))
	if err != nil {
		slog.Error("failed to mint tx-token", "pkg", "dispatch", "err", err)
		return ""
	}
	return t
}

// forwardWithFailover forwards req to a tag-matching peer, failing over to
// the next tag-matching peer when the attempt provably did not dispatch the
// callout: a transport-level forward error (peer unreachable/degraded — this
// class also covers peer HTTP-status rejections such as a 403 auth/replay
// refusal, which the forwarder folds into the forward error), or a
// peer answering NO_COMPUTE_MEMBER_FOR_TAG (it lost its matching calculation
// member between gossip advertisement and forward — nothing executed). Any
// other peer-classified failure means the callout was actually dispatched;
// re-executing it on another peer is not the dispatcher's call, so the
// response is returned unchanged for the caller to remint.
//
// Note on the transport-error case: the request may have reached the peer
// before the connection died, so a failover retry can re-execute the callout.
// That does not weaken existing semantics — this failure class already
// surfaces as retryable (DISPATCH_FORWARD_FAILED), telling the client to
// re-drive the whole dispatch; the failover hop automates that same retry.
//
// Each peer is tried at most once. When all tag-matching peers are exhausted,
// the LAST failure surfaces with the same taxonomy the single-attempt path
// produced. Returns the responding peer's NodeID alongside the response for
// caller-side logging.
func (d *ClusterDispatcher) forwardWithFailover(ctx context.Context, tenantID, tags, kind string, req DispatchCalloutRequest) (*DispatchCalloutResponse, string, error) {
	tried := make(map[string]bool)
	peer, err := d.findPeerWithPolling(ctx, tenantID, tags, tried)
	if err != nil {
		return nil, "", err
	}

	for {
		slog.Debug("forwarding callout to peer",
			"pkg", "dispatch", "kind", kind, "peer", peer.NodeID, "addr", peer.Addr, "tags", tags)

		resp, fwdErr := d.forwarder.ForwardCallout(ctx, peer.Addr, req)
		if fwdErr == nil && (resp.Success || resp.ErrorCode != common.ErrCodeNoComputeMemberForTag) {
			return resp, peer.NodeID, nil
		}

		tried[peer.NodeID] = true
		if fwdErr != nil {
			slog.Error("forward callout to peer failed", "pkg", "dispatch", "kind", kind, "peer", peer.NodeID, "err", fwdErr)
		} else {
			slog.Warn("peer lost matching calculation member, trying next peer",
				"pkg", "dispatch", "kind", kind, "peer", peer.NodeID, "tags", tags)
		}

		// A dead context ends the failover exactly like peer exhaustion: the
		// last failure surfaces with its usual taxonomy (retryable 503) — not
		// a bare ctx error, which classifyWorkflowError would collapse into a
		// non-retryable 400 WORKFLOW_FAILED.
		next, found := contract.NodeInfo{}, false
		if ctx.Err() == nil {
			next, found = d.findPeer(ctx, tenantID, tags, tried)
		}
		if !found {
			if fwdErr != nil {
				return nil, "", common.Operational(http.StatusServiceUnavailable, common.ErrCodeDispatchForwardFailed,
					forwardFailedClientMessage).AsRetryable()
			}
			return resp, peer.NodeID, nil
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
		peer, found := d.findPeer(ctx, tenantID, tags, exclude)
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

// findPeer queries the registry and returns a peer (not self, alive, not in
// exclude) whose tags for the given tenant overlap with the required tags.
func (d *ClusterDispatcher) findPeer(ctx context.Context, tenantID string, tags string, exclude map[string]bool) (contract.NodeInfo, bool) {
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
		if exclude[n.NodeID] {
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
func (d *ClusterDispatcher) buildProcessorRequest(entity *spi.Entity, processor spi.ProcessorDefinition, workflowName, transitionName, txID string, uc *spi.UserContext, tags string, tok string) DispatchCalloutRequest {
	return DispatchCalloutRequest{
		Kind:           "processor",
		Entity:         json.RawMessage(entity.Data),
		EntityMeta:     entity.Meta,
		Processor:      &processor,
		WorkflowName:   workflowName,
		TransitionName: transitionName,
		TxID:           txID,
		TenantID:       string(uc.Tenant.ID),
		Tags:           tags,
		UserID:         uc.UserID,
		PrincipalKind:  uc.Kind,
		Roles:          uc.Roles,
		TxToken:        tok,
	}
}

// buildCriteriaRequest constructs the cross-node dispatch request for criteria.
func (d *ClusterDispatcher) buildCriteriaRequest(entity *spi.Entity, criterion json.RawMessage, target, workflowName, transitionName, processorName, txID string, uc *spi.UserContext, tags string, tok string) DispatchCalloutRequest {
	return DispatchCalloutRequest{
		Kind:           "criteria",
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
		PrincipalKind:  uc.Kind,
		Roles:          uc.Roles,
		TxToken:        tok,
	}
}

// buildFunctionRequest constructs the cross-node dispatch request for a function callout.
func (d *ClusterDispatcher) buildFunctionRequest(entity *spi.Entity, fn spi.ScheduleFunction, workflowName, transitionName, txID string, uc *spi.UserContext, tags string, tok string) DispatchCalloutRequest {
	return DispatchCalloutRequest{
		Kind:           "function",
		Entity:         json.RawMessage(entity.Data),
		EntityMeta:     entity.Meta,
		Function:       &fn,
		WorkflowName:   workflowName,
		TransitionName: transitionName,
		TxID:           txID,
		TenantID:       string(uc.Tenant.ID),
		Tags:           tags,
		UserID:         uc.UserID,
		PrincipalKind:  uc.Kind,
		Roles:          uc.Roles,
		TxToken:        tok,
	}
}

// remintPeerError re-mints a peer's classified dispatch failure (see
// dispatchErrorResponse in handler.go, which populates the
// ErrorCode/ErrorStatus/ErrorRetryable trio) as a fresh *common.AppError on
// the forwarding node, so the caller sees the SAME taxonomy single-node
// dispatch would have produced for the equivalent failure — not a plain
// error that classifyWorkflowError collapses into 400 WORKFLOW_FAILED (B1,
// final review). Only called when resp.ErrorCode != "".
//
// The message is a generic, sanitized string — never resp.Error verbatim —
// matching the same client-safety posture as forwardFailedClientMessage
// (B2): the peer's local dispatch failure text (e.g. a compute-node error)
// must not leak through an untrusted intermediate hop unreviewed.
//
// Status 500 re-mints via InternalWithCode (matching how single-node
// dispatch mints SCHEDULE_FUNCTION_INVALID_RESULT — LevelInternal, sanitized
// ticket response). Every other status re-mints via Operational, chaining
// .AsRetryable() iff the peer classified it retryable — matching how
// single-node dispatch mints DISPATCH_TIMEOUT / NO_COMPUTE_MEMBER_FOR_TAG /
// COMPUTE_MEMBER_DISCONNECTED (LevelOperational, 503, retryable).
func remintPeerError(resp DispatchCalloutResponse) error {
	const genericMsg = "peer node dispatch failed"
	if resp.ErrorStatus == http.StatusInternalServerError {
		return common.InternalWithCode(resp.ErrorCode, genericMsg, nil)
	}
	appErr := common.Operational(resp.ErrorStatus, resp.ErrorCode, genericMsg)
	if resp.ErrorRetryable {
		appErr = appErr.AsRetryable()
	}
	return appErr
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
