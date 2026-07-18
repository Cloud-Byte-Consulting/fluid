# Azure deployment — MCP local, state in the cloud

Azure is the first-class scale-out target because it has the one managed
service purpose-built for this shape: the **Azure Durable Task Scheduler
(DTS)** — a backend-as-a-service for the Durable Task Framework, the same
framework `durabletask-go` implements. Workers connect *outbound* over
gRPC/TLS to `{scheduler}.{region}.durabletask.io`; the scheduler owns
orchestration state, history, and work-item dispatch; orchestrations and
activities execute wherever the worker process runs — including a laptop.

> Status: the local engine in this repo is deliberately shaped like a
> durable-task orchestration so the DTS-backed engine can replace it behind
> the same `NodeFunc`/journal semantics. This doc is the deployment design of
> record for that iteration.

## Deployment modes

| Mode | MCP server | Workers | State |
| --- | --- | --- | --- |
| Local (default) | laptop | same process | `~/.fluid` JSONL |
| **Hybrid** | laptop | same process (+ any machine on the task hub) | DTS task hub |
| Cloud scale | laptop (client role) | Azure Container Apps / AKS fleet | DTS task hub |

Hybrid behavior: pending work items live in the scheduler, not the worker.
If the only worker goes offline mid-run, the run waits; when any authorized
worker reconnects to the task hub, dispatch resumes and completed history is
never re-executed. A teammate running the binary against the same task hub
can finish your run.

## Configuration

```text
FLOW_BACKEND=dts
FLOW_DTS_CONNECTION=Endpoint=https://<scheduler>.<region>.durabletask.io;Authentication=ManagedIdentity
FLOW_DTS_TASKHUB=fluid
```

Local dev/CI parity via the DTS emulator (no Azure subscription needed):

```bash
docker run -d -p 8080:8080 -p 8082:8082 \
  -e DTS_TASK_HUB_NAMES="fluid" mcr.microsoft.com/dts/dts-emulator:latest
# FLOW_DTS_CONNECTION=Endpoint=http://localhost:8080;Authentication=None
```

## Best practices (Microsoft Learn, verified 2026-07)

- **Identity:** Entra ID everywhere. Locally `DefaultAzureCredential`
  (`az login`); Azure-hosted workers use managed identity. Grant
  **Durable Task Data Contributor** scoped to the scheduler or a single task
  hub.
- **Isolation:** one task hub per user/team/environment:
  `az durabletask taskhub create -g <rg> --scheduler-name <s> --name <hub>`.
- **Networking:** TLS gRPC by default; private endpoints when runs must not
  transit the public internet.
- **Monitoring:** the managed DTS dashboard gives run inspection out of the
  box.
- **Billing:** Consumption (per action, 500 actions/s, 30-day retention) for
  dev and most teams; Dedicated CUs (2,000 actions/s/CU, 90-day retention,
  HA at 3 CUs) for heavy production. LLM DAGs are low-throughput —
  Consumption usually suffices.

## Caveats

- Microsoft Learn lists the self-hosted Go SDK as community-supported /
  experimental (first-class: .NET, Python, Java, JS). Hybrid mode is opt-in
  and CI validates against the emulator; if it becomes a blocker, the worker
  half can move to a first-class SDK language without touching the MCP
  contract.
- Data residency: node prompts/outputs persist in scheduler history for
  30–90 days. Scope RBAC per task hub; treat private endpoints as required
  for sensitive work.

## Alternative: cloud-neutral Postgres backend on Azure

The same Postgres backend that serves AWS/GCP (see those docs) runs on
**Azure Database for PostgreSQL – Flexible Server**: Entra-only auth (token
as password via `az account get-access-token --resource-type oss-rdbms`,
managed identity for cloud workers; tokens live 5–60 min, acquire per
connection), `sslmode=require`, private endpoints or firewall rules.

Choose **DTS** for Azure-first (zero ops, dashboard, per-action billing);
choose **Postgres** when cloud portability or SQL access to run history
matters more.
