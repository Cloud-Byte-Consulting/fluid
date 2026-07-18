package runtime

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Store derives run status entirely from journal replay — crash-consistent,
// no second source of truth.
type Store struct {
	Dir string
}

// NodeStatus is one node's derived lifecycle state.
type NodeStatus struct {
	State    string    `json:"state"` // running | succeeded | failed
	Attempts int       `json:"attempts"`
	Started  time.Time `json:"started,omitempty"`
	Finished time.Time `json:"finished,omitempty"`
	Error    string    `json:"error,omitempty"`
}

// RunStatus is a run's complete derived state.
type RunStatus struct {
	RunID       string                `json:"runId"`
	Name        string                `json:"name,omitempty"`
	State       string                `json:"state"` // running | succeeded | failed | cancelled
	Reason      string                `json:"reason,omitempty"`
	Started     time.Time             `json:"started,omitempty"`
	Finished    time.Time             `json:"finished,omitempty"`
	TokensSpent int                   `json:"tokensSpent"`
	NodesTotal  int                   `json:"nodesTotal"`
	Nodes       map[string]NodeStatus `json:"nodes"`
}

// RunSummary is the list-view projection of a run.
type RunSummary struct {
	RunID      string    `json:"runId"`
	Name       string    `json:"name,omitempty"`
	State      string    `json:"state"`
	Started    time.Time `json:"started,omitempty"`
	Finished   time.Time `json:"finished,omitempty"`
	NodesDone  int       `json:"nodesDone"`
	NodesTotal int       `json:"nodesTotal"`
}

// NewRunID returns a unique, filesystem-safe run id.
func (s *Store) NewRunID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return fmt.Sprintf("run-%s-%s", time.Now().UTC().Format("20060102-150405"), hex.EncodeToString(b))
}

// Status replays runID's journal into a RunStatus.
// Unknown run ids return an error containing "not_found".
func (s *Store) Status(runID string) (RunStatus, error) {
	events, err := readJournal(journalPath(s.Dir, runID))
	if err != nil {
		return RunStatus{}, err
	}
	if events == nil {
		return RunStatus{}, fmt.Errorf("not_found: run %q", runID)
	}
	st := RunStatus{RunID: runID, State: "running", Nodes: map[string]NodeStatus{}}
	for _, e := range events {
		switch e.Event {
		case "run_started":
			if st.Started.IsZero() {
				st.Started = e.T
				st.Name = e.Name
				st.NodesTotal = e.Total
			}
			st.State = "running"
		case "node_started":
			n := st.Nodes[e.Node]
			n.State = "running"
			n.Attempts = e.Attempt
			if n.Started.IsZero() {
				n.Started = e.T
			}
			st.Nodes[e.Node] = n
			st.State = "running" // activity after a terminal event means a resume
		case "node_done":
			n := st.Nodes[e.Node]
			n.State = "succeeded"
			n.Finished = e.T
			n.Error = ""
			st.Nodes[e.Node] = n
			st.TokensSpent += e.Tokens
		case "node_failed":
			n := st.Nodes[e.Node]
			n.State = "failed"
			n.Finished = e.T
			n.Error = e.Error
			st.Nodes[e.Node] = n
		case "run_done":
			st.State, st.Finished = "succeeded", e.T
		case "run_failed":
			st.State, st.Finished, st.Reason = "failed", e.T, e.Reason
		case "run_cancelled":
			st.State, st.Finished = "cancelled", e.T
		}
	}
	return st, nil
}

// List returns summaries for every run in the store, newest first. A missing
// store directory is an empty store; a corrupt journal appears as a summary
// with state "corrupt" rather than failing the whole listing.
func (s *Store) List() ([]RunSummary, error) {
	entries, err := os.ReadDir(s.Dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []RunSummary
	for _, ent := range entries {
		name := ent.Name()
		if ent.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		runID := strings.TrimSuffix(name, ".jsonl")
		st, err := s.Status(runID)
		if err != nil {
			out = append(out, RunSummary{RunID: runID, State: "corrupt"})
			continue
		}
		sum := RunSummary{
			RunID: runID, Name: st.Name, State: st.State,
			Started: st.Started, Finished: st.Finished, NodesTotal: st.NodesTotal,
		}
		for _, n := range st.Nodes {
			if n.State == "succeeded" {
				sum.NodesDone++
			}
		}
		out = append(out, sum)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Started.Equal(out[j].Started) {
			return out[i].Started.After(out[j].Started)
		}
		return out[i].RunID > out[j].RunID
	})
	return out, nil
}

// Prune deletes journals of terminal runs (succeeded/failed/cancelled) whose
// files are older than olderThan. Non-terminal journals are never touched.
func (s *Store) Prune(olderThan time.Duration) ([]string, error) {
	runs, err := s.List()
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().Add(-olderThan)
	var removed []string
	for _, r := range runs {
		if r.State == "running" {
			continue
		}
		path := journalPath(s.Dir, r.RunID)
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			if err := os.Remove(path); err != nil {
				return removed, err
			}
			removed = append(removed, r.RunID)
		}
	}
	return removed, nil
}

func journalPath(dir, runID string) string {
	return filepath.Join(dir, runID+".jsonl")
}
