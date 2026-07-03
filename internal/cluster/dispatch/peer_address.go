package dispatch

import "github.com/cyoda-platform/cyoda-go/internal/cluster/peeraddr"

// ErrForbiddenPeerAddress is the sentinel returned when an address is rejected
// by the SSRF guard. It is re-exported from peeraddr so callers of the dispatch
// package that use errors.Is can reference it without importing peeraddr directly.
var ErrForbiddenPeerAddress = peeraddr.ErrForbiddenPeerAddress

// validatePeerAddress delegates to the shared peeraddr guard. See
// peeraddr.Validate for the full contract and commentary.
func validatePeerAddress(raw string, allowLoopback bool) error {
	return peeraddr.Validate(raw, allowLoopback)
}
