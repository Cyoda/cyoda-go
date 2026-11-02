package clusterstatus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/health"
	"github.com/cyoda-platform/cyoda-go/internal/lifecycle"
)

type fakePinger struct {
	err      error
	deadline time.Time
	has      bool
}

func (f *fakePinger) Ping(ctx context.Context) error {
	f.deadline, f.has = ctx.Deadline()
	return f.err
}

func normalLifecycle() *lifecycle.Lifecycle {
	lc := lifecycle.New(health.NewLatch())
	lc.MarkStarted()
	return lc
}

func TestSelf_Status(t *testing.T) {
	clk := Clock{Controllable: true, Version: 7, Writer: "node-a"}
	s := NewSelf(normalLifecycle(), &fakePinger{}, "v1", func() Clock { return clk }, 10*time.Second)
	want := NodeStatus{State: lifecycle.Normal, Database: true, Version: "v1", Clock: clk}
	if got := s.Status(context.Background()); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestSelf_PingBoundIsHalfAskTimeout(t *testing.T) {
	p := &fakePinger{}
	s := NewSelf(normalLifecycle(), p, "v1", func() Clock { return Clock{} }, 10*time.Second)
	s.Status(context.Background())
	if !p.has {
		t.Fatal("Ping context has no deadline")
	}
	// realtime: the test measures the real deadline handed to Ping
	d := time.Until(p.deadline) - 5*time.Second
	if d < -100*time.Millisecond || d > 100*time.Millisecond {
		t.Fatalf("deadline off from now+5s by %s", d)
	}
}

func TestSelf_PingFailure_DatabaseFalse_NoTextInStatus(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(old)

	p := &fakePinger{err: errors.New("dial tcp 10.0.0.9:5432: secret-dsn")}
	s := NewSelf(normalLifecycle(), p, "v1", func() Clock { return Clock{} }, 10*time.Second)
	st := s.Status(context.Background())
	if st.Database {
		t.Fatal("Database should be false")
	}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "10.0.0.9") {
		t.Fatalf("status leaks error text: %s", b)
	}
	log := buf.String()
	if !strings.Contains(log, "level=WARN") || !strings.Contains(log, "10.0.0.9") {
		t.Fatalf("WARN log should carry the error, got %q", log)
	}
}

func TestNodeStatus_Validate(t *testing.T) {
	ok := NodeStatus{State: lifecycle.Normal, Version: "v1"}
	with := func(f func(*NodeStatus)) NodeStatus { s := ok; f(&s); return s }
	cases := []struct {
		name    string
		s       NodeStatus
		wantErr bool
	}{
		{"starting", with(func(s *NodeStatus) { s.State = lifecycle.Starting }), false},
		{"normal", ok, false},
		{"draining", with(func(s *NodeStatus) { s.State = lifecycle.Draining }), false},
		{"unhealthy", with(func(s *NodeStatus) { s.State = lifecycle.Unhealthy }), false},
		{"256-byte version", with(func(s *NodeStatus) { s.Version = strings.Repeat("a", 256) }), false},
		{"lowercase state", with(func(s *NodeStatus) { s.State = "normal" }), true},
		{"empty state", with(func(s *NodeStatus) { s.State = "" }), true},
		{"257-byte version", with(func(s *NodeStatus) { s.Version = strings.Repeat("a", 257) }), true},
		{"257-byte writer", with(func(s *NodeStatus) { s.Clock.Writer = strings.Repeat("a", 257) }), true},
		{"non-UTF-8 version", with(func(s *NodeStatus) { s.Version = "\xff\xfe" }), true},
		{"non-UTF-8 writer", with(func(s *NodeStatus) { s.Clock.Writer = "\xff" }), true},
		{"clock version over int64", with(func(s *NodeStatus) { s.Clock.Version = uint64(math.MaxInt64) + 1 }), true},
		{"clock version MaxInt64", with(func(s *NodeStatus) { s.Clock.Version = math.MaxInt64 }), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.s.Validate(); (err != nil) != c.wantErr {
				t.Fatalf("err=%v, wantErr=%v", err, c.wantErr)
			}
		})
	}
}
