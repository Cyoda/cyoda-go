package client

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return raw
}

func TestAuditEvent_StateMachine_Decode(t *testing.T) {
	raw := loadFixture(t, "audit_state_machine_event.json")
	var ev AuditEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatalf("decode base: %v", err)
	}
	if ev.AuditEventType != "StateMachine" {
		t.Errorf("AuditEventType: got %q, want \"StateMachine\"", ev.AuditEventType)
	}
	if ev.MicrosTime == 0 {
		t.Error("MicrosTime is zero (canonical-required)")
	}
	if len(ev.Raw) == 0 {
		t.Error("Raw is empty after Unmarshal — UnmarshalJSON did not capture")
	}

	sm, err := ev.AsStateMachine()
	if err != nil {
		t.Fatalf("AsStateMachine: %v", err)
	}
	if sm.State != "CREATED" {
		t.Errorf("State: got %q, want \"CREATED\"", sm.State)
	}
	if sm.EventType != "FINISHED" {
		t.Errorf("EventType: got %q, want \"FINISHED\"", sm.EventType)
	}
	if len(sm.Data) == 0 {
		t.Error("Data is empty (fixture has data block)")
	}
	// Base fields should also be populated via embedding.
	if sm.AuditEvent.UtcTime.IsZero() {
		t.Error("Embedded AuditEvent.UtcTime is zero")
	}
}

func TestAuditEvent_EntityChange_Decode(t *testing.T) {
	raw := loadFixture(t, "audit_entity_change_event.json")
	var ev AuditEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatalf("decode base: %v", err)
	}
	ec, err := ev.AsEntityChange()
	if err != nil {
		t.Fatalf("AsEntityChange: %v", err)
	}
	if ec.ChangeType != "CREATE" {
		t.Errorf("ChangeType: got %q, want \"CREATE\"", ec.ChangeType)
	}
	if len(ec.Changes) == 0 {
		t.Error("Changes is empty (fixture has after-block)")
	}
}

func TestAuditEvent_System_Decode(t *testing.T) {
	raw := loadFixture(t, "audit_system_event.json")
	var ev AuditEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatalf("decode base: %v", err)
	}
	sys, err := ev.AsSystem()
	if err != nil {
		t.Fatalf("AsSystem: %v", err)
	}
	if sys.QueueName != "TRANSACTION_EXEC_1_PHASE_VERSION_CHECK" {
		t.Errorf("QueueName: got %q", sys.QueueName)
	}
	if sys.ShardID != "6" {
		t.Errorf("ShardID: got %q, want \"6\"", sys.ShardID)
	}
	if sys.Status != "PROCESSED" {
		t.Errorf("Status: got %q, want \"PROCESSED\"", sys.Status)
	}
}

func TestAuditEvent_WrongType_AsXReturnsError(t *testing.T) {
	raw := loadFixture(t, "audit_state_machine_event.json")
	var ev AuditEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatalf("decode base: %v", err)
	}
	if _, err := ev.AsEntityChange(); err == nil {
		t.Error("AsEntityChange on a StateMachine event should error")
	}
	if _, err := ev.AsSystem(); err == nil {
		t.Error("AsSystem on a StateMachine event should error")
	}
}

func TestEntityAuditEventsResponse_RoundTrip(t *testing.T) {
	raw := loadFixture(t, "audit_events_response.json")
	var resp EntityAuditEventsResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(resp.Items))
	}

	// Each item must dispatch to the right subtype.
	gotTypes := make([]string, len(resp.Items))
	for i, ev := range resp.Items {
		gotTypes[i] = ev.AuditEventType
	}
	wantTypes := []string{"StateMachine", "EntityChange", "System"}
	for i, want := range wantTypes {
		if gotTypes[i] != want {
			t.Errorf("Items[%d].AuditEventType: got %q, want %q", i, gotTypes[i], want)
		}
	}

	// Pagination shows hasNext=false, nextCursor empty.
	if resp.Pagination.HasNext {
		t.Error("Pagination.HasNext: got true, want false")
	}
	if resp.Pagination.NextCursor != "" {
		t.Errorf("Pagination.NextCursor: got %q, want empty", resp.Pagination.NextCursor)
	}
}

// mutateFixtureWithBogusField loads the named fixture, splices in a
// top-level extra field with the given name and string value, and
// returns the mutated bytes. Used by the strict-mode drift tests.
func mutateFixtureWithBogusField(t *testing.T, fixtureName, fieldName, fieldValue string) []byte {
	t.Helper()
	raw := loadFixture(t, fixtureName)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode fixture %s for mutation: %v", fixtureName, err)
	}
	m[fieldName] = fieldValue
	mutated, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("re-encode mutated fixture: %v", err)
	}
	return mutated
}

// TestAuditEvent_StrictModeRejectsStateMachineDrift verifies that the
// flat-alias UnmarshalJSON pattern actually enforces DisallowUnknownFields
// when re-decoding via AsStateMachine. Splices a bogus extra field into
// a valid StateMachine fixture and asserts AsStateMachine returns an
// error mentioning the field.
//
// This is the test that proves the central design claim of audit.go: that
// the embedded-UnmarshalJSON Go gotcha (where DisallowUnknownFields does
// not propagate through embedded types' custom UnmarshalJSON methods) is
// correctly worked around by the flat-alias pattern. Without this test
// the workaround is "trust me" rather than "tested" — a future refactor
// could silently regress strict mode without any failure signal.
func TestAuditEvent_StrictModeRejectsStateMachineDrift(t *testing.T) {
	mutated := mutateFixtureWithBogusField(t, "audit_state_machine_event.json", "bogusExtraField", "drift")

	var ev AuditEvent
	if err := json.Unmarshal(mutated, &ev); err != nil {
		t.Fatalf("base decode should succeed: %v", err)
	}
	_, err := ev.AsStateMachine()
	if err == nil {
		t.Fatal("AsStateMachine must reject the unknown bogusExtraField")
	}
	if !strings.Contains(err.Error(), "bogusExtraField") {
		t.Errorf("error should name the unknown field, got: %v", err)
	}
}

// TestAuditEvent_StrictModeRejectsEntityChangeDrift — same shape, exercises
// the EntityChange flat-alias UnmarshalJSON.
func TestAuditEvent_StrictModeRejectsEntityChangeDrift(t *testing.T) {
	mutated := mutateFixtureWithBogusField(t, "audit_entity_change_event.json", "bogusExtraField", "drift")

	var ev AuditEvent
	if err := json.Unmarshal(mutated, &ev); err != nil {
		t.Fatalf("base decode should succeed: %v", err)
	}
	_, err := ev.AsEntityChange()
	if err == nil {
		t.Fatal("AsEntityChange must reject the unknown bogusExtraField")
	}
	if !strings.Contains(err.Error(), "bogusExtraField") {
		t.Errorf("error should name the unknown field, got: %v", err)
	}
}

// TestAuditEvent_StrictModeRejectsSystemDrift — same shape for the System
// subtype.
func TestAuditEvent_StrictModeRejectsSystemDrift(t *testing.T) {
	mutated := mutateFixtureWithBogusField(t, "audit_system_event.json", "bogusExtraField", "drift")

	var ev AuditEvent
	if err := json.Unmarshal(mutated, &ev); err != nil {
		t.Fatalf("base decode should succeed: %v", err)
	}
	_, err := ev.AsSystem()
	if err == nil {
		t.Fatal("AsSystem must reject the unknown bogusExtraField")
	}
	if !strings.Contains(err.Error(), "bogusExtraField") {
		t.Errorf("error should name the unknown field, got: %v", err)
	}
}

// TestEntityAuditEventsResponse_StrictModeRejectsEnvelopeDrift verifies
// the response wrapper itself enforces DisallowUnknownFields. Splices an
// unknown top-level field into the response fixture and asserts the
// decode fails.
func TestEntityAuditEventsResponse_StrictModeRejectsEnvelopeDrift(t *testing.T) {
	raw := loadFixture(t, "audit_events_response.json")
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	m["bogusEnvelopeField"] = "drift"
	mutated, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}

	var resp EntityAuditEventsResponse
	err = json.Unmarshal(mutated, &resp)
	if err == nil {
		t.Fatal("EntityAuditEventsResponse must reject the unknown bogusEnvelopeField")
	}
	if !strings.Contains(err.Error(), "bogusEnvelopeField") {
		t.Errorf("error should name the unknown field, got: %v", err)
	}
}
