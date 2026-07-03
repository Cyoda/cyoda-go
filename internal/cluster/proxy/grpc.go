package proxy

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/grpc/metadata"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// GRPCTxTokenKey is the gRPC metadata key carrying the transaction routing token.
const GRPCTxTokenKey = "tx-token"

// ErrNodeUnavailable is returned (wrapped) by ResolveTarget when a token names
// a peer that is dead or unknown to the registry. Callers use errors.Is to map
// it to a TRANSACTION_NODE_UNAVAILABLE operational error without string-matching.
var ErrNodeUnavailable = errors.New(common.ErrCodeTransactionNodeUnavailable + ": transaction node is not available")

// ExtractGRPCToken reads the transaction token from gRPC incoming metadata.
// Returns an empty string if the key is absent or the metadata is missing.
func ExtractGRPCToken(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	vals := md.Get(GRPCTxTokenKey)
	if len(vals) == 0 {
		return ""
	}
	return vals[0]
}

// ResolveTarget determines whether a request should be proxied to a remote node.
//
// Returns:
//   - Empty token: shouldProxy=false (serve locally).
//   - Token for self: shouldProxy=false.
//   - Token for alive peer: shouldProxy=true, addr set.
//   - Token for dead/unknown peer: error with TRANSACTION_NODE_UNAVAILABLE.
//   - Invalid/expired token: error.
func ResolveTarget(ctx context.Context, signer *token.Signer, registry contract.NodeRegistry, selfNodeID string, tok string) (addr string, shouldProxy bool, err error) {
	if tok == "" {
		return "", false, nil
	}

	claims, err := signer.Verify(tok)
	if err != nil {
		return "", false, fmt.Errorf("%s: %w", common.ErrCodeBadRequest, err)
	}

	if claims.NodeID == selfNodeID {
		return "", false, nil
	}

	nodeAddr, alive, err := registry.Lookup(ctx, claims.NodeID)
	if err != nil {
		return "", false, fmt.Errorf("registry lookup: %w", err)
	}
	if !alive || nodeAddr == "" {
		return "", false, fmt.Errorf("%w (node %s)", ErrNodeUnavailable, claims.NodeID)
	}

	return nodeAddr, true, nil
}

// ResolveNodeInfo is like ResolveTarget but returns the peer's full NodeInfo
// so the caller can resolve transport-layer details (e.g. the gRPC endpoint).
// shouldProxy is true only when the token names a live peer other than self.
// The returned NodeInfo is only meaningful when shouldProxy is true.
func ResolveNodeInfo(ctx context.Context, signer *token.Signer, reg contract.NodeRegistry, selfNodeID string, tok string) (ni contract.NodeInfo, shouldProxy bool, err error) {
	if tok == "" {
		return contract.NodeInfo{}, false, nil
	}

	claims, err := signer.Verify(tok)
	if err != nil {
		return contract.NodeInfo{}, false, fmt.Errorf("%s: %w", common.ErrCodeBadRequest, err)
	}

	if claims.NodeID == selfNodeID {
		return contract.NodeInfo{}, false, nil
	}

	nodes, err := reg.List(ctx)
	if err != nil {
		return contract.NodeInfo{}, false, fmt.Errorf("registry list: %w", err)
	}
	for _, n := range nodes {
		if n.NodeID == claims.NodeID {
			if !n.Alive {
				return contract.NodeInfo{}, false, fmt.Errorf("%w (node %s)", ErrNodeUnavailable, claims.NodeID)
			}
			return n, true, nil
		}
	}
	return contract.NodeInfo{}, false, fmt.Errorf("%w (node %s)", ErrNodeUnavailable, claims.NodeID)
}
