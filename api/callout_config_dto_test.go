package api

import (
	"strings"
	"testing"
)

// TestCalloutConfigDtos pins the callout fields the three callout DTOs carry:
// idempotent on a processor, retryPolicy with the NONE/FIXED enum on all
// three, and a non-negative responseTimeoutMs whose description names the
// server-side upper bound (a setting, so it cannot be a schema `maximum`).
func TestCalloutConfigDtos(t *testing.T) {
	doc, err := GetSwagger()
	if err != nil {
		t.Fatalf("GetSwagger: %v", err)
	}
	for _, name := range []string{"ExternalizedProcessorConfigDto", "ExternalizedFunctionConfigDto", "ScheduleFunctionDto"} {
		ref := doc.Components.Schemas[name]
		if ref == nil || ref.Value == nil {
			t.Fatalf("%s schema missing", name)
		}
		props := ref.Value.Properties

		rp := props["retryPolicy"]
		if rp == nil || rp.Value == nil {
			t.Errorf("%s.retryPolicy missing", name)
		} else {
			got := map[any]bool{}
			for _, v := range rp.Value.Enum {
				got[v] = true
			}
			if len(got) != 2 || !got["NONE"] || !got["FIXED"] {
				t.Errorf("%s.retryPolicy enum = %v, want [NONE FIXED]", name, rp.Value.Enum)
			}
			if strings.Contains(rp.Value.Description, "delay") {
				t.Errorf("%s.retryPolicy description still promises a delay between tries: %q", name, rp.Value.Description)
			}
			if !strings.Contains(rp.Value.Description, "not a hard limit") {
				t.Errorf("%s.retryPolicy description must say the number of tries is not a hard limit: %q", name, rp.Value.Description)
			}
		}

		rt := props["responseTimeoutMs"]
		if rt == nil || rt.Value == nil {
			t.Fatalf("%s.responseTimeoutMs missing", name)
		}
		if rt.Value.Min == nil || *rt.Value.Min != 0 {
			t.Errorf("%s.responseTimeoutMs must declare minimum: 0", name)
		}
		if !strings.Contains(rt.Value.Description, "CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS") {
			t.Errorf("%s.responseTimeoutMs description must name the upper-bound setting: %q", name, rt.Value.Description)
		}
	}

	idem := doc.Components.Schemas["ExternalizedProcessorConfigDto"].Value.Properties["idempotent"]
	if idem == nil || idem.Value == nil || !idem.Value.Type.Is("boolean") {
		t.Error("ExternalizedProcessorConfigDto.idempotent must be a boolean property")
	}
}
