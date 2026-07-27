package e2e_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// search_bounded_test.go — direct (synchronous) search is BOUNDED-OR-FAIL over
// the full HTTP stack against real Postgres.
//
// `limit` caps the MATCHED SET; it does not page it. A matched set larger than
// the effective limit is a 400 SEARCH_RESULT_LIMIT, never a silently truncated
// top-N page. Exactly-at-limit is a plain 200. A limit below 1 is rejected
// 400 BAD_REQUEST, because a non-positive limit means UNBOUNDED at the storage
// layer and would hand the caller a synchronous search past the very cap this
// endpoint exists to enforce.
//
// Each case seeds just OVER a small explicit limit rather than over the 1000
// default: the default itself is proven in the cross-backend parity suite, and
// seeding 1001 entities here would only make this suite slow.
//
// The in-transaction overlay is covered too — reachable only through a joined
// compute-node callback, see search_intx_test.go's header for why.

// searchDirectRaw issues POST /api/search/direct/{model}/{version} with rawQuery
// appended verbatim (e.g. "?limit=2") and returns the status and the raw body —
// ndjson on success, an RFC 9457 problem document on error. directSearch
// (search_test.go) discards the body on a non-200, which is exactly what these
// tests must inspect.
func searchDirectRaw(t *testing.T, model string, modelVersion int, condition, rawQuery string) (int, string) {
	t.Helper()
	path := fmt.Sprintf("/api/search/direct/%s/%d%s", model, modelVersion, rawQuery)
	resp := doAuth(t, http.MethodPost, path, condition)
	return resp.StatusCode, readBody(t, resp)
}

// countNDJSONLines counts the non-blank lines of an application/x-ndjson body.
func countNDJSONLines(body string) int {
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

// seedMatching creates n committed entities that all carry marker in $.status,
// so simpleStatusCond(marker) (search_intx_test.go) matches exactly those n.
func seedMatching(t *testing.T, model string, n int, marker string) {
	t.Helper()
	for i := 0; i < n; i++ {
		createEntityE2E(t, model, 1, fmt.Sprintf(`{"name":"bounded-%d","amount":%d,"status":%q}`, i, i, marker))
	}
}

// TestSearchDirect_OverLimit_Returns400 — 3 matches against limit=2 must fail
// the whole request, not return the first 2. This is the behaviour change: the
// pre-bounded implementation answered 200 with a truncated page.
func TestSearchDirect_OverLimit_Returns400(t *testing.T) {
	const model = "e2e-search-bounded-over"
	const marker = "bounded-over"
	setupSearchModel(t, model)
	seedMatching(t, model, 3, marker)

	status, body := searchDirectRaw(t, model, 1, simpleStatusCond(marker), "?limit=2")
	if status != http.StatusBadRequest {
		t.Fatalf("3 matches over limit 2: got status %d, want 400; body=%s", status, body)
	}
	if code := problemErrorCode(body); code != "SEARCH_RESULT_LIMIT" {
		t.Fatalf("got errorCode %q, want SEARCH_RESULT_LIMIT; body=%s", code, body)
	}
}

// TestSearchDirect_AtLimit_Returns200 pins the ACCEPTED side of the boundary:
// exactly-at-limit is a full 200 carrying every match. Without this the
// over-limit test alone would be satisfied by a bound that is simply off by one.
func TestSearchDirect_AtLimit_Returns200(t *testing.T) {
	const model = "e2e-search-bounded-at"
	const marker = "bounded-at"
	setupSearchModel(t, model)
	seedMatching(t, model, 2, marker)

	status, body := searchDirectRaw(t, model, 1, simpleStatusCond(marker), "?limit=2")
	if status != http.StatusOK {
		t.Fatalf("2 matches at limit 2: got status %d, want 200; body=%s", status, body)
	}
	if n := countNDJSONLines(body); n != 2 {
		t.Fatalf("got %d ndjson lines, want 2; body=%s", n, body)
	}
}

// TestSearchDirect_LimitZero_Returns400 — limit=0 is rejected up front. At the
// storage layer a non-positive limit means UNBOUNDED, so accepting it would let
// a client bypass the cap entirely rather than ask for an empty page.
func TestSearchDirect_LimitZero_Returns400(t *testing.T) {
	const model = "e2e-search-bounded-zero"
	const marker = "bounded-zero"
	setupSearchModel(t, model)
	seedMatching(t, model, 1, marker)

	status, body := searchDirectRaw(t, model, 1, simpleStatusCond(marker), "?limit=0")
	if status != http.StatusBadRequest {
		t.Fatalf("limit=0: got status %d, want 400; body=%s", status, body)
	}
	if code := problemErrorCode(body); code != "BAD_REQUEST" {
		t.Fatalf("got errorCode %q, want BAD_REQUEST; body=%s", code, body)
	}
}

// searchHTTPLimited is searchHTTP's (search_intx_test.go) limit-carrying
// sibling: it issues the same POST /api/search/direct/{model}/{version} from
// inside a processor callback but with an explicit ?limit=N, so the bound can be
// exercised against the in-transaction overlay. When join is true the tx-token
// is echoed as X-Tx-Token and the search runs inside T.
func (rc *reqCtx) searchHTTPLimited(model string, version, limit int, condition string, join bool) (callbackResult, error) {
	path := fmt.Sprintf("/api/search/direct/%s/%d?limit=%d", model, version, limit)
	tok := ""
	if join {
		tok = rc.token
	}
	return rc.h.callback(http.MethodPost, path, condition, tok)
}

// TestSearchDirect_InTx_OverLimit_Returns400 proves the bound also counts the
// transaction's OWN uncommitted writes. Two matches are committed up front —
// under a limit of 2 that set is exactly at the bound. Then, from inside a live
// transaction T, a SYNC processor saves a third match and searches with
// limit=2: the joined search sees 3 survivors (read-your-own-writes) and must
// fail 400 SEARCH_RESULT_LIMIT, even though nothing outside T is over the bound.
//
// The unjoined control in the same callback is the test's teeth: identical
// condition, identical limit, no tx-token → 200 with the 2 committed rows. The
// only difference between the two is the overlay, so a 400 on the joined search
// can only come from the uncommitted third row being counted against the bound.
//
// The processor then succeeds, so T must still commit — a 4xx from an in-tx
// search must not poison the transaction (same invariant as
// TestIntxSearch_InTx_ErrorCodes).
func TestSearchDirect_InTx_OverLimit_Returns400(t *testing.T) {
	h := newCallbackHarness(t)

	const primary = "intx-bounded-primary"
	const secondary = "intx-bounded-secondary"
	const marker = "intx-bounded-marker"
	h.SetupModelWithWorkflow(t, secondary, secondaryWorkflow)

	// Two COMMITTED matches — exactly at the limit of 2 on their own.
	for i := 0; i < 2; i++ {
		_, status, body := h.CreateEntity(t, secondary, 1, fmt.Sprintf(`{"name":"committed-%d","amount":%d,"status":%q}`, i, i, marker))
		if status != http.StatusOK {
			t.Fatalf("seed committed secondary %d: status=%d body=%s", i, status, body)
		}
	}

	h.RegisterProc("cb-intx-bounded-search", func(rc *reqCtx) (map[string]any, error) {
		// The transaction's own (uncommitted) third match.
		created, err := rc.CreateEntity(secondary, 1, fmt.Sprintf(`{"name":"uncommitted","amount":99,"status":%q}`, marker))
		if err != nil {
			return nil, fmt.Errorf("callback create failed: %w", err)
		}
		if created.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("callback create status=%d body=%s", created.StatusCode, created.Body)
		}

		cond := simpleStatusCond(marker)

		// Joined: 3 survivors inside T against a bound of 2 → must be 400.
		joined, err := rc.searchHTTPLimited(secondary, 1, 2, cond, true /*join*/)
		if err != nil {
			return nil, fmt.Errorf("joined in-tx search: %w", err)
		}
		// Control: outside T only the 2 committed rows survive → 200.
		standalone, err := rc.searchHTTPLimited(secondary, 1, 2, cond, false)
		if err != nil {
			return nil, fmt.Errorf("standalone search: %w", err)
		}

		out := cloneData(rc.entityData)
		out["joinedStatus"] = float64(joined.StatusCode)
		out["joinedCode"] = problemErrorCode(joined.Body)
		out["standaloneStatus"] = float64(standalone.StatusCode)
		out["standaloneCount"] = float64(ndjsonStatusMatches(standalone.Body, marker))
		return out, nil
	})

	h.SetupModelWithWorkflow(t, primary, intxSearchPrimaryWF("primary-intx-bounded-wf", "cb-intx-bounded-search"))

	primaryID, status, body := h.CreateEntity(t, primary, 1, `{"name":"parent","amount":100,"status":"new"}`)
	if status != http.StatusOK {
		t.Fatalf("primary create: status=%d body=%s", status, body)
	}

	data := h.GetEntityData(t, primaryID)

	if got, _ := data["joinedStatus"].(float64); int(got) != http.StatusBadRequest {
		t.Errorf("joined in-tx search (3 survivors, limit 2): status=%v; want 400 (T's own uncommitted write must count against the bound)", data["joinedStatus"])
	}
	if got, _ := data["joinedCode"].(string); got != "SEARCH_RESULT_LIMIT" {
		t.Errorf("joined in-tx search: errorCode=%q; want SEARCH_RESULT_LIMIT", got)
	}

	// Control: the same request without the token is at the bound, not over it.
	if got, _ := data["standaloneStatus"].(float64); int(got) != http.StatusOK {
		t.Errorf("standalone search (2 committed, limit 2): status=%v; want 200", data["standaloneStatus"])
	}
	if c, _ := data["standaloneCount"].(float64); c != 2 {
		t.Errorf("standalone search matched %v rows; want 2 (only the committed matches are visible outside T)", data["standaloneCount"])
	}

	// A 4xx from an in-tx search must not poison T.
	if st, code := h.GetEntityState(t, primaryID); code != http.StatusOK || st != "ACTIVE" {
		t.Fatalf("primary state=%q http=%d; want ACTIVE/200 (a 400 in-tx search must not poison T)", st, code)
	}
}
