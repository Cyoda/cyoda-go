# cyoda Helm charts

| Chart | Purpose |
|-------|---------|
| [cyoda/](./cyoda/) | Production Helm deployment |

The chart is published to the Helm repository `https://cyoda.github.io/cyoda-go`
(`helm repo add cyoda https://cyoda.github.io/cyoda-go`, then `cyoda/cyoda`).
Its chart version and app version are in [`cyoda/Chart.yaml`](./cyoda/Chart.yaml).

See the per-chart `README.md` for operator documentation, values schema,
and upgrade notes.

For the overall provisioning design, see
`docs/superpowers/specs/2026-04-17-provisioning-helm-design.md`.
