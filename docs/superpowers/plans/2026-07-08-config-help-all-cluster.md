# Config help: `config all` + `config.cluster` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `cyoda help config all` (human table + `--format=json`) backed by a request-time `ConfigVar` registry, a `config.cluster` subtopic, and a completeness test that fails CI if any `CYODA_*` var is missing from the listing.

**Architecture:** A Go `ConfigVar` registry is the single source of truth. Root vars (`app`+`internal/cluster`) live in a table in the `help` package; plugin vars come from the pre-existing `spi.DescribablePlugin.ConfigVars()` interface, enumerated via `spi.RegisteredPlugins()` at request time. The aggregate is rendered as text (CLI default) or JSON (CLI `--format=json`, and always over HTTP). Placement avoids the `app`→`help` import cycle: the table lives in `help` (imports `spi`, never `app`); the default-drift test lives in `app_test`.

**Tech Stack:** Go 1.26, `cyoda-go-spi` v0.8.2 (`DescribablePlugin`, `ConfigVar`, `RegisteredPlugins` already present), existing `cmd/cyoda/help` embed/action framework.

## Global Constraints

- Go 1.26+; `log/slog` only (help CLI output uses `fmt.Fprint` to injected writers — user-facing, not logging).
- Never emit config **values** — `config all` lists names/defaults/descriptions only (Gate 3).
- No `cyoda-go-spi` change: the `DescribablePlugin` interface already exists and is consumed, not modified.
- No issue IDs in shipped artefacts (code, help content, JSON) — issue refs belong in commits/PR only.
- Env var scan regex (existing): `CYODA_[A-Z][A-Z0-9_]*`. Test-only exclusions (existing `isTestOnlyEnv`): prefixes `CYODA_TEST_`, `CYODA_MARKER`, `CYODA_DEBUG_`; suffix `_FOR_TESTING`.
- Registry JSON envelope mirrors `renderer.HelpPayload`: `{ "schema": 1, "version": <ver>, "vars": [...] }`.

---

### Task 1: `config.cluster` subtopic + `config.md` fixes

Moves the cluster/dispatch var block into its own subtopic (mirrors auth/cors/database/grpc/schema) and fixes the `config.cors` SEE-ALSO omission found during reconciliation.

**Files:**
- Create: `cmd/cyoda/help/content/config/cluster.md`
- Modify: `cmd/cyoda/help/content/config.md` (remove the "### Cluster and dispatch" block; add `config.cluster` to frontmatter `see_also`, SYNOPSIS list, and the bottom `## SEE ALSO`; add `config.cors` to the bottom `## SEE ALSO`)
- Test: `cmd/cyoda/help/help_test.go` (add `TestDefaultTree_ConfigClusterSubtopic`)

**Interfaces:**
- Consumes: existing `help.DefaultTree`, `Tree.Find`, `Topic.DottedPath`.
- Produces: a resolvable `config.cluster` topic; no exported Go symbols.

- [ ] **Step 1: Write the failing test**

In `cmd/cyoda/help/help_test.go`:

```go
func TestDefaultTree_ConfigClusterSubtopic(t *testing.T) {
	node := DefaultTree.Find([]string{"config", "cluster"})
	if node == nil {
		t.Fatal("config.cluster topic not found")
	}
	// The cluster/dispatch vars must now live under config.cluster, not config.
	for _, want := range []string{"CYODA_CLUSTER_ENABLED", "CYODA_SEED_NODES", "CYODA_DISPATCH_WAIT_TIMEOUT"} {
		if !strings.Contains(node.Body, want) {
			t.Errorf("config.cluster body missing %s", want)
		}
	}
	// config.md must list cluster (frontmatter see_also drives Descriptor.SeeAlso).
	cfg := DefaultTree.Find([]string{"config"})
	if cfg == nil {
		t.Fatal("config topic not found")
	}
	joined := strings.Join(cfg.Descriptor().SeeAlso, ",")
	for _, want := range []string{"config.cluster", "config.cors"} {
		if !strings.Contains(joined, want) {
			t.Errorf("config see_also missing %s (got %q)", want, joined)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/cyoda/help/ -run TestDefaultTree_ConfigClusterSubtopic -v`
Expected: FAIL — `config.cluster topic not found`.

- [ ] **Step 3: Create `content/config/cluster.md`**

Frontmatter must satisfy `parseFrontMatter` (required: `topic`, `title`, `stability` ∈ stable|evolving|experimental). Transcribe the cluster/dispatch bullets verbatim from the current `config.md` "### Cluster and dispatch" section (lines beginning `CYODA_CLUSTER_ENABLED` … `CYODA_KEEPALIVE_TIMEOUT`).

```markdown
---
topic: config.cluster
title: "cyoda cluster & dispatch configuration"
stability: stable
see_also:
  - config
  - run
---

# config.cluster

## NAME

config.cluster — multi-node clustering, gossip, and cross-node dispatch env vars.

## DESCRIPTION

<transcribe the CYODA_CLUSTER_ENABLED … CYODA_KEEPALIVE_TIMEOUT bullets
from config.md's "### Cluster and dispatch" section, unchanged>

## SEE ALSO

- config
- run
```

- [ ] **Step 4: Edit `config.md`**

Remove the entire `### Cluster and dispatch` block (the moved bullets). In its place under DESCRIPTION add a one-line pointer:

```markdown
### Cluster and dispatch

See `config.cluster` for multi-node clustering, gossip, and cross-node dispatch variables.
```

Add `- config.cluster` to the frontmatter `see_also` (after `config.schema`), to the SYNOPSIS bullet list (`- \`config.cluster\` — multi-node clustering, gossip, cross-node dispatch`), and to the bottom `## SEE ALSO`. Add `- config.cors` to the bottom `## SEE ALSO` (currently missing).

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./cmd/cyoda/help/ -run 'TestDefaultTree_ConfigClusterSubtopic|TestConfig_EnvVarCoverage' -v`
Expected: PASS (coverage test still green — vars moved within the content tree, still present).

- [ ] **Step 6: Commit**

```bash
git add cmd/cyoda/help/content/config/cluster.md cmd/cyoda/help/content/config.md cmd/cyoda/help/help_test.go
git commit -m "feat(help): add config.cluster subtopic; fix config.cors see-also"
```

---

### Task 2: `ConfigVar` type + root registry table

Defines the aggregate type and the authoritative root-var table (all `app` + `internal/cluster` vars). Plugin vars are added in Task 4; `CYODA_SCHEMA_*` belong to plugins (Task 5).

**Files:**
- Create: `cmd/cyoda/help/config_registry.go`
- Test: `cmd/cyoda/help/config_registry_test.go`

**Interfaces:**
- Produces:
  - `type ConfigVar struct { Name, Topic, Type, Default string; Required bool; Description string }` (json tags: `name,topic,type,omitempty,default,omitempty,required,omitempty,description`).
  - `var rootConfigVars []ConfigVar` (unexported) and `func RootConfigVars() []ConfigVar` (exported, returns a copy — consumed by the `app_test` binding test in Task 3).

- [ ] **Step 1: Write the failing test**

`cmd/cyoda/help/config_registry_test.go`:

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/cyoda/help/ -run TestRootConfigVars_WellFormed -v`
Expected: FAIL — `RootConfigVars` undefined.

- [ ] **Step 3: Create `config_registry.go` with the type and full table**

Transcribe every `app` + `internal/cluster` var. Authoritative sources: `app/config.go` (names + defaults, the `envX("CYODA_…", <default>)` calls at lines ~178–272) and `content/config/*.md` + `config.md` (one-line descriptions). Topics: `server` (HTTP_PORT, CONTEXT_PATH, ERROR_RESPONSE_MODE, LOG_LEVEL, SUPPRESS_BANNER, STARTUP_TIMEOUT, MAX_STATE_VISITS, MODEL_CACHE_LEASE, PROFILES, DEBUG), `admin` (ADMIN_PORT, ADMIN_BIND_ADDRESS, METRICS_REQUIRE_AUTH, METRICS_BEARER, OTEL_ENABLED), `search` (SEARCH_SNAPSHOT_TTL, SEARCH_REAP_INTERVAL, SEARCH_MAX_SORT_KEYS), `tx` (TX_TTL, TX_REAP_INTERVAL, TX_OUTCOME_TTL), `cluster` (CLUSTER_ENABLED, NODE_ID, NODE_ADDR, GRPC_NODE_ADDR, GOSSIP_ADDR, GOSSIP_STABILITY_WINDOW, SEED_NODES, HMAC_SECRET, PROXY_TIMEOUT, DISPATCH_WAIT_TIMEOUT, DISPATCH_FORWARD_TIMEOUT, TX_TOKEN_TTL, KEEPALIVE_INTERVAL, KEEPALIVE_TIMEOUT), `auth` (IAM_MODE, IAM_MOCK_ROLES, JWT_*, REQUIRE_JWT, IAM_TRUSTED_KEY_*, IAM_KEYPAIR_DEFAULT_VALIDITY_DAYS, IAM_M2M_ADMIN_ROLE_ENABLED, JWT_BOOTSTRAP_AUDIENCE, BOOTSTRAP_*, OIDC_*, HMAC_SECRET already in cluster — HMAC is cluster), `cors` (CORS_ENABLED, CORS_ALLOWED_ORIGINS), `grpc` (GRPC_PORT, COMPUTE_GRPC_ENDPOINT, COMPUTE_HTTP_BASE, COMPUTE_TOKEN). Do **not** include `*_FILE` variants (derived) or plugin vars. The Task 5 completeness test is the backstop that every non-excluded scanned var appears here or in a plugin's `ConfigVars()`.

```go
package help

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
	// … transcribe ALL remaining server/admin/search/tx/cluster/auth/cors/grpc vars …
}

// RootConfigVars returns a copy of the root var table.
func RootConfigVars() []ConfigVar {
	out := make([]ConfigVar, len(rootConfigVars))
	copy(out, rootConfigVars)
	return out
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/cyoda/help/ -run TestRootConfigVars_WellFormed -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/cyoda/help/config_registry.go cmd/cyoda/help/config_registry_test.go
git commit -m "feat(help): add ConfigVar type and root registry table"
```

---

### Task 3: Root default-drift binding test (`app_test`)

Binds the root table's `Default` strings to what `DefaultConfig()` actually produces under an empty environment, so a changed default in `config.go` fails CI. Lives in `app_test` to import both `app` and `help` without a production cycle.

**Files:**
- Create: `app/config_registry_binding_test.go` (package `app_test`)

**Interfaces:**
- Consumes: `help.RootConfigVars()`, `app.DefaultConfig()` (verify its exact name/signature in `app/config.go`; it returns the populated `Config`).

- [ ] **Step 1: Write the failing test**

```go
package app_test

import (
	"os"
	"strconv"
	"testing"

	"github.com/cyoda-platform/cyoda-go/app"
	"github.com/cyoda-platform/cyoda-go/cmd/cyoda/help"
)

// preConfigVars are read outside DefaultConfig() (profile loader / reserved),
// so their defaults can't be asserted against the Config struct.
var preConfigVars = map[string]bool{
	"CYODA_PROFILES": true,
	"CYODA_DEBUG":    true,
}

// defaultFor maps each root var to the rendered default DefaultConfig()
// yields under an empty env. Every non-preConfig root var MUST have an
// entry — the coverage assertion below enforces it.
func defaultFor(c app.Config) map[string]string {
	return map[string]string{
		"CYODA_HTTP_PORT":    strconv.Itoa(c.HTTPPort),
		"CYODA_CONTEXT_PATH": c.ContextPath,
		// … one entry per non-preConfig root var, rendered to match the
		//   table's Default string (durations via .String(), bools via
		//   strconv.FormatBool, secrets to "") …
	}
}

func TestRootConfigVars_MatchDefaults(t *testing.T) {
	for _, k := range []string{ // ensure empty env
		"CYODA_HTTP_PORT", "CYODA_CONTEXT_PATH",
	} {
		t.Setenv(k, "")
		os.Unsetenv(k)
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./app/ -run TestRootConfigVars_MatchDefaults -v`
Expected: FAIL — mappings incomplete / mismatches until `defaultFor` covers every root var and the table matches.

- [ ] **Step 3: Complete `defaultFor`**

Add one entry per non-preConfig root var, rendering each field to the exact string form used in the table (`time.Duration.String()` → `"5m"`, `strconv.FormatBool`, `strconv.Itoa`, secrets → `""`). Fix any table `Default` that disagrees with `DefaultConfig()`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./app/ -run TestRootConfigVars_MatchDefaults -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add app/config_registry_binding_test.go cmd/cyoda/help/config_registry.go
git commit -m "test(app): bind root ConfigVar defaults to DefaultConfig()"
```

---

### Task 4: `buildConfigRegistry` — aggregate root + plugin vars

Assembles the full registry at request time: root table + every registered `DescribablePlugin`'s `ConfigVars()`, with plugin topics assigned and dedup by name.

**Files:**
- Modify: `cmd/cyoda/help/config_registry.go`
- Test: `cmd/cyoda/help/config_registry_test.go`

**Interfaces:**
- Consumes: `spi.RegisteredPlugins() []string`, `spi.GetPlugin(name) (Plugin, bool)`, `spi.DescribablePlugin`, `spi.ConfigVar{Name,Description,Default,Required}`.
- Produces: `func buildConfigRegistry() []ConfigVar` (sorted by Topic then Name); `func pluginVarTopic(varName, pluginName string) string`.

- [ ] **Step 1: Write the failing test**

Add to `config_registry_test.go`. A blank import registers the real plugins so `spi.RegisteredPlugins()` returns them:

```go
import (
	_ "github.com/cyoda-platform/cyoda-go/plugins/memory"
	_ "github.com/cyoda-platform/cyoda-go/plugins/postgres"
	_ "github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/cyoda/help/ -run 'TestBuildConfigRegistry|TestPluginVarTopic' -v`
Expected: FAIL — `buildConfigRegistry`/`pluginVarTopic` undefined.

- [ ] **Step 3: Implement in `config_registry.go`**

```go
import (
	"sort"
	"strings"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/cyoda/help/ -run 'TestBuildConfigRegistry|TestPluginVarTopic' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/cyoda/help/config_registry.go cmd/cyoda/help/config_registry_test.go
git commit -m "feat(help): aggregate root + DescribablePlugin config vars"
```

---

### Task 5: Completeness test — registry ⊇ source scan (drives plugin `CYODA_SCHEMA_*` fix)

The authoritative guard. Reuses the existing `scanEnvVarsInGoSource`. It goes red because sqlite/postgres parse `CYODA_SCHEMA_*` but omit them from `ConfigVars()`; the fix adds them (Topic → `schema` via `pluginVarTopic`).

**Files:**
- Modify: `cmd/cyoda/help/config_registry_test.go`
- Modify: `plugins/sqlite/plugin.go`, `plugins/postgres/plugin.go`

**Interfaces:**
- Consumes: existing `scanEnvVarsInGoSource(t, root, dirs)`, `isTestOnlyEnv(v)`, `envVarPattern`.

- [ ] **Step 1: Write the failing test**

```go
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
```

If `repoRoot`/go.mod-walk is currently inline in `TestConfig_EnvVarCoverage`, extract it to a shared helper first (pure refactor, no behavior change).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/cyoda/help/ -run TestConfigAll_Complete -v`
Expected: FAIL — lists `CYODA_SCHEMA_EXTEND_MAX_RETRIES`, `CYODA_SCHEMA_SAVEPOINT_INTERVAL` (parsed by both SQL plugins, absent from their `ConfigVars()`).

- [ ] **Step 3: Add the schema vars to both plugins' `ConfigVars()`**

In `plugins/sqlite/plugin.go` and `plugins/postgres/plugin.go`, append to the returned slice (defaults match each plugin's `parseConfig`: sqlite savepoint `64` / retries `8`; verify postgres' via its `config.go`):

```go
{Name: "CYODA_SCHEMA_SAVEPOINT_INTERVAL", Description: "Rows per savepoint during schema extension", Default: "64"},
{Name: "CYODA_SCHEMA_EXTEND_MAX_RETRIES", Description: "Max retries on concurrent schema extension", Default: "8"},
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/cyoda/help/ -run TestConfigAll_Complete -v` (root module sees plugins via go.work)
Expected: PASS.

- [ ] **Step 5: Verify plugin modules still build/test**

Run: `cd plugins/sqlite && go test ./... && cd ../postgres && go test ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add cmd/cyoda/help/config_registry_test.go cmd/cyoda/help/help_test.go plugins/sqlite/plugin.go plugins/postgres/plugin.go
git commit -m "feat(help): completeness test; add CYODA_SCHEMA_* to plugin ConfigVars()"
```

---

### Task 6: `config all` rendering + CLI/HTTP wiring

Adds the shared JSON/text emitters, registers the HTTP action, and special-cases the CLI so `cyoda help config all` prints a table and `--format=json` prints JSON.

**Files:**
- Modify: `cmd/cyoda/help/config_registry.go` (emitters)
- Modify: `cmd/cyoda/help/actions.go` (register `config`/`all`)
- Modify: `cmd/cyoda/help/command.go` (CLI special-case)
- Test: `cmd/cyoda/help/config_registry_test.go`, `cmd/cyoda/help/command_test.go`

**Interfaces:**
- Produces: `func writeConfigAllJSON(w io.Writer) int`, `func writeConfigAllText(w io.Writer) int`.
- Consumes: `buildConfigRegistry`, existing `RunHelp` dispatch, `renderer` (not required — plain `fmt`/`json`).

- [ ] **Step 1: Write the failing tests**

```go
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
```

And in `command_test.go`, an end-to-end CLI test:

```go
func TestRunHelp_ConfigAll(t *testing.T) {
	var text bytes.Buffer
	if rc := RunHelp(DefaultTree, []string{"config", "all"}, &text, "v0.0.0", false, ""); rc != 0 {
		t.Fatalf("text rc=%d", rc)
	}
	if !strings.Contains(text.String(), "CYODA_HTTP_PORT") {
		t.Error("config all (text) missing vars")
	}
	var js bytes.Buffer
	if rc := RunHelp(DefaultTree, []string{"config", "all", "--format=json"}, &js, "v0.0.0", false, ""); rc != 0 {
		t.Fatalf("json rc=%d", rc)
	}
	if !json.Valid(js.Bytes()) {
		t.Error("config all --format=json not valid JSON")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./cmd/cyoda/help/ -run 'ConfigAll|WriteConfigAll' -v`
Expected: FAIL — emitters undefined; `RunHelp` treats `config all` as unknown.

- [ ] **Step 3: Implement emitters (`config_registry.go`)**

`version` isn't in scope inside an `ActionFunc`; use the build version the help package already exposes, or omit and let the CLI/HTTP layer not require it — simplest is a package var mirror. Here, read it from the same source `writeFullTreeJSON` uses (pass-through not available to actions, so emit `version` as the binary's version via `renderer`-independent constant is wrong). Instead, keep the envelope's `version` empty in the action and populate it only on the CLI path where `RunHelp` has `version`. Concretely, split:

```go
import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"
)

type configAllEnvelope struct {
	Schema  int         `json:"schema"`
	Version string      `json:"version"`
	Vars    []ConfigVar `json:"vars"`
}

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
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", v.Name, def, v.Description)
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(w, "cyoda help config all: %v\n", err)
		return 1
	}
	return 0
}
```

- [ ] **Step 4: Register the HTTP action (`actions.go`)**

Add to `actionRegistry`:

```go
	"config": {
		"all": {Handler: writeConfigAllJSON, ContentType: "application/json"},
	},
```

- [ ] **Step 5: CLI special-case (`command.go`)**

Before the generic topic/action dispatch (after `positional` is built and `format` validated), add:

```go
	if len(positional) == 2 && positional[0] == "config" && positional[1] == "all" {
		if resolveFormat(format, isTTY) == "json" {
			return writeConfigAllJSONVersion(out, version)
		}
		return writeConfigAllText(out)
	}
```

This runs before `tree.Find`, so the JSON path carries the real `version`; HTTP uses the registered action (version empty, consistent with action semantics).

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./cmd/cyoda/help/ -run 'ConfigAll|WriteConfigAll' -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add cmd/cyoda/help/config_registry.go cmd/cyoda/help/actions.go cmd/cyoda/help/command.go cmd/cyoda/help/config_registry_test.go cmd/cyoda/help/command_test.go
git commit -m "feat(help): render config all as text (CLI) and JSON (CLI+HTTP)"
```

---

### Task 7: HTTP endpoint coverage

Verifies `GET /help/config/all` over the real HTTP stack: 200 + `application/json` for GET, 405 for non-GET (existing help-route behavior).

**Files:**
- Create: `internal/api/help_configall_test.go`

**Interfaces:**
- Consumes: existing `RegisterHelpRoutes(mux, help.DefaultTree, "", version)` test setup pattern (mirror `internal/api/help_action_test.go`).

- [ ] **Step 1: Write the failing test**

```go
func TestHTTP_ConfigAll_JSON(t *testing.T) {
	mux := http.NewServeMux()
	RegisterHelpRoutes(mux, help.DefaultTree, "", "v0.0.0")
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/help/config/all")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type=%q", ct)
	}
	var env struct{ Vars []map[string]any `json:"vars"` }
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil || len(env.Vars) == 0 {
		t.Fatalf("decode/vars: %v len=%d", err, len(env.Vars))
	}

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/help/config/all", nil)
	pr, _ := http.DefaultClient.Do(req)
	if pr.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST status=%d want 405", pr.StatusCode)
	}
	pr.Body.Close()
}
```

Note: the HTTP help server binary registers plugins via blank import at startup; this in-process test does not, so the JSON here reflects root vars only — assert `len(Vars) > 0`, not plugin-specific names (those are covered by `TestBuildConfigRegistry` and `TestConfigAll_Complete`).

- [ ] **Step 2: Run test to verify it fails, then passes**

Run: `go test ./internal/api/ -run TestHTTP_ConfigAll_JSON -v`
Expected: FAIL first if action not wired (it is, from Task 6) → should PASS. If 200-but-wrong-shape, fix envelope.

- [ ] **Step 3: Commit**

```bash
git add internal/api/help_configall_test.go
git commit -m "test(api): cover GET /help/config/all (200 json, 405 non-GET)"
```

---

### Task 8: Gate-4 docs + full verification

**Files:**
- Modify: `README.md` (config reference: mention `cyoda help config all` / `--format=json` and the `config cluster` subtopic)
- Modify: `cmd/cyoda/help/content/config.md` (SYNOPSIS: add a line noting `config all` lists every var; JSON via `--format=json`)

**Interfaces:** none.

- [ ] **Step 1: Update README**

In the configuration section, add: “Run `cyoda help config all` for the complete env-var reference (add `--format=json` for machine-readable output); `cyoda help config cluster` covers multi-node/dispatch vars.” Keep it to 1–2 lines (compact-prose rule).

- [ ] **Step 2: Update `config.md` SYNOPSIS**

Add a bullet: `` - `config all` — flat listing of every variable (append `--format=json` for the docs-site JSON). ``

- [ ] **Step 3: Full verification**

Run: `go test ./... && (cd plugins/sqlite && go test ./...) && (cd plugins/postgres && go test ./...) && (cd plugins/memory && go test ./...)`
Run: `go vet ./...`
Run: `go build -o /tmp/cyoda ./cmd/cyoda && /tmp/cyoda help config all && /tmp/cyoda help config all --format=json | head -20 && /tmp/cyoda help config cluster | head`
Expected: all green; the three commands render the table, valid JSON, and the cluster subtopic.

- [ ] **Step 4: Commit**

```bash
git add README.md cmd/cyoda/help/content/config.md
git commit -m "docs: reference config all + config cluster (Gate 4)"
```

---

## Self-Review

**Spec coverage:** registry single-source (T2/T4) ✓; `spi.DescribablePlugin` consumption, request-time, CLI+HTTP (T4/T6/T7) ✓; root default-drift binding, pre-config exemption (T3) ✓; completeness test with fragment/`_FILE`/test-only special-cases + memory-skip + plugin-scope caveat (T5) ✓; `config all` text+JSON, HTTP always JSON (T6/T7) ✓; `config.cluster` subtopic + `config.cors` fix (T1) ✓; Gate-4 docs (T8) ✓. In-tree plugin `CYODA_SCHEMA_*` drift fixed as a consequence (T5) ✓. No new error codes → no `errors/*.md` (spec-confirmed). No cross-backend parity scenario (host behavior; plugin participation covered by completeness test) — matches spec.

**Placeholders:** the root table (T2) and `defaultFor` map (T3) are transcription-from-source *data*, not logic — the authoritative sources (`app/config.go`, `config.md`) are named, and the T3 binding test + T5 completeness test together fail CI until every row is present and correct. All logic steps carry complete code.

**Type consistency:** `ConfigVar{Name,Topic,Type,Default,Required,Description}` used identically in T2/T4/T6; `RootConfigVars()`/`buildConfigRegistry()`/`writeConfigAllJSONVersion`/`writeConfigAllText`/`pluginVarTopic` names consistent across tasks; `spi.ConfigVar{Name,Description,Default,Required}` fields match the SPI (verified).
