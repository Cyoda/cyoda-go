package help

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestAction_OpenAPIJSON(t *testing.T) {
	var buf bytes.Buffer
	code := emitOpenAPIJSON(&buf)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if got["openapi"] == nil || got["info"] == nil {
		t.Errorf("spec missing top-level fields: %+v", got)
	}
}

func TestAction_OpenAPIYAML(t *testing.T) {
	var buf bytes.Buffer
	code := emitOpenAPIYAML(&buf)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	var got map[string]interface{}
	if err := yaml.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("invalid YAML: %v", err)
	}
	if got["openapi"] == nil {
		t.Errorf("spec missing openapi field")
	}
}

func TestAction_GRPCProto(t *testing.T) {
	var buf bytes.Buffer
	code := emitGRPCProto(&buf)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	s := buf.String()
	if !strings.Contains(s, "cyoda-cloud-api.proto") {
		t.Errorf("output should include cyoda-cloud-api.proto marker")
	}
	if !strings.Contains(s, "cloudevents.proto") {
		t.Errorf("output should include cloudevents.proto marker")
	}
	if !strings.Contains(s, "syntax = \"proto3\"") {
		t.Errorf("output should include proto syntax declaration")
	}
}

func TestAction_GRPCJSON(t *testing.T) {
	var buf bytes.Buffer
	code := emitGRPCDescriptorJSON(&buf)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !json.Valid(buf.Bytes()) {
		t.Errorf("output is not valid JSON")
	}
}

func TestLookupAction(t *testing.T) {
	entry, ok := lookupAction("openapi", "json")
	if !ok {
		t.Error("openapi.json should be registered")
	}
	if entry.ContentType == "" {
		t.Error("openapi.json entry should carry a non-empty ContentType")
	}

	if _, ok := lookupAction("grpc", "proto"); !ok {
		t.Error("grpc.proto should be registered")
	}
	if _, ok := lookupAction("openapi", "bogus"); ok {
		t.Error("openapi.bogus should not resolve")
	}
	if _, ok := lookupAction("cli", "json"); ok {
		t.Error("cli.json should not resolve (cli has no registered actions)")
	}
}
