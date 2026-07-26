# Search predicate semantics — Cloud twin-alignment spec

This document is the contract Cyoda Cloud implements to stay aligned with
cyoda-go's search/criteria predicate semantics. cyoda-go is the authoritative
implementation; the behaviour described here is derived directly from its
design spec and implemented code. It also records the points where cyoda-go
**deliberately diverges** from Cyoda Cloud's own implementation — cyoda-go
leads the contract, so these are calls this document makes, not gaps to close.

## 1. Same-type, type-directed comparison

A predicate's operand is compared against the target field's **declared
type(s)** (a leaf may carry more than one across observed entities). The
operand is parsed against every declared type; a JSON number operand and a
numeric-looking string operand are treated identically. There is no
cross-type coincidental match — an operand that only parses as a string never
matches a numerically-stored value, and vice versa.

**Precise numerics.** Numbers compare via arbitrary-precision integer/decimal
types, not `float64` — correct beyond 2^53. This supersedes cyoda-go's prior
`float64`-based coercion.

## 2. Negative operators on absent/null — non-match (deliberate divergence)

A missing or JSON-`null` leaf **never matches any binary operator, including
negatives**: `NOT_EQUAL`, `NOT_CONTAINS`, `INOT_*`, and every other negated
operator null-guard to non-match rather than evaluating as `!positive`.

Cloud's core-libs matchers (`com.cyoda.core.conditions.NotEquals`,
`NotContains`) are `!positive` guarded only by **type-slot presence** — a
value-present-but-null slot can satisfy a negative condition under Cloud's
model. cyoda-go's single-document model has no per-type slot concept, and the
simpler rule — "null never matches, full stop" — is also what SQL, Postgres
JSONB, and CouchDB Mango do. cyoda-go adopts that rule rather than Cloud's
`!positive` twist. (MongoDB is the one engine that differs from both.) This is
a deliberate, principled divergence, not an oversight.

## 3. Mixed object-or-scalar field searchability

A JSON path observed as **both** an object (in some entities) and a bare
scalar (in others) is searchable at that path via its scalar type — a scalar
operand matches the scalar-valued entities; the object-valued entities remain
reachable through their child leaf sub-paths.

A **pure container** path — structural, with child fields, but never observed
holding a bare scalar — is **not** directly searchable: a scalar operand
against it returns `400 INVALID_FIELD_PATH` (navigate to a leaf sub-path
instead). Unary presence tests (`IS_NULL`/`NOT_NULL`) are exempt and remain
valid on a container path.

## 4. Meta-temporal accepts coarse operands (upscale)

`creationDate`/`lastUpdateTime` comparison/range operands are no longer
required to be full offset-bearing RFC3339 instants: a coarser value (e.g.
`"2024"`, `"2024-09"`, an offset-less date-time) is accepted and **upscaled**
through the same temporal-resolution graph used for data fields, then compared
as an instant. This relaxes the previous offset-mandatory rule for meta
fields. Data-field temporal comparison is lit up for the first time — six
subtypes (`LocalDate`, `LocalDateTime`, `LocalTime`, `ZonedDateTime`, `Year`,
`YearMonth`) with the same resolution-graph upscale/downscale and
imprecise-`EQUALS`-drop behaviour as meta fields.

## 5. Cloud quirks deliberately NOT replicated

cyoda-go does the principled thing instead of porting these known Cloud
behaviours:

- The two non-identical `BETWEEN` representations in Cloud's condition model.
- `BETWEEN`'s `double`-widening of bounds (precision loss) — cyoda-go compares
  `BETWEEN`/`BETWEEN_INCLUSIVE` bounds with the same precise numeric type as
  every other comparison.
- The `BETWEEN` UUID-comparator inconsistency.
- `Matches` null-via-NPE — cyoda-go's null/absent uniformity (§2) applies to
  `MATCHES_PATTERN` like every other binary operator; it never matches null,
  and never throws.

## 6. Accepted bounded divergence — regex dialect

`MATCHES_PATTERN` uses Go RE2 (whole-string anchored, case-sensitive); Cloud
uses `java.util.regex` (`Pattern.matches`). RE2 and the Java dialect diverge on
some constructs (backreferences, some lookaround forms). This divergence is
accepted and documented (`cyoda help predicates`) — not reconciled.

## 7. Out of scope — `searchInStrings` dual-slot

Cloud can optionally store a textual value in both its typed slot and a
separate `strings` slot (`alsoSaveInStrings`/`searchInStrings`), both
default-off and off on production paths. cyoda-go's single-classification
model has no per-type slot concept and cannot express "matches under either
of two type branches" this way — this capability is not ported.

## 8. Pushdown is a narrowing optimization, not a semantics source

SQL pushdown (sqlite/postgres) is **best-effort sound narrowing**: the kernel
re-checks every candidate and is the sole authority for match/no-match, so
results never diverge across backends regardless of how tightly (or loosely)
a backend's SQL translation narrows the candidate set.

- Each pushed leaf is classified **EXACT** (SQL predicate matches the kernel
  bit-for-bit) or **SOUND-SUPERSET** (may over-select; kernel re-check
  required). The fast path — skipping the Go re-check, pushing
  `LIMIT`/`OFFSET`, pushing grouped-stats `GROUP BY` — is used only when every
  pushed leaf is EXACT; any non-EXACT leaf falls back to full residual
  filtering with Go-side paging (and, for grouped stats, the existing
  streaming-tally path).
- `LIKE` and `NOT_EQUAL` are currently **residual-only** (the kernel evaluates
  them directly; no SQL predicate is pushed) — sound by construction.
- Numeric and temporal leaves are pushed as **SOUND-SUPERSET**: the SQL
  predicate narrows but the kernel re-checks. sqlite has no arbitrary-precision
  numeric type, so it is **permanently** sound-superset for numeric leaves
  regardless of future tightening.
- The memory backend re-checks every candidate unconditionally and is
  soundness-safe by construction.

## 9. Backend support

Search predicate evaluation is supported identically by memory, sqlite, and
postgres. The commercial backend must implement the same type-directed
semantics — the cross-backend parity suite validates predicate-evaluation
consistency across backends.
