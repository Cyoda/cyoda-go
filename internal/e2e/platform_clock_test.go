package e2e_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	"github.com/cyoda-platform/cyoda-go/app"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/registry"
	"github.com/cyoda-platform/cyoda-go/internal/platformclock"
)

// What the platform clock does to the platform's behaviour, through the full
// HTTP stack on postgres: stamps, moves through scheduled transitions, rate,
// async search reaping, in-transaction placeholder dates, the callout time
// attribute, the setting across cluster nodes, and the startup check. Each
// test runs on a stack and database of its own with clock control on. The
// API contract (status codes) is platform_clock_api_test.go's.
//
// Every test starts the clock at clockStart, years before real time, so a
// stamp that does not read the platform clock cannot pass by accident.

// clockStart is the business time every test starts at.
var clockStart = time.Date(2015, 3, 1, 9, 0, 0, 0, time.UTC)

// fireSlack bounds how late a scheduled transition may be recorded after its
// due time: one scan interval (50ms, schedulerTuning) at rate 1, plus the
// fire itself, with margin for a loaded host.
const fireSlack = 2 * time.Second

// clockTestStack is a stack of its own with clock control on and the tuned
// scheduler. configure runs last and may override either.
func clockTestStack(t *testing.T, configure func(*app.Config)) (*callbackHarness, *schedDB) {
	t.Helper()
	s := newSchedDB(t)
	h := newStackOn(t, s, func(cfg *app.Config) {
		cfg.ClockControlEnabled = true
		if configure != nil {
			configure(cfg)
		}
	})
	return h, s
}

// postClock sends a change as the platform operator and returns the answer.
func postClock(t *testing.T, h *callbackHarness, body string) clockBody {
	t.Helper()
	return decodeClock(t, h.clockAs(t, h.platformToken(t), http.MethodPost, "", body))
}

// getClock reads the clock as a tenant admin.
func getClock(t *testing.T, h *callbackHarness) clockBody {
	t.Helper()
	return decodeClock(t, h.clockAs(t, h.token(t), http.MethodGet, "", ""))
}

// startClockAt sets the clock to at, on a store that has recorded nothing
// yet.
func startClockAt(t *testing.T, h *callbackHarness, at time.Time) {
	t.Helper()
	c := postClock(t, h, fmt.Sprintf(`{"time":%q}`, at.Format(time.RFC3339Nano)))
	assertNear(t, "clock after the set", c.Now, at, 5*time.Second)
}

// advanceClock moves the clock forward by d and returns the answer.
func advanceClock(t *testing.T, h *callbackHarness, d time.Duration) clockBody {
	t.Helper()
	return postClock(t, h, fmt.Sprintf(`{"advanceByMs":%d}`, d.Milliseconds()))
}

// assertNear fails unless got lies within tol of want.
func assertNear(t *testing.T, what string, got, want time.Time, tol time.Duration) {
	t.Helper()
	if d := got.Sub(want); d < -tol || d > tol {
		t.Fatalf("%s = %s; want within %s of %s (off by %s)", what,
			got.UTC().Format(time.RFC3339Nano), tol, want.UTC().Format(time.RFC3339Nano), d)
	}
}

// assertFiredOnTime fails unless fired lies in [due, due+fireSlack].
func assertFiredOnTime(t *testing.T, what string, fired, due time.Time) {
	t.Helper()
	if d := fired.Sub(due); d < 0 || d > fireSlack {
		t.Fatalf("%s fired at %s, due %s: %s after its due time; want 0..%s", what,
			fired.UTC().Format(time.RFC3339Nano), due.UTC().Format(time.RFC3339Nano), d, fireSlack)
	}
}

// parseStamp parses an RFC 3339 stamp from a response.
func parseStamp(t *testing.T, what string, v any) time.Time {
	t.Helper()
	s, _ := v.(string)
	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("%s: unparseable time %q: %v", what, s, err)
	}
	return ts
}

// setupModelAs imports and locks the sample model and imports wf, as bearer.
func setupModelAs(t *testing.T, h *callbackHarness, bearer, model, wf string) {
	t.Helper()
	for _, step := range []struct{ method, path, body string }{
		{http.MethodPost, fmt.Sprintf("/api/model/import/JSON/SAMPLE_DATA/%s/1", model), workflowSampleModel},
		{http.MethodPut, fmt.Sprintf("/api/model/%s/1/lock", model), ""},
		{http.MethodPost, fmt.Sprintf("/api/model/%s/1/workflow/import", model), wf},
	} {
		resp := h.DoAuthBearer(t, step.method, step.path, step.body, "", bearer)
		if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
			t.Fatalf("%s %s: %d %s", step.method, step.path, resp.StatusCode, body)
		}
	}
}

// createAs creates an entity of model as bearer and returns its id and the
// transaction id of the create.
func createAs(t *testing.T, h *callbackHarness, bearer, model string) (id, txID string) {
	t.Helper()
	resp := h.DoAuthBearer(t, http.MethodPost, fmt.Sprintf("/api/entity/JSON/%s/1", model), workflowSampleModel, "", bearer)
	body := h.readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create %s: %d %s", model, resp.StatusCode, body)
	}
	var arr []struct {
		TransactionID string   `json:"transactionId"`
		EntityIDs     []string `json:"entityIds"`
	}
	if err := json.Unmarshal([]byte(body), &arr); err != nil || len(arr) == 0 || len(arr[0].EntityIDs) == 0 {
		t.Fatalf("create %s: unexpected answer %s", model, body)
	}
	return arr[0].EntityIDs[0], arr[0].TransactionID
}

// entityAt reads an entity as bearer, at pointInTime when it is not zero,
// and returns its meta.
func entityAt(t *testing.T, h *callbackHarness, bearer, id string, pointInTime time.Time) map[string]any {
	t.Helper()
	path := "/api/entity/" + id
	if !pointInTime.IsZero() {
		path += "?pointInTime=" + url.QueryEscape(pointInTime.UTC().Format(time.RFC3339Nano))
	}
	resp := h.DoAuthBearer(t, http.MethodGet, path, "", "", bearer)
	body := h.readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, resp.StatusCode, body)
	}
	var env struct {
		Meta map[string]any `json:"meta"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return env.Meta
}

// smEventsAs returns the entity's StateMachine audit events as bearer,
// newest first.
func smEventsAs(t *testing.T, h *callbackHarness, bearer, id string) []map[string]any {
	t.Helper()
	resp := h.DoAuthBearer(t, http.MethodGet, "/api/audit/entity/"+id+"?eventType=StateMachine&limit=500", "", "", bearer)
	body := h.readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("audit GET %s: %d %s", id, resp.StatusCode, body)
	}
	var r struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("decode audit page: %v (body %s)", err, body)
	}
	return r.Items
}

// scheduledFire is one scheduled transition's armed due time and the time
// its fire was recorded.
type scheduledFire struct {
	what      string
	due, fire time.Time
}

// firedTransition returns the due time the entity's transition was armed
// with and the time its SCHEDULED_TRANSITION_FIRE was recorded, failing
// unless each happened exactly once.
func firedTransition(t *testing.T, events []map[string]any, what, transition string) scheduledFire {
	t.Helper()
	pick := func(eventType string) map[string]any {
		var found []map[string]any
		for _, ev := range smEventsOfType(events, eventType) {
			if data, _ := ev["data"].(map[string]any); data["transition"] == transition {
				found = append(found, ev)
			}
		}
		if len(found) != 1 {
			t.Fatalf("%s: %d %s events for %s, want 1: %+v", what, len(found), eventType, transition, events)
		}
		return found[0]
	}
	return scheduledFire{
		what: what,
		due:  smEventScheduledTime(t, pick("SCHEDULED_TRANSITION_ARM")),
		fire: smEventTime(t, pick("SCHEDULED_TRANSITION_FIRE")),
	}
}

// awaitStateAs polls the entity's state as bearer until it is want.
func awaitStateAs(t *testing.T, h *callbackHarness, bearer, id, want string) {
	t.Helper()
	awaitScheduler(t, scheduledFireTimeout, fmt.Sprintf("entity %s in state %q", id, want), func() (bool, string) {
		st, _ := entityAt(t, h, bearer, id, time.Time{})["state"].(string)
		return st == want, fmt.Sprintf("state %q", st)
	})
}

// TestPlatformClock_PastStart_FirstRecordsCarryIt: a fresh deployment set
// years into the past stamps its first revision, its first audit event and
// its consistency time with that time, and reports it.
func TestPlatformClock_PastStart_FirstRecordsCarryIt(t *testing.T) {
	h, _ := clockTestStack(t, nil)
	startClockAt(t, h, clockStart)

	model := uniq("clk-first")
	h.SetupModelWithWorkflow(t, model, secondaryWorkflow)
	id, _ := createAs(t, h, h.token(t), model)

	meta := entityAt(t, h, h.token(t), id, time.Time{})
	assertNear(t, "creationDate", parseStamp(t, "creationDate", meta["creationDate"]), clockStart, 5*time.Second)
	assertNear(t, "lastUpdateTime", parseStamp(t, "lastUpdateTime", meta["lastUpdateTime"]), clockStart, 5*time.Second)

	events := h.GetAllAuditEvents(t, id)
	if len(events) == 0 {
		t.Fatalf("entity %s has no audit events", id)
	}
	for _, ev := range events {
		assertNear(t, fmt.Sprintf("audit %v utcTime", ev["eventType"]), smEventTime(t, ev), clockStart, 5*time.Second)
	}

	resp := h.DoAuth(t, http.MethodGet, "/api/entity/consistency-time", "", "")
	body := h.readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("consistency time: %d %s", resp.StatusCode, body)
	}
	var ct struct {
		ConsistencyTime time.Time `json:"consistencyTime"`
	}
	if err := json.Unmarshal([]byte(body), &ct); err != nil {
		t.Fatal(err)
	}
	assertNear(t, "consistency time", ct.ConsistencyTime, clockStart, 5*time.Second)
	assertNear(t, "GET /platform/clock", getClock(t, h).Now, clockStart, 5*time.Second)
}

// TestPlatformClock_AdvanceOneDay is the feature's "done when": a move of
// one day fires the transition due inside it at its due time, a revision
// written afterwards is dated the next day, and point-in-time reads answer
// on the platform clock.
func TestPlatformClock_AdvanceOneDay(t *testing.T) {
	h, _ := clockTestStack(t, nil)
	startClockAt(t, h, clockStart)

	model := uniq("clk-day")
	h.SetupModelWithWorkflow(t, model, fireOpenToDone("clk-day-wf", (3*time.Hour).Milliseconds(), 0))
	id, _ := createAs(t, h, h.token(t), model)

	moved := advanceClock(t, h, 24*time.Hour)
	day2 := clockStart.Add(24 * time.Hour)
	assertNear(t, "clock after the move", moved.Now, day2, 10*time.Second)

	awaitStateAs(t, h, h.token(t), id, "Done")
	f := firedTransition(t, h.GetSMAuditEvents(t, id), "Fire", "Fire")
	assertNear(t, "armed due time", f.due, clockStart.Add(3*time.Hour), 5*time.Second)
	assertFiredOnTime(t, "Fire", f.fire, f.due)

	// A revision written now is dated day 2.
	resp := h.DoAuth(t, http.MethodPut, "/api/entity/JSON/"+id, `{"name":"Day two","amount":2,"status":"draft"}`, "")
	if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("update: %d %s", resp.StatusCode, body)
	}
	meta := entityAt(t, h, h.token(t), id, time.Time{})
	if meta["state"] != "Done" {
		t.Fatalf("read with no pointInTime answers state %v; want Done", meta["state"])
	}
	assertNear(t, "new revision lastUpdateTime", parseStamp(t, "lastUpdateTime", meta["lastUpdateTime"]), day2, 10*time.Second)

	// Point-in-time reads answer on the platform's timeline.
	if st := entityAt(t, h, h.token(t), id, clockStart.Add(2*time.Hour))["state"]; st != "Open" {
		t.Fatalf("state at day 1 11:00 = %v; want Open", st)
	}
	if st := entityAt(t, h, h.token(t), id, clockStart.Add(4*time.Hour))["state"]; st != "Done" {
		t.Fatalf("state at day 1 13:00 = %v; want Done", st)
	}
	assertNear(t, "GET /platform/clock", getClock(t, h).Now, day2, 10*time.Second)
}

// TestPlatformClock_MoveOrderAcrossTenants: a move fires the transitions of
// entities in two tenants in due order, each at its own due time, and a
// transition armed by a fire inside the window fires in its place.
func TestPlatformClock_MoveOrderAcrossTenants(t *testing.T) {
	h, _ := clockTestStack(t, nil)
	startClockAt(t, h, clockStart)

	tokA := h.token(t)
	tokB := h.adminTokenFor(t, "clock-tenant-b-"+randSuffix(t)[:8], "clock-b-admin")
	hour := time.Hour.Milliseconds()

	modelA1, modelA4, modelB := uniq("clk-a1"), uniq("clk-a4"), uniq("clk-b2")
	setupModelAs(t, h, tokA, modelA1, fireOpenToDone("clk-a1-wf", hour, 0))
	setupModelAs(t, h, tokA, modelA4, fireOpenToDone("clk-a4-wf", 4*hour, 0))
	// Open -[Fire, +2h]-> Mid -[Then, +1h]-> Done: the fire arms a transition
	// due at +3h, between the other two tenants' fires.
	setupModelAs(t, h, tokB, modelB, schedDoc("clk-b2-wf", map[string]any{
		"Open": map[string]any{"transitions": []any{map[string]any{
			"name": "Fire", "next": "Mid", "manual": false, "schedule": map[string]any{"delayMs": 2 * hour}}}},
		"Mid": map[string]any{"transitions": []any{map[string]any{
			"name": "Then", "next": "Done", "manual": false, "schedule": map[string]any{"delayMs": hour}}}},
		"Done": map[string]any{},
	}))

	a1, _ := createAs(t, h, tokA, modelA1)
	b2, _ := createAs(t, h, tokB, modelB)
	a4, _ := createAs(t, h, tokA, modelA4)

	moved := advanceClock(t, h, 5*time.Hour)
	if moved.Now.Before(clockStart.Add(5 * time.Hour)) {
		t.Fatalf("clock after the move = %s; want at or after +5h", moved.Now)
	}
	awaitStateAs(t, h, tokA, a1, "Done")
	awaitStateAs(t, h, tokB, b2, "Done")
	awaitStateAs(t, h, tokA, a4, "Done")

	bEvents := smEventsAs(t, h, tokB, b2)
	fires := []scheduledFire{
		firedTransition(t, smEventsAs(t, h, tokA, a1), "tenant A +1h", "Fire"),
		firedTransition(t, bEvents, "tenant B +2h", "Fire"),
		firedTransition(t, bEvents, "tenant B +3h (armed by the +2h fire)", "Then"),
		firedTransition(t, smEventsAs(t, h, tokA, a4), "tenant A +4h", "Fire"),
	}
	for i, f := range fires {
		assertNear(t, f.what+" due time", f.due, clockStart.Add(time.Duration(i+1)*time.Hour), 10*time.Second)
		assertFiredOnTime(t, f.what, f.fire, f.due)
		if i > 0 && !f.fire.After(fires[i-1].fire) {
			t.Fatalf("%s fired at %s, not after %s at %s", f.what, f.fire, fires[i-1].what, fires[i-1].fire)
		}
	}
}

// TestPlatformClock_TimeLimitShorterThanMove_Fires: a transition whose time
// limit is shorter than the move fires at its due time; a jump would have
// recorded it expired.
func TestPlatformClock_TimeLimitShorterThanMove_Fires(t *testing.T) {
	h, _ := clockTestStack(t, nil)
	startClockAt(t, h, clockStart)

	model := uniq("clk-limit")
	h.SetupModelWithWorkflow(t, model, fireOpenToDone("clk-limit-wf", (2*time.Hour).Milliseconds(), time.Hour.Milliseconds()))
	id, _ := createAs(t, h, h.token(t), model)

	advanceClock(t, h, 24*time.Hour)
	awaitStateAs(t, h, h.token(t), id, "Done")
	events := h.GetSMAuditEvents(t, id)
	if exp := smEventsOfType(events, "SCHEDULED_TRANSITION_EXPIRE"); len(exp) != 0 {
		t.Fatalf("the transition was expired: %+v", exp)
	}
	f := firedTransition(t, events, "Fire", "Fire")
	assertFiredOnTime(t, "Fire", f.fire, f.due)
}

// TestPlatformClock_RateTwo: a rate change does not jump the time, and at
// rate 2 the clock runs twice as fast as real time.
func TestPlatformClock_RateTwo(t *testing.T) {
	h, _ := clockTestStack(t, nil)
	startClockAt(t, h, clockStart)

	r0 := time.Now() // realtime: bounds the server's reads
	before := getClock(t, h)
	changed := postClock(t, h, `{"rate":2}`)
	r1 := time.Now() // realtime: bounds the server's reads
	if changed.Rate != 2 {
		t.Fatalf("rate after the change = %v; want 2", changed.Rate)
	}
	// No jump: at most the real time between the two reads, at the new rate.
	if d := changed.Now.Sub(before.Now); d < 0 || d > 2*r1.Sub(r0)+50*time.Millisecond {
		t.Fatalf("the rate change moved the clock by %s within %s of real time", d, r1.Sub(r0))
	}

	// The real time between the two server reads lies within [a1-a0', b1-b0]
	// of the client's reads around them; at rate 2 the platform time between
	// them is twice that.
	a0 := time.Now() // realtime: bounds the server's read
	first := getClock(t, h)
	a1 := time.Now()            // realtime: bounds the server's read
	time.Sleep(2 * time.Second) // the measured interval, not a wait for an outcome
	b0 := time.Now()            // realtime: bounds the server's read
	second := getClock(t, h)
	b1 := time.Now() // realtime: bounds the server's read
	platform := second.Now.Sub(first.Now)
	lo, hi := 2*b0.Sub(a1)-50*time.Millisecond, 2*b1.Sub(a0)+50*time.Millisecond
	if platform < lo || platform > hi {
		t.Fatalf("platform time advanced %s over about %s of real time; want %s..%s", platform, b0.Sub(a1), lo, hi)
	}
	if second.Rate != 2 {
		t.Fatalf("rate = %v; want 2", second.Rate)
	}
}

// TestPlatformClock_PastStart_AsyncSearchSurvives: under a past clock an
// async search's results are not reaped early, and a transaction's submit
// time still answers. The reaper runs every 100ms with a two-minute TTL:
// were it to compare the platform's finish time with real time, the results
// would be years past their TTL and gone at the next pass.
//
// The model has no imported workflow, so the entity's create is the stack's
// first commit: the store prunes expired submit times after the first commit
// and then at most every five minutes, so this prune is the one that would
// drop the create's submit time if it compared it with real time.
func TestPlatformClock_PastStart_AsyncSearchSurvives(t *testing.T) {
	const reapInterval = 100 * time.Millisecond
	h, _ := clockTestStack(t, func(cfg *app.Config) {
		cfg.SearchReapInterval = reapInterval
		cfg.SearchSnapshotTTL = 2 * time.Minute
	})
	startClockAt(t, h, clockStart)

	model := uniq("clk-search")
	for _, step := range []struct{ method, path, body string }{
		{http.MethodPost, fmt.Sprintf("/api/model/import/JSON/SAMPLE_DATA/%s/1", model), workflowSampleModel},
		{http.MethodPut, fmt.Sprintf("/api/model/%s/1/lock", model), ""},
	} {
		resp := h.DoAuth(t, step.method, step.path, step.body, "")
		if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
			t.Fatalf("%s %s: %d %s", step.method, step.path, resp.StatusCode, body)
		}
	}
	id, txID := createAs(t, h, h.token(t), model)

	resp := h.DoAuth(t, http.MethodPost, "/api/search/async/"+model+"/1", `{"type":"group","operator":"AND","conditions":[]}`, "")
	body := h.readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("submit: %d %s", resp.StatusCode, body)
	}
	jobID := strings.Trim(strings.TrimSpace(body), `"`)
	if st := h.waitForAsyncTerminal(t, jobID, 15*time.Second); st != "SUCCESSFUL" {
		t.Fatalf("job status %s; want SUCCESSFUL", st)
	}

	// Several reaper passes, and a later commit, which evicts expired
	// submit times from the store's in-process map.
	time.Sleep(10 * reapInterval) // lets the reaper run; the outcome is asserted below
	createAs(t, h, h.token(t), model)

	resp = h.DoAuth(t, http.MethodGet, "/api/search/async/"+jobID, "", "")
	body = h.readBody(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, id) {
		t.Fatalf("results after the reaper ran: %d %s", resp.StatusCode, body)
	}
	resp = h.DoAuth(t, http.MethodGet, "/api/entity/"+id+"/transitions?transactionId="+url.QueryEscape(txID), "", "")
	if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("transitions by transactionId: %d %s", resp.StatusCode, body)
	}
}

// TestPlatformClock_InterruptedMove: a move waits while a due task is
// pending (no scheduler runs here); a client that gives up leaves the clock
// at the step the move reached, running from there.
func TestPlatformClock_InterruptedMove(t *testing.T) {
	h, _ := clockTestStack(t, func(cfg *app.Config) { cfg.Scheduler.Enabled = false })
	startClockAt(t, h, clockStart)

	model := uniq("clk-interrupt")
	h.SetupModelWithWorkflow(t, model, fireOpenToDone("clk-interrupt-wf", time.Hour.Milliseconds(), 0))
	id, _ := createAs(t, h, h.token(t), model)
	arms := smEventsOfType(h.GetSMAuditEvents(t, id), "SCHEDULED_TRANSITION_ARM")
	if len(arms) != 1 {
		t.Fatalf("%d SCHEDULED_TRANSITION_ARM events; want 1", len(arms))
	}
	due := smEventScheduledTime(t, arms[0])

	req, err := e2eNewRequest(t, http.MethodPost, h.baseURL+"/api/platform/clock",
		strings.NewReader(fmt.Sprintf(`{"advanceByMs":%d}`, (24*time.Hour).Milliseconds())))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.platformToken(t))
	req.Header.Set("Content-Type", "application/json")
	sent := time.Now() // realtime: bounds how far the clock can have run since the step
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err == nil {
		body := readBody(t, resp)
		t.Fatalf("the move answered %d %s; want it to wait at the due task", resp.StatusCode, body)
	}
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("move: %v; want a client timeout", err)
	}

	now := getClock(t, h).Now
	ran := time.Since(sent) // realtime: how long the clock can have run since the step
	if now.Before(due) || now.After(due.Add(ran+time.Second)) {
		t.Fatalf("clock after the interrupted move = %s; want the step's due time %s plus at most %s",
			now.Format(time.RFC3339Nano), due.Format(time.RFC3339Nano), ran+time.Second)
	}
	if st := entityAt(t, h, h.token(t), id, time.Time{})["state"]; st != "Open" {
		t.Fatalf("state = %v; want Open (no scheduler runs)", st)
	}
}

// TestPlatformClock_PlaceholderDatesInsideTx: inside a transaction, before
// the commit stamps it, a new entity read back carries platform-time dates.
// The transaction is a processor's: the compute node creates an entity in
// it and reads it back over the joined pass.
func TestPlatformClock_PlaceholderDatesInsideTx(t *testing.T) {
	h, _ := clockTestStack(t, nil)
	// Set first, before anything else reaches the store.
	startClockAt(t, h, clockStart)
	h.member = h.AttachCnode(t, cnodeSpec{name: "default", tags: []string{scheduledFnTag}, script: h.registeredScript}).m

	secondary, primary := uniq("clk-ptx-sec"), uniq("clk-ptx-pri")
	h.SetupModelWithWorkflow(t, secondary, secondaryWorkflow)
	h.SetupModelWithWorkflow(t, primary, procWorkflowJSON("clk-ptx-wf", "clk-ptx-proc", "SYNC", nil))

	type readBack struct {
		attached map[string]any // the primary's meta as dispatched
		meta     map[string]any // the secondary's meta read inside T
		err      error
	}
	got := make(chan readBack, 1)
	h.RegisterProc("clk-ptx-proc", func(rc *reqCtx) (map[string]any, error) {
		res, err := rc.CreateEntity(secondary, 1, workflowSampleModel)
		if err == nil && res.StatusCode != http.StatusOK {
			err = fmt.Errorf("create in T: %d %s", res.StatusCode, res.Body)
		}
		if err != nil {
			got <- readBack{err: err}
			return nil, err
		}
		read, err := rc.GetEntity(res.EntityID)
		if err == nil && read.StatusCode != http.StatusOK {
			err = fmt.Errorf("read in T: %d %s", read.StatusCode, read.Body)
		}
		var env struct {
			Meta map[string]any `json:"meta"`
		}
		if err == nil {
			err = json.Unmarshal([]byte(read.Body), &env)
		}
		got <- readBack{attached: rc.entityMeta, meta: env.Meta, err: err}
		return nil, err
	})

	createAs(t, h, h.token(t), primary)
	rb := awaitReceive(t, got, 10*time.Second, "the processor's read inside T")
	if rb.err != nil {
		t.Fatal(rb.err)
	}
	for _, f := range []struct {
		what string
		v    any
	}{
		{"secondary creationDate read in T", rb.meta["creationDate"]},
		{"secondary lastUpdateTime read in T", rb.meta["lastUpdateTime"]},
		{"primary creationDate as dispatched", rb.attached["creationDate"]},
	} {
		assertNear(t, f.what, parseStamp(t, f.what, f.v), clockStart, 5*time.Second)
	}
}

// TestPlatformClock_CalloutTimeAttribute: a schedule function reads the
// platform time from the callout's CloudEvents time attribute and returns an
// absolute fire time an hour later; a move of two hours fires it at that
// time.
func TestPlatformClock_CalloutTimeAttribute(t *testing.T) {
	h, s := clockTestStack(t, nil)
	startClockAt(t, h, clockStart)
	h.member = h.AttachCnode(t, cnodeSpec{name: "default", tags: []string{scheduledFnTag}, script: h.registeredScript}).m

	var attr atomic.Value // string: the time attribute the function received
	h.RegisterFunction("clk-abs-fire", func(rc *reqCtx) (string, map[string]any, error) {
		raw := rc.attrs["time"]
		attr.Store(raw)
		at, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return "", nil, fmt.Errorf("callout time attribute %q: %w", raw, err)
		}
		return "Schedule", map[string]any{"fireAt": at.Add(time.Hour).UnixMilli()}, nil
	})
	model := uniq("clk-callout")
	h.SetupModelWithWorkflow(t, model, scheduleFunctionWorkflowJSON("clk-callout-wf", validScheduleFunctionJSON("clk-abs-fire")))
	id, _ := createAs(t, h, h.token(t), model)

	raw, _ := attr.Load().(string)
	sent := parseStamp(t, "callout time attribute", raw)
	assertNear(t, "callout time attribute", sent, clockStart, 5*time.Second)
	want := sent.Add(time.Hour).UnixMilli()
	if n := s.count(t, "SELECT count(*) FROM scheduled_tasks WHERE entity_id = $1 AND scheduled_time = $2", id, want); n != 1 {
		t.Fatalf("no task armed at the attribute's time + 1h (%d)", want)
	}

	advanceClock(t, h, 2*time.Hour)
	awaitStateAs(t, h, h.token(t), id, "Closed")
	f := firedTransition(t, h.GetSMAuditEvents(t, id), "AutoClose", "AutoClose")
	if f.due.UnixMilli() != want {
		t.Fatalf("armed due time %s; want the attribute's time + 1h", f.due)
	}
	assertFiredOnTime(t, "AutoClose", f.fire, f.due)
}

// newClockSeed starts a cluster member outside cyoda-go that the clock
// test's nodes join through, and returns its gossip address: an app node's gossip port is the kernel's choice and cannot
// be named as a seed. It runs the platform clock's own gossip code, so like
// every node it adopts a broadcast setting and hands it to a node that joins
// through it.
func newClockSeed(t *testing.T) string {
	t.Helper()
	nodeID := "clock-seed-" + randSuffix(t)[:8]
	for attempt := 1; ; attempt++ {
		port := freeLoopbackPort(t)
		g, err := registry.NewGossip(registry.GossipConfig{
			NodeID:           nodeID,
			NodeAddr:         "http://127.0.0.1:1",
			BindAddr:         "127.0.0.1",
			BindPort:         port,
			SecretKey:        clusterHMACSecret32,
			ListScanInterval: 100 * time.Millisecond,
		})
		if err == nil {
			platformclock.Attach(platformclock.NewTest(nodeID, nil), g, g)
			t.Cleanup(func() { _ = g.Deregister(context.Background(), nodeID) })
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := g.Register(ctx, nodeID, ""); err != nil {
				t.Fatalf("clock seed could not start: %v", err)
			}
			return net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", port))
		}
		if attempt == 5 {
			t.Fatalf("clock seed could not bind a gossip port: %v", err)
		}
	}
}

// clockClusterNode is a cluster node with clock control on, on database s,
// joining through seed. Every node signs with key, so one token serves all.
func clockClusterNode(t *testing.T, s *schedDB, key *rsa.PrivateKey, nodeID, seed string) *callbackHarness {
	t.Helper()
	return newCalloutHarnessWithKey(t, key, func(cfg *app.Config) {
		t.Setenv("CYODA_POSTGRES_URL", s.url)
		schedulerTuning(cfg)
		cfg.ClockControlEnabled = true
		cfg.Cluster.Enabled = true
		cfg.Cluster.NodeID = nodeID
		cfg.Cluster.NodeAddr = fmt.Sprintf("http://127.0.0.1:%d", cfg.HTTPPort)
		cfg.Cluster.GossipAddr = "127.0.0.1:0"
		cfg.Cluster.SeedNodes = []string{seed}
		cfg.Cluster.StabilityWindow = 0
		cfg.Cluster.HMACSecret = clusterHMACSecret32
		cfg.Cluster.DispatchAllowLoopback = true
	})
}

// TestPlatformClock_TwoNodes_SettingPropagates: a change through node A
// reaches node B within gossip latency, and a node that joins afterwards
// starts on the cluster's setting.
func TestPlatformClock_TwoNodes_SettingPropagates(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	s := newSchedDB(t)
	seed := newClockSeed(t)
	sfx := randSuffix(t)[:8]
	a := clockClusterNode(t, s, key, "clock-a-"+sfx, seed)
	b := clockClusterNode(t, s, key, "clock-b-"+sfx, seed)

	// onSetting reports whether a node's clock is on the setting: at
	// clockStart plus at most the real time since the change began.
	posted := time.Now() // realtime: bounds how far the clock can have run since the change
	startClockAt(t, a, clockStart)
	onSetting := func(c clockBody) bool {
		ran := time.Since(posted) // realtime: how far the clock can have run since the change
		return !c.Now.Before(clockStart) && !c.Now.After(clockStart.Add(ran+time.Second))
	}

	deadline := time.Now().Add(2 * time.Second) // realtime: the propagation bound
	for {
		c := getClock(t, b)
		if onSetting(c) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("node B's clock %s 2s after the change on node A; want about %s", c.Now, clockStart)
		}
		time.Sleep(20 * time.Millisecond)
	}

	c := clockClusterNode(t, s, key, "clock-c-"+sfx, seed)
	if got := getClock(t, c); !onSetting(got) {
		t.Fatalf("node C's first clock read %s; want about %s", got.Now, clockStart)
	}
}

// platformCtx carries the platform clock's own principal, as the startup
// check reads the store.
func platformCtx() context.Context {
	return spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID: "platform-clock", UserName: "Platform clock", Kind: spi.PrincipalSystem,
		Tenant: spi.Tenant{ID: auth.PlatformTenantID, Name: "Platform"},
	})
}

// TestPlatformClock_StartupCheck_FunctionOnRealStore: the startup check over
// a real store fails when the store holds times later than real time, and
// passes when they lie in the past. The process exit on failure is waived
// (spec coverage matrix): e2e runs the app in-process.
func TestPlatformClock_StartupCheck_FunctionOnRealStore(t *testing.T) {
	check := func(h *callbackHarness) error {
		tm := h.app.TransactionManager()
		return platformclock.StartupCheck(platformCtx(), func(ctx context.Context) (time.Time, error) {
			return tm.LatestRecordedTime(ctx)
		}, time.Now()) // realtime: the check compares the store with real time
	}

	t.Run("ahead", func(t *testing.T) {
		h, _ := clockTestStack(t, nil)
		advanceClock(t, h, 24*time.Hour)
		model := uniq("clk-ahead")
		h.SetupModelWithWorkflow(t, model, secondaryWorkflow)
		createAs(t, h, h.token(t), model)
		err := check(h)
		if err == nil || !strings.Contains(err.Error(), "start over with a fresh store") || !strings.Contains(err.Error(), "real time") {
			t.Fatalf("startup check over a store one day ahead: %v; want the start-over error naming real time", err)
		}
	})
	t.Run("past", func(t *testing.T) {
		h, _ := clockTestStack(t, nil)
		startClockAt(t, h, clockStart)
		model := uniq("clk-past")
		h.SetupModelWithWorkflow(t, model, secondaryWorkflow)
		createAs(t, h, h.token(t), model)
		if err := check(h); err != nil {
			t.Fatalf("startup check over a store in the past: %v", err)
		}
	})
}
