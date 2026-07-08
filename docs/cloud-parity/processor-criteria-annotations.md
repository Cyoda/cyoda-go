# Processor & criteria annotations — Cloud twin-alignment spec

This document is the contract Cyoda Cloud implements to stay aligned with
cyoda-go's processor and criteria annotations feature. cyoda-go is the
authoritative implementation; the behaviour described here is derived
directly from its design spec and implemented code.

## 1. Wire shape

Two new optional fields on the `POST …/workflow/import` / `GET …/workflow/export`
DTOs, both reusing the existing workflow/state/transition `annotations` bag
shape:

| Element | Field | Placement |
|---|---|---|
| Processor | `annotations` | embedded on `ProcessorDefinition` |
| Workflow | `criterionAnnotations` | sibling to `criterion` on `WorkflowDefinition` |
| Transition | `criterionAnnotations` | sibling to `criterion` on `TransitionDefinition` |

`criterionAnnotations` is a **sibling** field, never embedded inside the
`criterion` value — the criterion tree round-trips verbatim and must not be
parsed or re-serialised to attach metadata to it.

```jsonc
{
  "name": "APPROVE",
  "next": "approved",
  "criterion": { /* opaque condition tree — untouched, verbatim */ },
  "criterionAnnotations": { "displayName": "Approved & funded" },
  "processors": [
    { "name": "notify", "type": "externalized",
      "annotations": { "displayName": "Send approval email" } }
  ]
}
```

## 2. Validation

Both fields reuse the existing annotations rule, applied per-field on import:

- Must be a JSON object (or absent) — non-object values are rejected with
  `400 VALIDATION_FAILED`.
- Capped at 64 KB per field, measured on the compacted form.
- Stored and re-emitted compacted; the engine never interprets the contents.
- No new error codes.

## 3. Well-known keys (advisory)

Two optional keys are documented for renderer use, uniform across all five
workflow element types (workflow, state, transition, processor, criterion):

| Key | Type | Meaning |
|---|---|---|
| `displayName` | string | Short human-readable label |
| `description` | string | Longer human-readable description |

These are a **documented convention, not an enforced schema** — the server
validates object-shape and size only, never key types. A client may store
`{"displayName": 123}` and it round-trips as-is.

## 4. Schema version

Workflow schema bumps **1.1 → 1.2**, additive MINOR:

- `1.2` accepted.
- `1.1` still accepted (dual-shape) — purely additive, every 1.1 payload
  stays valid unchanged.
- Export stamps the current version (`1.2`) regardless of the imported
  version.

## 5. Engine scope

Both fields are engine-ignored metadata: no workflow-execution behaviour
depends on their contents. cyoda-go does not forward processor `annotations`
to external compute-node gRPC dispatch requests — those requests are built
field-by-field and carry no annotations. `criterionAnnotations` is likewise
never forwarded: it is a transition/workflow sibling, and the criteria-dispatch
request (used for `FUNCTION` criteria, which are evaluated on a compute node)
carries only the bare `criterion` value. Cloud's own compute-dispatch path
should apply the same exclusion.

## 6. Backend support

Import/export of both fields is backend-agnostic — the fields live on the
workflow-configuration document, not in per-backend storage schemas. The
cross-backend parity suite validates round-trip fidelity across memory,
sqlite, and postgres; the commercial backend must round-trip identically.
