# #477 Plan 3 of 3 — Engine routing, 501 retirement, pin bump Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

> ## STATUS 2026-09-03 — Task 1 is DONE. Start at Task 2.
>
> Plan 2 merged to `cyoda-go-spi` `main` as `1d3b6ed`. Work continues on branch
> `fix/477-search-fallback` (pushed). Already committed here:
>
> - **Task 1 complete.** `ce9f8c6` pins all four `go.mod` files at
>   `v0.8.4-0.20260903130721-1d3b6ed501f0` and re-pins the plugin submodules to
>   this branch's HEAD. `make check-spi-pin-sync` passes. The plugin removals
>   (`e2a55a3`) are in, and all three plugins build, vet and test clean both in
>   workspace mode and under `GOWORK=off`.
> - **Task 6's plugin half is complete** (`e8db6d1`): each plugin has a
>   `drainAll` helper and no plugin test calls `GetAll`/`GetAllAsAt`. What
>   remains of Task 6 is the `internal/`, `internal/e2e/` and
>   `e2e/parity/txsearchryw` sites plus `internal/common/commontest/drain.go`.
> - **Beyond the plan**, three defects the conformance run exposed are fixed
>   here: `Search` checked `RolledBack` but not `Closed` on memory and sqlite
>   (`d9de052`); a postgres store op on a committed transaction wrapped no SPI
>   sentinel (`c7c77e7`, `3c37808`); and @pschleger ruled that `CompareAndSave`
>   **rejects an empty `expectedTxID`** rather than reading it as "expect no
>   entity" (`2818f96`, `53a781a`, `36fe3e5`, `061e335`, `33ab0c3`) — which also
>   made postgres's compare-and-save advisory lock dead code. Every postgres
>   test's transaction now goes through one `beginGuarded` helper
>   (`c3b9418`, `f84f775`, `5620ea4`).
>
> **The root module does not compile.** That is this plan's intended starting
> state: `internal/domain/search/service.go` and
> `e2e/parity/txsearchryw/tx_search_ryw_test.go` are the genuine failures;
> everything else cascades from importing the broken search package, so expect
> more once `service.go` is fixed.
>
> **Two findings Tasks 2-7 must act on**, both written up on Cyoda/cyoda-go#516
> under *Reference: the compare-and-save empty-ID ruling*:
> 1. `internal/domain/workflow/fire_scheduled.go:381,473` is the only engine
>    caller deriving `expectedTxID` from stored data rather than a client
>    `If-Match`. With the empty ID now rejected it can return an error where it
>    used to overwrite silently. Failing closed is right; guard it deliberately.
> 2. memory and sqlite overwrite `Meta.TransactionID` at commit while postgres
>    stamps only when the caller left it empty, and memory/sqlite compare an
>    in-transaction compare-and-save against the *buffered* entity's raw ID.
>    Latent, since callers stamp — but the escape hatch that hid it is gone.

**Goal:** Direct search has one path (`EntityStore.Search`), the whole-model fallback and every "store lacks the capability" branch are gone, the async executor fails closed, the 501 grouped-stats code is retired, and all four `go.mod` files pin the SPI commit Plan 2 merged.

**Architecture:** The pin bump makes the compiler drive the removal: `GetAll`/`GetAllAsAt` no longer exist, `Search`/`Iterate` are plain method calls. The search service validates a nil condition and a non-positive limit up front, classifies a translation failure, and calls `store.Search`. Five refusal branches, `drainIterate`, the in-memory fallback, the `Searcher`/`Iterable` assertions, `ErrBackendNotSupported` and `NOT_IMPLEMENTED_BY_BACKEND` are deleted. Tests that read a model with `GetAll` move to an `Iterate` drain helper.

**Tech Stack:** Go 1.26; `cyoda-go-spi` at the Plan-2 merge commit (pseudo-version computed in Task 1); plugins re-pinned with `make repin-plugins`.

**Spec:** `docs/superpowers/specs/2026-09-01-477-no-search-path-materialises-design.md` — §2, §4.7, §5, §6, §8 PR 3, §9, §10.

## Global Constraints

- Branch `fix/477-search-fallback` (this worktree), PR to `release/v0.8.4`, milestone `v0.8.4`. Plans 1 and 2 are merged first; rebase onto `origin/release/v0.8.4` before starting.
- Pin procedure (`.claude` memory + SPI `MAINTAINING.md`): ONE commit bumps the SPI pin in root + three plugin `go.mod`s; `make check-spi-pin-sync` must pass; `make repin-plugins` after plugin code changes; never a committed `replace`, never a committed `go.work` `use` line for the SPI.
- No new error code. No issue IDs in shipped artefacts. `log/slog` only. TDD per task.
- `make test` while iterating; `make test-full` and `make race` once at the end.
- Commit trailer:
  ```
  Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01C8cfZpHnEMh9rXna94SYGf
  ```

---

## File map

| File | Change |
|---|---|
| `go.mod`, `plugins/{memory,sqlite,postgres}/go.mod` (+ `go.sum`s) | SPI pin → Plan-2 merge pseudo-version (Task 1) |
| `plugins/memory/entity_store.go`, `plugins/sqlite/entity_store.go`, `plugins/postgres/entity_store.go` | delete `GetAll`/`GetAllAsAt`; memory also deletes `getAllSnapshotUnlocked` (Task 1) |
| `plugins/*/searcher.go`, `plugins/*/grouped_stats.go` | `var _ spi.Searcher`/`spi.Iterable` assertions → `var _ spi.EntityStore` (Task 1) |
| `internal/domain/search/service.go` | `Search` contract + one-path routing; fallback, `drainIterate`, `cond == nil` deleted; async executor branch deleted; `SubmitAsync` dry-run (Tasks 2–4) |
| `internal/domain/entity/service.go`, `internal/domain/entity/grouped_stats_service.go` | five refusal branches deleted; `ErrBackendNotSupported` deleted; `QueryGroupedStats` store param typed (Task 5) |
| `internal/common/error_codes.go`, `cmd/cyoda/help/content/errors/NOT_IMPLEMENTED_BY_BACKEND.md`, `errors.md`, `crud.md`, `api/openapi.yaml`, `internal/e2e/zzz_errorcode_matrix_test.go` | 501 code retired (Task 5) |
| `internal/testing/spitesthelpers` (new) or `internal/common/commontest` | `DrainAll(t, store, ctx, ref)` test helper (Task 6) |
| 19 test files | `GetAll` → helper; fallback tests deleted; doubles updated (Task 6) |
| `internal/e2e/search_test.go` | `TestSearchSort_PushdownFallbackAgree` deleted (Task 6) |
| docs (§8 list), `CHANGELOG.md`, `COMPATIBILITY.md`, `docs/cloud-parity/grouped-stats-iterate-required.md` (new), `docs/cloud-parity/README.md` | Task 7 |

---

### Task 1: Pin the new SPI and let the compiler drive the plugin removals

**DONE — `ce9f8c6` (pin + repin) and `e2a55a3` (plugin removals). Skip to Task 2.**

**Files:**
- Modify: four `go.mod` + `go.sum`; `plugins/memory/entity_store.go:447-545` (`GetAll`, `GetAllAsAt`), `:118-160` (`getAllSnapshotUnlocked`); `plugins/sqlite/entity_store.go:532-571` (`GetAll`), `:632-…` (`GetAllAsAt`); `plugins/postgres/entity_store.go:252-300` (`GetAll`, `GetAllAsAt`); `plugins/memory/searcher.go:12`, `plugins/sqlite/searcher.go:13`, `plugins/postgres/searcher.go:14`, `plugins/{memory,sqlite,postgres}/grouped_stats.go` compile-time assertions.

- [x] **Step 1: Bump the pin (one commit's worth, not yet committed)**

```bash
SHA=$(git -C /Users/paul/go-projects/cyoda-light/cyoda-go-spi rev-parse origin/main)   # Plan 2's merge commit
for d in . plugins/memory plugins/sqlite plugins/postgres; do (cd $d && go get github.com/cyoda-platform/cyoda-go-spi@$SHA); done
make check-spi-pin-sync
go build ./... 2>&1 | head -30
```

Expected: `check-spi-pin-sync` PASS; the build FAILS on every `GetAll`/`GetAllAsAt` definition-without-interface-use (plugins compile — an extra method is legal — but engine call sites at `internal/domain/search/service.go:736-738` fail: `store.GetAll undefined`) and on `spi.Iterable`/`spi.Searcher` references. These compile errors are the failing test for this task.

- [x] **Step 2: Delete the plugin methods and fix the assertions**

memory: delete `GetAll` (`entity_store.go:447-505`), `GetAllAsAt` (`:542-…`), `getAllSnapshotUnlocked` (`:118-160`); `searcher.go:12` becomes `var _ spi.EntityStore = (*EntityStore)(nil)`; `grouped_stats.go` `var _ spi.Iterable = …` → delete (the `EntityStore` assertion covers it), keep `var _ spi.GroupedAggregator`.
sqlite: delete `GetAll` (`entity_store.go:532-571`, including `getAllDirect`) and `GetAllAsAt`; `searcher.go:13` → `var _ spi.EntityStore = (*entityStore)(nil)`; `grouped_stats.go:43-46` drop the `Iterable` line.
postgres: delete `GetAll` (`entity_store.go:252-276`) and `GetAllAsAt` (`:277-…`); assertions likewise.

Run: `for p in memory sqlite postgres; do (cd plugins/$p && go build ./... && go vet ./...); done`
Expected: PASS. Plugin tests will not compile yet (test call sites use `GetAll`); Task 6 migrates them. Root build still fails on the engine — Tasks 2–5.

- [x] **Step 3: Commit the pin and the plugin removals together**

```bash
git add go.mod go.sum plugins/*/go.mod plugins/*/go.sum plugins/memory/entity_store.go plugins/memory/searcher.go plugins/memory/grouped_stats.go plugins/sqlite/entity_store.go plugins/sqlite/searcher.go plugins/sqlite/grouped_stats.go plugins/postgres/entity_store.go plugins/postgres/searcher.go plugins/postgres/grouped_stats.go
git commit -m "build(spi)!: pin cyoda-go-spi@<sha>; plugins lose GetAll/GetAllAsAt, Search and Iterate are EntityStore methods"
```

(Stage explicitly; `go.work` must stay untouched.)

---

### Task 2: `Search` — contract checks and one-path routing

**Files:**
- Modify: `internal/domain/search/service.go:559-836` (`Search` doc + body), `:983-1021` (`drainIterate`, delete)
- Test: `internal/domain/search/search_contract_test.go` (new, `package search_test`)

**Interfaces:**
- Consumes: `search.NewSearchService(factory, uuids, searchStore)`, fixtures in `service_test.go` (`tenantCtx`, `saveModelWithValAndItemsArray`, `saveEntity`, `searcherEntityStore`/`searcherFactory` at `:833-865` — keep those; `nonSearcherEntityStore`/`nonSearcherFactory` at `:867-880` are deleted in Task 6), `search.ValidateCondition`, `predicate.Condition` (one method: `Type() string`).
- Produces: `Search` errors: `Limit <= 0` → plain `error` "search: limit must be >= 1"; nil condition → `400 INVALID_CONDITION`; translation failure → `ClassifyStoreQueryError` then `400 INVALID_CONDITION`.

- [ ] **Step 1: Write the failing tests**

```go
package search_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

func newContractFixture(t *testing.T) (*search.SearchService, context.Context, spi.ModelRef) {
	t.Helper()
	base := memory.NewStoreFactory()
	t.Cleanup(func() { base.Close() })
	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "item", ModelVersion: "1"}
	saveModelWithValAndItemsArray(t, ctx, base, ref)
	saveEntity(t, ctx, base, ref, "e0", []byte(`{"val":0}`))
	searchStore, _ := base.AsyncSearchStore(context.Background())
	return search.NewSearchService(base, common.NewTestUUIDGenerator(), searchStore), ctx, ref
}

// Limit <= 0 is a caller contract violation, not a client-visible status:
// both transports resolve a positive limit before reaching the service.
func TestSearch_NonPositiveLimit_IsContractError(t *testing.T) {
	svc, ctx, ref := newContractFixture(t)
	cond := &predicate.SimpleCondition{JsonPath: "$.val", OperatorType: "EQUALS", Value: 0}
	for _, limit := range []int{0, -1} {
		_, err := svc.Search(ctx, ref, cond, search.SearchOptions{Limit: limit})
		if err == nil {
			t.Fatalf("limit %d: expected an error", limit)
		}
		var appErr *common.AppError
		if errors.As(err, &appErr) {
			t.Fatalf("limit %d: got an AppError (%d %s); a contract violation is a plain error", limit, appErr.Status, appErr.Code)
		}
	}
}

// A nil condition is rejected at the validation boundary.
func TestSearch_NilCondition_Is400InvalidCondition(t *testing.T) {
	svc, ctx, ref := newContractFixture(t)
	_, err := svc.Search(ctx, ref, nil, search.SearchOptions{Limit: 10})
	var appErr *common.AppError
	if !errors.As(err, &appErr) || appErr.Status != http.StatusBadRequest || appErr.Code != common.ErrCodeInvalidCondition {
		t.Fatalf("got %v, want 400 %s", err, common.ErrCodeInvalidCondition)
	}
}

// unknownCondition is a predicate.Condition outside the five wire types:
// validation's type switch accepts what it does not recognise, translation
// rejects it. This is the only way a translation failure is reachable.
type unknownCondition struct{}

func (unknownCondition) Type() string { return "caller-built" }

func TestSearch_TranslationFailure_Is400InvalidCondition(t *testing.T) {
	svc, ctx, ref := newContractFixture(t)
	_, err := svc.Search(ctx, ref, unknownCondition{}, search.SearchOptions{Limit: 10})
	var appErr *common.AppError
	if !errors.As(err, &appErr) || appErr.Status != http.StatusBadRequest || appErr.Code != common.ErrCodeInvalidCondition {
		t.Fatalf("got %v, want 400 %s", err, common.ErrCodeInvalidCondition)
	}
}

// The path-shaped leg of translation failure cannot be reached through
// Search (validation and translation share the grammar); the classifier's
// mapping of the wrapped sentinel is pinned directly.
func TestClassifyStoreQueryError_WrappedInvalidFilterPath_IsInvalidFieldPath(t *testing.T) {
	err := fmt.Errorf("translate: %w", spi.ErrInvalidFilterPath)
	appErr := search.ClassifyStoreQueryError(err)
	if appErr == nil || appErr.Status != http.StatusBadRequest || appErr.Code != common.ErrCodeInvalidFieldPath {
		t.Fatalf("got %v, want 400 %s", appErr, common.ErrCodeInvalidFieldPath)
	}
}
```

(add `"fmt"` to the imports.)

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/domain/search/ -run 'TestSearch_NonPositiveLimit|TestSearch_NilCondition|TestSearch_TranslationFailure|TestClassifyStoreQueryError_Wrapped'`
Expected: the package does not compile yet (Task 1 removed `GetAll`) — that is the red. After Step 3, the first three would fail on behaviour if the old code were restored; the fourth passes already (keep it).

- [ ] **Step 3: Rewrite `Search`**

Replace the doc comment (`:559-575`) with:

```go
// Search performs a synchronous, bounded-or-fail entity search.
//
// Contract: opts.Limit >= 1 (a non-positive limit is a caller error, not a
// client status — both transports resolve a positive limit first); cond is
// non-nil (a nil condition is 400 INVALID_CONDITION). The condition is
// validated structurally, against the model's paths and declared types,
// and for pattern operands, then translated to spi.Filter and pushed to
// EntityStore.Search. A translation failure — unreachable from validated
// input, reachable only with a caller-built condition type — is classified
// like any store rejection (a path-shaped failure is INVALID_FIELD_PATH) and
// otherwise 400 INVALID_CONDITION. There is one path: the store's Search.
// No whole-model read exists anywhere in the engine.
//
// Pre-execution path validation: every condition path is checked against
// the cached model schema's FieldsMap. When a path is unknown, the schema
// cache is refreshed exactly once via RefreshAndGet so a search referencing
// a peer's freshly-extended path succeeds after one authoritative read.
// Truly-unknown paths surface as 400 INVALID_FIELD_PATH. Unregistered
// models surface as 404 MODEL_NOT_FOUND.
```

At the top of the body, before the existing model lookup, add:

```go
	if opts.Limit <= 0 {
		return nil, fmt.Errorf("search: limit must be >= 1, got %d", opts.Limit)
	}
	if cond == nil {
		return nil, common.Operational(http.StatusBadRequest, common.ErrCodeInvalidCondition,
			"condition is required")
	}
```

Replace everything from the comment block "Delegate to the plugin for predicate pushdown…" (`:627`) through the end of the function (`:836`) with:

```go
	// One path: translate, push down. Every backend implements Search;
	// there is no capability ladder and no in-process fallback.
	filter, translateErr := spi.ConditionToFilter(cond, validatedFields)
	if translateErr != nil {
		if appErr := ClassifyStoreQueryError(translateErr); appErr != nil {
			return nil, appErr
		}
		return nil, common.Operational(http.StatusBadRequest, common.ErrCodeInvalidCondition,
			fmt.Sprintf("condition cannot be translated: %v", translateErr))
	}
	res, sErr := store.Search(ctx, filter, spi.SearchOptions{
		ModelName:    modelRef.EntityName,
		ModelVersion: modelRef.ModelVersion,
		PointInTime:  opts.PointInTime,
		Limit:        opts.Limit,
		OrderBy:      orderBy,
		TrackingRead: opts.TrackingRead,
	})
	if appErr := ClassifyStoreQueryError(sErr); appErr != nil {
		return nil, appErr
	}
	return res, sErr
}
```

Delete `drainIterate` (`:983-1021`) and the `sortEntities` call sites that only the fallback used (`sortEntities` itself stays: `ordersort.go` is used by tests and the `Iterate` rung no longer exists — check `grep -n "sortEntities(" internal/domain/search/*.go`; if only tests reference it, delete it and its test). Remove the now-unused `match` import if `match.Prepare` has no other caller in the file (`ClassifyStoreQueryError` still references `match.ErrUnevaluableLeaf`, so the import stays). Update the comment at `internal/domain/entity/grouped_stats_service.go:351` ("Mirrors drainIterate's ordering") to "Err() is read after Close(): some iterators surface a sticky scan error only at Close".

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/domain/search/ -run 'TestSearch_NonPositiveLimit|TestSearch_NilCondition|TestSearch_TranslationFailure|TestClassifyStoreQueryError_Wrapped'`
Expected: PASS once the package compiles — Task 6 fixes the remaining test files; if the package still fails to compile because of them, temporarily run with `-run` after Task 6 and record that here.

- [ ] **Step 5: Commit**

```bash
git add internal/domain/search/service.go internal/domain/search/search_contract_test.go internal/domain/entity/grouped_stats_service.go
git commit -m "feat(search)!: one search path — Search requires a limit and a condition, classifies a translation failure, pushes down"
```

---

### Task 3: Async executor fails closed; submit dry-runs translation

**Files:**
- Modify: `internal/domain/search/service.go:1054-1062` (`SubmitAsync` path validation), `:1305-1360` (`runAsyncJob` translate branch and `Iterable` assertion)
- Test: `internal/domain/search/async_submit_translation_test.go` (new)

- [ ] **Step 1: Write the failing test**

```go
package search_test

import (
	"errors"
	"net/http"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
)

// SubmitAsync translates the condition before persisting the job, so a
// condition that cannot be translated is refused at submission.
func TestSubmitAsync_TranslationFailure_Is400(t *testing.T) {
	svc, ctx, ref := newContractFixture(t)
	_, err := svc.SubmitAsync(ctx, ref, unknownCondition{}, search.SearchOptions{})
	var appErr *common.AppError
	if !errors.As(err, &appErr) || appErr.Status != http.StatusBadRequest || appErr.Code != common.ErrCodeInvalidCondition {
		t.Fatalf("got %v, want 400 %s", err, common.ErrCodeInvalidCondition)
	}
}

func TestSubmitAsync_NilCondition_Is400(t *testing.T) {
	svc, ctx, ref := newContractFixture(t)
	_, err := svc.SubmitAsync(ctx, ref, nil, search.SearchOptions{})
	var appErr *common.AppError
	if !errors.As(err, &appErr) || appErr.Status != http.StatusBadRequest || appErr.Code != common.ErrCodeInvalidCondition {
		t.Fatalf("got %v, want 400 %s", err, common.ErrCodeInvalidCondition)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/domain/search/ -run 'TestSubmitAsync_TranslationFailure|TestSubmitAsync_NilCondition'`
Expected: FAIL — today `SubmitAsync` accepts both (the unknown type clears validation and is persisted; nil reaches `ValidateCondition`, which accepts it).

- [ ] **Step 3: Implement**

In `SubmitAsync`, at the top of the validation sequence (before `ValidateCondition`):

```go
	if cond == nil {
		return "", common.Operational(http.StatusBadRequest, common.ErrCodeInvalidCondition,
			"condition is required")
	}
```

Replace `if _, vErr := s.validateConditionPaths(ctx, modelStore, modelRef, cond); vErr != nil {` (`:1057`) with:

```go
	validatedFields, vErr := s.validateConditionPaths(ctx, modelStore, modelRef, cond)
	if vErr != nil {
		return "", vErr
	}
```

and, after the pattern and type validation that follow it, add the dry-run:

```go
	// Translate once at submission: a persisted job carries a condition that
	// translates. (The result is discarded; the executor translates against
	// the schema as it stands at execution, which can only have grown.)
	if _, translateErr := spi.ConditionToFilter(cond, validatedFields); translateErr != nil {
		if appErr := ClassifyStoreQueryError(translateErr); appErr != nil {
			return "", appErr
		}
		return "", common.Operational(http.StatusBadRequest, common.ErrCodeInvalidCondition,
			fmt.Sprintf("condition cannot be translated: %v", translateErr))
	}
```

In `runAsyncJob`, replace the block from `filter, translateErr := spi.ConditionToFilter(cond, fields)` through the end of the `else {` that opens the `Iterable` branch (`:1305-1346`) so that the executor has no translate-failure branch and no capability check:

```go
	filter, err := spi.ConditionToFilter(cond, fields)
	if err != nil {
		// Ordinary error handling, not a designed branch: a condition that
		// translated at submission translates here (schema changes after
		// locking are additive). Anything else is an unexpected failure.
		s.writeAsyncFailure(jobCtx, jobID, jobFailureMessage(common.Internal("failed to translate search condition", err)), time.Now(), time.Since(start).Milliseconds())
		return
	}

	var (
		count   int
		prodErr error
		saveErr error
	)

	entityStore, err := s.factory.EntityStore(jobCtx)
	if err != nil {
		s.writeAsyncFailure(jobCtx, jobID, jobFailureMessage(err), time.Now(), time.Since(start).Milliseconds())
		return
	}
```

and un-indent the former `else` body so `entityStore.Iterate(...)` (was `iterableStore.Iterate`) is called directly. Delete the `iterableStore, ok := entityStore.(spi.Iterable)` block (`:1337-1346`).

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/domain/search/ -run 'TestSubmitAsync_'`
Expected: PASS (subject to the package compiling; see Task 6).

- [ ] **Step 5: Commit**

```bash
git add internal/domain/search/service.go internal/domain/search/async_submit_translation_test.go
git commit -m "feat(search): async submit translates the condition up front; the executor has no fallback branch"
```

---

### Task 4: Delete the remaining refusal branches in delete paths

**Files:**
- Modify: `internal/domain/entity/service.go:835-842`, `:1228-1234`, `:1538-1545`; `drainDeleteSelection`'s parameter type (`:1166`) from `spi.Iterable` to `spi.EntityStore` (or drop the parameter if the store is already in scope).

- [ ] **Step 1: Replace each assertion**

At each of the three sites, delete the `iterableStore, ok := entityStore.(spi.Iterable) / if !ok {…}` block and use `entityStore` where `iterableStore` was used. Change `drainDeleteSelection(ctx context.Context, iterableStore spi.Iterable, …)` to take `store spi.EntityStore`.

Run: `go build ./internal/domain/entity/ && grep -n "spi.Iterable\|spi.Searcher" internal/domain/entity/*.go`
Expected: builds (modulo tests); grep finds only `grouped_stats_service.go` (Task 5).

- [ ] **Step 2: Run the package tests that already exist for delete**

Run: `go test ./internal/domain/entity/ -run 'Delete'`
Expected: PASS (after Task 6 compiles the test files; otherwise defer the run to Task 6 Step 5 and note it).

- [ ] **Step 3: Commit**

```bash
git add internal/domain/entity/service.go
git commit -m "refactor(entity): delete-all and conditional delete call Iterate directly; no capability refusal"
```

---

### Task 5: Grouped stats — no capability ladder; retire `NOT_IMPLEMENTED_BY_BACKEND`

**Files:**
- Modify: `internal/domain/entity/grouped_stats_service.go:21-24` (`ErrBackendNotSupported`), `:55-62` (`QueryGroupedStats` `store any` → `spi.EntityStore`), `:75-77` (501 arm), `:130-142` doc, `:279-285` (branches 2–3); `internal/common/error_codes.go:157-161`; delete `cmd/cyoda/help/content/errors/NOT_IMPLEMENTED_BY_BACKEND.md`; `cmd/cyoda/help/content/errors.md:97`; `cmd/cyoda/help/content/crud.md:541,579,656`; `api/openapi.yaml:1151,1161,1277-1280`; `internal/e2e/zzz_errorcode_matrix_test.go:61`; `internal/domain/entity/grouped_stats_service_test.go:199-205` (`TestQueryGroupedStats_501WhenNoCapability`, delete); `internal/domain/entity/grouped_stats_handler_test.go:115-135` (`TestGroupedStatsHandler_BackendNotSupportedReturns501`, delete); `fakeIterable` double gains the `EntityStore` methods or the test passes a real memory store.

- [ ] **Step 1: The failing test is `TestErrCode_Parity`**

Run: `go test ./cmd/cyoda/help/ -run TestErrCode_Parity`
Expected: PASS today; after deleting the constant without the topic (or vice versa) it FAILS — do both together in Step 2 and run it again.

- [ ] **Step 2: Retire the code everywhere**

- `grouped_stats_service.go`: delete `ErrBackendNotSupported` and its doc; delete the `case errors.Is(err, ErrBackendNotSupported)` arm; `QueryGroupedStats(ctx, store spi.EntityStore, …)` and `queryGroupedStatsInner` likewise; the body's step 2 becomes `return s.tallyStreaming(ctx, store, model, fields, req, pushFilter, pushable, parsedCond)` with `tallyStreaming`'s parameter typed `spi.EntityStore`; delete step 3. Rewrite the doc at `:130-142`: "1. `GroupedAggregator` pushdown when the store implements it and accepts the shape; 2. otherwise stream `Iterate` and tally."
- `error_codes.go`: delete `ErrCodeNotImplementedByBackend` and its comment.
- Help: `git rm cmd/cyoda/help/content/errors/NOT_IMPLEMENTED_BY_BACKEND.md`; delete the `errors.md:97` bullet; in `crud.md` delete the "**Backend capability.**" paragraph (`:541`), the `NOT_IMPLEMENTED_BY_BACKEND` bullet (`:579`), and the `; 501 NOT_IMPLEMENTED_BY_BACKEND when …` clause (`:656`).
- `api/openapi.yaml`: delete the sentence at `:1151-1152` ("Backends that do not implement … 501"), remove `NOT_IMPLEMENTED_BY_BACKEND` from the code list at `:1161`, delete the `"501":` response block at `:1277-1282` for `queryGroupedEntityStatisticsForModel`.
- `zzz_errorcode_matrix_test.go:61`: drop `NOT_IMPLEMENTED_BY_BACKEND` from the comment.
- Tests: delete the two 501 tests; make `fakeIterable` satisfy `spi.EntityStore` by embedding `spi.EntityStore` (nil) and keeping its `Iterate` — or replace its uses with a memory store seeded with the rows. Choose embedding: `type fakeIterable struct { spi.EntityStore; entities []*spi.Entity; … }`.

Run: `go test ./cmd/cyoda/help/ -run 'TestErrCode_Parity|TestContent' && go build ./... && go test ./internal/domain/entity/ -run GroupedStats`
Expected: PASS; the OpenAPI gate `go test ./internal/oasdiffcheck/` — removing a response code is a breaking API change by oasdiff's classification; the gate is expected to flag it. Record the waiver in the PR body (spec §5: the code has no trigger).

- [ ] **Step 3: Commit**

```bash
git add internal/domain/entity/grouped_stats_service.go internal/domain/entity/grouped_stats_service_test.go internal/domain/entity/grouped_stats_handler_test.go internal/common/error_codes.go cmd/cyoda/help/content/errors.md cmd/cyoda/help/content/crud.md api/openapi.yaml internal/e2e/zzz_errorcode_matrix_test.go
git rm cmd/cyoda/help/content/errors/NOT_IMPLEMENTED_BY_BACKEND.md
git commit -m "feat(stats)!: retire 501 NOT_IMPLEMENTED_BY_BACKEND — Iterate is required, the code has no trigger"
```

---

### Task 6: Test migration — the `GetAll` readers, the fallback tests, the doubles

**Files:**
- Create: `internal/common/commontest/drain.go` (helper; the package already holds test-only helpers shared across domain packages) — for plugin tests, an equivalent `drainAll` in each plugin's `_test.go` (plugins cannot import cyoda-go internals).
- Modify (root): `internal/domain/search/service_test.go` (delete `nonSearcherEntityStore`/`nonSearcherFactory` `:867-880`, the three `TestSearch_FallbackBranch*` tests `:2286-2530` incl. `newFallbackFixture`, and the one `GetAll` use), `internal/domain/search/fallback_typed_test.go` (delete `TestSearchFallback_TypeDirectedWithRegisteredModel`; rewrite `TestSearchFallback_FailsClosedOnGenuineModelLoadError` on the plain path without the wrapper), `internal/domain/search/handler_timeout_test.go:162` (delete the test), `internal/domain/search/classify_store_query_error_test.go:345` (delete the test; `:395` stays), `internal/domain/entity/service_unique_keys_test.go` (2), `internal/domain/entity/service_list_test.go` (delete `noWholeModelEntityStore`/`noWholeModelStoreFactory` and the test that uses them — the compiler proves the claim now), `internal/domain/entity/mock_store_test.go` (`failingEntityStore` gains `Search`/`Iterate` returning `s.err`), `internal/e2e/list_intx_readset_test.go:154-201` (control leg → full `GetPage` drain), `internal/e2e/search_test.go:748-857` (delete `TestSearchSort_PushdownFallbackAgree` and its doc), `e2e/parity/txsearchryw/tx_search_ryw_test.go:198-231` (oracle → `Iterate` drain).
- Modify (plugins): `plugins/memory/entity_store_test.go` (12), `searcher_test.go` (3), `pit_boundary_test.go`, `concurrency_test.go`, `concurrency_inreadops_test.go`, `model_store_test.go` (2 — check they are entity-store calls); `plugins/sqlite/searcher_tx_test.go` (2), `pit_boundary_test.go`; `plugins/postgres/entity_store_test.go` (6), `searcher_tx_test.go` (2), `pit_committed_only_test.go` (2), `pit_boundary_test.go`, `pit_acquire_bound_test.go`, `model_store_test.go` (4 — check).

- [ ] **Step 1: Write the helper**

`internal/common/commontest/drain.go`:

```go
package commontest

import (
	"context"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// DrainAll reads every entity of modelRef through Iterate with a zero-value
// filter — the streamed replacement for the whole-model read tests used to
// call. asAt nil reads the live view (in-transaction: the merged view).
func DrainAll(t *testing.T, ctx context.Context, store spi.EntityStore, modelRef spi.ModelRef, asAt *time.Time) []*spi.Entity {
	t.Helper()
	it, err := store.Iterate(ctx, modelRef, spi.Filter{}, spi.IterateOptions{PointInTime: asAt})
	if err != nil {
		t.Fatalf("Iterate(%s): %v", modelRef.EntityName, err)
	}
	var out []*spi.Entity
	for it.Next() {
		out = append(out, it.Entity())
	}
	if err := it.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := it.Err(); err != nil {
		t.Fatalf("Iterate(%s) Err: %v", modelRef.EntityName, err)
	}
	if out == nil {
		out = []*spi.Entity{}
	}
	return out
}
```

Each plugin gets the same function as `drainAll` in a `helpers_drain_test.go` (package `<plugin>_test`).

- [ ] **Step 2: Migrate the mechanical sites**

For every `x, err := store.GetAll(ctx, ref)` → `x := commontest.DrainAll(t, ctx, store, ref, nil)` (drop the error check); `GetAllAsAt(ctx, ref, asAt)` → `DrainAll(t, ctx, store, ref, &asAt)`. Where a test asserted on `GetAll`'s read-set recording specifically, replace with `GetPage` (records unconditionally) and say so in the test comment. The ordering of `DrainAll` is the backend's canonical ID order, which is stable; tests that sorted the result keep sorting.

Run: `go vet ./... && for p in memory sqlite postgres; do (cd plugins/$p && go vet ./...); done`
Expected: no `GetAll` references remain: `grep -rn "\.GetAll(ctx\|\.GetAll(txCtx\|GetAllAsAt(" --include='*_test.go' internal e2e plugins | grep -v ModelStore` is empty.

- [ ] **Step 3: The two non-mechanical sites**

`e2e/parity/txsearchryw/tx_search_ryw_test.go` `assertRYWOracle`: replace `all, err := store.GetAll(txCtx, personRef)` with a drain over `store.Iterate(txCtx, personRef, spi.Filter{}, spi.IterateOptions{})` (inline loop, since parity cannot import `internal/`). Update its doc: "Search must return exactly the id-set that an unfiltered in-tx Iterate + spi.Prepare(filter).Match produces".

`internal/e2e/list_intx_readset_test.go` control leg: replace the `GetAll` with a full drain through `GetPage` (page size 100, loop until a short page), which records every returned entity unconditionally — the same model-wide read-set the old whole-model read produced. Update the doc comment ("the PRE-NARROWING read — GetAll" → "a whole-model read through GetPage, which records every returned page").

- [ ] **Step 4: Delete the fallback tests and doubles; update `failingEntityStore`**

Delete: `newFallbackFixture`, `matchAllFixtureCondition` (if only they use it), `TestSearch_FallbackBranchIsBounded`, `TestSearch_FallbackBranchUnboundedReturnsAll`, `TestSearch_FallbackBranchIsBounded_TranslateFailureRoute`, `nonSearcherEntityStore`, `nonSearcherFactory` (`service_test.go`); `TestSearchFallback_TypeDirectedWithRegisteredModel` and the `combinedFallbackFactory` parts that only served the wrapper (`fallback_typed_test.go` — keep `saveTypedModel`, `corruptSchemaModelStore`, and rewrite `TestSearchFallback_FailsClosedOnGenuineModelLoadError` as `TestSearch_FailsClosedOnGenuineModelLoadError` using a factory whose `ModelStore` is corrupt and a real memory `EntityStore`; the assertion is unchanged: an error, not an empty result); `TestSearch_FallbackLoop_PreExpiredCtx_ReturnsDeadlineExceeded`; `TestSearch_BareLeafField_MatchFallback_MapsTo400InvalidCondition`; `noWholeModelEntityStore`, `noWholeModelStoreFactory` and their test in `service_list_test.go`; `TestSearchSort_PushdownFallbackAgree` (`internal/e2e/search_test.go:748-857`; delete `directSearchSorted` only if `grep -c "directSearchSorted(" internal/e2e/*.go` drops to its definition).

`mock_store_test.go` `failingEntityStore`: add

```go
func (s *failingEntityStore) Search(_ context.Context, _ spi.Filter, _ spi.SearchOptions) ([]*spi.Entity, error) {
	return nil, s.err
}
func (s *failingEntityStore) Iterate(_ context.Context, _ spi.ModelRef, _ spi.Filter, _ spi.IterateOptions) (spi.Iterator, error) {
	return nil, s.err
}
```

and delete its `GetAll`/`GetAllAsAt` if present.

- [ ] **Step 5: Run the iteration tier**

Run: `make test`
Expected: green (unit + parity, all root packages compile). Then `for p in memory sqlite postgres; do (cd plugins/$p && go test ./...); done` — green, including the new spitest cases from Plan 2.

- [ ] **Step 6: Commit**

```bash
git add internal/common/commontest/drain.go internal/domain/search/*_test.go internal/domain/entity/*_test.go internal/e2e/*_test.go e2e/parity/txsearchryw/*_test.go plugins/*/*_test.go
git commit -m "test: read a model through Iterate; fallback tests deleted with the branch they pinned"
```

---

### Task 7: Docs, CHANGELOG, COMPATIBILITY, cloud-parity, repin, full verification

**Files:**
- Modify: `cmd/cyoda/help/content/search.md:49,373`; `docs/cloud-parity/tx-aware-search.md:14-16,30,71`; `docs/cloud-parity/direct-search-bounded-or-fail.md:17,32,42-44,48-53`; `docs/cloud-parity/path-grammar.md:517-518`; `docs/CONSISTENCY.md:200,508` and Appendix A/B mentions; `cmd/cyoda/help/content/workflows.md:174`; `docs/ARCHITECTURE.md` (search section: the "GetAll + in-memory match fallback" sentences, and `:1045`); `docs/plugins/*.md` (grep `GetAll`); `CHANGELOG.md`; `COMPATIBILITY.md` v0.8.4 row (tenth wave); `docs/cloud-parity/README.md`.
- Create: `docs/cloud-parity/grouped-stats-iterate-required.md`.

- [ ] **Step 1: Prose edits**

- `search.md:49`: replace "Only when translation fails (unsupported condition type) does the service fall back to in-memory filtering after a full `GetAll` scan." with "There is no in-memory fallback: a condition that cannot be translated is rejected with `400 INVALID_CONDITION` (or `INVALID_FIELD_PATH` for a path-shaped failure), and every backend implements the pushdown." `:373`: "enforced on every direct-search code path (Searcher pushdown and in-memory fallback alike)" → "enforced by the backend's bounded-or-fail `Search`".
- `tx-aware-search.md:14-16`: delete "; the engine only falls back when the search condition itself cannot be translated to a backend predicate (unsupported condition type)". `:30`, `:71`: drop `GetAllAsAt` from the lists.
- `direct-search-bounded-or-fail.md`: `:17` bullet → "`limit` is required and positive on the wire (the server resolves a default when omitted); the service rejects a non-positive limit as a caller error." `:32` → "`Limit >= 1` is REQUIRED at the SPI; `Limit <= 0` is a contract violation the store MUST reject." `:42` → "(transaction-bound and point-in-time alike)". `:43-44` invariant 2 → "A non-positive `limit` never reaches a backend; a backend MUST reject one rather than re-default it." `:48-53` "Known commercial-backend gap": the paragraph now describes conformance with the SPI rule; reword to "The commercial backend re-defaulted a non-positive `limit`; under the SPI rule above it must reject it — pinned by spitest."
- `path-grammar.md:517-518`: "the bounded search call and the unbounded streaming drain alike" → "the store's `Search` and `Iterate` alike".
- `CONSISTENCY.md:200`: "Use `GetAll` (unchanged, always-tracking) for a fence" → "Use a full `GetPage` drain (every returned page is recorded unconditionally) for a fence"; `:508` and `workflows.md:174`: `Get`/`GetAll`/`Search`/`Count` → `Get`/`GetPage`/`Iterate`/`Search`/`Count`. Appendix A/B walkthroughs (`:649, :709-764, :855-893, :1002-1167`): replace `entityStore.GetAll(Doctor)` with `GetPage` drains and keep the read-set reasoning (identical recording).
- `ARCHITECTURE.md`: in the search section replace the fallback sentences with "one path: `EntityStore.Search`"; `:1045` "(narrower than the old whole-model `GetAll` behaviour it replaced …)" → "(the page, not the model)". Also fix `internal/match`'s stale description in §1 ("consumed by memory plugin") → "in-process tree adapter over the SPI kernel: workflow criteria, delete and grouped-stats residuals".
- `docs/plugins/*.md`: reword any `GetAll` description to `GetPage`/`Iterate`.

Run: `grep -rn 'GetAll\b' docs cmd/cyoda/help/content README.md | grep -v "ModelStore\|EntityModelGetAll\|EntityGetAllRequest\|getAllEntities"` — Expected: empty. `go test ./cmd/cyoda/help/... -run TestContent` — PASS.

- [ ] **Step 2: Cloud-parity note and index**

`docs/cloud-parity/grouped-stats-iterate-required.md`:

```markdown
# Grouped statistics: `Iterate` is required, the 501 is withdrawn

cyoda-go defines the contract; Cloud mirrors it.

## Behaviour

`POST /api/entity/stats/{entityName}/{modelVersion}/query` no longer answers
`501 NOT_IMPLEMENTED_BY_BACKEND`. Every storage backend must implement the
streamed read (`EntityStore.Iterate`), so the endpoint always has an execution
path: pushdown through `GroupedAggregator` when the backend offers it and
accepts the shape, otherwise a streamed tally. The code is retired from the
error taxonomy and the OpenAPI document.

## Invariant Cloud must mirror

1. The grouped-statistics endpoint never answers 501 for a backend-capability
   reason.
2. In a transaction, grouped aggregation records nothing into the
   transaction's read-set (spitest `GroupedAggregator/InTxRecordsNothing`).
```

Add the row to `docs/cloud-parity/README.md`'s table.

- [ ] **Step 3: CHANGELOG and COMPATIBILITY**

`CHANGELOG.md` `### Breaking`:

```
- Grouped statistics no longer answer `501 NOT_IMPLEMENTED_BY_BACKEND`; the
  code is retired. Every backend must implement `Iterate`, so the endpoint
  always has an execution path.
- Direct search has one path. The in-memory whole-model fallback is deleted:
  a condition that cannot be translated is rejected with 400
  (`INVALID_CONDITION`, or `INVALID_FIELD_PATH` for a path-shaped failure)
  instead of scanning the model in process. No client-reachable request
  changes status.
```

`### Changed`: async submit translates the condition before persisting the job.

`COMPATIBILITY.md` v0.8.4 row — append a **tenth wave** paragraph: pin moves to `cyoda-go-spi@<sha>`; `GetAll`/`GetAllAsAt` removed; `Search` and `Iterate` required `EntityStore` methods, `Searcher`/`Iterable` deleted, no deprecation window and why; spitest cases removed/added (list from Plan 2's CHANGELOG entry); **eighth commercial-backend obligation**: delete the two methods, drop the assertions, move `model_store.go:252` to `Iterate`, pass `Transaction/DeleteThenCompareAndSave` (a same-transaction delete makes a compare-and-save conflict — it already does on that backend if it applies deletes eagerly, verify), `Count/InTxBufferShapes`, `GroupedAggregator/InTxRecordsNothing`, `TenantIsolation/GetPage`; the 501 withdrawal.

- [ ] **Step 4: Repin, full suite, race**

```bash
make repin-plugins
make test-full
make race
```
Expected: all green. Exit checks (spec §9):

```bash
grep -rn 'GetAll(ctx, .*ModelRef\|GetAllAsAt\|getAllTx\|spi\.Iterable\|spi\.Searcher\|drainIterate\|ErrBackendNotSupported\|nonSearcherEntityStore\|cond == nil' --include='*.go' internal plugins app cmd | grep -v _test
grep -rn GetAll plugins/ internal/domain/search --include='*.go' | grep -v "ModelStore\|models"
```
Expected: both empty.

- [ ] **Step 5: Commit**

```bash
git add cmd/cyoda/help/content docs CHANGELOG.md COMPATIBILITY.md go.mod go.sum
git commit -m "docs: one search path, no whole-model read; tenth SPI wave; 501 withdrawn"
```

- [ ] **Step 6: Review gates, PR, issue bookkeeping**

Dispatch the fresh-context code review and the security review on the full branch diff against `origin/release/v0.8.4`. `gh pr create --base release/v0.8.4` with milestone `v0.8.4`; body: spec link, the three-PR sequence, the oasdiff waiver for the withdrawn 501, "Closes #477". After merge: close #477 manually (release-branch merges do not auto-close), update #516 row 13 to DONE with the three PR numbers and the new pin, and note row 14 is next.

---

## Self-review

- **Spec coverage:** §2.1 → Task 2; §2.2 → Tasks 2, 4, 5; §2.3 → Task 3; §4.7 → Task 1; §5 (501) → Task 5; §6 rows → Tasks 2, 3, 6; §8 PR 3 bullets (six tests' fate, e2e deletion, doubles, docs list, rollback) → Tasks 6, 7; §9 exit checks → Task 7 Step 4.
- **Placeholders:** `<sha>` in two commit messages is the Plan-2 merge commit, computed in Task 1 Step 1 — a value, not a gap.
- **Type consistency:** `commontest.DrainAll(t, ctx, store, ref, asAt *time.Time)` used identically in Task 6; `failingEntityStore.Search/Iterate` match the SPI signatures from Plan 2 Task 1; `QueryGroupedStats(ctx, store spi.EntityStore, …)` in Task 5 matches `fakeIterable`'s embedding.
