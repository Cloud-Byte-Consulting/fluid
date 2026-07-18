package jsonschema

// Minimal JSON Schema validator: type, properties, required, items, enum,
// additionalProperties=false, and local "$ref": "#/$defs/...". Unknown
// keywords (pattern, minimum, minItems, ...) are IGNORED — external
// draft-2020-12 validators enforce more than this one does, never less.
// Deliberately small; swap for a full library without changing callers if
// richer keywords become load-bearing.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
)

// Validate checks doc against schema and returns human-readable
// violations (empty = valid).
func Validate(schema, doc []byte) []string {
	var s map[string]any
	if err := json.Unmarshal(schema, &s); err != nil {
		return []string{fmt.Sprintf("schema itself is invalid JSON: %v", err)}
	}
	var d any
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.UseNumber()
	if err := dec.Decode(&d); err != nil {
		return []string{fmt.Sprintf("document is invalid JSON: %v", err)}
	}
	return check(s, d, "$", s)
}

// check validates doc against schema; root carries the top-level schema so
// local "$ref": "#/$defs/<name>" references resolve.
func check(schema map[string]any, doc any, path string, root map[string]any) []string {
	if ref, ok := schema["$ref"].(string); ok {
		if name, ok := strings.CutPrefix(ref, "#/$defs/"); ok {
			if defs, ok := root["$defs"].(map[string]any); ok {
				if target, ok := defs[name].(map[string]any); ok {
					return check(target, doc, path, root)
				}
			}
		}
		return []string{fmt.Sprintf("%s: unresolvable $ref %q", path, ref)}
	}
	var out []string
	if enum, ok := schema["enum"].([]any); ok {
		matched := false
		for _, want := range enum {
			if jsonEqual(want, doc) {
				matched = true
				break
			}
		}
		if !matched {
			out = append(out, fmt.Sprintf("%s: value not in enum %v", path, enum))
		}
	}
	if typ, ok := schema["type"].(string); ok {
		if !typeMatches(typ, doc) {
			out = append(out, fmt.Sprintf("%s: wrong type, want %s", path, typ))
			return out // structural mismatch: deeper checks are meaningless
		}
	}
	if obj, ok := doc.(map[string]any); ok {
		if req, ok := schema["required"].([]any); ok {
			for _, k := range req {
				key, _ := k.(string)
				if _, present := obj[key]; !present {
					out = append(out, fmt.Sprintf("%s: missing required property %q", path, key))
				}
			}
		}
		props, _ := schema["properties"].(map[string]any)
		for key, sub := range props {
			if val, present := obj[key]; present {
				if subSchema, ok := sub.(map[string]any); ok {
					out = append(out, check(subSchema, val, path+"."+key, root)...)
				}
			}
		}
		if ap, ok := schema["additionalProperties"].(bool); ok && !ap {
			for key := range obj {
				if _, declared := props[key]; !declared {
					out = append(out, fmt.Sprintf("%s: additional property %q not allowed", path, key))
				}
			}
		}
	}
	if arr, ok := doc.([]any); ok {
		if items, ok := schema["items"].(map[string]any); ok {
			for i, item := range arr {
				out = append(out, check(items, item, fmt.Sprintf("%s[%d]", path, i), root)...)
			}
		}
	}
	return out
}

func typeMatches(typ string, doc any) bool {
	switch typ {
	case "object":
		_, ok := doc.(map[string]any)
		return ok
	case "array":
		_, ok := doc.([]any)
		return ok
	case "string":
		_, ok := doc.(string)
		return ok
	case "boolean":
		_, ok := doc.(bool)
		return ok
	case "null":
		return doc == nil
	case "number":
		_, ok := doc.(json.Number)
		return ok
	case "integer":
		n, ok := doc.(json.Number)
		if !ok {
			return false
		}
		f, err := n.Float64()
		return err == nil && f == math.Trunc(f)
	}
	return true // unknown type keyword: don't reject
}

// jsonEqual compares an enum literal (decoded without UseNumber) to a
// document value (decoded with UseNumber).
func jsonEqual(want, got any) bool {
	if n, ok := got.(json.Number); ok {
		if f, ok := want.(float64); ok {
			gf, err := n.Float64()
			return err == nil && gf == f
		}
	}
	return reflect.DeepEqual(want, got)
}
