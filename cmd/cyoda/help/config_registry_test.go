package help

import "testing"

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
