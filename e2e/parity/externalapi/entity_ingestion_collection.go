package externalapi

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/externalapi/driver"
	"github.com/cyoda-platform/cyoda-go/e2e/parity"
)

func init() {
	// External API scenario suite — tranche 1 (issue #118)
	// 04-entity-ingestion-collection
	parity.Register(
		parity.NamedTest{Name: "ExternalAPI_04_01_FamilyAndPets", Fn: RunExternalAPI_04_01_FamilyAndPets},
		parity.NamedTest{Name: "ExternalAPI_04_02_UpdateCollectionAge", Fn: RunExternalAPI_04_02_UpdateCollectionAge},
		parity.NamedTest{Name: "ExternalAPI_04_04_TransactionWindow", Fn: RunExternalAPI_04_04_TransactionWindow},
	)
}

// RunExternalAPI_04_01_FamilyAndPets — dictionary 04/01.
// Register two models (family/1 and pets/1), lock both, POST a heterogeneous
// collection containing 1 family + 1 pets item via CreateEntitiesCollection.
// Expect 2 entity IDs returned and each model to have exactly 1 entity.
func RunExternalAPI_04_01_FamilyAndPets(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)

	familyJSON := `{"name":"father","age":50,"kids":[{"name":"son","age":20}]}`
	petsJSON := `{"name":"cat","age":3,"species":"CAT"}`

	if err := d.CreateModelFromSample("family", 1, familyJSON); err != nil {
		t.Fatalf("CreateModelFromSample family: %v", err)
	}
	if err := d.LockModel("family", 1); err != nil {
		t.Fatalf("LockModel family: %v", err)
	}
	if err := d.CreateModelFromSample("pets", 1, petsJSON); err != nil {
		t.Fatalf("CreateModelFromSample pets: %v", err)
	}
	if err := d.LockModel("pets", 1); err != nil {
		t.Fatalf("LockModel pets: %v", err)
	}

	items := []driver.CollectionItem{
		{ModelName: "family", ModelVersion: 1, Payload: familyJSON},
		{ModelName: "pets", ModelVersion: 1, Payload: petsJSON},
	}
	ids, err := d.CreateEntitiesCollection(items)
	if err != nil {
		t.Fatalf("CreateEntitiesCollection: %v", err)
	}
	if len(ids) != 2 {
		t.Errorf("ids: got %d, want 2 (one per model)", len(ids))
	}

	for _, m := range []string{"family", "pets"} {
		list, err := d.ListEntitiesByModel(m, 1)
		if err != nil {
			t.Fatalf("ListEntitiesByModel(%s): %v", m, err)
		}
		if len(list) != 1 {
			t.Errorf("%s: got %d entities, want 1", m, len(list))
		}
	}
}

// RunExternalAPI_04_02_UpdateCollectionAge — dictionary 04/02.
// Self-contained — does not depend on 04/01 having run first.
// Register family2/1 and pets2/1, lock, create one entity in each via
// CreateEntitiesCollection. Then update both with incremented ages via
// UpdateEntitiesCollection (loopback transition). Verify each entity's
// age was updated by re-fetching via GetEntity.
//
// Note: The YAML scenario specifies transition "UPDATE", which requires a
// named workflow transition. This test uses the loopback (Transition: "")
// which is the cyoda-go equivalent of a data-only update — it works
// against any entity regardless of whether a workflow is imported.
func RunExternalAPI_04_02_UpdateCollectionAge(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)

	if err := d.CreateModelFromSample("family2", 1, `{"name":"f","age":50}`); err != nil {
		t.Fatalf("CreateModelFromSample family2: %v", err)
	}
	if err := d.LockModel("family2", 1); err != nil {
		t.Fatalf("LockModel family2: %v", err)
	}
	if err := d.CreateModelFromSample("pets2", 1, `{"name":"c","age":3}`); err != nil {
		t.Fatalf("CreateModelFromSample pets2: %v", err)
	}
	if err := d.LockModel("pets2", 1); err != nil {
		t.Fatalf("LockModel pets2: %v", err)
	}

	createIDs, err := d.CreateEntitiesCollection([]driver.CollectionItem{
		{ModelName: "family2", ModelVersion: 1, Payload: `{"name":"f","age":50}`},
		{ModelName: "pets2", ModelVersion: 1, Payload: `{"name":"c","age":3}`},
	})
	if err != nil || len(createIDs) != 2 {
		t.Fatalf("setup create: ids=%v err=%v", createIDs, err)
	}

	// Increment age: family2 entity 50→60, pets2 entity 3→13.
	// Loopback transition (Transition: "") performs a data-only update
	// without requiring a named workflow transition to be configured.
	updates := []driver.UpdateCollectionItem{
		{ID: createIDs[0], Payload: `{"name":"f","age":60}`, Transition: ""},
		{ID: createIDs[1], Payload: `{"name":"c","age":13}`, Transition: ""},
	}
	if _, err := d.UpdateEntitiesCollection(updates); err != nil {
		t.Fatalf("UpdateEntitiesCollection: %v", err)
	}

	// Verify each entity carries the incremented age.
	wantAges := map[string]float64{
		createIDs[0].String(): 60,
		createIDs[1].String(): 13,
	}
	for _, id := range createIDs {
		got, err := d.GetEntity(id)
		if err != nil {
			t.Fatalf("GetEntity(%s): %v", id, err)
		}
		ageFloat, ok := got.Data["age"].(float64)
		if !ok {
			t.Errorf("age not a number for %s: %v", id, got.Data["age"])
			continue
		}
		want := wantAges[id.String()]
		if ageFloat != want {
			t.Errorf("age for entity %s: got %v, want %v", id, ageFloat, want)
		}
	}
}

// RunExternalAPI_04_04_TransactionWindow — dictionary 04/04.
// Pins the documented `transactionWindow` chunking contract from issue
// #227 against every backend: a POST of N items with a client-supplied
// window=W produces ceil(N/W) chunk elements, each with a non-empty
// transactionId and a `entityIds` slice of the right length, and the
// entities are subsequently retrievable via ListEntitiesByModel.
//
// Concretely: window=3 with N=7 must produce three chunks of size 3, 3,
// and 1. The window-supplied form is what makes this an actual chunking
// exercise (the default window of 100 would otherwise collapse to a
// single chunk and the test would only verify aggregate count).
func RunExternalAPI_04_04_TransactionWindow(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)

	if err := d.CreateModelFromSample("txwin", 1, `{"k":1}`); err != nil {
		t.Fatalf("CreateModelFromSample txwin: %v", err)
	}
	if err := d.LockModel("txwin", 1); err != nil {
		t.Fatalf("LockModel txwin: %v", err)
	}

	const (
		N      = 7
		Window = 3
	)
	items := make([]driver.CollectionItem, 0, N)
	for i := 0; i < N; i++ {
		items = append(items, driver.CollectionItem{
			ModelName:    "txwin",
			ModelVersion: 1,
			Payload:      fmt.Sprintf(`{"k":%d}`, i),
		})
	}

	rawBody, err := d.CreateEntitiesCollectionRawWithWindow(items, Window)
	if err != nil {
		t.Fatalf("CreateEntitiesCollectionRawWithWindow: %v", err)
	}

	// Per-chunk response array. Three chunks expected: 3 + 3 + 1 = 7.
	type chunkInfo struct {
		TransactionID string   `json:"transactionId"`
		EntityIDs     []string `json:"entityIds"`
	}
	var chunks []chunkInfo
	if err := json.Unmarshal(rawBody, &chunks); err != nil {
		t.Fatalf("decode chunked response: %v; body=%s", err, rawBody)
	}

	expectedSizes := []int{3, 3, 1}
	if len(chunks) != len(expectedSizes) {
		t.Fatalf("chunk count: got %d, want %d (window=%d, N=%d); body=%s",
			len(chunks), len(expectedSizes), Window, N, rawBody)
	}

	totalIDs := 0
	for i, ch := range chunks {
		if ch.TransactionID == "" {
			t.Errorf("chunk[%d].transactionId is empty; body=%s", i, rawBody)
		}
		if len(ch.EntityIDs) != expectedSizes[i] {
			t.Errorf("chunk[%d].entityIds: got %d, want %d; body=%s",
				i, len(ch.EntityIDs), expectedSizes[i], rawBody)
		}
		totalIDs += len(ch.EntityIDs)
	}
	if totalIDs != N {
		t.Errorf("total entityIds across chunks: got %d, want %d; body=%s",
			totalIDs, N, rawBody)
	}

	// All N entities must be readable via ListEntitiesByModel.
	list, err := d.ListEntitiesByModel("txwin", 1)
	if err != nil {
		t.Fatalf("ListEntitiesByModel: %v", err)
	}
	if len(list) != N {
		t.Errorf("list after chunked create: got %d, want %d", len(list), N)
	}
}
