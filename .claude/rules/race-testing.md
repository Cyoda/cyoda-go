---
paths:
  - "**/*.go"
---
# Race Detector Discipline

`go test -race` is a sanity check at the end of a deliverable, not a continuous-iteration
gate. Run it once before creating a PR — not at every intermediate step.

Use `make race` (defined in `Makefile`) so local and CI run exactly the same scope.

## Rules

- **During iteration** (subagent dispatch, single-task verification, between commits):
  use `go test -short ./...` or scoped tests like `go test ./internal/foo/...`. No `-race`.
- **Before PR creation** (and only then): run `make race` once as a sanity check.
  CI invokes the same target, so a local pass strongly predicts a CI pass.
- **If a race-related bug is suspected**: run `-race` on the specific package
  while debugging, then drop it once the fix lands.
- **If you touch `internal/e2e`** and want race coverage on the change:
  `go test -race -timeout=20m ./internal/e2e/...` locally. Don't add E2E to
  the `make race` scope — see "Scope" below.

## Scope

`make race` runs `go test -race -timeout=15m` over every package **except**
`internal/e2e` (the full HTTP-stack E2E suite). Excluded because race
instrumentation pushes that single package past Go's default 10m per-package
timeout, and the production code paths it covers (engine cascade, cluster
dispatch, store mutexes, OIDC/model caches) are also exercised by the
workflow / cluster / plugin unit tests — which retain race coverage. The
unique-coverage loss is the narrow class of races reachable only through HTTP
ordering between concurrent inbound requests, and no current E2E test fans
out parallel clients to trigger them.

`internal/e2e/openapivalidator` (a middleware library) is kept in scope — only
the top-level `internal/e2e` test driver is excluded.

If you write a `TestConcurrent*`-style E2E test that genuinely needs race
coverage, prefer reshaping it into a domain-package unit test that calls the
production code directly (faster, narrower, gets race coverage from
`make race` automatically). Extending the carve-out should be the last resort.

## Why

`-race` instrumentation makes tests 2-10x slower per the documented overhead.
Running it at every step burns wall-clock time without finding new bugs —
races that exist after a small change were almost certainly there before. The
end-of-deliverable run catches what matters.
