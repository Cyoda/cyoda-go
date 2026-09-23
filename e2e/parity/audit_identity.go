package parity

import (
	"bytes"
	"fmt"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// audit_identity.go — cross-backend parity scenarios for audit event
// identity, order and cursor pagination.
//
// These prove, identically on memory / sqlite / postgres, the three
// properties the audit endpoint promises:
//
//  1. Identity: an EntityChange event's identity is (entityId, version); a
//     StateMachine event's identity is its store-assigned eventId. Two saves
//     of one entity inside a single transaction produce two distinct
//     EntityChange events sharing that transaction's id, and every
//     StateMachine event in the trail carries a distinct eventId that is
//     stable across repeated GETs.
//  2. Order: newest first — by instant, EntityChange before StateMachine at a
//     tied instant, then version DESC (EntityChange) or the eventId's time
//     field DESC then bytes DESC (StateMachine). This mirrors
//     internal/domain/audit.compareKeys exactly (audit_identity_test.go has
//     no local reimplementation to drift from it silently — see
//     auditKeyLess below).
//  3. Cursor: walking the trail with limit=1 and following nextCursor
//     reconstructs the same sequence as one unpaged GET.
//
// Setup reuses the compute-node callback fixture from callback_txjoin.go: a
// primary entity's SYNC processor (cb-ifmatch-update) creates a secondary
// entity and then updates it in-place with an If-Match precondition, all
// inside the primary transition's transaction T. The secondary therefore
// carries two EntityChange events (CREATE then UPDATE, v1 then v2) sharing
// one transactionId, plus the StateMachine events from its own auto
// transition (NONE -> STORED) — an ideal, backend-agnostic trail to prove
// identity, order and cursor pagination against.

// auditOrderKey is the sort key of one decoded audit event, extracted
// on the parity-client side. Its comparison (auditKeyLess) mirrors
// internal/domain/audit.compareKeys(): utcTime DESC, EntityChange before
// StateMachine at a tied instant, then version DESC (EntityChange) or the
// eventId's time field DESC then bytes DESC (StateMachine).
type auditOrderKey struct {
	at      time.Time
	kind    string
	version int64
	eventID uuid.UUID
}

// auditKeyOf decodes ev's subtype-specific identity field (version for
// EntityChange, eventId for StateMachine) and builds its order key. Fails
// the test on a decode error or an unparseable eventId — both indicate the
// server violated the audit contract, not a scenario bug.
func auditKeyOf(t *testing.T, ev client.AuditEvent) auditOrderKey {
	t.Helper()
	k := auditOrderKey{at: ev.UtcTime, kind: ev.AuditEventType}
	switch ev.AuditEventType {
	case "EntityChange":
		ec, err := ev.AsEntityChange()
		if err != nil {
			t.Fatalf("decode EntityChange for order check: %v", err)
		}
		k.version = ec.Version
	case "StateMachine":
		sm, err := ev.AsStateMachine()
		if err != nil {
			t.Fatalf("decode StateMachine for order check: %v", err)
		}
		id, err := uuid.Parse(sm.EventID)
		if err != nil {
			t.Fatalf("parse StateMachine eventId %q: %v", sm.EventID, err)
		}
		k.eventID = id
	default:
		t.Fatalf("audit event has unexpected auditEventType %q; order check only covers EntityChange/StateMachine", ev.AuditEventType)
	}
	return k
}

// auditKeyLess reports whether a must sort strictly before b under the
// documented order. See internal/domain/audit.compareKeys — this is the
// same total order, computed from the wire-decoded fields instead of the
// server's internal eventKey.
func auditKeyLess(a, b auditOrderKey) bool {
	if !a.at.Equal(b.at) {
		return a.at.After(b.at)
	}
	if a.kind != b.kind {
		return a.kind < b.kind // "EntityChange" < "StateMachine"
	}
	if a.kind == "EntityChange" {
		return a.version > b.version
	}
	if aTime, bTime := a.eventID.Time(), b.eventID.Time(); aTime != bTime {
		return aTime > bTime
	}
	return bytes.Compare(a.eventID[:], b.eventID[:]) > 0
}

// assertAuditOrder fails the test unless every adjacent pair of items is
// strictly ordered per auditKeyLess.
func assertAuditOrder(t *testing.T, items []client.AuditEvent) {
	t.Helper()
	if len(items) < 2 {
		return
	}
	keys := make([]auditOrderKey, len(items))
	for i, ev := range items {
		keys[i] = auditKeyOf(t, ev)
	}
	for i := 0; i+1 < len(keys); i++ {
		if !auditKeyLess(keys[i], keys[i+1]) {
			t.Fatalf("audit order violated at index %d->%d: item %d (%s) is not strictly before item %d (%s)",
				i, i+1, i, auditIdentity(t, items[i]), i+1, auditIdentity(t, items[i+1]))
		}
	}
}

// auditIdentity renders ev's identity as a comparable string: the version
// for an EntityChange event, the eventId for a StateMachine event. Used to
// compare two fetches of the same trail (same identities, same order)
// without relying on Go struct equality over decoded subtypes.
func auditIdentity(t *testing.T, ev client.AuditEvent) string {
	t.Helper()
	switch ev.AuditEventType {
	case "EntityChange":
		ec, err := ev.AsEntityChange()
		if err != nil {
			t.Fatalf("decode EntityChange identity: %v", err)
		}
		return fmt.Sprintf("EntityChange:%d", ec.Version)
	case "StateMachine":
		sm, err := ev.AsStateMachine()
		if err != nil {
			t.Fatalf("decode StateMachine identity: %v", err)
		}
		return fmt.Sprintf("StateMachine:%s", sm.EventID)
	default:
		return ev.AuditEventType
	}
}

// auditIdentities maps auditIdentity over items, preserving order.
func auditIdentities(t *testing.T, items []client.AuditEvent) []string {
	t.Helper()
	out := make([]string, len(items))
	for i, ev := range items {
		out[i] = auditIdentity(t, ev)
	}
	return out
}

// auditSetupJoinedSaves builds the shared fixture: a secondary model with a
// trivial auto-transitioning workflow, and a primary model whose SYNC
// cb-ifmatch-update processor creates a secondary and updates it in-place,
// inside the primary transition's transaction. Model/workflow names are
// namespaced by the caller-supplied tag so concurrently-registered scenarios
// never collide (ComputeTenant's models persist for the whole package run).
// Returns the created primary's id and the parsed secondary id.
func auditSetupJoinedSaves(t *testing.T, c *client.Client, tag string) (primaryID, secondaryID uuid.UUID) {
	t.Helper()

	secondary := "audit-" + tag + "-secondary"
	primary := "audit-" + tag + "-primary"
	marker := "audit-" + tag + "-marker"

	cbSetupModel(t, c, secondary, cbSampleSecondary, cbSecondaryWorkflow)
	cbSetupModel(t, c, primary, cbSampleIfMatchUpdate,
		cbPrimaryProcWorkflow("audit-"+tag+"-wf", "cb-ifmatch-update", "SYNC", cbContext(secondary, marker)))

	primaryID, err := c.CreateEntity(t, primary, 1, `{"name":"parent","amount":100,"status":"new"}`)
	if err != nil {
		t.Fatalf("primary create: %v", err)
	}
	prim, err := c.GetEntity(t, primaryID)
	if err != nil {
		t.Fatalf("GetEntity primary: %v", err)
	}
	if ok, _ := prim.Data["ifMatchOK"].(bool); !ok {
		t.Fatalf("callback If-Match update did not succeed against the in-T version: ifMatchStatus=%v (fixture broken)", prim.Data["ifMatchStatus"])
	}
	secIDStr, _ := prim.Data["secondaryId"].(string)
	if secIDStr == "" {
		t.Fatalf("primary data missing secondaryId: data=%+v", prim.Data)
	}
	secondaryID, err = uuid.Parse(secIDStr)
	if err != nil {
		t.Fatalf("parse secondaryId %q: %v", secIDStr, err)
	}
	return primaryID, secondaryID
}

// RunAuditIdentityJoinedSaves proves audit event identity across two saves
// of one entity inside a single transaction: the secondary from
// auditSetupJoinedSaves carries exactly two EntityChange events (create then
// in-place update) sharing one transactionId with distinct versions, v2
// before v1 — plus at least one StateMachine event from that same
// transaction, each with a distinct, store-assigned eventId. A second GET
// returns the identical set of identities in the identical order, and every
// adjacent pair obeys the documented order rule.
func RunAuditIdentityJoinedSaves(t *testing.T, fixture BackendFixture) {
	tenant := fixture.ComputeTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	_, secID := auditSetupJoinedSaves(t, c, "identity")

	resp1, err := c.GetAuditEvents(t, secID)
	if err != nil {
		t.Fatalf("GetAuditEvents: %v", err)
	}
	assertAuditOrder(t, resp1.Items)

	var ecs []*client.EntityChangeAuditEvent
	smEventIDs := make(map[string]bool)
	for _, ev := range resp1.Items {
		switch ev.AuditEventType {
		case "EntityChange":
			ec, err := ev.AsEntityChange()
			if err != nil {
				t.Fatalf("AsEntityChange: %v", err)
			}
			ecs = append(ecs, ec)
		case "StateMachine":
			sm, err := ev.AsStateMachine()
			if err != nil {
				t.Fatalf("AsStateMachine: %v", err)
			}
			if sm.EventID == "" {
				t.Fatalf("StateMachine event has empty eventId: %+v", sm)
			}
			if smEventIDs[sm.EventID] {
				t.Fatalf("StateMachine events share eventId %q — identity must be distinct per event", sm.EventID)
			}
			smEventIDs[sm.EventID] = true
		}
	}

	// Non-vacuity: exactly two EntityChange events (create + in-T update)
	// sharing one transactionId with distinct versions, v2 before v1 — and
	// at least one StateMachine event from that same transaction.
	if len(ecs) != 2 {
		t.Fatalf("secondary EntityChange event count = %d; want exactly 2 (create + in-T update): %v", len(ecs), auditIdentities(t, resp1.Items))
	}
	if ecs[0].TransactionID == "" {
		t.Fatalf("EntityChange events have empty transactionId")
	}
	if ecs[0].TransactionID != ecs[1].TransactionID {
		t.Fatalf("EntityChange events do not share one transactionId: %q vs %q", ecs[0].TransactionID, ecs[1].TransactionID)
	}
	if ecs[0].Version == ecs[1].Version {
		t.Fatalf("EntityChange events have identical version %d; want distinct versions for two saves", ecs[0].Version)
	}
	if ecs[0].Version <= ecs[1].Version {
		t.Fatalf("EntityChange events not ordered v2-before-v1: got version %d before version %d", ecs[0].Version, ecs[1].Version)
	}
	if len(smEventIDs) < 1 {
		t.Fatalf("expected >= 1 StateMachine event in the secondary's transaction, got 0 — the create's auto-transition must have emitted one")
	}

	// A second GET returns the identical eventIds/versions in the identical
	// order — identity and order are stable, not recomputed per request.
	resp2, err := c.GetAuditEvents(t, secID)
	if err != nil {
		t.Fatalf("second GetAuditEvents: %v", err)
	}
	got := auditIdentities(t, resp2.Items)
	want := auditIdentities(t, resp1.Items)
	if !slices.Equal(got, want) {
		t.Fatalf("second GET returned a different identity/order set:\n got  %v\n want %v", got, want)
	}
}

// RunAuditCursorWalkOverTie proves the cursor contract holds even across the
// tied StateMachine events auditSetupJoinedSaves produces (same instant,
// ordered only by the eventId's time field and bytes): walking the
// secondary's trail with limit=1, following nextCursor until hasNext is
// false, reconstructs exactly the same sequence as one unpaged GET.
func RunAuditCursorWalkOverTie(t *testing.T, fixture BackendFixture) {
	tenant := fixture.ComputeTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	_, secID := auditSetupJoinedSaves(t, c, "cursor")

	unpaged, err := c.GetAuditEventsPage(t, secID, url.Values{"limit": {"1000"}})
	if err != nil {
		t.Fatalf("GetAuditEventsPage (unpaged): %v", err)
	}
	if unpaged.Pagination.HasNext {
		t.Fatalf("unpaged GET (limit=1000) reports hasNext=true — trail is unexpectedly >= 1000 events")
	}
	if len(unpaged.Items) < 3 {
		t.Fatalf("unpaged trail has %d events; want >= 3 (2 EntityChange + >=1 StateMachine) for a non-trivial cursor walk", len(unpaged.Items))
	}

	var walked []client.AuditEvent
	cursor := ""
	maxIter := len(unpaged.Items) + 2
	for i := 0; ; i++ {
		if i >= maxIter {
			t.Fatalf("cursor walk exceeded %d iterations without hasNext=false (possible infinite loop)", maxIter)
		}
		q := url.Values{"limit": {"1"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		page, err := c.GetAuditEventsPage(t, secID, q)
		if err != nil {
			t.Fatalf("GetAuditEventsPage (walk step %d): %v", i, err)
		}
		if len(page.Items) != 1 {
			t.Fatalf("walk step %d: page has %d items; want exactly 1 for limit=1 (unless the trail is exhausted)", i, len(page.Items))
		}
		walked = append(walked, page.Items...)
		if !page.Pagination.HasNext {
			break
		}
		if page.Pagination.NextCursor == "" {
			t.Fatalf("walk step %d: hasNext=true but nextCursor is empty", i)
		}
		cursor = page.Pagination.NextCursor
	}

	got := auditIdentities(t, walked)
	want := auditIdentities(t, unpaged.Items)
	if !slices.Equal(got, want) {
		t.Fatalf("cursor walk (limit=1) does not reconstruct the unpaged list:\n got  %v\n want %v", got, want)
	}
}

// RunAuditFinishedEventIDMatchesSearch proves the workflow-finished lookup
// and the audit search agree on one event's identity: the eventId
// GetWorkflowFinished returns for the primary's transaction equals the
// eventId of that same transaction's STATE_MACHINE_FINISH event as found by
// searching the primary's audit trail directly.
//
// Uses the primary rather than the secondary: the secondary's trail carries
// TWO STATE_MACHINE_FINISH events in the shared transaction — one from its
// own auto-transition cascade (NONE -> STORED) and one from the callback's
// later loopback update (internal/domain/workflow/engine.go emits a
// START/FINISH pair around a loopback save too) — so "the" finished event is
// ambiguous there. The primary's transition (NONE -> ACTIVE, no loopback on
// itself) produces exactly one, which this scenario asserts as a
// precondition rather than silently picking the first match.
func RunAuditFinishedEventIDMatchesSearch(t *testing.T, fixture BackendFixture) {
	tenant := fixture.ComputeTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	primaryID, _ := auditSetupJoinedSaves(t, c, "finished")

	primAudit, err := c.GetAuditEvents(t, primaryID)
	if err != nil {
		t.Fatalf("GetAuditEvents: %v", err)
	}

	var txID string
	for _, ev := range primAudit.Items {
		if ev.TransactionID != "" {
			txID = ev.TransactionID
			break
		}
	}
	if txID == "" {
		t.Fatalf("primary's audit trail has no event with a transactionId: %v", auditIdentities(t, primAudit.Items))
	}

	var finishEventIDs []string
	for _, ev := range primAudit.Items {
		if ev.AuditEventType != "StateMachine" || ev.TransactionID != txID {
			continue
		}
		sm, err := ev.AsStateMachine()
		if err != nil {
			t.Fatalf("AsStateMachine: %v", err)
		}
		if sm.EventType == "STATE_MACHINE_FINISH" {
			finishEventIDs = append(finishEventIDs, sm.EventID)
		}
	}
	if len(finishEventIDs) != 1 {
		t.Fatalf("primary's transaction %s has %d STATE_MACHINE_FINISH events; want exactly 1 (unambiguous fixture precondition): %v",
			txID, len(finishEventIDs), auditIdentities(t, primAudit.Items))
	}
	wantEventID := finishEventIDs[0]

	status, result, err := c.GetWorkflowFinished(t, primaryID, txID)
	if err != nil {
		t.Fatalf("GetWorkflowFinished(%s, %s): status %d, err: %v", primaryID, txID, status, err)
	}
	if status != 200 {
		t.Fatalf("GetWorkflowFinished(%s, %s) = %d; want 200", primaryID, txID, status)
	}
	gotEventID, _ := result["eventId"].(string)
	if gotEventID == "" {
		t.Fatalf("GetWorkflowFinished response missing eventId: %+v", result)
	}
	if gotEventID != wantEventID {
		t.Fatalf("GetWorkflowFinished eventId %q != search's STATE_MACHINE_FINISH eventId %q for the same transaction", gotEventID, wantEventID)
	}
}

// RunAuditFinishedEventIsLatestOfTransaction proves the finished endpoint's
// pick among *multiple* STATE_MACHINE_FINISH events of one transaction: the
// secondary from auditSetupJoinedSaves carries two — one from its own
// auto-transition cascade (NONE -> STORED) and one from the callback's later
// loopback update (internal/domain/workflow/engine.go emits a START/FINISH
// pair around a loopback save too) — both sharing the transaction the
// callback ran in. The endpoint must return the one that sorts first in the
// documented total order (newest instant, then the eventId's time field
// DESC, then bytes DESC), which is also how the audit search already orders
// the trail — so the wanted event is the first STATE_MACHINE_FINISH
// encountered walking the search response for that transaction.
func RunAuditFinishedEventIsLatestOfTransaction(t *testing.T, fixture BackendFixture) {
	tenant := fixture.ComputeTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	_, secID := auditSetupJoinedSaves(t, c, "finished-latest")

	secAudit, err := c.GetAuditEvents(t, secID)
	if err != nil {
		t.Fatalf("GetAuditEvents: %v", err)
	}
	assertAuditOrder(t, secAudit.Items)

	var txID string
	for _, ev := range secAudit.Items {
		if ev.TransactionID != "" {
			txID = ev.TransactionID
			break
		}
	}
	if txID == "" {
		t.Fatalf("secondary's audit trail has no event with a transactionId: %v", auditIdentities(t, secAudit.Items))
	}

	var finishEventIDs []string
	for _, ev := range secAudit.Items {
		if ev.AuditEventType != "StateMachine" || ev.TransactionID != txID {
			continue
		}
		sm, err := ev.AsStateMachine()
		if err != nil {
			t.Fatalf("AsStateMachine: %v", err)
		}
		if sm.EventType == "STATE_MACHINE_FINISH" {
			finishEventIDs = append(finishEventIDs, sm.EventID)
		}
	}
	if len(finishEventIDs) != 2 {
		t.Fatalf("secondary's transaction %s has %d STATE_MACHINE_FINISH events; want exactly 2 (own cascade + callback loopback — unambiguous fixture precondition): %v",
			txID, len(finishEventIDs), auditIdentities(t, secAudit.Items))
	}
	// secAudit.Items is already ordered newest-first (asserted above), so the
	// first STATE_MACHINE_FINISH encountered while walking it is the one
	// that sorts first under compareKeys — the latest of the two.
	wantEventID := finishEventIDs[0]

	status, result, err := c.GetWorkflowFinished(t, secID, txID)
	if err != nil {
		t.Fatalf("GetWorkflowFinished(%s, %s): status %d, err: %v", secID, txID, status, err)
	}
	if status != 200 {
		t.Fatalf("GetWorkflowFinished(%s, %s) = %d; want 200", secID, txID, status)
	}
	gotEventID, _ := result["eventId"].(string)
	if gotEventID == "" {
		t.Fatalf("GetWorkflowFinished response missing eventId: %+v", result)
	}
	if gotEventID != wantEventID {
		t.Fatalf("GetWorkflowFinished eventId %q != the latest of the transaction's two STATE_MACHINE_FINISH events %q (search order: %v)",
			gotEventID, wantEventID, finishEventIDs)
	}
}
