---
topic: predicates
title: "predicates — search & criteria condition semantics"
stability: stable
see_also:
  - search
  - workflows
  - errors.CONDITION_TYPE_MISMATCH
  - errors.INVALID_FIELD_PATH
  - errors.INVALID_CONDITION
---

# predicates

## NAME

predicates — operator catalog and evaluation semantics for the `Condition` DSL used by search (`cyoda help search`) and workflow/transition criteria (`cyoda help workflows`). Both consume the same kernel, so everything here applies identically to both.

## OPERATORS

**Unary** (test presence, no operand type constraint):

- `IS_NULL` — the leaf is absent or JSON `null`.
- `NOT_NULL` — the leaf is present and non-null.

**Range** (operand is a two-element `[low, high]` array):

- `BETWEEN` — exclusive bounds.
- `BETWEEN_INCLUSIVE` — inclusive bounds.

**Binary — comparison** (same-type `compareTo`):

- `EQUALS`, `NOT_EQUAL`, `GREATER_THAN`, `GREATER_OR_EQUAL`, `LESS_THAN`, `LESS_OR_EQUAL`.
- `IEQUALS` (case-insensitive equals), `INOT_EQUAL` (case-insensitive not-equal).

**Binary — string** (case-sensitive; `I*`/`INOT_*` fold both sides):

- `CONTAINS`, `STARTS_WITH`, `ENDS_WITH`, and their `NOT_*` inverses.
- `ICONTAINS`/`INOT_CONTAINS`, `ISTARTS_WITH`/`INOT_STARTS_WITH`, `IENDS_WITH`/`INOT_ENDS_WITH`.

**Binary — pattern:**

- `LIKE` — anchored glob, see LIKE GRAMMAR below.
- `MATCHES_PATTERN` — anchored regular expression, see MATCHES_PATTERN below.

**Not supported:** `IS_CHANGED`/`IS_UNCHANGED` are change-generation operators, not search predicates — cyoda-go does not implement them.

## TYPE-DIRECTED COMPARISON

A condition's operand is compared against the target field's **declared type(s)** (a leaf may carry more than one, e.g. a field seen as both an integer and a string across entities). The operand is treated as a string and parse-tested against every declared type — a numeric-looking string operand (`"30"`) and a JSON number operand (`30`) are parsed identically and are **treated the same**. There is no cross-type coincidental matching: an operand that parses as a number only compares against numerically-stored values; an operand that parses only as a string compares against string-stored values. Numbers compare precisely (arbitrary-precision, not `float64` — correct beyond 2^53). Comparing against `MATCHES_PATTERN`/`LIKE` and string ops evaluate against textual stored values only; a string op against a non-textual stored value is a non-match, not an error.

## NULL SEMANTICS

A missing (absent) or JSON-`null` leaf **never matches any binary operator — including negatives**. `NOT_EQUAL`, `NOT_CONTAINS`, `INOT_*`, and every other negated op are **null-guarded to non-match**, not `!positive` — a null/absent field does not satisfy a negative condition just because it fails the positive one. `IS_NULL` / `NOT_NULL` are the only operators that test presence directly.

## LIKE GRAMMAR

`LIKE` compiles its operand to an anchored regular expression:

- `%` — matches any sequence of characters (including empty).
- `_` — matches exactly one character.
- `\` — escapes a following `%`, `_`, or `\` to its literal form.
- The match is **whole-string anchored** (the entire stored value must match, not a substring) and **case-sensitive**.

## MATCHES_PATTERN

`MATCHES_PATTERN` compiles the operand as a Go RE2 regular expression, whole-string anchored (equivalent to Java's `Pattern.matches`), case-sensitive. RE2 and the Java regex dialect diverge on some constructs (e.g. backreferences, some lookaround); this is an accepted, bounded divergence — not reconciled.

## VALIDATION

Validation is **parse-based**, evaluated at request time against the target model:

- `400 CONDITION_TYPE_MISMATCH` — the operand parses into **none** of the field's declared types (comparison and range operators only; string operators and unary presence tests carry no operand-type constraint and are always accepted).
- `400 INVALID_FIELD_PATH` — the field path is unknown to the model, **or** it names a pure-container (object) path: a scalar operator cannot compare against structure — navigate to a scalar leaf sub-path instead. (A path observed as both an object and a scalar across entities remains searchable via its scalar type — see `cyoda help search`.) `IS_NULL`/`NOT_NULL` are exempt from the container-path rejection since they test presence, not a value.
- `400 INVALID_CONDITION` — the operand is `null` on a binary/range operator, a range operator's value is not a two-element array, or the operand is an object/complex value.

There is no operator-versus-field-type rejection: `CONTAINS` on a numeric field or `GREATER_THAN` on a boolean field are accepted requests — they parse and simply evaluate to a (non-)match.

## SEE ALSO

- search
- workflows
- errors.CONDITION_TYPE_MISMATCH
- errors.INVALID_FIELD_PATH
- errors.INVALID_CONDITION
