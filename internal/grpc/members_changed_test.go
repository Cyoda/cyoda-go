package grpc

import "testing"

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func TestMemberRegistry_Changed_FiresOnAttachAndDetach(t *testing.T) {
	reg := NewMemberRegistry()

	before := reg.Changed()
	if isClosed(before) {
		t.Fatal("the channel must be open until something changes")
	}
	m := reg.Register("m-1", "tenant-1", []string{"x"}, noopSend, nil)
	if !isClosed(before) {
		t.Fatal("attach did not close the channel taken before it")
	}
	afterAttach := reg.Changed()
	if isClosed(afterAttach) {
		t.Fatal("the channel must be replaced, not left closed")
	}
	reg.Unregister(m)
	if !isClosed(afterAttach) {
		t.Fatal("detach did not close the channel taken before it")
	}
}

// Taking the channel before looking is what makes the wait race-free: a cnode
// that attaches after the look still wakes the waiter.
func TestMemberRegistry_Changed_TakenBeforeLooking_NoLostWakeUp(t *testing.T) {
	reg := NewMemberRegistry()
	ch := reg.Changed()
	if got := reg.Candidates("tenant-1", "x"); len(got) != 0 {
		t.Fatalf("expected no candidates yet, got %d", len(got))
	}
	m := reg.Register("m-1", "tenant-1", []string{"x"}, noopSend, nil)
	t.Cleanup(func() { reg.Unregister(m) })
	<-ch // returns at once; a lost wake-up would hang until the package timeout
	if got := reg.Candidates("tenant-1", "x"); len(got) != 1 {
		t.Fatalf("expected the new member to be a candidate, got %d", len(got))
	}
}

func TestMemberRegistry_Changed_UnregisterThatRemovedNothingIsNotAChange(t *testing.T) {
	reg := NewMemberRegistry()
	first := reg.Register("m-1", "tenant-1", nil, noopSend, nil)
	second := reg.Register("m-1", "tenant-1", nil, noopSend, nil) // displaces first
	t.Cleanup(func() { reg.Unregister(second) })

	ch := reg.Changed()
	reg.Unregister(first) // the displaced member's deferred Unregister
	if isClosed(ch) {
		t.Fatal("an Unregister that removed nothing must not signal a change")
	}
}
