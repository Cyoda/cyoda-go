package audit

import (
	"context"
	"reflect"
	"testing"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

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

// The item must not alias the store's map: building it twice leaves the map
// read back from the memory backend free of eventType.
func TestStateMachineItem_DoesNotShareTheStoredMap(t *testing.T) {
	ctx := spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID: "u", Tenant: spi.Tenant{ID: "t", Name: "t"}, Roles: []string{"USER"},
	})
	store, err := memory.NewStoreFactory().StateMachineAuditStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Record(ctx, "ent-1", spi.StateMachineEvent{
		EventType: spi.SMEventTransitionMade,
		EntityID:  "ent-1",
		Data:      map[string]any{"x": 1},
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		evs, err := store.GetEvents(ctx, "ent-1")
		if err != nil || len(evs) != 1 {
			t.Fatalf("GetEvents: %v, %d events", err, len(evs))
		}
		if _, err := stateMachineItem(evs[0], ""); err != nil {
			t.Fatal(err)
		}
	}
	evs, _ := store.GetEvents(ctx, "ent-1")
	if !reflect.DeepEqual(evs[0].Data, map[string]any{"x": 1}) {
		t.Fatalf("store map = %#v, want only x", evs[0].Data)
	}
}
