package parity

import (
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cyoda/cyoda-go/e2e/parity/client"
	"github.com/google/uuid"
)

func sortedIDStrings(ids []uuid.UUID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, id.String())
	}
	sort.Strings(out)
	return out
}

func sortedStrs(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// RunEntityDeleteAllPointInTime verifies that an empty-body delete with
// pointInTime selects the committed state as at that instant on every
// backend: entities created after the instant survive, an entity selected
// at the instant but already gone is reported per id, and verbose lists the
// attempted set.
func RunEntityDeleteAllPointInTime(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "entity-delete-all-pit-test"
	const modelVersion = 1
	setupSimpleWorkflow(t, c, modelName, modelVersion)

	a, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"A","amount":1,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity a: %v", err)
	}
	b, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"B","amount":2,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity b: %v", err)
	}
	tB := LatestChangeTime(t, c, b)
	time.Sleep(50 * time.Millisecond)
	cID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"C","amount":3,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity c: %v", err)
	}
	tC := LatestChangeTime(t, c, cID)
	instant := MidpointBetween(t, tB, tC)

	if err := c.DeleteEntity(t, b); err != nil {
		t.Fatalf("DeleteEntity b: %v", err)
	}

	res, err := c.DeleteEntitiesByModelVerbose(t, modelName, modelVersion, &instant)
	if err != nil {
		t.Fatalf("DeleteEntitiesByModelVerbose: %v", err)
	}
	if res.MatchedCount != 2 || res.RemovedCount != 1 {
		t.Errorf("matched/removed = %d/%d, want 2/1", res.MatchedCount, res.RemovedCount)
	}
	if _, ok := res.IDToError[b.String()]; !ok {
		t.Errorf("idToError = %v, want an entry for the already-gone id %s", res.IDToError, b)
	}
	if got, want := sortedStrs(res.IDs), sortedIDStrings([]uuid.UUID{a, b}); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("ids = %v, want the attempted set %v", got, want)
	}
	if _, err := c.GetEntity(t, a); err == nil {
		t.Error("a existed at the instant and must be gone")
	}
	if _, err := c.GetEntity(t, cID); err != nil {
		t.Errorf("c was created after the instant and must survive: %v", err)
	}
}

// RunEntityDeleteAllVerbose verifies that an empty-body delete with
// verbose=true lists every attempted id and removes every entity, on every
// backend.
func RunEntityDeleteAllVerbose(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "entity-delete-all-verbose-test"
	const modelVersion = 1
	setupSimpleWorkflow(t, c, modelName, modelVersion)

	var ids []uuid.UUID
	for i := 0; i < 3; i++ {
		id, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"V","amount":1,"status":"new"}`)
		if err != nil {
			t.Fatalf("CreateEntity[%d]: %v", i, err)
		}
		ids = append(ids, id)
	}

	res, err := c.DeleteEntitiesByModelVerbose(t, modelName, modelVersion, nil)
	if err != nil {
		t.Fatalf("DeleteEntitiesByModelVerbose: %v", err)
	}
	if res.MatchedCount != 3 || res.RemovedCount != 3 {
		t.Errorf("matched/removed = %d/%d, want 3/3", res.MatchedCount, res.RemovedCount)
	}
	if got, want := sortedStrs(res.IDs), sortedIDStrings(ids); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("ids = %v, want %v", got, want)
	}
	for _, id := range ids {
		if _, err := c.GetEntity(t, id); err == nil {
			t.Errorf("entity %s must be gone", id)
		}
	}
}
