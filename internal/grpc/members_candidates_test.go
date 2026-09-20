package grpc

import (
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func TestCandidates_TenantAndTagFilter(t *testing.T) {
	tests := []struct {
		name     string
		tenant   spi.TenantID
		required string
		wantIDs  []string
	}{
		{"matching tag", "tenant-1", "ml", []string{"m-1"}},
		{"no matching tag", "tenant-1", "java", nil},
		{"empty required matches every member of the tenant", "tenant-1", "", []string{"m-1", "m-2"}},
		{"wrong tenant", "tenant-3", "python", nil},
		{"any overlap is enough", "tenant-1", "java, go", []string{"m-2"}},
		{"another tenant's member on the same tag is never a candidate", "tenant-2", "python", []string{"m-other"}},
	}
	reg := NewMemberRegistry()
	for _, m := range []*Member{
		reg.Register("m-1", "tenant-1", []string{"python", "ml"}, noopSend, nil),
		reg.Register("m-2", "tenant-1", []string{"go"}, noopSend, nil),
		reg.Register("m-other", "tenant-2", []string{"python"}, noopSend, nil),
	} {
		t.Cleanup(func() { reg.Unregister(m) })
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := reg.Candidates(tt.tenant, tt.required)
			if len(got) != len(tt.wantIDs) {
				t.Fatalf("got %d candidates, want %v", len(got), tt.wantIDs)
			}
			for i, m := range got {
				if m.ID != tt.wantIDs[i] {
					t.Errorf("candidate %d = %s, want %s", i, m.ID, tt.wantIDs[i])
				}
			}
		})
	}
}

// The registry is a Go map, whose iteration order changes from call to call;
// Candidates must not.
func TestCandidates_OrderedByConnectedAtThenID(t *testing.T) {
	reg := NewMemberRegistry()
	b := reg.Register("b", "tenant-1", []string{"x"}, noopSend, nil)
	a := reg.Register("a", "tenant-1", []string{"x"}, noopSend, nil)
	c := reg.Register("c", "tenant-1", []string{"x"}, noopSend, nil)
	for _, m := range []*Member{a, b, c} {
		t.Cleanup(func() { reg.Unregister(m) })
	}
	t0 := time.Unix(1_000, 0)
	c.ConnectedAt = t0 // attached first
	a.ConnectedAt = t0.Add(time.Second)
	b.ConnectedAt = t0.Add(time.Second) // same instant as a: the id decides

	for i := 0; i < 20; i++ {
		got := reg.Candidates("tenant-1", "x")
		if len(got) != 3 || got[0] != c || got[1] != a || got[2] != b {
			ids := make([]string, len(got))
			for j, m := range got {
				ids[j] = m.ID
			}
			t.Fatalf("call %d: order = %v, want [c a b]", i, ids)
		}
	}
}
