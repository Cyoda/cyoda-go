package openapivalidator

// ModeKind is an int because comparing constants by string would tempt
// runtime configuration via env var, which we explicitly rejected (see ADR).
type ModeKind int

const (
	ModeRecord ModeKind = iota
	ModeEnforce
)

// Mode controls whether validation failures fail the suite.
//
// ModeRecord: collect mismatches, write the report file, do NOT fail.
// ModeEnforce: same, plus fail TestOpenAPIConformanceReport (full suite)
// or t.Errorf the requesting test (-run-filtered single-test workflow).
//
// Default is ModeRecord during the conformance work (commits 1-10 of #21).
// Flipped to ModeEnforce in Task 11.2 — the final commit of #21. See
// docs/adr/0001-openapi-server-spec-conformance.md.
const Mode = ModeEnforce
