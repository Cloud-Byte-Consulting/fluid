// Package mcp is fluid's Model Context Protocol boundary: a minimal,
// dependency-free stdio server speaking newline-delimited JSON-RPC 2.0 with
// the MCP initialize / tools/list / tools/call methods. stdout carries only
// protocol traffic; diagnostics belong on stderr.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log"
	"strings"
	"sync"

	"github.com/Cloud-Byte-Consulting/fluid/jsonschema"
)

const protocolVersion = "2024-11-05"

// Tool is one MCP tool: schema-validated input, handler returns a value that
// is marshaled into the tool result text.
type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
	Handler     func(ctx context.Context, args json.RawMessage) (any, error)
}

// Server serves MCP over a reader/writer pair (stdin/stdout in production).
type Server struct {
	Version string
	Tools   []Tool

	mu  sync.Mutex // serializes writes
	out *json.Encoder
}

// NewServer wires the fluid Service's tools into a Server.
func NewServer(svc *Service, version string) *Server {
	return &Server{Version: version, Tools: svc.Tools()}
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// Serve processes messages until EOF or ctx cancellation.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	s.out = json.NewEncoder(w)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			s.writeError(nil, -32700, "parse error: "+err.Error())
			continue
		}
		s.dispatch(ctx, req)
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		return err
	}
	return ctx.Err()
}

func (s *Server) dispatch(ctx context.Context, req request) {
	switch {
	case req.Method == "initialize":
		s.writeResult(req.ID, map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "fluid", "version": s.Version},
		})
	case strings.HasPrefix(req.Method, "notifications/"):
		if req.ID != nil { // notifications carry no id; answer politely if one appears
			s.writeResult(req.ID, map[string]any{})
		}
	case req.Method == "ping":
		s.writeResult(req.ID, map[string]any{})
	case req.Method == "tools/list":
		tools := make([]map[string]any, 0, len(s.Tools))
		for _, t := range s.Tools {
			tools = append(tools, map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"inputSchema": t.InputSchema,
			})
		}
		s.writeResult(req.ID, map[string]any{"tools": tools})
	case req.Method == "tools/call":
		s.callTool(ctx, req)
	default:
		if req.ID != nil {
			s.writeError(req.ID, -32601, "method not found: "+req.Method)
		}
	}
}

func (s *Server) callTool(ctx context.Context, req request) {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		s.writeError(req.ID, -32602, "invalid params: "+err.Error())
		return
	}
	for _, t := range s.Tools {
		if t.Name != params.Name {
			continue
		}
		args := params.Arguments
		if len(args) == 0 {
			args = []byte("{}")
		}
		if violations := jsonschema.Validate(t.InputSchema, args); len(violations) > 0 {
			s.writeToolError(req.ID, "invalid_input: "+strings.Join(violations, "; "))
			return
		}
		result, err := t.Handler(ctx, args)
		if err != nil {
			s.writeToolError(req.ID, err.Error())
			return
		}
		text, err := json.Marshal(result)
		if err != nil {
			s.writeToolError(req.ID, "internal: "+err.Error())
			return
		}
		s.writeResult(req.ID, map[string]any{
			"content": []map[string]any{{"type": "text", "text": string(text)}},
		})
		return
	}
	s.writeError(req.ID, -32602, "unknown tool: "+params.Name)
}

// writeToolError reports a tool-level failure in-band (isError), per MCP.
func (s *Server) writeToolError(id json.RawMessage, msg string) {
	s.writeResult(id, map[string]any{
		"content": []map[string]any{{"type": "text", "text": msg}},
		"isError": true,
	})
}

func (s *Server) writeResult(id json.RawMessage, result any) {
	s.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (s *Server) writeError(id json.RawMessage, code int, msg string) {
	s.write(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}})
}

func (s *Server) write(v any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.out.Encode(v); err != nil {
		log.Printf("write failed: %v", err) // stderr via log default
	}
}
