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
// ModeEnforce: same, plus fail TestOpenAPIConformanceReport when it runs.
// In a -run-filtered run, also fail the requesting test when the request
// named it (see Transport); FailOnUnreported fails the run on any mismatch
// the report test did not report.
//
// See docs/adr/0001-openapi-server-spec-conformance.md.
const Mode = ModeEnforce
