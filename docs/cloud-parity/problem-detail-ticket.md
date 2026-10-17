# A 5xx problem document carries a `ticket`

cyoda-go is the authoritative implementation. This file is the contract Cyoda
Cloud follows.

## Behaviour

Every 5xx problem document (`application/problem+json`) carries a top-level
`ticket`: a UUID that the server also wrote into its log line for the
underlying error. A caller quotes it; an operator finds the log line by it.
`ProblemDetail` declares the field (`type: string`, `format: uuid`). A 4xx
carries no `ticket`.

In the default response mode the `detail` of a 5xx is
`SERVER_ERROR: internal error [ticket: <uuid>]`; the same id is in `ticket`.
The cause never reaches the client.

cyoda-go: the field is `ProblemDetail.Ticket` (`json:"ticket,omitempty"`) at
`internal/common/errors.go:303`, set for an internal and a fatal error at
`internal/common/errors.go:396` and `:414`. A recovered panic pins the id it
logged (`internal/api/middleware/recovery.go:62`).

## Cloud today

Cloud's `ProblemDetail` (`docs/cyoda/openapi.yml`, schema `ProblemDetail`) has
`type`, `title`, `status`, `detail`, `instance` and `properties` and no
`ticket`; its 5xx documents carry no correlation id.

## What Cloud changes

On a 5xx, send `ticket` (a UUID) and log the underlying error under the same
id. Declare `ticket` in `ProblemDetail`. A client generated from cyoda-go's
document then has a typed field for it.
