package main

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"path"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	cyodaschemas "github.com/cyoda-platform/cyoda-go/docs/cyoda/schema"
)

// compilePublished compiles one schema of the published tree with the whole
// tree registered, each document under the `$id` its siblings reference it by:
// a response schema composes with BaseEvent, and a `$ref` says nothing until
// it resolves.
func compilePublished(t *testing.T, target string) *jsonschema.Schema {
	t.Helper()
	compiler := jsonschema.NewCompiler()
	err := fs.WalkDir(cyodaschemas.FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path.Ext(p) != ".json" {
			return err
		}
		raw, err := fs.ReadFile(cyodaschemas.FS, p)
		if err != nil {
			return err
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			return err
		}
		return compiler.AddResource(cyodaschemas.BaseID+p, doc)
	})
	if err != nil {
		t.Fatalf("register the schema tree: %v", err)
	}
	schema, err := compiler.Compile(cyodaschemas.BaseID + target)
	if err != nil {
		t.Fatalf("compile %s: %v", target, err)
	}
	return schema
}

// This client is the reference member a customer copies, so every answer it
// puts on the wire has to be one the published tree accepts — both the
// successful shape and the failure shape, which carry different fields.
func TestBuiltResponses_ValidateAgainstPublishedSchemas(t *testing.T) {
	d := &dispatcher{}
	const entityID = "1b4e28ba-2fa1-11d2-883f-0016d3cca427"
	retryable := true

	for name, tc := range map[string]struct {
		schema string
		build  func() (*cepb.CloudEvent, error)
	}{
		"a processor result": {"processing/EntityProcessorCalculationResponse.json", func() (*cepb.CloudEvent, error) {
			return d.buildProcessorResponse("r-1", entityID, json.RawMessage(`{"amount":42}`), true, "", nil)
		}},
		"a processor failure": {"processing/EntityProcessorCalculationResponse.json", func() (*cepb.CloudEvent, error) {
			return d.buildProcessorResponse("r-1", entityID, nil, false, "boom", &retryable)
		}},
		"a criteria verdict": {"processing/EntityCriteriaCalculationResponse.json", func() (*cepb.CloudEvent, error) {
			return d.buildCriteriaResponse("r-1", entityID, true, true, "", nil)
		}},
		"a criteria failure": {"processing/EntityCriteriaCalculationResponse.json", func() (*cepb.CloudEvent, error) {
			return d.buildCriteriaResponse("r-1", entityID, false, false, "boom", &retryable)
		}},
		"a function result": {"processing/EntityFunctionCalculationResponse.json", func() (*cepb.CloudEvent, error) {
			return d.buildFunctionResponse("r-1", entityID, "Schedule", map[string]any{"fireAt": 1}, true, "", nil)
		}},
		"a function failure": {"processing/EntityFunctionCalculationResponse.json", func() (*cepb.CloudEvent, error) {
			return d.buildFunctionResponse("r-1", entityID, "", nil, false, "boom", &retryable)
		}},
	} {
		t.Run(name, func(t *testing.T) {
			ce, err := tc.build()
			if err != nil {
				t.Fatalf("build the response: %v", err)
			}
			payload, err := extractTextData(ce)
			if err != nil {
				t.Fatalf("read the CloudEvent payload: %v", err)
			}
			inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(payload))
			if err != nil {
				t.Fatalf("the response is not valid JSON: %v", err)
			}
			if err := compilePublished(t, tc.schema).Validate(inst); err != nil {
				t.Errorf("%s refuses the answer this client sends:\n%s\n%v", tc.schema, payload, err)
			}
		})
	}
}
