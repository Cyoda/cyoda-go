// Package peeraddr validates cluster peer addresses against the SSRF guard.
//
// It is a leaf package (stdlib imports only) so it can be imported by both
// the dispatch forwarder and the proxy package without introducing an import
// cycle (dispatch → internal/grpc → proxy; proxy cannot import dispatch).
package peeraddr

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
)

// ErrForbiddenPeerAddress is returned when the caller is asked to dial an
// address that resolves to a loopback, link-local, unspecified, or multicast
// range. Sentinel so callers can distinguish SSRF-guard rejections from
// network errors.
var ErrForbiddenPeerAddress = errors.New("peer address is forbidden")

// Validate parses a cluster registry address and rejects addresses pointing
// at ranges the cluster must never dial: loopback, link-local
// (169.254.0.0/16, fe80::/10), IPv4/IPv6 unspecified, multicast. Literal IPs
// are checked directly; hostnames resolve at validation time and every A/AAAA
// answer must pass.
//
// This guards against the SSRF vector where an attacker who can write to the
// cluster registry pivots HMAC-authenticated dispatch or callback-proxy
// requests to an internal service (Postgres on 127.0.0.1, cloud metadata at
// 169.254.169.254, etc.).
//
// DNS resolution at validation time is a best-effort defence; a full defence
// against DNS rebinding would also pin the resolved IP on the dialer. That is
// out of scope here — the loopback/link-local guard is the first-order fix.
//
// When allowLoopback is true, 127.0.0.0/8 and ::1 are permitted so test
// harnesses that bind cluster nodes on 127.0.0.1 can still forward. Link-local,
// unspecified, and multicast remain rejected even with the flag set.
func Validate(raw string, allowLoopback bool) error {
	hostPort := raw
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return fmt.Errorf("%w: parse %q: %v", ErrForbiddenPeerAddress, raw, err)
		}
		hostPort = u.Host
	}
	host, _, err := net.SplitHostPort(hostPort)
	if err != nil {
		host = hostPort
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return fmt.Errorf("%w: empty host in %q", ErrForbiddenPeerAddress, raw)
	}

	// netip.ParseAddr handles IPv6 zone identifiers (e.g. fe80::1%eth0)
	// that net.ParseIP rejects. Without this, such literals fall through
	// to net.LookupIP, whose behaviour for zoned addresses varies by
	// libc resolver — darwin strips the zone and resolves, Linux may not.
	// Parsing uniformly as a literal keeps the guard platform-independent.
	if addr, err := netip.ParseAddr(host); err == nil {
		ip := addr.AsSlice()
		if err := checkIP(ip, allowLoopback); err != nil {
			return fmt.Errorf("%w: %v (%s)", ErrForbiddenPeerAddress, err, raw)
		}
		return nil
	}

	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("%w: resolve %q: %v", ErrForbiddenPeerAddress, host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("%w: %q resolved to no addresses", ErrForbiddenPeerAddress, host)
	}
	for _, ip := range ips {
		if err := checkIP(ip, allowLoopback); err != nil {
			return fmt.Errorf("%w: %q resolved to %s (%v)", ErrForbiddenPeerAddress, host, ip, err)
		}
	}
	return nil
}

// checkIP returns a descriptive error if ip is in any forbidden range.
// When allowLoopback is true, 127.0.0.0/8 and ::1 are permitted (test
// harnesses only — never set in production). Link-local, unspecified,
// and multicast remain rejected even with the flag set.
func checkIP(ip net.IP, allowLoopback bool) error {
	switch {
	case ip.IsLoopback():
		if allowLoopback {
			return nil
		}
		return fmt.Errorf("loopback %s", ip)
	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast():
		return fmt.Errorf("link-local %s", ip)
	case ip.IsUnspecified():
		return fmt.Errorf("unspecified %s", ip)
	case ip.IsMulticast():
		return fmt.Errorf("multicast %s", ip)
	}
	return nil
}
