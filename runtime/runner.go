// Package runtime executes a validated DagSpec in execution waves with a
// durable append-only journal, so an interrupted run resumes without
// repeating completed work.
//
// This is the local, dependency-free engine. Its Run/journal semantics are
// deliberately shaped like a durable-task orchestration (waves == whenAll
// fan-out, journal == history replay) so a durabletask-go backed engine can
// replace it behind the same interface.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/Cloud-Byte-Consulting/fluid/spec"
)

// NodeRun is everything an executor needs to run one node.
type NodeRun struct {
	RunID  string
	Node   spec.WorkflowNode
	Model  string                     // node model resolved against DefaultModel
	Inputs map[string]json.RawMessage // results of the node's dependencies
}

// NodeResult is a node's output plus its reported token usage.
type NodeResult struct {
	Output json.RawMessage
	Tokens int
}

// NodeFunc executes one node.
type NodeFunc func(ctx context.Context, nr NodeRun) (NodeResult, error)

// Transient marks err as retryable (rate limits, timeouts, 5xx).
// Unwrapped errors are treated as permanent and fail fast.
func Transient(err error) error { return transientErr{err} }

type transientErr struct{ error }

func (t transientErr) Unwrap() error { return t.error }

// IsTransient reports whether err was marked with Transient.
func IsTransient(err error) bool {
	var t transientErr
	return errors.As(err, &t)
}

// Runner executes DAGs and journals progress under Dir, one JSONL file per run.
type Runner struct {
	Dir     string
	Exec    NodeFunc
	Backoff func(attempt int) time.Duration // nil = default exponential backoff
}

func defaultBackoff(attempt int) time.Duration {
	d := 250 * time.Millisecond << (attempt - 1)
	if d > 4*time.Second {
		d = 4 * time.Second
	}
	return d
}

// Announce durably records that runID exists (run_started) without executing
// anything. Callers that detach Run into a goroutine call this first so the
// run is immediately observable via Store.Status. Safe to call before Run:
// Run will not write a second run_started.
func (r *Runner) Announce(d *spec.DagSpec, runID string) error {
	events, err := readJournal(journalPath(r.Dir, runID))
	if err != nil {
		return err
	}
	for _, e := range events {
		if e.Event == "run_started" {
			return nil
		}
	}
	journal, err := os.OpenFile(journalPath(r.Dir, runID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer journal.Close()
	return appendEvent(journal, event{Event: "run_started", Name: d.Name, Total: len(d.Nodes)})
}

// Run executes d under runID. Calling Run again with the same runID resumes:
// journaled node results are reused and completed nodes never re-execute.
// A run that previously completed returns its results without executing anything.
func (r *Runner) Run(ctx context.Context, d *spec.DagSpec, runID string) (map[string]json.RawMessage, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	waves, err := d.Waves()
	if err != nil {
		return nil, err
	}
	events, err := readJournal(journalPath(r.Dir, runID))
	if err != nil {
		return nil, err
	}
	done := make(map[string]json.RawMessage)
	spent := 0
	hadStart, completed := false, false
	for _, e := range events {
		switch e.Event {
		case "run_started":
			hadStart = true
		case "node_done":
			done[e.Node] = e.Result
			spent += e.Tokens
		case "run_done":
			completed = true
		}
	}
	if completed {
		return done, nil // idempotent: finished runs never re-execute
	}

	journal, err := os.OpenFile(journalPath(r.Dir, runID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	defer journal.Close()

	var mu sync.Mutex // guards done, spent, and journal writes
	logf := func(e event) error {
		mu.Lock()
		defer mu.Unlock()
		return appendEvent(journal, e)
	}
	if !hadStart {
		if err := logf(event{Event: "run_started", Name: d.Name, Total: len(d.Nodes)}); err != nil {
			return nil, err
		}
	}

	backoff := r.Backoff
	if backoff == nil {
		backoff = defaultBackoff
	}
	nodes := make(map[string]spec.WorkflowNode, len(d.Nodes))
	for _, n := range d.Nodes {
		nodes[n.ID] = n
	}
	sem := make(chan struct{}, d.Caps.MaxConcurrent)

	for _, wave := range waves {
		if ctx.Err() != nil {
			logf(event{Event: "run_cancelled"})
			return nil, fmt.Errorf("run cancelled: %w", ctx.Err())
		}
		mu.Lock()
		over := spent >= d.Caps.TokenBudget
		mu.Unlock()
		if over {
			reason := fmt.Sprintf("budget_exceeded: spent %d of %d tokens", spent, d.Caps.TokenBudget)
			logf(event{Event: "run_failed", Reason: reason})
			return nil, errors.New(reason)
		}

		// Build the wave's jobs up front: all reads of done happen before any
		// of this wave's goroutines (which write done) are launched.
		var jobs []NodeRun
		for _, id := range wave {
			if _, ok := done[id]; ok {
				continue // completed in a previous attempt
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
				errs[i] = r.runNode(ctx, job, d.Caps.MaxRounds, backoff, logf, func(res NodeResult) {
					mu.Lock()
					defer mu.Unlock()
					done[job.Node.ID] = res.Output
					spent += res.Tokens
				})
			}(i, job)
		}
		wg.Wait()
		for _, err := range errs {
			if err == nil {
				continue
			}
			if ctx.Err() != nil {
				logf(event{Event: "run_cancelled"})
				return nil, fmt.Errorf("run cancelled: %w", ctx.Err())
			}
			logf(event{Event: "run_failed", Reason: safeMsg(err)})
			return nil, err // journal keeps completed work; rerun resumes
		}
	}
	if err := logf(event{Event: "run_done"}); err != nil {
		return nil, err
	}
	return done, nil
}

// runNode drives one node through its attempt loop: transient errors retry
// with backoff up to maxRounds; permanent errors fail fast.
func (r *Runner) runNode(ctx context.Context, job NodeRun, maxRounds int, backoff func(int) time.Duration, logf func(event) error, commit func(NodeResult)) error {
	id := job.Node.ID
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("node %q: cancelled: %w", id, err)
		}
		if err := logf(event{Event: "node_started", Node: id, Attempt: attempt}); err != nil {
			return err
		}
		res, err := r.Exec(ctx, job)
		if err == nil {
			commit(res)
			return logf(event{Event: "node_done", Node: id, Attempt: attempt, Result: res.Output, Tokens: res.Tokens})
		}
		logf(event{Event: "node_failed", Node: id, Attempt: attempt, Error: safeMsg(err)})
		if !IsTransient(err) {
			return fmt.Errorf("node %q: %w", id, err)
		}
		if attempt >= maxRounds {
			return fmt.Errorf("node %q: rounds_exhausted after %d attempts: %w", id, attempt, err)
		}
		select {
		case <-time.After(backoff(attempt)):
		case <-ctx.Done():
			return fmt.Errorf("node %q: cancelled during backoff: %w", id, ctx.Err())
		}
	}
}
