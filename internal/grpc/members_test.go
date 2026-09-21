package grpc

import (
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
)

func noopSend(_ *cepb.CloudEvent) error { return nil }

func TestMemberRegistry_RegisterAndList(t *testing.T) {
	reg := NewMemberRegistry()
	tenant := spi.TenantID("tenant-1")
	tags := []string{"python", "default"}

	registered := reg.Register("m-1", tenant, tags, noopSend, nil)

	members := reg.List()
	if len(members) != 1 {
		t.Fatalf("expected 1 member, got %d", len(members))
	}
	m := members[0]
	if m.ID != registered.ID {
		t.Errorf("expected ID %s, got %s", registered.ID, m.ID)
	}
	if m.TenantID != tenant {
		t.Errorf("expected tenant %s, got %s", tenant, m.TenantID)
	}
	if len(m.Tags) != 2 || m.Tags[0] != "python" || m.Tags[1] != "default" {
		t.Errorf("unexpected tags: %v", m.Tags)
	}
	if m.ConnectedAt.IsZero() {
		t.Error("ConnectedAt should not be zero")
	}
}

func TestMemberRegistry_RegisterAndUnregister(t *testing.T) {
	reg := NewMemberRegistry()
	m := reg.Register("m-1", "tenant-1", []string{"a"}, noopSend, nil)

	reg.Unregister(m)

	if len(reg.List()) != 0 {
		t.Fatal("expected 0 members after unregister")
	}
}

// A displaced member's handler still runs its deferred Unregister. If that
// unregistered by ID it would delete the member that displaced it — the live
// connection — leaving the registry empty while the client believes it is
// registered. Unregister therefore only removes the entry when it is still
// the member the caller means.
func TestMemberRegistry_UnregisterOfADisplacedMemberLeavesTheLiveOne(t *testing.T) {
	reg := NewMemberRegistry()
	first := reg.Register("m-1", "tenant-1", []string{"a"}, noopSend, nil)
	second := reg.Register("m-1", "tenant-1", []string{"a"}, noopSend, nil)

	reg.Unregister(first)

	members := reg.List()
	if len(members) != 1 {
		t.Fatalf("expected the live member to remain, got %d member(s)", len(members))
	}
	if members[0] != second {
		t.Fatal("the registry holds a different member than the one that displaced the first")
	}
	if got := reg.Get("m-1"); got != second {
		t.Fatalf("Get returned %v, want the member that displaced the first", got)
	}

	select {
	case <-second.Evicted():
		t.Fatal("the displaced member's Unregister evicted the live member")
	default:
	}
}

func TestMember_TrackAndCompleteRequest(t *testing.T) {
	reg := NewMemberRegistry()
	m := reg.Register("m-1", "tenant-1", []string{"a"}, noopSend, nil)

	ch, err := m.TrackRequest("req-1")
	if err != nil {
		t.Fatalf("TrackRequest: %v", err)
	}

	go func() {
		m.CompleteRequest("req-1", &ProcessingResponse{
			Success: true,
			Payload: []byte(`{"result":"ok"}`),
		})
	}()

	select {
	case resp := <-ch:
		if resp == nil {
			t.Fatal("expected non-nil response")
		}
		if !resp.Success {
			t.Error("expected success=true")
		}
		if string(resp.Payload) != `{"result":"ok"}` {
			t.Errorf("unexpected payload: %s", resp.Payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for response")
	}
}

func TestMemberRegistry_UnregisterFailsPending(t *testing.T) {
	reg := NewMemberRegistry()
	m := reg.Register("m-1", "tenant-1", []string{"a"}, noopSend, nil)

	ch, err := m.TrackRequest("req-1")
	if err != nil {
		t.Fatalf("TrackRequest: %v", err)
	}

	reg.Unregister(m)

	select {
	case resp := <-ch:
		if resp == nil {
			t.Fatal("expected non-nil error response")
		}
		if resp.Success {
			t.Error("expected success=false for failed pending")
		}
		if resp.Error == "" {
			t.Error("expected non-empty error message")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for error response on pending channel")
	}
}

func TestMemberRegistry_GetExisting(t *testing.T) {
	reg := NewMemberRegistry()
	registered := reg.Register("m-1", "tenant-1", []string{"a"}, noopSend, nil)

	m := reg.Get(registered.ID)
	if m == nil {
		t.Fatal("expected non-nil member")
	}
	if m.ID != registered.ID {
		t.Errorf("expected ID %s, got %s", registered.ID, m.ID)
	}
}

func TestMemberRegistry_GetNonExistent(t *testing.T) {
	reg := NewMemberRegistry()

	m := reg.Get("does-not-exist")
	if m != nil {
		t.Fatal("expected nil for non-existent member")
	}
}
