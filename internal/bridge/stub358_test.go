package bridge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// stubMCP is a synthetic loopback Streamable HTTP MCP server (2025-11-25, JSON replies) with hostile knobs.
type stubMCP struct {
	mu          sync.Mutex
	sessions    map[string]bool
	n           int
	effects     int
	log         []stubEntry
	holdFirst   bool          // the first effect call blocks until holdRelease closes
	entered     chan struct{} // closed once the held effect call runs on the server
	holdRelease chan struct{}
	expireTool  string // first call of this tool answers 404 and drops its session
	slowEntered chan struct{}
	slowRelease chan struct{}
	onGET       func(w http.ResponseWriter, r *http.Request)
	srv         *httptest.Server
}
type stubEntry struct{ Method, Tool, Session, Auth string }

func newStub(t *testing.T) *stubMCP {
	s := &stubMCP{sessions: map[string]bool{}, entered: make(chan struct{}), holdRelease: make(chan struct{}), slowEntered: make(chan struct{}), slowRelease: make(chan struct{})}
	s.srv = httptest.NewServer(s)
	t.Cleanup(s.srv.Close)
	return s
}
func (s *stubMCP) count(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, e := range s.log {
		if e.Method == method {
			n++
		}
	}
	return n
}
func (s *stubMCP) effectCount() int { s.mu.Lock(); defer s.mu.Unlock(); return s.effects }
func (s *stubMCP) reply(w http.ResponseWriter, id any, result any, extra map[string]string) {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	w.Header().Set("Content-Type", "application/json")
	for k, v := range extra {
		w.Header().Set(k, v)
	}
	w.Write(b)
}
func (s *stubMCP) fail(w http.ResponseWriter, id any, code int, message string) {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}
func text(v string) map[string]any {
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": v}}}
}
func (s *stubMCP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		if s.onGET != nil {
			s.onGET(w, r)
			return
		}
		w.WriteHeader(405)
		return
	case "DELETE":
		s.mu.Lock()
		s.log = append(s.log, stubEntry{Method: "DELETE", Session: r.Header.Get("Mcp-Session-Id")})
		s.mu.Unlock()
		w.WriteHeader(200)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var msg struct {
		ID     any    `json:"id"`
		Method string `json:"method"`
		Params struct {
			Name string         `json:"name"`
			URI  string         `json:"uri"`
			Args map[string]any `json:"arguments"`
		} `json:"params"`
	}
	json.Unmarshal(body, &msg)
	sid := r.Header.Get("Mcp-Session-Id")
	s.mu.Lock()
	s.log = append(s.log, stubEntry{Method: msg.Method, Tool: msg.Params.Name, Session: sid, Auth: r.Header.Get("Authorization")})
	if msg.Method == "initialize" {
		s.n++
		sid = "S" + strconv.Itoa(s.n)
		s.sessions[sid] = true
		s.mu.Unlock()
		s.reply(w, msg.ID, map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}, "prompts": map[string]any{}, "resources": map[string]any{}}, "serverInfo": map[string]any{"name": "stub", "version": "1"}}, map[string]string{"Mcp-Session-Id": sid})
		return
	}
	known := s.sessions[sid]
	s.mu.Unlock()
	if !known {
		w.WriteHeader(404)
		return
	}
	if msg.ID == nil {
		w.WriteHeader(202)
		return
	}
	switch msg.Method {
	case "tools/list":
		tools := []any{}
		for _, n := range []string{"effect", "echo", "slow", "leak"} {
			tools = append(tools, map[string]any{"name": n, "inputSchema": map[string]any{"type": "object"}})
		}
		s.reply(w, msg.ID, map[string]any{"tools": tools}, nil)
	case "prompts/list":
		s.reply(w, msg.ID, map[string]any{"prompts": []any{}}, nil)
	case "prompts/get":
		switch msg.Params.Name {
		case "nope":
			s.fail(w, msg.ID, -32602, "unknown prompt: nope SECRET-TEXT-FROM-UPSTREAM")
		case "bad":
			s.fail(w, msg.ID, -32602, "missing required argument SECRET-TEXT-FROM-UPSTREAM")
		case "boom":
			s.fail(w, msg.ID, -32603, "internal SECRET-TEXT-FROM-UPSTREAM")
		default:
			s.fail(w, msg.ID, -32000, "server defined SECRET-TEXT-FROM-UPSTREAM")
		}
	case "resources/read":
		if msg.Params.URI == "mock://missing" {
			s.fail(w, msg.ID, -32002, "Resource not found SECRET-TEXT-FROM-UPSTREAM")
			return
		}
		s.reply(w, msg.ID, map[string]any{"contents": []any{map[string]any{"uri": msg.Params.URI, "text": "ok"}}}, nil)
	case "tools/call":
		switch msg.Params.Name {
		case "effect":
			s.mu.Lock()
			s.effects++
			first := s.effects == 1
			s.mu.Unlock()
			if first && s.holdFirst {
				close(s.entered)
				select {
				case <-s.holdRelease:
				case <-r.Context().Done():
					return
				}
			}
			s.reply(w, msg.ID, text("effect done"), nil)
		case "echo":
			s.mu.Lock()
			expire := s.expireTool == "echo"
			if expire {
				s.expireTool = ""
				delete(s.sessions, sid)
			}
			s.mu.Unlock()
			if expire {
				w.WriteHeader(404)
				return
			}
			s.reply(w, msg.ID, text("echo"), nil)
		case "slow":
			close(s.slowEntered)
			select {
			case <-s.slowRelease:
			case <-r.Context().Done():
				return
			}
			s.reply(w, msg.ID, text("slow done"), nil)
		case "leak":
			s.reply(w, msg.ID, text("auth="+r.Header.Get("Authorization")), nil)
		default:
			s.fail(w, msg.ID, -32602, "unknown tool")
		}
	default:
		s.fail(w, msg.ID, -32601, "method not found")
	}
}
func (s *stubMCP) gateway(t *testing.T, extra func(*ServerConfig)) *Gateway {
	t.Helper()
	c := ServerConfig{URL: s.srv.URL + "/mcp", ProtocolVersion: "2025-11-25"}
	if extra != nil {
		extra(&c)
	}
	g, err := New(Config{Servers: map[string]ServerConfig{"r": c}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.Close)
	return g
}
func body(r Response) string  { b, _ := json.Marshal(r.Body); return string(b) }
func has2(s, sub string) bool { return strings.Contains(s, sub) }

var _ = context.Background
