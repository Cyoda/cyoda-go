# Contributing to Cyoda-Go

## Methodology

This project follows **strict Red/Green TDD** and **trunk-based development** on `main`.

## Delivery Flow

Every feature follows this flow:

```
1. Create feature branch from main
2. Execute with strict Red/Green TDD:
   a. Write failing test (RED) — run it, verify it fails
   b. Implement minimal code (GREEN) — run it, verify it passes
   c. Refactor — all tests stay green
   d. Commit
3. Run E2E tests:
   - If Docker socket is available: run directly (go test ./internal/e2e/ -v)
   - If sandboxed without Docker: human operator runs E2E tests and provides feedback
4. Code review (code-reviewer)
   -> Fix all Critical/Important findings
5. Security audit (security-auditor)
   -> Fix all Critical/Important findings
6. Create PR to main
7. Squash merge
```

## Testing Policy

Every feature must have tests at the appropriate level before it can be merged.

**Unit tests** cover individual functions and components in isolation. They run fast, need no infrastructure, and form the bulk of the test suite.

**E2E tests** (`internal/e2e/`) spin up a full Cyoda-Go instance backed by PostgreSQL (via testcontainers) and exercise the complete HTTP API path — from request to database and back. They verify that wiring, middleware, auth, persistence, and business logic work together correctly. E2E tests require Docker.

**Reconciliation tests** (`test/recon/`, build tag `cyoda_recon`) compare Cyoda-Go responses against Cyoda Cloud to verify API-level compatibility. These are optional and require Cloud credentials.

```bash
go test ./... -v                          # all unit tests (no Docker needed)
go test ./internal/e2e/ -v               # E2E tests (requires Docker)
make race                                 # race detector (CI-parity scope) — run before every PR
go test -tags cyoda_recon ./test/recon/   # reconciliation (optional, needs Cloud)
```

`make race` runs `go test -race -timeout=15m` over every package except
`internal/e2e` and is what CI invokes — running it locally before a PR
predicts CI. See `.claude/rules/race-testing.md` for the scope rationale
and when to run `-race` against `./internal/e2e/...` manually.

## Common Commands

| Command | Description |
|---------|-------------|
| `go run ./cmd/cyoda` | Run from source |
| `go build -o bin/cyoda ./cmd/cyoda` | Build executable |
| `go test ./... -v` | Run all tests |
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

**Breaking-change gate.** The `openapi-breaking-change` CI job (`.github/workflows/openapi-breaking-change.yml`) runs `oasdiff` on every PR that touches `api/openapi.yaml` and rejects client-breaking edits: narrowing a type, removing an operation or parameter, or adding a required request field. (`oasdiff` does not classify response-sealing as breaking — that is caught by the `additionalProperties: false` check above.)

## Dependencies

No external web frameworks. No DI frameworks. No ORM.

- When bumping `cyoda-go-spi` in the root `go.mod`, bump it identically in every `plugins/*/go.mod` in the same PR. The `check-spi-pin-sync` CI gate enforces this. See [`MAINTAINING.md`](MAINTAINING.md#bumping-cyoda-go-spi) for the full procedure.
- **Set `GOPRIVATE=github.com/Cyoda-platform/*`** in your shell environment. This tells the Go toolchain to bypass `proxy.golang.org` and `sum.golang.org` for cyoda-platform modules — required because (a) the commercial `cyoda-go-cassandra` plugin is private, and (b) during periods when a `cyoda-go-spi` tag is being re-cut at a new commit (see the v0.8.0 retraction note in cyoda-go-spi's CHANGELOG), the public checksum database would otherwise serve stale SHAs. CI workflows set this in their `env:` block; developers should add `export GOPRIVATE=github.com/Cyoda-platform/*` to their shell profile.

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
