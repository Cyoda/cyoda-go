package app_test

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/app"
	"github.com/cyoda-platform/cyoda-go/cmd/cyoda/help"
)

// preConfigVars are root vars that DefaultConfig() does not populate — either
// because they are read outside it (profile loader, banner suppression) or
// because they belong to a different binary entirely (the compute-test-client,
// which is not part of the server's app.Config). Their table defaults can't be
// bound to a Config field, so they are exempt from the coverage assertion.
var preConfigVars = map[string]bool{
	"CYODA_PROFILES":              true, // read in app/envfiles.go before DefaultConfig() runs
	"CYODA_DEBUG":                 true, // reserved; not read anywhere yet
	"CYODA_SUPPRESS_BANNER":       true, // read directly in cmd/cyoda/main.go
	"CYODA_COMPUTE_GRPC_ENDPOINT": true, // compute-test-client side, not app.Config
	"CYODA_COMPUTE_TOKEN":         true, // compute-test-client side, not app.Config
	"CYODA_COMPUTE_HTTP_BASE":     true, // compute-test-client side, not app.Config
}

// durComponentRe splits a time.Duration.String() output into its
// (value, unit) components, e.g. "1h0m0s" -> [{1 h} {0 m} {0 s}].
var durComponentRe = regexp.MustCompile(`(\d+)(h|m|s|ms|us|ns)`)

// renderDuration normalizes time.Duration.String() to the canonical form used
// by the root ConfigVar table: trailing zero-value unit components are
// dropped (e.g. "5m0s" -> "5m", "1h0m0s" -> "1h"), but a duration that isn't a
// whole multiple of its largest unit is left as Go renders it (e.g. "1m30s").
func renderDuration(d time.Duration) string {
	matches := durComponentRe.FindAllStringSubmatch(d.String(), -1)
	end := len(matches)
	for end > 1 {
		v, _ := strconv.Atoi(matches[end-1][1])
		if v != 0 {
			break
		}
		end--
	}
	var sb strings.Builder
	for i := 0; i < end; i++ {
		sb.WriteString(matches[i][1])
		sb.WriteString(matches[i][2])
	}
	return sb.String()
}

// renderMillis renders a time.Duration stored from an integer-milliseconds
// env var (the three CYODA_OIDC_*_TIMEOUT_MS vars) back to its millisecond
// count, matching the table's "5000" form rather than Duration.String()'s "5s".
func renderMillis(d time.Duration) string {
	return strconv.FormatInt(int64(d/time.Millisecond), 10)
}

// defaultFor maps each root var to the rendered default DefaultConfig()
// yields under an empty env. Every non-preConfig root var MUST have an
// entry — the coverage assertion below enforces it.
func defaultFor(c app.Config) map[string]string {
	return map[string]string{
		// --- server ---
		"CYODA_HTTP_PORT":           strconv.Itoa(c.HTTPPort),
		"CYODA_CONTEXT_PATH":        c.ContextPath,
		"CYODA_ERROR_RESPONSE_MODE": c.ErrorResponseMode,
		"CYODA_LOG_LEVEL":           c.LogLevel,
		"CYODA_STARTUP_TIMEOUT":     renderDuration(c.StartupTimeout),
		"CYODA_MAX_STATE_VISITS":    strconv.Itoa(c.MaxStateVisits),
		"CYODA_MODEL_CACHE_LEASE":   renderDuration(c.ModelCacheLease),
		"CYODA_STORAGE_BACKEND":     c.StorageBackend,

		// --- admin ---
		"CYODA_ADMIN_PORT":           strconv.Itoa(c.Admin.Port),
		"CYODA_ADMIN_BIND_ADDRESS":   c.Admin.BindAddress,
		"CYODA_METRICS_REQUIRE_AUTH": strconv.FormatBool(c.Admin.MetricsRequireAuth),
		"CYODA_METRICS_BEARER":       "", // secret
		"CYODA_OTEL_ENABLED":         strconv.FormatBool(c.OTelEnabled),

		// --- search ---
		"CYODA_SEARCH_SNAPSHOT_TTL":  renderDuration(c.SearchSnapshotTTL),
		"CYODA_SEARCH_REAP_INTERVAL": renderDuration(c.SearchReapInterval),
		"CYODA_SEARCH_MAX_SORT_KEYS": strconv.Itoa(c.SearchMaxSortKeys),
		"CYODA_STATS_GROUP_MAX":      strconv.Itoa(c.StatsGroupMax),

		// --- tx ---
		"CYODA_TX_TTL":           renderDuration(c.Cluster.TxTTL),
		"CYODA_TX_REAP_INTERVAL": renderDuration(c.Cluster.TxReapInterval),
		"CYODA_TX_OUTCOME_TTL":   renderDuration(c.Cluster.OutcomeTTL),

		// --- cluster ---
		"CYODA_CLUSTER_ENABLED":          strconv.FormatBool(c.Cluster.Enabled),
		"CYODA_NODE_ID":                  c.Cluster.NodeID,
		"CYODA_NODE_ADDR":                c.Cluster.NodeAddr,
		"CYODA_GRPC_NODE_ADDR":           c.Cluster.GRPCNodeAddr,
		"CYODA_GOSSIP_ADDR":              c.Cluster.GossipAddr,
		"CYODA_GOSSIP_STABILITY_WINDOW":  renderDuration(c.Cluster.StabilityWindow),
		"CYODA_SEED_NODES":               strings.Join(c.Cluster.SeedNodes, ","),
		"CYODA_HMAC_SECRET":              "", // secret
		"CYODA_PROXY_TIMEOUT":            renderDuration(c.Cluster.ProxyTimeout),
		"CYODA_DISPATCH_WAIT_TIMEOUT":    renderDuration(c.Cluster.DispatchWaitTimeout),
		"CYODA_DISPATCH_FORWARD_TIMEOUT": renderDuration(c.Cluster.DispatchForwardTimeout),
		"CYODA_TX_TOKEN_TTL":             renderDuration(c.Cluster.TxTokenTTL),
		"CYODA_KEEPALIVE_INTERVAL":       strconv.Itoa(c.GRPC.KeepAliveInterval),
		"CYODA_KEEPALIVE_TIMEOUT":        strconv.Itoa(c.GRPC.KeepAliveTimeout),

		// --- auth ---
		"CYODA_IAM_MODE":                             c.IAM.Mode,
		"CYODA_IAM_MOCK_ROLES":                       strings.Join(c.IAM.MockRoles, ","),
		"CYODA_JWT_SIGNING_KEY":                      "", // secret
		"CYODA_JWT_ISSUER":                           c.IAM.JWTIssuer,
		"CYODA_JWT_AUDIENCE":                         c.IAM.JWTAudience,
		"CYODA_JWT_EXPIRY_SECONDS":                   strconv.Itoa(c.IAM.JWTExpiry),
		"CYODA_REQUIRE_JWT":                          strconv.FormatBool(c.IAM.RequireJWT),
		"CYODA_JWT_BOOTSTRAP_AUDIENCE":               c.IAM.BootstrapAudience,
		"CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED": strconv.FormatBool(c.IAM.TrustedKeyRegistrationEnabled),
		"CYODA_IAM_TRUSTED_KEY_MAX_PER_TENANT":       strconv.Itoa(c.IAM.TrustedKeyMaxPerTenant),
		"CYODA_IAM_TRUSTED_KEY_MAX_VALIDITY_DAYS":    strconv.Itoa(c.IAM.TrustedKeyMaxValidityDays),
		"CYODA_IAM_TRUSTED_KEY_MAX_JWK_PROPERTIES":   strconv.Itoa(c.IAM.TrustedKeyMaxJWKProperties),
		"CYODA_IAM_KEYPAIR_DEFAULT_VALIDITY_DAYS":    strconv.Itoa(c.IAM.KeypairDefaultValidityDays),
		"CYODA_IAM_M2M_ADMIN_ROLE_ENABLED":           strconv.FormatBool(c.IAM.M2MAdminRoleEnabled),
		"CYODA_BOOTSTRAP_CLIENT_ID":                  c.Bootstrap.ClientID,
		"CYODA_BOOTSTRAP_CLIENT_SECRET":              "", // secret
		"CYODA_BOOTSTRAP_TENANT_ID":                  c.Bootstrap.TenantID,
		"CYODA_BOOTSTRAP_USER_ID":                    c.Bootstrap.UserID,
		"CYODA_BOOTSTRAP_ROLES":                      c.Bootstrap.Roles,
		"CYODA_OIDC_REQUIRE_HTTPS":                   strconv.FormatBool(c.IAM.OIDC.RequireHTTPS),
		"CYODA_OIDC_CONNECT_TIMEOUT_MS":              renderMillis(c.IAM.OIDC.ConnectTimeout),
		"CYODA_OIDC_SOCKET_TIMEOUT_MS":               renderMillis(c.IAM.OIDC.SocketTimeout),
		"CYODA_OIDC_CONNECTION_REQUEST_TIMEOUT_MS":   renderMillis(c.IAM.OIDC.ConnectionRequestTimeout),
		"CYODA_OIDC_ALLOW_PRIVATE_NETWORKS":          strconv.FormatBool(c.IAM.OIDC.AllowPrivateNetworks),
		"CYODA_OIDC_ROLES_CLAIM":                     c.IAM.OIDC.DefaultRolesClaim,

		// --- cors ---
		"CYODA_CORS_ENABLED":         strconv.FormatBool(c.CORS.Enabled),
		"CYODA_CORS_ALLOWED_ORIGINS": strings.Join(c.CORS.AllowedOrigins, ","),

		// --- grpc ---
		"CYODA_GRPC_PORT": strconv.Itoa(c.GRPC.Port),
	}
}

// TestRootConfigVars_MatchDefaults binds help.RootConfigVars()'s Default
// strings to what app.DefaultConfig() actually produces under an empty
// environment. A future change to a default in config.go that isn't mirrored
// in the table fails this test.
func TestRootConfigVars_MatchDefaults(t *testing.T) {
	for _, v := range help.RootConfigVars() {
		t.Setenv(v.Name, "")
		os.Unsetenv(v.Name)
	}

	cfg := app.DefaultConfig()
	got := defaultFor(cfg)
	for _, v := range help.RootConfigVars() {
		if preConfigVars[v.Name] {
			continue
		}
		want, ok := got[v.Name]
		if !ok {
			t.Errorf("%s: no defaultFor mapping — add one", v.Name)
			continue
		}
		if v.Default != want {
			t.Errorf("%s: table default %q != DefaultConfig() %q", v.Name, v.Default, want)
		}
	}
}
