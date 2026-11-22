package e2e_test

// schema_extension_bench_test.go — burst latency and per-successful-write cost
// of concurrent PUTs on entities of ONE model, when the data fits the schema
// and when it extends it. Runs only with -bench; a plain `go test` skips it.
//
//	go test -run '^$' -bench BenchmarkConcurrentPUT_OneModel -benchtime=10s ./internal/e2e/
//
// A benchmark iteration is a ROUND: 32 goroutines each PUT once on an entity of
// their own, and the round ends when all 32 have answered (a barrier per
// round). A round therefore measures BURST latency, not sustained throughput:
// the slowest writer ends it.
//
// The extension cost grows with the model's field count, so a model is never
// shared across the whole b.N: rounds run in windows of at most benchWindow
// rounds, each on a fresh model whose field count is fixed at K before the
// timer starts (the sample imported has K extra fields; no setup is timed).
// Per-round cost is thus independent of b.N. Within a window the model still
// gains up to benchWindow*32 fields in the distinct case.
//
// Writes answered other than 2xx are counted by class and reported, never
// fatal, so a build that refuses concurrent extensions still yields a
// measurement. Metrics are per benchmark invocation (the counters are local to
// it), so the figures `go test` prints are those of the final invocation, the
// one with the largest b.N. ns/success is timed-region nanoseconds divided by
// successful PUTs; compare it, not ns/op, between builds that refuse
// different numbers of writes.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	benchWriters = 32
	benchWindow  = 5
)

// benchClient keeps one idle connection per writer so rounds do not pay for
// connection churn.
var benchClient = &http.Client{Transport: &http.Transport{
	MaxIdleConns:        benchWriters * 2,
	MaxIdleConnsPerHost: benchWriters,
	IdleConnTimeout:     90 * time.Second,
}}

// benchDo sends one request on benchClient and returns the status and body;
// status is -1 on a transport error.
func benchDo(ctx context.Context, method, path, body string) (int, string) {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := authRequestRaw(ctx, method, path, r)
	if err != nil {
		return -1, err.Error()
	}
	resp, err := benchClient.Do(req)
	if err != nil {
		return -1, err.Error()
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return -1, err.Error()
	}
	return resp.StatusCode, string(raw)
}

// benchTally counts PUT answers by class.
type benchTally struct {
	mu                                      sync.Mutex
	puts, ok, c409, other4xx, s5xx, transpt int64
}

func (t *benchTally) add(status int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.puts++
	switch {
	case status < 0:
		t.transpt++
	case status >= 200 && status < 300:
		t.ok++
	case status == http.StatusConflict:
		t.c409++
	case status >= 400 && status < 500:
		t.other4xx++
	default:
		t.s5xx++
	}
}

func BenchmarkConcurrentPUT_OneModel(b *testing.B) {
	// field returns the extra field a PUT of the given round and writer adds.
	distinct := func(round, w int) string { return fmt.Sprintf("d_%d_%d", round, w) }
	same := func(round, _ int) string { return fmt.Sprintf("s_%d", round) }

	b.Run("fits", func(b *testing.B) { benchConcurrentPUT(b, 0, nil) })
	for _, k := range []int{0, 100, 1000} {
		b.Run(fmt.Sprintf("distinctNewField/K=%d", k), func(b *testing.B) { benchConcurrentPUT(b, k, distinct) })
	}
	for _, k := range []int{0, 100, 1000} {
		b.Run(fmt.Sprintf("sameNewField/K=%d", k), func(b *testing.B) { benchConcurrentPUT(b, k, same) })
	}
}

// benchSample is the sample a model is imported from: the three fields the
// entities carry plus k padding fields the entities leave out.
func benchSample(k int) string {
	var sb strings.Builder
	sb.WriteString(`{"name":"x","amount":1,"status":"s"`)
	for i := range k {
		fmt.Fprintf(&sb, `,"pad_%d":"v"`, i)
	}
	sb.WriteString("}")
	return sb.String()
}

func benchMust(b *testing.B, ctx context.Context, method, path, body string) string {
	b.Helper()
	status, out := benchDo(ctx, method, path, body)
	if status != http.StatusOK {
		b.Fatalf("%s %s: status %d: %s", method, path, status, out)
	}
	return out
}

// benchWindowSetup creates a fresh locked STRUCTURAL model of k+3 fields and
// its 32 entities, outside the timed region. The model is torn down by the
// returned func (best effort).
func benchWindowSetup(b *testing.B, ctx context.Context, k int) (ids []string, teardown func()) {
	b.Helper()
	model := uniq("se-bench")
	benchMust(b, ctx, http.MethodPost, fmt.Sprintf("/api/model/import/JSON/SAMPLE_DATA/%s/1", model), benchSample(k))
	benchMust(b, ctx, http.MethodPut, fmt.Sprintf("/api/model/%s/1/lock", model), "")
	benchMust(b, ctx, http.MethodPost, fmt.Sprintf("/api/model/%s/1/changeLevel/STRUCTURAL", model), "")
	ids = make([]string, benchWriters)
	for i := range ids {
		out := benchMust(b, ctx, http.MethodPost, fmt.Sprintf("/api/entity/JSON/%s/1", model),
			fmt.Sprintf(`{"name":"w%d","amount":0,"status":"new"}`, i))
		var res []struct {
			EntityIDs []string `json:"entityIds"`
		}
		if err := json.Unmarshal([]byte(out), &res); err != nil || len(res) == 0 || len(res[0].EntityIDs) == 0 {
			b.Fatalf("create entity %d: unexpected answer %q (%v)", i, out, err)
		}
		ids[i] = res[0].EntityIDs[0]
	}
	return ids, func() {
		benchDo(ctx, http.MethodDelete, fmt.Sprintf("/api/entity/%s/1", model), "")
		benchDo(ctx, http.MethodPut, fmt.Sprintf("/api/model/%s/1/unlock", model), "")
		benchDo(ctx, http.MethodDelete, fmt.Sprintf("/api/model/%s/1", model), "")
	}
}

func benchConcurrentPUT(b *testing.B, k int, field func(round, writer int) string) {
	ctx := context.Background()
	var tally benchTally
	var timed time.Duration
	for done := 0; done < b.N; {
		b.StopTimer()
		ids, teardown := benchWindowSetup(b, ctx, k)
		b.StartTimer()
		rounds := min(benchWindow, b.N-done)
		start := time.Now()
		for round := 1; round <= rounds; round++ {
			var wg sync.WaitGroup
			for w := range benchWriters {
				wg.Add(1)
				go func() {
					defer wg.Done()
					extra := ""
					if field != nil {
						extra = fmt.Sprintf(`,%q:"v"`, field(round, w))
					}
					payload := fmt.Sprintf(`{"name":"w%d","amount":%d,"status":"new"%s}`, w, round, extra)
					status, _ := benchDo(ctx, http.MethodPut, "/api/entity/JSON/"+ids[w], payload)
					tally.add(status)
				}()
			}
			wg.Wait()
		}
		timed += time.Since(start)
		done += rounds
		b.StopTimer()
		teardown()
	}
	puts := float64(max(tally.puts, 1))
	b.ReportMetric(float64(tally.ok)/float64(b.N), "successes/op")
	if tally.ok > 0 {
		b.ReportMetric(float64(timed.Nanoseconds())/float64(tally.ok), "ns/success")
	}
	b.ReportMetric(float64(tally.puts-tally.ok)/puts, "errors/put")
	b.ReportMetric(float64(tally.c409)/puts, "c409/put")
	b.ReportMetric(float64(tally.other4xx)/puts, "other4xx/put")
	b.ReportMetric(float64(tally.s5xx)/puts, "s5xx/put")
	b.ReportMetric(float64(tally.transpt)/puts, "transport/put")
}
