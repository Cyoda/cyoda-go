package parity

import (
	"net/http"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// RunSearchMalformedPatternRejected pins the API boundary's pattern
// accept/reject set across every backend, for both pattern operators.
//
// The rejection is what makes the set backend-agnostic. An operand the kernel
// cannot compile is not an error downstream — Prepare's contract makes it a
// leaf that never matches — so the backends disagreed about what a caller saw:
// the in-tree evaluators returned a 200 and an empty page, while the
// commercial async evaluator propagated the compile error and failed every
// shard of the job. Rejecting at the boundary makes all of them answer 400
// INVALID_CONDITION to the same input, on the sync and async paths alike.
//
// The reject cases:
//
//   - `\Q` — compiles standalone, so a boundary that compiled the operand bare
//     accepted it, but the kernel evaluates the ANCHORED form and the
//     unterminated \Q swallows the appended `)\z`.
//   - `)|(` — the converse. It does not parse standalone, but anchoring is
//     concatenation, so `\A(?:)|()\z` compiles into an alternation matching
//     every stored value. Requiring BOTH keeps that family unrepresentable.
//   - `abc\` — a trailing unpaired escape, the only malformed operand the LIKE
//     glob grammar admits (cyoda-go-spi like_pattern.go).
//
// The accept cases guard against over-tightening: an operand that compiles
// both standalone and anchored, and a well-formed glob, must still search.
func RunSearchMalformedPatternRejected(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-search-pattern-validation"
	const modelVersion = 1
	if err := c.ImportModel(t, modelName, modelVersion, `{"name":"seed"}`); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := c.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	if err := c.ImportWorkflow(t, modelName, modelVersion, searchWorkflowJSON); err != nil {
		t.Fatalf("ImportWorkflow: %v", err)
	}

	aliceID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Alice"}`)
	if err != nil {
		t.Fatalf("CreateEntity Alice: %v", err)
	}

	for _, tc := range []struct {
		label string
		cond  string
	}{
		{"MATCHES_PATTERN anchor skew", `{"type":"simple","jsonPath":"$.name","operatorType":"MATCHES_PATTERN","value":"\\Q"}`},
		{"MATCHES_PATTERN unbalanced parens", `{"type":"simple","jsonPath":"$.name","operatorType":"MATCHES_PATTERN","value":")|("}`},
		{"LIKE trailing unpaired escape", `{"type":"simple","jsonPath":"$.name","operatorType":"LIKE","value":"abc\\"}`},
	} {
		status, body, err := c.SyncSearchRaw(t, modelName, modelVersion, tc.cond)
		if err != nil {
			t.Fatalf("[%s] SyncSearchRaw: %v", tc.label, err)
		}
		if status != http.StatusBadRequest {
			t.Fatalf("[%s] sync search: expected 400, got %d; body=%s", tc.label, status, body)
		}
		if !containsErrorCode(body, "INVALID_CONDITION") {
			t.Errorf("[%s] sync search: expected errorCode INVALID_CONDITION, body=%s", tc.label, body)
		}

		// The async path must reject synchronously too — a job accepted here
		// would only surface the problem as a FAILED status after polling.
		status, body, err = c.SubmitAsyncSearchRaw(t, modelName, modelVersion, tc.cond)
		if err != nil {
			t.Fatalf("[%s] SubmitAsyncSearchRaw: %v", tc.label, err)
		}
		if status != http.StatusBadRequest {
			t.Fatalf("[%s] async submit: expected 400, got %d; body=%s", tc.label, status, body)
		}
		if !containsErrorCode(body, "INVALID_CONDITION") {
			t.Errorf("[%s] async submit: expected errorCode INVALID_CONDITION, body=%s", tc.label, body)
		}
	}

	for _, tc := range []struct {
		label string
		cond  string
	}{
		{"MATCHES_PATTERN alternation", `{"type":"simple","jsonPath":"$.name","operatorType":"MATCHES_PATTERN","value":"Alice|Bob"}`},
		{"LIKE glob", `{"type":"simple","jsonPath":"$.name","operatorType":"LIKE","value":"A_ic%"}`},
	} {
		results, err := c.SyncSearch(t, modelName, modelVersion, tc.cond)
		if err != nil {
			t.Fatalf("[%s] SyncSearch: %v", tc.label, err)
		}
		assertResultIDSet(t, tc.label, results, []string{aliceID.String()})
	}
}
