# Test coverage — API/gRPC features

Full coverage = happy path AND every documented status/error code, on a running backend.

- HTTP: one test per endpoint × status/error code in `internal/e2e` (real Postgres).
- gRPC: one test per error class in `internal/grpc` (assert the envelope: `Success`, `Error.Code`).
- Backend-agnostic behavior: a cross-backend parity scenario in `e2e/parity` (memory/sqlite/postgres + commercial); register it in `registry.go`.
- HTTP and gRPC are separate entry points — cover both.
- Concurrency/race: isolated single-backend e2e, never the shared parity suite. Assert consistency (one winner, loser 409/412, no torn write), not a precise interleave.
- The spec's error table is the checklist; a missing cell blocks merge unless waived with a one-line reason.
