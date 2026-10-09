package proxy

import (
	"errors"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/peeraddr"
)

// TestDialTarget_AcceptsHostPort pins the legitimate forms: each is dialled
// through the dns resolver exactly as written.
func TestDialTarget_AcceptsHostPort(t *testing.T) {
	for _, addr := range []string{
		"10.0.0.5:9090",
		"[2001:db8::1]:9090",
		"[fd00::5]:9090",
		"node-1.cyoda-headless.ns.svc.cluster.local:9090",
		"localhost:9090",
		"dns:9090",
	} {
		got, err := dialTarget(addr)
		if err != nil {
			t.Errorf("dialTarget(%q) refused: %v", addr, err)
			continue
		}
		if want := "dns:///" + addr; got != want {
			t.Errorf("dialTarget(%q) = %q; want %q", addr, got, want)
		}
	}
}

// TestDialTarget_RefusesWhatTheDialerWouldReadDifferently pins that the guard
// and the dialer read the same host. grpc-go parses the target as a URL: it
// takes a scheme and authority from it, percent-decodes the endpoint, and cuts
// it at ? and #, while peeraddr.Validate checks the raw string. Each of these
// would dial a host the guard never checked.
func TestDialTarget_RefusesWhatTheDialerWouldReadDifferently(t *testing.T) {
	for _, addr := range []string{
		"passthrough://8.8.8.8/127.0.0.1:9090",
		"dns://8.8.8.8/127.0.0.1:9090",
		"passthrough:///127.0.0.1:9090",
		"passthrough:127.0.0.1:9090",
		"127.0.0.1?x.example.com:9090",
		"127.0.0.1#x.example.com:9090",
		"127%2e0%2e0%2e1:9090",
		"goo%67le.com:9090",
		"google.com?:9090",
		"[2001:db8::1%25eth0]:9090",
		"127.0.0.1",
	} {
		got, err := dialTarget(addr)
		if err == nil {
			t.Errorf("dialTarget(%q) = %q; want it refused", addr, got)
			continue
		}
		if !errors.Is(err, peeraddr.ErrForbiddenPeerAddress) {
			t.Errorf("dialTarget(%q) error = %v; want ErrForbiddenPeerAddress", addr, err)
		}
	}
}
