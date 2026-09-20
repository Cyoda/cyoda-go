package dispatch

import (
	"context"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

func TestBuildProcessorRequest_CarriesOwnerToken(t *testing.T) {
	signer, err := token.NewSigner(testSecret32)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	tok, err := signer.Issue(token.Claims{NodeID: "node-A", TxRef: "tx-9", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-tx-9", Major: 1})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	d := &ClusterDispatcher{selfNodeID: "node-A", signer: signer, tokenTTL: time.Minute}
	uc := &spi.UserContext{
		UserID: "user-1",
		Tenant: spi.Tenant{ID: "tenant-1"},
		Roles:  []string{"ROLE_USER"},
	}
	req := d.buildProcessorRequest(testEntity(), testProcessor(), "wf", "tr", "tx-9", uc, "tag", tok)
	if req.TxToken != tok {
		t.Fatalf("expected owner token on forwarded request, got %q", req.TxToken)
	}
}

func TestMintTxToken_APassOnTheContextWins(t *testing.T) {
	signer, err := token.NewSigner(testSecret32)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	d := &ClusterDispatcher{selfNodeID: "node-A", signer: signer, tokenTTL: time.Minute}
	ctx := internalgrpc.WithTxToken(context.Background(), "the-callouts-pass")
	if got := d.mintTxToken(ctx, "tx-9"); got != "the-callouts-pass" {
		t.Fatalf("mintTxToken = %q; want the pass already on the context", got)
	}
}
