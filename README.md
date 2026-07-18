# fluid

Model-agnostic dynamic workflows for AI coding harnesses (GitHub Copilot, OpenAI
Codex, OpenCode) via MCP. A calling model drafts a JSON DAG of agent nodes;
fluid validates it, executes it in waves, and journals progress so interrupted
runs resume without repeating completed work.

Plan of record: Notion — "Plan v2 — Go implementation, phased harness rollout"
(child pages cover Azure/AWS/GCP scale-out and local deployment).
Work items: Linear project **Flow**.

## Layout

- `spec/` — the language-neutral DAG contract: types, validation (identity,
  dangling references, caps, cycles), and topological execution-wave
  computation. Stdlib only.
- `runtime/` — local wave executor with a JSONL resume journal. Stdlib only.
  Shaped like a durable-task orchestration so a `durabletask-go` engine
  (SQLite / Azure DTS / Postgres backends) can replace it behind the same
  interface.
- `cmd/flow-mcp/` — MCP server entrypoint (not yet implemented).

## Develop

```bash
go vet ./...
go test ./... -race
```
