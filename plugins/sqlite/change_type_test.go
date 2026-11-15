package sqlite

import "testing"

// deriveChangeType is the single source of truth for the persisted change type
// across sqlite's non-tx and tx-commit write paths; both must agree with each
// other and with the memory/postgres backends. These cases pin that contract.
func TestDeriveChangeType(t *testing.T) {
	cases := []struct {
		name  string
		isNew bool
		want  string
	}{
		{"new entity", true, "CREATED"},
		{"existing entity", false, "UPDATED"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := deriveChangeType(c.isNew); got != c.want {
				t.Fatalf("deriveChangeType(%v) = %q, want %q", c.isNew, got, c.want)
			}
		})
	}
}
