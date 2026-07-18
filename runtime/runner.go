// Package runtime executes a validated DagSpec in execution waves with a
// durable append-only journal, so an interrupted run resumes without
// repeating completed work.
//
// This is the local, dependency-free engine. Its Run/journal semantics are
// deliberately shaped like a durable-task orchestration (waves == whenAll
// fan-out, journal == history replay) so a durabletask-go backed engine can
// replace it behind the same call without changing callers.
package runtime

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/Cloud-Byte-Consulting/fluid/spec"
)

// NodeRun is everything an executor needs to run one node.
type NodeRun struct {
	RunID  string
	Node   spec.WorkflowNode
	Model  string                     // node model resolved against DefaultModel
	Inputs map[string]json.RawMessage // results of the node's dependencies
}

// NodeFunc executes one node and returns its JSON result.
type NodeFunc func(ctx context.Context, nr NodeRun) (json.RawMessage, error)

// Runner executes DAGs and journals progress under Dir, one JSONL file per run.
type Runner struct {
	Dir  string
	Exec NodeFunc
}

type event struct {
	Event  string          `json:"event"` // "node_done" | "run_done"
	Node   string          `json:"node,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
}

// Run executes d under runID. Calling Run again with the same runID resumes:
// nodes recorded as done in the journal are skipped and their journaled
// results are fed to dependents.
func (r *Runner) Run(ctx context.Context, d *spec.DagSpec, runID string) (map[string]json.RawMessage, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	waves, err := d.Waves()
	if err != nil {
		return nil, err
	}
	done, err := r.load(runID)
	if err != nil {
		return nil, err
	}
	journal, err := os.OpenFile(r.path(runID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	defer journal.Close()

	nodes := make(map[string]spec.WorkflowNode, len(d.Nodes))
	for _, n := range d.Nodes {
		nodes[n.ID] = n
	}

	var mu sync.Mutex // guards done + journal writes
	sem := make(chan struct{}, d.Caps.MaxConcurrent)

	for _, wave := range waves {
		// Build the wave's jobs up front: all reads of done happen before any
		// of this wave's goroutines (which write done) are launched.
		var jobs []NodeRun
		for _, id := range wave {
			if _, ok := done[id]; ok {
				continue // already completed in a previous attempt
			}
			n := nodes[id]
			inputs := make(map[string]json.RawMessage, len(n.DependsOn))
			for _, dep := range n.DependsOn {
				inputs[dep] = done[dep]
			}
			model := n.Model
			if model == "" {
				model = d.DefaultModel
			}
			jobs = append(jobs, NodeRun{RunID: runID, Node: n, Model: model, Inputs: inputs})
		}
		var wg sync.WaitGroup
		errs := make([]error, len(jobs))
		for i, job := range jobs {
			wg.Add(1)
			go func(i int, job NodeRun) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				result, err := r.Exec(ctx, job)
				if err != nil {
					errs[i] = fmt.Errorf("node %q: %w", job.Node.ID, err)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				done[job.Node.ID] = result
				errs[i] = writeEvent(journal, event{Event: "node_done", Node: job.Node.ID, Result: result})
			}(i, job)
		}
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				return nil, err // journal keeps completed work; rerun resumes
			}
		}
	}
	if err := writeEvent(journal, event{Event: "run_done"}); err != nil {
		return nil, err
	}
	return done, nil
}

func (r *Runner) path(runID string) string {
	return filepath.Join(r.Dir, runID+".jsonl")
}

// load replays the journal for runID into a node→result map.
func (r *Runner) load(runID string) (map[string]json.RawMessage, error) {
	done := make(map[string]json.RawMessage)
	f, err := os.Open(r.path(runID))
	if os.IsNotExist(err) {
		return done, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		var e event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return nil, fmt.Errorf("corrupt journal %s: %w", r.path(runID), err)
		}
		if e.Event == "node_done" {
			done[e.Node] = e.Result
		}
	}
	return done, sc.Err()
}

func writeEvent(f *os.File, e event) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}
