package workflow

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// exportStubFactory provides a WorkflowStore that returns a fixed
// set of workflows whose Version differs from CurrentSchemaVersion.
// The exporter must overwrite that on the wire.
type exportStubFactory struct {
	spi.StoreFactory
	wfs []spi.WorkflowDefinition
}

func (f *exportStubFactory) WorkflowStore(_ context.Context) (spi.WorkflowStore, error) {
	return &exportStubWFStore{wfs: f.wfs}, nil
}

// ModelStore returns a stub that always reports the requested model
// exists. v0.8.0's export handler verifies the model is present before
// reporting on its workflows (mirroring import) and would otherwise NPE
// on the embedded-nil spi.StoreFactory.
func (f *exportStubFactory) ModelStore(_ context.Context) (spi.ModelStore, error) {
	return &exportStubModelStore{}, nil
}

type exportStubWFStore struct {
	spi.WorkflowStore
	wfs []spi.WorkflowDefinition
}

func (s *exportStubWFStore) Get(_ context.Context, _ spi.ModelRef) ([]spi.WorkflowDefinition, error) {
	return s.wfs, nil
}

type exportStubModelStore struct {
	spi.ModelStore
}

func (s *exportStubModelStore) Get(_ context.Context, ref spi.ModelRef) (*spi.ModelDescriptor, error) {
	return &spi.ModelDescriptor{Ref: ref}, nil
}

func TestExportStampsCurrentSchemaVersion(t *testing.T) {
	t.Parallel()
	h := &Handler{factory: &exportStubFactory{wfs: []spi.WorkflowDefinition{
		{Name: "wf-stale", Version: "0.0", InitialState: "S",
			States: map[string]spi.StateDefinition{"S": {}}},
	}}}
	req := httptest.NewRequest(http.MethodGet, "/api/model/E/1/workflow/export", nil)
	rec := httptest.NewRecorder()
	h.ExportEntityModelWorkflow(rec, req, "E", 1)
	if rec.Code != http.StatusOK {
		t.Fatalf("export status = %d; want 200; body: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Workflows []struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"workflows"`
	}
	rawBody := rec.Body.Bytes() // snapshot before Unmarshal consumes the buffer
	if err := json.Unmarshal(rawBody, &body); err != nil {
		t.Fatalf("decode export body: %v", err)
	}
	if len(body.Workflows) != 1 {
		t.Fatalf("got %d workflows; want 1", len(body.Workflows))
	}
	if body.Workflows[0].Version != CurrentSchemaVersion {
		t.Fatalf("export Version = %q; want %q", body.Workflows[0].Version, CurrentSchemaVersion)
	}
	if strings.Contains(string(rawBody), `"0.0"`) {
		t.Fatalf("stored stale Version 0.0 leaked into export body: %s", string(rawBody))
	}
}
