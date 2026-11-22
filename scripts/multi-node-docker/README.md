# Multi-node docker dev cluster

Developer/test tool for spinning up cyoda-go containers behind an
nginx load balancer. Not a canonical provisioning artifact — use
`deploy/` for that.

## Usage

    ./start-cluster.sh --nodes 3                  # postgres (default)
    ./start-cluster.sh --nodes 1 --profile sqlite
    ./start-cluster.sh --nodes 1 --profile memory
    ./stop-cluster.sh

## Profiles

| Profile    | Backend     | Multi-node | State lives in |
|------------|-------------|------------|----------------|
| `postgres` | Postgres 17 | yes (gossip-configured cluster) | named volume `pgdata` |
| `sqlite`   | SQLite      | **no** (single-node only) | named volume `cyoda-data` |
| `memory`   | In-memory   | **no** (single-node only) | container memory (lost on restart) |

Only `postgres` shares state across nodes. With `sqlite` or `memory`,
each container has isolated storage, so `--nodes >1` is rejected.

## Per-profile overrides

Drop a `.env.<profile>` file in this directory to pin cluster secrets
instead of letting the script auto-generate them. Loaded *before* the
base `.env`, so these override anything persisted from a previous run.
Example — `.env.postgres`:

    CYODA_JWT_SIGNING_KEY=...base64-PEM...
    CYODA_HMAC_SECRET=...

These are the only vars the overlay currently plumbs through — arbitrary
`CYODA_*` vars beyond this list are not propagated to the container.

Base `.env` (auto-generated on first run) caches the resolved values for
stability across restarts. Delete it to regenerate.

## First admin token

Each node holds the signing key, so any node can mint the first admin
token:

    docker exec minicyoda-node1 /cyoda token --tenant <tenant>

Use it to create M2M clients: `POST /api/clients`.
