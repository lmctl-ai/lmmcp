package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

const canary = "SYNTHETIC_TEST_SECRET_NEVER_REAL_350"

func TestMockProcess(t *testing.T) {
	if os.Getenv("LMMCP_TEST_PARENT_HELPER") == "1" {
		exe, _ := os.Executable()
		pidFile := os.Getenv("LMMCP_PID_FILE")
		cfg := Config{Servers: map[string]ServerConfig{"mock": {Command: exe, Args: []string{"-test.run=TestMockProcess"}, Env: map[string]string{"LMMCP_TEST_SERVER": "1", "LMMCP_HOLD_FOREVER": "1", "LMMCP_PID_FILE": pidFile}}}}
		g, _ := New(cfg, Options{})
		_ = request(g, "GET", "/v1/servers/mock/tools", "")
		for {
			time.Sleep(time.Hour)
		}
	}
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
		r.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{ProgressToken: canary, Progress: 1, Total: 3, Message: canary})
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
	if os.Getenv("LMMCP_HOLD_FOREVER") == "1" {
		for {
			time.Sleep(time.Hour)
		}
	}
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
		if r := request(g, "POST", "/v1/servers/mock/prompts/hello", body); r.Status != 400 {
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
	if r := request(g, "GET", "/v1/servers/mock/resources", ""); r.Status != 200 {
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
func TestMissingRemoteCredentialDoesNotConnect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("remote transport executed in slice 1") }))
	defer srv.Close()
	g, _ := New(Config{Servers: map[string]ServerConfig{"remote": {URL: srv.URL, Headers: map[string]string{"Authorization": "${NOT_READ}"}}}}, Options{Lookup: func(string) (string, bool) { return "", false }})
	defer g.Close()
	if r := request(g, "GET", "/v1/servers/remote/tools", ""); r.Status != 400 || Exit(r) != 2 {
		t.Fatal(r)
	}
}
func TestToolErrorCode(t *testing.T) {
	g, _ := fixture(t, false)
	r := request(g, "POST", "/v1/servers/mock/tools/error/call", `{}`)
	b, _ := json.Marshal(r.Body)
	if !strings.Contains(string(b), `"code":"SECRET_OUTPUT"`) {
		t.Fatal(string(b))
	}
}
func TestMissingEnvNoChild(t *testing.T) {
	exe, _ := os.Executable()
	g, _ := New(Config{Servers: map[string]ServerConfig{"mock": {Command: exe, Args: []string{"-test.run=TestMockProcess"}, Env: map[string]string{"LMMCP_TEST_SERVER": "1", "KEY": "${MISSING}"}}}}, Options{Lookup: func(string) (string, bool) { return "", false }})
	defer g.Close()
	if r := request(g, "GET", "/v1/servers/mock/tools", ""); r.Status != 400 || Exit(r) != 2 || g.StartCount() != 0 {
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

type failingOutputWriter struct{}

func (failingOutputWriter) Write([]byte) (int, error) {
	return 0, errors.New("broken pipe")
}

func TestStdioSigintOpenPipeAndBrokenOutput(t *testing.T) {
	g, _ := fixture(t, false)

	// 1. Broken stdout returns exit 3 (not 4)
	if code := g.Lines(context.Background(), strings.NewReader("{\"id\":\"1\",\"method\":\"GET\",\"path\":\"/health\"}\n"), failingOutputWriter{}); code != 3 {
		t.Fatalf("broken stdout returned %d, want 3", code)
	}

	// 2. Partial line at EOF (no newline) is rejected with 400, and valid id is preserved on decode failure
	var outPartial, outBadField bytes.Buffer
	if code := g.Lines(context.Background(), strings.NewReader("{\"id\":\"unterminated\",\"method\":\"GET\",\"path\":\"/health\"}"), &outPartial); code != 0 {
		t.Fatal(code)
	}
	if !strings.Contains(outPartial.String(), `"status":400`) || !strings.Contains(outPartial.String(), `"id":"unterminated"`) {
		t.Fatalf("unterminated EOF line not rejected with id preserved: %s", outPartial.String())
	}
	if code := g.Lines(context.Background(), strings.NewReader("{\"id\":\"req-99\",\"method\":\"GET\",\"path\":\"/health\",\"unknown\":1}\n"), &outBadField); code != 0 {
		t.Fatal(code)
	}
	if !strings.Contains(outBadField.String(), `"status":400`) || !strings.Contains(outBadField.String(), `"id":"req-99"`) {
		t.Fatalf("invalid request did not preserve id req-99: %s", outBadField.String())
	}

	// 3. Open blocking OS pipe stops promptly on context cancel without leaking goroutines
	if runtime.GOOS != "windows" {
		runtime.GC()
		time.Sleep(20 * time.Millisecond)
		baseG := runtime.NumGoroutine()
		rFile, wFile, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer wFile.Close()
		defer rFile.Close()

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan int, 1)
		outPr, outPw := io.Pipe()
		defer outPr.Close()
		defer outPw.Close()
		go func() {
			done <- g.Lines(ctx, rFile, outPw)
		}()
		_, _ = wFile.WriteString("{\"id\":\"1\",\"method\":\"GET\",\"path\":\"/health\"}\n")
		firstReply := make([]byte, 256)
		if _, err := outPr.Read(firstReply); err != nil {
			t.Fatal(err)
		}
		cancel()
		select {
		case code := <-done:
			if code != 4 {
				t.Fatalf("expected exit 4 on cancel, got %d", code)
			}
		case <-time.After(1 * time.Second):
			_ = wFile.Close()
			t.Fatal("Lines hung on open blocking pipe after context cancel")
		}
		runtime.GC()
		time.Sleep(50 * time.Millisecond)
		if afterG := runtime.NumGoroutine(); afterG > baseG+1 {
			t.Fatalf("goroutine leak after Lines cancel: before=%d after=%d", baseG, afterG)
		}
	}
}

func TestParentSigkillKillsChildOnLinuxWithPs(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux Pdeathsig check")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pidFile := t.TempDir() + "/child.pid"
	parent := exec.Command(exe, "-test.run=TestMockProcess")
	parent.Env = append(os.Environ(), "LMMCP_TEST_PARENT_HELPER=1", "LMMCP_PID_FILE="+pidFile)
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	var childPID int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(pidFile); err == nil && len(b) > 0 {
			childPID, _ = strconv.Atoi(strings.TrimSpace(string(b)))
			if childPID > 0 {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if childPID <= 0 {
		_ = parent.Process.Kill()
		_ = parent.Wait()
		t.Fatal("child PID not recorded")
	}
	defer func() {
		if childPID > 0 {
			_ = syscall.Kill(childPID, syscall.SIGKILL)
		}
	}()
	_ = parent.Process.Kill()
	_ = parent.Wait()
	// Wait briefly for kernel Pdeathsig delivery and verify via `ps`
	alive := true
	for i := 0; i < 25; i++ {
		out, _ := exec.Command("ps", "-p", strconv.Itoa(childPID), "-o", "pid=,stat=").Output()
		text := strings.TrimSpace(string(out))
		if text == "" || strings.Contains(text, "Z") {
			alive = false
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if alive {
		t.Fatalf("child process %d survived parent SIGKILL (ps shows still running)", childPID)
	}
}

func TestMultiHeaderOriginHostAndProtect(t *testing.T) {
	g, _ := fixture(t, false)
	handler := g.Handler("synthetic-bearer-token", "127.0.0.1:8888")

	// 1. Multi-value Origin header with empty first value must be refused (403)
	req := httptest.NewRequest("GET", "http://127.0.0.1:8888/v1/servers", nil)
	req.Header.Set("Authorization", "Bearer synthetic-bearer-token")
	req.Header.Add("Origin", "")
	req.Header.Add("Origin", "https://evil.invalid")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("multi-Origin bypass accepted with status %d", w.Code)
	}

	// 2. Duplicate Host header values must be refused (403)
	reqHost := httptest.NewRequest("GET", "http://127.0.0.1:8888/v1/servers", nil)
	reqHost.Header.Set("Authorization", "Bearer synthetic-bearer-token")
	reqHost.Header.Add("Host", "127.0.0.1:8888")
	reqHost.Header.Add("Host", "evil.invalid")
	wHost := httptest.NewRecorder()
	handler.ServeHTTP(wHost, reqHost)
	if wHost.Code != 403 {
		t.Fatalf("duplicate Host headers accepted with status %d", wHost.Code)
	}

	// 3. Protect blocks numeric secrets in json.Number
	if r := Protect(success(map[string]any{"pin": int64(9876543210123456)}), []string{"9876543210123456"}); r.Status != 502 {
		t.Fatalf("numeric secret in json.Number bypassed Protect: %#v", r)
	}

	// 4. Short env values (< 8 bytes, e.g. "1" or "dev") do not over-block normal responses
	exe, _ := os.Executable()
	cfgShort := Config{Servers: map[string]ServerConfig{"mock": {Command: exe, Args: []string{"-test.run=TestMockProcess"}, Env: map[string]string{"LMMCP_TEST_SERVER": "${SHORT_FLAG}"}, EnvAllowlist: []string{"PATH", "SHORT_MODE"}}}}
	gShort, _ := New(cfgShort, Options{Lookup: func(n string) (string, bool) {
		switch n {
		case "PATH":
			return os.Getenv("PATH"), true
		case "SHORT_FLAG":
			return "1", true
		case "SHORT_MODE":
			return "dev", true
		}
		return "", false
	}})
	defer gShort.Close()
	if r := request(gShort, "POST", "/v1/servers/mock/tools/echo/call", `{"value":"hello 1 dev world"}`); r.Status != 200 {
		t.Fatalf("short env values over-blocked tool output: %#v", r)
	}
}

func TestRejectUnknownQueryParameters(t *testing.T) {
	g, _ := fixture(t, false)
	handler := g.Handler("", "")
	for _, target := range []string{
		"http://127.0.0.1/v1/servers?token=secret",
		"http://127.0.0.1/v1/servers/mock/tools?x=1",
		"http://127.0.0.1/v1/servers/mock/resources/read?uri=mock://data&extra=1",
		"http://127.0.0.1/v1/servers/mock/resources/read?other=1",
	} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", target, nil))
		if w.Code != 400 || Exit(Response{Status: w.Code}) != 2 {
			t.Fatalf("unexpected status %d for %s", w.Code, target)
		}
	}
	if r := g.Dispatch(context.Background(), Request{Method: "GET", Path: "/v1/servers", Query: map[string]string{"token": "secret"}}); r.Status != 400 {
		t.Fatalf("stdio query parameter not rejected: %#v", r)
	}
}

func TestSurvivorMutationsAndGuards(t *testing.T) {
	// 1. Disabled server returns 400 SERVER_DISABLED (exit 2) without starting process (kills mut04)
	g, _ := fixture(t, false)
	c := g.cfg.Servers["mock"]
	c.Disabled = true
	g.cfg.Servers["mock"] = c
	if r := request(g, "GET", "/v1/servers/mock/tools", ""); r.Status != 400 || Exit(r) != 2 || g.StartCount() != 0 {
		t.Fatalf("disabled server failed guard: %#v", r)
	}
	c.Disabled = false
	g.cfg.Servers["mock"] = c

	// 2. POST without Content-Type: application/json returns 400 CONTENT_TYPE (kills mut05)
	handler := g.Handler("", "")
	req := httptest.NewRequest("POST", "http://127.0.0.1/v1/servers/mock/tools/echo/call", strings.NewReader(`{"value":"x"}`))
	req.Header.Set("Content-Type", "text/plain")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "CONTENT_TYPE") {
		t.Fatalf("POST with text/plain accepted: %d %s", w.Code, w.Body.String())
	}

	// 3. Pagination loop detection (kills mut02) and NeedsInput guard (kills mut03)
	var pageCalls atomic.Int64
	s := mcp.NewServer(&mcp.Implementation{Name: "loop", Version: "1"}, nil)
	s.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "tools/list" {
				if pageCalls.Add(1) > 3 {
					return &mcp.ListToolsResult{}, nil
				}
				return &mcp.ListToolsResult{Tools: []*mcp.Tool{{Name: "t", InputSchema: map[string]any{"type": "object"}}}, NextCursor: "repeat"}, nil
			}
			return next(ctx, method, req)
		}
	})
	srv := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{JSONResponse: true}))
	defer srv.Close()
	gLoop, _ := New(Config{Servers: map[string]ServerConfig{"r": {URL: srv.URL}}}, Options{})
	defer gLoop.Close()
	rLoop := request(gLoop, "GET", "/v1/servers/r/tools", "")
	bLoop, _ := json.Marshal(rLoop.Body)
	if rLoop.Status != 502 || !strings.Contains(string(bLoop), "INVALID_PAGINATION") {
		t.Fatalf("pagination loop not caught: %#v %s", rLoop, string(bLoop))
	}

	sInput := mcp.NewServer(&mcp.Implementation{Name: "input", Version: "1"}, nil)
	sInput.AddTool(&mcp.Tool{Name: "ask", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var ctr mcp.CallToolResult
		_ = json.Unmarshal([]byte(`{"resultType":"input_required","content":[]}`), &ctr)
		return &ctr, nil
	})
	srvInput := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return sInput }, &mcp.StreamableHTTPOptions{JSONResponse: true}))
	defer srvInput.Close()
	gInput, _ := New(Config{Servers: map[string]ServerConfig{"in": {URL: srvInput.URL}}}, Options{})
	defer gInput.Close()
	rInput := request(gInput, "POST", "/v1/servers/in/tools/ask/call", `{}`)
	bInput, _ := json.Marshal(rInput.Body)
	if rInput.Status != 502 || !strings.Contains(string(bInput), "INPUT_REQUIRED") {
		t.Fatalf("NeedsInput guard not triggered: %#v %s", rInput, string(bInput))
	}
}

func TestRejectInvalidUTF8AndConfigValidation(t *testing.T) {
	// 1. Invalid UTF-8 in Load, HTTP body, and stdio line is rejected with located error
	if _, err := Load([]byte("{\"mcpServers\":{\"a\":{\"command\":\"echo\xff\"}}}")); err == nil || !strings.Contains(err.Error(), "UTF8") {
		t.Fatalf("expected invalid UTF-8 error in Load, got %v", err)
	}
	g, _ := fixture(t, false)
	r := g.Dispatch(context.Background(), Request{Method: "POST", Path: "/v1/servers/mock/tools/echo/call", Body: json.RawMessage([]byte("{\"value\":\"bad\xff\"}"))})
	b, _ := json.Marshal(r.Body)
	if r.Status != 400 || !strings.Contains(string(b), "byte ") {
		t.Fatalf("invalid UTF-8 in POST body not rejected with location: %#v %s", r, string(b))
	}
	var out bytes.Buffer
	g.Lines(context.Background(), bytes.NewReader([]byte("{\"id\":\"1\",\"method\":\"GET\",\"path\":\"/v1/servers/\xff/tools\"}\n")), &out)
	if !strings.Contains(out.String(), `"status":400`) || !strings.Contains(out.String(), "byte ") {
		t.Fatalf("invalid UTF-8 in stdio line not rejected with location: %s", out.String())
	}

	// 2. Load rejects NUL in env values and literal headers
	for _, badCfg := range []string{
		`{"mcpServers":{"a":{"command":"echo","env":{"FOO":"a\u0000b"}}}}`,
		`{"mcpServers":{"a":{"url":"https://example.invalid/mcp","headers":{"X-Test":"a\u0000${TOKEN}"}}}}`,
	} {
		if _, err := Load([]byte(badCfg)); err == nil {
			t.Fatalf("Load accepted invalid config: %s", badCfg)
		}
	}
}

func TestOpenAPIAndRouteMethodsAndDeleteIDValidation(t *testing.T) {
	doc := OpenAPI()
	paths := doc["paths"].(map[string]any)
	defResp := paths["/v1/servers"].(map[string]any)["get"].(map[string]any)["responses"].(map[string]any)["default"].(map[string]any)
	if defResp["content"] == nil {
		t.Fatalf("OpenAPI default response missing content schema: %#v", defResp)
	}

	g, _ := fixture(t, false)
	for _, tc := range []struct {
		method, path, allow string
	}{
		{"POST", "/health", "GET"},
		{"POST", "/v1/openapi.json", "GET"},
		{"POST", "/v1/servers", "GET"},
		{"POST", "/v1/servers/mock/tools", "GET"},
		{"GET", "/v1/servers/mock/tools/echo/call", "POST"},
		{"GET", "/v1/requests/r1", "DELETE"},
	} {
		r := g.Dispatch(context.Background(), Request{Method: tc.method, Path: tc.path})
		if r.Status != 405 || r.Headers["Allow"] != tc.allow {
			t.Fatalf("%s %s returned status=%d allow=%q, want 405 allow=%q", tc.method, tc.path, r.Status, r.Headers["Allow"], tc.allow)
		}
	}

	// DELETE /v1/requests/{id} validates target ID and request ID
	if r := g.Dispatch(context.Background(), Request{Method: "DELETE", Path: "/v1/requests/bad%20id"}); r.Status != 400 {
		t.Fatalf("DELETE with invalid target id returned %d, want 400", r.Status)
	}
	if r := g.Dispatch(context.Background(), Request{ID: "bad id", Method: "DELETE", Path: "/v1/requests/r1"}); r.Status != 400 {
		t.Fatalf("DELETE with invalid request id returned %d, want 400", r.Status)
	}
	if r := g.Dispatch(context.Background(), Request{Method: "GET", Path: "/v1/servers", Body: json.RawMessage(`{"unexpected":1}`)}); r.Status != 400 {
		t.Fatalf("GET with non-empty body returned %d, want 400", r.Status)
	}
}

func TestToolListCachedPerSessionAndEmptySlice(t *testing.T) {
	var listCalls atomic.Int64
	s := mcp.NewServer(&mcp.Implementation{Name: "cache", Version: "1"}, nil)
	s.AddTool(&mcp.Tool{Name: "echo", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
	})
	s.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "tools/list" {
				listCalls.Add(1)
			}
			return next(ctx, method, req)
		}
	})
	srv := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{JSONResponse: true}))
	defer srv.Close()

	g, _ := New(Config{Servers: map[string]ServerConfig{"r": {URL: srv.URL}}}, Options{})
	defer g.Close()
	if r := request(g, "GET", "/v1/servers/r/tools", ""); r.Status != 200 {
		t.Fatal(r)
	}
	for i := 0; i < 3; i++ {
		if r := request(g, "POST", "/v1/servers/r/tools/echo/call", `{}`); r.Status != 200 {
			t.Fatal(r)
		}
	}
	if got := listCalls.Load(); got != 1 {
		t.Fatalf("tools/list called %d times across 1 discovery + 3 calls, want 1", got)
	}

	// Empty tool list serializes as [] not null
	sEmpty := mcp.NewServer(&mcp.Implementation{Name: "empty", Version: "1"}, nil)
	srvEmpty := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return sEmpty }, &mcp.StreamableHTTPOptions{JSONResponse: true}))
	defer srvEmpty.Close()
	gEmpty, _ := New(Config{Servers: map[string]ServerConfig{"e": {URL: srvEmpty.URL}}}, Options{})
	defer gEmpty.Close()
	rEmpty := request(gEmpty, "GET", "/v1/servers/e/tools", "")
	bEmpty, _ := json.Marshal(rEmpty.Body)
	if rEmpty.Status != 200 || !strings.Contains(string(bEmpty), `"data":[]`) {
		t.Fatalf("empty tools serialized as %s, want data:[]", string(bEmpty))
	}
}
