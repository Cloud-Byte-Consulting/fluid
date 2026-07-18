// Package provider executes DAG nodes against model providers. A Router
// implements runtime.NodeFunc: it parses each node's "provider:model" string,
// routes to the matching Adapter, builds the prompt from instructions plus
// dependency inputs, and enforces the node's outputSchema with bounded
// corrective retries.
package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/Cloud-Byte-Consulting/fluid/jsonschema"
	"github.com/Cloud-Byte-Consulting/fluid/runtime"
)

// Request is one completion request to a provider.
type Request struct {
	Model  string
	Prompt string
}

// Response is a provider's completion plus reported token usage.
type Response struct {
	Text   string
	Tokens int
}

// Adapter speaks one provider's API.
type Adapter interface {
	Complete(ctx context.Context, req Request) (Response, error)
}

// Router routes nodes to adapters. SchemaRetries bounds corrective retries
// when a node's outputSchema is violated (default 2).
type Router struct {
	Adapters      map[string]Adapter
	SchemaRetries int
}

// NewRouter returns a Router with the built-in adapters, reading credentials
// from the process environment at call time (harnesses own the env).
func NewRouter() *Router {
	return &Router{Adapters: map[string]Adapter{
		"anthropic": &Anthropic{Key: os.Getenv("ANTHROPIC_API_KEY")},
		"openai":    &OpenAI{Key: os.Getenv("OPENAI_API_KEY")},
	}}
}

// Exec implements runtime.NodeFunc.
func (r *Router) Exec(ctx context.Context, nr runtime.NodeRun) (runtime.NodeResult, error) {
	providerName, model, ok := strings.Cut(nr.Model, ":")
	if !ok || providerName == "" || model == "" {
		return runtime.NodeResult{}, fmt.Errorf("unknown_provider: model %q must be \"provider:model\"", nr.Model)
	}
	adapter, ok := r.Adapters[providerName]
	if !ok {
		return runtime.NodeResult{}, fmt.Errorf("unknown_provider: %q (have: %s)", providerName, strings.Join(r.providerNames(), ", "))
	}

	prompt := buildPrompt(nr)
	retries := r.SchemaRetries
	if retries == 0 {
		retries = 2
	}
	totalTokens := 0
	for attempt := 0; ; attempt++ {
		resp, err := adapter.Complete(ctx, Request{Model: model, Prompt: prompt})
		totalTokens += resp.Tokens
		if err != nil {
			return runtime.NodeResult{Tokens: totalTokens}, err
		}
		out, violations := enforceSchema(nr.Node.OutputSchema, resp.Text)
		if len(violations) == 0 {
			return runtime.NodeResult{Output: out, Tokens: totalTokens}, nil
		}
		if attempt >= retries {
			return runtime.NodeResult{Tokens: totalTokens},
				fmt.Errorf("schema_violation after %d attempts: %s", attempt+1, strings.Join(violations, "; "))
		}
		prompt = buildPrompt(nr) + fmt.Sprintf(
			"\n\nYour previous response violated the required output schema:\n- %s\nRespond again with ONLY a JSON document that satisfies the schema.",
			strings.Join(violations, "\n- "))
	}
}

func (r *Router) providerNames() []string {
	names := make([]string, 0, len(r.Adapters))
	for name := range r.Adapters {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// buildPrompt renders a node's instructions, dependency inputs, and output
// contract into one prompt.
func buildPrompt(nr runtime.NodeRun) string {
	var b strings.Builder
	b.WriteString(nr.Node.Instructions)
	if len(nr.Inputs) > 0 {
		b.WriteString("\n\nInputs from completed dependency nodes:")
		ids := make([]string, 0, len(nr.Inputs))
		for id := range nr.Inputs {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			fmt.Fprintf(&b, "\n### %s\n%s", id, nr.Inputs[id])
		}
	}
	if len(nr.Node.OutputSchema) > 0 {
		fmt.Fprintf(&b, "\n\nRespond with ONLY a JSON document matching this JSON Schema:\n%s", nr.Node.OutputSchema)
	}
	return b.String()
}

// enforceSchema validates text against schema. With no schema, any text is
// accepted and non-JSON is wrapped as {"text": ...}.
func enforceSchema(schema json.RawMessage, text string) (json.RawMessage, []string) {
	trimmed := strings.TrimSpace(text)
	trimmed = strings.TrimPrefix(trimmed, "```json")
	trimmed = strings.TrimPrefix(trimmed, "```")
	trimmed = strings.TrimSuffix(trimmed, "```")
	trimmed = strings.TrimSpace(trimmed)
	if len(schema) == 0 {
		if json.Valid([]byte(trimmed)) {
			return json.RawMessage(trimmed), nil
		}
		wrapped, _ := json.Marshal(map[string]string{"text": text})
		return wrapped, nil
	}
	violations := jsonschema.Validate(schema, []byte(trimmed))
	if len(violations) > 0 {
		return nil, violations
	}
	return json.RawMessage(trimmed), nil
}
