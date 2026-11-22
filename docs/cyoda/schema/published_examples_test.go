package cyodaschemas

import (
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// A compute-member author is told to build an answer from the published
// examples and to check it against this tree, so an example that the tree
// refuses is a defect in the pair. While the composition was ignored the two
// could drift without anything noticing, and they had: the examples omitted
// `id` and sent an explicit `"error": null`, which BaseEvent declares an
// object.
//
// The instances below are the shapes those documents publish, with the
// angle-bracket placeholders filled in and nothing else changed. They are
// transcribed by hand — this pins the schema side of the pair, so a tightening
// that would invalidate a published example fails here.
func TestPublishedExamples_Validate(t *testing.T) {
	const eid = `"entityId":"1b4e28ba-2fa1-11d2-883f-0016d3cca427"`
	for name, tc := range map[string]struct{ schema, instance string }{
		// cmd/cyoda/help/content/grpc.md — criteria response
		"a criteria verdict": {
			"processing/EntityCriteriaCalculationResponse.json",
			`{"id":"e-1","requestId":"r-1",` + eid + `,"success":true,"matches":true,"warnings":[]}`},
		// docs/cloud-parity/criterion-stoppage-reason.md — with a reason
		"a criteria refusal with a reason": {
			"processing/EntityCriteriaCalculationResponse.json",
			`{"id":"e-1","requestId":"r-1",` + eid + `,"success":true,"matches":false,"reason":"credit score 540 below threshold 600"}`},
		// cmd/cyoda/help/content/grpc.md — processor response
		"a processor result": {
			"processing/EntityProcessorCalculationResponse.json",
			`{"id":"e-1","requestId":"r-1",` + eid + `,"success":true,"payload":{"type":"JSON","data":{"amount":42}},"warnings":[]}`},
		// cmd/cyoda/help/content/grpc.md — function response
		"a function result": {
			"processing/EntityFunctionCalculationResponse.json",
			`{"id":"e-1","requestId":"r-1",` + eid + `,"success":true,"result":{"fireAt":1},"resultKind":"Schedule","warnings":[]}`},
		// cmd/cyoda/help/content/grpc.md — EventAckResponse text_data
		"an ack": {
			"processing/EventAckResponse.json",
			`{"id":"a-1","sourceEventId":"s-1","success":true,"warnings":[]}`},
	} {
		t.Run(name, func(t *testing.T) {
			schema := compileSchema(t, tc.schema)
			inst, err := jsonschema.UnmarshalJSON(strings.NewReader(tc.instance))
			if err != nil {
				t.Fatalf("the published example is not valid JSON: %v", err)
			}
			if err := schema.Validate(inst); err != nil {
				t.Errorf("%s refuses the example this tree is published with:\n%s\n%v",
					tc.schema, tc.instance, err)
			}
		})
	}
}

// The prose around those examples tells a compute-member author that every
// calculation response carries three identifying fields and that the rest is
// optional. That is a claim about the smallest answer the tree accepts, so it
// is checked the same way: the three fields alone and nothing else.
//
// The criteria response is deliberately absent. It owes one thing more — the
// verdict the criterion was asked for — and TestCriteriaResponse_Matches…
// covers that clause; listing it here would assert the opposite.
func TestSmallestDocumentedAnswer_Validates(t *testing.T) {
	const smallest = `{"id":"e-1","requestId":"r-1","entityId":"1b4e28ba-2fa1-11d2-883f-0016d3cca427"}`
	for _, schemaPath := range []string{
		"processing/EntityProcessorCalculationResponse.json",
		"processing/EntityFunctionCalculationResponse.json",
	} {
		t.Run(schemaPath, func(t *testing.T) {
			schema := compileSchema(t, schemaPath)
			inst, err := jsonschema.UnmarshalJSON(strings.NewReader(smallest))
			if err != nil {
				t.Fatalf("the instance is not valid JSON: %v", err)
			}
			if err := schema.Validate(inst); err != nil {
				t.Errorf("%s refuses the smallest answer the help topic documents:\n%s\n%v",
					schemaPath, smallest, err)
			}
		})
	}
}
