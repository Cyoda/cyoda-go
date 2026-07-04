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

## Entity-slice reconciliations (2026-07)

Per-finding contract decisions from the entity reconciliation slice (design
`docs/superpowers/specs/2026-07-02-openapi-contract-reconciliation-design.md`,
ADR 0003). Each states the direction and what Cloud must mirror.

- **E1 — `Envelope.meta` typed-but-open.** `meta` now mirrors the canonical
  `EntityMetadata.json` (required `id/state/creationDate/lastUpdateTime`; optional
  `modelKey/pointInTime/transitionForLatestSave/transactionId`); the
  `previousTransition` fossil is removed. Direction: spec-incomplete + spec-stale.
  Cloud MUST emit the same typed-but-open meta.
- **E1b — `modelKey` on all reads (deviation A2 abandoned).** Previously by-model reads
  (`getAllEntities`, search — HTTP and gRPC) omitted `modelKey` while by-id `getOneEntity`
  included it (the "A2" optimization). That deviation is abandoned: `modelKey` is now emitted on
  every entity read (single-get, list, search) across HTTP and gRPC — one uniform meta shape.
  Direction: needs-decision → decided (byte-saving rationale negligible; uniform meta is simpler
  and fully canonical). Cloud MUST emit `modelKey` on all entity reads. (The `EntityMetadata`
  schema already marks `modelKey` optional, so this is additive/non-breaking for consumers.)
- **E2 — conditional `deleteEntities` (HTTP).** `DELETE /entity/{name}/{version}`
  with an `AbstractConditionDto` body deletes only matching entities (empty body ⇒
  all); `verbose=true` returns deleted ids; `numberOfEntitites` (matched) and
  `numberOfEntititesRemoved` (removed) are distinct. Direction: server-gap (closed a
  data-loss defect). Cloud MUST honour the condition — never ignore it.
- **E3 — `getAllEntities` as-at.** The model-scoped list read honours `pointInTime`
  (via the list-PIT primitive) and stamps `meta.pointInTime`. Direction: server-gap.
- **E8 — `changeType` spelling = `CREATE/UPDATE/DELETE`.** Canonical, gRPC, HTTP, and
  OpenAPI now agree on the present-tense spelling (was `CREATED/UPDATED/DELETED` on
  HTTP+OpenAPI). Direction: needs-decision → decided (canonical/gRPC already used it;
  changing the Cloud-consumed canonical field was the higher-risk alternative).
  Cloud MUST emit `CREATE/UPDATE/DELETE`.

## Open questions (Cloud-fact-blocked)

Decided once the Cloud facts are gathered (Gate 7):

- **E6 — `EntityChangeMeta.fieldsChangedCount`.** Declared in canonical
  `EntityChangeMeta.json`, never emitted by cyoda-go. Question: **does Cloud emit
  `fieldsChangedCount`?** If yes → implement emission (server-gap); if no → remove
  from the canonical schema (spec-stale). Leaning: implement (removing a
  Cloud-consumed canonical field is higher-risk). Not implemented in this slice.
- **D2 — gRPC conditional delete.** The conditional delete is an HTTP-contract
  feature; the gRPC/proto contract exposes only unconditional delete-all + per-entity
  delete. Question: **does Cloud expect gRPC conditional-delete parity?** If yes →
  add it to the proto contract; if no → HTTP-only is correct. Not implemented here.
- **Error-code naming — `BAD_REQUEST` vs `MALFORMED_REQUEST`.** Entity write handlers
  emit `BAD_REQUEST` for malformed bodies; `grouped_stats` emits `MALFORMED_REQUEST`
  for the same class. Unifying spans create/collection/patch and the stats/search +
  model slices. Deferred to a cross-slice follow-on; the entity error tables document
  the codes actually emitted.
