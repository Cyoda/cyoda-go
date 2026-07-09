package help

import (
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
