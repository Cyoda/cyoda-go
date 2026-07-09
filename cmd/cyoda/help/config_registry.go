package help

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// ConfigVar documents one CYODA_* configuration variable for the
// `cyoda help config all` listing. Values are never rendered — only
// names, types, defaults, and descriptions.
type ConfigVar struct {
	Name        string `json:"name"`
	Topic       string `json:"topic"`
	Type        string `json:"type,omitempty"`     // int|duration|bool|string|csv; "" if unknown (plugin vars)
	Default     string `json:"default,omitempty"`  // rendered default; "" for secrets/unset
	Required    bool   `json:"required,omitempty"`
	Description string `json:"description"`
}

// rootConfigVars is the authoritative table for app + internal/cluster
// env vars. Plugin vars are contributed at request time via
// spi.DescribablePlugin (see buildConfigRegistry). Kept in sync with
// app.DefaultConfig() by TestRootConfigVars_MatchDefaults (app_test)
// and completed by TestConfigAll_Complete (config_registry_test.go).
var rootConfigVars = []ConfigVar{
	// --- server ---
	{Name: "CYODA_HTTP_PORT", Topic: "server", Type: "int", Default: "8080", Description: "HTTP listen port."},
	{Name: "CYODA_CONTEXT_PATH", Topic: "server", Type: "string", Default: "/api", Description: "URL prefix for all routes."},
	{Name: "CYODA_ERROR_RESPONSE_MODE", Topic: "server", Type: "string", Default: "sanitized", Description: "Error detail level: sanitized (generic message + ticket UUID for 5xx) or verbose (internal detail included; development only)."},
	{Name: "CYODA_LOG_LEVEL", Topic: "server", Type: "string", Default: "info", Description: "Log level: debug|info|warn|error."},
	{Name: "CYODA_SUPPRESS_BANNER", Topic: "server", Type: "bool", Default: "false", Description: "Silence startup and mock-auth banners (CI/tests only)."},
	{Name: "CYODA_STARTUP_TIMEOUT", Topic: "server", Type: "duration", Default: "30s", Description: "Deadline for plugin init, TM init, and (cluster mode) the gossip seed-join retry loop."},
	{Name: "CYODA_MAX_STATE_VISITS", Topic: "server", Type: "int", Default: "10", Description: "Max visits per state in workflow cascade."},
	{Name: "CYODA_MODEL_CACHE_LEASE", Topic: "server", Type: "duration", Default: "5m", Description: "Model cache lease duration; actual expiry is jittered ±10%."},
	{Name: "CYODA_STORAGE_BACKEND", Topic: "server", Type: "string", Default: "memory", Description: "Storage backend selection (memory|sqlite|postgres)."},
	{Name: "CYODA_PROFILES", Topic: "server", Type: "csv", Default: "", Description: "Comma-separated profile names; loads cyoda.<name>.env files before the process's own environment is consulted."},
	{Name: "CYODA_DEBUG", Topic: "server", Type: "", Default: "", Description: "Reserved; not currently read by the server."},

	// --- admin ---
	{Name: "CYODA_ADMIN_PORT", Topic: "admin", Type: "int", Default: "9091", Description: "Admin port for health and metrics."},
	{Name: "CYODA_ADMIN_BIND_ADDRESS", Topic: "admin", Type: "string", Default: "127.0.0.1", Description: "Admin listener bind address."},
	{Name: "CYODA_METRICS_REQUIRE_AUTH", Topic: "admin", Type: "bool", Default: "false", Description: "Require Bearer auth on /metrics; startup fails if true and CYODA_METRICS_BEARER is empty."},
	{Name: "CYODA_METRICS_BEARER", Topic: "admin", Type: "string", Default: "", Description: "Static Bearer token for GET /metrics. Supports _FILE suffix."},
	{Name: "CYODA_OTEL_ENABLED", Topic: "admin", Type: "bool", Default: "false", Description: "Enable OpenTelemetry tracing and metrics."},

	// --- search ---
	{Name: "CYODA_SEARCH_SNAPSHOT_TTL", Topic: "search", Type: "duration", Default: "1h", Description: "Search snapshot TTL."},
	{Name: "CYODA_SEARCH_REAP_INTERVAL", Topic: "search", Type: "duration", Default: "5m", Description: "Search snapshot reap interval."},
	{Name: "CYODA_SEARCH_MAX_SORT_KEYS", Topic: "search", Type: "int", Default: "16", Description: "Maximum number of sort keys per search request; values <= 0 clamp to the default."},
	{Name: "CYODA_STATS_GROUP_MAX", Topic: "search", Type: "int", Default: "10000", Description: "Cardinality ceiling for grouped-stats results; also caps the request limit parameter. Values <= 0 clamp to the default."},

	// --- tx ---
	{Name: "CYODA_TX_TTL", Topic: "tx", Type: "duration", Default: "1m", Description: "Transaction TTL."},
	{Name: "CYODA_TX_REAP_INTERVAL", Topic: "tx", Type: "duration", Default: "10s", Description: "Transaction reap interval."},
	{Name: "CYODA_TX_OUTCOME_TTL", Topic: "tx", Type: "duration", Default: "5m", Description: "Transaction outcome TTL."},

	// --- cluster ---
	{Name: "CYODA_CLUSTER_ENABLED", Topic: "cluster", Type: "bool", Default: "false", Description: "Enable multi-node clustering."},
	{Name: "CYODA_NODE_ID", Topic: "cluster", Type: "string", Default: "", Description: "Unique node identifier; required when CYODA_CLUSTER_ENABLED=true."},
	{Name: "CYODA_NODE_ADDR", Topic: "cluster", Type: "string", Default: "http://localhost:8080", Description: "This node's HTTP base URL; must include scheme."},
	{Name: "CYODA_GRPC_NODE_ADDR", Topic: "cluster", Type: "string", Default: "", Description: "This node's gRPC endpoint advertised to peers (host:port, no scheme)."},
	{Name: "CYODA_GOSSIP_ADDR", Topic: "cluster", Type: "string", Default: ":7946", Description: "Gossip protocol listen address ([host]:port)."},
	{Name: "CYODA_GOSSIP_STABILITY_WINDOW", Topic: "cluster", Type: "duration", Default: "2s", Description: "Gossip stability window."},
	{Name: "CYODA_SEED_NODES", Topic: "cluster", Type: "csv", Default: "", Description: "Comma-separated list of seed node addresses."},
	{Name: "CYODA_HMAC_SECRET", Topic: "cluster", Type: "string", Default: "", Description: "Hex-encoded HMAC secret for inter-node dispatch authentication; required when CYODA_CLUSTER_ENABLED=true. Supports _FILE suffix."},
	{Name: "CYODA_PROXY_TIMEOUT", Topic: "cluster", Type: "duration", Default: "30s", Description: "Request proxy timeout."},
	{Name: "CYODA_DISPATCH_WAIT_TIMEOUT", Topic: "cluster", Type: "duration", Default: "5s", Description: "How long the dispatcher polls gossip for a compute member with matching tags."},
	{Name: "CYODA_DISPATCH_FORWARD_TIMEOUT", Topic: "cluster", Type: "duration", Default: "30s", Description: "HTTP timeout for the cross-node forwarding call."},
	{Name: "CYODA_TX_TOKEN_TTL", Topic: "cluster", Type: "duration", Default: "1m30s", Description: "TTL of the signed transaction routing token minted on processor/criteria dispatch."},
	{Name: "CYODA_KEEPALIVE_INTERVAL", Topic: "cluster", Type: "int", Default: "10", Description: "Keep-alive send interval in seconds."},
	{Name: "CYODA_KEEPALIVE_TIMEOUT", Topic: "cluster", Type: "int", Default: "30", Description: "Keep-alive timeout in seconds."},

	// --- auth ---
	{Name: "CYODA_IAM_MODE", Topic: "auth", Type: "string", Default: "mock", Description: "Authentication mode: mock or jwt."},
	{Name: "CYODA_IAM_MOCK_ROLES", Topic: "auth", Type: "csv", Default: "ROLE_ADMIN,ROLE_M2M", Description: "Comma-separated default user roles assigned to all requests in mock mode."},
	{Name: "CYODA_JWT_SIGNING_KEY", Topic: "auth", Type: "string", Default: "", Description: "RSA private key in PEM format; required in jwt mode. Supports _FILE suffix."},
	{Name: "CYODA_JWT_ISSUER", Topic: "auth", Type: "string", Default: "cyoda", Description: "JWT issuer claim (iss)."},
	{Name: "CYODA_JWT_AUDIENCE", Topic: "auth", Type: "string", Default: "", Description: "Expected JWT audience (aud); empty disables the audience check."},
	{Name: "CYODA_JWT_EXPIRY_SECONDS", Topic: "auth", Type: "int", Default: "3600", Description: "Token lifetime in seconds."},
	{Name: "CYODA_REQUIRE_JWT", Topic: "auth", Type: "bool", Default: "false", Description: "Production safety floor; refuses to start unless IAM mode is jwt and a signing key is set."},
	{Name: "CYODA_JWT_BOOTSTRAP_AUDIENCE", Topic: "auth", Type: "string", Default: "client", Description: "Audience for the bootstrap signing key derived from CYODA_JWT_SIGNING_KEY; client or human."},
	{Name: "CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED", Topic: "auth", Type: "bool", Default: "false", Description: "Gates the /oauth/keys/trusted/* endpoints; disabled returns 404 FEATURE_DISABLED."},
	{Name: "CYODA_IAM_TRUSTED_KEY_MAX_PER_TENANT", Topic: "auth", Type: "int", Default: "10", Description: "Per-tenant cap on registered trusted keys; 0 means unbounded."},
	{Name: "CYODA_IAM_TRUSTED_KEY_MAX_VALIDITY_DAYS", Topic: "auth", Type: "int", Default: "365", Description: "Default validity for trusted keys when the registration request omits validTo."},
	{Name: "CYODA_IAM_TRUSTED_KEY_MAX_JWK_PROPERTIES", Topic: "auth", Type: "int", Default: "20", Description: "Caps the number of properties in a registered JWK."},
	{Name: "CYODA_IAM_KEYPAIR_DEFAULT_VALIDITY_DAYS", Topic: "auth", Type: "int", Default: "365", Description: "Default validity for the bootstrap signing key and runtime-issued keypairs."},
	{Name: "CYODA_IAM_M2M_ADMIN_ROLE_ENABLED", Topic: "auth", Type: "bool", Default: "false", Description: "Gates the withAdminRole=true query parameter on POST /clients."},
	{Name: "CYODA_BOOTSTRAP_CLIENT_ID", Topic: "auth", Type: "string", Default: "", Description: "Bootstrap M2M client ID."},
	{Name: "CYODA_BOOTSTRAP_CLIENT_SECRET", Topic: "auth", Type: "string", Default: "", Description: "Bootstrap M2M client secret; must be set when CYODA_BOOTSTRAP_CLIENT_ID is set. Supports _FILE suffix."},
	{Name: "CYODA_BOOTSTRAP_TENANT_ID", Topic: "auth", Type: "string", Default: "default-tenant", Description: "Tenant for the bootstrap client."},
	{Name: "CYODA_BOOTSTRAP_USER_ID", Topic: "auth", Type: "string", Default: "admin", Description: "User ID for the bootstrap client."},
	{Name: "CYODA_BOOTSTRAP_ROLES", Topic: "auth", Type: "csv", Default: "ROLE_ADMIN,ROLE_M2M", Description: "Comma-separated roles granted to the bootstrap client."},
	{Name: "CYODA_OIDC_REQUIRE_HTTPS", Topic: "auth", Type: "bool", Default: "true", Description: "Reject federated OIDC provider registration when the well-known config URI is not https."},
	{Name: "CYODA_OIDC_CONNECT_TIMEOUT_MS", Topic: "auth", Type: "int", Default: "5000", Description: "TCP connect timeout in milliseconds for OIDC discovery and JWKS endpoint fetches."},
	{Name: "CYODA_OIDC_SOCKET_TIMEOUT_MS", Topic: "auth", Type: "int", Default: "5000", Description: "HTTP read timeout in milliseconds for OIDC discovery and JWKS endpoint fetches."},
	{Name: "CYODA_OIDC_CONNECTION_REQUEST_TIMEOUT_MS", Topic: "auth", Type: "int", Default: "5000", Description: "Connection-pool request timeout in milliseconds for OIDC discovery and JWKS endpoint fetches."},
	{Name: "CYODA_OIDC_ALLOW_PRIVATE_NETWORKS", Topic: "auth", Type: "bool", Default: "false", Description: "Bypass the SSRF blocklist so private-network OIDC providers can be registered; test/dev only, never in production."},
	{Name: "CYODA_OIDC_ROLES_CLAIM", Topic: "auth", Type: "string", Default: "roles", Description: "JWT claim name from which role values are read for tokens issued by a federated OIDC provider."},

	// --- cors ---
	{Name: "CYODA_CORS_ENABLED", Topic: "cors", Type: "bool", Default: "true", Description: "Enable CORS middleware; false hands CORS handling to an upstream ingress."},
	{Name: "CYODA_CORS_ALLOWED_ORIGINS", Topic: "cors", Type: "csv", Default: "", Description: "Comma-separated allowed origins, or * for wildcard mode; empty selects loopback mode."},

	// --- grpc ---
	{Name: "CYODA_GRPC_PORT", Topic: "grpc", Type: "int", Default: "9090", Description: "gRPC listen port."},
	{Name: "CYODA_COMPUTE_GRPC_ENDPOINT", Topic: "grpc", Type: "string", Default: "", Description: "gRPC endpoint for a compute node to connect to (compute-client side)."},
	{Name: "CYODA_COMPUTE_TOKEN", Topic: "grpc", Type: "string", Default: "", Description: "Bearer token for compute-node authentication (compute-client side)."},
	{Name: "CYODA_COMPUTE_HTTP_BASE", Topic: "grpc", Type: "string", Default: "", Description: "HTTP base URL of the cyoda instance a compute node calls back into (compute-client side)."},
}

// RootConfigVars returns a copy of the root var table.
func RootConfigVars() []ConfigVar {
	out := make([]ConfigVar, len(rootConfigVars))
	copy(out, rootConfigVars)
	return out
}

// pluginVarTopic maps a plugin-contributed var to its help subtopic.
// Schema-extension vars are shared by SQL backends; SQLite/Postgres
// vars group under "database". Unknown prefixes fall back to the
// plugin name so a new backend is never silently mis-grouped.
func pluginVarTopic(varName, pluginName string) string {
	switch {
	case strings.HasPrefix(varName, "CYODA_SCHEMA_"):
		return "schema"
	case strings.HasPrefix(varName, "CYODA_SQLITE_"), strings.HasPrefix(varName, "CYODA_POSTGRES_"):
		return "database"
	default:
		return pluginName
	}
}

// buildConfigRegistry assembles the full config-var registry at call
// time: the root table plus every registered DescribablePlugin's
// ConfigVars(). Deduped by name (root wins). Sorted by topic, then name.
func buildConfigRegistry() []ConfigVar {
	out := make([]ConfigVar, 0, len(rootConfigVars))
	seen := map[string]bool{}
	for _, v := range rootConfigVars {
		out = append(out, v)
		seen[v.Name] = true
	}
	for _, name := range spi.RegisteredPlugins() {
		p, ok := spi.GetPlugin(name)
		if !ok {
			continue
		}
		dp, ok := p.(spi.DescribablePlugin)
		if !ok {
			continue // e.g. memory — no config vars
		}
		for _, cv := range dp.ConfigVars() {
			if seen[cv.Name] {
				continue
			}
			seen[cv.Name] = true
			out = append(out, ConfigVar{
				Name:        cv.Name,
				Topic:       pluginVarTopic(cv.Name, name),
				Default:     cv.Default,
				Required:    cv.Required,
				Description: cv.Description,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Topic != out[j].Topic {
			return out[i].Topic < out[j].Topic
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// configAllEnvelope is the JSON shape for `cyoda help config all
// --format=json` and `GET /help/config/all`.
type configAllEnvelope struct {
	Schema  int         `json:"schema"`
	Version string      `json:"version"`
	Vars    []ConfigVar `json:"vars"`
}

// writeConfigAllJSONVersion writes the config-all JSON envelope with an
// explicit version. Used by the CLI special-case in command.go, which
// has the real binary version in scope.
func writeConfigAllJSONVersion(w io.Writer, version string) int {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(configAllEnvelope{Schema: 1, Version: version, Vars: buildConfigRegistry()}); err != nil {
		fmt.Fprintf(w, "cyoda help config all: encode: %v\n", err)
		return 1
	}
	return 0
}

// writeConfigAllJSON is the action-registry entry (HTTP + generic CLI
// action dispatch); version is unknown here, so it is emitted empty.
func writeConfigAllJSON(w io.Writer) int { return writeConfigAllJSONVersion(w, "") }

// writeConfigAllText renders the full config-var registry as a
// tab-aligned table grouped by topic, for `cyoda help config all` on a
// terminal.
func writeConfigAllText(w io.Writer) int {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	var topic string
	for _, v := range buildConfigRegistry() {
		if v.Topic != topic {
			topic = v.Topic
			fmt.Fprintf(tw, "\n[%s]\n", topic)
		}
		def := v.Default
		if v.Required {
			def = "(required)"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", v.Name, v.Type, def, v.Description)
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(w, "cyoda help config all: %v\n", err)
		return 1
	}
	return 0
}
