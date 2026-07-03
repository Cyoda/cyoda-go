package e2e_test

import "testing"

func TestUncoveredOps_rules(t *testing.T) {
	all := []string{"live", "liveUncovered", "planned", "staleMarked"}
	exercised := map[string]bool{"live": true, "staleMarked": true}
	success2xx := map[string]bool{"live": true, "staleMarked": true}
	marked := map[string]string{"planned": "planned", "staleMarked": "planned"}

	unmarked, stale := uncoveredOps(all, exercised, success2xx, marked)

	// liveUncovered: neither exercised nor marked -> unmarked-uncovered
	if len(unmarked) != 1 || unmarked[0] != "liveUncovered" {
		t.Errorf("unmarked-uncovered = %v, want [liveUncovered]", unmarked)
	}
	// staleMarked: marked but returned 2xx -> stale marker
	if len(stale) != 1 || stale[0] != "staleMarked" {
		t.Errorf("stale = %v, want [staleMarked]", stale)
	}
}
