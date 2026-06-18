# docs/cyoda — Cyoda Cloud OpenAPI reference

This directory is a **read-only reference** mirror of the Cyoda Cloud
OpenAPI spec, used by cyoda-go for parity work. Nothing in this
directory is served at runtime — cyoda-go's authoritative, embedded
spec lives at `api/openapi.yaml` at the repo root.

## Layout

Two coexisting layers, each with a distinct purpose:

### `api/` — upstream-split mirror (parity reference)

```
api/openapi.yml             # aggregator
api/openapi-audit.yml       # audit operations
api/openapi-common.yml      # common schemas
api/openapi-entity-search.yml
api/openapi-iam.yml         # OAuth, OIDC, technical users
api/openapi-workflow.yml
```

Byte-identical mirror of `Cyoda-platform/cyoda` `develop` branch at
`client/src/main/resources/api/`. This is the form maintainers read
when checking parity ("does cyoda-go's `OidcProviderResponseDto`
match upstream?"). The split files are easier to diff against
upstream than the bundled form.

### `openapi.yml` — resolved bundle (citation anchor)

A single, all-refs-resolved bundle of the same content. Kept because
the parity test client and one handler test cite specific line
numbers as the canonical source-of-truth anchor:

```go
// Canonical: docs/cyoda/openapi.yml:1055 (getOneEntity).
```

These citations appear ~20 times across `e2e/parity/client/http.go`,
`e2e/parity/client/types.go`, and
`internal/domain/entity/handler_create_collection_chunking_test.go`.
The split files have different line numbers, so removing the bundle
would invalidate every citation.

## What lives where

| Topic | File |
|---|---|
| Cross-cloud parity notes (known divergences) | `cloud-divergences.md` |
| Range-index / dindex storage schema | `dindex-range-index-tables.md` |
| gRPC protobuf reference | `proto/` |
| JSON Schemas referenced by the spec | `schema/` |

## Sync procedure

When upstream Cloud spec changes, refresh both layers:

```bash
# 1. Refresh the split files from upstream
UPSTREAM=/path/to/Cyoda-platform/cyoda/client/src/main/resources/api
cp -r "$UPSTREAM"/* docs/cyoda/api/

# 2. Re-bundle. The bundled form is produced by the same tool the
#    upstream uses to publish its consolidated spec — currently
#    redocly-cli (`redocly bundle docs/cyoda/api/openapi.yml -o
#    docs/cyoda/openapi.yml`). If the tool changes upstream, follow
#    suit.

# 3. Verify citations still resolve. Line numbers will shift; the
#    citation comments in e2e/parity/client/*.go and
#    internal/domain/entity/handler_create_collection_chunking_test.go
#    must be updated to match the new line numbers.

# 4. Commit with a message naming the upstream commit SHA you synced from.
```

## What this directory is not

- **Not** the spec cyoda-go serves. That is `api/openapi.yaml` at repo
  root, embedded via `//go:embed` in `api/spec.go` and exposed at
  `/openapi.json` (Scalar UI at `/docs`), both under the configured
  context path (default `/api`).
- **Not** a binding contract. cyoda-go deliberately diverges from
  Cloud in specific places — see `cloud-divergences.md` for the
  canonical list.
- **Not** edited in place. All changes flow from upstream Cloud
  `develop` via the sync procedure above.
