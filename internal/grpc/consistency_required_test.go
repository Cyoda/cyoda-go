package grpc

import (
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/platformclock"
)

func TestNewServer_NilConsistencyPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewServer with a nil consistency service did not panic")
		}
	}()
	NewServer(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, "n", false, 0, true, nil, KeepAliveConfig{}, nil, func() platformclock.State { return platformclock.State{} })
}
