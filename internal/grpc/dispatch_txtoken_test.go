package grpc

import (
	"context"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// make32 returns a 32-byte secret for token signing in tests.
func make32(t *testing.T) []byte {
	t.Helper()
	b := make([]byte, 32)
	for i := range b {
		b[i] = byte(i + 1)
	}
	return b
}

func TestDispatch_MintsTxTokenFromTxID(t *testing.T) {
	signer, _ := token.NewSigner(make32(t))
	d := NewProcessorDispatcher(NewMemberRegistry(), common.NewTestUUIDGenerator(), signer, "node-A", time.Minute)

	tok := d.resolveTxToken(context.Background(), "tx-42")
	claims, err := signer.Verify(tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims.NodeID != "node-A" || claims.TxRef != "tx-42" {
		t.Fatalf("claims=%+v", claims)
	}
}

func TestDispatch_EmptyTxIDNoToken(t *testing.T) {
	signer, _ := token.NewSigner(make32(t))
	d := NewProcessorDispatcher(NewMemberRegistry(), common.NewTestUUIDGenerator(), signer, "node-A", time.Minute)
	if tok := d.resolveTxToken(context.Background(), ""); tok != "" {
		t.Fatalf("expected empty token, got %q", tok)
	}
}

func TestDispatch_CtxTokenOverridesSelfMint(t *testing.T) {
	signer, _ := token.NewSigner(make32(t))
	d := NewProcessorDispatcher(NewMemberRegistry(), common.NewTestUUIDGenerator(), signer, "node-B", time.Minute)
	ctx := WithTxToken(context.Background(), "pre-minted-A")
	if tok := d.resolveTxToken(ctx, "tx-42"); tok != "pre-minted-A" {
		t.Fatalf("expected ctx token, got %q", tok)
	}
}
