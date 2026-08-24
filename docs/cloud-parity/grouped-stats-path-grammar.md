# Grouped-stats path grammar — Cloud twin-alignment spec

cyoda-go leads this contract. `POST /entity/stats/{entityName}/{modelVersion}/query`
validates every `groupBy` entry and every aggregation `field` against a fixed
dotted-identifier grammar, at the API boundary, before any store access. Cloud
aligns to the same accept/reject set and the same error codes.

## The grammar

```
path    = "$." segment ( "." segment )*
segment = 1*( ALPHA / DIGIT / "_" / "-" )      ; ASCII only
```

The `$.` leader is **REQUIRED**. The validator does not rewrite: an accepted
path is echoed back verbatim in the response `groupKey[].path`.

The reserved `groupBy` token `state` (lifecycle state, never `$.`-prefixed) is
exempt and unchanged — it is a token, not a path. It is groupBy-only: there is
no defined aggregate over lifecycle state, so `state` as an aggregation `field`
is rejected as a path missing its leader.

Rejected, with **400 `INVALID_GROUP_BY_PATH`** for a `groupBy` entry and
**400 `INVALID_AGGREGATION_FIELD`** for an aggregation `field`:

- empty
- **no `$.` leader** — `country`, `a.b` (this is the 2026-08 tightening; see below)
- **bracket-quoted property access** — `$['country']`, `$.['country']`,
  `$['x']['y']` (write `$.country` / `$.x.y`)
- array projection or subscript — `$.items[*]`, `$.items[0]`, `$.items[?(@.x)]`
- recursive descent — `$..name`
- an empty segment: leading dot, trailing dot, `..` anywhere
- `$` on its own, or `$.` on its own
- any other byte: whitespace, `'`, `"`, `\`, `;`, `/`, `*`, `:`, `@`, `$` inside
  a segment, control bytes, and every non-ASCII rune

An array position is addressed as an ordinary numeric segment: `$.items.0`.

## 2026-08 tightening: the leader is mandatory, brackets are out

Two forms that were previously accepted are now rejected. Cloud must reject them
in step; this is a wire-contract change, not an internal one.

**Bare paths.** `groupBy: ["variantId"]` used to be silently read as
`$.variantId`. It is not a JSON Path, so it now errors. Project ruling: *"we
have a model syntax that is based on JSON Path nomenclature. Invalid paths are
invalid paths. We shouldn't try to accommodate incorrect syntax."* The
accommodation was also lossy in a visible way — the response echoed a
`groupKey[].path` spelling the client never sent.

**Bracket-quoted access.** `$['variantId']` used to be folded to `$.variantId`
before validation. It is now rejected, for a reason specific to this endpoint
carrying *two* path surfaces: the same request's `condition` `jsonPath` rejects
bracket forms (no evaluator in the stack resolves them). Folding them in
`groupBy` while 400ing them in `condition` would answer one request
inconsistently across two of its own fields.

Caller fix in both cases: write the path as JSON Path — `$.variantId`.

## Why the boundary, and why this width

The grammar is the one the storage SPI already documents on `Filter.Path` and
that every cyoda-go storage backend enforces on its own query paths — it is the
intersection every backend can serve, and on SQL backends it is also the
injection guard.

Enforcing it *only* in the backend was the defect this closes. cyoda-go's
grouped-stats service pushes the aggregation into the backend when it can and
otherwise tallies in process; the backend refused a malformed path but the
in-process tally did not — it resolved the path, missed, and bucketed every
entity as `null`. So the identical request answered `500` when pushed down and
`200` with wrong groups when not (a residual filter, a point-in-time query, or a
backend declining `stdev` are all enough to switch paths). Rejecting at the
boundary makes the two paths agree and leaves the backend check as a backstop.

The width is deliberate and is the part Cloud should weigh rather than assume: a
JSON key containing a space or a non-ASCII character is legal JSON and may be
reachable through a native JSON path expression on either platform. cyoda-go
still rejects it here, because a grouping dimension that only some execution
paths can resolve is worse than no result at all. If Cloud needs a wider set,
widen this grammar on both sides — do not widen one platform's validator.

## Test surface

- `internal/domain/entity/grouped_stats_validation_test.go` — reject table,
  positive-control table, and `TestValidateGroupedStatsRequest_StateIsATokenNotAPath`
  (the `state` token's exact scope).
- `internal/domain/entity/grouped_stats_validation_fuzz_test.go` — validator
  never panics; an accepted path is itself valid input and comes back unchanged.
- `internal/e2e/grouped_stats_invalid_path_test.go` — status + error code over
  real HTTP on postgres: `TestGroupedStats_NonJSONPathForms_Returns400` on both
  the pushdown-eligible and the point-in-time (streaming) shape,
  `TestGroupedStats_NonJSONPathCondition_Returns400` for the `condition`
  surface, and `TestGroupedStats_ValidPathForms_Still200` as the accept-side
  control.
- `e2e/parity/search_path_key.go` —
  `RunGroupedStatsPathRequiresJSONPathLeader`, registered in
  `e2e/parity/registry.go`, so every backend (including the commercial one)
  picks it up.

The parity scenario is there because the group path reaches either the plugin's
own validator (pushdown) or gjson (streaming tally), and which one depends on
the backend and the query shape — the exact split this validation exists to
close.
