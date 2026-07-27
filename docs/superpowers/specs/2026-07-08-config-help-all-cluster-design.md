# Config help: `config all` full listing + `config.cluster` subtopic

Issue: cyoda-go#395 (milestone v0.8.3). Cross-repo: Cyoda/cyoda-go-cassandra#60 (mirror, not implemented here).

## Motivation

`cyoda help config` documents 100% of `CYODA_*` vars (enforced by `TestConfig_EnvVarCoverage`), but:

1. The subtopic tree breaks out only `auth`, `cors`, `database`, `grpc`, `schema`. Cluster/dispatch, server, admin/metrics, and search/transaction vars live only in prose in the parent `config.md`. Multi-node is the primary target — cluster/dispatch config should be a first-class subtopic.
2. No single command authoritatively lists **all** vars, and there is no machine-consumable form for the docs site.

## Design

### Source of truth — a `ConfigVar` registry, assembled at request time

The listing is rendered from a Go registry, never from parsing markdown. Two contributors, aggregated on each request so runtime-registered plugins are visible:

- **Plugin vars** come from the SPI's existing optional interface. `cyoda-go-spi` already defines `DescribablePlugin { Plugin; ConfigVars() []ConfigVar }`; postgres, sqlite, and the commercial backend already implement it (memory deliberately does not). The root **consumes** it — `spi.RegisteredPlugins()` → `spi.GetPlugin(name)` → type-assert `DescribablePlugin` → `ConfigVars()`. This is **not** an SPI change (the interface pre-exists, unused by the root). It is reachable identically from the CLI (`cmd/cyoda`) and the HTTP help routes (`app/app.go`), and surfaces the out-of-tree commercial plugin with zero root import.
- **Root vars** (`app` + `internal/cluster`, which no plugin owns) come from a root-side table.

Aggregate type (richer than `spi.ConfigVar`, which is only `{Name, Description, Default, Required}`):

```go
type ConfigVar struct {
    Name        string // "CYODA_HTTP_PORT"
    Topic       string // server|admin|search|tx|cluster|auth|cors|database|grpc|schema|<plugin-name>
    Type        string // int|duration|bool|string|csv — best-effort; "" for plugin vars (SPI has no Type)
    Default     string // rendered default; "" for secrets/unset
    Description string
    FileSuffix  bool   // supports the CYODA_*_FILE variant
    Secret      bool   // root-known secrets; not load-bearing (values are never emitted)
}
```

Folding `spi.ConfigVar` → aggregate: `Name`/`Default`/`Description` map over; `Topic` = plugin name; `Type` = `""`. Dedup by `Name` (shared vars like `CYODA_SCHEMA_EXTEND_MAX_RETRIES` may appear in both a plugin's list and the scan) — first writer wins, root table before plugins.

### Placement (avoids an import cycle)

`app` imports `help` (`help.DefaultTree`), so `help` must **not** import `app`.

- The **root `ConfigVar` table** and `buildConfigRegistry()` live in the `help` package. `help` imports `spi` (a leaf module — no cycle) to enumerate plugins. No import of `app`.
- The **default-drift binding test** lives in an external `app_test` package: it imports both `app` (for `DefaultConfig()`) and `help` (for the exported root table). Test-only cross-import, no production cycle.
- The **completeness test** lives in the `help` package and blank-imports the plugins (root `go.mod` already requires them) so `spi.RegisteredPlugins()` returns them during the test.

### Command surface

- `cyoda help config all` — human-readable table grouped by topic (CLI default).
- `cyoda help config all --format=json` — JSON envelope `{schema:1, version, vars:[…]}`, mirroring the existing `HelpPayload` shape.
- `GET /help/config/all` — always `application/json` (consistent with all HTTP help, which is JSON by design).

Wiring (no action-framework change; openapi special-case untouched):

- Shared: `writeConfigAllJSON(w)` / `writeConfigAllText(w)`, both over `buildConfigRegistry()`.
- HTTP: register `config`/`all` in `actionRegistry` with `ContentType: application/json`, handler `writeConfigAllJSON`. Served by the existing action-mirror.
- CLI: `command.go` special-cases positional `["config","all"]` — `--format=json` → `writeConfigAllJSON`, otherwise → `writeConfigAllText`. Mirrors branches `command.go` already has.

### `config.cluster` subtopic

- New `content/config/cluster.md`; move the cluster/dispatch var block out of `config.md`'s prose into it, mirroring the existing subtopic structure. `config.md` keeps its synopsis bullet + SEE ALSO entry.
- Fold in the `config.cors` SEE-ALSO fix (present in frontmatter/synopsis, missing from the bottom list).

### Completeness test

Assert `names(buildConfigRegistry()) ⊇ scanEnvVarsInGoSource(root + plugins)`, reusing the existing scan. Special-cases:

- Strip trailing-underscore comment fragments (`CYODA_POSTGRES_`, `CYODA_SQLITE_`).
- Exclude `CYODA_*_FILE` from the scan — a derived variant, represented as `FileSuffix` on the base var, not a separate entry.
- Keep `isTestOnlyEnv` exclusions (`CYODA_TEST_*`, `CYODA_MARKER*`, `CYODA_DEBUG_*`, `*_FOR_TESTING`).
- Tolerate a registered plugin that does not implement `DescribablePlugin` (memory) — type-assert and skip.
- **Scope caveat**: the scan cannot see the out-of-tree commercial plugin, so this test is authoritative for the OSS build only. The commercial backend enforces its own completeness (Cyoda/cyoda-go-cassandra#60).

## Test plan

| Scenario | Layer |
|---|---|
| `config all` text lists every registry var, grouped by topic | help unit |
| `config all --format=json` emits valid envelope, all vars, stable field set | help unit |
| Registry names ⊇ source scan (root + plugins) — completeness | help unit (blank-imports plugins) |
| Root table defaults == `DefaultConfig()` under empty env; exempt pre-config vars (`CYODA_PROFILES`) | `app_test` |
| Plugin not implementing `DescribablePlugin` (memory) is skipped, not fatal | help unit |
| `GET /help/config/all` → 200 `application/json`, parseable envelope | `internal/api` |
| `GET /help/config/all` non-GET → 405 (existing help-route behavior) | `internal/api` |
| `cyoda help config cluster` resolves and renders the moved vars | help unit |
| `config.md` SEE ALSO includes all six subtopics incl. `config.cors` | help unit (existing SEE-ALSO test if present) |

No new error codes → no `errors/*.md` topic. No cross-backend parity scenario (help/config is host behavior, not a storage contract; plugin participation is covered by the completeness test + the SPI interface).

## Gate 4 docs

- `README.md` config reference: mention `cyoda help config all` / `--format=json` and the new `config cluster` subtopic.
- `content/config/*.md`: new `cluster.md`; `config.md` synopsis + SEE ALSO updated.
- No `cyoda-go-spi` pin bump, no chart/appVersion change, no binary release in this change → COMPATIBILITY.md untouched.

## Non-goals

- No implementation in the commercial backend (tracked in #60; it already implements `DescribablePlugin` but lacks a completeness/binding test and has already drifted by 5 vars).
- No generalization of the action framework for format negotiation — HTTP help is JSON by design, so it is unnecessary.
- No retirement of the openapi tag-resolver special-case.
- No migration of the existing hand-authored subtopic prose to generated content — the registry backs `config all`; the rich per-topic prose stays.
