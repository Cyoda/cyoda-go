package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ccRun drives goroutines that must not touch *testing.T: they report a failure
// on a channel the test goroutine checks, and stop when the context-like stop
// channel closes. halt is idempotent and joins every goroutine, so a deferred
// halt covers every exit path.
type ccRun struct {
	h     *callbackHarness
	stop  chan struct{}
	wg    sync.WaitGroup
	errs  chan error
	once  sync.Once
	mu    sync.Mutex
	acked map[string]struct{}
}

func newCCRun(h *callbackHarness) *ccRun {
	return &ccRun{h: h, stop: make(chan struct{}), errs: make(chan error, 64), acked: map[string]struct{}{}}
}

func (r *ccRun) halt() {
	r.once.Do(func() { close(r.stop) })
	r.wg.Wait()
}

func (r *ccRun) stopped() bool {
	select {
	case <-r.stop:
		return true
	default:
		return false
	}
}

func (r *ccRun) fail(format string, args ...any) {
	select {
	case r.errs <- fmt.Errorf(format, args...):
	default:
	}
}

func (r *ccRun) go_(fn func()) {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		fn()
	}()
}

// snapshot is the set of ids whose create has been acknowledged so far.
func (r *ccRun) snapshot() map[string]struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]struct{}, len(r.acked))
	for id := range r.acked {
		out[id] = struct{}{}
	}
	return out
}

func (r *ccRun) writer(model string, n int, count *atomic.Int64) {
	r.go_(func() {
		for i := 0; !r.stopped(); i++ {
			res := r.h.CreateEntityRaw(model, 1, fmt.Sprintf(`{"name":"w%d-%d","amount":%d,"status":"new"}`, n, i, i))
			if res.err != nil || res.status != http.StatusOK {
				r.fail("create: status=%d err=%v body=%s", res.status, res.err, res.body)
				return
			}
			r.mu.Lock()
			r.acked[res.entityID] = struct{}{}
			r.mu.Unlock()
			count.Add(1)
		}
	})
}

func (r *ccRun) get(path string) (string, error) {
	res, err := r.h.callback(http.MethodGet, path, "", "")
	if err != nil {
		return "", err
	}
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %d %s", path, res.StatusCode, res.Body)
	}
	return res.Body, nil
}

func (r *ccRun) consistencyTime() (string, error) {
	body, err := r.get("/api/entity/consistency-time")
	if err != nil {
		return "", err
	}
	var dto struct {
		ConsistencyTime string `json:"consistencyTime"`
	}
	if err := json.Unmarshal([]byte(body), &dto); err != nil || dto.ConsistencyTime == "" {
		return "", fmt.Errorf("consistency-time body %q: %v", body, err)
	}
	return dto.ConsistencyTime, nil
}

// listAt lists the model at c, page by page, in the order the server returns
// them.
func (r *ccRun) listAt(model, c string, pageSize int) ([]string, error) {
	var ids []string
	for page := 0; ; page++ {
		body, err := r.get(fmt.Sprintf("/api/entity/%s/1?pointInTime=%s&pageSize=%d&pageNumber=%d",
			model, url.QueryEscape(c), pageSize, page))
		if err != nil {
			return nil, err
		}
		var entities []struct {
			Meta struct {
				ID string `json:"id"`
			} `json:"meta"`
		}
		if err := json.Unmarshal([]byte(body), &entities); err != nil {
			return nil, fmt.Errorf("decode list page %d: %w", page, err)
		}
		for _, e := range entities {
			ids = append(ids, e.Meta.ID)
		}
		if len(entities) < pageSize {
			return ids, nil
		}
	}
}

// missing returns the acknowledged ids absent from got.
func missing(acked map[string]struct{}, got []string) []string {
	have := make(map[string]struct{}, len(got))
	for _, id := range got {
		have[id] = struct{}{}
	}
	var out []string
	for id := range acked {
		if _, ok := have[id]; !ok {
			out = append(out, id)
		}
	}
	return out
}

// asyncResultIDs submits a match-all async search with no pointInTime, waits
// for it, and returns every result id.
func (r *ccRun) asyncResultIDs(model string) ([]string, error) {
	res, err := r.h.callback(http.MethodPost, "/api/search/async/"+model+"/1", `{"type":"group","operator":"AND","conditions":[]}`, "")
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("async submit: %d %s", res.StatusCode, res.Body)
	}
	jobID := strings.Trim(strings.TrimSpace(res.Body), `"`)
	deadline := time.Now().Add(60 * time.Second)
	for {
		body, err := r.get("/api/search/async/" + jobID + "/status")
		if err != nil {
			return nil, err
		}
		var st struct {
			Status string `json:"searchJobStatus"`
		}
		if err := json.Unmarshal([]byte(body), &st); err != nil {
			return nil, fmt.Errorf("decode status: %w", err)
		}
		if st.Status == "SUCCESSFUL" {
			break
		}
		if st.Status != "RUNNING" || time.Now().After(deadline) {
			return nil, fmt.Errorf("async job %s status %q", jobID, st.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
	var ids []string
	const pageSize = 1000
	for page := 0; ; page++ {
		body, err := r.get(fmt.Sprintf("/api/search/async/%s?pageSize=%d&pageNumber=%d", jobID, pageSize, page))
		if err != nil {
			return nil, err
		}
		var parsed struct {
			Content []struct {
				Meta struct {
					ID string `json:"id"`
				} `json:"meta"`
			} `json:"content"`
			Page struct {
				TotalPages int `json:"totalPages"`
			} `json:"page"`
		}
		if err := json.Unmarshal([]byte(body), &parsed); err != nil {
			return nil, fmt.Errorf("decode results page %d: %w", page, err)
		}
		for _, e := range parsed.Content {
			ids = append(ids, e.Meta.ID)
		}
		if len(parsed.Content) == 0 || page+1 >= parsed.Page.TotalPages {
			return ids, nil
		}
	}
}

func (r *ccRun) checkErrs(t *testing.T) {
	t.Helper()
	for {
		select {
		case err := <-r.errs:
			t.Errorf("%v", err)
		default:
			return
		}
	}
}

// TestConsistency_ConcurrentWritersAndReaders: while 8 writers create entities,
// every consistency time C a reader takes includes every create acknowledged
// before C was requested, and two lists at the same C are identical. An async
// search submitted without a pointInTime includes every create acknowledged
// before the submit.
func TestConsistency_ConcurrentWritersAndReaders(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	const model = "e2e-cc-writers"
	s := newSchedDB(t)
	h := newStackOn(t, s, nil)
	h.SetupModelWithWorkflow(t, model, secondaryWorkflow)
	_ = h.token(t) // seeds the bearer the goroutine-safe helpers read

	r := newCCRun(h)
	defer r.halt()

	var created, listChecks, asyncChecks atomic.Int64
	for n := 0; n < 8; n++ {
		r.writer(model, n, &created)
	}
	for n := 0; n < 2; n++ {
		r.go_(func() {
			for !r.stopped() {
				acked := r.snapshot()
				c, err := r.consistencyTime()
				if err != nil {
					r.fail("consistency time: %v", err)
					return
				}
				first, err := r.listAt(model, c, 1000)
				if err != nil {
					r.fail("list at %s: %v", c, err)
					return
				}
				time.Sleep(30 * time.Millisecond)
				second, err := r.listAt(model, c, 1000)
				if err != nil {
					r.fail("second list at %s: %v", c, err)
					return
				}
				if !slices.Equal(first, second) {
					r.fail("two lists at %s differ: %d vs %d entities", c, len(first), len(second))
					return
				}
				if m := missing(acked, first); len(m) > 0 {
					r.fail("list at %s misses %d create(s) acknowledged before it was requested, e.g. %s", c, len(m), m[0])
					return
				}
				listChecks.Add(1)
			}
		})
	}
	r.go_(func() {
		for !r.stopped() {
			acked := r.snapshot()
			ids, err := r.asyncResultIDs(model)
			if err != nil {
				r.fail("async search: %v", err)
				return
			}
			if m := missing(acked, ids); len(m) > 0 {
				r.fail("async search without pointInTime misses %d create(s) acknowledged before the submit, e.g. %s", len(m), m[0])
				return
			}
			asyncChecks.Add(1)
		}
	})

	deadline := time.After(10 * time.Second)
wait:
	for {
		select {
		case <-deadline:
			break wait
		case err := <-r.errs:
			t.Errorf("%v", err)
			break wait
		case <-time.After(100 * time.Millisecond):
		}
	}
	r.halt()
	r.checkErrs(t)

	if created.Load() == 0 || listChecks.Load() == 0 || asyncChecks.Load() == 0 {
		t.Fatalf("vacuous run: creates=%d list checks=%d async checks=%d", created.Load(), listChecks.Load(), asyncChecks.Load())
	}
	t.Logf("creates=%d list checks=%d async checks=%d", created.Load(), listChecks.Load(), asyncChecks.Load())
}

// TestConsistency_PagingAtCIsStable: pages taken one after another at one C,
// while writers keep creating, add up to exactly one list at that C.
func TestConsistency_PagingAtCIsStable(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	const model = "e2e-cc-paging"
	s := newSchedDB(t)
	h := newStackOn(t, s, nil)
	h.SetupModelWithWorkflow(t, model, secondaryWorkflow)
	_ = h.token(t)

	r := newCCRun(h)
	defer r.halt()

	var created atomic.Int64
	for i := 0; i < 60; i++ {
		if _, status, body := h.CreateEntity(t, model, 1, fmt.Sprintf(`{"name":"seed%d","amount":%d,"status":"new"}`, i, i)); status != http.StatusOK {
			t.Fatalf("seed create: %d %s", status, body)
		}
	}
	for n := 0; n < 4; n++ {
		r.writer(model, n, &created)
	}

	for round := 0; round < 6; round++ {
		c, err := r.consistencyTime()
		if err != nil {
			t.Fatalf("consistency time: %v", err)
		}
		// Pages of 10, with writes landing between the page requests.
		var paged []string
		for page := 0; ; page++ {
			body, err := r.get(fmt.Sprintf("/api/entity/%s/1?pointInTime=%s&pageSize=10&pageNumber=%d", model, url.QueryEscape(c), page))
			if err != nil {
				t.Fatalf("page %d at %s: %v", page, c, err)
			}
			var entities []struct {
				Meta struct {
					ID string `json:"id"`
				} `json:"meta"`
			}
			if err := json.Unmarshal([]byte(body), &entities); err != nil {
				t.Fatalf("decode page %d: %v", page, err)
			}
			for _, e := range entities {
				paged = append(paged, e.Meta.ID)
			}
			if len(entities) < 10 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		whole, err := r.listAt(model, c, 1000)
		if err != nil {
			t.Fatalf("list at %s: %v", c, err)
		}
		if len(paged) < 60 {
			t.Fatalf("round %d: paged %d entities at %s, want at least the 60 seeded", round, len(paged), c)
		}
		if !slices.Equal(paged, whole) {
			t.Fatalf("round %d: union of pages (%d) differs from one list (%d) at %s", round, len(paged), len(whole), c)
		}
		select {
		case err := <-r.errs:
			t.Fatalf("%v", err)
		default:
		}
	}
	r.halt()
	r.checkErrs(t)
	if created.Load() == 0 {
		t.Fatalf("vacuous run: no create happened while paging")
	}
}
