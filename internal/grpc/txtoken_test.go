package grpc

import (
	"context"
	"testing"

	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
)

func TestAttachAndReadTxToken(t *testing.T) {
	ce := &cepb.CloudEvent{}
	AttachTxToken(ce, "tok-abc")
	if got := TxTokenFromCloudEvent(ce); got != "tok-abc" {
		t.Fatalf("got %q", got)
	}
}

func TestAttachTxToken_EmptyIsNoop(t *testing.T) {
	ce := &cepb.CloudEvent{}
	AttachTxToken(ce, "")
	if got := TxTokenFromCloudEvent(ce); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestTxTokenContextRoundTrip(t *testing.T) {
	ctx := WithTxToken(context.Background(), "tok-ctx")
	if got := TxTokenFromContext(ctx); got != "tok-ctx" {
		t.Fatalf("got %q", got)
	}
	if got := TxTokenFromContext(context.Background()); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}
