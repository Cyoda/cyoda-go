---
topic: cloudevents
title: "cloudevents — CloudEvent payload JSON Schemas"
stability: stable
see_also:
  - grpc
  - workflows
  - errors.DISPATCH_TIMEOUT
  - errors.DISPATCH_FORWARD_FAILED
---

# cloudevents

## NAME

cloudevents — JSON Schemas for every CloudEvent payload exchanged over the gRPC `CloudEventsService` bidirectional stream.

## SYNOPSIS

```
cyoda help cloudevents            # narrative (this page)
cyoda help cloudevents json       # full schemas tree as a single JSON document
```

## DESCRIPTION

cyoda-go uses [CloudEvents v1.0](https://cloudevents.io) as the transport envelope for event-driven processing. Every payload carried inside a CloudEvent — workflow calculation requests, calculation responses, entity transaction events, snapshot state, model descriptors — is validated against a JSON Schema (Draft 2020-12).

The canonical schema tree lives at `docs/cyoda/schema/` in this repo and is embedded into the binary at build time. It is the single source of truth for:

- **Runtime validation** of inbound CloudEvent payloads (in cyoda-go and external compute members).
- **Code generation** via `scripts/generate-events.sh` — the Go types in `api/grpc/events/types.go` are derived from this tree.
- **Downstream tooling**: documentation pipelines and SDK generators extract the tree from the binary (see `cyoda help cloudevents json`), so every version of cyoda ships with a matching, self-describing schema bundle.

## ORGANIZATION

The tree is organized by domain:

- **`common/`** — shared envelope fragments: `BaseEvent`, `DataPayload`, `CloudEventType`, `ModelSpec`, `ErrorCode`. Every domain-specific payload extends `BaseEvent`.
- **`common/statemachine/`** — workflow metadata descriptors: `WorkflowInfo`, `TransitionInfo`, `ProcessorInfo`.
- **`entity/`** — entity create / update / delete / transition / audit requests and responses.
- **`model/`** — model snapshot events and management requests.
- **`processing/`** — compute-member protocol events: `EntityCriteriaCalculationRequest`/`Response`, `EntityProcessorCalculationRequest`/`Response`, `EntityFunctionCalculationRequest`/`Response`, `CalculationMemberJoinEvent`, `CalculationMemberGreetEvent`, `EventAckResponse`.
- **`search/`** — search snapshot lifecycle events.

Every schema carries a `$id` in the form `https://cyoda.com/cloud/event/<relative-path>`. Inter-schema references use relative `$ref` paths (e.g. `../common/BaseEvent.json`) so a materialized filesystem copy resolves out of the box.

## ACTION: `cyoda help cloudevents json`

Emits the entire tree as a single JSON document. Shape:

```json
{
  "schema": 1,
  "version": "<binary-version>",
  "specVersion": "https://json-schema.org/draft/2020-12/schema",
  "baseId": "https://cyoda.com/cloud/event/",
  "schemas": {
    "common/BaseEvent.json":                { "$schema": "...", "$id": "...", ... },
    "common/statemachine/WorkflowInfo.json": { ... },
    "entity/EntityTransactionResponse.json": { ... },
    "processing/EntityCriteriaCalculationRequest.json": { ... },
    ...
  }
}
```

- **`schemas`** is a map keyed by relative path (identical to the directory layout). This lets downstream tooling fan the tree out to disk without renaming.
- **Values are complete JSON Schema documents**, structurally identical to the source file after normalizing whitespace and key ordering.
- **`$ref` values stay relative** — they are not rewritten to absolute URLs. A consumer that writes the tree to disk gets working resolution for free.
- **Keys are lexicographically sorted** — output is diff-stable across builds.
- **`baseId`** and **`specVersion`** are constants exposed for consumers that want to construct absolute IDs or validate against a specific meta-schema.

## VERIFICATION

Count emitted schemas:

```bash
cyoda help cloudevents json | jq '.schemas | keys | length'
```

List every schema path:

```bash
cyoda help cloudevents json | jq -r '.schemas | keys[]'
```

Materialize the tree to a directory:

```bash
cyoda help cloudevents json \
  | jq -r '.schemas | to_entries[] | "\(.key)\t\(.value | tojson)"' \
  | while IFS=$'\t' read -r path body; do
      mkdir -p "$(dirname "schemas/$path")"
      printf '%s\n' "$body" > "schemas/$path"
    done
```

## SEE ALSO

- grpc
- workflows
- errors.DISPATCH_TIMEOUT
- errors.DISPATCH_FORWARD_FAILED
