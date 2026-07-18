// Package spec defines the language-neutral Flow DAG contract and its
// validation rules. The public contract is plain JSON; these types are its
// canonical Go representation.
package spec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
)

// DagSpec is a complete workflow definition submitted by a calling model.
type DagSpec struct {
	Version      string         `json:"version"`
	Name         string         `json:"name,omitempty"`
	DefaultModel string         `json:"defaultModel,omitempty"`
	Nodes        []WorkflowNode `json:"nodes"`
	Caps         RunCaps        `json:"caps"`
}

// WorkflowNode is one unit of agent work in the DAG.
type WorkflowNode struct {
	ID           string          `json:"id"`
	Instructions string          `json:"instructions"`
	Model        string          `json:"model,omitempty"` // falls back to DagSpec.DefaultModel
	Tools        []string        `json:"tools,omitempty"`
	DependsOn    []string        `json:"dependsOn,omitempty"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
}

// RunCaps bound a run before anything is dispatched.
type RunCaps struct {
	MaxNodes      int `json:"maxNodes"`
	MaxRounds     int `json:"maxRounds"`
	MaxConcurrent int `json:"maxConcurrent"`
	TokenBudget   int `json:"tokenBudget"`
}

// Parse decodes and validates a DagSpec from JSON in one step.
func Parse(data []byte) (*DagSpec, error) {
	var d DagSpec
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("invalid_input: %w", err)
	}
	if err := d.Validate(); err != nil {
		return nil, err
	}
	return &d, nil
}

var idRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Validate checks identity, reference, cap, and acyclicity rules.
// It reports every problem it finds, joined into one error.
func (d *DagSpec) Validate() error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if d.Version == "" {
		fail("version is required")
	}
	if len(d.Nodes) == 0 {
		fail("nodes must be non-empty")
	}
	if d.Caps.MaxNodes <= 0 || d.Caps.MaxRounds <= 0 || d.Caps.MaxConcurrent <= 0 || d.Caps.TokenBudget <= 0 {
		fail("caps must all be positive (maxNodes=%d maxRounds=%d maxConcurrent=%d tokenBudget=%d)",
			d.Caps.MaxNodes, d.Caps.MaxRounds, d.Caps.MaxConcurrent, d.Caps.TokenBudget)
	}
	if d.Caps.MaxNodes > 0 && len(d.Nodes) > d.Caps.MaxNodes {
		fail("node count %d exceeds maxNodes %d", len(d.Nodes), d.Caps.MaxNodes)
	}

	seen := make(map[string]bool, len(d.Nodes))
	for i, n := range d.Nodes {
		switch {
		case n.ID == "":
			fail("nodes[%d]: id is required", i)
		case !idRe.MatchString(n.ID):
			fail("nodes[%d]: id %q must match %s", i, n.ID, idRe)
		case seen[n.ID]:
			fail("nodes[%d]: duplicate id %q", i, n.ID)
		default:
			seen[n.ID] = true
		}
		if n.Instructions == "" {
			fail("node %q: instructions are required", n.ID)
		}
		if n.Model == "" && d.DefaultModel == "" {
			fail("node %q: no model and no defaultModel", n.ID)
		}
		depSeen := make(map[string]bool, len(n.DependsOn))
		for _, dep := range n.DependsOn {
			if dep == n.ID {
				fail("node %q: depends on itself", n.ID)
			}
			if depSeen[dep] {
				fail("node %q: duplicate dependency %q", n.ID, dep)
			}
			depSeen[dep] = true
		}
	}
	// Dangling references (only meaningful once ids are known).
	for _, n := range d.Nodes {
		for _, dep := range n.DependsOn {
			if dep != n.ID && !seen[dep] {
				fail("node %q: depends on unknown node %q", n.ID, dep)
			}
		}
	}
	// Acyclicity: only check when the graph is otherwise well-formed enough.
	if len(errs) == 0 {
		if _, err := d.Waves(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Waves computes level-order execution waves: wave N contains every node whose
// dependencies are all satisfied by waves < N. Nodes within a wave run
// concurrently. Ordering within a wave is lexicographic for determinism.
// Returns an error naming the nodes involved if the graph has a cycle.
func (d *DagSpec) Waves() ([][]string, error) {
	indeg := make(map[string]int, len(d.Nodes))
	dependents := make(map[string][]string)
	for _, n := range d.Nodes {
		indeg[n.ID] = len(n.DependsOn)
		for _, dep := range n.DependsOn {
			dependents[dep] = append(dependents[dep], n.ID)
		}
	}
	var waves [][]string
	remaining := len(d.Nodes)
	current := readyNodes(indeg, nil)
	for len(current) > 0 {
		sort.Strings(current)
		waves = append(waves, current)
		remaining -= len(current)
		var next []string
		for _, id := range current {
			delete(indeg, id)
			for _, dep := range dependents[id] {
				indeg[dep]--
				if indeg[dep] == 0 {
					next = append(next, dep)
				}
			}
		}
		current = next
	}
	if remaining > 0 {
		var stuck []string
		for id := range indeg {
			stuck = append(stuck, id)
		}
		sort.Strings(stuck)
		return nil, fmt.Errorf("cycle detected involving nodes %v", stuck)
	}
	return waves, nil
}

func readyNodes(indeg map[string]int, into []string) []string {
	for id, n := range indeg {
		if n == 0 {
			into = append(into, id)
		}
	}
	return into
}
