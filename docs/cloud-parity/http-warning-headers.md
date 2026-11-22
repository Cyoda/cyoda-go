# HTTP responses carry warnings in `Cyoda-Warning` headers

cyoda-go is the authoritative implementation. This file is the contract Cyoda
Cloud follows.

## Behaviour

A request can raise warnings: conditions that did not fail the operation but
that the client should know about. Examples are an entity run by the default
workflow because no imported workflow applies to it, and a compute member's
own warnings, prefixed with the callout's kind and name (`processor <name>: `,
`criteria <name>: `, `function <name>: `). A gRPC response carries them in its
`warnings` array, as before. An HTTP response now carries them too:

- **Header:** `Cyoda-Warning`, one header field line per warning, in the order
  the warnings were raised. Success and error (`application/problem+json`)
  responses carry them alike. A response that raised no warning has no
  `Cyoda-Warning` header.
- **Encoding:** a value is visible ASCII (0x21–0x7E) and space only. It is the
  warning's UTF-8 text, with every byte of a non-ASCII character, every `%` and
  a space at either end percent-encoded in upper-case hex (`%E2%80%94` for
  `—`). A standard percent-decoder gives back the text; `+` is a plus sign. A
  control character (C0 — CR, LF and tab among them — DEL, C1) is sent as a
  space, and invalid UTF-8 as U+FFFD. A compute member's text therefore can
  never end the header line or add a header of its own.
- **Bounds:** at most 16 warnings, at most 512 bytes in one encoded value, at
  most 2048 bytes in all encoded values together. A longer value is cut between
  characters and ends with `%E2%80%A6` (`…`). When a warning does not fit, it
  and every later one are left out and one more value,
  `further warnings were omitted`, says so. The bounds keep the header block
  inside a proxy's response-header buffer (nginx's `proxy_buffer_size` is
  4 KiB on most platforms). The gRPC `warnings` array is not cut to them.
- **Timing:** the warnings are added when the response starts. A warning raised
  after that cannot be sent.
- **Routes:** every data route — the generated API routes and the grouped
  statistics route. The text is the same as in the gRPC `warnings` array:
  nothing internal is added.
- **Cluster:** a request forwarded to the node that owns its transaction carries
  the owner's `Cyoda-Warning` headers unchanged.
- **CORS:** a response that carries `Access-Control-Allow-Origin` (not a
  preflight) also carries `Access-Control-Expose-Headers: Cyoda-Warning`, so
  that a browser script can read the header.

cyoda-go: `internal/httpmw/warnings.go` (`Warnings`, the encoding and the
bounds), wired in `app/app.go` on the grouped-statistics chain and in the chi
mux's middleware; the header name is `common.WarningHeader`
(`internal/common/diagnostics.go`); the CORS exposure is in
`internal/api/middleware/cors.go`.

## What changed

An HTTP response carried no warning at all: the request's diagnostics bag was
installed on the gRPC entry points only, so a warning raised on an HTTP request
was dropped. The transitions reads, which are HTTP-only, had no client-facing
signal for the default-workflow fallback.

## Cloud today

Cloud's REST API carries no warning. The word `warnings` appears in two files
of the Cloud backend (`backend/src/main`), both messaging (CloudEvent) handlers
(`EntityCreateRequestHandler.kt`, `SnapshotCancelRequestHandler.kt`); no source
file mentions a warning header, and the published OpenAPI documents
(`client/src/main/resources/api/*.yml`) do not contain the word `warning`.

## What Cloud changes

Send every warning a REST request raises in a `Cyoda-Warning` header field
line with the encoding and the bounds above, add `Cyoda-Warning` to
`Access-Control-Expose-Headers` where Cloud answers CORS, and describe the
header in the API description, as cyoda-go's `api/openapi.yaml` does (section
"Warnings").

## Tests

- E2E (PostgreSQL): `internal/e2e/http_warnings_test.go`
  (`TestHTTPWarnings_DefaultWorkflowFallback`,
  `TestHTTPWarnings_ComputeMemberWarnings`) and
  `internal/e2e/workflow_failure_test.go`
  (`TestWorkflowFailure_ProcessorWarnings`).
- Wiring: `app/http_warnings_wiring_test.go` (create and the transitions read).
- Unit: `internal/httpmw/warnings_test.go` (encoding, bounds, the writer);
  `internal/cluster/proxy/http_test.go`
  (`TestHTTPProxy_RelaysTheOwnersWarnings`);
  `internal/api/middleware/cors_test.go`
  (`TestCORS_ExposesTheWarningHeader`).
