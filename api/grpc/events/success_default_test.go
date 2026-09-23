package events_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/api/grpc/events"
)

// `success` is declared `"type": "boolean"` with `"default": true`, so the
// three states of the key are not two: absent is the default, present-and-true
// or present-and-false is the flag, and present-and-null is an invalid
// instance. The server reads them that way — an omitted `success` is a
// successful answer, an explicit null is refused as an answer it cannot read —
// and these generated types are the surface a compute-member author writes
// against, so they must not disagree with it.
//
// go-jsonschema emits `!ok || v == nil` for a defaulted field, which takes the
// absent and the null branch together and calls both the default.
// scripts/generate-events.sh rewrites that; these tests are what says the
// rewrite is still there and still correct.

func TestSuccess_AbsentIsTheDefault(t *testing.T) {
	var resp events.EntityProcessorCalculationResponseJson
	const payload = `{"id":"e1","requestId":"r1","entityId":"11111111-1111-1111-1111-111111111111"}`
	if err := json.Unmarshal([]byte(payload), &resp); err != nil {
		t.Fatalf("an answer that omits success must decode: %v", err)
	}
	if !resp.Success {
		t.Error("success = false for an answer that omits the key; the schema's default is true")
	}
}

func TestSuccess_ExplicitNullIsRefused(t *testing.T) {
	var resp events.EntityProcessorCalculationResponseJson
	const payload = `{"id":"e1","requestId":"r1","entityId":"11111111-1111-1111-1111-111111111111","success":null}`
	err := json.Unmarshal([]byte(payload), &resp)
	if err == nil {
		t.Fatalf("an explicit null success decoded as success = %v; null is not a boolean and the server refuses it", resp.Success)
	}
	if !strings.Contains(err.Error(), "success") {
		t.Errorf("error %q does not name the field that was wrong", err)
	}
	if !strings.Contains(err.Error(), "EntityProcessorCalculationResponseJson") {
		t.Errorf("error %q does not name the type it came from", err)
	}
}

func TestSuccess_ExplicitValueIsKept(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want bool
	}{
		{"true", `true`, true},
		{"false", `false`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var resp events.EntityProcessorCalculationResponseJson
			payload := `{"id":"e1","requestId":"r1","entityId":"11111111-1111-1111-1111-111111111111","success":` + tc.raw + `}`
			if err := json.Unmarshal([]byte(payload), &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.Success != tc.want {
				t.Errorf("success = %v, want %v", resp.Success, tc.want)
			}
		})
	}
}

// The criteria and function answers carry the same field from the same shared
// BaseEvent, so the rewrite has to reach every generated type, not the one a
// test happened to pick.
func TestSuccess_EveryCalculationAnswerRefusesNull(t *testing.T) {
	const nullSuccess = `{"id":"e1","requestId":"r1","entityId":"11111111-1111-1111-1111-111111111111","success":null}`
	for _, tc := range []struct {
		name string
		into any
	}{
		{"processor", &events.EntityProcessorCalculationResponseJson{}},
		{"criteria", &events.EntityCriteriaCalculationResponseJson{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := json.Unmarshal([]byte(nullSuccess), tc.into); err == nil {
				t.Error("an explicit null success was accepted")
			}
		})
	}
}
