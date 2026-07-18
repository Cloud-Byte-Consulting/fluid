# Local deployment

The default mode: everything on one machine, zero cloud footprint. One static
binary, one journal directory.

## Install

```bash
# release binary (macOS arm64 example)
curl -Lo /usr/local/bin/flow-mcp \
  https://github.com/Cloud-Byte-Consulting/fluid/releases/latest/download/flow-mcp_<tag>_darwin_arm64
chmod +x /usr/local/bin/flow-mcp

# or from source
go install github.com/Cloud-Byte-Consulting/fluid/cmd/flow-mcp@latest

flow-mcp version
```

## Configure your harness

See the README for the exact Copilot / Codex / OpenCode snippets. Rules that
hold everywhere: the harness spawns `flow-mcp` over stdio and owns its
environment; `ANTHROPIC_API_KEY` / `OPENAI_API_KEY` go in the harness's MCP
`env` block; reload the harness after editing config.

## Using it

You never run `flow-mcp` interactively — you talk to your harness:

1. *"Fan out a security review across auth/, api/, and billing/ with one
   agent each, then a fourth agent that synthesizes a ranked findings list."*
2. The model drafts the DAG and calls `run_workflow` (preview first if it's
   being careful); you approve via the harness's native prompt.
3. It confirms, gets a `run_id` back instantly, and polls
   `get_workflow_status` while the run executes in the background.
4. Interrupted — laptop slept, harness crashed, Ctrl-C? Completed node work
   is journaled. In any later session: *"resume run \<run_id\>"* → the model
   calls `run_workflow` with that `run_id`, and only unfinished nodes execute.

## Where things live

| What | Where |
| --- | --- |
| Run state & history | `~/.fluid/run-*.jsonl` (override dir: `FLOW_STATE_DIR`) |
| Diagnostics | stderr → your harness's MCP log; stdout is protocol-only |
| Credentials | harness MCP `env` only — fluid never stores them |
| Cleanup | `flow-mcp prune -days 30` (terminal runs only; never touches running runs) |

Backup is `cp ~/.fluid/*.jsonl`. A corrupt journal shows up in
`list_workflows` as state `corrupt` instead of breaking the listing.

## Constraints

- Tool calls always return fast (well under Codex's 60 s default timeout);
  nothing blocks on run completion.
- "Offline" means the orchestrator works offline; node execution still needs
  network to reach provider APIs.
- One machine is one concurrency ceiling — when runs outgrow the laptop, see
  the cloud deployment docs.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| Tools not discoverable | Binary on PATH? Run `flow-mcp` by hand — it should idle silently. Harness MCP logs show spawn errors. |
| Server starts then dies | stderr in the harness MCP log; usually a bad env block or unwritable state dir. |
| Run stuck in `running` after a crash | That's a stale journal — resume it (`run_workflow` with `run_id`) or prune it. |
| Node keeps failing | `get_workflow_status` shows the per-node error: `missing_credential`, `schema_violation`, `rounds_exhausted`, or the provider message. |
