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
	reg := NewMemberRegistry()
	d := NewProcessorDispatcher(reg, NewRoundRobinSelector(reg), common.NewTestUUIDGenerator(), signer, "node-A", time.Minute, 30*time.Second, 60*time.Second)

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
	reg := NewMemberRegistry()
	d := NewProcessorDispatcher(reg, NewRoundRobinSelector(reg), common.NewTestUUIDGenerator(), signer, "node-A", time.Minute, 30*time.Second, 60*time.Second)
	if tok := d.resolveTxToken(context.Background(), ""); tok != "" {
		t.Fatalf("expected empty token, got %q", tok)
	}
}

func TestDispatch_CtxTokenOverridesSelfMint(t *testing.T) {
	signer, _ := token.NewSigner(make32(t))
	reg := NewMemberRegistry()
	d := NewProcessorDispatcher(reg, NewRoundRobinSelector(reg), common.NewTestUUIDGenerator(), signer, "node-B", time.Minute, 30*time.Second, 60*time.Second)
	ctx := WithTxToken(context.Background(), "pre-minted-A")
	if tok := d.resolveTxToken(ctx, "tx-42"); tok != "pre-minted-A" {
		t.Fatalf("expected ctx token, got %q", tok)
	}
}
