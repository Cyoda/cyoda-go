package contract_test

import (
	"context"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

func TestClientToken_RoundTrip(t *testing.T) {
	if ct, ok := contract.ClientTokenFrom(context.Background()); ok {
		t.Fatalf("empty context has a client token: %+v", ct)
	}
	want := contract.ClientToken{ClientID: "CLIENT0000000001", Gen: 4}
	ct, ok := contract.ClientTokenFrom(contract.WithClientToken(context.Background(), want))
	if !ok || ct != want {
		t.Fatalf("ClientTokenFrom = %+v, %v; want %+v, true", ct, ok, want)
	}
}
