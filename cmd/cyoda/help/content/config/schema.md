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

On the `sqlite` and `postgres` backends a model's schema extensions are kept as a log:
the model's base schema plus one row per extension. A read of the model folds the log
into the base schema. A savepoint row holds the folded schema, so a read starts from the
latest savepoint and folds only the extensions after it. The memory backend applies each
extension in place and keeps no log.

## OPTIONS

- `CYODA_SCHEMA_SAVEPOINT_INTERVAL` — number of a model's extensions per savepoint: the
  extension that brings the count since the model's latest savepoint to this value also
  writes a savepoint (default: `64`, minimum: `1`; a value below it falls back to the
  default). It bounds how many extensions a read of that model folds. The count is per
  model. A lower value makes reads of a model that extends often cheaper,
  at the cost of a savepoint row more often.

The prefix `CYODA_SCHEMA_` namespaces the schema-extension variables; they are not plugin-namespaced, and both the `sqlite` and `postgres` plugins read them.

Extensions have no retry budget: an extension that meets another extension of the same
model waits for it, until the request ends (`cyoda help models`, SCHEMA EXTENSION).

## EXAMPLES

**Model that extends on most writes (fewer extensions folded per read):**

```
CYODA_SCHEMA_SAVEPOINT_INTERVAL=16
```

**Default:**

```
CYODA_SCHEMA_SAVEPOINT_INTERVAL=64
```

## SEE ALSO

- config
- models
- run
