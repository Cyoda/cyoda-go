cyoda-go gates for this design (additional to the skill):

- After the design is agreed, before writing the spec, dispatch a fresh-context subagent to review it independently; don't bias it; iterate on findings.
- For any API/gRPC change the spec must include an error/status-code table per endpoint and a coverage matrix (scenario × layer: unit / running-backend e2e / cross-backend parity / gRPC). See `.claude/rules/test-coverage.md`.
