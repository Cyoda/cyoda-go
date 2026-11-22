package audit

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// key builds an eventKey for table-style compareKeys tests. An empty id
// leaves eventID at its zero value (uuid.Nil), which is fine here since
// these tests never compare on eventID for EntityChange keys.
func key(at time.Time, kind string, n int64, id string) eventKey {
	k := eventKey{at: at, kind: kind, version: n}
	if id != "" {
		k.eventID = uuid.MustParse(id)
	}
	return k
}

func TestCompare_TimeDescFirst(t *testing.T) {
	t0 := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	if compareKeys(key(t0.Add(time.Nanosecond), "StateMachine", 0, "00000000-0000-1000-8000-000000000001"),
		key(t0, "EntityChange", 9, "")) >= 0 {
		t.Fatal("newer event must sort first regardless of kind")
	}
}

func TestCompare_EntityChangeBeforeStateMachineAtOneInstant(t *testing.T) {
	t0 := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	if compareKeys(key(t0, "EntityChange", 1, ""), key(t0, "StateMachine", 0, "00000000-0000-1000-8000-000000000001")) >= 0 {
		t.Fatal("EntityChange must precede StateMachine at one instant")
	}
}

func TestCompare_VersionDesc(t *testing.T) {
	t0 := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	if compareKeys(key(t0, "EntityChange", 3, ""), key(t0, "EntityChange", 2, "")) >= 0 {
		t.Fatal("higher version first")
	}
}

func TestCompare_SMEventIDTimeDesc(t *testing.T) {
	t0 := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	older, _ := uuid.NewUUID()
	newer, _ := uuid.NewUUID()
	for newer.Time() == older.Time() {
		newer, _ = uuid.NewUUID()
	}
	if compareKeys(eventKey{at: t0, kind: "StateMachine", eventID: newer}, eventKey{at: t0, kind: "StateMachine", eventID: older}) >= 0 {
		t.Fatal("later-recorded id first")
	}
}

func TestCompare_SMSameTimeFieldBreaksOnBytes(t *testing.T) {
	t0 := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	a := uuid.MustParse("00000000-0000-1000-8000-000000000002")
	b := uuid.MustParse("00000000-0000-1000-8000-000000000001") // same time field, lower bytes
	ka, kb := eventKey{at: t0, kind: "StateMachine", eventID: a}, eventKey{at: t0, kind: "StateMachine", eventID: b}
	if compareKeys(ka, kb) >= 0 || compareKeys(kb, ka) <= 0 || compareKeys(ka, ka) != 0 {
		t.Fatal("same time field must still give a strict, antisymmetric order on the bytes")
	}
}
