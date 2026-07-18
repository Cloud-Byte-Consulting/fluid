package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"

	"github.com/Cloud-Byte-Consulting/fluid/runtime"
	"github.com/Cloud-Byte-Consulting/fluid/spec"
)

// Service holds fluid's tool implementations over the runtime.
type Service struct {
	Store  *runtime.Store
	Runner *runtime.Runner
	// Background is the context detached runs execute under — cancelling it
	// (e.g. on SIGTERM) cancels every in-flight run cleanly.
	Background context.Context

	mu     sync.Mutex
	active map[string]bool // run ids executing in this process
}

// Tools returns the three canonical fluid tools.
func (s *Service) Tools() []Tool {
	return []Tool{
		{
			Name: "run_workflow",
			Description: "Validate and run a workflow DAG of AI agent nodes. " +
				"Call with confirm=false (or omitted) to validate and preview the execution waves without running anything; " +
				"call with confirm=true to start the run — it returns a run_id immediately and executes in the background " +
				"(poll get_workflow_status). Pass run_id to resume a previously interrupted run without repeating completed nodes. " +
				"The dag_spec must satisfy this JSON Schema:\n" + string(spec.Schema()),
			InputSchema: json.RawMessage(`{
				"type": "object",
				"required": ["dag_spec"],
				"additionalProperties": false,
				"properties": {
					"dag_spec": {"type": "object", "description": "The workflow DAG (see schema in the tool description)."},
					"confirm": {"type": "boolean", "description": "false/omitted = preview only; true = start the run."},
					"run_id": {"type": "string", "description": "Resume this existing run instead of starting a new one."}
				}
			}`),
			Handler: s.runWorkflow,
		},
		{
			Name: "get_workflow_status",
			Description: "Get the status of a workflow run: run state (running|succeeded|failed|cancelled), per-node states " +
				"with attempts and timestamps, tokens spent, and failure reasons. Poll this after starting a run.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"required": ["run_id"],
				"additionalProperties": false,
				"properties": {"run_id": {"type": "string"}}
			}`),
			Handler: s.getStatus,
		},
		{
			Name: "list_workflows",
			Description: "List recent workflow runs, newest first: run_id, name, state, and node progress counts. " +
				"Optionally filter by state and cap the count with limit (default 20).",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"additionalProperties": false,
				"properties": {
					"state": {"type": "string", "enum": ["running", "succeeded", "failed", "cancelled"]},
					"limit": {"type": "integer"}
				}
			}`),
			Handler: s.listWorkflows,
		},
	}
}

func (s *Service) runWorkflow(_ context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		DagSpec json.RawMessage `json:"dag_spec"`
		Confirm bool            `json:"confirm"`
		RunID   string          `json:"run_id"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid_input: %w", err)
	}
	d, err := spec.Parse(args.DagSpec)
	if err != nil {
		return nil, fmt.Errorf("invalid_input: %w", err)
	}
	waves, err := d.Waves()
	if err != nil {
		return nil, fmt.Errorf("invalid_input: %w", err)
	}
	if !args.Confirm {
		return map[string]any{
			"status": "preview", "valid": true, "name": d.Name,
			"nodes": len(d.Nodes), "waves": waves, "caps": d.Caps,
			"next": "call run_workflow again with confirm=true to start",
		}, nil
	}
	runID := args.RunID
	if runID == "" {
		runID = s.Store.NewRunID()
	}
	s.mu.Lock()
	if s.active == nil {
		s.active = map[string]bool{}
	}
	if s.active[runID] {
		s.mu.Unlock()
		return nil, fmt.Errorf("already_running: run %q is executing; poll get_workflow_status", runID)
	}
	s.active[runID] = true
	s.mu.Unlock()

	// Record the run before returning so an immediate status poll finds it.
	if err := s.Runner.Announce(d, runID); err != nil {
		s.mu.Lock()
		delete(s.active, runID)
		s.mu.Unlock()
		return nil, fmt.Errorf("internal: %w", err)
	}

	bg := s.Background
	if bg == nil {
		bg = context.Background()
	}
	go func() {
		defer func() {
			if p := recover(); p != nil {
				log.Printf("run %s panicked: %v", runID, p)
			}
			s.mu.Lock()
			delete(s.active, runID)
			s.mu.Unlock()
		}()
		if _, err := s.Runner.Run(bg, d, runID); err != nil {
			log.Printf("run %s: %v", runID, err) // terminal state is in the journal
		}
	}()
	return map[string]any{
		"status": "started", "run_id": runID,
		"next": "poll get_workflow_status with this run_id",
	}, nil
}

func (s *Service) getStatus(_ context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid_input: %w", err)
	}
	return s.Store.Status(args.RunID)
}

func (s *Service) listWorkflows(_ context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		State string `json:"state"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid_input: %w", err)
	}
	if args.Limit <= 0 {
		args.Limit = 20
	}
	runs, err := s.Store.List()
	if err != nil {
		return nil, err
	}
	out := make([]runtime.RunSummary, 0, args.Limit)
	for _, r := range runs {
		if args.State != "" && r.State != args.State {
			continue
		}
		out = append(out, r)
		if len(out) >= args.Limit {
			break
		}
	}
	return map[string]any{"runs": out}, nil
}
