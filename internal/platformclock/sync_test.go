package platformclock_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/platformclock"
)

type fakeBroadcaster struct {
	mu   sync.Mutex
	subs map[string][]func([]byte)
	sent [][]byte
}

func (f *fakeBroadcaster) Broadcast(topic string, payload []byte) {
	f.mu.Lock()
	f.sent = append(f.sent, payload)
	hs := append([]func([]byte){}, f.subs[topic]...)
	f.mu.Unlock()
	for _, h := range hs {
		h(payload)
	}
}

func (f *fakeBroadcaster) Subscribe(topic string, h func([]byte)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.subs == nil {
		f.subs = map[string][]func([]byte){}
	}
	f.subs[topic] = append(f.subs[topic], h)
}

type fakeExchange struct {
	local func() []byte
	merge func([]byte)
}

func (e *fakeExchange) SetStateExchange(l func() []byte, m func([]byte)) { e.local, e.merge = l, m }

func validSetting(version uint64) platformclock.Setting {
	now := time.Now().UnixMicro() // realtime: test fixture wall anchor
	return platformclock.Setting{StartUnixMicro: now, AnchorUnixMicro: now, RatePPM: 2_000_000, Version: version, Writer: "node-a"}
}

func TestSync_BroadcastAppliesNewer(t *testing.T) {
	b := &fakeBroadcaster{}
	c := platformclock.NewTest("node-b", nil)
	ex := &fakeExchange{}
	platformclock.Attach(c, b, ex)

	s := validSetting(5)
	platformclock.Publish(b, s)
	if got := c.Setting(); got != s {
		t.Fatalf("setting = %+v, want %+v", got, s)
	}
	if ex.local == nil || ex.merge == nil {
		t.Fatal("state exchange not registered")
	}
	// local state carries the setting; merging it into another clock converges.
	c2 := platformclock.NewTest("node-c", nil)
	ex2 := &fakeExchange{}
	platformclock.Attach(c2, &fakeBroadcaster{}, ex2)
	ex2.merge(ex.local())
	if got := c2.Setting(); got != s {
		t.Fatalf("merged setting = %+v, want %+v", got, s)
	}
}

func TestSync_IgnoresOlderAndGarbage(t *testing.T) {
	b := &fakeBroadcaster{}
	c := platformclock.NewTest("node-b", nil)
	platformclock.Attach(c, b, &fakeExchange{})
	s := validSetting(5)
	c.Apply(s)

	older := validSetting(4)
	older.RatePPM = 3_000_000
	raw, _ := json.Marshal(older)
	b.Broadcast(platformclock.Topic, raw)

	bad := validSetting(9)
	bad.RatePPM = platformclock.MaxRatePPM + 1
	badRate, _ := json.Marshal(bad)
	noWriter := validSetting(9)
	noWriter.Writer = ""
	noW, _ := json.Marshal(noWriter)
	badAnchor := validSetting(9)
	badAnchor.AnchorUnixMicro = -1
	badA, _ := json.Marshal(badAnchor)
	badStart := validSetting(9)
	badStart.StartUnixMicro = 1 << 62
	badS, _ := json.Marshal(badStart)
	for _, p := range [][]byte{[]byte("not json"), nil, badRate, noW, badA, badS} {
		b.Broadcast(platformclock.Topic, p)
	}
	if got := c.Setting(); got != s {
		t.Fatalf("setting changed to %+v, want %+v", got, s)
	}
}

func TestSync_FreshClockCarriesNothing(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(old)

	fresh := platformclock.NewTest("node-a", nil)
	ex := &fakeExchange{}
	platformclock.Attach(fresh, &fakeBroadcaster{}, ex)
	if got := ex.local(); got != nil {
		t.Fatalf("local() = %q, want nil", got)
	}

	other := platformclock.NewTest("node-b", nil)
	before := other.Setting()
	ex2 := &fakeExchange{}
	platformclock.Attach(other, &fakeBroadcaster{}, ex2)
	ex2.merge(ex.local())
	if other.Setting() != before {
		t.Fatal("merge of a fresh clock's state changed the setting")
	}
	if buf.Len() != 0 {
		t.Fatalf("unexpected log output: %s", buf.String())
	}
}
