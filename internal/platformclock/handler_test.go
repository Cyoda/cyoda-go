package platformclock_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/platformclock"
)

type fakeController struct {
	now   time.Time
	rate  int64
	err   error
	calls []platformclock.Change
}

func (f *fakeController) State() platformclock.State {
	return platformclock.State{Now: f.now, RatePPM: f.rate, Controllable: true}
}

func (f *fakeController) Change(_ context.Context, ch platformclock.Change) (platformclock.State, error) {
	f.calls = append(f.calls, ch)
	return f.State(), f.err
}

type storeDown struct{}

func (storeDown) Error() string            { return "dial tcp 10.0.0.1:5432: refused" }
func (storeDown) StorageUnavailable() bool { return true }

func operatorReq(method, body string) *http.Request {
	r := httptest.NewRequest(method, "/platform/clock", strings.NewReader(body))
	uc := &spi.UserContext{UserID: "op", Tenant: spi.Tenant{ID: auth.PlatformTenantID}, Roles: []string{"ROLE_ADMIN"}}
	return r.WithContext(spi.WithUserContext(r.Context(), uc))
}

func problem(t *testing.T, rec *httptest.ResponseRecorder) (code string, body map[string]any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	props, _ := body["properties"].(map[string]any)
	code, _ = props["errorCode"].(string)
	return code, body
}

func newFake() *fakeController {
	return &fakeController{now: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), rate: 1_000_000}
}

func TestHandler_Post_400(t *testing.T) {
	cases := []struct{ name, body string }{
		{"malformed", `{`},
		{"unknown_member", `{"foo":1}`},
		{"empty", `{}`},
		{"both_time_and_advance", `{"time":"2031-01-01T00:00:00Z","advanceByMs":5}`},
		{"advance_zero", `{"advanceByMs":0}`},
		{"advance_not_integer", `{"advanceByMs":1.5}`},
		{"advance_overflow", `{"advanceByMs":9223372036854775807}`},
		{"rate_below", `{"rate":0.09}`},
		{"rate_above", `{"rate":100.1}`},
		{"rate_seven_digits", `{"rate":1.1234567}`},
		{"rate_not_number", `{"rate":"2"}`},
		{"rate_below_ppm", `{"rate":0.0999999}`},
		{"rate_just_above_max", `{"rate":100.000001}`},
		{"rate_null", `{"rate":null,"advanceByMs":5}`},
		{"time_null", `{"time":null,"rate":2}`},
		{"advance_null", `{"advanceByMs":null,"rate":2}`},
		{"time_not_rfc3339", `{"time":"yesterday"}`},
		{"time_before_1970", `{"time":"1969-12-31T23:59:59Z"}`},
		{"time_after_9999", `{"time":"9999-12-31T23:59:59-01:00"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			h := platformclock.NewHandler(f, auth.MockOperatorGuard(), true)
			rec := httptest.NewRecorder()
			h.ChangePlatformClock(rec, operatorReq(http.MethodPost, tc.body))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			if code, _ := problem(t, rec); code != "BAD_REQUEST" {
				t.Fatalf("code %q", code)
			}
			if len(f.calls) != 0 {
				t.Fatalf("service called: %+v", f.calls)
			}
		})
	}
}

func TestHandler_Post_Accepted(t *testing.T) {
	t.Run("advance and rate", func(t *testing.T) {
		f := newFake()
		h := platformclock.NewHandler(f, auth.MockOperatorGuard(), true)
		rec := httptest.NewRecorder()
		h.ChangePlatformClock(rec, operatorReq(http.MethodPost, `{"advanceByMs":5,"rate":1.234567}`))
		if rec.Code != 200 {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		c := f.calls[0]
		if c.AdvanceBy == nil || *c.AdvanceBy != 5*time.Millisecond || c.RatePPM == nil || *c.RatePPM != 1_234_567 {
			t.Fatalf("change %+v", c)
		}
	})
	t.Run("exponent rate and time", func(t *testing.T) {
		f := newFake()
		h := platformclock.NewHandler(f, auth.MockOperatorGuard(), true)
		rec := httptest.NewRecorder()
		h.ChangePlatformClock(rec, operatorReq(http.MethodPost, `{"time":"2031-01-01T00:00:00Z","rate":1e1}`))
		if rec.Code != 200 {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		c := f.calls[0]
		if c.At == nil || !c.At.Equal(time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)) || *c.RatePPM != 10_000_000 {
			t.Fatalf("change %+v", c)
		}
	})
	t.Run("rate bounds", func(t *testing.T) {
		for body, want := range map[string]int64{`{"rate":0.1}`: 100_000, `{"rate":100}`: 100_000_000, `{"rate":1.000001}`: 1_000_001} {
			f := newFake()
			h := platformclock.NewHandler(f, auth.MockOperatorGuard(), true)
			rec := httptest.NewRecorder()
			h.ChangePlatformClock(rec, operatorReq(http.MethodPost, body))
			if rec.Code != 200 || *f.calls[0].RatePPM != want {
				t.Fatalf("%s: %d %s", body, rec.Code, rec.Body.String())
			}
		}
	})
}

func TestHandler_Post_OrderOfChecks(t *testing.T) {
	// Not an operator: 403 FORBIDDEN before the disabled refusal.
	h := platformclock.NewHandler(nil, auth.OperatorGuard{}, false)
	r := httptest.NewRequest(http.MethodPost, "/platform/clock", strings.NewReader(`{`))
	uc := &spi.UserContext{UserID: "u", Tenant: spi.Tenant{ID: "acme"}, Roles: []string{"ROLE_ADMIN"}}
	r = r.WithContext(spi.WithUserContext(r.Context(), uc))
	rec := httptest.NewRecorder()
	h.ChangePlatformClock(rec, r)
	if code, _ := problem(t, rec); rec.Code != 403 || code != "FORBIDDEN" {
		t.Fatalf("non operator: %d %s", rec.Code, code)
	}
	// Operator, feature off: 403 CLOCK_CONTROL_DISABLED before body validation.
	h = platformclock.NewHandler(nil, auth.MockOperatorGuard(), false)
	rec = httptest.NewRecorder()
	h.ChangePlatformClock(rec, operatorReq(http.MethodPost, `{`))
	if code, _ := problem(t, rec); rec.Code != 403 || code != "CLOCK_CONTROL_DISABLED" {
		t.Fatalf("disabled: %d %s", rec.Code, code)
	}
}

func TestHandler_409_BeforeRecordedTime(t *testing.T) {
	f := newFake()
	latest := time.Date(2029, 5, 6, 7, 8, 9, 0, time.UTC)
	f.err = &platformclock.BeforeRecordedTimeError{Latest: latest}
	h := platformclock.NewHandler(f, auth.MockOperatorGuard(), true)
	rec := httptest.NewRecorder()
	h.ChangePlatformClock(rec, operatorReq(http.MethodPost, `{"time":"2020-01-01T00:00:00Z"}`))
	code, body := problem(t, rec)
	if rec.Code != 409 || code != "CLOCK_BEFORE_RECORDED_TIME" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if d, _ := body["detail"].(string); !strings.Contains(d, "2029-05-06T07:08:09Z") {
		t.Fatalf("detail %q lacks the latest time", d)
	}
}

func TestHandler_503_StoreFails(t *testing.T) {
	f := newFake()
	f.err = storeDown{}
	h := platformclock.NewHandler(f, auth.MockOperatorGuard(), true)
	rec := httptest.NewRecorder()
	h.ChangePlatformClock(rec, operatorReq(http.MethodPost, `{"time":"2020-01-01T00:00:00Z"}`))
	if code, _ := problem(t, rec); rec.Code != 503 || code != "STORAGE_UNAVAILABLE" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "10.0.0.1") {
		t.Fatal("503 leaks the cause")
	}
	f.err = spi.ErrConsistencyTimeUnavailable
	rec = httptest.NewRecorder()
	h.ChangePlatformClock(rec, operatorReq(http.MethodPost, `{"time":"2020-01-01T00:00:00Z"}`))
	if code, _ := problem(t, rec); rec.Code != 503 || code != "CONSISTENCY_TIME_UNAVAILABLE" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestHandler_500_Internal(t *testing.T) {
	f := newFake()
	f.err = errors.New("secret internal detail")
	h := platformclock.NewHandler(f, auth.MockOperatorGuard(), true)
	rec := httptest.NewRecorder()
	h.ChangePlatformClock(rec, operatorReq(http.MethodPost, `{"rate":2}`))
	_, body := problem(t, rec)
	if rec.Code != 500 || body["ticket"] == nil {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "secret internal detail") {
		t.Fatal("500 leaks the cause")
	}
}

func TestHandler_Get(t *testing.T) {
	t.Run("on", func(t *testing.T) {
		f := newFake()
		f.rate = 2_500_000
		h := platformclock.NewHandler(f, auth.MockOperatorGuard(), true)
		rec := httptest.NewRecorder()
		h.GetPlatformClock(rec, httptest.NewRequest(http.MethodGet, "/platform/clock", nil))
		var got struct {
			Now          time.Time `json:"now"`
			Rate         float64   `json:"rate"`
			Controllable bool      `json:"controllable"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != 200 {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		if !got.Now.Equal(f.now) || got.Rate != 2.5 || !got.Controllable {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("off", func(t *testing.T) {
		h := platformclock.NewHandler(nil, auth.MockOperatorGuard(), false)
		rec := httptest.NewRecorder()
		before := time.Now() // realtime: bounds the answer
		h.GetPlatformClock(rec, httptest.NewRequest(http.MethodGet, "/platform/clock", nil))
		var got struct {
			Now          time.Time `json:"now"`
			Rate         float64   `json:"rate"`
			Controllable bool      `json:"controllable"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != 200 {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		if got.Now.Before(before) || got.Rate != 1 || got.Controllable {
			t.Fatalf("%+v", got)
		}
	})
}

// A clock that has run past the year 9999 cannot be written as a timestamp:
// the read answers the generic 5xx, with a ticket, and no 200 header.
func TestHandler_Get_BeyondYear9999_Is500(t *testing.T) {
	f := newFake()
	f.now = time.Date(10000, 1, 1, 0, 0, 1, 0, time.UTC)
	h := platformclock.NewHandler(f, auth.MockOperatorGuard(), true)
	rec := httptest.NewRecorder()
	h.GetPlatformClock(rec, httptest.NewRequest(http.MethodGet, "/platform/clock", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body %q", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	if body["ticket"] == nil {
		t.Errorf("no ticket in %v", body)
	}
}

// A change whose resulting clock is past the year 9999 cannot be answered as
// a timestamp: the write has happened, and the response is the generic 5xx
// with a ticket, never a 200 with an empty body.
func TestHandler_Post_ResultBeyondYear9999_Is500(t *testing.T) {
	f := newFake()
	f.now = time.Date(10000, 1, 1, 0, 0, 1, 0, time.UTC)
	h := platformclock.NewHandler(f, auth.MockOperatorGuard(), true)
	rec := httptest.NewRecorder()
	h.ChangePlatformClock(rec, operatorReq(http.MethodPost, `{"rate":2}`))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body %q", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	if body["ticket"] == nil {
		t.Errorf("no ticket in %v", body)
	}
}

// A JSON number big.Rat refuses (an exponent beyond its cap) is a number out
// of range, not "not a number".
func TestHandler_Post_HugeExponentRate_IsOutOfRange(t *testing.T) {
	f := newFake()
	h := platformclock.NewHandler(f, auth.MockOperatorGuard(), true)
	rec := httptest.NewRecorder()
	h.ChangePlatformClock(rec, operatorReq(http.MethodPost, `{"rate":1e999999999}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	_, body := problem(t, rec)
	if detail, _ := body["detail"].(string); !strings.Contains(detail, "from 0.1 to 100") {
		t.Fatalf("detail = %q, want the range", detail)
	}
}

// The service's range refusal is a 400.
func TestHandler_Post_OutOfRange_Is400(t *testing.T) {
	f := newFake()
	f.err = &platformclock.OutOfRangeError{}
	h := platformclock.NewHandler(f, auth.MockOperatorGuard(), true)
	rec := httptest.NewRecorder()
	h.ChangePlatformClock(rec, operatorReq(http.MethodPost, `{"advanceByMs":1000}`))
	if code, _ := problem(t, rec); rec.Code != http.StatusBadRequest || code != "BAD_REQUEST" {
		t.Fatalf("got %d %s: %s", rec.Code, code, rec.Body.String())
	}
}
