---
topic: errors.DUPLICATE_AGGREGATION_ALIAS
title: "DUPLICATE_AGGREGATION_ALIAS — two aggregations claim the same response key"
stability: stable
see_also:
  - errors
  - crud
  - errors.INVALID_AGGREGATION_OP
  - errors.INVALID_AGGREGATION_FIELD
---

# errors.DUPLICATE_AGGREGATION_ALIAS

## NAME

DUPLICATE_AGGREGATION_ALIAS — two aggregations over different `(op, field)` pairs resolve to the same key in the response.

## SYNOPSIS

HTTP: `400` `Bad Request`. Retryable: `no`.

## DESCRIPTION

Raised by `POST /api/entity/stats/{entityName}/{modelVersion}/query`. Each bucket's `aggregations` object is keyed by the aggregation's `as` alias, or — when `as` is omitted — by a synthesized `<op>_<field>` with the leading `$.` stripped, so `{"op":"sum","field":"$.amount"}` becomes `sum_amount`. Two aggregations that compute different things cannot share one key, because only one of the two values could be returned under it.

The collision can arise between two explicit aliases, or between an explicit alias and a synthesized one (`as: "sum_amount"` on one entry alongside an unaliased `sum` over `$.amount` on another). The message carries the contested alias.

Repeating the *same* `(op, field)` pair does not raise this — identical pairs are deduplicated silently, since both would compute the same value.

Give the colliding aggregations distinct `as` aliases.

## SEE ALSO

- errors
- crud
- errors.INVALID_AGGREGATION_OP
- errors.INVALID_AGGREGATION_FIELD
