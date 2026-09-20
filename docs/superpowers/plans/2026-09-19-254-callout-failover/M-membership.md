# Stream M — cluster membership (`internal/cluster/registry`)

Implements spec §11 in full, the membership rows of §13, the membership
counters of §12, the node registry's `Changed()` of §5, and the membership
items of §14. Evidence base: research §12.

## What the code says today (read, not assumed)

- `internal/cluster/registry/gossip.go:35-40` — `nodeMeta` carries
  `Tags map[string][]string`. `:251-262` `UpdateTags` has two bare
  `g.mu.Unlock()` and ends in `g.list.UpdateNode(0)`, which blocks until the
  alive broadcast has been retransmitted to its limit (memberlist
  `memberlist.go:513-551`; about 0.8 s for two pnodes, 1.6 s for twenty) while
  `MemberRegistry.publishMu` is held by the caller
  (`internal/grpc/members.go:501-514`).
- `gossip.go:288-300` — `NodeMeta` returns nil past the limit; nothing retries.
- `gossip.go:183-194` — `Lookup` returns an error for unparseable metadata;
  `internal/cluster/proxy/http.go:57-66` turns that into a 500.
  `proxy/grpc.go:60-63` wraps it as a plain error. `List` (`:197-219`) skips
  the member with a WARN.
- `gossip.go:222-234` — `Deregister` called twice panics inside memberlist
  (`memberlist.go:653-655`, "leave after shutdown"). Fixed in M-3 because that
  task gives `Deregister` a worker to stop.
- memberlist v0.6.0 (`/Users/paul/go/pkg/mod/github.com/hashicorp/memberlist@v0.6.0`):
  `MetaMaxSize = 512` (`net.go:83`), enforced by panic (`memberlist.go:459,
  518`). `aliveNode` takes `nodeLock` for its whole body (`state.go:941-943`)
  and calls `NotifyJoin` / `NotifyUpdate` at `:1145-1152` — **including for the
  local node**, so `UpdateNode` fires `NotifyUpdate(self)`. `deadNode` calls
  `NotifyLeave` under the same lock (`state.go:1306`). `Create` runs `setAlive`
  → `aliveNode` → `NotifyJoin(self)` before it returns (`memberlist.go:248-259`).
  `NotifyMsg` is reached from the UDP packet handler (`net.go:767`) and from
  the TCP stream reader (`net.go:1344`). `Members()` returns only nodes that
  are not dead or left (`memberlist.go:608-620`). `SendReliable` →
  `sendUserMsg` (`net.go:926-957`): the **dial** is bounded by `TCPTimeout`
  (10 s in `DefaultLANConfig`, `config.go:311`); the write has no deadline (see
  Open points).
- **A latent data race in how the registry reads members (found while checking
  this plan, not in the research).** `Members()` hands out pointers into
  memberlist's node map (`memberlist.go:608-620`, `&n.Node`), and `aliveNode`
  rewrites those nodes' `Meta`, `Addr` and `Port` under `nodeLock`
  (`state.go:1131-1133`), which no caller can take. `Gossip.List` and
  `Gossip.Lookup` read `m.Meta` after `Members()` has released the lock
  (`gossip.go:184-190, 198-202`): a race on a slice header, so a torn read is
  possible. Today it is hidden only because `UpdateTags` blocks in
  `UpdateNode(0)` until the new metadata has reached the peer, so the existing
  test's first `List` comes after the write. With `UpdateTags` no longer
  blocking, `go test -race` reports it at once in the **unchanged**
  `TestGossipRegistry_TagPropagation`:
  `Write … memberlist.(*Memberlist).aliveNode() state.go:1131` /
  `Previous read … registry.(*Gossip).List()`. The unmodified tree passes
  `-race`. Decision 6 below is the fix; it is part of M-3 because M-3 is what
  exposes it.
- `gossipDelegate.NotifyMsg` has a third bare unlock, `d.subsMu.RUnlock()`
  (`gossip.go:309-311`), beside the two the spec names in `UpdateTags`. M-3's
  exit check covers the package, so M-3 fixes it too.
- `app/app.go:1155-1181` — `mustNewGossip` already exits the process on any
  `NewGossip` error, so "refuses to start" needs only an error from
  `NewGossip`. `app.go:573-584` wires `MemberRegistry.SetOnChange` to
  `Gossip.UpdateTags`; its signature does not change.
- `contract.NodeRegistry` (`internal/contract/registry.go:13-18`) has two
  implementations (`registry.Gossip`, `registry.Local`) and six test fakes:
  `internal/cluster/integration_test.go` `testRegistry`,
  `internal/cluster/scheduler_rpc_test.go` `fakeRegistry`,
  `internal/cluster/proxy/http_test.go` `fakeRegistry`,
  `internal/cluster/dispatch/cluster_dispatcher_test.go` `stubNodeRegistry`,
  `internal/grpc/txroute_interceptor_test.go` `fakeRouteRegistry`,
  `internal/scheduler/service_test.go` `fakeRegistry` (`countingRegistry`
  embeds it). `grep -rn '\[\]contract.NodeInfo, error)' --include='*.go' .`
  lists them all.

**V-3 (metric naming and registration), settled for this stream.** Instrument
names are dotted lower case under `cyoda.<area>.…`
(`internal/observability/dispatch_tracing.go:41-47`,
`plugins/postgres/metrics.go:23`). A package that owns instruments takes a
`metric.Meter` in its constructor and returns an error if an instrument cannot
be created, which `app.go` treats as a startup failure
(`internal/auth/reconcile_metrics.go:33-43`, `app.go:280-284`). `app.go` passes
`observability.Meter()`; the Prometheus scrape pipeline is always built
(`internal/observability/init.go:46-48, 113-115`), so the instruments appear at
`/metrics` whether or not `CYODA_OTEL_ENABLED` is set. Observable instruments
register a callback and unregister it on shutdown
(`plugins/postgres/metrics.go`). Tests read instruments through
`sdkmetric.NewManualReader()` (`internal/observability/tx_tracing_test.go:103`).

## Design decisions this plan makes inside §11

1. **`UpdateTags` never touches the network.** It takes the new version and
   list in one locked step, writes the metadata bytes under the same lock, pokes
   a one-slot channel and returns. The worker goroutine does `UpdateNode` and
   the sends. That is what takes `UpdateNode` off `MemberRegistry.publishMu`.
   The one-slot poke is level-triggered: a burst of changes coalesces, and the
   worker always reads the newest list, so a publish can never be dropped the
   way a queued event can.
2. **Every send runs in its own goroutine**, never on the worker: a dial to a
   pnode that has just died hangs for `TCPTimeout`, and a worker stuck in it
   would not store the list a waiting callout needs.
3. **An unchanged set publishes nothing.** `MemberRegistry` bumps its version on
   every attach, including a second cnode with tags already advertised. The
   store normalises (sorted, de-duplicated) and compares; an equal set keeps
   the version. This is what "tags are sorted, so an unchanged set marshals
   identically" buys, and it makes `computeTagsLocked`'s map order
   (`members.go:520-542`, another stream's file) harmless without touching it.
4. **"Holds the right list"** is `held == announced`, **or** the held list is a
   later `seq` of the announced epoch (it outran its metadata). See Open
   point 1 for why the second clause is required.
5. **Scan interval** is not a new setting. `registry.ScanIntervalFor(patience)`
   returns half the patience clamped to 100 ms … 1 s (1 s when patience is 0),
   and `app.go` passes it in `GossipConfig.ListScanInterval`.
6. **The registry keeps its own directory of alive members and never reads
   `memberlist.Members()`.** `NotifyJoin`, `NotifyUpdate` and `NotifyLeave` are
   the only place a member can be read safely — the node is handed over under
   memberlist's lock — so they copy it (name, address, port, metadata bytes)
   into a map under a small mutex of the registry's own, which is never held
   across a memberlist call or any I/O. `List`, `Lookup`, the publish fan-out,
   the scan and the gauge read that directory. This removes the race described
   above, and it means a join or a leave cannot be *lost*: the directory is
   state, not a queue. The bounded queue still carries the two list messages
   and a "look at this member" nudge; the scan repairs what it drops. It departs
   from §11's wording ("all four only copy what they were given … and enqueue
   it") in where three of the four copy *to*; Open point 7.

**How this plan was checked.** Every code block for M-1 … M-7 was assembled
into the final state and run against the worktree with `go vet -overlay` and
`go test -race -overlay` (the repository itself was not touched): green and
race-clean, three consecutive passes. The state after M-3 alone was assembled
and run the same way: green and race-clean, existing tests included. M-3's RED
was run against the unmodified tree and failed exactly as Step 2 says
(`metaLen=705 limit=512`; "NewGossip accepted an identity…"; `panic: leave
after shutdown`). The M-8 scenario was vetted against a stub of the harness
capability it consumes; it has not been run, because that capability does not
exist yet. The before/after snippets for `gossip.go`, `app.go`, the fakes and
the documents were not machine-checked beyond the assembled `gossip.go`.

## Coverage — §13 membership rows

| Row | Layer | Task |
|---|---|---|
| Many tenants on one pnode stay visible (R§12 sizes) | U | M-3 `TestGossipRegistry_ManyTenantsStayVisible` |
| | M | M-8 `Membership_ManyTenantsStayVisible` (consumes a harness capability) |
| Restart under the same id, with an earlier clock → new list accepted | U | M-2 `TestAcceptList` + `TestTagStore_RestartWithEarlierClock`; M-4 `TestGossipRegistry_RestartUnderSameID` |
| Late joiner / lost list message → fetched | U | M-4 `TestGossipRegistry_LateJoinerFetchesList`, `TestGossip_LostListIsFetchedAndScanRepeats` |
| Leaving pnode's list dropped | U | M-2 `TestTagStore_DropAndRetain`; M-4 `TestGossip_LeaveDropsList` |
| No call into memberlist from inside a callback | U + review | M-3 `TestDirectory_*` (what the callbacks do instead); M-4 `TestTagEvents_CallbacksNeverBlock`, `TestTagEvents_CopyWhatTheyAreGiven`; review item stated in M-4 |
| Identity over 512 bytes → refuses to start | U | M-3 `TestNewGossip_IdentityTooLarge_RefusesToStart` |
| Unparseable metadata → not alive; HTTP routing 503, not 500 | U | M-6 `TestGossipRegistry_UnparseableMetadata_NotAlive_HTTP503` |
| §12 counters: failed list sends, outstanding list requests | U | M-7 |
| §5 `Changed()` on the node registry | U | M-1, M-5 |

---

### Task M-1: `common.ChangeSignal` — a channel closed and replaced on every change

**Spec:** §5 "Patience (D7)" — "a channel closed and replaced on every change".

**Files:**
- Create: `internal/common/changesignal.go`
- Test: `internal/common/changesignal_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `func common.NewChangeSignal() *common.ChangeSignal`
  - `func (s *ChangeSignal) Changed() <-chan struct{}`
  - `func (s *ChangeSignal) Fire()`

  The `internal/grpc` stream needs the same primitive for
  `MemberRegistry.Changed()`. `internal/grpc` already imports
  `internal/common`; `internal/common` imports only the SPI. One type, both
  registries.

- [ ] **Step 1: Write the failing tests**

```go
package common

import (
	"sync"
	"testing"
	"time"
)

func TestChangeSignal_FireClosesTheChannelTakenBefore(t *testing.T) {
	s := NewChangeSignal()
	ch := s.Changed()

	select {
	case <-ch:
		t.Fatal("channel closed before any Fire")
	default:
	}

	s.Fire()

	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("channel taken before Fire was not closed by it")
	}
}

func TestChangeSignal_ChannelIsReplacedAfterFire(t *testing.T) {
	s := NewChangeSignal()
	first := s.Changed()
	s.Fire()
	second := s.Changed()

	if first == second {
		t.Fatal("Changed returned the closed channel again; every Fire must install a new one")
	}
	select {
	case <-second:
		t.Fatal("the replacement channel is already closed")
	default:
	}
}

func TestChangeSignal_EveryWaiterWakes(t *testing.T) {
	s := NewChangeSignal()
	const waiters = 8
	var wg sync.WaitGroup
	for range waiters {
		ch := s.Changed()
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-ch
		}()
	}
	s.Fire()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("not every waiter was woken by one Fire")
	}
}

func TestChangeSignal_ConcurrentFireAndChanged(t *testing.T) {
	s := NewChangeSignal()
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for range 1000 {
				s.Fire()
			}
		}()
		go func() {
			defer wg.Done()
			for range 1000 {
				_ = s.Changed()
			}
		}()
	}
	wg.Wait()
}
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./internal/common/ -run 'TestChangeSignal'`
Expected: FAIL — build error `undefined: NewChangeSignal`.

- [ ] **Step 3: Implement**

```go
package common

import "sync"

// ChangeSignal tells any number of waiters that something changed, without a
// timer loop. Changed returns a channel; the next Fire closes it and installs
// a fresh one.
//
// A waiter takes the channel BEFORE it looks at the state it cares about, and
// waits on it only if the state was not what it wanted. A change between the
// look and the wait has then already closed the channel it holds, so no
// wake-up is lost.
type ChangeSignal struct {
	mu sync.Mutex
	ch chan struct{}
}

// NewChangeSignal returns a signal whose first channel is open.
func NewChangeSignal() *ChangeSignal {
	return &ChangeSignal{ch: make(chan struct{})}
}

// Changed returns the channel the next Fire will close.
func (s *ChangeSignal) Changed() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ch
}

// Fire wakes every waiter holding the current channel and replaces it.
func (s *ChangeSignal) Fire() {
	s.mu.Lock()
	defer s.mu.Unlock()
	close(s.ch)
	s.ch = make(chan struct{})
}
```

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./internal/common/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```
git add internal/common/changesignal.go internal/common/changesignal_test.go && git commit -m "feat(common): ChangeSignal, a channel closed and replaced on every change

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task M-2: list versions, the acceptance rule and the tag store

**Spec:** §11 "Lists" — the version a pnode announces is the authority;
equality, never ordering across epochs; a pnode never accepts a foreign copy of
its own list; *Receive*; *Leave*; "Tags are sorted".

**Files:**
- Create: `internal/cluster/registry/tags.go`
- Test: `internal/cluster/registry/tags_internal_test.go`

**Interfaces:**
- Consumes: `common.NewChangeSignal`, `(*common.ChangeSignal).Fire` (M-1).
- Produces (package-private, used by M-3 … M-7):
  - `type listVersion struct { Epoch int64; Seq uint64 }` (JSON `e`, `s`)
  - `func acceptList(msg, announced listVersion, haveAnnounced bool, held listVersion, haveHeld bool) bool`
  - `func isCurrent(held listVersion, haveHeld bool, announced listVersion) bool`
  - `func newTagStore(self string, epoch int64, signal *common.ChangeSignal) *tagStore`
  - `func (s *tagStore) setOwn(tags map[string][]string, announce func(listVersion) error) (changed bool, err error)`
  - `func (s *tagStore) ownList() (listVersion, map[string][]string)`
  - `func (s *tagStore) put(node string, version listVersion, tags map[string][]string, announced listVersion, haveAnnounced bool) (stored bool)`
  - `func (s *tagStore) current(node string, announced listVersion) bool`
  - `func (s *tagStore) tagsOf(node string) map[string][]string`
  - `func (s *tagStore) drop(node string) bool`
  - `func (s *tagStore) retain(alive map[string]struct{}) int`

- [ ] **Step 1: Write the failing tests**

```go
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
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./internal/cluster/registry/ -run 'TestAcceptList|TestIsCurrent|TestTagStore'`
Expected: FAIL — build error `undefined: listVersion` (and `acceptList`, `newTagStore`).

- [ ] **Step 3: Implement** — `internal/cluster/registry/tags.go`

```go
package registry

import (
	"maps"
	"slices"
	"sync"

	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// listVersion names one state of one pnode's tag list. Epoch is the pnode's
// process start in unix nanoseconds; it only has to differ between two lives
// of the same pnode, and epochs of different pnodes are never compared. Seq
// counts the changes within one life.
//
// Versions are compared for equality. They are ordered only within one epoch.
// A pnode that restarts under the same id with a clock that stepped backwards
// announces a lower epoch than its previous life, and must not be mistaken
// for an older self.
type listVersion struct {
	Epoch int64  `json:"e"`
	Seq   uint64 `json:"s"`
}

type heldList struct {
	version listVersion
	tags    map[string][]string
}

// acceptList decides whether a received list replaces what is held for its
// pnode. The version the pnode announces in its metadata is the authority: a
// list is taken when it carries exactly that version, or when it is a later
// seq of the announced epoch (it arrived before its metadata did) and also
// later than the seq already held (two such lists can arrive out of order).
// With nothing announced there is nothing to measure the list against, and
// the join event for that pnode fetches it.
func acceptList(msg, announced listVersion, haveAnnounced bool, held listVersion, haveHeld bool) bool {
	if !haveAnnounced {
		return false
	}
	if msg == announced {
		return true
	}
	if msg.Epoch != announced.Epoch || msg.Seq < announced.Seq {
		return false
	}
	if haveHeld && held.Epoch == msg.Epoch && msg.Seq <= held.Seq {
		return false
	}
	return true
}

// isCurrent reports whether the held list needs no fetch: it is the announced
// one, or a later seq of the announced epoch whose metadata has not arrived.
func isCurrent(held listVersion, haveHeld bool, announced listVersion) bool {
	if !haveHeld {
		return false
	}
	if held == announced {
		return true
	}
	return held.Epoch == announced.Epoch && held.Seq > announced.Seq
}

// normaliseTags returns a private copy with every tag slice sorted and
// de-duplicated, so that one set of tags has one representation. A tenant
// with no tags is kept: its cnode still serves callouts that ask for no tag.
func normaliseTags(in map[string][]string) map[string][]string {
	out := make(map[string][]string, len(in))
	for tenant, tags := range in {
		cp := make([]string, len(tags))
		copy(cp, tags)
		slices.Sort(cp)
		out[tenant] = slices.Compact(cp)
	}
	return out
}

func copyTags(in map[string][]string) map[string][]string {
	out := make(map[string][]string, len(in))
	for tenant, tags := range in {
		out[tenant] = slices.Clone(tags)
	}
	return out
}

func tagsEqual(a, b map[string][]string) bool {
	return maps.EqualFunc(a, b, func(x, y []string) bool { return slices.Equal(x, y) })
}

// tagStore holds this pnode's own list and the lists it holds for its peers.
// Stored maps are never mutated after they are stored; readers get copies.
type tagStore struct {
	self   string
	signal *common.ChangeSignal

	mu   sync.RWMutex
	own  heldList
	held map[string]heldList
}

func newTagStore(self string, epoch int64, signal *common.ChangeSignal) *tagStore {
	return &tagStore{
		self:   self,
		signal: signal,
		own:    heldList{version: listVersion{Epoch: epoch}, tags: map[string][]string{}},
		held:   make(map[string]heldList),
	}
}

// setOwn replaces this pnode's list. The next version, the announcement of it
// and the list are one locked step, so one version never names two lists. An
// equal set changes nothing and announces nothing. announce runs under the
// store's lock and must not call back into the store.
func (s *tagStore) setOwn(tags map[string][]string, announce func(listVersion) error) (bool, error) {
	norm := normaliseTags(tags)
	s.mu.Lock()
	defer s.mu.Unlock()
	if tagsEqual(s.own.tags, norm) {
		return false, nil
	}
	next := listVersion{Epoch: s.own.version.Epoch, Seq: s.own.version.Seq + 1}
	if err := announce(next); err != nil {
		return false, err
	}
	s.own = heldList{version: next, tags: norm}
	return true, nil
}

// ownList returns this pnode's version and a copy of its list, read together.
func (s *tagStore) ownList() (listVersion, map[string][]string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.own.version, copyTags(s.own.tags)
}

// put stores a received list if acceptList allows it, and fires the change
// signal when the tags held for that pnode are different afterwards. A list
// naming this pnode is never accepted.
func (s *tagStore) put(node string, version listVersion, tags map[string][]string, announced listVersion, haveAnnounced bool) bool {
	if node == s.self {
		return false
	}
	norm := normaliseTags(tags)
	stored, changed := func() (bool, bool) {
		s.mu.Lock()
		defer s.mu.Unlock()
		cur, have := s.held[node]
		if !acceptList(version, announced, haveAnnounced, cur.version, have) {
			return false, false
		}
		s.held[node] = heldList{version: version, tags: norm}
		return true, !have || !tagsEqual(cur.tags, norm)
	}()
	if changed {
		s.signal.Fire()
	}
	return stored
}

// current reports whether the list held for node needs no fetch.
func (s *tagStore) current(node string, announced listVersion) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cur, have := s.held[node]
	return isCurrent(cur.version, have, announced)
}

// tagsOf returns a copy of the tags held for node — this pnode's own list for
// its own id — and an empty map when none is held yet.
func (s *tagStore) tagsOf(node string) map[string][]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if node == s.self {
		return copyTags(s.own.tags)
	}
	return copyTags(s.held[node].tags)
}

// drop forgets the list of a pnode that left.
func (s *tagStore) drop(node string) bool {
	dropped := func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		_, have := s.held[node]
		delete(s.held, node)
		return have
	}()
	if dropped {
		s.signal.Fire()
	}
	return dropped
}

// retain forgets every held list whose pnode is not in alive, and returns how
// many it dropped. It is the scan's repair for a leave event that was lost.
func (s *tagStore) retain(alive map[string]struct{}) int {
	n := func() int {
		s.mu.Lock()
		defer s.mu.Unlock()
		n := 0
		for node := range s.held {
			if _, ok := alive[node]; !ok {
				delete(s.held, node)
				n++
			}
		}
		return n
	}()
	if n > 0 {
		s.signal.Fire()
	}
	return n
}
```

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./internal/cluster/registry/...`
Expected: PASS (the new tests and every existing gossip test; nothing uses the
store yet).

- [ ] **Step 5: Commit**

```
git add internal/cluster/registry/tags.go internal/cluster/registry/tags_internal_test.go && git commit -m "feat(registry): tag list versions, the acceptance rule and the tag store

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task M-3: tenants and tags leave the node metadata

**Spec:** §11 "Defect", "Metadata", *Publish*, *Receive*, "`List` returns each
member's identity with its held tags", "`UpdateTags`' two bare `Unlock()`",
"Nothing calls into memberlist from inside one of its callbacks". D11.
Design decision 6 (the member directory) lands here, because this task is what
exposes the race it removes.

**M-3 and M-4 land together.** After M-3 alone a pnode that joins *after* a
list was sent never learns it (today the metadata carried it). M-4 closes that;
do not ship M-3 without it. M-3 on its own is green and race-clean (checked).

**Files:**
- Create: `internal/cluster/registry/directory.go`
- Create: `internal/cluster/registry/tags_worker.go`
- Modify: `internal/cluster/registry/gossip.go` — `nodeMeta`, `Gossip`,
  `NewGossip`, `Lookup`, `List`, `Deregister`, `UpdateTags`,
  `gossipDelegate.NodeMeta`, `gossipDelegate.NotifyMsg` (lines 34-49, 55-98,
  183-234, 249-315 as found)
- Create: `internal/cluster/registry/helpers_test.go`
- Test: `internal/cluster/registry/gossip_tags_test.go`,
  `internal/cluster/registry/directory_internal_test.go`
- Modify: `internal/cluster/registry/gossip_test.go:79-81` (the `fmt.Printf`
  diagnostic in `TestGossipRegistry_TwoNodes` becomes `t.Logf`; the `fmt`
  import goes)

**Existing tests and the decision for each:**
- `TestGossipRegistry_TagPropagation` (`gossip_test.go:85`) — **stays
  unchanged**. It pins the behaviour (a peer sees the tags), not the channel,
  and must pass over the new one — under `-race` too, which is what the
  directory is for.
- `TestGossipRegistry_TwoNodes`, `_GRPCAddrPropagation`, `_LookupUnknown`,
  `TestGossipRegistry_RegisterHonors*`, `TestGossipBroadcaster_*`,
  `TestEncodeTopicMsg_*` — stay. The topic framing is not changed;
  `NotifyMsg` changes only in how it releases its lock.
- No test pins the oversize WARN path (R§12); nothing to delete with it.

**Interfaces:**
- Consumes: M-1, M-2.
- Produces:
  - `registry.GossipConfig` unchanged in this task.
  - `func (g *Gossip) UpdateTags(tags map[string][]string) error` — same
    signature; now returns after the locked step, never blocks on the network.
  - `NewGossip` returns an error naming `CYODA_NODE_ID`, `CYODA_NODE_ADDR` and
    `CYODA_GRPC_NODE_ADDR` when the identity cannot fit.
  - `Deregister` is idempotent.
  - Wire: topic `cluster.tags`, payload `tagListMsg{NodeID n, Version v, Tags t}`.
  - Package-private: `directory` (`newDirectory`, `set`, `remove`, `get`,
    `all`); `tagEvents` (`newTagEvents(dir *directory)`, a
    `memberlist.EventDelegate`); `parseMeta`, `marshalMeta`,
    `(*Gossip).member`, `(*Gossip).announced`, `(*Gossip).sendAsync`,
    `encodeTagMsg`.

- [ ] **Step 1: Write the failing tests**

`internal/cluster/registry/helpers_test.go`:

```go
package registry_test

import (
	"context"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/registry"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// gossipCfg is the configuration the two-pnode tests share: loopback, a short
// stability window, an HTTP address derived from the id.
func gossipCfg(id string, port int, seeds ...string) registry.GossipConfig {
	return registry.GossipConfig{
		NodeID:          id,
		NodeAddr:        "http://" + id + ".test:8080",
		BindAddr:        "127.0.0.1",
		BindPort:        port,
		Seeds:           seeds,
		StabilityWindow: 200 * time.Millisecond,
	}
}

// startGossip creates and joins one pnode and leaves the cluster at cleanup.
func startGossip(t *testing.T, cfg registry.GossipConfig) *registry.Gossip {
	t.Helper()
	r, err := registry.NewGossip(cfg)
	if err != nil {
		t.Fatalf("NewGossip %s: %v", cfg.NodeID, err)
	}
	t.Cleanup(func() { _ = r.Deregister(context.Background(), cfg.NodeID) })
	if err := r.Register(context.Background(), cfg.NodeID, cfg.NodeAddr); err != nil {
		t.Fatalf("Register %s: %v", cfg.NodeID, err)
	}
	return r
}

// eventually polls cond until it holds or the time is up.
func eventually(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("not within %v: %s", within, what)
}

// nodeIn returns the entry for id in r's view.
func nodeIn(t *testing.T, r *registry.Gossip, id string) (contract.NodeInfo, bool) {
	t.Helper()
	nodes, err := r.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, n := range nodes {
		if n.NodeID == id {
			return n, true
		}
	}
	return contract.NodeInfo{}, false
}
```

`internal/cluster/registry/gossip_tags_test.go`:

```go
package registry_test

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/registry"
)

// TestGossipRegistry_ManyTenantsStayVisible reproduces the membership defect.
// Twelve tenants with 36-character ids and one 10-character tag each need
// about 81 + 12×(36+18) = 729 bytes in the old metadata, past memberlist's
// 512; the pnode then published nil metadata and vanished from every view,
// its own included.
func TestGossipRegistry_ManyTenantsStayVisible(t *testing.T) {
	ctx := context.Background()
	r1 := startGossip(t, gossipCfg("many-1", 23946))
	r2 := startGossip(t, gossipCfg("many-2", 23947, "127.0.0.1:23946"))

	want := make(map[string][]string, 12)
	for i := range 12 {
		want[fmt.Sprintf("0b7e2c54-9a1d-4f3e-8c6b-%012d", i)] = []string{"compute-01"}
	}
	if err := r1.UpdateTags(want); err != nil {
		t.Fatalf("UpdateTags: %v", err)
	}

	eventually(t, 5*time.Second, "many-2 sees all twelve tenants of many-1", func() bool {
		n, ok := nodeIn(t, r2, "many-1")
		return ok && reflect.DeepEqual(n.Tags, want)
	})

	self, ok := nodeIn(t, r1, "many-1")
	if !ok {
		t.Fatal("many-1 dropped out of its own view")
	}
	if !reflect.DeepEqual(self.Tags, want) {
		t.Errorf("many-1's own tags = %v, want all twelve tenants", self.Tags)
	}

	addr, alive, err := r2.Lookup(ctx, "many-1")
	if err != nil || !alive || addr != "http://many-1.test:8080" {
		t.Errorf("Lookup(many-1) from many-2 = (%q, %v, %v), want the address, alive, nil", addr, alive, err)
	}
}

func TestGossipRegistry_TagsAreReplacedAndRemoved(t *testing.T) {
	r1 := startGossip(t, gossipCfg("repl-1", 23948))
	r2 := startGossip(t, gossipCfg("repl-2", 23949, "127.0.0.1:23948"))

	if err := r1.UpdateTags(map[string][]string{"tenant-a": {"python"}, "tenant-b": {"go"}}); err != nil {
		t.Fatalf("UpdateTags: %v", err)
	}
	eventually(t, 5*time.Second, "repl-2 sees both tenants", func() bool {
		n, _ := nodeIn(t, r2, "repl-1")
		return len(n.Tags) == 2
	})

	// tenant-b's last cnode detached.
	if err := r1.UpdateTags(map[string][]string{"tenant-a": {"python"}}); err != nil {
		t.Fatalf("UpdateTags: %v", err)
	}
	eventually(t, 5*time.Second, "repl-2 sees tenant-b gone", func() bool {
		n, _ := nodeIn(t, r2, "repl-1")
		return reflect.DeepEqual(n.Tags, map[string][]string{"tenant-a": {"python"}})
	})
}

func TestNewGossip_IdentityTooLarge_RefusesToStart(t *testing.T) {
	cfg := gossipCfg("identity-too-large", 23950)
	cfg.NodeAddr = "http://" + strings.Repeat("a", 600) + ".test:8080"

	r, err := registry.NewGossip(cfg)
	if err == nil {
		_ = r.Deregister(context.Background(), cfg.NodeID)
		t.Fatal("NewGossip accepted an identity that cannot fit the cluster metadata; the pnode would run invisibly")
	}
	for _, setting := range []string{"CYODA_NODE_ID", "CYODA_NODE_ADDR", "CYODA_GRPC_NODE_ADDR"} {
		if !strings.Contains(err.Error(), setting) {
			t.Errorf("error %q does not name %s", err, setting)
		}
	}
}

func TestNewGossip_LargestIdentityThatFits_Starts(t *testing.T) {
	// The check uses the longest version there can be, so an identity that
	// passes it at startup passes it for the life of the process.
	cfg := gossipCfg("identity-fits", 23951)
	cfg.NodeAddr = "http://" + strings.Repeat("a", 300) + ".test:8080"
	r := startGossip(t, cfg)
	if _, ok := nodeIn(t, r, "identity-fits"); !ok {
		t.Fatal("the pnode is missing from its own view")
	}
}

func TestGossipRegistry_DeregisterTwice(t *testing.T) {
	r := startGossip(t, gossipCfg("dereg-twice", 23952))
	if err := r.Deregister(context.Background(), "dereg-twice"); err != nil {
		t.Fatalf("first Deregister: %v", err)
	}
	// startGossip's cleanup makes it three; none may panic.
	if err := r.Deregister(context.Background(), "dereg-twice"); err != nil {
		t.Fatalf("second Deregister: %v", err)
	}
}
```

`internal/cluster/registry/directory_internal_test.go`:

```go
package registry

import (
	"net"
	"sync"
	"testing"

	"github.com/hashicorp/memberlist"
)

func TestDirectory_SetKeepsAPrivateCopy(t *testing.T) {
	d := newDirectory()
	n := &memberlist.Node{Name: "peer", Addr: net.ParseIP("10.0.0.1"), Port: 7946, Meta: []byte(`{"id":"peer"}`)}
	d.set(n)

	// memberlist rewrites the node it handed over once the callback returns.
	copy(n.Meta, `XXXXXXXXXXXXX`)
	n.Addr[len(n.Addr)-1] = 99
	n.Port = 1

	got, ok := d.get("peer")
	if !ok {
		t.Fatal("peer is missing from the directory")
	}
	if string(got.Meta) != `{"id":"peer"}` {
		t.Errorf("Meta = %q; the directory kept memberlist's slice", got.Meta)
	}
	if !got.Addr.Equal(net.ParseIP("10.0.0.1")) || got.Port != 7946 {
		t.Errorf("address = %v:%d; the directory kept memberlist's node", got.Addr, got.Port)
	}
}

func TestDirectory_SetReplacesAndRemoveForgets(t *testing.T) {
	d := newDirectory()
	d.set(&memberlist.Node{Name: "a", Meta: []byte("one")})
	d.set(&memberlist.Node{Name: "b", Meta: []byte("b")})
	d.set(&memberlist.Node{Name: "a", Meta: []byte("two")})

	if got, _ := d.get("a"); string(got.Meta) != "two" {
		t.Errorf("a's Meta = %q, want the later one", got.Meta)
	}
	if n := len(d.all()); n != 2 {
		t.Errorf("all returned %d members, want 2", n)
	}

	d.remove("a")
	if _, ok := d.get("a"); ok {
		t.Error("a is still in the directory after remove")
	}
	d.remove("never-there")
	if n := len(d.all()); n != 1 {
		t.Errorf("all returned %d members, want 1", n)
	}
}

func TestDirectory_AllHandsOutDistinctCopies(t *testing.T) {
	d := newDirectory()
	d.set(&memberlist.Node{Name: "a"})
	d.set(&memberlist.Node{Name: "b"})

	all := d.all()
	if len(all) != 2 || all[0] == all[1] || all[0].Name == all[1].Name {
		t.Fatalf("all = %v; want two distinct members", all)
	}
	all[0].Name = "changed-by-reader"
	if _, ok := d.get("changed-by-reader"); ok {
		t.Error("a reader renamed a member inside the directory")
	}
}

func TestDirectory_ConcurrentUse(t *testing.T) {
	d := newDirectory()
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for range 500 {
				d.set(&memberlist.Node{Name: "peer", Meta: []byte("m")})
				d.remove("peer")
			}
		}()
		go func() {
			defer wg.Done()
			for range 500 {
				_, _ = d.get("peer")
				_ = d.all()
			}
		}()
	}
	wg.Wait()
}
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./internal/cluster/registry/`
Expected: FAIL — build error `undefined: newDirectory`.

With `directory_internal_test.go` set aside, the black-box tests fail against
today's code as follows (run and confirmed while writing this plan):
- `TestGossipRegistry_ManyTenantsStayVisible` — "not within 5s: many-2 sees
  all twelve tenants of many-1"; the log shows `node metadata exceeds limit
  metaLen=705 limit=512`.
- `TestNewGossip_IdentityTooLarge_RefusesToStart` — "NewGossip accepted an
  identity that cannot fit the cluster metadata…".
- `TestGossipRegistry_DeregisterTwice` — `panic: leave after shutdown`.
- `TestGossipRegistry_TagsAreReplacedAndRemoved` and
  `TestNewGossip_LargestIdentityThatFits_Starts` pass today; they pin behaviour
  the rewrite must keep.

- [ ] **Step 3: Implement**

`internal/cluster/registry/directory.go` (new):

```go
package registry

import (
	"bytes"
	"slices"
	"sync"

	"github.com/hashicorp/memberlist"
)

// directory is this pnode's own copy of the alive members, kept up to date by
// memberlist's event callbacks.
//
// It exists because memberlist.Members() hands out pointers into memberlist's
// node map, and memberlist rewrites those nodes' Meta, Addr and Port under a
// lock no caller can take: reading them from another goroutine is a data race
// on a slice header. Inside NotifyJoin, NotifyUpdate and NotifyLeave the node
// is handed over under that lock, so a copy taken there is safe, and it is the
// only place one can be taken. Everything else in this package reads members
// from here and never from memberlist.
//
// The callbacks run under memberlist's node lock, so set and remove do nothing
// but copy into a map under a mutex of their own, which is never held across a
// memberlist call or any I/O. Being state rather than a queue of events, the
// directory cannot lose a join or a leave.
type directory struct {
	mu      sync.RWMutex
	members map[string]memberlist.Node
}

func newDirectory() *directory {
	return &directory{members: make(map[string]memberlist.Node)}
}

// set records a private copy of n. It must be called from a memberlist event
// callback, where n is stable.
func (d *directory) set(n *memberlist.Node) {
	cp := *n
	cp.Addr = slices.Clone(n.Addr)
	cp.Meta = bytes.Clone(n.Meta)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.members[cp.Name] = cp
}

func (d *directory) remove(name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.members, name)
}

// get returns a copy of the member called name. Its Meta and Addr are shared
// with the directory's copy and are never written after set.
func (d *directory) get(name string) (*memberlist.Node, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	n, ok := d.members[name]
	if !ok {
		return nil, false
	}
	return &n, true
}

// all returns a copy of every alive member.
func (d *directory) all() []*memberlist.Node {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]*memberlist.Node, 0, len(d.members))
	for _, n := range d.members {
		out = append(out, &n)
	}
	return out
}
```

`internal/cluster/registry/tags_worker.go` (new):

```go
package registry

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/hashicorp/memberlist"
)

// topicTags carries one pnode's complete tag list to a peer over memberlist's
// reliable channel. memberlist delivers reliable user messages and gossip
// broadcasts to the same NotifyMsg, so it uses the topic framing of
// Broadcast/Subscribe.
const topicTags = "cluster.tags"

const (
	// tagEventQueueDepth bounds what memberlist's callbacks may leave for the
	// worker. A full queue drops the event; the scan makes it good.
	tagEventQueueDepth = 256
	// metaBroadcastTimeout bounds the wait for the metadata broadcast. On a
	// timeout the broadcast stays queued in memberlist and still goes out.
	metaBroadcastTimeout = 5 * time.Second
)

// tagListMsg is the payload of topicTags.
type tagListMsg struct {
	NodeID  string              `json:"n"`
	Version listVersion         `json:"v"`
	Tags    map[string][]string `json:"t,omitempty"`
}

type tagEventKind int

const (
	evList tagEventKind = iota
)

type tagEvent struct {
	kind    tagEventKind
	payload []byte // a copy of the message payload
}

// tagEvents is everything memberlist's callbacks are allowed to do: copy what
// they were given — into the directory, or into the worker's queue — and
// return. No method blocks and none calls into memberlist. NotifyJoin,
// NotifyUpdate and NotifyLeave run under memberlist's node lock, where a send
// stalls all membership processing for up to the TCP timeout and Members or
// UpdateNode deadlocks.
type tagEvents struct {
	dir *directory
	ch  chan tagEvent
}

var _ memberlist.EventDelegate = (*tagEvents)(nil)

func newTagEvents(dir *directory) *tagEvents {
	return &tagEvents{dir: dir, ch: make(chan tagEvent, tagEventQueueDepth)}
}

func (q *tagEvents) offer(ev tagEvent) {
	select {
	case q.ch <- ev:
	default:
	}
}

func (q *tagEvents) NotifyJoin(n *memberlist.Node)   { q.dir.set(n) }
func (q *tagEvents) NotifyUpdate(n *memberlist.Node) { q.dir.set(n) }
func (q *tagEvents) NotifyLeave(n *memberlist.Node)  { q.dir.remove(n.Name) }

// onList is the topicTags handler. memberlist reuses the buffer it passes to
// NotifyMsg, so the payload is copied before the handler returns.
func (q *tagEvents) onList(payload []byte) {
	q.offer(tagEvent{kind: evList, payload: bytes.Clone(payload)})
}

func encodeTagMsg(topic string, v any) []byte {
	payload, err := json.Marshal(v)
	if err != nil {
		slog.Error("failed to marshal tag list message",
			"pkg", "cluster/registry", "topic", topic, "err", err)
		return nil
	}
	return encodeTopicMsg(topic, payload)
}

// runTagWorker is the one goroutine that stores lists and starts sends. It is
// started after memberlist.Create returns, so g.list is set before it runs.
func (g *Gossip) runTagWorker() {
	defer close(g.done)
	for {
		select {
		case <-g.stop:
			return
		case <-g.publish:
			g.publishOwnList()
		case ev := <-g.events.ch:
			g.handleTagEvent(ev)
		}
	}
}

func (g *Gossip) handleTagEvent(ev tagEvent) {
	switch ev.kind {
	case evList:
		g.handleList(ev.payload)
	}
}

// publishOwnList re-advertises the metadata, which now names the new version,
// and sends the list to every other alive pnode. Neither waits on the worker.
func (g *Gossip) publishOwnList() {
	version, tags := g.tags.ownList()
	go func() {
		if err := g.list.UpdateNode(metaBroadcastTimeout); err != nil {
			slog.Debug("metadata broadcast still queued",
				"pkg", "cluster/registry", "nodeId", g.cfg.NodeID, "err", err)
		}
	}()
	msg := encodeTagMsg(topicTags, tagListMsg{NodeID: g.cfg.NodeID, Version: version, Tags: tags})
	peers := 0
	for _, m := range g.dir.all() {
		if m.Name == g.cfg.NodeID {
			continue
		}
		peers++
		g.sendAsync(m, "list", msg)
	}
	slog.Debug("published tag list",
		"pkg", "cluster/registry", "nodeId", g.cfg.NodeID,
		"seq", version.Seq, "tenants", len(tags), "peers", peers)
}

// sendAsync sends one reliable message without holding up the caller: a dial
// to a pnode that has just died hangs for memberlist's TCP timeout. to is a
// copy from the directory, never a pointer into memberlist's node map.
func (g *Gossip) sendAsync(to *memberlist.Node, kind string, msg []byte) {
	if msg == nil {
		return
	}
	go func() {
		err := g.list.SendReliable(to, msg)
		if err == nil {
			return
		}
		select {
		case <-g.stop:
			return // shutting down; the send was never going to matter
		default:
		}
		slog.Warn("failed to send tag list message; the peer will fetch it",
			"pkg", "cluster/registry", "peer", to.Name, "msg", kind, "err", err)
	}()
}

func (g *Gossip) handleList(payload []byte) {
	var msg tagListMsg
	if err := json.Unmarshal(payload, &msg); err != nil {
		slog.Warn("malformed tag list message",
			"pkg", "cluster/registry", "size", len(payload), "err", err)
		return
	}
	_, announced, ok := g.announced(msg.NodeID)
	stored := g.tags.put(msg.NodeID, msg.Version, msg.Tags, announced, ok)
	slog.Debug("tag list received",
		"pkg", "cluster/registry", "peer", msg.NodeID,
		"seq", msg.Version.Seq, "tenants", len(msg.Tags), "stored", stored)
}

// member returns a copy of the alive member called name.
func (g *Gossip) member(name string) (*memberlist.Node, bool) {
	return g.dir.get(name)
}

// announced returns the alive member called name and the list version its
// metadata announces. ok is false for a pnode that is not alive or whose
// metadata does not parse.
func (g *Gossip) announced(name string) (*memberlist.Node, listVersion, bool) {
	m, ok := g.member(name)
	if !ok {
		return nil, listVersion{}, false
	}
	nm, err := parseMeta(m.Meta)
	if err != nil {
		return nil, listVersion{}, false
	}
	return m, nm.Tags, true
}
```

`internal/cluster/registry/gossip.go` — changed regions.

Imports: add `"errors"` and `"github.com/cyoda-platform/cyoda-go/internal/common"`.

`nodeMeta` and `Gossip` (were lines 34-49):

```go
// nodeMeta is serialized as JSON in memberlist node metadata. It holds the
// pnode's identity and the version of its tag list — nothing that grows. The
// list itself travels by reliable message (see tags_worker.go).
type nodeMeta struct {
	ID       string      `json:"id"`
	Addr     string      `json:"addr"`
	GRPCAddr string      `json:"grpcAddr,omitempty"`
	Tags     listVersion `json:"tv"`
}

func marshalMeta(cfg GossipConfig, version listVersion) ([]byte, error) {
	return json.Marshal(nodeMeta{ID: cfg.NodeID, Addr: cfg.NodeAddr, GRPCAddr: cfg.GRPCNodeAddr, Tags: version})
}

// parseMeta decodes a member's metadata. Metadata without an id is not a
// pnode of this cluster and counts as unparseable.
func parseMeta(raw []byte) (nodeMeta, error) {
	var nm nodeMeta
	if err := json.Unmarshal(raw, &nm); err != nil {
		return nodeMeta{}, err
	}
	if nm.ID == "" {
		return nodeMeta{}, errors.New("metadata carries no node id")
	}
	return nm, nil
}

// checkIdentityFits refuses an identity that cannot fit memberlist's metadata.
// The size depends only on operator settings and on the digits of the version,
// so it is measured once, with the longest version there can be.
func checkIdentityFits(cfg GossipConfig) error {
	largest, err := marshalMeta(cfg, listVersion{Epoch: math.MaxInt64, Seq: math.MaxUint64})
	if err != nil {
		return fmt.Errorf("failed to marshal node metadata: %w", err)
	}
	if len(largest) > memberlist.MetaMaxSize {
		return fmt.Errorf("node identity needs %d bytes of cluster metadata and the limit is %d: shorten CYODA_NODE_ID, CYODA_NODE_ADDR or CYODA_GRPC_NODE_ADDR",
			len(largest), memberlist.MetaMaxSize)
	}
	return nil
}

// Gossip is a NodeRegistry backed by hashicorp/memberlist.
type Gossip struct {
	cfg      GossipConfig
	list     *memberlist.Memberlist
	delegate *gossipDelegate
	dir      *directory
	tags     *tagStore
	events   *tagEvents

	publish chan struct{} // one slot: this pnode's list changed
	stop    chan struct{}
	done    chan struct{}

	deregisterOnce sync.Once
	deregisterErr  error
}
```

`NewGossip` (was 55-98) — before:

```go
	nm := nodeMeta{ID: cfg.NodeID, Addr: cfg.NodeAddr, GRPCAddr: cfg.GRPCNodeAddr}
	metaBytes, err := json.Marshal(nm)
	if err != nil {
		return nil, fmt.Errorf("marshal node metadata: %w", err)
	}

	del := &gossipDelegate{
		meta: metaBytes,
		subs: make(map[string][]func([]byte)),
	}
	g := &Gossip{cfg: cfg, delegate: del, meta: nm}
```

after:

```go
	if err := checkIdentityFits(cfg); err != nil {
		return nil, err
	}
	// The epoch only has to differ between two lives of this pnode.
	epoch := time.Now().UnixNano()
	metaBytes, err := marshalMeta(cfg, listVersion{Epoch: epoch})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal node metadata: %w", err)
	}

	del := &gossipDelegate{
		meta: metaBytes,
		subs: make(map[string][]func([]byte)),
	}
	dir := newDirectory()
	events := newTagEvents(dir)
	del.subscribe(topicTags, events.onList)
	g := &Gossip{
		cfg:      cfg,
		delegate: del,
		dir:      dir,
		tags:     newTagStore(cfg.NodeID, epoch, common.NewChangeSignal()),
		events:   events,
		publish:  make(chan struct{}, 1),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
```

beside `mlCfg.Delegate = del`:

```go
	// Registered before Create: NotifyJoin fires for this pnode inside it, and
	// the directory must hold every member from the first one on.
	mlCfg.Events = events
```

and, directly after `del.queue = …` is set:

```go
	// Started only now: the worker needs g.list. What the callbacks left in
	// the queue in between is still there.
	go g.runTagWorker()
```

`Lookup` and `List` (were 183-219) read the directory, never
`g.list.Members()`. `Lookup`'s error branch is kept here — M-6 changes it under
its own test.

```go
func (g *Gossip) Lookup(_ context.Context, nodeID string) (string, bool, error) {
	m, ok := g.dir.get(nodeID)
	if !ok {
		return "", false, nil
	}
	nm, err := parseMeta(m.Meta)
	if err != nil {
		return "", false, fmt.Errorf("unmarshal metadata for %s: %w", nodeID, err)
	}
	return nm.Addr, true, nil
}

// List returns all alive members with their identity and the tags held for
// them — empty until a member's list has arrived.
func (g *Gossip) List(_ context.Context) ([]contract.NodeInfo, error) {
	members := g.dir.all()
	nodes := make([]contract.NodeInfo, 0, len(members))
	for _, m := range members {
		nm, err := parseMeta(m.Meta)
		if err != nil {
			slog.Warn("skipping member with bad metadata",
				"pkg", "cluster/registry",
				"memberName", m.Name,
				"err", err,
			)
			continue
		}
		nodes = append(nodes, contract.NodeInfo{
			NodeID:   nm.ID,
			Addr:     nm.Addr,
			GRPCAddr: nm.GRPCAddr,
			Alive:    true,
			Tags:     g.tags.tagsOf(m.Name),
		})
	}
	return nodes, nil
}
```

`Deregister` (was 222-234):

```go
// Deregister stops the tag worker and gracefully leaves the cluster. A second
// call returns the first call's result.
func (g *Gossip) Deregister(_ context.Context, _ string) error {
	g.deregisterOnce.Do(func() {
		close(g.stop)
		<-g.done
		if err := g.list.Leave(5 * time.Second); err != nil {
			g.deregisterErr = fmt.Errorf("leave cluster: %w", err)
			return
		}
		if err := g.list.Shutdown(); err != nil {
			g.deregisterErr = fmt.Errorf("shutdown memberlist: %w", err)
			return
		}
		slog.Info("left cluster",
			"pkg", "cluster/registry",
			"nodeId", g.cfg.NodeID,
		)
	})
	return g.deregisterErr
}
```

`UpdateTags` (was 249-262) — before:

```go
func (g *Gossip) UpdateTags(tags map[string][]string) error {
	g.mu.Lock()
	g.meta.Tags = tags
	metaBytes, err := json.Marshal(g.meta)
	if err != nil {
		g.mu.Unlock()
		return fmt.Errorf("marshal node metadata: %w", err)
	}
	g.delegate.updateMeta(metaBytes)
	g.mu.Unlock()
	return g.list.UpdateNode(0)
}
```

after:

```go
// UpdateTags replaces this pnode's tag list. The new version, the metadata
// that announces it and the list are one locked step; the network work is the
// worker's, so the caller never waits on a peer. A set equal to the current
// one changes nothing.
func (g *Gossip) UpdateTags(tags map[string][]string) error {
	changed, err := g.tags.setOwn(tags, func(version listVersion) error {
		metaBytes, err := marshalMeta(g.cfg, version)
		if err != nil {
			return fmt.Errorf("failed to marshal node metadata: %w", err)
		}
		g.delegate.updateMeta(metaBytes)
		return nil
	})
	if err != nil {
		return err
	}
	if changed {
		select {
		case g.publish <- struct{}{}:
		default: // a publish is pending; it reads the newest list
		}
	}
	return nil
}
```

`gossipDelegate.NodeMeta` (was 288-300) — the oversize branch is unreachable
after `checkIdentityFits` and is deleted:

```go
func (d *gossipDelegate) NodeMeta(int) []byte {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.meta
}
```

`gossipDelegate.NotifyMsg` (was 302-315) — the bare `RUnlock`, before:

```go
	d.subsMu.RLock()
	handlers := d.subs[topic]
	d.subsMu.RUnlock()
	for _, h := range handlers {
```

after:

```go
	handlers := func() []func([]byte) {
		d.subsMu.RLock()
		defer d.subsMu.RUnlock()
		return d.subs[topic]
	}()
	for _, h := range handlers {
```

The `gossipDelegate` doc comment's "NodeMeta carries per-node identity (ID,
addr, tags)" becomes "(id, addresses, tag list version)".

`gossip_test.go:79-81` — before:

```go
		for _, n := range nodes {
			fmt.Printf("  node: %s addr: %s alive: %v\n", n.NodeID, n.Addr, n.Alive)
		}
```

after (and `"fmt"` leaves the import block):

```go
		for _, n := range nodes {
			t.Logf("  node: %s addr: %s alive: %v", n.NodeID, n.Addr, n.Alive)
		}
```

**TDD waiver, recorded.** "`UpdateNode` … off the publish lock" is a
non-functional property. Asserting it needs either a wall-clock bound on
`UpdateTags` (flaky on a loaded runner) or a seam in production code. It is
reviewed instead: the body of `UpdateTags` contains no call on `g.list`.

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./internal/cluster/registry/...`
Expected: PASS, including the unchanged `TestGossipRegistry_TagPropagation`.

Because this task changes what `-race` sees, run it once here as the rule's
"race-related bug suspected" case allows, then drop it:
`go test -race ./internal/cluster/registry/...` → PASS.

Exit checks:
- `grep -rn 'node metadata exceeds limit' internal/` → no hits
- `grep -n 'Unlock()' internal/cluster/registry/*.go | grep -v defer` → no hits
- `grep -rn 'UpdateNode(0)' internal/cluster/registry/` → no hits
- `grep -n 'json:"tags' internal/cluster/registry/gossip.go` → no hits
- `grep -rn 'fmt.Printf' internal/cluster/registry/` → no hits
- `grep -rn '\.Members()' internal/cluster/registry/ --include='*.go' | grep -v _test` → no hits

- [ ] **Step 5: Commit**

```
git add internal/cluster/registry && git commit -m "fix(registry): tenants and tags leave the 512-byte node metadata (#254)

A pnode with more than a handful of tenants published nil metadata and
vanished from every view, its own included. The metadata now holds identity
and a list version; the list travels by reliable message. An identity that
cannot fit refuses to start. Members are read from a directory kept by
memberlist's event callbacks, never from Members(), whose nodes memberlist
rewrites under its own lock. UpdateTags no longer blocks on the network and
Deregister is idempotent.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task M-4: catching up — fetch on join and update, the scan, and leave

**Spec:** §11 *Catch up*, *Leave*, "Nothing calls into memberlist from inside
one of its callbacks"; *Receive* — "if what is held still differs from what is
announced the pnode fetches again at once" (as refined in Open point 1).

**Files:**
- Modify: `internal/cluster/registry/tags_worker.go` — topics, event kinds,
  `tagEvent`, the three `Notify*` methods, `runTagWorker`, `handleTagEvent`,
  `handleList`; new `onRequest`, `handleMember`, `handleLeave`,
  `handleRequest`, `requestList`, `scanLists`, `ScanIntervalFor`
- Modify: `internal/cluster/registry/gossip.go` — `GossipConfig`
  (`ListScanInterval`), `NewGossip` (the request subscription, the default
  interval)
- Modify: `app/app.go` `mustNewGossip` (as found at 1166-1175)
- Modify: `internal/cluster/registry/helpers_test.go` (`gossipCfg` sets a short
  scan interval)
- Create: `internal/cluster/registry/rawpeer_internal_test.go`
- Test: `internal/cluster/registry/tags_catchup_internal_test.go`,
  `internal/cluster/registry/gossip_catchup_test.go`

**Interfaces:**
- Consumes: M-2, M-3.
- Produces:
  - `GossipConfig.ListScanInterval time.Duration` (zero → 1 s)
  - `func registry.ScanIntervalFor(patience time.Duration) time.Duration`
  - Wire: topic `cluster.tags.request`, payload `tagRequestMsg{From string "from"}`
  - Test helpers for later tasks: `startInternalGossip`, `startRawPeer`,
    `rawMeta`, `(*rawPeer).send`, `waitFor`.

- [ ] **Step 1: Write the failing tests**

`internal/cluster/registry/rawpeer_internal_test.go` — a bare memberlist node
that plays a pnode whose lists get lost. It announces whatever metadata the
test gives it and sends a list only when the test says so.

```go
package registry

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/memberlist"
)

type rawPeer struct {
	name string
	list *memberlist.Memberlist

	mu       sync.Mutex
	meta     []byte
	requests []string     // From of every cluster.tags.request received
	lists    []tagListMsg // every cluster.tags received
}

var _ memberlist.Delegate = (*rawPeer)(nil)

func (p *rawPeer) NodeMeta(int) []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.meta
}

func (p *rawPeer) NotifyMsg(b []byte) {
	topic, payload, ok := decodeTopicMsg(b)
	if !ok {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	switch topic {
	case topicTagsRequest:
		var req tagRequestMsg
		if json.Unmarshal(payload, &req) == nil {
			p.requests = append(p.requests, req.From)
		}
	case topicTags:
		var msg tagListMsg
		if json.Unmarshal(payload, &msg) == nil {
			p.lists = append(p.lists, msg)
		}
	}
}

func (p *rawPeer) GetBroadcasts(int, int) [][]byte { return nil }
func (p *rawPeer) LocalState(bool) []byte          { return nil }
func (p *rawPeer) MergeRemoteState([]byte, bool)   {}

func rawMeta(t *testing.T, name string, version listVersion) []byte {
	t.Helper()
	b, err := json.Marshal(nodeMeta{ID: name, Addr: "http://" + name + ".test:8080", Tags: version})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func startRawPeer(t *testing.T, name string, port int, meta []byte, seed string) *rawPeer {
	t.Helper()
	p := &rawPeer{name: name, meta: meta}
	cfg := memberlist.DefaultLANConfig()
	cfg.Name = name
	cfg.BindAddr = "127.0.0.1"
	cfg.BindPort = port
	cfg.AdvertisePort = port
	cfg.Delegate = p
	cfg.LogOutput = io.Discard
	list, err := memberlist.Create(cfg)
	if err != nil {
		t.Fatalf("create raw peer %s: %v", name, err)
	}
	p.list = list
	t.Cleanup(func() { _ = list.Shutdown() })
	if _, err := list.Join([]string{seed}); err != nil {
		t.Fatalf("raw peer %s join %s: %v", name, seed, err)
	}
	return p
}

func (p *rawPeer) requestCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.requests)
}

func (p *rawPeer) receivedLists() []tagListMsg {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]tagListMsg(nil), p.lists...)
}

// announce replaces the metadata and re-advertises it.
func (p *rawPeer) announce(t *testing.T, meta []byte) {
	t.Helper()
	func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.meta = meta
	}()
	if err := p.list.UpdateNode(2 * time.Second); err != nil {
		t.Logf("raw peer UpdateNode: %v", err)
	}
}

// send addresses the target by its loopback port rather than through
// p.list.Members(), whose nodes memberlist rewrites under its own lock.
func (p *rawPeer) send(t *testing.T, to string, toPort int, topic string, v any) {
	t.Helper()
	target := &memberlist.Node{Name: to, Addr: net.ParseIP("127.0.0.1"), Port: uint16(toPort)}
	if err := p.list.SendReliable(target, encodeTagMsg(topic, v)); err != nil {
		t.Fatalf("raw peer send to %s: %v", to, err)
	}
}

// startInternalGossip starts one pnode. scan is its ListScanInterval: short
// where the scan is under test, an hour where a test must prove that some
// other path made the request.
func startInternalGossip(t *testing.T, id string, port int, scan time.Duration, seeds ...string) *Gossip {
	t.Helper()
	g, err := NewGossip(GossipConfig{
		NodeID:           id,
		NodeAddr:         "http://" + id + ".test:8080",
		BindAddr:         "127.0.0.1",
		BindPort:         port,
		Seeds:            seeds,
		StabilityWindow:  200 * time.Millisecond,
		ListScanInterval: scan,
	})
	if err != nil {
		t.Fatalf("NewGossip %s: %v", id, err)
	}
	t.Cleanup(func() { _ = g.Deregister(context.Background(), id) })
	if err := g.Register(context.Background(), id, ""); err != nil {
		t.Fatalf("Register %s: %v", id, err)
	}
	return g
}

func waitFor(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("not within %v: %s", within, what)
}
```

`internal/cluster/registry/tags_catchup_internal_test.go`:

```go
package registry

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/hashicorp/memberlist"
)

// TestTagEvents_CallbacksNeverBlock is the "slow peer does not stall
// membership events" test. memberlist calls NotifyJoin, NotifyUpdate and
// NotifyLeave under its node lock. With nobody draining the queue — which is
// what a worker held up behind a slow peer looks like, and is the real state
// between memberlist.Create and the worker's start — every callback must
// still return at once, dropping what does not fit.
func TestTagEvents_CallbacksNeverBlock(t *testing.T) {
	q := newTagEvents(newDirectory())
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
	q := newTagEvents(newDirectory())
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

	// A new version announced without its list: the metadata event fetches.
	v2 := listVersion{Epoch: 777, Seq: 2}
	before := p.requestCount()
	p.announce(t, rawMeta(t, "fetch-peer", v2))
	waitFor(t, 5*time.Second, "a request after the announced version moved on", func() bool {
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
	g1 := startInternalGossip(t, "leave-1", 24952, 200*time.Millisecond)
	g2 := startInternalGossip(t, "leave-2", 24953, 200*time.Millisecond, "127.0.0.1:24952")
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
```

`internal/cluster/registry/gossip_catchup_test.go`:

```go
package registry_test

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestGossipRegistry_LateJoinerFetchesList(t *testing.T) {
	r1 := startGossip(t, gossipCfg("late-1", 24954))
	want := map[string][]string{"tenant-a": {"ml", "python"}}
	if err := r1.UpdateTags(want); err != nil {
		t.Fatalf("UpdateTags: %v", err)
	}

	// late-2 was not a member when the list was sent.
	r2 := startGossip(t, gossipCfg("late-2", 24955, "127.0.0.1:24954"))
	eventually(t, 5*time.Second, "the late joiner holds late-1's list", func() bool {
		n, ok := nodeIn(t, r2, "late-1")
		return ok && reflect.DeepEqual(n.Tags, want)
	})
}

// TestGossipRegistry_RestartUnderSameID: the Helm chart's normal case. The
// second life has a new epoch and starts its seq again; its list must replace
// the first life's even though its seq is lower.
func TestGossipRegistry_RestartUnderSameID(t *testing.T) {
	ctx := context.Background()
	r1 := startGossip(t, gossipCfg("restart-1", 24956))
	first := startGossip(t, gossipCfg("restart-2", 24957, "127.0.0.1:24956"))
	for _, tags := range []map[string][]string{{"t": {"a"}}, {"t": {"a", "b"}}, {"t": {"old"}}} {
		if err := first.UpdateTags(tags); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, 5*time.Second, "restart-1 holds the first life's last list", func() bool {
		n, _ := nodeIn(t, r1, "restart-2")
		return reflect.DeepEqual(n.Tags, map[string][]string{"t": {"old"}})
	})

	if err := first.Deregister(ctx, "restart-2"); err != nil {
		t.Fatalf("Deregister: %v", err)
	}
	// Same id, another address, as a rescheduled pod has.
	second := startGossip(t, gossipCfg("restart-2", 24958, "127.0.0.1:24956"))
	if err := second.UpdateTags(map[string][]string{"t": {"new"}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, 8*time.Second, "restart-1 holds the second life's list", func() bool {
		n, ok := nodeIn(t, r1, "restart-2")
		return ok && reflect.DeepEqual(n.Tags, map[string][]string{"t": {"new"}})
	})
}
```

`helpers_test.go` — `gossipCfg` gains `ListScanInterval: 200 * time.Millisecond,`
after `StabilityWindow`.

- [ ] **Step 2: Run to verify RED**

Run: `go test ./internal/cluster/registry/`
Expected: FAIL — build errors `undefined: topicTagsRequest`, `undefined:
tagRequestMsg`, `q.onRequest undefined`, `undefined: ScanIntervalFor`,
`unknown field ListScanInterval`.

Behavioural RED, for the record: with only `gossip_catchup_test.go` added to
M-3's code, `TestGossipRegistry_LateJoinerFetchesList` fails with "not within
5s: the late joiner holds late-1's list".

- [ ] **Step 3: Implement**

`tags_worker.go` — topics, before:

```go
const topicTags = "cluster.tags"
```

after (the comment above it now covers both):

```go
const (
	topicTags        = "cluster.tags"
	topicTagsRequest = "cluster.tags.request"
)
```

beside `tagListMsg`:

```go
// tagRequestMsg is the payload of topicTagsRequest: "send me your list".
type tagRequestMsg struct {
	From string `json:"from"`
}
```

Event kinds and the event, before:

```go
const (
	evList tagEventKind = iota
)

type tagEvent struct {
	kind    tagEventKind
	payload []byte // a copy of the message payload
}
```

after:

```go
const (
	evList tagEventKind = iota
	evRequest
	evMember // a member joined or changed its metadata: look at it
	evLeave
)

type tagEvent struct {
	kind    tagEventKind
	node    string // evMember, evLeave: the member's name
	payload []byte // evList, evRequest: a copy of the message payload
}
```

The callbacks, before:

```go
func (q *tagEvents) NotifyJoin(n *memberlist.Node)   { q.dir.set(n) }
func (q *tagEvents) NotifyUpdate(n *memberlist.Node) { q.dir.set(n) }
func (q *tagEvents) NotifyLeave(n *memberlist.Node)  { q.dir.remove(n.Name) }
```

after — the directory first, then the nudge; the nudge may be dropped, the
directory entry cannot:

```go
func (q *tagEvents) NotifyJoin(n *memberlist.Node) {
	q.dir.set(n)
	q.offer(tagEvent{kind: evMember, node: n.Name})
}

func (q *tagEvents) NotifyUpdate(n *memberlist.Node) {
	q.dir.set(n)
	q.offer(tagEvent{kind: evMember, node: n.Name})
}

func (q *tagEvents) NotifyLeave(n *memberlist.Node) {
	q.dir.remove(n.Name)
	q.offer(tagEvent{kind: evLeave, node: n.Name})
}

// onRequest is the topicTagsRequest handler.
func (q *tagEvents) onRequest(payload []byte) {
	q.offer(tagEvent{kind: evRequest, payload: bytes.Clone(payload)})
}
```

Worker, before:

```go
func (g *Gossip) runTagWorker() {
	defer close(g.done)
	for {
		select {
		case <-g.stop:
			return
		case <-g.publish:
			g.publishOwnList()
		case ev := <-g.events.ch:
			g.handleTagEvent(ev)
		}
	}
}

func (g *Gossip) handleTagEvent(ev tagEvent) {
	switch ev.kind {
	case evList:
		g.handleList(ev.payload)
	}
}
```

after:

```go
func (g *Gossip) runTagWorker() {
	defer close(g.done)
	scan := time.NewTicker(g.cfg.ListScanInterval)
	defer scan.Stop()
	for {
		select {
		case <-g.stop:
			return
		case <-g.publish:
			g.publishOwnList()
		case ev := <-g.events.ch:
			g.handleTagEvent(ev)
		case <-scan.C:
			g.scanLists()
		}
	}
}

func (g *Gossip) handleTagEvent(ev tagEvent) {
	switch ev.kind {
	case evList:
		g.handleList(ev.payload)
	case evRequest:
		g.handleRequest(ev.payload)
	case evMember:
		g.handleMember(ev.node)
	case evLeave:
		g.handleLeave(ev.node)
	}
}

// handleMember runs for a pnode that joined or changed its metadata: if the
// list held for it is not the one it announces, ask for it. Events about this
// pnode are ignored — NotifyJoin fires for it inside memberlist.Create and
// NotifyUpdate inside every UpdateNode.
func (g *Gossip) handleMember(name string) {
	if name == g.cfg.NodeID {
		return
	}
	m, announced, ok := g.announced(name)
	if !ok {
		return
	}
	if !g.tags.current(name, announced) {
		g.requestList(m)
	}
}

func (g *Gossip) handleLeave(name string) {
	if name == g.cfg.NodeID {
		return
	}
	g.tags.drop(name)
}

func (g *Gossip) handleRequest(payload []byte) {
	var req tagRequestMsg
	if err := json.Unmarshal(payload, &req); err != nil {
		slog.Warn("malformed tag list request",
			"pkg", "cluster/registry", "size", len(payload), "err", err)
		return
	}
	if req.From == g.cfg.NodeID {
		return
	}
	m, ok := g.member(req.From)
	if !ok {
		return // the requester is not an alive member; its next event asks again
	}
	version, tags := g.tags.ownList()
	g.sendAsync(m, "list", encodeTagMsg(topicTags, tagListMsg{NodeID: g.cfg.NodeID, Version: version, Tags: tags}))
}

func (g *Gossip) requestList(m *memberlist.Node) {
	g.sendAsync(m, "request", encodeTagMsg(topicTagsRequest, tagRequestMsg{From: g.cfg.NodeID}))
}

// scanLists is the floor under the events: it compares every alive member's
// announced version with the list held and fetches wherever they differ, and
// it forgets the lists of pnodes that are gone. It is a full scan, not a list
// of outstanding requests, because an event dropped from a full queue leaves
// no request behind.
func (g *Gossip) scanLists() {
	alive := make(map[string]struct{})
	for _, m := range g.dir.all() {
		if m.Name == g.cfg.NodeID {
			continue
		}
		alive[m.Name] = struct{}{}
		nm, err := parseMeta(m.Meta)
		if err != nil {
			continue
		}
		if !g.tags.current(m.Name, nm.Tags) {
			g.requestList(m)
		}
	}
	g.tags.retain(alive)
}

// ScanIntervalFor returns the scan interval that suits a callout patience:
// half of it, so a list lost together with its events is fetched while a
// callout can still wait for it, within 100 ms … 1 s. With waiting disabled
// it is 1 s.
func ScanIntervalFor(patience time.Duration) time.Duration {
	const (
		shortest = 100 * time.Millisecond
		longest  = time.Second
	)
	if patience <= 0 {
		return longest
	}
	return min(max(patience/2, shortest), longest)
}
```

`handleList`, before:

```go
	_, announced, ok := g.announced(msg.NodeID)
	stored := g.tags.put(msg.NodeID, msg.Version, msg.Tags, announced, ok)
	slog.Debug("tag list received",
		"pkg", "cluster/registry", "peer", msg.NodeID,
		"seq", msg.Version.Seq, "tenants", len(msg.Tags), "stored", stored)
}
```

after:

```go
	m, announced, ok := g.announced(msg.NodeID)
	stored := g.tags.put(msg.NodeID, msg.Version, msg.Tags, announced, ok)
	slog.Debug("tag list received",
		"pkg", "cluster/registry", "peer", msg.NodeID,
		"seq", msg.Version.Seq, "tenants", len(msg.Tags), "stored", stored)
	// Still not the announced list: fetch at once — but only within the
	// announced epoch. A list of another epoch means this pnode's view of the
	// sender's metadata is behind; the answer to a fetch would be refused the
	// same way, so the metadata event (or the scan) fetches instead.
	if ok && msg.NodeID != g.cfg.NodeID && msg.Version.Epoch == announced.Epoch && !g.tags.current(msg.NodeID, announced) {
		g.requestList(m)
	}
}
```

`gossip.go` — `GossipConfig` gains:

```go
	// ListScanInterval is how often every member's announced list version is
	// compared with the list held, as the floor under the membership events.
	// Zero means one second. app.go derives it from the callout patience with
	// ScanIntervalFor.
	ListScanInterval time.Duration
```

`NewGossip`, at the top:

```go
	if cfg.ListScanInterval <= 0 {
		cfg.ListScanInterval = time.Second
	}
```

and beside the existing subscription:

```go
	del.subscribe(topicTags, events.onList)
	del.subscribe(topicTagsRequest, events.onRequest)
```

`app/app.go` `mustNewGossip`, before:

```go
		StabilityWindow: c.StabilityWindow,
		SecretKey:       c.HMACSecret,
	})
```

after:

```go
		StabilityWindow:  c.StabilityWindow,
		SecretKey:        c.HMACSecret,
		ListScanInterval: registry.ScanIntervalFor(c.DispatchWaitTimeout),
	})
```

**Review item (spec §13 "review + a test").** No method of `tagEvents`, and no
handler subscribed to the two tag topics, calls into memberlist or takes a lock
that another goroutine holds across a memberlist call or across I/O. The only
lock they take is `directory.mu`, whose critical sections are a map write.
Exit check: `tagEvents` has no field of type `*memberlist.Memberlist` or
`*Gossip`; `grep -n 'g\.list\.' internal/cluster/registry/directory.go` → no
hits.

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./internal/cluster/registry/... ./app/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```
git add internal/cluster/registry app/app.go && git commit -m "feat(registry): a pnode that is behind fetches the list — on join, on update and from a scan (#254)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task M-5: `Changed()` on the node registry

**Spec:** §5 "`MemberRegistry` and the node registry each expose `Changed()
<-chan struct{}`"; §11 "The registry's change signal (§5) fires on list
arrival, join and leave."

**Files:**
- Modify: `internal/contract/registry.go` (`NodeRegistry`)
- Modify: `internal/cluster/registry/local.go` (`Local`, `NewLocal`)
- Modify: `internal/cluster/registry/gossip.go` (`Gossip`, `NewGossip`)
- Modify: `internal/cluster/registry/tags_worker.go` (`tagEvents`,
  `newTagEvents`, `NotifyJoin`, `NotifyLeave`)
- Modify: `internal/cluster/registry/tags_catchup_internal_test.go` (the two
  `newTagEvents(newDirectory())` calls take the new arguments)
- Modify (one method each, compile fix — the interface grew):
  `internal/cluster/integration_test.go` (`testRegistry`),
  `internal/cluster/scheduler_rpc_test.go` (`fakeRegistry`),
  `internal/cluster/proxy/http_test.go` (`fakeRegistry`),
  `internal/cluster/dispatch/cluster_dispatcher_test.go` (`stubNodeRegistry`),
  `internal/grpc/txroute_interceptor_test.go` (`fakeRouteRegistry`),
  `internal/scheduler/service_test.go` (`fakeRegistry`; `countingRegistry`
  inherits it). The last is a test double's one-line method and the only touch
  in `internal/scheduler`; see Open point 4.
- Test: `internal/cluster/registry/changed_test.go`,
  `internal/cluster/registry/local_test.go`

**Interfaces:**
- Consumes: M-1, M-2 (`put`, `drop` and `retain` already fire), M-3 (the
  callbacks and the directory).
- Produces:
  - `contract.NodeRegistry` gains `Changed() <-chan struct{}` — closed at the
    next change of the cluster view: a peer's list arriving with different
    tags, a peer joining, a peer leaving. Take it **before** `List`.
  - `(*registry.Local).Changed()` — a channel that is never closed.
  - `(*registry.Gossip).Changed()`.
  - `newTagEvents(self string, dir *directory, signal *common.ChangeSignal)`.

Join and leave fire **from the callback**, not from the worker: `Fire` takes
only the signal's own mutex and touches no memberlist state, so it is safe
under memberlist's lock, and a join or leave whose nudge was dropped from a
full queue still wakes the waiters. The directory is written before `Fire`, so
a woken waiter's `List` already shows the change.

- [ ] **Step 1: Write the failing tests**

`internal/cluster/registry/changed_test.go`:

```go
package registry_test

import (
	"context"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

func closedWithin(ch <-chan struct{}, d time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(d):
		return false
	}
}

func TestGossipRegistry_Changed_FiresOnListArrival(t *testing.T) {
	r1 := startGossip(t, gossipCfg("chg-list-1", 25946))
	r2 := startGossip(t, gossipCfg("chg-list-2", 25947, "127.0.0.1:25946"))
	eventually(t, 5*time.Second, "chg-list-2 sees chg-list-1", func() bool {
		_, ok := nodeIn(t, r2, "chg-list-1")
		return ok
	})
	// Let the join's own signals pass, then take the channel before the change.
	time.Sleep(500 * time.Millisecond)
	var reg contract.NodeRegistry = r2
	ch := reg.Changed()

	if err := r1.UpdateTags(map[string][]string{"tenant-a": {"python"}}); err != nil {
		t.Fatal(err)
	}
	if !closedWithin(ch, 5*time.Second) {
		t.Fatal("a peer's list arrived and Changed was not closed")
	}

	// The way the owner's loop uses it: take the channel, look, wait. However
	// the wake-ups interleave, the list must become readable without polling.
	deadline := time.Now().Add(5 * time.Second)
	for {
		next := reg.Changed()
		if n, _ := nodeIn(t, r2, "chg-list-1"); len(n.Tags["tenant-a"]) == 1 {
			break
		}
		if !closedWithin(next, time.Until(deadline)) {
			t.Fatal("woken, but the list never became readable")
		}
	}
}

func TestGossipRegistry_Changed_FiresOnJoinAndLeave(t *testing.T) {
	r1 := startGossip(t, gossipCfg("chg-join-1", 25948))

	ch := r1.Changed()
	r2 := startGossip(t, gossipCfg("chg-join-2", 25949, "127.0.0.1:25948"))
	if !closedWithin(ch, 5*time.Second) {
		t.Fatal("a peer joined and Changed was not closed")
	}

	eventually(t, 5*time.Second, "chg-join-1 sees chg-join-2", func() bool {
		_, ok := nodeIn(t, r1, "chg-join-2")
		return ok
	})
	time.Sleep(500 * time.Millisecond)
	ch = r1.Changed()
	if err := r2.Deregister(context.Background(), "chg-join-2"); err != nil {
		t.Fatal(err)
	}
	if !closedWithin(ch, 5*time.Second) {
		t.Fatal("a peer left and Changed was not closed")
	}
}

func TestGossipRegistry_Changed_QuietClusterStaysOpen(t *testing.T) {
	r1 := startGossip(t, gossipCfg("chg-quiet-1", 25950))
	startGossip(t, gossipCfg("chg-quiet-2", 25951, "127.0.0.1:25950"))
	time.Sleep(time.Second) // joins and first fetches settle
	ch := r1.Changed()
	// Several scans pass; nothing changed, so nobody may be woken.
	if closedWithin(ch, time.Second) {
		t.Fatal("Changed closed with no change in the cluster view; a waiting callout would spin")
	}
}
```

Appended to `internal/cluster/registry/local_test.go` (which gains the import
`"github.com/cyoda-platform/cyoda-go/internal/contract"`):

```go
func TestLocalRegistry_ChangedNeverFires(t *testing.T) {
	var r contract.NodeRegistry = registry.NewLocal("node-1", "localhost:8080")
	ch := r.Changed()
	if ch == nil {
		t.Fatal("Changed returned nil")
	}
	select {
	case <-ch:
		t.Fatal("a single pnode has no peers; its view never changes")
	default:
	}
	if r.Changed() != ch {
		t.Error("Changed returned a different channel although nothing changed")
	}
}
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./internal/cluster/registry/ -run 'Changed'`
Expected: FAIL — build error `reg.Changed undefined (type contract.NodeRegistry
has no field or method Changed)`.

- [ ] **Step 3: Implement**

`internal/contract/registry.go`:

```go
type NodeRegistry interface {
	Register(ctx context.Context, nodeID string, addr string) error
	Lookup(ctx context.Context, nodeID string) (addr string, alive bool, err error)
	List(ctx context.Context) ([]NodeInfo, error)
	Deregister(ctx context.Context, nodeID string) error
	// Changed returns a channel that is closed at the next change of the
	// cluster view: a peer's tag list arriving, a peer joining, a peer
	// leaving. Every change installs a new channel. A caller that waits for
	// the view to change takes the channel BEFORE it calls List, so a change
	// between the two is not missed.
	Changed() <-chan struct{}
}
```

`internal/cluster/registry/local.go`:

```go
type Local struct {
	nodeID  string
	addr    string
	changed chan struct{} // never closed: a single pnode has no peers
}

func NewLocal(nodeID, addr string) *Local {
	return &Local{nodeID: nodeID, addr: addr, changed: make(chan struct{})}
}

// Changed returns a channel that is never closed: the view of a single pnode
// does not change.
func (l *Local) Changed() <-chan struct{} {
	return l.changed
}
```

`gossip.go` — `Gossip` gains `signal *common.ChangeSignal`; in `NewGossip`,
before:

```go
	dir := newDirectory()
	events := newTagEvents(dir)
	…
		tags:     newTagStore(cfg.NodeID, epoch, common.NewChangeSignal()),
```

after:

```go
	signal := common.NewChangeSignal()
	dir := newDirectory()
	events := newTagEvents(cfg.NodeID, dir, signal)
	…
		tags:     newTagStore(cfg.NodeID, epoch, signal),
		signal:   signal,
```

and:

```go
// Changed returns the channel closed at the next change of the cluster view.
func (g *Gossip) Changed() <-chan struct{} {
	return g.signal.Changed()
}
```

`tags_worker.go` (imports `internal/common`) — before:

```go
type tagEvents struct {
	dir *directory
	ch  chan tagEvent
}

func newTagEvents(dir *directory) *tagEvents {
	return &tagEvents{dir: dir, ch: make(chan tagEvent, tagEventQueueDepth)}
}
…
func (q *tagEvents) NotifyJoin(n *memberlist.Node) {
	q.dir.set(n)
	q.offer(tagEvent{kind: evMember, node: n.Name})
}
…
func (q *tagEvents) NotifyLeave(n *memberlist.Node) {
	q.dir.remove(n.Name)
	q.offer(tagEvent{kind: evLeave, node: n.Name})
}
```

after (a metadata update does not fire: the list that follows it does):

```go
type tagEvents struct {
	self   string
	dir    *directory
	signal *common.ChangeSignal
	ch     chan tagEvent
}

func newTagEvents(self string, dir *directory, signal *common.ChangeSignal) *tagEvents {
	return &tagEvents{self: self, dir: dir, signal: signal, ch: make(chan tagEvent, tagEventQueueDepth)}
}
…
func (q *tagEvents) NotifyJoin(n *memberlist.Node) {
	q.dir.set(n)
	if n.Name != q.self {
		q.signal.Fire()
	}
	q.offer(tagEvent{kind: evMember, node: n.Name})
}
…
func (q *tagEvents) NotifyLeave(n *memberlist.Node) {
	q.dir.remove(n.Name)
	if n.Name != q.self {
		q.signal.Fire()
	}
	q.offer(tagEvent{kind: evLeave, node: n.Name})
}
```

`tags_catchup_internal_test.go` — both `newTagEvents(newDirectory())` become
`newTagEvents("self", newDirectory(), common.NewChangeSignal())`, and the file
imports `"github.com/cyoda-platform/cyoda-go/internal/common"`.

Each fake gains one method. A nil channel blocks for ever in a `select`, which
is what a fake whose view never changes should do:

```go
func (r *testRegistry) Changed() <-chan struct{}     { return nil } // internal/cluster/integration_test.go
func (r *fakeRegistry) Changed() <-chan struct{}     { return nil } // internal/cluster/scheduler_rpc_test.go
func (r *fakeRegistry) Changed() <-chan struct{}     { return nil } // internal/cluster/proxy/http_test.go
func (r *stubNodeRegistry) Changed() <-chan struct{} { return nil } // internal/cluster/dispatch/cluster_dispatcher_test.go
func (f fakeRouteRegistry) Changed() <-chan struct{} { return nil } // internal/grpc/txroute_interceptor_test.go
func (r *fakeRegistry) Changed() <-chan struct{}     { return nil } // internal/scheduler/service_test.go
```

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./internal/cluster/... ./internal/contract/... ./internal/grpc/... ./internal/scheduler/... ./app/...` and `go vet ./...`
Expected: PASS; vet clean (every implementer of `NodeRegistry` compiles).

- [ ] **Step 5: Commit**

```
git add internal/contract internal/cluster internal/grpc/txroute_interceptor_test.go internal/scheduler/service_test.go && git commit -m "feat(registry): NodeRegistry.Changed — closed on list arrival, join and leave (#254)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task M-6: unparseable metadata is "not alive" — 503, not 500

**Spec:** §11 "`List` and `Lookup` treat unparseable metadata the same way —
not alive, with a WARN — so HTTP transaction routing answers 503, not 500."

**Files:**
- Modify: `internal/cluster/registry/gossip.go` (`Lookup`)
- Test: `internal/cluster/registry/gossip_badmeta_internal_test.go` — package
  `registry`, not `registry_test`: a member with unparseable metadata is
  invisible through `List` and "not alive" through the fixed `Lookup`, so from
  outside the package the test cannot tell "memberlist has not seen the
  stranger yet" from "the stranger is reported not alive", and would pass
  before the fix whenever it looked too early. It waits on `g.member` (M-3).
  `internal/cluster/proxy` does not import `registry`, so there is no cycle.

`internal/cluster/proxy/http.go:57-66` keeps its `err != nil` → 500 branch: it
is the right answer to a registry that genuinely fails, and
`contract.NodeRegistry` still allows one. `Gossip.Lookup` no longer reaches it.
Existing `TestHTTPProxy_*` stay unchanged.

**Interfaces:**
- Consumes: `token.(*Signer).Issue(nodeID, txRef string, expiresAt time.Time)
  (string, error)` and `proxy.HTTPRouting(signer, registry, selfNodeID,
  proxyTimeout, allowLoopback)` **as they are today**. The stream that changes
  the pass (spec §7) adjusts the one `Issue` call in this test if it changes
  that signature.
- Consumes: `(*Gossip).member` (M-3); `startInternalGossip`, `startRawPeer`,
  `waitFor` from `rawpeer_internal_test.go` (M-4).
- Produces: `Gossip.Lookup` returns `("", false, nil)` for a member whose
  metadata does not parse.

- [ ] **Step 1: Write the failing test**

```go
package registry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/proxy"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

func TestGossipRegistry_UnparseableMetadata_NotAlive_HTTP503(t *testing.T) {
	ctx := context.Background()
	r := startInternalGossip(t, "badmeta-1", 26946, 200*time.Millisecond)

	// A member of the gossip cluster whose metadata is not a pnode's.
	startRawPeer(t, "badmeta-stranger", 26947, []byte("not json"), "127.0.0.1:26946")
	waitFor(t, 5*time.Second, "memberlist on badmeta-1 has the stranger as an alive member", func() bool {
		_, ok := r.member("badmeta-stranger")
		return ok
	})

	addr, alive, err := r.Lookup(ctx, "badmeta-stranger")
	if err != nil {
		t.Fatalf("Lookup returned an error for unparseable metadata: %v — callers turn that into a 500", err)
	}
	if alive || addr != "" {
		t.Errorf("Lookup = (%q, %v), want not alive", addr, alive)
	}
	nodes, err := r.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, n := range nodes {
		if n.NodeID == "badmeta-stranger" {
			t.Error("List reports a member whose metadata does not parse")
		}
	}

	signer, err := token.NewSigner([]byte("test-secret-key-at-least-32-bytes!"))
	if err != nil {
		t.Fatal(err)
	}
	tok, err := signer.Issue("badmeta-stranger", "tx-1", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	handler := proxy.HTTPRouting(signer, r, "badmeta-1", 5*time.Second, true)(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	req := httptest.NewRequest(http.MethodGet, "/api/entity/1", nil)
	req.Header.Set(proxy.TxTokenHeader, tok)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), common.ErrCodeTransactionNodeUnavailable) {
		t.Errorf("body %s does not carry %s", rec.Body.String(), common.ErrCodeTransactionNodeUnavailable)
	}
}
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./internal/cluster/registry/ -run 'TestGossipRegistry_UnparseableMetadata'`
Expected: FAIL — "Lookup returned an error for unparseable metadata: unmarshal
metadata for badmeta-stranger: …".

- [ ] **Step 3: Implement** — `Lookup`, before:

```go
	nm, err := parseMeta(m.Meta)
	if err != nil {
		return "", false, fmt.Errorf("unmarshal metadata for %s: %w", nodeID, err)
	}
	return nm.Addr, true, nil
```

after:

```go
	nm, err := parseMeta(m.Meta)
	if err != nil {
		// Same verdict as List: a member that cannot be read is not a pnode
		// work can be sent to. Not an error — callers answer an error with a
		// 500, and this is a 503.
		slog.Warn("member has unparseable metadata; treating it as not alive",
			"pkg", "cluster/registry",
			"memberName", m.Name,
			"err", err,
		)
		return "", false, nil
	}
	return nm.Addr, true, nil
```

The doc comment of `Lookup` gains: "A member whose metadata does not parse is
reported as not alive."

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./internal/cluster/registry/... ./internal/cluster/proxy/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```
git add internal/cluster/registry && git commit -m "fix(registry): a member with unparseable metadata is not alive, so tx routing answers 503 not 500 (#254)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task M-7: the two membership instruments

**Spec:** §12 "failed list sends and outstanding list requests — the alert
`ARCHITECTURE.md:1922` promises". V-3 as settled at the top of this file.

**Files:**
- Create: `internal/cluster/registry/metrics.go`
- Modify: `internal/cluster/registry/gossip.go` (`GossipConfig.Meter`, `Gossip`,
  `NewGossip`, `Deregister`)
- Modify: `internal/cluster/registry/tags_worker.go` (`sendAsync`)
- Modify: `app/app.go` `mustNewGossip`
- Modify: `cmd/cyoda/help/content/telemetry.md` (**Metrics**)
- Test: `internal/cluster/registry/metrics_internal_test.go`

**Interfaces:**
- Consumes: M-3, M-4; `rawPeer` helpers (M-4); `observability.Meter()`.
- Produces:
  - `GossipConfig.Meter metric.Meter` (nil → no-op meter)
  - `cyoda.cluster.tags.send_failures` — `Int64Counter`, attribute `msg` =
    `list` | `request`
  - `cyoda.cluster.tags.lists_outstanding` — `Int64ObservableGauge`: alive
    peers whose announced list is not the one held.

- [ ] **Step 1: Write the failing tests**

```go
package registry

import (
	"context"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func startMeteredGossip(t *testing.T, id string, port int) (*Gossip, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	g, err := NewGossip(GossipConfig{
		NodeID:           id,
		NodeAddr:         "http://" + id + ".test:8080",
		BindAddr:         "127.0.0.1",
		BindPort:         port,
		StabilityWindow:  200 * time.Millisecond,
		ListScanInterval: 200 * time.Millisecond,
		Meter:            mp.Meter("test"),
	})
	if err != nil {
		t.Fatalf("NewGossip: %v", err)
	}
	t.Cleanup(func() { _ = g.Deregister(context.Background(), id) })
	return g, reader
}

// int64Value returns the sum of every data point of the named instrument.
func int64Value(t *testing.T, reader *sdkmetric.ManualReader, name string) (int64, bool) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			var total int64
			switch data := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, dp := range data.DataPoints {
					total += dp.Value
				}
			case metricdata.Gauge[int64]:
				for _, dp := range data.DataPoints {
					total += dp.Value
				}
			default:
				t.Fatalf("%s has unexpected data type %T", name, m.Data)
			}
			return total, true
		}
	}
	return 0, false
}

func TestGossipMetrics_ListsOutstanding(t *testing.T) {
	g, reader := startMeteredGossip(t, "metric-out-1", 27946)
	v1 := listVersion{Epoch: 5, Seq: 1}
	p := startRawPeer(t, "metric-out-peer", 27947, rawMeta(t, "metric-out-peer", v1), "127.0.0.1:27946")

	waitFor(t, 5*time.Second, "one peer's list is outstanding", func() bool {
		v, ok := int64Value(t, reader, "cyoda.cluster.tags.lists_outstanding")
		return ok && v == 1
	})

	waitFor(t, 5*time.Second, "the peer is asked", func() bool { return p.requestCount() >= 1 })
	p.send(t, "metric-out-1", 27946, topicTags, tagListMsg{NodeID: "metric-out-peer", Version: v1, Tags: map[string][]string{"t": {"x"}}})
	waitFor(t, 5*time.Second, "nothing is outstanding once the list is held", func() bool {
		v, ok := int64Value(t, reader, "cyoda.cluster.tags.lists_outstanding")
		return ok && v == 0
	})
	_ = g
}

func TestGossipMetrics_SendFailures(t *testing.T) {
	g, reader := startMeteredGossip(t, "metric-fail-1", 27948)
	p := startRawPeer(t, "metric-fail-peer", 27949, rawMeta(t, "metric-fail-peer", listVersion{Epoch: 5}), "127.0.0.1:27948")
	waitFor(t, 5*time.Second, "metric-fail-1 sees the peer", func() bool {
		_, ok := g.member("metric-fail-peer")
		return ok
	})

	// The peer dies without leaving. Until memberlist notices, it is still an
	// alive member whose port refuses connections.
	if err := p.list.Shutdown(); err != nil {
		t.Fatal(err)
	}
	if err := g.UpdateTags(map[string][]string{"tenant-a": {"python"}}); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 5*time.Second, "a failed send is counted", func() bool {
		v, ok := int64Value(t, reader, "cyoda.cluster.tags.send_failures")
		return ok && v >= 1
	})
}
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./internal/cluster/registry/ -run 'TestGossipMetrics'`
Expected: FAIL — build error `unknown field Meter in struct literal of type
GossipConfig`.

- [ ] **Step 3: Implement**

`internal/cluster/registry/metrics.go`:

```go
package registry

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

// tagMetrics are the membership instruments an operator alerts on: sends that
// failed, and peers whose list this pnode is still waiting for. A lasting
// non-zero lists_outstanding means callouts are not being handed to a pnode
// that could take them.
type tagMetrics struct {
	sendFailures metric.Int64Counter
	registration metric.Registration
}

func newTagMetrics(meter metric.Meter, outstanding func() int64) (*tagMetrics, error) {
	if meter == nil {
		meter = noop.NewMeterProvider().Meter("")
	}
	failures, err := meter.Int64Counter("cyoda.cluster.tags.send_failures",
		metric.WithDescription("Reliable tag-list messages to a peer that failed to send"))
	if err != nil {
		return nil, fmt.Errorf("instrument send_failures: %w", err)
	}
	gauge, err := meter.Int64ObservableGauge("cyoda.cluster.tags.lists_outstanding",
		metric.WithDescription("Alive peers whose announced tag list is not the one held"))
	if err != nil {
		return nil, fmt.Errorf("instrument lists_outstanding: %w", err)
	}
	reg, err := meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		o.ObserveInt64(gauge, outstanding())
		return nil
	}, gauge)
	if err != nil {
		return nil, fmt.Errorf("register lists_outstanding callback: %w", err)
	}
	return &tagMetrics{sendFailures: failures, registration: reg}, nil
}

func (m *tagMetrics) sendFailed(kind string) {
	m.sendFailures.Add(context.Background(), 1, metric.WithAttributes(attribute.String("msg", kind)))
}

func (m *tagMetrics) close() {
	_ = m.registration.Unregister()
}
```

`gossip.go` — `GossipConfig` gains:

```go
	// Meter registers the membership instruments. Nil means no instruments.
	Meter metric.Meter
```

(import `"go.opentelemetry.io/otel/metric"`), `Gossip` gains `metrics
*tagMetrics`, and `NewGossip`, after `g.list = list` and before the worker
starts:

```go
	metrics, err := newTagMetrics(cfg.Meter, g.outstandingLists)
	if err != nil {
		_ = list.Shutdown()
		return nil, fmt.Errorf("failed to create membership metrics: %w", err)
	}
	g.metrics = metrics
```

```go
// outstandingLists counts the alive peers whose announced list is not the one
// held. It runs on the metrics reader's goroutine and reads the directory.
func (g *Gossip) outstandingLists() int64 {
	var n int64
	for _, m := range g.dir.all() {
		if m.Name == g.cfg.NodeID {
			continue
		}
		nm, err := parseMeta(m.Meta)
		if err != nil {
			continue
		}
		if !g.tags.current(m.Name, nm.Tags) {
			n++
		}
	}
	return n
}
```

`Deregister`, inside the `Do`, after `<-g.done`: `g.metrics.close()`.

`tags_worker.go` `sendAsync`, before:

```go
		slog.Warn("failed to send tag list message; the peer will fetch it",
```

after:

```go
		g.metrics.sendFailed(kind)
		slog.Warn("failed to send tag list message; the peer will fetch it",
```

`app/app.go` `mustNewGossip` adds `Meter: observability.Meter(),` to the
`GossipConfig` literal. `observability.Init` runs in `cmd/cyoda/main.go:137`
before the app is built; the global meter also delegates instruments created
before a provider is set, so the order is not load-bearing.

`cmd/cyoda/help/content/telemetry.md`, **Metrics**, after the Postgres pool
block:

```markdown
Cluster membership metrics are exposed whenever `CYODA_CLUSTER_ENABLED=true`,
regardless of `CYODA_OTEL_ENABLED`:

- `cyoda.cluster.tags.send_failures` — `Int64Counter` — reliable tag-list messages to a peer that failed to send; labeled by `msg` (`list`, `request`). A failed send is repaired by the peer fetching the list; a steady rate points at a peer that gossip reaches and TCP does not.
- `cyoda.cluster.tags.lists_outstanding` — `Int64ObservableGauge` — alive peers whose announced tag list this node does not hold yet. Briefly non-zero after a join or a compute node attaching; alarm when it stays non-zero, because callouts are not handed to a peer whose tags are unknown.
```

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./internal/cluster/registry/... ./app/... ./cmd/cyoda/...`
Expected: PASS (the help-content tests under `cmd/cyoda` included).

- [ ] **Step 5: Commit**

```
git add internal/cluster/registry app/app.go cmd/cyoda/help/content/telemetry.md && git commit -m "feat(registry): count failed list sends and outstanding lists (#254)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task M-8: multi-pnode parity — many tenants on one pnode stay visible

**Spec:** §13 row "Many tenants on one pnode stay visible (R§12 sizes)", layer
**M**.

**Files:**
- Create: `e2e/parity/multinode/membership_many_tenants.go`

**Interfaces:**
- Consumes — **from the harness stream** (spec §13 "the fixtures gain an
  optional capability to start and stop extra compute clients"). This scenario
  needs that capability to take a **tenant** and a **pnode index**, which the
  spec's description (tags and a behaviour) does not mention; see Open point 3.
  Written against:

  ```go
  // ExtraComputeFixture is an optional capability of a MultiNodeFixture.
  type ExtraComputeFixture interface {
      // StartCompute attaches one more compute client to node nodeIdx as
      // tenantID with the given tags and behaviour ("" = answer normally),
      // returns once its cnode is registered, and detaches it at test cleanup.
      StartCompute(t *testing.T, nodeIdx int, tenantID string, tags []string, behaviour string)
  }
  ```

  If the harness stream's name or signature differs, this file follows it; the
  scenario's logic does not change.
- Consumes (existing): `multinode.Register`, `MultiNodeFixture`,
  `cbRouteSetupModel`, `computeMemberTag` (`callback_route.go`),
  `client.NewClient`, the `noop` processor of `cmd/compute-test-client`.
- Produces: scenario `Membership_ManyTenantsStayVisible`, picked up by every
  cluster-capable backend's `TestMultiNode`, the commercial backend's included.

This task has no RED against this branch once M-3 is in: it guards the fix at
the layer where a real cluster runs. Its RED is run once against the merge
base, as Step 2 says.

- [ ] **Step 1: Write the scenario**

```go
package multinode

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// membership_many_tenants.go — a pnode hosting cnodes of many tenants must
// stay visible to its peers. Tenants and tags once rode in memberlist's node
// metadata, capped at 512 bytes; past it the pnode published nothing, every
// peer dropped it, and no callout was ever handed to it — for all its
// tenants, not only the one that tipped it over.

func init() {
	Register(NamedTest{Name: "Membership_ManyTenantsStayVisible", Fn: RunMembership_ManyTenantsStayVisible})
}

func membershipWorkflow(wfName string) string {
	b, _ := json.Marshal(map[string]any{
		"importMode": "REPLACE",
		"workflows": []any{map[string]any{
			"version": "1.1", "name": wfName, "initialState": "NONE", "active": true,
			"states": map[string]any{
				"NONE": map[string]any{"transitions": []any{map[string]any{
					"name": "init", "next": "ACTIVE", "manual": false,
					"processors": []any{map[string]any{
						"type": "calculator", "name": "noop", "executionMode": "SYNC",
						"config": map[string]any{"calculationNodesTags": computeMemberTag},
					}},
				}}},
				"ACTIVE": map[string]any{},
			},
		}},
	})
	return string(b)
}

// RunMembership_ManyTenantsStayVisible attaches cnodes of four more tenants,
// three 40-character tags each, to node 0 — about 170 bytes per tenant in the
// old metadata, 680 for the four, past the 512-byte cap on their own. A
// callout driven from node 1 for the shared compute tenant must still be
// handed over to node 0.
func RunMembership_ManyTenantsStayVisible(t *testing.T, fixture MultiNodeFixture) {
	t.Helper()
	extra, ok := fixture.(ExtraComputeFixture)
	if !ok {
		t.Skip("fixture cannot start extra compute clients")
	}
	urls := fixture.BaseURLs()
	if len(urls) < 2 {
		t.Fatalf("needs ≥2 nodes, got %d", len(urls))
	}

	for i := range 4 {
		tags := make([]string, 3)
		for j := range tags {
			tags[j] = fmt.Sprintf("membership-%d-%d-%s", i, j, strings.Repeat("x", 25))
		}
		extra.StartCompute(t, 0, uuid.NewString(), tags, "")
	}

	tenant := fixture.ComputeTenant(t)
	const model = "membership-many-tenants"
	cbRouteSetupModel(t, client.NewClient(urls[0], tenant.Token), model,
		`{"name":"Test","amount":10,"status":"new"}`, membershipWorkflow("membership-many-tenants-wf"))

	// Node 1 hosts no cnode: the callout succeeds only if node 1 still sees
	// node 0 and the tag it hosts.
	owner := client.NewClient(urls[1], tenant.Token)
	id, err := owner.CreateEntity(t, model, 1, `{"name":"e","amount":1,"status":"new"}`)
	if err != nil {
		t.Fatalf("create via node 1 with many tenants attached to node 0: %v — node 0 has dropped out of node 1's view", err)
	}
	got, err := owner.GetEntity(t, id)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	if got.Meta.State != "ACTIVE" {
		t.Fatalf("state = %q, want ACTIVE (the callout was not handed to node 0)", got.Meta.State)
	}

	// And node 0 must still see itself: a create on it runs the callout locally
	// and its scheduler view is not empty.
	if _, err := client.NewClient(urls[0], tenant.Token).CreateEntity(t, model, 1, `{"name":"e0","amount":1,"status":"new"}`); err != nil {
		t.Fatalf("create via node 0: %v", err)
	}
}
```

- [ ] **Step 2: Prove RED at the merge base**

With the harness capability available, run the scenario against a build of the
merge base (`git stash`-free: a temporary worktree of the base with this file
and the harness change copied in):
`go test ./e2e/parity/postgres/ -run 'TestMultiNode/Membership_ManyTenantsStayVisible'`
Expected: FAIL — "create via node 1 …: … NO_COMPUTE_MEMBER_FOR_TAG".

- [ ] **Step 3: No production code** — M-3 and M-4 are the implementation.

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./e2e/parity/postgres/ -run 'TestMultiNode'` (Docker required).
Expected: PASS; the new scenario is listed among the subtests (not skipped on
the postgres fixture).

- [ ] **Step 5: Commit**

```
git add e2e/parity/multinode/membership_many_tenants.go && git commit -m "test(parity): many tenants on one pnode stay visible across the cluster (#254)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task M-9: documentation — what membership carries, and the limit that is gone

**Spec:** §14 — help `cluster.md`, `config/cluster.md`; `docs/ARCHITECTURE.md`
cluster discovery, DD-7, the operational-limits row; `CHANGELOG.md` Fixed.

**Files:**
- Modify: `cmd/cyoda/help/content/cluster.md` (## DISCOVERY, lines 42-46)
- Modify: `cmd/cyoda/help/content/config/cluster.md` (`CYODA_NODE_ID`,
  `CYODA_NODE_ADDR`, `CYODA_GRPC_NODE_ADDR`, lines 19-21)
- Modify: `docs/ARCHITECTURE.md` — §4.1 "Node metadata" (480-491), DD-7
  (1796-1802), §14.6 row "Gossip metadata size" (1922), §14.5 row "Cross-node
  consistency" last sentence (1912), package list line 107-109
- Modify: `CHANGELOG.md` `[Unreleased]` → `### Fixed`

Not this stream's: the `CYODA_DISPATCH_WAIT_TIMEOUT` lines
(`config/cluster.md:27`, `ARCHITECTURE.md:1618`), the dispatch section and its
failure table (`ARCHITECTURE.md:608-665`, `:1899`), DD-9's poll, the "Dispatch
poll interval" row (`:1929`). They describe the callout loop.

This is a documentation task; its check is the help-content test suite and a
read-through, not a RED test.

- [ ] **Step 1: `cmd/cyoda/help/content/cluster.md`, ## DISCOVERY** — after the
  first paragraph, insert:

```markdown
Each node announces, in its gossip metadata, who it is — its id and its HTTP and gRPC addresses — and the version of the list of compute tags it hosts. The list itself, one entry per tenant with a compute node attached, is sent to every peer over the membership layer's reliable (TCP) channel whenever it changes, and a node that finds it holds a different version from the one a peer announces asks that peer for it. One mechanism covers a lost message, a node that joins late, a node restarted under the same id, and a healed partition. The number of tenants and tags a node can host is not limited by the membership layer.

The metadata is limited to 512 bytes by `memberlist`. Its size depends only on `CYODA_NODE_ID`, `CYODA_NODE_ADDR` and `CYODA_GRPC_NODE_ADDR`; a node whose identity would not fit refuses to start and names the three settings. All membership traffic, the tag lists included, is encrypted with `CYODA_HMAC_SECRET`.
```

- [ ] **Step 2: `cmd/cyoda/help/content/config/cluster.md`** — append to the
  `CYODA_NODE_ID` entry: " Together with `CYODA_NODE_ADDR` and
  `CYODA_GRPC_NODE_ADDR` it must fit the 512-byte gossip metadata (about 420
  bytes for the three values together; 89 bytes are framing and the longest
  list version); a node whose identity does not fit
  refuses to start."

- [ ] **Step 3: `docs/ARCHITECTURE.md`** (present tense; no history)

  §4.1 — replace the "Node metadata" block and the sentence after it with:

````markdown
**Node metadata** (JSON, serialized in memberlist node meta) is identity plus a list version:

```go
type nodeMeta struct {
    ID       string      `json:"id"`                 // stable, operator-assigned
    Addr     string      `json:"addr"`               // HTTP address (e.g., "http://node-1:8123")
    GRPCAddr string      `json:"grpcAddr,omitempty"` // gRPC address, when advertised
    Tags     listVersion `json:"tv"`                 // {epoch: process start, unix nanos; seq: change counter}
}
```

Its size depends only on operator settings. `NewGossip` measures it with the longest version there can be and refuses to start past memberlist's `MetaMaxSize` (512 bytes).

**Tag lists** (tenant → compute tags) do not ride in the metadata. Each node holds one list per peer. The version a node announces in its own metadata is the authority for which of its lists is current; versions are compared for equality and ordered only within one epoch, so a node restarted under the same id — with a clock that stepped backwards, even — is never taken for an older self. On a change a node bumps `seq`, re-advertises its metadata and sends `{nodeID, version, tags}` to every alive peer with `memberlist.SendReliable` (topic `cluster.tags`). A peer stores a list whose version equals the announced one, or is a later `seq` of the announced epoch. A peer that holds no list for a node, or another version than the announced one, sends `cluster.tags.request` and the node answers with its list: on `NotifyJoin`, on `NotifyUpdate`, and from a periodic scan of all members that is the floor under both. `NotifyLeave` drops the node's list. memberlist's event callbacks run under its node lock and are the only place a member can be read safely, so they do nothing but copy: the member (name, address, metadata) into the registry's own directory of alive members, and a nudge into a bounded queue. Everything else — `List`, `Lookup`, the fan-out, the scan — reads that directory and never `memberlist.Members()`, whose nodes memberlist rewrites under a lock no caller can take. One worker goroutine stores lists, and every send runs on a goroutine of its own. `NodeRegistry.Changed()` is closed when a list arrives with different tags, when a peer joins and when one leaves.
````

  DD-7 — retitle "Tag Lists Beside Gossip Metadata"; Decision: "Each node
  announces a tag-list version in its gossip metadata and sends the list
  itself, organized per tenant, to each peer over memberlist's reliable
  channel; a peer that is behind fetches it." Rationale: keep "Avoids a
  centralized registry. Tag lookups are local memory reads"; replace the
  convergence sentence with "memberlist caps node metadata at 512 bytes, which
  a handful of tenants exceeds; the reliable channel has no size limit, and the
  announced version makes a lost message detectable."

  §14.6 row — replace with:
  `| Gossip metadata size | ~100–150 bytes per node | memberlist \`MetaMaxSize\` = 512 bytes | Identity and a list version only; tenants and tags travel by reliable message and are unbounded. A node whose identity does not fit refuses to start. Alert on \`cyoda.cluster.tags.lists_outstanding\` staying non-zero. |`

  §14.5 "Cross-node consistency" — last sentence becomes: "Cluster membership
  and the per-node compute-tag lists are eventually consistent with sub-second
  convergence."

  Package list, `registry/` — append: "; tag lists travel beside the metadata
  (`tags.go`, `tags_worker.go`)".

- [ ] **Step 4: `CHANGELOG.md` `[Unreleased]` → `### Fixed`**

```markdown
- **A node hosting compute nodes of more than a handful of tenants vanished
  from the cluster.** Tenants and tags rode in the membership layer's node
  metadata, which the library caps at 512 bytes — the eighth tenant with a
  36-character id, or the fourth with a 100-character one. Past it the node
  published empty metadata: every peer dropped it, no callout was handed to
  it for any of its tenants, a callback routed through a peer could not find
  the transaction's owner, the scheduler gave it no work, and it dropped out
  of its own view; nothing reported it beyond one warning line. The metadata
  now carries identity and a list version only; the lists travel over the
  membership layer's reliable channel, and a node that is behind fetches
  them. A node whose identity (`CYODA_NODE_ID`, `CYODA_NODE_ADDR`,
  `CYODA_GRPC_NODE_ADDR`) cannot fit the metadata **refuses to start**. Two
  instruments, `cyoda.cluster.tags.send_failures` and
  `cyoda.cluster.tags.lists_outstanding`, make the channel observable.
- **A transaction routed to a node whose membership metadata cannot be read
  answers `503 TRANSACTION_NODE_UNAVAILABLE`**, as for any other node that is
  not available. It answered `500`.
```

- [ ] **Step 5: Verify and commit**

Run: `go test ./cmd/cyoda/...`
Expected: PASS.
Check: `grep -n 'Monitor and alert' docs/ARCHITECTURE.md` → no hits;
`grep -n 'tags,omitempty' docs/ARCHITECTURE.md` → no hits.

```
git add cmd/cyoda/help/content/cluster.md cmd/cyoda/help/content/config/cluster.md docs/ARCHITECTURE.md CHANGELOG.md && git commit -m "docs: what cluster membership carries, and the metadata limit that no longer binds (#254)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

## Stream interface summary

**Other streams may consume from M:**

- `internal/common` (M-1): `func NewChangeSignal() *ChangeSignal`;
  `func (*ChangeSignal) Changed() <-chan struct{}`; `func (*ChangeSignal) Fire()`.
  Intended for `internal/grpc.MemberRegistry.Changed()` too — one primitive,
  both registries.
- `internal/contract` (M-5): `NodeRegistry.Changed() <-chan struct{}` — closed
  when a peer's list arrives with different tags, a peer joins, or a peer
  leaves; take it before `List`. `registry.Local.Changed()` never closes. **Any
  new fake of `contract.NodeRegistry` written by another stream needs the
  method**; the six existing fakes get it in M-5. If the owner's loop must
  compile before M-2…M-4 are in, M-1 and M-5's interface/`Local`/fakes part can
  go first; only `Gossip`'s firing depends on M-2 and M-3.
- `Gossip.List` and `Gossip.Lookup` read the registry's own member directory,
  not `memberlist.Members()`; a member appears in them once memberlist has
  raised its join event, which is when it enters `Members()` too.
- `registry.GossipConfig.ListScanInterval`, `registry.GossipConfig.Meter`,
  `func registry.ScanIntervalFor(patience time.Duration) time.Duration`
  (M-4, M-7) — wired in `app.go` `mustNewGossip` only.
- `contract.NodeInfo.Tags` keeps its type and meaning; it is now an **empty,
  non-nil map** until a member's list has arrived, each slice sorted and
  de-duplicated, and a private copy the caller may keep.
- `Gossip.UpdateTags(map[string][]string) error` keeps its signature; it no
  longer blocks on the network, and an unchanged set is a no-op. The
  `internal/grpc` stream need not sort in `computeTagsLocked`.
- `Gossip.Lookup` reports a member with unparseable metadata as `("", false,
  nil)`.
- Instruments: `cyoda.cluster.tags.send_failures` (`msg`),
  `cyoda.cluster.tags.lists_outstanding`; both documented in `telemetry.md` by
  M-7 — the stream adding the callout counters appends to the same section.
- Docs left to the callout/config streams (M-9 lists them): the
  `CYODA_DISPATCH_WAIT_TIMEOUT` lines, `ARCHITECTURE.md` dispatch section,
  failure-mode row 1899, DD-9, "Dispatch poll interval".

**M consumes:**

- `token.(*Signer).Issue(nodeID, txRef string, expiresAt time.Time)` and
  `proxy.HTTPRouting(...)` as they are today, in one test (M-6).
- From the harness stream (M-8): an optional multinode-fixture capability to
  attach an extra compute client **to a given pnode, as a given tenant**, with
  given tags — proposed `ExtraComputeFixture.StartCompute(t, nodeIdx, tenantID,
  tags, behaviour)`.
- `observability.Meter()` (existing).

## Open points

1. **Spec §11 *Receive*, "if what is held still differs from what is announced
   the pnode fetches again at once" — read literally, it loops.** Two cases.
   (a) A list that outran its metadata is stored (`held = {E,6}`, announced
   still `{E,5}`); held ≠ announced → fetch → the answer is `{E,6}` again, not
   later than held → dropped → still differs → fetch … a tight TCP loop until
   the alive message lands. (b) A restarted peer answers `{E2,1}` while this
   pnode still sees `{E1,9}` announced → dropped → fetch at once → same answer.
   The plan therefore (i) treats a held list that is a later `seq` of the
   announced epoch as current (`isCurrent`, M-2), and (ii) fetches at once only
   when the dropped or stored list is of the **announced epoch** (M-4
   `handleList`); in case (b) the metadata event that must follow, or the scan,
   fetches. Both terminate: a pnode's own version is never below what it
   announced (`setOwn` writes both in one locked step). The spec sentence
   should be amended to match.
2. **Spec §11 *Publish*, "each send bounded by memberlist's TCP timeout" —
   only the dial is.** `sendUserMsg` (`memberlist@v0.6.0/net.go:926-957`)
   dials with `TCPTimeout` and then `conn.Write`s with no deadline
   (`rawSendMsgStream`, `net.go:916`). For a list of a few kilobytes the write
   lands in the socket buffer and returns, so in practice the bound holds; a
   peer that accepts and never reads could hold a send goroutine until its own
   inbound deadline (`net.go:241`, the receiver sets one) closes the
   connection — 10 s. No send runs on the worker (decision 2), so nothing
   stalls; stated here because the spec's sentence is stronger than the
   library.
3. **Spec §13 harness paragraph gives the extra compute clients "tags and a
   behaviour"; two rows need more.** "Many tenants on one pnode stay visible"
   (M) and "Two tenants share a tag on one pnode" (M) both need a compute
   client under a **chosen tenant** attached to a **chosen pnode**. The stream
   cannot be opened from the scenario itself: `StartStreaming` requires
   `ROLE_M2M` (`internal/grpc/streaming.go:27-33`), `MultiNodeFixture` exposes
   neither gRPC endpoints nor the signing key
   (`e2e/parity/multinode/fixture.go`), and `NewTenant` mints `ROLE_ADMIN`
   (`fixtureutil.go:172-199`). M-8 is written against a proposed signature and
   blocks on the harness stream.
4. **"Do not plan anything in `internal/scheduler`" vs. the interface method.**
   Adding `Changed()` to `contract.NodeRegistry` stops
   `internal/scheduler/service_test.go` from compiling until its `fakeRegistry`
   gains the one-line method (M-5). That is a test double's compile fix, not a
   scheduler change; the alternative — a second, narrower interface only the
   owner's loop uses — would leave the node registry with two contracts. If the
   instruction is absolute, say so and M-5 switches to
   `contract.NodeChangeNotifier`.
5. **`ARCHITECTURE.md` DD-7** is not in my assignment's list (cluster discovery
   + operational-limits row) but describes exactly the mechanism M-3 replaces
   and would be false the moment it lands. M-9 rewrites it; tell me if the
   stream auditing `ARCHITECTURE.md` as a whole wants it instead.
6. **`Gossip.Deregister` called twice panics today** (memberlist
   `memberlist.go:653-655`). Not in the spec; fixed in M-3 under its own test
   because that task gives `Deregister` a worker to stop and the new tests
   deregister explicitly as well as at cleanup.
7. **A data race the research did not find, and a departure from §11's
   wording to remove it.** `Gossip.List`/`Lookup` read `m.Meta` through the
   pointers `memberlist.Members()` hands out, after it has released
   `nodeLock`; `aliveNode` rewrites `Meta`/`Addr`/`Port` under that lock
   (`memberlist@v0.6.0/state.go:1131-1133`). It is a race on a slice header in
   production today. The tests do not show it today only because
   `UpdateTags` blocks in `UpdateNode(0)` until the peer has the metadata;
   §11 rightly takes that block away, and `go test -race` then fails the
   **unchanged** `TestGossipRegistry_TagPropagation` (observed: `Write …
   aliveNode() state.go:1131` / `Previous read … (*Gossip).List()`). So the
   spec as written cannot pass `make race`. The scan §11 prescribes ("scans
   every member") would read the same pointers.
   The plan's answer (decision 6, M-3): the three event callbacks — the only
   place a member can be read under memberlist's lock — copy it into a
   directory under a mutex of the registry's own, and nothing in the package
   calls `Members()`. §11 says the callbacks "only copy what they were given …
   and enqueue it"; here three of them copy into the directory (state, so a
   join or leave cannot be dropped) and *also* enqueue a nudge. They still
   never block and never call into memberlist; `Fire` (M-5) is likewise a
   mutex and a channel close. Checked: race-clean over three passes. §11
   should be amended to say so.
8. **`sendAsync` hands `SendReliable` a copy of the node** taken from the
   directory, for the same reason; `SendReliable` uses only `Name`, `Addr` and
   `Port` (`memberlist.go:597-603`, `Node.FullAddress`). An address that
   changes without the metadata changing would not reach the directory, because
   memberlist raises no event for it (`state.go:1145-1152` — join, or metadata
   differs). A pnode's epoch is in its metadata, so a restarted pnode always
   differs; a live process does not change address.
