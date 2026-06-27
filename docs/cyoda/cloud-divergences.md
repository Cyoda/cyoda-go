# Cloud divergences

cyoda-go defines the API and integration contract; Cyoda Cloud aligns
to it. Most of the API surface matches one-for-one. This page tracks
the **deliberate, known divergences** — fields cyoda-go declares in
`api/openapi.yaml` but does not yet implement, behavior that
intentionally differs, or enterprise-tier features that live only in
the commercial backend. For features Cloud exposes that cyoda-go has
not yet implemented, see [`../cloud-parity/`](../cloud-parity/).

This is the canonical place for "I see this in the OpenAPI spec but
cyoda-go silently ignores it" entries. Add new rows here whenever a
divergence is identified.

| Surface | Divergence | Status | Tracking |
|---|---|---|---|
| `ProcessorDefinitionDto.asyncResult` | Field declared in OpenAPI; OSS backend rejects `asyncResult=true` at workflow import (400 VALIDATION_FAILED). The explicit `asyncResult=false` and absent cases are accepted and round-tripped. Crossover semantics need durable suspend state + cluster-wide work-stealing recovery + a distributed timer — implementable only in the commercial backend. | Reject-at-import on OSS; enterprise-tier in the commercial backend (not yet implemented there either). | [#223](https://github.com/cyoda/cyoda-go/issues/223) |
| `ProcessorDefinitionDto.crossoverToAsyncMs` | Field declared in OpenAPI; OSS backend rejects any non-nil `crossoverToAsyncMs` at workflow import (400 VALIDATION_FAILED), including the orphan case where `asyncResult` is absent or false. See `asyncResult` — same parity gap. | Reject-at-import on OSS; enterprise-tier in the commercial backend. | [#223](https://github.com/cyoda/cyoda-go/issues/223) |
| Remaining 501 Not Implemented endpoints | Declared in OpenAPI but unhandled at runtime. As of v0.8.0 the keypair + trusted-key (`/oauth/keys/*`), OIDC provider (`/oauth/oidc/providers/*`), and `/clients` surfaces are conformant; the 1 endpoint below still returns 501 via `internal/api/unimplemented.go`. Re-derive with the snippet beneath this table whenever IAM/account surfaces move. | Deferred. | [#194](https://github.com/cyoda/cyoda-go/issues/194) |
| `EdgeMessage.payload` content types beyond JSON | OpenAPI's `contentType` field suggests support for non-JSON; cyoda-go currently stores/returns JSON-encoded values only. Cloud has the same restriction today. | Future feature, would lead Cloud. | [#193](https://github.com/cyoda/cyoda-go/issues/193) |

## Current 501 snapshot

Re-run the derivation below to refresh this list.

- GET /account/subscriptions

## 501 endpoint derivation

Spin up a fresh `cyoda` binary in JWT mode and probe every spec operation; record those that still respond `501`. The snippet is canonical; the snapshot above is its current output.

```bash
# 1. Build and launch in JWT mode against an ephemeral sqlite store.
go build -o bin/cyoda ./cmd/cyoda
export CYODA_STORAGE_BACKEND=sqlite
export CYODA_SQLITE_PATH=$(mktemp -t cyoda-stubs-XXXXXX.db)
export CYODA_IAM_MODE=jwt
export CYODA_JWT_SIGNING_KEY="$(openssl genrsa 2048 2>/dev/null)"
export CYODA_BOOTSTRAP_CLIENT_ID=stub-probe
export CYODA_BOOTSTRAP_CLIENT_SECRET=stub-probe-secret
export CYODA_HTTP_PORT=18080
export CYODA_ADMIN_PORT=18081
export CYODA_GRPC_PORT=18082          # avoid the default 9090 clashing with a parallel instance
export CYODA_SUPPRESS_BANNER=true
bin/cyoda serve &
CYODA_PID=$!
sleep 3

# 2. Acquire a bootstrap token.
TOKEN=$(curl -fsS -u "$CYODA_BOOTSTRAP_CLIENT_ID:$CYODA_BOOTSTRAP_CLIENT_SECRET" \
  -d "grant_type=client_credentials" \
  http://127.0.0.1:18080/api/oauth/token \
  | python3 -c 'import json,sys;print(json.load(sys.stdin)["access_token"])')

# 3. Probe every (method, path) tuple in the spec and record those returning 501.
TOKEN="$TOKEN" python3 - <<'PYEOF'
import os, re, urllib.request, urllib.error, yaml
with open("api/openapi.yaml") as f:
    spec = yaml.safe_load(f)
token = os.environ.get("TOKEN", "")
UUID = "00000000-0000-0000-0000-000000000000"
def placeholder_for(schema):
    if not schema: return "x"
    t, fmt, enum = schema.get("type"), schema.get("format"), schema.get("enum")
    if enum: return str(enum[0])
    if t == "integer": return "1"
    if t == "string": return UUID if fmt == "uuid" else "probe"
    return "x"
stubs = []
for path, methods in spec.get("paths", {}).items():
    path_params = {p["name"]: p for p in methods.get("parameters", []) if p.get("in") == "path"}
    for method, op in methods.items():
        if method in ("parameters", "summary", "description") or not isinstance(op, dict):
            continue
        params_by_name = dict(path_params)
        for p in op.get("parameters", []) or []:
            if p.get("in") == "path":
                params_by_name[p["name"]] = p
        concrete = path
        for m in re.findall(r"\{([^}]+)\}", path):
            schema = (params_by_name.get(m) or {}).get("schema", {})
            concrete = concrete.replace("{" + m + "}", placeholder_for(schema))
        url = f"http://127.0.0.1:18080/api{concrete}"
        req = urllib.request.Request(url, method=method.upper())
        req.add_header("Authorization", f"Bearer {token}")
        if method.upper() in ("POST", "PUT", "PATCH"):
            req.add_header("Content-Type", "application/json"); req.data = b"{}"
        try: urllib.request.urlopen(req, timeout=2); code = 200
        except urllib.error.HTTPError as e: code = e.code
        except Exception: code = -1
        if code == 501: stubs.append(f"- {method.upper()} {path}")
for s in sorted(stubs): print(s)
PYEOF

# 4. Cleanup.
kill $CYODA_PID 2>/dev/null
wait $CYODA_PID 2>/dev/null
```

The probe substitutes `{format}`/`{converter}`/other enum path parameters with their first enum value, `{modelVersion}` with `1`, UUID-typed parameters with the zero UUID, and other string parameters with `probe`. Path parameters declared with `enum` constraints are honoured so the request reaches the handler rather than failing spec validation. Paste the script's stdout into the "Current 501 snapshot" section above, replacing the previous list.

## Adding a row

When you discover a divergence:

1. File a tracking issue (or reference an existing one).
2. Add a row above with: surface, what diverges, current status (silent-ignore / partial-impl / deferred / enterprise-only), tracking issue.
3. If the divergence is silently ignored, add a `⚠️` note to the OpenAPI field's `description` so SDK consumers see the gap at the spec layer too.

## Why we keep declaring fields we don't implement

Per ADR 0001 (`docs/adr/0001-openapi-server-spec-conformance.md`), our
spec mirrors Cloud's so client SDKs generated against either spec are
shape-compatible. Removing fields from the spec to match server
behavior would break that compatibility for clients moving between
deployments. Keeping the field declared with a `⚠️` divergence note is
the lesser evil.

## Anti-pattern

Never silently flip server behavior to *match* a Cloud field whose
shape we declare but whose runtime semantics we don't implement.
Either:

- Implement the field properly (preferred), OR
- Document the divergence here AND in the OpenAPI description (current
  policy for the rows above).

The "silently honor a fraction of the field" middle ground is what
this document exists to prevent.
