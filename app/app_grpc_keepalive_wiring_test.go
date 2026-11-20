package app

import (
	"testing"
	"time"

	internalgrpc "github.com/cyoda/cyoda-go/internal/grpc"
)

// CYODA_KEEPALIVE_INTERVAL / _TIMEOUT reach the gRPC server. They used to be
// parsed and dropped on the floor.
func TestNew_KeepAliveConfigReachesGRPCServer(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ContextPath = ""
	cfg.GRPC.KeepAliveInterval = 7
	cfg.GRPC.KeepAliveTimeout = 21
	a := New(cfg)
	t.Cleanup(func() { a.Shutdown(); _ = a.Close() })

	want := internalgrpc.KeepAliveConfig{Interval: 7 * time.Second, Timeout: 21 * time.Second}
	if got := a.GRPCServer().KeepAlive(); got != want {
		t.Fatalf("gRPC server keep-alive = %+v, want %+v", got, want)
	}
}
