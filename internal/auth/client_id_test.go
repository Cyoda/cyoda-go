package auth_test

import (
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

func TestValidClientID(t *testing.T) {
	for _, ok := range []string{"C1", "GC2693985CC61NUU", "backend", "Backend", "order-service", "compute.node_2", strings.Repeat("a", 100), "systems", "my-system"} {
		if !auth.ValidClientID(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "-x", ".x", "_x", "a:b", "a/b", "a b", "a%2Db", strings.Repeat("a", 101), "system", "SYSTEM", "System", "é"} {
		if auth.ValidClientID(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}
