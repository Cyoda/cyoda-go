package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/app"
)

// The API contract of GET and POST /platform/clock through the full HTTP
// stack. What the clock does to stamps is not asserted here.

type clockBody struct {
	Now          time.Time `json:"now"`
	Rate         float64   `json:"rate"`
	Controllable bool      `json:"controllable"`
}

// clockStack is a stack of its own with clock control on or off.
func clockStack(t *testing.T, enabled bool) *callbackHarness {
	t.Helper()
	return newStackOn(t, newSchedDB(t), func(cfg *app.Config) { cfg.ClockControlEnabled = enabled })
}

func (h *callbackHarness) clockAs(t *testing.T, bearer, method, query, body string) *http.Response {
	t.Helper()
	return h.DoAuthBearer(t, method, "/api/platform/clock"+query, body, "", bearer)
}

func decodeClock(t *testing.T, resp *http.Response) clockBody {
	t.Helper()
	raw := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	var c clockBody
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	return c
}

func TestPlatformClock_Get_FeatureOff(t *testing.T) {
	h := clockStack(t, false)
	before := time.Now() // realtime: bounds the answer
	// Any authenticated caller of any tenant reads it: the tenant admin token.
	c := decodeClock(t, h.clockAs(t, h.token(t), http.MethodGet, "", ""))
	if c.Controllable || c.Rate != 1 || c.Now.Before(before.Add(-time.Second)) {
		t.Fatalf("clock %+v", c)
	}
}

func TestPlatformClock_Get_FeatureOn(t *testing.T) {
	h := clockStack(t, true)
	c := decodeClock(t, h.clockAs(t, h.token(t), http.MethodGet, "", ""))
	if !c.Controllable || c.Rate != 1 {
		t.Fatalf("clock %+v", c)
	}
}

// TestPlatformClock_NoM2MRole: the clock is not a ROLE_M2M operation. A
// platform operator without ROLE_M2M changes it, and an authenticated caller
// without ROLE_M2M reads it.
func TestPlatformClock_NoM2MRole(t *testing.T) {
	h := clockStack(t, true)
	if c := decodeClock(t, h.clockAs(t, h.platformToken(t), http.MethodPost, "", `{"rate":2}`)); c.Rate != 2 {
		t.Fatalf("rate after the operator's change = %v; want 2", c.Rate)
	}
	tok, err := signServiceToken(h.signKey, "cyoda-callback-test", h.audience, "plain-user", "plain-tenant", "plain-user", []string{"ROLE_USER"})
	if err != nil {
		t.Fatal(err)
	}
	if c := decodeClock(t, h.clockAs(t, tok, http.MethodGet, "", "")); c.Rate != 2 {
		t.Fatalf("rate read by a caller without ROLE_M2M = %v; want 2", c.Rate)
	}
}

func TestPlatformClock_Post_Disabled403(t *testing.T) {
	h := clockStack(t, false)
	resp := h.clockAs(t, h.platformToken(t), http.MethodPost, "", `{"rate":2}`)
	assertProblemJSON(t, resp, http.StatusForbidden, "CLOCK_CONTROL_DISABLED")
}

func TestPlatformClock_Post_NonOperator403(t *testing.T) {
	h := clockStack(t, true)
	resp := h.clockAs(t, h.token(t), http.MethodPost, "", `{"rate":2}`)
	assertProblemJSON(t, resp, http.StatusForbidden, "FORBIDDEN")
	// The operator check comes before the disabled refusal.
	off := clockStack(t, false)
	assertProblemJSON(t, off.clockAs(t, off.token(t), http.MethodPost, "", `{"rate":2}`),
		http.StatusForbidden, "FORBIDDEN")
}

func TestPlatformClock_Post_OnBehalfOf403(t *testing.T) {
	h := clockStack(t, true)
	obo := oboTokenOn(t, h.baseURL, h.token(t), "mallory")
	assertOBORefusedAdmin(t, h.clockAs(t, obo, http.MethodPost, "", `{"rate":2}`))
	// An on-behalf-of caller may still read.
	decodeClock(t, h.clockAs(t, obo, http.MethodGet, "", ""))
}

func TestPlatformClock_Unauthenticated401(t *testing.T) {
	h := clockStack(t, true)
	for _, m := range []string{http.MethodGet, http.MethodPost} {
		req, err := e2eNewRequest(t, m, h.baseURL+"/api/platform/clock", strings.NewReader(`{"rate":2}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		assertProblemJSON(t, resp, http.StatusUnauthorized, "UNAUTHORIZED")
	}
}

func TestPlatformClock_QueryParamRefused400(t *testing.T) {
	h := clockStack(t, true)
	for _, m := range []string{http.MethodGet, http.MethodPost} {
		resp := h.clockAs(t, h.platformToken(t), m, "?x=1", `{"rate":2}`)
		assertProblemJSON(t, resp, http.StatusBadRequest, "BAD_REQUEST")
	}
}

func TestPlatformClock_Post_400(t *testing.T) {
	h := clockStack(t, true)
	cases := map[string]string{
		"malformed":             `{`,
		"unknown_member":        `{"foo":1}`,
		"empty":                 `{}`,
		"both_time_and_advance": `{"time":"2031-01-01T00:00:00Z","advanceByMs":5}`,
		"advance_zero":          `{"advanceByMs":0}`,
		"advance_overflow":      `{"advanceByMs":9223372036854775807}`,
		"advance_not_integer":   `{"advanceByMs":1.5}`,
		"advance_null":          `{"advanceByMs":null,"rate":2}`,
		"time_null":             `{"time":null,"rate":2}`,
		"rate_null":             `{"rate":null,"advanceByMs":5}`,
		"rate_not_number":       `{"rate":"2"}`,
		"rate_below":            `{"rate":0.09}`,
		"rate_above":            `{"rate":100.1}`,
		"rate_seven_digits":     `{"rate":1.1234567}`,
		"time_not_rfc3339":      `{"time":"yesterday"}`,
		"time_before_1970":      `{"time":"1969-12-31T23:59:59Z"}`,
		"time_after_9000":       `{"time":"9001-01-01T00:00:00Z"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			assertProblemJSON(t, h.clockAs(t, h.platformToken(t), http.MethodPost, "", body),
				http.StatusBadRequest, "BAD_REQUEST")
		})
	}
	// None of them changed the clock.
	if c := decodeClock(t, h.clockAs(t, h.platformToken(t), http.MethodGet, "", "")); c.Rate != 1 {
		t.Fatalf("clock changed by a refused request: %+v", c)
	}
}

// An advance whose target lies past the year 9000 is refused, and the clock
// stays where it was.
func TestPlatformClock_Post_AdvancePastYear9000_400(t *testing.T) {
	h := clockStack(t, true)
	near := decodeClock(t, h.clockAs(t, h.platformToken(t), http.MethodPost, "", `{"time":"9000-12-31T23:59:00Z"}`)).Now
	assertProblemJSON(t, h.clockAs(t, h.platformToken(t), http.MethodPost, "", `{"advanceByMs":120000}`),
		http.StatusBadRequest, "BAD_REQUEST")
	if c := decodeClock(t, h.clockAs(t, h.platformToken(t), http.MethodGet, "", "")); c.Now.Year() != 9000 || c.Now.Sub(near) > time.Minute {
		t.Fatalf("clock after the refusal = %s, want just after %s", c.Now, near)
	}
}

func TestPlatformClock_Post_RateThenGet(t *testing.T) {
	h := clockStack(t, true)
	c := decodeClock(t, h.clockAs(t, h.platformToken(t), http.MethodPost, "", `{"rate":2.5}`))
	if c.Rate != 2.5 || !c.Controllable {
		t.Fatalf("post answered %+v", c)
	}
	if g := decodeClock(t, h.clockAs(t, h.token(t), http.MethodGet, "", "")); g.Rate != 2.5 {
		t.Fatalf("get after post: %+v", g)
	}
}

func TestPlatformClock_Post_AdvanceThenGet(t *testing.T) {
	h := clockStack(t, true)
	start := decodeClock(t, h.clockAs(t, h.token(t), http.MethodGet, "", "")).Now
	c := decodeClock(t, h.clockAs(t, h.platformToken(t), http.MethodPost, "", fmt.Sprintf(`{"advanceByMs":%d}`, 3600*1000)))
	if c.Now.Sub(start) < time.Hour {
		t.Fatalf("clock advanced from %s to %s", start, c.Now)
	}
	if g := decodeClock(t, h.clockAs(t, h.token(t), http.MethodGet, "", "")); g.Now.Sub(start) < time.Hour {
		t.Fatalf("get after advance: %s", g.Now)
	}
}

func TestPlatformClock_BeforeRecordedTime409(t *testing.T) {
	h := clockStack(t, true)
	const sample = `{"k":1}`
	h.DoAuth(t, http.MethodPost, "/api/model/import/JSON/SAMPLE_DATA/ClockModel/1", sample, "").Body.Close()
	h.DoAuth(t, http.MethodPut, "/api/model/ClockModel/1/lock", "", "").Body.Close()
	if _, status, body := h.CreateEntity(t, "ClockModel", 1, sample); status != http.StatusOK {
		t.Fatalf("create entity: %d %s", status, body)
	}
	earlier := decodeClock(t, h.clockAs(t, h.token(t), http.MethodGet, "", "")).Now.Add(-time.Hour)
	resp := h.clockAs(t, h.platformToken(t), http.MethodPost, "", fmt.Sprintf(`{"time":%q}`, earlier.UTC().Format(time.RFC3339Nano)))
	raw := readBody(t, resp)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	var pd struct {
		Detail     string         `json:"detail"`
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal([]byte(raw), &pd); err != nil {
		t.Fatal(err)
	}
	if pd.Properties["errorCode"] != "CLOCK_BEFORE_RECORDED_TIME" || !strings.Contains(pd.Detail, "latest recorded time") {
		t.Fatalf("problem %s", raw)
	}
	latest, _ := pd.Properties["latestRecordedTime"].(string)
	lt, err := time.Parse(time.RFC3339, latest)
	if err != nil {
		t.Fatalf("latestRecordedTime %q is not RFC 3339: %v (%s)", latest, err, raw)
	}
	if !strings.HasSuffix(pd.Detail, latest) {
		t.Fatalf("detail %q does not name latestRecordedTime %q", pd.Detail, latest)
	}
	if !lt.After(earlier) {
		t.Fatalf("latestRecordedTime %s is not later than the requested %s", lt, earlier)
	}
	// The refused set left the clock where it was.
	if g := decodeClock(t, h.clockAs(t, h.token(t), http.MethodGet, "", "")); g.Now.Before(earlier.Add(time.Hour).Add(-time.Minute)) {
		t.Fatalf("clock moved by a refused set: %s", g.Now)
	}
}
