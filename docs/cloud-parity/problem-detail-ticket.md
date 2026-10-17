# An unexpected server error carries a `ticket`

cyoda-go is the authoritative implementation. This file is the contract Cyoda
Cloud follows.

## Behaviour

The problem document (`application/problem+json`) of an unexpected server
error (a 500 from an internal or fatal error) carries a top-level `ticket`: a
UUID that the server also wrote into its log line for the failure. A caller
quotes it; an operator finds the log line by it. `ProblemDetail` declares the
field (`type: string`, `format: uuid`). Every other response carries no
`ticket`: not a 4xx, and not an operational 5xx such as `503
STORAGE_UNAVAILABLE`, `NO_COMPUTE_MEMBER_FOR_TAG` or `CALLOUT_FAILED`. Nor does
an internal error whose cause is the client having gone away.

In the default response mode the `detail` of such an error is
`SERVER_ERROR: internal error [ticket: <uuid>]`; the same id is in `ticket`.
The cause never reaches the client.

cyoda-go: the field is `ProblemDetail.Ticket` (`json:"ticket,omitempty"`) at
`internal/common/errors.go:303`, set for an internal and a fatal error at
`internal/common/errors.go:396` and `:414`. A recovered panic pins the id it
logged (`internal/api/middleware/recovery.go:62`).

## Cloud today

Cloud's `ProblemDetail` (`docs/cyoda/openapi.yml`, schema `ProblemDetail`) has
`type`, `title`, `status`, `detail`, `instance` and `properties` and no
`ticket`; no source file of the Cloud backend (`backend/src/main`) contains the
word `ticket`.

## What Cloud changes

On an unexpected server error (500), send `ticket` (a UUID) and log the
failure under the same id. Declare `ticket` in `ProblemDetail`. A client generated from cyoda-go's
document then has a typed field for it.
