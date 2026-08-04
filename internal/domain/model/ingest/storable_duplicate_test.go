package ingest

import (
	"strings"
	"testing"
)

// TestRejectUnstorablePayload_DuplicateKeys pins the rejection of duplicate
// object names.
//
// A duplicated key is read differently by different parts of the system, on the
// same bytes in the same request: schema validation, the GET response and
// unique-key computation use encoding/json, which keeps the LAST occurrence,
// while the workflow criterion evaluator, search and grouped statistics use
// gjson, which keeps the FIRST. Measured consequence: an entity created with
// {"amount":"not-a-number","amount":5} is reported by the API as amount=5 while
// the criterion `amount == 5` does not fire, leaving the entity in the wrong
// workflow state with nothing logged.
//
// RFC 8259 says object names SHOULD be unique and explicitly permits rejecting
// input where they are not. Rejecting removes the ambiguity rather than
// silently picking a winner.
func TestRejectUnstorablePayload_DuplicateKeys(t *testing.T) {
	cases := []struct {
		name     string
		payload  string
		wantPath string // "" means accepted
	}{
		{name: "no duplicates", payload: `{"a":1,"b":2}`},
		{name: "same name in sibling objects is fine", payload: `{"x":{"a":1},"y":{"a":2}}`},
		{name: "same name at different depths is fine", payload: `{"a":{"a":{"a":1}}}`},
		{name: "same name across array elements is fine", payload: `{"xs":[{"a":1},{"a":2}]}`},
		{name: "empty object", payload: `{}`},
		{name: "repeated values are not repeated keys", payload: `{"a":1,"b":1}`},

		{name: "top-level duplicate", payload: `{"a":1,"a":2}`, wantPath: "a"},
		{name: "the measured corruption case", payload: `{"amount":"not-a-number","amount":5}`, wantPath: "amount"},
		{name: "duplicate nested", payload: `{"outer":{"k":1,"k":2}}`, wantPath: "outer.k"},
		{name: "duplicate inside an array element", payload: `{"xs":[{"ok":1},{"k":1,"k":2}]}`, wantPath: "xs[1].k"},
		{name: "three occurrences", payload: `{"a":1,"a":2,"a":3}`, wantPath: "a"},
		{name: "duplicate with differing types", payload: `{"a":{"n":1},"a":[1]}`, wantPath: "a"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := RejectUnstorable([]byte(tc.payload))

			if tc.wantPath == "" {
				if err != nil {
					t.Fatalf("payload %s rejected but has no duplicate names: %v", tc.payload, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("payload %s accepted; the duplicate name %q makes it read differently by different subsystems",
					tc.payload, tc.wantPath)
			}
			if !strings.Contains(err.Error(), tc.wantPath) {
				t.Errorf("error %q does not name the duplicated key %q", err, tc.wantPath)
			}
			if !strings.Contains(err.Error(), "more than once") {
				t.Errorf("error %q does not explain that the name appears more than once", err)
			}
		})
	}
}
