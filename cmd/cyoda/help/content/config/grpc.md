---
topic: config.grpc
title: "grpc configuration"
stability: stable
see_also:
  - config
  - grpc
  - run
---

# config.grpc

## NAME

config.grpc — gRPC listener settings and compute-node credentials.

## SYNOPSIS

cyoda exposes a gRPC endpoint for compute-node integration. The listener port is configured
via `CYODA_GRPC_PORT`. External compute nodes authenticate with `CYODA_COMPUTE_TOKEN` and
connect to the endpoint specified by `CYODA_COMPUTE_GRPC_ENDPOINT`.

## OPTIONS

### gRPC listener

- `CYODA_GRPC_PORT` — gRPC listen port (default: `9090`)

### Compute-node client

These variables are used by compute-node clients that connect to a running cyoda instance.

- `CYODA_COMPUTE_GRPC_ENDPOINT` — gRPC endpoint for the compute node to connect to,
  e.g. `localhost:9090` (required when running as a compute client)
- `CYODA_COMPUTE_TOKEN` — bearer token for compute-node authentication
  (required when running as a compute client)
- `CYODA_COMPUTE_HTTP_BASE` — HTTP base URL of the cyoda instance a compute node
  calls back into (e.g. to join the originating transaction); optional, enables
  callback-capable processors when set

## EXAMPLES

**Server (default port):**

```
CYODA_GRPC_PORT=9090
```

**Compute node client:**

```
CYODA_COMPUTE_GRPC_ENDPOINT=cyoda.internal:9090
CYODA_COMPUTE_TOKEN=my-token
```

## SEE ALSO

- config
- grpc
- run
