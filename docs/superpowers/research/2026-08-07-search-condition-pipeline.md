# Search-condition pipeline — factual reference

Research basis for the prepare/execute split (cyoda-go-spi#30). Written because
the design spec's first drafts asserted the behaviour of surrounding code without
reading it, and three of those assertions were wrong.

**Scope.** Everything a `predicate.Condition` passes through between arriving at a
transport and being evaluated against a row: entry points, validation, translation
to `spi.Filter`, path selection, and the per-row evaluation sites.

**Method.** Four independent investigations against the worktree at `e7960aa`, the
SPI worktree, and the commercial Cassandra checkout. Every claim below is from
reading the cited line. Where two investigations disagreed, the disagreement was
resolved by reading the code directly — noted inline. Claims marked
**[unverified]** could not be settled by reading and need a probe test.

> **Stale citations, deliberately not rewritten.** After this was written, the
> condition translator moved out of `internal/domain/search/filter_translate.go`
> into the SPI as `spi.ConditionToFilter` (cyoda-go#492 / cyoda-go-spi#28), taking
> the meta vocabulary, type classification and path normalisation with it. Every
> reference below to `filter_translate.go`, `orderclass.go`'s classification
> helpers or `path_validate.go`'s `normalisePath` now resolves in
> `cyoda-go-spi/condition_filter.go` and `order_class.go` instead. The *behaviour*
> described is unchanged — the move reconciled the two copies rather than altering
> either — but the file:line anchors are dead. Re-anchor them when this document is
> next worked on rather than trusting them.


---

## 1. Entry points

Nine doors. Six are "search-shaped" and funnel through `SearchService.Search` or
`SubmitAsync`; three do not.

| id | door | chain to evaluation |
|---|---|---|
| E1 | HTTP sync search | `search.Handler.SearchEntities` (`search/handler.go:79`) → `ParseCondition` (`:87`) → `SearchService.Search` (`service.go:245`) |
| E2 | HTTP async submit | `handler.go:176` → `SubmitAsync` (`service.go:418`) → goroutine re-enters `Search` (`:566`) |
| E3 | gRPC direct search | `grpc/search.go:59` → `handleDirectSearchRequest` (`:314`) → `Search` |
| E4 | gRPC snapshot search | `grpc/search.go:29` → `handleSnapshotSearchRequest` (`:130`) → `SubmitAsync` |
| E5 | HTTP grouped stats | `grouped_stats_handler.go:66` → `QueryGroupedStats` (`grouped_stats_service.go:55`) |
| E6 | HTTP delete-by-condition | `entity/handler.go:482` → `DeleteEntitiesConditional` (`entity/service.go:938`) → delegates to `Search` (`:1004`) |
| E7 | workflow criterion **import** | `workflow/handler.go:168` → `validateWorkflowStructure` → `validateCriterion` (`validate.go:227`) |
| E8 | workflow criterion **evaluation** | `engine.go:576/779/887` → `evaluateCriterion` (`:969`) → `match.Match` (`:1022`) |
| E9 | async job execution | the E2/E4 goroutine; a `spi.SelfExecutingSearchStore` bypasses it (`service.go:519-521`), no in-tree implementation |

There is **no gRPC door for grouped stats** and **none for delete-by-condition**.

**The OpenAPI spec constrains nothing at runtime.** `openapivalidator` is wired
only in `internal/e2e/e2e_test.go:172-176`; `app/app.go` has no validating
middleware. The generated enums in `api/generated.go` are therefore documentation,
not enforcement — and they are wider than the runtime accepts (~60 operator names
at `:920-970` against the 26 in `canonicalOperators`).

---

## 2. What each door validates

The search family (E1–E4, E6) runs, in order: `ValidateCondition` → model
registration → `validateConditionPaths` → `ValidateRegexPatterns` →
`validateConditionTypes`.

Two structural facts matter more than the list:

**`validateConditionTypes` is schema-conditional.** It returns nil — skipping the
entire type and meta-field pass — whenever `loadModelNode` yields nil
(`condition_type_validate.go:341-344`), which happens on a ModelStore error, a nil
descriptor, `len(desc.Schema) == 0`, or an unparseable schema (`:311-321`). This is
deliberate ("prefer empty-on-flake to a 5xx", `:335-339`). Through the public API a
registered model always carries a schema (`model/service.go:149-153`), so the gap
is a degraded-mode branch rather than a normal one — but "rejected at the boundary"
is conditional, not absolute.

**Grouped stats (E5) validates a different set, in a different order.** It runs
regex → structural → types, where search runs structural → paths → regex → types.
It passes `model = nil` to `ValidateConditionValueTypes` (`grouped_stats_service.go:174`)
even though it holds a `fields` map (`:112`), so the data-leaf operand-type check
and the container-path check never run. It has no analogue of
`validateConditionPaths` at all. It is simultaneously **stricter** on the meta
vocabulary (unconditional, no schema precondition) and **looser** on everything
schema-dependent.

**Workflow import (E7) shares exactly one validator with the search family.**
`walkCriterion` (`validate.go:246-266`) runs `compileMatchesPattern` on simple and
lifecycle leaves and `search.ValidateLifecycleCondition` on lifecycle leaves. It
runs none of: operator-name validation, operand-shape validation, BETWEEN arity,
function-condition rejection, data-path validation, operand-type validation. And
`validateCriterion` **returns nil on any `ParseCondition` failure**
(`validate.go:232-235`), so an unparseable criterion imports clean.

The comment at `search/handler.go:46-51` calling `ValidateCondition` "the single
boundary every transport funnels through" is accurate for E1–E6 and does not
extend to E7.

### Reachability of the five structural evaluator errors

This is the table the spec depends on.

| evaluator error | search family (E1–E4, E6) | grouped stats (E5) | workflow criteria (E7 import / E8 eval) |
|---|---|---|---|
| function condition (`match.go:42`) | rejected 400 `INVALID_CONDITION` (`operators.go:118-127`) | rejected 400 | accepted at import by design; top-level dispatched at `engine.go:975-987`; **reachable only nested inside a group** |
| unknown condition type (`match.go:44`) | unreachable — parse rejects | unreachable | unreachable (parse rejects at eval, giving a different error) |
| unknown lifecycle field (`match.go:167`) | rejected 400 `INVALID_FIELD_PATH` — **but only when the model schema loads** | rejected 400, unconditional | rejected at import (`validate.go:250-256`) |
| unknown group operator (`match.go:251`) | **never rejected anywhere** | never rejected | never rejected |
| unsupported operator name (`operators.go:33`) | rejected 400 (`BAD_REQUEST`) | rejected 400 (`INVALID_CONDITION`) | **not checked at import** — reachable |

Same fault, different code: unknown operator and BETWEEN arity are `BAD_REQUEST` on
search and `INVALID_CONDITION` on grouped stats.

---

## 3. Translation: where meaning changes silently

`ConditionToFilter` (`filter_translate.go:21`) errors only on a nil condition, a
function condition, or a `jsonPath` that fails `stripDollarDot`. Everything else
succeeds — including several cases where the resulting `Filter` does not mean what
the `Condition` said.

**T1 — any non-`OR` group operator becomes `AND`.** `filter_translate.go:134-137`,
no error. `spi.FilterOp` has no `NOT`, so `NOT` is not merely unvalidated, it is
**unrepresentable**. `NOT` is nevertheless a shipped enum value
(`api/generated.go:523`, `GroupConditionDtoOperatorNOT`). No walker inspects
`GroupCondition.Operator` — verified across `ValidateCondition` (`operators.go:111-117`),
`walkConditionTypes` (`condition_type_validate.go:66-72`), `walkRegexPatterns`
(`regex_validate.go:57-63`) and workflow `walkCriterion` (`validate.go:258-263`).

**T2 — group-operator case handling differs between the two evaluators.**
`groupToFilter` uses `EqualFold` for `"OR"`; `match.matchGroup` uses an exact switch
on `"AND"`/`"OR"` and errors otherwise (`match.go:228-252`). So `"or"` gives
`FilterOr` on the pushdown path and a hard error on the fallback path.

**T3 — `Declared` and `Coercion` are looked up with the RAW `jsonPath`.**
`simpleToFilter` uses `fields[c.JsonPath]` (`filter_translate.go:57`) and
`dataCoercion(c.JsonPath, fields)` (`:56`), but `FieldsMap` keys are `$.`-prefixed
(`normalisePath`, `path_validate.go:72-85`). A prefix-less path such as `"city"` —
which is accepted, and pinned as accepted by
`filter_translate_test.go:36-49` — therefore misses the map, yielding
`Declared = nil`. An empty declared set makes `expandCompare` return "operand parses
into no declared type" (SPI `eval_leaf.go:173-207`), which `evalLeafFilter` converts
to a non-match. **Net effect: `{"jsonPath":"city","operatorType":"EQUALS","value":"Berlin"}`
returns 200 with zero rows on a model where `$.city` exists and matches.** It clears
every gate on the way, because `validateConditionPaths` normalises and therefore
finds the path known. `arrayToFilter` does **not** have this bug — it normalises via
`arrayElementPath` (`:166`, `:198-204`). The two arms of the same file disagree.

**T4 — unknown operator name becomes `FilterMatchesRegex`.**
`mapOperator`'s default (`filter_translate.go:289-291`). Unreachable from E1–E6
because `ValidateCondition` gates it, but it collides "unknown" with a real operator
value, so any future drift between `canonicalOperators` and `mapOperator` becomes
silent rather than loud.

**T5 — unknown meta field translates to a valid leaf.** `lifecycleToFilter` cannot
fail (`:109-130`); anything not temporal gets `Declared = [String]`. The SPI's
`extractFilterMetaValue` (`filter_match.go:180-215`) accepts a **wider** vocabulary
than `sortableMetaFields` — `entity_id`, `version`, `created_at`, `updated_at`,
`model_name`, `model_version`, `change_type`, `transaction_id`. `match.matchLifecycle`
knows none of them and errors. A field in neither set produces an absent value, so
every binary operator non-matches but `IS_NULL` matches **every** entity.

**T6 — text and pattern operators on temporal meta fields.** `lifecycleToFilter`
routes on field identity, never on operator, so `CONTAINS "2021"` on `creationDate`
keeps `Declared = [ZonedDateTime]` and reaches `evalStringOp`, which runs
`strings.Contains` on the RFC3339 rendering. `internal/match` has an explicit guard
(`match.go:196-201`, `isTemporalOperator` at `:205-215`) and returns non-match. Both
return 200; they differ in rows.

Why this has not surfaced: the only e2e case,
`internal/e2e/search_temporal_test.go:283-296`, asserts zero results using operand
`"2021"` against entities created in 2026 — the substring genuinely does not occur,
so the test passes under both semantics and discriminates nothing. The same is true
of `grouped_stats_temporal_test.go:37`, `grpc/search_temporal_test.go:279` and
`criterion_temporal_test.go:39`. **[unverified]** — the discriminating case
(operand `"20"` or `"2026"`) has not been executed.

---

## 4. Path selection

Two production call sites attempt translation: `service.go:300` (search) and
`grouped_stats_service.go:193` (grouped stats). All three in-tree stores implement
`spi.Searcher`, so **the only route to the in-memory fallback is a translation
failure** — in practice a `jsonPath` outside `stripDollarDot`'s allowlist
`[A-Za-z0-9_.-]` (`filter_translate.go:220-229`), which rejects `[`, `]`, `*`, and
all non-ASCII letters.

Array-wildcard paths (`$.items[*].name`) therefore force a full `GetAll` scan —
confirmed by `filter_translate_test.go:341-359` and
`service_test.go:977-1044`, both executed and passing.

On SQL backends the pushdown answer is, in every non-exact case, **also**
`spi.MatchFilter`: `planQuery` installs the full original filter as `postFilter`
whenever the plan is not provably exact, and only `IsNull`/`NotNull` leaves are
exact (`sqlite/query_planner.go:70-81`). So the divergences below are really
`spi.MatchFilter` versus `internal/match.Match`.

| case | pushdown | fallback | differ |
|---|---|---|---|
| non-`OR` group operator, all children translatable | 200, AND semantics | not reached | answer silently wrong |
| non-`OR` group operator + one wildcard child | not reached | `match.Match` errors → 500 | **yes: 200 vs 500** |
| lowercase `"or"` | `FilterOr`, 200 | hard error → 500 | **yes** |
| meta field in the SPI's wider vocabulary, schema-less model | real match, 200 | error → 500 | **yes** |
| text operator on `creationDate` | 200, lexical match on RFC3339 | 200, always empty | **yes: same status, different rows** |
| schema present but unparseable | error discarded (`service.go:299`), untyped, 200 zero rows | fails closed (`:371-374`) → 500 | **yes** |
| scan-budget exhaustion | sqlite only, 400 `SCAN_BUDGET_EXHAUSTED` | impossible — `GetAll` has no budget | **yes** |
| in-transaction read-set | matched rows only | `GetAll` records **every** entity in the model | **yes** — later 409 risk |

**The caller cannot select the path**, but can force the fallback deterministically
by OR-ing in any leaf with a wildcard path — which is exactly what the repo's own
e2e suite does (`internal/e2e/search_test.go:727-736`).

---

## 5. The workflow criterion path

**What can be stored.** Because `validateCriterion` swallows parse errors and
`walkCriterion` checks so little, the following import cleanly and then fail on
every evaluation: any unparseable criterion; any unsupported or empty
`operatorType`; malformed BETWEEN arity; object operands; unknown data paths;
operand-type mismatches; every defect inside an `array` or `function` condition; a
`function` condition nested inside a group. Workflows stored before a validator
existed are never re-checked — `validateWorkflowStructure` runs on the incoming
document only (`validate.go:299-307`), and the merge paths do not re-validate.

**When the model schema loads.** `matchSimple` calls `fieldTypes` for **every**
simple leaf regardless of operator (`match.go:100-103`), and `matchArray` calls it
unconditionally before its values loop (`match.go:258-261`) — even for an array
condition with zero values. So "only comparison and range operators need declared
types" is false. What is true: a purely lifecycle criterion loads nothing, and
short-circuiting makes the load **data-dependent**, not shape-determined.

**Error precedence.** `engine.go:1022-1028` checks the match error **first** and
`loadErr` second, so a structural error masks an infrastructure failure — and the
`loadErr` is then discarded entirely, never logged. Reachable with the model store
down via `OR[$.age > 5, $.x IS_CHANGED]`: the first leaf latches `loadErr` and
degrades to non-match, the second raises "unsupported operator", and that is what
the caller sees.

**What failure produces.** `classifyWorkflowError` (`entity/service.go:2105-2151`)
ends in `common.Operational(http.StatusBadRequest, ErrCodeWorkflowFailed, …)`, so a
criterion evaluation error is **400 `WORKFLOW_FAILED`** carrying the raw text.
`ErrCriterionTypingInfra` is intercepted earlier (`:2130`) and becomes a sanitised
**500**. *(Two investigations disagreed here; resolved by reading
`classifyWorkflowError` directly.)* A criterion evaluation failure rolls the whole
transaction back — the entity write is discarded — via the deferred
`scope.Release()` (`entity/txscope.go:23`, `:136-139`).

**Nothing is cached.** `ParseCondition` runs on every evaluation
(`engine.go:970`), and `criterionName` (`:133-153`) parses the same bytes a
**second** time on every criterion-not-matched audit event. For the schema: the
descriptor bytes are cached process-wide and gossip-invalidated
(`modelcache/cache.go:110-124`, locked models only, 5-minute jittered lease), but
`schema.Unmarshal` builds a fresh `ModelNode` per call (`codec.go:101-107`), and
`FieldsMap()`'s cache hangs off that instance (`schema/field.go:30-37`). So the
**full field-tree walk, sort and map build runs on every criterion evaluation that
touches a data leaf**, per transition, per cascade step.

---

## 6. Per-row evaluation sites

Eight `spi.MatchFilter` symbol sites, but 14 production per-row evaluation points
once wrappers are followed: memory 5, sqlite 5, postgres 1, Cassandra 3.
`internal/match.MatchFilter` (`match.go:299`) has **zero** production callers.

**`postFilter` nil-ness controls three things**, and they are not the same on both
SQL backends:

| check | sqlite | postgres |
|---|---|---|
| `LIMIT` pushdown | `searcher.go:92` | `searcher.go:211` |
| native `GROUP BY` | `grouped_stats.go:301` | `grouped_stats.go:230` |
| scan budget armed | `searcher.go:107`, `:243` | **no scan budget at all** (`searcher.go:30-35`) |
| collection loop shape | — | `searcher.go:226` |

**The two backends disagree on the zero-value filter.** Postgres guards
(`searcher.go:193-196`) with a comment explaining why: without it, `planQuery`
treats the empty `Op` as non-pushable and installs the zero filter as a residual.
Sqlite `searchCommitted` calls `planQuery` unconditionally (`searcher.go:67`) and
therefore does exactly that — losing `LIMIT` pushdown and arming the scan budget.
An explicit empty `AND` (what `ConditionToFilter` emits for a nil condition) takes a
different branch and leaves `postFilter` nil. So the two spellings of "match
everything" behave differently on sqlite.

**Iterator construction sites**, exhaustively: `memoryIter` (`memory/grouped_stats.go:63`),
`sqliteSliceIter` (`sqlite/grouped_stats.go:90`), `sqliteIter` (`sqlite/grouped_stats.go:129`),
`postgresIter` — **two sites**, `postgres/searcher.go:220` and
`postgres/grouped_stats.go:89` — and Cassandra `entityIter` (`internal/store/grouped_stats.go:99`).
Plus two filter-carrying loops that are not iterator types: the `next` closure in
`sqlite/searcher.go:241-266` and the plain loop in `sqlite/searcher.go:106-137`.

**Concurrency.** One genuine cross-goroutine share: the Cassandra direct-search
fan-out (`search/direct_executor.go:274-287`) hands one `directRequest` to N
errgroup workers; the per-goroutine struct copies share `Values`, `Children` and
`Declared` backing arrays. Read-only today. Any prepared state hung off a filter
must be immutable after construction.

**One backend passes a different document.** `postgres/grouped_stats.go:172` passes
`doc` — the raw scanned JSONB — not `entity.Data`. `doc` retains a top-level
`_meta` key (`entity_doc.go:94`) that `unmarshalEntityDoc` strips and re-marshals
away for every other consumer (`:117`, `:131`). So on postgres alone, `_meta.state`
is reachable as an ordinary data path, and the bytes are pre-re-marshal.

**Cross-module tests pinning evaluators against each other:**
`TestMatchFilter_SqliteParity_Smoke` (`internal/match/match_filter_sqlite_parity_test.go:45`)
— the only one pinning a backend against `internal/match` — and `TestTxSearchRYW`
(`e2e/parity/txsearchryw/tx_search_ryw_test.go:225`), which uses
`GetAll + spi.MatchFilter` as its oracle across all three backends.

---

## 7. Defects found

Numbered for reference. Ownership is a proposal, not a decision.

| # | defect | severity | proposed owner |
|---|---|---|---|
| D1 | `simpleToFilter` looked up `Declared`/`Coercion` with the un-normalised path, so a prefix-less `jsonPath` silently returned zero rows (T3) | **high** — wrong answer, 200, no signal | **FIXED** (`5f62f38`) |
| D2 | sqlite `Search` lacked postgres's zero-filter guard: lost `LIMIT` pushdown, armed the scan budget | medium — backend divergence | **FIXED** (`5f62f38`) |
| D3 | `NOT` is an advertised group operator that silently becomes `AND`; on delete-by-condition (E6) this deletes the wrong entities | **high** — silent data loss | cyoda-go#487 (owns group operators; #487 does not currently record the delete consequence) |
| D4 | group-operator case handling differs between evaluators (`"or"`) | medium | cyoda-go#487 |
| D5 | the SPI's meta vocabulary is wider than `sortableMetaFields`; the two evaluators disagree | medium | cyoda-go#487 |
| D6 | postgres exposed `_meta.*` as a matchable data path; no other backend does | medium — backend divergence | **FIXED** (`6ddbcb0`) |
| D7 | workflow import accepts unparseable criteria and every non-regex defect | medium | **cyoda-go#488** — not a simple fix; needs a contract decision |
| D8 | grouped stats performs no data-path and no operand-type validation | medium | **already filed: cyoda-go#480** |
| D9 | `FieldsMap` is rebuilt from scratch on every criterion evaluation touching a data leaf | **measured — see §7.1** | decision owed |
| D10 | stale comments: `validate.go:200-204` claims a temporal rejection that does not happen; `regex_validate.go:71` and `:25-34` cite `opMatchesPattern`, which no longer exists; three sites cite a parity test `MatchFilterSqliteEvaluateFilterParity` that does not exist | low | spi#30 (Gate 6) |

### 7.1 D9 measured

Benchmarked on an M1 Max: one criterion evaluation (`ParseCondition` + `Match` on
`AND[state == "active", $.f > 5]`), with the schema reparsed per call as today,
against the same evaluation closing over a prebuilt field map.

| model schema | reparse per call | prebuilt map | reparse share | allocations |
|---|---|---|---|---|
| 10 fields | 56 µs | 11 µs | 80% | 247 → 91 |
| 50 fields | 130 µs | 15 µs | 88% | 757 → 91 |
| 200 fields | 322 µs | 19 µs | 94% | 2,651 → 91 |
| 1000 fields | 1,837 µs | 12 µs | 99.3% | 12,403 → 91 |

Splitting the reparse at 200 fields: `schema.Unmarshal` 207 µs, `FieldsMap()` build
55 µs, and a warm `FieldsMap()` on an existing node is 2.4 ns — the per-instance
cache works, it just never survives, because `Unmarshal` builds a new node each call.

Reading: the cost scales with **schema size, not entity size or result size**, and it
is 80–99% of the evaluation. It repeats per criterion evaluation — so per transition
and per cascade step within one save. The allocation count matters as much as the
wall clock: 12k allocations per evaluation on a large model is GC pressure on the
write path.

Whether this is material depends on model size in practice, which the code cannot
tell us. On a 10-field model it is noise beside a database round trip. On a
1000-field model it is comparable to the entire commit.

The fix is to stop rebuilding the parsed node per call, which means caching it —
model-layer machinery with its own decisions (where it lives, how it is keyed, how it
is invalidated, tenant scoping). It is deliberately **not** folded into this change.

---

## 8. Consequences for the prepared-filter design

1. **The reachability table in the spec needs the schema-conditional caveat.**
   "Unknown lifecycle field is already 400" holds only when the model schema loads.
2. **"Only comparison and range operators need declared types" is false.** Every
   simple leaf and every array condition resolves types today. Eager preparation
   should ask only for the leaves that actually consume types — which loads *less*
   than today, and correspondingly narrows when a model-store outage fails a
   criterion. That is a behaviour change in the fail-open direction for operators
   that never needed the schema, and it needs stating.
3. **Both "match everything" spellings now behave alike** on sqlite, so the
   `postFilter` absence test no longer has to pick one to pass (D2 fixed).
4. **The precedence inversion is confirmed**, with a better counterexample than the
   spec currently carries: `OR[$.age > 5, $.x IS_CHANGED]`.
5. **Criterion evaluation errors are 400, not 500** — the spec is right, one
   investigation was wrong.
6. **Two more construction sites and two non-iterator loops** must be migrated
   (§6).
7. **D1 sat directly in the blast radius** — `Declared` is precisely what `Prepare`
   consumes — and is fixed, so `Prepare` inherits correctly-resolved declared types
   rather than the empty sets a prefix-less path used to produce.

---

## 9. Not verified

- The discriminating temporal case (a text operator on `creationDate` with an
  operand that *does* occur in the rendering) has never been executed. Both
  semantics are consistent with the existing tests.
- Whether a model descriptor with `len(Schema) == 0` is reachable through the
  public API. `model/service.go:149-153` always marshals a schema; the branch is
  reachable through store or decode failure.
- Whether an empty `jsonPath` (`""`, which `stripDollarDot` accepts) errors in
  SQLite's `json_extract(data, '$.')`.
- Cassandra: whether a domain entity's `Data` reaching `spi.MatchFilter` ever
  carries a `_meta` key. Verified that Cassandra passes `entity.Data` verbatim and
  never re-wraps.
- `spi.SelfExecutingSearchStore` re-parse behaviour — no in-tree implementation.
