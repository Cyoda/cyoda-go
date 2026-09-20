package registry

import (
	"errors"
	"reflect"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common"
)

func TestAcceptList(t *testing.T) {
	v := func(epoch int64, seq uint64) listVersion { return listVersion{Epoch: epoch, Seq: seq} }
	tests := []struct {
		name          string
		msg           listVersion
		announced     listVersion
		haveAnnounced bool
		held          listVersion
		haveHeld      bool
		want          bool
	}{
		{name: "equals the announced version", msg: v(100, 5), announced: v(100, 5), haveAnnounced: true, want: true},
		{name: "nothing announced for the sender", msg: v(100, 5), haveAnnounced: false, want: false},
		{name: "outran its metadata: later seq of the announced epoch, nothing held", msg: v(100, 6), announced: v(100, 5), haveAnnounced: true, want: true},
		{name: "outran its metadata and later than what is held", msg: v(100, 7), announced: v(100, 5), haveAnnounced: true, held: v(100, 6), haveHeld: true, want: true},
		{name: "two lists out of order: not later than what is held", msg: v(100, 6), announced: v(100, 5), haveAnnounced: true, held: v(100, 7), haveHeld: true, want: false},
		{name: "older than the announced version", msg: v(100, 4), announced: v(100, 5), haveAnnounced: true, want: false},
		{name: "another epoch, numerically later", msg: v(200, 1), announced: v(100, 5), haveAnnounced: true, want: false},
		{name: "another epoch, numerically earlier", msg: v(50, 9), announced: v(100, 5), haveAnnounced: true, want: false},
		{name: "restart with an earlier clock: announced epoch is below the held one", msg: v(90, 1), announced: v(90, 1), haveAnnounced: true, held: v(100, 9), haveHeld: true, want: true},
		{name: "held list of an old epoch does not block a later seq of the announced epoch", msg: v(90, 2), announced: v(90, 1), haveAnnounced: true, held: v(100, 9), haveHeld: true, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := acceptList(tt.msg, tt.announced, tt.haveAnnounced, tt.held, tt.haveHeld)
			if got != tt.want {
				t.Errorf("acceptList = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsCurrent(t *testing.T) {
	v := func(epoch int64, seq uint64) listVersion { return listVersion{Epoch: epoch, Seq: seq} }
	tests := []struct {
		name      string
		held      listVersion
		haveHeld  bool
		announced listVersion
		want      bool
	}{
		{name: "nothing held", haveHeld: false, announced: v(100, 0), want: false},
		{name: "held equals announced", held: v(100, 3), haveHeld: true, announced: v(100, 3), want: true},
		{name: "held is behind", held: v(100, 2), haveHeld: true, announced: v(100, 3), want: false},
		{name: "held outran the metadata", held: v(100, 4), haveHeld: true, announced: v(100, 3), want: true},
		{name: "held is from another life of the pnode, later epoch number", held: v(200, 9), haveHeld: true, announced: v(100, 1), want: false},
		{name: "held is from another life of the pnode, earlier epoch number", held: v(50, 9), haveHeld: true, announced: v(100, 1), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isCurrent(tt.held, tt.haveHeld, tt.announced); got != tt.want {
				t.Errorf("isCurrent = %v, want %v", got, tt.want)
			}
		})
	}
}

func fired(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func TestTagStore_PutStoresSortedCopyAndFires(t *testing.T) {
	sig := common.NewChangeSignal()
	s := newTagStore("self", 1, sig)
	in := map[string][]string{"tenant-a": {"ml", "python", "ml"}}
	ch := sig.Changed()

	if !s.put("peer", listVersion{Epoch: 7, Seq: 1}, in, listVersion{Epoch: 7, Seq: 1}, true) {
		t.Fatal("put refused a list whose version equals the announced one")
	}
	if !fired(ch) {
		t.Error("a first list for a pnode did not fire the change signal")
	}

	in["tenant-a"][0] = "mutated-by-caller"
	got := s.tagsOf("peer")
	want := map[string][]string{"tenant-a": {"ml", "python"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tagsOf = %v, want %v (sorted, de-duplicated, not aliased to the caller's map)", got, want)
	}

	got["tenant-a"][0] = "mutated-by-reader"
	if again := s.tagsOf("peer"); !reflect.DeepEqual(again, want) {
		t.Errorf("tagsOf handed out the stored map: %v", again)
	}
}

func TestTagStore_SameTagsUnderANewVersionDoNotFire(t *testing.T) {
	sig := common.NewChangeSignal()
	s := newTagStore("self", 1, sig)
	tags := map[string][]string{"tenant-a": {"x"}}
	s.put("peer", listVersion{Epoch: 7, Seq: 1}, tags, listVersion{Epoch: 7, Seq: 1}, true)

	ch := sig.Changed()
	if !s.put("peer", listVersion{Epoch: 7, Seq: 2}, tags, listVersion{Epoch: 7, Seq: 2}, true) {
		t.Fatal("put refused the newer version")
	}
	if fired(ch) {
		t.Error("identical tags under a new version woke the waiters")
	}
	if !s.current("peer", listVersion{Epoch: 7, Seq: 2}) {
		t.Error("the newer version was not recorded")
	}
}

func TestTagStore_NeverAcceptsAForeignCopyOfItsOwnList(t *testing.T) {
	sig := common.NewChangeSignal()
	s := newTagStore("self", 1, sig)
	ch := sig.Changed()

	if s.put("self", listVersion{Epoch: 1, Seq: 0}, map[string][]string{"t": {"forged"}}, listVersion{Epoch: 1, Seq: 0}, true) {
		t.Fatal("put accepted a list naming this pnode")
	}
	if fired(ch) {
		t.Error("a refused list fired the change signal")
	}
	if got := s.tagsOf("self"); len(got) != 0 {
		t.Errorf("own tags = %v, want empty", got)
	}
}

func TestTagStore_RestartWithEarlierClock(t *testing.T) {
	s := newTagStore("self", 1, common.NewChangeSignal())
	s.put("peer", listVersion{Epoch: 1000, Seq: 9}, map[string][]string{"t": {"old"}}, listVersion{Epoch: 1000, Seq: 9}, true)

	// The pnode restarted under the same id on a host whose clock is behind.
	announced := listVersion{Epoch: 400, Seq: 1}
	if s.current("peer", announced) {
		t.Fatal("the list of the previous life counts as current for the new one")
	}
	if !s.put("peer", announced, map[string][]string{"t": {"new"}}, announced, true) {
		t.Fatal("the restarted pnode's list was taken for an older one and dropped")
	}
	if got := s.tagsOf("peer")["t"]; !reflect.DeepEqual(got, []string{"new"}) {
		t.Errorf("tags = %v, want [new]", got)
	}
}

func TestTagStore_DropAndRetain(t *testing.T) {
	sig := common.NewChangeSignal()
	s := newTagStore("self", 1, sig)
	for _, node := range []string{"a", "b", "c"} {
		s.put(node, listVersion{Epoch: 5, Seq: 1}, map[string][]string{"t": {node}}, listVersion{Epoch: 5, Seq: 1}, true)
	}

	ch := sig.Changed()
	if !s.drop("a") {
		t.Fatal("drop reported nothing held for a")
	}
	if !fired(ch) {
		t.Error("dropping a held list did not fire the change signal")
	}
	if s.drop("a") {
		t.Error("second drop of a reported a list")
	}
	if got := s.tagsOf("a"); len(got) != 0 {
		t.Errorf("tags of a dropped pnode = %v, want empty", got)
	}

	ch = sig.Changed()
	if n := s.retain(map[string]struct{}{"b": {}}); n != 1 {
		t.Errorf("retain dropped %d lists, want 1 (c)", n)
	}
	if !fired(ch) {
		t.Error("retain dropped a list without firing the change signal")
	}
	if s.current("c", listVersion{Epoch: 5, Seq: 1}) {
		t.Error("c is still held after retain")
	}
	if !s.current("b", listVersion{Epoch: 5, Seq: 1}) {
		t.Error("retain dropped b, which is alive")
	}
}

func TestTagStore_SetOwn(t *testing.T) {
	s := newTagStore("self", 42, common.NewChangeSignal())
	var announced []listVersion
	announce := func(v listVersion) error {
		announced = append(announced, v)
		return nil
	}

	changed, err := s.setOwn(map[string][]string{"t": {"b", "a"}}, announce)
	if err != nil || !changed {
		t.Fatalf("setOwn = (%v, %v), want (true, nil)", changed, err)
	}
	version, tags := s.ownList()
	if version != (listVersion{Epoch: 42, Seq: 1}) {
		t.Errorf("version = %+v, want epoch 42 seq 1", version)
	}
	if !reflect.DeepEqual(tags, map[string][]string{"t": {"a", "b"}}) {
		t.Errorf("own tags = %v, want sorted", tags)
	}

	// The same set in another order is not a change: no bump, no announcement.
	changed, err = s.setOwn(map[string][]string{"t": {"a", "b", "a"}}, announce)
	if err != nil || changed {
		t.Fatalf("setOwn of an equal set = (%v, %v), want (false, nil)", changed, err)
	}
	if len(announced) != 1 || announced[0] != (listVersion{Epoch: 42, Seq: 1}) {
		t.Errorf("announced = %+v, want exactly one announcement of seq 1", announced)
	}

	// An announcement that fails leaves version and list as they were, so one
	// version never names two lists.
	boom := errors.New("boom")
	changed, err = s.setOwn(map[string][]string{"t": {"c"}}, func(listVersion) error { return boom })
	if !errors.Is(err, boom) || changed {
		t.Fatalf("setOwn with a failing announcement = (%v, %v), want (false, boom)", changed, err)
	}
	version, tags = s.ownList()
	if version.Seq != 1 || !reflect.DeepEqual(tags, map[string][]string{"t": {"a", "b"}}) {
		t.Errorf("after a failed announcement: version %+v tags %v, want seq 1 and the old list", version, tags)
	}
}

func TestTagStore_TenantWithNoTagsIsKept(t *testing.T) {
	// A cnode that joined without tags still serves callouts that ask for no
	// tag, so its tenant must stay in the list.
	s := newTagStore("self", 1, common.NewChangeSignal())
	if _, err := s.setOwn(map[string][]string{"tenant-a": nil}, func(listVersion) error { return nil }); err != nil {
		t.Fatal(err)
	}
	_, tags := s.ownList()
	if _, ok := tags["tenant-a"]; !ok {
		t.Errorf("own tags = %v, want tenant-a present with no tags", tags)
	}
}
