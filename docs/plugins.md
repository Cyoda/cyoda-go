# Writing a Storage Plugin

This is the entry point for authors of out-of-tree cyoda storage plugins. The complete contract reference lives in `docs/ARCHITECTURE.md` §1 ("The `cyoda-go-spi` Module" and "Plugin Contract (summary)") and on `pkg.go.dev` for the SPI module itself; this file points you at the right destinations.

## Audience

You are writing a Go module that plugs into a custom cyoda binary as a new storage backend (alongside or instead of the stock `memory`, `sqlite`, and `postgres` plugins).

## Where the contract lives

- `docs/ARCHITECTURE.md` §1 — the SPI surface, the `Plugin` / `DescribablePlugin` / `Startable` / `StoreFactory` / `TransactionManager` interfaces, and how the binary resolves plugins via `spi.GetPlugin`.
- `pkg.go.dev/github.com/cyoda-platform/cyoda-go-spi` — the API documentation for the SPI module itself. The SPI is stdlib-only by design; depending on it does not pull in transitive dependencies.

## Reference implementations to fork

The in-tree plugins each ship with package documentation explicitly maintained as a reference for plugin authors:

- `plugins/memory/doc.go` — simplest implementation; in-process SI+FCW with `sync.RWMutex`. Read this first.
- `plugins/postgres/doc.go` — production-grade persistent storage with the `txID`-to-`pgx.Tx` bridge pattern for multi-node transaction routing, and the `DescribablePlugin` `ConfigVars()` pattern that drives `--help` output.
- `plugins/sqlite/` — single-file persistent storage with embedded SQL migrations and a JSON predicate planner; useful as a mid-complexity worked example between `memory` and `postgres`.

The Cassandra storage backend offered as a commercial product by Cyoda implements the same SPI contract; its source is not public, but no hidden interfaces are involved — every plugin uses the same surface as the in-tree examples.

## Custom binary

cyoda-go does not export a reusable `Main()` entrypoint; the supported pattern is to fork `cmd/cyoda/main.go` into your own module and add your plugin to its blank-import block, alongside the stock plugins:

```go
package main

import (
    _ "github.com/cyoda-platform/cyoda-go/plugins/memory"
    _ "github.com/cyoda-platform/cyoda-go/plugins/postgres"
    _ "github.com/cyoda-platform/cyoda-go/plugins/sqlite"
    _ "example.com/your-org/your-plugin"

    // ... rest of cmd/cyoda/main.go imports and the main() body
)
```

Selecting your plugin at runtime is then `CYODA_STORAGE_BACKEND=your-plugin-name`, where the name is whatever your `Plugin.Name()` method returns.

## SPI version pin discipline

Your plugin's `go.mod` must pin the same `cyoda-go-spi` version as the cyoda-go binary you compile into. If they diverge, your plugin will not satisfy the interfaces the binary expects.

When you bump `cyoda-go-spi` in your plugin, bump it identically in the cyoda-go binary's `go.mod` in the same release. The cyoda-go repository's CI gate `check-spi-pin-sync` enforces this rule for in-tree plugins; out-of-tree plugins follow the same convention. See `MAINTAINING.md` (section "Bumping cyoda-go-spi") for the full procedure.
