package parity

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// attribution.go — cross-backend parity scenarios for follow-on-action
// attribution (spec docs/superpowers/specs/2026-07-22-attribute-followon-actions-design.md
// §4/§5/§7/§9). These pin the BACKEND-AGNOSTIC attribution contract: the
// {attributed principal, executor} recorded on a change-history entry must be
// IDENTICAL on memory / sqlite / postgres (and any out-of-tree backend running
// the parity suite). The 3-way-divergence delete bug — different backends
// stamping a tombstone with different principals — is precisely what these
// guard against staying fixed.
//
// The parity tenant JWT carries a `scopes` claim and no `user_roles` key, so
// the validator resolves it to a SERVICE-kind principal (auth/validator.go).
// A direct client write therefore records attributedKind="service" and an
// executedBy of the same service principal. Scenarios assert relationships
// between recorded fields (executor == origin for a direct write, executor
// distinct from origin for a joined cascade, executor == system for a
// scheduled fire) rather than hard-coding fixture-internal principal ids —
// so they stay robust while still proving the cross-backend invariant.

// findChangeByType returns the newest change-history entry of the given
// canonical changeType (CREATE / UPDATE / DELETE), or nil. The /changes
// endpoint returns entries newest-first, so the first match is the newest.
func findChangeByType(changes []client.EntityChangeMeta, changeType string) *client.EntityChangeMeta {
	for i := range changes {
		if changes[i].ChangeType == changeType {
			return &changes[i]
		}
	}
	return nil
}

// awaitEntityStateAttr polls GetEntity until Meta.State == wantState, failing
// the test once timeout elapses. Local to attribution.go (the shared base
// package has no such helper; the scheduledtransition extension keeps its own).
func awaitEntityStateAttr(t *testing.T, c *client.Client, id uuid.UUID, wantState string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastState string
	for {
		got, err := c.GetEntity(t, id)
		if err != nil {
			t.Fatalf("GetEntity while awaiting state %q: %v", wantState, err)
		}
		lastState = got.Meta.State
		if lastState == wantState {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for entity %s to reach state %q; last state %q",
				timeout, id, wantState, lastState)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// RunAttributionTombstoneUniformity pins the tombstone-attribution contract for
// a direct (non-transactional) delete: the DELETE change records the SAME
// attributed principal as the entity's own CREATE, with the executor equal to
// the stager (the deleter) — identically on every backend. This is the guard
// that the delete-attribution 3-way divergence (some backends stamping the
// tombstone with the wrong principal, or nothing) stays fixed.
func RunAttributionTombstoneUniformity(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "attr-tombstone-uniformity"
	const modelVersion = 1
	setupSimpleWorkflow(t, c, modelName, modelVersion)

	entityID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"victim","amount":5,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	// Capture the create attribution: the tenant is a service principal, so
	// attributed = executor = that principal, attributedKind = service.
	created := findChangeByType(mustChanges(t, c, entityID), "CREATE")
	if created == nil {
		t.Fatalf("no CREATE change recorded for %s", entityID)
	}
	if created.User == "" {
		t.Fatalf("CREATE change has empty user (attributed principal): %+v", created)
	}
	if created.AttributedKind != "service" {
		t.Errorf("CREATE attributedKind = %q; want service (parity tenant is a service principal)", created.AttributedKind)
	}
	if created.ExecutedBy == nil {
		t.Fatalf("CREATE change missing executedBy: %+v", created)
	}
	if created.ExecutedBy.ID != created.User || created.ExecutedBy.Kind != "service" {
		t.Errorf("CREATE executor = %+v; want {id:%q, kind:service} (direct write: executor == attributed origin)",
			created.ExecutedBy, created.User)
	}

	if err := c.DeleteEntity(t, entityID); err != nil {
		t.Fatalf("DeleteEntity: %v", err)
	}

	// The tombstone must attribute to the SAME principal as the create, and the
	// executor must be the stager (the deleter) — here the same tenant. The
	// attributed principal is NOT dropped and NOT replaced by a system/empty
	// value on any backend.
	deleted := findChangeByType(mustChanges(t, c, entityID), "DELETE")
	if deleted == nil {
		t.Fatalf("no DELETE change (tombstone) recorded for %s; changes=%+v", entityID, mustChanges(t, c, entityID))
	}
	if deleted.User != created.User {
		t.Errorf("tombstone attributed user = %q; want %q (same principal as create — no divergence)",
			deleted.User, created.User)
	}
	if deleted.AttributedKind != "service" {
		t.Errorf("tombstone attributedKind = %q; want service", deleted.AttributedKind)
	}
	if deleted.ExecutedBy == nil {
		t.Fatalf("tombstone missing executedBy (the stager): %+v", deleted)
	}
	if deleted.ExecutedBy.ID != created.User || deleted.ExecutedBy.Kind != "service" {
		t.Errorf("tombstone executor = %+v; want {id:%q, kind:service} (executor == stager)",
			deleted.ExecutedBy, created.User)
	}
}

// RunAttributionExecutorRoundTrip pins the executor round-trip on change
// history: a create followed by a data update both surface user +
// attributedKind + executedBy identically across backends, and every entry's
// executor equals its attributed origin for a direct write.
func RunAttributionExecutorRoundTrip(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "attr-executor-roundtrip"
	const modelVersion = 1
	setupSimpleWorkflow(t, c, modelName, modelVersion)

	entityID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"rt","amount":1,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	if err := c.UpdateEntityData(t, entityID, `{"name":"rt","amount":2,"status":"new"}`); err != nil {
		t.Fatalf("UpdateEntityData: %v", err)
	}

	changes := mustChanges(t, c, entityID)
	create := findChangeByType(changes, "CREATE")
	update := findChangeByType(changes, "UPDATE")
	if create == nil || update == nil {
		t.Fatalf("expected both CREATE and UPDATE changes; got %+v", changes)
	}

	for _, tc := range []struct {
		label string
		entry *client.EntityChangeMeta
	}{
		{"CREATE", create},
		{"UPDATE", update},
	} {
		if tc.entry.User == "" {
			t.Errorf("%s: empty attributed user: %+v", tc.label, tc.entry)
		}
		if tc.entry.AttributedKind != "service" {
			t.Errorf("%s: attributedKind = %q; want service", tc.label, tc.entry.AttributedKind)
		}
		if tc.entry.ExecutedBy == nil {
			t.Fatalf("%s: missing executedBy: %+v", tc.label, tc.entry)
			continue
		}
		if tc.entry.ExecutedBy.Kind != "service" {
			t.Errorf("%s: executor kind = %q; want service", tc.label, tc.entry.ExecutedBy.Kind)
		}
		if tc.entry.ExecutedBy.ID != tc.entry.User {
			t.Errorf("%s: executor id = %q; want %q (direct write: executor == attributed origin)",
				tc.label, tc.entry.ExecutedBy.ID, tc.entry.User)
		}
	}

	// The two direct writes share one principal — the update did not silently
	// re-attribute to a different or empty actor on any backend.
	if create.User != update.User {
		t.Errorf("CREATE user %q != UPDATE user %q; want the same principal across versions",
			create.User, update.User)
	}
}

// RunAttributionScheduledArmedByFire pins scheduled-fire attribution across
// backends: a task armed by the tenant (ArmedBy = the arming origin) fires
// later via the platform scheduler; the fired anchor attributes to that arming
// principal, executed by the SYSTEM principal {system, system} — never the
// literal "scheduler", and never re-attributed to a different actor. Same on
// every backend.
func RunAttributionScheduledArmedByFire(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "attr-scheduled-fire"
	const modelVersion = 1
	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "attr-scheduled-fire-wf", "initialState": "OPEN", "active": true,
			"states": {
				"OPEN":   {"transitions": [{"name": "AutoClose", "next": "CLOSED", "manual": false,
					"schedule": {"delayMs": 300}}]},
				"CLOSED": {}
			}
		}]
	}`
	if err := c.ImportModel(t, modelName, modelVersion, `{"k":1}`); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := c.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	if err := c.ImportWorkflow(t, modelName, modelVersion, wf); err != nil {
		t.Fatalf("ImportWorkflow: %v", err)
	}

	entityID, err := c.CreateEntity(t, modelName, modelVersion, `{"k":1}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	// The arming principal is the entity's creator (the origin in effect at arm
	// time). The creator's version has a SERVICE executor (the tenant staged
	// it); capture that principal id.
	armingPrincipal := changePrincipalOf(t, mustChanges(t, c, entityID), "service", "creator (service executor)")

	// Wait for the platform scheduler to really fire the transition.
	awaitEntityStateAttr(t, c, entityID, "CLOSED", 15*time.Second)

	// The fired anchor is uniquely identified by its SYSTEM executor — the
	// platform, not a user or service principal, staged the fire. (Keying on
	// the executor rather than the CREATE/UPDATE changeType keeps this robust:
	// backends label the fired version's changeType differently, but the
	// attribution contract — attributed = ArmedBy origin, executor = system —
	// is what must hold identically everywhere.)
	changes := mustChanges(t, c, entityID)
	var fired *client.EntityChangeMeta
	for i := range changes {
		if changes[i].ExecutedBy != nil && changes[i].ExecutedBy.Kind == "system" {
			if fired != nil {
				t.Fatalf("more than one system-executor change; want exactly the fired anchor; changes=%+v", changes)
			}
			fired = &changes[i]
		}
	}
	if fired == nil {
		t.Fatalf("entity reached CLOSED but no system-executor change (fired anchor) recorded; changes=%+v", changes)
	}
	if fired.User != armingPrincipal {
		t.Errorf("fired anchor attributed user = %q; want %q (the arming principal / durable ArmedBy)",
			fired.User, armingPrincipal)
	}
	if fired.AttributedKind != "service" {
		t.Errorf("fired anchor attributedKind = %q; want service (arming principal is a service principal)",
			fired.AttributedKind)
	}
	if fired.ExecutedBy.ID != "system" {
		t.Errorf("fired anchor executor = %+v; want {id:system, kind:system} (the platform fired it, not a user/service)",
			fired.ExecutedBy)
	}
}

// changePrincipalOf returns the attributed User of the newest change entry
// whose executor is of the given kind, failing the test if none is found.
func changePrincipalOf(t *testing.T, changes []client.EntityChangeMeta, execKind, label string) string {
	t.Helper()
	for i := range changes {
		if changes[i].ExecutedBy != nil && changes[i].ExecutedBy.Kind == execKind {
			return changes[i].User
		}
	}
	t.Fatalf("no change entry with a %s executor (%s); changes=%+v", execKind, label, changes)
	return ""
}

// RunAttributionCascadeJoinedWrite pins cascade attribution across backends
// using the callback-capable compute-test-client: a SYNC processor whose
// callback CREATES a secondary entity inside the originating transition's
// transaction T (joined) records that secondary as attributed to the CHAIN
// ORIGIN (the primary's creator), executed by the distinct service principal
// of the compute member that staged the write. The origin propagates across
// the callback boundary identically on every backend; the executor is a
// different principal from the attributed origin — proving cascade writes
// carry the causal origin, not the immediate executor.
func RunAttributionCascadeJoinedWrite(t *testing.T, fixture BackendFixture) {
	tenant := fixture.ComputeTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const secondary = "attr-cascade-secondary"
	const primary = "attr-cascade-primary"
	const marker = "attr-cascade-mark"

	cbSetupModel(t, c, secondary, `{"name":"child","amount":1,"status":"new"}`, cbSecondaryWorkflow)
	cbSetupModel(t, c, primary, `{"name":"Test","amount":10,"status":"new"}`,
		cbPrimaryProcWorkflow("attr-cascade-wf", "cb-create-secondary", "SYNC", cbContext(secondary, marker)))

	primaryID, err := c.CreateEntity(t, primary, 1, `{"name":"parent","amount":100,"status":"new"}`)
	if err != nil {
		t.Fatalf("primary create: %v", err)
	}
	prim, err := c.GetEntity(t, primaryID)
	if err != nil {
		t.Fatalf("GetEntity primary: %v", err)
	}
	if prim.Meta.State != "ACTIVE" {
		t.Fatalf("primary state = %q; want ACTIVE (processor must have run)", prim.Meta.State)
	}
	secIDStr, _ := prim.Data["secondaryId"].(string)
	if secIDStr == "" {
		t.Fatalf("primary data missing secondaryId (callback did not create secondary): data=%+v", prim.Data)
	}
	secID, err := uuid.Parse(secIDStr)
	if err != nil {
		t.Fatalf("parse secondaryId %q: %v", secIDStr, err)
	}

	// The origin is the primary's creator (the tenant that issued the create).
	primCreate := findChangeByType(mustChanges(t, c, primaryID), "CREATE")
	if primCreate == nil || primCreate.User == "" {
		t.Fatalf("no usable CREATE change for primary %s: %+v", primaryID, primCreate)
	}
	origin := primCreate.User

	// The joined secondary write attributes to the chain ORIGIN, executed by a
	// DIFFERENT service principal (the compute member).
	secCreate := findChangeByType(mustChanges(t, c, secID), "CREATE")
	if secCreate == nil {
		t.Fatalf("no CREATE change for cascade secondary %s; changes=%+v", secID, mustChanges(t, c, secID))
	}
	if secCreate.User != origin {
		t.Errorf("cascade secondary attributed user = %q; want %q (chain origin propagated across the callback boundary)",
			secCreate.User, origin)
	}
	if secCreate.AttributedKind != "service" {
		t.Errorf("cascade secondary attributedKind = %q; want service", secCreate.AttributedKind)
	}
	if secCreate.ExecutedBy == nil {
		t.Fatalf("cascade secondary missing executedBy: %+v", secCreate)
	}
	if secCreate.ExecutedBy.Kind != "service" {
		t.Errorf("cascade secondary executor kind = %q; want service (the compute member)", secCreate.ExecutedBy.Kind)
	}
	if secCreate.ExecutedBy.ID == origin {
		t.Errorf("cascade secondary executor id = %q; want a DIFFERENT principal from the origin %q (executor is the staging member, not the origin)",
			secCreate.ExecutedBy.ID, origin)
	}
}

// mustChanges fetches the change history for entityID or fails the test.
func mustChanges(t *testing.T, c *client.Client, entityID uuid.UUID) []client.EntityChangeMeta {
	t.Helper()
	changes, err := c.GetEntityChanges(t, entityID)
	if err != nil {
		t.Fatalf("GetEntityChanges(%s): %v", entityID, err)
	}
	return changes
}
