# Contributing to Cyoda-Go

## Methodology

This project follows **strict Red/Green TDD**.

## Branches

Work for the next release is staged on a release branch named
`release/vX.Y.Z`. `main` is updated only when a release is cut, so a change
merged into `main` is not part of the release being built.

- Branch from the current release branch, and open your PR against it.
- If you are not sure which release branch is current, open the PR against
  `main`. A maintainer will retarget it.

## Delivery Flow

Every feature follows this flow:

```
1. Create feature branch from the current release/vX.Y.Z branch
2. Execute with strict Red/Green TDD:
   a. Write failing test (RED) — run it, verify it fails
   b. Implement minimal code (GREEN) — run it, verify it passes
   c. Refactor — all tests stay green
   d. Commit
3. Run E2E tests:
   - If Docker socket is available: run directly (`go test -timeout 30m ./internal/e2e/...` — the package takes ~10 min, which is `go test`'s default timeout), or `make test-full` for the whole suite. In a `-run`-filtered run, an OpenAPI response mismatch fails the test that made the request, and a mismatch that no test reported (a request that named no test, or a response that arrived after its test returned) fails the run: through `TestOpenAPIConformanceReport` when the filter includes it, otherwise when `TestMain` ends. Include it in the filter (`-run 'TestFoo|TestOpenAPIConformanceReport'`) to have the report file written
   - If sandboxed without Docker: human operator runs E2E tests and provides feedback
4. Code review (code-reviewer)
   -> Fix all Critical/Important findings
5. Security audit (security-auditor)
   -> Fix all Critical/Important findings
6. Create PR to the release branch
7. Squash merge
```

## Testing Policy

Every feature must have tests at the appropriate level before it can be merged.

**Unit tests** cover individual functions and components in isolation. They run fast, need no infrastructure, and form the bulk of the test suite.

**E2E tests** (`internal/e2e/`) spin up a full Cyoda-Go instance backed by PostgreSQL (via testcontainers) and exercise the complete HTTP API path — from request to database and back. They verify that wiring, middleware, auth, persistence, and business logic work together correctly. E2E tests require Docker.

**Reconciliation tests** (`test/recon/`, build tag `cyoda_recon`) compare Cyoda-Go responses against Cyoda Cloud to verify API-level compatibility. These are optional and require Cloud credentials.

```bash
make test        # iteration tier: unit + cross-backend parity (~115s cold, ~85s warm)
make test-full   # everything, root + all three plugin submodules (~15 min)
make race        # race detector (CI-parity scope) — CI runs it on every PR push
go test -tags cyoda_recon ./test/recon/   # reconciliation (optional, needs Cloud)
```

Both tiers require Docker and there is no Docker-free alternative: if Docker
cannot serve the suites, `make preflight` fails with what to fix. The
`plugins/postgres` suite is database-backed throughout and provisions its own
PostgreSQL container when `CYODA_TEST_DB_URL` is unset — CI sets that variable
and its service container is used instead. Prefer these
targets over a hand-rolled `go test ./...`, which reports `ok` for a suite that
never ran — a `TestMain` calling `os.Exit(0)` prints `ok  pkg  2.80s` having
executed nothing. The targets pipe through `scripts/testreport`, which fails
when a required suite ran no tests and prints failures verbatim rather than
burying them in `-v` output. The targets leave Go's test cache on — that is
what keeps a warm `make test` off its ~115s cold time — except for the
cross-backend parity suites, which each tier runs in a second, `-count=1`
invocation because their
fixtures build the server binary in a subprocess the test cache cannot see, so
a cached pass could be replayed over a server built from the old code.

`make race` runs `go test -race -timeout=15m` over every package except
`internal/e2e` and is what CI invokes — running it locally before a PR
predicts CI. See `.claude/rules/race-testing.md` for the scope rationale
and when to run `-race` against `./internal/e2e/...` manually.

## Common Commands

| Command | Description |
|---------|-------------|
| `go run ./cmd/cyoda` | Run from source |
| `go build -o bin/cyoda ./cmd/cyoda` | Build executable |
| `make test` | Iteration tier: unit + parity (~115s cold, ~85s warm — parity always re-runs) |
| `make test-full` | Everything, root + plugin submodules (~15 min) |
| `make race` | Run tests with race detector (CI-parity scope; excludes `internal/e2e`) |
| `go test -coverprofile=coverage.out ./...` | Test coverage |
| `./scripts/dev/run-docker-dev.sh` | Start with Docker + PostgreSQL |

## CI checks

Every PR must pass the following gates before it can be merged:

| Job | Workflow | Purpose |
|-----|----------|---------|
| `test` | `ci.yml` | Unit + integration tests, race detector, `go build`. |
| `per-module-hygiene` | `ci.yml` | Each plugin module builds and vets independently with `GOWORK=off` (protects downstream consumers). |
| `security` | `ci.yml` | `govulncheck` against the root module and each plugin submodule; `actions/dependency-review-action` on PR diffs. |
| `Analyze Go` | `codeql.yml` | CodeQL static analysis (`security-and-quality` pack). Findings surface in the Security tab and as PR annotations. Also runs on a weekly cron. |
| `shellcheck` | `ci.yml` | Lint for shell scripts. |
| `oasdiff` | `openapi-breaking-change.yml` | Breaking-change gate, on a PR that touches `api/openapi.yaml` or the gate (see below). |
| `client-probe` | `openapi-client-generation.yml` | Generates clients from `api/openapi.yaml` and round-trips the wire samples (see below). |

The `security` job is blocking: any vulnerability finding in the call graph, or any new dependency at `moderate` severity or above, fails the PR. To reproduce locally:

```bash
go install golang.org/x/vuln/cmd/govulncheck@v1.1.4
govulncheck ./...
(cd plugins/memory && govulncheck ./...)
(cd plugins/postgres && govulncheck ./...)
(cd plugins/sqlite && govulncheck ./...)
```

Release builds additionally run a Trivy scan against the published GHCR image (`.github/workflows/release.yml`). Results are surfaced in the release run's job summary. This is advisory — the tag is already published by the time Trivy runs; pre-merge gating is the `security` job's responsibility.

## OpenAPI operation status & evolution

Every operation in `api/openapi.yaml` must be in one of two states:

- **Live** — exercised by at least one test in `internal/e2e/`. No marker required.
- **Not-live** — carries `x-cyoda-status: planned` or `x-cyoda-status: unimplemented`.

The E2E conformance gate enforces both sides of this rule: an unmarked operation with no E2E coverage fails CI; a marked operation that returns 2xx fails CI (the marker must be removed once the operation is implemented and exercised).

**Schema authoring rule (ADR 0003).** Schemas are *typed-but-open*: enumerate every property the service emits, but never set `additionalProperties: false` on an evolvable schema. Sealing an object makes every additive field addition a breaking change. This is enforced automatically by `TestSpecHasNoSealedSchemas` (`internal/oasdiffcheck`), which fails if `api/openapi.yaml` contains `additionalProperties: false`.

**Breaking-change gate.** The `openapi-breaking-change` CI job (`.github/workflows/openapi-breaking-change.yml`) runs `oasdiff` on every PR that touches `api/openapi.yaml` and rejects client-breaking edits: narrowing a type, removing an operation or parameter, or adding a required request field. (`oasdiff` does not classify response-sealing as breaking — that is caught by the `additionalProperties: false` check above.) `.github/oasdiff-severity-levels.txt` raises to errors the changes the contract forbids and `oasdiff` would otherwise pass: adding a value to a response `enum`, removing an `enum` value, deleting a discriminator mapping key, renaming a discriminator property. A deliberate, reviewed breaking change is listed, with its reason, in `.github/oasdiff-err-ignore.txt`.

**Polymorphism and growth (ADR 0005).** These rules are checked by the spec tests in `api/` (`go test ./api/`), and the reasons for each are in `docs/adr/0005-openapi-client-generation.md`:

- A polymorphic type is a *base*: a schema with a discriminator and a `mapping` naming every member. Each member includes the base with `allOf`; every use is a `$ref` to the base; no `oneOf`/`anyOf` has a discriminator; a schema includes at most one base.
- No union of scalar types: a value of several scalar types is a schema with no `type`, whose other keywords state the rule. No inline `enum` on a property of a base (make it a named schema). No member declares its base's discriminator property.
- The contract grows by addition only. A value set that may grow is a plain string, never an `enum`: a client generated from an earlier spec cannot read an `enum` value it does not list, so adding one is a breaking change. Nothing published is renamed or removed — no mapping key, `enum` value or event type; a value cyoda-go stops sending stays, and its description says it is not sent. The severity file above is why the gate refuses a removal, a renamed discriminator, or a value added to a response `enum`.
- A use of a base in a response is never in its parent's `required`.

**Client-generation check.** The `client-probe` CI job (`.github/workflows/openapi-client-generation.yml`) runs on every PR. The Maven project `api/clientprobe/` generates `kotlin-spring` classes, with Cloud's `configOptions` only (not its type, import or schema mappings), and a `java` client from `api/openapi.yaml` with openapi-generator 7.18.0, compiles both, and reads and writes back every sample in `api/clientprobe/samples/`. Run it locally with `make client-probe`, which needs Java 21 and Maven; it is not part of `make test`.

A sample is one JSON file in `api/clientprobe/samples/`:

```json
{
  "schema": "StateMachineEventDto",
  "class": "StateMachineFinishedDto",
  "value": { "eventType": "STATE_MACHINE_FINISH", "state": "DONE", "success": true,
             "stopReason": { "type": "COMPLETED_NO_AUTO_TRANSITIONS", "description": "No automated transition from state \"DONE\"" } }
}
```

- `schema` — the component schema the value is read as; `class` — the concrete class the client reads it into: the member the base's discriminator selects, or the schema itself.
- `body` — in place of `schema`, for a request body that is not a component schema: `"<METHOD> <path>"`, with no `class`. The clients take such a body as an untyped value, so only the spec test checks it.
- `valid` — default `true`. A valid sample must validate against its schema and round-trip through both clients; an invalid one (`false`) must fail validation and is skipped by the clients.
- `future` — default `false`. `true` marks an audit page a later spec could send (an unknown audit kind, an unknown event type, an added field); `added` lists, as JSON pointers into `value`, exactly the members the spec does not declare.
- A valid sample states every property that has a schema `default`: the generated classes write an absent defaulted property back as its default.

`go test ./api/` validates every sample and checks coverage: every mapping entry of every base has a sample.

## Dependencies

No external web frameworks. No DI frameworks. No ORM.

- When bumping `cyoda-go-spi` in the root `go.mod`, bump it identically in every `plugins/*/go.mod` in the same PR. The `check-spi-pin-sync` CI gate enforces this. See [`MAINTAINING.md`](MAINTAINING.md#bumping-cyoda-go-spi) for the full procedure.
- **Set `GOPRIVATE=github.com/Cyoda-platform/*`** in your shell environment. This tells the Go toolchain to bypass `proxy.golang.org` and `sum.golang.org` for cyoda-platform modules — required to fetch the commercial `cyoda-go-cassandra` plugin, which is private. Add `export GOPRIVATE=github.com/Cyoda-platform/*` to your shell profile. This repo's CI does not set it: every module it resolves is public.

| Dependency | Purpose |
|------------|---------|
| Go standard library `net/http` | HTTP server and routing (Go 1.22+ pattern matching) |
| `github.com/google/uuid` | UUID generation |
| `github.com/jackc/pgx/v5` | PostgreSQL driver (only loaded when postgres backend is active) |
| `google.golang.org/grpc` | gRPC server for externalized processor/criteria dispatch |

## Provisioning Artifacts

Canonical provisioning artifacts (Helm chart, Docker Compose files) live under `deploy/`. Developer convenience scripts (local run, Docker dev setup) live under `scripts/dev/`. For the design rationale and structure of the shared provisioning layer, see [`docs/superpowers/specs/2026-04-16-provisioning-shared-design.md`](docs/superpowers/specs/2026-04-16-provisioning-shared-design.md).

## Developer Setup

1. [Claude Code CLI](https://docs.anthropic.com/en/docs/claude-code) with [superpowers](https://github.com/obra/superpowers)
2. [agent-safehouse](https://github.com/eugene1g/safehouse) — `brew install eugene1g/safehouse/agent-safehouse`
3. [Zed editor](https://zed.dev) — `brew install --cask zed`

## Help topic tree

The `cyoda help` topic tree is a stable interface. Topic paths (e.g. `config.database`, `errors.MODEL_NOT_FOUND`) are committed for the duration of a major version — tooling, documentation sites, and AI agents rely on them.

### Additions

New topics may be added freely at any point under existing parent paths. Adding a top-level topic is also permitted but update the hardcoded list in `cmd/cyoda/help/help_test.go` (`topLevelTopicsV061`) at the same time.

### Plugin-contributed topics (overlays)

External storage plugins can add their own help topics without editing the OSS content tree. A plugin embeds a `content/` directory (same layout and front-matter contract as the OSS base) and registers it from `init()`:

```go
//go:embed content
var helpContent embed.FS

func init() { help.RegisterOverlay(helpContent) }
```

The runtime tree (`help.BuildTree()`, used by both `cyoda help` and the HTTP help routes) merges the OSS base with every registered overlay: topics at fresh paths are added; a topic whose path collides with an OSS topic overrides it ("later wins", `see_also` unions unless `see_also_replace: true`). Overlay content is held to the same front-matter validation as the base — a malformed overlay fails binary startup. Prefer a dedicated path namespace for plugin topics to avoid unintended overrides. `help.DefaultTree` remains the OSS-only base (used by tests and OSS-only consumers).

### Renames / removals

A rename or removal requires:

1. A deprecation window of at least one minor release — the old path continues to work (renders the new topic's content with a deprecation notice).
2. An entry in the release notes calling out the change.
3. An update to CONTRIBUTING.md that documents the new path.

### Stability markers

Per-topic `stability:` value governs what consumers should expect:

- `stable` — content semantics locked. Wording may evolve; structure does not.
- `evolving` — may be reorganised between minors. No path changes without deprecation.
- `experimental` — may be reorganised or removed without deprecation. Used for stubs and early drafts.

### Primary audience — AI agents

`cyoda help` is optimised first for **AI agents** (Claude Skills, code-generation tools, embedded assistants) that discover the cyoda-go contract and produce working application code from it. Humans reading in a terminal are a second-order audience.

That primacy sets the content bar:

- **Enumerate, don't summarise.** List every env var, every error code, every endpoint, every CLI flag, every schema field. An agent benefits from the complete set; it does not skim. Phrases like *"and others"* / *"e.g."* / *"among others"* are forbidden — finish the list.
- **Exact signatures.** Request/response schemas go in fenced JSON/YAML blocks that an agent can copy verbatim. CLI flags and env vars include name, type, default, and validation rule. Proto method signatures are shown in full.
- **Concrete invocations.** Examples are full, runnable commands — complete `curl`, `docker run`, `grpcurl` calls with all required flags — not fragments. A fragment forces an agent to hallucinate the rest.
- **No hedging.** Never say *"usually"*, *"typically"*, *"in most cases"*. State the exact behaviour under exact conditions. If behaviour depends on a condition, spell the condition out.
- **Canonical IDs for cross-reference.** Use dotted paths (`errors.MODEL_NOT_FOUND`, `config.database`) — these resolve deterministically through `tree.Find` and the JSON payload's `see_also` field.
- **Predictable section headings.** `NAME`, `SYNOPSIS`, `DESCRIPTION`, `OPTIONS`, `FIELDS`, `REQUEST`, `RESPONSE`, `ERRORS`, `EXAMPLES`, `SEE ALSO` — agents parse by H2. Adding a section is fine; renaming an established one is not.

Line count is not a constraint. Exhaustive beats brief.

### Content voice — definitive, not tutorial

`cyoda help` is the **definitive reference** for flags, env vars, endpoints, schemas, error codes, and runtime options. Tutorials, narrative walkthroughs, and "getting started" storytelling live in the cyoda-docs site — which **references `cyoda help`** for contract details.

That division makes two rules binding on help authors:

1. **No outbound cross-references.** Help topic bodies must not point at `cyoda-docs`, external URLs, or "see the documentation site" footers. Internal `see_also` entries referencing other help topics are fine and expected. The cyoda-docs site is a research input when authoring — it is not a citation target.

2. **Contract, not prose.** Help states *what* a thing is, its *inputs*, *outputs*, and *errors*. It does not narrate ("first you'll want to…", "imagine you have a case where…"). Concrete examples are welcome when they *are* the contract — a minimal `curl` invocation, a sample env var block, a proto method signature. Multi-step walkthroughs belong in cyoda-docs.

If a topic seems hard to write without tutorial prose, that usually means the contract underneath is fuzzy — sharpen the contract before writing the help.
