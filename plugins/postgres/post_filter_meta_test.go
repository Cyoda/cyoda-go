package postgres

import (
	"encoding/json"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/google/uuid"
)

// The residual evaluator must see the same document every other backend's
// evaluator sees: the entity's domain data, without the storage-layer _meta
// block postgres merges into its JSONB column.
//
// memory and sqlite pass entity.Data, which has no _meta key at all, so a
// condition naming a data path under _meta cannot match there. Postgres must
// agree — a matchable path on one backend and not another is a divergence,
// and it exposes storage-layer internals as a queryable surface.
//
// Meta fields remain reachable the supported way, through Source: SourceMeta,
// which reads entity.Meta and is unaffected by this.
func TestEvalPostFilter_MetaBlockIsNotAMatchableDataPath(t *testing.T) {
	now := time.Now().UTC()
	ent := &spi.Entity{
		Data: json.RawMessage(`{"status":"OPEN"}`),
		Meta: spi.EntityMeta{
			ID:               uuid.NewString(),
			TenantID:         "t1",
			ModelRef:         spi.ModelRef{EntityName: "Order", ModelVersion: "1"},
			State:            "active",
			CreationDate:     now,
			LastModifiedDate: now,
		},
	}

	doc, err := marshalEntityDoc(ent, now, now, now, false)
	if err != nil {
		t.Fatalf("marshalEntityDoc: %v", err)
	}
	// Precondition: the stored document really does carry _meta.state.
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(doc, &probe); err != nil {
		t.Fatalf("unmarshal doc: %v", err)
	}
	if _, ok := probe["_meta"]; !ok {
		t.Fatal("precondition failed: stored doc has no _meta block")
	}

	decoded, err := unmarshalEntityDoc(doc)
	if err != nil {
		t.Fatalf("unmarshalEntityDoc: %v", err)
	}

	metaAsData := spi.Filter{
		Op:       spi.FilterEq,
		Path:     "_meta.state",
		Source:   spi.SourceData,
		Value:    "active",
		Declared: []spi.DataType{spi.String},
	}

	metaAsDataPF, err := spi.Prepare(metaAsData)
	if err != nil {
		t.Fatalf("spi.Prepare: %v", err)
	}
	got := evalPostFilter(metaAsDataPF, decoded)
	if got {
		t.Error("a data-source condition on _meta.state matched: postgres is exposing the storage-layer meta block as a queryable data path, which memory and sqlite do not")
	}

	// The domain data must still match, so the fix is not simply starving the
	// evaluator of the document.
	domain := spi.Filter{
		Op:       spi.FilterEq,
		Path:     "status",
		Source:   spi.SourceData,
		Value:    "OPEN",
		Declared: []spi.DataType{spi.String},
	}
	domainPF, err := spi.Prepare(domain)
	if err != nil {
		t.Fatalf("spi.Prepare: %v", err)
	}
	ok := evalPostFilter(domainPF, decoded)
	if !ok {
		t.Error("domain data condition failed to match")
	}

	// And a SourceMeta condition on the same field must still work — that is
	// the supported way to filter on state.
	// Declared mirrors what lifecycleToFilter stamps on a non-temporal meta
	// leaf; without it the comparison expands into no declared type.
	metaProper := spi.Filter{
		Op:       spi.FilterEq,
		Path:     "state",
		Source:   spi.SourceMeta,
		Value:    "active",
		Declared: []spi.DataType{spi.String},
	}
	metaProperPF, err := spi.Prepare(metaProper)
	if err != nil {
		t.Fatalf("spi.Prepare: %v", err)
	}
	ok = evalPostFilter(metaProperPF, decoded)
	if !ok {
		t.Error("SourceMeta condition on state failed to match")
	}
}
