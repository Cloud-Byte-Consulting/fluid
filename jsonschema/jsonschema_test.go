package jsonschema

// Specs for the minimal JSON Schema validator backing CLO-203 (and reused by
// CLO-197's fixture checks). Supports: type, properties, required, items,
// enum, additionalProperties=false. Semantic DAG rules stay in spec.Validate.

import (
	"strings"
	"testing"
)

func TestValidateSchema(t *testing.T) {
	cases := []struct {
		name    string
		schema  string
		doc     string
		wantErr string // "" = valid
	}{
		{"object ok", `{"type":"object","required":["a"],"properties":{"a":{"type":"string"}}}`, `{"a":"x"}`, ""},
		{"missing required", `{"type":"object","required":["a"]}`, `{}`, "required"},
		{"wrong type", `{"type":"object","properties":{"a":{"type":"number"}}}`, `{"a":"x"}`, "type"},
		{"integer ok", `{"type":"integer"}`, `3`, ""},
		{"integer rejects float", `{"type":"integer"}`, `3.5`, "type"},
		{"enum ok", `{"enum":["low","high"]}`, `"low"`, ""},
		{"enum violation", `{"enum":["low","high"]}`, `"mid"`, "enum"},
		{"array items", `{"type":"array","items":{"type":"string"}}`, `["a","b"]`, ""},
		{"array item violation", `{"type":"array","items":{"type":"string"}}`, `["a",1]`, "type"},
		{"additionalProperties false", `{"type":"object","additionalProperties":false,"properties":{"a":{}}}`, `{"a":1,"b":2}`, "additional"},
		{"nested path in message", `{"type":"object","properties":{"a":{"type":"object","required":["b"]}}}`, `{"a":{}}`, "a"},
		{"not json", `{"type":"object"}`, `nope`, "invalid JSON"},
		{"boolean ok", `{"type":"boolean"}`, `true`, ""},
		{"null ok", `{"type":"null"}`, `null`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			violations := Validate([]byte(tc.schema), []byte(tc.doc))
			if tc.wantErr == "" {
				if len(violations) != 0 {
					t.Fatalf("want valid, got %v", violations)
				}
				return
			}
			if len(violations) == 0 {
				t.Fatal("want violations, got none")
			}
			joined := strings.Join(violations, "; ")
			if !strings.Contains(joined, tc.wantErr) {
				t.Fatalf("violations %q missing %q", joined, tc.wantErr)
			}
		})
	}
}

func TestValidate_ResolvesLocalRefs(t *testing.T) {
	schema := `{
		"type": "object",
		"properties": {"items": {"type": "array", "items": {"$ref": "#/$defs/thing"}}},
		"$defs": {"thing": {"type": "object", "required": ["id"]}}
	}`
	if v := Validate([]byte(schema), []byte(`{"items":[{"id":1}]}`)); len(v) != 0 {
		t.Fatalf("want valid, got %v", v)
	}
	v := Validate([]byte(schema), []byte(`{"items":[{}]}`))
	if len(v) == 0 || !strings.Contains(strings.Join(v, ";"), "id") {
		t.Fatalf("ref'd schema not enforced: %v", v)
	}
}
