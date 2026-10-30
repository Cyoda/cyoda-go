package fixtureutil

import (
	"sort"
	"testing"
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

// LaunchFreshStack starts one cyoda-go node on the stock binary with
// FreshStackEnv and no compute client, signed by ks, and returns its base URL
// and a stop function that is also registered with t.Cleanup. The caller
// supplies a store of its own in backendEnv and releases it in its own
// cleanup, registered before this call so that it runs after the node stops.
func LaunchFreshStack(t *testing.T, ks *JWTKeySet, backendEnv []string, env map[string]string) (baseURL string, stop func()) {
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
	return node.BaseURL, node.Kill
}
