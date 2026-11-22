package events_test

import (
	"encoding/json"
	"testing"

	"github.com/cyoda/cyoda-go/api/grpc/events"
)

// Regression tests for precision loss above 2^53.
//
// Generated UnmarshalJSON methods on CloudEvent types use json.Unmarshal
// internally. Without UseNumber, any numeric literal in a freeform field
// (map[string]interface{} / interface{}) is decoded to float64, which
// loses precision above 2^53. These tests pin that CloudEvent types with
// freeform payloads (Condition on EntitySearchRequestJson, etc.) preserve
// large integers as json.Number.

const bigInt = "9007199254740993" // 2^53 + 1 — not representable in float64

func TestEntitySearchRequest_ConditionPreservesLargeInt(t *testing.T) {
	payload := []byte(`{
		"id":"00000000-0000-0000-0000-000000000000",
		"model":{"name":"widget","version":1},
		"condition":{"type":"simple","jsonPath":"$.size","operatorType":"EQUALS","value":` + bigInt + `}
	}`)

	var req events.EntitySearchRequestJson
	if err := json.Unmarshal(payload, &req); err != nil {
		t.Fatalf("unmarshal EntitySearchRequestJson: %v", err)
	}

	v, ok := req.Condition["value"]
	if !ok {
		t.Fatalf("Condition missing value field: %+v", req.Condition)
	}

	// With UseNumber: decoded as json.Number whose String() roundtrips.
	// Without UseNumber: decoded as float64, which loses 1 ULP → 9007199254740992.
	switch typed := v.(type) {
	case json.Number:
		if typed.String() != bigInt {
			t.Errorf("json.Number.String() = %q, want %q (precision loss)", typed.String(), bigInt)
		}
	case float64:
		t.Errorf("value decoded as float64 (%v); UseNumber not active — generated UnmarshalJSON bypasses precision handling", typed)
	default:
		t.Errorf("value has unexpected type %T: %v", v, v)
	}
}

func TestEntitySnapshotSearchRequest_ConditionPreservesLargeInt(t *testing.T) {
	payload := []byte(`{
		"id":"00000000-0000-0000-0000-000000000000",
		"model":{"name":"widget","version":1},
		"condition":{"type":"simple","jsonPath":"$.size","operatorType":"EQUALS","value":` + bigInt + `}
	}`)

	var req events.EntitySnapshotSearchRequestJson
	if err := json.Unmarshal(payload, &req); err != nil {
		t.Fatalf("unmarshal EntitySnapshotSearchRequestJson: %v", err)
	}

	v, ok := req.Condition["value"]
	if !ok {
		t.Fatalf("Condition missing value field: %+v", req.Condition)
	}
	if _, ok := v.(json.Number); !ok {
		t.Errorf("value decoded as %T (%v); want json.Number", v, v)
	}
}
