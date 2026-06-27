---
topic: errors.UNSUPPORTED_MEDIA_TYPE
title: "UNSUPPORTED_MEDIA_TYPE — PATCH format or Content-Type not supported"
stability: stable
see_also:
  - errors
  - errors.NOT_IMPLEMENTED
  - errors.BAD_REQUEST
---

# errors.UNSUPPORTED_MEDIA_TYPE

## NAME

UNSUPPORTED_MEDIA_TYPE — the PATCH request used a format or `Content-Type` that is not supported.

## SYNOPSIS

HTTP: `415` `Unsupported Media Type`. Retryable: `no`.

## DESCRIPTION

Returned by PATCH in two situations:

- The `{format}` path segment is not `JSON`. Merge patch is JSON-only; other format values (e.g. `CSV`) are not accepted for PATCH requests.
- The `Content-Type` header names a patch dialect that is not recognised. Only `application/merge-patch+json` (RFC 7386) is implemented.

Note that `application/json-patch+json` (RFC 6902, JSON Patch) is a recognised dialect but is not implemented; requests using that content type receive `501 NOT_IMPLEMENTED`, not `415`.

Not retryable as-is — the same request produces the same error until the format or content type is corrected.

## RECOVERY

Use `JSON` as the `{format}` path segment and set `Content-Type: application/merge-patch+json` in the request. Merge patch (RFC 7386) describes the delta as a JSON object whose keys are merged into the stored entity; keys present in the patch overwrite stored values, and keys explicitly set to `null` remove the field.

## SEE ALSO

- errors
- errors.NOT_IMPLEMENTED
- errors.BAD_REQUEST
