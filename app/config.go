package app

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/cluster"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

type Config struct {
	HTTPPort          int
	ContextPath       string
	ErrorResponseMode string
	MaxStateVisits    int
	LogLevel          string
	// Version is the ldflag-injected binary version string reported in the
	// REST /help payload. Defaults to "dev" when unset.
	Version            string
	IAM                IAMConfig
	GRPC               GRPCConfig
	Admin              AdminConfig
	Bootstrap          BootstrapConfig
	CORS               CORSConfig
	StorageBackend     string
	StartupTimeout     time.Duration
	Cluster            cluster.Config
	SearchSnapshotTTL  time.Duration
	SearchReapInterval time.Duration
	// ModelCacheLease is the baseline TTL for cached LOCKED model
	// descriptors. Actual expiry is jittered ±10% to prevent cross-
	// node herding. Defaults to 5m; tune via CYODA_MODEL_CACHE_LEASE.
	ModelCacheLease time.Duration
	OTelEnabled     bool
	// ExternalProcessing overrides the default gRPC processor dispatcher.
	// Used in tests to inject a LocalProcessingService.
	ExternalProcessing contract.ExternalProcessingService
	// StatsGroupMax is the cardinality ceiling for grouped-stats results
	// (POST /api/entity/stats/{entityName}/{modelVersion}/query). When the
	// service produces more distinct groupKey combinations than this value,
	// the request fails with 422 GROUP_CARDINALITY_EXCEEDED. Defaults to
	// 10000; tune via CYODA_STATS_GROUP_MAX. See spec D2.
	StatsGroupMax int
}

type AdminConfig struct {
	Port        int
	BindAddress string
	// MetricsRequireAuth (CYODA_METRICS_REQUIRE_AUTH) makes bearer auth
	// on /metrics mandatory at startup. Coupled predicate with
	// MetricsBearerToken — startup fails if required but token is empty.
	// Default false; the Helm chart sets it true.
	MetricsRequireAuth bool
	// MetricsBearerToken (CYODA_METRICS_BEARER, with _FILE suffix
	// support) is the static Bearer token required on GET /metrics when
	// non-empty. /livez and /readyz stay unauth regardless.
	MetricsBearerToken string
}

type GRPCConfig struct {
	Port              int
	KeepAliveInterval int // seconds
	KeepAliveTimeout  int // seconds
}

type IAMConfig struct {
	Mode           string
	MockUserID     string
	MockUserName   string
	MockTenantID   string
	MockTenantName string
	MockRoles      []string
	JWTSigningKey  string // PEM-encoded RSA private key (CYODA_JWT_SIGNING_KEY)
	JWTIssuer      string // JWT issuer claim (CYODA_JWT_ISSUER)
	JWTAudience    string // Expected JWT audience (CYODA_JWT_AUDIENCE); empty disables aud check
	JWTExpiry      int    // Token expiry in seconds (CYODA_JWT_EXPIRY_SECONDS)
	RequireJWT     bool   // CYODA_REQUIRE_JWT — when true, refuses to start unless mode=jwt and signing key set

	// NEW: IAM feature surface for /oauth/keys/* — passed through to auth.IAMFeatures.
	TrustedKeyRegistrationEnabled bool
	TrustedKeyMaxPerTenant        int
	TrustedKeyMaxValidityDays     int
	TrustedKeyMaxJWKProperties    int
	KeypairDefaultValidityDays    int
	BootstrapAudience             string
}

// CORSConfig controls cross-origin resource sharing for the public HTTP
// surface. See cmd/cyoda/help/content/config/cors.md for full operator-facing
// documentation.
//
// Modes (mutually exclusive):
//   - Disabled: Enabled=false. Middleware is not installed; deployers
//     handle CORS at an upstream ingress. OPTIONS returns chi default 405.
//   - Wildcard: Wildcard=true (CYODA_CORS_ALLOWED_ORIGINS=*). The literal
//     "*" is emitted in Access-Control-Allow-Origin.
//   - Allowlist: AllowedOrigins is non-empty. Exact-match only.
//   - Loopback: Enabled=true and AllowedOrigins is empty and Wildcard is
//     false. Default mode. Allows http(s)://localhost, 127.0.0.1, [::1] on
//     any port.
type CORSConfig struct {
	Enabled        bool     // CYODA_CORS_ENABLED, default true
	Wildcard       bool     // derived: true iff CYODA_CORS_ALLOWED_ORIGINS=="*"
	AllowedOrigins []string // populated only in allowlist mode (Wildcard==false, len > 0)
}

type BootstrapConfig struct {
	ClientID     string // CYODA_BOOTSTRAP_CLIENT_ID
	ClientSecret string // CYODA_BOOTSTRAP_CLIENT_SECRET (optional, generated if empty)
	TenantID     string // CYODA_BOOTSTRAP_TENANT_ID
	UserID       string // CYODA_BOOTSTRAP_USER_ID
	Roles        string // CYODA_BOOTSTRAP_ROLES (comma-separated)
}

func DefaultConfig() Config {
	// Resolve credential env vars first; _FILE paths take precedence over
	// the plain var when both are set. mustResolveSecretEnv panics if the
	// _FILE path is set but unreadable — that is a fatal startup misconfiguration.
	jwtSigningKey := envPEMFromSecret("CYODA_JWT_SIGNING_KEY")
	hmacSecret := envHexFromSecret("CYODA_HMAC_SECRET")
	bootstrapClientSecret := mustResolveSecretEnv("CYODA_BOOTSTRAP_CLIENT_SECRET")
	metricsBearerToken := mustResolveSecretEnv("CYODA_METRICS_BEARER")

	return Config{
		HTTPPort:          envInt("CYODA_HTTP_PORT", 8080),
		ContextPath:       envString("CYODA_CONTEXT_PATH", "/api"),
		ErrorResponseMode: envString("CYODA_ERROR_RESPONSE_MODE", "sanitized"),
		MaxStateVisits:    envInt("CYODA_MAX_STATE_VISITS", 10),
		LogLevel:          envString("CYODA_LOG_LEVEL", "info"),
		Version:           "dev",
		GRPC: GRPCConfig{
			Port:              envInt("CYODA_GRPC_PORT", 9090),
			KeepAliveInterval: envInt("CYODA_KEEPALIVE_INTERVAL", 10),
			KeepAliveTimeout:  envInt("CYODA_KEEPALIVE_TIMEOUT", 30),
		},
		Bootstrap: BootstrapConfig{
			ClientID:     envString("CYODA_BOOTSTRAP_CLIENT_ID", ""),
			ClientSecret: bootstrapClientSecret,
			TenantID:     envString("CYODA_BOOTSTRAP_TENANT_ID", "default-tenant"),
			UserID:       envString("CYODA_BOOTSTRAP_USER_ID", "admin"),
			Roles:        envString("CYODA_BOOTSTRAP_ROLES", "ROLE_ADMIN,ROLE_M2M"),
		},
		CORS: func() CORSConfig {
			wildcard, origins := parseCORSAllowedOrigins(envString("CYODA_CORS_ALLOWED_ORIGINS", ""))
			return CORSConfig{
				Enabled:        envBool("CYODA_CORS_ENABLED", true),
				Wildcard:       wildcard,
				AllowedOrigins: origins,
			}
		}(),
		SearchSnapshotTTL:  envDuration("CYODA_SEARCH_SNAPSHOT_TTL", 1*time.Hour),
		SearchReapInterval: envDuration("CYODA_SEARCH_REAP_INTERVAL", 5*time.Minute),
		ModelCacheLease:    envDuration("CYODA_MODEL_CACHE_LEASE", 5*time.Minute),
		OTelEnabled:        envBool("CYODA_OTEL_ENABLED", false),
		StorageBackend:     envString("CYODA_STORAGE_BACKEND", "memory"),
		StatsGroupMax:      envInt("CYODA_STATS_GROUP_MAX", 10000),
		Admin: AdminConfig{
			Port:               envInt("CYODA_ADMIN_PORT", 9091),
			BindAddress:        envString("CYODA_ADMIN_BIND_ADDRESS", "127.0.0.1"),
			MetricsRequireAuth: envBool("CYODA_METRICS_REQUIRE_AUTH", false),
			MetricsBearerToken: metricsBearerToken,
		},
		StartupTimeout: envDuration("CYODA_STARTUP_TIMEOUT", 30*time.Second),
		IAM: IAMConfig{
			Mode:                          envString("CYODA_IAM_MODE", "mock"),
			MockUserID:                    "mock-user-001",
			MockUserName:                  "Mock User",
			MockTenantID:                  "mock-tenant",
			MockTenantName:                "Mock Tenant",
			MockRoles:                     mockRolesFromEnv([]string{"ROLE_ADMIN", "ROLE_M2M"}),
			JWTSigningKey:                 jwtSigningKey,
			JWTIssuer:                     envString("CYODA_JWT_ISSUER", "cyoda"),
			JWTAudience:                   envString("CYODA_JWT_AUDIENCE", ""),
			JWTExpiry:                     envInt("CYODA_JWT_EXPIRY_SECONDS", 3600),
			RequireJWT:                    envBool("CYODA_REQUIRE_JWT", false),
			TrustedKeyRegistrationEnabled: envBool("CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED", false),
			TrustedKeyMaxPerTenant:        envInt("CYODA_IAM_TRUSTED_KEY_MAX_PER_TENANT", 10),
			TrustedKeyMaxValidityDays:     envInt("CYODA_IAM_TRUSTED_KEY_MAX_VALIDITY_DAYS", 365),
			TrustedKeyMaxJWKProperties:    envInt("CYODA_IAM_TRUSTED_KEY_MAX_JWK_PROPERTIES", 20),
			KeypairDefaultValidityDays:    envInt("CYODA_IAM_KEYPAIR_DEFAULT_VALIDITY_DAYS", 365),
			BootstrapAudience:             envString("CYODA_JWT_BOOTSTRAP_AUDIENCE", "client"),
		},
		Cluster: cluster.Config{
			Enabled:                envBool("CYODA_CLUSTER_ENABLED", false),
			NodeID:                 envString("CYODA_NODE_ID", ""),
			NodeAddr:               envString("CYODA_NODE_ADDR", "http://localhost:8080"),
			GossipAddr:             envString("CYODA_GOSSIP_ADDR", ":7946"),
			SeedNodes:              splitCSV(envString("CYODA_SEED_NODES", "")),
			StabilityWindow:        envDuration("CYODA_GOSSIP_STABILITY_WINDOW", 2*time.Second),
			TxTTL:                  envDuration("CYODA_TX_TTL", 60*time.Second),
			TxReapInterval:         envDuration("CYODA_TX_REAP_INTERVAL", 10*time.Second),
			ProxyTimeout:           envDuration("CYODA_PROXY_TIMEOUT", 30*time.Second),
			OutcomeTTL:             envDuration("CYODA_TX_OUTCOME_TTL", 5*time.Minute),
			HMACSecret:             hmacSecret,
			DispatchWaitTimeout:    envDuration("CYODA_DISPATCH_WAIT_TIMEOUT", 5*time.Second),
			DispatchForwardTimeout: envDuration("CYODA_DISPATCH_FORWARD_TIMEOUT", 30*time.Second),
		},
	}
}

// envPEMFromSecret resolves the raw value for a PEM credential via
// mustResolveSecretEnv (honouring <name>_FILE), then normalises it:
// if the value starts with "-----BEGIN" it is used as-is; otherwise it
// is treated as base64-encoded PEM (single-line friendly for .env files
// and docker env_file).
func envPEMFromSecret(key string) string {
	v := mustResolveSecretEnv(key)
	if v == "" || strings.HasPrefix(v, "-----BEGIN") {
		return v
	}
	decoded, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return v // not base64, return as-is
	}
	return string(decoded)
}

// envHexFromSecret resolves the raw value for a hex credential via
// mustResolveSecretEnv (honouring <name>_FILE), then decodes hex.
// Falls back to raw bytes if the value is not valid hex.
func envHexFromSecret(key string) []byte {
	v := mustResolveSecretEnv(key)
	if v == "" {
		return nil
	}
	b, err := hex.DecodeString(v)
	if err != nil {
		// Fall back to raw bytes if not valid hex
		return []byte(v)
	}
	return b
}

func envString(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	if v, ok := os.LookupEnv(key); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}

// mockRolesFromEnv parses CYODA_IAM_MOCK_ROLES and falls back to the
// given defaults if unset. If the variable is *set but resolves to zero
// entries* (empty string, only whitespace, only commas), we emit a WARN:
// silently granting the admin default in that case would mask an operator
// misconfiguration — they clearly intended to restrict the mock user.
func mockRolesFromEnv(fallback []string) []string {
	const key = "CYODA_IAM_MOCK_ROLES"
	raw, ok := os.LookupEnv(key)
	if !ok {
		return fallback
	}
	parts := splitCSV(raw)
	if len(parts) == 0 {
		slog.Warn("ignored empty role override, using defaults",
			"pkg", "app",
			"key", key,
			"rawValue", raw,
			"defaults", fallback,
		)
		return fallback
	}
	return parts
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

// parseCORSAllowedOrigins parses the CYODA_CORS_ALLOWED_ORIGINS env var.
// Returns wildcard=true iff the value is exactly "*". Otherwise returns the
// comma-separated list with whitespace trimmed; semantic validation is in
// ValidateCORS — empty-after-trim entries are deliberately preserved so
// that ValidateCORS can reject them with a clear error per the spec
// ("Reject empty entries — leading/trailing commas, double commas,
// whitespace-only entries error out").
// An empty raw value yields wildcard=false, origins=nil (loopback mode).
func parseCORSAllowedOrigins(raw string) (wildcard bool, origins []string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false, nil
	}
	if raw == "*" {
		return true, nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.TrimSpace(p))
	}
	return false, out
}

// Mode returns a human-readable label describing the CORS mode this config
// resolves to. Values: "disabled", "loopback", "wildcard", "allowlist".
func (c CORSConfig) Mode() string {
	switch {
	case !c.Enabled:
		return "disabled"
	case c.Wildcard:
		return "wildcard"
	case len(c.AllowedOrigins) == 0:
		return "loopback"
	default:
		return "allowlist"
	}
}

// ValidateCORS verifies the CORS configuration. It is called once at startup
// (from cmd/cyoda/main.go after slog initialisation) and returns an error
// for any invalid origin or mode combination. A non-nil return causes the
// binary to slog the error and os.Exit(1).
//
// Validation rules (full set):
//   - Wildcard==true and AllowedOrigins non-empty is a programming error
//     (parser rejects this earlier; defensive check here).
//   - Each origin in AllowedOrigins must be a valid RFC 6454 origin:
//     scheme + host + optional non-default port; no userinfo, path,
//     query, fragment, or trailing slash; lowercase scheme and host;
//     no non-ASCII characters in host (use punycode); not the literal
//     string "null"; not the literal "*" (use Wildcard mode).
func ValidateCORS(c CORSConfig) error {
	if !c.Enabled {
		return nil
	}
	if c.Wildcard && len(c.AllowedOrigins) > 0 {
		return fmt.Errorf("CYODA_CORS_ALLOWED_ORIGINS: wildcard \"*\" cannot be combined with explicit origins")
	}
	for _, o := range c.AllowedOrigins {
		if err := validateCORSOrigin(o); err != nil {
			return fmt.Errorf("CYODA_CORS_ALLOWED_ORIGINS: %w", err)
		}
	}
	return nil
}

// validateCORSOrigin returns nil iff o is a well-formed origin acceptable
// in the allowlist. Rejection rules per spec §"Allowlist normalization
// and validation". Run once at startup, never on the hot path.
func validateCORSOrigin(o string) error {
	if strings.TrimSpace(o) == "" {
		return fmt.Errorf("origin %q: empty entry not allowed", o)
	}
	if o == "null" {
		return fmt.Errorf("origin %q: literal \"null\" is not a valid allowlist entry", o)
	}
	if o == "*" || strings.Contains(o, "*") {
		return fmt.Errorf("origin %q: wildcard/glob is not supported; use CYODA_CORS_ALLOWED_ORIGINS=* for unrestricted mode", o)
	}
	// url.Parse normalises the scheme to lowercase, so we must check the
	// raw string before parsing to catch uppercase schemes.
	if idx := strings.Index(o, "://"); idx > 0 {
		rawScheme := o[:idx]
		if rawScheme != strings.ToLower(rawScheme) {
			return fmt.Errorf("origin %q: scheme must be lowercase", o)
		}
	}
	u, err := url.Parse(o)
	if err != nil {
		return fmt.Errorf("origin %q: failed to parse: %w", o, err)
	}
	if u.Scheme == "" {
		return fmt.Errorf("origin %q: missing scheme (must be http or https)", o)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("origin %q: invalid scheme %q (must be http or https)", o, u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("origin %q: missing host", o)
	}
	if host != strings.ToLower(host) {
		return fmt.Errorf("origin %q: host must be lowercase", o)
	}
	if strings.HasPrefix(host, "0x") || strings.HasPrefix(host, "0X") {
		return fmt.Errorf("origin %q: hex-encoded IPv4 host (%q) is not a canonical origin form", o, host)
	}
	if !isASCII(host) {
		return fmt.Errorf("origin %q: has non-ASCII host; convert to punycode (e.g. xn--…) before configuring", o)
	}
	if u.User != nil {
		return fmt.Errorf("origin %q: userinfo is not allowed", o)
	}
	if u.Path != "" {
		return fmt.Errorf("origin %q: path component (including trailing slash) is not allowed", o)
	}
	if u.RawQuery != "" {
		return fmt.Errorf("origin %q: query component is not allowed", o)
	}
	if u.Fragment != "" {
		return fmt.Errorf("origin %q: fragment is not allowed", o)
	}
	if port := u.Port(); port != "" {
		if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
			return fmt.Errorf("origin %q: default port (:%s) must be omitted", o, port)
		}
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("origin %q: port %q is not a valid TCP port (1-65535)", o, port)
		}
	}
	return nil
}

func isASCII(s string) bool {
	for _, r := range s {
		if r > 0x7F {
			return false
		}
	}
	return true
}

// ValidateIAM enforces startup-time IAM correctness. When CYODA_REQUIRE_JWT
// is set, mock mode is rejected and the signing key must be present. In JWT
// mode (regardless of RequireJWT) IAMFeatures are validated so that invalid
// env values for BootstrapAudience, TrustedKeyMaxPerTenant, MaxValidityDays,
// etc. fail startup rather than being silently ignored.
// Callers must invoke this before wiring auth in New().
func ValidateIAM(iam IAMConfig) error {
	// RequireJWT demands jwt mode — reject mock so a misconfigured Helm deploy
	// can never silently fall back to unauthenticated access.
	if iam.RequireJWT && iam.Mode != "jwt" {
		return fmt.Errorf("CYODA_REQUIRE_JWT=true but CYODA_IAM_MODE=%q (expected \"jwt\")", iam.Mode)
	}
	// Mock mode needs no further validation.
	if iam.Mode == "mock" {
		return nil
	}
	// JWT mode: signing-key presence gated on RequireJWT; feature validation
	// is unconditional so misconfigured env values fail at startup.
	if iam.RequireJWT && iam.JWTSigningKey == "" {
		return fmt.Errorf("CYODA_REQUIRE_JWT=true but CYODA_JWT_SIGNING_KEY is empty")
	}
	if err := iam.AuthIAMFeatures().Validate(); err != nil {
		return fmt.Errorf("IAM features validation: %w", err)
	}
	return nil
}

// AuthIAMFeatures projects the new IAM-feature fields out of IAMConfig
// into the auth-package value struct.
func (c IAMConfig) AuthIAMFeatures() auth.IAMFeatures {
	return auth.IAMFeatures{
		TrustedKeyRegistrationEnabled: c.TrustedKeyRegistrationEnabled,
		TrustedKeyMaxPerTenant:        c.TrustedKeyMaxPerTenant,
		TrustedKeyMaxValidityDays:     c.TrustedKeyMaxValidityDays,
		TrustedKeyMaxJWKProperties:    c.TrustedKeyMaxJWKProperties,
		KeypairDefaultValidityDays:    c.KeypairDefaultValidityDays,
		BootstrapAudience:             c.BootstrapAudience,
	}
}
