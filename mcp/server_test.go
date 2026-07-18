package mcp

// Specs for CLO-205 (stdio scaffold), CLO-206 (run_workflow), CLO-207
// (get_workflow_status / list_workflows). The server is driven end-to-end
// over in-memory pipes; every stdout line must be valid JSON-RPC.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cloud-Byte-Consulting/fluid/runtime"
)

// client drives a Server over pipes and decodes its replies.
type client struct {
	t   *testing.T
	in  io.WriteCloser
	out *bufio.Scanner
}

func startServer(t *testing.T, svc *Service) *client {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	srv := NewServer(svc, "test")
	go srv.Serve(context.Background(), inR, outW)
	sc := bufio.NewScanner(outR)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	t.Cleanup(func() { inW.Close() })
	return &client{t: t, in: inW, out: sc}
}

var nextID atomic.Int64

func (c *client) call(method string, params any) map[string]any {
	c.t.Helper()
	id := nextID.Add(1)
	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	b, _ := json.Marshal(req)
	if _, err := c.in.Write(append(b, '\n')); err != nil {
		c.t.Fatal(err)
	}
	if !c.out.Scan() {
		c.t.Fatal("no response")
	}
	line := c.out.Bytes()
	var resp map[string]any
	if err := json.Unmarshal(line, &resp); err != nil {
		c.t.Fatalf("stdout not protocol-clean, line %q: %v", line, err)
	}
	if resp["jsonrpc"] != "2.0" {
		c.t.Fatalf("not JSON-RPC: %v", resp)
	}
	return resp
}

// toolCall invokes tools/call and returns (text, isError).
func (c *client) toolCall(name string, args any) (string, bool) {
	c.t.Helper()
	resp := c.call("tools/call", map[string]any{"name": name, "arguments": args})
	if resp["error"] != nil {
		c.t.Fatalf("protocol error: %v", resp["error"])
	}
	result := resp["result"].(map[string]any)
	content := result["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	isErr, _ := result["isError"].(bool)
	return text, isErr
}

func quickService(t *testing.T) *Service {
	dir := t.TempDir()
	return &Service{
		Store: &runtime.Store{Dir: dir},
		Runner: &runtime.Runner{
			Dir: dir,
			Exec: func(_ context.Context, nr runtime.NodeRun) (runtime.NodeResult, error) {
				return runtime.NodeResult{Output: []byte(fmt.Sprintf(`{"from":%q}`, nr.Node.ID)), Tokens: 5}, nil
			},
			Backoff: func(int) time.Duration { return 0 },
		},
		Background: context.Background(),
	}
}

const validDag = `{
	"version": "1",
	"name": "review",
	"defaultModel": "fake:m",
	"caps": {"maxNodes": 5, "maxRounds": 2, "maxConcurrent": 2, "tokenBudget": 50000},
	"nodes": [
		{"id": "a", "instructions": "review auth/"},
		{"id": "b", "instructions": "review api/"},
		{"id": "c", "instructions": "synthesize", "dependsOn": ["a", "b"]}
	]
}`

func dagArgs(confirm bool, extra map[string]any) map[string]any {
	var dag map[string]any
	json.Unmarshal([]byte(validDag), &dag)
	args := map[string]any{"dag_spec": dag, "confirm": confirm}
	for k, v := range extra {
		args[k] = v
	}
	return args
}

// Scenario (CLO-205): Discovery
func TestServer_InitializeAndDiscoverTools(t *testing.T) {
	c := startServer(t, quickService(t))
	resp := c.call("initialize", map[string]any{"protocolVersion": "2024-11-05"})
	result := resp["result"].(map[string]any)
	if result["serverInfo"].(map[string]any)["name"] != "fluid" {
		t.Fatalf("bad serverInfo: %v", result)
	}
	c.call("notifications/initialized", nil) // must not break the stream (notification, no id — see below)

	resp = c.call("tools/list", nil)
	tools := resp["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 3 {
		t.Fatalf("want 3 tools, got %d", len(tools))
	}
	names := map[string]string{}
	for _, tool := range tools {
		m := tool.(map[string]any)
		names[m["name"].(string)] = m["description"].(string)
	}
	for _, want := range []string{"run_workflow", "get_workflow_status", "list_workflows"} {
		if _, ok := names[want]; !ok {
			t.Fatalf("missing tool %s in %v", want, names)
		}
	}
	if !strings.Contains(names["run_workflow"], "dependsOn") {
		t.Fatal("run_workflow description must embed the DAG schema")
	}
}

// Scenario (CLO-205): Invalid tool input
func TestServer_InvalidToolInputRejected(t *testing.T) {
	svc := quickService(t)
	c := startServer(t, svc)
	text, isErr := c.toolCall("run_workflow", map[string]any{"confirm": true}) // no dag_spec
	if !isErr || !strings.Contains(text, "invalid_input") {
		t.Fatalf("want invalid_input tool error, got %q (isErr=%v)", text, isErr)
	}
	runs, _ := svc.Store.List()
	if len(runs) != 0 {
		t.Fatal("no run may be created on invalid input")
	}
}

// Scenario (CLO-206): Preview without confirm
func TestRunWorkflow_PreviewCreatesNoRun(t *testing.T) {
	svc := quickService(t)
	c := startServer(t, svc)
	text, isErr := c.toolCall("run_workflow", dagArgs(false, nil))
	if isErr {
		t.Fatalf("preview errored: %s", text)
	}
	var preview struct {
		Status string     `json:"status"`
		Waves  [][]string `json:"waves"`
		Nodes  int        `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(text), &preview); err != nil {
		t.Fatal(err)
	}
	if preview.Status != "preview" || preview.Nodes != 3 || len(preview.Waves) != 2 {
		t.Fatalf("bad preview: %s", text)
	}
	runs, _ := svc.Store.List()
	if len(runs) != 0 {
		t.Fatal("preview must not create a run")
	}
}

// Scenario (CLO-206): Confirmed start returns immediately
// Scenario (CLO-207): Poll an active run / list runs
func TestRunWorkflow_ConfirmStartsAndIsObservable(t *testing.T) {
	svc := quickService(t)
	c := startServer(t, svc)

	start := time.Now()
	text, isErr := c.toolCall("run_workflow", dagArgs(true, nil))
	if isErr {
		t.Fatalf("start errored: %s", text)
	}
	if time.Since(start) > time.Second {
		t.Fatal("run_workflow must return fast")
	}
	var started struct {
		Status string `json:"status"`
		RunID  string `json:"run_id"`
	}
	if err := json.Unmarshal([]byte(text), &started); err != nil {
		t.Fatal(err)
	}
	if started.Status != "started" || started.RunID == "" {
		t.Fatalf("bad start response: %s", text)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		text, isErr = c.toolCall("get_workflow_status", map[string]any{"run_id": started.RunID})
		if isErr {
			t.Fatalf("status errored: %s", text)
		}
		var st runtime.RunStatus
		if err := json.Unmarshal([]byte(text), &st); err != nil {
			t.Fatal(err)
		}
		if st.State == "succeeded" {
			if st.TokensSpent != 15 || st.NodesTotal != 3 {
				t.Fatalf("bad terminal status: %s", text)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run never succeeded: %s", text)
		}
		time.Sleep(10 * time.Millisecond)
	}

	text, isErr = c.toolCall("list_workflows", map[string]any{})
	if isErr {
		t.Fatalf("list errored: %s", text)
	}
	var list struct {
		Runs []runtime.RunSummary `json:"runs"`
	}
	if err := json.Unmarshal([]byte(text), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Runs) != 1 || list.Runs[0].RunID != started.RunID {
		t.Fatalf("bad list: %s", text)
	}
}

// Scenario (CLO-206): Invalid DAG never starts
func TestRunWorkflow_InvalidDagReturnsAllProblems(t *testing.T) {
	svc := quickService(t)
	c := startServer(t, svc)
	var dag map[string]any
	json.Unmarshal([]byte(validDag), &dag)
	dag["nodes"].([]any)[2].(map[string]any)["dependsOn"] = []string{"a", "ghost"}
	delete(dag, "version")
	text, isErr := c.toolCall("run_workflow", map[string]any{"dag_spec": dag, "confirm": true})
	if !isErr {
		t.Fatalf("want tool error, got %s", text)
	}
	if !strings.Contains(text, "version is required") || !strings.Contains(text, "unknown node") {
		t.Fatalf("must report every problem: %s", text)
	}
	runs, _ := svc.Store.List()
	if len(runs) != 0 {
		t.Fatal("invalid DAG must not create a run")
	}
}

// Scenario (CLO-206): Server restart mid-run → resume by run_id
func TestRunWorkflow_ResumeByRunID(t *testing.T) {
	dir := t.TempDir()
	fail := atomic.Bool{}
	fail.Store(true)
	exec := func(_ context.Context, nr runtime.NodeRun) (runtime.NodeResult, error) {
		if nr.Node.ID == "c" && fail.Load() {
			return runtime.NodeResult{}, errors.New("crash")
		}
		return runtime.NodeResult{Output: []byte(`{}`), Tokens: 1}, nil
	}
	svc := &Service{
		Store:      &runtime.Store{Dir: dir},
		Runner:     &runtime.Runner{Dir: dir, Exec: exec, Backoff: func(int) time.Duration { return 0 }},
		Background: context.Background(),
	}
	c := startServer(t, svc)
	text, _ := c.toolCall("run_workflow", dagArgs(true, nil))
	var started struct {
		RunID string `json:"run_id"`
	}
	json.Unmarshal([]byte(text), &started)

	waitState(t, c, started.RunID, "failed")
	fail.Store(false) // "restart": the failure cause is gone

	text, isErr := c.toolCall("run_workflow", dagArgs(true, map[string]any{"run_id": started.RunID}))
	if isErr {
		t.Fatalf("resume errored: %s", text)
	}
	waitState(t, c, started.RunID, "succeeded")
}

func waitState(t *testing.T, c *client, runID, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		text, _ := c.toolCall("get_workflow_status", map[string]any{"run_id": runID})
		var st runtime.RunStatus
		json.Unmarshal([]byte(text), &st)
		if st.State == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("run never reached %s: %s", want, text)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Scenario (CLO-207): Unknown run id
func TestGetStatus_UnknownRunIsToolError(t *testing.T) {
	c := startServer(t, quickService(t))
	text, isErr := c.toolCall("get_workflow_status", map[string]any{"run_id": "nope"})
	if !isErr || !strings.Contains(text, "not_found") {
		t.Fatalf("want not_found tool error, got %q", text)
	}
}

// Scenario (CLO-205): protocol errors for unknown methods
func TestServer_UnknownMethodIsJSONRPCError(t *testing.T) {
	c := startServer(t, quickService(t))
	resp := c.call("bogus/method", nil)
	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("want JSON-RPC error, got %v", resp)
	}
	if errObj["code"].(float64) != -32601 {
		t.Fatalf("want -32601, got %v", errObj)
	}
}

// Deep-review finding: concurrent resumes of one run_id must not share a journal.
func TestRunWorkflow_ConcurrentResumeRejected(t *testing.T) {
	dir := t.TempDir()
	release := make(chan struct{})
	svc := &Service{
		Store: &runtime.Store{Dir: dir},
		Runner: &runtime.Runner{Dir: dir, Exec: func(_ context.Context, _ runtime.NodeRun) (runtime.NodeResult, error) {
			<-release
			return runtime.NodeResult{Output: []byte(`{}`)}, nil
		}},
		Background: context.Background(),
	}
	c := startServer(t, svc)
	text, _ := c.toolCall("run_workflow", dagArgs(true, nil))
	var started struct {
		RunID string `json:"run_id"`
	}
	json.Unmarshal([]byte(text), &started)

	text, isErr := c.toolCall("run_workflow", dagArgs(true, map[string]any{"run_id": started.RunID}))
	if !isErr || !strings.Contains(text, "already_running") {
		t.Fatalf("want already_running tool error, got %q (isErr=%v)", text, isErr)
	}
	close(release)
	waitState(t, c, started.RunID, "succeeded")
}
