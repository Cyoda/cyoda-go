package dispatch_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/dispatch"
)

// TestHTTPForwarder_RejectsLoopbackAddresses documents the SSRF guard on
// the cluster forwarder. Registry entries are trusted by default, but if
// an attacker can influence them (e.g. via a rogue node join, a config
// mistake, or a compromise of a peer) the forwarder must not proxy
// HMAC-signed requests to loopback or link-local addresses — doing so
// would let that attacker reach in-process databases, cloud metadata
// endpoints, or other services bound on 127.0.0.1.
//
// The guard is enforced at address validation time; the HTTP call must
// never be dialled.
func TestHTTPForwarder_RejectsLoopbackAddresses(t *testing.T) {
	fw := dispatch.NewHTTPForwarder(newTestPeerAuth(t), time.Second)

	cases := []string{
		"127.0.0.1:8080",
		"http://127.0.0.1:8080",
		"127.0.0.2:9000", // 127.0.0.0/8 range
		"localhost:8080",
		"[::1]:8080",
		"http://[::1]:8080",
	}
	for _, addr := range cases {
		_, err := fw.ForwardCallout(context.Background(), addr, makeProcessorReq())
		if err == nil {
			t.Errorf("ForwardCallout(%q) accepted loopback address", addr)
			continue
		}
		if !errors.Is(err, dispatch.ErrForbiddenPeerAddress) {
			t.Errorf("ForwardCallout(%q) = %v, want wraps ErrForbiddenPeerAddress", addr, err)
		}
	}
}

// TestHTTPForwarder_RejectsIPv6LinkLocalWithZoneID covers IPv6 link-local
// literals that carry a zone identifier (`fe80::1%eth0`). net.ParseIP
// rejects these, so the guard must either handle them directly or ensure
// rejection still fires rather than falling through to a noisy DNS error.
// The reported error must explicitly wrap ErrForbiddenPeerAddress with
// a link-local reason — not a DNS-lookup failure that happens to reject
// the address by accident.
func TestHTTPForwarder_RejectsIPv6LinkLocalWithZoneID(t *testing.T) {
	fw := dispatch.NewHTTPForwarder(newTestPeerAuth(t), time.Second)

	_, err := fw.ForwardCallout(context.Background(), "[fe80::1%eth0]:8080", makeProcessorReq())
	if err == nil {
		t.Fatal("forwarder accepted IPv6 link-local with zone ID")
	}
	if !errors.Is(err, dispatch.ErrForbiddenPeerAddress) {
		t.Fatalf("expected ErrForbiddenPeerAddress, got %v", err)
	}
	if !strings.Contains(err.Error(), "link-local") {
		t.Errorf("expected error to cite link-local rejection, got %v", err)
	}
	// Platform-independence: the rejection must come from parsing
	// fe80::1%eth0 as a literal IP, not from net.LookupIP accidentally
	// resolving the zoned form to fe80::1 (which darwin does but other
	// libc resolvers may not). If the error mentions "resolve" or
	// "lookup" the code is taking the DNS path and the fix regressed.
	if strings.Contains(err.Error(), "resolve") || strings.Contains(err.Error(), "lookup") {
		t.Errorf("expected IP-literal rejection path (platform-independent); got DNS path: %v", err)
	}
}

// TestHTTPForwarder_RejectsLinkLocalAddresses blocks the AWS/GCP/Azure
// metadata-service SSRF vector. 169.254.169.254 is the canonical target;
// the broader 169.254.0.0/16 range and IPv6 fe80::/10 are equally unsafe.
func TestHTTPForwarder_RejectsLinkLocalAddresses(t *testing.T) {
	fw := dispatch.NewHTTPForwarder(newTestPeerAuth(t), time.Second)

	cases := []string{
		"169.254.169.254:80",                 // AWS / Azure metadata
		"http://169.254.169.254/latest/api/", // full URL form
		"169.254.0.1:8080",                   // broader link-local range
		"[fe80::1]:8080",                     // IPv6 link-local
	}
	for _, addr := range cases {
		_, err := fw.ForwardCallout(context.Background(), addr, makeCriteriaReq())
		if err == nil {
			t.Errorf("ForwardCallout(%q) accepted link-local address", addr)
			continue
		}
		if !errors.Is(err, dispatch.ErrForbiddenPeerAddress) {
			t.Errorf("ForwardCallout(%q) = %v, want wraps ErrForbiddenPeerAddress", addr, err)
		}
	}
}

// TestHTTPForwarder_AcceptsRoutableAddress is the sanity-check happy
// path: a routable address with a valid response must still work.
func TestHTTPForwarder_AcceptsRoutableAddress(t *testing.T) {
	// We don't need real network traffic — we just need the address to
	// survive validation and the request to attempt connection. Pick an
	// address known to be unreachable (TEST-NET-1) with a tiny timeout;
	// the guard must let it through and the call must fail with a
	// *network* error, not ErrForbiddenPeerAddress.
	fw := dispatch.NewHTTPForwarder(newTestPeerAuth(t), 50*time.Millisecond)
	_, err := fw.ForwardCallout(context.Background(), "192.0.2.1:8080", makeProcessorReq())
	if err == nil {
		t.Fatal("expected network error for unreachable TEST-NET-1 address")
	}
	if errors.Is(err, dispatch.ErrForbiddenPeerAddress) {
		t.Fatalf("guard incorrectly rejected routable address: %v", err)
	}
	// Sanity check that this is a connection/network error, not a guard
	// error — the exact text depends on the transport but includes the
	// URL we passed.
	if !strings.Contains(err.Error(), "192.0.2.1") {
		t.Fatalf("expected error to reference the target addr, got: %v", err)
	}
}
