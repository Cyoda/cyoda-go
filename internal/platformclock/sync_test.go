package platformclock_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/platformclock"
)

type fakeExchange struct {
	local func() []byte
	merge func([]byte)
}

func (e *fakeExchange) SetStateExchange(l func() []byte, m func([]byte)) { e.local, e.merge = l, m }

func validSetting(version uint64) platformclock.Setting {
	now := time.Now().UnixMicro() // realtime: test fixture wall anchor
	return platformclock.Setting{StartUnixMicro: now, AnchorUnixMicro: now, RatePPM: 2_000_000, Version: version, Writer: "node-a"}
}

func TestSync_MergeAppliesNewer(t *testing.T) {
	src := platformclock.NewTest("node-a", nil)
	exA := &fakeExchange{}
	platformclock.Attach(src, exA)
	s := validSetting(5)
	src.Apply(s)

	c := platformclock.NewTest("node-b", nil)
	ex := &fakeExchange{}
	platformclock.Attach(c, ex)
	if ex.local == nil || ex.merge == nil {
		t.Fatal("state exchange not registered")
	}
	// The local state of one node carries its setting; merging it converges.
	ex.merge(exA.local())
	if got := c.Setting(); got != s {
		t.Fatalf("merged setting = %+v, want %+v", got, s)
	}
}

func TestSync_MergeIgnoresOlderAndGarbage(t *testing.T) {
	c := platformclock.NewTest("node-b", nil)
	ex := &fakeExchange{}
	platformclock.Attach(c, ex)
	s := validSetting(5)
	c.Apply(s)

	older := validSetting(4)
	older.RatePPM = 3_000_000
	raw, _ := json.Marshal(older)
	ex.merge(raw)

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
		ex.merge(p)
	}
	if got := c.Setting(); got != s {
		t.Fatalf("setting changed to %+v, want %+v", got, s)
	}
}

// A fresh clock serves no state, so memberlist sends none and never calls a
// peer's merge for it (it merges only state a peer sent).
func TestSync_FreshClockCarriesNothing(t *testing.T) {
	fresh := platformclock.NewTest("node-a", nil)
	ex := &fakeExchange{}
	platformclock.Attach(fresh, ex)
	if got := ex.local(); got != nil {
		t.Fatalf("local() = %q, want nil", got)
	}
}

// Changes stop at the end of the year 9000, but a clock keeps running past it
// and a rate change re-anchors there, so a joining node adopts any start up
// to 9999.
func TestSync_MergeAdoptsStartPastSetLimit(t *testing.T) {
	c := platformclock.NewTest("node-b", nil)
	ex := &fakeExchange{}
	platformclock.Attach(c, ex)
	s := validSetting(3)
	s.StartUnixMicro = time.Date(9500, 1, 1, 0, 0, 0, 0, time.UTC).UnixMicro()
	raw, _ := json.Marshal(s)
	ex.merge(raw)
	if got := c.Setting(); got != s {
		t.Fatalf("setting = %+v, want %+v", got, s)
	}
}

func holdPayload(t *testing.T, s platformclock.Setting) []byte {
	t.Helper()
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func TestHolder_HoldsNewerAndEchoesIt(t *testing.T) {
	c := platformclock.NewTest("node-b", nil)
	s := validSetting(5)
	got, err := platformclock.NewHolder(c).Hold(holdPayload(t, s))
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if got != s || c.Setting() != s {
		t.Fatalf("echo = %+v, held = %+v, want %+v", got, c.Setting(), s)
	}
}

func TestHolder_EqualEchoesSame(t *testing.T) {
	c := platformclock.NewTest("node-b", nil)
	h := platformclock.NewHolder(c)
	s := validSetting(5)
	if _, err := h.Hold(holdPayload(t, s)); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	got, err := h.Hold(holdPayload(t, s))
	if err != nil || got != s {
		t.Fatalf("second Hold = %+v, %v; want %+v", got, err, s)
	}
}

func TestHolder_OlderEchoesHeld(t *testing.T) {
	c := platformclock.NewTest("node-b", nil)
	h := platformclock.NewHolder(c)
	s5 := validSetting(5)
	if _, err := h.Hold(holdPayload(t, s5)); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	older := validSetting(4)
	older.RatePPM = 3_000_000
	got, err := h.Hold(holdPayload(t, older))
	if err != nil || got != s5 {
		t.Fatalf("older Hold = %+v, %v; want the held %+v", got, err, s5)
	}
}

func TestHolder_InvalidRefused(t *testing.T) {
	c := platformclock.NewTest("node-b", nil)
	s := validSetting(5)
	c.Apply(s)
	bad := validSetting(9)
	bad.RatePPM = platformclock.MaxRatePPM + 1
	noWriter := validSetting(9)
	noWriter.Writer = ""
	badAnchor := validSetting(9)
	badAnchor.AnchorUnixMicro = -1
	badStart := validSetting(9)
	badStart.StartUnixMicro = 1 << 62
	h := platformclock.NewHolder(c)
	for _, p := range [][]byte{[]byte("not json"), nil, holdPayload(t, bad), holdPayload(t, noWriter), holdPayload(t, badAnchor), holdPayload(t, badStart)} {
		if _, err := h.Hold(p); err == nil {
			t.Errorf("Hold(%q) accepted an invalid setting", p)
		}
	}
	if got := c.Setting(); got != s {
		t.Fatalf("setting changed to %+v, want %+v", got, s)
	}
}

func TestHolder_ClockOffRefuses(t *testing.T) {
	_, err := platformclock.NewHolder(nil).Hold(holdPayload(t, validSetting(5)))
	if !errors.Is(err, platformclock.ErrClockControlOff) {
		t.Fatalf("err = %v, want ErrClockControlOff", err)
	}
}
