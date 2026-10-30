package parity

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// The platform clock on every backend: the instance-wide clock changes what
// every record is stamped with, so each scenario runs on a stack of its own
// (FreshStackFixture) on a fresh store with clock control on. A fixture
// without that capability skips them. The scenarios set the clock as the
// platform operator and do everything else as a tenant of the stack. They
// cover only the behaviour that needs no compute node. The deeper behaviour (rate,
// ordering across tenants, an interrupted move, callouts, the cluster) is the
// e2e suite's, internal/e2e/platform_clock_test.go.

// clockParityStart is the business time every scenario starts at, years
// before real time, so a stamp that does not read the platform clock cannot
// pass by accident.
var clockParityStart = time.Date(2015, 3, 1, 9, 0, 0, 0, time.UTC)

const (
	// clockNear bounds how far a stamp may lie from the time it should carry:
	// the clock runs at rate 1 from the set, so this is the real time between
	// the set and the stamp, with wide margin for a loaded host.
	clockNear = 30 * time.Second
	// clockFireWait bounds the wait for a transition to fire after a move.
	clockFireWait = 60 * time.Second
	// clockSampleDoc is the sample document of the scenarios' models.
	clockSampleDoc = `{"name": "Test Order", "amount": 100, "status": "draft"}`
)

// clockStack is a fresh clock-controlled stack, a client on it as a tenant
// admin, and the platform operator's token for the clock.
type clockStack struct {
	base     string
	operator string
	tenant   string
	c        *client.Client
}

// newClockStack starts a fresh stack, or skips when the fixture cannot.
func newClockStack(t *testing.T, fixture BackendFixture, env map[string]string) *clockStack {
	t.Helper()
	f, ok := fixture.(FreshStackFixture)
	if !ok {
		t.Skip("the fixture does not implement FreshStackFixture: the platform clock needs a stack of its own")
	}
	stack := f.NewFreshStack(t, env)
	tenant := stack.NewTenant(t).Token
	return &clockStack{
		base:     stack.BaseURL(),
		operator: stack.PlatformOperator(t).Token,
		tenant:   tenant,
		c:        client.NewClient(stack.BaseURL(), tenant),
	}
}

// do sends a request to the stack with bearer.
func (s *clockStack) do(t *testing.T, bearer, method, path, body string) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, s.base+path, rd)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: read body: %v", method, path, err)
	}
	return resp.StatusCode, raw
}

// clockAnswer is the body of GET and POST /platform/clock.
type clockAnswer struct {
	Now          time.Time `json:"now"`
	Rate         float64   `json:"rate"`
	Controllable bool      `json:"controllable"`
}

// clock sends method to /platform/clock and decodes a 200 answer.
func (s *clockStack) clock(t *testing.T, method, body string) clockAnswer {
	t.Helper()
	bearer := s.tenant // anyone reads the clock
	if method == http.MethodPost {
		bearer = s.operator
	}
	status, raw := s.do(t, bearer, method, "/api/platform/clock", body)
	if status != http.StatusOK {
		t.Fatalf("%s /platform/clock %s: %d %s", method, body, status, raw)
	}
	var a clockAnswer
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatalf("decode clock answer %s: %v", raw, err)
	}
	return a
}

// startAt sets the clock to at, on a store that has recorded nothing yet.
func (s *clockStack) startAt(t *testing.T, at time.Time) {
	t.Helper()
	a := s.clock(t, http.MethodPost, fmt.Sprintf(`{"time":%q}`, at.Format(time.RFC3339Nano)))
	requireWithinClockNear(t, "clock after the set", a.Now, at)
}

// requireWithinClockNear fails unless got lies within clockNear of want.
func requireWithinClockNear(t *testing.T, what string, got, want time.Time) {
	t.Helper()
	if d := got.Sub(want); d < -clockNear || d > clockNear {
		t.Fatalf("%s = %s; want within %s of %s (off by %s)", what,
			got.UTC().Format(time.RFC3339Nano), clockNear, want.UTC().Format(time.RFC3339Nano), d)
	}
}

// setupModel imports and locks a model of the sample document and, when
// workflow is not empty, imports it.
func (s *clockStack) setupModel(t *testing.T, name, workflow string) {
	t.Helper()
	if err := s.c.ImportModel(t, name, 1, clockSampleDoc); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := s.c.LockModel(t, name, 1); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	if workflow != "" {
		if err := s.c.ImportWorkflow(t, name, 1, workflow); err != nil {
			t.Fatalf("ImportWorkflow: %v", err)
		}
	}
}

// consistencyTime reads GET /entity/consistency-time.
func (s *clockStack) consistencyTime(t *testing.T) time.Time {
	t.Helper()
	status, raw := s.do(t, s.tenant, http.MethodGet, "/api/entity/consistency-time", "")
	if status != http.StatusOK {
		t.Fatalf("consistency time: %d %s", status, raw)
	}
	var ct struct {
		ConsistencyTime time.Time `json:"consistencyTime"`
	}
	if err := json.Unmarshal(raw, &ct); err != nil {
		t.Fatalf("decode consistency time %s: %v", raw, err)
	}
	return ct.ConsistencyTime
}

// awaitState polls the entity until it is in want, failing clockFireWait
// after the start. The deadline only bounds the wait; the outcome is the
// state.
func (s *clockStack) awaitState(t *testing.T, id uuid.UUID, want string) {
	t.Helper()
	deadline := time.Now().Add(clockFireWait) // realtime: test poll deadline
	last := ""
	for {
		got, err := s.c.GetEntity(t, id)
		if err != nil {
			t.Fatalf("GetEntity: %v", err)
		}
		if last = got.Meta.State; last == want {
			return
		}
		if time.Now().After(deadline) { // realtime: test poll deadline
			t.Fatalf("entity %s not in state %q within %s; last state %q", id, want, clockFireWait, last)
		}
		time.Sleep(50 * time.Millisecond) // poll interval
	}
}

// scheduledOpenToDone is Open -[Fire, after delayMs]-> Done, with no
// processor, so it needs no compute node.
func scheduledOpenToDone(name string, delayMs int64) string {
	b, _ := json.Marshal(map[string]any{
		"importMode": "REPLACE",
		"workflows": []any{map[string]any{
			"version": "1.5", "name": name, "initialState": "Open", "active": true,
			"states": map[string]any{
				"Open": map[string]any{"transitions": []any{map[string]any{
					"name": "Fire", "next": "Done", "manual": false,
					"schedule": map[string]any{"delayMs": delayMs}}}},
				"Done": map[string]any{},
			},
		}},
	})
	return string(b)
}

// RunPlatformClockPastStart: a fresh deployment set years into the past
// stamps its first revision, its first audit event and its consistency time
// with that time, and reports it.
func RunPlatformClockPastStart(t *testing.T, fixture BackendFixture) {
	s := newClockStack(t, fixture, nil)
	s.startAt(t, clockParityStart)

	model := "clkparity-first-" + uuid.NewString()[:8]
	s.setupModel(t, model, "")
	id, err := s.c.CreateEntity(t, model, 1, clockSampleDoc)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	got, err := s.c.GetEntity(t, id)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	requireWithinClockNear(t, "creationDate", got.Meta.CreationDate, clockParityStart)
	requireWithinClockNear(t, "lastUpdateTime", got.Meta.LastUpdateTime, clockParityStart)

	events, err := s.c.GetAuditEvents(t, id)
	if err != nil {
		t.Fatalf("GetAuditEvents: %v", err)
	}
	if len(events.Items) == 0 {
		t.Fatalf("entity %s has no audit events", id)
	}
	for _, ev := range events.Items {
		requireWithinClockNear(t, fmt.Sprintf("audit %s utcTime", ev.AuditEventType), ev.UtcTime, clockParityStart)
	}

	requireWithinClockNear(t, "consistency time", s.consistencyTime(t), clockParityStart)
	requireWithinClockNear(t, "GET /platform/clock", s.clock(t, http.MethodGet, "").Now, clockParityStart)
}

// RunPlatformClockAdvanceOneDay: a move of one day fires the transition due
// inside it at its due time, a revision written afterwards is dated the next
// day, and point-in-time reads answer on the platform clock.
func RunPlatformClockAdvanceOneDay(t *testing.T, fixture BackendFixture) {
	s := newClockStack(t, fixture, nil)
	s.startAt(t, clockParityStart)

	model := "clkparity-day-" + uuid.NewString()[:8]
	s.setupModel(t, model, scheduledOpenToDone("clkparity-day-wf", (3*time.Hour).Milliseconds()))
	id, err := s.c.CreateEntity(t, model, 1, clockSampleDoc)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	day2 := clockParityStart.Add(24 * time.Hour)
	moved := s.clock(t, http.MethodPost, fmt.Sprintf(`{"advanceByMs":%d}`, (24*time.Hour).Milliseconds()))
	requireWithinClockNear(t, "clock after the move", moved.Now, day2)

	s.awaitState(t, id, "Done")
	var armed, fired time.Time
	events, err := s.c.GetAuditEvents(t, id)
	if err != nil {
		t.Fatalf("GetAuditEvents: %v", err)
	}
	for _, ev := range events.Items {
		if ev.AuditEventType != "StateMachine" {
			continue
		}
		sm, err := ev.AsStateMachine()
		if err != nil {
			t.Fatalf("AsStateMachine: %v", err)
		}
		switch sm.EventType {
		case "SCHEDULED_TRANSITION_ARM":
			var d struct {
				ScheduledTime int64 `json:"scheduledTime"` // epoch milliseconds
			}
			if err := json.Unmarshal(sm.Data, &d); err != nil || d.ScheduledTime == 0 {
				t.Fatalf("arm event data %s: %v", sm.Data, err)
			}
			armed = time.UnixMilli(d.ScheduledTime).UTC()
		case "SCHEDULED_TRANSITION_FIRE":
			fired = sm.UtcTime
		}
	}
	if armed.IsZero() || fired.IsZero() {
		t.Fatalf("entity %s: arm event at %s, fire event at %s; want both", id, armed, fired)
	}
	requireWithinClockNear(t, "armed due time", armed, clockParityStart.Add(3*time.Hour))
	// The fire is recorded at the due time, not at the end of the move: a
	// move that jumped would record it hours late.
	if d := fired.Sub(armed); d < 0 || d > clockNear {
		t.Fatalf("the transition fired at %s, due %s: %s after its due time; want 0..%s", fired, armed, d, clockNear)
	}

	// A revision written now is dated day 2.
	if err := s.c.UpdateEntityData(t, id, `{"name":"Day two","amount":2,"status":"draft"}`); err != nil {
		t.Fatalf("UpdateEntityData: %v", err)
	}
	got, err := s.c.GetEntity(t, id)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	if got.Meta.State != "Done" {
		t.Fatalf("state after the update = %s; want Done", got.Meta.State)
	}
	requireWithinClockNear(t, "new revision lastUpdateTime", got.Meta.LastUpdateTime, day2)

	// Point-in-time reads answer on the platform's timeline.
	for _, c := range []struct {
		at   time.Time
		want string
	}{
		{clockParityStart.Add(2 * time.Hour), "Open"},
		{clockParityStart.Add(4 * time.Hour), "Done"},
	} {
		at, err := s.c.GetEntityAt(t, id, c.at)
		if err != nil {
			t.Fatalf("GetEntityAt %s: %v", c.at, err)
		}
		if at.Meta.State != c.want {
			t.Fatalf("state at %s = %s; want %s", c.at.Format(time.RFC3339), at.Meta.State, c.want)
		}
	}
	requireWithinClockNear(t, "GET /platform/clock", s.clock(t, http.MethodGet, "").Now, day2)
}

// RunPlatformClockBeforeRecordedTime409: setting the clock before the latest
// recorded time is refused with 409 CLOCK_BEFORE_RECORDED_TIME, the detail
// names that time, and the clock is left where it was.
func RunPlatformClockBeforeRecordedTime409(t *testing.T, fixture BackendFixture) {
	s := newClockStack(t, fixture, nil)
	s.startAt(t, clockParityStart)

	model := "clkparity-409-" + uuid.NewString()[:8]
	s.setupModel(t, model, "")
	if _, err := s.c.CreateEntity(t, model, 1, clockSampleDoc); err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	earlier := clockParityStart.Add(-time.Hour)
	status, raw := s.do(t, s.operator, http.MethodPost, "/api/platform/clock", fmt.Sprintf(`{"time":%q}`, earlier.Format(time.RFC3339Nano)))
	if status != http.StatusConflict {
		t.Fatalf("set before the recorded time: %d %s; want 409", status, raw)
	}
	var pd struct {
		Detail     string         `json:"detail"`
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(raw, &pd); err != nil {
		t.Fatalf("decode problem %s: %v", raw, err)
	}
	if pd.Properties["errorCode"] != "CLOCK_BEFORE_RECORDED_TIME" {
		t.Fatalf("problem %s; want errorCode CLOCK_BEFORE_RECORDED_TIME", raw)
	}
	// The latest recorded time is the entity's create, on 2015-03-01.
	if !strings.Contains(pd.Detail, "latest recorded time") || !strings.Contains(pd.Detail, "2015-03-01") {
		t.Fatalf("detail %q; want it to name the latest recorded time on 2015-03-01", pd.Detail)
	}
	// The refused set left the clock where it was.
	if now := s.clock(t, http.MethodGet, "").Now; now.Before(clockParityStart) {
		t.Fatalf("clock after the refused set = %s; want it unmoved from %s", now, clockParityStart)
	}
}

// RunPlatformClockPastStartAsyncSearch: under a past clock an async search's
// results are not reaped early, and a transaction's submit time still
// answers. The reaper runs every 100ms with a two-minute TTL: were it to
// compare the platform's finish time with real time, the results would be
// years past their TTL and gone at the next pass.
func RunPlatformClockPastStartAsyncSearch(t *testing.T, fixture BackendFixture) {
	const reapInterval = 100 * time.Millisecond
	s := newClockStack(t, fixture, map[string]string{
		"CYODA_SEARCH_REAP_INTERVAL": reapInterval.String(),
		"CYODA_SEARCH_SNAPSHOT_TTL":  "2m",
	})
	s.startAt(t, clockParityStart)

	model := "clkparity-search-" + uuid.NewString()[:8]
	s.setupModel(t, model, "")
	id, txID, err := s.c.CreateEntityWithTxID(t, model, 1, clockSampleDoc)
	if err != nil {
		t.Fatalf("CreateEntityWithTxID: %v", err)
	}

	jobID, err := s.c.SubmitAsyncSearch(t, model, 1, `{"type":"group","operator":"AND","conditions":[]}`)
	if err != nil {
		t.Fatalf("SubmitAsyncSearch: %v", err)
	}
	deadline := time.Now().Add(clockFireWait) // realtime: test poll deadline
	for {
		st, err := s.c.GetAsyncSearchStatus(t, jobID)
		if err != nil {
			t.Fatalf("GetAsyncSearchStatus: %v", err)
		}
		if st == "SUCCESSFUL" {
			break
		}
		if st != "RUNNING" || time.Now().After(deadline) { // realtime: test poll deadline
			t.Fatalf("job status %s; want SUCCESSFUL within %s", st, clockFireWait)
		}
		time.Sleep(50 * time.Millisecond) // poll interval
	}

	// Observe ten reaper passes: the results must be there at every one. This
	// is an assertion over a window, not a wait for an outcome.
	hasResult := func() bool {
		page, err := s.c.GetAsyncSearchResults(t, jobID)
		if err != nil {
			t.Fatalf("GetAsyncSearchResults after the reaper ran: %v", err)
		}
		for _, e := range page.Content {
			if e.Meta.ID == id.String() {
				return true
			}
		}
		return false
	}
	windowEnd := time.Now().Add(10 * reapInterval) // realtime: spans reaper passes
	for time.Now().Before(windowEnd) {             // realtime: spans reaper passes
		if !hasResult() {
			t.Fatalf("the async search's results were reaped under a past clock")
		}
		time.Sleep(reapInterval / 2) // poll interval
	}

	// A later commit evicts expired submit times from the store's in-process
	// map; the first create's submit time must survive it.
	if _, err := s.c.CreateEntity(t, model, 1, clockSampleDoc); err != nil {
		t.Fatalf("second CreateEntity: %v", err)
	}
	if !hasResult() {
		t.Fatalf("the async search's results are gone after a later commit")
	}
	status, raw := s.do(t, s.tenant, http.MethodGet, "/api/entity/"+id.String()+"/transitions?transactionId="+url.QueryEscape(txID), "")
	if status != http.StatusOK {
		t.Fatalf("transitions by transactionId: %d %s", status, raw)
	}
}
