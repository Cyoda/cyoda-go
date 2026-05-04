package openapivalidator

import (
	"context"
	"testing"
)

func TestWithTestT_RoundTrips(t *testing.T) {
	ctx := WithTestT(context.Background(), t)
	got := TestTFromContext(ctx)
	if got != t {
		t.Errorf("got %v, want %v", got, t)
	}
}

func TestTestTFromContext_NilWhenAbsent(t *testing.T) {
	if got := TestTFromContext(context.Background()); got != nil {
		t.Errorf("got %v, want nil", got)
	}
}
