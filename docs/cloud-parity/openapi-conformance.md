# OpenAPI conformance — Cloud hand-off

Tracked in issue #369.

## Roles

cyoda-go **leads** `api/openapi.yaml`. Cyoda Cloud **conforms** to it — not the
reverse. Cloud aligns its contract to cyoda-go's; when the two disagree it is
Cloud that must reconcile, unless cyoda-go's spec is stale (tracked per-finding
in `../cyoda/cloud-divergences.md`).

## Live common ground

Operations **without** an `x-cyoda-status` extension are live — exercised end-to-end
in `internal/e2e/` and validated against the declared schema. This is the
shared contract surface Cloud MUST implement and conform to.

Operations carrying `x-cyoda-status: planned` or `x-cyoda-status: unimplemented`
are shared intent — both sides intend to implement them — but Cloud MAY or MAY
NOT have them yet. They are not part of the current conformance obligation.

## Contract access points

Cloud consumes the spec from either:

- `GET /openapi.json` (served at runtime by the cyoda-go binary)
- `cyoda help openapi json` / `cyoda help openapi yaml` (help subsystem artefact)

Both surfaces serve identical content from the embedded `api/openapi.yaml`.

## Tolerant-reader obligation

Response schemas in `api/openapi.yaml` are typed-but-open (ADR 0003, Decision 3):
properties are enumerated but objects are not sealed (`additionalProperties`
absent or `true`, never `false`). Cloud, as a consumer of cyoda-go responses,
MUST ignore unrecognised fields — new fields are additive, non-breaking changes.

The same obligation applies in the reverse direction: cyoda-go, as a consumer of
Cloud-emitted data in shared scenarios, also ignores unrecognised fields. Neither
side seals the envelope.

## Breaking-change discipline (cyoda-go side)

A CI gate (`openapi-breaking-change.yml`, using `oasdiff`) blocks merges that
introduce breaking spec changes — removing an operation or field, sealing an
open object, narrowing a type, or adding a required field to an existing
response. Cloud may rely on this gate as a stability guarantee: the live common
ground only grows; it does not shrink.

## Complement: field-level divergence catalog

`../cyoda/cloud-divergences.md` is the **inverse** catalog: fields declared in
`api/openapi.yaml` that cyoda-go does not yet implement server-side. This file
covers operation-level status and conformance obligations; the two are
complementary, not overlapping.

## Deferred open questions (resolve with Cloud team)

These two items are Cloud-fact-blocked and must be decided jointly before the
entity-slice spec can be closed:

**E6 — `EntityChangeMeta.fieldsChangedCount`.**
The canonical schema (`docs/cyoda/schema/common/EntityChangeMeta.json`) declares
`fieldsChangedCount`; cyoda-go's `getEntityChangesMetadata` handler never emits
it. Before cyoda-go implements it, we need to know: **does Cloud currently emit
`fieldsChangedCount`?** If yes, cyoda-go aligns to Cloud's existing behaviour.
If no, both sides implement it together from the spec's declaration.

**D2 — gRPC conditional-delete parity for `deleteEntities`.**
The HTTP `DELETE /entity/{entityName}/{modelVersion}` supports a condition body
(delete matching entities only). The gRPC `EntityService.DeleteEntityList` proto
takes an unconditional `EntityDeleteRequest`. **Does Cloud expect gRPC
conditional-delete parity**, and if so, what is the expected proto shape? Answer
needed before adding conditional-delete logic to the gRPC path.
