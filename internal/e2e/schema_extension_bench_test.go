package e2e_test

// schema_extension_bench_test.go — throughput of concurrent PUTs on entities
// of ONE model, when the data fits the schema and when it extends it. Runs
// only with -bench; a plain `go test` skips it.
//
//	go test -run '^$' -bench BenchmarkConcurrentPUT_OneModel -benchtime=10s ./internal/e2e/
//
// One benchmark iteration is a round: 32 goroutines each PUT once on an
// entity of their own. Answers other than 2xx are counted and reported as the
// custom metric "errors/put" rather than failing the benchmark, so a build
// that refuses concurrent extensions still yields a measurement.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
)

const benchWriters = 32

func BenchmarkConcurrentPUT_OneModel(b *testing.B) {
	// field returns the extra field a PUT of the given round and writer adds,
	// or "" when the data fits the schema.
	cases := []struct {
		name  string
		field func(round, writer int) string
	}{
		{"fits", func(int, int) string { return "" }},
		{"distinctNewField", func(round, w int) string { return fmt.Sprintf("d_%d_%d", round, w) }},
		{"sameNewField", func(round, _ int) string { return fmt.Sprintf("s_%d", round) }},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) { benchConcurrentPUT(b, c.field) })
	}
}

func benchConcurrentPUT(b *testing.B, field func(round, writer int) string) {
	ctx := context.Background()
	model := uniq("se-bench")
	benchMust(b, ctx, http.MethodPost,
		fmt.Sprintf("/api/model/import/JSON/SAMPLE_DATA/%s/1", model), workflowSampleModel)
	benchMust(b, ctx, http.MethodPut, fmt.Sprintf("/api/model/%s/1/lock", model), "")
	benchMust(b, ctx, http.MethodPost, fmt.Sprintf("/api/model/%s/1/changeLevel/STRUCTURAL", model), "")

	ids := make([]string, benchWriters)
	for i := range ids {
		body := benchMust(b, ctx, http.MethodPost, fmt.Sprintf("/api/entity/JSON/%s/1", model),
			fmt.Sprintf(`{"name":"w%d","amount":0,"status":"new"}`, i))
		var res []struct {
			EntityIDs []string `json:"entityIds"`
		}
		if err := json.Unmarshal([]byte(body), &res); err != nil || len(res) == 0 || len(res[0].EntityIDs) == 0 {
			b.Fatalf("create entity %d: unexpected answer %q (%v)", i, body, err)
		}
		ids[i] = res[0].EntityIDs[0]
	}

	var failed, total atomic.Int64
	b.ResetTimer()
	for round := 1; round <= b.N; round++ {
		var wg sync.WaitGroup
		for w := range benchWriters {
			wg.Add(1)
			go func() {
				defer wg.Done()
				extra := ""
				if f := field(round, w); f != "" {
					extra = fmt.Sprintf(`,%q:"v"`, f)
				}
				payload := fmt.Sprintf(`{"name":"w%d","amount":%d,"status":"new"%s}`, w, round, extra)
				res := resultOf(doAuthOnceRaw(ctx, http.MethodPut, "/api/entity/JSON/"+ids[w], payload))
				total.Add(1)
				if res.err != nil || res.status < 200 || res.status > 299 {
					failed.Add(1)
				}
			}()
		}
		wg.Wait()
	}
	b.StopTimer()
	b.ReportMetric(float64(failed.Load())/float64(max(total.Load(), 1)), "errors/put")
}

// benchMust sends one setup request and fails the benchmark unless it
// answers 200. It runs on the benchmark goroutine only.
func benchMust(b *testing.B, ctx context.Context, method, path, body string) string {
	b.Helper()
	resp, err := doAuthOnceRaw(ctx, method, path, body)
	if err != nil {
		b.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		b.Fatalf("%s %s: status %d: %s", method, path, resp.StatusCode, raw)
	}
	return string(raw)
}
