package help

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
)

func TestEmitWorkflowSchemaVersions_StructuredJSON(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	rc := emitWorkflowSchemaVersions(&buf)
	if rc != 0 {
		t.Fatalf("emitWorkflowSchemaVersions = %d; want 0", rc)
	}
	var got struct {
		Current   string `json:"current"`
		Supported []struct {
			Major    int `json:"major"`
			MinMinor int `json:"minMinor"`
			MaxMinor int `json:"maxMinor"`
		} `json:"supported"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal action output: %v; raw: %s", err, buf.String())
	}
	if got.Current != workflow.CurrentSchemaVersion {
		t.Fatalf("current = %q; want %q", got.Current, workflow.CurrentSchemaVersion)
	}
	if len(workflow.SupportedSchemaRanges) == 0 {
		t.Fatalf("workflow.SupportedSchemaRanges is empty — would silently match an empty payload")
	}
	if len(got.Supported) != len(workflow.SupportedSchemaRanges) {
		t.Fatalf("supported len = %d; want %d", len(got.Supported), len(workflow.SupportedSchemaRanges))
	}
	for i, want := range workflow.SupportedSchemaRanges {
		g := got.Supported[i]
		if g.Major != want.Major || g.MinMinor != want.MinMinor || g.MaxMinor != want.MaxMinor {
			t.Fatalf("supported[%d] = %+v; want %+v", i, g, want)
		}
	}
}

func TestWorkflowSchemaVersionsRegistered(t *testing.T) {
	t.Parallel()
	entry, ok := lookupAction("workflows.schema-version", "versions")
	if !ok {
		t.Fatalf("workflows.schema-version/versions not registered")
	}
	if entry.ContentType != "application/json" {
		t.Fatalf("ContentType = %q; want application/json", entry.ContentType)
	}
	if entry.Handler == nil {
		t.Fatalf("Handler is nil")
	}
}
