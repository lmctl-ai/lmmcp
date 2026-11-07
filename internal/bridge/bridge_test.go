package bridge

import (
	"context"
	"encoding/json"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const canary = "SYNTHETIC_TEST_SECRET_NEVER_REAL_350"

func TestMockProcess(t *testing.T) {
	if os.Getenv("LMMCP_TEST_SERVER") != "1" {
		return
	}
	if file := os.Getenv("LMMCP_PID_FILE"); file != "" {
		os.WriteFile(file, []byte(strconv.Itoa(os.Getpid())), 0600)
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	type Args struct {
		Value string `json:"value"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "echo", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, r *mcp.CallToolRequest, a Args) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: a.Value}}}, nil, nil
	})
	mcp.AddTool(s, &mcp.Tool{Name: "slow", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, r *mcp.CallToolRequest, a struct{}) (*mcp.CallToolResult, any, error) {
		r.Session.NotifyProgress(context.Background(), &mcp.ProgressNotificationParams{ProgressToken: r.Params.GetProgressToken(), Progress: 1, Total: 2})
		<-ctx.Done()
		r.Session.NotifyProgress(context.Background(), &mcp.ProgressNotificationParams{ProgressToken: r.Params.GetProgressToken(), Progress: 2, Total: 2})
		return nil, nil, ctx.Err()
	})
	mcp.AddTool(s, &mcp.Tool{Name: "progress", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, r *mcp.CallToolRequest, a struct{}) (*mcp.CallToolResult, any, error) {
		for i := 1; i <= 3; i++ {
			r.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{ProgressToken: r.Params.GetProgressToken(), Progress: float64(i), Total: 3, Message: canary})
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "done"}}}, nil, nil
	})
	yes := true
	mcp.AddTool(s, &mcp.Tool{Name: "destroy", Annotations: &mcp.ToolAnnotations{DestructiveHint: &yes}}, func(ctx context.Context, r *mcp.CallToolRequest, a struct{}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	})
	mcp.AddTool(s, &mcp.Tool{Name: "error"}, func(ctx context.Context, r *mcp.CallToolRequest, a struct{}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: canary}}}, nil, nil
	})
	mcp.AddTool(s, &mcp.Tool{Name: "crash"}, func(ctx context.Context, r *mcp.CallToolRequest, a struct{}) (*mcp.CallToolResult, any, error) {
		os.Exit(7)
		return nil, nil, nil
	})
	mcp.AddTool(s, &mcp.Tool{Name: "secret"}, func(ctx context.Context, r *mcp.CallToolRequest, a struct{}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: os.Getenv("TEST_TOKEN")}}}, nil, nil
	})
	s.AddResource(&mcp.Resource{URI: "mock://data", Name: "fixture"}, func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: "mock://data", Text: "fixture"}}}, nil
	})
	s.AddPrompt(&mcp.Prompt{Name: "hello"}, func(context.Context, *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: "hello"}}}}, nil
	})
	io.WriteString(os.Stderr, canary)
	s.Run(context.Background(), &mcp.StdioTransport{MaxLineLength: -1})
	os.Exit(0)
}
func fixture(t *testing.T, readonly bool) (*Gateway, chan Event) {
	t.Helper()
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	events := make(chan Event, 100)
	cfg := Config{Servers: map[string]ServerConfig{"mock": {Command: exe, Args: []string{"-test.run=TestMockProcess"}, Env: map[string]string{"LMMCP_TEST_SERVER": "1", "TEST_TOKEN": "${FIXTURE_SECRET}"}}}}
	g, e := New(cfg, Options{ReadOnly: readonly, Lookup: func(n string) (string, bool) { return canary, n == "FIXTURE_SECRET" }, Event: func(e Event) { events <- e }})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(g.Close)
	return g, events
}
func request(g *Gateway, method, path, body string) Response {
	return g.Dispatch(context.Background(), Request{Method: method, Path: path, Body: json.RawMessage(body)})
}
func TestConfig(t *testing.T) {
	for _, text := range []string{`{"mcpServers":{"a":{"command":"echo","args":[]}}}`, `{"mcpServers":{"a":{"url":"https://example.invalid/mcp","headers":{"Authorization":"Bearer ${TOKEN}"}}}}`} {
		if _, e := Load([]byte(text)); e != nil {
			t.Fatal(e)
		}
	}
	for _, text := range []string{`{}`, `{"mcpServers":{"a":{"command":"echo","oops":1}}}`, `{"mcpServers":{"a":{"command":"echo"},"a":{"command":"other"}}}`, `{"mcpServers":{"a":{"command":"echo","env":{"TOKEN":"${BAD-NAME}"}}}}`} {
		if _, e := Load([]byte(text)); e == nil {
			t.Fatal(text)
		}
	}
}
func TestLazyDiscoveryCall(t *testing.T) {
	g, _ := fixture(t, false)
	r := request(g, "GET", "/v1/servers", "")
	if r.Status != 200 || g.StartCount() != 0 {
		t.Fatal(r)
	}
	r = request(g, "GET", "/v1/servers/mock/tools", "")
	b, _ := json.Marshal(r.Body)
	if r.Status != 200 || !strings.Contains(string(b), "inputSchema") || !strings.Contains(string(b), "echo") {
		t.Fatal(r, string(b))
	}
	r = request(g, "POST", "/v1/servers/mock/tools/echo/call", `{"value":"hello"}`)
	b, _ = json.Marshal(r.Body)
	if r.Status != 200 || !strings.Contains(string(b), "hello") {
		t.Fatal(r, string(b))
	}
	if g.StartCount() != 1 {
		t.Fatal("not reused")
	}
}
func TestPolicy(t *testing.T) {
	g, _ := fixture(t, true)
	if r := request(g, "POST", "/v1/servers/mock/tools/destroy/call", `{}`); r.Status != 403 {
		t.Fatal(r)
	}
	if r := request(g, "POST", "/v1/servers/mock/tools/echo/call", `{"value":"x"}`); r.Status != 200 {
		t.Fatal(r)
	}
	g.cfg.Servers["mock"] = ServerConfig{DenyTools: []string{"echo"}}
	if r := request(g, "POST", "/v1/servers/mock/tools/echo/call", `{"value":"x"}`); r.Status != 403 {
		t.Fatal(r)
	}
}
func TestErrorsAndSecret(t *testing.T) {
	g, _ := fixture(t, false)
	for _, tool := range []string{"error", "secret"} {
		r := request(g, "POST", "/v1/servers/mock/tools/"+tool+"/call", `{}`)
		b, _ := json.Marshal(r.Body)
		if r.Status != 502 || strings.Contains(string(b), canary) {
			t.Fatal(r, string(b))
		}
	}
	for _, body := range []string{`[]`, `{"value":1,"value":2}`, `{`} {
		if r := request(g, "POST", "/v1/servers/mock/tools/echo/call", body); r.Status != 400 {
			t.Fatal(r)
		}
	}
}
func TestLargeNoImplicitCap(t *testing.T) {
	g, _ := fixture(t, false)
	body, _ := json.Marshal(map[string]string{"value": strings.Repeat("x", 17<<20)})
	r := request(g, "POST", "/v1/servers/mock/tools/echo/call", string(body))
	if r.Status != 200 {
		t.Fatal(r)
	}
	b, _ := json.Marshal(r.Body)
	if len(b) < 17<<20 {
		t.Fatal("clipped")
	}
}
func TestConcurrent(t *testing.T) {
	g, _ := fixture(t, false)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r := request(g, "POST", "/v1/servers/mock/tools/echo/call", `{"value":"x"}`); r.Status != 200 {
				t.Error(r)
			}
		}()
	}
	wg.Wait()
	if g.StartCount() != 1 {
		t.Fatal(g.StartCount())
	}
}
func TestCancelAndReuse(t *testing.T) {
	g, events := fixture(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Response, 1)
	go func() {
		done <- g.Dispatch(ctx, Request{Method: "POST", Path: "/v1/servers/mock/tools/slow/call", Body: json.RawMessage(`{}`), ID: "slow1"})
	}()
	for e := range events {
		if e.Kind == "progress" && e.Progress == 1 {
			break
		}
	}
	cancel()
	if r := <-done; r.Status != 499 {
		t.Fatal(r)
	}
	for e := range events {
		if e.Kind == "progress" && e.Progress == 2 {
			break
		}
	}
	if r := request(g, "POST", "/v1/servers/mock/tools/echo/call", `{"value":"still alive"}`); r.Status != 200 {
		t.Fatal(r)
	}
}
func TestHTTPAuth(t *testing.T) {
	g, _ := fixture(t, false)
	handler := g.Handler("synthetic-bearer", "")
	for _, modify := range []string{"missing", "wrong", "origin"} {
		r := httptest.NewRequest("GET", "http://127.0.0.1/v1/servers", nil)
		if modify != "missing" {
			r.Header.Set("Authorization", "Bearer synthetic-bearer")
		}
		if modify == "wrong" {
			r.Header.Set("Authorization", "Bearer wrong")
		}
		if modify == "origin" {
			r.Header.Set("Origin", "https://evil.invalid")
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code < 400 {
			t.Fatal(modify, w.Code)
		}
	}
	r := httptest.NewRequest("GET", "http://127.0.0.1/v1/servers", nil)
	r.Header.Set("Authorization", "Bearer synthetic-bearer")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatal(w.Code)
	}
}
func TestChildReapedAndRestart(t *testing.T) {
	g, _ := fixture(t, false)
	if r := request(g, "POST", "/v1/servers/mock/tools/crash/call", `{}`); r.Status != 502 {
		t.Fatal(r)
	}
	if r := request(g, "POST", "/v1/servers/mock/tools/echo/call", `{"value":"restarted"}`); r.Status != 200 {
		t.Fatal(r)
	}
	if g.StartCount() != 2 {
		t.Fatal(g.StartCount())
	}
	entry := g.entries["mock"]
	child := entry.process
	g.Close()
	if child.state == nil || child.command.ProcessState != child.state {
		t.Fatal("child was not waited/reaped")
	}
}
func TestDuplicateAndExplicitCancel(t *testing.T) {
	g, events := fixture(t, false)
	done := make(chan Response, 1)
	go func() {
		done <- g.Dispatch(context.Background(), Request{ID: "active", Method: "POST", Path: "/v1/servers/mock/tools/slow/call", Body: json.RawMessage(`{}`)})
	}()
	for e := range events {
		if e.Kind == "progress" {
			break
		}
	}
	if r := g.Dispatch(context.Background(), Request{ID: "active", Method: "GET", Path: "/v1/servers"}); r.Status != 409 {
		t.Fatal(r)
	}
	if r := request(g, "DELETE", "/v1/requests/active", ""); r.Status != 200 {
		t.Fatal(r)
	}
	if r := <-done; r.Status != 499 {
		t.Fatal(r)
	}
}
func TestDisconnectCancels(t *testing.T) {
	g, events := fixture(t, false)
	srv := httptest.NewServer(g.Handler("", ""))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	r, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+"/v1/servers/mock/tools/slow/call", strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "application/json")
	done := make(chan error, 1)
	go func() {
		res, e := http.DefaultClient.Do(r)
		if res != nil {
			res.Body.Close()
		}
		done <- e
	}()
	for e := range events {
		if e.Kind == "progress" && e.Progress == 1 {
			break
		}
	}
	cancel()
	if e := <-done; e == nil {
		t.Fatal("client did not cancel")
	}
	for e := range events {
		if e.Kind == "progress" && e.Progress == 2 {
			break
		}
	}
}
func TestLinesSameAPIAndLargeInput(t *testing.T) {
	g, _ := fixture(t, false)
	var out strings.Builder
	data, _ := json.Marshal(map[string]any{"id": "large", "method": "POST", "path": "/v1/servers/mock/tools/echo/call", "body": map[string]string{"value": strings.Repeat("x", 17<<20)}})
	if c := g.Lines(context.Background(), strings.NewReader(string(data)+"\n"), &out); c != 0 {
		t.Fatal(c)
	}
	var response map[string]any
	if json.Unmarshal([]byte(out.String()), &response) != nil || response["status"] != float64(200) || len(out.String()) < 17<<20 {
		t.Fatal("line clipped or failed")
	}
}
func TestLinesCancelWhileCallPending(t *testing.T) {
	g, events := fixture(t, false)
	in, w := io.Pipe()
	out, reader := io.Pipe()
	done := make(chan int, 1)
	go func() { done <- g.Lines(context.Background(), in, reader); reader.Close() }()
	io.WriteString(w, "{\"id\":\"slow\",\"method\":\"POST\",\"path\":\"/v1/servers/mock/tools/slow/call\",\"body\":{}}\n")
	for e := range events {
		if e.Kind == "progress" {
			break
		}
	}
	io.WriteString(w, "{\"id\":\"cancel\",\"method\":\"DELETE\",\"path\":\"/v1/requests/slow\"}\n")
	w.Close()
	dec := json.NewDecoder(out)
	seen := map[float64]bool{}
	for i := 0; i < 2; i++ {
		var r map[string]any
		if e := dec.Decode(&r); e != nil {
			t.Fatal(e)
		}
		seen[r["status"].(float64)] = true
	}
	if !seen[200] || !seen[499] {
		t.Fatal(seen)
	}
	if <-done != 0 {
		t.Fatal("stdio failed")
	}
}
func TestProgressAndPrivateErrors(t *testing.T) {
	g, events := fixture(t, false)
	if r := request(g, "POST", "/v1/servers/mock/tools/progress/call", `{}`); r.Status != 200 {
		t.Fatal(r)
	}
	for i := 0; i < 3; {
		e := <-events
		b, _ := json.Marshal(e)
		if strings.Contains(string(b), canary) {
			t.Fatal("progress leaked")
		}
		if e.Kind == "progress" {
			i++
		}
	}
	if r := request(g, "GET", "/v1/servers/unknown/tools", ""); r.Status != 404 {
		t.Fatal(r)
	}
}
func TestAPIContractEdges(t *testing.T) {
	g, _ := fixture(t, false)
	for _, r := range []Request{{Method: "GET", Path: "/v1/servers/mock/tools", ID: "bad id"}, {Method: "POST", Path: "/v1/servers/mock/tools/echo/call?token=secret", Body: json.RawMessage(`{}`)}} {
		if out := g.Dispatch(context.Background(), r); out.Status != 400 {
			t.Fatal(out)
		}
	}
	if r := request(g, "GET", "/v1/servers/mock/resources", ""); r.Status != 501 {
		t.Fatal(r)
	}
	if r := request(g, "POST", "/v1/servers/mock/tools/absent/call", `{}`); r.Status != 404 {
		t.Fatal(r)
	}
	for _, status := range []int{200, 400, 502, 499} {
		want := map[int]int{200: 0, 400: 2, 502: 3, 499: 4}[status]
		if Exit(Response{Status: status}) != want {
			t.Fatal(status)
		}
	}
}
func TestAllowAndHostPolicy(t *testing.T) {
	g, _ := fixture(t, false)
	c := g.cfg.Servers["mock"]
	c.AllowTools = []string{}
	g.cfg.Servers["mock"] = c
	if r := request(g, "POST", "/v1/servers/mock/tools/echo/call", `{"value":"x"}`); r.Status != 403 || g.StartCount() != 0 {
		t.Fatal(r)
	}
	handler := g.Handler("", "127.0.0.1:8888")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "http://evil.invalid/v1/servers", nil))
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
}
func TestOnlyDeclaredEnvIsRead(t *testing.T) {
	exe, _ := os.Executable()
	var mu sync.Mutex
	var names []string
	cfg := Config{Servers: map[string]ServerConfig{"mock": {Command: exe, Args: []string{"-test.run=TestMockProcess"}, Env: map[string]string{"LMMCP_TEST_SERVER": "1"}}}}
	g, _ := New(cfg, Options{Lookup: func(n string) (string, bool) {
		mu.Lock()
		names = append(names, n)
		mu.Unlock()
		return "synthetic", true
	}})
	defer g.Close()
	if r := request(g, "GET", "/v1/servers/mock/tools", ""); r.Status != 200 {
		t.Fatal(r)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, n := range names {
		if n != "PATH" {
			t.Fatal("read undeclared environment", n)
		}
	}
}
func TestOwnedShutdownWait(t *testing.T) {
	g, _ := fixture(t, false)
	request(g, "GET", "/v1/servers/mock/tools", "")
	child := g.entries["mock"].process
	defer func() {
		if child.command.ProcessState == nil {
			_ = child.command.Wait()
		}
	}()
	g.Close()
	if child.command.ProcessState == nil || child.state == nil {
		t.Fatal("owned child not reaped")
	}
}
func TestRemoteDoesNotConnect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("remote transport executed in slice 1") }))
	defer srv.Close()
	g, _ := New(Config{Servers: map[string]ServerConfig{"remote": {URL: srv.URL, Headers: map[string]string{"Authorization": "${NOT_READ}"}}}}, Options{Lookup: func(string) (string, bool) { t.Error("remote credential read"); return "", false }})
	defer g.Close()
	if r := request(g, "GET", "/v1/servers/remote/tools", ""); r.Status != 501 {
		t.Fatal(r)
	}
}
func TestToolErrorCode(t *testing.T) {
	g, _ := fixture(t, false)
	r := request(g, "POST", "/v1/servers/mock/tools/error/call", `{}`)
	b, _ := json.Marshal(r.Body)
	if !strings.Contains(string(b), `"code":"TOOL_ERROR"`) {
		t.Fatal(string(b))
	}
}
func TestMissingEnvNoChild(t *testing.T) {
	exe, _ := os.Executable()
	g, _ := New(Config{Servers: map[string]ServerConfig{"mock": {Command: exe, Args: []string{"-test.run=TestMockProcess"}, Env: map[string]string{"LMMCP_TEST_SERVER": "1", "KEY": "${MISSING}"}}}}, Options{Lookup: func(string) (string, bool) { return "", false }})
	defer g.Close()
	if r := request(g, "GET", "/v1/servers/mock/tools", ""); r.Status != 502 || g.StartCount() != 0 {
		t.Fatal(r)
	}
}
func TestEscapedSecretsAreBlocked(t *testing.T) {
	g, _ := fixture(t, false)
	secret := "synthetic\nquoted\"secret-é"
	g.opts.Lookup = func(n string) (string, bool) { return secret, n == "FIXTURE_SECRET" }
	r := request(g, "POST", "/v1/servers/mock/tools/secret/call", `{}`)
	b, _ := json.Marshal(r.Body)
	if r.Status != 502 || strings.Contains(string(b), "quoted") {
		t.Fatal(r)
	}
}
