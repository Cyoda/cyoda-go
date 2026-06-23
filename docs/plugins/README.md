# Storage plugins

cyoda-go's storage layer is a plugin system defined by the stable
[`cyoda-go-spi`](https://github.com/cyoda/cyoda-go-spi) module
(stdlib-only Go interfaces and value types). A running binary has
exactly one active plugin, selected at startup via `CYODA_STORAGE_BACKEND`.

## Open-source plugins shipped with the stock binary

- **[`memory`](IN_MEMORY.md)** (default) — ephemeral, microsecond-latency
  SI+FCW for tests and high-throughput digital-twin workloads.
- **[`sqlite`](SQLITE.md)** — persistent, zero-ops single-node storage for
  desktop, edge, and containerised single-node production.
- **[`postgres`](POSTGRES.md)** — durable multi-node storage. PostgreSQL
  `REPEATABLE READ` plus application-layer first-committer-wins delivers
  the same SI+FCW contract as the other plugins; works against any
  managed PostgreSQL 14+ platform.

## Commercial plugin

A **`cassandra`** plugin is available as a commercial offering from Cyoda
for deployments that need horizontal write scalability beyond a
single-primary PostgreSQL. See [cyoda.com](https://www.cyoda.com) and use
its contact page.

## Authoring your own plugin

Third-party plugins (Redis, ScyllaDB, FoundationDB, etc.) can be authored
against `cyoda-go-spi` and compiled into a custom binary via a blank
import in a local `main.go`. The stock plugins serve as reference
implementations:

- [`plugins/memory/doc.go`](../../plugins/memory/doc.go) — simplest reference
  implementation; start here.
- [`plugins/postgres/doc.go`](../../plugins/postgres/doc.go) — fully-featured
  reference implementation with migrations, connection pooling, and
  multi-node wiring.

See [`../ARCHITECTURE.md`](../ARCHITECTURE.md) §2 for the plugin contract.
