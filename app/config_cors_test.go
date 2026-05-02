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

func TestParseCORSAllowedOrigins_PreservesEmptyEntries(t *testing.T) {
	// Spec requires empty-after-trim entries to reach ValidateCORS so it
	// can reject them with a clear startup error. The parser must not
	// silently filter them out.
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{"double comma", "https://a.com,,https://b.com", []string{"https://a.com", "", "https://b.com"}},
		{"trailing comma", "https://a.com,", []string{"https://a.com", ""}},
		{"leading comma", ",https://a.com", []string{"", "https://a.com"}},
		{"whitespace-only entry", "https://a.com,   ,https://b.com", []string{"https://a.com", "", "https://b.com"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wildcard, got := parseCORSAllowedOrigins(tt.raw)
			if wildcard {
				t.Fatal("wildcard should be false")
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d entries (%v), want %d (%v)", len(got), got, len(tt.want), tt.want)
			}
			for i, g := range got {
				if g != tt.want[i] {
					t.Errorf("entry[%d] = %q, want %q", i, g, tt.want[i])
				}
			}
		})
	}
}

func TestParseCORSAllowedOrigins_ThenValidateCORS_RejectsEmptyEntries(t *testing.T) {
	// End-to-end env-path: the parser preserves empties, the validator
	// rejects them. This catches the regression where a deployer typos
	// "https://a.com,,https://b.com" and silently gets a 2-element
	// allowlist.
	wildcard, origins := parseCORSAllowedOrigins("https://a.com,,https://b.com")
	if wildcard {
		t.Fatal("wildcard should be false")
	}
	cfg := CORSConfig{Enabled: true, AllowedOrigins: origins}
	if err := ValidateCORS(cfg); err == nil {
		t.Error("expected ValidateCORS to reject empty entry, got nil")
	}
}

func TestValidateCORS_RejectionRules(t *testing.T) {
	tests := []struct {
		name      string
		origin    string
		wantInErr string // substring expected in error message
	}{
		{"empty entry", "", "empty"},
		{"uppercase scheme", "HTTPS://x.com", "lowercase"},
		{"uppercase host", "https://X.com", "lowercase"},
		{"non-https/http scheme", "ftp://x.com", "scheme"},
		{"missing scheme", "//x.com", "scheme"},
		{"default port https", "https://x.com:443", "default port"},
		{"default port http", "http://x.com:80", "default port"},
		{"trailing slash", "https://x.com/", "path"},
		{"path component", "https://x.com/admin", "path"},
		{"query component", "https://x.com?q=1", "query"},
		{"fragment", "https://x.com#frag", "fragment"},
		{"userinfo", "https://user@x.com", "userinfo"},
		{"non-ascii host", "https://пример.example", "punycode"},
		{"literal null", "null", "null"},
		{"literal wildcard mixed", "*", "wildcard"},
		{"glob pattern", "https://*.x.com", "wildcard"},
		{"unparseable", "://garbage", "parse"},
		{"whitespace only after trim", "   ", "empty"},
		{"port zero", "https://x.com:0", "1-65535"},
		{"port too large", "https://x.com:65536", "1-65535"},
		{"port way too large", "https://x.com:99999", "1-65535"},
		{"uppercase non-http scheme", "FTP://x.com", "lowercase"},
		{"hex-encoded IPv4 host", "http://0x7f000001", "hex"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := CORSConfig{Enabled: true, AllowedOrigins: []string{tt.origin}}
			err := ValidateCORS(cfg)
			if err == nil {
				t.Fatalf("expected error for %q, got nil", tt.origin)
			}
			if !strings.Contains(err.Error(), tt.wantInErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.wantInErr)
			}
		})
	}
}

func TestValidateCORS_PunycodeMessage(t *testing.T) {
	cfg := CORSConfig{Enabled: true, AllowedOrigins: []string{"https://пример.example"}}
	err := ValidateCORS(cfg)
	if err == nil {
		t.Fatal("expected error")
	}
	// The spec binds this exact wording for discoverability.
	want := "convert to punycode"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error %q must contain %q to be discoverable", err.Error(), want)
	}
}
