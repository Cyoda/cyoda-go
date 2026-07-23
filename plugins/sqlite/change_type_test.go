package sqlite

import "testing"

// deriveChangeType is the single source of truth for the persisted change type
// across sqlite's non-tx and tx-commit write paths; both must agree with each
// other and with the memory/postgres backends. These cases pin that contract.
func TestDeriveChangeType(t *testing.T) {
	cases := []struct {
		name   string
		caller string
		isNew  bool
		want   string
	}{
		{"new entity, no caller value", "", true, "CREATED"},
		{"new entity ignores caller override", "UPDATED", true, "CREATED"},
		{"existing entity, no caller value", "", false, "UPDATED"},
		{"existing entity, stale CREATED becomes UPDATED", "CREATED", false, "UPDATED"},
		{"existing entity preserves explicit override", "MIGRATED", false, "MIGRATED"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := deriveChangeType(c.caller, c.isNew); got != c.want {
				t.Fatalf("deriveChangeType(%q, %v) = %q, want %q", c.caller, c.isNew, got, c.want)
			}
		})
	}
}
