package main

import (
	"log/slog"
	"net"
	"sort"
	"testing"
)

// listenOn binds host:0 for one test and closes the socket at its end.
func listenOn(t *testing.T, host string) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatalf("listen on %q: %v", host, err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

// TestWarnMockIAMExposure pins the startup WARN: mock IAM makes every caller
// the mock principal, so an API listener that other hosts can reach is named
// at WARN. The check is on the socket actually bound, not on the configured
// string, so a wildcard or a host name that resolves off loopback both count.
// The admin listener is never named: mock IAM does not govern it.
func TestWarnMockIAMExposure(t *testing.T) {
	for _, tc := range []struct {
		name                string
		mode                string
		httpHost, grpcHost  string
		adminHost           string
		wantExposedSurfaces []string
	}{
		{"mock on loopback", "mock", "127.0.0.1", "127.0.0.1", "0.0.0.0", nil},
		{"mock http on every interface", "mock", "0.0.0.0", "127.0.0.1", "127.0.0.1", []string{"http"}},
		{"mock grpc on every interface", "mock", "127.0.0.1", "0.0.0.0", "127.0.0.1", []string{"grpc"}},
		{"mock both on every interface", "mock", "0.0.0.0", "0.0.0.0", "127.0.0.1", []string{"grpc", "http"}},
		{"jwt on every interface", "jwt", "0.0.0.0", "0.0.0.0", "0.0.0.0", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ls := serverListeners{
				http:  listenOn(t, tc.httpHost),
				grpc:  listenOn(t, tc.grpcHost),
				admin: listenOn(t, tc.adminHost),
			}
			records := captureLog(t, slog.LevelDebug, func() { warnMockIAMExposure(tc.mode, ls) })

			var got []string
			for _, r := range records {
				if r["level"] != "WARN" {
					t.Errorf("unexpected %v record: %v", r["level"], r)
					continue
				}
				surface, _ := r["listener"].(string)
				got = append(got, surface)
				if addr, _ := r["addr"].(string); addr == "" {
					t.Errorf("WARN for %s listener carries no addr: %v", surface, r)
				}
			}
			sort.Strings(got)
			if len(got) != len(tc.wantExposedSurfaces) {
				t.Fatalf("WARN named listeners %v; want %v", got, tc.wantExposedSurfaces)
			}
			for i := range got {
				if got[i] != tc.wantExposedSurfaces[i] {
					t.Fatalf("WARN named listeners %v; want %v", got, tc.wantExposedSurfaces)
				}
			}
		})
	}
}
