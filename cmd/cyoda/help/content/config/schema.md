---
topic: config.schema
title: "schema configuration"
stability: stable
see_also:
  - config
  - models
  - run
---

# config.schema

## NAME

config.schema — schema-extension log tuning.

## SYNOPSIS

cyoda maintains a schema-extension log for tracking model schema changes over time. These
variables control savepoint frequency and retry behaviour when extending schemas on storage
backends.

## OPTIONS

- `CYODA_SCHEMA_SAVEPOINT_INTERVAL` — number of schema extensions between savepoint rows in
  the schema-extension log (default: `64`, minimum: `1`). Lower values increase write
  amplification but reduce recovery time after a crash.
- `CYODA_SCHEMA_EXTEND_MAX_RETRIES` — plugin-layer retry budget for `ExtendSchema` on
  backends that support native schema extension with optimistic concurrency
  (default: `8`, minimum: `1`). Increase if you observe contention under high concurrency.

The prefix `CYODA_SCHEMA_` is used to namespace all schema-extension configuration variables.

## EXAMPLES

**High-write workload (more frequent savepoints):**

```
CYODA_SCHEMA_SAVEPOINT_INTERVAL=16
CYODA_SCHEMA_EXTEND_MAX_RETRIES=16
```

**Default:**

```
CYODA_SCHEMA_SAVEPOINT_INTERVAL=64
CYODA_SCHEMA_EXTEND_MAX_RETRIES=8
```

## SEE ALSO

- config
- models
- run
