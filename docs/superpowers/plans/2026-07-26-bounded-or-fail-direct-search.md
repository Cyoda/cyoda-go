# Bounded-or-Fail Direct Search Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make direct (synchronous) search bounded-or-fail on every backend — a matched set larger than the effective limit returns `400 SEARCH_RESULT_LIMIT` instead of a silently truncated top-N page.

**Architecture:** The contract is stated in `cyoda-go-spi` and enforced twice: by each OSS `spi.Searcher` (so a backend aborts as early as its storage allows and conforms when driven directly) and by the engine's non-`Searcher` fallback branch (so no direct-search route can truncate). `Offset` is removed from the search path entirely. Two transport-level lower-bound holes that would otherwise bypass the cap are closed, and the conditional-delete path is fixed to forward classified 4xx errors instead of burying them as 500s.

**Tech Stack:** Go 1.26, `cyoda-go-spi` (sibling module), plugins memory/sqlite/postgres (separate go modules), `spitest` conformance harness, testcontainers-go for e2e/parity.

**Spec:** `docs/superpowers/specs/2026-07-26-bounded-or-fail-direct-search-design.md`

## Global Constraints

- **TDD is mandatory.** Every implementation step is preceded by a failing test. No production code without a red test driving it.
- **`Limit > 0` is a bounded-or-fail cap.** A `Searcher` returns `spi.ErrSearchResultLimitExceeded` when the matched count exceeds it. It never returns a truncated prefix. Exactly-at-limit succeeds.
- **`Limit <= 0` means unbounded.** A plugin must never substitute a default of its own. The engine resolves the direct-search default (1000) at both transports before any plugin is called.
- **No issue IDs in shipped artefacts.** Never put `#NNN` in error messages, log output, response bodies, code comments, OpenAPI descriptions, or help-topic content. Issue IDs belong in commit messages, PR bodies, and spec/plan docs only.
- **SPI pin sync.** `github.com/cyoda-platform/cyoda-go-spi` must be pinned to the identical version in `go.mod`, `plugins/memory/go.mod`, `plugins/sqlite/go.mod`, `plugins/postgres/go.mod`. `make check-spi-pin-sync` enforces it.
- **`go.work` is tracked.** The local SPI `use` line added in Task 1 must never be committed. Never run `git add -A`; stage files explicitly.
- **Race detector is end-of-deliverable.** Do not run `-race` between tasks. `make race` runs once, in Task 13.
- **`log/slog` only.** Never `log.Printf` or `fmt.Printf`.
- Error wrapping uses `fmt.Errorf("...: %w", err)`.

## Repositories

Two repos are edited. The SPI is a sibling checkout, not a submodule.

| Repo | Path | Branch to create |
| --- | --- | --- |
| `cyoda-go-spi` | `/Users/paul/go-projects/cyoda-light/cyoda-go-spi` | `feat/437-bounded-or-fail-search` off the current `feat/431-cloud-aligned-search` HEAD (`678c953`) |
| `cyoda-go` | the current worktree | already on `worktree-feat-437-bounded-or-fail-search` |

**SPI version note for the release maintainer (not this plan's work):** `MAINTAINING.md` rules that `cyoda-go-spi` takes a **minor** bump for a breaking interface change and a patch for additive surface. Removing `SearchOptions.Offset` and renaming `MergePage` is breaking, so the milestone-end SPI tag is `v0.9.0`, not `v0.8.3`. Flag this to the maintainer at PR time; the tag is not cut by this plan.

## File Structure

**`cyoda-go-spi`**

- `merge_bounded.go` (renamed from `merge_page.go`) — the shared merge kernel; drops `offset`, gains the bound.
- `merge_bounded_test.go` (renamed from `merge_page_test.go`).
- `searcher.go` — `Searcher` interface doc, `SearchOptions` (loses `Offset`).
- `spitest/searcher.go` (new) — the `Searcher` conformance suite.
- `spitest/spitest.go` — registers the new suite.

**`cyoda-go`**

- `plugins/memory/searcher.go` — `matchSortPage` → `matchSortBounded`; `MergePage` → `MergeBounded`.
- `plugins/sqlite/searcher.go` — committed pushdown + residual + tx overlay.
- `plugins/postgres/searcher.go` — committed pushdown + residual.
- `internal/domain/search/service.go` — `SearchOptions.Offset` removed; fallback branch bounded; persisted job-opts drop `"offset"`.
- `internal/domain/search/handler.go` — reject `limit < 1`.
- `internal/grpc/search.go` — reject `limit < 1`.
- `internal/domain/entity/service.go` — forward classified 4xx from the delete-selection search.
- `internal/e2e/search_bounded_test.go` (new) — full-stack HTTP + in-tx coverage on real postgres.
- `e2e/parity/search.go`, `e2e/parity/registry.go`, `e2e/parity/client/http.go` — the rewritten parity scenario and the limit-bearing raw client helper.
- Docs: `cmd/cyoda/help/content/search.md`, `cmd/cyoda/help/content/errors/SEARCH_RESULT_LIMIT.md`, `api/openapi.yaml`, `CHANGELOG.md`, `COMPATIBILITY.md`, `docs/cloud-parity/direct-search-bounded-or-fail.md`.
- Pins: `go.mod`, `plugins/*/go.mod`.

---

### Task 1: SPI — `MergePage` becomes `MergeBounded`

Sets up local SPI composition (needed by every later task) and converts the shared merge kernel.

**Files:**
- Create: `/Users/paul/go-projects/cyoda-light/cyoda-go-spi/merge_bounded.go` (git-mv from `merge_page.go`)
- Create: `/Users/paul/go-projects/cyoda-light/cyoda-go-spi/merge_bounded_test.go` (git-mv from `merge_page_test.go`)
- Modify: the current worktree's `go.work` (local only, never committed)

**Interfaces:**
- Consumes: nothing.
- Produces: `spi.MergeBounded(next func() (*Entity, bool, error), adds []*Entity, deleted func(id string) bool, specs []OrderSpec, limit int) ([]*Entity, error)`. Returns `ErrSearchResultLimitExceeded` when `limit > 0` and total survivors exceed `limit`; drains unbounded when `limit <= 0`.

- [ ] **Step 1: Create the SPI branch and wire local composition**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git checkout -b feat/437-bounded-or-fail-search
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/feat-437-bounded-or-fail-search
go work edit -use /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git update-index --skip-worktree go.work
```

The `skip-worktree` bit keeps the absolute path out of every `git status` and off any commit. Undone in Task 13.

- [ ] **Step 2: Rename the files, keeping history**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git mv merge_page.go merge_bounded.go
git mv merge_page_test.go merge_bounded_test.go
```

- [ ] **Step 3: Write the failing tests**

Append to `merge_bounded_test.go`. `newEnt` / the existing helpers in that file already build `*spi.Entity` values; reuse whatever the file's existing tests use for construction rather than inventing a new helper.

```go
// sliceSource returns a MergeBounded `next` that yields the given entities.
func sliceSource(es []*Entity) func() (*Entity, bool, error) {
	i := 0
	return func() (*Entity, bool, error) {
		if i >= len(es) {
			return nil, false, nil
		}
		e := es[i]
		i++
		return e, true, nil
	}
}

func TestMergeBounded_OverLimitRaises(t *testing.T) {
	committed := []*Entity{ent("a"), ent("b"), ent("c")}
	_, err := MergeBounded(sliceSource(committed), nil, nil, nil, 2)
	if !errors.Is(err, ErrSearchResultLimitExceeded) {
		t.Fatalf("3 survivors over limit 2: got err %v, want ErrSearchResultLimitExceeded", err)
	}
}

func TestMergeBounded_ExactlyAtLimitSucceeds(t *testing.T) {
	committed := []*Entity{ent("a"), ent("b")}
	got, err := MergeBounded(sliceSource(committed), nil, nil, nil, 2)
	if err != nil {
		t.Fatalf("2 survivors at limit 2: unexpected err %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entities, want 2", len(got))
	}
}

func TestMergeBounded_UnboundedDrains(t *testing.T) {
	committed := []*Entity{ent("a"), ent("b"), ent("c")}
	got, err := MergeBounded(sliceSource(committed), nil, nil, nil, 0)
	if err != nil {
		t.Fatalf("limit 0 must be unbounded: unexpected err %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d entities, want 3", len(got))
	}
}

// The bound gates on TOTAL survivors, so adds alone can exceed it even when
// the committed stream is empty.
func TestMergeBounded_AddsAloneExceedLimit(t *testing.T) {
	adds := []*Entity{ent("a"), ent("b"), ent("c")}
	_, err := MergeBounded(sliceSource(nil), adds, nil, nil, 2)
	if !errors.Is(err, ErrSearchResultLimitExceeded) {
		t.Fatalf("3 adds over limit 2: got err %v, want ErrSearchResultLimitExceeded", err)
	}
}

// A deleted committed row is not a survivor and must not count toward the bound.
func TestMergeBounded_DeletedRowsDoNotCount(t *testing.T) {
	committed := []*Entity{ent("a"), ent("b"), ent("c")}
	deleted := func(id string) bool { return id == "b" }
	got, err := MergeBounded(sliceSource(committed), nil, deleted, nil, 2)
	if err != nil {
		t.Fatalf("2 survivors after suppression at limit 2: unexpected err %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entities, want 2", len(got))
	}
}
```

Delete every existing `MergePage` test that asserts offset behaviour or top-N truncation (`merge_bounded_test.go` lines around 37, 54, 75, 126 in the pre-rename file) and rename the survivors' calls to `MergeBounded` with the `offset` argument dropped.

- [ ] **Step 4: Run the tests to verify they fail**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && go test ./... -run 'MergeBounded' -v
```

Expected: FAIL — `undefined: MergeBounded`.

- [ ] **Step 5: Implement `MergeBounded`**

In `merge_bounded.go`, replace the doc comment, signature, and the two paging blocks. Everything between `out := make(...)` and the merge loop's `out = append(out, take)` is unchanged.

```go
// MergeBounded performs a bounded k-way merge of a sorted committed source
// (next, lazy pull) with a pre-sorted adds slice, skipping committed rows for
// which deleted(id) is true, ordered by LessByOrder(specs).
//
// limit > 0 is a bounded-or-fail cap on the merged result, not a page size:
// if the number of survivors exceeds limit, MergeBounded returns
// ErrSearchResultLimitExceeded rather than a truncated prefix. The bound gates
// on TOTAL survivors, so the adds slice alone can trip it. Memory is bounded
// to ~limit+1+len(adds): the committed source is pulled lazily and the merge
// stops the moment the bound is exceeded.
//
// limit <= 0 means unbounded — it drains and materializes the entire surviving
// sequence and never raises. Callers must not substitute a default for a
// non-positive limit; "unbounded" is a real, load-bearing request mode.
func MergeBounded(next func() (*Entity, bool, error), adds []*Entity, deleted func(id string) bool, specs []OrderSpec, limit int) ([]*Entity, error) {
	need := -1
	if limit > 0 {
		need = limit + 1
	}
```

Then replace the trailing paging block:

```go
		out = append(out, take)
		if need >= 0 && len(out) >= need {
			break
		}
	}
	if limit > 0 && len(out) > limit {
		return nil, fmt.Errorf("merge: %d or more matches exceed the limit of %d: %w", len(out), limit, ErrSearchResultLimitExceeded)
	}
	return out, nil
}
```

Add `"fmt"` to the imports.

- [ ] **Step 6: Run the tests to verify they pass**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && go test ./... -v
```

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git add merge_bounded.go merge_bounded_test.go
git commit -m "feat(search)!: MergePage becomes MergeBounded — bounded-or-fail, no offset

The merged result must fit within limit or the merge fails with
ErrSearchResultLimitExceeded; it never returns a truncated prefix. The
offset parameter is dropped: no direct-search transport exposes an
offset and no caller sets one."
```

---

### Task 2: SPI — remove `SearchOptions.Offset` and state the contract

**Files:**
- Modify: `/Users/paul/go-projects/cyoda-light/cyoda-go-spi/searcher.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `spi.SearchOptions` without the `Offset` field. `Searcher` interface doc carries the bounded-or-fail contract.

- [ ] **Step 1: Write the failing test**

Add to `searcher_test.go` (create it if absent, `package spi`). This is a compile-level contract test: it fails to build while `Offset` still exists.

```go
// TestSearchOptions_NoOffset pins the removal of the Offset field. Direct
// search exposes no offset on any transport, and a bounded-or-fail search has
// no page to offset into. If someone re-adds it, this stops compiling.
func TestSearchOptions_NoOffset(t *testing.T) {
	typ := reflect.TypeOf(SearchOptions{})
	if _, found := typ.FieldByName("Offset"); found {
		t.Fatal("SearchOptions.Offset must not exist: direct search does not paginate")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && go test ./... -run TestSearchOptions_NoOffset -v
```

Expected: FAIL — "SearchOptions.Offset must not exist".

- [ ] **Step 3: Remove the field and document the contract**

In `searcher.go`, delete `Offset int` from `SearchOptions`, extend the `Searcher` doc, and document `Limit`:

```go
// Searcher is an optional interface for storage plugins that support
// search predicate pushdown (e.g. SQL WHERE clauses). Plugins that
// implement Searcher get native query execution; those that don't
// fall back to in-memory filtering.
//
// Search is bounded-or-fail. SearchOptions.Limit > 0 is a cap on the matched
// set, not a page size: an implementation that finds more matches than Limit
// MUST return ErrSearchResultLimitExceeded and MUST NOT return a truncated
// prefix — a silently truncated result is a wrong answer the caller cannot
// distinguish from a complete one. Exactly-at-limit succeeds.
//
// Limit <= 0 means unbounded, and an implementation MUST NOT substitute a
// default of its own: the engine resolves the direct-search default before
// calling, so a non-positive Limit is a deliberate request for the complete
// matched set (scoped delete, async snapshot) and capping it silently
// truncates that caller's result.
//
// Search MUST honour an active transaction (read-your-own-writes): with no
// transaction active it is a committed pushdown; with a transaction active it
// overlays the transaction's write-set so the result is identical to what
// GetAll + in-memory match would produce. In-transaction point-in-time reads
// are committed-only — they never see the transaction's own uncommitted
// writes for the PIT dimension. Returned entities enter the transaction's
// read-set only when SearchOptions.TrackingRead is set; under bounded-or-fail
// that is exactly the matched set, since there is no page smaller than it.
type Searcher interface {
	Search(ctx context.Context, filter Filter, opts SearchOptions) ([]*Entity, error)
}

// SearchOptions configures bounding, ordering, and scoping for a search.
// There is no Offset: direct search does not paginate (async search does,
// over its persisted result-ID list).
type SearchOptions struct {
	ModelName    string
	ModelVersion string
	PointInTime  *time.Time

	// Limit is a bounded-or-fail cap on the matched set when > 0, and means
	// unbounded when <= 0. See the Searcher doc comment — both halves of that
	// contract are load-bearing.
	Limit   int
	OrderBy []OrderSpec
	...
}
```

Leave the `TrackingRead` field and its existing doc unchanged.

- [ ] **Step 4: Run the SPI tests**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && go test ./... -v
```

Expected: PASS. Add `"reflect"` and `"testing"` imports to `searcher_test.go` if the build complains.

- [ ] **Step 5: Commit**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git add searcher.go searcher_test.go
git commit -m "feat(search)!: drop SearchOptions.Offset, state the bounded-or-fail contract

Limit > 0 caps the matched set and fails when exceeded; Limit <= 0 is
unbounded and a plugin must not substitute a default. Offset is removed:
no direct-search transport exposes one."
```

---

### Task 3: SPI — `spitest` Searcher conformance suite

**Files:**
- Create: `/Users/paul/go-projects/cyoda-light/cyoda-go-spi/spitest/searcher.go`
- Modify: `/Users/paul/go-projects/cyoda-light/cyoda-go-spi/spitest/spitest.go:143` (add the suite registration)

**Interfaces:**
- Consumes: `spi.MergeBounded` semantics (Task 1), `spi.SearchOptions` without `Offset` (Task 2).
- Produces: `runSearcherSuite(t *testing.T, h Harness, tracker *skipTracker)`, registered as the `Searcher` subtest group. Every plugin calling `StoreFactoryConformance` gets it with no per-plugin wiring.

- [ ] **Step 1: Write the suite (this IS the test)**

Create `spitest/searcher.go`. It follows `spitest/entity.go`'s shape: obtain the store from the harness factory, seed via `withTx`, assert. `newEntity` and `withTx` are the existing helpers in `spitest/helpers.go`.

```go
package spitest

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// searcherSeedN is the match-set size the bounded-or-fail subtests seed.
// Deliberately tiny: every backend runs this suite, including ones where
// seeding is a network round-trip per entity.
const searcherSeedN = 5

// runSearcherSuite exercises the optional spi.Searcher contract. Backends
// whose EntityStore does not implement Searcher skip the whole group — the
// interface is optional by design, so absence is conformant, not a failure.
// (This is a type assertion rather than a Harness.Skip entry because
// StoreFactoryConformance fails on Skip keys that never match.)
func runSearcherSuite(t *testing.T, h Harness, tracker *skipTracker) {
	tenant := h.NewTenant()
	ctx := tenantContext(tenant)
	store, err := h.Factory.EntityStore(ctx)
	require.NoError(t, err)
	if _, ok := store.(spi.Searcher); !ok {
		t.Skip("EntityStore does not implement spi.Searcher (optional interface)")
	}

	t.Run("BoundedOrFail", func(t *testing.T) {
		runSubtest(t, tracker, h.Skip, "Searcher/BoundedOrFail", func(t *testing.T) {
			searcherBoundedOrFail(t, h, false)
		})
	})
	t.Run("BoundedOrFailInTx", func(t *testing.T) {
		runSubtest(t, tracker, h.Skip, "Searcher/BoundedOrFailInTx", func(t *testing.T) {
			searcherBoundedOrFail(t, h, true)
		})
	})
}

// searcherBoundedOrFail seeds searcherSeedN matching entities and asserts the
// three-way contract: under the limit fails, at the limit succeeds, unbounded
// returns everything. inTx runs the assertions inside a live transaction so
// each backend's read-your-own-writes overlay is held to the same bound.
func searcherBoundedOrFail(t *testing.T, h Harness, inTx bool) {
	t.Helper()
	tenant := h.NewTenant()
	ctx := tenantContext(tenant)
	const model = "searcher-bounded"

	withTx(t, h, ctx, func(txCtx spiCtx) {
		es, err := h.Factory.EntityStore(txCtx)
		require.NoError(t, err)
		for i := 0; i < searcherSeedN; i++ {
			e := newEntity(t, model, uuid.NewString(), map[string]any{"status": "match"})
			require.NoError(t, es.Save(txCtx, e))
		}
	})

	filter := spi.Filter{
		Op:     spi.OpEquals,
		Path:   "status",
		Source: spi.SourceData,
		Value:  "match",
	}
	opts := func(limit int) spi.SearchOptions {
		return spi.SearchOptions{ModelName: model, ModelVersion: "1", Limit: limit}
	}

	run := func(t *testing.T, limit int) ([]*spi.Entity, error) {
		t.Helper()
		if !inTx {
			es, err := h.Factory.EntityStore(ctx)
			require.NoError(t, err)
			return es.(spi.Searcher).Search(ctx, filter, opts(limit))
		}
		var got []*spi.Entity
		var sErr error
		withTx(t, h, ctx, func(txCtx spiCtx) {
			es, err := h.Factory.EntityStore(txCtx)
			require.NoError(t, err)
			got, sErr = es.(spi.Searcher).Search(txCtx, filter, opts(limit))
		})
		return got, sErr
	}

	t.Run("OverLimitFails", func(t *testing.T) {
		_, err := run(t, searcherSeedN-1)
		if !errors.Is(err, spi.ErrSearchResultLimitExceeded) {
			t.Fatalf("%d matches over limit %d: got err %v, want ErrSearchResultLimitExceeded",
				searcherSeedN, searcherSeedN-1, err)
		}
	})

	t.Run("AtLimitSucceeds", func(t *testing.T) {
		got, err := run(t, searcherSeedN)
		require.NoError(t, err)
		require.Len(t, got, searcherSeedN)
	})

	t.Run("UnboundedReturnsAll", func(t *testing.T) {
		got, err := run(t, 0)
		require.NoError(t, err)
		require.Len(t, got, searcherSeedN)
	})
}
```

Check `spitest/entity.go` for the exact `runSubtest` signature and the exact `spi.Filter` field and operator constant names before writing — mirror what that file already does rather than trusting the sketch above. Add the `uuid` import that `newEntity`'s id argument needs.

- [ ] **Step 2: Register the suite**

In `spitest/spitest.go`, after the `AsyncSearch` line at `:143`:

```go
	t.Run("AsyncSearch", func(t *testing.T) { runAsyncSearchSuite(t, h, tracker) })
	t.Run("Searcher", func(t *testing.T) { runSearcherSuite(t, h, tracker) })
}
```

- [ ] **Step 3: Run against a plugin to verify it fails**

The memory plugin still truncates, so the new suite must go red.

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/feat-437-bounded-or-fail-search/plugins/memory
go test ./... -run 'TestConformance/Searcher' -v
```

Expected: FAIL on `OverLimitFails` — got 4 entities and a nil error, want `ErrSearchResultLimitExceeded`.

- [ ] **Step 4: Commit the suite (still red — plugins are fixed in Tasks 4-6)**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git add spitest/searcher.go spitest/spitest.go
git commit -m "test(spitest): Searcher bounded-or-fail conformance suite

Holds every backend to the same three-way contract in and out of a
transaction: over the limit fails, at the limit succeeds, unbounded
returns everything. Auto-skips backends without spi.Searcher."
```

---

### Task 4: memory plugin — bounded-or-fail

**Files:**
- Modify: `plugins/memory/searcher.go:47,69,127,166-187`
- Test: `plugins/memory/searcher_test.go`

**Interfaces:**
- Consumes: `spi.MergeBounded` (Task 1), `spi.SearchOptions` without `Offset` (Task 2).
- Produces: `matchSortBounded(filter spi.Filter, rows []*spi.Entity, order []spi.OrderSpec, limit int) ([]*spi.Entity, error)` — replaces `matchSortPage`.

- [ ] **Step 1: Write the failing tests**

Append to `plugins/memory/searcher_test.go`, following that file's existing store/seed helpers.

```go
func TestMemorySearcher_OverLimitFails(t *testing.T) {
	store, ctx := newSearchStore(t) // existing helper in this file
	seedMatching(t, store, ctx, 3)  // existing helper; 3 entities matching `filter`
	_, err := store.Search(ctx, matchAllFilter, spi.SearchOptions{
		ModelName: searchModel, ModelVersion: "1", Limit: 2,
	})
	if !errors.Is(err, spi.ErrSearchResultLimitExceeded) {
		t.Fatalf("3 matches over limit 2: got err %v, want ErrSearchResultLimitExceeded", err)
	}
}

func TestMemorySearcher_AtLimitSucceeds(t *testing.T) {
	store, ctx := newSearchStore(t)
	seedMatching(t, store, ctx, 2)
	got, err := store.Search(ctx, matchAllFilter, spi.SearchOptions{
		ModelName: searchModel, ModelVersion: "1", Limit: 2,
	})
	if err != nil {
		t.Fatalf("2 matches at limit 2: unexpected err %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}
}

func TestMemorySearcher_UnboundedReturnsAll(t *testing.T) {
	store, ctx := newSearchStore(t)
	seedMatching(t, store, ctx, 3)
	got, err := store.Search(ctx, matchAllFilter, spi.SearchOptions{
		ModelName: searchModel, ModelVersion: "1", Limit: 0,
	})
	if err != nil {
		t.Fatalf("limit 0 must be unbounded: unexpected err %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d, want 3", len(got))
	}
}

// The in-tx overlay must apply the same bound: committed rows plus the
// transaction's own buffered writes together must fit.
func TestMemorySearcher_TxOverlayOverLimitFails(t *testing.T) {
	store, ctx := newSearchStore(t)
	seedMatching(t, store, ctx, 2)
	txCtx, done := beginTx(t, store, ctx) // existing helper in this package's tests
	defer done()
	saveMatching(t, store, txCtx, 1) // one buffered own-write → 3 survivors
	_, err := store.Search(txCtx, matchAllFilter, spi.SearchOptions{
		ModelName: searchModel, ModelVersion: "1", Limit: 2,
	})
	if !errors.Is(err, spi.ErrSearchResultLimitExceeded) {
		t.Fatalf("2 committed + 1 buffered over limit 2: got err %v, want ErrSearchResultLimitExceeded", err)
	}
}
```

Use whatever seed/store/tx helpers `plugins/memory/searcher_test.go` already defines; do not add parallel ones. Delete the offset assertions at `plugins/memory/searcher_test.go:105`.

- [ ] **Step 2: Run to verify they fail**

```bash
cd plugins/memory && go test ./... -run 'TestMemorySearcher_(OverLimit|AtLimit|Unbounded|TxOverlayOverLimit)' -v
```

Expected: FAIL — over-limit cases return 2 entities and a nil error.

- [ ] **Step 3: Implement**

Replace `matchSortPage` (`searcher.go:166-187`) with:

```go
// matchSortBounded filters rows with spi.MatchFilter, orders with
// spi.LessByOrder, and enforces the bounded-or-fail cap: limit > 0 means the
// whole matched set must fit, and a larger match set is an error rather than a
// truncated prefix. limit <= 0 is unbounded. Used by the non-tx and in-tx PIT
// branches; the RYW overlay branch gets the same bound from spi.MergeBounded.
func matchSortBounded(filter spi.Filter, rows []*spi.Entity, order []spi.OrderSpec, limit int) ([]*spi.Entity, error) {
	filtered := make([]*spi.Entity, 0, len(rows))
	for _, e := range rows {
		if spi.MatchFilter(filter, e.Data, e.Meta) {
			filtered = append(filtered, e)
			// Short-circuit before sorting: the result is an error either way.
			if limit > 0 && len(filtered) > limit {
				return nil, fmt.Errorf("search: more than %d matches: %w", limit, spi.ErrSearchResultLimitExceeded)
			}
		}
	}
	sortByOrder(filtered, order)
	return filtered, nil
}
```

Update the three call sites:

```go
// searcher.go:47 and :69 — both non-tx and in-tx-PIT branches
		return matchSortBounded(filter, committed, opts.OrderBy, opts.Limit)
```

```go
// searcher.go:127
	page, err := spi.MergeBounded(next, adds, deleted, opts.OrderBy, opts.Limit)
```

Update the `Search` doc comment at `:14-28`: replace "filter, sort, page" with "filter, sort, bound" and note that `spi.MergeBounded` supplies the overlay branch's bound. Update the read-set comment at `:132-142` to say the recorded set is exactly the matched set, since bounded-or-fail leaves no smaller page.

- [ ] **Step 4: Run the full memory suite**

```bash
cd plugins/memory && go test ./... -v
```

Expected: PASS, including `TestConformance/Searcher`.

- [ ] **Step 5: Commit**

```bash
git add plugins/memory/searcher.go plugins/memory/searcher_test.go
git commit -m "feat(memory): bounded-or-fail direct search

matchSortPage becomes matchSortBounded: a matched set larger than the
limit is an error, not a truncated prefix. The RYW overlay branch gets
the same bound from spi.MergeBounded."
```

---

### Task 5: sqlite plugin — bounded-or-fail

**Files:**
- Modify: `plugins/sqlite/searcher.go:70-80,120-137,285`
- Test: `plugins/sqlite/searcher_test.go`, `plugins/sqlite/searcher_tx_test.go`

**Interfaces:**
- Consumes: `spi.MergeBounded` (Task 1), `spi.SearchOptions` without `Offset` (Task 2).
- Produces: no new exported surface.

- [ ] **Step 1: Write the failing tests**

Append to `plugins/sqlite/searcher_test.go`, reusing that file's existing store/seed helpers. Two branches must be covered separately — the **pushdown** branch (no residual) and the **residual** branch (a regex or case-insensitive predicate forces a Go post-filter).

```go
func TestSQLiteSearcher_PushdownOverLimitFails(t *testing.T) {
	store, ctx := newSearchStore(t)
	seedMatching(t, store, ctx, 3)
	_, err := store.Search(ctx, pushableEqualsFilter, spi.SearchOptions{
		ModelName: searchModel, ModelVersion: "1", Limit: 2,
	})
	if !errors.Is(err, spi.ErrSearchResultLimitExceeded) {
		t.Fatalf("pushdown branch, 3 matches over limit 2: got err %v, want ErrSearchResultLimitExceeded", err)
	}
}

func TestSQLiteSearcher_ResidualOverLimitFails(t *testing.T) {
	store, ctx := newSearchStore(t)
	seedMatching(t, store, ctx, 3)
	// A regex predicate is not pushable, so this exercises the Go post-filter.
	_, err := store.Search(ctx, residualRegexFilter, spi.SearchOptions{
		ModelName: searchModel, ModelVersion: "1", Limit: 2,
	})
	if !errors.Is(err, spi.ErrSearchResultLimitExceeded) {
		t.Fatalf("residual branch, 3 matches over limit 2: got err %v, want ErrSearchResultLimitExceeded", err)
	}
}

func TestSQLiteSearcher_AtLimitSucceeds(t *testing.T) {
	store, ctx := newSearchStore(t)
	seedMatching(t, store, ctx, 2)
	got, err := store.Search(ctx, pushableEqualsFilter, spi.SearchOptions{
		ModelName: searchModel, ModelVersion: "1", Limit: 2,
	})
	if err != nil {
		t.Fatalf("2 matches at limit 2: unexpected err %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}
}

func TestSQLiteSearcher_UnboundedReturnsAll(t *testing.T) {
	store, ctx := newSearchStore(t)
	seedMatching(t, store, ctx, 3)
	got, err := store.Search(ctx, pushableEqualsFilter, spi.SearchOptions{
		ModelName: searchModel, ModelVersion: "1", Limit: 0,
	})
	if err != nil {
		t.Fatalf("limit 0 must be unbounded: unexpected err %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d, want 3", len(got))
	}
}
```

Append to `plugins/sqlite/searcher_tx_test.go`:

```go
func TestSearchTx_OverlayOverLimitFails(t *testing.T) {
	store, ctx := newSearchStore(t)
	seedMatching(t, store, ctx, 2)
	txCtx, done := beginTx(t, store, ctx)
	defer done()
	saveMatching(t, store, txCtx, 1) // 2 committed + 1 buffered own-write
	_, err := store.Search(txCtx, pushableEqualsFilter, spi.SearchOptions{
		ModelName: searchModel, ModelVersion: "1", Limit: 2,
	})
	if !errors.Is(err, spi.ErrSearchResultLimitExceeded) {
		t.Fatalf("overlay, 3 survivors over limit 2: got err %v, want ErrSearchResultLimitExceeded", err)
	}
}
```

Rewrite `TestSearchTx_TrackingRead_PagedWindowOnly` (`searcher_tx_test.go:286-330`) to the new invariant. It currently asserts that rows paged out by `Offset`/`Limit` stay out of `tx.ReadSet`; under bounded-or-fail there is no smaller page, so the invariant becomes "records exactly the matched committed set, and nothing buffered". Rename it `TestSearchTx_TrackingRead_RecordsMatchedSet` and assert that with 3 committed matches and `Limit: 3`, all three committed ids are in `tx.ReadSet` and no buffered id is. Delete the offset assertions at `searcher_test.go:302-361` and `searcher_tx_test.go:237`.

- [ ] **Step 2: Run to verify they fail**

```bash
cd plugins/sqlite && go test ./... -run 'OverLimit|AtLimit|Unbounded|TrackingRead_RecordsMatchedSet' -v
```

Expected: FAIL — over-limit cases return 2 entities and a nil error.

- [ ] **Step 3: Implement the pushdown branch**

`searcher.go:70-80` — request one more row than the cap so `limit+1` proves the overflow:

```go
	// When there is no residual, push the bound into SQL. Ask for limit+1: the
	// extra row is the proof that the matched set does not fit, which is what
	// bounded-or-fail must report instead of truncating to limit.
	if plan.postFilter == nil && opts.Limit > 0 {
		baseQuery += " LIMIT ?"
		baseArgs = append(baseArgs, opts.Limit+1)
	}
```

- [ ] **Step 4: Implement the residual branch and the shared check**

`searcher.go:120-137` — replace the Go-side offset/limit block. Inside the row loop, after `results = append(results, e)`:

```go
		results = append(results, e)
		if opts.Limit > 0 && len(results) > opts.Limit {
			return nil, fmt.Errorf("search: more than %d matches: %w", opts.Limit, spi.ErrSearchResultLimitExceeded)
		}
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("row iteration: %w", err)
	}

	// The pushdown branch asked SQL for limit+1; getting it means the matched
	// set does not fit. The residual branch reaches here only when it stayed
	// within the bound, so this is a no-op for it.
	if opts.Limit > 0 && len(results) > opts.Limit {
		return nil, fmt.Errorf("search: more than %d matches: %w", opts.Limit, spi.ErrSearchResultLimitExceeded)
	}

	return results, nil
}
```

Delete the trailing `if plan.postFilter != nil { ...offset/limit slicing... }` block entirely.

- [ ] **Step 5: Implement the overlay branch**

`searcher.go:285`:

```go
		page, mErr := spi.MergeBounded(next, adds, deleted, opts.OrderBy, opts.Limit)
```

Update the `Search` doc at `:15-26`, the `searchCommitted` doc at `:43-46`, and `searchTxOverlay`'s doc at `:189-197` to say "bound" rather than "page"/"pagination". Update the read-set comment at `:290-299` to the matched-set invariant. Note in `searchCommitted`'s doc that the scan budget and the result bound are independent and whichever trips first wins.

- [ ] **Step 6: Run the full sqlite suite**

```bash
cd plugins/sqlite && go test ./... -v
```

Expected: PASS, including `TestConformance/Searcher`.

- [ ] **Step 7: Commit**

```bash
git add plugins/sqlite/searcher.go plugins/sqlite/searcher_test.go plugins/sqlite/searcher_tx_test.go
git commit -m "feat(sqlite): bounded-or-fail direct search

Pushdown asks SQL for limit+1 so the overflow is provable; the residual
post-filter and the RYW overlay enforce the same bound. Offset handling
is removed. The scan budget and the result bound stay independent —
whichever trips first wins."
```

---

### Task 6: postgres plugin — bounded-or-fail

**Files:**
- Modify: `plugins/postgres/searcher.go:103-155`
- Test: `plugins/postgres/searcher_test.go`

**Interfaces:**
- Consumes: `spi.SearchOptions` without `Offset` (Task 2).
- Produces: no new exported surface. postgres never calls `MergeBounded` — a real `pgx.Tx` under REPEATABLE READ gives RYW natively, so the committed pushdown *is* the in-tx result and the bound applies once.

- [ ] **Step 1: Write the failing tests**

Append to `plugins/postgres/searcher_test.go`, using that file's existing container/store helpers. Same two branches as sqlite, plus the in-tx case (which exercises the same code path through a live `pgx.Tx`).

```go
func TestPGSearcher_PushdownOverLimitFails(t *testing.T) {
	store, ctx := newSearchStore(t)
	seedMatching(t, store, ctx, 3)
	_, err := store.Search(ctx, pushableEqualsFilter, spi.SearchOptions{
		ModelName: searchModel, ModelVersion: "1", Limit: 2,
	})
	if !errors.Is(err, spi.ErrSearchResultLimitExceeded) {
		t.Fatalf("pushdown branch, 3 matches over limit 2: got err %v, want ErrSearchResultLimitExceeded", err)
	}
}

func TestPGSearcher_ResidualOverLimitFails(t *testing.T) {
	store, ctx := newSearchStore(t)
	seedMatching(t, store, ctx, 3)
	_, err := store.Search(ctx, residualRegexFilter, spi.SearchOptions{
		ModelName: searchModel, ModelVersion: "1", Limit: 2,
	})
	if !errors.Is(err, spi.ErrSearchResultLimitExceeded) {
		t.Fatalf("residual branch, 3 matches over limit 2: got err %v, want ErrSearchResultLimitExceeded", err)
	}
}

func TestPGSearcher_AtLimitSucceeds(t *testing.T) {
	store, ctx := newSearchStore(t)
	seedMatching(t, store, ctx, 2)
	got, err := store.Search(ctx, pushableEqualsFilter, spi.SearchOptions{
		ModelName: searchModel, ModelVersion: "1", Limit: 2,
	})
	if err != nil {
		t.Fatalf("2 matches at limit 2: unexpected err %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}
}

func TestPGSearcher_UnboundedReturnsAll(t *testing.T) {
	store, ctx := newSearchStore(t)
	seedMatching(t, store, ctx, 3)
	got, err := store.Search(ctx, pushableEqualsFilter, spi.SearchOptions{
		ModelName: searchModel, ModelVersion: "1", Limit: 0,
	})
	if err != nil {
		t.Fatalf("limit 0 must be unbounded: unexpected err %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d, want 3", len(got))
	}
}
```

Delete `TestPGSearcher_UnboundedOffsetWithResidual` and the other offset assertions at `searcher_test.go:179-232`.

- [ ] **Step 2: Run to verify they fail**

```bash
cd plugins/postgres && go test ./... -run 'OverLimit|AtLimit|Unbounded' -v
```

Expected: FAIL — over-limit cases return 2 entities and a nil error. (Requires Docker.)

- [ ] **Step 3: Implement**

`searcher.go:103-113` — replace the LIMIT/OFFSET block:

```go
	// No residual → push the bound into SQL. Ask for limit+1: the extra row is
	// the proof that the matched set does not fit, which bounded-or-fail must
	// report instead of truncating to limit.
	if plan.postFilter == nil && opts.Limit > 0 {
		baseQuery += fmt.Sprintf(" LIMIT $%d", len(baseArgs)+1)
		baseArgs = append(baseArgs, opts.Limit+1)
	}
```

`searcher.go:124-155` — replace both collection blocks:

```go
	// No residual: SQL already applied the limit+1 probe; collect everything.
	if plan.postFilter == nil {
		for it.Next() {
			results = append(results, it.Entity())
		}
		if err := it.Err(); err != nil {
			return nil, err
		}
		if opts.Limit > 0 && len(results) > opts.Limit {
			return nil, fmt.Errorf("search: more than %d matches: %w", opts.Limit, spi.ErrSearchResultLimitExceeded)
		}
		return results, nil
	}

	// Residual present: postgresIter yields only post-filter matches. Stop the
	// moment the matched set is known not to fit — there is no page to gather.
	for it.Next() {
		results = append(results, it.Entity())
		if opts.Limit > 0 && len(results) > opts.Limit {
			return nil, fmt.Errorf("search: more than %d matches: %w", opts.Limit, spi.ErrSearchResultLimitExceeded)
		}
	}
	if err := it.Err(); err != nil {
		return nil, err
	}
	return results, nil
}
```

Update the `Search` doc at `:14-27`: the "Pagination" paragraph becomes a "Bounding" paragraph describing the limit+1 probe and the residual early-raise, and the S1 guard note about `Limit<=0` draining before applying the offset is deleted along with the offset. Update the read-set comment at `:61-64` — "Recording only the RETURNED page (post-LIMIT/OFFSET)" becomes "Recording the matched set, which under bounded-or-fail is everything the search returns".

- [ ] **Step 4: Run the full postgres suite**

```bash
cd plugins/postgres && go test ./... -v
```

Expected: PASS, including `TestConformance/Searcher`.

- [ ] **Step 5: Commit**

```bash
git add plugins/postgres/searcher.go plugins/postgres/searcher_test.go
git commit -m "feat(postgres): bounded-or-fail direct search

Pushdown asks SQL for limit+1 so the overflow is provable; the residual
post-filter raises the moment the matched set is known not to fit.
Offset handling is removed."
```

---

### Task 7: engine — bound the fallback branch, drop domain `Offset`

**Files:**
- Modify: `internal/domain/search/service.go:29-44,215-223,288-308,386-396`
- Test: `internal/domain/search/service_test.go`

**Interfaces:**
- Consumes: plugin bounded-or-fail (Tasks 4-6).
- Produces: `search.SearchOptions` without `Offset`. The `GetAll` + in-memory match branch returns a `*common.AppError` with `common.ErrCodeSearchResultLimit` when the matched set exceeds `opts.Limit`.

- [ ] **Step 1: Write the failing test**

Append to `internal/domain/search/service_test.go`. The fallback branch is reached when the condition cannot be translated to a `spi.Filter` — a `FunctionCondition` never can (`filter_translate.go:36`), which is the honest way to drive it.

```go
// The GetAll + in-memory match fallback (reached when a condition is not
// translatable to a pushdown filter — a function condition never is) must be
// bounded-or-fail too. Otherwise a translate-failure request silently
// truncates while the pushdown path 400s, which is the same divergence inside
// one backend that this change removes across backends.
func TestSearch_FallbackBranchIsBounded(t *testing.T) {
	svc, ctx, ref := newFallbackFixture(t, 3) // 3 matching entities, no Searcher
	_, err := svc.Search(ctx, ref, functionCondition(t), SearchOptions{Limit: 2})

	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("got err %v, want *common.AppError", err)
	}
	if appErr.Status != http.StatusBadRequest || appErr.Code != common.ErrCodeSearchResultLimit {
		t.Fatalf("got %d/%s, want 400/%s", appErr.Status, appErr.Code, common.ErrCodeSearchResultLimit)
	}
}

func TestSearch_FallbackBranchUnboundedReturnsAll(t *testing.T) {
	svc, ctx, ref := newFallbackFixture(t, 3)
	got, err := svc.Search(ctx, ref, functionCondition(t), SearchOptions{Limit: 0})
	if err != nil {
		t.Fatalf("limit 0 must be unbounded: unexpected err %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d, want 3", len(got))
	}
}
```

Build `newFallbackFixture` and `functionCondition` on the stub-store helpers already in `service_test.go`. Delete the offset assertions at `service_test.go:274-284`.

- [ ] **Step 2: Run to verify they fail**

```bash
go test ./internal/domain/search/... -run 'FallbackBranch' -v
```

Expected: FAIL — bounded case returns 2 entities and a nil error.

- [ ] **Step 3: Remove `Offset` from the domain options**

`service.go:29-44`, delete the `Offset` field from `SearchOptions`. Leave `ResultOptions.Offset` at `:46-50` untouched — that is async result-ID pagination, a different contract.

`service.go:215-223`, drop `Offset: opts.Offset,` from the `spi.SearchOptions` literal.

`service.go:386-396`, drop `Offset` from both the anonymous struct and the value it is built from, so the persisted async job-opts JSON no longer carries an `"offset"` key.

- [ ] **Step 4: Bound the fallback branch**

`service.go:288-308`, replace the sort/offset/limit tail:

```go
	sortEntities(matches, orderBy)

	// Bounded-or-fail, same contract as the Searcher path above. A truncated
	// prefix here would be indistinguishable from a complete result, so an
	// oversized match set is an error. Limit <= 0 is unbounded (async submit,
	// scoped delete) and never raises.
	if opts.Limit > 0 && len(matches) > opts.Limit {
		return nil, common.Operational(http.StatusBadRequest,
			common.ErrCodeSearchResultLimit,
			"matched result count exceeds the configured limit")
	}

	return matches, nil
}
```

Extend the comment block at `:241-248` to record that this branch's `GetAll` has already materialised the whole model and, in-transaction, already recorded it into the read-set before the bound can raise — so a request that 400s here still leaves the transaction with a model-wide read-set.

- [ ] **Step 5: Run the package tests**

```bash
go test ./internal/domain/search/... -v
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/domain/search/service.go internal/domain/search/service_test.go
git commit -m "feat(search): bound the in-memory fallback branch, drop domain Offset

The GetAll + in-memory match branch (reached when a condition is not
translatable to a pushdown filter) now fails with SEARCH_RESULT_LIMIT
instead of truncating, so no direct-search route can return a silent
prefix. SearchOptions.Offset is removed along with the persisted
job-opts key; async result pagination keeps its own ResultOptions."
```

---

### Task 8: transports — reject a non-positive `limit`

**Files:**
- Modify: `internal/domain/search/handler.go:112-129`
- Modify: `internal/grpc/search.go:336-340`
- Test: `internal/domain/search/handler_limit_test.go`, `internal/grpc/search_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: HTTP `400 BAD_REQUEST` for `limit < 1`; gRPC `CLIENT_ERROR` for `limit < 1`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/domain/search/handler_limit_test.go`:

```go
// limit=0 previously reached the SPI as Limit 0, which means UNBOUNDED — an
// unbounded synchronous NDJSON search, and a way around the cap this endpoint
// exists to enforce. Reject it, consistent with how limit > MaxPageSize is
// rejected rather than clamped.
func TestSearchEntities_LimitZeroRejected(t *testing.T) {
	rr := doSearch(t, "?limit=0") // existing helper in this file
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("limit=0: got status %d, want 400", rr.Code)
	}
	if code := problemErrorCode(rr.Body.String()); code != common.ErrCodeBadRequest {
		t.Fatalf("limit=0: got errorCode %q, want %s", code, common.ErrCodeBadRequest)
	}
}

func TestSearchEntities_LimitNegativeRejected(t *testing.T) {
	rr := doSearch(t, "?limit=-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("limit=-1: got status %d, want 400", rr.Code)
	}
}
```

Append to `internal/grpc/search_test.go`, modelled on the existing `TestDirectSearch_ResultLimitSentinel_ClientError` at `:719`:

```go
// gRPC validated neither bound on limit, so limit:-1 reached the SPI as
// Limit 0 — an unbounded search. HTTP has always rejected negatives; gRPC
// must too, or the compute-node transport is a cap bypass.
func TestDirectSearch_NonPositiveLimit_ClientError(t *testing.T) {
	for _, limit := range []int{0, -1} {
		resp := directSearch(t, withLimit(limit)) // existing harness helpers
		if resp.Success {
			t.Fatalf("limit=%d: got Success=true, want a client error", limit)
		}
		if resp.Error.Code != "CLIENT_ERROR" {
			t.Fatalf("limit=%d: got Error.Code %q, want CLIENT_ERROR", limit, resp.Error.Code)
		}
	}
}
```

- [ ] **Step 2: Run to verify they fail**

```bash
go test ./internal/domain/search/... -run LimitZeroRejected -v
go test ./internal/grpc/... -run NonPositiveLimit -v
```

Expected: FAIL — both currently return 200 / `Success=true`.

- [ ] **Step 3: Implement the HTTP check**

`handler.go:113-129`:

```go
	// Parse limit from string parameter.
	if params.Limit != nil {
		lim, err := strconv.Atoi(*params.Limit)
		if err != nil || lim < 1 {
			common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid limit"))
			return
		}
		// Reject (don't silently clamp): the async path does the same.
		// Silent clamping would hide misuse from clients and mask bugs
		// where a caller assumed a larger window than the server allows.
		// The lower bound matters just as much: a non-positive limit means
		// UNBOUNDED at the SPI, so accepting it would hand clients an
		// unbounded synchronous search past the cap.
		if lim > pagination.MaxPageSize {
			common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, fmt.Sprintf("limit exceeds maximum %d", pagination.MaxPageSize)))
			return
		}
		opts.Limit = lim
	} else {
		opts.Limit = DefaultDirectSearchLimit
	}
```

- [ ] **Step 4: Implement the gRPC check**

`internal/grpc/search.go:336-340`:

```go
	if req.Limit != nil {
		// A non-positive limit means UNBOUNDED at the SPI, so accepting it
		// would make this transport an unbounded-search bypass of the cap.
		// HTTP rejects the same values.
		if *req.Limit < 1 {
			return status.Errorf(codes.InvalidArgument, "invalid limit: must be at least 1")
		}
		opts.Limit = *req.Limit
	} else {
		opts.Limit = search.DefaultDirectSearchLimit
	}
```

Check how neighbouring validation failures in `handleDirectSearchRequest` are returned — if the surrounding code emits a CloudEvent error via `entityResponseError` rather than a gRPC status, match that instead, so the client sees the same envelope shape as every other client error on this RPC.

- [ ] **Step 5: Run both packages**

```bash
go test ./internal/domain/search/... ./internal/grpc/... -v
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/domain/search/handler.go internal/domain/search/handler_limit_test.go internal/grpc/search.go internal/grpc/search_test.go
git commit -m "fix(search): reject a non-positive direct-search limit on both transports

limit=0 over HTTP and limit<1 over gRPC reached the SPI as Limit 0,
which means unbounded — an unbounded synchronous search that walks past
the cap the endpoint exists to enforce."
```

---

### Task 9: conditional delete — forward classified 4xx

**Files:**
- Modify: `internal/domain/entity/service.go:947-951`
- Test: `internal/domain/entity/service_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: the delete-selection search's `*common.AppError` reaches the client with its own status and code instead of a `500 SERVER_ERROR` ticket.

- [ ] **Step 1: Write the failing test**

`common.Internal` unwraps only `ErrUniqueViolation` / `ErrPartialUniqueKey` / `ErrConflict` (`internal/common/errors.go:107`), so today every classified 4xx from the selection search is re-wrapped as a 500. That already buries sqlite's `SCAN_BUDGET_EXHAUSTED` and invalid-condition errors.

```go
// The selection search can fail with a classified 4xx — a scan budget
// exhausted, an unknown field path, an invalid condition. Wrapping those in
// common.Internal turns an actionable client error into an opaque 500 with a
// ticket, so the caller cannot tell a bad request from a server fault.
func TestDeleteEntities_Conditional_ForwardsSearch4xx(t *testing.T) {
	svc, ctx, ref := newDeleteFixture(t)
	svc.searchSvc = stubSearchFailing(common.Operational(
		http.StatusBadRequest, common.ErrCodeScanBudgetExhausted, "search scan budget exhausted"))

	_, err := svc.DeleteEntities(ctx, ref, someCondition(t))

	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("got err %v, want *common.AppError", err)
	}
	if appErr.Status != http.StatusBadRequest {
		t.Fatalf("got status %d, want 400", appErr.Status)
	}
	if appErr.Code != common.ErrCodeScanBudgetExhausted {
		t.Fatalf("got code %s, want %s", appErr.Code, common.ErrCodeScanBudgetExhausted)
	}
}
```

Use the delete-path fixtures already in `internal/domain/entity/service_test.go`; if the search service is not currently injectable there, inject it the way the surrounding tests inject their other collaborators rather than introducing a new seam.

- [ ] **Step 2: Run to verify it fails**

```bash
go test ./internal/domain/entity/... -run ForwardsSearch4xx -v
```

Expected: FAIL — got status 500 / `SERVER_ERROR`.

- [ ] **Step 3: Implement**

`internal/domain/entity/service.go:947-951`:

```go
	matched, err := h.searchSvc.Search(txCtx, ref, cond, search.SearchOptions{PointInTime: pointInTime, Limit: -1})
	if err != nil {
		h.rollbackOwned(txCtx, txID, owned)
		// A classified 4xx from the selection search (scan budget exhausted,
		// unknown field path, invalid condition) is the caller's error, not a
		// server fault — common.Internal would bury it as a 500 + ticket.
		var appErr *common.AppError
		if errors.As(err, &appErr) {
			return nil, appErr
		}
		return nil, common.Internal("failed to select entities for delete", err)
	}
```

- [ ] **Step 4: Run the package tests**

```bash
go test ./internal/domain/entity/... -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/domain/entity/service.go internal/domain/entity/service_test.go
git commit -m "fix(entity): forward classified 4xx from the delete-selection search

common.Internal unwraps only the conflict sentinels, so a scan-budget,
unknown-path or invalid-condition 400 from the selection search reached
the client as an opaque 500 + ticket."
```

---

### Task 10: e2e — full-stack coverage on real postgres

**Files:**
- Create: `internal/e2e/search_bounded_test.go`
- Test: same file

**Interfaces:**
- Consumes: everything from Tasks 4-9.
- Produces: nothing consumed downstream.

- [ ] **Step 1: Write the tests**

`internal/e2e` boots real postgres via testcontainers in `TestMain`. `problemErrorCode` (`callback_txjoin_errors_test.go:38`) extracts `properties.errorCode` from an RFC 9457 body. The in-tx case uses `reqCtx.searchHTTP` from `search_intx_test.go:44` — in-tx search is reachable only through a joined compute-node callback, which that harness provides.

```go
// Direct search is bounded-or-fail on every backend: a matched set larger than
// the effective limit is a 400, not a truncated page. Seeded just over a small
// explicit limit rather than over the 1000 default, so the test stays fast —
// the default itself is proven in the parity suite.
func TestSearchDirect_OverLimit_Returns400(t *testing.T) {
	h := newHarness(t)
	model := h.setupSearchModel(t)
	h.seedMatching(t, model, 3)

	status, body := h.searchRaw(t, model, matchAllCondition, "?limit=2")
	if status != http.StatusBadRequest {
		t.Fatalf("3 matches over limit 2: got status %d, want 400; body=%s", status, body)
	}
	if code := problemErrorCode(body); code != "SEARCH_RESULT_LIMIT" {
		t.Fatalf("got errorCode %q, want SEARCH_RESULT_LIMIT; body=%s", code, body)
	}
}

func TestSearchDirect_AtLimit_Returns200(t *testing.T) {
	h := newHarness(t)
	model := h.setupSearchModel(t)
	h.seedMatching(t, model, 2)

	status, body := h.searchRaw(t, model, matchAllCondition, "?limit=2")
	if status != http.StatusOK {
		t.Fatalf("2 matches at limit 2: got status %d, want 200; body=%s", status, body)
	}
	if n := countNDJSONLines(body); n != 2 {
		t.Fatalf("got %d ndjson lines, want 2", n)
	}
}

func TestSearchDirect_LimitZero_Returns400(t *testing.T) {
	h := newHarness(t)
	model := h.setupSearchModel(t)
	h.seedMatching(t, model, 1)

	status, body := h.searchRaw(t, model, matchAllCondition, "?limit=0")
	if status != http.StatusBadRequest {
		t.Fatalf("limit=0: got status %d, want 400; body=%s", status, body)
	}
	if code := problemErrorCode(body); code != "BAD_REQUEST" {
		t.Fatalf("got errorCode %q, want BAD_REQUEST; body=%s", code, body)
	}
}

// The in-transaction overlay is bounded too. Reachable only through a joined
// compute-node callback — see search_intx_test.go's header for why.
func TestSearchDirect_InTx_OverLimit_Returns400(t *testing.T) {
	// Seed 2 committed matches, then inside a joined callback save a third
	// (own-write) and search with limit=2: 3 survivors over the bound.
	// Assert 400 + SEARCH_RESULT_LIMIT on the joined request.
}
```

Fill in `TestSearchDirect_InTx_OverLimit_Returns400` against the callback harness the way `search_intx_test.go` does — the sketch above states the scenario and the assertion, and that file supplies `searchHTTP`, the tx-token plumbing, and the processor registration. Reuse the model/seed helpers already in `internal/e2e` rather than adding new ones.

- [ ] **Step 2: Run them (they should pass — Tasks 4-9 landed the behaviour)**

```bash
go test ./internal/e2e/... -run 'TestSearchDirect_' -v
```

Expected: PASS. If any fails, the defect is in Tasks 4-9, not here.

- [ ] **Step 3: Confirm the unbounded conditional-delete guard still passes**

```bash
go test ./internal/e2e/... -run TestDeleteEntities_Conditional -v
```

Expected: PASS. This is the regression that proves `Limit <= 0` stayed unbounded — 1050 matches must still delete.

- [ ] **Step 4: Commit**

```bash
git add internal/e2e/search_bounded_test.go
git commit -m "test(e2e): full-stack bounded-or-fail direct search on real postgres

Over-limit 400 SEARCH_RESULT_LIMIT, at-limit 200, limit=0 400
BAD_REQUEST, and the in-transaction overlay bound via the joined
compute-node callback harness."
```

---

### Task 11: parity — rewrite the truncation scenario

**Files:**
- Modify: `e2e/parity/search.go:262-288`
- Modify: `e2e/parity/registry.go:114`
- Modify: `e2e/parity/client/http.go` (add `SyncSearchRawLimit`)

**Interfaces:**
- Consumes: everything from Tasks 4-8.
- Produces: `RunSearchDirectBoundedOrFail(t *testing.T, fixture BackendFixture)`, registered as `SearchDirectBoundedOrFail`. Scenario count stays 218, so `registry_count_test.go` needs no change.

- [ ] **Step 1: Add the limit-bearing raw client helper**

`SyncSearchRaw` (`client/http.go:1162`) returns status and body but takes no limit; `SyncSearchSortedLimit` (`:1252`) takes a limit but decodes NDJSON and cannot see a 400. Add, next to `SyncSearchRaw`:

```go
// SyncSearchRawLimit is SyncSearchRaw with the `limit` query param set, so a
// negative-path assertion can see the status and problem body a bounded search
// produces. limit < 0 omits the param entirely (the "client omitted it" case).
func (c *Client) SyncSearchRawLimit(t *testing.T, modelName string, modelVersion int, condition string, limit int) (int, []byte, error) {
	t.Helper()
	path := fmt.Sprintf("/api/search/direct/%s/%d", modelName, modelVersion)
	if limit >= 0 {
		path += "?limit=" + strconv.Itoa(limit)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, c.baseURL+path, strings.NewReader(condition))
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("transport: %w", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, raw, nil
}
```

- [ ] **Step 2: Rewrite the scenario**

Replace `RunSearchOmittedLimitDefaults1000` (`search.go:262-288`) wholesale:

```go
// RunSearchDirectBoundedOrFail asserts the bounded-or-fail contract on every
// backend's direct-search path: the limit is a cap on the matched set, not a
// page size. A matched set larger than the limit is a 400, never a truncated
// prefix. The omitted-limit case doubles as proof that the documented default
// is still 1000.
func RunSearchDirectBoundedOrFail(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)
	const modelName = "parity-search-bounded-or-fail"
	const modelVersion = 1
	setupSearchModel(t, c, modelName, modelVersion)

	// 1001 matching entities: one more than the documented default of 1000.
	for i := 0; i < 1001; i++ {
		if _, err := c.CreateEntity(t, modelName, modelVersion,
			fmt.Sprintf(`{"name":"n%d","amount":1,"status":"new"}`, i)); err != nil {
			t.Fatalf("CreateEntity %d: %v", i, err)
		}
	}

	cond := `{"type":"simple","jsonPath":"$.status","operatorType":"EQUALS","value":"new"}`

	// Omitted limit → the 1000 default applies, and 1001 matches exceed it.
	status, body, err := c.SyncSearchRawLimit(t, modelName, modelVersion, cond, -1)
	if err != nil {
		t.Fatalf("SyncSearch (omitted limit): %v", err)
	}
	if status != http.StatusBadRequest {
		t.Errorf("omitted limit: got status %d, want 400; body=%s", status, body)
	}
	if code := errorCodeOf(body); code != "SEARCH_RESULT_LIMIT" {
		t.Errorf("omitted limit: got errorCode %q, want SEARCH_RESULT_LIMIT; body=%s", code, body)
	}

	// Explicit limit one short of the match count → same outcome.
	status, body, err = c.SyncSearchRawLimit(t, modelName, modelVersion, cond, 1000)
	if err != nil {
		t.Fatalf("SyncSearch (limit=1000): %v", err)
	}
	if status != http.StatusBadRequest {
		t.Errorf("limit=1000: got status %d, want 400; body=%s", status, body)
	}

	// Exactly at the match count → the whole set comes back.
	results, err := c.SyncSearchSortedLimit(t, modelName, modelVersion, cond, nil, 1001)
	if err != nil {
		t.Fatalf("SyncSearch (limit=1001): %v", err)
	}
	if len(results) != 1001 {
		t.Errorf("limit=1001: got %d results, want 1001", len(results))
	}

	// limit=0 means unbounded at the SPI, so the transport must reject it
	// rather than hand out an unbounded synchronous search.
	status, body, err = c.SyncSearchRawLimit(t, modelName, modelVersion, cond, 0)
	if err != nil {
		t.Fatalf("SyncSearch (limit=0): %v", err)
	}
	if status != http.StatusBadRequest {
		t.Errorf("limit=0: got status %d, want 400; body=%s", status, body)
	}
	if code := errorCodeOf(body); code != "BAD_REQUEST" {
		t.Errorf("limit=0: got errorCode %q, want BAD_REQUEST; body=%s", code, body)
	}
}
```

If `e2e/parity` has no `errorCodeOf` helper for RFC 9457 bodies, add one next to the scenario mirroring `internal/e2e`'s `problemErrorCode` (`callback_txjoin_errors_test.go:38`).

- [ ] **Step 3: Update the registry**

`registry.go:114`:

```go
	{"SearchDirectBoundedOrFail", RunSearchDirectBoundedOrFail},
```

The entry count is unchanged at 218, so `registry_count_test.go:9` stays as it is.

- [ ] **Step 4: Run parity on all three backends**

```bash
go test ./e2e/parity/memory/... -run 'SearchDirectBoundedOrFail' -v
go test ./e2e/parity/sqlite/... -run 'SearchDirectBoundedOrFail' -v
go test ./e2e/parity/postgres/... -run 'SearchDirectBoundedOrFail' -v
```

Expected: PASS on all three.

- [ ] **Step 5: Commit**

```bash
git add e2e/parity/search.go e2e/parity/registry.go e2e/parity/client/http.go
git commit -m "test(parity): assert bounded-or-fail direct search on every backend

SearchOmittedLimitDefaults1000 encoded top-N truncation as the expected
cross-backend behaviour; it becomes SearchDirectBoundedOrFail, covering
the omitted-limit default, an under-count limit, an exact-count limit,
and the rejected limit=0."
```

---

### Task 12: documentation

**Files:**
- Modify: `cmd/cyoda/help/content/errors/SEARCH_RESULT_LIMIT.md:24-26`
- Modify: `cmd/cyoda/help/content/search.md:45,165,314`
- Modify: `api/openapi.yaml` (searchEntities description + `limit` schema)
- Modify: `CHANGELOG.md`
- Modify: `COMPATIBILITY.md`
- Create: `docs/cloud-parity/direct-search-bounded-or-fail.md`

**Interfaces:**
- Consumes: the shipped behaviour from Tasks 4-11.
- Produces: nothing consumed downstream.

No new error codes, so no `errors/<CODE>.md` topic is added and `TestErrCode_Parity` is unaffected. Keep every one of these edits free of issue numbers.

- [ ] **Step 1: Fix the error-topic remedy**

`SEARCH_RESULT_LIMIT.md` currently advises a *smaller* `pageSize`, which now makes failure **more** likely. Replace the DESCRIPTION body:

```markdown
## DESCRIPTION

Direct (synchronous) search is bounded-or-fail: `limit` caps the matched result set rather than paging it. When more entities match than the limit allows, the request is rejected — it never returns a truncated prefix, because a partial result would be indistinguishable from a complete one.

Also returned when the requested `limit` itself exceeds the server maximum.

Not retryable with the same parameters. Narrow the condition, raise `limit` (up to the documented maximum), or use async search, which snapshots the full result set and pages over it.
```

- [ ] **Step 2: Rewrite the search help topic**

`search.md:45` — replace "The default result limit is 1000 entities per request; the maximum is 10000" with a sentence stating that `limit` bounds the matched set, that exceeding it returns `400 SEARCH_RESULT_LIMIT`, that the default when omitted is 1000, the maximum 10000, and that values below 1 are rejected.

`search.md:165` — the `limit` parameter line gains the lower bound: "string-encoded integer, minimum 1, maximum 10000; default 1000".

`search.md:314` — replace "Synchronous search does not paginate; use the `limit` parameter (maximum 10000; above rejects `400`) to bound results. For large datasets, use async search with page retrieval." with text stating that synchronous search neither paginates nor truncates: the matched set must fit within `limit` or the request fails, and any result set larger than that — including an ordered top-N over a large model — belongs on the async path.

- [ ] **Step 3: Update OpenAPI**

In `api/openapi.yaml`, the `searchEntities` description states the bounded-or-fail contract and the accepted `limit` range. The `limit` parameter is `type: string`, so its `maximum: 10000` is inert — replace it with a `pattern` that admits only positive integers and a description carrying the real bounds. Then:

```bash
go generate ./api/... && ./scripts/check-generated-in-sync.sh
```

- [ ] **Step 4: Add the CHANGELOG entry**

Under `## [Unreleased]`, in a `### Changed` block (create it if absent — the existing entries are under `### Added`):

```markdown
- **Direct search is bounded-or-fail on every backend (breaking).** A synchronous
  search whose matched set exceeds the effective `limit` now returns
  `400 SEARCH_RESULT_LIMIT` instead of silently truncating to the first `limit`
  results. The default when `limit` is omitted is unchanged at 1000, so a query
  matching more than 1000 entities that previously returned a truncated page now
  fails. Narrow the condition, raise `limit` (maximum 10000), or use async
  search, which snapshots and pages the full result set. Ordered top-N
  (`sort` + a small `limit`) is no longer available on the synchronous path —
  async search covers it. `limit=0`, which previously yielded an *unbounded*
  synchronous search, is now rejected with `400 BAD_REQUEST`; gRPC rejects
  `limit < 1` for the same reason. A conditional delete that hits a classified
  4xx while selecting its victim set now surfaces that error instead of an
  opaque `500`.
```

- [ ] **Step 5: Update COMPATIBILITY.md**

Add the new `cyoda-go-spi` version to the matrix with a note that it is a breaking SPI change (`SearchOptions.Offset` removed, `MergePage` → `MergeBounded`), so out-of-tree plugins must be rebuilt against it.

- [ ] **Step 6: Write the cloud-parity note**

Create `docs/cloud-parity/direct-search-bounded-or-fail.md` following the shape of the existing files in that directory. Record: the contract cyoda-go now defines (`Limit > 0` caps the matched set and fails; `Limit <= 0` is unbounded and must not be re-defaulted by a plugin), the `Offset` removal and `MergeBounded` rename, and the fact that the commercial backend already implemented bounded-or-fail but re-defaults a non-positive limit, which its own tracked issue resolves.

- [ ] **Step 7: Verify the help tree still builds**

```bash
go test ./cmd/cyoda/... -v
```

Expected: PASS, including `TestErrCode_Parity`.

- [ ] **Step 8: Commit**

```bash
git add cmd/cyoda/help/content/errors/SEARCH_RESULT_LIMIT.md cmd/cyoda/help/content/search.md api/openapi.yaml api/generated.go CHANGELOG.md COMPATIBILITY.md docs/cloud-parity/direct-search-bounded-or-fail.md
git commit -m "docs(search): document bounded-or-fail direct search

The SEARCH_RESULT_LIMIT remedy advised a smaller page size, which now
makes failure more likely. Help, OpenAPI, CHANGELOG, COMPATIBILITY and a
cloud-parity note all restate limit as a cap on the matched set."
```

---

### Task 13: pin the SPI, verify everything, open the PR

**Files:**
- Modify: `go.mod`, `plugins/memory/go.mod`, `plugins/sqlite/go.mod`, `plugins/postgres/go.mod`
- Modify: `go.work` (local only — the `use` line is removed, never committed)

**Interfaces:**
- Consumes: all prior tasks.
- Produces: a green branch pinned to a pushed SPI pseudo-version.

- [ ] **Step 1: Push the SPI branch**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git push -u origin feat/437-bounded-or-fail-search
git rev-parse --short HEAD
```

- [ ] **Step 2: Pseudo-pin the SPI in all four manifests**

Resolve the pseudo-version for the pushed HEAD, then apply it identically everywhere. `make check-spi-pin-sync` fails the build if the four disagree.

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/feat-437-bounded-or-fail-search
V=$(GOWORK=off GOFLAGS=-mod=mod go list -m -f '{{.Version}}' \
      github.com/cyoda-platform/cyoda-go-spi@feat/437-bounded-or-fail-search)
echo "pinning $V"
for m in . plugins/memory plugins/sqlite plugins/postgres; do
  (cd $m && go mod edit -require="github.com/cyoda-platform/cyoda-go-spi@$V")
done
```

- [ ] **Step 3: Drop the local `go.work` composition and refresh checksums**

```bash
go work edit -dropuse /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git update-index --no-skip-worktree go.work
git diff --exit-code go.work   # must be clean: the use line was never committed
for m in . plugins/memory plugins/sqlite plugins/postgres; do (cd $m && GOWORK=off go mod tidy); done
make check-spi-pin-sync
```

`git diff --exit-code go.work` must print nothing. If it shows an absolute path, the local `use` line leaked into the index — remove it before going further, or CI will fail on a path that exists only on this machine.

- [ ] **Step 4: Full verification**

```bash
go vet ./...
make test-all
make race
```

`make test-all` covers the root module plus all three plugin submodules (Docker required for the postgres testcontainers). `make race` is the single race pass for this deliverable — the first and only one.

Expected: all green. Do not proceed while anything is red.

- [ ] **Step 5: Commit the pins**

```bash
git add go.mod go.sum plugins/memory/go.mod plugins/memory/go.sum plugins/sqlite/go.mod plugins/sqlite/go.sum plugins/postgres/go.mod plugins/postgres/go.sum
git commit -m "chore: pseudo-pin cyoda-go-spi to the bounded-or-fail branch

Coordinated-release window pin across the root and all three plugin
submodules; the real tag lands at milestone end."
```

Stage these paths explicitly. Never `git add -A` — `go.work` is tracked and an accidental absolute path in it breaks CI.

- [ ] **Step 6: Open the PR against the release branch**

This is milestone v0.8.3 work, so the base is `release/v0.8.3`, **not** `main`. Confirm before creating:

```bash
git merge-base --is-ancestor origin/release/v0.8.3 HEAD && echo "base OK"
gh pr create --base release/v0.8.3 --title "feat(search)!: bounded-or-fail direct search on every backend (#437)" --body "..."
```

The PR body must state: `Closes #437`; that the SPI change is breaking and therefore needs a **minor** SPI tag (`v0.9.0`, not `v0.8.3`) per `MAINTAINING.md`; and links to the two filed follow-ups (cyoda-go-cassandra#79, cyoda-go#444). Apply the `v0.8.3` milestone to #437 — un-milestoned issues closed by a release-branch PR are invisible to the release notes.

---

## Self-Review

**Spec coverage.** Spec §2 rule → Tasks 2, 4-7. §2.1 top-N loss → Task 12 CHANGELOG + `search.md`. §2.2 offset removal → Tasks 1, 2, 4-7 (domain), 11 (tests). §3 SPI table → Tasks 1-3. §3.1 `Limit == 0` ambiguity → Task 2 doc + the filed cassandra issue. §4 enforcement sites → Tasks 4-7. §4.1 scan budget → Task 5 doc note; the divergence itself is the filed cyoda-go issue. §4.2 fallback path → Task 7. §5 transport validation → Task 8. §6 error tables → Tasks 8, 9, 10, 11. §7 coverage matrix → Tasks 3, 4-6, 7-11. §7.1 parity → Task 11. §7.2 broken tests → Tasks 1, 4, 5, 6, 7. §7.3 read-set → Tasks 4, 5, 6. §8 docs → Task 12; cross-repo issues already filed. §9 out of scope → nothing to build.

**Naming consistency.** `MergeBounded` is defined in Task 1 and called in Tasks 4 (memory) and 5 (sqlite); postgres never calls it, stated in Task 6. `matchSortBounded` is defined and called only in Task 4. `SyncSearchRawLimit` is defined and used only in Task 11. `RunSearchDirectBoundedOrFail` is defined in Task 11 step 2 and registered in step 3 under the matching name `SearchDirectBoundedOrFail`.

**Known softness.** Three test sketches name helpers that must be read out of the existing test files rather than invented — Task 3 (`runSubtest` signature, `spi.Filter` field and operator constant names), Tasks 4-6 (each plugin's `newSearchStore` / `seedMatching` / `beginTx` equivalents), and Task 10's in-tx case (the callback harness). Each step says so explicitly and names the file to read. This is deliberate: inventing helper names that collide with or shadow the real ones costs more than a lookup.
