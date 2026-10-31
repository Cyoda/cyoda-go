package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	"github.com/cyoda-platform/cyoda-go/app"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/registry"
	"github.com/cyoda-platform/cyoda-go/internal/platformclock"
)

// unusedStoreTime and unusedTasks stand in for a Service's store lookups where
// the test only changes the rate, which reads neither.
func unusedStoreTime(context.Context) (time.Time, error) {
	return time.Time{}, fmt.Errorf("a rate change does not read the store's time")
}

func unusedTasks(context.Context) (spi.ScheduledTaskStore, error) {
	return nil, fmt.Errorf("a rate change does not read the scheduled tasks")
}

// clockNode builds an app with the memory plugin and the context path
// removed, and serves its handler.
func clockNode(t *testing.T, mutate func(*app.Config)) *httptest.Server {
	t.Helper()
	cfg := app.DefaultConfig()
	cfg.ContextPath = ""
	if mutate != nil {
		mutate(&cfg)
	}
	a := app.New(cfg)
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(func() {
		srv.Close()
		if cfg.Cluster.Enabled {
			_ = a.NodeRegistry().Deregister(context.Background(), cfg.Cluster.NodeID)
		}
		_ = a.Close()
	})
	return srv
}

func call(t *testing.T, srv *httptest.Server, method, path, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, raw
}

// entityCreationDate registers a model, creates one entity and returns the
// creation date the store stamped on it.
func entityCreationDate(t *testing.T, srv *httptest.Server) time.Time {
	t.Helper()
	const sample = `{"k":1}`
	if st, raw := call(t, srv, http.MethodPost, "/model/import/JSON/SAMPLE_DATA/ClockWiring/1", sample); st != http.StatusOK {
		t.Fatalf("import model: %d %s", st, raw)
	}
	if st, raw := call(t, srv, http.MethodPut, "/model/ClockWiring/1/lock", ""); st != http.StatusOK {
		t.Fatalf("lock model: %d %s", st, raw)
	}
	st, raw := call(t, srv, http.MethodPost, "/entity/JSON/ClockWiring/1", sample)
	if st != http.StatusOK {
		t.Fatalf("create entity: %d %s", st, raw)
	}
	var created []struct {
		EntityIDs []string `json:"entityIds"`
	}
	if err := json.Unmarshal(raw, &created); err != nil || len(created) == 0 || len(created[0].EntityIDs) == 0 {
		t.Fatalf("create entity answered %s", raw)
	}
	st, raw = call(t, srv, http.MethodGet, "/entity/"+created[0].EntityIDs[0], "")
	if st != http.StatusOK {
		t.Fatalf("get entity: %d %s", st, raw)
	}
	var got struct {
		Meta struct {
			CreationDate time.Time `json:"creationDate"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(raw, &got); err != nil || got.Meta.CreationDate.IsZero() {
		t.Fatalf("get entity answered %s", raw)
	}
	return got.Meta.CreationDate
}

type clockAnswer struct {
	Now          time.Time `json:"now"`
	Rate         float64   `json:"rate"`
	Controllable bool      `json:"controllable"`
}

func getClock(t *testing.T, srv *httptest.Server) clockAnswer {
	t.Helper()
	st, raw := call(t, srv, http.MethodGet, "/platform/clock", "")
	if st != http.StatusOK {
		t.Fatalf("get clock: %d %s", st, raw)
	}
	var c clockAnswer
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func within(got, want time.Time, tol time.Duration) bool {
	d := got.Sub(want)
	return d >= -tol && d <= tol
}

// With the defaults a deployment stamps business time from the real clock.
func TestClockWiring_OffUsesReal(t *testing.T) {
	srv := clockNode(t, nil)
	before := time.Now() // realtime: the reference the stamp is held to
	got := entityCreationDate(t, srv)
	if !within(got, before, time.Second) {
		t.Fatalf("creationDate %s, real time %s", got, before)
	}
	if c := getClock(t, srv); c.Controllable || c.Rate != 1 {
		t.Fatalf("clock %+v", c)
	}
}

// With clock control on, the storage plugin is given the test clock: a
// setting in the past is accepted on a fresh store, and an entity created
// afterwards carries that time.
func TestClockWiring_OnSetsPluginClock(t *testing.T) {
	srv := clockNode(t, func(cfg *app.Config) { cfg.ClockControlEnabled = true })
	past := time.Date(2015, 3, 1, 9, 0, 0, 0, time.UTC)
	if st, raw := call(t, srv, http.MethodPost, "/platform/clock", fmt.Sprintf(`{"time":%q}`, past.Format(time.RFC3339))); st != http.StatusOK {
		t.Fatalf("set clock: %d %s", st, raw)
	}
	got := entityCreationDate(t, srv)
	if !within(got, past, 5*time.Second) {
		t.Fatalf("creationDate %s, want about %s", got, past)
	}
}

var clusterSecret = bytes.Repeat([]byte{0x5A}, 32)

// freePort returns a loopback port that was free a moment ago. Only the
// probe peer takes a port this way: its address must be known before the
// node seeds at it, and a probe that loses the race fails its own test.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

// A cluster node with clock control on adopts a peer's setting when it joins:
// the test clock is attached to gossip before the node registers, so the
// join-time state exchange carries the setting.
func TestClockWiring_OnClusterJoinAdoptsPeerSetting(t *testing.T) {
	ctx := context.Background()
	var (
		probe *registry.Gossip
		addr  string
	)
	for attempt := 1; ; attempt++ {
		port := freePort(t)
		g, err := registry.NewGossip(registry.GossipConfig{
			NodeID: "clock-probe", NodeAddr: "http://127.0.0.1:1",
			BindAddr: "127.0.0.1", BindPort: port, SecretKey: clusterSecret,
		})
		if err == nil {
			probe, addr = g, net.JoinHostPort("127.0.0.1", fmt.Sprint(port))
			break
		}
		if attempt == 5 {
			t.Fatalf("probe could not bind a gossip port: %v", err)
		}
	}
	t.Cleanup(func() { _ = probe.Deregister(ctx, "clock-probe") })
	held := platformclock.NewTest("clock-probe", nil)
	platformclock.Attach(held, probe, probe)
	if err := probe.Register(ctx, "clock-probe", "http://127.0.0.1:1"); err != nil {
		t.Fatal(err)
	}
	// A published setting; its broadcast is dropped (no publish), so the
	// node can learn it only by the join-time exchange.
	rate := int64(3_000_000)
	svc := platformclock.NewService(held, unusedStoreTime, unusedTasks, func(platformclock.Setting) {}, time.Second)
	if _, err := svc.Change(ctx, platformclock.Change{RatePPM: &rate}); err != nil {
		t.Fatal(err)
	}

	srv := clockNode(t, func(cfg *app.Config) {
		cfg.ClockControlEnabled = true
		cfg.Cluster.Enabled = true
		cfg.Cluster.NodeID = "clock-node"
		cfg.Cluster.NodeAddr = "http://127.0.0.1:1"
		cfg.Cluster.GossipAddr = "127.0.0.1:0"
		cfg.Cluster.SeedNodes = []string{addr}
		cfg.Cluster.StabilityWindow = 0
		cfg.Cluster.HMACSecret = clusterSecret
	})
	if c := getClock(t, srv); c.Rate != 3 || !c.Controllable {
		t.Fatalf("joined node clock %+v, want the peer's rate 3", c)
	}
}

// The startup order with clock control on is not observable without a hook
// in production code, which the project does not add; this pins the source
// the way callout_wiring_test.go does. The clock is attached to gossip only
// with the feature on and before registration; registration precedes the
// search reapers and the scheduler, so neither runs before the setting is
// merged; the startup check runs before both.
func TestClockWiring_StartupOrder(t *testing.T) {
	src, err := os.ReadFile("app.go")
	if err != nil {
		t.Fatal(err)
	}
	order := []string{
		`platformclock.Attach(`,
		`gossipReg.Register(`,
		`checkClockAtStartup(`,
		`startSearchReapers(&cfg`,
		`a.scheduler.Start(`,
	}
	last := -1
	for _, s := range order {
		if n := bytes.Count(src, []byte(s)); n != 1 {
			t.Fatalf("app.go has %q %d times, want once", s, n)
		}
		i := bytes.Index(src, []byte(s))
		if i < last {
			t.Errorf("app.go: %q comes before its predecessor in %q", s, order)
		}
		last = i
	}
	if !regexp.MustCompile(`(?s)if cfg\.ClockControlEnabled \{[^}]*platformclock\.Attach\(`).Match(src) {
		t.Error("app.go: platformclock.Attach is not guarded by the clock-control feature")
	}
}
