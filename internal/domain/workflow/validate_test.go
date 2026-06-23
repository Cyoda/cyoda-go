package workflow

import (
	"encoding/json"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func wfWithAnnotations(wfA, stateA, trA json.RawMessage) []spi.WorkflowDefinition {
	return []spi.WorkflowDefinition{{
		Name:         "wf",
		Version:      "1.1",
		InitialState: "S",
		Active:       true,
		Annotations:  wfA,
		States: map[string]spi.StateDefinition{
			"S": {
				Annotations: stateA,
				Transitions: []spi.TransitionDefinition{
					{Name: "t", Next: "S", Annotations: trA},
				},
			},
		},
	}}
}

func TestValidateAndNormalizeAnnotations_ObjectCompacted(t *testing.T) {
	wfs := wfWithAnnotations(
		json.RawMessage(`{ "a" : 1 }`),
		json.RawMessage("{\n  \"b\": 2\n}"),
		json.RawMessage(`{"c":3}`),
	)
	if err := validateAndNormalizeAnnotations(wfs); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := string(wfs[0].Annotations); got != `{"a":1}` {
		t.Errorf("workflow annotations not compacted: %s", got)
	}
	if got := string(wfs[0].States["S"].Annotations); got != `{"b":2}` {
		t.Errorf("state annotations not compacted: %s", got)
	}
	if got := string(wfs[0].States["S"].Transitions[0].Annotations); got != `{"c":3}` {
		t.Errorf("transition annotations not compacted: %s", got)
	}
}

func TestValidateAndNormalizeAnnotations_NullAndAbsentNormaliseToNil(t *testing.T) {
	wfs := wfWithAnnotations(json.RawMessage("null"), nil, json.RawMessage("  "))
	if err := validateAndNormalizeAnnotations(wfs); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wfs[0].Annotations != nil {
		t.Errorf("workflow null annotations should normalise to nil, got %s", wfs[0].Annotations)
	}
	if wfs[0].States["S"].Annotations != nil {
		t.Errorf("state absent annotations should be nil, got %s", wfs[0].States["S"].Annotations)
	}
	if wfs[0].States["S"].Transitions[0].Annotations != nil {
		t.Errorf("transition blank annotations should be nil, got %s", wfs[0].States["S"].Transitions[0].Annotations)
	}
}

func TestValidateAndNormalizeAnnotations_NonObjectRejected(t *testing.T) {
	cases := map[string]json.RawMessage{
		"array":  json.RawMessage(`[1,2,3]`),
		"string": json.RawMessage(`"hello"`),
		"number": json.RawMessage(`5`),
		"bool":   json.RawMessage(`true`),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			err := validateAndNormalizeAnnotations(wfWithAnnotations(raw, nil, nil))
			if err == nil || !strings.Contains(err.Error(), "must be a JSON object") {
				t.Fatalf("expected object-only error, got %v", err)
			}
		})
	}
}

func TestValidateAndNormalizeAnnotations_LocationInError(t *testing.T) {
	// State-level offender names the state.
	err := validateAndNormalizeAnnotations(wfWithAnnotations(nil, json.RawMessage(`[1]`), nil))
	if err == nil || !strings.Contains(err.Error(), `state "S"`) {
		t.Fatalf("expected state location in error, got %v", err)
	}
	// Transition-level offender names the transition.
	err = validateAndNormalizeAnnotations(wfWithAnnotations(nil, nil, json.RawMessage(`"x"`)))
	if err == nil || !strings.Contains(err.Error(), `transition "t"`) {
		t.Fatalf("expected transition location in error, got %v", err)
	}
}

func TestValidateAndNormalizeAnnotations_SizeCap(t *testing.T) {
	// At cap: a compacted object of exactly maxAnnotationsBytes is accepted.
	filler := strings.Repeat("a", maxAnnotationsBytes-len(`{"k":""}`))
	atCap := json.RawMessage(`{"k":"` + filler + `"}`)
	if l := len(atCap); l != maxAnnotationsBytes {
		t.Fatalf("test setup: atCap len = %d, want %d", l, maxAnnotationsBytes)
	}
	if err := validateAndNormalizeAnnotations(wfWithAnnotations(atCap, nil, nil)); err != nil {
		t.Fatalf("at-cap annotations should be accepted: %v", err)
	}
	// Over cap by one byte: rejected.
	over := json.RawMessage(`{"k":"` + filler + `b"}`)
	err := validateAndNormalizeAnnotations(wfWithAnnotations(over, nil, nil))
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected size-cap error, got %v", err)
	}
}
