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

func TestValidateAndNormalizeAnnotations_SizeCapMeasuredOnCompacted(t *testing.T) {
	// Build a value whose RAW length exceeds maxAnnotationsBytes but whose
	// COMPACTED form is tiny. The whitespace between tokens is insignificant
	// JSON and must be stripped before the cap is applied.
	rawPadded := json.RawMessage("{" + strings.Repeat(" ", maxAnnotationsBytes) + `"k":1}`)
	if len(rawPadded) <= maxAnnotationsBytes {
		t.Fatalf("test setup: raw len = %d, must be > %d", len(rawPadded), maxAnnotationsBytes)
	}
	wfs := wfWithAnnotations(rawPadded, nil, nil)
	if err := validateAndNormalizeAnnotations(wfs); err != nil {
		t.Fatalf("raw-padded annotations should be accepted (cap is on compacted bytes): %v", err)
	}
	// The stored value must be the compacted form, not the whitespace-padded original.
	const want = `{"k":1}`
	if got := string(wfs[0].Annotations); got != want {
		t.Errorf("annotations not compacted: got %q, want %q", got, want)
	}
}

func wfWithNewAnnotations(procA, wfCritA, trCritA json.RawMessage) []spi.WorkflowDefinition {
	return []spi.WorkflowDefinition{{
		Name:                 "wf",
		Version:              "1.2",
		InitialState:         "S",
		Active:               true,
		CriterionAnnotations: wfCritA,
		States: map[string]spi.StateDefinition{
			"S": {
				Transitions: []spi.TransitionDefinition{{
					Name:                 "t",
					Next:                 "S",
					CriterionAnnotations: trCritA,
					Processors: []spi.ProcessorDefinition{
						{Name: "p", Type: "externalized", Annotations: procA},
					},
				}},
			},
		},
	}}
}

func TestValidateAndNormalizeAnnotations_ProcessorAndCriterionCompacted(t *testing.T) {
	wfs := wfWithNewAnnotations(
		json.RawMessage(`{ "displayName" : "Proc" }`),
		json.RawMessage("{\n  \"displayName\": \"WF guard\"\n}"),
		json.RawMessage(`{"description":"t guard"}`),
	)
	if err := validateAndNormalizeAnnotations(wfs); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := string(wfs[0].CriterionAnnotations); got != `{"displayName":"WF guard"}` {
		t.Errorf("wf criterionAnnotations not compacted: %s", got)
	}
	tr := wfs[0].States["S"].Transitions[0]
	if got := string(tr.CriterionAnnotations); got != `{"description":"t guard"}` {
		t.Errorf("transition criterionAnnotations not compacted: %s", got)
	}
	if got := string(tr.Processors[0].Annotations); got != `{"displayName":"Proc"}` {
		t.Errorf("processor annotations not compacted: %s", got)
	}
}

func TestValidateAndNormalizeAnnotations_ProcessorNonObjectRejected(t *testing.T) {
	err := validateAndNormalizeAnnotations(wfWithNewAnnotations(json.RawMessage(`[1,2]`), nil, nil))
	if err == nil || !strings.Contains(err.Error(), "must be a JSON object") {
		t.Fatalf("expected object error, got %v", err)
	}
	if !strings.Contains(err.Error(), "processor") {
		t.Errorf("error should locate the processor: %v", err)
	}
}

func TestValidateAndNormalizeAnnotations_TransitionCriterionNonObjectRejected(t *testing.T) {
	err := validateAndNormalizeAnnotations(wfWithNewAnnotations(nil, nil, json.RawMessage(`"x"`)))
	if err == nil || !strings.Contains(err.Error(), "must be a JSON object") {
		t.Fatalf("expected object error, got %v", err)
	}
	if !strings.Contains(err.Error(), "criterionAnnotations") {
		t.Errorf("error should name criterionAnnotations: %v", err)
	}
}

func TestValidateAndNormalizeAnnotations_ProcessorOversizeRejected(t *testing.T) {
	big := json.RawMessage(`{"displayName":"` + strings.Repeat("a", 64*1024) + `"}`)
	err := validateAndNormalizeAnnotations(wfWithNewAnnotations(big, nil, nil))
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected size error, got %v", err)
	}
}
