cyoda-go gates for this plan (additional to the skill):

- Carry the spec's coverage matrix forward gap-free: every endpoint × status/error code → a running-backend test; backend-agnostic behavior → a cross-backend parity scenario; cover HTTP and gRPC; concurrency tests isolated, not in parity. See `.claude/rules/test-coverage.md`.
- Every new error code → a task adding `errors/<CODE>.md` (TestErrCode_Parity enforces it). Add the Gate-4 doc tasks the change needs (help topic, README, COMPATIBILITY, CHANGELOG).
