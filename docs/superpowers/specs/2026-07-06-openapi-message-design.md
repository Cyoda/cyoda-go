# OpenAPI contract reconciliation — Edge message slice (group 4)

Status: **Design agreed** (Paul, 2026-07-06). Part of the OpenAPI reconciliation effort (#369),
governed by ADR 0003 (`docs/adr/0003-openapi-contract-conformance-and-evolution.md`) and the
typed-but-open schema policy. Independently design-reviewed (fresh-context) before writing.

## 1. Scope

Reconcile the four **Edge message** operations in `api/openapi.yaml` with what
`internal/domain/messaging/handler.go` actually does, so the published contract describes real,
provable behavior (the enforce-mode conformance test validates every request/response against the
spec).

Operations in scope:

| operationId | route | handler |
|---|---|---|
| `newMessage` | `POST /message/new/{subject}` | `handler.go:32` |
| `getMessage` | `GET /message/{messageId}` | `handler.go:117` |
| `deleteMessage` | `DELETE /message/{messageId}` | `handler.go:194` |
| `deleteMessages` | `DELETE /message` | `handler.go:226` |

**Out of scope (deferred, tracked):**
- `transactionSize` / `transactionTimeoutMillis` are bound but never read (API-wide, not
  message-local). Honoring them is a separate feature — **#379** (supersedes #372). The message
  contract leaves these params documented as-is; #379 fixes them uniformly.
- Native non-JSON / content-type payload support — **#193** (open). The message contract documents
  the current JSON-envelope behavior (binary must be base64-stringified into the JSON `payload`, as
  the `EdgeMessagePayload` schema note already states).
- The 13 unrouted `Stream Data` ops (excluded-tag dead surface) — group 5.

HTTP-only surface: there is **no gRPC message service** (`internal/grpc/` has no message handler), so
the gRPC coverage column is N/A throughout. Messages are backend-agnostic (every `MessageStore`
backend implements the same contract), so one cross-backend parity scenario applies.

No new error codes: the ops emit only `BAD_REQUEST` (`ErrCodeBadRequest`) and `ENTITY_NOT_FOUND`
(`ErrCodeEntityNotFound`), both of which already have `cmd/cyoda/help/content/errors/*.md` topics.

## 2. Findings and reconciliation directions

Every server-behavior claim below was independently verified against the handler and the storage
backends.

### F1 — `deleteMessages` request body: `string` → array of uuid strings
Spec declares `type: string, format: uuid` (`openapi.yaml:3330`) and prose "All IDs must be version 1
(time-based) UUIDs" (`:3325`). The handler unmarshals `[]string` (`handler.go:235`). **Fix:** body
schema → `type: array, items: {type: string, format: uuid}`; drop the v1 prose. (oasdiff: breaking —
see §5.)

### F2 — `newMessage` request body: `string` → JSON object envelope
Spec declares `type: string` (`:3487`). The handler unmarshals an object
`{payload (required, any JSON value), meta-data (optional flat map)}`; missing `payload` → 400
(`handler.go:45-57`). **Fix:** introduce a named request schema `NewMessageRequest`:

```yaml
NewMessageRequest:
  type: object
  description: >
    A single JSON object carrying the message payload and optional flat metadata.
  properties:
    payload:
      $ref: "#/components/schemas/EdgeMessagePayload"   # any JSON value (string/number/object/array/…)
      description: >
        The message payload — any JSON value. Non-JSON content (binary, etc.) must be stringified
        (e.g. base64) and sent as a JSON string; see the EdgeMessagePayload note.
    meta-data:
      type: object
      additionalProperties: true
      description: Optional flat key→value map; indexed to enable search by metadata.
  required:
    - payload
```

`payload` reuses `EdgeMessagePayload` (already unrestricted — permits any JSON value including a
base64 JSON string), so the base64-binary workaround keeps working unchanged. (oasdiff: breaking —
see §5.) The two existing request examples are already objects, so they become consistent.

### F3 — `newMessage` description drops the fictional array-body claim
Desc claims the body may be "a single JSON object or an array of homomorphic JSON objects"
(`:3469`). A top-level array fails unmarshal → 400; only a single object is accepted. **Fix:** rewrite
the description to single-object-only.

### F5 — remove fictional `400 "not a time-based UUID"` / `uuid-v1` typing
No handler performs any v1/UUID-version validation. `getMessage` (`handler.go:117-191`) and
`deleteMessage` (`:194-223`) emit **only** 404 (`ErrNotFound`) or 500. A non-existent id — of any
UUID version, on any backend — resolves to 404 (message-id columns are `TEXT` in postgres
`migrations/000001…:82` and sqlite `:65`, a plain map key in memory; no cast error → no 500). **Fix:**
- `getMessage` / `deleteMessage`: **remove** the documented `400` (`:3644`, `:3715`) entirely; change
  the path param `format: uuid-v1` → `uuid`; drop the "Must be a version 1 UUID" prose. Keep `404`.
- `deleteMessages`: **keep** a `400`, but re-describe it as the *real* error — invalid JSON
  (`"invalid JSON: expected array of UUID strings: …"`, `handler.go:237`) — replacing the fictional
  "Not a Time UUID" example, and correct its `instance` to `/api/message` (no id; it is `DELETE /message`
  with a body).

### F6 — `getMessage.metaData` `ValueMaps` buckets → typed-but-open flat map
The handler emits `values`/`indexedValues` as flat `map[string]any`, each **always** seeded with
`typeReferences: {}` then flat user keys (`handler.go:165-177`). The `ValueMaps` typed-bucket schema
(strings/ints/doubles/localTimes/… `:10407`) does not model this. **Fix:** retype
`EdgeMessageMetaData.values`/`indexedValues` to:

```yaml
values:
  type: object
  additionalProperties: true
  description: >
    Flat map of metadata values. Always contains an injected `typeReferences` key
    (an empty object in this version).
```

(same for `indexedValues`). Do **not** over-model `typeReferences` (leave it as a present-but-open
key). Then **delete** the now-dead `ValueMaps` and `LocalTime` schemas (§4).

> **UPDATED during execution (design decision, Paul — cyoda-go leads the contract):** the
> `values`/`indexedValues` split and the injected `typeReferences: {}` are cyoda-cloud indexing
> workarounds — in cyoda-go `values` is always empty (all `meta-data` routes to indexed storage) and
> `typeReferences` is an always-empty placeholder. Rather than faithfully reproduce that cruft, the
> slice **simplifies** `metaData` to a single **flat symmetric map**: what a client PUTs in
> `meta-data` it GETs back in `metaData` (no `values`/`indexedValues`, no `typeReferences`). This is a
> response-shape change in the `GetMessage` handler (merge of stored `Values`+`IndexedValues`, no
> injection) plus `EdgeMessageMetaData` becoming `{type: object, additionalProperties: true}`. No SPI/
> storage/indexing change; cyoda-cloud conforms (recorded in `docs/cloud-parity/openapi-conformance.md`
> M3). F7 is subsumed (the example is now a flat map). See the Task 5 revision.

### F7 — `getMessage` 200 example shows the injected `typeReferences`
Example (`:3625`) shows `values: {}` / `indexedValues: {}`; the server always injects
`typeReferences: {}`. **Fix:** update the example to `values: {typeReferences: {}}` /
`indexedValues: {typeReferences: {}}` (plus any illustrative user keys).

### F8 — error examples show the always-present `errorCode` and drop fictional props
`common/errors.go:234` unconditionally sets `properties.errorCode` on every ProblemDetail; `Title`
is `http.StatusText(status)` (`:178`). The message examples are wrong in three ways (folded from the
review):
- **All** error examples omit `errorCode`. Add it.
- The `400` examples invent props the server never emits: `properties.error` on newMessage
  (`:3562`), `properties.uuid` on the get/delete/deleteMessages 400s (`:3372`,`:3660`,`:3731`).
  Remove them. The real `404` prop `messageId` (`handler.go:128`,`:205`) **is** correct — keep it and
  add `errorCode`.
- The **413** example title `Payload Too Large` (`:3575`) and detail are wrong: server emits
  `title: "Request Entity Too Large"`, `detail: "request payload exceeds maximum allowed limit of
  10MB"` (`handler.go:38`). Correct them.

## 3. One runtime change (Gate-6 consistency fix) — `deleteMessages` 413 parity

`newMessage` and `deleteMessages` both wrap the body in a 10MB `MaxBytesReader`, but on
overflow `newMessage` returns **413** (`handler.go:37-39`) while `deleteMessages` returns **500**
(`handler.go:230-232`) for the identical condition. This is an error-handling inconsistency, not a
feature gap. **Fix:** make `deleteMessages` map the `"http: request body too large"` error to
`413 BAD_REQUEST` exactly like `newMessage`, and document `413` on the op. Bounded, TDD-driven,
oasdiff-additive (new response, non-breaking). Flagged for Paul; drop from scope if undesired.

*(This is the only behavioral change in the slice; everything else is documentation reconciliation.)*

## 4. Schema changes

- **Add** `NewMessageRequest` (F2).
- **Retype** `EdgeMessageMetaData.values` / `indexedValues` to typed-but-open flat maps (F6).
- **Delete** `ValueMaps` (referenced only at `:10347`,`:10349`, both inside `EdgeMessageMetaData`) and
  `LocalTime` (referenced only inside `ValueMaps` at `:10461`). Both become unreferenced after F6 —
  verified dead. The generated Go types `ValueMaps`/`LocalTime`/`ValueMapsTypeReferences`/
  `ValueMapsYearMonthsMonth` are not used by any hand-written code, so regeneration is compile-safe.
- **Do NOT touch** `EntityTransactionResponse` (shared by 8 ops) — `newMessage` keeps using it. Its
  chunking/`failed[]` fields don't apply to messaging (handler always emits one element,
  `handler.go:107-113`), but under typed-but-open the extra optional fields are tolerable and the
  shared schema must not be edited.
- Regenerate `api/generated.go` (`go generate ./api` / `make` codegen target) after the YAML edits;
  the `codegen-sync` CI gate requires it committed. Verify `go build`, `go vet ./...`.

## 5. Per-endpoint error/status table (post-reconciliation)

| Op | 200 | 400 | 404 | 413 | 401/403 | 500 |
|---|---|---|---|---|---|---|
| `newMessage` | `array<EntityTransactionResponse>` (1 elem) | invalid JSON / missing `payload` (`BAD_REQUEST`) | — | body >10MB (`BAD_REQUEST`, status 413) | shared refs | `default` |
| `getMessage` | `EdgeMessageDto` (flat metaData) | **removed** | not found (`ENTITY_NOT_FOUND`, prop `messageId`) | — | shared refs | `default` |
| `deleteMessage` | `MessageDeleteResponse` | **removed** | not found (`ENTITY_NOT_FOUND`, prop `messageId`) | — | shared refs | `default` |
| `deleteMessages` | `array<MessageDeleteBatchResponse>` | invalid JSON array (`BAD_REQUEST`) | — | body >10MB (`BAD_REQUEST`, status 413) — **new (§3)** | shared refs | `default` |

The shared `401 Unauthorized` / `403 Forbidden` refs are a repo-wide convention on every `/api` op;
message routes are auth-gated with no role requirement, so they stay as-is (not reconciled
message-locally — that would create fresh inconsistency).

## 6. Coverage matrix (scenario × layer)

HTTP-only (gRPC N/A). `✔` = exists in `internal/e2e/message_test.go`; `✎` = add/extend.

| Scenario | unit | e2e (running Postgres) | cross-backend parity |
|---|---|---|---|
| `newMessage` 200 — object envelope; `payload` any JSON incl. base64 string | | ✎ extend | ✎ round-trip |
| `newMessage` 400 — missing `payload` | | ✎ | |
| `newMessage` 400 — invalid JSON | | ✎ | |
| `newMessage` 413 — body >10MB | | ✎ | |
| `getMessage` 200 — flat metaData incl. injected `typeReferences` | | ✎ extend | ✎ round-trip |
| `getMessage` 404 — valid absent uuid | ✔ | | |
| `getMessage` **v4 (non-v1) uuid → 404 not 400** (proves F5) | | ✎ | |
| `deleteMessage` 200 | ✔ | | |
| `deleteMessage` 404 | | ✎ | |
| `deleteMessage` **v4 uuid → 404 not 400** (proves F5) | | ✎ | |
| `deleteMessages` 200 — array body | ✔ | | ✎ round-trip |
| `deleteMessages` 400 — invalid array | | ✎ | |
| `deleteMessages` 413 — body >10MB (§3) | | ✎ | |

**F5 conformance-trap note:** the enforce-mode validator route-matches the path param against a UUID
regex (`internal/e2e/openapivalidator/validator.go:365-406`). With `format: uuid` retained, a
*non-UUID* id is rejected before the handler — so F5's "no v1 check" claim must be proven with a
valid **v4** UUID (which route-matches yet is not v1); a v1-validating server would have 400'd it, a
404 proves the validation is fiction. Keeping `format: uuid` (ids are server-generated time-UUIDs;
sibling endpoints use `format: uuid`) + v4-uuid 404 tests is the coherent choice.

**Parity:** add one backend-agnostic `new → get → delete` round-trip scenario (asserting the flat
metaData + injected `typeReferences` shape) to `e2e/parity` + `registry.go`, covering
memory/sqlite/postgres (+ commercial). Not a concurrency test (those stay isolated, never parity).

## 7. oasdiff plan

Two entries **required** in `.github/oasdiff-err-ignore.txt` (breaking, ERR-level), mirroring the
createCollection precedent (`:15`). Exact single-line text captured from the pinned oasdiff run at
implementation time (fail-closed if wording drifts):
- `newMessage` (`POST /message/new/{subject}`) request body `string` → `object` [request-body-type-changed].
- `deleteMessages` (`DELETE /message`) request body `string/uuid` → `array<string>` [request-body-type-changed].

**Verify, do not pre-generate** (likely WARN / non-breaking; add an entry only if the pinned oasdiff
classifies it ERR):
- Removing the `400` on getMessage/deleteMessage — `response-non-success-status-removed` (usually WARN).
- The `EdgeMessageMetaData` retype + `ValueMaps`/`LocalTime` deletion — removes optional nested props
  and adds `additionalProperties` (usually non-breaking).
- The new `413` responses (§3) — additive, non-breaking.

Capture the oasdiff exit code via redirect (not pipe-to-tail) per the group-3 lesson.

## 8. Documentation (Gate-4)

- No dedicated message help topic or README section exists — the OpenAPI spec **is** the doc; the
  YAML edits are the doc update.
- Add a `docs/cloud-parity/openapi-conformance.md` entry recording the message reconciliation
  decisions (v1-UUID fiction removed, metaData flat-map, request-body shapes, the §3 413 parity fix)
  and the #379 / #193 deferrals.
- CHANGELOG entry if the repo convention requires one (check during planning).
- No COMPATIBILITY / env-var / SPI-pin changes.

## 9. Execution notes (carried from prior slices)

- Controller runs full `internal/e2e` + conformance at consolidation points (subagent Docker is
  inconsistent; the controller's Docker works).
- After any runtime status change (the §3 413), update the `e2e/parity` registry if the round-trip
  scenario asserts it; `make race` runs the parity suite (per-task `internal/e2e` checkpoints don't).
- `go vet ./...` (not just `go build`) to catch all call sites after the regen.
- Keep the PR scoped — avoid a gofmt-sweep dragging in unrelated churn.
