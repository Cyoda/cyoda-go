package postgres

import (
	"encoding/json"
	"testing"
)

// TestEntityDoc_EmptyObjectDataRoundTripsAsDecodableJSON pins the round-trip for
// an entity whose domain payload is a legitimate empty object.
//
// marshalEntityDoc merges _meta into the domain data, so `{}` is stored as
// `{"_meta":{...}}`. On read, _meta is deleted and — before this was fixed —
// nothing remained, so Data was left nil. The consumer decodes Data directly
// (internal/domain/entity/service.go), and decoding a zero-length slice is
// io.EOF, which the service reports as an internal error. The observable
// result was that `{}` wrote successfully and then returned 500 forever, on
// the entity AND on the whole model's list endpoint.
//
// Data must therefore come back as decodable JSON, never as a zero-length
// slice.
func TestEntityDoc_EmptyObjectDataRoundTripsAsDecodableJSON(t *testing.T) {
	ent := testEntity()
	ent.Data = []byte(`{}`)

	raw, err := marshalEntityDoc(ent, testTime, testTime, testTime, false)
	if err != nil {
		t.Fatalf("marshalEntityDoc: %v", err)
	}

	got, err := unmarshalEntityDoc(raw)
	if err != nil {
		t.Fatalf("unmarshalEntityDoc: %v", err)
	}

	if len(got.Data) == 0 {
		t.Fatalf("Data came back empty; the consumer decodes it directly and a zero-length slice is io.EOF -> 500")
	}
	var m map[string]any
	if err := json.Unmarshal(got.Data, &m); err != nil {
		t.Fatalf("Data is not decodable JSON (%q): %v", got.Data, err)
	}
	if len(m) != 0 {
		t.Errorf("Data = %q, want an empty object", got.Data)
	}
}

// TestEntityDoc_EmptyObjectVersionRoundTrips covers the same property on the
// version-history read path, which the model-wide list endpoint uses.
func TestEntityDoc_EmptyObjectVersionRoundTrips(t *testing.T) {
	ent := testEntity()
	ent.Data = []byte(`{}`)

	raw, err := marshalEntityDoc(ent, testTime, testTime, testTime, false)
	if err != nil {
		t.Fatalf("marshalEntityDoc: %v", err)
	}

	ver, err := unmarshalEntityVersion(raw, 1, testTime)
	if err != nil {
		t.Fatalf("unmarshalEntityVersion: %v", err)
	}
	if len(ver.Entity.Data) == 0 {
		t.Fatalf("version Data came back empty; decoding it is io.EOF -> 500 on the list path")
	}
	var m map[string]any
	if err := json.Unmarshal(ver.Entity.Data, &m); err != nil {
		t.Fatalf("version Data is not decodable JSON (%q): %v", ver.Entity.Data, err)
	}
}

// TestEntityDoc_DeletedVersionKeepsNoDomainData guards the fix from
// over-reaching. A DELETED version legitimately carries no domain data — that
// is distinct from an empty object, and the distinction is load-bearing for
// version-history consumers. Only a non-deleted document with no domain keys
// may be reported as `{}`.
func TestEntityDoc_DeletedVersionKeepsNoDomainData(t *testing.T) {
	ent := testEntity()
	ent.Data = nil

	raw, err := marshalEntityDoc(ent, testTime, testTime, testTime, true /* deleted */)
	if err != nil {
		t.Fatalf("marshalEntityDoc: %v", err)
	}

	got, err := unmarshalEntityDoc(raw)
	if err != nil {
		t.Fatalf("unmarshalEntityDoc: %v", err)
	}
	if len(got.Data) != 0 {
		t.Errorf("deleted version Data = %q, want no domain data", got.Data)
	}
}
