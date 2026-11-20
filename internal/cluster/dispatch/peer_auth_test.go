package dispatch_test

import (
	"context"
	"testing"

	"github.com/cyoda/cyoda-go/internal/cluster/dispatch"
)

func TestPeerIdentity_MissingFromEmptyContext(t *testing.T) {
	_, ok := dispatch.PeerIdentityFromContext(context.Background())
	if ok {
		t.Fatal("expected PeerIdentity absent from bare context")
	}
}

func TestPeerIdentity_RoundTripThroughContext(t *testing.T) {
	id := dispatch.NewPeerIdentityForTesting("aead-v1", "")
	ctx := dispatch.WithPeerIdentity(context.Background(), id)

	got, ok := dispatch.PeerIdentityFromContext(ctx)
	if !ok {
		t.Fatal("expected PeerIdentity present after WithPeerIdentity")
	}
	if got.AuthMethod() != "aead-v1" {
		t.Fatalf("auth method = %q, want aead-v1", got.AuthMethod())
	}
	if got.NodeID() != "" {
		t.Fatalf("unexpected node id on shared-key identity: %q", got.NodeID())
	}
}

func TestPeerIdentity_FutureMTLSIdentityCarriesNodeID(t *testing.T) {
	// This test codifies the Option-2 (mTLS) contract: the same abstraction
	// must be able to carry a node-specific identity. If this test stays
	// green after future refactors, Option 2 stays additive.
	id := dispatch.NewPeerIdentityForTesting("mtls", "node-b")
	ctx := dispatch.WithPeerIdentity(context.Background(), id)
	got, _ := dispatch.PeerIdentityFromContext(ctx)
	if got.AuthMethod() != "mtls" || got.NodeID() != "node-b" {
		t.Fatalf("mTLS identity not preserved: auth=%q node=%q", got.AuthMethod(), got.NodeID())
	}
}
