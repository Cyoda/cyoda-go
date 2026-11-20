package externalapi

import (
	"encoding/json"
	"testing"

	"github.com/cyoda/cyoda-go/e2e/externalapi/driver"
	"github.com/cyoda/cyoda-go/e2e/parity"
)

// bodyPreview returns the first 200 bytes of body for diagnostic logging,
// suffixed with an ellipsis when truncated. Bounds test-output volume and
// limits the surface for accidental sensitive-data leakage from server
// responses to a single short prefix.
func bodyPreview(body []byte) string {
	const max = 200
	if len(body) <= max {
		return string(body)
	}
	return string(body[:max]) + "...(truncated)"
}

func init() {
	// External API scenario suite — tranche 1
	// 03-entity-ingestion-single
	parity.Register(
		parity.NamedTest{Name: "ExternalAPI_03_01_CreateEntitySuccess", Fn: RunExternalAPI_03_01_CreateEntitySuccess},
		parity.NamedTest{Name: "ExternalAPI_03_02_ListOfObjects", Fn: RunExternalAPI_03_02_ListOfObjects},
		parity.NamedTest{Name: "ExternalAPI_03_03_AllFieldsRoundTrip", Fn: RunExternalAPI_03_03_AllFieldsRoundTrip},
		parity.NamedTest{Name: "ExternalAPI_03_04_FamilyNested", Fn: RunExternalAPI_03_04_FamilyNested},
	)
}

// RunExternalAPI_03_01_CreateEntitySuccess — dictionary 03/01.
// Register a simple model, lock it, create one entity, and verify the
// returned UUID is non-zero and the data round-trips correctly.
func RunExternalAPI_03_01_CreateEntitySuccess(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)

	if err := d.CreateModelFromSample("simple1", 1, `{"key1": 123}`); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := d.LockModel("simple1", 1); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	id, err := d.CreateEntity("simple1", 1, `{"key1": 42}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	if id.String() == "00000000-0000-0000-0000-000000000000" {
		t.Fatal("expected non-zero entityId")
	}
	got, err := d.GetEntity(id)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	// JSON numbers decode to float64 via encoding/json.
	if got.Data["key1"] != float64(42) {
		t.Errorf("data.key1: got %v (%T), want 42", got.Data["key1"], got.Data["key1"])
	}
}

// RunExternalAPI_03_02_ListOfObjects — dictionary 03/02.
// POST a JSON array of objects creates one entity per element.
// The YAML scenario uses a 2-element array and asserts entity_count=2.
func RunExternalAPI_03_02_ListOfObjects(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)

	// Register from a single-object sample so the model root type is
	// OBJECT (not ARRAY). The create endpoint accepts either an object or
	// a JSON array of objects and creates one entity per element —
	// regardless of how the model was registered.
	if err := d.CreateModelFromSample("simple2", 1, `{"key": 123}`); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := d.LockModel("simple2", 1); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	status, body, err := d.CreateEntityRaw("simple2", 1, `[{"key": 123}, {"key": 456}]`)
	if err != nil {
		t.Fatalf("CreateEntityRaw: %v (status %d body=%s)", err, status, bodyPreview(body))
	}
	if status != 200 {
		t.Fatalf("status: got %d, want 200 (body=%s)", status, bodyPreview(body))
	}
	// Decode the response as the standard EntityTransactionInfo array.
	var txInfos []struct {
		TransactionID string   `json:"transactionId,omitempty"`
		EntityIDs     []string `json:"entityIds"`
	}
	if err := json.Unmarshal(body, &txInfos); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, bodyPreview(body))
	}
	var totalIDs int
	for _, tx := range txInfos {
		totalIDs += len(tx.EntityIDs)
	}
	if totalIDs != 2 {
		t.Errorf("got %d total entity IDs, want 2 (txInfos=%v)", totalIDs, txInfos)
	}
	list, err := d.ListEntitiesByModel("simple2", 1)
	if err != nil {
		t.Fatalf("ListEntitiesByModel: %v", err)
	}
	if len(list) != 2 {
		t.Errorf("list size: got %d, want 2", len(list))
	}
}

// RunExternalAPI_03_03_AllFieldsRoundTrip — dictionary 03/03.
// Register a model covering all supported scalar types, lock, create one
// entity, and verify that each field is present in the read-back data.
func RunExternalAPI_03_03_AllFieldsRoundTrip(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)

	// Sample covers every supported field class: string, integer, bool,
	// null, float, array, nested object.
	sample := `{"s":"hi","i":7,"b":true,"n":null,"f":1.5,"arr":[1,2],"obj":{"x":1}}`
	if err := d.CreateModelFromSample("allfields", 1, sample); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := d.LockModel("allfields", 1); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	id, err := d.CreateEntity("allfields", 1, sample)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	got, err := d.GetEntity(id)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	// Assert each non-null field is present in the round-tripped data.
	// "n" (null) is intentionally excluded: encoding/json unmarshals a
	// JSON null value as a nil map entry which may or may not be present
	// depending on whether the server strips nil fields on serialisation.
	for _, k := range []string{"s", "i", "b", "f", "arr", "obj"} {
		if _, ok := got.Data[k]; !ok {
			t.Errorf("missing round-tripped field %q in data: %v", k, got.Data)
		}
	}
}

// RunExternalAPI_03_04_FamilyNested — dictionary 03/04.
// Register a flat-array family model (each member is a sibling object, not
// a tree), lock, create all 5 members in one POST, and verify 5 entities
// were stored.
func RunExternalAPI_03_04_FamilyNested(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)

	// Per YAML 03/04: the model is a family-member object. Register using a
	// single-object sample so the model root type is OBJECT (not ARRAY).
	// The create endpoint accepts a JSON array and creates one entity per
	// element — each family member becomes its own entity.
	if err := d.CreateModelFromSample("family", 1, `{"name":"Father","age":50,"relation":"FATHER"}`); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	familyArray := `[{"name":"Father","age":50,"relation":"FATHER"},{"name":"Daughter","age":20,"relation":"DAUGHTER"},{"name":"Son","age":18,"relation":"SON"},{"name":"GrandDaughter","age":2,"relation":"GRANDDAUGHTER"},{"name":"GrandSon","age":1,"relation":"GRANDSON"}]`
	if err := d.LockModel("family", 1); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	status, body, err := d.CreateEntityRaw("family", 1, familyArray)
	if err != nil {
		t.Fatalf("CreateEntityRaw: %v (status %d body=%s)", err, status, bodyPreview(body))
	}
	if status != 200 {
		t.Fatalf("status: got %d, want 200 (body=%s)", status, bodyPreview(body))
	}
	list, err := d.ListEntitiesByModel("family", 1)
	if err != nil {
		t.Fatalf("ListEntitiesByModel: %v", err)
	}
	if len(list) != 5 {
		t.Errorf("family list size: got %d, want 5", len(list))
	}
}
