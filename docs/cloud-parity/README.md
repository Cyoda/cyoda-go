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
| `nested-join-tx-serialisation.md` | Per-tx gate must release across external dispatch so depth-2+ nested joined cascades commit atomically instead of deadlocking |
