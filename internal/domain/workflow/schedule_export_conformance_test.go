package workflow

import (
	"encoding/json"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	genapi "github.com/cyoda-platform/cyoda-go/api"
)

// TestScheduleWireShapeConformsToPublishedSchema asserts that what the export
// handler puts on the wire for a schedule validates against the published
// TransitionScheduleDto. The export path marshals spi types directly (see
// ExportEntityModelWorkflow), so the SPI struct's json tags *are* the published
// export shape: a key the schema forbids is a contract violation, not an
// internal detail. Three shapes are checked: a static delay on its own, a
// Function-driven schedule (no delay at all — the shape that regressed), and
// a static delay carrying a timeout — timeoutMs is documented as independent
// of delayMs, so it needs its own case rather than riding along unlabeled.
func TestScheduleWireShapeConformsToPublishedSchema(t *testing.T) {
	t.Parallel()

	doc, err := genapi.GetSwagger()
	if err != nil {
		t.Fatalf("GetSwagger: %v", err)
	}
	ref := doc.Components.Schemas["TransitionScheduleDto"]
	if ref == nil || ref.Value == nil {
		t.Fatal("TransitionScheduleDto schema missing")
	}
	schema := ref.Value

	timeout := int64(5000)
	cases := []struct {
		name     string
		schedule spi.TransitionSchedule
	}{
		{
			name:     "static_delay",
			schedule: spi.TransitionSchedule{DelayMs: 86400000},
		},
		{
			name: "function_driven_no_delay",
			schedule: spi.TransitionSchedule{Function: &spi.ScheduleFunction{
				Name:                 "computeFire",
				ResultKind:           "Schedule",
				CalculationNodesTags: "scheduler",
				RetryPolicy:          "NONE",
			}},
		},
		{
			name:     "static_delay_with_timeout",
			schedule: spi.TransitionSchedule{DelayMs: 86400000, TimeoutMs: &timeout},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bs, err := json.Marshal(tc.schedule)
			if err != nil {
				t.Fatalf("marshal schedule: %v", err)
			}
			var value any
			if err := json.Unmarshal(bs, &value); err != nil {
				t.Fatalf("unmarshal to generic value: %v", err)
			}
			if err := schema.VisitJSON(value); err != nil {
				t.Errorf("exported schedule does not satisfy TransitionScheduleDto: %v\nwire shape: %s", err, bs)
			}
		})
	}
}
