package spec

import _ "embed"

//go:embed dagspec.schema.json
var schemaJSON []byte

// Schema returns the canonical DAG JSON Schema — the public authoring
// contract embedded in run_workflow's tool description. Structural rules
// only; semantic rules (cycles, cap arithmetic) live in Validate.
func Schema() []byte { return schemaJSON }
