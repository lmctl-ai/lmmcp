package bridge

import (
	"context"
	"encoding/json"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"net/http"
	"net/http/httptest"
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
		`{"url":"http://example.com/mcp"}`, `{"url":"http://user:pass@127.0.0.1/mcp"}`, `{"url":"http://127.0.0.1/mcp","headers":{"Authorization":"Bearer literal"}}`, `{"url":"http://127.0.0.1/mcp","type":"typo"}`, `{"url":"http://127.0.0.1/mcp","type":"sse","transport":"auto"}`, `{"url":"http://127.0.0.1/mcp","headers":{"Content-Type":"${BAD}"}}`,
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
