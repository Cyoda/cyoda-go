package help

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
)

// workflowSchemaVersionsPayload mirrors what the versions action
// emits. Defined here (not in the workflow package) because it's a
// wire-format concern of the help subsystem; the workflow package
// owns the constants.
type workflowSchemaVersionsPayload struct {
	Current   string                 `json:"current"`
	Supported []workflow.SchemaRange `json:"supported"`
}

// emitWorkflowSchemaVersions writes the supported workflow-schema
// version manifest as JSON. The shape is the contract; consumers
// pin tooling against it. See cmd/cyoda/help/content/workflows/
// schema-version.md for the human-readable explanation.
func emitWorkflowSchemaVersions(w io.Writer) int {
	// Defensive copy so the registered action never aliases the
	// production slice (ownership rule 6: consumed once serialized).
	supported := make([]workflow.SchemaRange, len(workflow.SupportedSchemaRanges))
	copy(supported, workflow.SupportedSchemaRanges)
	payload := workflowSchemaVersionsPayload{
		Current:   workflow.CurrentSchemaVersion,
		Supported: supported,
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(payload); err != nil {
		fmt.Fprintf(w, "cyoda help workflows schema-version versions: encode: %v\n", err)
		return 1
	}
	return 0
}
