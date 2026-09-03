# Grouped statistics: `Iterate` is required, the 501 is withdrawn

cyoda-go defines the contract; Cloud mirrors it.

## Behaviour

`POST /api/entity/stats/{entityName}/{modelVersion}/query` no longer answers
`501 NOT_IMPLEMENTED_BY_BACKEND`. Every storage backend must implement the
streamed read (`EntityStore.Iterate`), so the endpoint always has an execution
path: pushdown through `GroupedAggregator` when the backend offers it and
accepts the shape, otherwise a streamed tally. The code is retired from the
error taxonomy and the OpenAPI document.

## Invariant Cloud must mirror

1. The grouped-statistics endpoint never answers 501 for a backend-capability
   reason.
2. In a transaction, grouped aggregation records nothing into the
   transaction's read-set (`spitest`'s `GroupedAggregator/InTxRecordsNothing`).
