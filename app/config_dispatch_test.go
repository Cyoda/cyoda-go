package app

import (
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/cluster"
)

func validDispatchConfig() cluster.Config {
	return cluster.Config{
		DispatchWaitTimeout:    5 * time.Second,
		DispatchConnectTimeout: 2 * time.Second,
		DispatchForwardTimeout: 30 * time.Second,
	}
}

func TestDefaultConfig_DispatchDurations(t *testing.T) {
	unsetEnv(t, "CYODA_DISPATCH_WAIT_TIMEOUT", "CYODA_DISPATCH_CONNECT_TIMEOUT", "CYODA_DISPATCH_FORWARD_TIMEOUT")
	got := DefaultConfig().Cluster
	if got.DispatchWaitTimeout != 5*time.Second {
		t.Errorf("default DispatchWaitTimeout = %s, want 5s", got.DispatchWaitTimeout)
	}
	if got.DispatchConnectTimeout != 2*time.Second {
		t.Errorf("default DispatchConnectTimeout = %s, want 2s", got.DispatchConnectTimeout)
	}
	if got.DispatchForwardTimeout != 30*time.Second {
		t.Errorf("default DispatchForwardTimeout = %s, want 30s", got.DispatchForwardTimeout)
	}
	t.Setenv("CYODA_DISPATCH_CONNECT_TIMEOUT", "500ms")
	if got := DefaultConfig().Cluster.DispatchConnectTimeout; got != 500*time.Millisecond {
		t.Errorf("DispatchConnectTimeout override = %s, want 500ms", got)
	}
	// 0 is the documented "do not wait" value and must survive DefaultConfig.
	t.Setenv("CYODA_DISPATCH_WAIT_TIMEOUT", "0s")
	if got := DefaultConfig().Cluster.DispatchWaitTimeout; got != 0 {
		t.Errorf("DispatchWaitTimeout=0s = %s, want 0", got)
	}
}

func TestValidateDispatch(t *testing.T) {
	if err := ValidateDispatch(validDispatchConfig()); err != nil {
		t.Fatalf("defaults rejected: %v", err)
	}
	noWait := validDispatchConfig()
	noWait.DispatchWaitTimeout = 0
	if err := ValidateDispatch(noWait); err != nil {
		t.Fatalf("patience 0 (waiting disabled) rejected: %v", err)
	}

	cases := []struct {
		name     string
		mutate   func(*cluster.Config)
		wantName string
	}{
		{"negative patience", func(c *cluster.Config) { c.DispatchWaitTimeout = -time.Second }, "CYODA_DISPATCH_WAIT_TIMEOUT"},
		{"zero connect timeout", func(c *cluster.Config) { c.DispatchConnectTimeout = 0 }, "CYODA_DISPATCH_CONNECT_TIMEOUT"},
		{"negative connect timeout", func(c *cluster.Config) { c.DispatchConnectTimeout = -time.Second }, "CYODA_DISPATCH_CONNECT_TIMEOUT"},
		{"zero forward timeout", func(c *cluster.Config) { c.DispatchForwardTimeout = 0 }, "CYODA_DISPATCH_FORWARD_TIMEOUT"},
		{"negative forward timeout", func(c *cluster.Config) { c.DispatchForwardTimeout = -time.Second }, "CYODA_DISPATCH_FORWARD_TIMEOUT"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validDispatchConfig()
			tc.mutate(&c)
			err := ValidateDispatch(c)
			if err == nil {
				t.Fatalf("ValidateDispatch = nil, want an error")
			}
			if !strings.Contains(err.Error(), tc.wantName) {
				t.Errorf("error must name %s; got: %v", tc.wantName, err)
			}
		})
	}
}
