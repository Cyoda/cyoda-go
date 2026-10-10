package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/app"
)

func TestPrintBannerTo_Suppressed(t *testing.T) {
	t.Setenv("CYODA_SUPPRESS_BANNER", "true")
	var buf bytes.Buffer
	printBannerTo(&buf, app.DefaultConfig())
	if buf.Len() != 0 {
		t.Fatalf("expected empty output when suppressed, got %q", buf.String())
	}
}

func TestPrintBannerTo_NotSuppressed(t *testing.T) {
	t.Setenv("CYODA_SUPPRESS_BANNER", "")
	var buf bytes.Buffer
	printBannerTo(&buf, app.DefaultConfig())
	if buf.Len() == 0 {
		t.Fatal("expected banner output, got empty")
	}
}

// TestPrintBannerTo_ShowsBindAddresses pins that the banner names the host each
// API listener binds, not only its port: ":8080" reads as every interface.
func TestPrintBannerTo_ShowsBindAddresses(t *testing.T) {
	t.Setenv("CYODA_SUPPRESS_BANNER", "")
	cfg := app.DefaultConfig()
	cfg.HTTP.BindAddress, cfg.HTTPPort = "127.0.0.1", 8080
	cfg.GRPC.BindAddress, cfg.GRPC.Port = "::1", 9090
	var buf bytes.Buffer
	printBannerTo(&buf, cfg)
	for _, want := range []string{"HTTP 127.0.0.1:8080", "gRPC [::1]:9090"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("banner is missing %q:\n%s", want, buf.String())
		}
	}
}

func TestMockAuthWarning_EmittedInMockMode(t *testing.T) {
	t.Setenv("CYODA_SUPPRESS_BANNER", "")
	var buf bytes.Buffer
	cfg := app.DefaultConfig()
	cfg.IAM.Mode = "mock"
	printMockAuthWarningTo(&buf, cfg)
	if !bytes.Contains(buf.Bytes(), []byte("MOCK AUTH IS ACTIVE")) {
		t.Fatalf("expected MOCK AUTH warning, got %q", buf.String())
	}
}

func TestMockAuthWarning_NotEmittedInJWTMode(t *testing.T) {
	t.Setenv("CYODA_SUPPRESS_BANNER", "")
	var buf bytes.Buffer
	cfg := app.DefaultConfig()
	cfg.IAM.Mode = "jwt"
	printMockAuthWarningTo(&buf, cfg)
	if buf.Len() != 0 {
		t.Fatalf("expected no warning in jwt mode, got %q", buf.String())
	}
}

func TestMockAuthWarning_SuppressedByFlag(t *testing.T) {
	t.Setenv("CYODA_SUPPRESS_BANNER", "true")
	var buf bytes.Buffer
	cfg := app.DefaultConfig()
	cfg.IAM.Mode = "mock"
	printMockAuthWarningTo(&buf, cfg)
	if buf.Len() != 0 {
		t.Fatalf("expected no warning when suppressed, got %q", buf.String())
	}
}
