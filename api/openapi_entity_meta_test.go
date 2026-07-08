package api

import "testing"

// TestEnvelopeMetaTypedButOpen asserts Envelope.meta is a typed-but-open schema
// mirroring the canonical EntityMetadata.json: required id/state/creationDate/
// lastUpdateTime, optional modelKey/pointInTime/transitionForLatestSave/
// transactionId, never sealed, and no previousTransition fossil.
func TestEnvelopeMetaTypedButOpen(t *testing.T) {
	doc, err := GetSwagger()
	if err != nil {
		t.Fatalf("GetSwagger: %v", err)
	}
	env := doc.Components.Schemas["Envelope"]
	if env == nil || env.Value == nil {
		t.Fatal("Envelope schema missing")
	}
	metaRef := env.Value.Properties["meta"]
	if metaRef == nil || metaRef.Value == nil {
		t.Fatal("Envelope.meta missing or unresolved")
	}
	meta := metaRef.Value

	req := map[string]bool{}
	for _, r := range meta.Required {
		req[r] = true
	}
	for _, want := range []string{"id", "state", "creationDate", "lastUpdateTime"} {
		if !req[want] {
			t.Errorf("meta.required missing %q", want)
		}
	}
	for _, want := range []string{"id", "state", "creationDate", "lastUpdateTime", "modelKey", "pointInTime", "transitionForLatestSave", "transactionId"} {
		if meta.Properties[want] == nil {
			t.Errorf("meta.properties missing %q", want)
		}
	}
	if meta.AdditionalProperties.Has != nil && !*meta.AdditionalProperties.Has {
		t.Error("meta must be typed-but-open, never additionalProperties:false")
	}
	if meta.Properties["previousTransition"] != nil {
		t.Error("previousTransition fossil must be removed from meta")
	}
}
