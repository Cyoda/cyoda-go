# Cyoda-Go

Lightweight, multi-node Go digital twin of the Cyoda platform.
Goal: API and behavioral fidelity with Cyoda Cloud.

## Development Gates

These are STOP-and-verify checkpoints. Do not proceed past a gate without completing it.

### Gate 1: TDD is mandatory
Do not write implementation code without a failing test driving it.
Use `superpowers:test-driven-development` skill for all feature and bugfix work.
See `.claude/rules/tdd.md` for the full protocol.

### Gate 2: E2E test coverage
When adding or changing user-facing behavior (API responses, workflow semantics, error codes),
add or update E2E tests in `internal/e2e/` to cover the change through the full HTTP stack.
E2E tests are self-contained: `TestMain` starts a PostgreSQL container via testcontainers-go
and an in-process `httptest.Server` with JWT auth — no external instance needed.
Just run `go test ./internal/e2e/... -v` (requires Docker running).

### Gate 3: Security by default
Never log credentials, tokens, secrets, or signing keys at any level.
Every data path must be verified for tenant isolation — no cross-tenant leakage.
Validate input at system boundaries. Sanitize output — no stack traces or internals in responses.

### Gate 4: Documentation hygiene
When changing env vars, update the relevant `cmd/cyoda/help/content/config/*.md` help topic, `README.md`, and `DefaultConfig()` together.
When changing public interfaces or developer workflow, check `README.md` and `CONTRIBUTING.md`.
When bumping the `cyoda-go-spi` pin in `go.mod`, when tagging a `cyoda-go` binary release, when changing the chart `version:` / `appVersion:`, or when out-of-tree-plugin pin guidance changes, update `COMPATIBILITY.md` in the same change.

### Gate 5: Verify before claiming done
Use `superpowers:verification-before-completion` skill before claiming work is complete.
Run `go test ./... -v` and confirm green (this includes E2E tests). Run `go vet ./...` for static analysis.
E2E tests spin up their own PostgreSQL + HTTP server automatically — just run them.
Do not claim work is done if any test — unit, integration, or E2E — is failing.

Race detector (`go test -race ./...`) is a one-shot sanity check before PR creation,
not a per-step gate — see `.claude/rules/race-testing.md`.

### Gate 6: Continuous improvement — resolve, don't defer
We strive for continual improvement of code quality and progressively reduce
technical debt. When you spot an issue — dead code, an outdated comment, a
weakened invariant, a missed test, a stale TODO — **resolve it now via
red/green TDD within reason**, do not delegate it to "later". The default is
to fix; deferring requires a reason (out of scope, architectural decision
needs human input, would balloon the change beyond reviewability).

"Within reason" means:
- Bounded in scope: the fix is comprehensible alongside the work that surfaced it.
- Reversible: small commits, each with its own failing test.
- Doesn't bypass other gates: still TDD, still reviewed, still verified.

If the fix is structural, requires a design decision, or would significantly
expand the change, **stop and surface the choice to the human** rather than
silently leaving it broken. Recording a `TODO(...)` is the last resort, not
the first response.

## External Storage Plugins

The Cassandra storage plugin lives in a separate repository:
- Repo: https://github.com/Cyoda-platform/cyoda-go-cassandra
- Local checkout: `../cyoda-go-cassandra`

When doing cross-plugin work (changing the SPI, adding parity tests, modifying
shared contracts), verify the change does not break the Cassandra plugin. The
parity test registry (`e2e/parity/registry.go`) is picked up by all backends
including Cassandra — new parity tests will surface there on their next
dependency update.

## Go Conventions

- Go 1.26+. Use `log/slog` exclusively — never `log.Printf` or `fmt.Printf`.
- Manual dependency injection via constructors. No DI frameworks.
- Wrap errors with context: `fmt.Errorf("failed to X: %w", err)`.
- Use `uuid.UUID` type, not `string`, for UUIDs.
- Config via env vars with `CYODA_` prefix and sensible defaults.
- 4xx errors: full domain detail with error code. 5xx: generic message + ticket UUID.
- Deferred work: `// TODO(plan-reference): description`. Run `make todos` to list all.

## Workflow

### New feature
brainstorming → writing-plans → subagent-driven-development → verification-before-completion → requesting-code-review → security-review → PR/merge

### Bugfix
test-driven-development → verification-before-completion → requesting-code-review → security-review → PR/merge

### Receiving review feedback
receiving-code-review

All workflow skills are in the `superpowers:` namespace.
Security review uses `antigravity-bundle-security-developer:cc-skill-security-review`.

Do not skip steps. Brainstorming prevents building the wrong thing.
TDD prevents shipping untested code. Verification prevents false "done" claims.
Review and security audit prevent defects reaching main.

## Common Commands

Plugin submodules (`plugins/memory`, `plugins/sqlite`, `plugins/postgres`)
each have their own `go.mod`; Go's `./...` recursion does **not** cross
module boundaries, so root-module commands miss them. Use the `-all`
aggregator targets below when you want coverage across the whole repo.

- Test (root module): `go test ./... -v` — root incl. internal/e2e (requires Docker)
- Test (root module, unit only): `go test -short ./... -v`
- Test (root + every plugin submodule): `make test-all` — covers root + `plugins/memory|sqlite|postgres`; Docker required for postgres testcontainers
- Test (root + plugins, short): `make test-short-all`
- Test (E2E only): `go test ./internal/e2e/... -v`
- Coverage (root module): `go test -coverprofile=coverage.out ./...` — run inside each `plugins/*` for per-plugin coverage
- Race detector (root module): `go test -race ./...`
- Build: `go build -o bin/cyoda ./cmd/cyoda`
- Tidy: `go mod tidy`
- Vet (root module): `go vet ./...` — the `per-module-hygiene` CI job vets each plugin separately

## Cloud Deployment (Terraform)
Terraform configurations for AWS, GCP, and Azure live in `deploy/terraform/`.