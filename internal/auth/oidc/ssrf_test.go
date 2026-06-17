package oidc

import (
	"errors"
	"net"
	"strings"
	"testing"
)

func TestValidateRegisterURI_RejectsBlockedRanges(t *testing.T) {
	cases := []struct {
		name string
		uri  string
	}{
		{"ipv4-loopback", "https://127.0.0.1/.well-known/openid-configuration"},
		{"ipv4-link-local-metadata", "https://169.254.169.254/.well-known/openid-configuration"},
		{"ipv4-rfc1918-10", "https://10.0.0.1/.well-known/openid-configuration"},
		{"ipv4-rfc1918-172", "https://172.16.0.1/.well-known/openid-configuration"},
		{"ipv4-rfc1918-192", "https://192.168.1.1/.well-known/openid-configuration"},
		{"ipv6-loopback", "https://[::1]/.well-known/openid-configuration"},
		{"ipv6-link-local", "https://[fe80::1]/.well-known/openid-configuration"},
		{"ipv6-ula", "https://[fc00::1]/.well-known/openid-configuration"},
		{"ipv6-mapped-v4-loopback", "https://[::ffff:127.0.0.1]/.well-known/openid-configuration"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateRegisterURI(c.uri, true, false)
			if !errors.Is(err, ErrSSRFBlocked) {
				t.Errorf("err = %v, want ErrSSRFBlocked", err)
			}
		})
	}
}

func TestValidateRegisterURI_AllowsPublic(t *testing.T) {
	err := validateRegisterURI("https://8.8.8.8/.well-known/openid-configuration", true, false)
	if err != nil {
		t.Errorf("public IP rejected: %v", err)
	}
}

func TestValidateRegisterURI_RejectsHTTPWhenRequireHTTPS(t *testing.T) {
	err := validateRegisterURI("http://example.com/.well-known/openid-configuration", true, false)
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Errorf("expected https-required error, got %v", err)
	}
}

func TestValidateRegisterURI_AllowsHTTPWhenNotRequired(t *testing.T) {
	err := validateRegisterURI("http://8.8.8.8/.well-known/openid-configuration", false, false)
	if err != nil {
		t.Errorf("unexpected error with requireHTTPS=false: %v", err)
	}
}

func TestValidateRegisterURI_AllowPrivateNetworksOverride(t *testing.T) {
	err := validateRegisterURI("https://127.0.0.1/.well-known/openid-configuration", true, true)
	if err != nil {
		t.Errorf("allowPrivate=true should permit loopback, got %v", err)
	}
}

func TestIsBlockedIP_Ranges(t *testing.T) {
	cases := []struct {
		ip      string
		blocked bool
	}{
		{"127.0.0.1", true},
		{"169.254.169.254", true},
		{"10.0.0.5", true},
		{"172.16.1.1", true},
		{"192.168.0.1", true},
		{"::1", true},
		{"fe80::1", true},
		{"fc00::1", true},
		{"::ffff:127.0.0.1", true},   // IPv4-mapped loopback
		{"::ffff:10.0.0.1", true},    // IPv4-mapped RFC1918
		{"::ffff:169.254.254.254", true}, // IPv4-mapped link-local (metadata service range)
		{"::ffff:8.8.8.8", false},    // IPv4-mapped public — must NOT block
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		{"2001:4860:4860::8888", false}, // Google DNS IPv6
	}
	for _, c := range cases {
		t.Run(c.ip, func(t *testing.T) {
			ip := net.ParseIP(c.ip)
			if ip == nil {
				t.Fatalf("ParseIP(%q) returned nil", c.ip)
			}
			if got := isBlockedIP(ip); got != c.blocked {
				t.Errorf("isBlockedIP(%s) = %v, want %v", c.ip, got, c.blocked)
			}
		})
	}
}

func TestValidateRegisterURI_MissingHostRejects(t *testing.T) {
	// Defensive: URI with no host should not silently pass.
	err := validateRegisterURI("https:///path", true, false)
	if err == nil {
		t.Error("expected error for missing host, got nil")
	}
}
