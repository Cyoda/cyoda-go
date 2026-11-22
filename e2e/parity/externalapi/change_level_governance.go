package externalapi

import (
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"testing"

	"github.com/cyoda/cyoda-go/e2e/externalapi/driver"
	"github.com/cyoda/cyoda-go/e2e/externalapi/errorcontract"
	"github.com/cyoda/cyoda-go/e2e/parity"
)

func init() {
	parity.Register(
		parity.NamedTest{Name: "ExternalAPI_02_01_SetChangeLevelStructural", Fn: RunExternalAPI_02_01_SetChangeLevelStructural},
		parity.NamedTest{Name: "ExternalAPI_02_02_StructuralNullFieldNoChangelog", Fn: RunExternalAPI_02_02_StructuralNullFieldNoChangelog},
		parity.NamedTest{Name: "ExternalAPI_02_03_TypeWideningIntToFloat", Fn: RunExternalAPI_02_03_TypeWideningIntToFloat},
		parity.NamedTest{Name: "ExternalAPI_02_04_TypeNarrowingFloatToInt", Fn: RunExternalAPI_02_04_TypeNarrowingFloatToInt},
		parity.NamedTest{Name: "ExternalAPI_02_05_UpdatedSchemaThenLockAndSave", Fn: RunExternalAPI_02_05_UpdatedSchemaThenLockAndSave},
		parity.NamedTest{Name: "ExternalAPI_02_06_MultinodeTypeLevelAllFields", Fn: RunExternalAPI_02_06_MultinodeTypeLevelAllFields},
		parity.NamedTest{Name: "ExternalAPI_02_07_ConcurrentExtendVersions", Fn: RunExternalAPI_02_07_ConcurrentExtendVersions},
	)
}

// RunExternalAPI_02_01_SetChangeLevelStructural — dictionary 02/01.
func RunExternalAPI_02_01_SetChangeLevelStructural(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)
	if err := d.CreateModelFromSample("cl1", 1, `{"k":1}`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := d.SetChangeLevel("cl1", 1, "STRUCTURAL"); err != nil {
		t.Fatalf("SetChangeLevel: %v", err)
	}
	// Assertion: the call returned nil. Deeper inspection (verifying
	// the change_level field landed on the model) requires a model-
	// detail GET endpoint that cyoda-go does not expose.
}

// RunExternalAPI_02_02_StructuralNullFieldNoChangelog — dictionary 02/02.
func RunExternalAPI_02_02_StructuralNullFieldNoChangelog(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)
	sample := `{"items":[{"k":1}]}`
	if err := d.CreateModelFromSample("cl2", 1, sample); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := d.SetChangeLevel("cl2", 1, "STRUCTURAL"); err != nil {
		t.Fatalf("SetChangeLevel: %v", err)
	}
	if err := d.LockModel("cl2", 1); err != nil {
		t.Fatalf("lock: %v", err)
	}
	if _, err := d.CreateEntity("cl2", 1, `{"items":null}`); err != nil {
		t.Fatalf("CreateEntity[null-array]: %v", err)
	}
	// A subsequent populated entity must still succeed (no schema
	// growth from the null).
	if _, err := d.CreateEntity("cl2", 1, `{"items":[{"k":2}]}`); err != nil {
		t.Fatalf("CreateEntity[populated]: %v", err)
	}
}

// RunExternalAPI_02_03_TypeWideningIntToFloat — dictionary 02/03 (NEGATIVE).
// Dictionary expects HTTP 400 + FoundIncompatibleTypeWitEntityModelException.
// equiv_or_better: cyoda-go emits INCOMPATIBLE_TYPE @400 with
// structured Props (fieldPath, expectedType, actualType).
func RunExternalAPI_02_03_TypeWideningIntToFloat(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)
	if err := d.CreateModelFromSample("cl3", 1, `{"price": 13}`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := d.LockModel("cl3", 1); err != nil {
		t.Fatalf("lock: %v", err)
	}
	status, body, err := d.CreateEntityRaw("cl3", 1, `{"price": 13.111}`)
	if err != nil {
		t.Fatalf("CreateEntityRaw: %v", err)
	}
	// equiv_or_better: INCOMPATIBLE_TYPE maps to
	// FoundIncompatibleTypeWithEntityModelException in the cloud dictionary;
	// detail string carries "value of type DOUBLE is not compatible with [INTEGER]"
	// and properties carries the structured (fieldPath, expectedType, actualType).
	errorcontract.Match(t, status, body, errorcontract.ExpectedError{
		HTTPStatus: http.StatusBadRequest,
		ErrorCode:  "INCOMPATIBLE_TYPE",
	})
}

// RunExternalAPI_02_04_TypeNarrowingFloatToInt — dictionary 02/04.
func RunExternalAPI_02_04_TypeNarrowingFloatToInt(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)
	if err := d.CreateModelFromSample("cl4", 1, `{"price": 13.5}`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := d.LockModel("cl4", 1); err != nil {
		t.Fatalf("lock: %v", err)
	}
	if _, err := d.CreateEntity("cl4", 1, `{"price": 14}`); err != nil {
		t.Fatalf("CreateEntity[int-into-float]: %v", err)
	}
}

// RunExternalAPI_02_05_UpdatedSchemaThenLockAndSave — dictionary 02/05.
func RunExternalAPI_02_05_UpdatedSchemaThenLockAndSave(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)
	if err := d.CreateModelFromSample("cl5", 1, `{"a": 1}`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := d.UpdateModelFromSample("cl5", 1, `{"a": 1, "b": "hello"}`); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := d.LockModel("cl5", 1); err != nil {
		t.Fatalf("lock: %v", err)
	}
	if _, err := d.CreateEntity("cl5", 1, `{"a": 2, "b": "world"}`); err != nil {
		t.Fatalf("CreateEntity[extended]: %v", err)
	}
}

// RunExternalAPI_02_06_MultinodeTypeLevelAllFields — dictionary 02/06.
// Bound N=10 (not 100) — parity smoke does not need load testing.
func RunExternalAPI_02_06_MultinodeTypeLevelAllFields(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)
	const N = 10
	allFields := `{"s":"hi","i":7,"b":true,"f":1.5,"arr":[1,2],"obj":{"x":1}}`
	if err := d.CreateModelFromSample("cl6", 1, allFields); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := d.SetChangeLevel("cl6", 1, "TYPE"); err != nil {
		t.Fatalf("SetChangeLevel: %v", err)
	}
	if err := d.LockModel("cl6", 1); err != nil {
		t.Fatalf("lock: %v", err)
	}
	for i := 0; i < N; i++ {
		if _, err := d.CreateEntity("cl6", 1, allFields); err != nil {
			t.Fatalf("CreateEntity[%d]: %v", i, err)
		}
	}
	list, err := d.ListEntitiesByModel("cl6", 1)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != N {
		t.Errorf("entity count: got %d, want %d", len(list), N)
	}
}

// RunExternalAPI_02_07_ConcurrentExtendVersions — dictionary 02/07.
// Bound N=5 (not 30) — parity is about the contract not the load.
func RunExternalAPI_02_07_ConcurrentExtendVersions(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)
	const N = 5
	var wg sync.WaitGroup
	errs := make(chan error, N)
	for v := 1; v <= N; v++ {
		wg.Add(1)
		go func(version int) {
			defer wg.Done()
			sample := fmt.Sprintf(`{"f%d": %d}`, version, version)
			if err := d.CreateModelFromSample("cl7", version, sample); err != nil {
				errs <- fmt.Errorf("create v%d: %w", version, err)
				return
			}
			if err := d.SetChangeLevel("cl7", version, "STRUCTURAL"); err != nil {
				errs <- fmt.Errorf("setchangelevel v%d: %w", version, err)
			}
		}(v)
	}
	wg.Wait()
	close(errs)
	var msgs []string
	for e := range errs {
		msgs = append(msgs, e.Error())
	}
	if len(msgs) > 0 {
		t.Fatalf("concurrent ops failed: %s errors: %s", strconv.Itoa(len(msgs)), fmt.Sprint(msgs))
	}
	models, err := d.ListModels()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	count := 0
	for _, m := range models {
		if m.ModelName == "cl7" {
			count++
		}
	}
	if count != N {
		t.Errorf("model versions: got %d, want %d", count, N)
	}
}
