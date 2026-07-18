package spec

// Specs for CLO-197 "Publish the canonical DAG JSON Schema artifact".

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/Cloud-Byte-Consulting/fluid/jsonschema"
)

// Scenario: Schema accepts what the validator accepts
func TestSchema_AcceptsValidDags(t *testing.T) {
	d := valid()
	doc, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if v := jsonschema.Validate(Schema(), doc); len(v) != 0 {
		t.Fatalf("valid DAG rejected by schema: %v", v)
	}
}

// Scenario: Schema rejects structural defects
func TestSchema_RejectsStructuralDefects(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want string
	}{
		{"missing version", `{"nodes":[{"id":"a","instructions":"x"}],"caps":{"maxNodes":1,"maxRounds":1,"maxConcurrent":1,"tokenBudget":1}}`, "version"},
		{"missing nodes", `{"version":"1","caps":{"maxNodes":1,"maxRounds":1,"maxConcurrent":1,"tokenBudget":1}}`, "nodes"},
		{"wrong cap type", `{"version":"1","nodes":[{"id":"a","instructions":"x"}],"caps":{"maxNodes":"many","maxRounds":1,"maxConcurrent":1,"tokenBudget":1}}`, "maxNodes"},
		{"unknown top-level field", `{"version":"1","bogus":true,"nodes":[{"id":"a","instructions":"x"}],"caps":{"maxNodes":1,"maxRounds":1,"maxConcurrent":1,"tokenBudget":1}}`, "bogus"},
		{"unknown node field", `{"version":"1","nodes":[{"id":"a","instructions":"x","surprise":1}],"caps":{"maxNodes":1,"maxRounds":1,"maxConcurrent":1,"tokenBudget":1}}`, "surprise"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := jsonschema.Validate(Schema(), []byte(tc.doc))
			if len(v) == 0 {
				t.Fatal("want violations, got none")
			}
			if !strings.Contains(strings.Join(v, "; "), tc.want) {
				t.Fatalf("violations %v missing %q", v, tc.want)
			}
		})
	}
}

// Scenario: Schema and Go types cannot drift
func TestSchema_DriftGuard(t *testing.T) {
	var s struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Defs       map[string]struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(Schema(), &s); err != nil {
		t.Fatal(err)
	}
	compare := func(structType reflect.Type, schemaProps map[string]json.RawMessage, name string) {
		tags := map[string]bool{}
		for i := 0; i < structType.NumField(); i++ {
			tag := strings.Split(structType.Field(i).Tag.Get("json"), ",")[0]
			if tag != "" && tag != "-" {
				tags[tag] = true
			}
		}
		for tag := range tags {
			if _, ok := schemaProps[tag]; !ok {
				t.Errorf("%s: Go field %q missing from schema", name, tag)
			}
		}
		for prop := range schemaProps {
			if !tags[prop] {
				t.Errorf("%s: schema property %q missing from Go type", name, prop)
			}
		}
	}
	compare(reflect.TypeOf(DagSpec{}), s.Properties, "DagSpec")
	compare(reflect.TypeOf(WorkflowNode{}), s.Defs["node"].Properties, "WorkflowNode")
	compare(reflect.TypeOf(RunCaps{}), s.Defs["caps"].Properties, "RunCaps")
}
