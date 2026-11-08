package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func remoteFixture(t *testing.T, jsonReply bool) (*Gateway, *mcp.Server) {
	t.Helper()
	s := mcp.NewServer(&mcp.Implementation{Name: "remote", Version: "1"}, nil)
	s.AddTool(&mcp.Tool{Name: "echo", InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "hello"}}, StructuredContent: map[string]any{"value": 7}}, nil
	})
	s.AddTool(&mcp.Tool{Name: "fail", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "failure"}}}, nil
	})
	s.AddResource(&mcp.Resource{Name: "blob", URI: "mock://blob"}, func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: "mock://blob", MIMEType: "application/octet-stream", Blob: []byte{0, 1, 2}}}}, nil
	})
	s.AddPrompt(&mcp.Prompt{Name: "greet", Arguments: []*mcp.PromptArgument{{Name: "name", Required: true}}}, func(_ context.Context, r *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: r.Params.Arguments["name"]}}}}, nil
	})
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{JSONResponse: jsonReply})
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	cfg, err := Load([]byte(`{"mcpServers":{"remote":{"url":"` + server.URL + `","protocolVersion":"2025-11-25"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	g, _ := New(cfg, Options{})
	t.Cleanup(g.Close)
	return g, s
}
func TestRemoteSurfaces(t *testing.T) {
	for _, mode := range []bool{true, false} {
		t.Run(map[bool]string{true: "json", false: "sse"}[mode], func(t *testing.T) {
			g, _ := remoteFixture(t, mode)
			for _, path := range []string{"tools", "resources", "resources/templates", "prompts"} {
				r := request(g, "GET", "/v1/servers/remote/"+path, "")
				if r.Status != 200 {
					t.Fatal(path, r)
				}
			}
			r := g.Dispatch(context.Background(), Request{Method: "GET", Path: "/v1/servers/remote/resources/read", Query: map[string]string{"uri": "mock://blob"}})
			b, _ := json.Marshal(r.Body)
			if r.Status != 200 || !containsSegment(string(b), `"blob":"AAEC"`) {
				t.Fatal(r, string(b))
			}
			r = request(g, "POST", "/v1/servers/remote/prompts/greet", `{"name":"Ada"}`)
			b, _ = json.Marshal(r.Body)
			if r.Status != 200 || !containsSegment(string(b), "Ada") {
				t.Fatal(r)
			}
			r = request(g, "POST", "/v1/servers/remote/tools/echo/call", `{}`)
			b, _ = json.Marshal(r.Body)
			if r.Status != 200 || !containsSegment(string(b), `"structuredContent":{"value":7}`) {
				t.Fatal(r, string(b))
			}
			r = request(g, "POST", "/v1/servers/remote/tools/fail/call", `{}`)
			if r.Status != 200 || Exit(r) != 3 {
				t.Fatal(r, Exit(r))
			}
		})
	}
}
func TestRemoteConfigPolicy(t *testing.T) {
	for _, raw := range []string{
		`{"url":"http://example.com/mcp"}`, `{"url":"http://192.0.2.1/mcp"}`, `{"url":"http://user:pass@127.0.0.1/mcp"}`, `{"url":"http://127.0.0.1/mcp","headers":{"Authorization":"Bearer literal"}}`, `{"url":"http://127.0.0.1/mcp","type":"typo"}`, `{"url":"http://127.0.0.1/mcp","type":"sse","transport":"auto"}`, `{"url":"http://127.0.0.1/mcp","headers":{"Content-Type":"${BAD}"}}`,
	} {
		if _, e := Load([]byte(`{"mcpServers":{"r":` + raw + `}}`)); e == nil {
			t.Fatal(raw)
		}
	}
}
func TestRemoteAuthAndNoRedirect(t *testing.T) {
	var received atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { received.Add(1); w.WriteHeader(200) }))
	defer target.Close()
	for _, status := range []int{401, 403, 302} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer synthetic-354" {
				t.Error("missing authorization")
			}
			if status == 302 {
				w.Header().Set("Location", target.URL)
			}
			w.WriteHeader(status)
		}))
		cfg, e := Load([]byte(`{"mcpServers":{"r":{"url":"` + server.URL + `","headers":{"Authorization":"Bearer ${KEY}"}}}}`))
		if e != nil {
			t.Fatal(e)
		}
		g, _ := New(cfg, Options{Lookup: func(string) (string, bool) { return "synthetic-354", true }})
		r := request(g, "GET", "/v1/servers/r/tools", "")
		if r.Status < 400 {
			t.Fatal(r)
		}
		g.Close()
		server.Close()
	}
	if received.Load() != 0 {
		t.Fatal("credential redirect")
	}
}

func TestRemoteLargeEvent(t *testing.T) {
	g, s := remoteFixture(t, false)
	text := strings.Repeat("z", 17<<20)
	s.AddTool(&mcp.Tool{Name: "large", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil
	})
	r := request(g, "POST", "/v1/servers/remote/tools/large/call", `{}`)
	b, _ := json.Marshal(r.Body)
	if r.Status != 200 || !strings.Contains(string(b), text) {
		t.Fatal("large SSE result incomplete", r.Status)
	}
}
func TestRemoteHeaderBeforeNetwork(t *testing.T) {
	var hits atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.WriteHeader(500) }))
	defer s.Close()
	for _, value := range []string{"a\rb", "a\nb", "a\x00b"} {
		g, _ := New(Config{Servers: map[string]ServerConfig{"r": {URL: s.URL, Headers: map[string]string{"Authorization": "${KEY}"}}}}, Options{Lookup: func(string) (string, bool) { return value, true }})
		r := request(g, "GET", "/v1/servers/r/tools", "")
		b, _ := json.Marshal(r.Body)
		if r.Status != 502 || !strings.Contains(string(b), "INVALID_HEADER") {
			t.Fatal(r)
		}
		g.Close()
	}
	if hits.Load() != 0 {
		t.Fatal("invalid header reached network")
	}
}
func TestCatalogInvalidArguments(t *testing.T) {
	g, _ := remoteFixture(t, true)
	for _, r := range []Request{{Method: "GET", Path: "/v1/servers/remote/resources/read"}, {Method: "GET", Path: "/v1/servers/remote/resources/read", Query: map[string]string{"uri": ""}}, {Method: "POST", Path: "/v1/servers/remote/prompts/greet", Body: json.RawMessage(`{"name":1}`)}, {Method: "POST", Path: "/v1/servers/remote/prompts/greet", Body: json.RawMessage(`null`)}} {
		if x := g.Dispatch(context.Background(), r); x.Status != 400 {
			t.Fatal(x)
		}
	}
}
func TestOpenAPISlice2(t *testing.T) {
	paths := OpenAPI()["paths"].(map[string]any)
	for _, p := range []string{"/v1/servers/{server}/resources", "/v1/servers/{server}/resources/read", "/v1/servers/{server}/resources/templates", "/v1/servers/{server}/prompts", "/v1/servers/{server}/prompts/{prompt}"} {
		if paths[p] == nil {
			t.Fatal(p)
		}
	}
}

func TestRemoteOriginHop(t *testing.T) {
	var hits atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.WriteHeader(200) }))
	defer s.Close()
	u, _ := url.Parse("http://127.0.0.1:1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	state := remoteState{ctx: ctx, origin: u, base: &http.Transport{}, headers: http.Header{"Authorization": []string{"Bearer synthetic"}}}
	req, _ := http.NewRequest("GET", s.URL, nil)
	r, e := state.RoundTrip(req)
	if e == nil {
		r.Body.Close()
		t.Fatal("origin change accepted")
	}
	if hits.Load() != 0 {
		t.Fatal("credential origin hop")
	}
}
func TestRemoteRetryOnce(t *testing.T) {
	for _, failInit := range []bool{false, true} {
		t.Run(map[bool]string{false: "expired-twice", true: "reinitialize-fails"}[failInit], func(t *testing.T) {
			var initializations atomic.Int64
			s := mcp.NewServer(&mcp.Implementation{Name: "retry", Version: "1"}, nil)
			h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{JSONResponse: true})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" {
					b, _ := io.ReadAll(r.Body)
					r.Body = io.NopCloser(bytes.NewReader(b))
					var message struct {
						Method string `json:"method"`
					}
					json.Unmarshal(b, &message)
					if message.Method == "initialize" {
						n := initializations.Add(1)
						if failInit && n > 1 {
							w.WriteHeader(403)
							return
						}
					}
					if message.Method == "tools/list" {
						w.WriteHeader(404)
						return
					}
				}
				h.ServeHTTP(w, r)
			}))
			defer server.Close()
			g, _ := New(Config{Servers: map[string]ServerConfig{"r": {URL: server.URL, ProtocolVersion: "2025-11-25"}}}, Options{})
			defer g.Close()
			r := request(g, "GET", "/v1/servers/r/tools", "")
			b, _ := json.Marshal(r.Body)
			want := "SESSION_EXPIRED"
			if failInit {
				want = "SESSION_REINITIALIZE_FAILED"
			}
			if r.Status != 502 || !strings.Contains(string(b), want) || initializations.Load() != 2 {
				t.Fatal(r, string(b), initializations.Load())
			}
		})
	}
}

func TestRemoteChangeKeepsActiveCall(t *testing.T) {
	g, s := remoteFixture(t, true)
	entered := make(chan struct{})
	events := make(chan Event, 20)
	g.opts.Event = func(e Event) { events <- e }
	s.AddTool(&mcp.Tool{Name: "hold", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan Response, 1)
	go func() {
		done <- g.Dispatch(ctx, Request{Method: "POST", Path: "/v1/servers/remote/tools/hold/call", Body: json.RawMessage(`{}`)})
	}()
	<-entered
	previous := g.entries["remote"].session
	s.AddTool(&mcp.Tool{Name: "new", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{}, nil
	})
	for e := range events {
		if e.Kind == "list_changed" {
			break
		}
	}
	r := request(g, "GET", "/v1/servers/remote/tools", "")
	if r.Status != 200 || g.entries["remote"].session != previous {
		t.Fatal("list refresh interrupted an active generation", r)
	}
	select {
	case x := <-done:
		t.Fatal("active call ended on list change", x)
	default:
	}
	cancel()
	if x := <-done; x.Status != 499 {
		t.Fatal(x)
	}
}
