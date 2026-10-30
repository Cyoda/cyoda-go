package fixtureutil

import (
	"sort"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
)

// FreshStackEnv returns the environment of a fresh clock-controlled stack:
// the backend's own variables (pointing at a store of its own), the tuned
// server settings, clock control on, and then env, which overrides them (the
// last value of a duplicated key wins).
func FreshStackEnv(backendEnv []string, env map[string]string) []string {
	out := append([]string{}, backendEnv...)
	out = append(out, TunedServerEnv()...)
	out = append(out, "CYODA_CLOCK_CONTROL_ENABLED=true")
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}

// FreshStack is a running fresh stack: it mints tokens signed with the key the
// stack trusts, as a BackendFixture does (parity.FreshStack).
type FreshStack struct {
	baseURL string
	ks      *JWTKeySet
}

var _ parity.FreshStack = (*FreshStack)(nil)

// BaseURL implements parity.FreshStack.
func (s *FreshStack) BaseURL() string { return s.baseURL }

// NewTenant implements parity.FreshStack.
func (s *FreshStack) NewTenant(t *testing.T) parity.Tenant {
	t.Helper()
	return MintTenantJWT(t, s.ks)
}

// PlatformOperator implements parity.FreshStack.
func (s *FreshStack) PlatformOperator(t *testing.T) parity.Tenant {
	t.Helper()
	return MintPlatformOperatorJWT(t, s.ks)
}

// LaunchFreshStack starts one cyoda-go node on the stock binary with
// FreshStackEnv and no compute client, signed by ks, and stops it with
// t.Cleanup. The caller supplies a store of its own in backendEnv and
// releases it in its own cleanup, registered before this call so that it runs
// after the node stops.
func LaunchFreshStack(t *testing.T, ks *JWTKeySet, backendEnv []string, env map[string]string) *FreshStack {
	t.Helper()
	bin, err := BuildCyodaBinary()
	if err != nil {
		t.Fatalf("fresh stack: %v", err)
	}
	node, err := LaunchCyodaNode(bin, ks, FreshStackEnv(backendEnv, env), 0)
	if err != nil {
		t.Fatalf("fresh stack: %v", err)
	}
	t.Cleanup(node.Kill)
	return &FreshStack{baseURL: node.BaseURL, ks: ks}
}
