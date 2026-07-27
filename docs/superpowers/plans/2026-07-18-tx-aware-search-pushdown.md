# Tx-aware Search Pushdown Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop gating storage-plugin search pushdown on `tx == nil`; make `spi.Searcher` tx-aware by contract so in-transaction search overlays the tx write-set on index-pushed candidates instead of scanning the whole model via `GetAll`.

**Architecture:** Three canonical search helpers (`MatchFilter`, `LessByOrder`, `MergePage`) move into the core `spi` package, collapsing a hand-synced triple implementation. `sqlite`/`postgres`/`memory` gain a tx-aware `Search`: committed candidates stream from the index in `ORDER BY` order and are merged with the in-process `tx.Buffer`/`tx.Deletes` via a bounded streaming merge. Read-set participation of an in-tx search is an explicit opt-in query flag `TrackingRead` (default off = cheap snapshot predicate read; on = returned rows enter the read-set for FCW validation). The engine drops the `tx == nil` gate; `GetAll`+`match` survives only for the condition-translation-failure path.

**Tech Stack:** Go 1.26, `github.com/tidwall/gjson`, `github.com/cyoda-platform/cyoda-go-spi` (coordinated release), PostgreSQL/SQLite testcontainers, CloudEvents gRPC.

## Global Constraints

- **SI+FCW contract, entity-granular** (`docs/CONSISTENCY.md` §1). In-tx search is a predicate read: phantom write-skew is out of contract (§3/§7.3). Do not weaken or over-claim isolation.
- **RYW parity is the acceptance bar:** in-tx `Search` results MUST equal `GetAll`+`match` for the same tx state.
- **`TrackingRead` default `false`** everywhere (domain, spi, HTTP, gRPC). No-op when no active tx.
- **memory/sqlite/postgres in-tx search must not full-scan the model** for indexable predicates.
- **PIT-in-tx stays committed-only** (matches `GetAllAsAt`, "always reads committed data").
- **No backend divergence** (`docs/CLAUDE.md`): a backend differing on the same contract is a bug; parity scenarios guard it.
- **Multi-node primary** (`.claude/rules/multi-node-primary.md`): in-tx `Searcher.Search` requires co-location with the tx owner; state and test it, don't descope.
- **SPI coordinated release** (memory: `feedback_spi_coordinated_release_procedure`, MAINTAINING.md): SPI changes land on `cyoda-go-spi` main; local composition via `go.work` (skip-worktree), **never** committed `replace`; re-pin pseudo-version across all 4 `go.mod` in ONE commit. No per-issue SPI tag (v0.8.3 window is open; one tag at milestone end).
- **Mutex discipline** (`.claude/rules/go-mutex-discipline.md`): `RLock`/`RUnlock` via `defer`, IIFE for early release. Lock order `tx.OpMu` → store mutex.
- **No issue IDs in shipped artefacts** (memory: `feedback_no_issue_ids_in_code`): no `#420` in code/OpenAPI/help/errors/response bodies; issue IDs only in commits/PR/spec.
- **Logging:** `log/slog` only, never log payloads at INFO.
- **Race:** `make race` once before PR, not per-step.

## Source of truth

Spec: `docs/superpowers/specs/2026-07-18-tx-aware-search-pushdown-design.md`. Read it before starting.

---

## File Structure

**cyoda-go-spi (separate module, local checkout `/Users/paul/go-projects/cyoda-light/cyoda-go-spi`):**
- Create `filter_match.go` — `MatchFilter` + leaf/extract/compare/like helpers (ported from cyoda-go `internal/match/match.go:174-489`).
- Create `filter_match_test.go` — ported + new cases.
- Create `order_compare.go` — `LessByOrder` (ported from `internal/domain/search/ordersort.go`).
- Create `order_compare_test.go`.
- Create `merge_page.go` — `MergePage`.
- Create `merge_page_test.go`.
- Modify `searcher.go` — add `SearchOptions.TrackingRead bool`; tighten `Searcher` godoc.
- Modify `go.mod` — add `github.com/tidwall/gjson`.

**cyoda-go root module:**
- Modify `internal/match/match.go` — `MatchFilter` delegates to `spi.MatchFilter`.
- Modify `internal/domain/search/ordersort.go` — `lessByKey`/`sortEntities` delegate to `spi.LessByOrder`.
- Modify `internal/domain/search/service.go` — drop `tx == nil` gate; thread `TrackingRead`; `SearchOptions.TrackingRead`.
- Modify `internal/domain/search/handler.go` — parse `trackingRead` into `SearchOptions`.
- Modify `internal/grpc/search.go` — decode `trackingRead` from the CloudEvent search payload.
- Modify `api/openapi.yaml` — `trackingRead` on the sync search request schema.
- Modify `docs/CONSISTENCY.md`, `cmd/cyoda/help/content/...`, `docs/cloud-parity/`, `COMPATIBILITY.md`, `CHANGELOG.md`.
- Create `e2e/parity/tx_search_ryw.go` (+ register in `e2e/parity/registry.go`).
- Create `internal/e2e/search_intx_test.go`, `internal/e2e/search_intx_tracking_test.go`.
- Create cluster e2e case in the existing multi-node e2e file.

**Plugins (own go.mod each):**
- Modify `plugins/sqlite/searcher.go`, `plugins/sqlite/post_filter.go`.
- Modify `plugins/postgres/searcher.go`, `plugins/postgres/grouped_stats.go`.
- Create `plugins/memory/searcher.go` (+ `plugins/memory/searcher_test.go`).

---

## Phase 1 — SPI canonical helpers (local `go.work` composition)

### Task 0: Compose the local SPI into the workspace

**Files:**
- Modify (skip-worktree, do NOT commit): `go.work`

- [ ] **Step 1: Add the local SPI to `go.work` without committing it**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/feat-420-tx-aware-search-pushdown
go work edit -use=/Users/paul/go-projects/cyoda-light/cyoda-go-spi
git update-index --skip-worktree go.work
git diff --stat   # go.work shows as modified locally but skip-worktree hides it from commits
```

- [ ] **Step 2: Verify composition resolves**

Run: `go list -m github.com/cyoda-platform/cyoda-go-spi`
Expected: prints `github.com/cyoda-platform/cyoda-go-spi => /Users/paul/go-projects/cyoda-light/cyoda-go-spi`

- [ ] **Step 3: Baseline the SPI repo is clean**

Run: `cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && git status && go test ./...`
Expected: clean tree, tests pass.

No commit (workspace-only setup).

---

### Task 1: `spi.MatchFilter` + gjson dependency

**Files (in `cyoda-go-spi`):**
- Create: `filter_match.go`
- Test: `filter_match_test.go`
- Modify: `go.mod`

**Interfaces:**
- Produces: `func MatchFilter(f Filter, data []byte, meta EntityMeta) bool` — zero-value filter (empty `Op`) matches all; explicit empty `FilterAnd`→true, empty `FilterOr`→false; unsupported op → non-match (no error). Semantics identical to cyoda-go `internal/match.MatchFilter`.

- [ ] **Step 1: Add gjson to the SPI module**

Run in `/Users/paul/go-projects/cyoda-light/cyoda-go-spi`:
```bash
go get github.com/tidwall/gjson@latest
```

- [ ] **Step 2: Write the failing test**

Create `filter_match_test.go`. Port the table of cases exercised by cyoda-go's `internal/match` filter tests, plus these explicit cases:

```go
package spi_test

import (
	"testing"
	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func meta(id, state string) spi.EntityMeta { return spi.EntityMeta{ID: id, State: state} }

func TestMatchFilter_ZeroValueMatchesAll(t *testing.T) {
	if !spi.MatchFilter(spi.Filter{}, []byte(`{"a":1}`), meta("e1", "S")) {
		t.Fatal("zero-value filter must match all")
	}
}

func TestMatchFilter_EmptyAndIsTrue_EmptyOrIsFalse(t *testing.T) {
	if !spi.MatchFilter(spi.Filter{Op: spi.FilterAnd}, []byte(`{}`), meta("e1", "S")) {
		t.Fatal("empty AND is identity true")
	}
	if spi.MatchFilter(spi.Filter{Op: spi.FilterOr}, []byte(`{}`), meta("e1", "S")) {
		t.Fatal("empty OR is identity false")
	}
}

func TestMatchFilter_EqAndContainsAndMeta(t *testing.T) {
	data := []byte(`{"name":"alpha","n":7}`)
	eq := spi.Filter{Op: spi.FilterEq, Source: spi.SourceData, Path: "name", Value: "alpha"}
	if !spi.MatchFilter(eq, data, meta("e1", "S")) {
		t.Fatal("eq should match")
	}
	gt := spi.Filter{Op: spi.FilterGt, Source: spi.SourceData, Path: "n", Value: 3}
	if !spi.MatchFilter(gt, data, meta("e1", "S")) {
		t.Fatal("gt numeric should match")
	}
	mstate := spi.Filter{Op: spi.FilterEq, Source: spi.SourceMeta, Path: "state", Value: "ACTIVE"}
	if !spi.MatchFilter(mstate, data, meta("e1", "ACTIVE")) {
		t.Fatal("meta state eq should match")
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && go test ./ -run TestMatchFilter`
Expected: FAIL — `undefined: spi.MatchFilter`.

- [ ] **Step 4: Port the implementation**

Create `filter_match.go` in package `spi`. Port verbatim the block `internal/match/match.go:197-489` (functions `MatchFilter`, `evalFilter`, `evalLeafFilter`, `extractFilterValue`, `extractFilterDataValue`, `extractFilterMetaValue`, `timeToMicro`, `compareFilterValues`, `toFilterFloat64`, `matchFilterLike`, `matchFilterLikeHelper`, `toGjsonResult`) and the `opMatchesPattern` regex helper it calls (from `internal/match/operators.go` — port only what `evalLeafFilter` needs for `FilterMatchesRegex`). Change `package match` → `package spi`, drop the `spi.` qualifier on `Filter`/`EntityMeta`/`FilterEq…`, keep `gjson` import. Do not alter any comparison semantics — this is a verbatim move.

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./ -run TestMatchFilter -v`
Expected: PASS.

- [ ] **Step 6: Commit (in the SPI repo)**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git add filter_match.go filter_match_test.go go.mod go.sum
git commit -m "feat: add canonical spi.MatchFilter (Filter evaluator)"
```

---

### Task 2: `spi.LessByOrder`

**Files (in `cyoda-go-spi`):**
- Create: `order_compare.go`, `order_compare_test.go`

**Interfaces:**
- Produces: `func LessByOrder(a, b *Entity, specs []OrderSpec) bool` — total order matching each plugin's SQL `ORDER BY`: per-spec Kind comparison (Numeric lenient-float, Temporal ms-floor, Bool, Text byte-order), NULLS-LAST (missing sorts after present regardless of Desc), `entity_id` ascending byte-order tiebreaker unless the terminal spec already resolves to `entity_id`.

- [ ] **Step 1: Write the failing test**

Create `order_compare_test.go` with cases: numeric asc/desc, missing-value NULLS-LAST under both directions, temporal ms-floor tie, and the `entity_id` tiebreaker when all keys tie:

```go
package spi_test

import (
	"testing"
	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func ent(id string, data string) *spi.Entity {
	return &spi.Entity{Data: []byte(data), Meta: spi.EntityMeta{ID: id}}
}

func TestLessByOrder_TiebreakByEntityID(t *testing.T) {
	specs := []spi.OrderSpec{{Path: "n", Source: spi.SourceData, Kind: spi.OrderNumeric}}
	a, b := ent("aaa", `{"n":1}`), ent("bbb", `{"n":1}`)
	if !spi.LessByOrder(a, b, specs) {
		t.Fatal("equal keys must break by entity_id asc")
	}
}

func TestLessByOrder_NullsLast(t *testing.T) {
	specs := []spi.OrderSpec{{Path: "n", Source: spi.SourceData, Kind: spi.OrderNumeric}}
	present, missing := ent("a", `{"n":5}`), ent("b", `{}`)
	if !spi.LessByOrder(present, missing, specs) {
		t.Fatal("present must sort before missing (NULLS LAST) ascending")
	}
	descSpecs := []spi.OrderSpec{{Path: "n", Source: spi.SourceData, Kind: spi.OrderNumeric, Desc: true}}
	if !spi.LessByOrder(present, missing, descSpecs) {
		t.Fatal("present must still sort before missing (NULLS LAST) descending")
	}
}
```

- [ ] **Step 2: Run to verify fail**

Run: `go test ./ -run TestLessByOrder`
Expected: FAIL — `undefined: spi.LessByOrder`.

- [ ] **Step 3: Port the comparator**

Create `order_compare.go` in package `spi`. Port the comparison logic from `internal/domain/search/ordersort.go` (`sortEntities`/`lessByKey` and their leaf extractors `dataLeaf`/`metaLeaf` and the Kind coercions), exposing a single `LessByOrder(a, b *Entity, specs []OrderSpec) bool`. Preserve the NULLS-LAST and `entity_id` tiebreaker rules exactly (they mirror the SQL in `plugins/sqlite/searcher.go:orderByFieldExpr` and `plugins/postgres/searcher.go:orderByFieldExpr`). Reuse `timeToMicro` from `filter_match.go` for Temporal.

- [ ] **Step 4: Run to verify pass**

Run: `go test ./ -run TestLessByOrder -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add order_compare.go order_compare_test.go
git commit -m "feat: add canonical spi.LessByOrder (OrderSpec comparator)"
```

---

### Task 3: `spi.MergePage`

**Files (in `cyoda-go-spi`):**
- Create: `merge_page.go`, `merge_page_test.go`

**Interfaces:**
- Consumes: `LessByOrder`.
- Produces: `func MergePage(next func() (*Entity, bool, error), adds []*Entity, deleted func(id string) bool, specs []OrderSpec, offset, limit int) ([]*Entity, error)`.
  - `next` yields the committed source in `LessByOrder` order (lazy pull; `(nil,false,nil)` = exhausted).
  - `adds` MUST already be sorted by `LessByOrder`.
  - `deleted(id)` true ⇒ the committed row is skipped.
  - Rows from `next` whose id is `deleted` are dropped; `adds` are interleaved in order; result is the `[offset, offset+limit)` window. `limit<=0` = unbounded. Early-stop once `offset+limit` survivors are gathered when `limit>0`.

- [ ] **Step 1: Write the failing test**

```go
package spi_test

import (
	"testing"
	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func slcNext(es []*spi.Entity) func() (*spi.Entity, bool, error) {
	i := 0
	return func() (*spi.Entity, bool, error) {
		if i >= len(es) {
			return nil, false, nil
		}
		e := es[i]
		i++
		return e, true, nil
	}
}
func none(string) bool { return false }

func TestMergePage_InterleavesAddsInOrder(t *testing.T) {
	specs := []spi.OrderSpec{{Path: "n", Source: spi.SourceData, Kind: spi.OrderNumeric}}
	committed := []*spi.Entity{ent("a", `{"n":1}`), ent("c", `{"n":3}`)}
	adds := []*spi.Entity{ent("b", `{"n":2}`)}
	got, err := spi.MergePage(slcNext(committed), adds, none, specs, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{got[0].Meta.ID, got[1].Meta.ID, got[2].Meta.ID}
	if ids[0] != "a" || ids[1] != "b" || ids[2] != "c" {
		t.Fatalf("want [a b c], got %v", ids)
	}
}

func TestMergePage_SkipsDeletedAndPagesWithOffsetLimit(t *testing.T) {
	specs := []spi.OrderSpec{{Path: "n", Source: spi.SourceData, Kind: spi.OrderNumeric}}
	committed := []*spi.Entity{ent("a", `{"n":1}`), ent("b", `{"n":2}`), ent("c", `{"n":3}`), ent("d", `{"n":4}`)}
	del := func(id string) bool { return id == "b" }
	got, err := spi.MergePage(slcNext(committed), nil, del, specs, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Meta.ID != "c" {
		t.Fatalf("want [c] after skipping b, offset 1 limit 1, got %v", got)
	}
}
```

- [ ] **Step 2: Run to verify fail**

Run: `go test ./ -run TestMergePage`
Expected: FAIL — `undefined: spi.MergePage`.

- [ ] **Step 3: Implement**

Create `merge_page.go` in package `spi`:

```go
package spi

// MergePage performs a bounded k-way merge of a sorted committed source
// (next, lazy pull) with a pre-sorted adds slice, skipping committed rows
// for which deleted(id) is true, ordered by LessByOrder(specs). It returns
// the [offset, offset+limit) window. limit<=0 means unbounded. Memory is
// bounded to ~offset+limit+len(adds): the committed source is pulled lazily
// and the merge early-stops once enough survivors are gathered.
func MergePage(next func() (*Entity, bool, error), adds []*Entity, deleted func(id string) bool, specs []OrderSpec, offset, limit int) ([]*Entity, error) {
	need := -1
	if limit > 0 {
		need = offset + limit
	}
	out := make([]*Entity, 0, 16)
	ai := 0
	// pull the next non-deleted committed row (buffered one-ahead)
	var cur *Entity
	advance := func() error {
		for {
			e, ok, err := next()
			if err != nil {
				return err
			}
			if !ok {
				cur = nil
				return nil
			}
			if deleted != nil && deleted(e.Meta.ID) {
				continue
			}
			cur = e
			return nil
		}
	}
	if err := advance(); err != nil {
		return nil, err
	}
	for {
		haveC := cur != nil
		haveA := ai < len(adds)
		if !haveC && !haveA {
			break
		}
		var take *Entity
		switch {
		case haveC && haveA:
			if LessByOrder(adds[ai], cur, specs) {
				take = adds[ai]
				ai++
			} else {
				take = cur
				if err := advance(); err != nil {
					return nil, err
				}
			}
		case haveA:
			take = adds[ai]
			ai++
		default:
			take = cur
			if err := advance(); err != nil {
				return nil, err
			}
		}
		out = append(out, take)
		if need >= 0 && len(out) >= need {
			break
		}
	}
	if offset > 0 {
		if offset >= len(out) {
			return nil, nil
		}
		out = out[offset:]
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./ -run TestMergePage -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add merge_page.go merge_page_test.go
git commit -m "feat: add spi.MergePage (bounded streaming merge)"
```

---

### Task 4: `SearchOptions.TrackingRead` + `Searcher` godoc

**Files (in `cyoda-go-spi`):**
- Modify: `searcher.go`

**Interfaces:**
- Produces: `SearchOptions.TrackingRead bool`; tightened `Searcher` contract godoc.

- [ ] **Step 1: Add the field and contract godoc**

In `searcher.go`, add to `SearchOptions`:
```go
	// TrackingRead, when true and a transaction is active, records the
	// entities this search returns into the transaction's read-set, so
	// commit-time first-committer-wins validates them (a FOR-SHARE / locking
	// read, implemented optimistically). Default false: a plain snapshot
	// predicate read that records nothing. No-op when no transaction is
	// active. In-transaction search never prevents phantoms regardless of
	// this flag (see docs/CONSISTENCY.md).
	TrackingRead bool
```
Extend the `Searcher` godoc: `Search` MUST honour an active transaction (read-your-own-writes) — with no tx it is a committed pushdown; with a tx it overlays the tx write-set to produce results identical to `GetAll`+in-memory match. In-tx point-in-time reads are committed-only. Returned entities enter the read-set only when `SearchOptions.TrackingRead` is set.

- [ ] **Step 2: Verify it builds**

Run: `cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && go build ./... && go test ./...`
Expected: builds, tests pass.

- [ ] **Step 3: Commit**

```bash
git add searcher.go
git commit -m "feat: add SearchOptions.TrackingRead; document tx-aware Searcher contract"
```

---

### Task 5: Re-point `internal/match.MatchFilter` at `spi.MatchFilter`

**Files:**
- Modify: `internal/match/match.go`

**Interfaces:**
- Consumes: `spi.MatchFilter`.

- [ ] **Step 1: Run the existing parity smoke test (baseline green)**

Run: `cd <worktree> && go test ./internal/match/... ./e2e/parity/... -run MatchFilter`
Expected: PASS (pre-change baseline).

- [ ] **Step 2: Delegate**

In `internal/match/match.go`, replace the body of `MatchFilter` (and delete the now-duplicated `evalFilter`/`evalLeafFilter`/`extractFilter*`/`compareFilterValues`/`toFilterFloat64`/`matchFilterLike*`/`toGjsonResult`/`timeToMicro` helpers, migrating any still referenced by the `Match`/`predicate.Condition` path only if they are shared — verify with `grep`) so `MatchFilter` calls `spi.MatchFilter`:
```go
func MatchFilter(f spi.Filter, data []byte, meta spi.EntityMeta) bool {
	return spi.MatchFilter(f, data, meta)
}
```
Keep helpers still used by the `Match(predicate.Condition,...)` path. Remove only those exclusively used by the deleted Filter path.

- [ ] **Step 3: Run tests**

Run: `go test ./internal/match/... ./e2e/parity/... -v`
Expected: PASS (semantics unchanged — same code, now shared).

- [ ] **Step 4: Commit**

```bash
git add internal/match/match.go
git commit -m "refactor: delegate internal/match.MatchFilter to spi.MatchFilter"
```

---

### Task 6: Re-point plugin filter evaluators + engine comparator

**Files:**
- Modify: `plugins/sqlite/post_filter.go`, `plugins/postgres/grouped_stats.go`, `internal/domain/search/ordersort.go`

**Interfaces:**
- Consumes: `spi.MatchFilter`, `spi.LessByOrder`.

- [ ] **Step 1: Baseline green**

Run: `go test ./internal/domain/search/... && (cd plugins/sqlite && go test ./...) && (cd plugins/postgres && go test ./...)`
Expected: PASS.

- [ ] **Step 2: Delegate sqlite**

In `plugins/sqlite/post_filter.go`, make `evaluateFilter(f spi.Filter, entity *spi.Entity) (bool, error)` call `spi.MatchFilter(f, entity.Data, entity.Meta)` (return `nil` error) and remove the now-dead `evaluateLeaf` + helpers exclusive to it. Keep the exported signature stable for existing callers.

- [ ] **Step 3: Delegate postgres**

In `plugins/postgres/grouped_stats.go`, make `evalPostFilter(f spi.Filter, entity *spi.Entity, doc []byte) (bool, error)` call `spi.MatchFilter(f, doc, entity.Meta)` and remove dead helpers exclusive to it.

- [ ] **Step 4: Delegate engine comparator**

In `internal/domain/search/ordersort.go`, make `sortEntities(entities, specs)` sort via `sort.SliceStable` using `spi.LessByOrder(entities[i], entities[j], specs)`; delete `lessByKey` and its leaf helpers now living in the SPI.

- [ ] **Step 5: Run tests**

Run: `go test ./internal/domain/search/... && (cd plugins/sqlite && go test ./...) && (cd plugins/postgres && go test ./...)`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add plugins/sqlite/post_filter.go plugins/postgres/grouped_stats.go internal/domain/search/ordersort.go
git commit -m "refactor: route plugin filter eval + engine sort through spi helpers"
```

---

## Phase 2 — memory becomes a Searcher

### Task 7: `plugins/memory` `Search`

**Files:**
- Create: `plugins/memory/searcher.go`, `plugins/memory/searcher_test.go`

**Interfaces:**
- Consumes: `spi.MatchFilter`, `spi.LessByOrder`, `spi.MergePage`.
- Produces: `*EntityStore` implements `spi.Searcher`.

- [ ] **Step 1: Write failing tests (RYW + non-tx parity + TrackingRead)**

Create `plugins/memory/searcher_test.go` covering: non-tx `Search` equals `GetAll`+`MatchFilter`; in-tx created-in-T match present; in-tx deleted-in-T absent; in-tx buffered-no-longer-matches absent; in-tx PIT committed-only; `TrackingRead=true` records returned ids into `tx.ReadSet`, `false` records none. Use the existing memory test factory helpers (see `plugins/memory/entity_store_test.go` for `newTestStore`/tx begin patterns).

```go
func TestMemorySearch_InTx_CreatedInTxMatchPresent(t *testing.T) {
	// begin tx, Save an entity matching status=ACTIVE into the buffer,
	// Search(status=ACTIVE); expect the buffered entity present.
	// (mirror the assertion of GetAll+match)
}
func TestMemorySearch_TrackingRead_RecordsReturnedOnly(t *testing.T) {
	// two committed entities: one matches, one doesn't.
	// Search(match, TrackingRead:true) in tx → tx.ReadSet contains only the match id.
	// Search(match, TrackingRead:false) in a fresh tx → tx.ReadSet empty.
}
```

- [ ] **Step 2: Run to verify fail**

Run: `cd plugins/memory && go test ./ -run TestMemorySearch`
Expected: FAIL — `*EntityStore` has no `Search`.

- [ ] **Step 3: Implement `Search`**

Create `plugins/memory/searcher.go`. `var _ spi.Searcher = (*EntityStore)(nil)`. Implement:
- Non-tx and in-tx PIT: iterate the committed model snapshot (reuse `getAllSnapshotUnlocked` for PIT, current-state iteration otherwise), keep rows where `spi.MatchFilter(filter, e.Data, e.Meta)`, sort with `spi.LessByOrder`, page with offset/limit. No read-set for PIT.
- In-tx, PIT==nil: build `committed` = sorted matching snapshot rows (as a slice → wrap in a `next` closure); build sorted `adds` from `tx.Buffer` (`ModelRef` match, `!tx.Deletes[id]`, `spi.MatchFilter`); `deleted(id)=tx.Deletes[id] || _,ok:=tx.Buffer[id]`; call `spi.MergePage`. All `tx.Buffer`/`tx.Deletes` reads under `tx.OpMu.RLock` (IIFE), lock order `tx.OpMu`→`entityMu`, fail fast on `tx.RolledBack`.
- If `opts.TrackingRead && tx != nil`: for each returned entity whose id is NOT in `tx.Buffer` (committed rows), set `tx.ReadSet[id]=true` under the RLock.

- [ ] **Step 4: Run tests**

Run: `go test ./ -run TestMemorySearch -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add plugins/memory/searcher.go plugins/memory/searcher_test.go
git commit -m "feat: memory entity store implements tx-aware spi.Searcher"
```

---

## Phase 3 — sqlite tx-aware Search

### Task 8: sqlite in-tx overlay (PIT==nil)

**Files:**
- Modify: `plugins/sqlite/searcher.go`
- Test: `plugins/sqlite/searcher_tx_test.go` (create)

**Interfaces:**
- Consumes: `spi.MatchFilter`, `spi.LessByOrder`, `spi.MergePage`, `spi.GetTransaction`.

- [ ] **Step 1: Write failing tests**

Create `plugins/sqlite/searcher_tx_test.go`: begin a tx on the sqlite store, buffer a create/update/delete, and assert `Search` RYW parity with `GetAll`+`MatchFilter`; assert a narrow-predicate in-tx search over a large model stays within `SearchScanLimit` (does not full-scan); assert `TrackingRead` records returned committed ids into `tx.ReadSet`.

- [ ] **Step 2: Run to verify fail**

Run: `cd plugins/sqlite && go test ./ -run TestSearchTx`
Expected: FAIL (in-tx overlay not implemented — current `Search` ignores the tx).

- [ ] **Step 3: Implement the in-tx branch**

In `plugins/sqlite/searcher.go` `Search`, at the top read `tx := spi.GetTransaction(ctx)`. When `tx != nil && opts.PointInTime == nil`:
- Build the committed candidate SQL exactly as today but **without** the SQL `LIMIT`/`OFFSET` push (always stream in `ORDER BY` order; keep the residual post-filter loop and `SearchScanLimit`).
- Wrap `rows` iteration in a `next func() (*spi.Entity, bool, error)` that scans one row, applies the residual post-filter, and yields matches.
- Under `tx.OpMu.RLock` (IIFE; fail fast on `tx.RolledBack`; lock order `tx.OpMu`→store mutex), read `tx.Buffer`/`tx.Deletes`; build sorted `adds` (`ModelRef` match, `!tx.Deletes`, `spi.MatchFilter`); `deleted(id)=tx.Deletes[id]||inBuffer(id)`.
- `results, err := spi.MergePage(next, adds, deleted, opts.OrderBy, opts.Offset, opts.Limit)`.
- If `opts.TrackingRead`: `tx.ReadSet[id]=true` for each returned committed id (id ∉ buffer) under the RLock.
Leave the `tx == nil` and `tx != nil && PIT != nil` (Task 9) paths on their branches.

- [ ] **Step 4: Run tests**

Run: `go test ./ -run TestSearchTx -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add plugins/sqlite/searcher.go plugins/sqlite/searcher_tx_test.go
git commit -m "feat: sqlite tx-aware Search overlay (buffer merge, TrackingRead)"
```

---

### Task 9: sqlite in-tx PIT committed-only pushdown

**Files:**
- Modify: `plugins/sqlite/searcher.go`
- Test: `plugins/sqlite/searcher_tx_test.go`

- [ ] **Step 1: Write failing test**

Add a test: begin a tx, buffer a write, `Search` with `PointInTime` set to before the write → result excludes the buffered write (committed-only) and equals `GetAllAsAt`+`MatchFilter`; assert no `tx.ReadSet` entries (PIT records none). Also assert the query does not route through `GetAllAsAt` full-scan (narrow predicate stays within scan budget).

- [ ] **Step 2: Run to verify fail**

Run: `go test ./ -run TestSearchTxPIT`
Expected: FAIL — in-tx PIT currently would need the engine `GetAllAsAt` path; here we assert the plugin pushdown.

- [ ] **Step 3: Implement**

In `Search`, when `tx != nil && opts.PointInTime != nil`: run the existing committed PIT pushdown (`searchPointInTimeBase` path) unchanged — no overlay, no read-set. (This branch is reached now that the engine de-guards in Task 16; ensure it compiles and behaves identically to the `tx == nil` PIT path.)

- [ ] **Step 4: Run tests**

Run: `go test ./ -run TestSearchTx -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add plugins/sqlite/searcher.go plugins/sqlite/searcher_tx_test.go
git commit -m "feat: sqlite in-tx PIT search is committed-only pushdown"
```

---

## Phase 4 — postgres tx-aware Search

### Task 10: postgres in-tx overlay (PIT==nil)

**Files:**
- Modify: `plugins/postgres/searcher.go`
- Test: `plugins/postgres/searcher_tx_test.go` (create)

**Interfaces:**
- Consumes: `spi.MatchFilter`, `spi.MergePage`, `spi.GetTransaction`, `TransactionManager.recordReadIfInTx`.

- [ ] **Step 1: Write failing tests**

Create `plugins/postgres/searcher_tx_test.go` (testcontainers — mirror `plugins/postgres/searcher_test.go` setup): RYW parity with `GetAll`+`MatchFilter` for buffered create/update/delete; `TrackingRead=true` records returned ids into the postgres read-set (assert via `txState.RecordRead` effect / a subsequent conflicting commit aborts); `TrackingRead=false` records none (a concurrent write to a returned entity does NOT abort).

- [ ] **Step 2: Run to verify fail**

Run: `cd plugins/postgres && go test ./ -run TestSearchTx`
Expected: FAIL.

- [ ] **Step 3: Implement the in-tx branch**

In `plugins/postgres/searcher.go` `Search`, add `tx := spi.GetTransaction(ctx)`. When `tx != nil && opts.PointInTime == nil`: stream committed candidates via the existing `postgresIter` **without** the SQL `LIMIT`/`OFFSET` push; wrap `it` in a `next` closure; under `tx.OpMu.RLock` build sorted `adds`/`deleted` from `tx.Buffer`/`tx.Deletes` (same shape as sqlite Task 8); `spi.MergePage(...)`. If `opts.TrackingRead`: call the transaction manager's `recordReadIfInTx(ctx, id, version)` for each returned committed entity (record its observed version). Access the manager via the store's existing handle (see how `Save`/`Get` reach `recordReadIfInTx` in `plugins/postgres/entity_store.go`).

- [ ] **Step 4: Run tests**

Run: `go test ./ -run TestSearchTx -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add plugins/postgres/searcher.go plugins/postgres/searcher_tx_test.go
git commit -m "feat: postgres tx-aware Search overlay (buffer merge, TrackingRead)"
```

---

### Task 11: postgres in-tx PIT committed-only pushdown

**Files:**
- Modify: `plugins/postgres/searcher.go`
- Test: `plugins/postgres/searcher_tx_test.go`

- [ ] **Step 1: Write failing test**

Add: in-tx `Search` with `PointInTime` before a buffered write excludes the write (committed-only), equals `GetAllAsAt`+`MatchFilter`, records no read.

- [ ] **Step 2: Run to verify fail**

Run: `go test ./ -run TestSearchTxPIT`
Expected: FAIL.

- [ ] **Step 3: Implement**

When `tx != nil && opts.PointInTime != nil`: run the existing committed PIT pushdown unchanged (no overlay, no read-set).

- [ ] **Step 4: Run tests / Commit**

Run: `go test ./ -run TestSearchTx -v` → PASS.
```bash
git add plugins/postgres/searcher.go plugins/postgres/searcher_tx_test.go
git commit -m "feat: postgres in-tx PIT search is committed-only pushdown"
```

---

## Phase 5 — TrackingRead plumbing + engine de-guard

### Task 12: domain `SearchOptions.TrackingRead` → spi

**Files:**
- Modify: `internal/domain/search/service.go`

- [ ] **Step 1: Write failing test**

In `internal/domain/search/service_test.go`, add a test that `Search` with `SearchOptions{TrackingRead:true}` passes `TrackingRead:true` into the `spi.Searcher` call. Use the existing spy/fake Searcher pattern in that test file (or add one that records the received `spi.SearchOptions`).

- [ ] **Step 2: Run to verify fail**

Run: `go test ./internal/domain/search/ -run TrackingRead`
Expected: FAIL — field absent / not threaded.

- [ ] **Step 3: Implement**

Add `TrackingRead bool` to the domain `SearchOptions` struct (service.go:30). In the pushdown branch, set `TrackingRead: opts.TrackingRead` on the `spi.SearchOptions` literal.

- [ ] **Step 4: Run / Commit**

Run: `go test ./internal/domain/search/ -v` → PASS.
```bash
git add internal/domain/search/service.go internal/domain/search/service_test.go
git commit -m "feat: thread TrackingRead through the search service"
```

---

### Task 13: engine de-guard

**Files:**
- Modify: `internal/domain/search/service.go`
- Test: `internal/domain/search/service_test.go`

- [ ] **Step 1: Write failing test**

Add a test proving in-tx routing: with a fake store implementing `spi.Searcher` and an active tx in ctx, `Search` calls `Searcher.Search` (NOT `GetAll`). Add a second test: when `ConditionToFilter` fails (use a condition that doesn't translate), it falls back to `GetAll`+match even in-tx.

- [ ] **Step 2: Run to verify fail**

Run: `go test ./internal/domain/search/ -run DeGuard`
Expected: FAIL — current code gates on `tx == nil`, so in-tx calls `GetAll`.

- [ ] **Step 3: Implement**

In `service.go`, change `if searcher, ok := store.(spi.Searcher); ok && tx == nil {` to `if searcher, ok := store.(spi.Searcher); ok {`. Update the comment block (remove the "in-transaction searches bypass pushdown" rationale; state the new contract). Leave the `GetAll`/`GetAllAsAt`+match fallback for the translate-failure path; add a one-line code comment that the in-tx translate-failure fallback conservatively records the whole-model read-set regardless of `TrackingRead`.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/domain/search/... -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/domain/search/service.go internal/domain/search/service_test.go
git commit -m "feat: route in-transaction search through the plugin Searcher"
```

---

### Task 14: HTTP `trackingRead` request field

**Files:**
- Modify: `internal/domain/search/handler.go` (and the search request DTO it decodes — locate via `grep -rn "PointInTime" internal/domain/search/*.go internal/api* internal/http*`)

- [ ] **Step 1: Write failing e2e-ish handler test**

In the search handler test (find with `grep -rln "func Test" internal/domain/search/*handler*`), assert a request body carrying `"trackingRead": true` yields `SearchOptions.TrackingRead == true`.

- [ ] **Step 2: Run to verify fail**

Run: `go test ./internal/domain/search/ -run Handler.*Tracking`
Expected: FAIL.

- [ ] **Step 3: Implement**

Add an optional `trackingRead` (bool, default false) to the sync-search request DTO and set `opts.TrackingRead` at both `SearchOptions{...}` construction sites (`handler.go:127`, `:219`). Only the synchronous search path (not async submit).

- [ ] **Step 4: Run / Commit**

Run: `go test ./internal/domain/search/... -v` → PASS.
```bash
git add internal/domain/search/handler.go <dto file>
git commit -m "feat: accept trackingRead on the sync HTTP search request"
```

---

### Task 15: gRPC `trackingRead` in the CloudEvent search payload

**Files:**
- Modify: `internal/grpc/search.go`

- [ ] **Step 1: Write failing test**

In `internal/grpc/search_test.go` (or the grpc e2e), assert a CloudEvent search payload with `trackingRead:true` produces `search.SearchOptions{TrackingRead:true}` at `internal/grpc/search.go:148`/`:332`.

- [ ] **Step 2: Run to verify fail**

Run: `go test ./internal/grpc/ -run Tracking`
Expected: FAIL.

- [ ] **Step 3: Implement**

Decode `trackingRead` from the search request payload struct used in `internal/grpc/search.go` and set it on both `search.SearchOptions{...}` literals. (The payload is JSON inside the CloudEvent — extend the request struct that already carries `PointInTime`.)

- [ ] **Step 4: Run / Commit**

Run: `go test ./internal/grpc/... -v` → PASS.
```bash
git add internal/grpc/search.go
git commit -m "feat: accept trackingRead in the gRPC search payload"
```

---

## Phase 6 — cross-cutting tests

### Task 16: cross-backend parity — in-tx RYW

**Files:**
- Create: `e2e/parity/tx_search_ryw.go`
- Modify: `e2e/parity/registry.go`

- [ ] **Step 1: Write the parity scenario**

Create `e2e/parity/tx_search_ryw.go` following the existing scenario shape in `e2e/parity/`. Within an active tx: create a matching entity, update a committed entity out of match, delete a committed match; then `Search` and assert the RYW result set (present/absent per the reference semantics), pagination with a buffered add interleaving at an order boundary, and in-tx PIT committed-only. No concurrency (parity is single-threaded). Register in `registry.go`.

- [ ] **Step 2: Run to verify fail then pass**

Run: `go test ./e2e/parity/... -run TxSearchRYW -v` (memory/sqlite/postgres)
Expected: PASS on all three (the feature is implemented; this pins parity). If any backend diverges, fix that backend — divergence is a bug.

- [ ] **Step 3: Commit**

```bash
git add e2e/parity/tx_search_ryw.go e2e/parity/registry.go
git commit -m "test: cross-backend parity for in-transaction RYW search"
```

---

### Task 17: isolated concurrency e2e — TrackingRead conflict footprint

**Files:**
- Create: `internal/e2e/search_intx_tracking_test.go`

- [ ] **Step 1: Write the tests (per backend, isolated — not parity)**

Assert, on a single running backend: (a) `TrackingRead=true` — tx A searches (returns entity X), tx B updates X and commits, tx A's commit → `spi.ErrConflict` / HTTP 409 retryable; (b) `TrackingRead=true` — B updates a **non-returned** entity Y → A commits OK; (c) `TrackingRead=false` — B updates returned X → A commits OK (snapshot read). Assert consistency (one coherent outcome), never a precise interleave (`.claude/rules/concurrency-tests-not-in-parity.md`).

- [ ] **Step 2: Run**

Run: `go test ./internal/e2e/ -run IntxTracking -v`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/e2e/search_intx_tracking_test.go
git commit -m "test: in-tx search TrackingRead conflict footprint (isolated)"
```

---

### Task 18: HTTP + gRPC e2e — in-tx search via tx-token

**Files:**
- Create: `internal/e2e/search_intx_test.go`

- [ ] **Step 1: Write the tests**

Using the tx-token join harness (see `internal/e2e/callback_txjoin_grpc_search_test.go` and `callback_harness_test.go`): open a tx, perform a buffered write, then a sync HTTP search and a gRPC search within the tx; assert RYW results and the `trackingRead` field is accepted (200 / gRPC success envelope). Cover the documented status codes on this path (200; 400 invalid path; 404 unknown model).

- [ ] **Step 2: Run**

Run: `go test ./internal/e2e/ -run IntxSearch -v`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/e2e/search_intx_test.go
git commit -m "test: e2e in-transaction search over HTTP and gRPC"
```

---

### Task 19: cluster e2e — owner-forwarded in-tx search

**Files:**
- Modify: the multi-node e2e file (locate: `grep -rln "TestMultiNode\|cluster" internal/e2e/*.go`)

- [ ] **Step 1: Write the test**

In the multi-node harness, open a tx on node A, buffer a write, issue an in-tx search whose request lands on node B; assert it is forwarded to the owner and returns RYW results via the overlay (not an empty/committed-only result). This pins the co-location invariant.

- [ ] **Step 2: Run**

Run: `go test ./internal/e2e/ -run MultiNode.*Search -v`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/e2e/<multinode_file>.go
git commit -m "test: cluster in-transaction search forwards to tx owner (RYW)"
```

---

## Phase 7 — docs & release mechanics

### Task 20: Docs — CONSISTENCY.md, OpenAPI, help, cloud-parity, godoc

**Files:**
- Modify: `docs/CONSISTENCY.md`, `api/openapi.yaml`, `cmd/cyoda/help/content/` (search topic), `docs/cloud-parity/` (new file), `CHANGELOG.md`
- Modify (SPI): `cyoda-go-spi` `searcher.go` godoc (already in Task 4)

- [ ] **Step 1: CONSISTENCY.md**

Add a subsection under §3/§5: in-tx `Search` is a predicate read; by default (`TrackingRead=false`) it records nothing and does not prevent phantoms; `TrackingRead=true` records the returned entities into the read-set (entity-level FCW, still no phantom protection). Reword the fence guidance: the fence requires reading **every** entity the invariant covers — a predicate returning a subset breaks it; use `GetAll` or a peer-complete `TrackingRead` search.

- [ ] **Step 2: OpenAPI**

Add optional `trackingRead: {type: boolean, default: false}` to the sync search request schema in `api/openapi.yaml` (embedded via `//go:embed`; do NOT re-enable native embed — see memory `project_openapi_spec_embed_via_goembed`). Keep the schema typed-but-open (no `additionalProperties:false`).

- [ ] **Step 3: Help topic + artefacts**

Update the search help topic under `cmd/cyoda/help/content/` to document `trackingRead`. Regenerate/verify `cyoda help openapi json|yaml` and `cyoda help grpc` reflect it. Keep prose compact (memory `feedback_compact_prose`).

- [ ] **Step 4: cloud-parity + CHANGELOG**

Create `docs/cloud-parity/tx-aware-search.md` describing the tx-aware `Searcher` contract + `trackingRead` field Cloud mirrors (Gate 7). Add a CHANGELOG entry.

- [ ] **Step 5: Verify docs build/tests**

Run: `go test ./cmd/cyoda/... ./internal/... -run Help -short`
Expected: PASS (help completeness tests green).

- [ ] **Step 6: Commit**

```bash
git add docs/CONSISTENCY.md api/openapi.yaml cmd/cyoda/help/content docs/cloud-parity/tx-aware-search.md CHANGELOG.md
git commit -m "docs: tx-aware search + trackingRead (CONSISTENCY, OpenAPI, help, cloud-parity)"
```

---

### Task 21: SPI release composition — pin the pseudo-version

**Files:**
- Modify: SPI repo (push main), then cyoda-go `go.mod` + `plugins/*/go.mod` (all 4), un-skip-worktree `go.work`

**Follow** MAINTAINING.md "Coordinated release across sibling repos" and memory `feedback_spi_coordinated_release_procedure` / `reference_plugin_pseudo_pin_window`.

- [ ] **Step 1: Land SPI on main**

In `cyoda-go-spi`, ensure Tasks 1–4 are committed; push to `main` (no tag). Capture the new main HEAD SHA.

- [ ] **Step 2: Re-pin the pseudo-version across all 4 go.mod**

Use the repo's `make repin-plugins` / repin target (see `reference_plugin_pseudo_pin_window`) or `go get github.com/cyoda-platform/cyoda-go-spi@<sha>` in root + each plugin, producing a `v0.8.3-0.<ts>-<sha>` pseudo-version pinned identically in `go.mod`, `plugins/memory/go.mod`, `plugins/postgres/go.mod`, `plugins/sqlite/go.mod`.

- [ ] **Step 3: Drop the local go.work override**

```bash
go work edit -dropuse=/Users/paul/go-projects/cyoda-light/cyoda-go-spi
git update-index --no-skip-worktree go.work
git checkout go.work   # restore committed go.work (., ./plugins/*)
```

- [ ] **Step 4: Verify resolution without the workspace override**

Run: `GOFLAGS=-mod=mod make test-all` (root + all plugins; Docker up)
Expected: resolves the pinned pseudo-version and passes.

- [ ] **Step 5: Commit the pin bump (one commit)**

```bash
git add go.mod go.sum plugins/*/go.mod plugins/*/go.sum
git commit -m "chore: re-pin cyoda-go-spi pseudo-version for tx-aware search helpers"
```

---

## Phase 8 — verification

### Task 22: Full verification + race

- [ ] **Step 1: Root + plugins tests**

Run: `make test-all` (Docker up) — root incl. e2e + memory/sqlite/postgres.
Expected: all green.

- [ ] **Step 2: Vet**

Run: `go vet ./...` (root) and per-plugin `go vet ./...`.
Expected: clean.

- [ ] **Step 3: Race (one-shot, pre-PR)**

Run: `make race` then `go test -race -timeout=20m ./internal/e2e/...` (if e2e touched).
Expected: no data races.

- [ ] **Step 4: todos / dead code sweep**

Run: `make todos`
Expected: no new `TODO(...)` introduced by this change (Gate 6).

- [ ] **Step 5: Confirm no issue IDs leaked into shipped artefacts**

Run: `grep -rn "420" api/openapi.yaml cmd/cyoda/help/content internal/common/error_codes.go 2>/dev/null`
Expected: no matches (issue IDs only in commits/PR/spec).

No commit (verification only).

---

## Self-Review (completed by plan author)

- **Spec coverage:** SPI helpers (Tasks 1-3), delegations (5-6), TrackingRead field (4,12,14,15), memory Searcher (7), sqlite (8-9), postgres (10-11), engine de-guard (13), parity (16), concurrency footprint (17), HTTP/gRPC e2e (18), cluster (19), docs incl. CONSISTENCY/OpenAPI/help/cloud-parity/COMPATIBILITY-note (20), SPI pin (21), verification+race (22). No new error codes → no `errors/<CODE>.md` task needed. COMPATIBILITY.md full row is deferred to the milestone-end SPI tag (noted in Task 20/spec), not this change.
- **Placeholder scan:** porting tasks name exact source file:line ranges; new logic (MergePage, overlay, TrackingRead recording) has full code or precise mechanical steps. Locate-via-grep is used only for two test-file/DTO lookups whose exact names are environment-dependent.
- **Type consistency:** `MatchFilter(Filter,[]byte,EntityMeta)bool`, `LessByOrder(*Entity,*Entity,[]OrderSpec)bool`, `MergePage(next,adds,deleted,specs,offset,limit)`, `SearchOptions.TrackingRead bool` used consistently across SPI, plugins, engine, transports.
