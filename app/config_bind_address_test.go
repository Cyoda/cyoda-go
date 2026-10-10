package app

import (
	"os"
	"testing"
)

// TestDefaultConfig_APIBindAddressesDefaultToLoopback pins that the HTTP and
// gRPC listeners, like the admin listener, bind loopback unless configured
// otherwise — in every IAM mode, so a mock-IAM instance is never on the
// network by accident.
func TestDefaultConfig_APIBindAddressesDefaultToLoopback(t *testing.T) {
	for _, mode := range []string{"mock", "jwt"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("CYODA_IAM_MODE", mode)
			for _, k := range []string{"CYODA_HTTP_BIND_ADDRESS", "CYODA_GRPC_BIND_ADDRESS"} {
				// Registers the restore hook, then makes the key absent so the
				// compiled-in default applies.
				t.Setenv(k, "")
				_ = os.Unsetenv(k)
			}
			cfg := DefaultConfig()
			if cfg.HTTP.BindAddress != "127.0.0.1" {
				t.Errorf("HTTP.BindAddress = %q, want %q", cfg.HTTP.BindAddress, "127.0.0.1")
			}
			if cfg.GRPC.BindAddress != "127.0.0.1" {
				t.Errorf("GRPC.BindAddress = %q, want %q", cfg.GRPC.BindAddress, "127.0.0.1")
			}
		})
	}
}

func TestDefaultConfig_APIBindAddressOverrides(t *testing.T) {
	t.Setenv("CYODA_HTTP_BIND_ADDRESS", "0.0.0.0")
	t.Setenv("CYODA_GRPC_BIND_ADDRESS", "::")
	cfg := DefaultConfig()
	if cfg.HTTP.BindAddress != "0.0.0.0" {
		t.Errorf("HTTP.BindAddress = %q, want %q", cfg.HTTP.BindAddress, "0.0.0.0")
	}
	if cfg.GRPC.BindAddress != "::" {
		t.Errorf("GRPC.BindAddress = %q, want %q", cfg.GRPC.BindAddress, "::")
	}
}
