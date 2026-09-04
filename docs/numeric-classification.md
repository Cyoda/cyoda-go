# Numeric Classification in cyoda-go

This document describes cyoda-go's numeric classification policy for
data ingestion. The raw algorithm is ported from Cyoda Cloud's
classifier; cyoda-go deliberately diverges in two ways documented
below.

**Authoritative references:**
- Algorithm source: [`numeric-type-classification-analysis.md`](numeric-type-classification-analysis.md)
- Design spec: [`superpowers/specs/2026-04-21-data-ingestion-qa-subproject-a1-design.md`](superpowers/specs/2026-04-21-data-ingestion-qa-subproject-a1-design.md)

## DataType enum

cyoda-go's `DataType` enum carries the following numeric types:

**Integer family** (widening order): `Integer ⊂ Long ⊂ BigInteger ⊂ UnboundInteger`.

**Decimal family** (widening order): `Double ⊂ BigDecimal ⊂ UnboundDecimal`.

Cyoda Cloud additionally has `Byte`, `Short`, and `Float` — **dropped in cyoda-go**. Values Cyoda Cloud classifies as BYTE or SHORT classify as INTEGER in cyoda-go; values Cyoda Cloud classifies as FLOAT classify as DOUBLE.

## Classification algorithm

Every ingested numeric value flows through:

1. `json.NewDecoder(r).UseNumber().Decode(...)` preserves the source literal as a `json.Number` string.
2. `ParseDecimal(s)` parses into `(unscaled *big.Int, scale int32)`.
3. `StripTrailingZeros()` normalizes.
4. **Value-based branch:** `scale <= 0` (whole number, possibly after stripping) → `ClassifyInteger(unscaled × 10^(-scale))`. Otherwise → `ClassifyDecimal(d)`.

### Integer classification

| Magnitude | DataType |
|---|---|
| `[-2^31, 2^31-1]` | `Integer` |
| `[-2^63, 2^63-1] \ Integer range` | `Long` |
| `[-2^127, 2^127-1] \ Long range` | `BigInteger` |
| beyond | `UnboundInteger` |

### Decimal classification

After `StripTrailingZeros`, evaluated in order:

- `precision <= 15 AND |scale| <= 292` → `Double`.
- `precision <= 38 AND (precision - scale) <= 20 AND scale <= 18` → `BigDecimal` (definite fit).
- `precision <= 39 AND (precision - scale) <= 21 AND scale <= 18 AND SetScale(18).Unscaled().IsInt128()` → `BigDecimal` (loose fit).
- Otherwise → `UnboundDecimal`.

The `BigDecimal` boundary is Trino-compatible by design: BigDecimal values fit Trino's fixed-scale Int128 encoding, so downstream Trino-backed storage can index them directly.

## Collapse rule (`CollapseNumeric`)

A field's `TypeSet` always collapses its numeric members to exactly one `DataType`. Non-numeric members (String, Boolean, etc.) are preserved unchanged; `NULL` is dropped when any concrete type is present.

**Same-family collapse:** keep the widest rank observed.

**Cross-family collapse** uses the widening lattice (DataType.kt:240-287). The result is the narrowest type in the lattice that every input widens to. Because several cross-family pairs have no direct widening edge, the result is often `UnboundDecimal`:

| Input set | Result | Reason |
|---|---|---|
| `{Integer, Double}` | `Double` | `Integer → Double` is a direct lattice edge |
| `{Integer, BigDecimal}` | `BigDecimal` | `Integer → BigDecimal` direct |
| `{Integer, UnboundDecimal}` | `UnboundDecimal` | direct |
| `{Long, Double}` | `UnboundDecimal` | `Long → Double` blocked (precision loss); intersect = `{UnboundDecimal}` |
| `{Long, BigDecimal}` | `BigDecimal` | `Long → BigDecimal` direct |
| `{Long, UnboundDecimal}` | `UnboundDecimal` | direct |
| `{BigInteger, Double}` | `UnboundDecimal` | no direct edge; intersect = `{UnboundDecimal}` |
| `{BigInteger, BigDecimal}` | `UnboundDecimal` | `BigInteger → BigDecimal` blocked; intersect = `{UnboundDecimal}` |
| `{BigInteger, UnboundDecimal}` | `UnboundDecimal` | direct |
| `{UnboundInteger, any decimal}` | `UnboundDecimal` | `UnboundInteger → UnboundDecimal` is its only outgoing edge |
| `{Double, BigDecimal}` | `UnboundDecimal` | `Double → BigDecimal` blocked (scale mismatch); intersect = `{UnboundDecimal}` |

This matches Cyoda Cloud's `findCommonDataType` (`DataType.kt:293-309`) restricted to numeric inputs. Cyoda additionally falls back to `STRING` for incompatible non-numeric pairs; cyoda-go does not — STRING fallback is a Cyoda-internal plumbing choice (every leaf is also stored as a string ValueMap for search indexing), not semantic behavior cyoda-go replicates. For cross-kind polymorphic cases, cyoda-go keeps the TypeSet as a union (tracked under A.3).

## Validation compatibility

`IsAssignableTo(dataT, schemaT)` realizes the widening lattice (per Cyoda Cloud `DataType.kt:239-272`, minus dropped types). Notable asymmetries:

- `Integer → Double` — **allowed**. Integer's 2^31 range fits Double's 53-bit mantissa.
- `Long → Double` — **blocked**. Long's 2^63 exceeds Double's 53-bit mantissa; precision loss.
- `Integer → BigDecimal` — allowed. `Long → BigDecimal` — allowed.
- `Double → BigDecimal` — **blocked**. Envelopes differ.
- Any integer → any decimal family reachable via the above — allowed.
- Any decimal → integer family — **blocked**.
- `NULL → anything` — allowed.

Under strict validation (`ChangeLevel = ""`), a decimal value against an `Integer` schema is rejected. Under extension (`ChangeLevel = Type` or higher), the same input triggers schema widening via the collapse rule — the field becomes `BigDecimal`.

The change-level gate asks the same question before charging for a change: a value whose classified type is assignable to one the leaf already declares leaves the admitted value space untouched, so it consumes no `ChangeLevel` permission. A whole number in the `Integer` range against a `Double` schema is the everyday case — `Integer → Double` is allowed, so the write is accepted at every level including `ArrayLength`. The asymmetries above bound this precisely: a whole number past 2³¹ classifies `Long`, `Long → Double` is blocked, and that write is a genuine type change collapsing the leaf to `UnboundDecimal`.

## Intentional divergences from Cyoda Cloud

1. **No polymorphism within numerics.** Cyoda Cloud retains polymorphic numeric sets (e.g., `{FLOAT, DOUBLE, BIG_DECIMAL}`) and collapses only at read time via `findCommonDataType`, which falls back to `STRING` when no common type exists — silently corrupting numeric data. cyoda-go always stores the collapsed numeric type; cross-family mixing promotes to BigDecimal/UnboundDecimal without any STRING fallback. Bug fix, not a neutral divergence.

2. **Value-based integer/decimal split.** Cyoda Cloud routes on Jackson node kind (`IntNode` vs `DecimalNode`), which is a leaky abstraction over JSON grammar: `"1.0"` and `"1e0"` both denote the integer 1 but take different classifier branches. cyoda-go classifies by value — any whole-number literal classifies via `ClassifyInteger` regardless of source syntax.
