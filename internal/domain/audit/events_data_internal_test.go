package audit

import (
	"reflect"
	"testing"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Each case also asserts the stored map is unchanged after the call: the
// snapshot comparison is the guard that the item never aliases or mutates the
// stored data.
func TestStateMachineItem_DataCarriesEventType(t *testing.T) {
	cases := []struct {
		name   string
		stored map[string]any
		want   map[string]any
	}{
		{"merges the type into the data", map[string]any{"x": 1}, map[string]any{"x": 1, "eventType": "TRANSITION_MAKE"}},
		{"the event's type wins over a stored one", map[string]any{"eventType": "WRONG"}, map[string]any{"eventType": "TRANSITION_MAKE"}},
		{"nil data becomes the type alone", nil, map[string]any{"eventType": "TRANSITION_MAKE"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var before map[string]any
			if tc.stored != nil {
				before = map[string]any{}
				for k, v := range tc.stored {
					before[k] = v
				}
			}
			item, err := stateMachineItem(spi.StateMachineEvent{
				EventType: spi.SMEventTransitionMade,
				EntityID:  "e",
				TimeUUID:  uuid.NewString(),
				Data:      tc.stored,
			}, "")
			if err != nil {
				t.Fatal(err)
			}
			if got := item.body["data"]; !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("data = %#v, want %#v", got, tc.want)
			}
			if !reflect.DeepEqual(tc.stored, before) {
				t.Fatalf("stored map changed: %#v, want %#v", tc.stored, before)
			}
		})
	}
}
