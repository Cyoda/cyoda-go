# Direct-Search Limit Correctness Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix direct-search limit handling so an omitted `limit` defaults to 1000 (not unbounded) on the `Searcher` pushdown path (#432); let a `Searcher` surface `SEARCH_RESULT_LIMIT` (400) via a new SPI sentinel (#433); fold in the adjacent `ErrScanBudgetExhausted` 500-bug as an SPI sentinel with a new `SCAN_BUDGET_EXHAUSTED` (400) code; and make grouped-stats sentinel→status translation transport-symmetric at the service layer.

**Architecture:** Resolve the omitted-limit default at the two direct entry points (HTTP `SearchEntities`, gRPC `handleDirectSearchRequest`) so the service can treat `Limit==0` as unbounded uniformly across both branches. Add two SPI sentinels (`ErrSearchResultLimitExceeded`, `ErrScanBudgetExhausted`) and translate them to `*common.AppError` **at the service layer** so both transports (which already forward `*common.AppError`) surface the documented 4xx — the gRPC path only understands `*common.AppError`, so per-handler mapping would leave gRPC at 500. Move grouped-stats' handler-switch translation into `QueryGroupedStats` for the same reason.

**Tech Stack:** Go 1.26; `github.com/cyoda-platform/cyoda-go-spi` (separate module, pseudo-version pinned across 4 `go.mod` files); sqlite/postgres/memory storage plugins (each its own module); testcontainers-go (postgres parity); httptest; CloudEvents gRPC.

## Global Constraints

- **SPI coordinated release:** follow `MAINTAINING.md` "Coordinated release across sibling repos" — SPI change lands FIRST on the SPI `feat/schedule-function` branch (the live v0.8.3 SPI window), then cyoda-go repins. No SPI tag this milestone (deferred to v0.8.3 tag). Local composition via an **uncommitted** `go.work` `use ../cyoda-go-spi` line (skip-worktree); never `git add -A` (go.work is tracked in CI-safe form).
- **Pin all 4 `go.mod`:** root + `plugins/memory` + `plugins/postgres` + `plugins/sqlite` move to the same new SPI pseudo-version in ONE commit.
- **No issue IDs in shipped artefacts:** never put `#432`/`#433` in error messages, help topics, OpenAPI, code comments, log output, or response bodies. Issue IDs belong only in commits, PR body, and spec/plan docs.
- **Error-code↔help-topic bijection:** every new error code needs `cmd/cyoda/help/content/errors/<CODE>.md`; `TestErrCode_Parity` enforces it strictly.
- **Compact prose** in help/CHANGELOG/comments: actionable core only.
- **Constant values (verbatim):** `DefaultDirectSearchLimit = 1000`; `pagination.MaxPageSize = 10000`; new code string `"SCAN_BUDGET_EXHAUSTED"`; existing code string `"SEARCH_RESULT_LIMIT"`.
- **Race detector:** run once at end-of-deliverable only, before PR — never per-step.
- **Per-plugin tests:** `go test ./...` from root skips `plugins/*` (separate modules); run each plugin module explicitly in end-of-deliverable verification.
- **PR target:** `release/v0.8.3`; body carries `Closes #432`, `Closes #433`; both milestoned v0.8.3 at merge.

---

## File Structure

**SPI (separate repo `../cyoda-go-spi`, branch `feat/schedule-function`):**
- Modify: `errors.go` — add `ErrSearchResultLimitExceeded`, `ErrScanBudgetExhausted`.

**cyoda-go engine:**
- Modify: `go.mod`, `plugins/memory/go.mod`, `plugins/postgres/go.mod`, `plugins/sqlite/go.mod` — repin SPI.
- Modify: `go.work` — local uncommitted `use ../cyoda-go-spi` for dev/test (do NOT commit).
- Modify: `internal/common/errors.go` — add `(*AppError).WithCause`.
- Modify: `internal/common/error_codes.go` — add `ErrCodeScanBudgetExhausted`.
- Create: `cmd/cyoda/help/content/errors/SCAN_BUDGET_EXHAUSTED.md`.
- Modify: `internal/domain/search/handler.go` — `DefaultDirectSearchLimit` const; omitted-limit default in `SearchEntities`.
- Modify: `internal/grpc/search.go` — omitted-limit default in `handleDirectSearchRequest`.
- Modify: `internal/domain/search/service.go` — remove fallback 1000 default; translate the two SPI sentinels to `AppError`.
- Modify: `internal/domain/entity/grouped_stats_service.go` — inner/outer split; `classifyGroupedStatsError`; scan-budget mapping.
- Modify: `internal/domain/entity/grouped_stats_handler.go` — collapse switch to `errors.As`-forward.
- Modify: `plugins/sqlite/searcher.go` — return `spi.ErrScanBudgetExhausted`.

**Tests:**
- Modify: `internal/domain/search/service_test.go` — 0=unbounded; sentinel→AppError.
- Modify: `internal/domain/search/handler_test.go` (or new `handler_limit_test.go`) — HTTP entry-point default + sentinel 400.
- Modify: `internal/grpc/search_test.go` — gRPC entry-point default + sentinel 400.
- Modify: `internal/common/errors_test.go` — `WithCause` preserves `errors.Is`.
- Modify: `plugins/sqlite/searcher_test.go` — assert `spi.ErrScanBudgetExhausted`.
- Modify: `internal/domain/entity/grouped_stats_service_test.go` — AppError contract (errors.Is still holds via WithCause) + scan-budget mapping.
- Create: parity scenario `RunSearchOmittedLimitDefaults1000` in `e2e/parity/search.go`; register in `e2e/parity/registry.go`; bump `e2e/parity/registry_count_test.go`.
- Create: sqlite-only e2e for scan-budget 400 (search + grouped-stats) — `e2e/parity/sqlite/` or an sqlite plugin-level HTTP test (see Task 8).
- Modify: `CHANGELOG.md`.

---

## Task 1: SPI sentinels + repin across 4 modules

**Files:**
- Modify: `../cyoda-go-spi/errors.go`
- Modify: `go.mod`, `plugins/memory/go.mod`, `plugins/postgres/go.mod`, `plugins/sqlite/go.mod`
- Modify (local, uncommitted): `go.work`

**Interfaces:**
- Produces: `spi.ErrSearchResultLimitExceeded error`, `spi.ErrScanBudgetExhausted error` — consumed by Tasks 4, 5, 6.

- [ ] **Step 1: Add the two sentinels to the SPI errors file**

In `../cyoda-go-spi/errors.go`, after `ErrGroupCardinalityExceeded` (line ~103), add:

```go
// ErrSearchResultLimitExceeded is returned by a Searcher whose direct search
// matched more entities than the configured result-limit cap (bounded-or-fail
// contract). The engine maps it to a client-facing 400.
var ErrSearchResultLimitExceeded = errors.New("search result limit exceeded")

// ErrScanBudgetExhausted is returned by a Searcher or streaming aggregator
// whose residual (non-pushdown) scan examined more rows than its configured
// scan budget before completing. The engine maps it to a client-facing 400.
var ErrScanBudgetExhausted = errors.New("scan budget exhausted")
```

- [ ] **Step 2: Build the SPI module to verify it compiles**

Run: `cd ../cyoda-go-spi && go build ./... && go vet ./...`
Expected: no output (success).

- [ ] **Step 3: Commit and push the SPI change on the milestone branch**

```bash
cd ../cyoda-go-spi
git add errors.go
git commit -m "feat: add ErrSearchResultLimitExceeded + ErrScanBudgetExhausted sentinels

Engine maps both to client-facing 400s (SEARCH_RESULT_LIMIT / SCAN_BUDGET_EXHAUSTED).

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
git push origin feat/schedule-function
```

Capture the new commit SHA: `git rev-parse --short HEAD`.

- [ ] **Step 4: Add the local go.work use line (uncommitted) so the engine sees the sentinels immediately**

Edit `go.work` to add `./` sibling `use ../cyoda-go-spi`. Then mark it skip-worktree so it is never staged:

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go
# Add the line to go.work (keep existing use block; append):
#   use ../cyoda-go-spi
git update-index --skip-worktree go.work
```

Verify local resolution: `go list -m github.com/cyoda-platform/cyoda-go-spi` shows the local path.

- [ ] **Step 5: Repin all 4 go.mod to the new SPI pseudo-version**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go
GOFLAGS=-mod=mod GOWORK=off go get github.com/cyoda-platform/cyoda-go-spi@feat/schedule-function
for m in plugins/memory plugins/postgres plugins/sqlite; do
  (cd "$m" && GOFLAGS=-mod=mod GOWORK=off go get github.com/cyoda-platform/cyoda-go-spi@feat/schedule-function)
done
```

Confirm all four `go.mod` now show the SAME new `v0.8.3-0.<timestamp>-<sha>` pseudo-version:

Run: `grep -h cyoda-go-spi go.mod plugins/*/go.mod`
Expected: four identical pseudo-version lines with the SHA from Step 3.

- [ ] **Step 6: Verify the whole workspace still builds with the pin**

Run: `GOWORK=off go build ./... && for m in plugins/memory plugins/postgres plugins/sqlite; do (cd "$m" && GOWORK=off go build ./...); done`
Expected: success (the sentinels are unused yet, which is fine — no reference).

- [ ] **Step 7: Commit the repin (4 go.mod + go.sum), NOT go.work**

```bash
git add go.mod go.sum plugins/memory/go.mod plugins/memory/go.sum \
        plugins/postgres/go.mod plugins/postgres/go.sum \
        plugins/sqlite/go.mod plugins/sqlite/go.sum
git status --short   # verify go.work is NOT staged
git commit -m "build: repin cyoda-go-spi to add search bounded-fail sentinels

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 2: `AppError.WithCause` + `SCAN_BUDGET_EXHAUSTED` code + help topic

**Files:**
- Modify: `internal/common/errors.go`
- Modify: `internal/common/error_codes.go`
- Create: `cmd/cyoda/help/content/errors/SCAN_BUDGET_EXHAUSTED.md`
- Test: `internal/common/errors_test.go`

**Interfaces:**
- Produces: `func (e *common.AppError) WithCause(err error) *common.AppError`; `common.ErrCodeScanBudgetExhausted = "SCAN_BUDGET_EXHAUSTED"` — consumed by Tasks 4, 6.

- [ ] **Step 1: Write the failing test for WithCause preserving errors.Is**

Add to `internal/common/errors_test.go`:

```go
func TestAppError_WithCause_PreservesErrorsIs(t *testing.T) {
	sentinel := errors.New("some sentinel")
	appErr := common.Operational(http.StatusBadRequest, "SOME_CODE", "human message").WithCause(sentinel)

	if !errors.Is(appErr, sentinel) {
		t.Errorf("errors.Is(appErr, sentinel) = false, want true")
	}
	var ae *common.AppError
	if !errors.As(appErr, &ae) {
		t.Fatalf("errors.As(appErr, &AppError) = false, want true")
	}
	if ae.Status != http.StatusBadRequest || ae.Code != "SOME_CODE" {
		t.Errorf("got status=%d code=%q, want 400/SOME_CODE", ae.Status, ae.Code)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/common/ -run TestAppError_WithCause_PreservesErrorsIs -v`
Expected: FAIL — `appErr.WithCause undefined (type *common.AppError has no field or method WithCause)`.

- [ ] **Step 3: Implement WithCause**

In `internal/common/errors.go`, after `AsRetryable` (line ~75), add:

```go
// WithCause attaches a wrapped cause to a freshly-constructed *AppError and
// returns the receiver for fluent chaining. The cause is exposed via Unwrap,
// so errors.Is(returned, cause) holds — use this when translating a storage
// SPI sentinel into a client-facing AppError while keeping the sentinel
// inspectable by callers. Like AsRetryable, this mutates the receiver; call
// only on a just-constructed Operational(...), never on a shared instance.
func (e *AppError) WithCause(err error) *AppError {
	e.Err = err
	return e
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/common/ -run TestAppError_WithCause_PreservesErrorsIs -v`
Expected: PASS.

- [ ] **Step 5: Add the SCAN_BUDGET_EXHAUSTED error code**

In `internal/common/error_codes.go`, inside the search-codes `const` block (after `ErrCodeSearchResultLimit`, line ~73), add:

```go
	// ErrCodeScanBudgetExhausted is returned when a residual (non-pushdown)
	// search or streaming aggregation examined more rows than the backend's
	// configured scan budget before completing. Non-retryable: the client
	// must narrow the query or add an indexable predicate.
	ErrCodeScanBudgetExhausted = "SCAN_BUDGET_EXHAUSTED"
```

- [ ] **Step 6: Create the help topic (required for TestErrCode_Parity)**

Create `cmd/cyoda/help/content/errors/SCAN_BUDGET_EXHAUSTED.md`. Match the format of an existing topic — inspect `cmd/cyoda/help/content/errors/SEARCH_RESULT_LIMIT.md` first, then write compact content:

```markdown
# SCAN_BUDGET_EXHAUSTED

**HTTP 400.** A search or grouped-stats request used a predicate that could not
be pushed down to storage, and evaluating the residual filter examined more
rows than the backend's scan budget before completing.

## Cause

The condition (or part of it) is not indexable — for example a regex or a
wildcard path — so the backend must scan and post-filter rows. When the number
of scanned rows exceeds the configured limit, the request fails fast instead of
running unbounded.

## Fix

- Narrow the query with an indexable predicate (equality/range on a stored field).
- Reduce the candidate set with an additional selective condition.
- If the broad scan is intentional, raise the backend scan-budget configuration.

Non-retryable: retrying the same request will fail identically.
```

- [ ] **Step 7: Run the error-code parity test**

Run: `go test ./cmd/cyoda/... -run TestErrCode_Parity -v`
Expected: PASS (new code has its topic; no orphan topics).

- [ ] **Step 8: Commit**

```bash
git add internal/common/errors.go internal/common/errors_test.go \
        internal/common/error_codes.go cmd/cyoda/help/content/errors/SCAN_BUDGET_EXHAUSTED.md
git commit -m "feat(common): AppError.WithCause + SCAN_BUDGET_EXHAUSTED error code

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 3: #432 — omitted-limit default at entry points; service treats 0 as unbounded

**Files:**
- Modify: `internal/domain/search/handler.go` (const + `SearchEntities`)
- Modify: `internal/grpc/search.go` (`handleDirectSearchRequest`)
- Modify: `internal/domain/search/service.go` (remove fallback 1000 default)
- Test: `internal/domain/search/service_test.go`, `internal/domain/search/handler_test.go`, `internal/grpc/search_test.go`

**Interfaces:**
- Consumes: `searcherFactory` / `searcherEntityStore` capture stubs (already in `service_test.go` lines ~646-720).
- Produces: `search.DefaultDirectSearchLimit int = 1000` — referenced by handler and gRPC.

- [ ] **Step 1: Write the failing service unit test (0 = unbounded, no injected 1000)**

Add to `internal/domain/search/service_test.go` (reuse the existing `searcherEntityStore`/`searcherFactory` capture pattern; the stub must capture `opts` — extend it with a `capturedOpts spi.SearchOptions` field if not present):

```go
func TestSearch_LimitZeroPassesUnboundedToSearcher(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()
	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}
	saveMinimalModel(t, ctx, base, ref)

	realStore, _ := base.EntityStore(ctx)
	ses := &searcherEntityStore{
		EntityStore: realStore,
		searchFn: func(_ context.Context, _ spi.Filter, opts spi.SearchOptions) ([]*spi.Entity, error) {
			return nil, nil // result set irrelevant here
		},
	}
	factory := &searcherFactory{StoreFactory: base, entityStore: ses}
	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := base.AsyncSearchStore(context.Background())
	svc := search.NewSearchService(factory, uuids, searchStore)

	cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: "Alice"}
	if _, err := svc.Search(ctx, ref, cond, search.SearchOptions{Limit: 0}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if ses.capturedOpts.Limit != 0 {
		t.Errorf("spiLimit = %d, want 0 (unbounded); service must not inject 1000", ses.capturedOpts.Limit)
	}
}
```

(If `searcherEntityStore` has no `capturedOpts`, add `capturedOpts spi.SearchOptions` and set it in its `Search` method: `s.capturedOpts = opts`.)

- [ ] **Step 2: Run test to verify current behavior**

Run: `go test ./internal/domain/search/ -run TestSearch_LimitZeroPassesUnboundedToSearcher -v`
Expected: PASS already for the pushdown branch (it maps 0→0). This test **locks** the "0=unbounded" contract so the Step 5 fallback-removal cannot regress it. If it fails, the capture wiring is wrong — fix the stub before proceeding.

- [ ] **Step 3: Write the failing HTTP entry-point test (omitted → 1000; present → N)**

Add to a new file `internal/domain/search/handler_limit_test.go` (package `search_test`). Construct the handler directly over a capture stub so no seeding is needed:

```go
func TestSearchEntities_OmittedLimitDefaultsTo1000(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()
	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}
	saveMinimalModel(t, ctx, base, ref)

	realStore, _ := base.EntityStore(ctx)
	ses := &searcherEntityStore{EntityStore: realStore,
		searchFn: func(_ context.Context, _ spi.Filter, _ spi.SearchOptions) ([]*spi.Entity, error) { return nil, nil }}
	factory := &searcherFactory{StoreFactory: base, entityStore: ses}
	searchStore, _ := base.AsyncSearchStore(context.Background())
	h := search.NewHandler(search.NewSearchService(factory, common.NewTestUUIDGenerator(), searchStore))

	body := `{"type":"simple","jsonPath":"$.name","operatorType":"EQUALS","value":"Alice"}`
	req := httptest.NewRequest(http.MethodPost, "/search/direct/person/1", strings.NewReader(body)).WithContext(ctx)
	rr := httptest.NewRecorder()
	h.SearchEntities(rr, req, "person", 1, genapi.SearchEntitiesParams{}) // params.Limit == nil

	if ses.capturedOpts.Limit != 1000 {
		t.Errorf("omitted limit → spiLimit %d, want 1000", ses.capturedOpts.Limit)
	}
}
```

(Confirm the exact param type name `genapi.SearchEntitiesParams` and import alias from `handler.go`. Verify `saveMinimalModel`/`tenantCtx` are visible in `search_test` package — they live in `service_test.go`, same package, so they are.)

- [ ] **Step 4: Run test to verify it fails**

Run: `go test ./internal/domain/search/ -run TestSearchEntities_OmittedLimitDefaultsTo1000 -v`
Expected: FAIL — `spiLimit 0, want 1000` (entry point does not yet default).

- [ ] **Step 5: Add the constant and the HTTP default**

In `internal/domain/search/handler.go`, add to the `const` block (line ~23):

```go
	// DefaultDirectSearchLimit is applied to a direct (synchronous) search when
	// the client omits the limit, bounding the result set that protects the
	// cluster. Async submit intentionally leaves the limit unset (store-all,
	// paginated at retrieval).
	DefaultDirectSearchLimit = 1000
```

In `SearchEntities`, the limit block (line ~107-121) currently only sets `opts.Limit` inside `if params.Limit != nil`. Add the else:

```go
	if params.Limit != nil {
		// ... existing parse + MaxPageSize reject (unchanged) ...
		opts.Limit = lim
	} else {
		opts.Limit = DefaultDirectSearchLimit
	}
```

- [ ] **Step 6: Run the HTTP test to verify it passes**

Run: `go test ./internal/domain/search/ -run TestSearchEntities_OmittedLimitDefaultsTo1000 -v`
Expected: PASS.

- [ ] **Step 7: Write + run the failing gRPC entry-point test**

Add to `internal/grpc/search_test.go` (package `grpc`). Build a service over the capture stub and call `handleDirectSearchRequest` (or `EntitySearchCollection`) with no `limit` field:

```go
func TestDirectSearch_OmittedLimitDefaultsTo1000(t *testing.T) {
	base := memory.NewStoreFactory()
	ctx := grpcTenantCtx() // spi.WithUserContext(...) as in newTestEnv
	ref := spi.ModelRef{EntityName: "capdef", ModelVersion: "1"}
	saveMinimalModelGRPC(t, ctx, base, ref) // register model via base.ModelStore

	realStore, _ := base.EntityStore(ctx)
	ses := &searcherEntityStoreG{EntityStore: realStore,
		searchFn: func(_ context.Context, _ spi.Filter, _ spi.SearchOptions) ([]*spi.Entity, error) { return nil, nil }}
	factory := &searcherFactoryG{StoreFactory: base, entityStore: ses}
	searchStore, _ := base.AsyncSearchStore(context.Background())
	svc := &CloudEventsServiceImpl{searchService: search.NewSearchService(factory, common.NewDefaultUUIDGenerator(), searchStore)}

	ce := makeCE(EntitySearchRequest, map[string]any{
		"id":        "s-capdef-1",
		"model":     map[string]any{"name": "capdef", "version": 1},
		"condition": map[string]any{"type": "simple", "jsonPath": "$.name", "operatorType": "EQUALS", "value": "Alice"},
		// no "limit"
	})
	stream := &mockEntityStream{ctx: ctx}
	if err := svc.EntitySearchCollection(ce, stream); err != nil {
		t.Fatalf("stream error: %v", err)
	}
	if ses.capturedOpts.Limit != 1000 {
		t.Errorf("gRPC omitted limit → spiLimit %d, want 1000", ses.capturedOpts.Limit)
	}
}
```

(Define `searcherEntityStoreG`/`searcherFactoryG` capture stubs in the grpc test package — mirror the search-package ones. `grpcTenantCtx`/`saveMinimalModelGRPC` are small helpers: reuse `newTestEnv`'s UserContext shape and register `&spi.ModelDescriptor{Ref: ref}` via `base.ModelStore(ctx)`.)

Run: `go test ./internal/grpc/ -run TestDirectSearch_OmittedLimitDefaultsTo1000 -v`
Expected: FAIL — `spiLimit 0, want 1000`.

- [ ] **Step 8: Add the gRPC default**

In `internal/grpc/search.go` `handleDirectSearchRequest` (line ~336):

```go
	if req.Limit != nil {
		opts.Limit = *req.Limit
	} else {
		opts.Limit = search.DefaultDirectSearchLimit
	}
```

Run: `go test ./internal/grpc/ -run TestDirectSearch_OmittedLimitDefaultsTo1000 -v`
Expected: PASS.

- [ ] **Step 9: Remove the service's implicit 1000 fallback default**

In `internal/domain/search/service.go`, the fallback branch (lines ~256-260):

```go
	// Apply limit. Default 1000 when zero; negative means unbounded (no cap).
	limit := opts.Limit
	if limit == 0 {
		limit = 1000
	}
	if limit > 0 && limit < len(matches) {
		matches = matches[:limit]
	}
```

becomes (0 and negative both mean unbounded, matching the pushdown branch and the SPI convention):

```go
	// Apply limit. 0 or negative means unbounded (no cap); the direct entry
	// points resolve an omitted client limit to DefaultDirectSearchLimit before
	// reaching the service, so 0 here means an explicit store-all (async submit
	// or an internal caller), never "client omitted".
	if opts.Limit > 0 && opts.Limit < len(matches) {
		matches = matches[:opts.Limit]
	}
```

- [ ] **Step 10: Run the search + grpc suites**

Run: `go test ./internal/domain/search/ ./internal/grpc/ -v`
Expected: PASS (existing tests + the three new ones). If any existing test asserted the old fallback-1000-on-omitted behavior at the service level, update it to the new contract (0=unbounded) — verify by reading the failure.

- [ ] **Step 11: Commit**

```bash
git add internal/domain/search/handler.go internal/domain/search/handler_limit_test.go \
        internal/grpc/search.go internal/grpc/search_test.go \
        internal/domain/search/service.go internal/domain/search/service_test.go
git commit -m "fix(search): default omitted direct-search limit to 1000 at entry points (#432)

Resolve DefaultDirectSearchLimit at the HTTP + gRPC entry points and treat
Limit==0 as unbounded uniformly in the service, so the Searcher pushdown path
honours the documented default instead of returning unbounded.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 4: #433 — translate SPI bounded-fail sentinels to 400 at the service

**Files:**
- Modify: `internal/domain/search/service.go`
- Test: `internal/domain/search/service_test.go`, `internal/domain/search/handler_limit_test.go`, `internal/grpc/search_test.go`

**Interfaces:**
- Consumes: `spi.ErrSearchResultLimitExceeded`, `spi.ErrScanBudgetExhausted` (Task 1); `common.ErrCodeSearchResultLimit`, `common.ErrCodeScanBudgetExhausted` (Task 2); `(*AppError).WithCause` (Task 2).

- [ ] **Step 1: Write the failing service unit tests (both sentinels → 400 AppError)**

Add to `internal/domain/search/service_test.go`:

```go
func TestSearch_SearcherResultLimitSentinel_MapsTo400(t *testing.T) {
	svc, ctx, ref := newStubSearcherService(t, func(_ context.Context, _ spi.Filter, _ spi.SearchOptions) ([]*spi.Entity, error) {
		return nil, fmt.Errorf("plugin detail: %w", spi.ErrSearchResultLimitExceeded)
	})
	cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: "Alice"}
	_, err := svc.Search(ctx, ref, cond, search.SearchOptions{Limit: 10})

	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("want *common.AppError, got %T: %v", err, err)
	}
	if appErr.Status != http.StatusBadRequest || appErr.Code != common.ErrCodeSearchResultLimit {
		t.Errorf("got %d/%q, want 400/%s", appErr.Status, appErr.Code, common.ErrCodeSearchResultLimit)
	}
	if !errors.Is(err, spi.ErrSearchResultLimitExceeded) {
		t.Errorf("errors.Is(err, ErrSearchResultLimitExceeded) = false; WithCause must preserve the sentinel")
	}
}

func TestSearch_SearcherScanBudgetSentinel_MapsTo400(t *testing.T) {
	svc, ctx, ref := newStubSearcherService(t, func(_ context.Context, _ spi.Filter, _ spi.SearchOptions) ([]*spi.Entity, error) {
		return nil, fmt.Errorf("examined N rows: %w", spi.ErrScanBudgetExhausted)
	})
	cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: "Alice"}
	_, err := svc.Search(ctx, ref, cond, search.SearchOptions{Limit: 10})

	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("want *common.AppError, got %T: %v", err, err)
	}
	if appErr.Status != http.StatusBadRequest || appErr.Code != common.ErrCodeScanBudgetExhausted {
		t.Errorf("got %d/%q, want 400/%s", appErr.Status, appErr.Code, common.ErrCodeScanBudgetExhausted)
	}
}
```

Add a small factory helper near the other helpers in `service_test.go` (DRY — Task 3 and 4 both use it):

```go
func newStubSearcherService(t *testing.T, fn func(context.Context, spi.Filter, spi.SearchOptions) ([]*spi.Entity, error)) (*search.SearchService, context.Context, spi.ModelRef) {
	t.Helper()
	base := memory.NewStoreFactory()
	t.Cleanup(func() { base.Close() })
	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}
	saveMinimalModel(t, ctx, base, ref)
	realStore, _ := base.EntityStore(ctx)
	ses := &searcherEntityStore{EntityStore: realStore, searchFn: fn}
	factory := &searcherFactory{StoreFactory: base, entityStore: ses}
	searchStore, _ := base.AsyncSearchStore(context.Background())
	return search.NewSearchService(factory, common.NewTestUUIDGenerator(), searchStore), ctx, ref
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/domain/search/ -run 'TestSearch_Searcher(ResultLimit|ScanBudget)Sentinel' -v`
Expected: FAIL — the raw sentinel is returned, `errors.As(&AppError)` is false.

- [ ] **Step 3: Add the service-layer translation**

In `internal/domain/search/service.go`, the pushdown branch (lines ~202-210) currently ends with `return searcher.Search(ctx, filter, spi.SearchOptions{...})`. Capture and translate:

```go
		res, sErr := searcher.Search(ctx, filter, spi.SearchOptions{
			ModelName:    modelRef.EntityName,
			ModelVersion: modelRef.ModelVersion,
			PointInTime:  opts.PointInTime,
			Limit:        spiLimit,
			Offset:       opts.Offset,
			OrderBy:      orderBy,
			TrackingRead: opts.TrackingRead,
		})
		switch {
		case errors.Is(sErr, spi.ErrSearchResultLimitExceeded):
			return nil, common.Operational(http.StatusBadRequest,
				common.ErrCodeSearchResultLimit,
				"matched result count exceeds the configured limit").WithCause(sErr)
		case errors.Is(sErr, spi.ErrScanBudgetExhausted):
			return nil, common.Operational(http.StatusBadRequest,
				common.ErrCodeScanBudgetExhausted,
				"search scan budget exhausted; narrow the query or add an indexable predicate").WithCause(sErr)
		}
		return res, sErr
```

- [ ] **Step 4: Run the service tests to verify they pass**

Run: `go test ./internal/domain/search/ -run 'TestSearch_Searcher(ResultLimit|ScanBudget)Sentinel' -v`
Expected: PASS.

- [ ] **Step 5: Write + run the failing HTTP transport test (sentinel → 400 errorCode)**

Add to `internal/domain/search/handler_limit_test.go`:

```go
func TestSearchEntities_ResultLimitSentinel_Returns400(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()
	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}
	saveMinimalModel(t, ctx, base, ref)
	realStore, _ := base.EntityStore(ctx)
	ses := &searcherEntityStore{EntityStore: realStore,
		searchFn: func(_ context.Context, _ spi.Filter, _ spi.SearchOptions) ([]*spi.Entity, error) {
			return nil, spi.ErrSearchResultLimitExceeded
		}}
	searchStore, _ := base.AsyncSearchStore(context.Background())
	h := search.NewHandler(search.NewSearchService(&searcherFactory{StoreFactory: base, entityStore: ses}, common.NewTestUUIDGenerator(), searchStore))

	body := `{"type":"simple","jsonPath":"$.name","operatorType":"EQUALS","value":"Alice"}`
	req := httptest.NewRequest(http.MethodPost, "/search/direct/person/1", strings.NewReader(body)).WithContext(ctx)
	rr := httptest.NewRecorder()
	h.SearchEntities(rr, req, "person", 1, genapi.SearchEntitiesParams{})

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	// error body is a ProblemDetail with properties.errorCode
	var obj map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &obj)
	props, _ := obj["properties"].(map[string]any)
	if props == nil || props["errorCode"] != common.ErrCodeSearchResultLimit {
		t.Errorf("errorCode = %v, want %s; body=%s", props, common.ErrCodeSearchResultLimit, rr.Body.String())
	}
}
```

Run: `go test ./internal/domain/search/ -run TestSearchEntities_ResultLimitSentinel_Returns400 -v`
Expected: PASS (translation already added in Step 3; this test proves the HTTP transport forwards it). If the error body shape differs, inspect an existing 400 handler test (`INVALID_FIELD_PATH`) and match the exact JSON path.

- [ ] **Step 6: Write + run the failing gRPC transport test (sentinel → CLIENT_ERROR/SEARCH_RESULT_LIMIT)**

Add to `internal/grpc/search_test.go`, reusing the Task-3 stub helpers:

```go
func TestDirectSearch_ResultLimitSentinel_ClientError(t *testing.T) {
	base := memory.NewStoreFactory()
	ctx := grpcTenantCtx()
	ref := spi.ModelRef{EntityName: "caperr", ModelVersion: "1"}
	saveMinimalModelGRPC(t, ctx, base, ref)
	realStore, _ := base.EntityStore(ctx)
	ses := &searcherEntityStoreG{EntityStore: realStore,
		searchFn: func(_ context.Context, _ spi.Filter, _ spi.SearchOptions) ([]*spi.Entity, error) {
			return nil, spi.ErrSearchResultLimitExceeded
		}}
	searchStore, _ := base.AsyncSearchStore(context.Background())
	svc := &CloudEventsServiceImpl{searchService: search.NewSearchService(&searcherFactoryG{StoreFactory: base, entityStore: ses}, common.NewDefaultUUIDGenerator(), searchStore)}

	ce := makeCE(EntitySearchRequest, map[string]any{
		"id":        "s-caperr-1",
		"model":     map[string]any{"name": "caperr", "version": 1},
		"condition": map[string]any{"type": "simple", "jsonPath": "$.name", "operatorType": "EQUALS", "value": "Alice"},
		"limit":     10,
	})
	stream := &mockEntityStream{ctx: ctx}
	if err := svc.EntitySearchCollection(ce, stream); err != nil {
		t.Fatalf("stream error: %v", err)
	}
	var typed events.EntityResponseJson
	validateResponse(t, stream.sent[0], &typed)
	if typed.Success {
		t.Fatal("want success=false")
	}
	if typed.Error.Code != "CLIENT_ERROR" || !strings.Contains(typed.Error.Message, common.ErrCodeSearchResultLimit) {
		t.Errorf("got code=%q msg=%q, want CLIENT_ERROR / contains %s", typed.Error.Code, typed.Error.Message, common.ErrCodeSearchResultLimit)
	}
}
```

Run: `go test ./internal/grpc/ -run TestDirectSearch_ResultLimitSentinel_ClientError -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/domain/search/service.go internal/domain/search/service_test.go \
        internal/domain/search/handler_limit_test.go internal/grpc/search_test.go
git commit -m "feat(search): map Searcher bounded-fail sentinels to 400 at the service (#433)

ErrSearchResultLimitExceeded -> 400 SEARCH_RESULT_LIMIT;
ErrScanBudgetExhausted -> 400 SCAN_BUDGET_EXHAUSTED. Both transports forward the
AppError, so gRPC no longer 500s on a Searcher bounded-fail.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 5: sqlite plugin returns the SPI scan-budget sentinel

**Files:**
- Modify: `plugins/sqlite/searcher.go`
- Test: `plugins/sqlite/searcher_test.go`

**Interfaces:**
- Consumes: `spi.ErrScanBudgetExhausted` (Task 1, via the sqlite module's repinned SPI).

- [ ] **Step 1: Update the existing scan-budget test to assert the SPI sentinel**

In `plugins/sqlite/searcher_test.go` `TestSearcher_ScanBudgetExhausted` (line ~364), change the assertion target:

```go
	if !errors.Is(err, spi.ErrScanBudgetExhausted) {
		t.Fatalf("expected spi.ErrScanBudgetExhausted, got: %v", err)
	}
```

(Ensure the SPI is imported as `spi "github.com/cyoda-platform/cyoda-go-spi"` in the test file — it already is.)

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd plugins/sqlite && GOWORK=off go test ./... -run TestSearcher_ScanBudgetExhausted -v`
Expected: FAIL — the search still wraps the plugin-local `ErrScanBudgetExhausted`, which is a different error identity than `spi.ErrScanBudgetExhausted`.
(If you rely on the local `go.work use ../cyoda-go-spi`, run without `GOWORK=off`; but the plugin module must resolve the repinned SPI — prefer `GOWORK=off` to test the shipped pin.)

- [ ] **Step 3: Return the SPI sentinel from both raise sites**

In `plugins/sqlite/searcher.go`, change line 99 and line 241 to wrap the SPI sentinel, and remove the now-unused plugin-local var (lines 16-19):

```go
// line 99:
			return nil, fmt.Errorf("%w: examined %d rows", spi.ErrScanBudgetExhausted, s.cfg.SearchScanLimit)
// line 241:
					return nil, false, fmt.Errorf("%w: examined %d rows", spi.ErrScanBudgetExhausted, s.cfg.SearchScanLimit)
```

Delete the local declaration:

```go
// REMOVE lines 16-19:
// var ErrScanBudgetExhausted = errors.New("scan budget exhausted")
```

Then grep for any remaining reference to the local `ErrScanBudgetExhausted` in the plugin (non-test and test) and repoint to `spi.ErrScanBudgetExhausted`:

Run: `grep -rn "ErrScanBudgetExhausted" plugins/sqlite/`
Expected after edits: only `spi.ErrScanBudgetExhausted` occurrences remain.

- [ ] **Step 4: Run the plugin test to verify it passes**

Run: `cd plugins/sqlite && GOWORK=off go test ./... -run TestSearcher_ScanBudgetExhausted -v`
Expected: PASS.

- [ ] **Step 5: Run the full sqlite plugin suite**

Run: `cd plugins/sqlite && GOWORK=off go test ./...`
Expected: PASS (no other references to the removed var; if the `errors` import becomes unused in `searcher.go`, remove it).

- [ ] **Step 6: Commit**

```bash
git add plugins/sqlite/searcher.go plugins/sqlite/searcher_test.go
git commit -m "fix(sqlite): return spi.ErrScanBudgetExhausted so the engine maps it to 400

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 6: grouped-stats — service-level sentinel translation (transport-symmetric)

**Files:**
- Modify: `internal/domain/entity/grouped_stats_service.go`
- Modify: `internal/domain/entity/grouped_stats_handler.go`
- Test: `internal/domain/entity/grouped_stats_service_test.go`

**Interfaces:**
- Consumes: `common.Operational(...).WithCause`, `common.ErrCodeScanBudgetExhausted`, `spi.ErrScanBudgetExhausted`, `spi.ErrGroupCardinalityExceeded`, `entity.ErrBackendNotSupported`, `entity.ErrInvalidCondition`, `search.ErrInvalidFieldPath`, `search.ErrConditionTypeMismatch`.
- Produces: `QueryGroupedStats` now returns `*common.AppError` for the six known sentinels (each wrapping the sentinel via `WithCause`), raw error otherwise.

- [ ] **Step 1: Write the failing test — scan-budget from grouped-stats maps to 400 AppError, arbitrary error still raw**

Add to `internal/domain/entity/grouped_stats_service_test.go`:

```go
func TestQueryGroupedStats_ScanBudgetMapsTo400(t *testing.T) {
	// Iterable whose iter.Err() returns the scan-budget sentinel.
	svc, ctx, store, model, req := newStreamingStatsFixture(t, spi.ErrScanBudgetExhausted)
	_, err := svc.QueryGroupedStats(ctx, store, model, req)

	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("want *common.AppError, got %T: %v", err, err)
	}
	if appErr.Status != http.StatusBadRequest || appErr.Code != common.ErrCodeScanBudgetExhausted {
		t.Errorf("got %d/%q, want 400/%s", appErr.Status, appErr.Code, common.ErrCodeScanBudgetExhausted)
	}
	if !errors.Is(err, spi.ErrScanBudgetExhausted) {
		t.Errorf("WithCause must preserve the sentinel")
	}
}
```

For the arbitrary-error contract, extend the EXISTING `TestQueryGroupedStats_PushdownArbitraryErrorPropagates` assertion to confirm it is NOT an AppError (so the handler default still 500s):

```go
	// after the existing errors.Is(err, wantErr) assertion, add:
	var appErr *common.AppError
	if errors.As(err, &appErr) {
		t.Errorf("arbitrary storage error must NOT be wrapped as AppError (would become 4xx): %v", err)
	}
```

(`newStreamingStatsFixture` may not exist — if the suite has a streaming fake aggregator/iterable, reuse it and inject `iter.Err()` returning the sentinel; otherwise add a minimal `spi.Iterable` fake whose iterator returns the sentinel from `Err()`. Follow the existing `fakeAggregator`/streaming fakes in this test file.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/domain/entity/ -run 'TestQueryGroupedStats_(ScanBudgetMapsTo400|PushdownArbitraryErrorPropagates)' -v`
Expected: FAIL — scan-budget returns raw sentinel (not AppError); the arbitrary-error sub-assertion passes already (proving we won't regress it).

- [ ] **Step 3: Split QueryGroupedStats into inner logic + one classification site**

In `internal/domain/entity/grouped_stats_service.go`, rename the current exported method body to an unexported `queryGroupedStatsInner` (identical logic, same params/returns), and add a thin exported wrapper + classifier:

```go
// QueryGroupedStats runs the grouped-stats query and translates the known
// domain/SPI sentinels into client-facing *common.AppError before returning,
// so every transport (HTTP now, gRPC later) surfaces the documented status
// without a per-handler switch. Unknown storage/driver errors are returned
// unchanged and surface as 500 via the transport's Internal fallback.
func (s *GroupedStatsService) QueryGroupedStats(
	ctx context.Context, store spi.EntityStore, model spi.ModelRef, req *ValidatedGroupedStatsRequest,
) ([]GroupedStatsBucket, error) {
	buckets, err := s.queryGroupedStatsInner(ctx, store, model, req)
	if err != nil {
		return nil, classifyGroupedStatsError(err)
	}
	return buckets, nil
}

// classifyGroupedStatsError maps the six known sentinels to operational
// AppErrors (each wrapping the sentinel so errors.Is still holds); any other
// error is returned unchanged (→ 500 at the transport).
func classifyGroupedStatsError(err error) error {
	switch {
	case errors.Is(err, ErrBackendNotSupported):
		return common.Operational(http.StatusNotImplemented, "NOT_IMPLEMENTED_BY_BACKEND",
			"backend does not support grouped stats").WithCause(err)
	case errors.Is(err, spi.ErrGroupCardinalityExceeded):
		return common.Operational(http.StatusUnprocessableEntity, "GROUP_CARDINALITY_EXCEEDED",
			"group cardinality exceeds the configured maximum").WithCause(err)
	case errors.Is(err, spi.ErrScanBudgetExhausted):
		return common.Operational(http.StatusBadRequest, common.ErrCodeScanBudgetExhausted,
			"scan budget exhausted; narrow the query or add an indexable predicate").WithCause(err)
	case errors.Is(err, ErrInvalidCondition):
		return common.Operational(http.StatusBadRequest, common.ErrCodeInvalidCondition, err.Error()).WithCause(err)
	case errors.Is(err, search.ErrInvalidFieldPath):
		return common.Operational(http.StatusBadRequest, common.ErrCodeInvalidFieldPath, err.Error()).WithCause(err)
	case errors.Is(err, search.ErrConditionTypeMismatch):
		return common.Operational(http.StatusBadRequest, common.ErrCodeConditionTypeMismatch, err.Error()).WithCause(err)
	}
	return err
}
```

(Confirm the `common`, `search`, `spi`, `net/http`, `errors` imports are present in this file; add any missing. The status/code strings are copied verbatim from the current handler switch — no behaviour change for the five existing sentinels.)

- [ ] **Step 4: Collapse the handler switch to an AppError forward**

In `internal/domain/entity/grouped_stats_handler.go`, replace the whole `switch { case errors.Is(...) ... }` block (lines ~149-195) with the standard forward used by the search handler:

```go
	buckets, err := h.svc.QueryGroupedStats(r.Context(), store, model, validated)
	if err != nil {
		var appErr *common.AppError
		if errors.As(err, &appErr) {
			common.WriteError(w, r, appErr)
			return
		}
		common.WriteError(w, r, common.Internal("grouped-stats dispatch failed", err))
		return
	}
	common.WriteJSON(w, http.StatusOK, buckets)
```

Remove now-unused imports from the handler (e.g. `spi`, `search` if only the switch referenced them) — let `go build` guide you.

- [ ] **Step 5: Run the grouped-stats tests to verify they pass**

Run: `go test ./internal/domain/entity/ -run TestQueryGroupedStats -v`
Expected: PASS. The five existing `errors.Is(err, sentinel)` service tests still pass because `classifyGroupedStatsError` wraps each sentinel via `WithCause`. If any assert the raw error is NOT an AppError, that will now differ for the five known sentinels — update those specific assertions to check the AppError status/code (the sentinel is still reachable via `errors.Is`). Do NOT change `PushdownArbitraryErrorPropagates` (must stay non-AppError).

- [ ] **Step 6: Run the grouped-stats handler HTTP tests**

Run: `go test ./internal/domain/entity/ -run GroupedStats -v`
Expected: PASS — the HTTP handler tests still see 501/422/400 for the same inputs (behaviour preserved; translation site moved).

- [ ] **Step 7: Commit**

```bash
git add internal/domain/entity/grouped_stats_service.go \
        internal/domain/entity/grouped_stats_handler.go \
        internal/domain/entity/grouped_stats_service_test.go
git commit -m "refactor(entity): translate grouped-stats sentinels to AppError in the service

Moves the handler-level sentinel switch into QueryGroupedStats (transport-symmetric),
adds SCAN_BUDGET_EXHAUSTED mapping for the sqlite streaming path.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 7: Cross-backend parity — omitted-limit defaults to 1000

**Files:**
- Modify: `e2e/parity/search.go` (add `RunSearchOmittedLimitDefaults1000`)
- Modify: `e2e/parity/registry.go` (register in `allTests`)
- Modify: `e2e/parity/registry_count_test.go` (bump `wantParityScenarioCount`)

**Interfaces:**
- Consumes: `BackendFixture`, `client.NewClient`, `setupSearchModel`, `c.CreateEntity`, `c.SyncSearch` (NDJSON→`[]EntityResult`).

- [ ] **Step 1: Write the parity scenario (RED against the pre-fix engine, GREEN now)**

Add to `e2e/parity/search.go`:

```go
// RunSearchOmittedLimitDefaults1000 asserts that a direct search omitting the
// limit returns at most the documented default (1000), on every backend's
// Searcher pushdown path.
func RunSearchOmittedLimitDefaults1000(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)
	const modelName = "parity-search-omitted-limit-1000"
	const modelVersion = 1
	setupSearchModel(t, c, modelName, modelVersion)

	// Seed 1001 matching entities so the default 1000 is observably a cap.
	for i := 0; i < 1001; i++ {
		if _, err := c.CreateEntity(t, modelName, modelVersion,
			fmt.Sprintf(`{"name":"n%d","amount":1,"status":"new"}`, i)); err != nil {
			t.Fatalf("CreateEntity %d: %v", i, err)
		}
	}

	cond := `{"type":"simple","jsonPath":"$.status","operatorType":"EQUALS","value":"new"}`
	results, err := c.SyncSearch(t, modelName, modelVersion, cond) // no limit param
	if err != nil {
		t.Fatalf("SyncSearch: %v", err)
	}
	if len(results) != 1000 {
		t.Errorf("omitted-limit result count = %d, want 1000 (documented default cap)", len(results))
	}
}
```

(Confirm the exact `c.SyncSearch` signature and that it issues the request with NO `limit` query param — check `e2e/parity/client/http.go:1269`. If `SyncSearch` hard-codes a limit, add a `SyncSearchNoLimit` client method that omits it. Confirm `setupSearchModel`'s field schema includes `status`.)

- [ ] **Step 2: Register the scenario and bump the count gate**

In `e2e/parity/registry.go` `allTests`, add near the other search entries:

```go
	{"SearchOmittedLimitDefaults1000", RunSearchOmittedLimitDefaults1000},
```

In `e2e/parity/registry_count_test.go`, increment `wantParityScenarioCount` by 1.

- [ ] **Step 3: Run the memory parity backend (fast, no Docker)**

Run: `go test ./e2e/parity/memory/ -run 'TestParity/SearchOmittedLimitDefaults1000' -v`
Expected: PASS. (Also confirms `TestParityScenarioCount` passes with the bumped constant: `go test ./e2e/parity/ -run TestParityScenarioCount -v`.)

- [ ] **Step 4: Run the sqlite parity backend (fast, no Docker)**

Run: `go test ./e2e/parity/sqlite/ -run 'TestParity/SearchOmittedLimitDefaults1000' -v`
Expected: PASS.

- [ ] **Step 5: Run the postgres parity backend (Docker required)**

Run: `go test ./e2e/parity/postgres/ -run 'TestParity/SearchOmittedLimitDefaults1000' -v`
Expected: PASS. If Docker is unavailable in this environment, note it and defer this single run to CI (CI runs the postgres parity job); do not skip silently — record that it was deferred.

- [ ] **Step 6: Commit**

```bash
git add e2e/parity/search.go e2e/parity/registry.go e2e/parity/registry_count_test.go
git commit -m "test(parity): omitted direct-search limit caps at 1000 on every backend

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 8: sqlite running-backend e2e — SCAN_BUDGET_EXHAUSTED surfaces as 400

**Files:**
- Create/Modify: an sqlite running-backend test asserting HTTP + gRPC 400 `SCAN_BUDGET_EXHAUSTED` on a residual-scan search, plus grouped-stats.

**Rationale (coverage-gate note):** `SCAN_BUDGET_EXHAUSTED` is only produced by the sqlite backend (memory/postgres have no scan budget), so it is a **single-backend running e2e**, not a cross-backend parity scenario. `SEARCH_RESULT_LIMIT` is produced by **no** OSS backend (all SQL-truncate), so it has **no** running-backend test by construction — it is covered by the service unit test + HTTP/gRPC stub-Searcher transport tests in Task 4; this exception is intentional and recorded here.

- [ ] **Step 1: Locate the running-backend seam that lets the scan limit be lowered**

The scan budget only triggers when `SearchScanLimit` is small and the filter is non-pushable (regex). Two options — pick the one the existing e2e harness supports:
  - (a) An env knob: check whether cyoda-go reads `CYODA_SQLITE_SEARCH_SCAN_LIMIT` at startup (`plugins/sqlite/config.go:31` shows it does). The sqlite parity fixture sets sqlite env (`e2e/parity/sqlite/fixture.go:62-96`); a dedicated fixture variant with `CYODA_SQLITE_SEARCH_SCAN_LIMIT=3` makes a regex search over >3 rows fail.
  - (b) A plugin-level HTTP test that stands up the sqlite backend via `NewStoreFactoryForTestWithScanLimit` behind the search handler.

Prefer (a) if a per-test env override is feasible; otherwise (b). Inspect `e2e/parity/sqlite/fixture.go` and `internal/app` wiring to decide.

- [ ] **Step 2: Write the failing HTTP test (regex search over >budget rows → 400 SCAN_BUDGET_EXHAUSTED)**

Using the chosen seam, seed >3 entities, issue a direct search with a non-pushable regex condition on a data field, and assert:

```go
	// status 400, ProblemDetail properties.errorCode == "SCAN_BUDGET_EXHAUSTED"
	if status != http.StatusBadRequest { t.Fatalf("want 400, got %d; body=%s", status, body) }
	if !containsErrorCode(body, common.ErrCodeScanBudgetExhausted) {
		t.Errorf("want SCAN_BUDGET_EXHAUSTED, body=%s", body)
	}
```

(Regex condition shape: `{"type":"simple","jsonPath":"$.val","operatorType":"IMATCHES","value":".*"}` — confirm the exact non-pushable operator token the sqlite planner leaves as a residual, cross-referencing `TestSearcher_ScanBudgetExhausted`'s `spi.FilterMatchesRegex`.)

- [ ] **Step 3: Run to verify it fails (pre-Task-4/5 behavior would be 500)**

Run the new test.
Expected before Tasks 4+5 wired: 500; after: PASS (400). Since Tasks 4+5 are already committed, this should PASS once the seam is correct; if it 500s, the sentinel is not reaching the service as `spi.ErrScanBudgetExhausted` — recheck Task 5.

- [ ] **Step 4: Add the gRPC counterpart and a grouped-stats counterpart**

Mirror Step 2 for gRPC (`EntitySearchCollection`, assert `CLIENT_ERROR` + message contains `SCAN_BUDGET_EXHAUSTED`) and for grouped-stats (POST `/entity/{name}/{v}/stats/grouped` with a regex residual over >budget rows → 400 `SCAN_BUDGET_EXHAUSTED`).

- [ ] **Step 5: Run the sqlite e2e group**

Run the new tests.
Expected: PASS (all three: search HTTP, search gRPC, grouped-stats HTTP).

- [ ] **Step 6: Commit**

```bash
git add <new sqlite e2e test files>
git commit -m "test(e2e): sqlite scan-budget exhaustion surfaces as 400 SCAN_BUDGET_EXHAUSTED

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 9: Docs (Gate-4) + end-of-deliverable verification

**Files:**
- Modify: `CHANGELOG.md`
- Verify: `cmd/cyoda/help/content/search.md` (already documents the 1000 default — confirm wording still accurate, no change expected).

- [ ] **Step 1: Add CHANGELOG entries**

Under the v0.8.3 section of `CHANGELOG.md`, add compact entries (no issue numbers in the shipped changelog body unless the project convention includes them — match existing entries' style):

```markdown
### Fixed
- Direct search now applies the documented default limit (1000) when the client
  omits `limit`, on all storage backends (previously unbounded on the pushdown path).

### Added
- Storage backends can signal a bounded-search failure: `SEARCH_RESULT_LIMIT` (400)
  when a matched result set exceeds the cap, and `SCAN_BUDGET_EXHAUSTED` (400) when a
  non-indexable residual scan exceeds the backend's row budget (previously HTTP 500).
```

(Match the exact heading/format the file already uses; if it groups by type or by version differently, follow that.)

- [ ] **Step 2: Confirm search help topic wording**

Run: `grep -n "1000" cmd/cyoda/help/content/search.md`
Expected: existing "default 1000" lines are now accurate for the pushdown path. No edit needed unless wording implied the default was already enforced everywhere — if so, tighten it.

- [ ] **Step 3: Full engine build + vet**

Run: `GOWORK=off go build ./... && GOWORK=off go vet ./...`
Expected: success.

- [ ] **Step 4: Full engine test suite**

Run: `GOWORK=off go test ./...`
Expected: PASS. Investigate any failure before proceeding.

- [ ] **Step 5: Per-plugin test suites (separate modules)**

Run:
```bash
for m in plugins/memory plugins/postgres plugins/sqlite; do
  echo "== $m =="; (cd "$m" && GOWORK=off go test ./...) || echo "FAIL $m"
done
```
Expected: PASS for each (postgres may need Docker; if unavailable, note deferral to CI).

- [ ] **Step 6: Race detector (once, whole deliverable)**

Run: `GOWORK=off go test -race ./internal/domain/search/ ./internal/grpc/ ./internal/domain/entity/`
Expected: PASS, no race warnings.

- [ ] **Step 7: Error-code parity + count gates**

Run: `go test ./cmd/cyoda/... -run TestErrCode_Parity && go test ./e2e/parity/ -run TestParityScenarioCount`
Expected: PASS.

- [ ] **Step 8: Commit docs**

```bash
git add CHANGELOG.md
git commit -m "docs: changelog for direct-search limit default + bounded-fail 400s

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

- [ ] **Step 9: Push and open the PR**

```bash
git push -u origin fix/direct-search-limits-432-433
gh pr create --base release/v0.8.3 --title "fix(search): direct-search limit default + Searcher bounded-fail 400s" \
  --body "$(cat <<'EOF'
Implements #432 and #433 (+ adjacent sqlite scan-budget 500-bug) for release/v0.8.3.

- #432: omitted direct-search limit now defaults to 1000 (resolved at HTTP + gRPC
  entry points); the service treats Limit==0 as unbounded uniformly.
- #433: new SPI sentinel ErrSearchResultLimitExceeded → 400 SEARCH_RESULT_LIMIT,
  translated at the service so both transports surface it.
- Adjacent: ErrScanBudgetExhausted promoted to an SPI sentinel → new 400
  SCAN_BUDGET_EXHAUSTED (was 500 on both transports).
- Grouped-stats sentinel translation moved into the service (transport-symmetric).

SPI: two sentinels added on feat/schedule-function; all 4 go.mod repinned.

Closes #432
Closes #433

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
```

- [ ] **Step 10: Milestone both issues (release-branch invariant)**

```bash
gh issue edit 432 --milestone "v0.8.3"
gh issue edit 433 --milestone "v0.8.3"
```

---

## Self-Review Notes

- **Spec coverage:** Part A → Task 3; Part B (both sentinels) → Tasks 1,2,4,5; Part C → Task 6; error/status tables → Tasks 3,4,6,8; coverage matrix → Tasks 3,4,6,7,8 (with the two explicit running-backend exceptions recorded in Task 8: `SEARCH_RESULT_LIMIT` has no OSS producer; `SCAN_BUDGET_EXHAUSTED` is sqlite-only). Out-of-scope backstop sentinels → no task (documented in spec).
- **SPI-first ordering:** Task 1 lands + repins before any engine code references the sentinels.
- **Type consistency:** `DefaultDirectSearchLimit` (Task 3) is referenced by the same name in handler and gRPC; `WithCause`/`ErrCodeScanBudgetExhausted` (Task 2) are referenced by Tasks 4 and 6; `classifyGroupedStatsError` is internal to Task 6.
- **No behavioural change for the 5 existing grouped-stats sentinels** — statuses/codes copied verbatim; only the translation site moves; `WithCause` keeps `errors.Is` green; arbitrary-error 500 explicitly protected.
