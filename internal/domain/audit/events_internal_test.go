package audit

import (
	"testing"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TestStateMachineItem_RejectsInvalidID pins that stateMachineItem treats
// every id a real store would never assign as a store fault, not just an
// unparsable string. No backend ever hands out the nil UUID
// (00000000-0000-0000-0000-000000000000) as a TimeUUID — accepting it would
// let a corrupted or blank-initialized field through as if it were a real
// identity.
func TestStateMachineItem_RejectsInvalidID(t *testing.T) {
	cases := []struct {
		name     string
		timeUUID string
	}{
		{"empty", ""},
		{"not a UUID", "not-a-uuid"},
		{"nil UUID", uuid.Nil.String()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := stateMachineItem(spi.StateMachineEvent{
				EventType: spi.SMEventStarted,
				EntityID:  "some-entity-id",
				TimeUUID:  tc.timeUUID,
			}, "")
			if err == nil {
				t.Fatalf("expected an error for TimeUUID %q, got nil", tc.timeUUID)
			}
		})
	}
}

// TestEntityChangeItem_ActorKindAndExecutedBy pins the attribution surface on
// an EntityChange event: actor.kind from AttributedKind, executedBy {id,kind}
// from Executor.
func TestEntityChangeItem_ActorKindAndExecutedBy(t *testing.T) {
	v := spi.EntityVersionMeta{Version: 1, ChangeType: "CREATED", Timestamp: time.Now(), User: "alice",
		AttributedKind: spi.PrincipalUser, Executor: spi.Principal{ID: "OBOCLIENT0000001", Kind: spi.PrincipalService}}
	item := entityChangeItem(v, "e1", "acme")
	actor := item.body["actor"].(map[string]any)
	if actor["id"] != "alice" || actor["kind"] != "user" || actor["legalId"] != "acme" {
		t.Fatalf("actor = %v", actor)
	}
	exe := item.body["executedBy"].(map[string]any)
	if exe["id"] != "OBOCLIENT0000001" || exe["kind"] != "service" {
		t.Fatalf("executedBy = %v", exe)
	}
}

// TestEntityChangeItem_LegacyNoAttribution pins the absent-attribution shape:
// a legacy row with no AttributedKind/Executor emits neither key.
func TestEntityChangeItem_LegacyNoAttribution(t *testing.T) {
	v := spi.EntityVersionMeta{Version: 1, ChangeType: "CREATED", Timestamp: time.Now(), User: "alice"}
	item := entityChangeItem(v, "e1", "acme")
	actor := item.body["actor"].(map[string]any)
	if _, ok := actor["kind"]; ok {
		t.Fatalf("actor = %v, want no kind key for a legacy row", actor)
	}
	if _, ok := item.body["executedBy"]; ok {
		t.Fatalf("body = %v, want no executedBy key when Executor is zero", item.body)
	}
}

// TestStateMachineItem_ActorAndExecutedBy pins the attribution surface on a
// StateMachine event: actor {id,name,kind,legalId} from Attributed,
// executedBy {id,kind} from Executor.
func TestStateMachineItem_ActorAndExecutedBy(t *testing.T) {
	ev := spi.StateMachineEvent{EventType: spi.SMEventTransitionMade, EntityID: "e1",
		TimeUUID: "00000000-0000-1000-8000-000000000001", Timestamp: time.Now(),
		Attributed: spi.Principal{ID: "alice", Kind: spi.PrincipalUser},
		Executor:   spi.Principal{ID: "OBOCLIENT0000001", Kind: spi.PrincipalService}}
	item, err := stateMachineItem(ev, "acme")
	if err != nil {
		t.Fatal(err)
	}
	actor := item.body["actor"].(map[string]any)
	if actor["id"] != "alice" || actor["kind"] != "user" || actor["legalId"] != "acme" {
		t.Fatalf("actor = %v", actor)
	}
	if exe := item.body["executedBy"].(map[string]any); exe["id"] != "OBOCLIENT0000001" || exe["kind"] != "service" {
		t.Fatalf("executedBy = %v", exe)
	}
}

// TestStateMachineItem_LegacyNoAttribution pins the absent-attribution shape:
// an event with no Attributed/Executor principal emits neither the actor nor
// the executedBy key.
func TestStateMachineItem_LegacyNoAttribution(t *testing.T) {
	ev := spi.StateMachineEvent{EventType: spi.SMEventTransitionMade, EntityID: "e1",
		TimeUUID: "00000000-0000-1000-8000-000000000001", Timestamp: time.Now()}
	item, err := stateMachineItem(ev, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := item.body["actor"]; ok {
		t.Fatalf("body = %v, want no actor key when Attributed is zero", item.body)
	}
	if _, ok := item.body["executedBy"]; ok {
		t.Fatalf("body = %v, want no executedBy key when Executor is zero", item.body)
	}
}
