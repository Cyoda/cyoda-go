# TDD

`superpowers:test-driven-development` is the protocol: one failing test at a
time, watched failing for the right reason, then the minimum code to pass it,
then refactor.

In this project:

- Put the test at the layer the behavior lives in. User-facing behavior needs
  the e2e, parity and gRPC tests that `test-coverage.md` lists, not only a unit
  test.
- "The rest of the suite still passes" means `make test` while iterating and
  `make test-full` before calling the work done — not a hand-rolled
  `go test ./...`.
