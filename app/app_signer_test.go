package app

import (
	"testing"

	_ "github.com/cyoda-platform/cyoda-go/plugins/memory"
)

func TestSigner_AlwaysPresentSingleNode(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Cluster.Enabled = false
	a := New(cfg)
	if a.TokenSigner() == nil {
		t.Fatal("expected non-nil TokenSigner in single-node mode")
	}
	if a.selfNodeID != "local" {
		t.Fatalf("expected selfNodeID 'local', got %q", a.selfNodeID)
	}
}
