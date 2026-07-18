package spec

import (
	"reflect"
	"strings"
	"testing"
)

func caps() RunCaps {
	return RunCaps{MaxNodes: 10, MaxRounds: 3, MaxConcurrent: 4, TokenBudget: 100_000}
}

func node(id string, deps ...string) WorkflowNode {
	return WorkflowNode{ID: id, Instructions: "do " + id, DependsOn: deps}
}

func valid() *DagSpec {
	return &DagSpec{
		Version:      "1",
		DefaultModel: "anthropic:claude-sonnet",
		Caps:         caps(),
		Nodes:        []WorkflowNode{node("a"), node("b"), node("c", "a", "b")},
	}
}

func TestValidateOK(t *testing.T) {
	if err := valid().Validate(); err != nil {
		t.Fatalf("expected valid, got: %v", err)
	}
}

func TestValidateFailures(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*DagSpec)
		want string // substring of the error
	}{
		{"missing version", func(d *DagSpec) { d.Version = "" }, "version is required"},
		{"no nodes", func(d *DagSpec) { d.Nodes = nil }, "nodes must be non-empty"},
		{"bad id", func(d *DagSpec) { d.Nodes[0].ID = "has space" }, "must match"},
		{"empty id", func(d *DagSpec) { d.Nodes[0].ID = "" }, "id is required"},
		{"duplicate id", func(d *DagSpec) { d.Nodes[1].ID = "a" }, "duplicate id"},
		{"missing instructions", func(d *DagSpec) { d.Nodes[0].Instructions = "" }, "instructions are required"},
		{"no model anywhere", func(d *DagSpec) { d.DefaultModel = "" }, "no model and no defaultModel"},
		{"self dependency", func(d *DagSpec) { d.Nodes[0].DependsOn = []string{"a"} }, "depends on itself"},
		{"duplicate dependency", func(d *DagSpec) { d.Nodes[2].DependsOn = []string{"a", "a"} }, "duplicate dependency"},
		{"dangling reference", func(d *DagSpec) { d.Nodes[2].DependsOn = []string{"a", "ghost"} }, "unknown node"},
		{"zero cap", func(d *DagSpec) { d.Caps.TokenBudget = 0 }, "caps must all be positive"},
		{"too many nodes", func(d *DagSpec) { d.Caps.MaxNodes = 2 }, "exceeds maxNodes"},
		{"cycle", func(d *DagSpec) { d.Nodes[0].DependsOn = []string{"c"} }, "cycle detected"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := valid()
			tc.mut(d)
			err := d.Validate()
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

func TestValidateReportsAllProblems(t *testing.T) {
	d := valid()
	d.Version = ""
	d.Nodes[0].Instructions = ""
	err := d.Validate()
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"version is required", "instructions are required"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestWaves(t *testing.T) {
	d := &DagSpec{
		Version:      "1",
		DefaultModel: "m",
		Caps:         caps(),
		Nodes: []WorkflowNode{
			node("synth", "rev-a", "rev-b", "rev-c"),
			node("rev-c", "prep"),
			node("rev-b", "prep"),
			node("rev-a", "prep"),
			node("prep"),
		},
	}
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	waves, err := d.Waves()
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"prep"}, {"rev-a", "rev-b", "rev-c"}, {"synth"}}
	if !reflect.DeepEqual(waves, want) {
		t.Fatalf("waves = %v, want %v", waves, want)
	}
}

func TestWavesDeterministic(t *testing.T) {
	d := valid()
	first, _ := d.Waves()
	for i := 0; i < 20; i++ {
		again, _ := d.Waves()
		if !reflect.DeepEqual(first, again) {
			t.Fatalf("non-deterministic waves: %v vs %v", first, again)
		}
	}
}

func TestParse(t *testing.T) {
	good := `{
		"version": "1",
		"defaultModel": "anthropic:claude-sonnet",
		"caps": {"maxNodes": 5, "maxRounds": 2, "maxConcurrent": 2, "tokenBudget": 50000},
		"nodes": [
			{"id": "a", "instructions": "review auth/"},
			{"id": "b", "instructions": "review api/"},
			{"id": "c", "instructions": "synthesize", "dependsOn": ["a", "b"],
			 "outputSchema": {"type": "object"}}
		]
	}`
	d, err := Parse([]byte(good))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(d.Nodes) != 3 || d.Nodes[2].OutputSchema == nil {
		t.Fatalf("unexpected parse result: %+v", d)
	}

	if _, err := Parse([]byte(`{"version": "1", "unknownField": true}`)); err == nil {
		t.Fatal("expected error for unknown field")
	}
	if _, err := Parse([]byte(`not json`)); err == nil {
		t.Fatal("expected error for malformed json")
	}
}
