package oidc

import (
	"context"
	"fmt"
	"net"
	"net/url"
)

// blockedNets holds the SSRF blocklist CIDRs per spec D10. Parsed once at
// package init so invalid CIDR strings surface as a startup panic rather than
// a silent runtime failure.
var blockedNets []*net.IPNet

func init() {
	cidrs := []string{
		"127.0.0.0/8",    // IPv4 loopback
		"169.254.0.0/16", // IPv4 link-local (AWS metadata, GCP metadata, etc.)
		"10.0.0.0/8",     // RFC1918
		"172.16.0.0/12",  // RFC1918
		"192.168.0.0/16", // RFC1918
		"::1/128",        // IPv6 loopback
		"fe80::/10",      // IPv6 link-local
		"fc00::/7",       // IPv6 ULA (RFC4193)
		// Note: IPv4-mapped IPv6 addresses (::ffff:a.b.c.d) are canonicalised to
		// 4-byte form by isBlockedIP via To4() before the CIDR check, so private
		// addresses embedded in IPv6-mapped form are caught by the IPv4 CIDRs above.
		// A ::ffff:0:0/96 entry is intentionally omitted: Go's net.ParseCIDR
		// represents it as 0.0.0.0/0 in 16-byte form, which would match all IPv4
		// addresses including public ones.
	}
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic(fmt.Sprintf("oidc: invalid blocklist CIDR %q: %v", c, err))
		}
		blockedNets = append(blockedNets, n)
	}
}

// isBlockedIP reports whether ip falls in any of the SSRF blocklist ranges
// (loopback, link-local, RFC1918, IPv6 ULA, IPv4-mapped IPv6).
//
// net.ParseIP returns IPv4 addresses in 16-byte IPv4-in-IPv6 form (::ffff:a.b.c.d).
// To avoid the IPv4-mapped CIDR (::ffff:0:0/96) matching all IPv4 addresses, we
// canonicalise any IPv4-mapped address to its 4-byte form before checking, so the
// IPv4 CIDRs handle private IPv4 ranges and the mapped CIDR only fires for
// IPv4-mapped addresses where the IPv4 portion is not otherwise recognised (an
// edge case not reachable in practice given the RFC1918 + loopback coverage).
func isBlockedIP(ip net.IP) bool {
	// Canonicalise IPv4-in-IPv6 (::ffff:a.b.c.d) to 4-byte IPv4 so that the
	// IPv4-specific CIDRs (127/8, 10/8, etc.) match correctly and the broadened
	// ::ffff:0:0/96 entry only covers addresses not already handled by them.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	for _, n := range blockedNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// validateRegisterURI performs register-time SSRF + scheme checks per D10.
// This is a UX layer — the security boundary is safeDialContext, which re-checks
// at fetch time on every dial.
//
//   - requireHTTPS=true rejects http:// schemes.
//   - allowPrivate=true skips the blocklist check (test/dev only, controlled
//     by CYODA_OIDC_ALLOW_PRIVATE_NETWORKS).
//
// Returns ErrSSRFBlocked for blocklist hits, plain error for malformed/scheme.
func validateRegisterURI(rawURI string, requireHTTPS, allowPrivate bool) error {
	u, err := url.Parse(rawURI)
	if err != nil {
		return fmt.Errorf("malformed URI: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("unsupported scheme %q (want https or http)", u.Scheme)
	}
	if requireHTTPS && u.Scheme != "https" {
		return fmt.Errorf("https required (CYODA_OIDC_REQUIRE_HTTPS=true)")
	}
	if u.Host == "" {
		return fmt.Errorf("missing host in URI")
	}

	if allowPrivate {
		return nil
	}

	host := u.Hostname()
	// If the host is an IP literal, check it directly without a DNS round-trip.
	if ip := net.ParseIP(host); ip != nil {
		if isBlockedIP(ip) {
			return fmt.Errorf("%w: host %s in blocklist", ErrSSRFBlocked, host)
		}
		return nil
	}

	// Hostname: resolve and check every answer.
	ips, err := net.DefaultResolver.LookupIP(context.Background(), "ip", host)
	if err != nil {
		return fmt.Errorf("DNS lookup failed for %s: %w", host, err)
	}
	for _, ip := range ips {
		if isBlockedIP(ip) {
			return fmt.Errorf("%w: %s resolves to %s in blocklist", ErrSSRFBlocked, host, ip)
		}
	}
	return nil
}

// safeDialContext returns a DialContext function that re-checks every resolved
// IP against the blocklist before allowing the connection — the fetch-time
// security boundary that closes DNS-rebind windows the register-time check
// cannot defend.
//
// When allowPrivate is true (test/dev override) the dial proceeds without
// the blocklist check.
func safeDialContext(allowPrivate bool) func(ctx context.Context, network, addr string) (net.Conn, error) {
	dialer := &net.Dialer{}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if allowPrivate {
			return dialer.DialContext(ctx, network, addr)
		}
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("safedialer: malformed addr %q: %w", addr, err)
		}
		// Resolve and check every candidate IP.
		ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
		if err != nil {
			return nil, fmt.Errorf("safedialer: DNS lookup failed for %s: %w", host, err)
		}
		for _, ip := range ips {
			if isBlockedIP(ip) {
				return nil, fmt.Errorf("%w: %s resolves to %s in blocklist", ErrSSRFBlocked, host, ip)
			}
		}
		// Reconstruct addr from the first non-blocked IP and dial.
		targetAddr := net.JoinHostPort(ips[0].String(), port)
		return dialer.DialContext(ctx, network, targetAddr)
	}
}
