# Entity Partial-Update (PATCH / RFC 7386 Merge Patch) — Design

- **Issue:** Cyoda/cyoda-go#341
- **Target release:** v0.8.2 (additive, non-breaking)
- **Status:** Approved design, pre-implementation
- **Date:** 2026-06-24

## 1. Motivation

Many cyoda-go applications — frequently LLM-generated — incorrectly assume an
entity update only needs the fields that changed. They call the update endpoint
with a transition and either no body, an empty body, or a body containing only a
few fields. The current update endpoints
(`PUT /api/entity/{format}/{entityId}[/{transition}]`) have **wholesale-replace**
semantics: `updated.Data` is set directly to the request body and the stored
payload is loaded but never merged. Any field absent from the request is silently
destroyed.

There is no partial-update / patch capability today. This design adds one as a
first-class, additive feature. Existing `PUT` replace semantics are **untouched**.

## 2. Framing: cyoda-go leads the contract

cyoda-go now **defines** the API/integration contract; Cyoda Cloud follows and
mirrors it. This reverses the historical "digital twin with fidelity *to* Cloud"
framing. Two consequences for this work:

1. The patch contract is published in a Cloud-facing form precise enough for Cloud
   to implement the twin-alignment — under a new `docs/cloud-parity/` folder.
2. Fidelity-direction language across the repo is updated (surgically) to reflect
   that cyoda-go defines the contract and Cloud aligns.

There is no shared ticketing system across the two streams yet; `docs/cloud-parity/`
is the coordination surface in the interim.

## 3. Scope

**In scope (this issue):** single-entity patch, across **both** the HTTP API and
the gRPC layer.

**Out of scope (deferred, mechanical follow-ups):**
- Collection/batch patch (`PATCH /api/entity/{format}` + streaming gRPC variant).
- XML patch (Merge Patch is JSON-only by RFC 7386).
- RFC 6902 (JSON Patch) *implementation* — scaffolded only (see §6.2).

## 4. Merge semantics — RFC 7386 (JSON Merge Patch)

The request body is a sparse JSON object applied to the **stored** entity payload:

- A key present in the patch with a non-null value **overwrites** the target key.
- A key whose value is itself an object **merges recursively**.
- A key present in the patch with an explicit `null` value **deletes** that key
  from the target.
- Arrays are **replaced wholesale** — there are no element-level operations.
- A non-object patch document (e.g. a bare scalar or array) **replaces** the target
  entirely, per the RFC.
- The empty patch `{}` is a valid **no-op merge**: the data is unchanged, but the
  request is still a normal update — it commits a new transaction and fires the
  loopback (or named transition). "A patch that changes no data still advances
  workflow state" is intentional and consistent with PUT-with-empty-body today.

The implementation must be a faithful realisation of RFC 7386 (the recursive
`MergePatch` algorithm), so Cloud has a *specification* to match, not an
observed behaviour to reverse-engineer. The algorithm is small enough to implement
directly; the RFC's appendix test cases become unit tests (§10). The merge must
operate on number-preserving decoding (`json.Number`, as `decodeJSONPreservingNumbers`
does for update) so large `int64` values survive a merge without `float64`
coercion.

## 5. The "no stale premise" precondition model

A merge is applied relative to a base. A caller computes the delta against some
state it read earlier; if the stored entity has moved underneath it, merging the
delta onto the *new* current state can encode a stale premise (the classic
lost-update problem). PATCH is therefore inherently more dangerous than a full
replace, and we make the *choice* explicit rather than letting callers blunder
into a blind patch.

**The precondition token is the entity's `transactionId`** — the `transactionId`
of the caller's last read, exactly as the existing `PUT` `If-Match` uses (the
backends' `CompareAndSave` compares the stored `_meta.transaction_id`, not
`EntityMeta.Version`; the `crud.md` help already documents `If-Match` as the
"transaction ID of last read"). The contract text says **transactionId**, never
"version", to avoid pointing Cloud at the wrong field.

`If-Match` is **required to be present in some form** on every PATCH. Three states:

| `If-Match` value | Meaning | Outcome |
| --- | --- | --- |
| `"<transactionId>"` | Conditional: the merge base must still be this transactionId | **412 Precondition Failed** if the stored transactionId differs |
| `*` | Unconditional opt-out: "I am not pinning a transactionId; merge onto current" | Proceeds (entity must exist) |
| *absent* | — | **428 Precondition Required**; the error body explains the two valid choices |

`*` is the RFC 7232 wildcard ("any current representation"); folding the opt-out
into the same field that carries the token means the two states can never
contradict each other. 428 is RFC 6585, which exists precisely to "prevent the
lost update problem" by forcing requests to be conditional.

**`*` translation (mechanism).** `*` is *not* passed down to `CompareAndSave` as
the literal string `"*"` — that would be compared against the stored
`transaction_id` and never match, yielding a spurious 412. Instead `*` is
translated to **"no CAS"** (the empty-`IfMatch` path that `UpdateEntity` already
takes for an unconditional save), with entity existence guaranteed by the in-transaction
base `Get` (a missing entity is a 404 before any save). So `*` = "save
unconditionally, but the entity must exist."

This is a **deliberate divergence from `PUT`**, where a missing `If-Match` means
"unconditional replace." PATCH treats a missing `If-Match` as 428. The divergence
is justified by patch being base-relative, and is documented as such.

The token reuses the existing optimistic-concurrency mechanism (the
`transactionId`-based `If-Match` already implemented for `PUT`); no new concurrency
primitive is introduced.

## 6. HTTP contract

### 6.1 Endpoints

New verb, parallel to the existing `PUT` endpoints (which are unchanged):

- `PATCH /api/entity/{format}/{entityId}` — loopback (no named transition)
- `PATCH /api/entity/{format}/{entityId}/{transition}` — patch carrying a named transition

Transition stays orthogonal to the patch body, exactly as with update today
(loopback vs `ManualTransition`). The same query parameters the `PUT` endpoints
accept (`transactionTimeoutMillis`, `waitForConsistencyAfter`) apply unchanged, and
the response shape is the existing `EntityTransactionResponse`.

### 6.2 Patch dialect via `Content-Type`

| `Content-Type` | Dialect | Behaviour |
| --- | --- | --- |
| `application/merge-patch+json` | RFC 7386 | Implemented |
| `application/json-patch+json` | RFC 6902 | Scaffolded — recognised and routed, returns **501 Not Implemented** |
| anything else | — | **415 Unsupported Media Type** |

### 6.3 Format

`{format}` **must be `JSON`.** Merge Patch is JSON-only by RFC 7386; `XML` ⇒
**415 Unsupported Media Type**. The `{format}` path segment is **retained for
structural symmetry with the `PUT` routes** (which carry it); only `JSON` is
accepted, so the segment is effectively fixed for PATCH. (Decision: kept for
symmetry over the format-less `GET /api/entity/{entityId}` shape.)

### 6.4 HTTP error model

| Code | Condition |
| --- | --- |
| 404 Not Found | Entity does not exist (covers soft-deleted — `Get` filters `NOT deleted`) |
| 409 Conflict (retryable) | A concurrent writer committed between the in-tx base read and this save (read-set conflict at commit). Possible even under `If-Match: *`; caller may retry. |
| 412 Precondition Failed | `If-Match: "<transactionId>"` supplied and the stored transactionId differs |
| 415 Unsupported Media Type | `XML` format, or a `Content-Type` other than the two patch media types |
| 428 Precondition Required | No `If-Match` header present |
| 501 Not Implemented | `Content-Type: application/json-patch+json` (RFC 6902 scaffold) |
| 4xx (domain) | The **merged result** fails strict model-schema validation (full domain detail + error code) |

**409 vs 412.** 412 is the *up-front* conditional rejection when the caller's
`If-Match` transactionId no longer matches at `CompareAndSave`. 409 is the
*commit-time* read-set conflict the transaction manager raises when a concurrent
writer touches the base entity after our in-tx snapshot but before commit — the
mechanism that makes the single-transaction guarantee (§8) hold even for
`If-Match: *`. Both are legitimate, documented outcomes; 409 is retryable.

**Precedence** (request-shape validation before resource state, so existence is
never leaked to a malformed or unconditional request): media-type/format
resolution first — unknown `Content-Type` or `XML` ⇒ 415; recognised-but-unimplemented
`application/json-patch+json` ⇒ 501 — then `If-Match` presence (absent ⇒ 428),
then entity lookup (404), then transactionId compare (412), then merge + strict
schema validation (4xx); 409 can only arise at commit. So XML with no `If-Match`
returns 415, not 428.

4xx responses carry full domain detail per the project's error conventions; 5xx
remain generic. No issue IDs appear in any shipped error text.

## 7. gRPC contract (no proto change)

The `entityManage` unary RPC dispatches on the CloudEvent `type` string, so no
`.proto` change and no regeneration of the gRPC stubs is required.

- New CloudEvent type **`EntityPatchRequest`** — the type-*string* constant is
  hand-written in `internal/grpc/cloudevent_types.go` (alongside
  `EntityUpdateRequest`), handled by a new case in `EntityManage`.
- New payload + enum types are **generated, not hand-edited**. `api/grpc/events/types.go`
  is stamped `DO NOT EDIT` and produced by `scripts/generate-events.sh` from
  JSON-schema sources under `docs/cyoda/schema/`. So `EntityPatchRequest`'s payload
  (`{ entityId, patch, transition?, ifMatch, patchFormat }`) and the `PatchFormat`
  enum are authored as **new schema files** (e.g.
  `docs/cyoda/schema/entity/EntityPatchRequest.json` and a `PatchFormat.json`
  mirroring `common/DataFormat.json`), and the generator is re-run. Editing
  `types.go` directly would be reverted by the next `make generate`.
- `PatchFormat` values: `MERGE_PATCH` (implemented) and `JSON_PATCH` (not
  implemented). This is the gRPC mirror of the HTTP media type.

**gRPC error representation (important — corrects an earlier draft).** Domain
failures over gRPC are *not* surfaced as gRPC `status` codes. `EntityManage`
returns an `EntityTransactionResponse` **envelope** with `Success: false` and an
`Error` block, and `buildErrorFields` (`internal/grpc/errors.go`) collapses **all**
operational errors to `Error.Code = "CLIENT_ERROR"`, carrying the real code only
inside the message string (`"<CODE>: detail"`). Therefore:

- `JSON_PATCH` ⇒ a `NOT_IMPLEMENTED` operational error, surfaced in the envelope as
  `Success:false, Error.Code:"CLIENT_ERROR", Error.Message:"NOT_IMPLEMENTED: ..."`.
- empty `ifMatch` ⇒ a `PRECONDITION_REQUIRED` operational error, surfaced the same
  envelope way; `"<transactionId>"` mismatch ⇒ the existing precondition error;
  `"*"` ⇒ unconditional (translated to no-CAS, as in §5).

This per-code flattening is a **pre-existing limitation of the gRPC envelope**, not
specific to PATCH. We keep it as-is for the non-breaking v0.8.2 release and document
it precisely. Surfacing distinct, machine-readable gRPC error identity (per-code, or
`codes.Unimplemented`/`codes.FailedPrecondition`) is a wire-contract change affecting
every operation and is tracked as a separate future-release improvement
(Cyoda/cyoda-go#342). Until then the Cloud-parity contract states the HTTP status
codes as canonical and documents the gRPC envelope's `CLIENT_ERROR`-plus-message
representation as the current mirror.

The HTTP media type and the gRPC `patchFormat` enum are two idiomatic expressions
of the same choice; the contract doc states the mapping explicitly so Cloud can
mirror both layers.

## 8. Service layer

### 8.1 The internal seam — a merge hook on the update flow, not a parallel method

The entire `UpdateEntity` flow from "have the new payload" onward — open tx, `Get`
base, validate, build `updated`, call the engine's `*WithIfMatch`, CAS, commit — is
identical for patch. The *only* patch-specific work is: between the in-tx `Get` and
the construction of `updated.Data`, apply the merge to `existing.Data`. So PATCH is
implemented by **refactoring `UpdateEntity` to accept an optional transform hook**
applied to the base payload inside the existing transaction, **not** by a parallel
`PatchEntity` that re-implements (and would inevitably fork) the ~200-line
tx/engine/CAS sequence. A naive "compute merged bytes, then call `UpdateEntity`"
is explicitly rejected: it would read the base in a *different* transaction from the
save and lose the atomicity guarantee below.

The public entry point `PatchEntity(ctx, PatchEntityInput)` builds the merge hook
and delegates into the shared flow:

```
PatchEntityInput {
    EntityID    string
    Format      string        // must be "JSON"
    Patch       json.RawMessage
    PatchFormat string        // "MERGE_PATCH" | "JSON_PATCH"
    Transition  string        // optional, empty for loopback
    IfMatch     string        // "<transactionId>" | "*"; required (absence is 428 at the edge)
}
```

### 8.2 Flow, executed atomically within a single transaction

1. Tenant-scoped, in-transaction `Get` of the base entity (`existing.Data`). The
   `Get` is tenant-scoped and filters soft-deleted, so a missing/soft-deleted base
   is a 404 before any save — and the read is registered in the transaction's
   read-set.
2. If `PatchFormat == JSON_PATCH` ⇒ return not-implemented (HTTP 501; gRPC envelope
   per §7).
3. Apply the RFC 7386 merge of `Patch` onto `existing.Data` (number-preserving) ⇒
   merged payload.
4. **Strictly validate** the merged result against the model schema — *validate
   only, never extend* (see §8.3). A `null`-deletion that removes a required field
   fails here.
5. Run the merged payload through the **existing** loopback / named-transition +
   `If-Match` machinery (`LoopbackWithIfMatch` / `ManualTransitionWithIfMatch`) — no
   new engine path. `If-Match: *` is translated to the no-CAS path (§5).

**Ordering under a named transition.** The merge is applied *first*; the transition's
processors then run on the merged state and may further mutate the entity. So a
processor can legitimately overwrite a field the patch set — the observable result
of a patch is *not* guaranteed to retain the patched values when a transition with
mutating processors is named. This is identical to PUT-plus-transition and is stated
explicitly in the contract (it is more than "orthogonal").

**Atomicity.** Because the base read, merge, validation, and conditional save all
occur within one transaction, and every backend re-validates the transaction's
read-set at commit (raising `spi.ErrConflict` ⇒ **409 retryable** if a concurrent
writer touched the base after our snapshot), the merged result is always derived
from the exact base being overwritten — *including under `If-Match: *`*. There is no
read-merge-write race; the residual conflict surfaces as the documented 409 (§6.4),
not silent corruption.

### 8.3 Strict validation — deliberate divergence from PUT

`UpdateEntity` uses `validateOrExtend`, which — when the model's `ChangeLevel`
permits — *extends and writes* the model schema for previously-unseen fields. PATCH
deliberately does **not** reuse the extend behaviour: it validates the merged result
**strictly** (validate-only) regardless of `ChangeLevel`, so a stray/typo'd key in a
sparse delta is rejected rather than silently widening the tenant's model. This is a
considered divergence from PUT (which may extend), justified by partial deltas —
especially LLM-generated ones, the §1 motivation — being the most likely source of
accidental fields. The divergence is documented in the Cloud-parity contract.

Both the HTTP handler and the gRPC `EntityManage` case reach the same shared flow via
`PatchEntity`, mirroring how `UpdateEntity` is shared today.

## 9. Documentation deliverables

1. **`docs/cloud-parity/` (new folder)**
   - `README.md` stating the convention: cyoda-go leads the contract; this folder
     holds Cloud-facing implementation specs; no shared ticketing yet. Cross-links
     to `docs/cyoda/cloud-divergences.md` (the inverse vector: fields cyoda-go
     declares but does not yet implement).
   - `entity-patch.md` — the PATCH contract for Cloud to implement: merge
     semantics (§4), precondition model (§5), HTTP + gRPC surfaces (§6–7), error
     table. Framed explicitly as *the spec Cloud implements*.
2. **`cmd/cyoda/help/content/crud.md`** — add PATCH to the SYNOPSIS and a
   partial-update section (media types, `If-Match` three-state, error codes).
3. **`api/openapi.yaml`** — new PATCH operations (the generator already supports
   `patch:` ops — see the existing OIDC patch operation). Add `409`/`415`/`428`/`501`
   responses; reuse a `412`/`409` component if one exists, otherwise define them.
   HTTP routes are regenerated via oapi-codegen and the handler is hand-written,
   mirroring `UpdateSingle`.
4. **Fidelity-direction reframe (surgical)** — reword the genuine "who-follows-whom"
   framing to "cyoda-go defines the contract; Cloud aligns":
   - `CLAUDE.md:3-4` (mission statement)
   - `docs/cyoda/cloud-divergences.md:1-7` (intro)
   - `docs/cyoda/README.md:2-6` (read-only mirror purpose)
   The per-feature `"accepted for Cyoda Cloud parity"` notes in `crud.md`,
   `workflows.md`, and the error topics describe specific declared-but-unimplemented
   fields — a *different* sense of "parity". They are reworded case-by-case during
   planning (who-implements-what), **not** by blanket find/replace.
5. **Release note** — `docs/release-notes/v0.8.2.md` + `CHANGELOG.md` entry at
   release time; issue #341 milestoned to v0.8.2 (the milestone is the changelog
   source).

## 10. Testing (TDD)

RED-first throughout (project Gate 1).

- **Merge function unit tests** — the RFC 7386 appendix test cases verbatim, plus
  null-delete, nested-object merge, array-wholesale-replace, non-object-patch
  replacement, empty-`{}` no-op, and a **number-fidelity** case asserting an
  `int64 > 2^53` survives a merge without `float64` coercion.
- **Service-level patch tests** — strict merged-result validation (incl.
  required-field removal failure **and** a stray new field being *rejected*, not
  schema-extended), the three `If-Match` states (transactionId-match,
  transactionId-moved ⇒ 412, `*` ⇒ unconditional via no-CAS, absent ⇒ 428), and
  JSON_PATCH ⇒ not-implemented.
- **Atomicity test** — a *concurrent interleaved commit* (not a sequential call):
  a second writer commits to the base between PATCH's in-tx base read and its save,
  asserting the read-set conflict surfaces as 409 — including under `If-Match: *`.
  A sequential call would be a no-op and must not be mistaken for an atomicity test.
- **E2E (HTTP stack)** — each media type, 404/409/412/415/428/501, and a
  transition-carrying patch (asserting merge-then-processor ordering), through the
  full HTTP server (project Gate 2).
- **gRPC E2E** — an `EntityPatchRequest` round-trip including the `patchFormat`
  enum and the `ifMatch` states, asserting the **envelope** error representation
  (`Success:false`, `Error.Code:"CLIENT_ERROR"`, code-in-message) per §7.

## 11. Out-of-scope follow-ups (recorded, not built)

- Collection/batch patch — reuses the per-item envelope the collection `PUT`
  already has; the single-item merge/precondition/validation semantics are
  identical, so it is a mechanical extension.
- XML patch.
- RFC 6902 implementation — the dialect is already selectable; only the algorithm
  and its error/validation wiring remain. (Conscious bet: we ship a public 501 for
  `application/json-patch+json` / `JSON_PATCH` ahead of demand, to nail the
  selector shape now.)
- **gRPC per-code error identity** — the envelope flattens all operational errors to
  `CLIENT_ERROR` (§7); surfacing distinct gRPC error codes is tracked as
  Cyoda/cyoda-go#342 for a future release.

## 12. Design-review disposition

This spec was revised after an independent fresh-context code review. Verified
corrections folded in: the precondition token is `transactionId` (not "version");
`If-Match: *` is translated to no-CAS + existence, never the literal `"*"`; the
single-transaction guarantee's residual conflict is a documented **409**; gRPC
errors use the existing `CLIENT_ERROR` envelope (not gRPC status codes); the gRPC
payload/enum types are generated from `docs/cyoda/schema/`, not hand-edited;
PatchEntity is a **merge hook on the shared update flow**, not a forked method;
merge-then-transition ordering and empty-`{}` semantics are stated.

Decisions taken on the review's open forks:
- **gRPC error identity:** keep the `CLIENT_ERROR` envelope for v0.8.2 (non-breaking);
  per-code identity deferred to #342.
- **Patch validation:** **strict validate-only** — patch may not extend the model
  schema (divergence from PUT, §8.3).
- **Fidelity-direction reframe:** kept folded into this change (§9.4).
- **`{format}` segment:** kept for PUT symmetry (§6.3).
