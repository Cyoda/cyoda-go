package audit

import (
	"testing"

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
			})
			if err == nil {
				t.Fatalf("expected an error for TimeUUID %q, got nil", tc.timeUUID)
			}
		})
	}
}
