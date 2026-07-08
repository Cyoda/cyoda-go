package parity

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// RunPITBoundaryExactT asserts exact-T inclusivity: querying as-at a version's
// own reported timestamp returns that version, uniformly across backends. It is
// the cross-engine guard for the canonical inclusive <= PIT rule. Sub-ms
// over-inclusion is covered by per-engine white-box tests, not here, because
// the commercial backend stores at millisecond precision.
func RunPITBoundaryExactT(t *testing.T, fixture BackendFixture) {
	t.Helper()
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "pitb"
	const modelVersion = 1

	if err := c.ImportModel(t, modelName, modelVersion, `{"k":1}`); err != nil {
		t.Fatalf("create model: %v", err)
	}
	if err := c.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("lock model: %v", err)
	}

	id, err := c.CreateEntity(t, modelName, modelVersion, `{"k":1}`)
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	t1 := pitbLatestChangeTime(t, c, id)

	// Space writes ≥1ms apart so each version lands in a distinct millisecond.
	// The commercial backend stores timestamps at millisecond precision; two
	// writes inside the same wall-clock millisecond would report identical
	// TimeOfChange values, making the exact-T assertion for the earlier version
	// ambiguous (as-at that millisecond inclusively resolves to the later one).
	// This does not weaken the guard — each assertion still queries a version's
	// own exact reported timestamp, which is what exposes a strict-`<` bound.
	time.Sleep(2 * time.Millisecond)
	if err := c.UpdateEntityData(t, id, `{"k":2}`); err != nil {
		t.Fatalf("update k=2: %v", err)
	}
	t2 := pitbLatestChangeTime(t, c, id)

	time.Sleep(2 * time.Millisecond)
	if err := c.UpdateEntityData(t, id, `{"k":3}`); err != nil {
		t.Fatalf("update k=3: %v", err)
	}
	t3 := pitbLatestChangeTime(t, c, id)

	// Precondition: the spacing above must yield strictly increasing version
	// timestamps on every supported backend. A violation means the backend's
	// timestamp resolution is coarser than the spacing — surface it clearly
	// rather than as a confusing exact-T failure below.
	if !t1.Before(t2) || !t2.Before(t3) {
		t.Fatalf("version timestamps not strictly increasing: t1=%s t2=%s t3=%s",
			t1.Format(time.RFC3339Nano), t2.Format(time.RFC3339Nano), t3.Format(time.RFC3339Nano))
	}

	// As-at exactly each version's write time returns that version.
	pitbAssertKAt(t, c, id, t1, 1)
	pitbAssertKAt(t, c, id, t2, 2)
	pitbAssertKAt(t, c, id, t3, 3)
}

// pitbLatestChangeTime returns the most recent TimeOfChange for the entity —
// the timestamp of the version just written.
func pitbLatestChangeTime(t *testing.T, c *client.Client, id uuid.UUID) time.Time {
	t.Helper()
	changes, err := c.GetEntityChanges(t, id)
	if err != nil {
		t.Fatalf("GetEntityChanges: %v", err)
	}
	if len(changes) == 0 {
		t.Fatal("GetEntityChanges returned no entries")
	}
	latest := changes[0].TimeOfChange
	for _, ch := range changes[1:] {
		if ch.TimeOfChange.After(latest) {
			latest = ch.TimeOfChange
		}
	}
	return latest
}

// pitbAssertKAt queries the entity at the exact timestamp at and asserts that
// the returned data.k equals wantK. A failure indicates the backend is not
// honouring the inclusive <= boundary for point-in-time reads.
func pitbAssertKAt(t *testing.T, c *client.Client, id uuid.UUID, at time.Time, wantK float64) {
	t.Helper()
	got, err := c.GetEntityAt(t, id, at)
	if err != nil {
		t.Fatalf("GetEntityAt(%s): %v", at.Format(time.RFC3339Nano), err)
	}
	if got.Data["k"] != wantK {
		t.Errorf("GetEntityAt(%s) k=%v, want %v (exact-T inclusivity violated)",
			at.Format(time.RFC3339Nano), got.Data["k"], wantK)
	}
}
