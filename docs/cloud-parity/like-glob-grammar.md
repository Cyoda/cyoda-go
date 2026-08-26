# `LIKE` is a glob, not a regex — Cloud twin-alignment spec

This document is the contract Cyoda Cloud implements to stay aligned with
cyoda-go's `LIKE` operator. cyoda-go is the authoritative implementation.

`LIKE` used to be evaluated by translating the operand into a regular
expression and compiling that. It is now matched directly as a glob, by the
shared kernel in `cyoda-go-spi` (`like_pattern.go`). Any implementation that
keeps a translation of its own will diverge on the cases below — the
translation is the source of the divergence, so replacing it is the fix, not
patching it.

## Grammar

Exactly three characters are special. Nothing else is — in particular **no
regex metacharacter has any meaning in a `LIKE` operand**. `.`, `*`, `+`, `?`,
`[`, `(`, `|`, `^` and `$` are literal text.

- `%` — any sequence of characters, including empty.
- `_` — exactly one character.
- `\` — escapes the character that follows it to its literal form.

The match is **whole-string anchored** and **case-sensitive**. An operand
matches only if it consumes the entire stored value.

## The points a regex translation gets wrong

1. **`%` and `_` match a newline.** A translated `.`/`.*` does not match `\n`
   unless the translator sets the `s` (dot-all) flag, so a stored value
   containing a newline silently failed to match. `%` means *any* sequence and
   `_` means *any* one character, newlines included.
2. **`\` escapes ANY following character, not only `%`, `_` and `\`.** `\a`
   matches a literal `a`. A translator that passes an unrecognised escape
   through to the regex engine either changes its meaning (`\d`, `\b`, `\w`
   become regex classes) or fails to compile.
3. **A pattern ending in an unpaired `\` is invalid.** It has nothing to
   escape. The kernel rejects it with an error wrapping `spi.ErrInvalidPattern`
   rather than matching, guessing, or compiling a trailing literal backslash.
4. **Literal text is compared bytewise.** An operand carrying invalid UTF-8
   matches the byte-identical stored value; it must not be transcoded to
   U+FFFD. Otherwise `LIKE "\xff"` fails against a stored `"\xff"` while
   wrongly matching `"�"` — inconsistent with `EQUALS`/`CONTAINS` on the
   same pair.
5. **`_` advances by one rune, not one byte.** A multi-byte character is one
   `_`.

Runs of `%` collapse (`%%%%` behaves as `%`). That is a performance property,
not a semantic one, but it is what keeps the scan from going quadratic in the
number of wildcards.

## Conformance

`spitest` pins this grammar for every backend as
`Searcher/Pattern/LikeGrammar` (with `EscapedLetterIsLiteral`,
`EscapedPercentIsLiteral`, `DoubledEscapeIsOneBackslash`,
`AnyRunReachesNewline`, `OneCharReachesNewline`, `AnyRunMatchesEverything`,
`WholeStringAnchored`, `CaseSensitive`) and `Searcher/Pattern/MalformedLike`.
The in-tree memory, sqlite and postgres backends pass all of them.

A backend carrying its own `LIKE`→regex implementation fails these subtests. It
must adopt the kernel rather than align its translation: two implementations of
one grammar drift, which is the reason this contract exists.

## Validation

`ValidateLeafPattern` and `ValidateConditionPatterns` are exported from the SPI
so a boundary validator derives the same accept/reject set as the evaluator
instead of hand-rolling a bare compile. `MATCHES_PATTERN` requires a standalone
parse **and** an anchored compile — a pattern that parses bare but fails once
anchored is rejected at the boundary rather than at evaluation time.

Wiring cyoda-go's own boundary validator onto these exports — so a malformed
`LIKE` is rejected as `400` at the API boundary rather than failing during
evaluation — is tracked separately as cyoda-go#479, and is not yet in place.
