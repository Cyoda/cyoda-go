package help

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	_ "github.com/cyoda-platform/cyoda-go/plugins/memory"
	_ "github.com/cyoda-platform/cyoda-go/plugins/postgres"
	_ "github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

func TestRootConfigVars_WellFormed(t *testing.T) {
	vars := RootConfigVars()
	if len(vars) < 40 {
		t.Fatalf("expected the full root var set, got %d", len(vars))
	}
	seen := map[string]bool{}
	validTopic := map[string]bool{
		"server": true, "admin": true, "search": true, "tx": true,
		"cluster": true, "auth": true, "cors": true, "grpc": true,
	}
	for _, v := range vars {
		if v.Name == "" || v.Description == "" {
			t.Errorf("var %+v missing name/description", v)
		}
		if seen[v.Name] {
			t.Errorf("duplicate var %s", v.Name)
		}
		seen[v.Name] = true
		if !validTopic[v.Topic] {
			t.Errorf("var %s has non-root topic %q", v.Name, v.Topic)
		}
	}
	// Cluster vars are root-owned (they live in DefaultConfig()).
	if !seen["CYODA_CLUSTER_ENABLED"] || !seen["CYODA_NODE_ID"] {
		t.Error("cluster vars missing from root table")
	}
	// Schema/database vars are plugin-owned — must NOT be in the root table.
	if seen["CYODA_SQLITE_PATH"] || seen["CYODA_SCHEMA_SAVEPOINT_INTERVAL"] {
		t.Error("plugin-owned var leaked into root table")
	}
}

func TestBuildConfigRegistry_IncludesPluginVars(t *testing.T) {
	reg := buildConfigRegistry()
	idx := map[string]ConfigVar{}
	for _, v := range reg {
		idx[v.Name] = v
	}
	// Plugin vars present, topic assigned.
	if v, ok := idx["CYODA_SQLITE_PATH"]; !ok || v.Topic != "database" {
		t.Errorf("CYODA_SQLITE_PATH: got %+v, want topic=database", v)
	}
	if v, ok := idx["CYODA_POSTGRES_URL"]; !ok || !v.Required {
		t.Errorf("CYODA_POSTGRES_URL: got %+v, want Required", v)
	}
	// Root vars still present.
	if _, ok := idx["CYODA_HTTP_PORT"]; !ok {
		t.Error("root var CYODA_HTTP_PORT missing from aggregate")
	}
	// memory implements no ConfigVars() — must not blow up (skipped).
}

// TestConfigAll_Complete is the authoritative completeness guard: every
// CYODA_* var referenced in source (cmd, app, plugins, internal) must
// appear in the aggregate registry rendered by `cyoda help config all`.
// Nothing can be added to a plugin's parseConfig without also showing up
// here.
func TestConfigAll_Complete(t *testing.T) {
	root := repoRoot(t) // reuse the go.mod-walk helper from TestConfig_EnvVarCoverage
	scanned := scanEnvVarsInGoSource(t, root, []string{"cmd", "app", "plugins", "internal"})

	registry := map[string]bool{}
	for _, v := range buildConfigRegistry() {
		registry[v.Name] = true
	}

	var missing []string
	for name := range scanned {
		if isTestOnlyEnv(name) {
			continue
		}
		if strings.HasSuffix(name, "_") {
			continue // comment fragment, e.g. CYODA_POSTGRES_
		}
		if strings.HasSuffix(name, "_FILE") {
			continue // derived variant of a base var
		}
		if !registry[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("CYODA_* vars in source but absent from `config all`:\n  %s", strings.Join(missing, "\n  "))
	}
}

func TestPluginVarTopic(t *testing.T) {
	cases := map[[2]string]string{
		{"CYODA_SCHEMA_SAVEPOINT_INTERVAL", "sqlite"}: "schema",
		{"CYODA_SQLITE_PATH", "sqlite"}:               "database",
		{"CYODA_POSTGRES_URL", "postgres"}:            "database",
		{"CYODA_WHATEVER", "future"}:                  "future",
	}
	for in, want := range cases {
		if got := pluginVarTopic(in[0], in[1]); got != want {
			t.Errorf("pluginVarTopic(%q,%q)=%q want %q", in[0], in[1], got, want)
		}
	}
}

func TestWriteConfigAllJSON_Envelope(t *testing.T) {
	var buf bytes.Buffer
	if rc := writeConfigAllJSON(&buf); rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	var env struct {
		Schema  int         `json:"schema"`
		Version string      `json:"version"`
		Vars    []ConfigVar `json:"vars"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if env.Schema != 1 || len(env.Vars) < 40 {
		t.Fatalf("schema=%d vars=%d", env.Schema, len(env.Vars))
	}
	names := map[string]bool{}
	for _, v := range env.Vars {
		names[v.Name] = true
	}
	if !names["CYODA_HTTP_PORT"] || !names["CYODA_SQLITE_PATH"] {
		t.Error("json missing expected vars")
	}
}

func TestWriteConfigAllText_ListsVars(t *testing.T) {
	var buf bytes.Buffer
	if rc := writeConfigAllText(&buf); rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	s := buf.String()
	for _, want := range []string{"CYODA_HTTP_PORT", "CYODA_CLUSTER_ENABLED", "CYODA_SQLITE_PATH", "cluster", "database"} {
		if !strings.Contains(s, want) {
			t.Errorf("text output missing %q", want)
		}
	}
}
