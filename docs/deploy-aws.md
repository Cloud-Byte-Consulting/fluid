# AWS deployment — MCP local, state in the cloud

AWS has no managed Durable Task Framework backend, so the equivalence is
assembled rather than bought. Three routes, in recommendation order.

## Route A — same engine, cloud state (recommended)

`durabletask-go`'s backend is a public, pluggable interface (the in-repo
SQLite backend is the template; a community libsql backend proves the port
path). A **Postgres backend** pointed at **Aurora Serverless v2 or RDS for
PostgreSQL** gives the hybrid shape: engine and MCP server on the laptop,
orchestration state in AWS. One bounded implementation serves AWS, GCP, and
Azure (Flexible Server).

```text
FLOW_BACKEND=postgres
FLOW_PG_DSN=postgres://fluid@<cluster>.<region>.rds.amazonaws.com:5432/fluid?sslmode=require
```

Best practices:

- **IAM database authentication** — short-lived tokens instead of passwords
  (`aws rds generate-db-auth-token`); Secrets Manager only where IAM auth
  doesn't fit.
- **TLS required**; RDS Proxy for connection pooling once a worker fleet
  joins.
- The honest friction point is **laptop → RDS connectivity**: RDS lives in a
  VPC, so either a public endpoint locked to your IPs by security group +
  TLS, or a VPN/SSM tunnel. There is no equivalent of DTS's "dial a public
  gRPC endpoint with cloud-native auth."

## Route B — AWS-native orchestration: Step Functions Activities

Step Functions **Activities** are the architectural analog of DTS work-item
dispatch: a worker anywhere (including a laptop) long-polls
`GetActivityTask` over HTTPS, receives a task + token, does the work, and
reports `SendTaskSuccess`/`SendTaskFailure` with heartbeats. Standard
Workflows run up to a year; billing per state transition.

Cost for fluid: a second compiler target — the DAG would compile to ASL
(`Parallel`/`Map` states) — plus divergent retry/history semantics to keep
conformant. Choose this only under an AWS-native mandate.

## Route C — Temporal (self-hosted on EKS, or Temporal Cloud)

Closest *shape* match to DTS on any cloud: workers poll task queues over
gRPC, state lives in the cluster, and the Temporal **Go SDK is first-class
and production-grade**. Trade-off: it's a second engine — adopting it means
maintaining two orchestration integrations or replacing durabletask-go
everywhere. The fallback if durabletask-go maturity becomes a real problem.

## Mode mapping vs Azure

| Mode | Azure | AWS equivalent |
| --- | --- | --- |
| Local | embedded engine + JSONL/SQLite | identical — no cloud involved |
| Hybrid | DTS task hub (gRPC, Entra ID) | Postgres backend on Aurora/RDS (IAM auth) |
| Cloud scale | ACA/AKS workers + DTS | ECS/EKS workers + Aurora, or Step Functions Activities, or Temporal |
| Dashboard | managed DTS dashboard | none built-in for Route A (use `get_workflow_status` / SQL); Step Functions console for B; Temporal Web UI for C |
