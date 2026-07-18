package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/Cloud-Byte-Consulting/fluid/spec"
)

func threeNodeDag() *spec.DagSpec {
	return &spec.DagSpec{
		Version:      "1",
		DefaultModel: "fake:model",
		Caps:         spec.RunCaps{MaxNodes: 10, MaxRounds: 3, MaxConcurrent: 4, TokenBudget: 100_000},
		Nodes: []spec.WorkflowNode{
			{ID: "a", Instructions: "review auth/"},
			{ID: "b", Instructions: "review api/"},
			{ID: "c", Instructions: "synthesize", DependsOn: []string{"a", "b"}},
		},
	}
}

// countingExec records per-node execution counts and returns a JSON result.
type countingExec struct {
	mu      sync.Mutex
	counts  map[string]int
	failing map[string]bool // nodes that should fail
}

func newCountingExec() *countingExec {
	return &countingExec{counts: map[string]int{}, failing: map[string]bool{}}
}

func (c *countingExec) run(_ context.Context, nr NodeRun) (NodeResult, error) {
	c.mu.Lock()
	c.counts[nr.Node.ID]++
	fail := c.failing[nr.Node.ID]
	c.mu.Unlock()
	if fail {
		return NodeResult{}, errors.New("boom")
	}
	out := json.RawMessage(fmt.Sprintf(`{"from":%q,"inputs":%d}`, nr.Node.ID, len(nr.Inputs)))
	return NodeResult{Output: out, Tokens: 1}, nil
}

func TestRunExecutesWavesInOrder(t *testing.T) {
	ex := newCountingExec()
	var mu sync.Mutex
	var order []string
	r := &Runner{Dir: t.TempDir(), Exec: func(ctx context.Context, nr NodeRun) (NodeResult, error) {
		mu.Lock()
		order = append(order, nr.Node.ID)
		mu.Unlock()
		if nr.Node.ID == "c" {
			if len(nr.Inputs) != 2 || nr.Inputs["a"] == nil || nr.Inputs["b"] == nil {
				t.Errorf("c inputs missing dependency results: %v", nr.Inputs)
			}
		}
		return ex.run(ctx, nr)
	}}
	results, err := r.Run(context.Background(), threeNodeDag(), "run1")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("want 3 results, got %v", results)
	}
	if order[len(order)-1] != "c" {
		t.Fatalf("c must run last, order was %v", order)
	}
}

func TestRunResumesWithoutRepeatingWork(t *testing.T) {
	dir := t.TempDir()
	ex := newCountingExec()
	ex.failing["c"] = true
	r := &Runner{Dir: dir, Exec: ex.run}

	if _, err := r.Run(context.Background(), threeNodeDag(), "run2"); err == nil {
		t.Fatal("expected first attempt to fail on c")
	}
	if ex.counts["a"] != 1 || ex.counts["b"] != 1 {
		t.Fatalf("a and b should have run once: %v", ex.counts)
	}

	// "Restart the process": new Runner over the same journal dir, c now healthy.
	ex.failing["c"] = false
	r2 := &Runner{Dir: dir, Exec: ex.run}
	results, err := r2.Run(context.Background(), threeNodeDag(), "run2")
	if err != nil {
		t.Fatal(err)
	}
	if ex.counts["a"] != 1 || ex.counts["b"] != 1 {
		t.Fatalf("completed nodes were re-executed on resume: %v", ex.counts)
	}
	if ex.counts["c"] != 2 {
		t.Fatalf("c should have been retried exactly once more: %v", ex.counts)
	}
	if len(results) != 3 {
		t.Fatalf("want full results after resume, got %v", results)
	}
}

func TestRunIdempotentAfterCompletion(t *testing.T) {
	dir := t.TempDir()
	ex := newCountingExec()
	r := &Runner{Dir: dir, Exec: ex.run}
	if _, err := r.Run(context.Background(), threeNodeDag(), "run3"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(context.Background(), threeNodeDag(), "run3"); err != nil {
		t.Fatal(err)
	}
	for id, n := range ex.counts {
		if n != 1 {
			t.Fatalf("node %s executed %d times across identical reruns", id, n)
		}
	}
}

func TestRunRespectsConcurrencyCap(t *testing.T) {
	d := threeNodeDag()
	d.Caps.MaxConcurrent = 1
	var mu sync.Mutex
	inFlight, maxInFlight := 0, 0
	r := &Runner{Dir: t.TempDir(), Exec: func(ctx context.Context, nr NodeRun) (NodeResult, error) {
		mu.Lock()
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		mu.Unlock()
		defer func() { mu.Lock(); inFlight--; mu.Unlock() }()
		return NodeResult{Output: []byte(`{}`)}, nil
	}}
	if _, err := r.Run(context.Background(), d, "run4"); err != nil {
		t.Fatal(err)
	}
	if maxInFlight != 1 {
		t.Fatalf("maxConcurrent=1 violated, saw %d in flight", maxInFlight)
	}
}
