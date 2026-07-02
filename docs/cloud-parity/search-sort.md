# Search result sorting — Cloud twin-alignment spec

This document is the contract Cyoda Cloud implements to stay aligned with
cyoda-go's search result sorting feature. cyoda-go is the authoritative
implementation; the behaviour described here is derived directly from its design
spec and implemented code.

## 1. HTTP surface

### Endpoints

```
POST /api/entity/{entityName}/{modelVersion}/search
POST /api/entity/{entityName}/{modelVersion}/search/async
```

Both endpoints accept the same `sort` query parameter — repeatable, in
precedence order.

### Sort key grammar

```
sort = [@]path[:asc|desc]
```

- A **bare dotted path** (`price`, `address.city`) sorts by a scalar field in
  the entity data payload.
- An **`@`-prefixed name** (`@creationDate`) sorts by a meta field.
- Direction (`asc` or `desc`) is optional; it defaults to `asc`.
- Multiple `sort` parameters are accepted; their declaration order is sort
  precedence.

**Example:**

```
?sort=price:asc&sort=@creationDate:desc
```

### Sort key cap

The number of `sort` parameters per request is capped by
`CYODA_SEARCH_MAX_SORT_KEYS` (server default: `16`). Requests exceeding the cap
are rejected with `400 INVALID_FIELD_PATH`.

### Error table

| Code | Condition |
|---|---|
| 400 Bad Request — `INVALID_FIELD_PATH` | A `sort` path is absent from the locked model schema, refers to a non-scalar or array field, names an unknown meta field, exceeds the server's key cap, or has malformed grammar |

## 2. gRPC surface

### CloudEvent type

The `entitySearch` and `entitySnapshotSearch` unary RPCs accept an
`EntitySearchRequestJson` payload. Sort keys are passed as the `orderBy` array
field — structured, not a query-string grammar.

### `orderBy` element shape

```json
{
  "path":   "<dotted-path>",
  "source": "data" | "meta",
  "desc":   true | false
}
```

- `path` — required; a dotted scalar path (data) or a canonical meta field name
  (meta).
- `source` — `"data"` (default when absent) or `"meta"`.
- `desc` — `false` when absent (ascending).

Array element order is sort precedence.

### gRPC error representation

Sort-related failures surface as `Success: false`, `Error.Code: "CLIENT_ERROR"`,
`Error.Message: "INVALID_FIELD_PATH: ..."`. This follows the pre-existing
`CLIENT_ERROR` envelope convention — see entity-patch.md §4 for background.

## 3. Canonical ordering semantic

All backends (memory, sqlite, postgres, commercial) must produce **identical
ordering** for the same `sort` keys. Four canonical comparison classes exist;
the engine assigns the class from the field's declared schema type and passes it
to each backend via `OrderSpec.Kind`:

| Kind | Comparison | Schema types |
|---|---|---|
| `OrderText` | Byte order (BINARY / `COLLATE "C"` / `bytes.Compare`) | `string`, `uuid`, `localDate`, `localDateTime`, `localTime`, `zonedDateTime`, `year`, `yearMonth`, and derived types |
| `OrderNumeric` | IEEE-754 double | All numeric types (`integer`, `long`, `float`, `double`, `bigDecimal`, …) |
| `OrderBool` | `false < true` | `boolean` |
| `OrderTemporal` | Chronological (ms-floored epoch instant) | Engine meta date fields only (`creationDate`, `lastUpdateTime`) |

`OrderText` is the zero value; any backend implementation must treat an absent
`Kind` as `OrderText`.

## 4. Meta field allowlist

Only the following meta field names are accepted in sort keys:

| Name | Ordering class |
|---|---|
| `state` | `OrderText` |
| `creationDate` | `OrderTemporal` |
| `lastUpdateTime` | `OrderTemporal` |
| `transitionForLatestSave` | `OrderText` |
| `transactionId` | `OrderText` |
| `id` | `OrderText` |

Any other `@`-prefixed name returns `400 INVALID_FIELD_PATH`.

## 5. NULLS-LAST and the entity_id tiebreaker

Two rules apply to every sort, regardless of direction:

1. **NULLS-LAST** — absent or JSON-`null` values always sort after all present
   values, for both `asc` and `desc` directions.
2. **`entity_id` tiebreaker** — `entity_id` ascending is appended as the final
   sort key unless the last explicit key already targets `entity_id`. This makes
   result order deterministic across repeated calls and backends.

These rules are not configurable; Cloud must apply them unconditionally.

## 6. Backend support

Search result sorting is supported by memory, sqlite, and postgres backends.
The commercial backend must implement identical ordering semantics — the
cross-backend parity suite validates sort-key consistency across backends.
