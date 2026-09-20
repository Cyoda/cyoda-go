package registry

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/hashicorp/memberlist"

	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// TestTagEvents_CallbacksNeverBlock is the "slow peer does not stall
// membership events" test. memberlist calls NotifyJoin, NotifyUpdate and
// NotifyLeave under its node lock. With nobody draining the queue — which is
// what a worker held up behind a slow peer looks like, and is the real state
// between memberlist.Create and the worker's start — every callback must
// still return at once, dropping what does not fit.
func TestTagEvents_CallbacksNeverBlock(t *testing.T) {
	q := newTagEvents("self", newDirectory(), common.NewChangeSignal())
	node := &memberlist.Node{Name: "peer"}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range tagEventQueueDepth {
			q.NotifyJoin(node)
			q.NotifyUpdate(node)
			q.NotifyLeave(node)
			q.onList([]byte(`{}`))
			q.onRequest([]byte(`{}`))
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a membership callback blocked on a full queue")
	}
	if got := len(q.ch); got != tagEventQueueDepth {
		t.Errorf("queue holds %d events, want it full at %d", got, tagEventQueueDepth)
	}
}

func TestTagEvents_CopyWhatTheyAreGiven(t *testing.T) {
	q := newTagEvents("self", newDirectory(), common.NewChangeSignal())
	buf := []byte(`{"n":"peer"}`)
	q.onList(buf)
	copy(buf, `XXXXXXXXXXXX`) // memberlist reuses the buffer

	ev := <-q.ch
	if string(ev.payload) != `{"n":"peer"}` {
		t.Errorf("payload = %q; the handler kept memberlist's buffer", ev.payload)
	}
}

func TestScanIntervalFor(t *testing.T) {
	tests := []struct {
		patience time.Duration
		want     time.Duration
	}{
		{patience: 0, want: time.Second},
		{patience: 5 * time.Second, want: time.Second},
		{patience: time.Second, want: 500 * time.Millisecond},
		{patience: 100 * time.Millisecond, want: 100 * time.Millisecond},
		{patience: 10 * time.Millisecond, want: 100 * time.Millisecond},
	}
	for _, tt := range tests {
		if got := ScanIntervalFor(tt.patience); got != tt.want {
			t.Errorf("ScanIntervalFor(%v) = %v, want %v", tt.patience, got, tt.want)
		}
	}
}

// TestGossip_LostListIsFetchedAndScanRepeats: a peer announces a list version
// and its list message never arrives. The pnode must ask for it on the join
// event, and — when that request too goes unanswered and no further metadata
// event comes — ask again from the scan.
func TestGossip_LostListIsFetchedAndScanRepeats(t *testing.T) {
	g := startInternalGossip(t, "fetch-1", 24946, 200*time.Millisecond)
	v1 := listVersion{Epoch: 777, Seq: 1}
	p := startRawPeer(t, "fetch-peer", 24947, rawMeta(t, "fetch-peer", v1), "127.0.0.1:24946")

	waitFor(t, 5*time.Second, "a second request, which only the scan can have sent", func() bool {
		return p.requestCount() >= 2
	})
	p.mu.Lock()
	from := p.requests[0]
	p.mu.Unlock()
	if from != "fetch-1" {
		t.Errorf("request names %q as the requester, want fetch-1", from)
	}

	want := map[string][]string{"tenant-a": {"python"}}
	p.send(t, "fetch-1", 24946, topicTags, tagListMsg{NodeID: "fetch-peer", Version: v1, Tags: want})
	waitFor(t, 3*time.Second, "the fetched list is held", func() bool {
		return reflect.DeepEqual(g.tags.tagsOf("fetch-peer"), want)
	})

	// Once the right list is held the requests stop.
	settled := p.requestCount()
	time.Sleep(3 * 200 * time.Millisecond)
	if got := p.requestCount(); got > settled+1 {
		t.Errorf("%d more requests after the list was held; the scan keeps fetching a current list", got-settled)
	}
}

// TestGossip_MetadataUpdateFetchesAtOnce isolates the metadata-update fetch
// from the scan: an hour-long scan interval means the second request can only
// be the fetch that the update nudge (NotifyUpdate -> evMember ->
// handleMember) sent.
func TestGossip_MetadataUpdateFetchesAtOnce(t *testing.T) {
	startInternalGossip(t, "update-1", 25958, time.Hour)
	v1 := listVersion{Epoch: 777, Seq: 1}
	p := startRawPeer(t, "update-peer", 25959, rawMeta(t, "update-peer", v1), "127.0.0.1:25958")

	waitFor(t, 5*time.Second, "the join request", func() bool {
		return p.requestCount() >= 1
	})

	// A new version announced without its list: the metadata event fetches.
	v2 := listVersion{Epoch: 777, Seq: 2}
	before := p.requestCount()
	p.announce(t, rawMeta(t, "update-peer", v2))
	waitFor(t, 5*time.Second, "a second request after the announced version moved on", func() bool {
		return p.requestCount() > before
	})
}

func TestGossip_AnswersARequestWithItsOwnList(t *testing.T) {
	g := startInternalGossip(t, "answer-1", 24948, 200*time.Millisecond)
	own := map[string][]string{"tenant-a": {"go", "python"}}
	if err := g.UpdateTags(own); err != nil {
		t.Fatal(err)
	}
	p := startRawPeer(t, "answer-peer", 24949, rawMeta(t, "answer-peer", listVersion{Epoch: 1}), "127.0.0.1:24948")

	waitFor(t, 3*time.Second, "answer-1 sees the raw peer", func() bool {
		_, ok := g.member("answer-peer")
		return ok
	})
	p.send(t, "answer-1", 24948, topicTagsRequest, tagRequestMsg{From: "answer-peer"})

	version, _ := g.tags.ownList()
	waitFor(t, 3*time.Second, "the raw peer receives answer-1's list", func() bool {
		for _, l := range p.receivedLists() {
			if l.NodeID == "answer-1" && l.Version == version && reflect.DeepEqual(l.Tags, own) {
				return true
			}
		}
		return false
	})
}

func TestGossip_OlderListTriggersAFetchAtOnce(t *testing.T) {
	// No scan within this test: the second request can only be the fetch that
	// follows the dropped list.
	g := startInternalGossip(t, "stale-1", 24950, time.Hour)
	v5 := listVersion{Epoch: 9, Seq: 5}
	p := startRawPeer(t, "stale-peer", 24951, rawMeta(t, "stale-peer", v5), "127.0.0.1:24950")
	waitFor(t, 5*time.Second, "the first request", func() bool { return p.requestCount() >= 1 })

	before := p.requestCount()
	p.send(t, "stale-1", 24950, topicTags, tagListMsg{NodeID: "stale-peer", Version: listVersion{Epoch: 9, Seq: 4}, Tags: map[string][]string{"t": {"old"}}})

	if got := g.tags.tagsOf("stale-peer"); len(got) != 0 {
		t.Errorf("a list older than the announced version was stored: %v", got)
	}
	waitFor(t, 3*time.Second, "a request following the dropped list", func() bool {
		return p.requestCount() > before
	})
}

func TestGossip_LeaveDropsList(t *testing.T) {
	// An hour-long scan on both pnodes: retain(alive) would drop the departed
	// peer's list on its own, so a scan-driven pass would not prove that
	// handleLeave does the work.
	g1 := startInternalGossip(t, "leave-1", 24952, time.Hour)
	g2 := startInternalGossip(t, "leave-2", 24953, time.Hour, "127.0.0.1:24952")
	if err := g2.UpdateTags(map[string][]string{"tenant-a": {"python"}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "leave-1 holds leave-2's list", func() bool {
		return len(g1.tags.tagsOf("leave-2")) == 1
	})

	if err := g2.Deregister(context.Background(), "leave-2"); err != nil {
		t.Fatalf("Deregister: %v", err)
	}
	waitFor(t, 5*time.Second, "leave-2's list is dropped", func() bool {
		return len(g1.tags.tagsOf("leave-2")) == 0
	})
}

// TestGossip_RequestFromUnparseableMember_NoAnswer: a directory entry whose
// metadata does not parse is what List skips and the registry treats as not
// alive. handleRequest must answer the same way it does everywhere else in
// the registry: nothing sent, not this pnode's tag list.
func TestGossip_RequestFromUnparseableMember_NoAnswer(t *testing.T) {
	g := startInternalGossip(t, "reqbad-1", 25956, time.Hour)
	p := startRawPeer(t, "reqbad-stranger", 25957, []byte("not json"), "127.0.0.1:25956")
	waitFor(t, 5*time.Second, "reqbad-1 has the stranger as a member", func() bool {
		_, ok := g.member("reqbad-stranger")
		return ok
	})

	p.send(t, "reqbad-1", 25956, topicTagsRequest, tagRequestMsg{From: "reqbad-stranger"})

	time.Sleep(500 * time.Millisecond)
	if lists := p.receivedLists(); len(lists) != 0 {
		t.Errorf("a member whose metadata does not parse got an answer to its request: %v", lists)
	}
}
