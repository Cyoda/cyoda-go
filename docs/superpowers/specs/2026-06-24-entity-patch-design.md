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

The implementation must be a faithful realisation of RFC 7386 (the recursive
`MergePatch` algorithm), so Cloud has a *specification* to match, not an
observed behaviour to reverse-engineer. The algorithm is small enough to implement
directly; the RFC's appendix test cases become unit tests (§10).

## 5. The "no stale premise" precondition model

A merge is applied relative to a base. A caller computes the delta against some
version it read earlier; if the stored entity has moved underneath it, merging the
delta onto the *new* current state can encode a stale premise (the classic
lost-update problem). PATCH is therefore inherently more dangerous than a full
replace, and we make the *choice* explicit rather than letting callers blunder
into a blind patch.

`If-Match` is **required to be present in some form** on every PATCH. Three states:

| `If-Match` value | Meaning | Outcome |
| --- | --- | --- |
| `"<version>"` | Conditional: the merge base must equal this version | **412 Precondition Failed** if the stored version has moved |
| `*` | Unconditional opt-out: "I am not pinning a version; merge onto current" | Proceeds (entity must exist) |
| *absent* | — | **428 Precondition Required**; the error body explains the two valid choices |

`*` is the RFC 7232 wildcard ("any current representation"); folding the opt-out
into the same field that carries the version means the two states can never
contradict each other. 428 is RFC 6585, which exists precisely to "prevent the
lost update problem" by forcing requests to be conditional.

This is a **deliberate divergence from `PUT`**, where a missing `If-Match` means
"unconditional replace." PATCH treats a missing `If-Match` as 428. The divergence
is justified by patch being base-relative, and is documented as such.

The version token reuses the existing optimistic-concurrency mechanism (the ETag /
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
**415 Unsupported Media Type**. The `{format}` path segment is retained for
structural symmetry with the `PUT` routes (and forward-compatibility), but only
`JSON` is accepted.

### 6.4 HTTP error model

| Code | Condition |
| --- | --- |
| 404 Not Found | Entity does not exist |
| 412 Precondition Failed | `If-Match: "<version>"` supplied and the stored version has moved |
| 415 Unsupported Media Type | `XML` format, or a `Content-Type` other than the two patch media types |
| 428 Precondition Required | No `If-Match` header present |
| 501 Not Implemented | `Content-Type: application/json-patch+json` (RFC 6902 scaffold) |
| 4xx (domain) | The **merged result** fails model-schema validation (full domain detail + error code) |

**Precedence** (request-shape validation before resource state, so existence is
never leaked to a malformed or unconditional request): media-type/format
resolution first — unknown `Content-Type` or `XML` ⇒ 415; recognised-but-unimplemented
`application/json-patch+json` ⇒ 501 — then `If-Match` presence (absent ⇒ 428),
then entity lookup (404), then version compare (412), then merge + schema
validation (4xx). So XML with no `If-Match` returns 415, not 428.

4xx responses carry full domain detail per the project's error conventions; 5xx
remain generic. No issue IDs appear in any shipped error text.

## 7. gRPC contract (no proto change)

The `entityManage` unary RPC dispatches on the CloudEvent `type` string, so no
`.proto` change and no regeneration is required.

- New CloudEvent type **`EntityPatchRequest`** (registered alongside
  `EntityUpdateRequest` in `internal/grpc/cloudevent_types.go`), handled by a new
  case in `EntityManage`.
- New payload type `EntityPatchPayloadJson` in `api/grpc/events/types.go`:
  `{ entityId, patch, transition?, ifMatch, patchFormat }`.
- New Go string-enum `PatchFormatJson` following the existing `DataFormatJson`
  pattern (generated-style constants, not a proto3 enum): `MERGE_PATCH`
  (implemented) and `JSON_PATCH` (⇒ gRPC `UNIMPLEMENTED`). This is the gRPC mirror
  of the HTTP media type.
- `ifMatch` carries `"<version>"` / `"*"` / empty. Empty ⇒ the gRPC equivalent of
  428 (a `FailedPrecondition`/precondition-required-style error consistent with how
  the existing `ENTITY_MODIFIED` mapping is surfaced).

The HTTP media type and the gRPC `patchFormat` enum are two idiomatic expressions
of the same choice; the contract doc states the mapping explicitly so Cloud can
mirror both layers.

## 8. Service layer

A new `PatchEntity(ctx, PatchEntityInput)` method in
`internal/domain/entity/service.go`, parallel to `UpdateEntity`:

```
PatchEntityInput {
    EntityID    string
    Format      string        // must be "JSON"
    Patch       json.RawMessage
    PatchFormat string        // "MERGE_PATCH" | "JSON_PATCH"
    Transition  string        // optional, empty for loopback
    IfMatch     string        // "<version>" | "*"; required (absence handled at the edge as 428)
}
```

Flow, executed **atomically within a single transaction**:

1. Load the base entity (`existing.Data`).
2. If `PatchFormat == JSON_PATCH` ⇒ return not-implemented (501 / `UNIMPLEMENTED`).
3. Apply the RFC 7386 merge of `Patch` onto `existing.Data` ⇒ merged payload.
4. Validate the **merged result** against the model schema (reusing
   `validateOrExtend`); a `null`-deletion that removes a required field fails here.
5. Funnel the merged payload into the **existing** loopback / named-transition +
   `If-Match` machinery that `UpdateEntity` already uses — no new engine path.

Because the read-of-base, merge, validation, and conditional save all occur within
one transaction, the merged result is always derived from the exact version being
overwritten — including under `If-Match: *` — so there is no read-merge-write race.

Both the HTTP handler and the gRPC `EntityManage` case call `PatchEntity`, so there
is a single code path, mirroring how `UpdateEntity` is shared today.

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
3. **`api/openapi.yaml`** — new PATCH operations; add `415`/`428`/`501` to
   `components/responses` (reuse the existing `412`).
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
  null-delete, nested-object merge, array-wholesale-replace, and non-object-patch
  replacement.
- **Service-level patch tests** — merged-result schema validation (incl.
  required-field removal failure), the three `If-Match` states (version-match,
  version-moved ⇒ 412, `*` ⇒ unconditional, absent ⇒ 428), JSON_PATCH ⇒
  not-implemented, and transactional atomicity of read-merge-validate-save.
- **E2E (HTTP stack)** — each media type, 404/412/415/428/501, and a
  transition-carrying patch, through the full HTTP server (project Gate 2).
- **gRPC E2E** — an `EntityPatchRequest` round-trip including the `patchFormat`
  enum and the `ifMatch` states.

## 11. Out-of-scope follow-ups (recorded, not built)

- Collection/batch patch — reuses the per-item envelope the collection `PUT`
  already has; the single-item merge/precondition/validation semantics are
  identical, so it is a mechanical extension.
- XML patch.
- RFC 6902 implementation — the dialect is already selectable; only the algorithm
  and its error/validation wiring remain.
