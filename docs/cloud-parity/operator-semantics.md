# Operator semantics — Cloud twin-alignment spec

cyoda-go leads this contract. Cloud aligns to the same evaluation rules.

This document defines what an operator means once a path is resolved: how an
operand is compared against a value, how null and absent values behave, and which
Cloud behaviours cyoda-go deliberately does not copy.

`path-grammar.md` defines how a path is written and what it addresses. The two
documents divide as follows: that one is the left side of a predicate, this one
is the operator and the right side.

## 1. Same-type, type-directed comparison

An operand is compared against the field's **declared type or types**. A field
may carry more than one across the entities of a model.

The operand is parsed against every declared type. A JSON number operand and a
numeric-looking string operand are treated identically. There is no cross-type
coincidental match: an operand that parses only as a string never matches a
numerically stored value, and a numeric operand never matches a stored string.

Numbers compare through arbitrary-precision integer and decimal types, not
`float64`. Comparison is correct beyond 2^53.

An operand that fits no declared type of a known field is `400
CONDITION_TYPE_MISMATCH`.

## 2. Null and absent never match

**A missing or JSON-`null` value never matches any binary operator, including a
negative one.** `NOT_EQUAL`, `NOT_CONTAINS`, the `INOT_*` family and every other
negated operator answer non-match on null rather than evaluating as "not the
positive result".

The two presence tests are the exception, because they ask about presence rather
than compare a value: `IS_NULL` matches a missing or null value, `NOT_NULL` does
not.

This is a deliberate divergence. Cloud's core-libs matchers guard `!positive` by
type-slot presence, so a slot that is present but null can satisfy a negative
condition under Cloud's model. cyoda-go's single-document model has no per-type
slot. The simpler rule — null never matches — is also what SQL, PostgreSQL JSONB
and CouchDB Mango do. cyoda-go adopts it.

`MATCHES_PATTERN` follows the same rule. It never matches null, and it never
throws.

## 3. Temporal comparison accepts coarse operands

`creationDate` and `lastUpdateTime` operands do not have to be full
offset-bearing RFC3339 instants. A coarser value — `"2024"`, `"2024-09"`, an
offset-less date-time — is accepted, upscaled through the temporal-resolution
graph, and compared as an instant.

Data fields compare temporally through the same graph. Six subtypes carry it:
`LocalDate`, `LocalDateTime`, `LocalTime`, `ZonedDateTime`, `Year`, `YearMonth`.
Upscale, downscale and the imprecise-`EQUALS` drop behave identically for a data
field and a meta field.

## 4. The operator set

Twenty-six operators exist. They split into three groups, and the split decides
what happens when a field has no declared type.

| Group | Operators | Needs a declared type |
|---|---|---|
| comparison and ordering | `EQUALS`, `NOT_EQUAL`, `GREATER_THAN`, `GREATER_OR_EQUAL`, `LESS_THAN`, `LESS_OR_EQUAL`, `BETWEEN`, `BETWEEN_INCLUSIVE` | yes |
| presence | `IS_NULL`, `NOT_NULL` | no |
| string and pattern | `CONTAINS`, `NOT_CONTAINS`, `STARTS_WITH`, `NOT_STARTS_WITH`, `ENDS_WITH`, `NOT_ENDS_WITH`, `LIKE`, `MATCHES_PATTERN`, and the case-insensitive family `IEQUALS`, `INOT_EQUAL`, `ICONTAINS`, `INOT_CONTAINS`, `ISTARTS_WITH`, `INOT_STARTS_WITH`, `IENDS_WITH`, `INOT_ENDS_WITH` | no |

The eight in the first group need a type slot to compare in. With no declared
type they answer non-match. The other eighteen never read a declared type and
keep evaluating.

The sixteen string and pattern operators stringify the **operand** only. A stored
value that is not textual is a non-match; it is never stringified to be compared.
The two presence tests compare nothing at all — they read whether the value is
present and non-null.

This is why a condition cannot be answered without the model schema. A mixed
condition does not degrade uniformly — it answers a short result set that looks
complete. `path-grammar.md` section 6 states the resulting rule.

An operator name outside this set is `400 INVALID_CONDITION`, on every surface
that carries a condition, workflow import included. It is not mapped onto a near
match: a misspelling routed to a pattern operator answers a different question and
returns the rows the caller meant to exclude.

## 5. Operand rules

Three operand shapes are refused, because each one otherwise fails silently:

| Operand | Rule |
|---|---|
| an object | refused — an object denotes no scalar any operator compares against |
| `BETWEEN` / `BETWEEN_INCLUSIVE` with other than two entries | refused — the operator needs exactly `[lo, hi]` |
| an uncompilable `LIKE` or `MATCHES_PATTERN` pattern | refused `400 INVALID_CONDITION` |

A `MATCHES_PATTERN` operand is checked **twice**, and both checks are required.

The raw operand is parsed first. Then the anchored form the evaluator actually
compiles is parsed. Neither check subsumes the other: the anchored form of `)|(`
is `\A(?:)|()\z`, which compiles and whose first branch matches every stored
value, so checking only the anchored form admits a pattern that matches
everything. Checking only the raw form admits patterns the anchoring then breaks.

## 6. Pattern dialects

`MATCHES_PATTERN` uses Go RE2: whole-string anchored, case-sensitive. Cloud uses
`java.util.regex` through `Pattern.matches`. The two dialects diverge on
backreferences and some lookaround forms. This divergence is accepted and is not
reconciled. `cyoda help predicates` states it for the caller.

`LIKE` is a glob dialect, not a regular expression. `like-glob-grammar.md` defines
it.

## 7. Pushdown narrows; it does not decide

A backend may translate part of a condition into its own query language. That is
an optimisation.

- The evaluator is the sole authority for match and non-match. Results do not
  differ across backends however tightly or loosely a backend narrows.
- A backend that narrows re-checks the candidates it selected, and its answer is
  final for that request — the engine does not re-check them again. So the
  re-check inside a backend and the evaluator the engine runs when nothing was
  pushed must be the same resolver. `path-grammar.md` section 10 states that
  requirement.
- Each pushed leaf is **exact** — the backend predicate matches the evaluator
  exactly — or a **sound superset**, which may over-select and requires the
  re-check. The fast path, which skips the re-check and pushes paging and
  grouping into the backend, runs only when every pushed leaf is exact. One
  non-exact leaf drops the whole plan to a full re-check, with paging applied
  after it and, for grouped stats, the streaming tally.
- A backend predicate must never under-select. A row a query drops is never
  re-examined, so over-selection is recoverable and under-selection is not.
- `LIKE` and `NOT_EQUAL` are evaluated by the evaluator only; no backend
  predicate is pushed for them.
- Numeric and temporal leaves push as sound supersets on both SQL backends.
  Neither compares with the evaluator's precision: SQLite's comparison is decided
  by storage class, and PostgreSQL binds the operand as a double. Neither is a
  defect to close — the re-check is what makes the answer exact.
- The memory backend pushes nothing. It has no narrowing step, so it is sound
  with nothing to re-check.

## 8. Not ported from Cloud

cyoda-go does not copy these Cloud behaviours:

- the two non-identical `BETWEEN` representations in Cloud's condition model;
- `BETWEEN`'s `double`-widening of its bounds, which loses precision — cyoda-go
  compares `BETWEEN` and `BETWEEN_INCLUSIVE` bounds with the same precise numeric
  type as every other comparison;
- the `BETWEEN` UUID-comparator inconsistency;
- `Matches` reporting null through a null-pointer exception — section 2 applies
  to `MATCHES_PATTERN` like every other binary operator.

`searchInStrings` is out of scope. Cloud can store a textual value in both its
typed slot and a separate `strings` slot. cyoda-go's single-classification model
has no per-type slot and cannot express "matches under either of two type
branches" this way.

## 9. Backend support

Memory, SQLite and PostgreSQL evaluate predicates identically. The commercial
backend implements the same rules. The cross-backend parity suite checks
predicate-evaluation consistency across every backend wired into it.
