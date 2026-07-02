package grpc

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// resolveGRPCAddr returns the gRPC dial target for a peer node: the peer's
// advertised gRPC endpoint if it published one, else derived from the peer's
// HTTP host and this node's gRPC port (uniform-deployment fallback).
func resolveGRPCAddr(ni contract.NodeInfo, localGRPCPort int) (string, error) {
	if ni.GRPCAddr != "" {
		return ni.GRPCAddr, nil
	}
	addr := ni.Addr
	if addr == "" {
		return "", fmt.Errorf("node %s has no HTTP address to derive gRPC addr from", ni.NodeID)
	}
	// Ensure the addr has a scheme so url.Parse handles it correctly.
	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}
	u, err := url.Parse(addr)
	if err != nil {
		return "", fmt.Errorf("parse peer HTTP addr %q: %w", ni.Addr, err)
	}
	host := u.Hostname()
	if host == "" {
		return "", fmt.Errorf("parse peer HTTP addr %q: no host", ni.Addr)
	}
	return host + ":" + strconv.Itoa(localGRPCPort), nil
}
