package app

import (
	"strings"
	"testing"
)

func TestValidateCORS_HappyPaths(t *testing.T) {
	tests := []struct {
		name string
		cfg  CORSConfig
	}{
		{
			name: "disabled",
			cfg:  CORSConfig{Enabled: false},
		},
		{
			name: "loopback (default)",
			cfg:  CORSConfig{Enabled: true},
		},
		{
			name: "wildcard",
			cfg:  CORSConfig{Enabled: true, Wildcard: true},
		},
		{
			name: "allowlist single",
			cfg:  CORSConfig{Enabled: true, AllowedOrigins: []string{"https://admin.example.com"}},
		},
		{
			name: "allowlist multiple",
			cfg: CORSConfig{Enabled: true, AllowedOrigins: []string{
				"https://admin.example.com",
				"http://localhost:3000",
				"https://[::1]:8443",
				"https://xn--e1afmkfd.example",
			}},
		},
		{
			name: "allowlist with non-default port",
			cfg:  CORSConfig{Enabled: true, AllowedOrigins: []string{"http://x.com:8080"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateCORS(tt.cfg); err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestValidateCORS_Mode(t *testing.T) {
	tests := []struct {
		name string
		cfg  CORSConfig
		want string
	}{
		{"disabled", CORSConfig{Enabled: false}, "disabled"},
		{"loopback", CORSConfig{Enabled: true}, "loopback"},
		{"wildcard", CORSConfig{Enabled: true, Wildcard: true}, "wildcard"},
		{"allowlist", CORSConfig{Enabled: true, AllowedOrigins: []string{"https://x.com"}}, "allowlist"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.Mode(); got != tt.want {
				t.Errorf("Mode() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestValidateCORS_RejectsWildcardMixedWithExplicit(t *testing.T) {
	// CORSConfig itself shouldn't be reachable in this state via env parsing,
	// but ValidateCORS must defend against it at the boundary.
	cfg := CORSConfig{Enabled: true, Wildcard: true, AllowedOrigins: []string{"https://x.com"}}
	err := ValidateCORS(cfg)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "wildcard") {
		t.Errorf("error message should mention wildcard: %v", err)
	}
}
