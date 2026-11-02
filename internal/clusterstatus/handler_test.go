package clusterstatus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/lifecycle"
)

// fakeReader records the arguments of each Read and answers report and err.
type fakeReader struct {
	report  Report
	err     error
	calls   int
	waitFor int
	timeout time.Duration
}

func (f *fakeReader) Read(_ context.Context, waitFor int, timeout time.Duration) (Report, error) {
	f.calls++
	f.waitFor, f.timeout = waitFor, timeout
	return f.report, f.err
}

func operatorRequest() *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/platform/cluster", nil)
	uc := &spi.UserContext{UserID: "op", Tenant: spi.Tenant{ID: auth.PlatformTenantID}, Roles: []string{"ROLE_ADMIN"}}
	return r.WithContext(spi.WithUserContext(r.Context(), uc))
}

func i32(v int32) *int32 { return &v }

func upNormalEntry(id string) Entry {
	st := NodeStatus{State: lifecycle.Normal, Database: true, Version: "v1", Clock: Clock{Controllable: true, Version: 7, Writer: "n1"}}
	return Entry{ID: id, Reachability: Up, State: lifecycle.Normal, Status: &st}
}

// assertJSON compares got and want as JSON values: member order aside, they
// must be equal, so a member that should be absent, or an array that is null,
// fails.
func assertJSON(t *testing.T, got []byte, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("body %s: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("body\n got %s\nwant %s", got, want)
	}
}

func TestHandler_RangeRefusals(t *testing.T) {
	cases := []struct {
		name   string
		params genapi.GetPlatformClusterParams
		param  string
	}{
		{"waitForNodes 0", genapi.GetPlatformClusterParams{WaitForNodes: i32(0)}, "waitForNodes"},
		{"waitForNodes 1001", genapi.GetPlatformClusterParams{WaitForNodes: i32(1001)}, "waitForNodes"},
		{"timeoutMs 0", genapi.GetPlatformClusterParams{WaitForNodes: i32(1), TimeoutMs: i32(0)}, "timeoutMs"},
		{"timeoutMs 600001", genapi.GetPlatformClusterParams{WaitForNodes: i32(1), TimeoutMs: i32(600001)}, "timeoutMs"},
		{"timeoutMs alone", genapi.GetPlatformClusterParams{TimeoutMs: i32(100)}, "timeoutMs"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeReader{}
			rec := httptest.NewRecorder()
			NewHandler(f, auth.OperatorGuard{}).GetPlatformCluster(rec, operatorRequest(), tc.params)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400: %s", rec.Code, rec.Body)
			}
			var body struct {
				Properties map[string]any `json:"properties"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Properties["errorCode"] != common.ErrCodeBadRequest || body.Properties["parameter"] != tc.param {
				t.Fatalf("properties %v, want errorCode %s and parameter %s", body.Properties, common.ErrCodeBadRequest, tc.param)
			}
			if f.calls != 0 {
				t.Fatalf("the service was called %d times", f.calls)
			}
		})
	}
}

func TestHandler_GuardFirst(t *testing.T) {
	f := &fakeReader{}
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/platform/cluster", nil)
	NewHandler(f, auth.OperatorGuard{}).GetPlatformCluster(rec, r, genapi.GetPlatformClusterParams{WaitForNodes: i32(0)})
	if rec.Code != http.StatusUnauthorized || f.calls != 0 {
		t.Fatalf("status %d, calls %d; want 401 and no call", rec.Code, f.calls)
	}
}

func TestHandler_NoWait_OneRound(t *testing.T) {
	f := &fakeReader{report: Report{Operational: true, Counts: []Count{{Up, lifecycle.Normal, 1}}, Nodes: []Entry{upNormalEntry("n1")}}}
	rec := httptest.NewRecorder()
	NewHandler(f, auth.OperatorGuard{}).GetPlatformCluster(rec, operatorRequest(), genapi.GetPlatformClusterParams{})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if f.waitFor != 0 {
		t.Fatalf("waitFor %d, want 0", f.waitFor)
	}
	want := `{"operational":true,"counts":[{"reachability":"UP","state":"NORMAL","count":1}],` +
		`"nodes":[{"id":"n1","reachability":"UP","state":"NORMAL","database":true,"version":"v1",` +
		`"clock":{"controllable":true,"version":7,"writer":"n1"}}]}`
	assertJSON(t, rec.Body.Bytes(), want)
}

func TestHandler_WaitDefaultsTimeout30s(t *testing.T) {
	f := &fakeReader{report: Report{Counts: []Count{}, Nodes: []Entry{}}}
	rec := httptest.NewRecorder()
	NewHandler(f, auth.OperatorGuard{}).GetPlatformCluster(rec, operatorRequest(), genapi.GetPlatformClusterParams{WaitForNodes: i32(2)})
	if f.calls != 1 || f.waitFor != 2 || f.timeout != 30*time.Second {
		t.Fatalf("calls %d, waitFor %d, timeout %s; want 1, 2, 30s", f.calls, f.waitFor, f.timeout)
	}
}

func TestHandler_TimeoutPassedThrough(t *testing.T) {
	f := &fakeReader{report: Report{Counts: []Count{}, Nodes: []Entry{}}}
	rec := httptest.NewRecorder()
	NewHandler(f, auth.OperatorGuard{}).GetPlatformCluster(rec, operatorRequest(), genapi.GetPlatformClusterParams{WaitForNodes: i32(1000), TimeoutMs: i32(600000)})
	if f.waitFor != 1000 || f.timeout != 600*time.Second {
		t.Fatalf("waitFor %d, timeout %s; want 1000, 10m", f.waitFor, f.timeout)
	}
}

func TestHandler_503SameBody(t *testing.T) {
	stateless := Entry{ID: "n2", Reachability: Down}
	f := &fakeReader{report: Report{
		Operational: false,
		Counts:      []Count{{Up, lifecycle.Normal, 1}, {Reachability: Down, Count: 1}},
		Nodes:       []Entry{upNormalEntry("n1"), stateless},
	}}
	rec := httptest.NewRecorder()
	NewHandler(f, auth.OperatorGuard{}).GetPlatformCluster(rec, operatorRequest(), genapi.GetPlatformClusterParams{})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type %q, want application/json", ct)
	}
	var dto genapi.ClusterStatusDto
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatal(err)
	}
	if dto.Operational || len(dto.Nodes) != 2 || len(dto.Counts) != 2 {
		t.Fatalf("body %s", rec.Body)
	}
	// A DOWN member carries no database, version or clock; one without a
	// state carries no state, in its entry and in its count.
	var raw struct {
		Counts []json.RawMessage `json:"counts"`
		Nodes  []json.RawMessage `json:"nodes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	assertJSON(t, raw.Nodes[1], `{"id":"n2","reachability":"DOWN"}`)
	assertJSON(t, raw.Counts[1], `{"reachability":"DOWN","count":1}`)
}

func TestHandler_EmptyArraysNotNull(t *testing.T) {
	f := &fakeReader{report: Report{Counts: []Count{}, Nodes: []Entry{}}}
	rec := httptest.NewRecorder()
	NewHandler(f, auth.OperatorGuard{}).GetPlatformCluster(rec, operatorRequest(), genapi.GetPlatformClusterParams{})
	assertJSON(t, rec.Body.Bytes(), `{"operational":false,"counts":[],"nodes":[]}`)
}

func TestHandler_ReadError_500Ticket(t *testing.T) {
	f := &fakeReader{err: errors.New("boom")}
	rec := httptest.NewRecorder()
	NewHandler(f, auth.OperatorGuard{}).GetPlatformCluster(rec, operatorRequest(), genapi.GetPlatformClusterParams{})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", rec.Code)
	}
	var body struct {
		Ticket string `json:"ticket"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Ticket == "" || strings.Contains(rec.Body.String(), "boom") {
		t.Fatalf("body %s: want a ticket and no cause text", rec.Body)
	}
}

func TestHandler_ClientGone_NoTicket(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(old)

	f := &fakeReader{err: common.ClientGone(context.Canceled)}
	rec := httptest.NewRecorder()
	NewHandler(f, auth.OperatorGuard{}).GetPlatformCluster(rec, operatorRequest(), genapi.GetPlatformClusterParams{})
	log := buf.String()
	if !strings.Contains(log, "level=DEBUG") {
		t.Fatalf("want a DEBUG line, got %q", log)
	}
	for _, lvl := range []string{"level=INFO", "level=WARN", "level=ERROR"} {
		if strings.Contains(log, lvl) {
			t.Fatalf("logged above DEBUG: %q", log)
		}
	}
	if strings.Contains(rec.Body.String(), "ticket") {
		t.Fatalf("body %s carries a ticket", rec.Body)
	}
}
