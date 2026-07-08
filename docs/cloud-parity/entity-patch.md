# Entity PATCH contract — Cloud twin-alignment spec

This document is the contract Cyoda Cloud implements to stay aligned with
cyoda-go's entity partial-update (PATCH) feature. cyoda-go is the authoritative
implementation; the behaviour described here is derived directly from its design
spec and implemented code.

## 1. Merge semantics — RFC 7386 (JSON Merge Patch)

The request body is a **sparse JSON object** applied to the stored entity
payload using the RFC 7386 `MergePatch` algorithm:

- A key present in the patch with a **non-null value** overwrites that key in
  the stored payload.
- A key whose value is itself an **object** merges recursively.
- A key present in the patch with an explicit **`null` value** deletes that key
  from the stored payload.
- **Arrays** are replaced wholesale — there are no element-level operations.
- A **non-object** patch document (bare scalar, bare array) replaces the stored
  payload entirely, per the RFC.
- The **empty patch `{}`** is a valid no-op merge: the stored data is
  unchanged, but the request still commits a new transaction and fires the
  loopback or named transition. A patch that changes no data still advances
  workflow state.

The merge must operate on **number-preserving decoding** so that `int64` values
larger than 2^53 survive a merge without `float64` coercion.

### Strict validation

The **merged result** is validated strictly against the model schema. PATCH
**never** extends the model schema, regardless of the model's `ChangeLevel`
setting. This is a deliberate divergence from PUT (which may extend the schema
when `ChangeLevel` permits):

**Hard limitation:** a PATCH can never introduce a field the model schema does
not already allow — even when the model is in an extend-permitting `ChangeLevel`.
To add a genuinely new field, callers must use PUT (which extends), then PATCH
thereafter.

Validation is applied to the merged result, not to the patch body alone. A
`null`-deletion that removes a field the schema marks `required` fails
validation; if the schema does not mark the field `required`, the deletion
succeeds and the field is removed.

### Merge-then-processor ordering

When a named transition is supplied, **processors run after the merge** and may
overwrite fields the patch set. The observable result of a PATCH with a
mutating transition is not guaranteed to retain the patched values. This is
identical to PUT-plus-transition.

## 2. The `If-Match` precondition — three states

`If-Match` is **required to be present in some form** on every PATCH request.
The precondition token is the entity's **`transactionId`** — the `transactionId`
from the caller's last read of this entity (stored in `_meta.transaction_id`).
This is never referred to as "version".

| `If-Match` value | Meaning | Outcome |
|---|---|---|
| `"<transactionId>"` | Conditional: the stored entity must still carry this transactionId as its merge base | **412 Precondition Failed** if the stored transactionId differs |
| `*` | Unconditional opt-out: merge onto the current state, whatever it is | Proceeds (entity must exist); translated internally to a no-CAS save |
| *absent* | — | **428 Precondition Required**; the error body is educational (see below) |

### The 428 educational body

The 428 response body must explain the two valid choices to the caller. This is
the message cyoda-go returns:

> missing If-Match: send `If-Match: <transactionId>` from your last GET of this
> entity to patch safely, or `If-Match: *` to explicitly accept last-writer-wins

The target audience is naive or LLM-generated clients that will not send
`If-Match` unprompted. The body teaches the GET-first discipline.

### `If-Match: *` mechanism

`*` is **not** passed to the conditional-save comparison as the literal string
`"*"`. Instead it is translated to an unconditional save (no CAS), with entity
existence guaranteed by the in-transaction base read. `If-Match: *` means "save
unconditionally, but the entity must exist."

This is a deliberate divergence from PUT, where a missing `If-Match` means
unconditional replace. PATCH treats a missing `If-Match` as 428; the divergence
is justified by merge being base-relative.

## 3. HTTP surface

### Endpoints

```
PATCH /api/entity/{format}/{entityId}
PATCH /api/entity/{format}/{entityId}/{transition}
```

The first form fires the loopback. The second form carries a named transition.
Both are parallel to the existing PUT endpoints, which are unchanged.

The same query parameters PUT accepts (`transactionTimeoutMillis`,
`waitForConsistencyAfter`) apply unchanged. The response shape is the existing
`EntityTransactionResponse`.

### Format

`{format}` **must be `JSON`**. Merge Patch is JSON-only by RFC 7386. Supplying
`XML` returns **415 Unsupported Media Type**. The `{format}` segment is retained
for structural symmetry with the PUT routes; only `JSON` is accepted for PATCH.

### Patch dialect — `Content-Type`

| `Content-Type` | Dialect | Behaviour |
|---|---|---|
| `application/merge-patch+json` | RFC 7386 JSON Merge Patch | Implemented |
| `application/json-patch+json` | RFC 6902 JSON Patch | Scaffolded — returns **501 Not Implemented** |
| anything else | — | **415 Unsupported Media Type** |

### Error table

| Code | Condition |
|---|---|
| 404 Not Found | Entity does not exist (including soft-deleted) |
| 409 Conflict (retryable) | A concurrent writer committed between the in-transaction base read and this save (read-set conflict at commit). Possible even under `If-Match: *`; the caller may retry. |
| 412 Precondition Failed | `If-Match: "<transactionId>"` supplied and the stored transactionId differs. Error code: `ENTITY_MODIFIED` ("entity has been modified since last read"). |
| 415 Unsupported Media Type | `XML` format, or a `Content-Type` other than the two recognised patch media types |
| 428 Precondition Required | No `If-Match` header present |
| 501 Not Implemented | `Content-Type: application/json-patch+json` |
| 4xx (domain) | The merged result fails strict model-schema validation; full domain detail plus error code |

### 409 vs 412

**412** is the *up-front* conditional rejection: the caller's `If-Match`
transactionId no longer matches the stored one at the point of the conditional
save. The stored entity moved before the merge began.

**409** is the *commit-time* read-set conflict: the transaction manager detects
that a concurrent writer touched the base entity after the in-transaction
snapshot but before commit. 409 is retryable and can occur even under
`If-Match: *`.

### Error precedence

Request-shape validation runs before resource-state checks, so entity existence
is never leaked to a malformed or unconditional request:

1. Media type / format resolution — unrecognised `Content-Type` or `XML` ⇒ **415**;
   recognised-but-unimplemented `application/json-patch+json` ⇒ **501**
2. `If-Match` presence — absent ⇒ **428**
3. Entity lookup — not found or soft-deleted ⇒ **404**
4. transactionId compare — mismatch ⇒ **412**
5. Merge + strict schema validation — invalid merged result ⇒ **4xx domain**
6. Commit — read-set conflict ⇒ **409** (only possible at this stage)

Example: a request with `XML` format and no `If-Match` returns **415**, not 428.

## 4. gRPC surface

### CloudEvent type

The `entityManage` unary RPC dispatches on the CloudEvent `type` string. The
new type for entity patch is **`EntityPatchRequest`**.

No `.proto` change is required. The type string is a new constant alongside the
existing `EntityUpdateRequest`.

### Payload fields

```
EntityPatchRequest {
    entityId    string        // required
    patch       bytes         // the merge-patch body (JSON)
    transition  string        // optional; empty for loopback
    ifMatch     string        // "<transactionId>" | "*"; required (absence → CLIENT_ERROR)
    patchFormat PatchFormat   // enum; see below
}
```

### `PatchFormat` enum

| Value | Meaning |
|---|---|
| `MERGE_PATCH` | RFC 7386 JSON Merge Patch (implemented) |
| `JSON_PATCH` | RFC 6902 JSON Patch (not implemented) |

This is the gRPC mirror of the HTTP `Content-Type` dialect selector. The same
501 / not-implemented behaviour applies when `JSON_PATCH` is supplied.

### gRPC error representation — important

Domain failures over gRPC are **not** surfaced as gRPC `status` codes. The
`EntityManage` RPC always returns an `EntityTransactionResponse` envelope.
On failure: `Success: false`, `Error.Code: "CLIENT_ERROR"`, and the real
operational code inside the `Error.Message` string as `"<CODE>: detail"`.

Examples of how PATCH-specific errors appear in the envelope:

| Condition | `Error.Code` | `Error.Message` (prefix) |
|---|---|---|
| `patchFormat: JSON_PATCH` | `CLIENT_ERROR` | `NOT_IMPLEMENTED: ...` |
| `ifMatch` absent or empty | `CLIENT_ERROR` | `PRECONDITION_REQUIRED: ...` |
| transactionId mismatch | `CLIENT_ERROR` | `ENTITY_MODIFIED: ...` |
| schema validation failure | `CLIENT_ERROR` | `VALIDATION_FAILED: ...` |

This `CLIENT_ERROR` flattening is a **pre-existing limitation** of the gRPC
envelope shared by all operations, not something specific to PATCH. The HTTP
status codes are canonical; the gRPC envelope's representation is the current
mirror. Surfacing distinct, machine-readable gRPC error identity per operation
code is a tracked future gRPC-error improvement.

### `ifMatch: "*"` on gRPC

The same translation applies: `"*"` is mapped to an unconditional (no-CAS) save
with entity-existence guaranteed by the in-transaction base read. It is **not**
compared literally against the stored transactionId.

## 5. Atomicity guarantee

The base read, merge, validation, and conditional save all occur within a
**single transaction**. Every storage backend re-validates the transaction's
read-set at commit. If a concurrent writer touches the base entity after the
in-transaction snapshot but before commit, the backend raises a conflict surfaced
as the documented **409 Conflict** — not silent data corruption. This guarantee
holds even under `If-Match: *`.
