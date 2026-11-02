package platformclock

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/common"
)

var confirmSetting = Setting{StartUnixMicro: 1, AnchorUnixMicro: 1, RatePPM: 1_000_000, Version: 3, Writer: "b"}

// pushFake records the pushes and answers per node id; a node without a
// behaviour echoes the setting it was sent.
type pushFake struct {
	mu      sync.Mutex
	pushed  []string
	byNode  map[string]func(ctx context.Context, s Setting) (Setting, error)
	started chan string
}

func (f *pushFake) push(ctx context.Context, p Peer, s Setting) (Setting, error) {
	f.mu.Lock()
	f.pushed = append(f.pushed, p.NodeID)
	b := f.byNode[p.NodeID]
	f.mu.Unlock()
	if f.started != nil {
		f.started <- p.NodeID
	}
	if b == nil {
		return s, nil
	}
	return b(ctx, s)
}

func (f *pushFake) saw() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := slices.Clone(f.pushed)
	slices.Sort(out)
	return out
}

func peersOf(ids ...string) func() []Peer {
	return func() []Peer {
		var ps []Peer
		for _, id := range ids {
			ps = append(ps, Peer{NodeID: id, Addr: "addr-" + id})
		}
		return ps
	}
}

func notConfirmed(t *testing.T, err error) *NotConfirmedError {
	t.Helper()
	var nc *NotConfirmedError
	if !errors.As(err, &nc) {
		t.Fatalf("want *NotConfirmedError, got %v", err)
	}
	return nc
}

func TestSingleNode_ListsSelf(t *testing.T) {
	got, err := SingleNode("local")(context.Background(), confirmSetting)
	if err != nil || !slices.Equal(got, []string{"local"}) {
		t.Fatalf("%v %v", got, err)
	}
}

func TestClusterConfirm_ListsEveryNodeAscending(t *testing.T) {
	f := &pushFake{}
	got, err := Cluster("b", peersOf("c", "b", "a"), f.push, time.Second)(context.Background(), confirmSetting)
	if err != nil || !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Fatalf("%v %v", got, err)
	}
	if saw := f.saw(); !slices.Equal(saw, []string{"a", "c"}) {
		t.Fatalf("pushed to %v", saw)
	}
}

func TestClusterConfirm_PushFails(t *testing.T) {
	f := &pushFake{byNode: map[string]func(context.Context, Setting) (Setting, error){
		"c": func(context.Context, Setting) (Setting, error) { return Setting{}, errors.New("dial addr-c refused") },
	}}
	_, err := Cluster("b", peersOf("a", "c"), f.push, time.Second)(context.Background(), confirmSetting)
	nc := notConfirmed(t, err)
	if !slices.Equal(nc.Nodes, []string{"c"}) {
		t.Fatalf("%v", nc.Nodes)
	}
	if !strings.Contains(err.Error(), "c") || strings.Contains(err.Error(), "addr-c") {
		t.Fatalf("error text %q", err.Error())
	}
}

func TestClusterConfirm_EchoDiffers(t *testing.T) {
	fields := map[string]func(*Setting){
		"StartUnixMicro":  func(s *Setting) { s.StartUnixMicro++ },
		"AnchorUnixMicro": func(s *Setting) { s.AnchorUnixMicro++ },
		"RatePPM":         func(s *Setting) { s.RatePPM++ },
		"Version":         func(s *Setting) { s.Version++ },
		"Writer":          func(s *Setting) { s.Writer += "x" },
	}
	for name, mutate := range fields {
		t.Run(name, func(t *testing.T) {
			f := &pushFake{byNode: map[string]func(context.Context, Setting) (Setting, error){
				"a": func(_ context.Context, s Setting) (Setting, error) { mutate(&s); return s, nil },
			}}
			_, err := Cluster("b", peersOf("a", "c"), f.push, time.Second)(context.Background(), confirmSetting)
			if nc := notConfirmed(t, err); !slices.Equal(nc.Nodes, []string{"a"}) {
				t.Fatalf("%v", nc.Nodes)
			}
		})
	}
}

func TestClusterConfirm_NoUsableAddress(t *testing.T) {
	f := &pushFake{}
	peers := func() []Peer { return []Peer{{NodeID: "x"}} }
	_, err := Cluster("b", peers, f.push, time.Second)(context.Background(), confirmSetting)
	if nc := notConfirmed(t, err); !slices.Equal(nc.Nodes, []string{"x"}) {
		t.Fatalf("%v", nc.Nodes)
	}
	if saw := f.saw(); len(saw) != 0 {
		t.Fatalf("pushed to %v", saw)
	}
}

func TestClusterConfirm_SilentPeerEndsAtBound(t *testing.T) {
	f := &pushFake{byNode: map[string]func(context.Context, Setting) (Setting, error){
		"a": func(ctx context.Context, _ Setting) (Setting, error) { <-ctx.Done(); return Setting{}, ctx.Err() },
	}}
	done := make(chan error, 1)
	go func() {
		_, err := Cluster("b", peersOf("a"), f.push, 50*time.Millisecond)(context.Background(), confirmSetting)
		done <- err
	}()
	select {
	case err := <-done:
		if nc := notConfirmed(t, err); !slices.Equal(nc.Nodes, []string{"a"}) {
			t.Fatalf("%v", nc.Nodes)
		}
		if errors.Is(err, common.ErrClientGone) {
			t.Fatal("a bound expiry is not a departed client")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the round did not end at its bound")
	}
}

func TestClusterConfirm_PushesInParallel(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(2)
	barrier := func(_ context.Context, s Setting) (Setting, error) {
		wg.Done()
		wg.Wait()
		return s, nil
	}
	f := &pushFake{byNode: map[string]func(context.Context, Setting) (Setting, error){"a": barrier, "c": barrier}}
	done := make(chan error, 1)
	go func() {
		_, err := Cluster("b", peersOf("a", "c"), f.push, 5*time.Second)(context.Background(), confirmSetting)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pushes are not parallel")
	}
}

func TestClusterConfirm_ClientGone(t *testing.T) {
	f := &pushFake{
		started: make(chan string, 1),
		byNode: map[string]func(context.Context, Setting) (Setting, error){
			"a": func(ctx context.Context, _ Setting) (Setting, error) { <-ctx.Done(); return Setting{}, ctx.Err() },
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := Cluster("b", peersOf("a"), f.push, 5*time.Second)(ctx, confirmSetting)
		done <- err
	}()
	<-f.started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, common.ErrClientGone) {
			t.Fatalf("want client gone, got %v", err)
		}
		if errors.As(err, new(*NotConfirmedError)) {
			t.Fatal("a departed client is not a confirmation failure")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the round did not stop")
	}
}

type warnCounter struct {
	slog.Handler
	mu sync.Mutex
	n  int
}

func (w *warnCounter) Enabled(context.Context, slog.Level) bool { return true }
func (w *warnCounter) Handle(_ context.Context, r slog.Record) error {
	if r.Level >= slog.LevelWarn {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.n++
	}
	return nil
}

func TestClusterConfirm_ClientGoneLogsNoWarn(t *testing.T) {
	wc := &warnCounter{Handler: slog.NewTextHandler(io.Discard, nil)}
	prev := slog.Default()
	slog.SetDefault(slog.New(wc))
	defer slog.SetDefault(prev)

	f := &pushFake{
		started: make(chan string, 1),
		byNode: map[string]func(context.Context, Setting) (Setting, error){
			"a": func(ctx context.Context, _ Setting) (Setting, error) { <-ctx.Done(); return Setting{}, ctx.Err() },
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := Cluster("b", peersOf("a"), f.push, 5*time.Second)(ctx, confirmSetting)
		done <- err
	}()
	<-f.started
	cancel()
	if err := <-done; !errors.Is(err, common.ErrClientGone) {
		t.Fatalf("want client gone, got %v", err)
	}
	wc.mu.Lock()
	defer wc.mu.Unlock()
	if wc.n != 0 {
		t.Fatalf("%d WARN records for a departed client", wc.n)
	}
}

func TestCluster_PanicsOnMissingCollaborator(t *testing.T) {
	push := func(context.Context, Peer, Setting) (Setting, error) { return Setting{}, nil }
	cases := map[string]func(){
		"self":  func() { Cluster("", peersOf(), push, time.Second) },
		"peers": func() { Cluster("a", nil, push, time.Second) },
		"push":  func() { Cluster("a", peersOf(), nil, time.Second) },
		"bound": func() { Cluster("a", peersOf(), push, 0) },
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("no panic")
				}
			}()
			fn()
		})
	}
}
