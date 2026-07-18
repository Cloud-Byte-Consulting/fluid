# GCP deployment — MCP local, state in the cloud

Like AWS, GCP has no managed Durable Task Framework backend — hybrid mode is
assembled. GCP's saving grace: the Cloud SQL Go connector makes
laptop-to-cloud-state connectivity the cleanest of the three clouds.

## Route A — same engine, cloud state (recommended)

The same Postgres backend as AWS (public, pluggable `durabletask-go` backend
interface), pointed at **Cloud SQL for PostgreSQL** or **AlloyDB**. The
official [cloud-sql-go-connector](https://github.com/GoogleCloudPlatform/cloud-sql-go-connector)
dials Cloud SQL securely from anywhere — automatic mTLS and **IAM database
authentication** with short-lived tokens. No VPN, no password management, no
manually exposed endpoint: a laptop authenticates with
`gcloud auth application-default login` and the connector does the rest.

```text
FLOW_BACKEND=postgres
FLOW_PG_INSTANCE=<project>:<region>:<instance>   # via cloud-sql-go-connector
FLOW_PG_DATABASE=fluid
```

Best practices: IAM database authentication over built-in passwords;
Application Default Credentials locally, service-account identity for cloud
workers; private IP + the connector in production; least-privilege Cloud SQL
roles per database.

## Route B — GCP-native orchestration: Cloud Workflows + Pub/Sub + callbacks

Cloud Workflows is YAML-defined serverless orchestration with **callbacks**:
a workflow mints a callback endpoint and pauses (up to a year) until an
external system POSTs the result. The local-worker pattern: workflow
publishes each node's work item to **Pub/Sub** → the laptop worker
subscribes and executes → worker calls the callback URL with the result.
Workable, but three services glued together plus a second compiler target
(DAG → Workflows YAML), with higher per-step dispatch latency than a polling
gRPC worker. Only worth it under a GCP-native mandate.

## Route C — Temporal (self-hosted on GKE, or Temporal Cloud)

Identical reasoning to the AWS doc: best-in-class Go SDK, exact DTS shape,
at the cost of being a second engine. The engine-swap fallback.

## Mode mapping vs Azure

| Mode | Azure | GCP equivalent |
| --- | --- | --- |
| Local | embedded engine + JSONL/SQLite | identical — no cloud involved |
| Hybrid | DTS task hub (gRPC, Entra ID) | Postgres backend on Cloud SQL/AlloyDB via cloud-sql-go-connector (IAM auth) |
| Cloud scale | ACA/AKS workers + DTS | Cloud Run/GKE workers + Cloud SQL, or Workflows + Pub/Sub + callbacks, or Temporal |
| Dashboard | managed DTS dashboard | none built-in for Route A; Workflows console for B; Temporal Web UI for C |

## Cross-cloud summary

Azure is the only provider with a managed Durable Task backend — hence the
first scale-out iteration. The recommended AWS/GCP path is one shared
Postgres backend (which also runs on Azure Flexible Server), keeping the MCP
contract and DAG spec identical everywhere: **one backend, three clouds**,
with Azure DTS as the managed premium option.
