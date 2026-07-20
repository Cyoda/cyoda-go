# docs/cloud-parity — Cloud-facing implementation contracts

cyoda-go **defines** the API and integration contract; Cyoda Cloud follows and
mirrors it. This folder holds the Cloud-facing implementation specs — contracts
written precisely enough for the Cyoda Cloud team to implement twin-alignment.

There is no shared ticketing system across the two streams yet; this folder is
the interim coordination surface. Each file in this folder describes a feature
or behaviour that Cloud must implement to stay aligned with cyoda-go.

## Relationship to cloud-divergences.md

`../cyoda/cloud-divergences.md` is the **inverse vector**: fields cyoda-go
*declares* in its OpenAPI spec but does not yet implement on the OSS server side.
This folder is the opposite direction — behaviour cyoda-go *has implemented* that
Cloud needs to adopt.

## Contents

| File | Covers |
|---|---|
| `entity-patch.md` | PATCH single-entity contract (RFC 7386 merge patch) |
| `openapi-conformance.md` | OpenAPI operation status, live common ground, tolerant-reader obligation, deferred open questions (E6, D2) |
| `search-sort.md` | Search result sorting — HTTP `sort` grammar, gRPC `orderBy`, canonical ordering semantics |
| `processor-criteria-annotations.md` | Processor `annotations` + workflow/transition `criterionAnnotations`, well-known renderer keys, schema 1.1 → 1.2 |
| `processor-attach-entity-default.md` | Processor `config.attachEntity` defaults to `true` on import, aligning with the function callouts |
| `nested-join-tx-serialisation.md` | Per-tx gate must release across external dispatch so depth-2+ nested joined cascades commit atomically instead of deadlocking |
| `criterion-stoppage-reason.md` | Criteria `reason` field: 400-response delivery + durable audit `data.reason` on automated/skip paths |
| `scheduled-transitions.md` | Scheduled-transition runtime: arm/cancel atomicity, one-shot criterion, grace-band expiry, explicit-fire reject, audit events, settled-interval reset |
| `tx-aware-search.md` | In-transaction `Search` is RYW-correct pushdown (no full-model fallback); `trackingRead` opt-in read-set recording; tx-owner co-location requirement |
