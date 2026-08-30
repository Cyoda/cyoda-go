# Changelog

All notable changes to Cyoda-Go are documented here. The project follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) conventions and [Semantic Versioning](https://semver.org/) — pre-1.0, where the minor component signals a breaking change and new features ship in patches (see [README — Versioning](./README.md#versioning)).

## [Unreleased]

### Breaking

- **The `POLYMORPHIC_SLOT` error code is retired.** It meant "raising
  `changeLevel` will not help you". Giving a path a kind it does not declare is
  a `STRUCTURAL` change now, so raising the level is exactly what resolves it,
  and the extension path has no rejection left that the code described. Below
  `STRUCTURAL` such a write answers `400 VALIDATION_FAILED` like any other
  change-level violation, with the level named in the message. The help topic
  `errors.POLYMORPHIC_SLOT` goes with it.

- **A model's schema node holds the set of kinds it was observed as.** The
  persisted form gains `"kinds"`; a node with at most one branch still writes
  `"kind"`, so every monomorphic node — nullable or not — serialises
  byte-identically to before and no model needs migrating. Both spellings are
  accepted on read, and a node stored under the old single-label form restores
  every branch its payload carries rather than the one the label happened to
  name. `cyoda-go-spi` carries the node, the codec and the field walk now, and
  its `ModelNode` API changed accordingly: an out-of-tree plugin that decodes a
  schema itself needs the new pin.

- **A search whose model schema cannot be loaded now fails instead of
  answering.** Field-path validation consults the model's schema to decide
  whether a condition's paths exist. When that load failed — the model store
  unreachable, or the stored schema unparseable — validation was skipped and
  the query ran anyway, returning `200` with a result set.

  The result set was not merely unvalidated, it was **wrong**. With no fields
  map the translator stamps an empty declared-type set on every leaf, and that
  does not degrade leaves uniformly: the eight comparison and ordering
  operators (`EQUALS`, `NOT_EQUAL`, the four inequalities, `BETWEEN`,
  `BETWEEN_INCLUSIVE`) collapse to a non-match while the other eighteen — the
  presence tests, the string and pattern operators, and the case-insensitive
  family — keep matching. Rows that should have matched were dropped, silently,
  and the short page was indistinguishable from a complete one.

  A schema-load failure is now `500` with a ticket id on `/search/direct` and
  `/search/async`, over HTTP and gRPC alike. An async job whose schema becomes
  unreadable between submit and execution — a load separate from the one submit
  performed — ends `FAILED` rather than recording the same short page as
  `SUCCESSFUL`.

  Conditional `DELETE /entity/{name}/{version}` and grouped stats already
  failed closed on this and are unchanged.

  A **lifecycle-only** condition is unaffected and still succeeds: a meta leaf
  takes its type from the static meta vocabulary, not from the model schema, so
  the schema is not a dependency of that request.

- **A condition naming a data path on a model that declares no fields is now
  rejected**, on `/search/direct`, `/search/async`, conditional
  `DELETE /entity/{name}/{version}` and grouped stats. Such a model was previously treated as
  "nothing to validate against", so any path at all was accepted and the query
  answered — and on the delete path that decided which rows were removed. It is
  instead a model in which the named path does not exist, and the request is
  `400 INVALID_FIELD_PATH`, the same answer any other unknown path already
  received.

  This state is **not reachable through the public API**: model import always
  stores a marshalled schema, and a schema declaring no fields still yields a
  non-empty fields map, which already rejected unknown paths. The change closes
  it against an out-of-band or legacy row rather than against a request anyone
  can send today, which is why no E2E test accompanies it.

- **Grouped stats now validates its paths against the model.** It previously
  performed no schema-membership check of any kind — not on the condition, the
  `groupBy` paths, or the aggregate fields — while `/search/direct` and
  conditional delete both rejected an undeclared path with
  `400 INVALID_FIELD_PATH`.

  What made this worse than a missing check is that the answer looked real. A
  condition leaf on an undeclared field annihilates to a non-match and returns
  no buckets; an undeclared `groupBy` path buckets every entity together under
  `"value": null`; and an undeclared `SUM` reports `"total": null` alongside a
  correct `count`. All three returned `200`.

  Its condition type check was schema-blind for the same reason — the service
  passed a nil model, so only the model-independent arm ran. An operand parsing
  into none of a declared field's types is now `400 CONDITION_TYPE_MISMATCH`,
  matching `/search/direct`.

  All three surfaces get the same bounded single schema refresh search and
  delete already had, so a field a peer node has just added to the model is not
  falsely rejected on a node whose cached descriptor predates the schema-change
  event.

  A path's shape is held to the model on the endpoints that validate:
  `$.items[*].sku` asserts `items` is an array and `$.items.sku` asserts it is
  an object, and the spelling that contradicts the model is rejected rather
  than reinterpreted.

- **An invalid `LIKE` or `MATCHES_PATTERN` operand is now rejected at the
  request boundary instead of being accepted.** The boundary used to compile
  the operand on its own, while every evaluator compiles the *anchored* form,
  `\A(?:operand)\z` — two derivations of one rule, in two repositories. They
  disagreed, and the request went through anyway:

  - **`MATCHES_PATTERN` was accepted and then failed.** An unterminated `\Q`
    quotes whatever follows it, so `\Q` compiles standalone but swallows the
    anchor wrapper's own `)\z`. The caller got a `200` and a job id, and the
    job went `FAILED` — an error surface they had been told at the boundary
    they would not hit.
  - **`LIKE` was not validated at all.** A trailing unpaired escape (`abc\`)
    reached the evaluator, where an operand that cannot be expanded becomes a
    leaf that never matches: a `200` and an empty page. It was also a
    cross-backend divergence — the in-tree evaluators returned empty where the
    commercial async evaluator failed every shard of the job.

  Both surfaces now call the kernel's own derivation, so the boundary accepts
  exactly what the evaluator accepts. Those two classes are the whole of the
  change: **a request carrying one of them returned `200` before and returns
  `400` now**. Nothing that was rejected before is accepted now, and a
  well-formed pattern is unaffected. An async submit rejects synchronously — no
  job is created.

  Affected surfaces and codes: `400 INVALID_CONDITION` on `/search/direct`,
  `/search/async`, conditional `DELETE /entity/{name}/{version}` and the
  grouped-stats `condition`; `400 VALIDATION_FAILED` on workflow import, where a
  workflow or transition `criterion` carrying such an operand is now rejected
  rather than misbehaving on every later evaluation of the transition. HTTP and
  gRPC alike.

  Workflows already stored are not re-validated: a criterion imported before
  this release keeps evaluating as a leaf that never matches until it is
  re-imported. That matches how the other import-time structural rules behave.

- **`LIKE` is now matched as a glob, not translated into a regular expression.
  Two caller-visible behaviours change.** `LIKE` used to be rewritten into a
  regex and handed to the regex engine; it is now matched directly by a glob
  matcher in the shared kernel. The change is that the regex engine no longer
  sees the operand at all, so nothing can leak through to it:

  1. **`%` and `_` now match a newline.** They were rewritten to `.*?` and `.`,
     which do not match `\n` without the dot-all flag. A stored value containing
     a newline silently failed to match a pattern that should have matched it;
     it now matches.
  2. **A backslash escape is now literal, where the regex engine used to
     interpret it.** The rewriter passed `\` through untouched, so a regex
     escape survived into the compiled pattern: **`LIKE "\d"` matched any
     digit** — `"7"` matched it — and `\w`, `\s`, `\b`, `\n`, `\t` behaved as
     their regex selves too. `\` now escapes the character after it to its
     literal form, whatever that character is, so `\d` matches the single
     character `d`. Any operand carrying a backslash before an ordinary
     character changes meaning. Escaping `%`, `_` and `\` is unaffected: `\%`
     was a literal `%` before and still is.

  A pattern ending in an **unpaired `\`** is now a named error condition rather
  than an accident. It used to produce a regex that failed to compile, and a
  leaf whose pattern will not compile never matches, so the search succeeded
  with an empty result. It is now invalid, and the request is rejected at the
  boundary — see the entry above. Below the boundary the evaluator still treats
  it as a leaf that never matches, but no caller reaches that. Spell a literal
  trailing backslash `\\`.

  Literal text is compared bytewise, so an operand carrying invalid UTF-8 now
  matches the byte-identical stored value instead of being transcoded to U+FFFD;
  and `_` advances by one UTF-8 rune rather than one byte.

  Affected surfaces: every one that takes a condition — `/search/direct`,
  `/search/async`, conditional `DELETE /entity/{name}/{version}`, the
  grouped-stats `condition`, and a workflow or transition `criterion`. HTTP and
  gRPC alike. An invalid pattern is now rejected at the API boundary — see the
  entry above.

- **A path whose last hop is an array wildcard now addresses the array's
  ELEMENTS. It used to resolve to the array's length.**
  `$.tags[*]` means "some element of `tags`". It was resolved to the *count* of
  `tags`, so a comparison on it compared the operand against a number:
  `{"jsonPath":"$.tags[*]","operatorType":"EQUALS","value":"red"}` compared
  `"red"` against `2` and never matched.

  **This changes results for any caller using such a path.** A search that
  returned an empty page now returns the matching entities. A workflow criterion
  that silently never fired now fires — so entities that sat in the state before
  a guarded transition will start advancing through it on their next save. In the
  other direction, a comparison that happened to hold against the *length*
  (`$.tags[*] GREATER_THAN 1` on a three-element array; `NOT_NULL` on an **empty**
  array, whose length `0` is a present number) no longer matches. Presence tests
  are existential and therefore vacuously false on an empty array: neither
  `NOT_NULL` nor `IS_NULL` matches `{"tags": []}` on `$.tags[*]`.

  Affected surfaces: every one that takes a condition — `/search/direct`,
  `/search/async`, conditional `DELETE /entity/{name}/{version}`, the
  grouped-stats `condition`, and a workflow or transition `criterion`. HTTP and
  gRPC alike.

  Multiple array hops were broken by the same arithmetic and are fixed with it,
  including paths with **no** trailing wildcard: `$.matrix[*][*]`,
  `$.a[*].b[*]` and `$.orders[*].lines[*].sku` all compared against a nested
  array rather than the values they address, and never matched.

  Newly rejected: a trailing wildcard on an array of **pure objects**
  (`$.items[*]` where every element is `{"sku": …}`) with a scalar operand is
  **400 `INVALID_FIELD_PATH`** — the element has substructure and no scalar form,
  so the comparison could only ever be false. Navigate to the leaf sub-path
  (`$.items[*].sku`). An array of scalars, and an array whose elements were also
  observed as bare scalars, stay valid; so do `IS_NULL` / `NOT_NULL` on any of
  them, which carry no scalar operand.

  ```diff
  - {"type":"simple","jsonPath":"$.items[*]",     "operatorType":"EQUALS","value":"A1"}
  + {"type":"simple","jsonPath":"$.items[*].sku", "operatorType":"EQUALS","value":"A1"}
  ```

  Remedy for a caller who was relying on the length: there is no path spelling for
  it. Address the elements, or filter on a field that carries the count.
  See `docs/cloud-parity/path-grammar.md`.

- **A field path must now be written as JSON Path — the `$.` leader is required,
  and the whole path is validated.**
  A bare `amount` is not a path and is rejected; it is no longer read as `$.amount`.
  So are bracket-quoted property access (`$['x']`, `$.['x']`, `$.a["b"]`), an empty
  or trailing segment (`$..a`, `$.a.`), and any character outside
  `1*( ALPHA / DIGIT / "_" / "-" )`. The grammar is now:

  ```
  jsonPath  = "$." segment ( "." segment )*
  segment   = name subscript*
  name      = 1*( ALPHA / DIGIT / "_" / "-" )   ; ASCII only
  subscript = "[" ( "*" / 1*DIGIT ) "]"          ; the digit run must fit an int32
  ```

  The digit-run bound is `int32`, not Go's `int` (`int64` on every supported
  platform): `int32` is the intersection every in-tree backend can address —
  PostgreSQL renders a positional index as a `jsonb` operand, and an index
  above `int32` fails to parse there (`jsonb ->> bigint` does not exist) rather
  than answering a result, which without a backend-specific error classifier
  surfaced as an unclassified `500` instead of a `400`. `$.tags[2147483647]`
  (`int32` max) stays accepted; `$.tags[2147483648]` is rejected the same as
  any other malformed subscript.

  ```diff
  - {"type":"simple","jsonPath":"amount",      "operatorType":"GREATER_THAN","value":50}
  + {"type":"simple","jsonPath":"$.amount",    "operatorType":"GREATER_THAN","value":50}
  -   "groupBy": ["variantId"]
  +   "groupBy": ["$.variantId"]
  ```

  Affected surfaces and codes: a condition `jsonPath` → **400 `INVALID_FIELD_PATH`**
  on `/search/direct`, `/search/async`, conditional `DELETE /entity/{name}/{version}`
  and the grouped-stats `condition`; a grouped-stats `groupBy` entry → **400
  `INVALID_GROUP_BY_PATH`**; an aggregation `field` → **400
  `INVALID_AGGREGATION_FIELD`**. HTTP and gRPC both reject (gRPC as an envelope
  error, not an empty stream).

  Before, a bare condition path returned **200 with correct-looking results**: the
  pushdown translator refused it, but every call site treats a translate failure as
  "fall back to in-memory evaluation", and that evaluator resolves a bare path
  happily — so the query silently ran as a full scan. Bracket-quoted access was
  worse: nothing in the stack resolves it, so it answered an empty page for a field
  that exists. A bare `groupBy` entry was rewritten to `$.`-form, and the response
  echoed a group-key path the client never sent; anything else malformed
  (`$.first name`, `$..name`, `$.café`, `.leading`, `trailing.`, a bare `$`)
  reached the storage layer, and what happened there depended on how the query
  ran — the pushdown path failed **500**, while any request the backend declined
  to push down (a residual filter, a point-in-time query, sqlite declining
  `stdev`) fell through to the in-process tally, where the lookup missed and
  every entity landed in one `null` bucket: a plausible-looking, wrong **200**.

  **Malformed array subscripts are newly rejected, and this is the class most
  likely to bite.** The path used to be scanned only as far as the first `[`;
  everything after it went unread, so `$.a[-1]`, `$.a[0:2]`, `$.a[0,1]`,
  `$.a[?(@.x)]`, `$.a[]`, `$.[0]`, `$.a[ 0]`, an unclosed or unmatched bracket
  (`$.a[`, `$.a[0`, `$.a]`) and even trailing junk after a valid subscript
  (`$.a[0]b`, `$.a[0];DROP`, `$.a[*]..b`) all classified as "not pushdownable"
  and fell back to the in-memory evaluator — which resolves none of them. The
  answer was **200 with an empty page** for a field that exists, or on the two
  surfaces with no schema backstop behind them, worse: a grouped-stats
  `condition` (validated against a nil model) returned **200 with wrong
  buckets**, and a workflow criterion imported cleanly and then **silently never
  fired**. All are now rejected at the boundary, each with its own surface's
  code — `INVALID_FIELD_PATH` on a condition, `INVALID_GROUP_BY_PATH` /
  `INVALID_AGGREGATION_FIELD` on grouped stats, and `VALIDATION_FAILED` at
  workflow import (see the next entry).

  Still accepted: condition paths with a **well-formed** subscript — the wildcard
  `[*]` or a non-negative index (`$.tags[*].name`, `$.arr[0]`, `$.matrix[*][*]`,
  `$.orders[*].lines[*].sku`) — valid JSON Path. A positional index now pushes
  down like any other field; a wildcard leaf still evaluates in memory, because
  no backend has a wildcard accessor. Grouped-stats `groupBy`/`field` still
  reject every subscript, well-formed or not, because a group key must be a
  single scalar.
  The reserved `groupBy` token `state` is a token, not a path, and needs no leader;
  it is groupBy-only, so `state` as an aggregation `field` is now rejected.
  Workflow criteria obey the same grammar, enforced at workflow import — see the
  next entry.

  Fix for callers: prefix the path with `$.`, and replace bracket access with dotted
  access. Replace a malformed subscript with `[*]` or a non-negative index — there
  is no rewrite for a slice, union, filter expression or negative index, because no
  evaluator ever resolved them, so a query using one was already returning an empty
  page. On grouped-stats `groupBy`/`field`, address an array position as a numeric
  segment instead (`$.items.0`). See
  `docs/cloud-parity/path-grammar.md`, or `cyoda help crud`.

- **A workflow or transition `criterion` `jsonPath` must now be JSON Path too, and
  is rejected at workflow import.**
  A criterion uses the same model syntax as a search condition, but it evaluates
  through the in-process predicate evaluator and never through the pushdown
  translator — so nothing rejected a bare path, and a criterion on `amount`
  imported cleanly and fired transitions. One syntax, two spellings of what a
  path is.

  ```diff
    "criterion": {
  -   "type": "simple", "jsonPath": "amount",   "operatorType": "GREATER_THAN", "value": 50
  +   "type": "simple", "jsonPath": "$.amount", "operatorType": "GREATER_THAN", "value": 50
    }
  ```

  Before: `POST /api/model/{entityName}/{modelVersion}/workflow/import` accepted it
  with **200** and the transition fired. After: **400 `VALIDATION_FAILED`**, with the
  offending workflow / state / transition named in `detail` — the same code and shape
  every other import-time criterion rejection uses. Checked on `simple` and `array`
  clauses at any nesting depth; a `lifecycle` clause names a meta field rather than a
  path and a `function` clause carries none, so both stay exempt. Array subscripts
  (`$.tags[*].name`, `$.arr[0]`) stay valid — criteria are evaluated in memory, which
  resolves them.

  Validation runs on the incoming request only, so an already-stored workflow keeps
  evaluating; it fails on its next re-import, which is where the fix gets made.

  Fix for callers: prefix the path with `$.`. See
  `docs/cloud-parity/path-grammar.md`.

- **`CYODA_TX_TTL`, `CYODA_TX_REAP_INTERVAL` and `CYODA_TX_OUTCOME_TTL` are removed.**
  They configured a transaction reaper that never ran — nothing ever registered a
  transaction with it — so the TTL they advertised was never enforced. The reaper and
  its package are deleted; setting the variables now has no effect. A transaction's
  lifetime is bounded instead by a deferred rollback on every exit path, plus the
  PostgreSQL ceilings below.

- **PostgreSQL connections now carry `statement_timeout` and
  `idle_in_transaction_session_timeout`, both defaulting to `5m`.** A statement that
  runs longer, or a connection that sits idle inside an open transaction longer, is
  aborted by the server. Set `CYODA_POSTGRES_STATEMENT_TIMEOUT=0` or
  `CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT=0` to disable either. A workflow processor whose
  `responseTimeoutMs` exceeds the idle ceiling has its transaction aborted; the default
  `responseTimeoutMs` of 30s sits well under it. The idle ceiling applies per gap, not
  per transaction, so a long cascade that writes between callouts is unaffected.

- **Acquiring a pooled connection now waits at most
  `CYODA_POSTGRES_ACQUIRE_TIMEOUT` (default `10s`)** and then fails with **503
  `STORAGE_UNAVAILABLE`**, retryable, instead of queueing behind a saturated pool.
  Two classes are covered. Opening a transaction: entity writes, and the schema
  extension an auto-evolving model performs — which previously reported the same
  saturated pool as a `500` with a ticket. And needing a *second* connection
  while the caller's transaction already holds one: a point-in-time read or an
  async-search submit issued inside a transaction, both of which deliberately
  run off the transaction. The timeout does not bound a plain non-transactional
  read, which waits on the pool without a deadline. (The async-search
  scan is classified the same way now, but its job record already reported a
  fixed message and is unchanged.)

- **The SQLite backend now opens a dedicated read connection pool, raising its
  memory ceiling.** Reads (paged lists, change history, by-transaction lookups,
  non-transactional iteration, async-search result pages) move off the single
  writer connection onto a second pool, so a long undrained scan can no longer
  starve concurrent writes or queue an interactive read behind itself.
  `CYODA_SQLITE_CACHE_SIZE` (default `64000` KiB) is a **per-connection** page
  cache, so the resident ceiling is now `(readers + 1) × CYODA_SQLITE_CACHE_SIZE`
  rather than one cache: on an 8-CPU host with the defaults, ≈ 562 MiB where it
  was ≈ 62.5 MiB.
  The new `CYODA_SQLITE_READER_POOL_SIZE` sizes the pool (default `GOMAXPROCS`
  clamped to `4`..`8`; minimum 1, and a value below it falls back to the
  default). `GOMAXPROCS` follows the CPU quota and is
  blind to the memory limit, so a container generous on cores and tight on
  memory must lower this — not `CYODA_SQLITE_CACHE_SIZE`, which shrinks the
  writer's cache along with the readers'. `cyoda help config database`.

- **With `CYODA_POSTGRES_AUTO_MIGRATE=true`, migrations now run before the
  schema-compatibility check.** A node booting alongside a peer's in-flight migration
  waits for it rather than exiting with a dirty-schema error. A schema genuinely left
  dirty by a failed migration still refuses to start, with the same actionable message,
  and a database newer than the binary is still refused.

- **A workflow criterion carrying an operator nobody can evaluate now fails the
  save.** Workflow import validates a `MATCHES_PATTERN` regex but not an
  operator name, so `AND[state == "SHIPPED", $.amount FROBNICATE 1]` stored
  cleanly. The evaluator used to walk the condition lazily and short-circuit
  past the bad operator for any entity outside `SHIPPED`, so the save
  returned 2xx and the transition silently never fired. The whole condition
  is now inspected up front, so the same import now fails with **400
  `WORKFLOW_FAILED`** and the transaction rolls back. A criterion nobody can
  evaluate must not be read as "condition not met" — fix the operator name
  before importing.

- **A search or criterion group condition must use exactly `AND` or `OR`.**
  `GroupCondition.Operator` was never checked at validation, so anything else
  cleared it and the two execution paths disagreed on what to do with it: the
  pushdown translator mapped any non-`OR` value (matched case-insensitively)
  to `AND` and answered **200** with the wrong rows, while the in-memory
  fallback raised a structural error that surfaced as a **500** on
  client-supplied input. Both now reject it at the shared validation boundary
  with **400**. This is case-sensitive — lowercase `"or"` is rejected too,
  matching the parser and the evaluator, neither of which ever accepted it.

- **Model field names must be addressable by a search `jsonPath`.** A field name
  is now accepted only if it is a valid `jsonPath` segment: one or more ASCII
  letters, digits, `_` or `-`. Anything else — spaces, dots, quotes, brackets,
  `$`, `@`, `:`, the evaluator's own metacharacters (`*`, `?`, `#`, `|`, `!`,
  `\`), or any non-ASCII character — and the empty name are rejected
  with **400 `VALIDATION_FAILED`**, naming the offending key and the object that
  declares it. The rule is enforced on both paths that establish a model's field
  set: the sample-data model import, and the ChangeLevel-driven schema extension
  performed by an entity write (single, collection, transition, and
  processor-returned data), over HTTP and gRPC alike.

  Previously the model layer recorded any JSON key while the query layer could
  address only this charset, so a document could establish a field that nothing
  could ever search. Ingestion that previously succeeded will now fail.

  No migration is provided: rename the key in the source data and re-establish
  the model. See `docs/cloud-parity/model-field-name-grammar.md`.

- **A storage backend rejecting a malformed field path now answers 400, not a
  500 with a support ticket.** Each plugin keeps its own path check as a
  backstop behind the API boundary; when one fired, the engine had no
  classification for it, so malformed input surfaced as an internal error with a
  ticket UUID — inviting an operator to investigate a server fault that was
  really just bad input. The `spi.ErrInvalidFilterPath` sentinel is now mapped to
  **400 `INVALID_FIELD_PATH`** (with a server-side WARN, since reaching the
  backstop means the boundary grammar and a plugin's check disagree). The
  mapping is applied on both store branches — the bounded `Search` call and the
  unbounded `Iterate` drain, the latter of which had no classification at all —
  so the same input no longer answers 400 or 500 depending on whether the
  request carried a positive `limit`.

- **On PostgreSQL, a `pointInTime` read issued inside a joined transaction is
  now committed-only.** It ran on the caller's own transaction connection, so a
  snapshot read answered with that transaction's *uncommitted* writes: an entity
  the transaction had just created was returned by a read taken "as at" an
  instant before it existed, and one it had updated came back at the uncommitted
  payload. It now ignores the ambient transaction and answers from committed
  state. **Memory and sqlite already behaved this way** — they buffer
  in-transaction writes off the store, so a point-in-time query never saw them —
  so this closes a backend divergence rather than changing a cross-backend
  contract. Only PostgreSQL deployments see a difference, and PostgreSQL is the
  production backend, so treat it as breaking.

  Five read families change — the single-entity read (`GET
  /api/entity/{entityId}?pointInTime=` and `.../transitions?pointInTime=`), the
  model-scoped list (`GET /api/entity/{entityName}/{modelVersion}?pointInTime=`),
  direct search and conditional `DELETE /api/entity/{entityName}/{modelVersion}`
  with `pointInTime`, the streamed scan behind grouped stats
  (`POST /api/entity/stats/{entityName}/{modelVersion}/query` with
  `pointInTime`), and the gRPC mirrors of all of them.

  This is reachable over the wire, not just through the SPI: the `X-Tx-Token`
  join middleware wraps the whole API mux (`internal/httpmw/txjoin_mw.go`), so
  a compute-node callback running inside a transition's transaction takes
  exactly this path and now gets `404 ENTITY_NOT_FOUND` where it previously got
  the entity.

  Fix for callers: omit `pointInTime`. A current-state read inside a transaction
  is read-your-own-writes correct, and is the read a caller wanting its own
  writes back should be using. See `docs/cloud-parity/tx-aware-search.md`.

- **A value whose kind the field does not declare is now rejected.** A field
  declared `STRING` accepted an array or an object on write and stored it,
  while correctly refusing a number or a boolean in the same field. Validation
  asked what `DataType` a value has before asking whether its JSON kind was
  admissible at all, and the classifier answers `STRING` for anything it does
  not recognise — so a container matched a `STRING` declaration. The reverse
  direction was always enforced (`expected array, got string`), so the hole was
  one-directional.

  A leaf declaration now checks kind first, and answers `400 VALIDATION_FAILED`
  with `expected scalar, got array` — the mirror of the check a container
  declaration already made. `null` is unchanged: it follows the declaration —
  always admissible on a scalar field, and on a container field where the model
  observed one — and is not a kind of its own.

  A genuinely polymorphic field — one observed in more than one kind while the
  model is `UNLOCKED` — declares each kind and admits all of them. Validation
  now selects the branch by the value's own kind rather than by the node's
  dominant one, which is what makes the array branch of an object-and-array
  union admissible; and the persisted schema carries every branch back, where it
  used to restore only the children of such a node and silently narrow the model
  on the first read back.

  This closes the only door through which an array reached a field whose
  declared type says it cannot hold one, and with it a class of stored value no
  predicate on that field could address consistently.

  Kind mismatches also name the offending kind in the wire vocabulary now, and
  name every kind the field does declare (`expected object or array, got
  string`), rather than leaking a Go type name (`got map[string]interface {}`).

  With a `changeLevel` set, every declared kind is writable and a new kind is
  added at `STRUCTURAL` — see the entries below, which land in this same
  release.

- **A payload that fails against the model now answers `400 VALIDATION_FAILED`,
  not `400 BAD_REQUEST`.** The error dictionary already drew the line here:
  `VALIDATION_FAILED` is "the payload parses but violates the registered model
  schema", and `BAD_REQUEST` is "the server cannot parse the request". The
  entity handler did not follow it — its catch-all answered `BAD_REQUEST` for
  every generic validation failure, so both codes' documented meanings were
  wrong and an SDK could not branch on them.

  The catch-all now answers `VALIDATION_FAILED`, on every entity ingress. It
  covers an undeclared field, a value whose kind the field does not declare, a
  change the `changeLevel` does not permit, and an unaddressable field name.
  `INCOMPATIBLE_TYPE` (a leaf's DataType) is unchanged, and so is every
  `BAD_REQUEST` raised before validation — an unparseable body, a parameter out
  of range, unstorable bytes. A payload that proposes a kind change now answers
  `VALIDATION_FAILED` too; see the `POLYMORPHIC_SLOT` entry under Breaking.
  The status stays `400` throughout, so only a client that branches on the code
  is affected. See `docs/cloud-parity/validation-failure-code.md`.

- **A JSON array posted to the sample-data import registers a different model,
  and some previously-accepted bodies are now refused.** See the entry under
  Fixed: an array body used to register a model describing an array at the root,
  and a scalar body used to register a model rooted at a scalar — both `200`,
  both unusable. The array body now derives the merge of its documents; a body
  that is neither a document nor a collection of documents is `400
  VALIDATION_FAILED`.

- **`SIMPLE_VIEW` and `JSON_SCHEMA` emit different keys for two shapes.** Also
  detailed under Fixed. An array of arrays moves from `.m[*]` to `.m[*][*]`; a
  field declaring more than one kind gains a second entry for its other branch;
  an array whose elements were never observed is named `.a[*]: NULL` instead of
  being omitted; and `JSON_SCHEMA` unions render as `anyOf` rather than `oneOf`
  (`oneOf` requires exactly one branch to match, so it rejected values the model
  admits whenever two branches rendered the same JSON Schema shape). Consumers
  that parse the exported model see the new keys.

- **One path grammar and one resolver now govern every surface that carries a
  `jsonPath`** — search conditions, workflow criteria, `groupBy`, aggregation
  fields and sort keys. See `docs/cloud-parity/path-grammar.md` and
  `docs/cloud-parity/operator-semantics.md`. Caller-visible consequences:

  - **An `array` clause's `jsonPath` must now carry a trailing `[*]`.** A bare
    path (`{"type":"array","jsonPath":"$.tags","values":["A"]}`) addresses the
    array itself, not its elements, and cannot carry a positional test. It is
    `400 INVALID_FIELD_PATH` on the search surface and `400 VALIDATION_FAILED`
    at workflow import — both doors now enforce the same rule; before this
    fix the criterion door accepted a clause the search door already refused.

  - **An `array` clause's `values` are now type- and shape-checked**, the same
    checks a `simple` clause already had. An object entry (`{"a":1}`) is now
    `400 INVALID_CONDITION` instead of reaching the evaluator and being
    compared as the literal text `map[a:1]`.

  - **An unknown `operatorType` now answers `400 INVALID_CONDITION` on every
    surface that carries a condition, not just some of them.** It previously
    answered `400 INVALID_CONDITION` on grouped stats but fell through to the
    coarser `400 BAD_REQUEST` on `/search/direct`, `/search/async` and
    conditional `DELETE /entity/{name}/{version}` — one error class, two codes
    depending only on which endpoint served the request. The status stays
    `400` throughout; only the code changes.

  - **A workflow or transition criterion carrying an unknown `operatorType`
    now fails import**, `400 VALIDATION_FAILED`, naming the offending
    operator, workflow and transition. It previously imported cleanly — the
    operator table was never consulted at this door — and the transition it
    guarded then silently never fired on every later evaluation, with no
    result page to look wrong.

  - **A string or pattern operator (`CONTAINS`, `MATCHES_PATTERN`, the
    case-insensitive family, …) on `creationDate` or `lastUpdateTime` is now
    `400 INVALID_CONDITION`.** It previously answered `200`, and which rows
    came back depended on which evaluator served the request: a pushdown
    route bridges the field to its RFC3339 text and matches lexically, while
    the in-memory evaluator and every workflow criterion refused the operator
    and never matched. The same condition and the same data answered two ways
    depending only on the query plan; rejecting it at the shared boundary
    makes both answers unreachable rather than picking one.

  - **A bare path no longer matches an array's elements, and a `[*]` path no
    longer matches a scalar value.** The in-memory evaluator (the memory
    backend, the SQL backends' residual re-check, and every workflow
    criterion) used to route on the *stored value's* shape: a bare path over
    an array matched existentially across its elements, and reaching a scalar
    behind a `[*]` hop fell through to comparing the scalar directly. What a
    path addresses is now decided by the path's syntax alone, matching the
    pushdown kernel — see section 3's union rule. A field that is
    consistently one shape across every entity is unaffected; a polymorphic
    field observed as both a scalar and an array may see fewer matches on a
    bare or wildcard path than before, and more on the path that was already
    the well-formed spelling for that entity's branch.

  - **A subscripted path (`tags[0]`, `tags[*]`) is rejected on `groupBy`, an
    aggregation field and a sort key**, `400`, on every backend. These three
    surfaces name one scalar value per entity; a subscript names an array
    position or a set, neither of which is a grouping dimension, an
    aggregation field or a sort key. `path-grammar.md` section 7 states the
    rule; the three surfaces share one scanner with the filter-path grammar
    minus the subscript production, so they cannot drift from it again.

### Added

- **`STORAGE_UNAVAILABLE` — 503, retryable.** Raised when the pool cannot supply a
  connection within the acquire timeout, when an operation finds its transaction
  already aborted by the idle-in-transaction ceiling, or when the database connection
  goes away underneath it. `503` is now declared on every storage-backed operation in
  `api/openapi.yaml` — 52 of them, because any storage-backed operation can meet an
  outage, not only the entity writes. Fifty share one response component; the two
  transitions reads keep their own, since those also answer `503` when a `function`
  selection criterion has no connected compute member.
  `cyoda help errors STORAGE_UNAVAILABLE`.

- **Five PostgreSQL ceilings:** `CYODA_POSTGRES_STATEMENT_TIMEOUT` (`5m`),
  `CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT` (`5m`), `CYODA_POSTGRES_ACQUIRE_TIMEOUT` (`10s`),
  `CYODA_POSTGRES_MIGRATE_LOCK_TIMEOUT` (`5m`) and
  `CYODA_POSTGRES_SEARCH_STATEMENT_TIMEOUT` (`30m`). Each takes a Go duration; `0`
  disables that limit, and a malformed value fails startup rather than silently
  falling back to the default. `cyoda help config database`.

- **Panic recovery on the gRPC server (unary and stream) and on every HTTP route**,
  which previously covered only the `/` catch-all — so a gRPC panic killed the process
  and an HTTP panic on any specific route dropped the connection with no ProblemDetail
  and no ticket. A recovered panic at any of the four sites that run engine or store
  work — the two request doors, the async-search goroutine and the scheduler's dispatch
  goroutine — permanently marks the node unhealthy: `GET /health` reports `503 DOWN`
  and the admin `/readyz` reports `503`, so Kubernetes drops the pod from its Service
  and new client connections stop within ~10-15s. The node's state is unverified, so
  withdrawing it is deliberate. Know the bound, though: peer-forwarded work keeps
  arriving — peers address each other through the gossip registry rather than the
  Service, and scheduler distribution does not read node liveness — established
  connections stay open, and nothing restarts the node, since `/livez` is unchanged
  (a deterministic panic would otherwise become a restart loop). Read the ticket from
  the node's log and replace the pod.

- **`transactionTimeoutMillis` is now honored on entity writes and `newMessage`.**
  All seven entity write operations (`create`, `createCollection`, `updateSingle`,
  `updateSingleWithLoopback`, `updateCollection`, `patchSingle`,
  `patchSingleWithLoopback`) plus `newMessage` accepted this query parameter and
  silently ignored it. Set, it bounds the time the server may spend before the
  first commit; exceeding it rolls back and fails with **408
  `TRANSACTION_TIMEOUT`** — nothing is committed. On a chunked write
  (`transactionWindow`), the bound only covers time-to-first-commit: once a
  chunk has committed, further expiry surfaces through the existing per-chunk
  `200` contract instead of a 408. Rejected with **400** on a request that
  joins an open transaction, where honoring it is unsafe (the joiner does not
  own the transaction). Absent, behavior is unchanged. The gRPC mirror
  (`transactionTimeoutMs` on the equivalent CloudEvent requests) honors the
  same semantics, returning a `CLIENT_ERROR` envelope prefixed
  `TRANSACTION_TIMEOUT: …`. `cyoda help errors TRANSACTION_TIMEOUT`.
  ([#379](https://github.com/Cyoda-platform/cyoda-go/issues/379))

- **`transactionSize` is now honored on `deleteEntities` and `deleteMessages`.**
  Set, matching ids/messages are deleted in independent batches of that size
  instead of one transaction or one call. `deleteEntities` re-validates each
  id's version against the batch resolved before batching began, reporting a
  version conflict or a failed batch's ids per-id in `deleteResult.idToError`
  rather than retrying; batches already committed before a later failure stay
  committed. `deleteMessages` reports one `{entityIds, success}` element per
  batch, and a failed batch does not stop later batches. Rejected with **400**
  on a request that joins an open transaction (batching per-transaction commit
  is unsatisfiable for a joiner). Absent, behavior is unchanged. The gRPC
  `EntityDeleteAllRequest.transactionSize` mirror is honored the same way when
  explicitly sent, and its response's `errorsById` is now populated with
  per-id batch failures.
  ([#379](https://github.com/Cyoda-platform/cyoda-go/issues/379))

- **`searchEntities` regains `timeoutMillis` and `408 SEARCH_TIMEOUT`,**
  completing the intent recorded when the previous, unenforced version of this
  parameter was removed in v0.8.2. Set, the search is aborted once the
  deadline elapses and the request fails **408 `SEARCH_TIMEOUT`** with no
  partial results returned; rejected with **400** on a request that joins an
  open transaction. Enforced uniformly across memory, sqlite and postgres.
  `cyoda help errors SEARCH_TIMEOUT`.
  ([#379](https://github.com/Cyoda-platform/cyoda-go/issues/379))

- **`POST /api/search/async/{entityName}/{modelVersion}` now runs on a
  bounded worker pool instead of one goroutine per submission**, with a
  retryable **503 `SEARCH_QUEUE_FULL`** once both the running workers and
  the submit queue are exhausted. Five new env vars, all validated at
  startup rather than silently clamped:
  `CYODA_SEARCH_ASYNC_WORKERS` (default `8`), `CYODA_SEARCH_ASYNC_QUEUE`
  (default `256`), `CYODA_SEARCH_ASYNC_MAX_PER_TENANT` (default `8`, tracking
  the worker count; `0` disables),
  `CYODA_SEARCH_JOB_HEARTBEAT_INTERVAL` (default `15s`),
  and `CYODA_SEARCH_JOB_STALE_AFTER` (default `5m`, must be at least 4x the
  heartbeat interval). `cyoda help config` (Search internals) and
  `cyoda help errors SEARCH_QUEUE_FULL`.

  **`CYODA_SEARCH_ASYNC_MAX_PER_TENANT` is on by default and lowers the
  accepted-in-flight ceiling — plan for it.** It caps how many jobs *one*
  tenant may have in flight (queued **and** running are counted together) on a
  node, so a single-tenant deployment's ceiling drops from `workers + queue`
  (8 + 256 = 264) to **8**: a 50-submission burst that was previously accepted
  in full now gets 8 accepted and 42 answered `503 SEARCH_QUEUE_FULL`. That is
  the point — the cap is what stops one tenant filling the shared queue and
  locking every other tenant out — but a single-tenant deployment sees only the
  cost. Raise it, or set `0` to restore first-come-first-served across tenants.
  Clients must already handle `503` here; a submitter that did not retry now
  notices.

- **A batched delete that can never finish now fails instead of running
  forever**, with a new retryable **409 `DELETE_NOT_CONVERGED`**.
  `DELETE /api/entity/{entityName}/{modelVersion}` with `transactionSize` set
  and no `pointInTime` re-selects the matching entities before every batch and
  stops when a pass finds nothing left; if entities matching the condition are
  created at least as fast as they are removed, that pass never comes up empty
  and the request previously never returned. It is now capped at a fixed
  number of selection cycles — sized to be unreachable by any converging
  delete — and fails at the cap. This is a caller-visible change: a request in
  that state used to hang, and now gets a 409 it must handle. Batches
  committed before the cap stay deleted, and the response fails rather than
  reporting the partial pass as the complete requested set. The gRPC mirror
  (`EntityDeleteAllRequest` with `transactionSize`) returns the same code in a
  `CLIENT_ERROR` envelope. `cyoda help errors DELETE_NOT_CONVERGED`.

- **Streamed async search results.** Results are saved to storage
  incrementally as the scan runs, instead of being fully materialized in
  memory and saved once at the end. A running job stamps its own liveness
  on `CYODA_SEARCH_JOB_HEARTBEAT_INTERVAL`, starting the moment it is
  submitted (including while still queued, not only while scanning); the
  same poll picks up a cross-node cancel or an externally-recorded terminal
  status.

- **Paged entity-list reads.** `GET /entity/{entityName}/{modelVersion}`
  now pages at the store instead of loading the whole model and slicing in
  Go — no O(model) materialisation for a request that only asked for one
  page. Order is stable and deterministic within a given storage engine;
  the specific order is storage-engine-specific (entity-ID based) — see
  each backend's "Canonical entity-ID order" section in `docs/plugins/`.

- **Purposed history reads.** `GET /entity/{entityId}/changes` and the
  audit-event transaction lookup now use metadata-only, purpose-built
  reads instead of the general-purpose full-history-with-payloads read
  they previously shared — bounded by one entity's own version history,
  never a model-wide scan for what only needs one field's worth of audit
  metadata.

### Changed

- **The server no longer imposes a scan budget on search. sqlite's
  residual-scan budget and its `CYODA_SQLITE_SEARCH_SCAN_LIMIT` are removed,
  along with the `SCAN_BUDGET_EXHAUSTED` error code.** A non-indexable condition
  (a regex, a wildcard path) forces a residual scan; sqlite used to meter its
  examined rows and fail the search with `400 SCAN_BUDGET_EXHAUSTED` once the
  budget was passed. Such a search now runs to completion and returns its
  matches, closing the divergence with memory and postgres, which never had a
  budget.

  This is a relaxation — requests that used to fail now succeed, and no request
  that used to succeed changes — so no caller has to act. A caller that matched
  on `SCAN_BUDGET_EXHAUSTED` can drop that arm; the code is gone from the error
  table, the help topics and the OpenAPI document.

  Bounding search *time* is the caller's, and it has the levers: `timeoutMillis`
  on direct search (`408 SEARCH_TIMEOUT`), and job cancellation on async, which
  takes effect mid-flight. Omitting them means unbounded, by choice. Bounding
  search *memory* remains the server's, and every search path streams. Operators
  who set `CYODA_SQLITE_SEARCH_SCAN_LIMIT` should remove it: it is no longer
  read, and an unknown `CYODA_*` variable is otherwise inert.

- **A postgres async search job that exceeds the backend ceiling now says what
  to do about it.** The async status response carries no error-code field, so the
  job record's message is the caller's entire report, and it read only
  `search exceeded the search statement ceiling`. It now names both ways out:

  > `search exceeded the backend's async search ceiling — narrow the query, or
  > have the operator raise or disable the ceiling (see the config.database help
  > topic)`

  Backend-neutral as before — no driver detail, no SQL, no backend name — and
  the `config.database` help topic now states that `0` disables the ceiling.
  `CYODA_POSTGRES_SEARCH_STATEMENT_TIMEOUT` (default `30m`) itself is unchanged:
  it is deliberate operator configuration, not a per-request principle guard.

- **Commits are now shielded from a client disconnect or an expired
  `transactionTimeoutMillis`/`timeoutMillis` deadline arriving mid-commit.**
  Every commit call (the final commit, each commit-before-dispatch segment
  commit, and `newMessage`'s `Save`) now runs on a deadline-shielded context
  with its own budget, so a deadline expiring while a commit is in flight can
  no longer produce an in-doubt "client sees failure but the write is durable"
  outcome. This also closes a pre-existing backend divergence on a plain
  client disconnect during commit: postgres and sqlite previously aborted the
  in-flight flush, while memory ran it to completion; all three now complete
  it. Once a commit succeeds, the response is success regardless of when a
  deadline later fires.

- **A client disconnect now aborts in-flight per-item loop work on memory and
  sqlite, matching postgres's existing behavior.** New generic cancellation
  checks in the chunk loop, collection per-item loops, conditional-delete
  per-id/per-batch loops, `newMessage`'s pre-save check, and the workflow
  cascade loop fire on any context cancellation, not only the new feature
  deadlines. Previously only postgres (via pgx) stopped in-flight work on
  disconnect; memory and sqlite ran the operation to completion regardless.
  Work already past its last commit/flush point stays durable — this only
  stops further, not-yet-committed work from starting. The same alignment
  extends to the read path: memory's and sqlite's search scan loops and
  memory's `GetAll`/`GetAllAsAt` now abort on client disconnect too, though a
  read has no durability to protect either way.

- **A bare context-cancellation error escaping a workflow evaluation is now a
  sanitized 500 instead of 400 `WORKFLOW_FAILED`.** `classifyWorkflowError`'s
  catch-all previously mapped every unclassified error, including a raw
  `context.Canceled`/`context.DeadlineExceeded`, to a 400 carrying the error's
  own text as domain detail — misattributing a server-/infra-side
  cancellation to the caller and risking leaking internal detail into a 4xx
  body. It is now routed through `common.Internal`, matching how every other
  infra-only failure in this classifier is handled.

- **The search-condition translator now lives in `cyoda-go-spi`.** `ConditionToFilter`
  and the model-schema read core behind it move out of cyoda-go into the SPI, and
  cyoda-go deletes its own copy rather than keeping one. The reason is a storage
  backend that runs its own searches: it cannot reach the shared leaf-comparison
  kernel without the translator, so it ends up shipping a second evaluator that
  answers the same query differently. Two copies of this code already existed briefly
  and drifted within days, which is why this lands as a move. No behaviour changes for
  callers of the HTTP or gRPC API. Requires a coordinated release: the SPI tags first,
  then cyoda-go's pins follow.

- **A model's parsed schema is now cached alongside its descriptor.** Evaluating a
  workflow criterion against a data field re-read the stored schema and rebuilt the
  whole field map on every evaluation — per transition, and per step of a cascade.
  Measured, that was 80–99% of the evaluation, scaling with schema size: on a
  1000-field model, 1.84 ms and 12,400 allocations per evaluation, now 12 µs and 91.
  The parsed form is held on the descriptor cache entry, so it is dropped by the same
  invalidation and lease the bytes already follow.

- **The search leaf evaluator now prepares once per query instead of once per
  candidate row.** Operand parsing, declared-type bucketing and
  `regexp.Compile` were query-invariant work that ran again for every entity a
  query considered. The worst case was the in-memory fallback: a condition on
  an array-wildcard path (`$.items[*].name`) has no pushdown representation,
  so it scanned the whole model with a fresh compile per entity. A
  prepare/execute split — `Prepare` resolves the invariant work once,
  `Match` runs per row and does none of it — removes that cost. No search or
  criterion answer changes as a result; this is throughput only.

- **A workflow processor's returned data is now governed by the model, exactly as a
  client's write is.** Previously a processor could write anything at all: content no
  backend could store (returning **500**), or fields the model does not declare —
  producing an entity the API would return but then refuse to accept back on a `PUT`,
  and that the model export did not mention. Returned data now passes the same
  storability guard and the same schema validation or extension as a client write, so
  a processor may introduce a new field exactly where the model's `changeLevel` would
  let a client do so, and not otherwise. When it may not, the transition fails with
  **400 WORKFLOW_FAILED** and rolls back, leaving neither the entity nor any schema
  change behind — rather than blaming the caller, who sent nothing invalid.

  **Integrators:** a processor that writes a field outside its model now needs that
  model's `changeLevel` set, or the field declared in the model. This applies to
  every ingress that runs a cascade, including scheduled transitions and
  peer-forwarded dispatch.
  ([#25](https://github.com/Cyoda-platform/cyoda-go/issues/25))

- **The sqlite plugin now numbers a new entity's first version 1, matching
  memory and postgres.** It previously started at 0, which was
  indistinguishable from an unset version wherever a caller checks "is
  Version populated." Only entities created from now on are affected — an
  entity that already exists on a running sqlite instance keeps its stored
  version numbers exactly as they are and its next save simply continues
  that same sequence (no renumbering or migration happens).

- **Reading an async search job's results before it finishes now answers
  with what has been saved so far**, instead of only becoming readable once
  the job reaches a terminal status internally and then being served all at
  once. Client-visible behavior is unchanged — results still only surface
  through the API once the job is `SUCCESSFUL` — but the underlying
  `GetResultIDs` contract is now explicit about mid-run reads instead of
  leaving it undefined.

- **Conditional delete (`DELETE /entity/{entityName}/{modelVersion}` with a
  condition body, and the unconditional delete-all) now streams its
  selection phase** instead of loading every matching entity into memory
  first. The batched mode (`transactionSize` set) stays O(page) per batch
  when deleting live state, but a request pinned to a `pointInTime` is
  O(matched IDs) even in batched mode — deleting a live row cannot change
  what a historical snapshot matched, so the live-state re-scan trick that
  keeps the streamed mode's memory bounded would never converge there;
  a single streamed drain (still never materializing full entity rows) is
  used instead.

- **Listing entities from inside a joined transaction (a compute-node
  callback calling back into `GET /entity/{entityName}/{modelVersion}`) now
  records only the returned page into the transaction's conflict read-set,
  not the whole model.** The commit-time first-committer-wins check
  therefore no longer aborts a transaction over a concurrent write to an
  entity of the same model that never appeared on the page it listed. This
  is a deliberate narrowing from the previous whole-model behaviour, not an
  accidental relaxation: a processor that lists a page and then saves
  within the same transaction is still protected against a conflicting
  write to anything it actually read.

- **Upgrading a populated PostgreSQL deployment to this release briefly
  blocks writers to `entities`.** Migration `000008` adds the index that
  backs paged entity-list reads with a plain `CREATE INDEX` rather than
  `CREATE INDEX CONCURRENTLY` — `CONCURRENTLY` provably deadlocks this
  project's concurrent multi-node boot path (a genuine lock cycle between
  golang-migrate's advisory lock and `CONCURRENTLY`'s own multi-phase wait,
  `SQLSTATE 40P01`). A plain `CREATE INDEX` avoids that deadlock at the
  cost of a writer-blocking window for the duration of the build. Size the
  maintenance window to the `entities` table's row count before upgrading;
  see `docs/plugins/POSTGRES.md` ("Canonical entity-ID order" /
  "Operational notes and limits") for the full mechanism and the structural
  gap this leaves for any future same-shaped migration.

### Fixed

- **A write matching a kind the model declares is accepted with a `changeLevel`
  set.** The extension gate compared one kind per path, so a model with a
  multi-kind field refused half of its own declared data at every level — with a
  message telling the client to send the declared kind, which is what the client
  had sent. Such a model was unusable with a `changeLevel` set.

- **An entity write can establish a second kind for a field, at `STRUCTURAL`.**
  Previously only a sample-data import could, so a model could describe a shape
  no write was able to record.

- **Three schema widenings that reached the client as a `500` are now
  expressible.** Each was accepted by the extension and then could not be turned
  into a delta: a field first written as `[]` and later holding object elements
  (the commonest), a field observed only as `null` and later holding an object
  or an array, and an array whose element was never observed at all. The last
  also never widened: an array observed with no content did not notice it was
  gaining an element, so the model kept declaring nothing there. That last one
  is a tightening as well as a fix — such a write used to be accepted at any
  level precisely because nothing was recorded, and it now costs
  `ARRAY_ELEMENTS` like any other element-type change. The walker never
  produces an array with no element, so only a model stored that way is
  affected.

- **Writing `null` to a field declared as a scalar no longer requires `TYPE`
  level.** A scalar declaration already admits null, so the write proposes no
  schema change and the delta it produced was empty; it was refused below `TYPE`
  as a "type change" regardless.

- **A unique key can no longer end up over a field that admits more than one
  kind.** A claim is computed by tokenizing the value at the keyed path, and
  tokenizing refuses an object or an array — so such a key could be enforced for
  only half the values the field declares, and the model would declare a kind no
  write could ever supply. The check only noticed a keyed path leaving the
  model's field list, which a path gaining a second kind does not do. A second
  sample-data import could therefore union an object onto a keyed field and be
  accepted (`200`); it now answers `422 INVALID_UNIQUE_KEY_DEFINITION` and
  registers nothing. The same rule guards the unique-key declaration and the
  schema extension an entity write performs.

- **A search against a field observed as more than one kind no longer silently
  returns fewer rows on a backend that executes searches itself.** That
  executor's own schema decoder dispatched on a single kind label and dropped
  the other branches a union's payload carried, so a predicate on a dropped path
  found no declared type and matched nothing — no error, just a narrower answer.
  The node, its decoder and the field walk have one implementation now, shared
  by the engine and every executor.

- **A JSON array posted to the sample-data import is read as a collection of
  sample documents.** It previously returned `200` and registered a model
  describing an array at the *root*: `SIMPLE_VIEW` rendered it as `{}`, which
  reads as an empty model, and the model then refused the very documents it was
  derived from. The entity ingress already reads an array body as "a collection
  of items of the same type", so an array of documents now derives their merge
  — the same result successive imports onto an `UNLOCKED` model produce. A body
  that is neither a document nor a collection of them (a scalar root, a
  non-object element) is refused with `400 VALIDATION_FAILED`, naming the
  offending element, and leaves no model behind.

- **The model export describes every branch a field declares.** Both exporters
  described a node by its dominant kind, which dropped two things. An array of
  arrays rendered as `.m[*]: NULL` — the type of the intermediate array, which
  has none — instead of naming the elements at `.m[*][*]`, the same `jsonPath`
  a search uses to address them. And a field observed as both a scalar and a
  container showed only the container branch, so two models that enforce
  differently rendered identically and an operator inspecting the export was
  told something false. `SIMPLE_VIEW` now spells one `[*]` hop per array level
  and names every branch a field declares — including the elements' own branches
  and an array whose elements were never observed (`.a[*]: NULL`, previously
  omitted entirely). `JSON_SCHEMA` renders a kind union as an `anyOf` over its
  branches: it used `oneOf`, which requires exactly one branch to match and so
  rejected values the model admits whenever two branches rendered the same JSON
  Schema shape (`Integer` and `Long` both render `{"type":"integer"}`).

- **A stored model no longer loses the array branch of an object-and-array
  union.** `Merge` records a field observed as both an object and an array as a
  single node that keeps its element, but the schema codec restored only the
  children of such a node — so the array branch vanished on the first read back
  and the model refused a document it had just been derived from. Every branch
  the wire form carries is now restored, independently of the node's kind.

- **A search now finds the declared types of every branch of a polymorphic
  field.** The walk backing the fields map emitted the scalar branch of an
  object-or-scalar union but not of an array-or-scalar one, and never followed
  the array branch of an object-and-array union. Both unions admit values of
  both kinds on a write, so a predicate on the missing branch found no declared
  type: per the filter contract that does not degrade operators uniformly — the
  comparison and ordering operators collapse to a non-match while the string
  and presence operators keep evaluating — and the field looked as though it
  simply held no matching data.

- **A gRPC `orderBy` path is now held to the same grammar as an HTTP `sort`
  key, instead of being taken at face value.** The HTTP parser refuses a path
  that is not a dotted scalar; gRPC built its sort key from the client's path
  verbatim and relied on the path being present in the model schema. That is
  not the same check: a scalar leaf inside an array of objects is recorded
  under the wildcard key (`$.items[*].name`) and is *not* flagged as an array,
  so schema lookup and the array guard both admitted it.

  Where such a key ended up depended on which branch served the request. The
  pushdown branch was refused by each plugin's own path validator — a `400`,
  with a warning that the boundary and the plugin disagreed. The in-memory
  branch had no such backstop: the path went to the evaluator, which has no
  bracket syntax, so every entity missed, all compared equal, and the caller
  received `200` with results that were simply not sorted. A request the engine
  could not honour was answered rather than refused.

  Both gRPC search doors — direct and async submit — now answer `400
  INVALID_FIELD_PATH` for an array projection, a positional subscript, or any
  character outside the segment charset, refusing at submit rather than
  failing a job later. HTTP behaviour is unchanged.

- **An async search job now records a storage backend's rejection of the
  request as the client error it is.** The synchronous path classifies the
  cross-backend sentinels — a refused filter path, an exceeded result limit —
  into a `400`; the async executor assigned the store's error straight
  through, so the persisted job record read `search failed unexpectedly` for
  input that was simply malformed, while the same cause on the synchronous
  door read as a client error. The same classification now runs on both, for
  the iterate error and for a sticky scan error surfaced at `Close`. Note the
  record is not served to callers today — no status surface carries a failure
  message — so this is an operator-facing and forward-looking fix, not a
  change to any response.

- **The grouped-statistics endpoint's error codes are now documented.** All ten
  codes the endpoint raises — `MALFORMED_REQUEST`, `MISSING_GROUP_BY`,
  `DUPLICATE_GROUP_BY`, `INVALID_GROUP_BY_PATH`, `INVALID_AGGREGATION_OP`,
  `INVALID_AGGREGATION_FIELD`, `DUPLICATE_AGGREGATION_ALIAS`, `INVALID_LIMIT`,
  `GROUP_CARDINALITY_EXCEEDED` and `NOT_IMPLEMENTED_BY_BACKEND` — were inline
  string literals with no constant and no help topic, so `cyoda help errors
  <CODE>` answered 404 for every one of them and the error-code parity test
  could not see them. Each now has a constant and a topic.

  Two codes are removed from the `crud` topic's grouped-stats table:
  `INVALID_POINT_IN_TIME` and `INVALID_OPERATOR`. Neither is emitted anywhere
  in the server — an unparseable `pointInTime` is part of strict body decoding
  and answers `MALFORMED_REQUEST`, which the table now says. `api/openapi.yaml`
  was also wrong on one point — an out-of-range `limit` is `INVALID_LIMIT`, not
  `MALFORMED_REQUEST`.

- **A path addressing one array element by position (`$.arr[0]`) now resolves,
  instead of answering an empty page for a field that holds the value.** It is
  valid JSON Path and is accepted at the API boundary. The in-memory evaluator
  is the resolver of last resort — every leaf a backend does not push down
  still falls through to it — and it did not resolve this one. Three lookups
  missed, each independently enough to make the leaf false for every entity:
  the evaluator handed gjson a path it has no syntax for (`arr[0]`, where
  gjson wants `arr.0`); the declared-type lookup
  probed a schema key that cannot exist (`$.arr[0]` — the schema records an
  array's element once, under `$.arr[*]`), and a comparison with no declared
  type expands into nothing; and search's field-existence check rejected the
  path **400** as naming a field the model does not declare. The wildcard
  spelling of the same path worked throughout, so two spellings of one path
  disagreed. Affects a search `condition`, a conditional delete, a grouped-stats
  residual, and a workflow criterion alike.

  One consequence is a new rejection: a positional path now type-checks like
  its wildcard twin, so `{"jsonPath":"$.arr[0]","operatorType":"EQUALS","value":"abc"}`
  against an integer array is **400 `CONDITION_TYPE_MISMATCH`** rather than an
  empty page. Negative indices, slices, unions and filter expressions are
  unchanged — no evaluator in the stack resolves them.

- **The gRPC changes-metadata read no longer reports a `transactionId` for a
  deleted entity's tombstone.** `transactionId` is present only when
  `hasEntity` is true; the HTTP handler gates on it, the gRPC handler did
  not, so `EntityChangesMetadataGetRequest` surfaced the delete
  transaction's id over gRPC while `GET /entity/{id}/changes` omitted it for
  the same entity. The two doors now agree, matching the documented
  contract.

- **Cancelling an async search job no longer leaves it permanently un-reapable.**
  `CancelAsync` called the generic status-update path instead of the store's
  `Cancel`, which never stamped a finish time on the job — and the background
  reaper only ever removes terminal jobs that have one, so every cancelled job
  accumulated in storage for the life of the process. `CancelAsync` now
  dispatches through `Cancel`, and all three storage backends (memory, SQLite,
  PostgreSQL) stamp the finish time as part of the same transition that marks
  the job `CANCELLED`, so it is reaped on the same schedule as a completed or
  failed job.

- **`POST /api/oauth/oidc/providers/reload` no longer destroys the JWKS key cache
  it is documented to refresh.** The reload rebuilt the provider list but installed
  empty key sources and never re-warmed them, so every federated token failed with
  **401** `unknown kid` until a process restart — including tokens of providers that
  were healthy before the call. The reload now carries surviving key sources across
  the rebuild and force-warms every loaded provider (on the receiving node and, in a
  cluster, on every broadcast peer); invalidated providers are excluded — their
  endpoints are explicitly distrusted. A provider whose discovery fetch fails
  during the refresh keeps its previously cached keys, with freshness still
  governed by the standard JWKS cache TTL (fail closed).

- **A provider whose IdP was unreachable during the startup JWKS warm-up no longer
  stays keyless for the life of the process.** The warm-up was one-shot — if cyoda
  booted ahead of the IdP, every federated token failed with **401** until a restart
  that won the race. Failed warm-ups are now retried every 30 seconds until the IdP
  becomes reachable, and the recovery is logged.

- **Resolving a transaction's submit time is now tenant-gated.** `GetSubmitTime`
  was the only transaction-lifecycle method without the tenant check the rest of
  the surface enforces: a caller supplying another tenant's transaction ID —
  reachable via `GET /entity/{id}/transitions?transactionId=` — could learn the
  transaction's submit time (committed) or its in-flight state. All three storage
  backends now reject cross-tenant lookups before any state-dependent response,
  the endpoint answers the same **400** for a foreign transaction ID as for a
  nonexistent one, and the SQLite `submit_times` table gains a `tenant_id` column
  (migration 000005, drop-and-recreate — rows carry a 1-hour TTL) so the
  persistent-fallback lookup is gated too.

- **Sorting by a `$.`-prefixed field path over gRPC now works.** Sort-key resolution
  prepended `$.` by hand, which is not idempotent. The HTTP layer strips the prefix
  before resolving, so HTTP was unaffected; gRPC passes the client's path through
  verbatim, so `orderBy` on `$.city` was looked up as `$.$.city` and returned **400
  `INVALID_FIELD_PATH`** for a field that exists. The two transports now answer the same
  request identically.

- **A field path written without the `$.` prefix now resolves its declared type.**
  `city` and `$.city` are both accepted and both pass field-path validation, but only
  the prefixed form resolved against the model schema. The unprefixed form came back
  with no declared type, and a type-directed comparison with no declared type matches
  nothing — so `city EQUALS "Berlin"` returned **200 with an empty page** on a model
  whose `$.city` holds `Berlin`. In a workflow criterion the same defect made the leaf
  evaluate false for **every** entity, so the transition silently never fired.
  Declared-temporal fields reached this way were also compared as text rather than as
  timestamps, and the type-soundness check skipped such a leaf entirely, so an operand
  that should have been rejected `400 CONDITION_TYPE_MISMATCH` was accepted and
  answered with an empty page. A genuinely unknown path still resolves to no declared
  type, which is the intended degrade-to-no-match.

- **PostgreSQL's in-Go residual filter no longer sees the internal `_meta` block.**
  Postgres stores an entity as one document with the domain data and a storage-level
  `_meta` block side by side, and the Go-side evaluator was handed the un-stripped
  document, so a condition naming a data path under `_meta` matched there and on no
  other backend. It now receives the same domain data every other backend passes.
  Entity state, creation date and the other metadata remain searchable the supported
  way, which is unaffected. **This does not yet close the surface**: for `IS_NULL`
  and `NOT_NULL` the query is answered entirely in SQL with no Go re-check, and the
  SQL still resolves a data path against the merged document. That remainder is
  tracked separately.

- **SQLite treats a zero-value filter as "match all", like the other backends.** It was
  installed as a residual post-filter instead, which disabled `LIMIT` pushdown and native
  `GROUP BY`. No cyoda-go request reaches this — every route
  spells "match everything" as an empty `AND`, which already worked — so this is storage
  contract conformance rather than a user-visible fix, and it matters to anything driving
  the storage interface directly.

- **A model-store outage evaluating a workflow criterion no longer gets masked
  by a structural error on a sibling conjunct.** `evaluateCriterion` checked
  the match error first and the model-load error second, so a malformed
  operator on one conjunct of e.g. `OR[$.age > 5, $.x IS_CHANGED]` reported
  `400` for what was actually a server-side outage, and the load error was
  then discarded unlogged. The infrastructure failure is now checked first
  and wins — failing closed on an unavailable dependency a correct result
  requires, rather than reporting it as a client error.

- **An entity write now releases its transaction on every exit path, including a
  panic.** Previously a panic between begin and commit left the transaction neither
  committed nor rolled back, with its pooled connection never returned; repeated, that
  exhausts the pool and the node stops serving.

- **The workflow engine releases the segments it opens itself.** A `FUNCTION` criterion
  callout failing mid-cascade left the post-segment transaction open with no panic
  involved — an ordinary compute-node failure was enough. On memory and sqlite, which
  have no database-side ceiling underneath, that leak was permanent.

- **A criterion evaluated after a `COMMIT_BEFORE_DISPATCH` segment now receives that
  segment's transaction id** rather than the already-committed cascade-entry id, so a
  compute node's callback can join it instead of being told the transaction is gone.

- **A collection update whose engine conflicted past a committed segment now aborts the
  batch.** It was treating that conflict as a per-item If-Match failure, isolating the
  item and writing every later item into an already-committed transaction — losing them
  with a 200 response.

- **`statement_timeout` (SQLSTATE `57014`) and `idle_in_transaction_session_timeout`
  (SQLSTATE `25P03`) are classified** rather than surfacing as unexplained errors, and a
  `25P03` abort releases the per-transaction bookkeeping the killed session left behind.
  A statement cancelled by the ceiling is a `500` with a ticket, not a retryable `503` —
  re-running it would exceed the same ceiling again — and the log names the setting that
  fired.

- **A storage outage no longer answers `404 Not Found`.** Async-search status and
  results, trusted-key delete/invalidate/reactivate, the audit transaction lookup, and
  the entity read behind `DELETE /entity/{entityId}`, the single and collection
  updates and `GET /entity/{entityId}/transitions` all collapsed any store error into
  a not-found result, so a database outage reported "it does not exist" — a
  substituted answer that stops a client retrying. They now return
  **503 `STORAGE_UNAVAILABLE`**, retryable; an entity that genuinely is not there
  still returns `404` with the code and detail it always had.

- **The async-search results endpoint no longer interpolates a raw driver error into a
  `400` response body**, where it could carry connection detail. A job that is still
  running returns `400` naming its status; every other failure is classified.

- **An async-search result page is no longer silently short when the store fails.**
  An entity that could not be read while building the page was logged and skipped, so
  a storage blip returned `200` with fewer results than `total` claimed and nothing to
  distinguish it from a job that really matched that many. Only a result id whose
  entity has genuinely been hard-deleted since the scan recorded it is still skipped;
  any other read failure fails the page.

- **On a model with several imported workflows, every operation after creation ran the
  wrong workflow's definition.** A named transition, a loopback re-evaluation and a
  scheduled transition firing all resolved the workflow by "the first active definition
  that declares the entity's current state", ignoring the entity's selection criterion.
  Where definitions share state names — the normal shape for a per-kind machine — that
  is always the *first* declared workflow, for every entity: the wrong guards,
  processors and target states, silently and fail-open. Entities admitted past guards
  belonging to another kind, and a transition declared on one kind only was reported as
  absent (**400 TRANSITION_NOT_FOUND**) for every entity. Selection at creation was correct, which is why the binding looked
  right in the creation audit. All four doors now resolve through the documented
  criterion rules on every call, and the `WORKFLOW_SKIP` / `WORKFLOW_FOUND` audit
  events — previously emitted only on creation — record which definition ran on each
  of them.

  **Integrators:** because selection is re-evaluated per call, an entity whose payload
  changes can re-bind to a different definition. If its current state is not declared
  there, the engine no longer falls through to a definition that happens to declare it:
  the transition is rejected with **400 WORKFLOW_FAILED** and a loopback settles as a
  no-op. A scheduled task the newly selected workflow no longer declares is not
  cancelled by that write — it is discarded when it next comes due, recorded as
  `SCHEDULED_TRANSITION_CANCEL`. Prefer selection criteria that stay true for an
  entity's whole lifetime, and that read fields a caller cannot rewrite in the same
  request: the criterion is evaluated against the payload of the request being served,
  so where definitions differ in what they permit, the selection field is a security
  control.
  ([#465](https://github.com/Cyoda-platform/cyoda-go/issues/465))

- **`GET /entity/{entityId}/transitions` no longer answers from the wrong workflow when
  a selection criterion cannot be evaluated, and no longer writes to the audit trail.**
  A criterion that failed to evaluate — a `function` criterion with no compute member
  for its tags, for instance — was swallowed and the *default* workflow's transitions
  were returned instead: a wrong-but-available answer. It now fails the request. The
  same read was also recording `WORKFLOW_SKIP` / `WORKFLOW_FOUND` events against an
  empty transaction id, despite intending not to; it now records nothing. A criterion
  that merely does not *match* still resolves to the default workflow, which is
  selection working as documented.
  ([#465](https://github.com/Cyoda-platform/cyoda-go/issues/465))

- **A payload that repeats a name within one object is now rejected with 400.**
  A duplicated name was read as the *last* occurrence by schema validation, the `GET`
  response and unique-key computation, and as the *first* by the workflow criterion
  evaluator, search and grouped statistics — on the same bytes in the same request.
  An entity created with `{"amount":"not-a-number","amount":5}` was reported by the API
  as `amount=5` while a criterion `amount == 5` did not fire, leaving it in the wrong
  workflow state with nothing logged. All three backends were affected, since the
  criterion runs against the request bytes before any store normalisation. Names
  repeated in sibling objects, across array elements or at different depths are
  ordinary JSON and remain accepted. RFC 8259 permits rejecting duplicate names.
  ([#25](https://github.com/Cyoda-platform/cyoda-go/issues/25))

- **A number outside PostgreSQL's `numeric` range is now rejected with 400 instead of
  failing inside the store.** Beyond 131072 digits before the decimal point or 16383
  after, the write returned **500 SERVER_ERROR** on postgres while memory and sqlite
  accepted it. Only reachable on a field that inferred an unbounded numeric type. The
  check is on the *effective* weight and scale, so `1.5e-16383` is rejected despite
  having one fraction digit and an in-range exponent, while `0.0001e131075` is accepted
  because leading zeros are not significant. It is purely lexical — deciding that
  `1e1000000` is too large never builds a million-digit value.
  ([#25](https://github.com/Cyoda-platform/cyoda-go/issues/25))

- **A processor returning `{"data":null}` no longer panics and leaks a database
  connection.** The literal `null` is non-empty, so it passed the empty-payload check
  and then unmarshalled into a *nil map*, and assigning into a nil map panics. The
  panic was recovered only by the HTTP middleware several packages up, unwinding past
  the entity service's non-deferred rollback — so the transaction was neither committed
  nor rolled back and its pooled connection was never returned. Repeated, that exhausts
  the pool and the node stops serving. The plugin now returns a clean error, so the
  normal error path runs and the transaction is released.
  ([#25](https://github.com/Cyoda-platform/cyoda-go/issues/25))

- **An empty entity payload no longer makes the entity — and its whole model's
  listing — permanently unreadable on PostgreSQL.** `{}` was accepted with 200 and
  then failed every subsequent read with **500 SERVER_ERROR**: not only `GET` of that
  entity, but `GET /entity/{model}/{version}` for the entire model, because one
  unreadable row failed the whole listing. Updating a healthy, readable entity to `{}`
  bricked it the same way. The plugin merges its `_meta` block into the domain data,
  so `{}` was stored as `{"_meta":…}`; on read `_meta` was removed and nothing
  remained, leaving no data to decode. An empty payload now round-trips as `{}`, while
  a DELETED version — which legitimately carries no domain data — still reports none.
  The memory and sqlite stores were unaffected, so this was also a backend divergence.
  ([#25](https://github.com/Cyoda-platform/cyoda-go/issues/25))

- **Unpaired UTF-16 surrogates and invalid UTF-8 in an HTTP entity payload are now
  rejected with 400 on every backend.** Both are accepted by Go's JSON parser and
  rejected by PostgreSQL text/jsonb, so they reached the store and came back as
  **500 SERVER_ERROR** with a support ticket, while memory and sqlite accepted them —
  the same divergence as the NUL case below. The guard reads the raw request bytes
  rather than the decoded value, which is load-bearing: Go's decoder silently rewrites
  both forms to U+FFFD, so validating the decoded value cannot see them, and
  re-serialising it would store a replacement character the client never sent.
  Correctly paired surrogates, literal emoji and a client-sent U+FFFD remain valid
  payload content. The gRPC entity API now carries the client's payload bytes
  verbatim to the same guard: it previously decoded and re-marshalled the payload
  before validation, which rewrote both forms to U+FFFD — storing a character the
  client never sent — and collapsed duplicate keys instead of rejecting them. All
  five gRPC entity write events (create, update, patch, create-collection,
  update-collection) now enforce the full guard set, matching HTTP.
  ([#25](https://github.com/Cyoda-platform/cyoda-go/issues/25),
  [#468](https://github.com/Cyoda-platform/cyoda-go/issues/468))

- **An entity payload containing a NUL (U+0000) is now rejected with 400 on every
  backend.** `{"name":"a\u0000b"}` is valid JSON and passes schema validation, but
  PostgreSQL's text and jsonb types cannot represent U+0000 — so the write reached
  the store and failed there, returning **500 SERVER_ERROR** with a support ticket
  for what is a client input error. The memory and sqlite stores accepted the same
  payload, making the set of storable values depend on the backend. All entity write
  paths (create, batch-array create, collection create, update, collection update)
  now reject it at the boundary with 400 `BAD_REQUEST`, naming the offending field
  path. (NUL survives the gRPC ingress's decode, so it is caught there too.) Covered by a cross-backend parity scenario.
  ([#25](https://github.com/Cyoda-platform/cyoda-go/issues/25))

- **A request body with trailing content after a valid JSON value now returns
  400, not 500.** `POST /entity/{format}/{name}/{version}` accepted a body such as
  `{"x":1}}}`: the decoder stops at the end of the first JSON value and ignores
  whatever follows, so the request passed validation while the *original* — still
  malformed — bytes went on to be persisted, surfacing the client's input error as
  a storage failure with a support ticket. Entity payload decoding now requires the
  body to hold exactly one JSON value, matching `json.Unmarshal`.
  ([#25](https://github.com/Cyoda-platform/cyoda-go/issues/25))

- **Point-in-time tests no longer compare two clocks.** `TestParity/GetAllEntitiesAsAt`
  flaked on postgres: it built its `pointInTime` from the test process's clock and
  compared it against version times stamped by the *database* — on a testcontainer
  the Docker VM's clock, measured lagging the host by 10–13 ms under load, more than
  the 10 ms sleep it relied on. Every affected test now takes its boundary from the
  backend's own clock; sleeps remain only to separate consecutive versions. Covers the
  parity suite, `internal/e2e`, the postgres plugin's own as-at tests (which had 2 ms,
  10 ms and zero-margin variants), and the SPI conformance harness, whose `Harness.Now`
  now reads the database clock.
  ([#460](https://github.com/Cyoda-platform/cyoda-go/issues/460))

- **`GET /entity/{id}/transitions` no longer 404s an existing entity when the
  database clock runs ahead of the application.** With no `pointInTime` supplied, the
  handler defaulted to `time.Now()` and issued a *historical* read — comparing the
  application's clock against database-stamped version times. When the database ran
  ahead, a just-written version compared as not-yet-valid and the entity read as
  missing, so a request for the current state got **404 ENTITY_NOT_FOUND** for an
  entity that exists. A request with no point in time now reads the current version.
  Same fix in `GetAvailableTransitions`, behind `/platform-api/entity/fetch/transitions`.
  ([#460](https://github.com/Cyoda-platform/cyoda-go/issues/460))

- **Scheduled-transition e2e tests no longer race an HTTP round-trip against the
  timer.** `TestE2E_ScheduledTransition_FiresThroughHTTPStack` and `_LoopbackDefersTimer`
  asserted "has not fired yet" by reading the state back and expecting the old value,
  which under load loses to the delay and reports a defect that does not exist. Both now
  assert from the server's audit trail: the transition was *armed* with its delay applied,
  and the fire did not precede the armed time. `getSMAuditEvents` gained an explicit page
  size — the default 20-item page silently truncated the history one of them reasons about.
  ([#460](https://github.com/Cyoda-platform/cyoda-go/issues/460))

- **A late compute-node reply no longer leaves a dangling gRPC dispatch
  entry.** `internal/grpc/dispatch.go` removed a pending request from its
  tracking map only via `CompleteRequest`/`FailAllPending`, so the
  `ctx.Done()` timeout arm, the `time.After` timeout arm, and a `Send`
  failure all abandoned their request without cleaning up its map entry — a
  bounded per-request leak, hottest on a reachable write deadline. All three
  paths now run through one deferred cleanup.
  ([#379](https://github.com/Cyoda-platform/cyoda-go/issues/379))

- **SQLite's message batch-delete no longer fails outright on a large id
  list.** `MessageStore.DeleteBatch` built one `IN (?,…)` clause for the
  whole list and broke on SQLite's bound-variable limit (32766 in the
  `ncruces/go-sqlite3` driver's wasm build). It now chunks the `IN` list at a
  size well under that limit; message delete was already documented as
  non-transactional, so the chunking is not user-visible beyond no longer
  failing.
  ([#379](https://github.com/Cyoda-platform/cyoda-go/issues/379))

- **An async search job whose owning node crashed no longer stays `RUNNING`
  forever with stale partial results.** Nothing previously reclaimed an
  orphaned job — the reaper only ever removed *terminal* jobs past their
  TTL. A background reaper now claims any `RUNNING` job whose heartbeat has
  gone silent for `CYODA_SEARCH_JOB_STALE_AFTER` and marks it `FAILED` with
  a safe generic message. This milestone's disposition fails the job
  outright rather than re-executing it elsewhere in the cluster (a
  re-execution follow-up is tracked separately); the important behavior
  change is that a crashed node's async jobs now reach a terminal state at
  all.

- **An async search job's results are no longer subject to a torn write
  from two nodes believing they both own it.** Every job now carries a
  claim epoch: `Heartbeat`, streamed result saves, and the terminal status
  write are all fenced against it, so an executor that was reaped and later
  recovers has its next write rejected instead of silently corrupting a
  result set another node has since taken over.

- **`GET /entity/{entityId}/changes` no longer returns an unstable order
  for two versions sharing the same timestamp.** The sort was
  timestamp-only; entries with an identical timestamp could reorder between
  otherwise-identical requests. Newest-first is now tie-broken by version
  number descending, which is stable.

- **A tombstone's `hasEntity` in the changes/audit response is now
  consistent across storage backends.** It was derived from "is the
  returned entity payload non-nil," which some backends left non-nil on a
  DELETED row and others did not; `hasEntity` is now the canonical,
  change-type-derived `Deleted` flag, so a delete's history entry reports
  the same `hasEntity` value regardless of which backend served it.

- **A repeated unknown sort field now costs one authoritative schema read,
  not one per request.** `resolveSortKeys` refreshes a `DATA` sort key
  absent from the cached schema exactly once before refusing it (mirroring
  the condition-path bound issue #77 established), but it never consulted
  the field-path negative cache that bound already applies to, so a
  serially repeated bogus sort key paid a full `RefreshAndGet` — an
  authoritative model-store read plus a full schema re-parse, which also
  repopulates the shared model-descriptor cache — on every single request.
  It now routes through the same `PathValidationCache` a condition path
  uses, bounding it per `(tenant, model, path)`.

- **Translating a condition tree no longer re-desugars every subtree at
  every level.** `spi.ConditionToFilter` desugars the whole tree once, but
  `groupToFilter` recursed back through `ConditionToFilter` for each child,
  re-running the desugar pass on that child's already-desugared subtree —
  O(n·depth) instead of O(n), measured at ~36× for 500 leaves at depth 250.
  `internal/match.Prepare`'s `prepareGroup` had the identical defect. Both
  now recurse into a desugar-free dispatch instead.

- **Three permissive defaults on an unreachable parse error are now
  fail-closed.** `rejectSubscript` (group-by/aggregate-field/sort-path
  subscript rejection) and `pathHasWildcard` (pushdown-safety wildcard
  detection), in each of `plugins/memory`, `plugins/sqlite` and
  `plugins/postgres`, defaulted to the permissive outcome — accept, or
  "not a wildcard" (pushable) — when `spi.ParseFilterPath` failed on their
  input. Every call site validates the path first, so this was unreachable
  in practice, but the default direction violated
  `.claude/rules/correctness-over-availability.md`: a dependency (a
  successful parse) a correct "no subscript" / "not pushable" answer
  requires now fails the check instead of being treated as satisfying it.

## [0.8.3] — 2026-07-27

### Added

- **Scheduled state transitions now fire automatically.** A transition
  carrying `schedule: {delayMs, timeoutMs}` fires `delayMs` after the entity
  enters its source state, via a durable per-backend `ScheduledTask` store
  (memory/sqlite/postgres) armed and cancelled atomically with the entity
  write. A cluster coordinator (lowest-live-node-ID, pluggable) scans due
  tasks and distributes them (round-robin, pluggable) to a fire-and-forget
  peer RPC; the firing node re-reads the task and entity as a guard before
  acting, so a stale or superseded task is a silent no-op. The transition's
  criterion is evaluated **once** at fire time — `false` declines the
  transition (the entity stays put, not retried); a lateness grace band
  separates expiry (`timeoutMs` exceeded, dropped unfired) from firing so
  clock skew across nodes cannot produce a contradictory expire-and-fire.
  Any entity write that leaves the entity in the same state (including a
  routine data update or self-loop) resets the timer. Explicitly firing a
  scheduled transition by name still returns `400 TRANSITION_NOT_FOUND`
  (reworded: "is scheduled and fires automatically; it is not manually
  fireable"). New audit events: `SCHEDULED_TRANSITION_ARM`/`FIRE`/`EXPIRE`/`CANCEL`.
  New env vars: `CYODA_SCHEDULER_ENABLED`, `CYODA_SCHEDULER_SCAN_INTERVAL`,
  `CYODA_SCHEDULER_BATCH_SIZE`, `CYODA_SCHEDULER_DISTRIBUTION`,
  `CYODA_SCHEDULER_COORDINATOR`, `CYODA_SCHEDULER_REDISPATCH_BACKOFF`,
  `CYODA_SCHEDULER_EXPIRY_GRACE`.
  ([#251](https://github.com/Cyoda-platform/cyoda-go/issues/251))

  **Per-entity firing time via a `schedule.function` compute-node callout.**
  A scheduled transition's `schedule` may now carry `function` instead of a
  static `delayMs` — mutually exclusive with it — naming a compute node
  (routed by `calculationNodesTags`, same conventions as an externalized
  processor/criterion) that computes the firing time per entity at arm time.
  The callout returns a `resultKind: "Schedule"` result shaped
  `{fireAt|fireAfterMs, expireAt?|expireAfterMs?}` (absolute epoch-ms or
  relative), resolved into the same `scheduledTime`/`timeoutMs` a static
  `delayMs` schedule would populate. A resolved expiry at or before the
  resolved fire time is **born expired**: the transition is never armed, any
  prior scheduling for it is cancelled, and a `SCHEDULED_TRANSITION_EXPIRE`
  audit event is recorded directly — the entity write still succeeds. A
  malformed or wrong-kind result rejects the entity write with the new
  `500 SCHEDULE_FUNCTION_INVALID_RESULT`; the compute node being unreachable,
  disconnected, or timing out surfaces the retryable `503`s below instead —
  fail-closed either way, never a silent skip. Additive workflow schema
  change: the workflow schema moves to **1.3** and every existing 1.1/1.2
  payload remains valid (dual-shape). New error code:
  `SCHEDULE_FUNCTION_INVALID_RESULT` (500). Contract note: because a
  `function` schedule carries no `delayMs`, `TransitionScheduleDto.delayMs`
  is no longer a `required` property — consumers of the workflow-**export**
  response (`GET …/workflow/export`) must treat `delayMs` as optional on a
  1.3+ schedule (a pre-1.3 exported workflow always has it). This is an
  intentional, schema-version-gated contract change, allowlisted in the
  oasdiff breaking-change gate.
  ([#419](https://github.com/Cyoda-platform/cyoda-go/issues/419))

  **Uniform compute-infra `503`s across processor/criterion/function
  callouts.** `NO_COMPUTE_MEMBER_FOR_TAG`, `DISPATCH_TIMEOUT`,
  `DISPATCH_FORWARD_FAILED`, and `COMPUTE_MEMBER_DISCONNECTED` now surface
  as retryable `503` uniformly regardless of which of the three callout
  kinds triggered the dispatch — previously some of these paths
  (e.g. no matching compute member) fell through to a misleading
  `400 WORKFLOW_FAILED`. No new error codes.
  ([#251](https://github.com/Cyoda-platform/cyoda-go/issues/251))

- **Follow-on workflow actions are attributed to the originating principal.**
  A cascaded write (a processor reacting to a user's transition and writing
  other entities) and a scheduled fire were previously recorded as the compute
  service account and a fake `"scheduler"` user respectively, losing the human
  who caused them. Attribution and authorization are now separate: a follow-on
  is **executed** with system/service authority but **attributed** to the
  principal captured server-side when it was created — the transaction's origin
  for a cascade (propagated through every joined write, including a cross-node
  proxied join), the durable `ArmedBy` on the scheduled task for a timer fire.
  Origin is platform-set only; no request field or worker input can change it.
  Principals now carry an explicit kind (`user`/`service`/`system`) instead of
  being sniffed from `ROLE_M2M`. `GET /entity/{entityId}/changes` gains
  `attributedKind` and `executedBy: {id, kind}` per change; `user` is unchanged
  for existing consumers, and rows written before this change omit the two new
  fields entirely rather than emitting `null`. New env var
  `CYODA_IAM_MOCK_KIND` (default `user`) sets the principal kind in mock mode.
  New compute-node SDK helper `api/grpc/authctx` exposes `Type`/`ID`/`Roles`
  plus `Require(ce, role)`, a fail-closed role gate. See the `authtype` wire
  change under **Changed**.
  ([#430](https://github.com/Cyoda-platform/cyoda-go/issues/430))

- **Bounded-search failures now surface as `400`, not `500`** — a storage backend
  (`Searcher` or grouped-stats aggregator) that detects a matched result set
  exceeding its configured cap now returns `400 SEARCH_RESULT_LIMIT`; one whose
  non-indexable residual scan exceeds the backend's row budget now returns
  `400 SCAN_BUDGET_EXHAUSTED` — both previously surfaced as an opaque `500`
  ticket on every transport. sqlite raises `SCAN_BUDGET_EXHAUSTED` on direct
  search today; the commercial backend's index-driven searcher is expected to
  raise `SEARCH_RESULT_LIMIT`. New error code: `SCAN_BUDGET_EXHAUSTED` (400).
  ([#433](https://github.com/Cyoda-platform/cyoda-go/issues/433))

- **Tx-aware search pushdown + `trackingRead`** — an in-transaction search no
  longer falls back to a full `GetAll` scan plus in-memory filtering; the
  plugin-level `Searcher` now honours the active transaction directly,
  read-your-own-writes correct against the transaction's own uncommitted
  writes, with no full-model materialisation (memory/sqlite overlay the
  transaction buffer on the committed stream; postgres runs the query
  natively on its own `pgx.Tx`). A new optional `trackingRead` boolean
  (default `false`) on the synchronous search endpoints (HTTP
  `POST /api/search/direct/{entityName}/{modelVersion}` and the gRPC
  `Search` RPC) opts a search into recording its **returned** entities into
  the transaction's read-set, so a concurrent write to one of them aborts
  the transaction at commit — entity-level, same as `GetAll`, still no
  phantom-write-skew protection. Default behaviour (`trackingRead=false`)
  is a plain snapshot read that records nothing, a lighter-weight read-set
  footprint than before. See `docs/CONSISTENCY.md` §3c for the updated fence
  guidance. Async search does not expose the flag (it runs detached, outside
  any transaction). No new error codes.
  ([#420](https://github.com/Cyoda-platform/cyoda-go/issues/420))

- **Criterion stoppage reason** — a criteria compute node's `EntityCriteriaCalculationResponse`
  may now carry an optional `reason` string on `matches: false`, explaining why a passage was
  blocked. A manual explicit transition rejected by its criterion appends the reason to the
  `400 WORKFLOW_FAILED` detail (`transition "<name>" criterion not matched: <reason>`) — the
  guaranteed, backend-independent surface. For the automated-cascade and workflow-selection
  paths, the reason is additionally recorded durably on the state-machine audit trail:
  `TRANSITION_NOT_MATCH_CRITERION`'s `data` carries `{workflowName, transition, criterion, reason}`
  and `WORKFLOW_SKIP`'s carries `{workflowName, reason}`. A manual rejection's own audit event is
  not forced durable (it rolls back with the no-op transaction, same as before). Reason is capped
  at 2 KiB; an omitted reason defaults to `"criterion did not match"`. No new error codes.
  ([#413](https://github.com/Cyoda-platform/cyoda-go/issues/413))

- **Polymorphic temporal type detection** — the schema classifier now recognises
  `LOCAL_DATE`, `LOCAL_DATE_TIME`, `LOCAL_TIME`, `ZONED_DATE_TIME`, `YEAR`, and
  `YEAR_MONTH` instead of classifying every temporal string as `STRING`, and the
  simple-view exporter reports them. Beyond classification fidelity this is what
  lets a backend build range indexes on temporal fields. Ships as part of the
  shared type-directed kernel (see **Changed**).
  ([#137](https://github.com/Cyoda-platform/cyoda-go/issues/137))

- **Storage plugins can contribute `cyoda help` topics.** `help.RegisterOverlay(fs.FS)`
  lets a plugin hand its embedded `content/` tree to the help loader from `init()`,
  mirroring the existing SPI storage-factory registration pattern; the binary now
  builds its tree from the OSS content plus every registered overlay. Topics at
  fresh paths are added; collisions follow the loader's existing "later wins" plus
  `see_also` union semantics, and front-matter validation applies identically to
  overlay content. OSS-only behaviour is unchanged when no overlay is registered.
  ([#439](https://github.com/Cyoda-platform/cyoda-go/issues/439))

- **`cyoda help config all` and two new config subtopics.** `config all` prints
  every `CYODA_*` variable as a table (`--format=json` for the machine-consumable
  form; HTTP `GET /help/config/all` always returns JSON), assembled at request
  time from a root-side registry plus each registered plugin's SPI
  `DescribablePlugin.ConfigVars()` — so an out-of-tree backend's variables appear
  with no root import. New `config.cluster` and `config.scheduler` subtopics give
  the cluster/dispatch and scheduler variables first-class discoverability instead
  of prose in the parent topic. A completeness test asserts the registry covers
  every `CYODA_*` variable scanned from the root and in-tree plugin sources, and a
  default-drift test binds each registry default to `DefaultConfig()`.
  ([#395](https://github.com/Cyoda-platform/cyoda-go/issues/395))

- **gRPC entity PATCH documented, with an event-type parity guard** — the `grpc`
  help topic's entity-management event-type catalogue was missing
  `EntityPatchRequest` (and the model-management list was missing
  `EntityModelSetUniqueKeysRequest`) even though both are registered and tested,
  making them invisible to every downstream consumer of the help artefacts. Both
  are now documented, and a parity test pins the catalogue against the registered
  event-type constants so it cannot drift again. Documentation only — no runtime
  change. ([#401](https://github.com/Cyoda-platform/cyoda-go/issues/401))

### Changed

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
  `limit < 1` for the same reason. SPI-side this is a breaking change:
  `Searcher.Search` must fail rather than truncate, `MergePage` is renamed
  `MergeBounded` (drops its `offset` parameter), and `SearchOptions.Offset` is
  removed outright. See `docs/cloud-parity/direct-search-bounded-or-fail.md`.
  ([#437](https://github.com/Cyoda-platform/cyoda-go/issues/437))

- **Search/criteria predicate evaluation is now type-directed and same-type
  only**, aligning cyoda-go with Cyoda Cloud's evaluation model (see
  `cyoda help predicates`, `docs/cloud-parity/operator-semantics.md`).
  Observable changes:
  - Comparison is same-type: an operand is parsed against the field's
    declared type(s), so a numeric-looking string and a JSON number are
    treated identically; there is no cross-type coincidental match.
  - Numbers compare via precise arbitrary-precision types, replacing the
    prior `float64`-based coercion (correct beyond 2^53).
  - `LIKE` is now an anchored, escaped glob (`%`/`_`/`\`-escape, whole-string,
    case-sensitive) on every backend — SQL `LIKE` is no longer wildcard-neutered.
  - Data-field temporal comparison is lit up (six subtypes with resolution
    upscale/downscale), subsuming the earlier temporal-search work; meta-field
    temporal (`creationDate`/`lastUpdateTime`) now also accepts a coarser
    operand (e.g. a bare year) and upscales it, relaxing the prior
    offset-mandatory rule.
  - Validation is parse-based: a `400` is returned only when the operand
    parses into none of the field's declared types. This replaces the
    previous value-type-assignability check, so some requests that used to
    400 now succeed (and vice versa) — see `errors.CONDITION_TYPE_MISMATCH`.
  - Negative operators (`NOT_EQUAL`, `NOT_CONTAINS`, `INOT_*`, ...) on an
    absent or `null` field now evaluate to **non-match** (previously matched
    via `!positive`).
  - `BETWEEN_INCLUSIVE` is fixed: it previously fell through to a regex
    evaluation on `Searcher`-backed stores instead of an inclusive range check.
  - A field observed as both an object and a bare scalar is now searchable
    via its scalar type; a scalar operator against a pure-container (object)
    path now rejects `400 INVALID_FIELD_PATH` instead of silently returning
    an empty result.
  - `IS_CHANGED`/`IS_UNCHANGED` remain unimplemented — not search predicates.

  Structurally, the two Go leaf-comparison implementations (`internal/match` and
  `spi.MatchFilter`) now delegate to one shared kernel in the SPI, so the
  fallback and pushdown paths cannot drift; the SQL planners mirror it and stay
  guarded by the cross-backend parity suite.
  ([#431](https://github.com/Cyoda-platform/cyoda-go/issues/431))

- **Processor `config.attachEntity` now defaults to `true`.** A processor whose
  `config` omits `attachEntity` is imported with `attachEntity: true`, so the
  full entity payload is attached to its calculation request — matching
  `schedule.function` and the criterion `function` callout, which already
  default to `true`. Set `attachEntity: false` explicitly to opt out. Existing
  workflows that omit the field and re-import (e.g. export → import) will start
  attaching the entity payload; compute nodes that ignore the payload are
  unaffected.
  ([#421](https://github.com/Cyoda-platform/cyoda-go/issues/421))

- **CloudEvent `authtype` values are now `user`/`service`/`system` (breaking).**
  The Auth Context extension attribute previously emitted `user` or
  `service_account`, inferred by sniffing `ROLE_M2M`; it is now driven by the
  originating principal's explicit kind. `service_account` is retired. The
  attribute is always present and faithful — an unset or unrecognized kind fails
  the callout dispatch rather than emitting a normalized value. Compute nodes
  switching on the old string must be updated.
  ([#430](https://github.com/Cyoda-platform/cyoda-go/issues/430))

### Fixed

- **Conditional delete forwards a classified 4xx from its delete-selection
  search** instead of surfacing an opaque `500`.
  ([#437](https://github.com/Cyoda-platform/cyoda-go/issues/437))

- **Direct search now applies the documented default limit** — omitting `limit`
  on `POST /api/search/direct/{entityName}/{modelVersion}` (and the gRPC
  `EntitySearchCollection`) now caps results at the documented default of 1000
  on every storage backend; previously this default was applied only on the
  `GetAll`+match fallback branch, while the `Searcher` pushdown branch used by
  all three OSS backends (memory/sqlite/postgres) treated an omitted limit as
  unbounded, returning the entire matched set.
  ([#432](https://github.com/Cyoda-platform/cyoda-go/issues/432))

- **`creationDate`/`lastUpdateTime` meta filters now compare chronologically, not
  lexically** — search conditions and workflow criteria on these fields compare
  values as floored epoch-milliseconds (consistent across memory/sqlite/postgres),
  not as raw RFC3339 strings, so results no longer depend on incidental
  lexical-vs-temporal ordering agreement. Both fields now accept only comparison
  operators (`EQUALS`/`NOT_EQUAL`/`GREATER_THAN`/`LESS_THAN`/`GREATER_OR_EQUAL`/
  `LESS_OR_EQUAL`/`BETWEEN`/`IS_NULL`/`NOT_NULL`) with offset-bearing RFC3339
  operands; a string/pattern operator or a non-RFC3339 operand is rejected
  `400 CONDITION_TYPE_MISMATCH`, and an unknown meta filter field is rejected
  `400 INVALID_FIELD_PATH`. (The offset-mandatory rule was subsequently relaxed
  by the type-directed kernel — see **Changed**.)
  ([#423](https://github.com/Cyoda-platform/cyoda-go/issues/423))

- **Depth-2 nested joined cascade no longer deadlocks the transaction** — when a
  joined compute-node callback ran a transition whose own SYNC processor drove a
  *further* joined write on the same transaction `T` (a 2-deep same-transaction
  cascade), the third-level write blocked on the per-transaction gate the
  second-level callback still held while parked in its processor dispatch. The
  transaction hung for the full 30 s dispatch timeout and then failed
  `WORKFLOW_FAILED`, forcing callers to break the join (run the inner transition
  in its own transaction) and sacrifice cross-entity atomicity. The per-tx gate
  is now **released across every external dispatch** (every processor mode —
  SYNC / ASYNC_SAME_TX / ASYNC_NEW_TX — and FUNCTION criterion call-out) and
  re-acquired before the buffer is touched again —
  generalising the owner-side H3 invariant ("never hold the gate across the
  engine's dispatch") to every joined callback. The dispatch window touches no
  local buffer and is the one place a descendant callback can re-enter, so the
  release is safe for concurrent same-tx siblings. Covers both HTTP and gRPC entry
  points (both funnel through the same entity handler).
  ([#410](https://github.com/Cyoda-platform/cyoda-go/issues/410))

- **Compute-node callback transaction-join now covers the gRPC search RPCs** — a
  processor/criteria callback that presented a valid `tx-token` on `EntitySearch` /
  `EntitySearchCollection` had the token silently ignored: the joining
  `txRouteInterceptor` was wired only for the write RPCs (`EntityManage` /
  `EntityManageCollection`), so a callback's searches ran unjoined against
  last-committed state while its writes joined the originating transaction `T`.
  Read-your-own-writes was therefore asymmetric — a processor could not search for
  entities it created or transitioned earlier in the same still-open transaction
  (silent stale results, no error). The interceptor now routes the search RPCs
  through the same join / peer-forward path: a local-owner token joins `T` for the
  read; a peer-owner token forwards the search to the owner (B→A), mirroring the
  write path. Routing/join failures on a search return the search-shaped error
  envelope (`EntityResponse`, `Success=false`) rather than a raw gRPC status, and
  a token-less search is unchanged (no join). The HTTP entity API already joined
  reads (route-agnostic `X-Tx-Token` middleware) and was unaffected.
  ([#402](https://github.com/Cyoda-platform/cyoda-go/issues/402))

- **gRPC server no longer echoes member keep-alives** — the streaming receive loop
  replied with a keep-alive to every inbound member keep-alive while also sending
  its own on a 10 s ticker. Against a client that likewise echoes, this produced a
  delay-free feedback loop that pinned both the server and the compute node at
  ~100% CPU indefinitely, with no INFO-level log output. An inbound keep-alive is
  now liveness-only (`UpdateLastSeen`), removing the storm class regardless of
  client behaviour. ([#417](https://github.com/Cyoda-platform/cyoda-go/issues/417))

- **Malformed criterion regexes are rejected at workflow import** — a
  `MATCHES_PATTERN` criterion carrying a non-compiling pattern (e.g. `"["`)
  imported successfully and then errored on every evaluation of that transition.
  Import validation now compiles each pattern with the exact call the evaluator
  uses, so the failure surfaces at registration as `400 VALIDATION_FAILED`
  instead of at runtime. No schema-version bump: a non-compiling regex was never
  a working criterion, so no valid config is newly rejected. Eval-time behaviour
  is untouched. ([#425](https://github.com/Cyoda-platform/cyoda-go/issues/425))

- **Boolean search conditions on postgres no longer 500** — a `simple` search
  condition comparing a JSON path to a boolean (`{"operatorType":"EQUALS","value":true}`)
  returned `500 SERVER_ERROR` (`unable to encode true into text format for text (OID 25):
  cannot find encode plan`) on the postgres backend. The query planner bound a raw Go
  `bool` against the text-typed `doc->>'path'` extraction, which pgx cannot encode; the
  operand is now rendered as its text form (`"true"`/`"false"`), matching the lexicographic
  text comparison already used for strings and the memory/sqlite backends. Affected normal
  searches and grouped-stats queries alike (shared query planner); memory and sqlite were
  never affected. ([#399](https://github.com/Cyoda-platform/cyoda-go/issues/399))

### Security

- Bumped `google.golang.org/grpc` `v1.81.1` → `v1.82.1` (HIGH). The grouped
  Dependabot proposal stopped at `v1.82.0` and would have left the advisory open.

- Bumped `github.com/getkin/kin-openapi` `v0.142.0` → `v0.144.0` for
  GHSA-r277-6w6q-xmqw and GHSA-jpcw-4wr7-c3vq (CRITICAL, fail-open auth bypass).
  The advisories are in `openapi3filter`, which cyoda-go uses only in the E2E
  conformance validator; the shipped binary uses kin-openapi's `openapi3` for
  spec parsing (help topic tags, the Scalar docs page) and never for request
  authentication. Dependabot proposed `v0.142.0`, short of the fix.

- Bumped `github.com/oapi-codegen/oapi-codegen/v2` `v2.7.0` → `v2.7.1` (LOW) and
  regenerated `api/generated.go`.

- Cleared every open CodeQL finding. Two were genuine bugs: a path traversal via
  a caller-supplied message id reaching `os.Remove` in the memory plugin's
  message store, and a typed-nil-in-interface guard that could never fire,
  marshalling a dangling `$ref` to `"null"` in the help renderer. Also fixed
  silent `int32` truncation with a swallowed parse error and two overflow-prone
  size hints. `go/log-injection` (49 sites) is excluded in
  `.github/codeql-config.yml` with a test pinning the rationale: `slog`'s
  `TextHandler` quotes injected newlines, so the protection is a property of the
  handler rather than of each call site.
  ([#449](https://github.com/Cyoda-platform/cyoda-go/issues/449),
  [#450](https://github.com/Cyoda-platform/cyoda-go/issues/450))

## [0.8.2] — 2026-07-08

### Added

- **Entity partial-update (PATCH / RFC 7386 merge patch)** — `PATCH /api/entity/{format}/{entityId}`
  and `PATCH /api/entity/{format}/{entityId}/{transition}` apply a sparse JSON patch to the stored
  payload with RFC 7386 merge semantics (non-null key overwrites, explicit `null` deletes, omitted
  key untouched), closing the data-loss footgun where `PUT`'s wholesale-replace silently destroyed
  omitted fields. JSON-only (`XML` ⇒ `415`); `application/merge-patch+json` is implemented,
  `application/json-patch+json` (RFC 6902) returns `501`. `If-Match` is **required** (the merge is
  relative to the base the caller read): absent ⇒ `428 PRECONDITION_REQUIRED`, stale ⇒ `412`. The
  merged result is validated strictly against the model schema — PATCH never extends the model,
  even in an extend-permitting mode. Under a named transition the merge is applied first, then the
  transition's processors run on the merged state. New error codes: `PRECONDITION_REQUIRED` (428),
  `UNSUPPORTED_MEDIA_TYPE` (415).
  ([#341](https://github.com/Cyoda-platform/cyoda-go/issues/341))

- **Renderer annotations on processors & criteria** — the engine-ignored `annotations` bag now
  extends to the two workflow elements that lacked it: processors carry an embedded `annotations`
  object, and criteria carry a sibling `criterionAnnotations` object on the workflow and on each
  transition (the criterion tree round-trips verbatim and is never parsed to attach metadata). Two
  well-known optional keys — `displayName`, `description` — are documented uniformly across all
  five element types (workflow, state, transition, processor, criterion) for renderer and
  condition-builder use. Object-only, capped at 64 KB per field, stored and re-emitted compacted,
  never interpreted by the engine; processor annotations are stripped before dispatch and never
  reach compute members. Additive schema change: the workflow schema moves to **1.2** and every
  existing 1.1 payload remains valid (dual-shape). No new error codes.
  ([#384](https://github.com/Cyoda-platform/cyoda-go/issues/384))

- **Composite unique keys** — entity models can declare one or more composite unique keys via
  `PUT /model/{entityName}/{modelVersion}/unique-keys` (UNLOCKED models only). Each key is an
  ordered set of scalar field paths; uniqueness is scoped to `(tenant, model, version)` live
  entities. All-or-nothing null rule: all fields null or absent ⇒ exempt; partial ⇒
  `422 INVALID_UNIQUE_KEY`; all present ⇒ enforced on create and update. String comparison is
  byte-exact (case-sensitive, no normalization). Soft-delete frees the value-set.
  Supported by memory, sqlite, and postgres; the commercial backend returns
  `422 COMPOSITE_KEY_UNSUPPORTED` until its own support lands.
  New error codes: `UNIQUE_VIOLATION` (409), `INVALID_UNIQUE_KEY` (422),
  `COMPOSITE_KEY_UNSUPPORTED` (422), `INVALID_UNIQUE_KEY_DEFINITION` (422).

- **Search result sorting** — both search endpoints (`POST /api/search/direct/{entityName}/{modelVersion}`
  and the async variant `POST /api/search/async/{entityName}/{modelVersion}`) now accept one or more
  `sort` query parameters (HTTP) or a structured
  `orderBy` array (gRPC) to control result order. HTTP grammar: `[@]path[:asc|desc]` — bare
  dotted path for scalar data fields; `@`-prefixed name for meta fields (`state`, `creationDate`,
  `lastUpdateTime`, `transitionForLatestSave`, `transactionId`, `id`). Ordering is canonical
  across all backends: Text (byte order), Numeric (IEEE-754 double), Bool (`false < true`),
  Temporal (ms-floored chronological for meta date fields). Absent/null values sort last;
  `entity_id` is always the final tiebreaker. Unsortable, array, or unknown paths return
  `400 INVALID_FIELD_PATH`. Sort key count is capped at `CYODA_SEARCH_MAX_SORT_KEYS`
  (default `16`). New SPI field: `OrderSpec.Kind OrderKind` (enum: `OrderText`, `OrderNumeric`,
  `OrderBool`, `OrderTemporal`); ships with `cyoda-go-spi v0.8.2`.

- **Compute-node callback transaction-join** — processor and criteria-evaluation
  callbacks from a compute node now join the originating workflow transaction
  (`T`) rather than running in a standalone transaction. The engine mints a
  signed HMAC tx-token `{NodeID, TxRef}` before each dispatch and attaches it
  to the outbound CloudEvent as the `cyodatxtoken` extension attribute. Compute
  nodes echo the token on callbacks (`X-Tx-Token` HTTP header / `tx-token` gRPC
  metadata); the receiving node verifies the HMAC and routes the callback to the
  owner — local `Join` when owner is self, HTTP reverse proxy or gRPC EntityManage
  B→A forward otherwise. Callbacks see the cascade's uncommitted writes; callback
  acks are provisional until `T` commits. `ASYNC_NEW_TX` callbacks join `T` via a
  savepoint so writes are discarded on processor failure without aborting the
  cascade. An absent token causes the callback to run standalone (`Begin`), which
  is the normal behaviour for `COMMIT_BEFORE_DISPATCH` with
  `startNewTxOnDispatch=false`. New env vars: `CYODA_TX_TOKEN_TTL` (token
  validity, default `90s`), `CYODA_GRPC_NODE_ADDR` (gRPC address advertised in
  tokens for B→A forwarding), `CYODA_COMPUTE_HTTP_BASE` (base URL for
  compute-test-client HTTP callbacks).

- **Conditional `deleteEntities`** — `DELETE /api/entity/{entityName}/{modelVersion}` now honours
  an `AbstractConditionDto` request body, deleting only matching entities (empty body ⇒ all).
  `verbose=true` returns the deleted ids; `numberOfEntitites` (matched) and
  `numberOfEntititesRemoved` (removed) are reported separately. Closes a data-loss defect where
  the condition was ignored and the whole model was wiped. New error code `INVALID_CONDITION` (400).

- **`getAllEntities` point-in-time** — the model-scoped list read honours `pointInTime`, returning
  entities as-at the supplied time and stamping `meta.pointInTime`.

- **OpenAPI error-code conformance** — the E2E conformance validator now enforces documented
  error codes (`errorCode` string granularity) for the entity endpoints, in addition to response
  shapes.

- **Config-conditional `501` documented** — 21 IAM-gated operations (OIDC providers, JWT
  keypairs, trusted keys, M2M clients) now declare `501 NOT_IMPLEMENTED` in the spec when
  `CYODA_IAM_MODE ≠ jwt`. The 5 trusted-key ops additionally declare `404 FEATURE_DISABLED`
  when `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED=false` (default off); the `501` is only
  reached when that feature is enabled and IAM ≠ jwt.

- **`getTechnicalUserToken` spec completions** — `client_credentials` grant type, `405
  method_not_allowed` on non-POST requests, and `server_error` / `method_not_allowed` error
  enum values are now declared in the spec.

### Changed

- **`DELETE /model/{entityName}/{modelVersion}` now enforces the documented
  UNLOCKED precondition** — deleting a `LOCKED` model returns `409
  MODEL_ALREADY_LOCKED` (previously the lock state was ignored). Unlock the model
  first. The `409 MODEL_HAS_ENTITIES` guard is unchanged.

- **Entity `meta` is typed-but-open** — `Envelope.meta` mirrors the canonical `EntityMetadata`
  (typed properties, never sealed); the obsolete `previousTransition` field is removed.

- **`listOidcProviders.activeOnly` retyped to boolean** — standard truthy values
  (`1`, `true`, `TRUE`, `t`) now correctly filter active-only results; unparseable
  values such as `?activeOnly=yes` return `400` instead of silently meaning false.

- **`changeType` spelling** — entity change records now use the canonical `CREATE/UPDATE/DELETE`
  across HTTP, gRPC, and the OpenAPI schema (HTTP previously emitted `CREATED/UPDATED/DELETED`).

- **gRPC entity `meta`** — now includes `modelKey` (and `pointInTime` when as-at), matching HTTP.

- Tightened the `create`/`createCollection` request-body schemas to their real shapes; documented
  unique-key `409`/`422` codes and reverse-chronological change ordering on the entity endpoints.

- PostgreSQL search now pushes supported predicates into SQL (JSONB `->>`
  extraction, numeric/range/string comparisons) instead of loading every entity
  of a model and filtering them in memory. Non-pushable operators (regex,
  case-insensitive) are post-filtered while rows stream, and `LIMIT`/`OFFSET`
  are pushed into SQL when no residual remains. This is a constant-factor win —
  no full-result wire transfer, no decode of every document, filtering and
  pagination done in the database — not a JSON-path-index speedup; adding
  indexes on queried paths remains a separate operational step. SQLite already
  did this; the memory backend keeps filtering in memory by design.
  ([#37](https://github.com/Cyoda-platform/cyoda-go/issues/37))

- **Unknown model → `404 MODEL_NOT_FOUND` (uniform)** — all model-scoped read
  operations (`getAllEntities`, `getEntityStatisticsForModel`,
  `getEntityStatisticsByStateForModel`, `searchEntities`, `submitAsyncSearchJob`,
  `queryGroupedEntityStatisticsForModel`) now return `404 MODEL_NOT_FOUND` when
  the requested model is not registered for the calling tenant. Previously these
  paths returned empty results silently; the ad-hoc `UNKNOWN_MODEL` code used by
  grouped-stats is retired.

- **`searchEntities` limit enforcement** — `limit > 10000` is now rejected with
  `400 BAD_REQUEST` across synchronous search (HTTP), gRPC direct search, and
  async search submission. Previously the spec documented this as a silent clamp.

- **`searchEntities` content type** — the synchronous search endpoint responds
  with `application/x-ndjson` only; the previously-listed `application/json`
  variant is removed from the contract.

- **`listOidcProviders` fictional `403` removed** — the `403` response was never
  emitted by the server (the endpoint is auth-only, not admin-only); the spec entry
  is removed.

- **Edge-message request metadata field renamed `meta-data` → `metaData`** — the
  `POST /api/message/new/{subject}` request envelope now carries its optional metadata map
  under `metaData` (camelCase), symmetric with the `getMessage` response and consistent with
  the rest of the API's JSON naming. The former kebab-case `meta-data` key is no longer honored;
  its contents are ignored. Breaking input change, shipped in a patch because edge messages have
  no known consumers. A new `cyoda help messages` topic documents the full edge-message API.
  ([#386](https://github.com/Cyoda-platform/cyoda-go/issues/386))

### Removed

- **`pointInTime` param on `getAsyncSearchResults`** — the point-in-time is
  fixed at job submission; the param was a no-op and is removed from the contract.

- **`timeoutMillis` param and `408` on `searchEntities`** — these were fictional
  contract surface not backed by an implementation; both are removed. Actual
  per-request timeout support is tracked separately.

- **Fictional time-based-UUID `400` on `getStateMachineFinishedEvent`** — any
  valid UUID is accepted; the fictional constraint is removed from the spec.

### Fixed

- **OIDC / admin op error envelope** — the 7 OIDC provider ops and
  `searchEntityAuditEvents` now declare `application/problem+json` `ProblemDetail`
  errors in the spec, matching the server. `getTechnicalUserToken` retains the
  RFC-6749 flat OAuth shape (`{error, error_description}`).

- **`registerOidcProvider` duplicate returns `409`** — duplicate provider
  registration now returns `409 OIDC_PROVIDER_DUPLICATE` (was `400`). The `400`
  path remains for validation failures (`OIDC_SSRF_BLOCKED`, `OIDC_INVALID_TENANT`,
  malformed body).

- **`ProblemDetailDto` schema consolidated** — the structural duplicate is removed;
  the 9 async-search error responses now reference the canonical `ProblemDetail`
  schema.

- **`getStateMachineFinishedEvent` error envelope** — error responses now use
  `application/problem+json` (`ProblemDetail`), matching the rest of the API.

- **`getAsyncSearchResults` documented default page size** — corrected from 10
  to 1000 in the spec; the implementation was already using 1000.

- Point-in-time ("as at T") reads now apply one canonical rule across all
  storage engines and read paths: inclusive of the requested instant (`<=`),
  compared at native precision, with no millisecond round-up. Previously the
  memory engine and the SQL `GetAsAt`/`GetAllAsAt` paths rounded the requested
  time up to the next millisecond (over-including later same-millisecond
  versions), and sqlite used a strict `<` bound — so different backends, and
  different read paths within one backend, could disagree at sub-millisecond
  boundaries. ([#349](https://github.com/Cyoda-platform/cyoda-go/issues/349))

### Security

- Bumped the Go toolchain `go 1.26.4` → `go 1.26.5` (root + all three plugin
  modules and `go.work`) to clear govulncheck advisory GO-2026-5856, a reachable
  `crypto/tls` vulnerability in the Go standard library fixed in go1.26.5.

- Bumped the indirect `github.com/yuin/goldmark` dependency `v1.7.13` → `v1.7.17`
  to clear govulncheck advisory GO-2026-5320 (XSS in goldmark HTML rendering,
  reached via `glamour` in the `cyoda help` renderer). The renderer only formats
  first-party help content embedded in the binary, so the advisory was not
  reachable with attacker-controlled input; the bump keeps the security scan clean.

## [0.8.1] — 2026-06-23

> No `v0.8.0` release. The `cyoda-go-spi v0.8.0` tag was poisoned by a premature
> tag on the Go module proxy and abandoned in favour of `v0.8.1` (see
> [COMPATIBILITY.md](./COMPATIBILITY.md)). To keep the binary aligned with the
> SPI it pins, `cyoda-go` skips `v0.8.0` too — `v0.8.1` is the first v0.8.x
> binary release. No functionality differs from what `v0.8.0` would have shipped.

### Added

- Optional `annotations` JSON-object field on workflows, states, and transitions — arbitrary client-owned metadata, stored and round-tripped (compacted) but never interpreted by the engine. Object-only, capped at 64 KB per field.
- New error code `WORKFLOW_SCHEMA_VERSION_UNSUPPORTED` (`400`).
- New help topic `workflows.schema-version` documenting the wire-format contract.
- New help action `cyoda help workflows schema-version versions` emitting the supported-version manifest as JSON.
- HTTP help-action mirror: `GET /help/<topic>/<action>` now reachable for every registered action (`grpc proto/json`, `openapi json/yaml/tags`, `cloudevents json`, `workflows.schema-version versions`) with declared `Content-Type`.
- OIDC provider per-tenant registry with 7 REST endpoints under `/oauth/oidc/providers` (register, list, get, update, invalidate, reactivate, delete, reload). Closes [#284](https://github.com/Cyoda-platform/cyoda-go/issues/284).
- Chained multi-issuer JWT validator: `JWKSValidator` (local issuer) first, `OIDCValidator` (registered OIDC providers) second — per ADR 0002 decision D3.
- Per-provider configurable fields: `issuers` (accepted `iss` values), `expectedAudiences`, `rolesClaim` (overrides the global default per-provider).
- Cluster broadcast for OIDC cache eviction: when a provider record is mutated or reloaded, a fire-and-forget message on `oidc-providers.invalidate` evicts the JWKS cache on every peer node — consistent with the model-cache invalidation pattern (single topic, no ACK required per ADR 0002 D7).
- 6 new env vars: `CYODA_OIDC_REQUIRE_HTTPS`, `CYODA_OIDC_CONNECT_TIMEOUT_MS`, `CYODA_OIDC_SOCKET_TIMEOUT_MS`, `CYODA_OIDC_CONNECTION_REQUEST_TIMEOUT_MS`, `CYODA_OIDC_ALLOW_PRIVATE_NETWORKS`, `CYODA_OIDC_ROLES_CLAIM`.
- 4 new error codes: `OIDC_PROVIDER_DUPLICATE`, `OIDC_PROVIDER_NOT_FOUND`, `OIDC_PROVIDER_INACTIVE`, `OIDC_SSRF_BLOCKED`. (5 additional error codes are wire-stubbed for future bearer-auth translation.)
- ADR 0002 — Federated Identity Provider Architecture (`docs/adr/0002-federated-identity-provider-architecture.md`).
- `/oauth/keys/keypair/*` and `/oauth/keys/trusted/*` — 10 admin endpoints now conform to the OpenAPI surface via chi-routed adapters in `internal/domain/account/`. ([#281](https://github.com/Cyoda-platform/cyoda-go/issues/281), sub-issue of [#194](https://github.com/Cyoda-platform/cyoda-go/issues/194))
- `/clients` OpenAPI surface — `GET /clients`, `POST /clients`, `DELETE /clients/{clientId}`, `PUT /clients/{clientId}/secret`. M2M client management is now reachable at the spec-conformant paths with the spec-conformant DTOs.
- 6 new error codes: `FEATURE_DISABLED`, `KEY_OWNED_BY_DIFFERENT_TENANT`, `KEYPAIR_NOT_FOUND`, `TRUSTED_KEY_CAP_REACHED`, `UNSUPPORTED_ALGORITHM`, `UNSUPPORTED_KEY_TYPE`.
- Error code `M2M_CLIENT_NOT_FOUND` (HTTP 404) emitted by the `/clients` admin operations on unknown or cross-tenant `clientId`.
- 5 new env vars: `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED`, `CYODA_IAM_TRUSTED_KEY_MAX_PER_TENANT`, `CYODA_IAM_TRUSTED_KEY_MAX_VALIDITY_DAYS`, `CYODA_IAM_TRUSTED_KEY_MAX_JWK_PROPERTIES`, `CYODA_IAM_KEYPAIR_DEFAULT_VALIDITY_DAYS`. Plus `CYODA_JWT_BOOTSTRAP_AUDIENCE`.
- `CYODA_IAM_M2M_ADMIN_ROLE_ENABLED` env (default `false`) gates the `withAdminRole=true` query parameter on `POST /clients`. When off the request returns `404` with error code `FEATURE_DISABLED`.

### Changed

- **Cross-tenant OIDC routing is now deterministic and safe.** When two tenants register the same `wellKnownConfigUri` (same physical IdP), tokens are no longer routed non-deterministically via Go's randomized map iteration. Resolution now uses a two-layer disambiguation: (1) audience-based routing — if the JWT's `aud` claim uniquely matches one tenant's `ExpectedAudiences`, that provider is selected; (2) if no unique audience match exists (both providers have empty or overlapping `ExpectedAudiences`), the token is rejected with `401 UNAUTHORIZED` (`ErrAmbiguousProvider`). Operators sharing an IdP across tenants MUST set distinct `expectedAudiences` on each registration. A `WARN` log (`oidc.cross_tenant_audience_overlap`) is emitted at Register-time when audience overlap is detected. ([#284](https://github.com/Cyoda-platform/cyoda-go/issues/284))
- **OIDC pinned `issuers` are now enforced at discovery-fetch time.** When a provider's `issuers` list is non-empty, the discovery document's `issuer` field must match one of the pinned values; a mismatch refuses to install the provider source (logged at `WARN` with SHA-256 hashes of the issuer values, never raw strings) and the provider remains in the Phase-2-pending state until the admin reconciles. This is defence-in-depth: the runtime `issMatches` already enforces the pin at token-resolution time, but enforcing it at fetch time prevents the registry from caching an attacker-controlled `discoveryDoc.Issuer` value that could be silently trusted by future diagnostics or metrics code. ([#284](https://github.com/Cyoda-platform/cyoda-go/issues/284))
- OIDC provider registration now requires the calling tenant to be UUID-shaped.
  Bootstrap deployments using the literal `default-tenant` string for
  `CYODA_BOOTSTRAP_TENANT_ID` must migrate to a real tenant UUID before
  registering OIDC providers. Returns `400 BAD_REQUEST` with code
  `OIDC_INVALID_TENANT` otherwise.
- Seven `/oauth/oidc/providers/*` endpoints previously returned `501 NOT_IMPLEMENTED`; they now return real responses. Clients that special-cased the 501 status on these paths should update their error handling.
- Legacy `/oauth/keys/` prefix mux entry removed from `app/app.go`; chi router now owns all `/oauth/keys/*` paths.
- JWKS endpoint (`/.well-known/jwks.json`) now publishes grace-period-invalidated keys until their `validTo` passes.
- `KVTrustedKeyStore` KV-key encoding within the `trusted-keys` namespace changed from `<kid>` to `<tenantID>:<kid>`. Tenant isolation is now enforced at the storage layer.
- Trusted-key per-tenant cap counts only currently-valid keys (matches Cyoda Cloud).
- `withAdminRole` query parameter on `POST /clients` tightened from `string` to `boolean` in `api/openapi.yaml`. This is a deliberate divergence from the upstream Cyoda Cloud OpenAPI declaration.
- `auth.M2MClient.TenantID` promoted from `string` to `spi.TenantID`. `M2MClient` now carries `CreatedAt` and `UpdatedAt` timestamps.
- Workflow import (`POST /model/{entityName}/{modelVersion}/workflow/import`) now rejects structurally broken workflows with `400 VALIDATION_FAILED` instead of accepting them and degrading silently at runtime. New rules: empty workflow name, duplicate workflow names within a single request, empty `initialState`, `initialState` not declared in `states`, empty state-map key, transition `next` not declared in `states`, empty or duplicate transition names within a state, empty processor name, workflow / state / transition / processor name length > 256 chars, and unknown `executionMode` values. OpenAPI schema updated with matching `minLength: 1` + `maxLength: 256` on identifier fields and `propertyNames` on the `states` map. See ⚠️ Breaking changes below. ([#255](https://github.com/Cyoda-platform/cyoda-go/issues/255))
- Workflow import (`POST /model/{entityName}/{modelVersion}/workflow/import`) now honours an explicit `"active": false` on each incoming `WorkflowDefinition`. The handler previously force-overrode every incoming `active` to `true`; it now defaults to `true` only when the field is absent (or explicitly `null`) and passes explicit `true` / `false` through unchanged. This restores export → REPLACE re-import idempotency and lets operators stage inactive workflows. See ⚠️ Breaking changes below. ([#256](https://github.com/Cyoda-platform/cyoda-go/issues/256))
- Workflow engine: substitution of the embedded default workflow now emits a `slog.Warn` line in addition to the existing response-body warning. Log fields: `pkg=workflow`, `tenant`, `entityName`, `modelVersion`, `entityId`, `reason` ∈ {`no_workflows_imported`, `no_criterion_matched`}. The body warning text is corrected per cause — `"no workflows imported for model — using default workflow"` for the cold paths in `Execute` / `ManualTransition` / `Loopback`, `"no imported workflow matched entity — using default workflow"` for `selectWorkflow`. Operators driving large fleets can now detect models silently running on the embedded default. ([#256](https://github.com/Cyoda-platform/cyoda-go/issues/256))
- Workflow import (`POST /model/{entityName}/{modelVersion}/workflow/import`) now decodes the request body with `DisallowUnknownFields`. Unknown fields — at the top level *or* nested in the workflow / state / transition / processor sub-shapes — are rejected with `400 BAD_REQUEST` and the offending field name surfaced in the response detail (Go's decoder emits `json: unknown field "X"`). Typos like `"transitionn"` for `"transitions"` no longer silently import as a no-op. See ⚠️ Breaking changes below. The broader `DisallowUnknownFields` sweep across entity / cluster / auth boundaries remains in [#145](https://github.com/Cyoda-platform/cyoda-go/issues/145). ([#264](https://github.com/Cyoda-platform/cyoda-go/issues/264))

### Removed

- Private `/account/m2m*` HTTP surface and its `internal/auth/m2m.go` handler. M2M client management is exclusively at `/clients` going forward. `/account/m2m*` was never OpenAPI-declared.
- `501 NOT_IMPLEMENTED` response declarations on `listTechnicalUsers`, `createTechnicalUser`, `deleteTechnicalUser`, `resetTechnicalUserSecret`, `getTechnicalUserToken` in `api/openapi.yaml`.

### ⚠️ Breaking changes

- **Reactivate semantics changed.** `POST /oauth/keys/keypair/{keyId}/reactivate` and `POST /oauth/keys/trusted/{keyId}/reactivate` now require a `ReactivateKeyRequestDto` body with `validTo > now` (and `> validFrom` if supplied). Previously these endpoints had no request body. Cyoda Cloud's behaviour of clearing `validTo` to nil (zombie key) is intentionally not adopted; see [#281](https://github.com/Cyoda-platform/cyoda-go/issues/281) spec for rationale.
- **Trusted-key registration is disabled by default.** Set `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED=true` to enable. Customers using `/oauth/keys/trusted/*` through the legacy mux must opt in.
- **Bootstrap signing key now has finite validity.** Defaults to 365 days (configurable via `CYODA_IAM_KEYPAIR_DEFAULT_VALIDITY_DAYS`). Long-running deployments must rotate before expiry; the startup banner emits a `WARN` if the active key expires within 30 days.
- **Algorithm scope.** cyoda-go v0.8.0 signs and verifies `RS256` only. The OpenAPI declares the full enum (`RS*`, `PS*`, `ES*`, `EdDSA`); non-`RS256` values are rejected with `400 UNSUPPORTED_ALGORITHM`. Trusted-key registration accepts only `kty=RSA` JWKs (`kty=EC`/`OKP` rejected with `400 UNSUPPORTED_KEY_TYPE`). v0.8.1 follow-up tracks multi-algorithm + non-RSA `kty` support.
- **Workflow-import structural validation tightened.** Imports that previously succeeded with structurally broken shapes (typo'd `executionMode`, empty/dangling `initialState`, transitions pointing at undeclared states, duplicate workflow names within a request, duplicate transition names within a state, empty workflow / state / transition / processor names, identifiers longer than 256 characters) now fail with `400 VALIDATION_FAILED`. These new H4/H6 structural rules apply to the **incoming request only** — existing stored workflows are not retroactively re-validated against them, so an in-place upgrade does not invalidate previously-imported configurations. The pre-existing cycle-detection and `startNewTxOnDispatch` flag-coherence checks continue to run against the merged result and so still catch regressions in stored workflows, preserving pre-v0.8.0 semantics for those specific invariants. ([#255](https://github.com/Cyoda-platform/cyoda-go/issues/255))
- **Workflow-import `active` field is no longer force-overridden.** Previously, every incoming workflow's `active` was unconditionally set to `true`, so a client sending `"active": false` was silently re-activated. The handler now passes explicit `true` / `false` through unchanged and only defaults to `true` when the field is absent. Clients that were relying on the force-override (knowingly or not) to coerce inactive workflows to active on import must update their payloads to send `"active": true` explicitly (or omit the field). ([#256](https://github.com/Cyoda-platform/cyoda-go/issues/256))
- **Workflow-import `workflows: []` is rejected in `REPLACE` / `ACTIVATE` modes.** Previously these modes accepted an empty workflows array, silently wiping or deactivating all stored workflows for the model and falling back to the embedded default at runtime — HTTP 200 hid the destruction. Both modes now return `400 VALIDATION_FAILED` with detail `"empty workflows array not allowed in REPLACE/ACTIVATE mode — use MERGE if you intended a no-op"`. The `workflows` key being absent entirely is equivalent to `[]` under JSON unmarshal semantics and is rejected the same way. `MERGE` with an empty array remains a legitimate no-op. ([#256](https://github.com/Cyoda-platform/cyoda-go/issues/256))
- **Workflow-import body is strict-decoded.** The `POST /model/{entityName}/{modelVersion}/workflow/import` handler now rejects any unknown JSON field anywhere in the import-request body — at the top level, on a workflow, on a state, on a transition, on a processor, or on a processor's `config` — with `400 BAD_REQUEST`. The handler also rejects trailing JSON content after the request object (a body like `{...valid request...}{junk}`), which `json.Decoder.Decode` would otherwise silently drop. Clients that previously relied on the silent-drop behaviour (e.g. sending forward-compat extras intended for a future cyoda-cloud version, or with typo'd field names) must clean their payloads before upgrading. The trade-off is intentional: typos like `"transitionn"` for `"transitions"` used to import as a no-op workflow with zero transitions, hiding the configuration error from operators. The response detail names the offending field verbatim so the fix is trivial. ([#264](https://github.com/Cyoda-platform/cyoda-go/issues/264))
- **Workflow-import `version` field is now strictly validated** as semver `MAJOR.MINOR`, and v0.8.0 bumps the supported set from `1.0` to `1.1`. Previously accepted values like `"1"` are rejected with `400 WORKFLOW_SCHEMA_VERSION_UNSUPPORTED`. `"1.0"` (the schema shipped on `release/v0.7.x`) is also rejected — v0.8.0's import surface adds enough strictness (structural validation #255, active semantics #256, asyncResult/crossover rejection #261, retryPolicy enum #262, strict-decoder #264, scheduled-transition shape) that staying on `1.0` would conflate two distinct contracts. v0.7.x clients must regenerate workflow payloads against `"1.1"`. Authoritative supported set: `GET /help/workflows/schema-version/versions` or `cyoda help workflows schema-version versions`. Bump rules and per-version notes: `docs/workflow-schema-versioning.md`.

### Fixed

- **Workflow audit-log `desc` preview is rune-aware.** The `truncateForLog` helper used by the workflow-import audit logger previously measured byte length and sliced byte offsets, splitting multi-byte UTF-8 characters (CJK, emoji, accented Latin) mid-codepoint and emitting invalid UTF-8 into the audit log. The helper now counts runes and cuts on rune boundaries, matching its documented contract. The cap is renamed `descLogPreviewRunes` to remove the bytes-vs-runes ambiguity. Surfaced by the [#264](https://github.com/Cyoda-platform/cyoda-go/issues/264) security review.
- **Workflow cycle-detection error reporting is deterministic.** `validateWorkflowLoops` previously iterated `wf.States` in Go map order, so a workflow with two or more disjoint unguarded automated cycles reported a different cycle path per run. The detector now sorts state names before iteration so the lexicographically-first cycle is reported. Surfaced by the [#264](https://github.com/Cyoda-platform/cyoda-go/issues/264) security review.

### Known limitations

- **Runtime-issued signing keypairs are lost on process restart.** The bootstrap key survives (its KID is deterministic per PEM). Persistent signing-key storage is tracked in a v0.8.x follow-up.
- **Pre-v0.8.0 KV trusted-key entries are orphaned.** Within the `trusted-keys` namespace, entries are now keyed `<tenantID>:<kid>` (was bare `<kid>`). v0.8.0 does not query the old shape; affected entries are left in place but not loaded. Operators must re-register affected keys. To audit, look for entries in the `trusted-keys` namespace whose key contains no `:` separator (the exact query depends on the KV backend; for the SQLite plugin: `SELECT key FROM kv_store WHERE namespace='trusted-keys' AND key NOT LIKE '%:%'`).
- **v0.8.0 → pre-v0.8.0 rollback hazard.** Trusted keys created under v0.8.0 are visible to pre-v0.8.0 binaries as mangled-kid entries (`<tenantID>:<kid>` treated as the kid). Purge out-of-band before rollback if visibility matters.
- **M2M clients created via `POST /clients` are held in-memory by the default `InMemoryM2MClientStore` and do not survive a server restart.** Customers running with the in-memory IAM mode must re-create their clients on every restart. A persistence follow-up tracking storage-SPI backing is on the roadmap; see the v0.8.0 milestone discussion.

### Dependencies

- Routine minor/patch dependency maintenance across the root and plugin modules: OpenTelemetry 1.43 → 1.44 (SDK, metric, trace, exporters, contrib), `jackc/pgx/v5` 5.9 → 5.10, `golang.org/x/crypto` 0.52 → 0.53, `getkin/kin-openapi` 0.139 → 0.140, `oapi-codegen/runtime` 1.4.1 → 1.4.2, `testcontainers-go` postgres 0.42 → 0.43, `ncruces/go-sqlite3` 0.34 → 0.35, and assorted `golang.org/x` updates.

---

## [0.7.0] — 2026-05-05

This release reconciles the OpenAPI spec with the actual server (#21,
breaking — see below), adds API-wide CORS, hardens the supply chain
with cosign keyless signatures on archives + checksums, closes a
suite of locking-discipline and tenant-isolation gaps across the
plugins, and ships a new chart docs surface for Gateway API operators.
18 issues closed in this milestone.

### ⚠️ Breaking changes (wire format)

The OpenAPI spec at `api/openapi.yaml` has been reconciled with the actual server wire format across all 81 declared operations. Clients generated from the pre-0.7.0 spec will be incorrect for the endpoints listed below — regenerate clients against `v0.7.0`'s `api/openapi.yaml` (or fetch via `cyoda help openapi yaml`).

**Server response shape changes:**

- **`GET /message/{messageId}` (`getMessage`)** — `content` field is now embedded JSON, not a JSON-encoded string. Wire was `"content": "{\"x\":1}"`; now `"content": {"x":1}`. Clients that did `JSON.parse(content)` must consume `content` directly.
- **Stub error code (account/IAM/OIDC/OAuth-keys ops)** — `errorCode` value in 501 responses changed from `"BAD_REQUEST"` to `"NOT_IMPLEMENTED"`. Pairs correctly with the HTTP status now.
- **`getStateMachineFinishedEvent`** — response now includes `microsTime` field (additive; non-breaking unless client strict-rejects unknown fields).

**Spec declaration changes (server unchanged but client codegen will differ):**

- All 4xx/5xx responses on entity ops, workflow export/import, and shared `components.responses.*` fragments now declare `Content-Type: application/problem+json` (RFC 9457). Server has always emitted this; spec was wrong.
- `getEntityChangesMetadata.changeType` enum corrected from `[CREATE, UPDATE, DELETE]` to `[CREATED, UPDATED, DELETED]`.
- `EntityTransactionResponse.entityIds` declared as `array<string>` (UUIDs), not `array<object>`.
- `getOneEntity` response declares the `Envelope` named schema `{type, data, meta}` instead of loose `type:object`.
- 7 malformed `type:array + sibling $ref` sites in the spec corrected to well-formed `type:array, items:{ $ref:... }` (`create`, `createCollection`, `updateCollection`, `getEntityChangesMetadata`, 3 statistics variants, `getAvailableEntityModels`).
- `messaging.deleteMessage` declares `MessageDeleteResponse` (`{entityIds: array<string>}`) instead of `EntityTransactionResponse` (no `transactionId` was ever emitted by the server).
- `messaging.deleteMessages` and `newMessage` declare `array<EntityTransactionResponse>` (was `type:string`, which never matched the server).
- 22 IAM/OAuth/OIDC/account stub endpoints declare `501 Not Implemented` per the design's deferred-implementation policy. Real implementation is tracked in #194. Clients generated from the pre-0.7.0 spec for these endpoints will be wrong.
- `basicAuth` security scheme declared (was referenced but never declared).

### Added

#### API + observability
- **`COMMIT_BEFORE_DISPATCH` processor execution mode** ([#27](https://github.com/Cyoda-platform/cyoda-go/issues/27)) — per-processor saga semantics for long-running cascades. Marking a processor with `executionMode: COMMIT_BEFORE_DISPATCH` (CBD) tells the engine to commit `TX_pre` before the processor runs and start `TX_post` for follow-on work, breaking the all-or-nothing dependency between the cascade-entry transaction and external dispatch. Engine surface gains `EngineResult.FinalCtx` / `FinalTxID` / `Segmented` so callers can commit the engine's final segment instead of the original (already-committed for CBD) entry transaction. New engine entry-points `ManualTransitionWithIfMatch` / `LoopbackWithIfMatch` plumb the caller's `If-Match` expected-txID via a single-shot context slot and apply `CompareAndSave` instead of `Save` at the FIRST CBD segment-flush, so a stale precondition aborts BEFORE any external dispatch fires (spec §4.1).
- **Per-item `ifMatch` on `PUT /api/entity/{format}` (bulk update)** ([#228](https://github.com/Cyoda-platform/cyoda-go/issues/228)) — same cross-request optimistic-concurrency precondition the single-item PUT endpoints support via `If-Match`, scoped per item on the bulk endpoint. Routing mirrors single `UpdateEntity`'s post-#27 flow: for CBD cascades the engine consumes the precondition at the first segment-flush; for non-segmenting cascades the handler applies `CompareAndSave` post-engine. Per-item `ENTITY_MODIFIED` conflicts are isolated to a new optional per-chunk `failed[]` array — the chunk still commits its remaining successful items rather than rolling everything back. Other per-item failures (missing entity, validation, non-conflict engine errors) continue to roll the chunk back, matching the pre-#228 [#92](https://github.com/Cyoda-platform/cyoda-go/issues/92) contract. When every item in a chunk fails its precondition, the chunk still commits as a zero-write transaction so the surfaced `transactionId` remains meaningful for audit correlation. Wire-format additions on `EntityTransactionResponse`: optional `failed[]` with `{entityId, error: {code, message, itemIndex}}`. `failed` uses JSON `omitempty` — fully-successful chunks keep the pre-#228 shape unchanged.
- **`TRANSITION_ABORTED` audit event** ([#228](https://github.com/Cyoda-platform/cyoda-go/issues/228) reviewer S1) — whenever a stale `ifMatch` precondition rejects an in-flight transition (engine CBD first-segment flush or handler-side post-engine `CompareAndSave`), the engine emits a paired `TRANSITION_ABORTED` event into the state-machine audit log alongside the entry-side `STATE_MACHINE_START` so consumers see a self-consistent entry+abort sequence. Payload carries `{reason: "ENTITY_MODIFIED", transitionName, expectedTxId, actualTxId}`. Applies to single `UpdateEntity` stale-`If-Match` and to the new `UpdateEntityCollection` per-item-isolated path. New constant added to `StateMachineAuditEventDto.eventType` enum.
- **API-wide CORS support** ([#196](https://github.com/Cyoda-platform/cyoda-go/issues/196)). New CORS middleware at `internal/api/middleware/cors.go` wraps the entire handler chain. Loopback-by-default mode (`http(s)://localhost`, `127.0.0.1`, `[::1]` on any port) — zero-config dev ergonomics, secure-by-default in production. Wildcard requires explicit `CYODA_CORS_ALLOWED_ORIGINS=*` (with startup WARN). Allowlist mode for production. Master switch `CYODA_CORS_ENABLED`. `/_internal/*` excluded from CORS; cluster proxy strips `Origin` and `Access-Control-Request-*` headers on outbound peer-to-peer requests (defence-in-depth).
- **OpenAPI runtime conformance validator** (`internal/e2e/openapivalidator/`) — every E2E response is matched against the spec via `kin-openapi`. Drift fails the build. Documented in [ADR 0001](./docs/adr/0001-openapi-server-spec-conformance.md).
- **2 previously-undocumented customer endpoints declared in the spec:**
  - `getEntityTransitions` (GET `/entity/{entityId}/transitions`)
  - `fetchEntityTransitions` (GET `/platform-api/entity/fetch/transitions`)
- **7 new named schemas** in `components/schemas/`: `Envelope`, `EdgeMessagePayload`, `MessageDeleteResponse`, `MessageDeleteBatchResponse`, `TransitionNameList`, `WorkflowImportSuccessDto`, `AuditEvent` (oneOf+discriminator union for state-machine + entity-change + system audit events).
- **4 shared response fragments** in `components/responses/`: `Unauthorized`, `Forbidden`, `InternalServerError`, `NotImplemented`.

#### Supply chain
- **Cosign keyless signatures** on release archives and `SHA256SUMS` ([#47](https://github.com/Cyoda-platform/cyoda-go/issues/47)). Sigstore + GitHub Actions OIDC; signing identity bound to `release.yml@refs/tags/v…` (push-trigger only). `scripts/install.sh` auto-verifies when `cosign` is on PATH; opt-out via `CYODA_COSIGN_VERIFY=false`; force-fail-without-cosign via `CYODA_COSIGN_VERIFY=required`.
- **`install.sh` published as a release asset at a stable URL** ([#49](https://github.com/Cyoda-platform/cyoda-go/issues/49)). The canonical install URL is now `https://github.com/cyoda-platform/cyoda-go/releases/latest/download/install.sh` — pinned per release, not a moving target on `main`.

#### SPI + cluster
- **`modelcache.CachingModelStore.SubscribeLocal`** ([#174](https://github.com/Cyoda-platform/cyoda-go/issues/174)) — in-process invalidation hook that fires for every model invalidation regardless of cluster topology. The path-validation cache wires through this so single-node and multi-node deployments alike react to schema changes immediately.
- **`fixtureutil.LaunchCyodaClusterAndComputeWithBinaries`** ([#157](https://github.com/Cyoda-platform/cyoda-go/issues/157)) — caller-supplied-binary variant of the cluster launcher, mirroring the single-node `…WithBinaries` symmetry. Out-of-tree backend plugins (e.g. `cyoda-go-cassandra`) can now drive the shared parity scenario suite against their own `cmd/cyoda` binary.
- **`AsRetryable()` fluent helper on `*AppError`** ([#140](https://github.com/Cyoda-platform/cyoda-go/issues/140)) — separates the (status, code, retryable) axes that were previously bundled into specialised `Conflict` / `RetryableConflict` constructors. Permits retryable 4xx with a specific dictionary code (previously unreachable). The deprecated constructors are removed; all callers migrated.

#### Documentation
- **Chart docs**: `deploy/helm/cyoda/docs/migrating-from-ingress.md` ([#57](https://github.com/Cyoda-platform/cyoda-go/issues/57)) — six-step Ingress2Gateway 1.0 walkthrough; `deploy/helm/cyoda/docs/gateway-api-policies.md` ([#58](https://github.com/Cyoda-platform/cyoda-go/issues/58)) — concrete `BackendTrafficPolicy` (rate limiting) and `SecurityPolicy` (JWT, CORS) YAML for Envoy Gateway, plus Cilium and Contour reference patterns.
- **Concurrency model** at `docs/CONCURRENCY.md` ([#199](https://github.com/Cyoda-platform/cyoda-go/issues/199) PR-C3) — per-node lock and state inventory, the SPI tx-state locking contract, cluster-routing failure modes; complements `docs/CONSISTENCY.md` (isolation contract) and `docs/ARCHITECTURE.md` (cluster routing).

### Fixed

#### API correctness
- **Collection endpoints now match the documented `transactionWindow` chunking + engine-routing contract** ([#227](https://github.com/Cyoda-platform/cyoda-go/issues/227)) — four pieces:
  - `CreateEntityCollection` now routes every item through the workflow engine. Pre-fix the handler hard-coded `State="CREATED"` and called `entityStore.SaveAll` directly, so the workflow's `initialState` was ignored, automated cascade transitions never fired during create, and no `STATE_MACHINE_*` audit events were emitted for collection-created entities. Now mirrors single `CreateEntity`'s engine flow per item.
  - `CreateCollection` and `UpdateCollection` handlers honor `transactionWindow` and chunk per the documented contract (default 100, max 1000, validated to (0, 1000]). Pre-fix `CreateCollection` ignored the param entirely and `UpdateCollection` rejected oversize batches with 400. Both now split the batch into chunks of size `window`, commit each chunk in its own transaction in commit order, and emit one `EntityTransactionResponse` element per committed chunk. Each chunk remains all-or-nothing internally, and chunks committed before any later failure stay durable.
  - Single-create endpoint `POST /api/entity/{format}/{entityName}/{modelVersion}` array-body path now chunks too. The handler auto-detects a JSON-array body and previously delegated the whole batch to `CreateEntityCollection` in one transaction, silently ignoring the advertised `transactionWindow` query param. It now applies the same chunking + per-chunk-array response shape as `CreateCollection`. Single-object (non-array) body behaviour is unchanged. Shared chunking primitive `Handler.runChunkedCreate` extracted so the wire contract lives in one place.
  - Wire format on partial-success: HTTP 200 with the per-chunk array, where the failed chunk appears as an `error` element carrying `{code, message, chunkIndex}` instead of `{transactionId, entityIds}`. The first-chunk-fail case (no durable progress) keeps the existing 4xx `application/problem+json` envelope. The `EntityTransactionResponse` schema is relaxed accordingly: `entityIds` is no longer required, and the optional `error` sub-object is declared.
- **`transactionTimeoutMillis` and `waitForConsistencyAfter` documented as Cloud-parity gaps** ([#227](https://github.com/Cyoda-platform/cyoda-go/issues/227)) — these query params on all five entity-mutation endpoints (single create, single update, single update-loopback, collection create, collection update) are advertised by the spec but not honored by the cyoda-go open-source storage plugins. Param descriptions now carry the same vendor-neutral "storage-engine-plugin dependent" caveat established in [#223](https://github.com/Cyoda-platform/cyoda-go/issues/223) for `asyncResult`/`crossoverToAsyncMs`. No Go code change; the params are still parsed-and-ignored.
- `GetOneEntity` now propagates the `transactionId` query parameter ([#150](https://github.com/Cyoda-platform/cyoda-go/issues/150)) — previously silently dropped, returning the latest entity instead of the at-tx snapshot. Bogus `transactionId` returns `ENTITY_NOT_FOUND@404` matching the dictionary contract (parity scenario `12_05` unblocked).
- `GetEntityChangesMetadata` now propagates `pointInTime` ([#152](https://github.com/Cyoda-platform/cyoda-go/issues/152)) — previously dropped; full history truncated to `timeOfChange ≤ pointInTime` as the dictionary requires.
- `messaging.GetMessage` content field — JSON-in-string defect (the original [#21](https://github.com/Cyoda-platform/cyoda-go/issues/21) confirmed defect for messaging).
- `messaging.NewMessage` — dead-code branch in `json.Compact` fallback removed; replaced with explicit invariant-broken 500.
- `audit.GetStateMachineFinishedEvent` — missing `microsTime` field added.
- `search.cancelAsyncSearch` 400 path — uses `WriteError` (proper Content-Type) instead of raw `WriteJSON`.
- `account` stub handlers — error code corrected to `NOT_IMPLEMENTED`.

#### Concurrency + tenant isolation
- **`tx.OpMu` locking discipline rolled out across `plugins/memory`** ([#176](https://github.com/Cyoda-platform/cyoda-go/issues/176)) — `Get`, `GetAll`, `Delete`, `DeleteAll`, `Exists`, `Count` now follow the `tx.OpMu.RLock()` + `defer tx.OpMu.RUnlock()` pattern that PR #153 established for `Save`/`CompareAndSave`. Six new race-conditional regression tests (one per method) green under `-race`.
- **`tx.OpMu` coverage gap on Savepoint/RollbackToSavepoint/Join** closed across the memory and sqlite plugins ([#199](https://github.com/Cyoda-platform/cyoda-go/issues/199) PR-A, PR-C1) and SPI v0.6.1 formalises the contract godoc + `.claude/rules/tx-state-locking.md` (PR-B).
- **Tenant-isolation hardening across plugin transaction-manager surfaces** ([#199](https://github.com/Cyoda-platform/cyoda-go/issues/199) PR-A, PR-C1, PR-C2) — every TM lifecycle method (`Commit`, `Rollback`, `Join`, `Savepoint`, `RollbackToSavepoint`, `ReleaseSavepoint`) on memory, sqlite, and postgres now verifies `uc.Tenant.ID == txState.tenantID` before any state mutation. Postgres was uniquely missing these checks despite RLS — RLS is row-level by design and does not extend to transaction-lifecycle commands.
- **Path-validation cache: single-node invalidation lost on schema-change** ([#174](https://github.com/Cyoda-platform/cyoda-go/issues/174)) — pre-fix the cache subscribed to the cluster broadcaster directly, so single-node deployments (where the broadcaster is nil) never received invalidations. Now wired through `modelcache.SubscribeLocal` so local mutations and gossip events alike reach the cache.
- **Path-validation cache: cross-tenant noisy-neighbor eviction** ([#175](https://github.com/Cyoda-platform/cyoda-go/issues/175)) — pre-fix a single global otter cache (10000 entries) allowed a flooding tenant to S3-FIFO-evict another tenant's entries. Restructured into per-`(tenant, ref)` bucket map of bounded otter caches. Cross-tenant flooding is contained to the attacker's own bucket.
- **Path-validation cache: bucket-map size cap with LRU eviction** ([#218](https://github.com/Cyoda-platform/cyoda-go/issues/218)) — bucket map now caps at 10000 buckets total with LRU eviction. Bounds memory under adversarial workloads (a tenant with model-creation privilege at scale).

#### Internal hygiene
- Pre-existing tag-list test stale entry (`CQL Execution Statistics` removed).
- Root `go.mod` / `go.sum` tidied after dependabot PR #180 bumped sqlite plugin deps without propagating to root (was breaking `Release smoke` and `per-module-hygiene` jobs on `main`).

### Refactored

- **`AppError` 4xx constructors** ([#140](https://github.com/Cyoda-platform/cyoda-go/issues/140)) — `Conflict()` and `RetryableConflict()` removed; replaced by `Operational(status, code, msg).AsRetryable()` chain. Wire shape unchanged at every existing call site; the change is purely API ergonomics.

### Process / Documentation

- ADR 0001 added: chose runtime validation via `kin-openapi` over compile-time strict typing (oapi-codegen strict-server, ogen, goa all evaluated).
- Conformance audit table at `docs/superpowers/audits/2026-04-29-openapi-conformance-audit.md` — one row per operationId, dispositioned with commit SHA. Carried forward as the starting point for future external-spec reconciliation work.
- Per-plugin tx-locking audit docs landed at `docs/audits/2026-05-{memory,sqlite,postgres}-plugin-tx-locking.md`.
- Issue [#194](https://github.com/Cyoda-platform/cyoda-go/issues/194) filed for the 22 stub-implemented IAM/OAuth/OIDC/account endpoints (deferred per the A+C policy of the OpenAPI conformance ADR).
- Issue [#200](https://github.com/Cyoda-platform/cyoda-go/issues/200) filed for SPI sentinel error types (rolled-back / closed / commit-in-progress) — deferred to v0.8.0.

### Versioning policy

`v0.6.x` is no longer maintained. No back-port branch exists. Consumers needing 0.6.x stability should pin to `v0.6.3`. If a concrete need emerges, open an issue and we'll consider branching `release/v0.6.x` from the `v0.6.3` tag.

---

## [0.6.3] — 2026-04-28 and earlier

For releases prior to 0.7.0, see the [Releases page](https://github.com/Cyoda-platform/cyoda-go/releases) and the git history. This is the first release with a maintained CHANGELOG.
