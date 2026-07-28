# Developer helper scripts

Local-development helpers. These are NOT canonical provisioning
artifacts — the canonical artifacts live under `deploy/`.

- `run-local.sh` — run cyoda-go via `go run` using the `local`
  profile (in-memory storage, mock auth).
- `run-docker-dev.sh` — run cyoda-go + Postgres via docker compose
  for local development, with a fresh JWT signing key and a
  randomized bootstrap client secret per run.
- `compose.yaml` — PostgreSQL only, for running a locally built
  cyoda-go against the postgres backend on bare metal. Driven by the
  `make dev-*` targets; credentials and port match
  `.env.postgres.example`.

## Running against local PostgreSQL

```
make dev-up     # start PostgreSQL, wait until healthy
make dev-run    # build and run cyoda-go against it
make dev-down   # stop        (dev-reset also deletes the volume)
```

`deploy/docker/compose.yaml` is a different thing: the packaged
application in one container, sqlite-backed. Use this file when you want
to run cyoda-go yourself and only need the database.
