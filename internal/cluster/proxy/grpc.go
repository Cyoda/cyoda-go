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

// ErrNodeUnavailable is returned (wrapped) by ResolveNodeInfo when a token names
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

// ResolveNodeInfo determines whether a request carrying the transaction token
// tok must be proxied to the node that owns the transaction, and returns that
// peer's full NodeInfo so the caller can resolve its gRPC endpoint.
//
//   - Empty token, or a token for self: shouldProxy=false (serve locally).
//   - Token for a live peer: shouldProxy=true, with the peer's NodeInfo.
//   - Token for a dead or unknown peer: an error wrapping ErrNodeUnavailable.
//   - Invalid or expired token: an error.
//
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
