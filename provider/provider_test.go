package provider

// Specs for CLO-202 "Provider router" and CLO-203 "Structured-output enforcement".

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Cloud-Byte-Consulting/fluid/runtime"
	"github.com/Cloud-Byte-Consulting/fluid/spec"
)

func nodeRun(model, instructions string) runtime.NodeRun {
	return runtime.NodeRun{
		RunID: "r1",
		Node:  spec.WorkflowNode{ID: "n1", Instructions: instructions},
		Model: model,
	}
}

// fakeAnthropic returns an httptest server speaking just enough of the
// Messages API, recording the requests it receives.
func fakeAnthropic(t *testing.T, reply string) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var reqs []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		body["_path"] = r.URL.Path
		body["_key"] = r.Header.Get("x-api-key")
		mu.Lock()
		reqs = append(reqs, body)
		mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]any{{"type": "text", "text": reply}},
			"usage":   map[string]any{"input_tokens": 7, "output_tokens": 5},
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &reqs
}

func fakeOpenAI(t *testing.T, reply string) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var reqs []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		body["_path"] = r.URL.Path
		body["_auth"] = r.Header.Get("Authorization")
		mu.Lock()
		reqs = append(reqs, body)
		mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": reply}}},
			"usage":   map[string]any{"total_tokens": 12},
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &reqs
}

// Scenario Outline: Route by prefix (anthropic)
func TestRouter_RoutesAnthropicByPrefix(t *testing.T) {
	srv, reqs := fakeAnthropic(t, "hello")
	router := &Router{Adapters: map[string]Adapter{
		"anthropic": &Anthropic{Key: "k-ant", BaseURL: srv.URL},
	}}
	res, err := router.Exec(context.Background(), nodeRun("anthropic:claude-sonnet-4", "say hello"))
	if err != nil {
		t.Fatal(err)
	}
	if len(*reqs) != 1 {
		t.Fatalf("want 1 request, got %d", len(*reqs))
	}
	got := (*reqs)[0]
	if got["model"] != "claude-sonnet-4" {
		t.Fatalf("model = %v, want claude-sonnet-4", got["model"])
	}
	if got["_key"] != "k-ant" {
		t.Fatalf("x-api-key not sent: %v", got["_key"])
	}
	if res.Tokens != 12 { // 7 in + 5 out
		t.Fatalf("tokens = %d, want 12", res.Tokens)
	}
	var out map[string]string
	if err := json.Unmarshal(res.Output, &out); err != nil || out["text"] != "hello" {
		t.Fatalf("output = %s", res.Output)
	}
}

// Scenario Outline: Route by prefix (openai)
func TestRouter_RoutesOpenAIByPrefix(t *testing.T) {
	srv, reqs := fakeOpenAI(t, "hi")
	router := &Router{Adapters: map[string]Adapter{
		"openai": &OpenAI{Key: "k-oai", BaseURL: srv.URL},
	}}
	res, err := router.Exec(context.Background(), nodeRun("openai:gpt-5", "say hi"))
	if err != nil {
		t.Fatal(err)
	}
	got := (*reqs)[0]
	if got["model"] != "gpt-5" {
		t.Fatalf("model = %v, want gpt-5", got["model"])
	}
	if got["_auth"] != "Bearer k-oai" {
		t.Fatalf("bearer auth not sent: %v", got["_auth"])
	}
	if res.Tokens != 12 {
		t.Fatalf("tokens = %d, want 12", res.Tokens)
	}
}

// Scenario: Unknown provider
func TestRouter_UnknownProviderFailsFast(t *testing.T) {
	router := &Router{Adapters: map[string]Adapter{}}
	_, err := router.Exec(context.Background(), nodeRun("acme:foo", "x"))
	if err == nil || !strings.Contains(err.Error(), "unknown_provider") {
		t.Fatalf("want unknown_provider, got %v", err)
	}
	if runtime.IsTransient(err) {
		t.Fatal("unknown_provider must be permanent")
	}
}

// Scenario: Malformed model string
func TestRouter_MalformedModelFailsFast(t *testing.T) {
	router := &Router{Adapters: map[string]Adapter{}}
	_, err := router.Exec(context.Background(), nodeRun("no-colon", "x"))
	if err == nil || !strings.Contains(err.Error(), "unknown_provider") {
		t.Fatalf("want unknown_provider, got %v", err)
	}
}

// Scenario: Missing credential
func TestRouter_MissingCredentialFailsFast(t *testing.T) {
	router := &Router{Adapters: map[string]Adapter{
		"anthropic": &Anthropic{Key: "", BaseURL: "http://127.0.0.1:9"},
	}}
	_, err := router.Exec(context.Background(), nodeRun("anthropic:m", "x"))
	if err == nil || !strings.Contains(err.Error(), "missing_credential") {
		t.Fatalf("want missing_credential, got %v", err)
	}
	if !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Fatalf("error must name the env var: %v", err)
	}
	if runtime.IsTransient(err) {
		t.Fatal("missing_credential must be permanent")
	}
}

// Scenario: Error classification
func TestAdapters_ClassifyHTTPErrors(t *testing.T) {
	for _, tc := range []struct {
		status    int
		transient bool
	}{
		{429, true}, {500, true}, {503, true},
		{400, false}, {401, false}, {403, false},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			w.Write([]byte(`{"error":"x"}`))
		}))
		router := &Router{Adapters: map[string]Adapter{
			"anthropic": &Anthropic{Key: "k", BaseURL: srv.URL},
		}}
		_, err := router.Exec(context.Background(), nodeRun("anthropic:m", "x"))
		srv.Close()
		if err == nil {
			t.Fatalf("status %d: expected error", tc.status)
		}
		if runtime.IsTransient(err) != tc.transient {
			t.Fatalf("status %d: transient = %v, want %v", tc.status, runtime.IsTransient(err), tc.transient)
		}
	}
}

// --- CLO-203 structured output ---

// scriptedAdapter returns canned replies in order and records requests.
type scriptedAdapter struct {
	replies []string
	reqs    []Request
}

func (s *scriptedAdapter) Complete(_ context.Context, req Request) (Response, error) {
	s.reqs = append(s.reqs, req)
	reply := s.replies[0]
	if len(s.replies) > 1 {
		s.replies = s.replies[1:]
	}
	return Response{Text: reply, Tokens: 3}, nil
}

func schemaNode(schema string) runtime.NodeRun {
	nr := nodeRun("fake:m", "produce a finding")
	nr.Node.OutputSchema = json.RawMessage(schema)
	return nr
}

const findingSchema = `{
	"type": "object",
	"required": ["severity", "title"],
	"properties": {
		"severity": {"type": "string", "enum": ["low", "high"]},
		"title": {"type": "string"}
	}
}`

// Scenario: Valid output passes through
func TestSchema_ValidOutputPassesThrough(t *testing.T) {
	ad := &scriptedAdapter{replies: []string{`{"severity":"high","title":"races"}`}}
	router := &Router{Adapters: map[string]Adapter{"fake": ad}}
	res, err := router.Exec(context.Background(), schemaNode(findingSchema))
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Output) != `{"severity":"high","title":"races"}` {
		t.Fatalf("output altered: %s", res.Output)
	}
	if len(ad.reqs) != 1 {
		t.Fatalf("want 1 call, got %d", len(ad.reqs))
	}
}

// Scenario: Corrective retry on violation
func TestSchema_CorrectiveRetryOnViolation(t *testing.T) {
	ad := &scriptedAdapter{replies: []string{
		`{"severity":"catastrophic","title":"races"}`, // enum violation
		`{"severity":"high","title":"races"}`,
	}}
	router := &Router{Adapters: map[string]Adapter{"fake": ad}}
	res, err := router.Exec(context.Background(), schemaNode(findingSchema))
	if err != nil {
		t.Fatal(err)
	}
	if len(ad.reqs) != 2 {
		t.Fatalf("want 2 calls, got %d", len(ad.reqs))
	}
	if !strings.Contains(ad.reqs[1].Prompt, "severity") {
		t.Fatalf("second call must carry corrective feedback, got: %s", ad.reqs[1].Prompt)
	}
	if string(res.Output) != `{"severity":"high","title":"races"}` {
		t.Fatalf("output = %s", res.Output)
	}
}

// Scenario: Persistent violation fails the node
func TestSchema_PersistentViolationFails(t *testing.T) {
	ad := &scriptedAdapter{replies: []string{`not even json`}}
	router := &Router{Adapters: map[string]Adapter{"fake": ad}}
	_, err := router.Exec(context.Background(), schemaNode(findingSchema))
	if err == nil || !strings.Contains(err.Error(), "schema_violation") {
		t.Fatalf("want schema_violation, got %v", err)
	}
	if len(ad.reqs) != 3 { // 1 initial + 2 corrective retries
		t.Fatalf("want 3 calls, got %d", len(ad.reqs))
	}
	if runtime.IsTransient(err) {
		t.Fatal("schema_violation must be permanent")
	}
}

// Scenario: No schema, no enforcement
func TestSchema_NoSchemaWrapsText(t *testing.T) {
	ad := &scriptedAdapter{replies: []string{"plain prose answer"}}
	router := &Router{Adapters: map[string]Adapter{"fake": ad}}
	res, err := router.Exec(context.Background(), nodeRun("fake:m", "summarize"))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]string
	if err := json.Unmarshal(res.Output, &out); err != nil || out["text"] != "plain prose answer" {
		t.Fatalf("output = %s", res.Output)
	}
}

// Scenario: Dependency inputs reach the prompt
func TestRouter_InputsIncludedInPrompt(t *testing.T) {
	ad := &scriptedAdapter{replies: []string{"ok"}}
	router := &Router{Adapters: map[string]Adapter{"fake": ad}}
	nr := nodeRun("fake:m", "synthesize the findings")
	nr.Inputs = map[string]json.RawMessage{"rev-a": []byte(`{"finding":"races in auth"}`)}
	if _, err := router.Exec(context.Background(), nr); err != nil {
		t.Fatal(err)
	}
	p := ad.reqs[0].Prompt
	if !strings.Contains(p, "rev-a") || !strings.Contains(p, "races in auth") {
		t.Fatalf("prompt missing dependency inputs: %s", p)
	}
}
