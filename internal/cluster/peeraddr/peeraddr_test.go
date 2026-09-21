package peeraddr_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/peeraddr"
)

// TestValidate_RejectsLoopback confirms that loopback addresses are blocked
// when allowLoopback=false and permitted when allowLoopback=true.
func TestValidate_RejectsLoopback(t *testing.T) {
	cases := []string{
		"127.0.0.1:8080",
		"http://127.0.0.1:8080",
		"127.0.0.2:9000", // 127.0.0.0/8 range
		"localhost:8080",
		"[::1]:8080",
		"http://[::1]:8080",
	}
	for _, addr := range cases {
		err := peeraddr.Validate(context.Background(), addr, false)
		if err == nil {
			t.Errorf("Validate(%q, false) accepted loopback address", addr)
			continue
		}
		if !errors.Is(err, peeraddr.ErrForbiddenPeerAddress) {
			t.Errorf("Validate(%q, false) = %v, want wraps ErrForbiddenPeerAddress", addr, err)
		}
	}
}

// TestValidate_AllowsLoopback confirms allowLoopback=true permits loopback
// (returns nil for literal loopback IPs; hostname resolution is bypassed for
// literal IPs so we use 127.0.0.1 directly).
func TestValidate_AllowsLoopback(t *testing.T) {
	cases := []string{
		"127.0.0.1:8080",
		"http://127.0.0.1:8080",
		"[::1]:8080",
	}
	for _, addr := range cases {
		if err := peeraddr.Validate(context.Background(), addr, true); err != nil {
			t.Errorf("Validate(%q, true) = %v, want nil (loopback allowed)", addr, err)
		}
	}
}

// TestValidate_RejectsLinkLocal confirms the AWS/GCP metadata-service SSRF
// vector is blocked regardless of allowLoopback.
func TestValidate_RejectsLinkLocal(t *testing.T) {
	cases := []string{
		"169.254.169.254:80",
		"http://169.254.169.254/latest/api/",
		"169.254.0.1:8080",
		"[fe80::1]:8080",
	}
	for _, allow := range []bool{false, true} {
		for _, addr := range cases {
			err := peeraddr.Validate(context.Background(), addr, allow)
			if err == nil {
				t.Errorf("Validate(%q, %v) accepted link-local address", addr, allow)
				continue
			}
			if !errors.Is(err, peeraddr.ErrForbiddenPeerAddress) {
				t.Errorf("Validate(%q, %v) = %v, want ErrForbiddenPeerAddress", addr, allow, err)
			}
		}
	}
}

// TestValidate_RejectsIPv6LinkLocalWithZoneID confirms the platform-independent
// zone-ID path is taken (no DNS fallback).
func TestValidate_RejectsIPv6LinkLocalWithZoneID(t *testing.T) {
	err := peeraddr.Validate(context.Background(), "[fe80::1%eth0]:8080", false)
	if err == nil {
		t.Fatal("Validate accepted IPv6 link-local with zone ID")
	}
	if !errors.Is(err, peeraddr.ErrForbiddenPeerAddress) {
		t.Fatalf("expected ErrForbiddenPeerAddress, got %v", err)
	}
	if !strings.Contains(err.Error(), "link-local") {
		t.Errorf("expected error to cite link-local, got %v", err)
	}
	if strings.Contains(err.Error(), "resolve") || strings.Contains(err.Error(), "lookup") {
		t.Errorf("expected IP-literal rejection path; got DNS path: %v", err)
	}
}

// TestValidate_AcceptsRoutableAddress confirms a routable (TEST-NET-1) address
// passes validation — the guard does not reject non-forbidden ranges.
func TestValidate_AcceptsRoutableAddress(t *testing.T) {
	// 192.0.2.0/24 is TEST-NET-1 (RFC 5737) — routable but not reachable.
	// Validation must pass; network error (if any) comes later.
	if err := peeraddr.Validate(context.Background(), "192.0.2.1:8080", false); err != nil {
		t.Errorf("Validate(TEST-NET-1, false) = %v, want nil", err)
	}
}

// A name lookup runs under the caller's context, so a hand-over whose time is
// already spent does not wait on a resolver that is slow or down. The context
// ending is a refusal like any other: nothing about the address was
// established, so it is not dialled.
func TestValidate_ResolvesUnderTheCallersContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	err := peeraddr.Validate(ctx, "a-name-that-must-be-resolved.example:8080", false)
	if err == nil {
		t.Fatal("an address that could not be resolved was accepted")
	}
	if !errors.Is(err, peeraddr.ErrForbiddenPeerAddress) {
		t.Errorf("err = %v, want it to wrap ErrForbiddenPeerAddress", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want the context's own error — the lookup did not run under it", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("the lookup took %v although the context had already ended", took)
	}
}

// Only the lookup is under the context: an address that is a literal needs no
// resolver and is judged on its own, whatever the context says.
func TestValidate_LiteralNeedsNoResolver(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := peeraddr.Validate(ctx, "192.0.2.1:8080", false); err != nil {
		t.Errorf("Validate(literal) = %v, want nil — no lookup is needed", err)
	}
	if err := peeraddr.Validate(ctx, "169.254.169.254:80", false); !errors.Is(err, peeraddr.ErrForbiddenPeerAddress) {
		t.Errorf("Validate(link-local) = %v, want the guard's refusal", err)
	}
}
