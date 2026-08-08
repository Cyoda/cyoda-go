# Prepare/execute split for the search leaf evaluator

Design spec for cyoda-go-spi#30. Spans `cyoda-go-spi` and `cyoda-go`; the
commercial Cassandra backend follows as a third step.

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


## 1. Problem

Query-invariant work runs once per candidate entity instead of once per query.
Two evaluators reach the same leaf kernel and both do it:

| evaluator | tree | per-row work that does not depend on the row |
|---|---|---|
| `spi.MatchFilter` | `spi.Filter` | `OperandString`/`valuesToStrings`, `ExpandLeaf` (operand parse + type bucketing), **`regexp.Compile`** |
| `cyoda-go/internal/match.Match` | `predicate.Condition` | the above, plus `convertJSONPath` and the `fieldTypes(path)` lookup |

`regexp.Compile` is the reported symptom (`eval_leaf.go:142,146`). `evalLeafFast`
(`eval_leaf.go:546`) short-circuits only `Eq/Ne/Gt/Gte/Lt/Lte`, so `LIKE` and
`MATCHES_PATTERN` always reach the compile. Operand parsing and bucketing are the
same defect, cheaper.

`ExpandLeaf`'s doc comment (`eval_leaf.go:22-25`) already claims to be
"the once-per-query work". That claim is false today and is what concealed the
defect.

The worst-affected path is not the pushdown one. A condition on an
array-wildcard path (`$.laureates[*].motivation`) has no `Filter` representation —
`stripDollarDot` (`internal/domain/search/filter_translate.go:220-239`) rejects any
character outside `[A-Za-z0-9_.-]` — so `ConditionToFilter` fails and `search/service.go` falls back
to `GetAll` + `match.Match` per row: a full model scan with a compile on every
entity.

## 2. Shape

A prepare/execute split, with **two tree walkers over one shared leaf kernel**.
`Prepare` resolves everything that depends only on the query and returns an
immutable value that any number of goroutines may `Match` concurrently — the
Cassandra `direct_executor` hands one filter to N errgroup workers.

The two evaluators are **not** unified here. That is cyoda-go#464, and it is not
available anyway while `Filter` cannot express array wildcards. Both prepared leaf
types hold a `spi.Expansion` **plus the addressing each evaluator needs to fetch
the stored value** — `Source` and `Path` on the SPI side (`filter_match.go:91-97`),
and the `convertJSONPath`-converted gjson path on the `match` side, which §1 lists
as per-row work to hoist. The expansion is the shared part, which is what makes a
later merge mechanical; it is not the whole leaf.

Answers do not change. This is a pure refactor apart from §5, which is forced by
dropping the per-row error return and which nothing else in the change depends on.

The prepared tree is a new tree because it has to hold the expansions, not as a
defence against a caller mutating the source afterwards. No such defence is owed:
`.claude/rules/ownership-mutability.md` rule 7 makes `Prepare` a transformation
that consumes its input, and nothing in either repo mutates a filter after
building it.

## 3. cyoda-go-spi surface

```go
// added
type PreparedFilter struct{ … }              // immutable; safe for concurrent Match
func Prepare(f Filter) PreparedFilter
func (p PreparedFilter) Match(data []byte, meta EntityMeta) bool

// deleted
func MatchFilter(Filter, []byte, EntityMeta) bool
func EvalLeafString(FilterOp, string, []string, []DataType, gjson.Result) (bool, error)
func (Expansion) Void() bool                 // zero call sites anywhere

// unchanged
func ExpandLeaf(…) (Expansion, error)        // validation-boundary entry; keeps its error
func EvalLeaf(Expansion, gjson.Result) bool  // per-row primitive
```

`Prepare` returns **no error**. `evalLeafFilter` (`filter_match.go:76-83`) absorbs
an expansion error into a non-match by documented contract; a leaf that fails to
expand becomes a leaf that never matches. **Encode that explicitly** — a `bool` on
the prepared leaf, checked before `EvalLeaf`. Do not rely on the zero `Expansion`
happening to never match: it does today only because `kindUnary` is the zero kind
and an empty op falls through `EvalLeaf`'s switch (`eval_leaf.go:298`), which is an
undocumented coupling to iota ordering in a struct this change is already editing. Preserving that is what keeps this
change semantics-free on the SPI side. Promoting it to a hard 400 is a
cross-backend change and belongs to cyoda-go-spi#30's declared follow-up.
`ExpandLeaf` keeps its error because `condition_type_validate.go` consumes it.

`EvalLeafString` is deleted for the same reason as `MatchFilter`: it is the
leaf-level fused call. Leaving it would let `internal/match` keep compiling per
row while only the `Filter` side was forced open.

`evalLeafFast` becomes unreachable and is deleted. Its only purpose was avoiding
per-row expansion, and it is documented as "strictly result-identical" to the
general path. Two independent differential runs during design review (46,000 and
3,000,000 generated cases) found zero divergences, and §8's merge gate covers the
deletion without a separate test. A divergence found during implementation is a
live bug and comes back for a call rather than being deleted over.

`PreparedFilter` does not carry `Filter.Coercion`: no evaluator reads it, only the
sqlite/postgres SQL planners do. Explicit decision, not an omission.

**Zero values.** `Prepare(Filter{})` and the zero `PreparedFilter{}` both
match-all, mirroring `MatchFilter`'s `f.Op == ""` check. The root-versus-child
asymmetry is load-bearing and must survive:

| filter | today | required after |
|---|---|---|
| root `Filter{}` | true | true |
| root `Filter{Op: And}` (no children) | true | true |
| root `Filter{Op: Or}` (no children) | false | false |
| `AND[leaf, Filter{}]` | **false** | false |
| `OR[non-matching leaf, Filter{}]` | false | false |

A zero-`Op` child is not match-all: `evalFilter` routes it to `evalLeafFilter`,
`ExpandLeaf` hits its default arm, and the leaf never matches. A recursive
`Prepare` that hoists the `Op == ""` check into the recursion silently flips rows
4 and 5. This is the most likely implementation slip in the change.

sqlite depends on this in two places that must survive the migration —
`plugins/sqlite/grouped_stats.go:97-101` and `:163-165` both special-case an
empty `Op` before reaching the evaluator.

## 4. cyoda-go surface

```go
package match
type Prepared struct{ … }
func Prepare(cond predicate.Condition, fieldTypes FieldTypes) (Prepared, error)
func (p Prepared) Match(data []byte, meta spi.EntityMeta) bool   // no error

// deleted
func Match(predicate.Condition, []byte, spi.EntityMeta, FieldTypes) (bool, error)
func MatchFilter(spi.Filter, []byte, spi.EntityMeta) bool         // forwarder, zero non-test callers
```

`Prepare` here returns an error where the SPI's does not, because the two consume
different input types: `Filter.Op` is a closed enum, while `predicate.Condition`
carries free-text operator and field names that can name nothing.

`Match` loses its error return. Every error `match.Match` can produce is a
structural property of the condition, not of the row:

| error | site |
|---|---|
| function conditions not implemented | `match.go:42` |
| unknown condition type | `match.go:44` |
| unknown lifecycle field | `match.go:167` |
| unknown group operator | `match.go:251` |
| unsupported operator name | `operators.go:33` |

All five move to `Prepare`. Row-dependent failures are already swallowed to
non-match (`operators.go:42-44` for expansion errors, `match.go:200` for a
marshal failure, `match.go:276` for `matchArray`'s discarded error) and stay that
way — §5 covers why that matters.

Three details an implementer would otherwise have to guess:

- An `ArrayCondition` needs **one expansion per non-nil position**, not one for
  the leaf: `matchArray` (`match.go:263-279`) applies `EQUALS` per position with a
  distinct operand each time.
- `previousTransition` must be canonicalised to `transitionForLatestSave`
  (`match.go:149-151`) **before** the unknown-lifecycle-field check, or a working
  field name starts erroring.
- `matchSimple` routes on data shape, not condition shape: `result.IsArray()`
  (`match.go:107`) picks the array-wildcard path per row. Both paths consume the
  same expansion, so hoisting is safe — but the routing stays per row.

**Two things must NOT become errors at prepare time.** Both are deliberate
never-match behaviour that sits in front of the error path:

- `matchTemporalMeta` (`match.go:194-196`) returns `(false, nil)` for any
  non-temporal operator on `creationDate`/`lastUpdateTime`, before
  `applyOperator` can raise "unsupported operator". Verified:
  `creationDate IS_CHANGED` → `(false, nil)`, while `state IS_CHANGED` → error.
  The guard is field-dependent, not operator-dependent.
- A leaf whose `ExpandLeaf` fails (an operand parsing into no declared type, an
  untyped field under a comparison operator) is a never-match leaf, not an error.
  `search/service.go:361-380` deliberately supplies nil declared types for
  unknown paths so that comparison leaves degrade to non-match.

## 5. Declared behaviour change: when a malformed condition is reported

Today the five structural errors are only *raised* if the tree walk reaches them.
Verified:

```
AND[$.n EQUALS 1, $.n IS_CHANGED]   doc={"n":1}  ->  error "unsupported operator: IS_CHANGED"
AND[$.n EQUALS 1, $.n IS_CHANGED]   doc={"n":2}  ->  (false, nil)
```

That example needs `fieldTypes` to return a declared type for `$.n` (measured with
`[Integer]`). With `fieldTypes` nil — the shape `search/service.go` supplies for
unknown paths — both documents give `(false, nil)`, because the untyped `EQUALS`
leaf expands to an error that `operators.go:42-44` swallows before the sibling is
reached.

`matchGroup` (`match.go:224-253`) returns on the first child error, and
short-circuiting means later children are never visited. The same masking happens
inside `matchArrayWildcard` when the array is empty (`match.go:121-132`), and at
the search and grouped-stats boundaries when the candidate set is empty.

Hoisting the checks into `Prepare` makes reporting **deterministic with respect to
the data**: a malformed condition is reported from its own shape, not from which
rows happen to be present. It does not make reporting deterministic across
evaluators — see the reachability table below and cyoda-go#487.

**Where this is actually observable.** Four of the five errors are already
unreachable from most live entry points, so the declared change is much narrower
than the error list suggests:

| error | search / grouped-stats | workflow criteria |
|---|---|---|
| function condition (`match.go:42`) | rejected 400 at boundary (`search/operators.go:118-127`) | top-level intercepted (`engine.go:975-987`); **reachable only nested inside a group** |
| unknown condition type (`match.go:44`) | unreachable — parse rejects first | unreachable |
| unknown lifecycle field (`match.go:167`) | **400 whenever the model schema loads** (`validateLifecycleType` -> `errInvalidFieldPath`, `condition_type_validate.go:267-270`); `validateConditionTypes` skips the whole pass when `loadModelNode` returns nil — store error, no schema bound, or unparseable schema (`:311-321`) — a degraded branch, not the normal one | **already rejected at import** (`workflow/validate.go:250-256` -> `search.ValidateLifecycleCondition`) |
| unknown group operator (`match.go:251`) | **reachable, fallback path only** | **reachable** — nothing inspects `GroupCondition.Operator` |
| unsupported operator name (`operators.go:33`) | rejected 400 at boundary (`validateConditionAtDepth`) | **reachable** — import checks patterns only (`validate.go:248-249`) |

`isKnownMetaFilterField` (`orderclass.go:143-152`) is the closed meta vocabulary
and is exactly the set `match.matchLifecycle` handles, so the third row cannot
drift.

**No status code changes.** An earlier draft declared 500 -> 400 for unknown
lifecycle field and unknown group operator on the fallback paths. The first is
already a 400 and never reaches the evaluator. The second is genuinely a 500
today (`search/service.go:386`; `grouped_stats_service.go:275`) — but it is also a
**200** on the pushdown path, because `groupToFilter` (`internal/domain/search/filter_translate.go:141-145`) maps any
non-`OR` operator to `FilterAnd` without error. Moving one arm of a two-way divergence from 500 to 400 leaves it just as
divergent and pre-empts cyoda-go#487, which owns the case and fixes it at the
boundary where both paths meet. **The error mapping is therefore left exactly as
it is.** Gate 7 does not apply to a status code here, and no parity answer moves.

**Workflow criteria are where the change is visible.** `evaluateCriterion`
(`engine.go:969`) runs `ParseCondition` only. A stored criterion
`AND[state == "X", $.amount FROBNICATE 1]` — an unsupported operator name, which
import does not check — today evaluates false for entities outside state X and the
transition simply does not fire; afterwards it fails for every entity. Accepted
deliberately: a criterion that cannot be evaluated must not be silently treated as
"not satisfied". This is the one Gate-7 item, and it needs its own E2E test (§8).
An unknown *meta field* cannot be used to build that test — import already rejects
it.

**Eager typing also changes when infra failures fire.** `evaluateCriterion`'s
`fieldTypes` closure (`engine.go:1000-1020`) latches `loadErr` on first call.
Today `AND[state == "X", $.amount > 100]` never invokes it for an entity outside
state X, so a model-store outage yields `(false, nil)` and the transition simply
does not fire. Afterwards every criterion containing a data leaf resolves types,
so the same outage yields `ErrCriterionTypingInfra` -> sanitised 500
(`internal/domain/entity/service.go:2130`) for entities the short-circuit
previously spared. Declared, and covered by the same E2E row: a criterion that
cannot be typed must not be silently read as "not satisfied".

**Error precedence is fixed here rather than left to fall out.** With the model
store down, `AND[$.a > 1, $.b IS_CHANGED]` today reports the infra failure;
after the change `Prepare` could report the malformed operator first. The infra
failure must win: the migration checks `loadErr` after `Prepare` and before any
structural error `Prepare` returns, so an outage is never reported as a client
error.

**This inverts today's precedence, and the inversion is deliberate.**
`engine.go:1023-1028` checks the match error *first* and `loadErr` second, so a
structural error currently masks an infra failure. It is reachable: with the model
store down, `AND[$.a IS_NULL, $.b IS_CHANGED]` latches `loadErr` — `matchSimple`
calls `fieldTypes` for every simple leaf regardless of operator
(`internal/match/match.go:100-103`) — then matches `IS_NULL` true, raises
"unsupported operator" on the sibling, and returns the client error. Reporting a
client error for a server-side outage is the wrong way round, and Gate 1 of the
design philosophy says the operation fails closed on the unavailable dependency.
Pinned by a test using that condition, not one that short-circuits.

The pre-existing cross-path divergences for unknown group operator and unknown
meta field are **not** resolved here; they are cyoda-go#487.

## 6. Preparation is eager

`Prepare` calls the caller's `fieldTypes` during preparation and never again. The
closure is consumed, not retained: the engine's (`engine.go:1000-1020`) mutates
three captured variables with no synchronisation, so holding it inside a value
shared across goroutines would introduce a data race. Calling it once, on one
goroutine, is safe by construction.

**`Prepare` resolves declared types for exactly the leaf set `match.Match` resolves
today: every `SimpleCondition` and every `ArrayCondition`, whatever the operator.**
Not only comparison and range leaves. `matchSimple` calls `fieldTypes` before it
dispatches on the operator (`internal/match/match.go:100-103`), and `matchArray`
calls it unconditionally before its values loop (`:258-261`) — so `IS_NULL`,
`CONTAINS` and `MATCHES_PATTERN` all resolve types today even though `ExpandLeaf`
ignores the result for them. A purely lifecycle criterion still resolves nothing.

Matching that set exactly is deliberate, and it is the whole reason to state it.
Narrowing to "leaves that consume types" would resolve *fewer* than today, and the
leaves it stopped resolving are precisely the ones that would stop latching
`loadErr` — so with the model store down, a criterion like `$.name CONTAINS "x"`
would go from failing the transition to letting it proceed. That is a fail-open
movement on the write path, which
`.claude/rules/correctness-over-availability.md` forbids and which nothing here
needs. The cost of the wider set is one `loadFieldsMap` per criterion evaluation
either way, because the closure memoises within a call.

The only remaining difference from today is a criterion mixing a short-circuiting
lifecycle conjunct with a data leaf — `AND[state == "X", $.amount > 100]` against
an entity outside state X — which now resolves the second leaf's type where today
the short-circuit skips it. That direction is fail-closed and is covered by §5.

That resolution is cheap. `store.Get` is served by the process-wide,
gossip-invalidated descriptor cache (`internal/cluster/modelcache`), which caches
locked models — and a model holding data is necessarily locked, because a save
against an unlocked model is rejected `409 MODEL_NOT_LOCKED`
(`internal/domain/entity/service.go:214-216`). The parsed schema is cached on the
same entry, so `loadFieldsMap` no longer re-unmarshals per call; it reads an
already-built field map. No model-loading code changes in *this* work.

Preparing at workflow-definition load, which cyoda-go-spi#30 suggests, is not done:
workflows are fetched per save (`engine.go:664`) and criteria are parsed per
evaluation, so it needs a cache that does not exist, to remove one parse.

## 7. Call sites

The naive count of `spi.MatchFilter` symbol sites is 8, but three of them are
themselves the per-row shim. Swapping their bodies would leave the compile per
row and add the fused call's overhead. Their signatures change and the hoist
lands at their callers.

| wrapper | hoist point |
|---|---|
| `plugins/sqlite/post_filter.go:21` `evaluateFilter` (+ exported `EvaluateFilter:13`, used cross-module by `internal/match/match_filter_sqlite_parity_test.go`) | `sqlite/searcher.go:131,266`, `sqlite/grouped_stats.go:166,235` |
| `plugins/postgres/grouped_stats.go:444` `evalPostFilter` | `postgres/grouped_stats.go:172` |
| `plugins/memory/grouped_stats.go:501` `msMatchFilter` (call at `:502`) | `memory/grouped_stats.go:216,263` |
| `plugins/memory/searcher.go:179` | `matchSortBounded` (`searcher.go:176`) and its callers |
| `plugins/memory/searcher.go:87,104` | inline in `EntityStore.Search`'s in-tx read-your-writes branch |
| `plugins/sqlite/searcher.go:292` | inline in the in-tx buffered-adds loop (structural twin of `memory/searcher.go:104`) |

`sqlPlan.postFilter *spi.Filter` (`sqlite/query_planner.go:27`,
`postgres/query_planner.go:31`) is applied per row — via an iterator struct on the
Iterate paths and postgres Search, and inline in a raw `for rows.Next()` loop in
sqlite Search (`sqlite/searcher.go:130-131`, `:265-266`) — those loops call
`evaluateFilter`, the wrapper above, not `spi.MatchFilter` directly.

Its **nil-ness is load-bearing in three separate ways**, and collapsing it to a
zero `PreparedFilter` (which means match-all, not absent) breaks all three
silently, with every result still correct:

1. `LIMIT` pushdown is lost (`sqlite/searcher.go:99`, `postgres/searcher.go:211`;
   `postgres/searcher.go:226` separately selects the collection-loop shape).
2. Native `GROUP BY` is disabled on every query (`sqlite/grouped_stats.go:301`,
   `postgres/grouped_stats.go:230` -> `ErrAggregationNotPushdownable`).
3. The scan budget is armed on every query (`sqlite/searcher.go:114,257`), turning
   currently-passing searches into scan-limit errors. postgres has no scan budget.

**Representation, so this is not left to the implementer.** `sqlPlan` keeps
`postFilter *spi.Filter` unchanged — the planner's own predicates read its
nil-ness — and gains `preparedPostFilter *spi.PreparedFilter`, set non-nil exactly
when `postFilter` is non-nil, populated once where the plan is built. Row loops
read the prepared field; planner decisions keep reading the existing one. A
`PreparedFilter` value plus a separate `bool`, or replacing the field outright,
both put the nil-ness invariant back in play at every consumer.

Four consumers, and every construction site must be migrated together:
the two raw inline loops (`sqlite/searcher.go:123-124`, `:251-252`),
`sqliteIter` (`sqlite/grouped_stats.go:132`, `:200`, `:234-235`), and
`postgresIter`, which is built from **two** sites —
`postgres/grouped_stats.go:89-92` **and `postgres/searcher.go:220`**.

Absence therefore stays a nil pointer, and §8 pins all three consequences.

`Match(data []byte, meta EntityMeta)` mirrors `MatchFilter`'s existing shape and is
kept. An earlier draft justified it by claiming postgres must pass a document that
differs from `entity.Data`; that divergence was a bug and is fixed — every backend
now passes `entity.Data`. The signature stays because the sites supply bytes from
different sources (scanned rows, tx buffers, materialised snapshots) and threading a
`*spi.Entity` through them buys nothing.

Per-row `match.Match` callers hoisting `Prepare` above the loop:
`search/service.go:384`, `entity/grouped_stats_service.go:273`.
`workflow/engine.go:1022` re-sites in place (§6).

Comments naming `spi.MatchFilter` go stale across both repos and the `.md` tree;
they are updated in the same change. Three are wrong independently of this work
and are corrected: `eval_leaf.go:22-25` (the false "once-per-query work" claim,
plus `:29-31` documenting the deleted `EvalLeafString`), `regex_validate.go:25-34`
**and `:71`** (both mandate mirroring `opMatchesPattern`, which no longer exists
anywhere — also cited at `internal/domain/workflow/validate.go:215,269`), and
`filter_match.go:16-18` / `internal/match/match.go:291` (both cite a parity
scenario `MatchFilterSqliteEvaluateFilterParity` that does not exist).

`eval_leaf_test.go:45,265-270` is the existing fast-path-vs-general differential.
Deleting `evalLeafFast` makes it vacuous — it would compare the general path with
itself — so it is retired in the same commit as the deletion.

`internal/match/match_filter_sqlite_parity_test.go` (`:45`, `:325`) is the only
cross-module test pinning `internal/match.MatchFilter` ≡ `sqlite.EvaluateFilter`.
Both sides change, so it **migrates to the prepared form** — `match.Prepare(...)`
against `sqlite`'s prepared equivalent — rather than being deleted; it is the only
thing holding the two implementations to the same answer across the module
boundary.

## 8. Testing

**The merge gate** is equivalence of the rewritten tree walk.
`Prepare(f).Match(d, m)` must equal `MatchFilter(f, d, m)` over a randomised
`(filter, entity)` corpus, with no exceptions and no carve-outs — this change
alters no answers, so the net can demand exact agreement. The reference is a
frozen copy of `MatchFilter`, `evalFilter`, `evalLeafFilter`, `EvalLeafString` and
`evalLeafFast`; freezing only `evalFilter` leaves the reference calling live code
through the others, and `MatchFilter` itself carries the root `f.Op == ""` check
(`filter_match.go:41-44`) that §3's whole asymmetry table turns on. Same shape for `match.Prepare(...).Match(...)` against a frozen
`match.Match`. The generator emits only well-formed conditions, so it never
reaches §5's changed cases; those are covered by a separate hand-written table,
one row per declared change.

| property | how |
|---|---|
| compiles exactly once per query, SPI | internal test in package `spi`, swapping a package-level `compileRegex` indirection; exactly 1 compile across a 1000-row `Match` loop |
| zero-value semantics | all five rows of §3's table |
| concurrency | one `PreparedFilter` across N goroutines under `-race`, asserting all N agree on the result, not merely that nothing races |
| `postFilter` absence (§7) | a filter-free query still pushes `LIMIT`, still uses native `GROUP BY`, and does not arm the scan budget — for both spellings of match-all, the zero `Filter{}` and the explicit empty `AND` |
| cross-module agreement (§7) | the migrated sqlite parity test: `match.Prepare` ≡ sqlite's prepared evaluator |
| infra-failure precedence (§5) | model store down + `OR[$.age > 5, $.x IS_CHANGED]` reports the infra failure, not the client error. The first leaf must be one `Prepare` resolves a type for, or `loadErr` never latches and the row tests nothing |
| criterion behaviour change (§5) | E2E: a stored criterion `AND[state == "X", $.amount FROBNICATE 1]` makes the save return **400 `WORKFLOW_FAILED`** with the transaction rolled back, for an entity outside state X, where today it returns 2xx and the transition silently does not fire (`classifyWorkflowError`, `entity/service.go:2105-2151`) |

The compile-counter test must not be `t.Parallel()` and must not overlap the
shared-`PreparedFilter` concurrency test, or the indirection swap is itself a data
race.

**Two rows were considered and deliberately left out.**

A second compile counter at `plugins/memory`, to prove `Prepare` is not sitting
inside the row loop, would need the `compileRegex` indirection **exported** from
the SPI — the plugin is a separate module and cannot reach an unexported package
var. Adding test-only surface to a public SPI that three backends implement costs
more than it buys. Placement is instead pinned by §7 naming every hoist site
explicitly, and a regression is visible in the search benchmarks. This is the one
gap the test suite does not close; it is a review obligation, stated rather than
papered over.

A standalone `evalLeafFast` ≡ general-path differential is redundant. The merge
gate's reference includes `EvalLeafString`, which routes monomorphic
`String`/`UnboundDecimal` comparables through `evalLeafFast` first (SPI
`eval_leaf.go:523-532`), so any divergence the deletion could cause already
surfaces in the randomised corpus. Two prior runs (46,000 and 3,000,000 cases)
found none.

**Coverage waivers, stated explicitly** (`.claude/rules/test-coverage.md`):

- *No endpoint status table.* No API or gRPC surface changes shape, no new error
  codes, and §5 changes no status code. There is nothing to tabulate.
- *No gRPC row.* gRPC search funnels through the same `SearchService.Search` as
  HTTP, and no status or body reachable from it moves. The §5 criterion change **is**
  reachable over gRPC — `internal/grpc/entity.go:52,101,226,290,354` call the same
  entity handlers HTTP does — but the engine is transport-blind and both doors map
  the error through the same `classifyWorkflowError`, so a gRPC row would exercise
  the transport, not the change. Waived on that basis, not on unreachability.
- *No `e2e/parity` scenario.* The criterion change is engine behaviour evaluated
  identically on every backend — the storage layer is not consulted. Parity
  scenarios exist to catch backends disagreeing on one contract; there is no
  backend-varying answer here to pin. The `postFilter` row is deliberately
  per-backend rather than parity, because what it asserts (`LIMIT` pushdown,
  native `GROUP BY`, scan-budget arming) is backend-specific by construction.

## 9. A known divergence this change deliberately leaves alone

`CONTAINS "2021"` on `creationDate` matches via `spi.MatchFilter` and does not via
`internal/match.Match` — same API request, two answers depending on whether the
query pushes down. Measured, not inferred. It is a v0.8.3 regression (`27b29dc`,
#443, which added `isTemporalOperator` to one evaluator only).

It is **not** fixed here. Text and pattern operators on a temporal field are a
predicate cyoda-go#487 removes; making the two evaluators agree on what such a
query evaluates to would specify semantics for a feature being withdrawn, in code
#487 then makes unreachable. There is one fix — refuse the condition at the shared
validation boundary — and #487 owns it.

Leaving it also restores the strongest property this refactor can have: no answer
changes, so §8's equivalence gate needs no carve-out and no commit ordering.

Two things follow, and both are part of this change:

- **A comment at each evaluator**, at `internal/match.matchTemporalMeta` and at
  the filter-side leaf kernel, stating that text and pattern operators on a
  temporal meta field are not a supported predicate, that the two evaluators are
  known to disagree, and that the resolution is refusal at the validation boundary
  — **not** alignment in either evaluator. No issue number appears in the comment,
  per the project convention on shipped artefacts; it has to stand on its own.
- **The shipped docs stay as they are.** `cmd/cyoda/help/content/search.md` and
  `predicates.md` already describe these queries as rejected with
  `400 CONDITION_TYPE_MISMATCH`, rewritten ahead of the code on purpose so that
  refusing them later is a bug fix rather than the removal of an advertised
  feature. Do not "correct" them back to describe current behaviour.

## 10. Landing

1. **cyoda-go-spi**, to `main`. `### Breaking` CHANGELOG section with migration
   notes; both `KNOWN_CONSUMERS.md` entries notified before merge, linked from the
   PR body. The `// Deprecated:` one-release grace is waived deliberately: removal
   is the mechanism that forces each caller to re-site `Prepare`.
2. **cyoda-go**, to `release/v0.8.4`. Gate 7 applies to one thing only: §5's
   workflow-criterion change, where a stored criterion that previously failed to
   fire now fails the transition. Cloud mirrors criterion semantics, so it is
   recorded in `docs/cloud-parity/`. No status code moves, so nothing else in the
   contract is touched.
   `docs/workflow-schema-versioning.md` needs no schema bump (the import surface is
   unchanged) but does need a Changelog entry, for the same reason its v0.8.3
   malformed-regex entry exists: §5 newly rejects a stored criterion that
   previously ran. All four `go.mod` files currently pin the SPI tag `v0.8.3`; this
   change repins them to a pseudo-version, in the root and all three
   `plugins/*/go.mod` together — the `pin-sync` CI job (`.github/workflows/ci.yml`,
   via `scripts/check-spi-pin-sync.sh`) enforces that they agree. Separately,
   `make repin-plugins` pseudo-version-pins the in-repo plugin modules in the root
   `go.mod` to cyoda-go's own pushed HEAD; it is a local maintainer command, not a
   CI job, and does not touch SPI pins. The coordinated-release procedure is
   cyoda-go's `MAINTAINING.md` ("Coordinated release across sibling repos"); the
   SPI's pre-1.0 breaking-change rule is a three-condition conjunction, all three of
   which this change meets. `COMPATIBILITY.md`'s v0.8.4 row currently reads "no SPI
   change, deliberately … no SPI tag and no coordinated cross-repo release"; it is
   rewritten in this change, per Gate 4.
3. **Commercial Cassandra backend**, later: bumps `cyoda-go-spi`, `cyoda-go` and
   `plugins/{memory,postgres}` in one `go.mod` change and migrates its own call
   sites. It blank-imports two plugins at v0.8.3, so it cannot build against the
   new SPI until step 2 has merged and produced consumable plugin versions.

## 11. Not in scope

- **Bounding regex program size.** Computing it without acting on it is dead code;
  acting on it changes which queries are rejected, and the failure mode is a
  cross-backend decision. cyoda-go-spi#30's declared follow-up owns it and can land
  the computation and its consumer together. It was never blocked on this change:
  `regex_validate.go` already runs once per request. #30 argues the bound becomes
  affordable *inside* `Prepare`; putting it in `regex_validate.go` instead is what
  keeps `Prepare` error-free, which the Cassandra fan-out depends on.
- **Refusing conditions that cannot match** — cyoda-go#487. Covers the temporal
  divergence in §9, the validator/kernel pattern mismatch, unvalidated `LIKE`, and
  bounding numeric magnitude.
- **Unifying the two evaluators** — cyoda-go#464.
- **Per-request work budgets** — cyoda-go#475.
- **Allocations.** cyoda-go-spi#30 asks that `Prepare` on a pattern-free filter
  allocate no more than the old fused call. With `evalLeafFast` deleted, a one-shot
  monomorphic-`String` `EQ` leaf goes from zero allocations to roughly two, paid
  once per query and never per row. Accepted rather than keeping a duplicate
  evaluator alive to satisfy it. The remaining genuine one-shot caller is
  `evaluateCriterion`, which already pays a full `predicate.ParseCondition` per
  call.
