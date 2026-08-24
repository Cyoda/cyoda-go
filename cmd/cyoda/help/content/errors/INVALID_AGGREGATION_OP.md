---
topic: errors.INVALID_AGGREGATION_OP
title: "INVALID_AGGREGATION_OP — unknown aggregation operator"
stability: stable
see_also:
  - errors
  - crud
  - errors.INVALID_AGGREGATION_FIELD
  - errors.DUPLICATE_AGGREGATION_ALIAS
---

# errors.INVALID_AGGREGATION_OP

## NAME

INVALID_AGGREGATION_OP — an aggregation's `op` is not one of the supported operators.

## SYNOPSIS

HTTP: `400` `Bad Request`. Retryable: `no`.

## DESCRIPTION

Raised by `POST /api/entity/stats/{entityName}/{modelVersion}/query`. The supported operators are `sum`, `avg`, `min`, `max` and `stdev`, spelled lowercase. Anything else — including `count`, an uppercase spelling, or a typo — is rejected, and the message echoes the value that was sent.

There is no `count` operator because every bucket already carries `count`. `stdev` is the sample standard deviation (divisor `n − 1`) and is `null` when a bucket has fewer than two numeric samples.

## SEE ALSO

- errors
- crud
- errors.INVALID_AGGREGATION_FIELD
- errors.DUPLICATE_AGGREGATION_ALIAS
