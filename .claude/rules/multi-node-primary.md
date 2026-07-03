# cyoda-go is primarily multi-node

Cluster mode is the primary operating target. `CYODA_CLUSTER_ENABLED=false` is
the default only to make getting started easy — it is NOT a signal that
cluster/HA features are secondary or descopable. Do not defer or descope
multi-node correctness (proxy routing, tx-affinity, cross-node callback join,
peer failover) on proportionality grounds. Design cross-node correctness in
from the start; reviewers must not treat single-node as "the common case".
